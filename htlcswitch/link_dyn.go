package htlcswitch

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/dyn"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// defaultMaxLinkCSVDelay is the fallback maximum to_self_delay accepted in a
// dynamic-commitments proposal when the link config does not specify one. It
// matches the daemon-wide default maximum local CSV delay.
const defaultMaxLinkCSVDelay uint16 = 2016

// errDynDisabled is returned when a dynamic-commitments operation is requested
// on a link that does not have the feature enabled.
var errDynDisabled = fmt.Errorf("dynamic commitments not enabled for channel")

// dynProposalReq is a locally-initiated dynamic-commitments proposal request
// handed to the htlcManager event loop via InitDynProposal.
type dynProposalReq struct {
	// req is the parameter-change proposal to negotiate.
	req dyn.ProposalRequest

	// resp receives the outcome of starting the negotiation: nil once the
	// dyn_propose has been sent, or an error if quiescence or a
	// precondition failed. It is buffered so the loop never blocks on it.
	resp chan error
}

// dynAckSigner is the concrete dyn.Signer used by the link. It signs dyn_ack
// messages with our node identity key and verifies the peer's dyn_ack against
// its node identity key, delegating to the domain-separated lnwire helpers.
type dynAckSigner struct {
	// nodePriv is our node identity private key, used to produce dyn_ack
	// signatures.
	nodePriv *btcec.PrivateKey

	// peerPub is the peer's node identity public key, used to verify the
	// dyn_ack signatures they send us.
	peerPub *btcec.PublicKey
}

// A compile-time assertion that dynAckSigner implements dyn.Signer.
var _ dyn.Signer = (*dynAckSigner)(nil)

// newDynAckSigner constructs a dyn.Signer over the given node identity private
// key and peer node identity public key.
func newDynAckSigner(nodePriv *btcec.PrivateKey,
	peerPub *btcec.PublicKey) *dynAckSigner {

	return &dynAckSigner{
		nodePriv: nodePriv,
		peerPub:  peerPub,
	}
}

// SignDynAck produces our dyn_ack signature over the accepted proposal.
//
// NOTE: Part of the dyn.Signer interface.
func (s *dynAckSigner) SignDynAck(chainHash chainhash.Hash,
	chanID lnwire.ChannelID, nextCommitHeight uint64,
	proposalTLVs []byte) (lnwire.Sig, error) {

	return lnwire.SignDynAck(
		s.nodePriv, chainHash, chanID, nextCommitHeight, proposalTLVs,
	)
}

// VerifyDynAck verifies the peer's dyn_ack signature over the proposal we sent.
//
// NOTE: Part of the dyn.Signer interface.
func (s *dynAckSigner) VerifyDynAck(sig lnwire.Sig, chainHash chainhash.Hash,
	chanID lnwire.ChannelID, nextCommitHeight uint64,
	proposalTLVs []byte) error {

	return lnwire.VerifyDynAck(
		s.peerPub, sig, chainHash, chanID, nextCommitHeight,
		proposalTLVs,
	)
}

// dynMessageSender is the concrete dyn.MessageSender used by the link. It sends
// dyn wire messages to the peer over the same send path the link uses for all
// other channel messages.
type dynMessageSender struct {
	// peer is the peer connection the messages are sent to.
	peer lnpeer.Peer
}

// A compile-time assertion that dynMessageSender implements dyn.MessageSender.
var _ dyn.MessageSender = (*dynMessageSender)(nil)

// SendMessage sends the given messages to the peer, in order.
//
// NOTE: Part of the dyn.MessageSender interface.
func (s *dynMessageSender) SendMessage(_ context.Context,
	msgs ...lnwire.Message) error {

	return s.peer.SendMessage(false, msgs...)
}

// dynChannelView is the concrete dyn.ChannelView used by the link. It exposes a
// read-only window onto the live channelLink/LightningChannel state that the
// dyn.Updater consults for its eligibility preconditions and proposal
// validation.
type dynChannelView struct {
	// link is the channel link this view reads from.
	link *channelLink
}

// A compile-time assertion that dynChannelView implements dyn.ChannelView.
var _ dyn.ChannelView = (*dynChannelView)(nil)

