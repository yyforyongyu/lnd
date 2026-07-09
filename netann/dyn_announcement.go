package netann

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrChanNotConfirmed is returned when a private->public conversion is
	// attempted on a channel whose funding transaction does not yet have a
	// confirmed short_channel_id. A channel_announcement is keyed by the
	// confirmed SCID, so it cannot be produced before confirmation.
	ErrChanNotConfirmed = errors.New("cannot make channel public: funding " +
		"transaction has no confirmed short_channel_id")

	// ErrScidAliasChannel is returned when a private->public conversion is
	// attempted on an option_scid_alias channel. These channels deliberately
	// use aliases instead of the real SCID, so publicly announcing them would
	// leak the very SCID the type is meant to hide.
	ErrScidAliasChannel = errors.New("cannot make channel public: " +
		"option_scid_alias channels cannot be announced")

	// ErrShutdownPending is returned when a private->public conversion is
	// attempted while a cooperative shutdown is in progress on the channel.
	// Announcing a channel that is being torn down would advertise a
	// route that is about to disappear.
	ErrShutdownPending = errors.New("cannot make channel public: a " +
		"shutdown is pending on the channel")
)

// FlagsTransition classifies how a dynamic channel_flags update changes the
// channel's public/private announcement intent. channel_flags is treated as a
// channel-wide announcement intent, distinct from the per-direction
// enable/disable policy bits carried in an ordinary channel_update.
type FlagsTransition uint8

const (
	// FlagsUnchanged means the announcement intent (the FFAnnounceChannel
	// bit) did not change.
	FlagsUnchanged FlagsTransition = iota

	// FlagsPrivateToPublic means the channel is being converted from a
	// private channel into a publicly-announced one.
	FlagsPrivateToPublic

	// FlagsPublicToPrivate means the channel is being converted from a
	// publicly-announced channel into a private one.
	FlagsPublicToPrivate
)

// String returns a human-readable representation of the transition.
func (t FlagsTransition) String() string {
	switch t {
	case FlagsUnchanged:
		return "unchanged"
	case FlagsPrivateToPublic:
		return "private->public"
	case FlagsPublicToPrivate:
		return "public->private"
	default:
		return "unknown"
	}
}

// ClassifyFlagsChange determines the announcement-intent transition implied by
// a change from oldFlags to newFlags. Only the FFAnnounceChannel bit carries
// announcement intent; changes to other bits do not affect the public/private
// status.
func ClassifyFlagsChange(oldFlags, newFlags lnwire.FundingFlag) FlagsTransition {
	wasPublic := oldFlags&lnwire.FFAnnounceChannel != 0
	isPublic := newFlags&lnwire.FFAnnounceChannel != 0

	switch {
	case !wasPublic && isPublic:
		return FlagsPrivateToPublic

	case wasPublic && !isPublic:
		return FlagsPublicToPrivate

	default:
		return FlagsUnchanged
	}
}

// CheckPrivateToPublic verifies that the given channel satisfies every
// precondition for being converted from a private channel into a publicly
// announced one. It returns a typed error (matchable with errors.Is) for the
// first unmet precondition, or nil if the conversion may proceed.
//
// The preconditions mirror those the funding manager enforces for a channel
// that is announced at open time:
//
//   - The funding transaction must have a confirmed short_channel_id. A
//     channel_announcement is keyed by the confirmed SCID and cannot be built
//     before confirmation (for a zero-conf channel this means the real SCID
//     must have confirmed, not just the alias).
//   - The channel type must not be option_scid_alias. Such channels only ever
//     use aliases; announcing them would leak the real SCID.
//   - Neither peer may have a cooperative shutdown in progress.
func CheckPrivateToPublic(channel *channeldb.OpenChannel,
	shutdownPending bool) error {

	// The funding transaction must be confirmed so that a real SCID exists
	// to key the channel_announcement by.
	if channel.IsPending {
		return ErrChanNotConfirmed
	}

	// A zero-conf channel opens against an alias SCID; its real SCID is only
	// available once the funding transaction confirms. We cannot announce
	// it until the confirmed SCID is known.
	if channel.IsZeroConf() && !channel.ZeroConfConfirmed() {
		return ErrChanNotConfirmed
	}

	// option_scid_alias channels are private by construction: they only
	// share aliases and keep the real SCID hidden. Converting one to public
	// would defeat that guarantee, so it is rejected outright.
	if channel.ChanType.HasScidAliasChan() {
		return ErrScidAliasChannel
	}

	// A channel that is being cooperatively closed must not be announced as
	// a usable route.
	if shutdownPending {
		return ErrShutdownPending
	}

	return nil
}

