package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// acceptedProposalTlvs returns the serialized accepted_tlvs stream (the
// dyn_propose_tlvs records for types 0 through 6) used by the DynCommit tests.
func acceptedProposalTlvs() []byte {
	return []byte{
		// DustLimit tlv (type 0).
		0x0,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// MaxValueInFlight tlv (type 1).
		0x1,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// HtlcMinimum tlv (type 2).
		0x2,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// ChannelReserve tlv (type 3).
		0x3,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// CsvDelay tlv (type 4).
		0x4,      // type.
		0x2,      // length.
		0x0, 0x8, // value.

		// MaxAcceptedHTLCs tlv (type 5).
		0x5,      // type.
		0x2,      // length.
		0x0, 0x8, // value.

		// ChannelFlags tlv (type 6).
		0x6, // type.
		0x1, // length.
		0x1, // value (FFAnnounceChannel).
	}
}

// TestDynCommitEncodeDecode checks that the Encode and Decode methods for
// DynCommit (the wire dyn_commit_sig) round-trip correctly. It exercises both
// the non-taproot form (an ECDSA commit_signature with no partial signature)
// and the taproot form (a zeroed commit_signature plus a
// partial_signature_with_nonce record in the dyn_commit_sig_tlvs stream).
func TestDynCommitEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate a random channel ID.
	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Generate two random sigs: the proposer's commitment sig and the
	// responder's dyn_ack sig.
	commitSigBytes, err := generateRandomBytes(64)
	require.NoError(t, err)

	ackSigBytes, err := generateRandomBytes(64)
	require.NoError(t, err)

	// The non-taproot form omits the partial signature and carries an
	// unknown odd record in the trailing dyn_commit_sig_tlvs stream to
	// verify that message-level extra data round-trips.
	t.Run("non-taproot", func(t *testing.T) {
		t.Parallel()

		acceptedTlvData := acceptedProposalTlvs()
		require.Less(t, len(acceptedTlvData), 0xfd)

		// The trailing dyn_commit_sig_tlvs stream, carrying just an
		// unknown odd extra record (no partial signature).
		trailingTlvData := []byte{
			0x6f,       // type.
			0x2,        // length.
			0x79, 0x79, // value.
		}

		// Assemble the wire bytes: channel_id || commit_signature ||
		// dyn_ack_signature || bigsize(accepted_tlvs_len) ||
		// accepted_tlvs || dyn_commit_sig_tlvs.
		rawBytes := make([]byte, 0)
		rawBytes = append(rawBytes, chanIDBytes...)
		rawBytes = append(rawBytes, commitSigBytes...)
		rawBytes = append(rawBytes, ackSigBytes...)
		rawBytes = append(rawBytes, byte(len(acceptedTlvData)))
		rawBytes = append(rawBytes, acceptedTlvData...)
		rawBytes = append(rawBytes, trailingTlvData...)

		// Decode the raw bytes.
		msg := &DynCommit{}
		r := bytes.NewBuffer(rawBytes)
		require.NoError(t, msg.Decode(r, 0))

		// The channel id, both sigs and the proposal parameters should
		// be populated, while the partial signature is absent.
		require.Equal(t, chanID, msg.ChanID)
		require.Equal(t, commitSigBytes, msg.CommitSig.bytes[:])
		require.Equal(t, ackSigBytes, msg.AckSig.bytes[:])
		require.True(t, msg.DustLimit.IsSome())
		require.True(t, msg.ChannelFlags.IsSome())
		require.True(t, msg.PartialSig.IsNone())

		t.Logf("Encoded msg is %v", lnutils.SpewLogClosure(msg))

		// Encoding the message again must reproduce the raw bytes.
		w := new(bytes.Buffer)
		require.NoError(t, msg.Encode(w, 0))
		require.Equal(t, rawBytes, w.Bytes())
	})

	// The taproot form sets commit_signature to 64 zero bytes and includes
	// the partial_signature_with_nonce record. We build the message from a
	// struct and assert a full struct round-trip.
	t.Run("taproot", func(t *testing.T) {
		t.Parallel()

		// Build a valid MuSig2 partial signature with nonce.
		sig, err := NewSigFromSchnorrRawSignature(commitSigBytes)
		require.NoError(t, err)

		sigScalar := new(btcec.ModNScalar)
		sigScalar.SetByteSlice(sig.RawBytes())

		// Generate a valid MuSig2 nonce (two compressed public keys).
		_, pub1 := btcec.PrivKeyFromBytes(chanIDBytes)
		_, pub2 := btcec.PrivKeyFromBytes(commitSigBytes[:32])

		var nonce Musig2Nonce
		copy(nonce[:33], pub1.SerializeCompressed())
		copy(nonce[33:], pub2.SerializeCompressed())

		sigWithNonce := NewPartialSigWithNonce(nonce, *sigScalar)

		// Assemble a taproot DynCommit: a zeroed commit_signature, the
		// dyn_ack signature, an accepted proposal parameter, the
		// partial signature, and an unknown extra record.
		dp := DynPropose{ChanID: chanID}

		csvDelay := dp.CsvDelay.Zero()
		csvDelay.Val = 144
		dp.CsvDelay = tlv.SomeRecordT(csvDelay)

		var ackSig Sig
		copy(ackSig.bytes[:], ackSigBytes)

		msg := &DynCommit{
			DynPropose: dp,
			AckSig:     ackSig,
			PartialSig: MaybePartialSigWithNonce(sigWithNonce),
			ExtraData:  ExtraOpaqueData{0x6f, 0x2, 0x79, 0x79},
		}

		// The commit_signature MUST be all-zero for taproot channels.
		require.Equal(t, Sig{}, msg.CommitSig)

		// Encode, then decode into a fresh message.
		w := new(bytes.Buffer)
		require.NoError(t, msg.Encode(w, 0))

		t.Logf("Encoded msg is %v", lnutils.SpewLogClosure(msg))

		decoded := &DynCommit{}
		require.NoError(
			t, decoded.Decode(bytes.NewBuffer(w.Bytes()), 0),
		)

		// The decoded message must match the original, including the
		// preserved partial signature.
		require.Equal(t, msg, decoded)
		require.True(t, decoded.PartialSig.IsSome())
		require.Equal(t, Sig{}, decoded.CommitSig)
		require.True(t, decoded.CsvDelay.IsSome())

		decoded.PartialSig.WhenSomeV(func(p PartialSigWithNonce) {
			require.Equal(t, nonce, p.Nonce)
		})
	})
}