// ChanID returns the channel id this view is for.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) ChanID() lnwire.ChannelID {
	return v.link.ChanID()
}

// ChainHash returns the genesis hash of the chain the channel is on.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) ChainHash() chainhash.Hash {
	return v.link.channel.State().ChainHash
}

// LocalChanCfg returns our current channel configuration.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) LocalChanCfg() channeldb.ChannelConfig {
	return v.link.channel.State().LocalChanCfg
}

// RemoteChanCfg returns the peer's current channel configuration.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) RemoteChanCfg() channeldb.ChannelConfig {
	return v.link.channel.State().RemoteChanCfg
}

// Capacity returns the total channel capacity.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) Capacity() btcutil.Amount {
	return v.link.channel.State().Capacity
}

// NextCommitHeight returns the commitment number the dynamic update will be
// committed at. At quiescence both commitment chains are synchronized and have
// no pending updates, so the next local commitment height is shared by both
// peers and is the value bound into the dyn_ack digest.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) NextCommitHeight() uint64 {
	return v.link.channel.State().LocalCommitment.CommitHeight + 1
}

// BothChannelReady returns true only once both peers have sent and received
// channel_ready and the reestablish dance has completed. This mirrors the
// link's own eligibility check without the quiescence gate (which is
// necessarily active while a dyn update is negotiated).
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) BothChannelReady() bool {
	return v.link.channel.RemoteNextRevocation() != nil &&
		v.link.channel.ShortChanID() != hop.Source &&
		v.link.isReestablished()
}

// IsQuiescent returns true if the channel has been driven to quiescence.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) IsQuiescent() bool {
	return v.link.quiescer.IsQuiescent()
}

// QuiescenceInitiator returns the party that initiated quiescence.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	return v.link.quiescer.QuiescenceInitiator()
}

// HasHTLCs returns true if either commitment has any HTLCs, including trimmed
// HTLCs. Trimmed HTLCs (those below the applicable dust limit) carry no output
// but are still recorded on the persisted commitment with an OutputIndex of -1,
// so counting the commitment HTLC sets captures them too.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) HasHTLCs() bool {
	state := v.link.channel.State()

	return len(state.LocalCommitment.Htlcs) > 0 ||
		len(state.RemoteCommitment.Htlcs) > 0
}

// HasPendingUpdates returns true if there are update-log entries that are not
// yet bilaterally committed on both chains.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) HasPendingUpdates() bool {
	return v.link.channel.OweCommitment() ||
		v.link.channel.NeedCommitment()
}

// HasPendingShutdown returns true if a cooperative shutdown is in progress on
// the channel.
//
// NOTE: for this branch we surface the shutdown state the link tracks, i.e.
// whether we have previously sent a shutdown for the channel. Full bilateral
// shutdown coordination lives in the peer, which will not initiate a dyn update
// while a shutdown is outstanding.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) HasPendingShutdown() bool {
	return v.link.cfg.PreviouslySentShutdown.IsSome()
}

// HasPendingSpliceOrRBF returns true if a splice or an RBF of a pending
// funding/close is in progress on the channel.
//
// NOTE: the link does not track splice/RBF state; that coordination lives in
// the peer/funding layer, which gates dyn initiation. There is no in-link
// signal to consult today, so this conservatively returns false and relies on
// the higher layer not to drive a dyn update mid-splice.
//
// NOTE: Part of the dyn.ChannelView interface.
func (v *dynChannelView) HasPendingSpliceOrRBF() bool {
	return false
}

// newDynUpdater constructs the dyn.Updater for this link, wiring the concrete
// adapters over the live link/channel/peer objects. It uses a durable,
// channeldb-backed Persister so accepted-proposal context survives a restart
// and drives the reconnect resume/forget decisions in handleDynReestablish. The
// same Persister instance is cached on the link so the reestablish path can
// consult it directly.
func (l *channelLink) newDynUpdater() *dyn.Updater {
	l.dynPersister = newDBDynPersister(l.channel.State().Db)

	return dyn.NewUpdater(dyn.Config{
		Signer:           l.cfg.DynAckSigner,
		ChannelView:      &dynChannelView{link: l},
		Persister:        l.dynPersister,
		Sender:           &dynMessageSender{peer: l.cfg.Peer},
		Clock:            clock.NewDefaultClock(),
		MaxLocalCSVDelay: l.dynMaxCSVDelay(),
	})
}

