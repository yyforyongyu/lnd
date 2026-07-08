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

	// Create extra data records.
	producers, err := dc.ExtraData.RecordProducers()
	if err != nil {
		return err
	}

	// Append the accepted proposal records.
	producers = append(producers, dynProposeRecords(&dc.DynPropose)...)

	// Encode all known records.
	var tlvData ExtraOpaqueData
	err = tlvData.PackRecords(producers...)
	if err != nil {
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

	// Parse out TLV records.
	var tlvRecords ExtraOpaqueData
	if err := ReadElement(r, &tlvRecords); err != nil {
		return err
	}

	// Prepare receiving buffers to be filled by TLV extraction.
	var dustLimit tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
	var maxValue tlv.RecordT[tlv.TlvType1, MilliSatoshi]
	var htlcMin tlv.RecordT[tlv.TlvType2, MilliSatoshi]
	var reserve tlv.RecordT[tlv.TlvType3, tlv.BigSizeT[btcutil.Amount]]
	csvDelay := dc.CsvDelay.Zero()
	maxHtlcs := dc.MaxAcceptedHTLCs.Zero()
	chanFlags := dc.ChannelFlags.Zero()

	// Parse all known records and extra data.
	knownRecords, extraData, err := ParseAndExtractExtraData(
		tlvRecords, &dustLimit, &maxValue, &htlcMin, &reserve,
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
