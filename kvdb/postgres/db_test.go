//go:build kvdb_postgres

package postgres

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

// TestInterface performs all interfaces tests for this database driver.
func TestInterface(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer stop()

	f, err := NewFixture("")
	require.NoError(t, err)

	// dbType is the database type name for this driver.
	const dbType = "postgres"

	ctx := context.Background()
	cfg := &Config{
		Dsn: f.Dsn,
	}

	walletdbtest.TestInterface(t, dbType, ctx, cfg, prefix)
}