// dynMaxCSVDelay returns the maximum to_self_delay accepted in a
// dynamic-commitments proposal for this link.
func (l *channelLink) dynMaxCSVDelay() uint16 {
	if l.cfg.MaxLocalCSVDelay == 0 {
		return defaultMaxLinkCSVDelay
	}

	return l.cfg.MaxLocalCSVDelay
}

// InitDynProposal requests a locally-initiated dynamic-commitments parameter
// change. It hands the request to the htlcManager event loop, which drives the
// channel to quiescence (with us as the quiescence initiator) and then starts
// the negotiation. It returns a channel that receives nil once the negotiation
// has been kicked off (dyn_propose sent), or an error if the feature is
// disabled or a precondition failed.
//
// This is the entry point the RPC drives; tests exercise it directly.
//
// NOTE: Part of the ChannelUpdateHandler interface.
func (l *channelLink) InitDynProposal(
	req dyn.ProposalRequest) <-chan error {

	resp := make(chan error, 1)

	if l.updater == nil {
		resp <- errDynDisabled

		return resp
	}

	dReq := dynProposalReq{
		req:  req,
		resp: resp,
	}

	select {
	case l.dynProposalReqs <- dReq:
	case <-l.cg.Done():
		resp <- ErrLinkShuttingDown
	}

	return resp
}

// handleDynProposalReq starts servicing a locally-initiated dyn proposal from
// within the event loop. It stashes the request, drives quiescence with us as
// the initiator, and starts the negotiation immediately if the channel is
// already quiescent. If quiescence cannot begin yet the request stays pending
// and is picked up by maybeStartDynNegotiation once quiescence completes.
//
// NOTE: MUST be called from the htlcManager event loop.
func (l *channelLink) handleDynProposalReq(ctx context.Context,
	req dynProposalReq) {

	if l.updater == nil {
		l.replyDynProposal(req, errDynDisabled)

		return
	}

	// Refuse to clobber an in-flight negotiation or a pending request.
	if l.pendingDynProposal.IsSome() ||
		l.updater.State() != dyn.StateIdle {

		l.replyDynProposal(req, dyn.ErrNegotiationInProgress)

		return
	}

	// Stash the request so we can start it once we reach quiescence.
	l.pendingDynProposal = fn.Some(req)

	// Drive quiescence with us as the initiator. We must be the quiescence
	// initiator to be the dyn proposer.
	stfuReq, _ := fn.NewReq[fn.Unit, fn.Result[lntypes.ChannelParty]](
		fn.Unit{},
	)
	l.quiescer.InitStfu(stfuReq)

	if err := l.quiescer.SendOwedStfu(); err != nil {
		l.pendingDynProposal = fn.None[dynProposalReq]()
		l.replyDynProposal(req, err)
		l.stfuFailf("dyn SendOwedStfu: %v", err)

		return
	}

	// If the channel is already quiescent (for example the peer initiated
	// concurrently and we won the initiator tie), start right away.
	// Otherwise the negotiation begins when the peer's stfu arrives.
	l.maybeStartDynNegotiation(ctx)
}

// maybeStartDynNegotiation starts a pending locally-initiated dyn negotiation
// if the channel has reached quiescence. It is invoked at each point quiescence
// may have just completed. It is a no-op if the feature is disabled, no request
// is pending, or the channel is not yet quiescent.
//
// NOTE: MUST be called from the htlcManager event loop.
func (l *channelLink) maybeStartDynNegotiation(ctx context.Context) {
	if l.updater == nil {
		return
	}

	l.pendingDynProposal.WhenSome(func(req dynProposalReq) {
		if !l.quiescer.IsQuiescent() {
			return
		}

		// We are about to act on the request; clear it first so a
		// failure path does not leave it pending.
		l.pendingDynProposal = fn.None[dynProposalReq]()

		t, err := l.updater.InitProposal(ctx, req.req)
		l.replyDynProposal(req, err)
		if err != nil {
			l.log.Warnf("Unable to start dyn proposal for "+
				"ChannelID(%v): %v", l.ChanID(), err)

			// The negotiation never got off the ground, so there is
			// nothing to commit; release the channel from
			// quiescence.
			l.quiescer.Resume()

			return
		}

		l.processDynTransition(ctx, t, nil)
	})
}

