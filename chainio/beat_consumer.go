package chainio

// BeatConsumer defines a supplementary component that should be used by
// subsystems which implement the `Consumer` interface. It partially implements
// the `Consumer` interface by providing the method `ProcessBlock` such that
// subsystems don't need to re-implement it.
//
// While inheritance is not commonly used in Go, subsystems embedding this
// struct cannot pass the interface check for `Consumer` because the `Name`
// method is not implemented, which gives us a "mortise and tenon" structure.
// In addition to reducing code duplication, this design allows `ProcessBlock`
// to work on the concrete type `Beat` to access its internal states.
type BeatConsumer struct {
	// BlockbeatChan is a channel to receive blocks from Blockbeat. The
	// received block contains the best known height and the txns confirmed
	// in this block.
	BlockbeatChan chan Blockbeat

	// name is the name of the consumer which embeds the BlockConsumer.
	name string

	// quit is a channel that closes when the BlockConsumer is shutting
	// down.
	//
	// NOTE: this quit channel should be mounted to the same quit channel
	// used by the subsystem.
	quit chan struct{}

	// currentBeat is the current beat of the consumer.
	currentBeat Beat
}

// NewBeatConsumer creates a new BlockConsumer.
func NewBeatConsumer(quit chan struct{}, name string, beat Beat) BeatConsumer {
	b := BeatConsumer{
		BlockbeatChan: make(chan Blockbeat),
		quit:          quit,
		name:          name,
	}

	b.setCurrentBeat(beat)

	return b
}

// ProcessBlock takes a blockbeat and sends it to the blockbeat channel.
//
// NOTE: part of the `chainio.Consumer` interface.
func (b *BeatConsumer) ProcessBlock(beat Beat) <-chan error {
	// Update the current height.
	b.setCurrentBeat(beat)

	select {
	// Send the beat to the blockbeat channel. It's expected that the
	// consumer will read from this channel and process the block. Once
	// processed, it should return the error or nil to the beat.Err chan.
	case b.BlockbeatChan <- beat:
		beat.log.Tracef("Sent blockbeat to %v", b.name)

	case <-b.quit:
		beat.log.Debugf("[%s] received shutdown", b.name)

		select {
		case beat.errChan <- nil:
		default:
		}

		return beat.errChan
	}

	return beat.errChan
}

// setCurrentBeat sets the current beat of the consumer.
func (b *BeatConsumer) setCurrentBeat(beat Beat) {
	beat.log.Tracef("set current height for [%s]", b.name)
	b.currentBeat = beat
}

// CurrentBeat returns the current blockbeat of the consumer. This is used by
// subsystems to retrieve the current blockbeat during their startup.
func (b *BeatConsumer) CurrentBeat() Beat {
	return b.currentBeat
}
