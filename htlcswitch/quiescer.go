//nolint:unused
package htlcswitch

import (
	"fmt"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrStfuAlreadySent indicates that this channel has already sent an
	// Stfu message for this negotiation.
	ErrStfuAlreadySent = fmt.Errorf("stfu already sent")

	// ErrStfuAlreadyRcvd indicates that this channel has already received
	// an Stfu message for this negotiation.
	ErrStfuAlreadyRcvd = fmt.Errorf("stfu already received")

	// ErrNoQuiescenceInitiator indicates that the caller has requested the
	// quiescence initiator for a channel that is not yet quiescent.
	ErrNoQuiescenceInitiator = fmt.Errorf(
		"indeterminate quiescence initiator: channel is not quiescent",
	)

	// ErrPendingRemoteUpdates indicates that we have received an Stfu while
	// the remote party has issued updates that are not yet bilaterally
	// committed.
	ErrPendingRemoteUpdates = fmt.Errorf(
		"stfu received with pending remote updates",
	)

	// ErrPendingLocalUpdates indicates that we are attempting to send an
	// Stfu while we have issued updates that are not yet bilaterally
	// committed.
	ErrPendingLocalUpdates = fmt.Errorf(
		"stfu send attempted with pending local updates",
	)
)

// quiescerCfg is a config structure used to initialize a quiescer giving it the
// appropriate functionality to interact with the channel state that the
// quiescer must syncrhonize with.
type quiescerCfg struct {
	// chanID marks what channel we are managing the state machine for. This
	// is important because the quiescer is responsible for constructing the
	// messages we send out and the ChannelID is a key field in that
	// message.
	chanID lnwire.ChannelID

	// channelInitiator indicates which ChannelParty originally opened the
	// channel. This is used to break ties when both sides of the channel
	// send Stfu claiming to be the initiator.
	channelInitiator lntypes.ChannelParty

	// numPendingUpdates is a function that returns the number of pending
	// originated by the party in the first argument that have yet to be
	// committed to the commitment transaction held by the party in the
	// second argument.
	numPendingUpdates func(lntypes.ChannelParty,
		lntypes.ChannelParty) uint64

	// sendMsg is a function that can be used to send an Stfu message over
	// the wire.
	sendMsg func(lnwire.Stfu) error
}

// quiescer is a state machine that tracks progression through the quiescence
// protocol.
type quiescer struct {
	cfg quiescerCfg

	// localInit indicates whether our path through this state machine was
	// initiated by our node. This can be true or false independently of
	// remoteInit.
	localInit bool

	// remoteInit indicates whether we received Stfu from our peer where the
	// message indicated that the remote node believes it was the initiator.
	// This can be true or false independently of localInit.
	remoteInit bool

	// sent tracks whether or not we have emitted Stfu for sending.
	sent bool

	// received tracks whether or not we have received Stfu from our peer.
	received bool
}

// newQuiescer creates a new quiescer for the given channel.
func newQuiescer(cfg quiescerCfg) quiescer {
	return quiescer{
		cfg: cfg,
	}
}

// recvStfu is called when we receive an Stfu message from the remote.
func (q *quiescer) recvStfu(msg lnwire.Stfu) error {
	// At the time of this writing, this check that we have already received
	// an Stfu is not strictly necessary, according to the specification.
	// However, it is fishy if we do and it is unclear how we should handle
	// such a case so we will err on the side of caution.
	if q.received {
		return fmt.Errorf("%w for channel %v", ErrStfuAlreadyRcvd,
			q.cfg.chanID)
	}

	if !q.canRecvStfu() {
		return fmt.Errorf("%w for channel %v", ErrPendingRemoteUpdates,
			q.cfg.chanID)
	}

	q.received = true

	// If the remote party sets the initiator bit to true then we will
	// remember that they are making a claim to the initiator role. This
	// does not necessarily mean they will get it, though.
	q.remoteInit = msg.Initiator

	return nil
}

// makeStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *quiescer) makeStfu() fn.Result[lnwire.Stfu] {
	if q.sent {
		return fn.Errf[lnwire.Stfu]("%w for channel %v",
			ErrStfuAlreadySent, q.cfg.chanID)
	}

	if !q.canSendStfu() {
		return fn.Errf[lnwire.Stfu]("%w for channel %v",
			ErrPendingLocalUpdates, q.cfg.chanID)
	}

	stfu := lnwire.Stfu{
		ChanID:    q.cfg.chanID,
		Initiator: q.localInit,
	}

	return fn.Ok(stfu)
}

// oweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu, or we are the initiator
// but have not yet sent an Stfu.
func (q *quiescer) oweStfu() bool {
	return q.received && !q.sent
}

// needStfu returns true if the remote owes us an Stfu. They owe us an Stfu when
// we have sent but not yet received an Stfu.
func (q *quiescer) needStfu() bool {
	return q.sent && !q.received
}

// isQuiescent returns true if the state machine has been driven all the way to
// completion. If this returns true, processes that depend on channel quiescence
// may proceed.
func (q *quiescer) isQuiescent() bool {
	return q.sent && q.received
}

// quiescenceInitiator determines which ChannelParty is the initiator of
// quiescence for the purposes of downstream protocols. If the channel is not
// currently quiescent, this method will return ErrNoQuiescenceInitiator.
func (q *quiescer) quiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	switch {
	case !q.isQuiescent():
		return fn.Err[lntypes.ChannelParty](ErrNoQuiescenceInitiator)

	case q.localInit && q.remoteInit:
		// In the case of a tie, the channel initiator wins.
		return fn.Ok(q.cfg.channelInitiator)

	case !q.localInit && !q.remoteInit:
		return fn.Err[lntypes.ChannelParty](ErrNoQuiescenceInitiator)

	case q.localInit:
		return fn.Ok(lntypes.Local)

	case q.remoteInit:
		return fn.Ok(lntypes.Remote)
	}

	panic("impossible: non-exhaustive switch quiescer.quiescenceInitiator")
}

// canSendUpdates returns true if we haven't yet sent an Stfu which would mark
// the end of our ability to send updates.
func (q *quiescer) canSendUpdates() bool {
	return !q.sent && !q.localInit
}

// canRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *quiescer) canRecvUpdates() bool {
	return !q.received
}

// canSendStfu returns true if we can send an Stfu.
func (q *quiescer) canSendStfu() bool {
	return q.cfg.numPendingUpdates(lntypes.Local, lntypes.Local) == 0 &&
		q.cfg.numPendingUpdates(lntypes.Local, lntypes.Remote) == 0
}

// canRecvStfu returns true if we can receive an Stfu.
func (q *quiescer) canRecvStfu() bool {
	return q.cfg.numPendingUpdates(lntypes.Remote, lntypes.Local) == 0 &&
		q.cfg.numPendingUpdates(lntypes.Remote, lntypes.Remote) == 0
}

// sendOwedStfu sends Stfu if it owes one. It returns an error if the state
// machine is in an invalid state.
func (q *quiescer) sendOwedStfu() error {
	if !q.oweStfu() || !q.canSendStfu() {
		return nil
	}

	stfu, err := q.makeStfu().Unpack()
	if err != nil {
		return err
	}

	err = q.cfg.sendMsg(stfu)
	if err != nil {
		return err
	}

	q.sent = true

	return nil
}