// replyDynProposal delivers the outcome of a proposal request back to the
// caller. The response channel is buffered, so this never blocks.
func (l *channelLink) replyDynProposal(req dynProposalReq, err error) {
	select {
	case req.resp <- err:
	default:
	}
}

// handleDynMessage routes an incoming dyn wire message into the mounted
// dyn.Updater and processes the resulting transition. When the feature is not
// enabled for this channel the message is logged and ignored; a well-behaved
// peer will not send dyn messages unless the feature was negotiated.
//
// NOTE: MUST be called from the htlcManager event loop.
func (l *channelLink) handleDynMessage(ctx context.Context,
	msg lnwire.Message) error {

	if l.updater == nil {
		l.log.Warnf("Received %v but dynamic commitments not enabled "+
			"for ChannelID(%v); ignoring", msg.MsgType(),
			l.ChanID())

		return nil
	}

	var (
		t   *dyn.Transition
		err error
	)

	switch m := msg.(type) {
	case *lnwire.DynPropose:
		t, err = l.updater.RecvPropose(ctx, m)

	case *lnwire.DynAck:
		t, err = l.updater.RecvAck(ctx, m)

	case *lnwire.DynReject:
		t, err = l.updater.RecvReject(ctx, m)

	case *lnwire.DynCommit:
		t, err = l.updater.RecvCommit(ctx, m)

	default:
		return fmt.Errorf("unexpected dyn message type %T", msg)
	}

	l.processDynTransition(ctx, t, err)

	return err
}

// processDynTransition acts on the result of feeding an event to the
// dyn.Updater. A processing error or a transition that requests disconnect
// drops the connection, matching how quiescence-protocol violations are
// handled. When the transition carries a ready-to-commit handoff, the
// commitment dance is driven.
//
// NOTE: MUST be called from the htlcManager event loop.
func (l *channelLink) processDynTransition(ctx context.Context,
	t *dyn.Transition, err error) {

	if err != nil {
		// A processing error on an incoming dyn message is a protocol
		// violation by the peer during quiescence; drop the connection
		// as we do for other stfu-protocol violations.
		l.dynFailf("dyn negotiation error: %v", err)

		return
	}

	if t == nil {
		return
	}

	if t.Disconnect {
		l.dynFailf("dyn negotiation requires disconnect (%s->%s)",
			t.From, t.To)

		return
	}

	t.Handoff.WhenSome(func(h dyn.CommitHandoff) {
		if herr := l.handleDynCommitHandoff(ctx, h); herr != nil {
			// A failure to send, sign, stage, or record the bundled
			// dyn_commit_sig aborts the dance mid-flight. Reset the
			// negotiation by failing the link so the peer disconnects
			// cleanly and a fresh reestablish recovers, rather than
			// leaving the channel quiesced or with a stale
			// pendingDynDance.
			l.dynFailf("dyn commit handoff failed: %v", herr)
		}
	})
}

// dynDance carries the state needed to finish a dynamic-commitments commitment
// dance once both commitment chains have locked in the agreed parameter change.
type dynDance struct {
	// handoff is the agreed parameter update handed off by the negotiation
	// state machine.
	handoff dyn.CommitHandoff

	// lockIn is, per commitment chain, the last height the outgoing
	// (pre-update) parameters applied to. Both chains lock in the new
	// parameters at the bound commitment height, so this is that height
	// minus one on each side.
	lockIn lntypes.Dual[uint64]
}

