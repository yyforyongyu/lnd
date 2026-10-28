package lnd

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/stretchr/testify/assert"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestGetAllPermissions(t *testing.T) {
	perms := GetAllPermissions()

	// Currently there are 16 entity:action pairs in use.
	assert.Equal(t, len(perms), 16)
}

// mockDataParser is a mock implementation of the AuxDataParser interface.
type mockDataParser struct {
}

// InlineParseCustomData replaces any custom data binary blob in the given RPC
// message with its corresponding JSON formatted data. This transforms the
// binary (likely TLV encoded) data to a human-readable JSON representation
// (still as byte slice).
func (m *mockDataParser) InlineParseCustomData(msg proto.Message) error {
	switch m := msg.(type) {
	case *lnrpc.ChannelBalanceResponse:
		m.CustomChannelData = []byte(`{"foo": "bar"}`)

		return nil

	default:
		return fmt.Errorf("mock only supports ChannelBalanceResponse")
	}
}

func TestAuxDataParser(t *testing.T) {
	// We create an empty channeldb, so we can fetch some channels.
	cdb := channeldb.OpenForTesting(t, t.TempDir())

	r := &rpcServer{
		server: &server{
			chanStateDB: cdb.ChannelStateDB(),
			implCfg: &ImplementationCfg{
				AuxComponents: AuxComponents{
					AuxDataParser: fn.Some[AuxDataParser](
						&mockDataParser{},
					),
				},
			},
		},
	}

	// With the aux data parser in place, we should get a formatted JSON
	// in the custom channel data field.
	resp, err := r.ChannelBalance(nil, &lnrpc.ChannelBalanceRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, []byte(`{"foo": "bar"}`), resp.CustomChannelData)

	// If we don't supply the aux data parser, we should get the raw binary
	// data. Which in this case is just two VarInt fields (1 byte each) that
	// represent the value of 0 (zero active and zero pending channels).
	r.server.implCfg.AuxComponents.AuxDataParser = fn.None[AuxDataParser]()

	resp, err = r.ChannelBalance(nil, &lnrpc.ChannelBalanceRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, []byte{0x00, 0x00}, resp.CustomChannelData)
}

// TestStopDaemonBeforeRPCStartup makes sure StopDaemon can be called during
// the wallet-unlocked startup window, before addDeps has populated the
// rpcServer's server dependencies and while r.server is still nil. That
// startup state can last for an extended period when lnd is waiting for an
// outbound remote signer to connect before the full RPC server becomes active
// when lnd is configured to use an outbound remote signer.
func TestStopDaemonBeforeRPCStartup(t *testing.T) {
	interceptor, err := signal.Intercept()
	require.NoError(t, err)

	r := &rpcServer{
		interceptor: interceptor,
		server:      nil,
	}

	resp, err := r.StopDaemon(t.Context(), &lnrpc.StopRequest{})
	require.NoError(t, err)
	require.Equal(t, "shutdown initiated, check logs for progress",
		resp.Status)

	select {
	case <-interceptor.ShutdownChannel():

	case <-time.After(time.Second):
		t.Fatal("expected shutdown request to be delivered")
	}
}

// TestRpcCommitmentType tests the rpcCommitmentType returns the corect
// commitment type given a channel type.
func TestRpcCommitmentType(t *testing.T) {
	tests := []struct {
		name     string
		chanType channeldb.ChannelType
		want     lnrpc.CommitmentType
	}{
		{
			name: "tapscript overlay",
			chanType: channeldb.SimpleTaprootFeatureBit |
				channeldb.TapscriptRootBit,
			want: lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY,
		},
		{
			name:     "simple taproot",
			chanType: channeldb.SimpleTaprootFeatureBit,
			want:     lnrpc.CommitmentType_SIMPLE_TAPROOT,
		},
		{
			name:     "lease expiration",
			chanType: channeldb.LeaseExpirationBit,
			want:     lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		},
		{
			name:     "anchors",
			chanType: channeldb.AnchorOutputsBit,
			want:     lnrpc.CommitmentType_ANCHORS,
		},
		{
			name:     "tweakless",
			chanType: channeldb.SingleFunderTweaklessBit,
			want:     lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		},
		{
			name:     "legacy",
			chanType: channeldb.SingleFunderBit,
			want:     lnrpc.CommitmentType_LEGACY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(
				t, tt.want, rpcCommitmentType(tt.chanType),
			)
		})
	}
}

