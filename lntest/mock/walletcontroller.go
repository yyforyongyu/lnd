package mock

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
)

// CoinPkScript is the existing witness output used by funding fixtures.
var CoinPkScript, _ = hex.DecodeString(
	"001431df1bde03c074d0cf21ea2529427e1499b8f1de",
)

// WalletController records maintained-wallet calls in lnd consumer fixtures.
// Embedded unused capabilities fail if a test unexpectedly invokes them.
type WalletController struct {
	lnwallet.WalletAPI
	lnwallet.WalletServices
	mock.Mock

	Utxos []*base.Utxo
}

var _ lnwallet.WalletAPI = (*WalletController)(nil)
var _ lnwallet.WalletServices = (*WalletController)(nil)

// IsWatchOnly returns the native mode used to distinguish remote signing.
func (w *WalletController) IsWatchOnly() bool {
	return w.Called().Bool(0)
}

// DerivePubKey records read-only public derivation without allocating children.
func (w *WalletController) DerivePubKey(ctx context.Context,
	params base.DerivePubKeyParams) (*btcec.PublicKey, error) {

	args := w.Called(ctx, params)
	key, _ := args.Get(0).(*btcec.PublicKey)
	return key, args.Error(1)
}

// ListAccounts supplies the native catalog, including empty accounts.
func (w *WalletController) ListAccounts(ctx context.Context) (
	[]base.AccountInfo, error) {

	args := w.Called(ctx)
	accounts, _ := args.Get(0).([]base.AccountInfo)
	return accounts, args.Error(1)
}

// GetAccount records scope-aware lookup for account constraints and fee quotes.
func (w *WalletController) GetAccount(ctx context.Context,
	scope waddrmgr.KeyScope, name string) (*base.AccountInfo, error) {

	args := w.Called(ctx, scope, name)
	info, _ := args.Get(0).(*base.AccountInfo)
	return info, args.Error(1)
}

// ScriptForOutput supplies native ownership metadata for offline inputs too.
func (w *WalletController) ScriptForOutput(ctx context.Context,
	output wire.TxOut) (base.OutputScriptInfo, error) {

	args := w.Called(ctx, output)
	info, _ := args.Get(0).(base.OutputScriptInfo)
	return info, args.Error(1)
}

// ListAddresses returns the native payment-account address projection.
func (w *WalletController) ListAddresses(ctx context.Context, name string,
	addrType waddrmgr.AddressType) ([]base.AddressProperty, error) {

	args := w.Called(ctx, name, addrType)
	addresses, _ := args.Get(0).([]base.AddressProperty)
	return addresses, args.Error(1)
}

// ListTxns returns maintained history for public RPC filtering tests.
func (w *WalletController) ListTxns(ctx context.Context,
	startHeight, endHeight int32) ([]*base.TxDetail, error) {

	args := w.Called(ctx, startHeight, endHeight)
	txns, _ := args.Get(0).([]*base.TxDetail)
	return txns, args.Error(1)
}

// Info returns the arranged native synchronization snapshot.
func (w *WalletController) Info(ctx context.Context) (*base.Info, error) {
	args := w.Called(ctx)
	info, _ := args.Get(0).(*base.Info)
	return info, args.Error(1)
}

// NewAddress records native selector and branch use.
func (w *WalletController) NewAddress(ctx context.Context,
	selector base.AccountSelector, internal bool) (base.AddressInfo,
	error) {

	args := w.Called(ctx, selector, internal)
	info, _ := args.Get(0).(base.AddressInfo)
	return info, args.Error(1)
}

// GetAddressInfo returns arranged wallet-owned address metadata.
func (w *WalletController) GetAddressInfo(ctx context.Context,
	addr address.Address) (base.AddressInfo, error) {

	args := w.Called(ctx, addr)
	info, _ := args.Get(0).(base.AddressInfo)
	return info, args.Error(1)
}

// ListUnspent returns the fixture's arranged native UTXO snapshot.
func (w *WalletController) ListUnspent(ctx context.Context,
	query base.UtxoQuery) ([]*base.Utxo, error) {

	args := w.Called(ctx, query)
	if result, ok := args.Get(0).(func() []*base.Utxo); ok {
		return result(), args.Error(1)
	}
	utxos, _ := args.Get(0).([]*base.Utxo)

	return utxos, args.Error(1)
}