// handleDynCommitHandoff drives the bundled dyn_commit_sig commitment dance
// once negotiation has produced an agreed parameter update. Negotiation is
// owned by the dyn.Updater; from here the link + wallet execute the dance and
// apply the params once both chains lock in.
//
// The agreed update is staged into the channel's update log so the ordinary
// SignNextCommitment / ReceiveNewCommitment machinery renders and validates the
// next commitment under the new params with zero HTLCs. The proposer sends the
// bundled dyn_commit_sig; the responder feeds the received one through the
// ordinary commitment_signed handler. This works for both non-taproot channels
// (ECDSA commit_signature) and taproot channels (musig2
// partial_signature_with_nonce, signed against the channel's ambient
// next_local_nonce). The remaining revoke_and_ack / commitment_signed exchange
// rides the normal commitment-dance handlers, which call maybeFinishDynDance to
// apply the params and exit quiescence at the termination point (once both
// chains have locked in).
//
// What remains for the reestablish branch (branch 9): retransmitting the
// bundled dyn_commit_sig when a peer's channel_reestablish expects it,
// forgetting a negotiation that disconnects before dyn_commit_sig, and durably
// persisting the bundled sig so the dance survives a restart. Those are out of
// scope here.
func (l *channelLink) handleDynCommitHandoff(ctx context.Context,
	h dyn.CommitHandoff) error {

	l.log.Infof("Dyn negotiation agreed for ChannelID(%v): proposer=%v, "+
		"height=%d, params=%s", l.ChanID(), h.Proposer,
		h.NextCommitHeight, h.Params)

	// Drive our side of the bundled dyn_commit_sig dance first. Only once it
	// succeeds do we record the pending dance, so that a mid-flight failure
	// (which the caller turns into a link reset) never leaves stale
	// pendingDynDance state behind.
	var err error
	if h.Proposer.IsLocal() {
		err = l.driveDynProposerCommit(ctx, h)
	} else {
		err = l.driveDynResponderCommit(ctx, h)
	}
	if err != nil {
		return err
	}

	// Both commitment chains are synchronized at quiescence, so the agreed
	// update binds to the same next commitment height on both, and the
	// outgoing (pre-update) params applied through the prior height on each
	// side. Stash the handoff so maybeFinishDynDance can apply the params
	// and exit quiescence once both chains have locked in.
	lockInHeight := h.NextCommitHeight - 1
	l.pendingDynDance = fn.Some(dynDance{
		handoff: h,
		lockIn: lntypes.Dual[uint64]{
			Local:  lockInHeight,
			Remote: lockInHeight,
		},
	})

	return nil
}

// driveDynProposerCommit executes the proposer side of the bundled
// dyn_commit_sig dance. It stages the agreed update into our local update log,
// signs the responder's next commitment under the new params, sends the bundled
// dyn_commit_sig, and records the send with the negotiation state machine. The
// responder then replies with a normal revoke_and_ack and commitment_signed,
// which flow through the ordinary commitment-dance handlers.
//
// NOTE: MUST be called from the htlcManager event loop.
func (l *channelLink) driveDynProposerCommit(ctx context.Context,
	h dyn.CommitHandoff) error {

	// Stage the agreed change so the next commitment we sign for the remote
	// party is rendered with the new params and zero HTLCs.
	if _, err := l.channel.AddDynUpdate(h.Params); err != nil {
		return fmt.Errorf("stage dyn update: %w", err)
	}

	// Sign the responder's next commitment. dyn_commit_sig is the
	// commitment_signed for that state; dynamic updates carry no HTLCs, so
	// there are no HTLC signatures.
	newCommit, err := l.channel.SignNextCommitment(ctx)
	if err != nil {
		return fmt.Errorf("sign dyn commitment: %w", err)
	}

	// Bundle the produced signature exactly as a normal commitment_signed
	// carries it. For taproot (musig2) channels SignNextCommitment produces
	// a partial_signature_with_nonce (and leaves the ECDSA CommitSig as the
	// blank 64 zero bytes); for non-taproot channels it produces the ECDSA
	// CommitSig (and PartialSig is absent). The nonce used is the channel's
	// ambient next_local_nonce, so no separate nonce exchange is needed.
	dynCommit := &lnwire.DynCommit{
		DynPropose: *h.Proposal,
		CommitSig:  newCommit.CommitSig,
		AckSig:     h.AckSig,
		PartialSig: newCommit.PartialSig,
	}
	if err := l.cfg.Peer.SendMessage(false, dynCommit); err != nil {
		return fmt.Errorf("send dyn_commit_sig: %w", err)
	}

	// Record that we have sent the bundled dyn_commit_sig, moving the state
	// machine to its committing terminal state. MarkCommitSent persists the
	// real commitment signature we just sent (the ECDSA CommitSig for a
	// non-taproot channel or the taproot PartialSig) so a disconnect after
	// this point resolves to retransmit rather than forget the negotiation.
	commitSig, partialSig := dynCommitSigOptions(
		newCommit.CommitSig, newCommit.PartialSig,
	)
	if _, err := l.updater.MarkCommitSent(
		ctx, commitSig, partialSig,
	); err != nil {

		return fmt.Errorf("mark commit sent: %w", err)
	}

	return nil
}

