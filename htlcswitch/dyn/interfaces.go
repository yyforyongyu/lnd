package dyn

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Signer abstracts the production of and verification of dyn_ack signatures. It
// hides the key material from the state machine: an implementation signs with
// our node identity key and verifies against the peer's node identity key. The
// digest inputs are supplied by the caller and match lnwire.DynAckSigDigest.
//
// Branch 6 wires this to the real keychain/signer; tests inject a fake.
type Signer interface {
	// SignDynAck produces our dyn_ack signature over the accepted proposal.
	// The proposalTLVs must be the exact dyn_propose_tlvs stream, i.e. the
	// output of DynPropose.SerializeTlvData.
	SignDynAck(chainHash chainhash.Hash, chanID lnwire.ChannelID,
		nextCommitHeight uint64, proposalTLVs []byte) (lnwire.Sig,
		error)

	// VerifyDynAck verifies the peer's dyn_ack signature over the proposal
	// we sent. It returns nil on success and an error otherwise.
	VerifyDynAck(sig lnwire.Sig, chainHash chainhash.Hash,
		chanID lnwire.ChannelID, nextCommitHeight uint64,
		proposalTLVs []byte) error
}

// ChannelView is the read-only window the state machine has onto the live
// channel. It exposes the eligibility predicates the machine enforces as
// preconditions, plus the channel data needed to build, validate, and bind a
// proposal. It is deliberately narrow so the machine can be unit-tested without
// a live link.
//
// Branch 6 implements this over the real channelLink/LightningChannel; tests
// inject a fake.
type ChannelView interface {
	// ChanID returns the channel id this view is for.
	ChanID() lnwire.ChannelID

	// ChainHash returns the genesis hash of the chain the channel is on. It
	// is bound into the dyn_ack signature digest.
	ChainHash() chainhash.Hash

	// LocalChanCfg returns our current channel configuration.
	LocalChanCfg() channeldb.ChannelConfig

	// RemoteChanCfg returns the peer's current channel configuration.
	RemoteChanCfg() channeldb.ChannelConfig

	// Capacity returns the total channel capacity, used to bound the
	// channel reserve during validation.
	Capacity() btcutil.Amount

	// NextCommitHeight returns the commitment number the dynamic update will
	// be committed at. At quiescence both commitment chains are synchronized
	// and have no pending updates, so this value is unambiguous and shared
	// by both peers when computing the dyn_ack digest.
	NextCommitHeight() uint64

	// BothChannelReady returns true only once both peers have sent and
	// received channel_ready. A dynamic update may neither be sent nor
	// accepted before this holds.
	BothChannelReady() bool

	// IsQuiescent returns true if the channel has been driven to
	// quiescence. Dynamic updates require a quiescent channel.
	IsQuiescent() bool

	// QuiescenceInitiator returns the party that initiated quiescence. The
	// proposer of a dynamic update must be the quiescence initiator. It
	// returns an error if the channel is not currently quiescent.
	QuiescenceInitiator() fn.Result[lntypes.ChannelParty]

	// HasHTLCs returns true if either commitment has any HTLCs, including
	// trimmed HTLCs. A dynamic update requires that there are none.
	HasHTLCs() bool

	// HasPendingUpdates returns true if there are update-log entries that
	// are not yet bilaterally committed on both chains.
	HasPendingUpdates() bool

	// HasPendingShutdown returns true if a cooperative shutdown is in
	// progress on the channel.
	HasPendingShutdown() bool

	// HasPendingSpliceOrRBF returns true if a splice or an RBF of a pending
	// funding/close is in progress on the channel.
	HasPendingSpliceOrRBF() bool
}

// AcceptedProposal is the context that must survive across the persistence
// boundaries the extension BOLT calls out, so a reconnect (branch 8) can decide
// whether to resume or forget a negotiation. It captures the accepted proposal,
// which party proposed it, the commitment number it binds to, and, once known,
// the responder's dyn_ack signature.
type AcceptedProposal struct {
	// Proposer is the party that sent the dyn_propose. If it equals
	// lntypes.Local we are the proposer, otherwise we are the responder.
	Proposer lntypes.ChannelParty

	// Proposal is the accepted proposal message.
	Proposal *lnwire.DynPropose

	// NextCommitHeight is the commitment number the update binds to; it is
	// bound into the dyn_ack signature digest.
	NextCommitHeight uint64

	// AckSig is the responder's dyn_ack signature. It is set on the
	// responder once it signs, and on the proposer once it verifies the
	// received dyn_ack.
	AckSig fn.Option[lnwire.Sig]

	// CommitSig is the proposer's dyn_commit_sig commitment signature. Its
	// presence is the persisted flag that a dyn_commit_sig has crossed the
	// wire before a disconnect: the proposer sets it when it sends the
	// bundled dyn_commit_sig, and the responder sets it when it receives and
	// persists one before sending its revoke_and_ack. A negotiation with a
	// persisted CommitSig is retained and retransmitted across a reconnect;
	// one without is forgotten (see Decide).
	//
	// TODO(dyn): the commitment dance that actually produces and consumes
	// dyn_commit_sig is not complete on the branch this is stacked on, so
	// nothing sets this in the live flow yet. The reconnect decision logic
	// and the durable persistence handle it now so the retransmission path
	// is ready to be wired once the dance lands.
	CommitSig fn.Option[lnwire.Sig]
}

// Params returns the canonical channel-params view of the accepted proposal.
func (a *AcceptedProposal) Params() lnwallet.ChannelParams {
	return lnwallet.ChannelParamsFromDynPropose(a.Proposal)
}

// Persister persists the accepted-proposal context at the boundaries the state
// machine calls out. Branch 8 implements this over channeldb; the in-memory
// MemPersister is provided for tests. The state machine never assumes a
// particular storage medium.
type Persister interface {
	// StoreAcceptedProposal persists (or overwrites) the accepted-proposal
	// context for the given channel.
	StoreAcceptedProposal(ctx context.Context, chanID lnwire.ChannelID,
		p AcceptedProposal) error

	// FetchAcceptedProposal loads the persisted context for the given
	// channel, if any.
	FetchAcceptedProposal(ctx context.Context,
		chanID lnwire.ChannelID) (fn.Option[AcceptedProposal], error)

	// DeleteAcceptedProposal removes any persisted context for the given
	// channel. It is a no-op if none exists.
	DeleteAcceptedProposal(ctx context.Context,
		chanID lnwire.ChannelID) error
}

// MessageSender sends wire messages to the peer on the other end of the
// channel. Branch 6 wires this to the peer connection; tests inject a recorder.
type MessageSender interface {
	// SendMessage sends the given messages to the peer, in order.
	SendMessage(ctx context.Context, msgs ...lnwire.Message) error
}