// nativeTxStream records the RPC projection without a second event queue.
type nativeTxStream struct {
	grpc.ServerStream
	testifymock.Mock
}

// Context returns the arranged stream lifetime used for native subscription.
func (s *nativeTxStream) Context() context.Context {
	ctx, _ := s.Called().Get(0).(context.Context)

	return ctx
}

// Send records the public transaction payload delivered by the RPC worker.
func (s *nativeTxStream) Send(tx *lnrpc.Transaction) error {
	return s.Called(tx).Error(0)
}

// TestSubscribeTransactionsNative checks event projection and native closure.
func TestSubscribeTransactionsNative(t *testing.T) {
	t.Parallel()

	// Arrange one queued native event followed by wallet-owned closure.
	// Strict expectations require one subscription cancellation.
	w := &mock.WalletController{}
	t.Cleanup(func() { w.AssertExpectations(t) })
	events := make(chan *wallet.TxEvent, 1)
	events <- &wallet.TxEvent{
		Detail: &wallet.TxDetail{
			Value: 123,
		},
	}
	close(events)
	cancelMock := &testifymock.Mock{}
	cancelMock.On("Cancel").Return().Once()
	t.Cleanup(func() { cancelMock.AssertExpectations(t) })
	cancel := func() { cancelMock.MethodCalled("Cancel") }
	w.On("SubscribeTxns", t.Context()).Return(
		(<-chan *wallet.TxEvent)(events), cancel, nil,
	).Once()
	stream := &nativeTxStream{}
	t.Cleanup(func() { stream.AssertExpectations(t) })
	stream.On("Context").Return(t.Context()).Times(3)
	stream.On(
		"Send",
		testifymock.MatchedBy(func(tx *lnrpc.Transaction) bool {
			return tx.Amount == 123
		}),
	).
		Return(nil).
		Once()
	r := &rpcServer{
		server: &server{
			cc: &chainreg.ChainControl{
				Wallet: &lnwallet.LightningWallet{
					Cfg: lnwallet.Config{
						Wallet: w,
					},
				},
			},
		},
	}

	// Act synchronously; a closed native stream must terminate this RPC.
	err := r.SubscribeTransactions(&lnrpc.GetTransactionsRequest{}, stream)

	// Assert normal closure completes without a transport error; cleanup
	// verifies the payload and the single cancellation at their boundaries.
	require.NoError(t, err)
}

// TestNewAddressNative checks native receiving dispatch for both wire forms.
func TestNewAddressNative(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		addrType lnrpc.AddressType
		scope    waddrmgr.KeyScope
	}{
		{
			addrType: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
			scope:    waddrmgr.KeyScopeBIP0084,
		},
		{
			addrType: lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH,
			scope:    waddrmgr.KeyScopeBIP0084,
		},
		{
			addrType: lnrpc.AddressType_NESTED_PUBKEY_HASH,
			scope:    waddrmgr.KeyScopeBIP0049Plus,
		},
		{
			addrType: lnrpc.AddressType_UNUSED_NESTED_PUBKEY_HASH,
			scope:    waddrmgr.KeyScopeBIP0049Plus,
		},
		{
			addrType: lnrpc.AddressType_TAPROOT_PUBKEY,
			scope:    waddrmgr.KeyScopeBIP0086,
		},
		{
			addrType: lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY,
			scope:    waddrmgr.KeyScopeBIP0086,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.addrType.String(), func(t *testing.T) {
			// Arrange the exact scoped selector; Wallet owns unused
			// child reuse.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			addr, err := address.NewAddressWitnessPubKeyHash(
				make([]byte, 20), &chaincfg.MainNetParams,
			)
			require.NoError(t, err)
			w.On("NewAddress", t.Context(),
				wallet.NewAccountSelectorByName(
					tc.scope, "custom",
				),
				false).
				Return(wallet.AddressInfo{Addr: addr}, nil).
				Once()
			lw := &lnwallet.LightningWallet{
				Cfg: lnwallet.Config{
					Wallet: w,
				},
			}
			r := &rpcServer{
				server: &server{
					cc: &chainreg.ChainControl{
						Wallet: lw,
					},
				},
			}

			// Act through the public RPC with the caller's
			// requested account.
			resp, err := r.NewAddress(
				t.Context(),
				&lnrpc.NewAddressRequest{
					Type:    tc.addrType,
					Account: "custom",
				},
			)

			// Assert the native address passes through without a
			// second allocator.
			require.NoError(t, err)
			require.Equal(t, addr.String(), resp.Address)
		})
	}
}

