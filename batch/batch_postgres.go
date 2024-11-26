//go:build kvdb_postgres
// +build kvdb_postgres

package batch

import (
	"github.com/lightningnetwork/lnd/kvdb"
)

// run executes the current batch of requests. It avoids doing actual batching
// to prevent excessive serialization errors and deadlocks.
func (b *batch) run() {
	// Clear the batch from its scheduler, ensuring that no new requests are
	// added to this batch.
	b.clear(b)

	// If a cache lock was provided, hold it until the this method returns.
	// This is critical for ensuring external consistency of the operation,
	// so that caches don't get out of sync with the on disk state.
	if b.locker != nil {
		b.locker.Lock()
		defer b.locker.Unlock()
	}

	// Apply each request in the batch in its own transaction. Requests that
	// fail will be retried by the caller.
	for _, req := range b.reqs {
		err := kvdb.Update(b.db, func(tx kvdb.RwTx) error {
			return req.Update(tx)
		}, func() {
			if req.Reset != nil {
				req.Reset()
			}
		})
		switch {
		case err != nil:
			req.errChan <- errSolo
		case req.OnCommit != nil:
			req.errChan <- req.OnCommit(err)
		default:
			req.errChan <- nil
		}
	}
}
