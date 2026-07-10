package dyn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// DefaultTimeout is the default negotiation timeout. If a negotiation
	// does not reach the commitment-handoff boundary within this window the
	// machine fails and the caller disconnects, matching the extension BOLT
	// requirement that stale pre-commit negotiations are abandoned.
	DefaultTimeout = 30 * time.Second
)

var (
	// ErrInvalidTransition is returned when an event is fed to the state
	// machine in a state that does not accept it. It wraps a description of
	// the offending (state, event) pair.
	ErrInvalidTransition = errors.New("invalid state transition")

	// ErrNegotiationInProgress is returned when a new local proposal is
	// requested while a negotiation is already in progress.
	ErrNegotiationInProgress = errors.New("dyn negotiation in progress")

	// ErrChannelNotReady is returned when a proposal is attempted or
	// accepted before both peers have exchanged channel_ready.
	ErrChannelNotReady = errors.New("channel_ready not exchanged")

	// ErrNotQuiescent is returned when a proposal is attempted or accepted
	// while the channel is not quiescent.
	ErrNotQuiescent = errors.New("channel is not quiescent")

	// ErrNotQuiescenceInitiator is returned when the proposer is not the
	// quiescence initiator, which the extension BOLT requires.
	ErrNotQuiescenceInitiator = errors.New(
		"proposer is not the quiescence initiator",
	)

	// ErrHTLCsActive is returned when a proposal is attempted or accepted
	// while either commitment carries HTLCs (including trimmed HTLCs).
	ErrHTLCsActive = errors.New("channel has active HTLCs")

	// ErrPendingUpdates is returned when a proposal is attempted or
	// accepted while there are uncommitted update-log entries.
	ErrPendingUpdates = errors.New("channel has pending updates")

	// ErrPendingShutdown is returned when a proposal is attempted or
	// accepted while a cooperative shutdown is in progress.
	ErrPendingShutdown = errors.New("channel has a pending shutdown")

	// ErrPendingSpliceOrRBF is returned when a proposal is attempted or
	// accepted while a splice or RBF is in progress.
	ErrPendingSpliceOrRBF = errors.New("channel has a pending splice/RBF")

	// ErrProposalMismatch is returned when a received dyn_commit_sig does
	// not carry the same proposal the responder previously acked.
	ErrProposalMismatch = errors.New(
		"dyn_commit_sig proposal does not match acked proposal",
	)
)

// ProposalRequest is a local request to renegotiate the channel parameters. RPC
// wiring that builds it lands in a later branch.
type ProposalRequest struct {
	// Params are the parameter changes to propose. Proposer-owned semantics
	// apply: the change updates the values we advertised at channel open,
	// except ChannelFlags, which is channel-wide.
	Params lnwallet.ChannelParams
}

// CommitHandoff is the ready-to-commit signal the machine emits once
// negotiation succeeds. It hands the agreed update to the link, which then
// performs the commitment dance (branch 6). This state machine deliberately
// does not perform the dance itself.
type CommitHandoff struct {
	// Proposer identifies who proposed the update. If it equals
	// lntypes.Local we are the proposer and must build and send the bundled
	// dyn_commit_sig; otherwise we are the responder and must verify and
	// apply the received one.
	Proposer lntypes.ChannelParty

	// Params is the agreed parameter set.
	Params lnwallet.ChannelParams

	// Proposal is the accepted proposal message.
	Proposal *lnwire.DynPropose

	// NextCommitHeight is the commitment number the update binds to.
	NextCommitHeight uint64

	// AckSig is the responder's dyn_ack signature: the signature we
	// verified (proposer handoff) or the one we produced (responder
	// handoff).
	AckSig lnwire.Sig

	// ReceivedCommit is the bundled dyn_commit_sig, set only on the
	// responder handoff.
	ReceivedCommit fn.Option[*lnwire.DynCommit]
}

