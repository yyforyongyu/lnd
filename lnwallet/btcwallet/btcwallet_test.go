package btcwallet

import (
	"bytes"
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// walletControllerMock records calls at the maintained adapter boundary;
// unused embedded operations have no implementation and fail if invoked.
type walletControllerMock struct {
	walletAPI
	mock.Mock
}

// LeaseOutput returns the wallet's ownership conflict to test error mapping.
func (w *walletControllerMock) LeaseOutput(ctx context.Context,
	id wtxmgr.LockID, op wire.OutPoint,
	duration time.Duration) (time.Time, error) {

	args := w.Called(ctx, id, op, duration)
	expiry, _ := args.Get(0).(time.Time)
	return expiry, args.Error(1)
}

// ListLeasedOutputs exposes only the maintained lease fields to the adapter.
func (w *walletControllerMock) ListLeasedOutputs(ctx context.Context) (
	[]*wallet.LeasedOutput, error) {

	args := w.Called(ctx)
	leases, _ := args.Get(0).([]*wallet.LeasedOutput)

	return leases, args.Error(1)
}

// ListUnspent returns the arranged wallet view, including custody and locks.
func (w *walletControllerMock) ListUnspent(ctx context.Context,
	query wallet.UtxoQuery) ([]*wallet.Utxo, error) {

	args := w.Called(ctx, query)
	utxos, _ := args.Get(0).([]*wallet.Utxo)

	return utxos, args.Error(1)
}

// GetTx supplies parent transactions for the adapter's maturity check.
func (w *walletControllerMock) GetTx(ctx context.Context,
	hash chainhash.Hash) (*wallet.TxDetail, error) {

	args := w.Called(ctx, hash)
	tx, _ := args.Get(0).(*wallet.TxDetail)

	return tx, args.Error(1)
}

// CreateTransaction checks selection before returning the unsigned handoff.
func (w *walletControllerMock) CreateTransaction(ctx context.Context,
	intent *wallet.TxIntent) (*txauthor.AuthoredTx, error) {

	args := w.Called(ctx, intent)
	tx, _ := args.Get(0).(*txauthor.AuthoredTx)

	return tx, args.Error(1)
}

// GetPrivKeyForAddress supplies the maintained signer's ambiguous key error.
func (w *walletControllerMock) GetPrivKeyForAddress(ctx context.Context,
	addr address.Address) (*btcec.PrivateKey, error) {

	args := w.Called(ctx, addr)
	key, _ := args.Get(0).(*btcec.PrivateKey)

	return key, args.Error(1)
}

// GetAddressInfo distinguishes unknown addresses from owned public imports.
func (w *walletControllerMock) GetAddressInfo(ctx context.Context,
	addr address.Address) (wallet.AddressInfo, error) {

	args := w.Called(ctx, addr)
	info, _ := args.Get(0).(wallet.AddressInfo)

	return info, args.Error(1)
}

// DerivePrivKey records whether the legacy zero-locator fallback was allowed.
func (w *walletControllerMock) DerivePrivKey(ctx context.Context,
	path wallet.BIP32Path) (*btcec.PrivateKey, error) {

	args := w.Called(ctx, path)
	key, _ := args.Get(0).(*btcec.PrivateKey)

	return key, args.Error(1)
}

// TestWatchOnlySendOutputs preserves balances and unsigned transaction
// handoff while excluding locked outputs and immature coinbase outputs.
func TestWatchOnlySendOutputs(t *testing.T) {
	t.Parallel()

	// Arrange a real watch-only wallet so custody uses the same immutable
	// Manager result as production. Its unsynced chain mock keeps runtime
	// work bounded by Manager.Stop; transaction reads use the adapter mock.
	params := &chaincfg.RegressionNetParams
	source := &lnmock.MockChain{}
	source.On("IsCurrent").Return(false).Maybe()
	source.On("GetBestBlock").
		Return(params.GenesisHash, int32(0), nil).Maybe()
	source.On("GetBlockHash", int64(0)).
		Return(params.GenesisHash, nil).Maybe()
	source.On("GetBlockHeader", params.GenesisHash).
		Return(&params.GenesisBlock.Header, nil).Maybe()
	source.On("BackEnd").Return("mock").Maybe()
	manager, err := wallet.NewManager(t.Context(), wallet.ManagerConfig{
		Backend:     wallet.DBBackendSQLite,
		DataSource:  filepath.Join(t.TempDir(), "wallet.sqlite"),
		ChainParams: *params,
		ChainSource: source,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.Stop())
		source.AssertExpectations(t)
	})
	_, err = manager.Start(t.Context())
	require.NoError(t, err)
	watchOnly, err := manager.Create(wallet.CreateWalletParams{
		Name:              "lnd",
		Mode:              wallet.ModeShell,
		WatchOnly:         true,
		PrivatePassphrase: []byte("test-password"),
	})
	require.NoError(t, err)
	backend := &walletControllerMock{}
	controller := &BtcWallet{
		wallet:    backend,
		netParams: params,
		cfg: &Config{
			WatchOnly: true,
			Wallet:    watchOnly,
		},
	}

	// Supply an ordinary witness credit and an immature coinbase credit.
	// Both lack local keys, but only the ordinary credit is eligible for
	// remote signing. A third locked credit must also stay excluded.
	_, pubKey := btcec.PrivKeyFromBytes([]byte{1})
	script, err := input.WitnessPubKeyHash(
		pubKey.SerializeCompressed(),
	)
	require.NoError(t, err)
	var utxos []*wallet.Utxo
	for _, coinbase := range []bool{false, true} {
		parent := wire.NewMsgTx(2)
		prev := wire.OutPoint{Index: 1}
		if coinbase {
			prev.Index = math.MaxUint32
		}
		parent.AddTxIn(wire.NewTxIn(&prev, nil, nil))
		parent.AddTxOut(&wire.TxOut{Value: 100000, PkScript: script})
		var raw bytes.Buffer
		require.NoError(t, parent.Serialize(&raw))
		hash := parent.TxHash()
		utxos = append(utxos, &wallet.Utxo{
			OutPoint:      wire.OutPoint{Hash: hash},
			Amount:        100000,
			PkScript:      script,
			Confirmations: 1,
			Spendable:     false,
		})
		backend.On("GetTx", mock.Anything, hash).
			Return(&wallet.TxDetail{
				RawTx: raw.Bytes(),
			}, nil).Twice()
	}
	utxos = append(utxos, &wallet.Utxo{Locked: true})
	backend.On("ListUnspent", mock.Anything, wallet.UtxoQuery{
		Account:  lnwallet.DefaultAccountName,
		MinConfs: 1,
		MaxConfs: math.MaxInt32,
	}).Return(utxos, nil).Twice()
	unsigned := wire.NewMsgTx(2)
	unsigned.AddTxIn(wire.NewTxIn(&utxos[0].OutPoint, nil, nil))
	output := &wire.TxOut{Value: 50000, PkScript: script}
	unsigned.AddTxOut(output)
	backend.On("CreateTransaction", mock.Anything,
		mock.MatchedBy(func(intent *wallet.TxIntent) bool {
			policy, ok := intent.Inputs.(*wallet.InputsPolicy)
			if !ok {
				return false
			}
			pool, ok := policy.Source.(*wallet.CoinSourceUTXOs)

			return ok && len(pool.UTXOs) == 1 &&
				pool.UTXOs[0] == utxos[0].OutPoint
		})).Return(&txauthor.AuthoredTx{Tx: unsigned}, nil).Once()

	// Act through the public balance and send methods used by lnd and
	// RPCKeyRing. The send must return before any local signing/publishing.
	balance, err := controller.ConfirmedBalance(
		1, lnwallet.DefaultAccountName,
	)
	require.NoError(t, err)
	tx, err := controller.SendOutputs(
		nil, []*wire.TxOut{output}, 2500, 1, "", nil,
	)

	// Assert only remote-signable unlocked value is counted and the exact
	// authored transaction reaches the remote signer's established handoff.
	require.Equal(t, btcutil.Amount(100000), balance)
	require.ErrorIs(t, err, wallet.ErrTxUnsigned)
	require.Same(t, unsigned, tx)
	backend.AssertExpectations(t)
}

