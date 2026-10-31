package lnwallet

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestReservedValueMaturity ensures unavailable rewards cannot replace anchor
// reserves, while mature or ordinary watch-only funds remain remotely usable.
func TestReservedValueMaturity(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name          string
		watchOnly     bool
		spendable     bool
		confirmations int32
		coinbase      bool
		wantError     bool
	}{
		{
			name:          "local mature",
			spendable:     true,
			confirmations: 100,
		},
		{
			name:          "local immature",
			confirmations: 1,
			coinbase:      true,
			wantError:     true,
		},
		{
			name:          "remote mature",
			watchOnly:     true,
			confirmations: 100,
		},
		{
			name:          "remote immature",
			watchOnly:     true,
			confirmations: 1,
			coinbase:      true,
			wantError:     true,
		},
		{
			name:          "remote ordinary",
			watchOnly:     true,
			confirmations: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange one mature coin being spent and one candidate
			// reserve. Only native metadata determines whether the
			// remaining candidate can secure the anchor channel.
			spent := wire.OutPoint{
				Index: 1,
			}
			remaining := wire.OutPoint{
				Index: 2,
			}
			w := &mockWalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			w.On("ListUnspent", mock.Anything, wallet.UtxoQuery{
				Account:  DefaultAccountName,
				MaxConfs: math.MaxInt32,
			}).Return([]*wallet.Utxo{
				{
					OutPoint:  spent,
					Amount:    20_000,
					PkScript:  CoinPkScript,
					Spendable: true,
				},
				{
					OutPoint:      remaining,
					Amount:        AnchorChanReservedValue,
					PkScript:      CoinPkScript,
					Spendable:     tc.spendable,
					Confirmations: tc.confirmations,
				},
			}, nil).Once()
			if !tc.spendable {
				w.On("IsWatchOnly").Return(tc.watchOnly).Once()
			}
			if tc.watchOnly && tc.confirmations < 100 {
				parent := wire.NewMsgTx(2)
				previous := wire.OutPoint{
					Hash: chainhash.Hash{1},
				}
				if tc.coinbase {
					previous = wire.OutPoint{
						Index: math.MaxUint32,
					}
				}
				parent.AddTxIn(wire.NewTxIn(
					&previous, nil, nil,
				))
				var raw bytes.Buffer
				require.NoError(t, parent.Serialize(&raw))
				w.On("GetTx", mock.Anything, remaining.Hash).
					Return(&wallet.TxDetail{
						RawTx: raw.Bytes(),
					}, nil).Once()
			}
			lw := &LightningWallet{
				Cfg: Config{
					Wallet:    w,
					NetParams: chaincfg.MainNetParams,
				},
			}

			// Act through the public reserve check after spending
			// the
			// only independently mature input.
			_, err := lw.CheckReservedValue(
				[]wire.OutPoint{spent}, nil, 1,
			)

			// Assert immature rewards cannot satisfy the reserve;
			// cleanup verifies the exact metadata lookups once.
			if tc.wantError {
				require.ErrorIs(t,
					err, ErrReservedValueInvalidated,
				)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestHandleFundingCounterPartySigsMissingReservation tests the missing
// reservation response.
func TestHandleFundingCounterPartySigsMissingReservation(t *testing.T) {
	t.Parallel()

	wallet := &LightningWallet{
		fundingLimbo: make(map[uint64]*ChannelReservation),
	}
	completeChan := make(chan *chanstate.OpenChannel, 1)
	errChan := make(chan error, 1)

	wallet.handleFundingCounterPartySigs(&addCounterPartySigsMsg{
		pendingFundingID: 1,
		completeChan:     completeChan,
		err:              errChan,
	})

	require.Len(t, completeChan, 1)
	require.Nil(t, <-completeChan)
	require.Len(t, errChan, 1)
	require.ErrorContains(t, <-errChan, "non-existent funding state")
}

// TestRegisterFundingIntent checks RegisterFundingIntent behaves as expected.
func TestRegisterFundingIntent(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a testing wallet.
	lw, err := NewLightningWallet(Config{})
	require.NoError(err)

	// Init an empty testing channel ID.
	var testID [32]byte

	// Call the method with empty ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrEmptyPendingChanID)

	// Modify the ID and call the method again should result in no error.
	testID[0] = 1
	err = lw.RegisterFundingIntent(testID, nil)
	require.NoError(err)

	// Call the method using the same ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrDuplicatePendingChanID)
}