// dynCommitSigOptions splits a commitment signature into the ECDSA and taproot
// partial-signature option forms the dyn negotiation state machine persists. A
// taproot channel produces a partial_signature_with_nonce and a blank ECDSA
// signature, so the partial form is returned and the ECDSA form is absent;
// otherwise the ECDSA signature is returned and the partial form is absent.
func dynCommitSigOptions(commitSig lnwire.Sig,
	partial lnwire.OptPartialSigWithNonceTLV) (fn.Option[lnwire.Sig],
	fn.Option[lnwire.PartialSigWithNonce]) {

	var ps fn.Option[lnwire.PartialSigWithNonce]
	partial.WhenSome(func(r lnwire.PartialSigWithNonceTLV) {
		ps = fn.Some(r.Val)
	})
	if ps.IsSome() {
		return fn.None[lnwire.Sig](), ps
	}

	return fn.Some(commitSig), fn.None[lnwire.PartialSigWithNonce]()
}

// driveDynResponderCommit executes the responder side of the bundled
// dyn_commit_sig dance. It stages the agreed update into the remote
// (proposer's) update log, then feeds the bundled commitment sig through the
// commitment_signed handler, which validates and persists our newly rendered
// commitment before sending revoke_and_ack, and signs the proposer's next
// commitment (a normal commitment_signed). The proposer's final revoke_and_ack
// then flows through the ordinary handler.
//
// NOTE: MUST be called from the htlcManager event loop.
func (l *channelLink) driveDynResponderCommit(ctx context.Context,
	h dyn.CommitHandoff) error {

	dynCommit, err := h.ReceivedCommit.UnwrapOrErr(
		fmt.Errorf("responder handoff missing dyn_commit_sig"),
	)
	if err != nil {
		return err
	}

	// Stage the agreed change proposed by the remote party so our next local
	// commitment is validated, and the proposer's next commitment is signed,
	// under the new params.
	if _, err := l.channel.ReceiveDynUpdate(h.Params); err != nil {
		return fmt.Errorf("stage dyn update: %w", err)
	}

	// dyn_commit_sig is the commitment_signed for our next commitment. Reuse
	// the ordinary handler so the received signature is validated and the
	// resulting commitment is persisted (via ReceiveNewCommitment) before we
	// send revoke_and_ack, and so our reply (revoke_and_ack plus a normal
	// commitment_signed for the proposer's next commitment) is produced
	// exactly as for any commitment dance. Dynamic updates carry no HTLCs and
	// hence no HTLC/aux signatures.
	//
	// For taproot channels the signature rides the
	// partial_signature_with_nonce field and the ECDSA CommitSig is blank;
	// for non-taproot channels the ECDSA CommitSig carries it and PartialSig
	// is absent. Forwarding both mirrors how a normal commitment_signed is
	// handled: ReceiveNewCommitment verifies the partial sig against our next
	// commitment using the channel's existing nonce state for taproot, and
	// the ECDSA sig otherwise.
	//
	// Before we send our revoke_and_ack, durably persist the received
	// dyn_commit_sig (its ECDSA or taproot partial form). The extension BOLT
	// requires the receiver to persist the received dyn_commit_sig before
	// revoke_and_ack so a reconnect resolves to resume rather than forget the
	// locked-in update. The accepted proposal TLVs and dyn_ack signature were
	// already persisted by the negotiation state machine at accept time.
	commitSig, partialSig := dynCommitSigOptions(
		dynCommit.CommitSig, dynCommit.PartialSig,
	)
	if err := l.updater.RecordCommitReceived(
		ctx, commitSig, partialSig,
	); err != nil {

		return fmt.Errorf("record received dyn_commit_sig: %w", err)
	}

	return l.processRemoteCommitSig(ctx, &lnwire.CommitSig{
		ChanID:     l.ChanID(),
		CommitSig:  dynCommit.CommitSig,
		PartialSig: dynCommit.PartialSig,
	})
}

