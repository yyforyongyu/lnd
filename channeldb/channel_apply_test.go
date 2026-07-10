package channeldb

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestApplyChannelParamsPolicyOnly asserts that a policy-only parameter change
// (one with no EpochParamChange) is persisted, changes the per-side bounds and
// channel flags, and crucially does NOT record any commit-chain epoch or alter
// the script/output-rendering (CSV/dust) parameters.
func TestApplyChannelParamsPolicyOnly(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")
	cdb := fullDB.ChannelStateDB()

	channel := createTestChannel(t, cdb)

	// Capture the initial rendering params so we can assert they are
	// untouched by a policy-only change.
	initLocalCP := channel.LocalChanCfg.CommitmentParams
	initRemoteCP := channel.RemoteChanCfg.CommitmentParams
	require.Empty(t, channel.CommitChainEpochHistory.Historical.Local)
	require.Empty(t, channel.CommitChainEpochHistory.Historical.Remote)

	// Change a policy-only parameter (max accepted htlcs) on the remote
	// side, and flip the announcement intent.
	newLocalBounds := channel.LocalChanCfg.ChannelStateBounds
	newRemoteBounds := channel.RemoteChanCfg.ChannelStateBounds
	newRemoteBounds.MaxAcceptedHtlcs = 42
	newRemoteBounds.ChanReserve = btcutil.Amount(7777)
	newFlags := channel.ChannelFlags ^ lnwire.FFAnnounceChannel

	err = channel.ApplyChannelParams(
		newLocalBounds, newRemoteBounds, newFlags,
		fn.None[EpochParamChange](),
	)
	require.NoError(t, err)

	assertPolicyOnly := func(t *testing.T, c *OpenChannel) {
		t.Helper()

		// Policy bounds and flags reflect the change.
		require.EqualValues(t, 42, c.RemoteChanCfg.MaxAcceptedHtlcs)
		require.EqualValues(
			t, 7777, c.RemoteChanCfg.ChanReserve,
		)
		require.Equal(t, newFlags, c.ChannelFlags)

		// No epoch was recorded, and the rendering params are unchanged.
		require.Empty(t, c.CommitChainEpochHistory.Historical.Local)
		require.Empty(t, c.CommitChainEpochHistory.Historical.Remote)
		require.Equal(t, initLocalCP, c.LocalChanCfg.CommitmentParams)
		require.Equal(t, initRemoteCP, c.RemoteChanCfg.CommitmentParams)
	}

	// The in-memory channel reflects the change...
	assertPolicyOnly(t, channel)

	// ... and so does a fresh copy loaded from disk.
	fetched, err := cdb.FetchChannel(channel.FundingOutpoint)
	require.NoError(t, err)
	assertPolicyOnly(t, fetched)
}

// TestApplyChannelParamsEpoch asserts that a script/output-rendering parameter
// change (CSV/dust) closes the outgoing epoch on both commitment chains at the
// per-side lock-in heights, installs the new normalized params, keeps the
// configs' CommitmentParams in sync with the epoch history, and persists all of
// it.
func TestApplyChannelParamsEpoch(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")
	cdb := fullDB.ChannelStateDB()

	channel := createTestChannel(t, cdb)

	// Record the outgoing (current) rendering params; these are what the
	// closed epochs must capture.
	oldLocalCP := channel.LocalChanCfg.CommitmentParams
	oldRemoteCP := channel.RemoteChanCfg.CommitmentParams

	// The Local party proposes both a new dust limit (for itself) and a new
	// CSV (imposed on the Remote counterparty).
	const (
		newDust      = btcutil.Amount(500)
		newCsv       = uint16(720)
		localLockIn  = uint64(11)
		remoteLockIn = uint64(9)
	)

	epoch := EpochParamChange{
		WhoSpecified: lntypes.Local,
		Params: CommitmentParams{
			DustLimit: newDust,
			CsvDelay:  newCsv,
		},
		LockInHeights: lntypes.Dual[uint64]{
			Local:  localLockIn,
			Remote: remoteLockIn,
		},
	}

	err = channel.ApplyChannelParams(
		channel.LocalChanCfg.ChannelStateBounds,
		channel.RemoteChanCfg.ChannelStateBounds,
		channel.ChannelFlags, fn.Some(epoch),
	)
	require.NoError(t, err)

	assertEpoch := func(t *testing.T, c *OpenChannel) {
		t.Helper()

		hist := c.CommitChainEpochHistory

		// One closed epoch per side, capturing the outgoing params at
		// the per-side lock-in heights.
		require.Len(t, hist.Historical.Local, 1)
		require.Len(t, hist.Historical.Remote, 1)
		require.Equal(t, CommitChainEpoch{
			LastHeight: localLockIn,
			Params:     oldLocalCP,
		}, hist.Historical.Local[0])
		require.Equal(t, CommitChainEpoch{
			LastHeight: remoteLockIn,
			Params:     oldRemoteCP,
		}, hist.Historical.Remote[0])

		// The proposer (Local) adopts the new dust but keeps its CSV;
		// the counterparty (Remote) adopts the new CSV but keeps its
		// dust.
		require.Equal(t, CommitmentParams{
			DustLimit: newDust,
			CsvDelay:  oldLocalCP.CsvDelay,
		}, hist.Current.Local)
		require.Equal(t, CommitmentParams{
			DustLimit: oldRemoteCP.DustLimit,
			CsvDelay:  newCsv,
		}, hist.Current.Remote)

		// The configs' rendering params must track the epoch history.
		require.Equal(
			t, hist.Current.Local, c.LocalChanCfg.CommitmentParams,
		)
		require.Equal(
			t, hist.Current.Remote,
			c.RemoteChanCfg.CommitmentParams,
		)

		// The historical params for the outgoing heights resolve back to
		// the old values, while heights past lock-in resolve to current.
		require.Equal(
			t, oldLocalCP,
			c.CommitParamsForHeight(lntypes.Local, localLockIn),
		)
		require.Equal(
			t, hist.Current.Local,
			c.CommitParamsForHeight(lntypes.Local, localLockIn+1),
		)
	}

	// The in-memory channel reflects the change...
	assertEpoch(t, channel)

	// ... and so does a fresh copy loaded from disk.
	fetched, err := cdb.FetchChannel(channel.FundingOutpoint)
	require.NoError(t, err)
	assertEpoch(t, fetched)
}
