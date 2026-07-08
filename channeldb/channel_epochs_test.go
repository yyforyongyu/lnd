package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// encodeDecodeEpochHistory round-trips a CommitChainEpochHistory through a TLV
// stream using its Record() producer and returns the decoded value. It also
// asserts that Size() exactly matches the number of encoded bytes, which is
// required for the TLV length prefix to be correct.
func encodeDecodeEpochHistory(t *testing.T,
	hist CommitChainEpochHistory) CommitChainEpochHistory {

	t.Helper()

	// First, assert that the raw body encoding matches the advertised size.
	var body bytes.Buffer
	require.NoError(t, hist.encode(&body))
	require.EqualValues(t, hist.Size(), body.Len(),
		"Size() must match encoded body length")

	// Now round-trip through a full TLV stream via the RecordProducer.
	var buf bytes.Buffer
	encStream, err := tlv.NewStream(hist.Record())
	require.NoError(t, err)
	require.NoError(t, encStream.Encode(&buf))

	var decoded CommitChainEpochHistory
	decStream, err := tlv.NewStream(decoded.Record())
	require.NoError(t, err)
	require.NoError(t, decStream.Decode(bytes.NewReader(buf.Bytes())))

	return decoded
}

// TestCommitChainEpochHistoryEncodeDecode verifies that a
// CommitChainEpochHistory survives a TLV encode/decode round-trip for the
// empty, single-epoch, and multi-epoch cases across both sides.
func TestCommitChainEpochHistoryEncodeDecode(t *testing.T) {
	t.Parallel()

	current := lntypes.Dual[CommitmentParams]{
		Local:  CommitmentParams{DustLimit: 354, CsvDelay: 144},
		Remote: CommitmentParams{DustLimit: 546, CsvDelay: 288},
	}

	testCases := []struct {
		name string
		hist CommitChainEpochHistory
	}{
		{
			name: "empty history",
			hist: NewCommitChainEpochHistory(current),
		},
		{
			name: "local epochs only",
			hist: CommitChainEpochHistory{
				Current: current,
				Historical: lntypes.Dual[[]CommitChainEpoch]{
					Local: []CommitChainEpoch{
						{
							LastHeight: 10,
							Params: CommitmentParams{
								DustLimit: 330,
								CsvDelay:  100,
							},
						},
					},
				},
			},
		},
		{
			name: "multi-epoch both sides",
			hist: CommitChainEpochHistory{
				Current: current,
				Historical: lntypes.Dual[[]CommitChainEpoch]{
					Local: []CommitChainEpoch{
						{
							LastHeight: 5,
							Params: CommitmentParams{
								DustLimit: 300,
								CsvDelay:  50,
							},
						},
						{
							LastHeight: 12,
							Params: CommitmentParams{
								DustLimit: 320,
								CsvDelay:  75,
							},
						},
					},
					Remote: []CommitChainEpoch{
						{
							LastHeight: 7,
							Params: CommitmentParams{
								DustLimit: 400,
								CsvDelay:  60,
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			decoded := encodeDecodeEpochHistory(t, tc.hist)
			require.Equal(t, tc.hist, decoded)
		})
	}
}

// TestCommitChainEpochHistoryParamsAt verifies that ParamsAt returns the
// parameters in effect at a given height, treating epoch boundaries as
// inclusive of the LastHeight and resolving each side independently.
func TestCommitChainEpochHistoryParamsAt(t *testing.T) {
	t.Parallel()

	var (
		localEpoch0 = CommitmentParams{DustLimit: 300, CsvDelay: 50}
		localEpoch1 = CommitmentParams{DustLimit: 320, CsvDelay: 75}
		localCurr   = CommitmentParams{DustLimit: 340, CsvDelay: 100}

		remoteEpoch0 = CommitmentParams{DustLimit: 400, CsvDelay: 60}
		remoteCurr   = CommitmentParams{DustLimit: 420, CsvDelay: 90}
	)

	hist := CommitChainEpochHistory{
		Current: lntypes.Dual[CommitmentParams]{
			Local:  localCurr,
			Remote: remoteCurr,
		},
		Historical: lntypes.Dual[[]CommitChainEpoch]{
			Local: []CommitChainEpoch{
				{LastHeight: 5, Params: localEpoch0},
				{LastHeight: 10, Params: localEpoch1},
			},
			Remote: []CommitChainEpoch{
				{LastHeight: 7, Params: remoteEpoch0},
			},
		},
	}

	tests := []struct {
		name   string
		party  lntypes.ChannelParty
		height uint64
		want   CommitmentParams
	}{
		{
			name:   "local at first epoch boundary",
			party:  lntypes.Local,
			height: 5,
			want:   localEpoch0,
		},
		{
			name:   "local within first epoch",
			party:  lntypes.Local,
			height: 3,
			want:   localEpoch0,
		},
		{
			name:   "local just past first boundary",
			party:  lntypes.Local,
			height: 6,
			want:   localEpoch1,
		},
		{
			name:   "local at second epoch boundary",
			party:  lntypes.Local,
			height: 10,
			want:   localEpoch1,
		},
		{
			name:   "local past all epochs uses current",
			party:  lntypes.Local,
			height: 11,
			want:   localCurr,
		},
		{
			name:   "remote within only epoch",
			party:  lntypes.Remote,
			height: 7,
			want:   remoteEpoch0,
		},
		{
			name:   "remote past epoch uses current",
			party:  lntypes.Remote,
			height: 8,
			want:   remoteCurr,
		},
		{
			name:   "remote independent of local boundary",
			party:  lntypes.Remote,
			height: 6,
			want:   remoteEpoch0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hist.ParamsAt(tc.party, tc.height)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestCommitChainEpochHistoryParamsAtEmpty verifies that when there is no
// history, ParamsAt always resolves to the current parameters for the queried
// side.
func TestCommitChainEpochHistoryParamsAtEmpty(t *testing.T) {
	t.Parallel()

	current := lntypes.Dual[CommitmentParams]{
		Local:  CommitmentParams{DustLimit: 354, CsvDelay: 144},
		Remote: CommitmentParams{DustLimit: 546, CsvDelay: 288},
	}
	hist := NewCommitChainEpochHistory(current)

	require.Equal(t, current.Local, hist.ParamsAt(lntypes.Local, 0))
	require.Equal(t, current.Local, hist.ParamsAt(lntypes.Local, 9999))
	require.Equal(t, current.Remote, hist.ParamsAt(lntypes.Remote, 42))
}

// TestCommitChainEpochHistoryRecordParamChange verifies that recording a
// parameter change closes out the current epoch on both chains at the given
// lock-in heights and normalizes the new current parameters correctly: the
// specifying party's dust changes while its CSV carries over, and the
// counterparty's CSV changes while its dust carries over.
func TestCommitChainEpochHistoryRecordParamChange(t *testing.T) {
	t.Parallel()

	origLocal := CommitmentParams{DustLimit: 300, CsvDelay: 50}
	origRemote := CommitmentParams{DustLimit: 400, CsvDelay: 60}

	hist := NewCommitChainEpochHistory(lntypes.Dual[CommitmentParams]{
		Local:  origLocal,
		Remote: origRemote,
	})

	// The local party declares a change: a new dust limit for itself and a
	// new to_self_delay that it imposes on the remote party.
	newParams := CommitmentParams{DustLimit: 333, CsvDelay: 144}
	lockIn := lntypes.Dual[uint64]{Local: 10, Remote: 12}
	hist.RecordParamChange(lntypes.Local, newParams, lockIn)

	// The outgoing epochs should be closed at their respective heights with
	// the original parameters.
	require.Equal(t, []CommitChainEpoch{
		{LastHeight: 10, Params: origLocal},
	}, hist.Historical.Local)
	require.Equal(t, []CommitChainEpoch{
		{LastHeight: 12, Params: origRemote},
	}, hist.Historical.Remote)

	// The specifying party (local) keeps its old CSV but takes the new dust.
	require.Equal(t, CommitmentParams{
		DustLimit: 333,
		CsvDelay:  50,
	}, hist.Current.Local)

	// The counterparty (remote) keeps its old dust but takes the new CSV.
	require.Equal(t, CommitmentParams{
		DustLimit: 400,
		CsvDelay:  144,
	}, hist.Current.Remote)

	// Lookups before and after the boundary should now differ per side.
	require.Equal(t, origLocal, hist.ParamsAt(lntypes.Local, 10))
	require.Equal(t, hist.Current.Local, hist.ParamsAt(lntypes.Local, 11))
	require.Equal(t, origRemote, hist.ParamsAt(lntypes.Remote, 12))
	require.Equal(t, hist.Current.Remote, hist.ParamsAt(lntypes.Remote, 13))

	// A second change should stack onto the history and remain
	// round-trippable through TLV.
	hist.RecordParamChange(
		lntypes.Remote,
		CommitmentParams{DustLimit: 555, CsvDelay: 200},
		lntypes.Dual[uint64]{Local: 20, Remote: 22},
	)
	require.Len(t, hist.Historical.Local, 2)
	require.Len(t, hist.Historical.Remote, 2)

	decoded := encodeDecodeEpochHistory(t, hist)
	require.Equal(t, hist, decoded)
}

// TestOpenChannelCommitEpochHistoryDefault verifies that a channel persisted
// without an epoch history (the read-time default path) is read back with a
// single implicit "epoch 0" per side derived from its channel configs.
func TestOpenChannelCommitEpochHistoryDefault(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)

	cdb := fullDB.ChannelStateDB()

	state := createTestChannel(t, cdb)

	openChannels, err := cdb.FetchOpenChannels(state.IdentityPub)
	require.NoError(t, err)
	fetched := openChannels[0]

	// No epoch was ever recorded, so nothing is persisted; the history must
	// be defaulted from the channel configs.
	require.Empty(t, fetched.CommitChainEpochHistory.Historical.Local)
	require.Empty(t, fetched.CommitChainEpochHistory.Historical.Remote)

	require.Equal(
		t, fetched.LocalChanCfg.CommitmentParams,
		fetched.CommitParamsForHeight(lntypes.Local, 0),
	)
	require.Equal(
		t, fetched.LocalChanCfg.CommitmentParams,
		fetched.CommitParamsForHeight(lntypes.Local, 123456),
	)
	require.Equal(
		t, fetched.RemoteChanCfg.CommitmentParams,
		fetched.CommitParamsForHeight(lntypes.Remote, 999),
	)
}

// TestOpenChannelCommitEpochHistoryPersistence verifies that a recorded
// parameter change is persisted as an optional TLV and survives a fetch, with
// historical lookups returning the pre-change parameters.
func TestOpenChannelCommitEpochHistoryPersistence(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)

	cdb := fullDB.ChannelStateDB()

	state := createTestChannel(t, cdb)

	// Capture the original per-side params so we can assert on them after
	// the change is recorded.
	origLocal := state.LocalChanCfg.CommitmentParams
	origRemote := state.RemoteChanCfg.CommitmentParams

	// Record a change on the local side and persist it.
	hist := state.CommitChainEpochHistory
	newParams := CommitmentParams{
		DustLimit: origLocal.DustLimit + btcutil.Amount(11),
		CsvDelay:  origRemote.CsvDelay + 7,
	}
	hist.RecordParamChange(
		lntypes.Local, newParams,
		lntypes.Dual[uint64]{Local: 100, Remote: 200},
	)
	require.NoError(t, state.PutCommitChainEpochHistory(hist))

	// Re-fetch from disk and confirm the history was persisted verbatim.
	openChannels, err := cdb.FetchOpenChannels(state.IdentityPub)
	require.NoError(t, err)
	fetched := openChannels[0]

	require.Equal(t, hist, fetched.CommitChainEpochHistory)

	// Historical lookups by height should now return the pre-change params
	// before the boundary and the new params after it, per side.
	require.Equal(
		t, origLocal, fetched.CommitParamsForHeight(lntypes.Local, 100),
	)
	require.Equal(
		t, hist.Current.Local,
		fetched.CommitParamsForHeight(lntypes.Local, 101),
	)
	require.Equal(
		t, origRemote,
		fetched.CommitParamsForHeight(lntypes.Remote, 200),
	)
	require.Equal(
		t, hist.Current.Remote,
		fetched.CommitParamsForHeight(lntypes.Remote, 201),
	)
}