// DynAnnouncementConfig bundles the dependencies the DynAnnouncementManager
// needs to reconcile the gossip layer with a committed dynamic-commitments
// parameter update. Every dependency is injected so the manager unit-tests
// without a live server, gossiper, or funding manager.
type DynAnnouncementConfig struct {
	// MessageSigner signs channel_update messages that validate under our
	// node identity key.
	MessageSigner lnwallet.MessageSigner

	// OurKeyLoc locates our node identity key used to sign channel_updates.
	OurKeyLoc keychain.KeyLocator

	// FetchChannel returns the persisted OpenChannel for the given channel
	// point. It is read after the dynamic update has locked in, so it
	// reflects the newly committed configs and channel_flags.
	FetchChannel func(wire.OutPoint) (*channeldb.OpenChannel, error)

	// FetchOurChannelUpdate returns the most recent channel_update we
	// advertised for our direction of the given channel, along with whether
	// the channel is still private (i.e. has no channel_announcement proof).
	FetchOurChannelUpdate func(wire.OutPoint) (*lnwire.ChannelUpdate1, bool,
		error)

	// ApplyChannelUpdate signs-off on a freshly signed channel_update by
	// handing it to the gossiper for local application and broadcast. The
	// private flag selects whether the peer alias (rather than the real
	// SCID) should be used, matching the server's applyChannelUpdate.
	ApplyChannelUpdate func(*lnwire.ChannelUpdate1, *wire.OutPoint,
		bool) error

	// IsShutdownPending reports whether a cooperative shutdown is in
	// progress on the channel. It gates the private->public conversion.
	IsShutdownPending func(lnwire.ChannelID) bool

	// ReAnnounceChannel re-triggers the AnnounceSignatures exchange for a
	// formerly-private channel so that a channel_announcement is produced
	// and the channel joins the public graph. It may be nil while the deep
	// funding-manager re-trigger is not yet wired (see makePublic).
	ReAnnounceChannel func(context.Context, *channeldb.OpenChannel) error

	// RequestDisable disables our direction of the channel via a signed
	// channel_update with the disable bit set (typically
	// ChanStatusManager.RequestDisable). The manual flag mirrors the
	// ChanStatusManager semantics.
	RequestDisable func(wire.OutPoint, bool) error

	// MinHTLCOut is the daemon's default minimum forwarded HTLC. It is used
	// as a floor for the advertised htlc_minimum_msat, matching the funding
	// manager's extractAnnounceParams.
	MinHTLCOut lnwire.MilliSatoshi
}

// DynAnnouncementManager reconciles the channel-announcement/gossip state with
// a committed dynamic-commitments parameter update. It is the entry point the
// link/server invokes after applyDynParams has locked a dynamic update in for a
// channel (see htlcswitch: NotifyDynParamsLockIn).
//
// It is entirely inert for channels that never undergo a dynamic update: the
// link only invokes it on the dyn apply path, so ordinary gossip is unaffected.
type DynAnnouncementManager struct {
	cfg DynAnnouncementConfig
}

// NewDynAnnouncementManager constructs a DynAnnouncementManager from the given
// config.
func NewDynAnnouncementManager(
	cfg DynAnnouncementConfig) *DynAnnouncementManager {

	return &DynAnnouncementManager{cfg: cfg}
}