// Transition is the result of feeding an event to the machine. It reports the
// states straddled by the event, any wire messages the machine sent to the peer
// as a result, an optional ready-to-commit handoff, and whether the caller
// should disconnect.
type Transition struct {
	// From is the state before the event was applied.
	From State

	// To is the state after the event was applied.
	To State

	// Messages are the wire messages the machine sent to the peer via the
	// configured MessageSender as a result of the event, in send order. It
	// is informational: the machine has already dispatched them.
	Messages []lnwire.Message

	// Handoff, when present, signals that negotiation succeeded and the link
	// should drive the commitment dance.
	Handoff fn.Option[CommitHandoff]

	// Disconnect is true when the caller should drop the connection, i.e.
	// on a fatal protocol error or a negotiation timeout.
	Disconnect bool
}

// Config bundles the injected dependencies of an Updater. Every edge of the
// machine is an interface so it unit-tests without a live link/peer.
type Config struct {
	// Signer signs and verifies dyn_ack signatures.
	Signer Signer

	// ChannelView is the read-only window onto the live channel.
	ChannelView ChannelView

	// Persister persists the accepted-proposal context.
	Persister Persister

	// Sender sends wire messages to the peer.
	Sender MessageSender

	// Clock provides the time reference for the negotiation timeout.
	Clock clock.Clock

	// Timeout is the negotiation timeout. If zero, DefaultTimeout is used.
	Timeout time.Duration

	// MaxLocalCSVDelay is our configured maximum acceptable to_self_delay,
	// used when validating a proposal.
	MaxLocalCSVDelay uint16
}

// Updater is the scoped dynamic-commitments negotiation state machine. It owns
// dyn-param negotiation only: it drives dyn_propose/dyn_ack/dyn_reject to an
// agreement and then hands off to the link for the commitment dance. It is safe
// for concurrent use.
type Updater struct {
	// mu serializes all transitions, so the machine advances one event at a
	// time and its observable state is always consistent.
	mu sync.Mutex

	// cfg holds the injected dependencies.
	cfg Config

	// chanID is the channel this updater manages, cached from the view.
	chanID lnwire.ChannelID

	// state is the current state of the machine.
	state State

	// role is the proposer party for the in-flight session: lntypes.Local
	// if we proposed, lntypes.Remote if the peer did. It is only meaningful
	// while a session is active.
	role lntypes.ChannelParty

	// proposal is the in-flight proposal message: the one we sent (proposer)
	// or the one we acked (responder).
	proposal *lnwire.DynPropose

	// nextHeight is the commitment number the in-flight update binds to.
	nextHeight uint64

	// ackSig holds the responder's dyn_ack signature once known.
	ackSig fn.Option[lnwire.Sig]

	// deadline is the time at which the in-flight negotiation times out. It
	// is only meaningful while timerArmed is true.
	deadline time.Time

	// timerArmed is true while a negotiation timeout is pending.
	timerArmed bool
}

// NewUpdater constructs an Updater in the Idle state from the given config.
func NewUpdater(cfg Config) *Updater {
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}

	return &Updater{
		cfg:    cfg,
		chanID: cfg.ChannelView.ChanID(),
		state:  StateIdle,
	}
}

// State returns the current state of the machine.
func (u *Updater) State() State {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.state
}

// InitProposal starts a local proposal. It enforces the send-side
// preconditions, validates the resulting channel, sends a dyn_propose, persists
// the proposal context, and moves the machine to AwaitingAck. On any
// precondition or validation failure it returns a typed error and leaves the
// machine Idle so the caller can surface the reason and exit quiescence.
func (u *Updater) InitProposal(ctx context.Context,
	req ProposalRequest) (*Transition, error) {

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.state != StateIdle {
		return nil, ErrNegotiationInProgress
	}

	// We are the proposer, so we must be the quiescence initiator and the
	// send-side preconditions must hold.
	if err := u.checkPreconditions(lntypes.Local); err != nil {
		return nil, err
	}

	view := u.cfg.ChannelView

	// Validate the whole resulting channel with proposer-owned semantics
	// before we commit to sending anything.
	err := req.Params.Validate(
		lntypes.Local, view.LocalChanCfg(), view.RemoteChanCfg(),
		view.Capacity(), u.cfg.MaxLocalCSVDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid local proposal: %w", err)
	}

	dp := req.Params.ToDynPropose(u.chanID)

	// Record the in-flight session state and persist the proposal context
	// at the dyn_propose boundary.
	u.role = lntypes.Local
	u.proposal = dp
	u.nextHeight = view.NextCommitHeight()
	u.ackSig = fn.None[lnwire.Sig]()

	if err := u.persist(ctx); err != nil {
		u.resetLocked()
		return nil, fmt.Errorf("persist proposal: %w", err)
	}

	t, err := u.applyEvent(eventPropose)
	if err != nil {
		return nil, err
	}

	if err := u.send(ctx, t, dp); err != nil {
		return nil, err
	}

	u.armTimeout()

	log.Infof("Sent dyn_propose for ChannelID(%v), height=%d", u.chanID,
		u.nextHeight)

	return t, nil
}

