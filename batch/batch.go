package batch

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/kvdb"
)

// errSolo is a sentinel error indicating that the requester should re-run the
// operation in isolation.
var errSolo = errors.New(
	"batch function returned an error and should be re-run solo",
)

type request struct {
	*Request
	errChan chan error
}

type batch struct {
	db     kvdb.Backend
	start  sync.Once
	reqs   []*request
	clear  func(b *batch)
	locker sync.Locker
}

// trigger is the entry point for the batch and ensures that run is started at
// most once.
func (b *batch) trigger() {
	b.start.Do(b.run)
}
