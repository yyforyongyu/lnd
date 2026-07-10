package netann_test

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/stretchr/testify/require"
)

// TestClassifyFlagsChange verifies the announcement-intent transition
// classification for every combination of the FFAnnounceChannel bit.
func TestClassifyFlagsChange(t *testing.T) {
	t.Parallel()

	const (
		priv = lnwire.FundingFlag(0)
		pub  = lnwire.FFAnnounceChannel
	)

	tests := []struct {
		name string
		old  lnwire.FundingFlag
		new  lnwire.FundingFlag
		want netann.FlagsTransition
	}{
		{"priv->pub", priv, pub, netann.FlagsPrivateToPublic},
		{"pub->priv", pub, priv, netann.FlagsPublicToPrivate},
		{"pub->pub", pub, pub, netann.FlagsUnchanged},
		{"priv->priv", priv, priv, netann.FlagsUnchanged},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := netann.ClassifyFlagsChange(tc.old, tc.new)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestCheckPrivateToPublic verifies each private->public precondition rejection
// case as well as the success case.
func TestCheckPrivateToPublic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		channel         *channeldb.OpenChannel
		shutdownPending bool
		wantErr         error
	}{
		{
			name: "unconfirmed funding tx",
			channel: &channeldb.OpenChannel{
				IsPending: true,
			},
			wantErr: netann.ErrChanNotConfirmed,
		},
		{
			name: "zero-conf not yet confirmed",
			channel: &channeldb.OpenChannel{
				ChanType: channeldb.ZeroConfBit |
					channeldb.ScidAliasFeatureBit,
			},
			wantErr: netann.ErrChanNotConfirmed,
		},
		{
			name: "option_scid_alias channel",
			channel: &channeldb.OpenChannel{
				ChanType: channeldb.ScidAliasChanBit,
			},
			wantErr: netann.ErrScidAliasChannel,
		},
		{
			name:            "shutdown pending",
			channel:         &channeldb.OpenChannel{},
			shutdownPending: true,
			wantErr:         netann.ErrShutdownPending,
		},
		{
			name:    "all preconditions met",
			channel: &channeldb.OpenChannel{},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := netann.CheckPrivateToPublic(
				tc.channel, tc.shutdownPending,
			)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// dynHarness bundles the mock dependencies wired into a DynAnnouncementManager
// so tests can inspect what the manager did.
type dynHarness struct {
	mgr *netann.DynAnnouncementManager

	// channel is the OpenChannel the FetchChannel dep returns.
	channel *channeldb.OpenChannel

	// currentUpdate is the channel_update FetchOurChannelUpdate returns.
	currentUpdate *lnwire.ChannelUpdate1

	// private is the private flag FetchOurChannelUpdate returns.
	private bool

	// applied captures channel_updates handed to ApplyChannelUpdate.
	applied []*lnwire.ChannelUpdate1

	// disableCalls captures RequestDisable invocations as (outpoint, manual).
	disableCalls []disableCall

	// reAnnounceCalls counts ReAnnounceChannel invocations.
	reAnnounceCalls int

	// shutdownPending is returned by the IsShutdownPending dep.
	shutdownPending bool
}

type disableCall struct {
	outpoint wire.OutPoint
	manual   bool
}

// newDynHarness constructs a DynAnnouncementManager over inspectable mocks. The
// channel starts private, confirmed, and non-alias unless the test mutates it.
func newDynHarness(t *testing.T) *dynHarness {
	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	signer := netann.NewNodeSigner(
		keychain.NewPrivKeyMessageSigner(privKey, testKeyLoc),
	)

	h := &dynHarness{
		channel: &channeldb.OpenChannel{
			FundingOutpoint: randOutpoint(t),
			ShortChannelID:  lnwire.NewShortChanIDFromInt(42),
			Capacity:        1_000_000,
		},
	}

	cfg := netann.DynAnnouncementConfig{
		MessageSigner: signer,
		OurKeyLoc:     testKeyLoc,
		FetchChannel: func(wire.OutPoint) (*channeldb.OpenChannel,
			error) {

			return h.channel, nil
		},
		FetchOurChannelUpdate: func(wire.OutPoint) (
			*lnwire.ChannelUpdate1, bool, error) {

			return h.currentUpdate, h.private, nil
		},
		ApplyChannelUpdate: func(u *lnwire.ChannelUpdate1,
			_ *wire.OutPoint, _ bool) error {

			h.applied = append(h.applied, u)
			return nil
		},
		IsShutdownPending: func(lnwire.ChannelID) bool {
			return h.shutdownPending
		},
		RequestDisable: func(op wire.OutPoint, manual bool) error {
			h.disableCalls = append(h.disableCalls, disableCall{
				outpoint: op,
				manual:   manual,
			})
			return nil
		},
		MinHTLCOut: 1_000,
	}

	h.mgr = netann.NewDynAnnouncementManager(cfg)

	return h
}

// baseUpdate returns a channel_update carrying the given advertised HTLC
// limits, usable as the FetchOurChannelUpdate starting point.
func baseUpdate(htlcMin, htlcMax lnwire.MilliSatoshi) *lnwire.ChannelUpdate1 {
	return &lnwire.ChannelUpdate1{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(42),
		MessageFlags:    lnwire.ChanUpdateRequiredMaxHtlc,
		HtlcMinimumMsat: htlcMin,
		HtlcMaximumMsat: htlcMax,
	}
}

// TestProcessDynParamsPrivateToPublicPreconditions ensures that each
// private->public precondition failure surfaces as the corresponding typed
// error from the top-level entry point, and that no channel_update is
// broadcast when a precondition fails.
func TestProcessDynParamsPrivateToPublicPreconditions(t *testing.T) {
	t.Parallel()

	makePublicParams := lnwallet.ChannelParams{
		ChannelFlags: fn.Some(lnwire.FFAnnounceChannel),
	}

	tests := []struct {
		name    string
		setup   func(h *dynHarness)
		wantErr error
	}{
		{
			name: "unconfirmed funding tx",
			setup: func(h *dynHarness) {
				h.channel.IsPending = true
			},
			wantErr: netann.ErrChanNotConfirmed,
		},
		{
			name: "option_scid_alias channel",
			setup: func(h *dynHarness) {
				h.channel.ChanType = channeldb.ScidAliasChanBit
			},
			wantErr: netann.ErrScidAliasChannel,
		},
		{
			name: "shutdown pending",
			setup: func(h *dynHarness) {
				h.shutdownPending = true
			},
			wantErr: netann.ErrShutdownPending,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newDynHarness(t)
			h.currentUpdate = baseUpdate(1_000, 500_000)
			h.private = true
			tc.setup(h)

			err := h.mgr.ProcessDynParamsLockIn(
				context.Background(),
				h.channel.FundingOutpoint, lntypes.Local,
				lnwire.FundingFlag(0), makePublicParams,
			)
			require.ErrorIs(t, err, tc.wantErr)

			// A rejected conversion must not broadcast anything.
			require.Empty(t, h.applied)
		})
	}
}

// TestProcessDynParamsPrivateToPublicSuccess verifies that once preconditions
// pass, the manager re-triggers the announcement flow and publishes a fresh
// enabled directional channel_update.
func TestProcessDynParamsPrivateToPublicSuccess(t *testing.T) {
	t.Parallel()

	h := newDynHarness(t)
	h.currentUpdate = baseUpdate(1_000, 500_000)
	h.private = true

	// Wire a ReAnnounceChannel dep so we can assert the re-trigger fires.
	cfg := netann.DynAnnouncementConfig{
		MessageSigner: netann.NewNodeSigner(
			keychain.NewPrivKeyMessageSigner(
				mustPrivKey(t), testKeyLoc,
			),
		),
		OurKeyLoc: testKeyLoc,
		FetchChannel: func(wire.OutPoint) (*channeldb.OpenChannel,
			error) {

			return h.channel, nil
		},
		FetchOurChannelUpdate: func(wire.OutPoint) (
			*lnwire.ChannelUpdate1, bool, error) {

			return h.currentUpdate, h.private, nil
		},
		ApplyChannelUpdate: func(u *lnwire.ChannelUpdate1,
			_ *wire.OutPoint, _ bool) error {

			h.applied = append(h.applied, u)
			return nil
		},
		IsShutdownPending: func(lnwire.ChannelID) bool { return false },
		ReAnnounceChannel: func(context.Context,
			*channeldb.OpenChannel) error {

			h.reAnnounceCalls++
			return nil
		},
		MinHTLCOut: 1_000,
	}
	mgr := netann.NewDynAnnouncementManager(cfg)

	err := mgr.ProcessDynParamsLockIn(
		context.Background(), h.channel.FundingOutpoint, lntypes.Local,
		lnwire.FundingFlag(0),
		lnwallet.ChannelParams{
			ChannelFlags: fn.Some(lnwire.FFAnnounceChannel),
		},
	)
	require.NoError(t, err)

	// The announcement re-trigger must have fired once.
	require.Equal(t, 1, h.reAnnounceCalls)

	// A fresh directional channel_update must have been published with the
	// disable bit cleared.
	require.Len(t, h.applied, 1)
	require.Zero(t,
		h.applied[0].ChannelFlags&lnwire.ChanUpdateDisabled,
	)
}

// TestProcessDynParamsPublicToPrivate verifies that a public->private
// conversion disables our direction via a manual RequestDisable.
func TestProcessDynParamsPublicToPrivate(t *testing.T) {
	t.Parallel()

	h := newDynHarness(t)
	h.channel.ChannelFlags = lnwire.FFAnnounceChannel
	h.currentUpdate = baseUpdate(1_000, 500_000)

	err := h.mgr.ProcessDynParamsLockIn(
		context.Background(), h.channel.FundingOutpoint, lntypes.Local,
		lnwire.FFAnnounceChannel,
		lnwallet.ChannelParams{
			ChannelFlags: fn.Some(lnwire.FundingFlag(0)),
		},
	)
	require.NoError(t, err)

	// Our direction must be disabled with the manual flag set so a
	// background re-enable does not undo the conversion.
	require.Len(t, h.disableCalls, 1)
	require.Equal(t, h.channel.FundingOutpoint, h.disableCalls[0].outpoint)
	require.True(t, h.disableCalls[0].manual)
}

// TestProcessDynParamsRealignHtlcMinimum verifies that a committed change to
// the inbound htlc_minimum_msat emits a corrected directional channel_update.
func TestProcessDynParamsRealignHtlcMinimum(t *testing.T) {
	t.Parallel()

	h := newDynHarness(t)
	h.private = true

	// We advertise a 1000 msat minimum, but the committed config now
	// requires a 5000 msat minimum on our direction.
	h.currentUpdate = baseUpdate(1_000, 500_000)
	h.channel.LocalChanCfg.MinHTLC = 5_000
	h.channel.LocalChanCfg.MaxPendingAmount = 500_000

	err := h.mgr.ProcessDynParamsLockIn(
		context.Background(), h.channel.FundingOutpoint, lntypes.Remote,
		lnwire.FundingFlag(0),
		lnwallet.ChannelParams{
			HtlcMinimum: fn.Some(lnwire.MilliSatoshi(5_000)),
		},
	)
	require.NoError(t, err)

	require.Len(t, h.applied, 1)
	require.Equal(t, lnwire.MilliSatoshi(5_000),
		h.applied[0].HtlcMinimumMsat)
	require.Equal(t, lnwire.MilliSatoshi(500_000),
		h.applied[0].HtlcMaximumMsat)
}

// TestProcessDynParamsRealignMaxInFlight verifies that a max_in_flight decrease
// that invalidates the advertised htlc_maximum_msat triggers a corrected
// channel_update capping the maximum at the new value.
func TestProcessDynParamsRealignMaxInFlight(t *testing.T) {
	t.Parallel()

	h := newDynHarness(t)
	h.private = true

	// We advertise a 500000 msat maximum, but the committed config now caps
	// in-flight at 300000 msat, invalidating the advertised maximum.
	h.currentUpdate = baseUpdate(1_000, 500_000)
	h.channel.LocalChanCfg.MinHTLC = 1_000
	h.channel.LocalChanCfg.MaxPendingAmount = 300_000

	err := h.mgr.ProcessDynParamsLockIn(
		context.Background(), h.channel.FundingOutpoint, lntypes.Remote,
		lnwire.FundingFlag(0),
		lnwallet.ChannelParams{
			MaxValueInFlight: fn.Some(lnwire.MilliSatoshi(300_000)),
		},
	)
	require.NoError(t, err)

	require.Len(t, h.applied, 1)
	require.Equal(t, lnwire.MilliSatoshi(300_000),
		h.applied[0].HtlcMaximumMsat)
	// The max-htlc message flag must remain set.
	require.True(t, h.applied[0].MessageFlags.HasMaxHtlc())
}

// TestProcessDynParamsRealignNoop verifies that when the advertised policy
// already matches the committed params (the common proposer-side case), no
// channel_update is emitted.
func TestProcessDynParamsRealignNoop(t *testing.T) {
	t.Parallel()

	h := newDynHarness(t)
	h.private = true

	// Advertised policy already matches the committed local config.
	h.currentUpdate = baseUpdate(2_000, 400_000)
	h.channel.LocalChanCfg.MinHTLC = 2_000
	h.channel.LocalChanCfg.MaxPendingAmount = 400_000

	err := h.mgr.ProcessDynParamsLockIn(
		context.Background(), h.channel.FundingOutpoint, lntypes.Local,
		lnwire.FundingFlag(0),
		lnwallet.ChannelParams{
			HtlcMinimum: fn.Some(lnwire.MilliSatoshi(2_000)),
		},
	)
	require.NoError(t, err)
	require.Empty(t, h.applied)
}

// TestProcessDynParamsRealignFloor verifies that a committed htlc_minimum below
// our default forwarding floor does not trigger a re-advertisement, since we
// never advertise below the floor.
func TestProcessDynParamsRealignFloor(t *testing.T) {
	t.Parallel()

	h := newDynHarness(t)
	h.private = true

	// We advertise the 1000 msat floor. The committed minimum drops to 100
	// msat, which is below the floor, so our advertised minimum is unchanged.
	h.currentUpdate = baseUpdate(1_000, 500_000)
	h.channel.LocalChanCfg.MinHTLC = 100
	h.channel.LocalChanCfg.MaxPendingAmount = 500_000

	err := h.mgr.ProcessDynParamsLockIn(
		context.Background(), h.channel.FundingOutpoint, lntypes.Remote,
		lnwire.FundingFlag(0),
		lnwallet.ChannelParams{
			HtlcMinimum: fn.Some(lnwire.MilliSatoshi(100)),
		},
	)
	require.NoError(t, err)
	require.Empty(t, h.applied)
}

// mustPrivKey returns a fresh private key or fails the test.
func mustPrivKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return privKey
}