// RecvPropose handles an incoming dyn_propose from the peer, i.e. the responder
// flow. It enforces the accept-side preconditions and validates the proposal.
// On success it signs and sends a dyn_ack, persists its acceptance, and moves
// to AckSent. On any precondition or validation failure it sends a dyn_reject
// with the offending bits (a bare reject for non-parameter failures) and moves
// to Rejected.
func (u *Updater) RecvPropose(ctx context.Context,
	msg *lnwire.DynPropose) (*Transition, error) {

	u.mu.Lock()
	defer u.mu.Unlock()

	// A proposal is only legal from Idle. Receiving one at any other point
	// is a protocol violation.
	if u.state != StateIdle {
		return nil, fmt.Errorf("%w: dyn_propose in state %s",
			ErrInvalidTransition, u.state)
	}

	// The peer is the proposer, so it must be the quiescence initiator and
	// the accept-side preconditions must hold. A precondition failure is a
	// bare rejection (no specific parameter is at fault).
	if err := u.checkPreconditions(lntypes.Remote); err != nil {
		log.Warnf("Rejecting dyn_propose for ChannelID(%v): %v",
			u.chanID, err)

		return u.reject(ctx, msg, nil)
	}

	// Validate the whole resulting channel with proposer-owned semantics.
	// The peer's own dust applies to its config; the rest constrain us.
	view := u.cfg.ChannelView
	params := lnwallet.ChannelParamsFromDynPropose(msg)
	err := params.Validate(
		lntypes.Remote, view.LocalChanCfg(), view.RemoteChanCfg(),
		view.Capacity(), u.cfg.MaxLocalCSVDelay,
	)
	if err != nil {
		log.Warnf("Rejecting dyn_propose for ChannelID(%v): %v",
			u.chanID, err)

		return u.reject(ctx, msg, rejectBitsForErr(err))
	}

	// The proposal is acceptable. Sign our dyn_ack over the exact proposal
	// TLVs bound to the next commitment height and chain.
	tlvs, err := msg.SerializeTlvData()
	if err != nil {
		return nil, fmt.Errorf("serialize proposal tlvs: %w", err)
	}

	nextHeight := view.NextCommitHeight()
	sig, err := u.cfg.Signer.SignDynAck(
		view.ChainHash(), u.chanID, nextHeight, tlvs,
	)
	if err != nil {
		return nil, fmt.Errorf("sign dyn_ack: %w", err)
	}

	// Record the in-flight session state and persist our acceptance before
	// the dyn_ack goes out on the wire.
	u.role = lntypes.Remote
	u.proposal = msg
	u.nextHeight = nextHeight
	u.ackSig = fn.Some(sig)

	if err := u.persist(ctx); err != nil {
		u.resetLocked()
		return nil, fmt.Errorf("persist acceptance: %w", err)
	}

	t, err := u.applyEvent(eventProposeOK)
	if err != nil {
		return nil, err
	}

	ack := &lnwire.DynAck{ChanID: u.chanID, Sig: sig}
	if err := u.send(ctx, t, ack); err != nil {
		return nil, err
	}

	u.armTimeout()

	log.Infof("Accepted dyn_propose for ChannelID(%v), sent dyn_ack, "+
		"height=%d", u.chanID, nextHeight)

	return t, nil
}

