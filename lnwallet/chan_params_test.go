package lnwallet

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// paramsTestCapacity is the channel capacity used by the validation tests. Its
// fifth (the max reserve) is 200_000 sat.
const paramsTestCapacity = btcutil.Amount(1_000_000)

// paramsMaxCSV is the maximum CSV delay used by the validation tests.
const paramsMaxCSV = uint16(2016)

// validParamConfig returns a ChannelConfig whose dynamic parameters all satisfy
// BOLT #2 and LND policy for a paramsTestCapacity channel.
func validParamConfig(dust btcutil.Amount, csv uint16,
	reserve btcutil.Amount, maxInFlight, minHTLC lnwire.MilliSatoshi,
	maxAccepted uint16) channeldb.ChannelConfig {

	return channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			ChanReserve:      reserve,
			MaxPendingAmount: maxInFlight,
			MinHTLC:          minHTLC,
			MaxAcceptedHtlcs: maxAccepted,
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: dust,
			CsvDelay:  csv,
		},
	}
}

// TestValidateChannelParams verifies that ValidateChannelParams accepts a sane
// resulting param set and rejects each cross-coupled or out-of-bounds violation
// with the expected sentinel error.
func TestValidateChannelParams(t *testing.T) {
	t.Parallel()

	// A fully valid baseline for both peers.
	baseLocal := validParamConfig(
		500, 144, 20_000, 500_000, 1_000, 100,
	)
	baseRemote := validParamConfig(
		600, 144, 20_000, 500_000, 1_000, 100,
	)

	tests := []struct {
		name    string
		local   channeldb.ChannelConfig
		remote  channeldb.ChannelConfig
		wantErr error
	}{
		{
			name:   "valid",
			local:  baseLocal,
			remote: baseRemote,
		},
		{
			name: "dust below minimum",
			local: validParamConfig(
				300, 144, 20_000, 500_000, 1_000, 100,
			),
			remote:  baseRemote,
			wantErr: ErrDynDustLimitTooSmall,
		},
		{
			name:  "dust above maximum",
			local: baseLocal,
			remote: validParamConfig(
				2_000, 144, 20_000, 500_000, 1_000, 100,
			),
			wantErr: ErrDynDustLimitTooLarge,
		},
		{
			// Both dust limits are individually valid, but the
			// smaller reserve drops below the larger dust limit.
			name: "reserve below dust",
			local: validParamConfig(
				500, 144, 400, 500_000, 1_000, 100,
			),
			remote: validParamConfig(
				600, 144, 20_000, 500_000, 1_000, 100,
			),
			wantErr: ErrDynReserveBelowDust,
		},
		{
			name:  "reserve over capacity",
			local: baseLocal,
			remote: validParamConfig(
				600, 144, 300_000, 500_000, 1_000, 100,
			),
			wantErr: ErrDynReserveTooLarge,
		},
		{
			name: "csv too large",
			local: validParamConfig(
				500, 5_000, 20_000, 500_000, 1_000, 100,
			),
			remote:  baseRemote,
			wantErr: ErrDynCsvDelayTooLarge,
		},
		{
			name:  "max accepted htlcs too large",
			local: baseLocal,
			remote: validParamConfig(
				600, 144, 20_000, 500_000, 1_000, 484,
			),
			wantErr: ErrDynMaxAcceptedTooLarge,
		},
		{
			name: "htlc minimum exceeds max in flight",
			local: validParamConfig(
				500, 144, 20_000, 1_000, 2_000, 100,
			),
			remote:  baseRemote,
			wantErr: ErrDynHtlcMinTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateChannelParams(
				tc.local, tc.remote, paramsTestCapacity,
				paramsMaxCSV,
			)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestChannelParamsDynProposeRoundTrip verifies that a ChannelParams survives a
// round-trip through the lnwire.DynPropose message, and that a partial proposal
// only carries its set fields.
func TestChannelParamsDynProposeRoundTrip(t *testing.T) {
	t.Parallel()

	var chanID lnwire.ChannelID

	// A full proposal round-trips losslessly.
	full := ChannelParams{
		DustLimit:        fn.Some(btcutil.Amount(500)),
		MaxValueInFlight: fn.Some(lnwire.MilliSatoshi(600_000)),
		HtlcMinimum:      fn.Some(lnwire.MilliSatoshi(1_000)),
		ChannelReserve:   fn.Some(btcutil.Amount(20_000)),
		CsvDelay:         fn.Some(uint16(288)),
		MaxAcceptedHtlcs: fn.Some(uint16(100)),
		ChannelFlags:     fn.Some(lnwire.FFAnnounceChannel),
	}
	require.Equal(
		t, full,
		ChannelParamsFromDynPropose(full.ToDynPropose(chanID)),
	)

	// A partial proposal only carries its set fields.
	partial := ChannelParams{
		CsvDelay:       fn.Some(uint16(144)),
		ChannelReserve: fn.Some(btcutil.Amount(9_000)),
	}
	dp := partial.ToDynPropose(chanID)
	require.True(t, dp.DustLimit.IsNone())
	require.True(t, dp.MaxAcceptedHTLCs.IsNone())
	require.True(t, dp.CsvDelay.IsSome())
	require.True(t, dp.ChannelReserve.IsSome())
	require.Equal(t, partial, ChannelParamsFromDynPropose(dp))
}

// TestChannelParamsConfigRoundTrip verifies that extracting a party's
// advertised parameters from the channel configs and re-applying them yields
// the original configs, for both parties.
func TestChannelParamsConfigRoundTrip(t *testing.T) {
	t.Parallel()

	local := validParamConfig(500, 5, 20_000, 500_000, 1_000, 100)
	remote := validParamConfig(600, 4, 30_000, 400_000, 2_000, 200)

	for _, party := range []lntypes.ChannelParty{
		lntypes.Local, lntypes.Remote,
	} {
		t.Run(party.String(), func(t *testing.T) {
			t.Parallel()

			params := ChannelParamsFromConfigs(
				party, local, remote, lnwire.FFAnnounceChannel,
			)
			gotLocal, gotRemote := params.ApplyToConfigs(
				party, local, remote,
			)
			require.Equal(t, local, gotLocal)
			require.Equal(t, remote, gotRemote)
		})
	}
}

// TestChannelParamsApplyToConfigs asserts the proposer-owned mapping: the
// proposer's own dust limit updates its own config, while the remaining
// parameters update the counterparty's config.
func TestChannelParamsApplyToConfigs(t *testing.T) {
	t.Parallel()

	local := validParamConfig(500, 5, 20_000, 500_000, 1_000, 100)
	remote := validParamConfig(600, 4, 30_000, 400_000, 2_000, 200)

	params := ChannelParams{
		DustLimit:        fn.Some(btcutil.Amount(400)),
		CsvDelay:         fn.Some(uint16(288)),
		ChannelReserve:   fn.Some(btcutil.Amount(25_000)),
		MaxAcceptedHtlcs: fn.Some(uint16(150)),
	}

	// Local proposes: its own dust changes, and the rest land on Remote.
	newLocal, newRemote := params.ApplyToConfigs(lntypes.Local, local, remote)
	require.EqualValues(t, 400, newLocal.DustLimit)
	require.EqualValues(t, 5, newLocal.CsvDelay)         // unchanged
	require.EqualValues(t, 20_000, newLocal.ChanReserve) // unchanged
	require.EqualValues(t, 288, newRemote.CsvDelay)
	require.EqualValues(t, 25_000, newRemote.ChanReserve)
	require.EqualValues(t, 150, newRemote.MaxAcceptedHtlcs)
	require.EqualValues(t, 600, newRemote.DustLimit) // unchanged

	// Remote proposes: its own dust changes, and the rest land on Local.
	newLocal, newRemote = params.ApplyToConfigs(lntypes.Remote, local, remote)
	require.EqualValues(t, 400, newRemote.DustLimit)
	require.EqualValues(t, 288, newLocal.CsvDelay)
	require.EqualValues(t, 25_000, newLocal.ChanReserve)
	require.EqualValues(t, 150, newLocal.MaxAcceptedHtlcs)
	require.EqualValues(t, 500, newLocal.DustLimit) // unchanged
}

// newParamsTestChannel returns a persisted LightningChannel whose dust limits
// have been brought into the range dynamic-params validation enforces (the
// default test dust limits of 200/1300 fall outside [354, 1062]).
func newParamsTestChannel(t *testing.T) *LightningChannel {
	t.Helper()

	alice, _, err := CreateTestChannels(t, channeldb.SingleFunderTweaklessBit)
	require.NoError(t, err)

	cs := alice.channelState
	heights := lntypes.Dual[uint64]{Local: 1, Remote: 1}

	// Bring the Local dust into range (keeping the Remote's CSV).
	require.NoError(t, cs.ApplyChannelParams(
		cs.LocalChanCfg.ChannelStateBounds,
		cs.RemoteChanCfg.ChannelStateBounds, cs.ChannelFlags,
		fn.Some(channeldb.EpochParamChange{
			WhoSpecified: lntypes.Local,
			Params: channeldb.CommitmentParams{
				DustLimit: 500,
				CsvDelay:  cs.RemoteChanCfg.CsvDelay,
			},
			LockInHeights: heights,
		}),
	))

	// Bring the Remote dust into range (keeping the Local's CSV).
	require.NoError(t, cs.ApplyChannelParams(
		cs.LocalChanCfg.ChannelStateBounds,
		cs.RemoteChanCfg.ChannelStateBounds, cs.ChannelFlags,
		fn.Some(channeldb.EpochParamChange{
			WhoSpecified: lntypes.Remote,
			Params: channeldb.CommitmentParams{
				DustLimit: 600,
				CsvDelay:  cs.LocalChanCfg.CsvDelay,
			},
			LockInHeights: heights,
		}),
	))

	return alice
}

// epochCounts returns the number of closed epochs on each side.
func epochCounts(c *channeldb.OpenChannel) (int, int) {
	return len(c.CommitChainEpochHistory.Historical.Local),
		len(c.CommitChainEpochHistory.Historical.Remote)
}

// TestLightningChannelApplyChannelParams exercises the end-to-end apply flow on
// a LightningChannel: policy-only changes must not touch the epoch history,
// CSV/dust changes must, invalid proposals are rejected without mutating state,
// and an empty proposal is rejected.
func TestLightningChannelApplyChannelParams(t *testing.T) {
	t.Parallel()

	heights := lntypes.Dual[uint64]{Local: 5, Remote: 4}

	t.Run("policy-only records no epoch", func(t *testing.T) {
		t.Parallel()

		lc := newParamsTestChannel(t)
		cs := lc.channelState
		localEpochs, remoteEpochs := epochCounts(cs)

		// Local proposes a new max_accepted_htlcs, which is a policy
		// parameter imposed on the Remote counterparty.
		params := ChannelParams{
			MaxAcceptedHtlcs: fn.Some(uint16(200)),
		}
		require.NoError(t, lc.ApplyChannelParams(
			lntypes.Local, params, paramsMaxCSV, heights,
		))

		// The counterparty's config reflects the change...
		require.EqualValues(
			t, 200, cs.RemoteChanCfg.MaxAcceptedHtlcs,
		)

		// ... and no new epoch was recorded on either side.
		gotLocal, gotRemote := epochCounts(cs)
		require.Equal(t, localEpochs, gotLocal)
		require.Equal(t, remoteEpochs, gotRemote)
	})

	t.Run("csv change records epoch", func(t *testing.T) {
		t.Parallel()

		lc := newParamsTestChannel(t)
		cs := lc.channelState
		localEpochs, remoteEpochs := epochCounts(cs)
		oldLocalCsv := cs.LocalChanCfg.CsvDelay

		// Local proposes a new CSV, which is imposed on the Remote
		// counterparty and is epoch-affecting.
		params := ChannelParams{
			CsvDelay: fn.Some(uint16(1_000)),
		}
		require.NoError(t, lc.ApplyChannelParams(
			lntypes.Local, params, paramsMaxCSV, heights,
		))

		// The counterparty's CSV changed; the proposer's is untouched.
		require.EqualValues(t, 1_000, cs.RemoteChanCfg.CsvDelay)
		require.Equal(t, oldLocalCsv, cs.LocalChanCfg.CsvDelay)

		// A new epoch was recorded on each side at the lock-in heights.
		gotLocal, gotRemote := epochCounts(cs)
		require.Equal(t, localEpochs+1, gotLocal)
		require.Equal(t, remoteEpochs+1, gotRemote)

		lastLocal := cs.CommitChainEpochHistory.Historical.Local[gotLocal-1]
		lastRemote :=
			cs.CommitChainEpochHistory.Historical.Remote[gotRemote-1]
		require.EqualValues(t, heights.Local, lastLocal.LastHeight)
		require.EqualValues(t, heights.Remote, lastRemote.LastHeight)

		// The config's rendering params track the epoch history.
		require.Equal(
			t, cs.CommitChainEpochHistory.Current.Remote,
			cs.RemoteChanCfg.CommitmentParams,
		)
	})

	t.Run("invalid proposal rejected", func(t *testing.T) {
		t.Parallel()

		lc := newParamsTestChannel(t)
		cs := lc.channelState
		before := cs.RemoteChanCfg.MaxAcceptedHtlcs
		localEpochs, remoteEpochs := epochCounts(cs)

		// 484 exceeds the protocol maximum of 483.
		params := ChannelParams{
			MaxAcceptedHtlcs: fn.Some(uint16(484)),
		}
		err := lc.ApplyChannelParams(
			lntypes.Local, params, paramsMaxCSV, heights,
		)
		require.ErrorIs(t, err, ErrDynMaxAcceptedTooLarge)

		// No state was mutated.
		require.Equal(t, before, cs.RemoteChanCfg.MaxAcceptedHtlcs)
		gotLocal, gotRemote := epochCounts(cs)
		require.Equal(t, localEpochs, gotLocal)
		require.Equal(t, remoteEpochs, gotRemote)
	})

	t.Run("empty proposal rejected", func(t *testing.T) {
		t.Parallel()

		lc := newParamsTestChannel(t)
		err := lc.ApplyChannelParams(
			lntypes.Local, ChannelParams{}, paramsMaxCSV, heights,
		)
		require.True(t, errors.Is(err, ErrDynParamsNoChange))
	})
}
