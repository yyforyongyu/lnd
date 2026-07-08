package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// dynAckTestProposal builds a DynPropose with a couple of parameters set and
// returns the serialized dyn_propose_tlvs stream that a dyn_ack signature
// covers.
func dynAckTestProposal(t *testing.T, chanID ChannelID) []byte {
	t.Helper()

	dp := &DynPropose{ChanID: chanID}

	var csvRec tlv.RecordT[tlv.TlvType4, uint16]
	csvRec.Val = 144
	dp.CsvDelay = tlv.SomeRecordT(csvRec)

	var maxRec tlv.RecordT[tlv.TlvType5, uint16]
	maxRec.Val = 30
	dp.MaxAcceptedHTLCs = tlv.SomeRecordT(maxRec)

	tlvData, err := dp.SerializeTlvData()
	require.NoError(t, err)

	return tlvData
}

// TestDynAckSignVerify checks the dyn_ack signature helper over both a freshly
// signed signature and one that has round-tripped through the wire, plus the
// relevant negative paths.
func TestDynAckSignVerify(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()

	var chainHash chainhash.Hash
	copy(chainHash[:], bytes.Repeat([]byte{0xab}, 32))

	var chanID ChannelID
	copy(chanID[:], bytes.Repeat([]byte{0xcd}, 32))

	const nextHeight uint64 = 42

	tlvData := dynAckTestProposal(t, chanID)

	// The digest must be deterministic for identical inputs.
	require.Equal(t,
		DynAckSigDigest(chainHash, chanID, nextHeight, tlvData),
		DynAckSigDigest(chainHash, chanID, nextHeight, tlvData),
	)

	sig, err := SignDynAck(priv, chainHash, chanID, nextHeight, tlvData)
	require.NoError(t, err)

	// Positive: verifies under the signer's identity key.
	require.NoError(t, VerifyDynAck(
		pub, sig, chainHash, chanID, nextHeight, tlvData,
	))

	// The signature should still verify after a wire round trip through a
	// DynAck message (which resets the sig type to ECDSA on decode).
	ack := &DynAck{ChanID: chanID, Sig: sig}
	w := new(bytes.Buffer)
	require.NoError(t, ack.Encode(w, 0))

	decoded := &DynAck{}
	require.NoError(t, decoded.Decode(bytes.NewBuffer(w.Bytes()), 0))
	require.NoError(t, VerifyDynAck(
		pub, decoded.Sig, chainHash, chanID, nextHeight, tlvData,
	))

	// Negative: a different next commitment number must not verify.
	require.ErrorIs(t, VerifyDynAck(
		pub, sig, chainHash, chanID, nextHeight+1, tlvData,
	), ErrDynAckSigInvalid)

	// Negative: a different identity key must not verify.
	otherPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	require.ErrorIs(t, VerifyDynAck(
		otherPriv.PubKey(), sig, chainHash, chanID, nextHeight,
		tlvData,
	), ErrDynAckSigInvalid)

	// Negative: tampered proposal TLVs must not verify.
	tampered := append([]byte{}, tlvData...)
	tampered[len(tampered)-1] ^= 0xff
	require.ErrorIs(t, VerifyDynAck(
		pub, sig, chainHash, chanID, nextHeight, tampered,
	), ErrDynAckSigInvalid)

	// Negative: a different chain hash must not verify.
	var otherChain chainhash.Hash
	copy(otherChain[:], bytes.Repeat([]byte{0x11}, 32))
	require.ErrorIs(t, VerifyDynAck(
		pub, sig, otherChain, chanID, nextHeight, tlvData,
	), ErrDynAckSigInvalid)
}
