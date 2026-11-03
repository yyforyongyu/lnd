package lncfg_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	bwmock "github.com/btcsuite/btcwallet/bwtest/mock"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestDBDefaultConfig tests that the default DB config is created as expected.
func TestDBDefaultConfig(t *testing.T) {
	defaultConfig := lncfg.DefaultDB()

	require.Equal(t, lncfg.BoltBackend, defaultConfig.Backend)
	require.Equal(
		t, kvdb.DefaultBoltAutoCompactMinAge,
		defaultConfig.Bolt.AutoCompactMinAge,
	)
	require.Equal(t, kvdb.DefaultDBTimeout, defaultConfig.Bolt.DBTimeout)
	// Implicitly, the following fields are default to false.
	require.False(t, defaultConfig.Bolt.AutoCompact)
	require.True(t, defaultConfig.Bolt.NoFreelistSync)
}

// TestDBSQLWalletBackendSelection verifies that fresh SQL nodes select native
// storage while upgrades reopen the same wallet through the managed KVDB API.
func TestDBSQLWalletBackendSelection(t *testing.T) {
	// Arrange both storage histories for each SQL driver. Tagged runs
	// exercise their enabled driver; default builds need no SQL fixture.
	testCases := []struct {
		name           string
		backend        string
		available      bool
		existingWallet bool
	}{
		{
			name:      "sqlite/fresh wallet",
			backend:   lncfg.SqliteBackend,
			available: kvdb.SqliteBackend,
		},
		{
			name:           "sqlite/existing wallet",
			backend:        lncfg.SqliteBackend,
			available:      kvdb.SqliteBackend,
			existingWallet: true,
		},
		{
			name:      "postgres/fresh wallet",
			backend:   lncfg.PostgresBackend,
			available: kvdb.PostgresBackend,
		},
		{
			name:           "postgres/existing wallet",
			backend:        lncfg.PostgresBackend,
			available:      kvdb.PostgresBackend,
			existingWallet: true,
		},
	}

	for _, tc := range testCases {
		if !tc.available {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			// Arrange isolated real storage at the same
			// namespace used by an existing lnd node. A
			// maintained Manager creates the occupied case,
			// avoiding a fabricated wallet-existence marker.
			chanDir := t.TempDir()
			walletDir := t.TempDir()
			cfg := lncfg.DefaultDB()
			cfg.Backend = tc.backend

			// Use a real isolated server for PostgreSQL so
			// both drivers exercise their storage namespace.
			if tc.backend == lncfg.PostgresBackend {
				fixture := sqldb.NewTestPgFixture(
					t, sqldb.DefaultPostgresFixtureLifetime,
				)
				t.Cleanup(func() {
					fixture.TearDown(t)
				})
				cfg.Postgres.Dsn = fixture.
					GetConfig("test").Dsn
			}

			// Initialize pools before either wallet owner
			// opens the configured SQL namespace.
			require.NoError(t, cfg.Init(t.Context(), chanDir))

			// Open the pre-cutover namespace directly so the
			// fixture cannot select fresh native storage.
			legacyOpen := func(ctx context.Context) (
				kvdb.Backend, error) {

				if tc.backend == lncfg.PostgresBackend {
					pgCfg := lncfg.GetPostgresConfigKVDB(
						cfg.Postgres,
					)
					pgCfg.WithGlobalLock = cfg.Postgres.
						WalletDBWithGlobalLock

					return kvdb.Open(
						kvdb.PostgresBackendName, ctx,
						pgCfg, lncfg.NSWalletDB,
					)
				}

				return kvdb.Open(
					kvdb.SqliteBackendName, ctx,
					lncfg.GetSqliteConfigKVDB(cfg.Sqlite),
					walletDir, lncfg.SqliteChainDBName,
					lncfg.NSWalletDB,
				)
			}

			// A nondefault public password proves selection
			// can inspect storage before credentials arrive.
			params := chaincfg.RegressionNetParams
			pubPass := []byte("existing-public-password")
			privPass := []byte("private-password")
			birthday := params.GenesisBlock.Header.Timestamp

			// Retain the original account identity and chain
			// fixture for the selected Manager to reopen.
			var originalAccounts []wallet.AccountInfo
			var chain *bwmock.Chain
			if tc.existingWallet {
				// Unsynced genesis parks recovery until Stop;
				// fixture cleanup checks optional startup.
				chain = bwmock.NewGenesisChain(t, false)

				// Bind creation to the old SQL namespace
				// through the maintained KVDB adapter.
				creatorConfig := wallet.ManagerConfig{
					Backend:           wallet.DBBackendKVDB,
					KVDBOpen:          legacyOpen,
					ChainParams:       params,
					ChainSource:       chain,
					KVDBPubPassphrase: pubPass,
				}
				creator, err := wallet.NewManager(
					t.Context(), creatorConfig,
				)
				require.NoError(t, err)

				// Register shutdown before startup can fail
				// and leave a live storage owner behind.
				t.Cleanup(func() {
					require.NoError(t, creator.Stop())
				})

				// Start the Manager before asking it to
				// create a wallet in the existing source.
				_, err = creator.Start(t.Context())
				require.NoError(t, err)

				// Generate real keys so a new native wallet
				// cannot satisfy the identity assertion.
				createParams := wallet.CreateWalletParams{
					Name:              "lnd",
					Mode:              wallet.ModeGenSeed,
					PubPassphrase:     pubPass,
					PrivatePassphrase: privPass,
					Birthday:          birthday,
				}
				created, err := creator.Create(createParams)
				require.NoError(t, err)

				// Save account XPubs and paths before the
				// creating owner releases its connection.
				originalAccounts, err = created.ListAccounts(
					t.Context(),
				)
				require.NoError(t, err)
				require.NotEmpty(t, originalAccounts)
				require.NotEmpty(
					t, originalAccounts[0].PublicKey,
				)

				// Drain the old owner before startup probes
				// or the next Manager claim this source.
				require.NoError(t, creator.Stop())
			}

			// Act through lnd's startup configuration, after
			// the old owner has released its wallet connection.
			backends, err := cfg.GetBackends(
				t.Context(), chanDir, walletDir,
				t.TempDir(), false, false, btclog.Disabled,
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				for _, closeDB := range backends.CloseFuncs {
					require.NoError(t, closeDB())
				}
			})

			// Assert empty storage keeps the existing native
			// creation contract and its configured location.
			selected := backends.WalletDB
			if !tc.existingWallet {
				require.Nil(t, selected.KVDBOpen)
				if tc.backend == lncfg.PostgresBackend {
					require.Equal(
						t, wallet.DBBackendPostgres,
						selected.Backend,
					)
					require.Equal(
						t, cfg.Postgres.Dsn,
						selected.DataSource,
					)
				} else {
					require.Equal(
						t, wallet.DBBackendSQLite,
						selected.Backend,
					)
					require.Equal(
						t, filepath.Join(
							walletDir,
							"wallet.sqlite",
						), selected.DataSource,
					)
				}

				return
			}

			// Assert occupied storage selects its existing
			// managed source instead of a fresh native wallet.
			require.Equal(
				t, wallet.DBBackendKVDB, selected.Backend,
			)
			require.NotNil(t, selected.KVDBOpen)

			// Act on the occupied handoff using the same public
			// credential after the pre-password existence probe.
			selected.ChainParams = params
			selected.ChainSource = chain
			selected.KVDBPubPassphrase = pubPass
			manager, err := wallet.NewManager(
				t.Context(), selected,
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, manager.Stop())
			})
			wallets, err := manager.Start(t.Context())
			require.NoError(t, err)
			require.Len(t, wallets, 1)
			accounts, err := wallets[0].ListAccounts(
				t.Context(),
			)

			// Assert reopening retains the original account XPubs
			// and paths instead of creating unrelated keys.
			require.NoError(t, err)
			require.ElementsMatch(t, originalAccounts, accounts)
		})
	}
}