// ProcessDynParamsLockIn reconciles the gossip layer with a dynamic-commitments
// parameter update that has just locked in for the given channel. It performs
// two independent adjustments:
//
//  1. channel_update policy realignment: if the committed update changed the
//     inbound HTLC limits (htlc_minimum_msat or max_htlc_value_in_flight_msat)
//     in a way that no longer matches what we advertise, it emits a corrected
//     directional channel_update.
//  2. announcement intent: if the committed update changed the channel_flags
//     announcement bit, it drives the private->public or public->private
//     conversion (subject to the private->public preconditions).
//
// oldFlags is the channel_flags value before the update; params is the agreed
// change; proposer identifies which party proposed it (proposer-owned
// semantics). Errors are returned to the caller for logging; the parameter
// update itself has already been committed and persisted regardless.
func (m *DynAnnouncementManager) ProcessDynParamsLockIn(ctx context.Context,
	chanPoint wire.OutPoint, proposer lntypes.ChannelParty,
	oldFlags lnwire.FundingFlag, params lnwallet.ChannelParams) error {

	log.Debugf("Reconciling gossip state for dyn params lock-in on "+
		"ChannelPoint(%v): proposer=%v", chanPoint, proposer)

	channel, err := m.cfg.FetchChannel(chanPoint)
	if err != nil {
		return fmt.Errorf("dyn gossip: fetch channel %v: %w",
			chanPoint, err)
	}

	// First, keep our advertised channel_update consistent with the
	// committed inbound HTLC limits. This is independent of any
	// announcement-intent change and must run for both public and private
	// channels.
	if params.HtlcMinimum.IsSome() || params.MaxValueInFlight.IsSome() {
		if err := m.realignChannelUpdate(chanPoint, channel); err != nil {
			return fmt.Errorf("dyn gossip: realign channel "+
				"update for %v: %w", chanPoint, err)
		}
	}

	// Next, react to any change in the channel's announcement intent.
	transition := FlagsUnchanged
	params.ChannelFlags.WhenSome(func(newFlags lnwire.FundingFlag) {
		transition = ClassifyFlagsChange(oldFlags, newFlags)
	})

	switch transition {
	case FlagsUnchanged:
		return nil

	case FlagsPrivateToPublic:
		return m.makePublic(ctx, chanPoint, channel)

	case FlagsPublicToPrivate:
		return m.makePrivate(chanPoint, channel)

	default:
		return nil
	}
}

