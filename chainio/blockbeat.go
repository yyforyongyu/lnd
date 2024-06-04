package chainio

import "github.com/lightningnetwork/lnd/chainntnfs"

// Blockbeat contains the block epoch and a buffer error chan.
//
// TODO(yy): extend this to check for confirmation status - which serves as the
// single source of truth, to avoid the potential race between receiving blocks
// and `GetTransactionDetails/RegisterSpendNtfn/RegisterConfirmationsNtfn`.
type Blockbeat struct {
	// Epoch is the current block epoch the blockbeat is aware of.
	Epoch chainntnfs.BlockEpoch

	// Err is a buffered chan that receives an error or nil from
	// ProcessBlock.
	Err chan error
}

// NewBlockbeat creates a new beat with the specified block epoch and a
// buffered error chan.
func NewBlockbeat(epoch chainntnfs.BlockEpoch) Blockbeat {
	return Blockbeat{
		Epoch: epoch,
		Err:   make(chan error, 1),
	}
}
