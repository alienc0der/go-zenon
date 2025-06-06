package mock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"

	"github.com/zenon-network/go-zenon/chain"
	"github.com/zenon-network/go-zenon/chain/genesis"
	g "github.com/zenon-network/go-zenon/chain/genesis/mock"
	"github.com/zenon-network/go-zenon/chain/nom"
	"github.com/zenon-network/go-zenon/common"
	"github.com/zenon-network/go-zenon/common/db"
	"github.com/zenon-network/go-zenon/common/types"
	"github.com/zenon-network/go-zenon/consensus"
	"github.com/zenon-network/go-zenon/pillar"
	"github.com/zenon-network/go-zenon/protocol"
	"github.com/zenon-network/go-zenon/verifier"
	"github.com/zenon-network/go-zenon/vm"
	"github.com/zenon-network/go-zenon/vm/vm_context"
	"github.com/zenon-network/go-zenon/zenon"
)

const (
	SkipVmChanges = "PASS-VM-CHANGES"
	NoVmChanges   = `
storage
balance`
)

var AllLoggers = []common.Logger{
	common.ZenonLogger,
	common.ChainLogger,
	common.SupervisorLogger,
	common.P2PLogger,
	common.PillarLogger,
	common.RPCLogger,
	common.WalletLogger,
	common.EmbeddedLogger,
	common.VmLogger,
	common.ProtocolLogger,
	common.FetcherLogger,
	common.DownloaderLogger,
}

type AutogeneratedBlockEntry struct {
	Msg           string              `json:"msg"`
	Identifier    types.AccountHeader `json:"identifier"`
	SendBlock     types.AccountHeader `json:"send-block-header"`
	ReturnedError string              `json:"returned-error"`
}

type ProducerLogSaver struct {
	format  log15.Format
	buffer  *bytes.Buffer
	results map[types.Hash]string
}

func (f *ProducerLogSaver) Format(r *log15.Record) []byte {
	r.Time = common.Clock.Now()
	data := f.format.Format(r)

	e := &AutogeneratedBlockEntry{}
	err := json.Unmarshal(data, e)
	if err == nil && e.Msg == "generated embedded-block" {
		f.results[e.SendBlock.Hash] = e.ReturnedError
	}

	return data
}

type MockContractCaller struct {
	stack         string
	t             common.T
	sendBlockHash types.Hash
	results       *map[types.Hash]string
}

func (mcc *MockContractCaller) LateCallFunction() (string, error) {
	str, ok := (*mcc.results)[mcc.sendBlockHash]
	if !ok {
		return "", errors.Errorf("'can't find sendBlock of contract. Maybe the send-block is not cemented yet or the test doesn't end with a 'InsertNewMomentum()'?\n")
	} else {
		return "", errors.Errorf("%v", str)
	}
}

func getSignFunc(address types.Address) vm.SignFunc {
	for _, keyPair := range g.AllKeyPairs {
		if keyPair.Address == address {
			return keyPair.Signer
		}
	}
	panic(fmt.Sprintf("Can't get sign func for %v\n", address))
}

type mockClock struct {
	chain    chain.Chain
	lastTime *time.Time
}

func (clock *mockClock) Now() time.Time {
	store := clock.chain.GetFrontierMomentumStore()
	// DB didn't stop. Setting new frontier time.
	if store != nil {
		momentum, err := store.GetFrontierMomentum()
		common.DealWithErr(err)
		t := *momentum.Timestamp
		clock.lastTime = &t
	}
	return *clock.lastTime
}

type mockZenon struct {
	lastTime         *time.Time
	t                common.T
	log              log15.Logger
	producerLogSaver *ProducerLogSaver

	pillars    []pillar.Manager
	chain      chain.Chain
	consensus  consensus.Consensus
	supervisor *vm.Supervisor

	loggers              []log15.Logger
	handlers             []log15.Handler
	initialEpochDuration time.Duration
}

func (zenon *mockZenon) SyncInfo() *protocol.SyncInfo {
	return &protocol.SyncInfo{
		State:         protocol.SyncDone,
		CurrentHeight: 0,
		TargetHeight:  0,
	}
}
func (zenon *mockZenon) SyncState() protocol.SyncState {
	return protocol.SyncDone
}
func (zenon *mockZenon) CreateMomentum(momentumTransaction *nom.MomentumTransaction) {
	insert := zenon.chain.AcquireInsert("mock-zenon create-momentum")
	defer insert.Unlock()
	err := zenon.chain.AddMomentumTransaction(insert, momentumTransaction)
	if err != nil {
		panic(fmt.Errorf("failed to insert own momentum. reason:%w", err))
	}
	for _, block := range momentumTransaction.Momentum.Content {
		zenon.log.Info("added block to momentum", "momentum-height", momentumTransaction.Momentum.Height, "identifier", block)
	}
}
func (zenon *mockZenon) CreateAccountBlock(accountBlockTransaction *nom.AccountBlockTransaction) {
	insert := zenon.chain.AcquireInsert("mock-zenon create-account-block")
	defer insert.Unlock()
	err := zenon.chain.AddAccountBlockTransaction(insert, accountBlockTransaction)
	if err == nil {
		zenon.log.Info("inserted block", "identifier", accountBlockTransaction.Block.Header())
	} else {
		zenon.log.Info("failed to insert block", "reason", err, "identifier", accountBlockTransaction.Block.Header())
	}
	if err != nil {
		zenon.log.Error("failed to insert own account-block.", "reason", err)
	}
}