// TestLeaseOutputRejectsLockedOutput preserves WalletController's conflict
// sentinel when the maintained wallet rejects an already reserved outpoint.
func TestLeaseOutputRejectsLockedOutput(t *testing.T) {
	t.Parallel()

	// Arrange a wallet reservation conflict for the exact lease request.
	// The adapter delegates reservation ownership to the maintained wallet.
	backend := &walletControllerMock{}
	backend.On(
		"LeaseOutput", mock.Anything, wtxmgr.LockID{},
		wire.OutPoint{}, time.Minute,
	).Return(time.Time{}, wallet.ErrOutputAlreadyLocked).Once()
	controller := &BtcWallet{wallet: backend}

	// Act through WalletController, as callers do when reserving an input.
	_, err := controller.LeaseOutput(
		wtxmgr.LockID{}, wire.OutPoint{}, time.Minute,
	)

	// Assert the public lnd sentinel and that only the declared call
	// occurred.
	require.ErrorIs(t, err, wtxmgr.ErrOutputAlreadyLocked)
	backend.AssertExpectations(t)
}

// TestListLeasedOutputs preserves the bbolt lease values needed for balances
// without requiring unavailable spend-depth metadata from the wallet.
func TestListLeasedOutputs(t *testing.T) {
	t.Parallel()

	// Arrange a persisted lease and its serialized parent output. The
	// existing mock supplies the maintained reader results, while the
	// legacy controller previously rejected these available values.
	parent := wire.NewMsgTx(2)
	parent.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
	parent.AddTxOut(wire.NewTxOut(12345, []byte{0x51}))
	var raw bytes.Buffer
	require.NoError(t, parent.Serialize(&raw))
	lease := &wallet.LeasedOutput{
		OutPoint:   wire.OutPoint{Hash: parent.TxHash()},
		LockID:     wtxmgr.LockID{1},
		Expiration: time.Unix(123, 0),
	}
	backend := &walletControllerMock{}
	backend.On("ListLeasedOutputs", mock.Anything).
		Return([]*wallet.LeasedOutput{lease}, nil).Once()
	backend.On("GetTx", mock.Anything, lease.OutPoint.Hash).
		Return(&wallet.TxDetail{RawTx: raw.Bytes()}, nil).Once()
	controller := &BtcWallet{
		wallet: backend,
		cfg: &Config{
			ManagerConfig: wallet.ManagerConfig{
				Backend: wallet.DBBackendKVDB,
			},
		},
	}

	// Act through the public adapter method used by WalletBalance.
	leases, err := controller.ListLeasedOutputs()

	// Assert the available identity, expiration and output values survive
	// conversion, so the balance caller can sum the actual locked amount.
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, lease.OutPoint, leases[0].Outpoint)
	require.Equal(t, lease.LockID, leases[0].LockID)
	require.Equal(t, lease.Expiration, leases[0].Expiration)
	require.Equal(t, parent.TxOut[0].Value, leases[0].Value)
	require.Equal(t, parent.TxOut[0].PkScript, leases[0].PkScript)
	backend.AssertExpectations(t)
}