// maybeFinishDynDance completes an in-flight dynamic-commitments commitment
// dance once both commitment chains have locked in the agreed parameter change.
// It is invoked at each commitment-dance step that may have just locked in the
// last chain (after processing a revoke_and_ack or a commitment_signed). It is
// a no-op unless a dance is in flight and the channel has returned to a clean
// state, i.e. both chains sign the same updates with the dyn update committed
// and compacted away.
//
// On completion it applies the agreed params to the persistent channel config
// (recording a commit-chain epoch for script-rendering changes), clears the
// negotiation state so a fresh proposal is possible, and exits quiescence at
// the BOLT-defined termination point.
//
// NOTE: MUST be called from the htlcManager event loop.
func (l *channelLink) maybeFinishDynDance(ctx context.Context) {
	l.pendingDynDance.WhenSome(func(d dynDance) {
		// The dance is only complete once both chains sign the same
		// updates again: the agreed update has been committed on both
		// commitments and compacted out of the logs.
		if !l.channel.IsChannelClean() {
			return
		}

		// Apply the agreed params to the persistent local/remote config
		// and, for script-rendering changes, record the commit-chain
		// epoch at the per-side lock-in heights.
		if err := l.applyDynParams(d.handoff, d.lockIn); err != nil {
			l.dynFailf("apply dyn params: %v", err)

			return
		}

		l.pendingDynDance = fn.None[dynDance]()

		// Clear the negotiation state so the channel can negotiate a
		// fresh update after a new quiescence session.
		if err := l.updater.Reset(ctx); err != nil {
			l.log.Errorf("Unable to reset dyn updater for "+
				"ChannelID(%v): %v", l.ChanID(), err)
		}

		// Both chains have locked in; exit quiescence at the
		// BOLT-defined termination point.
		l.quiescer.Resume()

		l.log.Infof("Dyn update locked in for ChannelID(%v): params=%s",
			l.ChanID(), d.handoff.Params)
	})
}

// applyDynParams applies an agreed dynamic-commitments parameter update to the
// underlying channel at the point the given commitment chains lock in. It is
// the "apply at lock-in" step of the commitment dance: it validates and
// atomically persists the resulting local/remote config and, for
// script-rendering params (CSV/dust), records a commit-chain epoch at the given
// per-side lock-in heights so historical scripts stay reconstructable.
//
// lockIn gives, per commitment chain, the last height the outgoing (pre-update)
// params applied to; it is only consulted for epoch-affecting changes.
func (l *channelLink) applyDynParams(h dyn.CommitHandoff,
	lockIn lntypes.Dual[uint64]) error {

	return l.channel.ApplyChannelParams(
		h.Proposer, h.Params, l.dynMaxCSVDelay(), lockIn,
	)
}

