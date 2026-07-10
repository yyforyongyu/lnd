package channeldb

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// dynTestSig returns a well-formed 64-byte ECDSA signature for use where the
// signature content is not itself under test.
func dynTestSig(t *testing.T) lnwire.Sig {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Produce a deterministic 64-byte signature via the dyn_ack helper so
	// the bytes are always a valid low-S ECDSA signature.
	wireSig, err := lnwire.SignDynAck(
		priv, [32]byte{}, lnwire.ChannelID{}, 0, []byte{0x00},
	)
	require.NoError(t, err)

	return wireSig
}

// dynTestPartialSig returns a well-formed musig2 partial signature with nonce
// for use where the signature content is not itself under test. The nonce is
// built from two real compressed secp256k1 points so it passes the wire-level
// nonce validation performed on decode.
func dynTestPartialSig(t *testing.T) lnwire.PartialSigWithNonce {
	t.Helper()

	priv1, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	priv2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var nonce [66]byte
	copy(nonce[:33], priv1.PubKey().SerializeCompressed())
	copy(nonce[33:], priv2.PubKey().SerializeCompressed())

	var s btcec.ModNScalar
	s.SetInt(42)

	return *lnwire.NewPartialSigWithNonce(nonce, s)
}

// dynTestProposal builds a DynPropose that exercises multiple TLV fields so the
// round-trip covers a non-trivial proposal.
func dynTestProposal() *lnwire.DynPropose {
	dp := &lnwire.DynPropose{
		ChanID: lnwire.ChannelID{0xaa, 0xbb, 0xcc},
	}

	var dust tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
	dust.Val = tlv.NewBigSizeT(btcutil.Amount(700))
	dp.DustLimit = tlv.SomeRecordT(dust)

	var csv tlv.RecordT[tlv.TlvType4, uint16]
	csv.Val = 288
	dp.CsvDelay = tlv.SomeRecordT(csv)

	var maxHtlcs tlv.RecordT[tlv.TlvType5, uint16]
	maxHtlcs.Val = 30
	dp.MaxAcceptedHTLCs = tlv.SomeRecordT(maxHtlcs)

	return dp
}

// TestDynAcceptedProposalRoundTrip persists, loads, and deletes a
// DynAcceptedProposal, asserting the loaded value matches what was stored, that
// absence yields fn.None, and that delete is idempotent.
func TestDynAcceptedProposalRoundTrip(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	chanID := lnwire.ChannelID{0x11, 0x22, 0x33}

	// Absence must yield fn.None with no error, both before the bucket
	// exists and generally.
	got, err := cdb.FetchDynAcceptedProposal(chanID)
	require.NoError(t, err)
	require.True(t, got.IsNone())

	// Deleting a non-existent entry is a no-op.
	require.NoError(t, cdb.DeleteDynAcceptedProposal(chanID))

	testCases := []struct {
		name string
		prop DynAcceptedProposal
	}{
		{
			name: "proposer, pre-ack",
			prop: DynAcceptedProposal{
				Proposer:         lntypes.Local,
				Proposal:         dynTestProposal(),
				NextCommitHeight: 7,
				AckSig:           fn.None[lnwire.Sig](),
				CommitSig:        fn.None[lnwire.Sig](),
			},
		},
		{
			name: "proposer, ack received",
			prop: DynAcceptedProposal{
				Proposer:         lntypes.Local,
				Proposal:         dynTestProposal(),
				NextCommitHeight: 42,
				AckSig:           fn.Some(dynTestSig(t)),
				CommitSig:        fn.None[lnwire.Sig](),
			},
		},
		{
			name: "responder, ack sent",
			prop: DynAcceptedProposal{
				Proposer:         lntypes.Remote,
				Proposal:         dynTestProposal(),
				NextCommitHeight: 99,
				AckSig:           fn.Some(dynTestSig(t)),
				CommitSig:        fn.None[lnwire.Sig](),
			},
		},
		{
			name: "commit sig persisted (ecdsa)",
			prop: DynAcceptedProposal{
				Proposer:         lntypes.Local,
				Proposal:         dynTestProposal(),
				NextCommitHeight: 100,
				AckSig:           fn.Some(dynTestSig(t)),
				CommitSig:        fn.Some(dynTestSig(t)),
			},
		},
		{
			name: "commit sig persisted (taproot partial)",
			prop: DynAcceptedProposal{
				Proposer:         lntypes.Local,
				Proposal:         dynTestProposal(),
				NextCommitHeight: 101,
				AckSig:           fn.Some(dynTestSig(t)),
				CommitSig:        fn.None[lnwire.Sig](),
				PartialSig: fn.Some(
					dynTestPartialSig(t),
				),
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(
				t, cdb.StoreDynAcceptedProposal(chanID, tc.prop),
			)

			loaded, err := cdb.FetchDynAcceptedProposal(chanID)
			require.NoError(t, err)
			require.True(t, loaded.IsSome())

			got := loaded.UnsafeFromSome()
			require.Equal(t, tc.prop.Proposer, got.Proposer)
			require.Equal(
				t, tc.prop.NextCommitHeight,
				got.NextCommitHeight,
			)
			require.Equal(t, tc.prop.AckSig, got.AckSig)
			require.Equal(t, tc.prop.CommitSig, got.CommitSig)
			require.Equal(t, tc.prop.PartialSig, got.PartialSig)

			// The proposal must survive the encode/decode round
			// trip byte-for-byte.
			assertProposalEqual(t, tc.prop.Proposal, got.Proposal)
		})
	}

	// Storing again overwrites, and delete removes the entry so a
	// subsequent fetch is fn.None.
	require.NoError(t, cdb.DeleteDynAcceptedProposal(chanID))

	got, err = cdb.FetchDynAcceptedProposal(chanID)
	require.NoError(t, err)
	require.True(t, got.IsNone())
}

// assertProposalEqual asserts two DynPropose messages encode identically.
func assertProposalEqual(t *testing.T, want, got *lnwire.DynPropose) {
	t.Helper()

	wantTLV, err := want.SerializeTlvData()
	require.NoError(t, err)

	gotTLV, err := got.SerializeTlvData()
	require.NoError(t, err)

	require.Equal(t, want.ChanID, got.ChanID)
	require.Equal(t, wantTLV, gotTLV)
}
