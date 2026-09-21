package btcwallet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// createAccountWallet exposes only expected account operations. Unused embedded
// capabilities have no implementation, so an accidental extra call fails.
type createAccountWallet struct {
	walletAPI
	mock.Mock
}

// ListAccounts returns the arranged snapshot used for lnd's global name check.
func (w *createAccountWallet) ListAccounts(ctx context.Context) (
	[]base.AccountInfo, error) {

	args := w.Called(ctx)
	accounts, _ := args.Get(0).([]base.AccountInfo)
	return accounts, args.Error(1)
}

// NewAccount verifies that lnd forwards the exact scope and name to btcwallet.
func (w *createAccountWallet) NewAccount(ctx context.Context,
	params base.NewAccountParams) (*base.AccountInfo, error) {

	args := w.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	account, _ := args.Get(0).(*base.AccountInfo)

	return account, args.Error(1)
}

// TestCreateAccount preserves reserved names, cross-scope name uniqueness and
// the requested derivation scope at the maintained account boundary.
func TestCreateAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		accountName string
		keyScope    waddrmgr.KeyScope
		existing    []base.AccountInfo
		expectedErr string
	}{
		{
			name:        "taproot account created",
			accountName: "custom",
			keyScope:    waddrmgr.KeyScopeBIP0086,
		},
		{
			name:        "witness pubkey account created",
			accountName: "custom",
			keyScope:    waddrmgr.KeyScopeBIP0084,
		},
		{
			name:        "empty name rejected",
			keyScope:    waddrmgr.KeyScopeBIP0086,
			expectedErr: "account name is required",
		},
		{
			name:        "default account name reserved",
			accountName: lnwallet.DefaultAccountName,
			keyScope:    waddrmgr.KeyScopeBIP0086,
			expectedErr: "reserved by the wallet",
		},
		{
			name:        "imported account name reserved",
			accountName: waddrmgr.ImportedAddrAccountName,
			keyScope:    waddrmgr.KeyScopeBIP0086,
			expectedErr: "reserved by the wallet",
		},
		{
			name:        "duplicate in requested scope rejected",
			accountName: "custom",
			keyScope:    waddrmgr.KeyScopeBIP0086,
			existing: []base.AccountInfo{
				{
					AccountName: "custom",
					KeyScope:    waddrmgr.KeyScopeBIP0086,
				},
			},
			expectedErr: "already exists",
		},
		{
			name:        "duplicate in other scope rejected",
			accountName: "custom",
			keyScope:    waddrmgr.KeyScopeBIP0086,
			existing: []base.AccountInfo{
				{
					AccountName: "custom",
					KeyScope:    waddrmgr.KeyScopeBIP0084,
				},
			},
			expectedErr: "already exists",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange only calls admitted by the name guard.
			// Successful creation must receive this exact scope and
			// unnumbered request.
			backend := &createAccountWallet{}
			if test.accountName == "custom" {
				backend.On("ListAccounts", mock.Anything).
					Return(test.existing, nil).Once()
			}
			if test.expectedErr == "" {
				request := base.NewAccountParams{
					Scope: test.keyScope,
					Name:  test.accountName,
				}
				backend.On(
					"NewAccount", mock.Anything, request,
				).Return(&base.AccountInfo{
					AccountName: test.accountName,
					KeyScope:    test.keyScope,
				}, nil).Once()
			}
			w := &BtcWallet{wallet: backend}

			// Act through the public adapter, including its shared
			// name lock.
			props, err := w.CreateAccount(
				test.keyScope,
				test.accountName,
			)

			// Assert rejected names produce no account; successful
			// calls retain the requested identity and exhaust all
			// required calls.
			if test.expectedErr != "" {
				require.ErrorContains(t, err, test.expectedErr)
				require.Nil(t, props)
			} else {
				require.NoError(t, err)
				require.Equal(
					t,
					test.accountName,
					props.AccountName,
				)
				require.Equal(t, test.keyScope, props.KeyScope)
			}
			backend.AssertExpectations(t)
		})
	}
}

// TestCreateAccountWalletError preserves backend refusal and unlock errors
// while adding the requested account name to their context.
func TestCreateAccountWalletError(t *testing.T) {
	t.Parallel()
	for _, walletErr := range []error{
		base.ErrAccountOperationUnsupported,
		errors.New("wallet is locked"),
	} {
		t.Run(walletErr.Error(), func(t *testing.T) {
			// Arrange a free name followed by a failing maintained
			// create.
			backend := &createAccountWallet{}
			backend.On("ListAccounts", mock.Anything).
				Return([]base.AccountInfo(nil), nil).Once()
			request := base.NewAccountParams{
				Scope: waddrmgr.KeyScopeBIP0086,
				Name:  "custom",
			}
			backend.On("NewAccount", mock.Anything, request).
				Return(nil, walletErr).Once()
			w := &BtcWallet{wallet: backend}

			// Act by requesting an account through the same public
			// method.
			_, err := w.CreateAccount(
				waddrmgr.KeyScopeBIP0086,
				"custom",
			)

			// Assert callers retain errors.Is matching and useful
			// context.
			require.ErrorIs(t, err, walletErr)
			require.ErrorContains(t, err, "custom")
			backend.AssertExpectations(t)
		})
	}
}

// TestCreateAccountSerialisesCallers keeps simultaneous requests inside one
// name-check/create critical section using the existing account lock.
func TestCreateAccountSerialisesCallers(t *testing.T) {
	t.Parallel()

	// Arrange eight distinct names and measure overlapping backend calls.
	// Pausing the mock widens the same scheduling window as the old
	// fixture.
	const callers = 8
	var active, peak atomic.Int32
	observe := func(mock.Arguments) {
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
	}
	backend := &createAccountWallet{}
	backend.On("ListAccounts", mock.Anything).
		Return([]base.AccountInfo(nil), nil).Times(callers).Run(observe)
	for i := range callers {
		name := fmt.Sprintf("custom-%d", i)
		backend.On("NewAccount", mock.Anything, base.NewAccountParams{
			Scope: waddrmgr.KeyScopeBIP0086,
			Name:  name,
		}).Return(&base.AccountInfo{
			AccountName: name,
		}, nil).Once().Run(observe)
	}
	w := &BtcWallet{wallet: backend}

	// Act concurrently and collect errors for assertions in the test owner.
	var wg sync.WaitGroup
	results := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.CreateAccount(
				waddrmgr.KeyScopeBIP0086,
				fmt.Sprintf("custom-%d", i),
			)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	// Assert each request succeeded and backend calls never overlapped.
	for err := range results {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, peak.Load())
	backend.AssertExpectations(t)
}