// TestWalletBalanceNative retains empty accounts and excludes leases/immature
// rewards while counting tracked imported funds without local signing custody.
func TestWalletBalanceNative(t *testing.T) {
	t.Parallel()
	for _, accountFilter := range []string{"", "missing"} {
		t.Run("account "+accountFilter, func(t *testing.T) {
			// Arrange native account/UTXO snapshots. Parent reads
			// separate immature rewards from imported funds,
			// keeping
			// key custody distinct from output maturity.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			scope84 := waddrmgr.KeyScopeBIP0084
			scope86 := waddrmgr.KeyScopeBIP0086
			var utxos []*wallet.Utxo
			if accountFilter == "missing" {
				w.On("ListAccountsByName",
					t.Context(), accountFilter,
				).Return([]wallet.AccountInfo{}, nil).Once()
			} else {
				w.On("ListAccounts", t.Context()).Return(
					[]wallet.AccountInfo{
						{
							AccountName: "default",
							KeyScope:    scope84,
						},
						{
							AccountName: "default",
							KeyScope:    scope86,
						},
						{
							AccountName: "empty",
						},
					}, nil,
				).Once()
				utxos = []*wallet.Utxo{
					{
						Account:       "default",
						Amount:        100,
						Spendable:     true,
						Confirmations: 2,
					},
					{
						Account:       "default",
						Amount:        50,
						Locked:        true,
						Confirmations: 2,
					},
					{
						Account:       "default",
						Amount:        1000,
						Confirmations: 1,
						OutPoint: wire.OutPoint{
							Hash: chainhash.Hash{1},
						},
					},
					{
						Account:       "imported",
						Amount:        25,
						Confirmations: 1,
						OutPoint: wire.OutPoint{
							Hash: chainhash.Hash{2},
						},
					},
					{
						Account:   "default",
						Amount:    5,
						Spendable: true,
					},
				}
				for index, utxo := range utxos[2:4] {
					previous := wire.OutPoint{
						Index: math.MaxUint32,
					}
					if index == 1 {
						previous.Hash[0] = 1
					}
					parent := wire.NewMsgTx(2)
					parent.AddTxIn(wire.NewTxIn(
						&previous, nil, nil,
					))
					var raw bytes.Buffer
					require.NoError(t,
						parent.Serialize(&raw),
					)
					w.On("GetTx", t.Context(),
						utxo.OutPoint.Hash,
					).
						Return(&wallet.TxDetail{
							RawTx: raw.Bytes(),
						}, nil).Once()
				}
			}
			w.On("ListUnspent", t.Context(), wallet.UtxoQuery{
				Account:  accountFilter,
				MaxConfs: math.MaxInt32,
			}).Return(utxos, nil).Once()
			db := channeldb.OpenForTesting(t, t.TempDir())
			net := chainreg.BitcoinMainNetParams
			lw := &lnwallet.LightningWallet{
				Cfg: lnwallet.Config{
					Wallet:   w,
					Database: db.ChannelStateDB(),
				},
			}
			r := &rpcServer{
				cfg: &Config{
					ActiveNetParams: net,
				},
				server: &server{
					cc: &chainreg.ChainControl{
						Wallet: lw,
					},
				},
			}

			// Act through the public RPC, including the requested
			// account filter rather than private aggregation
			// helpers.
			resp, err := r.WalletBalance(t.Context(),
				&lnrpc.WalletBalanceRequest{
					Account: accountFilter,
				})

			// Assert unknown names fail, empty accounts survive,
			// leases stay disjoint and imported value stays
			// visible.
			if accountFilter != "" {
				require.ErrorIs(t,
					err, wallet.ErrAccountNotFound,
				)
				require.Nil(t, resp)

				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 130, resp.TotalBalance)
			require.EqualValues(t, 125, resp.ConfirmedBalance)
			require.EqualValues(t, 5, resp.UnconfirmedBalance)
			require.EqualValues(t, 50, resp.LockedBalance)
			require.Contains(t, resp.AccountBalance, "empty")
			require.Zero(t,
				resp.AccountBalance["empty"].ConfirmedBalance)
			imported := resp.AccountBalance["imported"]
			require.EqualValues(t, 25, imported.ConfirmedBalance)
		})
	}
}

