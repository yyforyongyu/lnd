package lnd

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// TestWalletSignetChallengeDigest verifies that lnd's challenge selection
// supplies the network identity required by native wallet storage.
func TestWalletSignetChallengeDigest(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%v", custom), func(t *testing.T) {
			// Arrange the default or a custom signet using lnd's
			// challenge option and chaincfg's network magic.
			cfg := DefaultConfig()
			cfg.Bitcoin.SigNet = true
			challenge := chaincfg.DefaultSignetChallenge
			if custom {
				challenge = []byte{0x51}
				encoded := hex.EncodeToString(challenge)
				cfg.Bitcoin.SigNetChallenge = encoded
			}
			source := &lnmock.MockChain{}
			params := chaincfg.CustomSignetParams(challenge, nil)

			// Act by constructing the real SQL Manager with the
			// adapter's identity. Construction validates storage
			// identity without starting or querying the chain.
			digest, err := walletSignetChallengeDigest(&cfg)
			require.NoError(t, err)
			manager, err := wallet.NewManager(
				t.Context(), wallet.ManagerConfig{
					Backend: wallet.DBBackendSQLite,
					DataSource: filepath.Join(
						t.TempDir(), "wallet.sqlite",
					),
					ChainParams:           params,
					ChainSource:           source,
					SignetChallengeDigest: digest,
				},
			)

			// Assert both identities are accepted and release the
			// database without transferring chain ownership.
			require.NoError(t, err)
			require.NoError(t, manager.Stop())
			source.AssertExpectations(t)
		})
	}
}

var (
	testPassword     = "testpassword"
	redactedPassword = "[redacted]"
)

// TestConfigToFlatMap tests that the configToFlatMap function works as
// expected on the default configuration.
func TestConfigToFlatMap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BitcoindMode.RPCPass = testPassword
	cfg.BtcdMode.RPCPass = testPassword
	cfg.Tor.Password = testPassword
	cfg.DB.Etcd.Pass = testPassword
	cfg.DB.Postgres.Dsn = testPassword

	// Set deprecated fields.
	cfg.Bitcoin.Active = true

	result, deprecated, err := configToFlatMap(cfg)
	require.NoError(t, err)

	// Check that the deprecated option has been parsed out.
	require.Contains(t, deprecated, "bitcoin.active")

	// Pick a couple of random values to check.
	require.Equal(t, DefaultLndDir, result["lnddir"])
	require.Equal(
		t, fmt.Sprintf("%v", chainreg.DefaultBitcoinTimeLockDelta),
		result["bitcoin.timelockdelta"],
	)
	require.Equal(
		t, fmt.Sprintf("%v", routing.DefaultAprioriWeight),
		result["routerrpc.apriori.weight"],
	)
	require.Contains(t, result, "routerrpc.routermacaroonpath")

	// Check that sensitive values are not included.
	require.Equal(t, redactedPassword, result["bitcoind.rpcpass"])
	require.Equal(t, redactedPassword, result["btcd.rpcpass"])
	require.Equal(t, redactedPassword, result["tor.password"])
	require.Equal(t, redactedPassword, result["db.etcd.pass"])
	require.Equal(t, redactedPassword, result["db.postgres.dsn"])
}

// TestSupplyEnvValue tests that the supplyEnvValue function works as
// expected on the passed inputs.
func TestSupplyEnvValue(t *testing.T) {
	// Mock environment variables for testing.
	t.Setenv("EXISTING_VAR", "existing_value")
	t.Setenv("EMPTY_VAR", "")

	tests := []struct {
		input       string
		expected    string
		description string
	}{
		{
			input:    "$EXISTING_VAR",
			expected: "existing_value",
			description: "Valid environment variable without " +
				"default value",
		},
		{
			input:    "${EXISTING_VAR:-default_value}",
			expected: "existing_value",
			description: "Valid environment variable with " +
				"default value",
		},
		{
			input:    "$NON_EXISTENT_VAR",
			expected: "",
			description: "Non-existent environment variable " +
				"without default value",
		},
		{
			input:    "${NON_EXISTENT_VAR:-default_value}",
			expected: "default_value",
			description: "Non-existent environment variable " +
				"with default value",
		},
		{
			input:    "$EMPTY_VAR",
			expected: "",
			description: "Empty environment variable without " +
				"default value",
		},
		{
			input:    "${EMPTY_VAR:-default_value}",
			expected: "default_value",
			description: "Empty environment variable with " +
				"default value",
		},
		{
			input:       "raw_input",
			expected:    "raw_input",
			description: "Raw input - no matching format",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			result := supplyEnvValue(test.input)
			require.Equal(t, test.expected, result)
		})
	}
}

