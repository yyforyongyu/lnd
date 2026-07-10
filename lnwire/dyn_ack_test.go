package lnwire

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/stretchr/testify/require"
)

// TestDynAckEncodeDecode checks that the Encode and Decode methods for DynAck
// work as expected. DynAck carries only a channel id and an acceptance
// signature; any trailing bytes are opaque extra data.
func TestDynAckEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate random channel ID.
	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Generate random sig.
	sigBytes, err := generateRandomBytes(64)
	require.NoError(t, err)

	var sig Sig
	copy(sig.bytes[:], sigBytes)

	// Trailing opaque extra data.
	extraData := []byte{
		0x6f,       // type.
		0x2,        // length.
		0x79, 0x79, // value.
	}

	msg := &DynAck{}

	totalLen := len(chanIDBytes) + len(sigBytes) + len(extraData)
	rawBytes := make([]byte, 0, totalLen)

	rawBytes = append(rawBytes, chanIDBytes...)
	rawBytes = append(rawBytes, sigBytes...)
	rawBytes = append(rawBytes, extraData...)

	// Decode the raw bytes.
	r := bytes.NewBuffer(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	require.Equal(t, chanID, msg.ChanID)
	require.Equal(t, sig.bytes, msg.Sig.bytes)

	t.Logf("Encoded msg is %v", lnutils.SpewLogClosure(msg))

	// Encode the msg into raw bytes and assert the encoded bytes equal to
	// the rawBytes.
	w := new(bytes.Buffer)
	err = msg.Encode(w, 0)
	require.NoError(t, err)

	require.Equal(t, rawBytes, w.Bytes())
}
