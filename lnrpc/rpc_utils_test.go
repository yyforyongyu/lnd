package lnrpc

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

// TestMarshalUtxosAvailability preserves lease/maturity filtering without
// hiding imported tapscript funds solely because they lack local signing keys.
func TestMarshalUtxosAvailability(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name          string
		locked        bool
		confirmations int32
		coinbase      bool
		visible       bool
	}{
		{
			name:   "leased output",
			locked: true,
		},
		{
			name:          "immature coinbase",
			confirmations: 1,
			coinbase:      true,
		},
		{
			name:          "mature watch-only output",
			confirmations: 100,
			visible:       true,
		},
		{
			name:          "imported tapscript without keys",
			confirmations: 1,
			visible:       true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange native tapscript metadata with no local key.
			// Parent lookup is necessary only when maturity cannot
			// be established from the native snapshot alone.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			script := append(
				[]byte{0x51, 0x20}, make([]byte, 32)...,
			)
			utxo := &wallet.Utxo{
				AddressType:   waddrmgr.TaprootScript,
				PkScript:      script,
				Amount:        123,
				Locked:        tc.locked,
				Confirmations: tc.confirmations,
			}
			if !tc.locked && tc.confirmations < 100 {
				previous := wire.OutPoint{
					Hash: chainhash.Hash{1},
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
				require.NoError(t, parent.Serialize(&raw))
				w.On("GetTx", t.Context(), utxo.OutPoint.Hash).
					Return(&wallet.TxDetail{
						RawTx: raw.Bytes(),
					}, nil).Once()
			}

			// Act through the public formatter used by both RPCs.
			outputs, err := MarshalUtxos(t.Context(), w,
				[]*wallet.Utxo{utxo}, &chaincfg.MainNetParams)

			// Assert unavailable coins disappear but tracked
			// imported
			// P2TR value keeps its existing wire representation.
			require.NoError(t, err)
			if !tc.visible {
				require.Empty(t, outputs)
				return
			}
			require.Len(t, outputs, 1)
			require.Equal(t, AddressType_TAPROOT_PUBKEY,
				outputs[0].AddressType)
			require.EqualValues(t,
				utxo.Amount, outputs[0].AmountSat,
			)
		})
	}
}

// TestPreviousOutpoints preserves input ownership in transaction details
// using the maintained reader's resolved previous outputs.
func TestPreviousOutpoints(t *testing.T) {
	t.Parallel()

	// Arrange the existing ownership cases as maintained reader snapshots.
	// The expected lnd result keeps every input, including external inputs.
	first := wire.OutPoint{Index: 0}
	second := wire.OutPoint{Index: 1}
	tests := []struct {
		name     string
		previous []wallet.PrevOut
		expected []*PreviousOutPoint
	}{
		{
			name: "both outpoints are wallet controlled",
			previous: []wallet.PrevOut{
				{
					OutPoint: wire.OutPoint{
						Index: 0,
					},
					IsOurs: true,
				},
				{
					OutPoint: wire.OutPoint{
						Index: 1,
					},
					IsOurs: true,
				},
			},
			expected: []*PreviousOutPoint{
				{
					Outpoint:    first.String(),
					IsOurOutput: true,
				},
				{
					Outpoint:    second.String(),
					IsOurOutput: true,
				},
			},
		},
		{
			name: "only one outpoint is wallet controlled",
			previous: []wallet.PrevOut{
				{
					OutPoint: wire.OutPoint{
						Index: 0,
					},
					IsOurs: true,
				},
				{
					OutPoint: wire.OutPoint{
						Index: 1,
					},
					IsOurs: false,
				},
			},
			expected: []*PreviousOutPoint{
				{
					Outpoint:    first.String(),
					IsOurOutput: true,
				},
				{
					Outpoint:    second.String(),
					IsOurOutput: false,
				},
			},
		},
		{
			name: "no outpoint is wallet controlled",
			previous: []wallet.PrevOut{
				{
					OutPoint: wire.OutPoint{
						Index: 0,
					},
					IsOurs: false,
				},
				{
					OutPoint: wire.OutPoint{
						Index: 1,
					},
					IsOurs: false,
				},
			},
			expected: []*PreviousOutPoint{
				{
					Outpoint:    first.String(),
					IsOurOutput: false,
				},
				{
					Outpoint:    second.String(),
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
			detail := RPCTransaction(&wallet.TxDetail{
				PrevOuts: test.previous,
			})

			// Assert all previous outputs survive in their original
			// order with wallet ownership supplied by the
			// maintained snapshot.
			if len(test.expected) == 0 {
				require.Empty(t, detail.PreviousOutpoints)
			} else {
				require.Equal(
					t,
					test.expected,
					detail.PreviousOutpoints,
				)
			}
		})
	}
}

// TestTransactionDetailsPage verifies the bounds returned for transaction
// pagination, including requests that would overflow with uint32 addition.
func TestTransactionDetailsPage(t *testing.T) {
	t.Parallel()

	// Arrange native history entries with distinct hashes so public page
	// membership and returned indices can both be checked.
	txDetails := []*wallet.TxDetail{
		{
			Hash: chainhash.Hash{1},
		},
		{
			Hash: chainhash.Hash{2},
		},
		{
			Hash: chainhash.Hash{3},
		},
		{
			Hash: chainhash.Hash{4},
		},
	}

	testCases := []struct {
		name          string
		offset        uint32
		limit         uint32
		expectedPage  []*wallet.TxDetail
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
			expectedPage: []*wallet.TxDetail{},
		},
		{
			name:         "maximum offset",
			offset:       math.MaxUint32,
			limit:        1,
			expectedPage: []*wallet.TxDetail{},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Act through the same native-history formatter used by
			// RPC.
			page := RPCTransactionDetails(
				txDetails,
				testCase.offset,
				testCase.limit,
			)

			// Assert membership and indices without assuming
			// wallet-specific
			// DTOs or reproducing the pagination implementation.
			require.Len(
				t,
				page.Transactions,
				len(testCase.expectedPage),
			)
			for i, expected := range testCase.expectedPage {
				require.Equal(
					t,
					expected.Hash.String(),
					page.Transactions[i].TxHash,
				)
			}
			require.Equal(
				t,
				testCase.expectedFirst,
				page.FirstIndex,
			)
			require.Equal(t, testCase.expectedLast, page.LastIndex)
		})
	}
}
