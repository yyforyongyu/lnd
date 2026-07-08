package lnwire

import (
	"bytes"
	"io"
)

// DynRejectBit is a bit position in the dyn_reject parameter_rejections
// bitfield. It is a fixed enumeration that uses the same bit ordering as
// feature vectors; it is NOT a feature bit and is NOT indexed by TLV type.
//
// Bit 0 is permanently reserved for unknown_or_unsupported_tlv. Bits 1-7 map to
// the parameters defined by the dynamic commitments extension BOLT. Future
// parameter rejection bits MUST be appended starting at bit 8, regardless of
// the proposed parameter's TLV type.
type DynRejectBit uint16

const (
	// DynRejectUnknownOrUnsupportedTLV is set to signal that the proposal
	// contained an unknown or unsupported (odd) TLV type. This bit is
	// permanently reserved.
	DynRejectUnknownOrUnsupportedTLV DynRejectBit = 0

	// DynRejectDustLimit rejects the proposed dust_limit_satoshis.
	DynRejectDustLimit DynRejectBit = 1

	// DynRejectMaxValueInFlight rejects the proposed
	// max_htlc_value_in_flight_msat.
	DynRejectMaxValueInFlight DynRejectBit = 2

	// DynRejectHtlcMinimum rejects the proposed htlc_minimum_msat.
	DynRejectHtlcMinimum DynRejectBit = 3

	// DynRejectChannelReserve rejects the proposed
	// channel_reserve_satoshis.
	DynRejectChannelReserve DynRejectBit = 4

	// DynRejectCsvDelay rejects the proposed to_self_delay.
	DynRejectCsvDelay DynRejectBit = 5

	// DynRejectMaxAcceptedHTLCs rejects the proposed max_accepted_htlcs.
	DynRejectMaxAcceptedHTLCs DynRejectBit = 6

	// DynRejectChannelFlags rejects the proposed channel_flags.
	DynRejectChannelFlags DynRejectBit = 7
)

// DynReject is the message sent by the responder to reject a dyn_propose. It
// carries a fixed-enumeration bitfield calling out which of the proposed
// parameters are unacceptable. A zero-valued bitfield rejects the negotiation
// without identifying specific parameters.
type DynReject struct {
	// ChanID identifies the channel whose parameter re-negotiation is being
	// rejected.
	ChanID ChannelID

	// UpdateRejections is the parameter_rejections bitfield. It uses the
	// same bit ordering as feature vectors but is a fixed enumeration (see
	// DynRejectBit), not a feature vector. Use SetRejection/IsRejected to
	// manipulate the individual bits.
	UpdateRejections RawFeatureVector

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	//
	// NOTE: Since the fields in this structure are part of the TLV stream,
	// ExtraData will contain all TLV records _except_ the ones that are
	// present in earlier parts of this structure.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynReject implements the lnwire.Message
// interface.
var _ Message = (*DynReject)(nil)

// A compile time check to ensure DynReject implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*DynReject)(nil)

// A compile time check to ensure DynReject implements the lnwire.LinkUpdater
// interface.
var _ LinkUpdater = (*DynReject)(nil)

// NewDynReject constructs a DynReject for the given channel with the provided
// rejection bits set.
func NewDynReject(chanID ChannelID, bits ...DynRejectBit) *DynReject {
	dr := &DynReject{
		ChanID:           chanID,
		UpdateRejections: *NewRawFeatureVector(),
	}
	for _, bit := range bits {
		dr.SetRejection(bit)
	}

	return dr
}

// SetRejection sets the given rejection bit in the parameter_rejections
// bitfield.
func (dr *DynReject) SetRejection(bit DynRejectBit) {
	if dr.UpdateRejections.features == nil {
		dr.UpdateRejections = *NewRawFeatureVector()
	}

	dr.UpdateRejections.Set(FeatureBit(bit))
}

// IsRejected returns true if the given rejection bit is set in the
// parameter_rejections bitfield.
func (dr *DynReject) IsRejected(bit DynRejectBit) bool {
	return dr.UpdateRejections.IsSet(FeatureBit(bit))
}

// Encode serializes the target DynReject into the passed io.Writer.
// Serialization will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dr *DynReject) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, dr.ChanID); err != nil {
		return err
	}

	if err := WriteRawFeatureVector(w, &dr.UpdateRejections); err != nil {
		return err
	}

	return WriteBytes(w, dr.ExtraData)
}

// Decode deserializes the serialized DynReject stored in the passed io.Reader
// into the target DynReject using the deserialization rules defined by the
// passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dr *DynReject) Decode(r io.Reader, _ uint32) error {
	var extra ExtraOpaqueData

	if err := ReadElements(
		r, &dr.ChanID, &dr.UpdateRejections, &extra,
	); err != nil {
		return err
	}

	if len(extra) != 0 {
		dr.ExtraData = extra
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynReject on the wire.
//
// This is part of the lnwire.Message interface.
func (dr *DynReject) MsgType() MessageType {
	return MsgDynReject
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (dr *DynReject) SerializedSize() (uint32, error) {
	return MessageSerializedSize(dr)
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (dr *DynReject) TargetChanID() ChannelID {
	return dr.ChanID
}
