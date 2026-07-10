package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// DynCommit is the message used to irrefutably execute a dynamic commitment
// update. On the wire this is the dyn_commit_sig message. It bundles the
// accepted proposal TLVs, the responder's dyn_ack acceptance signature, and the
// proposer's commitment signature for the responder's next commitment. This
// avoids a round trip and the race where a commitment signature is processed
// before the dynamic update it commits.
type DynCommit struct {
	// DynPropose is an embedded version of the accepted DynPropose. It
	// carries the channel id and the exact set of accepted proposal TLVs.
	DynPropose

	// CommitSig is the proposer's commitment signature for the responder's
	// next commitment after applying the accepted proposal. It is
	// equivalent to the signature field of commitment_signed. Since dynamic
	// commitments require no HTLCs, there are no HTLC signatures.
	CommitSig Sig

	// AckSig is the responder's dyn_ack acceptance signature, echoed back
	// so the responder can re-verify the accepted parameters and commitment
	// number even if the pre-dyn_commit_sig negotiation state was forgotten
	// across a reconnect.
	AckSig Sig

	// PartialSig is the musig2 extended partial signature (with the
	// signer's public nonce) for the responder's next commitment. It is
	// the partial_signature_with_nonce record (type 2) of the
	// dyn_commit_sig_tlvs stream and is equivalent to the
	// partial_signature_with_nonce TLV of commitment_signed.
	//
	// NOTE: This field is only populated for taproot (musig2) channels. In
	// that case the CommitSig field above MUST be 64 zero bytes. For
	// non-taproot channels this field MUST be absent and CommitSig carries
	// the ECDSA commitment signature instead.
	PartialSig OptPartialSigWithNonceTLV

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynCommit implements the lnwire.Message
// interface.
var _ Message = (*DynCommit)(nil)

// A compile time check to ensure DynCommit implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*DynCommit)(nil)

// A compile time check to ensure DynCommit implements the lnwire.LinkUpdater
// interface. TargetChanID is promoted from the embedded DynPropose.
var _ LinkUpdater = (*DynCommit)(nil)

// Encode serializes the target DynCommit into the passed io.Writer.
// Serialization will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dc *DynCommit) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, dc.DynPropose.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, dc.CommitSig); err != nil {
		return err
	}

	if err := WriteSig(w, dc.AckSig); err != nil {
		return err
	}

	// The accepted proposal parameters are serialized as a standalone,
	// length-prefixed TLV stream (accepted_tlvs). This is the exact
	// dyn_propose_tlvs byte stream the dyn_ack signature commits to, so it
	// is kept separate from the dyn_commit_sig_tlvs stream below.
	acceptedProducers, err := dc.DynPropose.ExtraData.RecordProducers()
	if err != nil {
		return err
	}
	acceptedProducers = append(
		acceptedProducers, dynProposeRecords(&dc.DynPropose)...,
	)

	var acceptedTLVs ExtraOpaqueData
	if err := acceptedTLVs.PackRecords(acceptedProducers...); err != nil {
		return err
	}

	// Write the accepted_tlvs stream as a bigsize length prefix followed by
	// the serialized records.
	var buf [8]byte
	err = tlv.WriteVarInt(w, uint64(len(acceptedTLVs)), &buf)
	if err != nil {
		return err
	}
	if err := WriteBytes(w, acceptedTLVs); err != nil {
		return err
	}

	// Finally, encode the dyn_commit_sig_tlvs stream. For taproot channels
	// this carries the partial_signature_with_nonce record (type 2). Any
	// message-level extra data is appended here as well.
	producers, err := dc.ExtraData.RecordProducers()
	if err != nil {
		return err
	}
	dc.PartialSig.WhenSome(func(sig PartialSigWithNonceTLV) {
		producers = append(producers, &sig)
	})

	var tlvData ExtraOpaqueData
	if err := tlvData.PackRecords(producers...); err != nil {
		return err
	}

	return WriteBytes(w, tlvData)
}

