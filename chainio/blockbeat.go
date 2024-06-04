package chainio

import (
	"fmt"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

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

	// log is the customized logger for the blockbeat which prints the
	// block height.
	log btclog.Logger
}

// NewBlockbeat creates a new beat with the specified block epoch and a
// buffered error chan.
func NewBlockbeat(epoch chainntnfs.BlockEpoch) Blockbeat {
	b := Blockbeat{
		Epoch: epoch,
		Err:   make(chan error, 1),
	}

	// Create a customized logger for the blockbeat.
	logPrefix := fmt.Sprintf("Height[%6d]:", b.Epoch.Height)
	b.log = build.NewPrefixLog(logPrefix, clog)

	return b
}