// RecvAck handles an incoming dyn_ack from the peer, i.e. the proposer flow. It
// verifies the signature over the proposal we sent; on success it records the
// signature, persists the received-ack context, and emits a ready-to-commit
// handoff. On a verification failure it moves to Failed and signals disconnect.
func (u *Updater) RecvAck(ctx context.Context,
	msg *lnwire.DynAck) (*Transition, error) {

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.state != StateAwaitingAck {
		return nil, fmt.Errorf("%w: dyn_ack in state %s",
			ErrInvalidTransition, u.state)
	}

	tlvs, err := u.proposal.SerializeTlvData()
	if err != nil {
		return nil, fmt.Errorf("serialize proposal tlvs: %w", err)
	}

	view := u.cfg.ChannelView
	err = u.cfg.Signer.VerifyDynAck(
		msg.Sig, view.ChainHash(), u.chanID, u.nextHeight, tlvs,
	)
	if err != nil {
		log.Errorf("Invalid dyn_ack for ChannelID(%v): %v", u.chanID,
			err)

		return u.fail(ctx)
	}

	// The ack is valid. Persist the received-ack context so a reconnect can
	// tell an acked negotiation from an un-acked one.
	u.ackSig = fn.Some(msg.Sig)
	if err := u.persist(ctx); err != nil {
		return nil, fmt.Errorf("persist received ack: %w", err)
	}

	t, err := u.applyEvent(eventAckOK)
	if err != nil {
		return nil, err
	}

	// Negotiation succeeded; the negotiation timer no longer applies.
	u.clearTimeout()

	t.Handoff = fn.Some(u.handoff(fn.None[*lnwire.DynCommit]()))

	log.Infof("Received valid dyn_ack for ChannelID(%v), ready to commit",
		u.chanID)

	return t, nil
}

// RecvReject handles an incoming dyn_reject from the peer, i.e. the proposer
// flow. It moves the machine to Rejected and forgets the negotiation; per the
// locked design decisions a retry requires fresh quiescence.
func (u *Updater) RecvReject(ctx context.Context,
	msg *lnwire.DynReject) (*Transition, error) {

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.state != StateAwaitingAck {
		return nil, fmt.Errorf("%w: dyn_reject in state %s",
			ErrInvalidTransition, u.state)
	}

	t, err := u.applyEvent(eventReject)
	if err != nil {
		return nil, err
	}

	u.clearTimeout()
	if err := u.forget(ctx); err != nil {
		return nil, err
	}

	log.Infof("Peer rejected dyn_propose for ChannelID(%v)", u.chanID)

	return t, nil
}

// RecvCommit handles an incoming bundled dyn_commit_sig from the peer, i.e. the
// responder flow. It checks that the bundled proposal matches the one we acked,
// then moves to Committing and emits a ready-to-commit handoff carrying the
// received message so the link can verify the commitment signature and drive
// the dance. On a mismatch it moves to Failed and signals disconnect.
func (u *Updater) RecvCommit(ctx context.Context,
	msg *lnwire.DynCommit) (*Transition, error) {

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.state != StateAckSent {
		return nil, fmt.Errorf("%w: dyn_commit_sig in state %s",
			ErrInvalidTransition, u.state)
	}

	// Defend against a peer that bundles a different proposal than the one
	// we acked: the accepted TLVs must be byte-identical.
	got, err := msg.DynPropose.SerializeTlvData()
	if err != nil {
		return nil, fmt.Errorf("serialize commit proposal: %w", err)
	}
	want, err := u.proposal.SerializeTlvData()
	if err != nil {
		return nil, fmt.Errorf("serialize acked proposal: %w", err)
	}
	if !bytes.Equal(got, want) {
		log.Errorf("dyn_commit_sig proposal mismatch for "+
			"ChannelID(%v)", u.chanID)

		return u.fail(ctx)
	}

	t, err := u.applyEvent(eventCommitRecv)
	if err != nil {
		return nil, err
	}

	u.clearTimeout()

	t.Handoff = fn.Some(u.handoff(fn.Some(msg)))

	log.Infof("Received dyn_commit_sig for ChannelID(%v), ready to commit",
		u.chanID)

	return t, nil
}