// TestGetTransactionsAccount filters received funds and owned input spends
// using native account addresses, excluding unrelated wallet transactions.
func TestGetTransactionsAccount(t *testing.T) {
	t.Parallel()

	// Arrange three native history entries: receive, spend and unrelated.
	// The spend resolves its owned previous output from maintained history.
	w := &mock.WalletController{}
	schema := waddrmgr.ScopeAddrMap[waddrmgr.KeyScopeBIP0084]
	t.Cleanup(func() { w.AssertExpectations(t) })
	addr, err := address.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.MainNetParams,
	)
	require.NoError(t, err)
	receive := &wallet.TxDetail{
		Hash: chainhash.Hash{1},
		Outputs: []wallet.Output{
			{
				Addresses: []address.Address{addr},
			},
		},
	}
	spend := &wallet.TxDetail{
		Hash: chainhash.Hash{2},
		PrevOuts: []wallet.PrevOut{
			{
				IsOurs: true,
				OutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{4},
					Index: 2,
				},
			},
		},
	}
	unrelated := &wallet.TxDetail{
		Hash: chainhash.Hash{3},
	}
	w.On("ListTxns", t.Context(), int32(0), int32(-1)).Return(
		[]*wallet.TxDetail{receive, spend, unrelated}, nil,
	).Once()
	w.On("ListAccountsByName", t.Context(), "custom").Return(
		[]wallet.AccountInfo{
			{
				KeyScope:   waddrmgr.KeyScopeBIP0084,
				AddrSchema: schema,
			},
		}, nil,
	).Once()
	w.On("ListAddresses", t.Context(), "custom", waddrmgr.WitnessPubKey).
		Return([]wallet.AddressProperty{
			{
				Address: addr,
			},
		}, nil).Once()
	w.On("GetTx", t.Context(), chainhash.Hash{4}).Return(
		&wallet.TxDetail{
			Outputs: []wallet.Output{
				{
					Index:     2,
					Addresses: []address.Address{addr},
				},
			},
		}, nil,
	).Once()
	r := &rpcServer{
		server: &server{
			cc: &chainreg.ChainControl{
				Wallet: &lnwallet.LightningWallet{
					Cfg: lnwallet.Config{
						Wallet: w,
					},
				},
			},
		},
	}

	// Act through the public account-filtered history query.
	resp, err := r.GetTransactions(t.Context(),
		&lnrpc.GetTransactionsRequest{
			Account: "custom",
		},
	)

	// Assert both directions of account activity remain, without matching
	// unrelated activity merely because it belongs to this wallet.
	require.NoError(t, err)
	require.Len(t, resp.Transactions, 2)
	require.ElementsMatch(t,
		[]string{receive.Hash.String(), spend.Hash.String()},
		[]string{
			resp.Transactions[0].TxHash,
			resp.Transactions[1].TxHash,
		},
	)
}

