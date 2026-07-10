package dyn

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/lntypes"
)

// ReconnectAction enumerates what to do with a persisted dyn-negotiation
// context when a channel is reestablished after a disconnect. It is the output
// of the pure Decide function, which encodes the locked reconnect rules of the
// dynamic-commitments extension BOLT.
type ReconnectAction uint8

const (
	// ReconnectForget indicates the persisted negotiation must be abandoned
	// and its context deleted. This covers every pre-dyn_commit_sig
	// negotiation with one exception (see ReconnectRetainAndWait): a
	// proposer that has not persisted a dyn_commit_sig — whether or not it
	// received a dyn_ack — as well as a responder that had not yet sent its
	// dyn_ack. A received dyn_ack in particular MUST NOT be reused after a
	// reconnect unless a dyn_commit_sig was persisted before the disconnect.
	ReconnectForget ReconnectAction = iota

	// ReconnectRetainAndWait indicates a responder that sent its dyn_ack
	// before the disconnect must retain the acceptance across the reconnect
	// and wait for the proposer's dyn_commit_sig, a superseding proposal, or
	// a timeout, without proactively resending any dyn message itself.
	ReconnectRetainAndWait

	// ReconnectRetransmitCommitSig indicates the proposer persisted a
	// dyn_commit_sig that the peer has not yet processed, so it must be
	// retransmitted before any other update even though quiescence has
	// ended. It is counted as the corresponding commitment_signed for the
	// peer's next_commitment_number accounting.
	ReconnectRetransmitCommitSig

	// ReconnectResume indicates the dyn_commit_sig has already been processed
	// by the peer (or was received by us as the responder), so the update is
	// effectively locked in and normal commitment retransmission covers the
	// remainder of the dance.
	ReconnectResume
)

// String returns a human-readable representation of the action.
func (a ReconnectAction) String() string {
	switch a {
	case ReconnectForget:
		return "Forget"

	case ReconnectRetainAndWait:
		return "RetainAndWait"

	case ReconnectRetransmitCommitSig:
		return "RetransmitCommitSig"

	case ReconnectResume:
		return "Resume"

	default:
		return fmt.Sprintf("Unknown(%d)", a)
	}
}

// ReestablishState captures the reconnect-time inputs, beyond the persisted
// AcceptedProposal, that the reconnect decision depends on. These come from the
// channel_reestablish the peer sent on reconnection.
type ReestablishState struct {
	// PeerNextCommitHeight is the next_commitment_number the peer advertised
	// in its channel_reestablish (its NextLocalCommitHeight), i.e. the
	// commitment number it expects to receive from us next. A persisted
	// dyn_commit_sig must be retransmitted when this still equals the height
	// the dyn_commit_sig binds to, since a dyn_commit_sig is counted as the
	// commitment_signed for that height.
	PeerNextCommitHeight uint64
}

// Decide is the pure reconnect decision function. Given a persisted
// AcceptedProposal and the peer's reestablish state, it returns the action the
// caller must take, implementing the locked design decisions:
//
//   - Pre-dyn_commit_sig negotiations abort on disconnect (no retransmit of
//     dyn_propose/dyn_ack/dyn_reject).
//   - A received dyn_ack MUST NOT be reused after reconnect unless a
//     dyn_commit_sig was persisted before disconnect.
//   - A responder that sent dyn_ack retains its acceptance across reconnect and
//     waits for dyn_commit_sig / a proposer update / a timeout.
//   - A persisted dyn_commit_sig is retransmitted when the peer still expects
//     it, and is otherwise treated as already locked in.
//
// It performs no side effects and does not consult persistence; the caller
// loads the persisted context and applies the returned action.
func Decide(p AcceptedProposal, r ReestablishState) ReconnectAction {
	ackKnown := p.AckSig.IsSome()
	commitSigned := p.HasCommitSig()
	proposer := p.Proposer == lntypes.Local

	switch {
	// No dyn_commit_sig crossed the wire before the disconnect. Every such
	// negotiation is abandoned, except a responder that already sent its
	// dyn_ack, which retains the acceptance and waits.
	case !commitSigned:
		if !proposer && ackKnown {
			return ReconnectRetainAndWait
		}

		return ReconnectForget

	// A dyn_commit_sig was persisted. Only the proposer ever sends one, so
	// only the proposer retransmits, and only while the peer's next expected
	// commitment number still points at the height the dyn_commit_sig binds
	// to.
	case proposer && r.PeerNextCommitHeight == p.NextCommitHeight:
		return ReconnectRetransmitCommitSig

	// Either the peer has already advanced past the dyn_commit_sig, or we are
	// the responder that received one: the update is locked in and normal
	// commitment retransmission handles the rest.
	default:
		return ReconnectResume
	}
}

// Restore rehydrates the in-memory session of a freshly-constructed Updater
// from a persisted AcceptedProposal after a reconnect, so the machine can
// continue (or complete) a negotiation that survived the disconnect. The target
// state is derived from the persisted role and signatures. It is the caller's
// responsibility to only call this for negotiations that Decide says to keep
// (RetainAndWait, RetransmitCommitSig, Resume); a negotiation to forget must be
// dropped via Reset instead.
//
// The context is accepted for symmetry with the rest of the API and future use;
// Restore itself performs no persistence.
func (u *Updater) Restore(_ context.Context, p AcceptedProposal) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if p.Proposal == nil {
		return fmt.Errorf("cannot restore dyn session with nil proposal")
	}

	u.role = p.Proposer
	u.proposal = p.Proposal
	u.nextHeight = p.NextCommitHeight
	u.ackSig = p.AckSig
	u.commitSig = p.CommitSig
	u.partialSig = p.PartialSig
	u.clearTimeout()

	proposer := p.Proposer == lntypes.Local

	switch {
	// A dyn_commit_sig was persisted, so the negotiation is past the
	// abort/timeout boundary and in the committing handoff.
	case p.HasCommitSig():
		u.state = StateCommitting

	// Proposer that received and verified the dyn_ack but had not yet sent
	// the dyn_commit_sig.
	case proposer && p.AckSig.IsSome():
		u.state = StateAccepted

	// Proposer that had sent dyn_propose and was still awaiting the dyn_ack.
	// The negotiation is still time-bounded.
	case proposer:
		u.state = StateAwaitingAck
		u.armTimeout()

	// Responder that had sent its dyn_ack and is waiting for the
	// dyn_commit_sig. The negotiation is still time-bounded.
	case p.AckSig.IsSome():
		u.state = StateAckSent
		u.armTimeout()

	// Responder without a persisted dyn_ack is not a valid state to retain:
	// the responder only persists after signing its dyn_ack.
	default:
		u.resetLocked()

		return fmt.Errorf("cannot restore responder session without a "+
			"persisted dyn_ack for ChannelID(%v)", u.chanID)
	}

	log.Infof("Restored dyn session for ChannelID(%v): role=%v, state=%s, "+
		"height=%d", u.chanID, u.role, u.state, u.nextHeight)

	return nil
}
