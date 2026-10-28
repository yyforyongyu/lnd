//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lntest/mock"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestXCreateAccountExperimentalGate checks release and dev admission.
func TestXCreateAccountExperimentalGate(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		ack  bool
	}{
		{
			name: "without acknowledgement",
		},
		{
			name: "explicit acknowledgement",
			ack:  true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange native creation only when the public gate
			// admits it. A strict mock proves rejected release
			// requests do not reach Wallet.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			allowed := tc.ack || build.IsDevBuild()
			scope := waddrmgr.KeyScopeBIP0086
			addrType := AddressType_TAPROOT_PUBKEY
			schema := waddrmgr.ScopeAddrMap[scope]
			if allowed {
				w.On(
					"NewAccount",
					testifymock.Anything,
					wallet.NewAccountParams{
						Scope: waddrmgr.KeyScopeBIP0086,
						Name:  "custom",
					},
				).
					Return(&wallet.AccountInfo{
						AccountName: "custom",
						KeyScope:    scope,
						AddrSchema:  schema,
					}, nil).
					Once()
			}
			server := &WalletKit{cfg: &Config{
				Wallet: w, ChainParams: &chaincfg.MainNetParams,
			}}

			// Act with the same request under the actual build's
			// gate.
			resp, err := server.XCreateAccount(
				t.Context(),
				&XCreateAccountRequest{
					Name:              "custom",
					AddressType:       addrType,
					IKnowWhatIAmDoing: tc.ack,
				},
			)

			// Assert refusal stays explicit while admitted calls
			// return real account metadata, without panic
			// assertions or skipped dev coverage.
			if !allowed {
				require.ErrorIs(
					t,
					err,
					errAccountCreationNotAcked,
				)
				require.Nil(t, resp)

				return
			}
			require.NoError(t, err)
			require.Equal(t, "custom", resp.Account.Name)
		})
	}
}
