package lnwire

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// DynAckSigTag is the domain-separation tag that is prefixed to the payload
// signed by a dyn_ack signature. It binds the signature to the dynamic
// commitments protocol and this version of the signed message.
const DynAckSigTag = "lightning-dynamic-commitments-dyn-ack-v1"

var (
	// ErrDynAckSigInvalid is returned when a dyn_ack signature does not
	// verify against the expected payload and public key.
	ErrDynAckSigInvalid = errors.New("dyn_ack signature invalid")

	// ErrDynAckSigNotLowS is returned when a dyn_ack signature does not use
	// a low-S value as required by the dynamic commitments extension BOLT.
	ErrDynAckSigNotLowS = errors.New("dyn_ack signature is not low-S")
)

// DynAckSigDigest computes the digest that a dyn_ack signature covers. It is
// the double-SHA256 of the domain-tagged payload:
//
//	DynAckSigTag ||
//	chain_hash ||
//	channel_id ||
//	u64(next_commitment_number) ||
//	dyn_propose_tlvs
//
// The nextCommitHeight is the next commitment number the responder expects to
// receive from the proposer. The proposalTLVs is the exact encoded
// dyn_propose_tlvs stream, i.e. the output of DynPropose.SerializeTlvData.
func DynAckSigDigest(chainHash chainhash.Hash, chanID ChannelID,
	nextCommitHeight uint64, proposalTLVs []byte) []byte {

	var buf bytes.Buffer

	// The tag is fixed length so no length prefix is required for domain
	// separation.
	buf.WriteString(DynAckSigTag)
	buf.Write(chainHash[:])
	buf.Write(chanID[:])

	var height [8]byte
	binary.BigEndian.PutUint64(height[:], nextCommitHeight)
	buf.Write(height[:])

	buf.Write(proposalTLVs)

	return chainhash.DoubleHashB(buf.Bytes())
}

// SignDynAck produces the responder's dyn_ack signature over the accepted
// proposal. The returned signature is a 64-byte low-S ECDSA signature (btcec's
// ecdsa.Sign is deterministic and always produces a low-S value) that verifies
// under the responder's node identity public key.
func SignDynAck(priv *btcec.PrivateKey, chainHash chainhash.Hash,
	chanID ChannelID, nextCommitHeight uint64,
	proposalTLVs []byte) (Sig, error) {

	digest := DynAckSigDigest(
		chainHash, chanID, nextCommitHeight, proposalTLVs,
	)

	return NewSigFromSignature(ecdsa.Sign(priv, digest))
}

// VerifyDynAck verifies a dyn_ack signature against the accepted proposal under
// the responder's node identity public key. It returns nil if the signature is
// valid, ErrDynAckSigNotLowS if the signature is not low-S, or
// ErrDynAckSigInvalid if the signature does not verify. Any decoding error is
// returned directly.
func VerifyDynAck(pub *btcec.PublicKey, sig Sig, chainHash chainhash.Hash,
	chanID ChannelID, nextCommitHeight uint64, proposalTLVs []byte) error {

	// The signature must use a low-S value. The S component occupies the
	// second half of the fixed-size wire encoding.
	var s btcec.ModNScalar
	s.SetByteSlice(sig.bytes[32:64])
	if s.IsOverHalfOrder() {
		return ErrDynAckSigNotLowS
	}

	sigObj, err := sig.ToSignature()
	if err != nil {
		return err
	}

	digest := DynAckSigDigest(
		chainHash, chanID, nextCommitHeight, proposalTLVs,
	)
	if !sigObj.Verify(digest, pub) {
		return ErrDynAckSigInvalid
	}

	return nil
}