// TestBtcWalletStop proves Manager releases storage before lnd stops its
// borrowed chain source, without introducing a mock Manager lifecycle.
func TestBtcWalletStop(t *testing.T) {
	t.Parallel()

	// Arrange a real Manager holding a bbolt database. The chain's Stop
	// expectation attempts a bounded reopen, which succeeds only after the
	// Manager has joined wallet work and released the database lock.
	params := &chaincfg.RegressionNetParams
	source := &lnmock.MockChain{}
	source.On("IsCurrent").Return(false).Maybe()
	source.On("GetBestBlock").
		Return(params.GenesisHash, int32(0), nil).Maybe()
	source.On("GetBlockHash", int64(0)).
		Return(params.GenesisHash, nil).Maybe()
	source.On("GetBlockHeader", params.GenesisHash).
		Return(&params.GenesisBlock.Header, nil).Maybe()
	source.On("BackEnd").Return("mock").Maybe()
	dbPath := filepath.Join(t.TempDir(), "wallet.db")
	source.On("Stop").Run(func(mock.Arguments) {
		db, err := walletdb.Open(
			"bdb",
			dbPath,
			true,
			time.Second,
			false,
		)
		require.NoError(t, err)
		require.NoError(t, db.Close())
	}).Return().Once()
	manager, err := wallet.NewManager(t.Context(), wallet.ManagerConfig{
		Backend:           wallet.DBBackendKVDB,
		DataSource:        dbPath,
		ChainParams:       *params,
		ChainSource:       source,
		KVDBPubPassphrase: defaultPubPassphrase,
		Timeout:           time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Stop()) })
	_, err = manager.Start(t.Context())
	require.NoError(t, err)
	_, err = manager.Create(wallet.CreateWalletParams{
		Name:              "lnd",
		Mode:              wallet.ModeImportSeed,
		Seed:              seedBytes,
		PubPassphrase:     defaultPubPassphrase,
		PrivatePassphrase: []byte("test-password"),
	})
	require.NoError(t, err)
	controller := &BtcWallet{
		cfg:   &Config{Manager: manager},
		chain: source,
	}

	// Act through WalletController's ordinary shutdown entry point.
	err = controller.Stop()

	// Assert the database was available at chain shutdown and the owned
	// source received exactly the single Stop arranged above.
	require.NoError(t, err)
	source.AssertExpectations(t)
}

