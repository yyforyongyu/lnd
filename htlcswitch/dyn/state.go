package dyn

import "fmt"

// State enumerates the states of the dynamic-commitments negotiation state
// machine. The machine negotiates a single parameter change per session; once a
// terminal state is reached the caller must Reset (after a fresh quiescence
// session) before a new negotiation can begin.
//
// The machine is role-aware: a session is driven either as the proposer (we
// sent dyn_propose) or as the responder (the peer sent dyn_propose). The role
// is fixed for the lifetime of a session.
type State uint8

const (
	// StateIdle is the initial and reset state. No negotiation is in
	// progress and the machine is ready to either start a local proposal or
	// accept a remote one.
	StateIdle State = iota

	// StateAwaitingAck is the proposer state entered after we send a
	// dyn_propose. We are waiting for the responder to reply with a dyn_ack
	// (accept) or a dyn_reject (reject), or for the negotiation to time
	// out.
	StateAwaitingAck

	// StateAckSent is the responder state entered after we validate an
	// incoming dyn_propose and send our dyn_ack. We are waiting for the
	// proposer's bundled dyn_commit_sig, or for the negotiation to time
	// out. Per the extension BOLT the responder retains this acceptance
	// until it observes the commit, a superseding proposal, or a timeout.
	StateAckSent

	// StateAccepted is the proposer state entered after we receive and
	// verify the responder's dyn_ack. Negotiation has succeeded and the
	// machine emits a ready-to-commit handoff: the link is expected to build
	// and send the bundled dyn_commit_sig and then call MarkCommitSent.
	StateAccepted

	// StateCommitting is the committing-handoff state. Negotiation is
	// complete and the commitment dance has been handed off to the link
	// (branch 6 owns the dance). The proposer enters it once it has sent the
	// bundled dyn_commit_sig; the responder enters it once it has received
	// one. This is terminal for this state machine.
	StateCommitting

	// StateRejected is a terminal state entered when a dyn_reject is sent or
	// received. Per the locked design decisions a rejection ends the current
	// quiescence session; a retry requires fresh quiescence.
	StateRejected

	// StateFailed is a terminal state entered on an unrecoverable error: an
	// invalid dyn_ack signature, an illegal protocol message, or a
	// negotiation timeout. The caller is expected to disconnect.
	StateFailed
)

// String returns a human-readable representation of the state.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "Idle"

	case StateAwaitingAck:
		return "AwaitingAck"

	case StateAckSent:
		return "AckSent"

	case StateAccepted:
		return "Accepted"

	case StateCommitting:
		return "Committing"

	case StateRejected:
		return "Rejected"

	case StateFailed:
		return "Failed"

	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// IsTerminal returns true if no further transitions are possible from the state
// without a Reset.
func (s State) IsTerminal() bool {
	switch s {
	case StateCommitting, StateRejected, StateFailed:
		return true

	default:
		return false
	}
}

// eventType enumerates the inputs that drive the state machine. An event is the
// already-resolved outcome of processing a request or a wire message: for
// example, the decision of whether an incoming proposal is acceptable is made
// by the caller before it is fed to the transition function as either
// eventProposeOK or eventProposeBad. This keeps the transition function a
// total, side-effect-free function of (state, event).
type eventType uint8

const (
	// eventPropose is a local request to start a proposal. Valid from Idle
	// and moves the machine to AwaitingAck (proposer role).
	eventPropose eventType = iota

	// eventProposeOK is a remote dyn_propose that passed preconditions and
	// validation. Valid from Idle and moves the machine to AckSent
	// (responder role).
	eventProposeOK

	// eventProposeBad is a remote dyn_propose that failed preconditions or
	// validation. Valid from Idle and moves the machine to Rejected.
	eventProposeBad

	// eventAckOK is a valid dyn_ack received by the proposer. Valid from
	// AwaitingAck and moves the machine to Accepted.
	eventAckOK

	// eventReject is a dyn_reject received by the proposer. Valid from
	// AwaitingAck and moves the machine to Rejected.
	eventReject

	// eventCommitSent signals the proposer has sent the bundled
	// dyn_commit_sig. Valid from Accepted and moves the machine to
	// Committing.
	eventCommitSent

	// eventCommitRecv signals the responder has received the bundled
	// dyn_commit_sig. Valid from AckSent and moves the machine to
	// Committing.
	eventCommitRecv

	// eventTimeout signals the negotiation timeout has elapsed. Valid from
	// the two waiting states (AwaitingAck, AckSent) and moves the machine to
	// Failed.
	eventTimeout

	// eventFail signals an unrecoverable protocol error (bad signature,
	// mismatched commit). Valid from any active state and moves the machine
	// to Failed.
	eventFail
)

// String returns a human-readable representation of the event.
func (e eventType) String() string {
	switch e {
	case eventPropose:
		return "Propose"

	case eventProposeOK:
		return "ProposeOK"

	case eventProposeBad:
		return "ProposeBad"

	case eventAckOK:
		return "AckOK"

	case eventReject:
		return "Reject"

	case eventCommitSent:
		return "CommitSent"

	case eventCommitRecv:
		return "CommitRecv"

	case eventTimeout:
		return "Timeout"

	case eventFail:
		return "Fail"

	default:
		return fmt.Sprintf("Unknown(%d)", e)
	}
}

// transitions is the documented transition function of the state machine,
// expressed as a table mapping the current state and an event to the next
// state. An entry that is absent means the event is illegal in that state and
// advance will return ErrInvalidTransition. Terminal states have no outgoing
// entries.
//
// The table intentionally does not encode conditional targets (for example the
// accept-vs-reject decision for an incoming proposal). Those decisions are made
// by the caller, which then feeds the resolved event (eventProposeOK vs
// eventProposeBad) to advance.
var transitions = map[State]map[eventType]State{
	StateIdle: {
		eventPropose:    StateAwaitingAck,
		eventProposeOK:  StateAckSent,
		eventProposeBad: StateRejected,
	},
	StateAwaitingAck: {
		eventAckOK:   StateAccepted,
		eventReject:  StateRejected,
		eventTimeout: StateFailed,
		eventFail:    StateFailed,
	},
	StateAckSent: {
		eventCommitRecv: StateCommitting,
		eventTimeout:    StateFailed,
		eventFail:       StateFailed,
	},
	StateAccepted: {
		eventCommitSent: StateCommitting,
		eventFail:       StateFailed,
	},
	StateCommitting: {},
	StateRejected:   {},
	StateFailed:     {},
}

// advance is the pure transition function. Given the current state and an
// event it returns the next state, or ErrInvalidTransition if the event is not
// legal in the current state. It performs no side effects.
func advance(from State, e eventType) (State, error) {
	next, ok := transitions[from][e]
	if !ok {
		return from, fmt.Errorf("%w: event %s in state %s",
			ErrInvalidTransition, e, from)
	}

	return next, nil
}
