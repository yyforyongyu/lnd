package lnwallet

import (
	"context"
	"encoding/hex"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/mock"
)

var (
	CoinPkScript, _ = hex.DecodeString(
		"001431df1bde03c074d0cf21ea2529427e1499b8f1de",
	)
)

// mockWalletController records the native operations exercised by wallet tests.
// Unused capabilities panic through the embedded interfaces if called.
type mockWalletController struct {
	WalletAPI
	WalletServices
	mock.Mock

	PublishedTransactions chan *wire.MsgTx
}

var _ WalletAPI = (*mockWalletController)(nil)
var _ WalletServices = (*mockWalletController)(nil)

// IsWatchOnly returns the arranged native custody mode for coin consumers.
func (w *mockWalletController) IsWatchOnly() bool {
	return w.Called().Bool(0)
}

// ListUnspent returns the arranged native coins without implementing selection.
func (w *mockWalletController) ListUnspent(ctx context.Context,
	query wallet.UtxoQuery) ([]*wallet.Utxo, error) {

	args := w.Called(ctx, query)
	utxos, _ := args.Get(0).([]*wallet.Utxo)
	return utxos, args.Error(1)
}

// GetTx supplies parent metadata when remote coinbase maturity needs checking.
func (w *mockWalletController) GetTx(ctx context.Context,
	hash chainhash.Hash) (*wallet.TxDetail, error) {

	args := w.Called(ctx, hash)
	detail, _ := args.Get(0).(*wallet.TxDetail)
	return detail, args.Error(1)
}

// Broadcast records native publication and returns its arranged outcome.
func (w *mockWalletController) Broadcast(ctx context.Context,
	tx *wire.MsgTx, label string) error {

	return w.Called(ctx, tx, label).Error(0)
}

// Start records application initialization for lifecycle tests.
func (w *mockWalletController) Start() error {
	return w.Called().Error(0)
}

// Stop records joined application shutdown for lifecycle tests.
func (w *mockWalletController) Stop() error {
	return w.Called().Error(0)
}

// mockChainNotifier is a mock implementation of the ChainNotifier interface.
type mockChainNotifier struct {
	SpendChan chan *chainntnfs.SpendDetail
	EpochChan chan *chainntnfs.BlockEpoch
	ConfChan  chan *chainntnfs.TxConfirmation
}

// RegisterConfirmationsNtfn returns a ConfirmationEvent that contains a channel
// that the tx confirmation will go over.
func (c *mockChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	return &chainntnfs.ConfirmationEvent{
		Confirmed: c.ConfChan,
		Cancel:    func() {},
	}, nil
}

// RegisterSpendNtfn returns a SpendEvent that contains a channel that the spend
// details will go over.
func (c *mockChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return &chainntnfs.SpendEvent{
		Spend:  c.SpendChan,
		Cancel: func() {},
	}, nil
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent that contains a channel that
// block epochs will go over.
func (c *mockChainNotifier) RegisterBlockEpochNtfn(
	blockEpoch *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent,
	error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: c.EpochChan,
		Cancel: func() {},
	}, nil
}

// Start currently returns a dummy value.
func (c *mockChainNotifier) Start() error {
	return nil
}

// Started currently returns a dummy value.
func (c *mockChainNotifier) Started() bool {
	return true
}

// Stop currently returns a dummy value.
func (c *mockChainNotifier) Stop() error {
	return nil
}

type mockChainIO struct{}

func (*mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, 0, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32, _ <-chan struct{}) (*wire.TxOut, error) {

	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock,
	error) {

	return nil, nil
}

func (*mockChainIO) GetBlockHeader(
	blockHash *chainhash.Hash) (*wire.BlockHeader, error) {

	return nil, nil
}

type MockAuxLeafStore struct{}

// A compile time check to ensure that MockAuxLeafStore implements the
// AuxLeafStore interface.
var _ AuxLeafStore = (*MockAuxLeafStore)(nil)

// FetchLeavesFromView attempts to fetch the auxiliary leaves that
// correspond to the passed aux blob, and pending original (unfiltered)
// HTLC view.
func (*MockAuxLeafStore) FetchLeavesFromView(
	_ CommitDiffAuxInput) fn.Result[CommitDiffAuxResult] {

	return fn.Ok(CommitDiffAuxResult{})
}