// TestPreviousOutpoints preserves input order and ownership in lnd's
// transaction details using the maintained reader's resolved previous outputs.
func TestPreviousOutpoints(t *testing.T) {
	t.Parallel()

	// Arrange the existing ownership cases as maintained reader snapshots.
	// The expected lnd result keeps every input, including external inputs.
	first := wire.OutPoint{Index: 0}
	second := wire.OutPoint{Index: 1}
	tests := []struct {
		name     string
		previous []wallet.PrevOut
		expected []lnwallet.PreviousOutPoint
	}{
		{
			name: "both outpoints are wallet controlled",
			previous: []wallet.PrevOut{
				{
					OutPoint: wire.OutPoint{Index: 0},
					IsOurs:   true,
				},
				{
					OutPoint: wire.OutPoint{Index: 1},
					IsOurs:   true,
				},
			},
			expected: []lnwallet.PreviousOutPoint{
				{
					OutPoint:    first.String(),
					IsOurOutput: true,
				},
				{
					OutPoint:    second.String(),
					IsOurOutput: true,
				},
			},
		},
		{
			name: "only one outpoint is wallet controlled",
			previous: []wallet.PrevOut{
				{
					OutPoint: wire.OutPoint{Index: 0},
					IsOurs:   true,
				},
				{
					OutPoint: wire.OutPoint{Index: 1},
					IsOurs:   false,
				},
			},
			expected: []lnwallet.PreviousOutPoint{
				{
					OutPoint:    first.String(),
					IsOurOutput: true,
				},
				{
					OutPoint:    second.String(),
					IsOurOutput: false,
				},
			},
		},
		{
			name: "no outpoint is wallet controlled",
			previous: []wallet.PrevOut{
				{
					OutPoint: wire.OutPoint{Index: 0},
					IsOurs:   false,
				},
				{
					OutPoint: wire.OutPoint{Index: 1},
					IsOurs:   false,
				},
			},
			expected: []lnwallet.PreviousOutPoint{
				{
					OutPoint:    first.String(),
					IsOurOutput: false,
				},
				{
					OutPoint:    second.String(),
					IsOurOutput: false,
				},
			},
		},
		{
			name: "tx is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act through the conversion used by both history
			// readers.
			detail := transactionDetail(&wallet.TxDetail{
				PrevOuts: test.previous,
			})

			// Assert all previous outputs survive in their original
			// order with wallet ownership supplied by the
			// maintained snapshot.
			require.Equal(
				t,
				test.expected,
				detail.PreviousOutpoints,
			)
		})
	}
}

