package chainio

import (
	"github.com/lightningnetwork/lnd/fn"
)

// Consumer defines a blockbeat consumer interface. Subsystems that need block
// info must implement it.
type Consumer interface {
	// Name returns a human-readable string for this subsystem.
	Name() string

	// ProcessBlock takes a blockbeat and processes it. A receive-only
	// error chan must be returned.
	//
	// NOTE: When implementing this, it's very important to send back the
	// error or nil to the channel `b.Err` immediately, otherwise
	// BlockbeatDispatcher will timeout and lnd will shutdown.
	ProcessBlock(b Blockbeat) <-chan error
}

// BlockConsumer defines a supplementary component that should be used by
// subsystems which implement the `Consumer` interface. It partially implements
// the `Consumer` interface by providing the method `ProcessBlock` such that
// subsystems don't need to re-implement it.
type BlockConsumer struct {
	// BlockbeatChan is a channel to receive blocks from Blockbeat. The
	// received block contains the best known height and the transactions
	// confirmed in this block.
	BlockbeatChan chan Blockbeat

	// Beat is the last blockbeat received.
	Beat Blockbeat

	// name is the name of the consumer which embeds the BlockConsumer.
	name string

	// quit is a channel that closes when the BlockConsumer is shutting
	// down.
	//
	// NOTE: this quit channel should be mounted to the same quit channel
	// used by the subsystem.
	quit chan struct{}
}

// NewBlockConsumer creates a new BlockConsumer.
func NewBlockConsumer(quit chan struct{}, name string) BlockConsumer {
	return BlockConsumer{
		BlockbeatChan: make(chan Blockbeat),
		quit:          quit,
		name:          name,
	}
}

// ProcessBlock takes a blockbeat and sends it to the blockbeat channel.
//
// NOTE: part of the `chainio.Consumer` interface.
func (b *BlockConsumer) ProcessBlock(beat Blockbeat) <-chan error {
	// Remember the last blockbeat received.
	b.Beat = beat

	select {
	// Send the beat to the blockbeat channel. It's expected that the
	// consumer will read from this channel and process the block. Once
	// processed, it should return the error or nil to the beat.Err chan.
	case b.BlockbeatChan <- beat:
		beat.log.Tracef("Sent blockbeat to %v", b.name)

	case <-b.quit:
		beat.log.Debugf("[%s] received shutdown", b.name)

		select {
		case beat.Err <- nil:
			// Notify we've processed the block.
			fn.SendOrQuit(beat.Err, nil, b.quit)

		default:
		}

		return beat.Err
	}

	return beat.Err
}