// FetchLeavesFromCommit attempts to fetch the auxiliary leaves that
// correspond to the passed aux blob, and an existing channel
// commitment.
func (*MockAuxLeafStore) FetchLeavesFromCommit(_ AuxChanState,
	_ channeldb.ChannelCommitment, _ CommitmentKeyRing,
	_ lntypes.ChannelParty) fn.Result[CommitDiffAuxResult] {

	return fn.Ok(CommitDiffAuxResult{})
}

// FetchLeavesFromRevocation attempts to fetch the auxiliary leaves
// from a channel revocation that stores balance + blob information.
func (*MockAuxLeafStore) FetchLeavesFromRevocation(
	_ *channeldb.RevocationLog) fn.Result[CommitDiffAuxResult] {

	return fn.Ok(CommitDiffAuxResult{})
}

// ApplyHtlcView serves as the state transition function for the custom
// channel's blob. Given the old blob, and an HTLC view, then a new
// blob should be returned that reflects the pending updates.
func (*MockAuxLeafStore) ApplyHtlcView(
	_ CommitDiffAuxInput) fn.Result[fn.Option[tlv.Blob]] {

	return fn.Ok(fn.None[tlv.Blob]())
}

// EmptyMockJobHandler is a mock job handler that just sends an empty response
// to all jobs.
func EmptyMockJobHandler(jobs []AuxSigJob) {
	for _, sigJob := range jobs {
		sigJob.Resp <- AuxSigJobResp{}
	}
}

// MockAuxSigner is a mock implementation of the AuxSigner interface.
type MockAuxSigner struct {
	mock.Mock

	jobHandlerFunc func([]AuxSigJob)
}

// NewAuxSignerMock creates a new mock aux signer with the given job handler.
func NewAuxSignerMock(jobHandler func([]AuxSigJob)) *MockAuxSigner {
	return &MockAuxSigner{
		jobHandlerFunc: jobHandler,
	}
}

// SubmitSecondLevelSigBatch takes a batch of aux sign jobs and
// processes them asynchronously.
func (a *MockAuxSigner) SubmitSecondLevelSigBatch(chanState AuxChanState,
	tx *wire.MsgTx, jobs []AuxSigJob) error {

	args := a.Called(chanState, tx, jobs)

	if a.jobHandlerFunc != nil {
		a.jobHandlerFunc(jobs)
	}

	return args.Error(0)
}

// PackSigsFunc packs aux signatures into a blob. A test can register one of
// these with the mock instead of a fixed value, so that the packed blob can
// depend on the signatures handed in. That matters because a real aux signer
// emits exactly one slot per HTLC.
type PackSigsFunc func([]fn.Option[tlv.Blob]) fn.Result[fn.Option[tlv.Blob]]

// UnpackSigsFunc is the counterpart of PackSigsFunc, recovering the per-HTLC
// slots from a packed blob.
type UnpackSigsFunc func(fn.Option[tlv.Blob]) fn.Result[[]fn.Option[tlv.Blob]]

// PackSigs takes a series of aux signatures and packs them into a
// single blob that can be sent alongside the CommitSig messages.
func (a *MockAuxSigner) PackSigs(
	sigs []fn.Option[tlv.Blob]) fn.Result[fn.Option[tlv.Blob]] {

	args := a.Called(sigs)

	if f, ok := args.Get(0).(PackSigsFunc); ok {
		return f(sigs)
	}

	return args.Get(0).(fn.Result[fn.Option[tlv.Blob]])
}

// UnpackSigs takes a packed blob of signatures and returns the
// original signatures for each HTLC, keyed by HTLC index.
func (a *MockAuxSigner) UnpackSigs(
	sigs fn.Option[tlv.Blob]) fn.Result[[]fn.Option[tlv.Blob]] {

	args := a.Called(sigs)

	if f, ok := args.Get(0).(UnpackSigsFunc); ok {
		return f(sigs)
	}

	return args.Get(0).(fn.Result[[]fn.Option[tlv.Blob]])
}

// VerifySecondLevelSigs attempts to synchronously verify a batch of aux
// sig jobs.
func (a *MockAuxSigner) VerifySecondLevelSigs(chanState AuxChanState,
	tx *wire.MsgTx, jobs []AuxVerifyJob) error {

	args := a.Called(chanState, tx, jobs)

	return args.Error(0)
}

type MockAuxContractResolver struct{}

// ResolveContract is called to resolve a contract that needs
// additional information to resolve properly. If no extra information
// is required, a nil Result error is returned.
func (*MockAuxContractResolver) ResolveContract(
	ResolutionReq) fn.Result[tlv.Blob] {

	return fn.Ok[tlv.Blob](nil)
}
