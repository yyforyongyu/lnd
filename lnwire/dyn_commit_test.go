package lnwire

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/stretchr/testify/require"
)

// TestDynCommitEncodeDecode checks that the Encode and Decode methods for
// DynCommit (the wire dyn_commit_sig) work as expected. It bundles a commitment
// signature, the responder's dyn_ack signature, and the accepted proposal TLVs.
func TestDynCommitEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate random channel ID.
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

	// Create test data for the accepted proposal TLVs.
	testTlvData := []byte{
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

		// ExtraData - unknown odd tlv record.
		0x6f,       // type.
		0x2,        // length.
		0x79, 0x79, // value.
	}

	msg := &DynCommit{}

	totalLen := len(chanIDBytes) + len(commitSigBytes) +
		len(ackSigBytes) + len(testTlvData)
	rawBytes := make([]byte, 0, totalLen)

	rawBytes = append(rawBytes, chanIDBytes...)
	rawBytes = append(rawBytes, commitSigBytes...)
	rawBytes = append(rawBytes, ackSigBytes...)
	rawBytes = append(rawBytes, testTlvData...)

	// Decode the raw bytes.
	r := bytes.NewBuffer(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	// The channel id, both sigs and the parameters should be populated.
	require.Equal(t, chanID, msg.ChanID)
	require.Equal(t, commitSigBytes, msg.CommitSig.bytes[:])
	require.Equal(t, ackSigBytes, msg.AckSig.bytes[:])
	require.True(t, msg.DustLimit.IsSome())
	require.True(t, msg.ChannelFlags.IsSome())

	t.Logf("Encoded msg is %v", lnutils.SpewLogClosure(msg))

	// Encode the msg into raw bytes and assert the encoded bytes equal to
	// the rawBytes.
	w := new(bytes.Buffer)
	err = msg.Encode(w, 0)
	require.NoError(t, err)

	require.Equal(t, rawBytes, w.Bytes())
}