func (zenon *mockZenon) InsertNewMomentum() {
	store := zenon.chain.GetFrontierMomentumStore()
	previousMomentum, err := store.GetFrontierMomentum()
	common.DealWithErr(err)
	t := previousMomentum.Timestamp.Add(time.Second * 10)
	expected, err := zenon.consensus.GetMomentumProducer(t)
	common.DealWithErr(err)
	if expected == nil {
		panic("nil expected")
	}

	for _, pillarE := range zenon.pillars {
		if *pillarE.GetCoinBase() == *expected {
			pillarE.Process(consensus.ProducerEvent{
				Producer:  *expected,
				StartTime: t,
				EndTime:   t.Add(time.Second * 10),
				Name:      "",
			}).Wait()
		}
	}
}
func (zenon *mockZenon) InsertMomentumsTo(targetHeight uint64) {
	currentHeight := zenon.chain.GetFrontierMomentumStore().Identifier().Height
	for i := currentHeight + 1; i <= targetHeight; i += 1 {
		zenon.InsertNewMomentum()
	}
}

func (zenon *mockZenon) CallContract(template *nom.AccountBlock) *common.Expecter {
	template.BlockType = nom.BlockTypeUserSend
	if !types.IsEmbeddedAddress(template.ToAddress) {
		zenon.t.Fatalf("Unable to CallContract for block %v. Reason: ToAddress is not a contract address", template)
	}

	sendBlock := zenon.InsertSendBlock(template, nil, SkipVmChanges)

	mcc := &MockContractCaller{
		stack:         common.GetStack(),
		t:             zenon.t,
		results:       &zenon.producerLogSaver.results,
		sendBlockHash: sendBlock.Hash,
	}

	return common.LateCaller(mcc.LateCallFunction)
}

func (zenon *mockZenon) InsertSendBlock(template *nom.AccountBlock, expectedError error, expectedVmChanges string) *nom.AccountBlock {
	template.BlockType = nom.BlockTypeUserSend
	transaction, err := zenon.supervisor.GenerateFromTemplate(template, getSignFunc(template.Address))
	common.ExpectError(zenon.t, err, expectedError)
	if err == nil {
		if expectedVmChanges != SkipVmChanges {
			common.ExpectString(zenon.t, db.DebugPatch(transaction.Changes), expectedVmChanges)
		}
		zenon.CreateAccountBlock(transaction)
		return transaction.Block
	}
	return nil
}
func (zenon *mockZenon) InsertReceiveBlock(fromHeader types.AccountHeader, template *nom.AccountBlock, expectedError error, expectedVmChanges string) *nom.AccountBlock {
	store := zenon.chain.GetFrontierAccountStore(fromHeader.Address)
	fromBlock, err := store.ByHeight(fromHeader.Height)
	if fromBlock == nil {
		zenon.t.Fatalf("failed to get from-transaction from header %v. Maybe is not cemented?", fromHeader)
	}
	if fromBlock.Hash != fromHeader.Hash {
		zenon.t.Fatalf("transaction has different identifier. Expected %v but got %v", fromHeader.Identifier(), fromBlock.Identifier())
	}

	common.FailIfErr(zenon.t, err)
	if template == nil {
		template = &nom.AccountBlock{}
	}

	if template.BlockType == 0 {
		template.BlockType = nom.BlockTypeUserReceive
	}
	if template.Address.IsZero() {
		template.Address = fromBlock.ToAddress
	}
	if template.FromBlockHash.IsZero() {
		template.FromBlockHash = fromBlock.Hash
	}

	transaction, err := zenon.supervisor.GenerateFromTemplate(template, getSignFunc(template.Address))

	common.ExpectError(zenon.t, err, expectedError)
	if err == nil {
		if expectedVmChanges != SkipVmChanges {
			common.ExpectString(zenon.t, db.DebugPatch(transaction.Changes), expectedVmChanges)
		}
		zenon.CreateAccountBlock(transaction)
		return transaction.Block
	}
	return nil
}

func (zenon *mockZenon) EmbeddedContext(address types.Address) vm_context.AccountVmContext {
	momentumStore := zenon.chain.GetFrontierMomentumStore()
	accountStore := zenon.chain.GetFrontierAccountStore(address)

	return vm_context.NewAccountContext(
		momentumStore,
		accountStore,
		zenon.consensus.FixedPillarReader(momentumStore.Identifier()),
	)

}