// GetUtxo returns arranged owned-output metadata.
func (w *WalletController) GetUtxo(ctx context.Context,
	op wire.OutPoint) (*base.Utxo, error) {

	args := w.Called(ctx, op)
	// Shared funding fixtures arrange metadata from the requested outpoint
	// without a mock wallet store or losing manual-input identity.
	if result, ok := args.Get(0).(func(wire.OutPoint) *base.Utxo); ok {
		return result(op), args.Error(1)
	}
	utxo, _ := args.Get(0).(*base.Utxo)

	return utxo, args.Error(1)
}

// GetTx returns arranged native transaction metadata.
func (w *WalletController) GetTx(ctx context.Context,
	hash chainhash.Hash) (*base.TxDetail, error) {

	args := w.Called(ctx, hash)
	detail, _ := args.Get(0).(*base.TxDetail)
	return detail, args.Error(1)
}

// LeaseOutput records the owner, duration and native options.
func (w *WalletController) LeaseOutput(ctx context.Context,
	id wtxmgr.LockID, op wire.OutPoint, duration time.Duration,
	opts ...base.LeaseOption) (time.Time, error) {

	args := w.Called(ctx, id, op, duration, opts)
	return args.Get(0).(time.Time), args.Error(1)
}

// ReleaseOutput records lease ownership when returning a coin.
func (w *WalletController) ReleaseOutput(ctx context.Context,
	id wtxmgr.LockID, op wire.OutPoint) error {

	return w.Called(ctx, id, op).Error(0)
}

// ListLeasedOutputs returns the arranged native lease snapshot.
func (w *WalletController) ListLeasedOutputs(ctx context.Context) (
	[]*base.LeasedOutput, error) {

	args := w.Called(ctx)
	leases, _ := args.Get(0).([]*base.LeasedOutput)
	return leases, args.Error(1)
}

// Broadcast records publication without creating a separate mock wallet store.
func (w *WalletController) Broadcast(ctx context.Context,
	tx *wire.MsgTx, label string) error {

	return w.Called(ctx, tx, label).Error(0)
}

// Start records application wallet startup.
func (w *WalletController) Start() error {
	return w.Called().Error(0)
}

// Stop records joined application wallet shutdown.
func (w *WalletController) Stop() error {
	return w.Called().Error(0)
}

// DecorateInputs returns the arranged packet without a mock wallet algorithm.
func (w *WalletController) DecorateInputs(ctx context.Context,
	packet *psbt.Packet, skipUnknown bool) (*psbt.Packet, error) {

	args := w.Called(ctx, packet, skipUnknown)
	result, _ := args.Get(0).(*psbt.Packet)
	return result, args.Error(1)
}

// DeleteUnconfirmedTx records root deletion at the maintained boundary.
func (w *WalletController) DeleteUnconfirmedTx(ctx context.Context,
	hash chainhash.Hash) error {

	return w.Called(ctx, hash).Error(0)
}

// FinalizePsbt records native local finalization without requiring origins.
func (w *WalletController) FinalizePsbt(ctx context.Context,
	packet *psbt.Packet) error {

	return w.Called(ctx, packet).Error(0)
}

// FundPsbt returns the arranged packet and native reservation owner.
func (w *WalletController) FundPsbt(ctx context.Context,
	intent *base.FundIntent) (*base.FundResult, error) {

	args := w.Called(ctx, intent)
	result, _ := args.Get(0).(*base.FundResult)
	return result, args.Error(1)
}

// NewAccount records scoped native creation without a mock account registry.
func (w *WalletController) NewAccount(ctx context.Context,
	params base.NewAccountParams) (*base.AccountInfo, error) {

	args := w.Called(ctx, params)
	info, _ := args.Get(0).(*base.AccountInfo)
	return info, args.Error(1)
}

// SubscribeTxns borrows the arranged event stream and cancellation function.
func (w *WalletController) SubscribeTxns(ctx context.Context) (
	<-chan *base.TxEvent, func(), error) {

	args := w.Called(ctx)
	events, _ := args.Get(0).(<-chan *base.TxEvent)
	cancel, _ := args.Get(1).(func())

	return events, cancel, args.Error(2)
}

// ListAccountsByName returns native scoped matches for name-only requests.
func (w *WalletController) ListAccountsByName(ctx context.Context,
	name string) ([]base.AccountInfo, error) {

	args := w.Called(ctx, name)
	accounts, _ := args.Get(0).([]base.AccountInfo)
	return accounts, args.Error(1)
}
