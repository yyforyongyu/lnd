package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDynRejectBitfield checks the parameter_rejections bitfield helpers and
// that the encoding matches the bit ordering defined by the dynamic commitments
// extension BOLT.
func TestDynRejectBitfield(t *testing.T) {
	t.Parallel()

	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Reject both dust_limit_satoshis (bit 1) and
	// max_htlc_value_in_flight_msat (bit 2).
	dr := NewDynReject(
		chanID, DynRejectDustLimit, DynRejectMaxValueInFlight,
	)

	require.True(t, dr.IsRejected(DynRejectDustLimit))
	require.True(t, dr.IsRejected(DynRejectMaxValueInFlight))
	require.False(t, dr.IsRejected(DynRejectUnknownOrUnsupportedTLV))
	require.False(t, dr.IsRejected(DynRejectChannelFlags))

	// SetRejection should be additive.
	dr.SetRejection(DynRejectChannelFlags)
	require.True(t, dr.IsRejected(DynRejectChannelFlags))

	// A freshly zero-valued DynReject must not panic on IsRejected and
	// should report no rejections.
	var zero DynReject
	require.False(t, zero.IsRejected(DynRejectDustLimit))
}

// TestDynRejectEncodeDecode checks that the Encode and Decode methods for
// DynReject round trip, and that the bitfield encoding matches the BOLT example
// (rejecting dust_limit and max_htlc_value_in_flight is 0b00000110).
func TestDynRejectEncodeDecode(t *testing.T) {
	t.Parallel()

	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	dr := NewDynReject(
		chanID, DynRejectDustLimit, DynRejectMaxValueInFlight,
	)

	w := new(bytes.Buffer)
	require.NoError(t, dr.Encode(w, 0))

	// The channel id is followed by the length-prefixed feature vector. The
	// vector fits in a single byte equal to 0b00000110 = 0x06.
	encoded := w.Bytes()
	require.Equal(t, chanIDBytes, encoded[:32])
	require.Equal(t, []byte{0x00, 0x01, 0x06}, encoded[32:35])

	// Decode into a fresh message and assert the rejection bits survive the
	// round trip.
	decoded := &DynReject{}
	require.NoError(t, decoded.Decode(bytes.NewBuffer(encoded), 0))

	require.Equal(t, chanID, decoded.ChanID)
	require.True(t, decoded.IsRejected(DynRejectDustLimit))
	require.True(t, decoded.IsRejected(DynRejectMaxValueInFlight))
	require.False(t, decoded.IsRejected(DynRejectChannelReserve))
}