// TestEstimateFeeMaturity quotes only immediately usable local or remote funds,
// so immature rewards cannot produce a successful payment fee estimate.
func TestEstimateFeeMaturity(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name      string
		watchOnly bool
		coinbase  bool
		spendable bool
		wantError bool
	}{
		{
			name:      "local usable",
			spendable: true,
		},
		{
			name:      "local immature",
			coinbase:  true,
			wantError: true,
		},
		{
			name:      "remote usable",
			watchOnly: true,
		},
		{
			name:      "remote immature",
			watchOnly: true,
			coinbase:  true,
			wantError: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange one native candidate and public change-key
			// metadata; quotation must never allocate an address.
			w := &mock.WalletController{}
			scope := waddrmgr.KeyScopeBIP0086
			schema := waddrmgr.ScopeAddrMap[scope]
			name := lnwallet.DefaultAccountName
			t.Cleanup(func() { w.AssertExpectations(t) })
			utxo := &wallet.Utxo{
				OutPoint: wire.OutPoint{
					Hash: chainhash.Hash{1},
				},
				Amount:        100_000,
				PkScript:      mock.CoinPkScript,
				Confirmations: 1,
				Spendable:     tc.spendable,
			}
			w.On("ListUnspent", t.Context(), wallet.UtxoQuery{
				Account:  lnwallet.DefaultAccountName,
				MinConfs: 1,
				MaxConfs: math.MaxInt32,
			}).Return([]*wallet.Utxo{utxo}, nil).Once()
			if !tc.spendable {
				w.On("IsWatchOnly").Return(tc.watchOnly).Once()
			}
			if tc.watchOnly {
				previous := wire.OutPoint{
					Hash: chainhash.Hash{2},
				}
				if tc.coinbase {
					previous = wire.OutPoint{
						Index: math.MaxUint32,
					}
				}
				parent := wire.NewMsgTx(2)
				parent.AddTxIn(wire.NewTxIn(
					&previous, nil, nil,
				))
				var raw bytes.Buffer
				require.NoError(t,
					parent.Serialize(&raw),
				)
				w.On("GetTx", t.Context(), utxo.OutPoint.Hash).
					Return(&wallet.TxDetail{
						RawTx: raw.Bytes(),
					}, nil).Once()
			}
			_, pubKey := btcec.PrivKeyFromBytes([]byte{1})
			w.On("GetAccount", t.Context(), scope, name).
				Return(&wallet.AccountInfo{
					AccountName: name,
					KeyScope:    waddrmgr.KeyScopeBIP0086,
					AddrSchema:  schema,
				}, nil).Once()
			selector := wallet.NewAccountSelectorByName(scope, name)
			w.On("DerivePubKey", t.Context(),
				wallet.DerivePubKeyParams{
					Account: selector,
					Branch:  1,
				},
			).Return(pubKey, nil).Once()
			addr, err := address.NewAddressWitnessPubKeyHash(
				make([]byte, 20), &chaincfg.MainNetParams,
			)
			require.NoError(t, err)
			feeEstimator := chainfee.NewStaticEstimator(1000, 253)
			partial := &chainreg.PartialChainControl{
				FeeEstimator: feeEstimator,
			}
			net := chainreg.BitcoinMainNetParams
			lw := &lnwallet.LightningWallet{
				Cfg: lnwallet.Config{
					Wallet: w,
				},
			}
			r := &rpcServer{
				cfg: &Config{
					ActiveNetParams: net,
				},
				server: &server{
					cc: &chainreg.ChainControl{
						PartialChainControl: partial,
						Wallet:              lw,
					},
				},
			}

			// Act through the onchain:read RPC with the same
			// confirmation policy for local and remote custody.
			resp, err := r.EstimateFee(t.Context(),
				&lnrpc.EstimateFeeRequest{
					AddrToAmount: map[string]int64{
						addr.String(): 10_000,
					},
					TargetConf: 6,
					MinConfs:   1,
				},
			)

			// Assert missing mature funds cannot quote a payment;
			// a usable remote output behaves like a local one.
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, resp)
			} else {
				require.NoError(t, err)
				require.Positive(t, resp.FeeSat)
			}
		})
	}
}