// MarkCommitSent records that the proposer has sent the bundled dyn_commit_sig,
// completing this machine's role and moving it to Committing. From here the
// link owns the commitment dance and the negotiation is no longer abortable on
// a reconnect.
func (u *Updater) MarkCommitSent(_ context.Context) (*Transition, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.state != StateAccepted {
		return nil, fmt.Errorf("%w: commit-sent in state %s",
			ErrInvalidTransition, u.state)
	}

	t, err := u.applyEvent(eventCommitSent)
	if err != nil {
		return nil, err
	}

	log.Infof("Sent dyn_commit_sig for ChannelID(%v), dance handed off",
		u.chanID)

	return t, nil
}

// CheckTimeout advances the negotiation timeout. The caller ticks it (for
// example from a select loop or a periodic sweep); the machine consults its
// injected clock. If a waiting negotiation has passed its deadline the machine
// moves to Failed, forgets the negotiation, and returns a transition that
// signals disconnect. Otherwise it returns a no-op transition (From == To).
func (u *Updater) CheckTimeout(ctx context.Context) (*Transition, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !u.timerArmed || u.cfg.Clock.Now().Before(u.deadline) {
		return &Transition{From: u.state, To: u.state}, nil
	}

	log.Warnf("Dyn negotiation timed out for ChannelID(%v) in state %s",
		u.chanID, u.state)

	from := u.state
	t, err := u.applyEvent(eventTimeout)
	if err != nil {
		return nil, err
	}
	t.From = from
	t.Disconnect = true

	if err := u.forget(ctx); err != nil {
		return nil, err
	}

	return t, nil
}

// Reset returns the machine to Idle and drops any in-flight session state,
// after clearing persisted context. It is used to begin a fresh negotiation
// after a terminal state, which requires fresh quiescence.
func (u *Updater) Reset(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	err := u.cfg.Persister.DeleteAcceptedProposal(ctx, u.chanID)
	if err != nil {
		return fmt.Errorf("delete proposal: %w", err)
	}

	u.resetLocked()

	return nil
}

// checkPreconditions enforces the eligibility rules that must hold before a
// proposal is sent or accepted, given the party that is the proposer. It
// returns a typed error naming the first unmet precondition.
func (u *Updater) checkPreconditions(proposer lntypes.ChannelParty) error {
	view := u.cfg.ChannelView

	if !view.BothChannelReady() {
		return ErrChannelNotReady
	}

	if !view.IsQuiescent() {
		return ErrNotQuiescent
	}

	// The proposer must be the quiescence initiator.
	initiator, err := view.QuiescenceInitiator().Unpack()
	if err != nil {
		return ErrNotQuiescent
	}
	if initiator != proposer {
		return ErrNotQuiescenceInitiator
	}

	if view.HasHTLCs() {
		return ErrHTLCsActive
	}

	if view.HasPendingUpdates() {
		return ErrPendingUpdates
	}

	if view.HasPendingShutdown() {
		return ErrPendingShutdown
	}

	if view.HasPendingSpliceOrRBF() {
		return ErrPendingSpliceOrRBF
	}

	return nil
}

// reject sends a dyn_reject with the given rejection bits (a bare reject when
// bits is empty) and moves the machine to Rejected. It is the responder's
// unhappy path.
func (u *Updater) reject(ctx context.Context, msg *lnwire.DynPropose,
	bits []lnwire.DynRejectBit) (*Transition, error) {

	t, err := u.applyEvent(eventProposeBad)
	if err != nil {
		return nil, err
	}

	dr := lnwire.NewDynReject(msg.ChanID, bits...)
	if err := u.send(ctx, t, dr); err != nil {
		return nil, err
	}

	return t, nil
}

// fail moves the machine to Failed, forgets the negotiation, and returns a
// transition that signals disconnect. It is the shared unrecoverable-error
// path.
func (u *Updater) fail(ctx context.Context) (*Transition, error) {
	t, err := u.applyEvent(eventFail)
	if err != nil {
		return nil, err
	}

	u.clearTimeout()
	if err := u.forget(ctx); err != nil {
		return nil, err
	}

	t.Disconnect = true

	return t, nil
}

// handoff builds the ready-to-commit handoff for the current in-flight session.
func (u *Updater) handoff(
	commit fn.Option[*lnwire.DynCommit]) CommitHandoff {

	return CommitHandoff{
		Proposer:         u.role,
		Params:           lnwallet.ChannelParamsFromDynPropose(u.proposal),
		Proposal:         u.proposal,
		NextCommitHeight: u.nextHeight,
		AckSig:           u.ackSig.UnwrapOr(lnwire.Sig{}),
		ReceivedCommit:   commit,
	}
}