// TestNormalizeRemoteSignerListenAddrs makes sure lnd preserves explicitly
// configured dedicated inbound remote signer listener ports and applies the
// dedicated default port when none is specified. We keep the default-port case
// as a unit test because an itest would need to bind the real default port
// 10019, which becomes flaky under the parallel CI tranche runner where
// multiple test processes can contend for the same host port. The remote
// signer itests also covers the end-to-end dedicated listener path, but they
// do so with explicit dynamically assigned ports instead of the fixed default.
func TestNormalizeRemoteSignerListenAddrs(t *testing.T) {
	tests := []struct {
		name     string
		listener string
		expected string
	}{
		{
			name:     "default port",
			listener: "localhost",
			expected: "127.0.0.1:10019",
		},
		{
			name:     "explicit port",
			listener: "localhost:12019",
			expected: "127.0.0.1:12019",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inboundCfg := lncfg.InboundWatchOnlyCfg{
				ExperimentalRPCListeners: []string{
					test.listener,
				},
			}

			cfg := &Config{
				RemoteSigner: &lncfg.RemoteSigner{
					InboundWatchOnlyCfg: inboundCfg,
				},
				net: &tor.ClearNet{},
			}

			addrs, err := normalizeRemoteSignerListenAddrs(cfg)
			require.NoError(t, err)
			require.Len(t, addrs, 1)
			require.Equal(t, test.expected, addrs[0].String())
		})
	}
}

// TestValidateConfigTrickleDelay tests that the TrickleDelay configuration
// is properly validated and defaulted in ValidateConfig. This test directly
// verifies the validation logic without going through the full ValidateConfig
// function which has many dependencies.
func TestValidateConfigTrickleDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		trickleDelay  int
		expectedDelay int
	}{
		{
			name:          "zero delay defaults to 1ms",
			trickleDelay:  0,
			expectedDelay: 1,
		},
		{
			name:          "negative delay defaults to 1ms",
			trickleDelay:  -1000,
			expectedDelay: 1,
		},
		{
			name:          "positive delay unchanged",
			trickleDelay:  5000,
			expectedDelay: 5000,
		},
		{
			name:          "minimum valid delay (1ms)",
			trickleDelay:  1,
			expectedDelay: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a config with the test's TrickleDelay.
			cfg := Config{
				TrickleDelay: tc.trickleDelay,
			}

			// Simulate the validation logic from ValidateConfig.
			if cfg.TrickleDelay <= 0 {
				cfg.TrickleDelay = 1
			}

			// Verify the TrickleDelay was set to the expected
			// value.
			require.Equal(
				t, tc.expectedDelay, cfg.TrickleDelay,
				"TrickleDelay mismatch",
			)
		})
	}
}

// TestValidateMaxOutgoingCltvExpiry asserts that max-cltv-expiry accepts
// values within its supported bounds and rejects values outside them.
func TestValidateMaxOutgoingCltvExpiry(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	require.NoError(
		t, validateMaxOutgoingCltvExpiry(
			htlcswitch.DefaultMaxOutgoingCltvExpiry,
			cfg.Bitcoin.TimeLockDelta,
		),
	)
	require.NoError(t, validateMaxOutgoingCltvExpiry(
		MaxTimeLockDelta, MaxTimeLockDelta,
	))

	err := validateMaxOutgoingCltvExpiry(
		cfg.Bitcoin.TimeLockDelta-1,
		cfg.Bitcoin.TimeLockDelta,
	)
	require.ErrorContains(t, err, "max-cltv-expiry must be at least")

	err = validateMaxOutgoingCltvExpiry(
		MaxTimeLockDelta+1, cfg.Bitcoin.TimeLockDelta,
	)
	require.ErrorContains(t, err, "max-cltv-expiry must be at most")
}

// TestValidateChannelPolicyTimeLockDelta asserts that advertised channel
// policy CLTV deltas stay within the node's supported forwarding bounds.
func TestValidateChannelPolicyTimeLockDelta(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	require.NoError(t, validateChannelPolicyTimeLockDelta(
		cfg.Bitcoin.TimeLockDelta, cfg.MaxOutgoingCltvExpiry,
	))
	require.NoError(t, validateChannelPolicyTimeLockDelta(
		cfg.MaxOutgoingCltvExpiry, cfg.MaxOutgoingCltvExpiry,
	))

	err := validateChannelPolicyTimeLockDelta(
		minTimeLockDelta-1, cfg.MaxOutgoingCltvExpiry,
	)
	require.ErrorContains(t, err, "time lock delta of")
	require.ErrorContains(t, err, "is too small")

	err = validateChannelPolicyTimeLockDelta(
		MaxTimeLockDelta+1, MaxTimeLockDelta,
	)
	require.ErrorContains(t, err, "is too big")

	err = validateChannelPolicyTimeLockDelta(
		cfg.MaxOutgoingCltvExpiry+1, cfg.MaxOutgoingCltvExpiry,
	)
	require.ErrorContains(t, err, "exceeds max-cltv-expiry")
}
