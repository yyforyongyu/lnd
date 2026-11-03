//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestLockInputsLifetime preserves explicit owner and native lifetime choices.
func TestLockInputsLifetime(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		noExpiry bool
	}{
		{
			name: "timed",
		},
		{
			name:     "no expiry",
			noExpiry: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange one known output with exact native
			// acquisition calls. The backend result, rather than
			// lnd state, describes lifetime.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			owner := wtxmgr.LockID{9}
			op := wire.OutPoint{Index: 1}
			w.On("GetUtxo", testifymock.Anything, op).
				Return(&wallet.Utxo{OutPoint: op}, nil).Once()
			duration := chanfunding.DefaultLockDuration
			expiry := time.Unix(123, 0)
			var depth uint32
			if tc.noExpiry {
				duration, expiry, depth = 0, time.Time{}, 1
			}
			opts := testifymock.MatchedBy(
				func(opts []wallet.LeaseOption) bool {
					return len(opts) == int(depth)
				},
			)
			w.On(
				"LeaseOutput",
				testifymock.Anything,
				owner,
				op,
				duration,
				opts,
			).
				Return(expiry, nil).
				Once()

			// Act through the funding reservation helper with the
			// caller owner.
			leases, err := lockInputs(
				t.Context(),
				w,
				[]wire.OutPoint{op},
				&owner,
				0,
				depth,
			)

			// Assert native identity/lifetime survives without a
			// wrapper DTO.
			require.NoError(t, err)
			require.Len(t, leases, 1)
			require.Equal(t, owner, leases[0].LockID)
			require.Equal(t, op, leases[0].OutPoint)
			require.Equal(t, expiry, leases[0].Expiration)
			require.Equal(t, tc.noExpiry, leases[0].NoExpiry)
		})
	}
}

// TestLockInputsRollback preserves acquired-owner cleanup and visible failures.
func TestLockInputsRollback(t *testing.T) {
	t.Parallel()
	leaseErr := errors.New("lease failed")
	lookupErr := errors.New("output lookup failed")
	releaseErr := errors.New("release failed")
	customOwner := wtxmgr.LockID{9, 8, 7}
	testCases := []struct {
		name       string
		owner      *wtxmgr.LockID
		noExpiry   bool
		lookupErr  error
		leaseErr   error
		releaseErr error
	}{
		{
			name:     "timed acquisition failure",
			owner:    &customOwner,
			leaseErr: leaseErr,
		},
		{
			name:     "no expiry acquisition failure",
			owner:    &customOwner,
			noExpiry: true,
			leaseErr: leaseErr,
		},
		{
			name:       "cleanup failure remains attributable",
			owner:      &customOwner,
			noExpiry:   true,
			leaseErr:   leaseErr,
			releaseErr: releaseErr,
		},
		{
			name:      "metadata failure with custom owner",
			owner:     &customOwner,
			noExpiry:  true,
			lookupErr: lookupErr,
		},
		{
			name:      "metadata failure with internal owner",
			lookupErr: lookupErr,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange one acquired lease followed by the existing
			// lookup or acquisition failure. Cleanup must use the
			// owner that acquired it.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			first, second := wire.OutPoint{
				Index: 1,
			}, wire.OutPoint{
				Index: 2,
			}
			owner := chanfunding.LndInternalLockID
			if tc.owner != nil {
				owner = *tc.owner
			}
			duration := chanfunding.DefaultLockDuration
			expiry := time.Unix(123, 0)
			var depth uint32
			if tc.noExpiry {
				duration, expiry, depth = 0, time.Time{}, 1
			}
			opts := testifymock.MatchedBy(
				func(opts []wallet.LeaseOption) bool {
					return len(opts) == int(depth)
				},
			)
			w.On("GetUtxo", testifymock.Anything, first).
				Return(&wallet.Utxo{OutPoint: first}, nil).
				Once()
			w.On(
				"LeaseOutput",
				testifymock.Anything,
				owner,
				first,
				duration,
				opts,
			).
				Return(expiry, nil).
				Once()
			w.On("GetUtxo", testifymock.Anything, second).
				Return(
					&wallet.Utxo{OutPoint: second},
					tc.lookupErr,
				).
				Once()
			if tc.lookupErr == nil {
				w.On(
					"LeaseOutput",
					testifymock.Anything,
					owner,
					second,
					duration,
					opts,
				).
					Return(time.Time{}, tc.leaseErr).
					Once()
			}
			w.On(
				"ReleaseOutput",
				testifymock.Anything,
				owner,
				first,
			).
				Return(tc.releaseErr).
				Once()

			// Act through the same partial-acquisition cleanup used
			// by FundPsbt.
			leases, err := lockInputs(
				t.Context(),
				w,
				[]wire.OutPoint{
					first,
					second,
				},
				tc.owner,
				0,
				depth,
			)

			// Assert failure cannot publish a successful result or
			// hide a lease whose cleanup failed; its owner and
			// outpoint remain recoverable.
			require.Nil(t, leases)
			if tc.lookupErr != nil {
				require.ErrorIs(t, err, tc.lookupErr)
			} else {
				require.ErrorIs(t, err, tc.leaseErr)
			}
			if tc.releaseErr != nil {
				require.ErrorIs(t, err, tc.releaseErr)
				require.ErrorContains(t, err, first.String())
				require.ErrorContains(
					t,
					err,
					hex.EncodeToString(owner[:]),
				)
			}
		})
	}
}
