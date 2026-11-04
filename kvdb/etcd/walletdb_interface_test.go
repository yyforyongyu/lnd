//go:build kvdb_etcd
// +build kvdb_etcd

package etcd

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
	"github.com/stretchr/testify/require"
)

// TestWalletDBInterface performs the WalletDB interface test suite for the
// etcd database driver.
func TestWalletDBInterface(t *testing.T) {
	f := NewEtcdTestFixture(t)
	cfg := f.BackendConfig()
	walletdbtest.TestInterface(t, dbType, t.Context(), &cfg)
}

// TestCursorPrevAfterMissingSeek verifies the reverse block-iteration pattern
// can reach committed records after seeking beyond the final block height.
func TestCursorPrevAfterMissingSeek(t *testing.T) {
	t.Parallel()

	// Arrange: Commit two block-height keys before opening a new cursor.
	// The wallet's native rollback uses this same single-writer driver mode
	// and must read persisted records rather than only pending STM writes.
	f := NewEtcdTestFixture(t)
	db := f.NewBackend(true)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	bucketKey := []byte("blocks")
	firstKey := []byte{0, 0, 0, 1}
	lastKey := []byte{0, 0, 0, 2}
	lastValue := []byte("last block")
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(bucketKey)
		if err != nil {
			return err
		}

		err = bucket.Put(firstKey, []byte("first block"))
		if err != nil {
			return err
		}

		return bucket.Put(lastKey, lastValue)
	})
	require.NoError(t, err)
	tx, err := db.BeginReadWriteTx()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	cursor := tx.ReadWriteBucket(bucketKey).ReadWriteCursor()

	// Act: Match the native reverse iterator's initial seek above all valid
	// heights, followed by a backward step when the seek finds no record.
	missedKey, missedValue := cursor.Seek([]byte{0xff, 0xff, 0xff, 0xff})
	previousKey, previousValue := cursor.Prev()

	// Assert: The seek reports no match while preserving its position.
	// Backward iteration must reach the final block so rollback can run.
	require.Nil(t, missedKey)
	require.Nil(t, missedValue)
	require.Equal(t, lastKey, previousKey)
	require.Equal(t, lastValue, previousValue)
}
