package chainio

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/chainntnfs"
)

// BlockbeatDispatcher is a service that handles dispatching new blocks to
// `lnd`'s subsystems. During startup, subsystems that are block-driven should
// implement the `Consumer` interface and register themselves via
// `RegisterQueue`. When two subsystems are independent of each other, they
// should be registered in differet queues so blocks are notified concurrently.
// Otherwise, when living in the same queue, the subsystems are notified of the
// new blocks sequentially, which means it's critical to understand the
// relationship of these systems to properly handle the order.
type BlockbeatDispatcher struct {
	wg sync.WaitGroup

	// notifier is used to receive new block epochs.
	notifier chainntnfs.ChainNotifier

	// beat is the latest blockbeat received.
	beat Blockbeat

	// consumerQueues is a map of consumers that will receive blocks. Each
	// queue is notified concurrently, and consumers in the same queue is
	// notified sequentially.
	consumerQueues map[uint32][]Consumer

	// counter is used to assign a unique id to each queue.
	counter atomic.Uint32

	// quit is used to signal the BlockbeatDispatcher to stop.
	quit chan struct{}
}

// NewBlockbeatDispatcher returns a new blockbeat dispatcher instance.
func NewBlockbeatDispatcher(n chainntnfs.ChainNotifier) *BlockbeatDispatcher {
	return &BlockbeatDispatcher{
		notifier:       n,
		quit:           make(chan struct{}),
		consumerQueues: make(map[uint32][]Consumer),
	}
}

// RegisterQueue takes a list of consumers and register them in the same queue.
//
// NOTE: these consumers are notified sequentially.
func (b *BlockbeatDispatcher) RegisterQueue(consumers []Consumer) {
	qid := b.counter.Add(1)

	b.consumerQueues[qid] = append(b.consumerQueues[qid], consumers...)
	clog.Infof("Registered queue=%d with %d blockbeat consumers", qid,
		len(consumers))

	for _, c := range consumers {
		clog.Debugf("Consumer [%s] registered in queue %d", c.Name(),
			qid)
	}
}

// Start starts the blockbeat dispatcher - it registers a block notification
// and monitors and dispatches new blocks in a goroutine. It will refuse to
// start if there are no registered consumers.
func (b *BlockbeatDispatcher) Start() error {
	// Make sure consumers are registered.
	if len(b.consumerQueues) == 0 {
		return fmt.Errorf("no consumers registered")
	}

	// Start listening to new block epochs.
	blockEpochs, err := b.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return fmt.Errorf("register block epoch ntfn: %w", err)
	}

	clog.Infof("BlockbeatDispatcher is starting with %d consumer queues",
		len(b.consumerQueues))
	defer clog.Debug("BlockbeatDispatcher started")

	b.wg.Add(1)
	go b.dispatchBlocks(blockEpochs)

	return nil
}

// Stop shuts down the blockbeat dispatcher.
func (b *BlockbeatDispatcher) Stop() {
	clog.Info("BlockbeatDispatcher is stopping")
	defer clog.Debug("BlockbeatDispatcher stopped")

	// Signal the dispatchBlocks goroutine to stop.
	close(b.quit)
	b.wg.Wait()
}

// dispatchBlocks listens to new block epoch and dispatches it to all the
// consumers. Each queue is notified concurrently, and the consumers in the
// same queue are notified sequentially.
func (b *BlockbeatDispatcher) dispatchBlocks(
	blockEpochs *chainntnfs.BlockEpochEvent) {

	defer b.wg.Done()
	defer blockEpochs.Cancel()

	for {
		select {
		case blockEpoch, ok := <-blockEpochs.Epochs:
			if !ok {
				clog.Debugf("Block epoch channel closed")
				return
			}

			clog.Infof("Received new block %v at height %d, "+
				"notifying consumers...", blockEpoch.Hash,
				blockEpoch.Height)

			// Update the current block epoch.
			b.beat = NewBlockbeat(*blockEpoch)

			// Notify all consumers.
			b.notifyQueues()

			b.beat.log.Info("Notified all consumers on new block")

		case <-b.quit:
			clog.Debugf("BlockbeatDispatcher quit signal received")
			return
		}
	}
}

// notifyQueues notifies each queue concurrently about the latest block epoch.
func (b *BlockbeatDispatcher) notifyQueues() {
	// errChans is a map of channels that will be used to receive errors
	// returned from notifying the consumers.
	errChans := make(map[uint32]chan error, len(b.consumerQueues))

	// Notify each queue in goroutines.
	for qid, consumers := range b.consumerQueues {
		b.beat.log.Debugf("Notifying queue=%d with %d consumers",
			qid, len(consumers))

		// Create a signal chan.
		errChan := make(chan error)
		errChans[qid] = errChan

		// Notify each queue concurrently.
		go func(qid uint32, c []Consumer, b Blockbeat) {
			// Notify each consumer in this queue sequentially.
			errChan <- NotifySequential(c, b)
		}(qid, consumers, b.beat)
	}

	// Wait for all consumers in each queue to finish.
	for qid, errChan := range errChans {
		select {
		case err := <-errChan:
			// It's critical that the subsystems can process blocks
			// correctly and timely, if an error returns, we'd
			// gracefully shutdown lnd to bring attentions.
			if err != nil {
				clog.Criticalf("Queue=%d failed to process "+
					"block: %v", qid, err)

				return
			}

			b.beat.log.Debugf("Notified queue=%d on block %d", qid,
				b.beat.Epoch.Height)

		case <-b.quit:
		}
	}
}
