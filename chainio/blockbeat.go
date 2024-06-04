package chainio

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
)

// DefaultProcessBlockTimeout is the timeout value used when waiting for one
// consumer to finish processing the new block epoch.
var DefaultProcessBlockTimeout = 30 * time.Second

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

// notify sends the blockbeat to the specified consumer. It requires the
// consumer to finish processing the block under 30s, otherwise a timeout error
// is returned.
func notify(c Consumer, b Blockbeat) error {
	// Record the time it takes the consumer to process this block.
	start := time.Now()

	log.Debugf("Sending block %v to consumer: %v", b.Epoch.Height, c.Name())

	// We expect the consumer to finish processing this block under 30s,
	// otherwise a timeout error is returned.
	err, timeout := fn.RecvOrTimeout(
		c.ProcessBlock(b), DefaultProcessBlockTimeout,
	)
	if err != nil {
		return fmt.Errorf("%s: ProcessBlock got: %w", c.Name(), err)
	}
	if timeout != nil {
		return fmt.Errorf("%s timed out while processing block",
			c.Name())
	}

	log.Debugf("Consumer [%s] processed block %d in %v", c.Name(),
		b.Epoch.Height, time.Since(start))

	return nil
}

// NotifySequential takes a list of consumers and notify them about the new
// epoch sequentially.
func NotifySequential(consumers []Consumer, b Blockbeat) error {
	for _, c := range consumers {
		// Construct a new beat with a buffered error chan.
		beat := NewBlockbeat(b.Epoch)

		// Send the copy of the beat to the consumer.
		if err := notify(c, beat); err != nil {
			log.Errorf("Consumer=%v failed to process "+
				"block: %v", c.Name(), err)

			return err
		}
	}

	return nil
}

// NotifyConcurrent notifies each consumer concurrently about the latest block
// epoch.
func NotifyConcurrent(consumers []Consumer, b Blockbeat) error {
	// errChans is a map of channels that will be used to receive errors
	// returned from notifying the consumers.
	errChans := make(map[string]chan error, len(consumers))

	// Notify each queue in goroutines.
	for _, c := range consumers {
		// Create a signal chan.
		errChan := make(chan error, 1)
		errChans[c.Name()] = errChan

		// Notify each consumer concurrently.
		go func(c Consumer, epoch chainntnfs.BlockEpoch) {
			// Construct a new beat with a buffered error chan.
			beat := NewBlockbeat(epoch)

			// Send the copy of the beat to the consumer.
			errChan <- notify(c, beat)
		}(c, b.Epoch)
	}

	// Wait for all consumers in each queue to finish.
	for name, errChan := range errChans {
		err := <-errChan
		if err != nil {
			log.Errorf("Consumer=%v failed to process block: %v",
				name, err)

			return err
		}
	}

	return nil
}