// makePublic drives a private->public conversion for a channel whose
// channel_flags announcement bit was just set. It enforces the conversion
// preconditions, (re-)triggers the announcement-signature exchange so a
// channel_announcement is produced, and then publishes a fresh directional
// channel_update.
func (m *DynAnnouncementManager) makePublic(ctx context.Context,
	chanPoint wire.OutPoint, channel *channeldb.OpenChannel) error {

	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	shutdownPending := false
	if m.cfg.IsShutdownPending != nil {
		shutdownPending = m.cfg.IsShutdownPending(chanID)
	}

	// Enforce the real preconditions before touching gossip state.
	if err := CheckPrivateToPublic(channel, shutdownPending); err != nil {
		return err
	}

	// Re-trigger the AnnounceSignatures exchange so both peers exchange
	// their halves of the channel_announcement proof and the channel joins
	// the public graph.
	//
	// TODO(dyn): the AnnounceSignatures re-trigger requires funding-manager
	// state (the local/bitcoin funding keys and the peer coordination that
	// produces and processes the AnnounceSignatures1 proof) that is not
	// wired to this manager on the current branch. The remaining sub-steps
	// are:
	//
	//  1. Have the funding manager rebuild the chanAnnouncement (see
	//     funding.Manager.newChanAnnouncement) for the now-public channel,
	//     using its confirmed SCID.
	//  2. Send our AnnounceSignatures1 half to the peer and await theirs,
	//     reusing the annAfterSixConfs/announceChannel path so the gossiper
	//     assembles the full channel_announcement and sets the edge's
	//     AuthProof.
	//  3. Emit a fresh NodeAnnouncement so the channel is attributable.
	//
	// Until that lands, ReAnnounceChannel is nil in production, so the
	// channel_announcement is not produced and the channel_update below is
	// still advertised privately (FetchOurChannelUpdate reports the channel
	// as private because its AuthProof is absent). The preconditions and the
	// publish path are exercised regardless.
	if m.cfg.ReAnnounceChannel != nil {
		if err := m.cfg.ReAnnounceChannel(ctx, channel); err != nil {
			return fmt.Errorf("re-announce channel %v: %w",
				chanPoint, err)
		}
	} else {
		log.Warnf("Dyn private->public conversion for ChannelPoint"+
			"(%v): AnnounceSignatures re-trigger is not wired "+
			"(TODO(dyn)); publishing directional channel_update "+
			"only", chanPoint)
	}

	// Publish a fresh directional channel_update for our side of the newly
	// public channel with the disable bit cleared.
	if err := m.readvertise(
		chanPoint, ChanUpdSetDisable(false),
	); err != nil {
		return fmt.Errorf("publish channel update for %v: %w",
			chanPoint, err)
	}

	log.Infof("Dyn converted ChannelPoint(%v) private->public", chanPoint)

	return nil
}

// makePrivate drives a public->private conversion for a channel whose
// channel_flags announcement bit was just cleared.
//
// A channel_announcement that has already propagated cannot be removed from the
// public graph: gossip has no "un-announce" message, and other nodes retain the
// announcement and any channel_updates they have seen. The realistic action is
// therefore to disable our direction so we stop advertising the channel as a
// usable route. Both peers run this after the same lock-in, so each disables
// its own direction and the union is "both directions disabled" — but this is
// NOT equivalent to erasing the channel's historical public presence: the
// channel_announcement and prior updates remain in other nodes' graphs, and the
// channel remains discoverable there until it is closed.
func (m *DynAnnouncementManager) makePrivate(chanPoint wire.OutPoint,
	_ *channeldb.OpenChannel) error {

	// We can only sign a channel_update for our own direction, so we disable
	// that direction. The counterparty disables its direction after the same
	// lock-in. manual=true so a background re-enable does not silently undo
	// the user-driven conversion.
	if m.cfg.RequestDisable != nil {
		if err := m.cfg.RequestDisable(chanPoint, true); err != nil {
			return fmt.Errorf("disable channel %v: %w", chanPoint,
				err)
		}
	}

	log.Infof("Dyn converted ChannelPoint(%v) public->private: disabled "+
		"our direction (channel remains in other nodes' graphs until "+
		"closed)", chanPoint)

	return nil
}