// Decode deserializes the serialized DynCommit stored in the passed io.Reader
// into the target DynCommit using the deserialization rules defined by the
// passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dc *DynCommit) Decode(r io.Reader, _ uint32) error {
	// Parse out the required fields.
	err := ReadElements(
		r, &dc.DynPropose.ChanID, &dc.CommitSig, &dc.AckSig,
	)
	if err != nil {
		return err
	}

	// Read the length-prefixed accepted proposal TLV stream
	// (accepted_tlvs).
	var buf [8]byte
	acceptedLen, err := tlv.ReadVarInt(r, &buf)
	if err != nil {
		return err
	}

	acceptedBytes := make([]byte, acceptedLen)
	if _, err := io.ReadFull(r, acceptedBytes); err != nil {
		return err
	}
	acceptedTLVs := ExtraOpaqueData(acceptedBytes)

	// Prepare receiving buffers to be filled by TLV extraction.
	var dustLimit tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
	var maxValue tlv.RecordT[tlv.TlvType1, MilliSatoshi]
	var htlcMin tlv.RecordT[tlv.TlvType2, MilliSatoshi]
	var reserve tlv.RecordT[tlv.TlvType3, tlv.BigSizeT[btcutil.Amount]]
	csvDelay := dc.CsvDelay.Zero()
	maxHtlcs := dc.MaxAcceptedHTLCs.Zero()
	chanFlags := dc.ChannelFlags.Zero()

	// Parse the accepted proposal records and any extra proposal data.
	knownRecords, propExtra, err := ParseAndExtractExtraData(
		acceptedTLVs, &dustLimit, &maxValue, &htlcMin, &reserve,
		&csvDelay, &maxHtlcs, &chanFlags,
	)
	if err != nil {
		return err
	}

	// Check the results of the TLV Stream decoding and appropriately set
	// message fields.
	if _, ok := knownRecords[dc.DustLimit.TlvType()]; ok {
		dc.DustLimit = tlv.SomeRecordT(dustLimit)
	}
	if _, ok := knownRecords[dc.MaxValueInFlight.TlvType()]; ok {
		dc.MaxValueInFlight = tlv.SomeRecordT(maxValue)
	}
	if _, ok := knownRecords[dc.HtlcMinimum.TlvType()]; ok {
		dc.HtlcMinimum = tlv.SomeRecordT(htlcMin)
	}
	if _, ok := knownRecords[dc.ChannelReserve.TlvType()]; ok {
		dc.ChannelReserve = tlv.SomeRecordT(reserve)
	}
	if _, ok := knownRecords[dc.CsvDelay.TlvType()]; ok {
		dc.CsvDelay = tlv.SomeRecordT(csvDelay)
	}
	if _, ok := knownRecords[dc.MaxAcceptedHTLCs.TlvType()]; ok {
		dc.MaxAcceptedHTLCs = tlv.SomeRecordT(maxHtlcs)
	}
	if _, ok := knownRecords[dc.ChannelFlags.TlvType()]; ok {
		dc.ChannelFlags = tlv.SomeRecordT(chanFlags)
	}

	// Preserve any unknown records that were part of the accepted proposal
	// so the accepted_tlvs stream round-trips faithfully.
	if len(propExtra) > 0 {
		dc.DynPropose.ExtraData = propExtra
	}

	// The remaining bytes form the dyn_commit_sig_tlvs stream, which may
	// carry the partial_signature_with_nonce record (type 2) plus any
	// message-level extra data.
	var tlvRecords ExtraOpaqueData
	if err := ReadElement(r, &tlvRecords); err != nil {
		return err
	}

	partialSig := dc.PartialSig.Zero()
	sigRecords, extraData, err := ParseAndExtractExtraData(
		tlvRecords, &partialSig,
	)
	if err != nil {
		return err
	}
	if _, ok := sigRecords[partialSig.TlvType()]; ok {
		dc.PartialSig = tlv.SomeRecordT(partialSig)
	}

	dc.ExtraData = extraData

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynCommit on the wire.
//
// This is part of the lnwire.Message interface.
func (dc *DynCommit) MsgType() MessageType {
	return MsgDynCommit
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (dc *DynCommit) SerializedSize() (uint32, error) {
	return MessageSerializedSize(dc)
}