// TestDBWalletBackendSelection verifies that the default channel backend gives
// fresh wallets native SQL storage while preserving existing wallet sources.
func TestDBWalletBackendSelection(t *testing.T) {
	// Arrange both supported handoffs: native storage for an empty wallet
	// directory and the existing KVDB source pending a migration decision.
	testCases := []struct {
		name           string
		existingWallet bool
		backend        wallet.DBBackend
		fileName       string
	}{
		{
			name:     "fresh wallet",
			backend:  wallet.DBBackendSQLite,
			fileName: "wallet.sqlite",
		},
		{
			name:           "existing wallet",
			existingWallet: true,
			backend:        wallet.DBBackendKVDB,
			fileName:       lncfg.WalletDBName,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange real directories so opening channel databases
			// cannot stand in for creating a wallet. The marker
			// proves source preservation, not wallet parsing.
			chanDir := t.TempDir()
			walletDir := t.TempDir()
			walletPath := filepath.Join(
				walletDir, lncfg.WalletDBName,
			)
			original := []byte("existing wallet source")
			if tc.existingWallet {
				require.NoError(t, os.WriteFile(
					walletPath, original, 0600,
				))
			}
			cfg := lncfg.DefaultDB()

			// Act through the public startup handoff. GetBackends
			// opens other DBs; Manager opens the selected wallet
			// later, once its passphrase is available.
			backends, err := cfg.GetBackends(
				t.Context(), chanDir, walletDir,
				t.TempDir(), false, false, btclog.Disabled,
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				for _, closeDB := range backends.CloseFuncs {
					require.NoError(t, closeDB())
				}
			})

			// Assert startup selects the intended storage identity
			// and retains the existing options and source bytes
			// when a wallet is already present.
			require.Equal(t, tc.backend, backends.WalletDB.Backend)
			require.Equal(
				t, filepath.Join(walletDir, tc.fileName),
				backends.WalletDB.DataSource,
			)
			if tc.existingWallet {
				require.Equal(
					t, cfg.Bolt.NoFreelistSync,
					backends.WalletDB.NoFreelistSync,
				)
				require.Equal(
					t, cfg.Bolt.DBTimeout,
					backends.WalletDB.Timeout,
				)
				actual, err := os.ReadFile(walletPath)
				require.NoError(t, err)
				require.Equal(t, original, actual)
			}
		})
	}
}
