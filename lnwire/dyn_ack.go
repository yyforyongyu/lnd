package lnwire

import (
	"bytes"
	"io"
)

// DynAck is the message used to accept the parameters of a dynamic commitment
// negotiation. It carries the responder's acceptance signature over the
// proposed parameters (see dyn_ack_sig.go).
type DynAck struct {
	// ChanID is the ChannelID of the channel that is currently undergoing
	// a dynamic commitment negotiation.
	ChanID ChannelID

	// Sig is a signature that acknowledges and approves the parameters
	// that were requested in the DynPropose. See DynAckSigDigest for the
	// exact signed payload.
	Sig Sig

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynAck implements the lnwire.Message
// interface.
var _ Message = (*DynAck)(nil)

// A compile time check to ensure DynAck implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*DynAck)(nil)

// A compile time check to ensure DynAck implements the lnwire.LinkUpdater
// interface.
var _ LinkUpdater = (*DynAck)(nil)

// Encode serializes the target DynAck into the passed io.Writer. Serialization
// will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, da.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, da.Sig); err != nil {
		return err
	}

	return WriteBytes(w, da.ExtraData)
}

// Decode deserializes the serialized DynAck stored in the passed io.Reader into
// the target DynAck using the deserialization rules defined by the passed
// protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Decode(r io.Reader, _ uint32) error {
	// Parse out main message.
	if err := ReadElements(
		r, &da.ChanID, &da.Sig, &da.ExtraData,
	); err != nil {
		return err
	}

	// This is required to pass the fuzz test round trip equality check.
	if len(da.ExtraData) == 0 {
		da.ExtraData = nil
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynAck on the wire.
//
// This is part of the lnwire.Message interface.
func (da *DynAck) MsgType() MessageType {
	return MsgDynAck
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (da *DynAck) SerializedSize() (uint32, error) {
	return MessageSerializedSize(da)
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (da *DynAck) TargetChanID() ChannelID {
	return da.ChanID
}
