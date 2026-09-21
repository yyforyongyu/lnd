package lncfg

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/sqldb"
)

const (
	ChannelDBName      = "channel.db"
	MacaroonDBName     = "macaroons.db"
	DecayedLogDbName   = "sphinxreplay.db"
	TowerClientDBName  = "wtclient.db"
	TowerServerDBName  = "watchtower.db"
	WalletDBName       = "wallet.db"
	WalletSQLiteDBName = "wallet.sqlite"
	NeutrinoDBName     = "neutrino.db"

	SqliteChannelDBName  = "channel.sqlite"
	SqliteChainDBName    = "chain.sqlite"
	SqliteNeutrinoDBName = "neutrino.sqlite"
	SqliteTowerDBName    = "watchtower.sqlite"
	SqliteNativeDBName   = "lnd.sqlite"

	BoltBackend                = "bolt"
	EtcdBackend                = "etcd"
	PostgresBackend            = "postgres"
	SqliteBackend              = "sqlite"
	DefaultBatchCommitInterval = 500 * time.Millisecond

	defaultPostgresMaxConnections = 50

	// NSChannelDB is the namespace name that we use for the combined graph
	// and channel state DB.
	NSChannelDB = "channeldb"

	// NSMacaroonDB is the namespace name that we use for the macaroon DB.
	NSMacaroonDB = "macaroondb"

	// NSDecayedLogDB is the namespace name that we use for the sphinx
	// replay a.k.a. decayed log DB.
	NSDecayedLogDB = "decayedlogdb"

	// NSTowerClientDB is the namespace name that we use for the watchtower
	// client DB.
	NSTowerClientDB = "towerclientdb"

	// NSTowerServerDB is the namespace name that we use for the watchtower
	// server DB.
	NSTowerServerDB = "towerserverdb"

	// NSWalletDB is the namespace name that we use for the wallet DB.
	NSWalletDB = "walletdb"

	// NSNeutrinoDB is the namespace name that we use for the neutrino DB.
	NSNeutrinoDB = "neutrinodb"
)

