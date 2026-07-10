package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDynProposeEncodeDecode checks that the Encode and Decode methods work as
// expected. The raw bytes exercise the dyn_propose_tlvs numbering defined by
// the dynamic commitments extension BOLT.
func TestDynProposeEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate random channel ID.
	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Create test data for the TLVs. The actual value doesn't matter, as we
	// only care about that the raw bytes can be decoded into a msg, and the
	// msg can be encoded into the exact same raw bytes.
	testTlvData := []byte{
		// DustLimit tlv (type 0).
		0x0,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize:  1_000_000).

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

	msg := &DynPropose{}

	rawBytes := make([]byte, 0, len(chanIDBytes)+len(testTlvData))
	rawBytes = append(rawBytes, chanIDBytes...)
	rawBytes = append(rawBytes, testTlvData...)

	r := bytes.NewBuffer(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	// Every known parameter should have been mapped to its field.
	require.True(t, msg.DustLimit.IsSome())
	require.True(t, msg.MaxValueInFlight.IsSome())
	require.True(t, msg.HtlcMinimum.IsSome())
	require.True(t, msg.ChannelReserve.IsSome())
	require.True(t, msg.CsvDelay.IsSome())
	require.True(t, msg.MaxAcceptedHTLCs.IsSome())
	require.True(t, msg.ChannelFlags.IsSome())

	// The channel flags should round trip to the announce bit.
	flags := msg.ChannelFlags.UnwrapOrFailV(t)
	require.Equal(t, FFAnnounceChannel, flags)

	w := new(bytes.Buffer)
	err = msg.Encode(w, 0)
	require.NoError(t, err)

	require.Equal(t, rawBytes, w.Bytes())
}