func (zenon *mockZenon) SaveLogs(logger common.Logger) *common.Expecter {
	return common.SaveLogs(logger)
}
func (zenon *mockZenon) ExpectBalance(address types.Address, standard types.ZenonTokenStandard, expected int64) {
	amount, err := zenon.chain.GetFrontierAccountStore(address).GetBalance(standard)
	common.FailIfErr(zenon.t, err)
	if amount == nil {
		amount = big.NewInt(0)
	}
	common.ExpectAmount(zenon.t, amount, big.NewInt(expected))
}

// protocol

func (zenon *mockZenon) Init() error {
	common.DealWithErr(zenon.chain.Init())
	common.DealWithErr(zenon.consensus.Init())
	for _, pillarE := range zenon.pillars {
		common.DealWithErr(pillarE.Init())
	}
	return nil
}
func (zenon *mockZenon) Start() error {
	common.DealWithErr(zenon.chain.Start())
	common.DealWithErr(zenon.consensus.Start())
	for _, pillarE := range zenon.pillars {
		common.DealWithErr(pillarE.Start())
	}
	return nil
}
func (zenon *mockZenon) Stop() error {
	for _, pillarE := range zenon.pillars {
		common.DealWithErr(pillarE.Stop())
	}
	common.DealWithErr(zenon.consensus.Stop())
	common.DealWithErr(zenon.chain.Stop())

	zenon.chain = nil
	zenon.consensus = nil
	zenon.pillars = nil

	for i := range zenon.loggers {
		zenon.loggers[i].SetHandler(zenon.handlers[i])
	}

	consensus.EpochDuration = zenon.initialEpochDuration
	return nil
}
func (zenon *mockZenon) StopPanic() {
	zenon.log.Debug("finished")
	common.DealWithErr(zenon.Stop())
}

func (zenon *mockZenon) Chain() chain.Chain {
	return zenon.chain
}
func (zenon *mockZenon) Consensus() consensus.Consensus {
	return zenon.consensus
}
func (zenon *mockZenon) Verifier() verifier.Verifier {
	return nil
}
func (zenon *mockZenon) Protocol() *protocol.ProtocolManager {
	return nil
}
func (zenon *mockZenon) Producer() pillar.Manager {
	return nil
}
func (zenon *mockZenon) Config() *zenon.Config {
	return nil
}
func (zenon *mockZenon) Broadcaster() protocol.Broadcaster {
	return zenon
}

func NewMockZenon(t common.T) MockZenon {
	return newMockZenon(t, consensus.EpochDuration)
}
func NewMockZenonWithCustomEpochDuration(t common.T, epochDuration time.Duration) MockZenon {
	return newMockZenon(t, epochDuration)
}

func newMockZenon(t common.T, customEpochDuration time.Duration) MockZenon {
	// silence loggers
	common.ChainLogger.SetHandler(log15.LvlFilterHandler(log15.LvlError, log15.StderrHandler))
	common.ConsensusLogger.SetHandler(log15.LvlFilterHandler(log15.LvlError, log15.StderrHandler))
	common.SupervisorLogger.SetHandler(log15.LvlFilterHandler(log15.LvlError, log15.StderrHandler))
	consensus.EpochDuration = customEpochDuration

	ch := chain.NewChain(db.NewLevelDBManager(t.TempDir()), genesis.NewGenesis(g.EmbeddedGenesis))
	cs := consensus.NewConsensus(db.NewMemDB(), ch, true)
	supervisor := vm.NewSupervisor(ch, cs)
	zenon := &mockZenon{
		t:                    t,
		log:                  common.ZenonLogger,
		chain:                ch,
		consensus:            cs,
		supervisor:           supervisor,
		loggers:              make([]log15.Logger, len(AllLoggers)),
		handlers:             make([]log15.Handler, len(AllLoggers)),
		initialEpochDuration: consensus.EpochDuration,
	}

	for i := range AllLoggers {
		zenon.loggers[i] = AllLoggers[i]
		zenon.handlers[i] = AllLoggers[i].GetHandler()
	}

	common.Clock = &mockClock{
		chain: ch,
	}

	handler := &ProducerLogSaver{
		format:  log15.JsonFormat(),
		buffer:  new(bytes.Buffer),
		results: map[types.Hash]string{},
	}
	var logHandle []log15.Handler

	logHandle = append(logHandle, log15.LvlFilterHandler(log15.LvlInfo, log15.StreamHandler(handler.buffer, handler)))
	logHandle = append(logHandle, log15.LvlFilterHandler(log15.LvlError, log15.StderrHandler))

	common.PillarLogger.SetHandler(log15.MultiHandler(
		logHandle...,
	))
	zenon.producerLogSaver = handler

	pillars := make([]pillar.Manager, len(g.PillarKeys))
	for i, key := range g.PillarKeys {
		pillars[i] = pillar.NewPillar(ch, cs, zenon)
		pillars[i].SetCoinBase(key)
	}
	zenon.pillars = pillars

	zenon.Init()
	zenon.Start()

	return zenon
}
