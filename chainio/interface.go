package chainio

// Blockbeat defines an interface that can be used by subsystems to retrieve
// block data. It is sent by the BlockbeatDispatcher whenever a new block is
// received. Once the subsystem finishes processing the block, it must signal
// it by calling NotifyBlockProcessed.
//
// The blockchain is a state machine - whenever there's a state change, it's
// manifested in a block. The blockbeat is a way to notify subsystems of this
// state change, and to provide them with the data they need to process it. In
// other words, subsystems must react to this state change and should consider
// being driven by the blockbeat in their own state machines.
type Blockbeat interface {
	// NotifyBlockProcessed signals that the block has been processed. It
	// takes an error resulted from processing the block, and a quit chan
	// of the subsystem.
	//
	// NOTE: This method must be called by the subsystem after it has
	// finished processing the block. Extreme caution must be taken when
	// returning an error as it will shutdown lnd.
	//
	// TODO(yy): Define fatal and non-fatal errors.
	NotifyBlockProcessed(err error, quitChan chan struct{})

	// Height returns the current block height.
	Height() int32

	// DispatchConcurrent sends the blockbeat to the specified consumers
	// concurrently.
	DispatchConcurrent(consumers []Consumer) error

	// DispatchConcurrent sends the blockbeat to the specified consumers
	// sequentially.
	DispatchSequential(consumers []Consumer) error
}

// Consumer defines a blockbeat consumer interface. Subsystems that need block
// info must implement it.
type Consumer interface {
	// Name returns a human-readable string for this subsystem.
	Name() string

	// ProcessBlock takes a blockbeat and processes it. A receive-only
	// error chan must be returned.
	//
	// NOTE: When implementing this, it's very important to send back the
	// error or nil to the channel `b.errChan` immediately, otherwise
	// BlockbeatDispatcher will timeout and lnd will shutdown.
	ProcessBlock(b Blockbeat) <-chan error
}