// TestTransactionDetailsPage verifies the bounds returned for transaction
// pagination, including requests that would overflow with uint32 addition.
func TestTransactionDetailsPage(t *testing.T) {
	t.Parallel()

	txDetails := []*lnwallet.TransactionDetail{{}, {}, {}, {}}

	testCases := []struct {
		name          string
		offset        uint32
		limit         uint32
		expectedPage  []*lnwallet.TransactionDetail
		expectedFirst uint64
		expectedLast  uint64
	}{
		{
			name:          "zero limit returns remainder",
			offset:        1,
			expectedPage:  txDetails[1:],
			expectedFirst: 1,
			expectedLast:  3,
		},
		{
			name:          "limit selects page",
			offset:        1,
			limit:         2,
			expectedPage:  txDetails[1:3],
			expectedFirst: 1,
			expectedLast:  2,
		},
		{
			name:          "limit exceeds remainder",
			offset:        2,
			limit:         10,
			expectedPage:  txDetails[2:],
			expectedFirst: 2,
			expectedLast:  3,
		},
		{
			name:          "offset plus limit exceeds uint32",
			offset:        1,
			limit:         math.MaxUint32,
			expectedPage:  txDetails[1:],
			expectedFirst: 1,
			expectedLast:  3,
		},
		{
			name:         "offset equals transaction count",
			offset:       uint32(len(txDetails)),
			limit:        1,
			expectedPage: []*lnwallet.TransactionDetail{},
		},
		{
			name:         "maximum offset",
			offset:       math.MaxUint32,
			limit:        1,
			expectedPage: []*lnwallet.TransactionDetail{},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			page, first, last := transactionDetailsPage(
				txDetails, testCase.offset, testCase.limit,
			)

			require.Equal(t, testCase.expectedPage, page)
			require.Equal(t, testCase.expectedFirst, first)
			require.Equal(t, testCase.expectedLast, last)
		})
	}
}

// TestCheckMempoolAcceptance asserts the CheckMempoolAcceptance behaves as
// expected.
func TestCheckMempoolAcceptance(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock chain.Interface.
	mockChain := &lnmock.MockChain{}
	defer mockChain.AssertExpectations(t)

	// Create a test tx and a test max feerate.
	tx := wire.NewMsgTx(2)
	maxFeeRate := float64(0)

	// Create a test wallet.
	wallet := &BtcWallet{
		chain: mockChain,
	}

	// Assert that when the chain backend doesn't support
	// `TestMempoolAccept`, an error is returned.
	//
	// Mock the chain backend to not support `TestMempoolAccept`.
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		nil, rpcclient.ErrBackendVersion).Once()

	err := wallet.CheckMempoolAcceptance(tx)
	rt.ErrorIs(err, rpcclient.ErrBackendVersion)

	// Assert that when the chain backend doesn't implement
	// `TestMempoolAccept`, an error is returned.
	//
	// Mock the chain backend to not support `TestMempoolAccept`.
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		nil, chain.ErrUnimplemented).Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.ErrorIs(err, chain.ErrUnimplemented)

	// Assert that when the returned results are not as expected, an error
	// is returned.
	//
	// Mock the chain backend to return more than one result.
	results := []*btcjson.TestMempoolAcceptResult{
		{Txid: "txid1", Allowed: true},
		{Txid: "txid2", Allowed: false},
	}
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		results, nil).Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.ErrorContains(err, "expected 1 result from TestMempoolAccept")

	// Assert that when the tx is rejected, the reason is converted to an
	// RPC error and returned.
	//
	// Mock the chain backend to return one result.
	results = []*btcjson.TestMempoolAcceptResult{{
		Txid:         tx.TxHash().String(),
		Allowed:      false,
		RejectReason: "insufficient fee",
	}}
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		results, nil).Once()
	mockChain.On("MapRPCErr", mock.Anything).Return(
		chain.ErrInsufficientFee).Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.ErrorIs(err, chain.ErrInsufficientFee)

	// Assert that when the tx is accepted, no error is returned.
	//
	// Mock the chain backend to return one result.
	results = []*btcjson.TestMempoolAcceptResult{
		{Txid: tx.TxHash().String(), Allowed: true},
	}
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		results, nil).Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.NoError(err)
}
