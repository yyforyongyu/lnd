package chainio

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

	// quit is a channel that closes when the BlockConsumer is shutting
	// down.
	//
	// NOTE: this quit channel should be mounted to the same quit channel
	// used by the subsystem.
	quit chan struct{}
}

// NewBlockConsumer creates a new BlockConsumer.
func NewBlockConsumer(quit chan struct{}) BlockConsumer {
	return BlockConsumer{
		BlockbeatChan: make(chan Blockbeat),
		quit:          quit,
	}
}

// ProcessBlock takes a blockbeat and sends it to the blockbeat channel.
//
// NOTE: part of the `chainio.Consumer` interface.
func (b *BlockConsumer) ProcessBlock(beat Blockbeat) <-chan error {
	select {
	case b.BlockbeatChan <- beat:
		log.Debugf("Received block beat for height=%d",
			beat.Epoch.Height)

	case <-b.quit:
		return nil
	}

	return beat.Err
}