// applyEvent runs the pure transition function for the given event, updates the
// machine's state on success, and returns a Transition describing the state
// change with no messages or handoff attached yet.
func (u *Updater) applyEvent(e eventType) (*Transition, error) {
	from := u.state
	to, err := advance(from, e)
	if err != nil {
		return nil, err
	}

	u.state = to

	log.Tracef("ChannelID(%v): %s --%s--> %s", u.chanID, from, e, to)

	return &Transition{From: from, To: to}, nil
}

// send dispatches the given messages via the configured sender and records them
// on the transition. It is a no-op when there are no messages.
func (u *Updater) send(ctx context.Context, t *Transition,
	msgs ...lnwire.Message) error {

	if len(msgs) == 0 {
		return nil
	}

	if err := u.cfg.Sender.SendMessage(ctx, msgs...); err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	t.Messages = append(t.Messages, msgs...)

	return nil
}

// persist writes the current in-flight session as an AcceptedProposal.
func (u *Updater) persist(ctx context.Context) error {
	return u.cfg.Persister.StoreAcceptedProposal(
		ctx, u.chanID, AcceptedProposal{
			Proposer:         u.role,
			Proposal:         u.proposal,
			NextCommitHeight: u.nextHeight,
			AckSig:           u.ackSig,
		},
	)
}

// forget clears any persisted context and drops the in-flight session data,
// while preserving the current (typically terminal) state so the caller can
// still observe why the negotiation ended.
func (u *Updater) forget(ctx context.Context) error {
	err := u.cfg.Persister.DeleteAcceptedProposal(ctx, u.chanID)
	if err != nil {
		return fmt.Errorf("delete proposal: %w", err)
	}

	u.clearInflight()

	return nil
}

// clearInflight drops the in-flight session data (role, proposal, height, ack
// signature, and timer) without touching the state. The caller must hold u.mu.
func (u *Updater) clearInflight() {
	u.role = lntypes.Local
	u.proposal = nil
	u.nextHeight = 0
	u.ackSig = fn.None[lnwire.Sig]()
	u.clearTimeout()
}

// resetLocked drops the in-flight session data and returns the machine to Idle.
// The caller must hold u.mu.
func (u *Updater) resetLocked() {
	u.clearInflight()
	u.state = StateIdle
}

// armTimeout arms the negotiation timeout relative to the injected clock.
func (u *Updater) armTimeout() {
	u.deadline = u.cfg.Clock.Now().Add(u.cfg.Timeout)
	u.timerArmed = true
}

// clearTimeout disarms the negotiation timeout.
func (u *Updater) clearTimeout() {
	u.timerArmed = false
}

// rejectBitsForErr maps a ValidateChannelParams error to the dyn_reject bits
// that identify the offending parameter. It returns nil for errors that do not
// map to a specific parameter, which yields a bare rejection.
func rejectBitsForErr(err error) []lnwire.DynRejectBit {
	switch {
	case errors.Is(err, lnwallet.ErrDynDustLimitTooSmall),
		errors.Is(err, lnwallet.ErrDynDustLimitTooLarge):

		return []lnwire.DynRejectBit{lnwire.DynRejectDustLimit}

	case errors.Is(err, lnwallet.ErrDynReserveBelowDust),
		errors.Is(err, lnwallet.ErrDynReserveTooLarge):

		return []lnwire.DynRejectBit{lnwire.DynRejectChannelReserve}

	case errors.Is(err, lnwallet.ErrDynCsvDelayTooLarge):
		return []lnwire.DynRejectBit{lnwire.DynRejectCsvDelay}

	case errors.Is(err, lnwallet.ErrDynMaxAcceptedTooLarge):
		return []lnwire.DynRejectBit{lnwire.DynRejectMaxAcceptedHTLCs}

	case errors.Is(err, lnwallet.ErrDynHtlcMinTooLarge):
		return []lnwire.DynRejectBit{lnwire.DynRejectHtlcMinimum}

	default:
		return nil
	}
}