// handleDynReestablish applies the dynamic-commitments reconnect rules on
// channel reestablishment. It consults the durable accepted-proposal context
// and, per the locked design decisions, either forgets a stale negotiation,
// retains a responder acceptance and waits, retransmits a persisted
// dyn_commit_sig, or resumes a locked-in update. It is driven from
// syncChanStates with the peer's channel_reestablish so the peer's next
// expected commitment height is known.
//
// Non-dyn channels never mount an updater or a persister, so this is a
// no-op for them and normal commitment_signed / revoke_and_ack
// retransmission (driven by ProcessChanSyncMsg in syncChanStates) is
// entirely unaffected.
//
// NOTE: MUST be called from syncChanStates before the link starts its normal
// event loop.
func (l *channelLink) handleDynReestablish(ctx context.Context,
	remoteSync *lnwire.ChannelReestablish) error {

	// The feature is inert for this channel: nothing was ever persisted, so
	// there is nothing to resume or forget.
	if l.updater == nil || l.dynPersister == nil {
		return nil
	}

	chanID := l.ChanID()

	persisted, err := l.dynPersister.FetchAcceptedProposal(ctx, chanID)
	if err != nil {
		return fmt.Errorf("fetch dyn proposal for ChannelID(%v): %w",
			chanID, err)
	}

	// No negotiation was in flight across the disconnect; nothing to do.
	if persisted.IsNone() {
		return nil
	}
	p := persisted.UnsafeFromSome()

	reest := dyn.ReestablishState{
		PeerNextCommitHeight: remoteSync.NextLocalCommitHeight,
	}
	action := dyn.Decide(p, reest)

	l.log.Infof("Dyn reconnect for ChannelID(%v): proposer=%v, height=%d, "+
		"action=%s", chanID, p.Proposer, p.NextCommitHeight, action)

	switch action {
	// Abort and forget the negotiation: delete the persisted context and
	// return the state machine to Idle so a fresh quiescence session can
	// start a new negotiation later. No dyn message is retransmitted.
	case dyn.ReconnectForget:
		if err := l.updater.Reset(ctx); err != nil {
			return fmt.Errorf("reset dyn updater for "+
				"ChannelID(%v): %w", chanID, err)
		}

	// We are the responder and sent our dyn_ack before the disconnect.
	// Retain the acceptance by rehydrating the updater to AckSent so that a
	// subsequent dyn_commit_sig (or a superseding proposal, or a timeout) is
	// handled correctly. We do not proactively resend the dyn_ack.
	case dyn.ReconnectRetainAndWait:
		if err := l.updater.Restore(ctx, p); err != nil {
			return fmt.Errorf("restore dyn updater for "+
				"ChannelID(%v): %w", chanID, err)
		}

	// We are the proposer and a dyn_commit_sig was persisted that the peer
	// has not yet processed. Rehydrate the machine and retransmit the
	// bundled dyn_commit_sig before any other update.
	//
	// TODO(dyn): the retransmission itself and the commitment-number
	// accounting depend on the branch-6 commitment dance that produces and
	// persists dyn_commit_sig, which is not complete on the branch this is
	// stacked on. The concrete remaining sub-steps are:
	//
	//  1. Reconstruct the bundled lnwire.DynCommit from the persisted
	//     context (p.Proposal, p.AckSig, p.CommitSig) and send it via
	//     l.cfg.Peer before the normal msgsToReSend loop in syncChanStates,
	//     so it precedes any other update even though quiescence has ended.
	//  2. Count the sent dyn_commit_sig as the commitment_signed for the
	//     responder's next_commitment_number so ProcessChanSyncMsg does not
	//     also retransmit a bare commitment_signed for the same height, and
	//     so the revocation accounting (next_revocation_number) lines up.
	//  3. Apply the params at lock-in and clear the persisted context once
	//     both chains have locked in (see handleDynCommitHandoff).
	//
	// Until the dance lands, p.CommitSig is always None in the live flow, so
	// Decide never returns this action in production; it is exercised only by
	// unit tests that inject a CommitSig. We still rehydrate the machine so
	// the structure is complete and correct.
	case dyn.ReconnectRetransmitCommitSig:
		if err := l.updater.Restore(ctx, p); err != nil {
			return fmt.Errorf("restore dyn updater for "+
				"ChannelID(%v): %w", chanID, err)
		}

		l.log.Warnf("Dyn dyn_commit_sig retransmission for "+
			"ChannelID(%v) is not yet wired to the commitment "+
			"dance; skipping resend", chanID)

	// The dyn_commit_sig has already been processed by the peer (or received
	// by us as the responder): the update is locked in and the remainder of
	// the dance is covered by normal commitment retransmission. Rehydrate the
	// machine to its committing state.
	//
	// TODO(dyn): clearing the persisted context and applying the params at
	// lock-in depend on the branch-6 dance (see handleDynCommitHandoff).
	case dyn.ReconnectResume:
		if err := l.updater.Restore(ctx, p); err != nil {
			return fmt.Errorf("restore dyn updater for "+
				"ChannelID(%v): %w", chanID, err)
		}
	}

	return nil
}

// dynFailf fails the link on a dynamic-commitments protocol violation. As with
// quiescence violations, only link state (not channel state) is affected, so we
// opt to drop the connection and let a fresh reestablish recover.
func (l *channelLink) dynFailf(format string, args ...interface{}) {
	l.failf(LinkFailureError{
		code:             ErrStfuViolation,
		FailureAction:    LinkFailureDisconnect,
		PermanentFailure: false,
		Warning:          true,
	}, format, args...)
}