// DB holds database configuration for LND.
//
//nolint:ll
type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	BatchCommitInterval time.Duration `long:"batch-commit-interval" description:"The maximum duration the channel graph batch schedulers will wait before attempting to commit a batch of pending updates. This can be tradeoff database contenion for commit latency."`

	Etcd *etcd.Config `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt *kvdb.BoltConfig `group:"bolt" namespace:"bolt" description:"Bolt settings."`

	Postgres *sqldb.PostgresConfig `group:"postgres" namespace:"postgres" description:"Postgres settings."`

	Sqlite *sqldb.SqliteConfig `group:"sqlite" namespace:"sqlite" description:"Sqlite settings."`

	UseNativeSQL bool `long:"use-native-sql" description:"If set to true, native SQL will be used instead of KV emulation for tables that support it. Subsystems which support native SQL tables: Invoices, Graph."`

	SkipNativeSQLMigration bool `long:"skip-native-sql-migration" description:"If set to true, the KV to native SQL migration will be skipped. Note that this option is intended for users who experience non-resolvable migration errors. Enabling after there is a non-resolvable migration error that resulted in an incomplete migration will cause that partial migration to be abandoned and ignored and an empty database will be used instead. Since invoices are currently the only native SQL database used, our channels will still work but the invoice history will be forgotten. This option has no effect if native SQL is not in use (db.use-native-sql=false)."`

	NoGraphCache bool `long:"no-graph-cache" description:"Don't use the in-memory graph cache for path finding. Much slower but uses less RAM. Can only be used with a bolt database backend."`

	SyncGraphCacheLoad bool `long:"sync-graph-cache-load" description:"Force synchronous loading of the graph cache. This will block the startup until the graph cache is fully loaded into memory. This is useful if any bugs appear with the new async loading feature of the graph cache."`

	PruneRevocation bool `long:"prune-revocation" description:"Run the optional migration that prunes the revocation logs to save disk space."`

	NoRevLogAmtData bool `long:"no-rev-log-amt-data" description:"If set, the to-local and to-remote output amounts of revoked commitment transactions will not be stored in the revocation log. Note that once this data is lost, a watchtower client will not be able to back up the revoked state."`

	NoGcDecayedLog bool `long:"no-gc-decayed-log" description:"Do not run the optional migration that garbage collects the decayed log to save disk space."`
}

// DefaultDB creates and returns a new default DB config.
func DefaultDB() *DB {
	return &DB{
		Backend:             BoltBackend,
		BatchCommitInterval: DefaultBatchCommitInterval,
		Bolt: &kvdb.BoltConfig{
			NoFreelistSync:    true,
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
		},
		Etcd: &etcd.Config{
			// Allow at most 32 MiB messages by default.
			MaxMsgSize: 32768 * 1024,
		},
		Postgres: &sqldb.PostgresConfig{
			MaxConnections: defaultPostgresMaxConnections,
			// Normally we don't use a global lock for channeldb
			// access, but if a user encounters huge concurrency
			// issues, they can enable this to use a global lock.
			ChannelDBWithGlobalLock: false,
			// Default to true to maintain safe single-writer
			// behavior until the wallet subsystem is upgraded to
			// a native sql schema.
			WalletDBWithGlobalLock: true,
			QueryConfig:            *sqldb.DefaultPostgresConfig(),
		},
		Sqlite: &sqldb.SqliteConfig{
			MaxConnections: sqldb.DefaultSqliteMaxConns,
			BusyTimeout:    sqldb.DefaultSqliteBusyTimeout,
			QueryConfig:    *sqldb.DefaultSQLiteConfig(),
		},
		UseNativeSQL:           false,
		SkipNativeSQLMigration: false,
	}
}

// Validate validates the DB config.
func (db *DB) Validate() error {
	switch db.Backend {
	case BoltBackend:
		if db.UseNativeSQL {
			return fmt.Errorf("cannot use native SQL with bolt " +
				"backend")
		}

	case SqliteBackend:
		if err := db.Sqlite.Validate(); err != nil {
			return err
		}
	case PostgresBackend:
		if err := db.Postgres.Validate(); err != nil {
			return err
		}

	case EtcdBackend:
		if db.UseNativeSQL {
			return fmt.Errorf("cannot use native SQL with etcd " +
				"backend")
		}

		if !db.Etcd.Embedded && db.Etcd.Host == "" {
			return fmt.Errorf("etcd host must be set")
		}

	default:
		return fmt.Errorf("unknown backend, must be either '%v', "+
			"'%v', '%v' or '%v'", BoltBackend, EtcdBackend,
			PostgresBackend, SqliteBackend)
	}

	// The path finding uses a manual read transaction that's open for a
	// potentially long time. That works fine with the locking model of
	// bbolt but can lead to locks or rolled back transactions with etcd or
	// postgres. And since we already have a smaller memory footprint for
	// remote database setups (due to not needing to memory-map the bbolt DB
	// files), we can keep the graph in memory instead. But for mobile
	// devices the tradeoff between a smaller memory footprint and the
	// longer time needed for path finding might be a desirable one.
	if db.NoGraphCache && db.Backend != BoltBackend {
		return fmt.Errorf("cannot use no-graph-cache with database "+
			"backend '%v'", db.Backend)
	}

	return nil
}

// Init should be called upon start to pre-initialize database access dependent
// on configuration.
func (db *DB) Init(ctx context.Context, dbPath string) error {
	// Start embedded etcd server if requested.
	switch {
	case db.Backend == EtcdBackend && db.Etcd.Embedded:
		cfg, _, err := kvdb.StartEtcdTestBackend(
			dbPath, db.Etcd.EmbeddedClientPort,
			db.Etcd.EmbeddedPeerPort, db.Etcd.EmbeddedLogFile,
		)
		if err != nil {
			return err
		}

		// Override the original config with the config for
		// the embedded instance.
		db.Etcd = cfg

	case db.Backend == PostgresBackend:
		sqlbase.Init(db.Postgres.MaxConnections)

	case db.Backend == SqliteBackend:
		sqlbase.Init(db.Sqlite.MaxConns())
	}

	return nil
}

// DatabaseBackends is a two-tuple that holds the set of active database
// backends for the daemon. The two backends we expose are the graph database
// backend, and the channel state backend.
type DatabaseBackends struct {
	// GraphDB points to the database backend that contains the less
	// critical data that is accessed often, such as the channel graph and
	// chain height hints.
	GraphDB kvdb.Backend

	// ChanStateDB points to a possibly networked replicated backend that
	// contains the critical channel state related data.
	ChanStateDB kvdb.Backend

	// HeightHintDB points to a possibly networked replicated backend that
	// contains the chain height hint related data.
	HeightHintDB kvdb.Backend

	// MacaroonDB points to a database backend that stores the macaroon root
	// keys.
	MacaroonDB kvdb.Backend

	// DecayedLogDB points to a database backend that stores the decayed log
	// data.
	DecayedLogDB kvdb.Backend

	// TowerClientDB points to a database backend that stores the watchtower
	// client data. This might be nil if the watchtower client is disabled.
	TowerClientDB kvdb.Backend

	// TowerServerDB points to a database backend that stores the watchtower
	// server data. This might be nil if the watchtower server is disabled.
	TowerServerDB kvdb.Backend

	// WalletDB locates the wallet database owned by its Manager.
	WalletDB wallet.ManagerConfig

	// NativeSQLStore holds a reference to the native SQL store that can
	// be used for native SQL queries for tables that already support it.
	// This may be nil if the use-native-sql flag was not set.
	NativeSQLStore sqldb.DB

	// Remote indicates whether the database backends are remote, possibly
	// replicated instances or local bbolt or sqlite backed databases.
	Remote bool

	// CloseFuncs is a map of close functions for each of the initialized
	// DB backends keyed by their namespace name.
	CloseFuncs map[string]func() error
}

// GetPostgresConfigKVDB converts a sqldb.PostgresConfig to a kvdb
// postgres.Config.
func GetPostgresConfigKVDB(cfg *sqldb.PostgresConfig) *postgres.Config {
	return &postgres.Config{
		Dsn:            cfg.Dsn,
		Timeout:        cfg.Timeout,
		MaxConnections: cfg.MaxConnections,
	}
}

// GetSqliteConfigKVDB converts a sqldb.SqliteConfig to a kvdb sqlite.Config.
func GetSqliteConfigKVDB(cfg *sqldb.SqliteConfig) *sqlite.Config {
	return &sqlite.Config{
		Timeout:        cfg.Timeout,
		BusyTimeout:    cfg.BusyTimeout,
		MaxConnections: cfg.MaxConnections,
		PragmaOptions:  cfg.PragmaOptions,
	}
}

// GetBackends returns a set of kvdb.Backends as set in the DB config.
func (db *DB) GetBackends(ctx context.Context, chanDBPath,
	walletDBPath, towerServerDBPath string, towerClientEnabled,
	towerServerEnabled bool, logger btclog.Logger) (*DatabaseBackends,
	error) {

	// We keep track of all the kvdb backends we actually open and return a
	// reference to their close function so they can be cleaned up properly
	// on error or shutdown.
	closeFuncs := make(map[string]func() error)

	// If we need to return early because of an error, we invoke any close
	// function that has been initialized so far.
	returnEarly := true
	defer func() {
		if !returnEarly {
			return
		}

		for _, closeFunc := range closeFuncs {
			_ = closeFunc()
		}
	}()

	switch db.Backend {
	case EtcdBackend:
		// As long as the graph data, channel state and height hint
		// cache are all still in the channel.db file in bolt, we
		// replicate the same behavior here and use the same etcd
		// backend for those three sub DBs. But we namespace it properly
		// to make such a split even easier in the future. This will
		// break lnd for users that ran on etcd with 0.13.x since that
		// code used the root namespace. We assume that nobody used etcd
		// for mainnet just yet since that feature was clearly marked as
		// experimental in 0.13.x.
		etcdBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSChannelDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd DB: %w", err)
		}
		closeFuncs[NSChannelDB] = etcdBackend.Close

		etcdMacaroonBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSMacaroonDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd macaroon "+
				"DB: %v", err)
		}
		closeFuncs[NSMacaroonDB] = etcdMacaroonBackend.Close

		etcdDecayedLogBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSDecayedLogDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd decayed "+
				"log DB: %v", err)
		}
		closeFuncs[NSDecayedLogDB] = etcdDecayedLogBackend.Close

		etcdTowerClientBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSTowerClientDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd tower "+
				"client DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = etcdTowerClientBackend.Close

		etcdTowerServerBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSTowerServerDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd tower "+
				"server DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = etcdTowerServerBackend.Close

		returnEarly = false

		return &DatabaseBackends{
			GraphDB:       etcdBackend,
			ChanStateDB:   etcdBackend,
			HeightHintDB:  etcdBackend,
			MacaroonDB:    etcdMacaroonBackend,
			DecayedLogDB:  etcdDecayedLogBackend,
			TowerClientDB: etcdTowerClientBackend,
			TowerServerDB: etcdTowerServerBackend,
			// Manager cannot adopt an externally owned walletdb
			// handle. Preserve the selected mode so startup reports
			// that limit.
			WalletDB: wallet.ManagerConfig{
				Backend: "etcd-walletdb",
			},
			Remote:     true,
			CloseFuncs: closeFuncs,
		}, nil

	case PostgresBackend:
		// Convert the sqldb PostgresConfig to a kvdb postgres.Config.
		// This is a temporary measure until we migrate all kvdb SQL
		// users to native SQL.
		postgresConfig := GetPostgresConfigKVDB(db.Postgres)

		// Create a separate config for channeldb with the global lock
		// setting if configured.
		postgresConfigChannelDB := GetPostgresConfigKVDB(db.Postgres)
		postgresConfigChannelDB.WithGlobalLock = db.Postgres.
			ChannelDBWithGlobalLock

		postgresBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			postgresConfigChannelDB, NSChannelDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres graph "+
				"DB: %v", err)
		}
		closeFuncs[NSChannelDB] = postgresBackend.Close

		postgresMacaroonBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			postgresConfig, NSMacaroonDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres "+
				"macaroon DB: %v", err)
		}
		closeFuncs[NSMacaroonDB] = postgresMacaroonBackend.Close

		postgresDecayedLogBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			postgresConfig, NSDecayedLogDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres "+
				"decayed log DB: %v", err)
		}
		closeFuncs[NSDecayedLogDB] = postgresDecayedLogBackend.Close

		postgresTowerClientBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			postgresConfig, NSTowerClientDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres tower "+
				"client DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = postgresTowerClientBackend.Close

		postgresTowerServerBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			postgresConfig, NSTowerServerDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres tower "+
				"server DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = postgresTowerServerBackend.Close

		// Create a separate config for wallet with the global lock
		// setting if configured.
		postgresConfigWalletDB := GetPostgresConfigKVDB(db.Postgres)
		postgresConfigWalletDB.WithGlobalLock = db.Postgres.
			WalletDBWithGlobalLock

		postgresWalletBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			postgresConfigWalletDB, NSWalletDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres wallet "+
				"DB: %v", err)
		}
		// Read the existing wallet marker before selecting native
		// storage. A populated walletdb needs migration; a new native
		// wallet must never silently replace its keys. This probe owns
		// no runtime.
		err = kvdb.View(postgresWalletBackend, func(tx kvdb.RTx) error {
			bucket := tx.ReadBucket([]byte("lnwallet"))
			if bucket == nil {
				return nil
			}
			if string(bucket.Get([]byte("ready"))) == "ready" {
				return fmt.Errorf("existing walletdb " +
					"requires migration before " +
					"native wallet " +
					"storage can be used")
			}

			return nil
		}, func() {})
		closeErr := postgresWalletBackend.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if !db.UseNativeSQL {
			return nil, fmt.Errorf("managed wallet cannot open " +
				"external postgres walletdb storage")
		}
		if lnrpc.FileExists(filepath.Join(walletDBPath, WalletDBName)) {
			return nil, fmt.Errorf("existing bbolt wallet " +
				"requires migration before native " +
				"wallet storage can be used")
		}

		var nativeSQLStore sqldb.DB
		if db.UseNativeSQL {
			nativePostgresStore, err := sqldb.NewPostgresStore(
				db.Postgres,
			)
			if err != nil {
				return nil, fmt.Errorf("error opening "+
					"native postgres store: %v", err)
			}

			nativeSQLStore = nativePostgresStore
			closeFuncs[PostgresBackend] = nativePostgresStore.Close
		}

		// Warn if the user is trying to switch over to a Postgres DB
		// while there is a wallet or channel bbolt DB still present.
		warnExistingBoltDBs(
			logger, "postgres", walletDBPath, WalletDBName,
		)
		warnExistingBoltDBs(
			logger, "postgres", chanDBPath, ChannelDBName,
		)

		returnEarly = false

		return &DatabaseBackends{
			GraphDB:       postgresBackend,
			ChanStateDB:   postgresBackend,
			HeightHintDB:  postgresBackend,
			MacaroonDB:    postgresMacaroonBackend,
			DecayedLogDB:  postgresDecayedLogBackend,
			TowerClientDB: postgresTowerClientBackend,
			TowerServerDB: postgresTowerServerBackend,
			// Native wallet storage has separate tables/files and
			// is opened and closed exclusively by the wallet
			// Manager.
			WalletDB: wallet.ManagerConfig{
				Backend:        wallet.DBBackendPostgres,
				DataSource:     db.Postgres.Dsn,
				MaxConnections: db.Postgres.MaxConnections,
			},
			NativeSQLStore: nativeSQLStore,
			Remote:         true,
			CloseFuncs:     closeFuncs,
		}, nil

	case SqliteBackend:
		// Convert the sqldb SqliteConfig to a kvdb sqlite.Config.
		// This is a temporary measure until we migrate all kvdb SQL
		// users to native SQL.
		sqliteConfig := GetSqliteConfigKVDB(db.Sqlite)

		// Note that for sqlite, we put kv tables for the channel.db,
		// wtclient.db and sphinxreplay.db all in the channel.sqlite db.
		// The tables for wallet.db and macaroon.db are in the
		// chain.sqlite db and watchtower.db tables are in the
		// watchtower.sqlite db. The reason for the multiple sqlite dbs
		// is twofold. The first reason is that it maintains the file
		// structure that users are used to. The second reason is the
		// fact that sqlite only supports one writer at a time which
		// would cause deadlocks in the code due to the wallet db often
		// being accessed during a write to another db.
		sqliteBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, sqliteConfig, chanDBPath,
			SqliteChannelDBName, NSChannelDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite graph "+
				"DB: %v", err)
		}
		closeFuncs[NSChannelDB] = sqliteBackend.Close

		sqliteMacaroonBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, sqliteConfig, walletDBPath,
			SqliteChainDBName, NSMacaroonDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite "+
				"macaroon DB: %v", err)
		}
		closeFuncs[NSMacaroonDB] = sqliteMacaroonBackend.Close

		sqliteDecayedLogBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, sqliteConfig, chanDBPath,
			SqliteChannelDBName, NSDecayedLogDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite decayed "+
				"log DB: %v", err)
		}
		closeFuncs[NSDecayedLogDB] = sqliteDecayedLogBackend.Close

		sqliteTowerClientBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, sqliteConfig, chanDBPath,
			SqliteChannelDBName, NSTowerClientDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite tower "+
				"client DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = sqliteTowerClientBackend.Close

		sqliteTowerServerBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, sqliteConfig,
			towerServerDBPath, SqliteTowerDBName, NSTowerServerDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite tower "+
				"server DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = sqliteTowerServerBackend.Close

		sqliteWalletBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, sqliteConfig, walletDBPath,
			SqliteChainDBName, NSWalletDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite macaroon "+
				"DB: %v", err)
		}
		// Read the existing wallet marker before selecting native
		// storage. A populated walletdb needs migration; a new native
		// wallet must never silently replace its keys. This probe owns
		// no runtime.
		err = kvdb.View(sqliteWalletBackend, func(tx kvdb.RTx) error {
			bucket := tx.ReadBucket([]byte("lnwallet"))
			if bucket == nil {
				return nil
			}
			if string(bucket.Get([]byte("ready"))) == "ready" {
				return fmt.Errorf("existing walletdb " +
					"requires migration before " +
					"native wallet " +
					"storage can be used")
			}

			return nil
		}, func() {})
		closeErr := sqliteWalletBackend.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if !db.UseNativeSQL {
			return nil, fmt.Errorf("managed wallet cannot open " +
				"external sqlite walletdb storage")
		}
		if lnrpc.FileExists(filepath.Join(walletDBPath, WalletDBName)) {
			return nil, fmt.Errorf("existing bbolt wallet " +
				"requires migration before native " +
				"wallet storage can be used")
		}

		var nativeSQLStore sqldb.DB
		if db.UseNativeSQL {
			nativeSQLiteStore, err := sqldb.NewSqliteStore(
				db.Sqlite,
				path.Join(chanDBPath, SqliteNativeDBName),
			)
			if err != nil {
				return nil, fmt.Errorf("error opening "+
					"native SQLite store: %v", err)
			}

			nativeSQLStore = nativeSQLiteStore
			closeFuncs[SqliteBackend] = nativeSQLiteStore.Close
		}

		// Warn if the user is trying to switch over to a sqlite DB
		// while there is a wallet or channel bbolt DB still present.
		warnExistingBoltDBs(
			logger, "sqlite", walletDBPath, WalletDBName,
		)
		warnExistingBoltDBs(
			logger, "sqlite", chanDBPath, ChannelDBName,
		)

		returnEarly = false

		return &DatabaseBackends{
			GraphDB:       sqliteBackend,
			ChanStateDB:   sqliteBackend,
			HeightHintDB:  sqliteBackend,
			MacaroonDB:    sqliteMacaroonBackend,
			DecayedLogDB:  sqliteDecayedLogBackend,
			TowerClientDB: sqliteTowerClientBackend,
			TowerServerDB: sqliteTowerServerBackend,
			// Native wallet storage has separate tables/files and
			// is opened and closed exclusively by the wallet
			// Manager.
			WalletDB: wallet.ManagerConfig{
				Backend: wallet.DBBackendSQLite,
				DataSource: filepath.Join(
					walletDBPath, WalletSQLiteDBName,
				),
				MaxConnections: db.Sqlite.MaxConnections,
			},
			NativeSQLStore: nativeSQLStore,
			CloseFuncs:     closeFuncs,
		}, nil
	}

	// We're using all bbolt based databases by default.
	boltBackend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            chanDBPath,
		DBFileName:        ChannelDBName,
		DBTimeout:         db.Bolt.DBTimeout,
		NoFreelistSync:    db.Bolt.NoFreelistSync,
		AutoCompact:       db.Bolt.AutoCompact,
		AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
	})
	if err != nil {
		return nil, fmt.Errorf("error opening bolt DB: %w", err)
	}
	closeFuncs[NSChannelDB] = boltBackend.Close

	macaroonBackend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            walletDBPath,
		DBFileName:        MacaroonDBName,
		DBTimeout:         db.Bolt.DBTimeout,
		NoFreelistSync:    db.Bolt.NoFreelistSync,
		AutoCompact:       db.Bolt.AutoCompact,
		AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
	})
	if err != nil {
		return nil, fmt.Errorf("error opening macaroon DB: %w", err)
	}
	closeFuncs[NSMacaroonDB] = macaroonBackend.Close

	decayedLogBackend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            chanDBPath,
		DBFileName:        DecayedLogDbName,
		DBTimeout:         db.Bolt.DBTimeout,
		NoFreelistSync:    db.Bolt.NoFreelistSync,
		AutoCompact:       db.Bolt.AutoCompact,
		AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
	})
	if err != nil {
		return nil, fmt.Errorf("error opening decayed log DB: %w", err)
	}
	closeFuncs[NSDecayedLogDB] = decayedLogBackend.Close

	// The tower client is optional and might not be enabled by the user. We
	// handle it being nil properly in the main server.
	var towerClientBackend kvdb.Backend
	if towerClientEnabled {
		towerClientBackend, err = kvdb.GetBoltBackend(
			&kvdb.BoltBackendConfig{
				DBPath:            chanDBPath,
				DBFileName:        TowerClientDBName,
				DBTimeout:         db.Bolt.DBTimeout,
				NoFreelistSync:    db.Bolt.NoFreelistSync,
				AutoCompact:       db.Bolt.AutoCompact,
				AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("error opening tower client "+
				"DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = towerClientBackend.Close
	}

	// The tower server is optional and might not be enabled by the user. We
	// handle it being nil properly in the main server.
	var towerServerBackend kvdb.Backend
	if towerServerEnabled {
		towerServerBackend, err = kvdb.GetBoltBackend(
			&kvdb.BoltBackendConfig{
				DBPath:            towerServerDBPath,
				DBFileName:        TowerServerDBName,
				DBTimeout:         db.Bolt.DBTimeout,
				NoFreelistSync:    db.Bolt.NoFreelistSync,
				AutoCompact:       db.Bolt.AutoCompact,
				AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("error opening tower server "+
				"DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = towerServerBackend.Close
	}

	returnEarly = false

	return &DatabaseBackends{
		GraphDB:       boltBackend,
		ChanStateDB:   boltBackend,
		HeightHintDB:  boltBackend,
		MacaroonDB:    macaroonBackend,
		DecayedLogDB:  decayedLogBackend,
		TowerClientDB: towerClientBackend,
		TowerServerDB: towerServerBackend,
		// Manager opens the existing local wallet file after the public
		// passphrase is available and owns its eventual close.
		WalletDB: wallet.ManagerConfig{
			Backend: wallet.DBBackendKVDB,
			DataSource: filepath.Join(
				walletDBPath, WalletDBName,
			),
			NoFreelistSync: db.Bolt.NoFreelistSync,
			Timeout:        db.Bolt.DBTimeout,
		},
		CloseFuncs: closeFuncs,
	}, nil
}

// warnExistingBoltDBs checks if there is an existing bbolt database in the
// given location and logs a warning if so.
func warnExistingBoltDBs(log btclog.Logger, dbType, dir, fileName string) {
	if lnrpc.FileExists(filepath.Join(dir, fileName)) {
		log.Warnf("Found existing bbolt database file in %s/%s while "+
			"using database type %s. Existing data will NOT be "+
			"migrated to %s automatically!", dir, fileName, dbType,
			dbType)
	}
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