// realignChannelUpdate re-advertises our directional channel_update when the
// committed inbound HTLC limits no longer match what we advertise.
//
// The advertised htlc_minimum_msat / htlc_maximum_msat are derived from our
// local channel config exactly as the funding manager derives them at open time
// (see extractAnnounceParams). Because a dynamic proposal is proposer-owned and
// its htlc_minimum_msat / max_htlc_value_in_flight_msat changes are written to
// the counterparty's config, only the counterparty (responder) sees its own
// local config change: on the proposer this recompute is a no-op, so a single
// symmetric implementation is correct on both nodes.
func (m *DynAnnouncementManager) realignChannelUpdate(chanPoint wire.OutPoint,
	channel *channeldb.OpenChannel) error {

	update, private, err := m.cfg.FetchOurChannelUpdate(chanPoint)
	switch {
	// No advertised policy exists for this channel yet; there is nothing to
	// realign.
	case errors.Is(err, graphdb.ErrEdgeNotFound),
		errors.Is(err, ErrUnableToExtractChanUpdate):

		return nil

	case err != nil:
		return err
	}

	wantMin, wantMax := desiredForwardingPolicy(
		channel.LocalChanCfg, channel.Capacity, m.cfg.MinHTLCOut,
	)

	// Detect the two realignment cases the spec calls out:
	//
	//   - the advertised htlc_minimum_msat no longer matches the committed
	//     inbound minimum, and
	//   - a max_htlc_value_in_flight_msat decrease has left our advertised
	//     htlc_maximum_msat above the committed cap (invalidating it).
	//
	// We also re-advertise on any other htlc_maximum change so the two stay
	// consistent, but the invalidating decrease is the safety-critical case.
	minChanged := update.HtlcMinimumMsat != wantMin
	maxInvalidated := update.HtlcMaximumMsat > wantMax
	maxChanged := update.HtlcMaximumMsat != wantMax

	if !minChanged && !maxChanged {
		// Our advertised policy already reflects the committed params
		// (the common case on the proposer side).
		return nil
	}

	log.Infof("Dyn realigning channel_update for ChannelPoint(%v): "+
		"htlc_minimum %v->%v, htlc_maximum %v->%v (min_changed=%v, "+
		"max_invalidated=%v)", chanPoint, update.HtlcMinimumMsat,
		wantMin, update.HtlcMaximumMsat, wantMax, minChanged,
		maxInvalidated)

	err = m.signAndApply(
		chanPoint, private, update, ChanUpdSetHtlcMinMax(wantMin, wantMax),
	)
	if err != nil {
		return err
	}

	return nil
}

// readvertise re-signs and re-broadcasts our latest directional channel_update
// for the channel with the given modifiers applied. It fetches the most recent
// advertised update as its starting point so the timestamp increases
// monotonically.
func (m *DynAnnouncementManager) readvertise(chanPoint wire.OutPoint,
	mods ...ChannelUpdateModifier) error {

	update, private, err := m.cfg.FetchOurChannelUpdate(chanPoint)
	if err != nil {
		return err
	}

	return m.signAndApply(chanPoint, private, update, mods...)
}

// signAndApply applies the given modifiers to the update, signs it under our
// node key with a bumped timestamp, and hands it to the gossiper for local
// application and broadcast.
func (m *DynAnnouncementManager) signAndApply(chanPoint wire.OutPoint,
	private bool, update *lnwire.ChannelUpdate1,
	mods ...ChannelUpdateModifier) error {

	mods = append(mods, ChanUpdSetTimestamp)
	err := SignChannelUpdate(
		m.cfg.MessageSigner, m.cfg.OurKeyLoc, update, mods...,
	)
	if err != nil {
		return err
	}

	return m.cfg.ApplyChannelUpdate(update, &chanPoint, private)
}

// desiredForwardingPolicy computes the htlc_minimum_msat and htlc_maximum_msat
// that our directional channel_update should advertise given our (possibly
// updated) local channel config, the channel capacity, and the daemon's minimum
// forwarded HTLC floor. It mirrors funding.Manager.extractAnnounceParams so the
// realigned update matches how the values were originally derived.
func desiredForwardingPolicy(cfg channeldb.ChannelConfig,
	capacity btcutil.Amount, minHTLCFloor lnwire.MilliSatoshi) (
	lnwire.MilliSatoshi, lnwire.MilliSatoshi) {

	// The minimum we can forward is set by the remote party (stored in our
	// local config), floored at our own default policy minimum.
	fwdMin := cfg.MinHTLC
	if fwdMin < minHTLCFloor {
		fwdMin = minHTLCFloor
	}

	// The maximum we can forward is the peer's max-in-flight, capped at the
	// channel capacity.
	fwdMax := cfg.MaxPendingAmount
	capacityMSat := lnwire.NewMSatFromSatoshis(capacity)
	if fwdMax > capacityMSat {
		fwdMax = capacityMSat
	}

	return fwdMin, fwdMax
}
