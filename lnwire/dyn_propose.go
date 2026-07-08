package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// DynPropose is a message that is sent during a dynamic commitments negotiation
// process. It is sent by the proposer to propose a new set of channel
// parameters. The TLV type numbers follow the dyn_propose_tlvs stream defined
// in the dynamic commitments extension BOLT.
type DynPropose struct {
	// ChanID identifies the channel whose parameters we are trying to
	// re-negotiate.
	ChanID ChannelID

	// DustLimit, if not nil, proposes a change to the dust_limit_satoshis
	// for the sender's commitment transaction.
	DustLimit tlv.OptionalRecordT[
		tlv.TlvType0, tlv.BigSizeT[btcutil.Amount],
	]

	// MaxValueInFlight, if not nil, proposes a change to the
	// max_htlc_value_in_flight_msat limit of the sender.
	MaxValueInFlight tlv.OptionalRecordT[tlv.TlvType1, MilliSatoshi]

	// HtlcMinimum, if not nil, proposes a change to the htlc_minimum_msat
	// floor of the sender.
	HtlcMinimum tlv.OptionalRecordT[tlv.TlvType2, MilliSatoshi]

	// ChannelReserve, if not nil, proposes a change to the
	// channel_reserve_satoshis requirement of the recipient.
	ChannelReserve tlv.OptionalRecordT[
		tlv.TlvType3, tlv.BigSizeT[btcutil.Amount],
	]

	// CsvDelay, if not nil, proposes a change to the to_self_delay
	// requirement of the recipient.
	CsvDelay tlv.OptionalRecordT[tlv.TlvType4, uint16]

	// MaxAcceptedHTLCs, if not nil, proposes a change to the
	// max_accepted_htlcs limit of the sender.
	MaxAcceptedHTLCs tlv.OptionalRecordT[tlv.TlvType5, uint16]

	// ChannelFlags, if not nil, proposes a change to the channel_flags
	// announcement intent for the channel. It uses the same bits as the
	// channel_flags field of open_channel.
	ChannelFlags tlv.OptionalRecordT[tlv.TlvType6, FundingFlag]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	//
	// NOTE: Since the fields in this structure are part of the TLV stream,
	// ExtraData will contain all TLV records _except_ the ones that are
	// present in earlier parts of this structure.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynPropose implements the lnwire.Message
// interface.
var _ Message = (*DynPropose)(nil)

// A compile time check to ensure DynPropose implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*DynPropose)(nil)

// A compile time check to ensure DynPropose implements the lnwire.LinkUpdater
// interface.
var _ LinkUpdater = (*DynPropose)(nil)

// Encode serializes the target DynPropose into the passed io.Writer.
// Serialization will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dp *DynPropose) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, dp.ChanID); err != nil {
		return err
	}

	// Create extra data records.
	producers, err := dp.ExtraData.RecordProducers()
	if err != nil {
		return err
	}

	// Append the known records.
	producers = append(producers, dynProposeRecords(dp)...)

	// Encode all records.
	var tlvData ExtraOpaqueData
	err = tlvData.PackRecords(producers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, tlvData)
}

// Decode deserializes the serialized DynPropose stored in the passed io.Reader
// into the target DynPropose using the deserialization rules defined by the
// passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dp *DynPropose) Decode(r io.Reader, _ uint32) error {
	// Parse out the only required field.
	if err := ReadElements(r, &dp.ChanID); err != nil {
		return err
	}

	// Parse out TLV stream.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	// Prepare receiving buffers to be filled by TLV extraction.
	var dustLimit tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
	var maxValue tlv.RecordT[tlv.TlvType1, MilliSatoshi]
	var htlcMin tlv.RecordT[tlv.TlvType2, MilliSatoshi]
	var reserve tlv.RecordT[tlv.TlvType3, tlv.BigSizeT[btcutil.Amount]]
	csvDelay := dp.CsvDelay.Zero()
	maxHtlcs := dp.MaxAcceptedHTLCs.Zero()
	chanFlags := dp.ChannelFlags.Zero()

	knownRecords, extraData, err := ParseAndExtractExtraData(
		tlvRecords, &dustLimit, &maxValue, &htlcMin, &reserve,
		&csvDelay, &maxHtlcs, &chanFlags,
	)
	if err != nil {
		return err
	}

	// Check the results of the TLV Stream decoding and appropriately set
	// message fields.
	if _, ok := knownRecords[dp.DustLimit.TlvType()]; ok {
		dp.DustLimit = tlv.SomeRecordT(dustLimit)
	}

	if _, ok := knownRecords[dp.MaxValueInFlight.TlvType()]; ok {
		dp.MaxValueInFlight = tlv.SomeRecordT(maxValue)
	}

	if _, ok := knownRecords[dp.HtlcMinimum.TlvType()]; ok {
		dp.HtlcMinimum = tlv.SomeRecordT(htlcMin)
	}

	if _, ok := knownRecords[dp.ChannelReserve.TlvType()]; ok {
		dp.ChannelReserve = tlv.SomeRecordT(reserve)
	}

	if _, ok := knownRecords[dp.CsvDelay.TlvType()]; ok {
		dp.CsvDelay = tlv.SomeRecordT(csvDelay)
	}

	if _, ok := knownRecords[dp.MaxAcceptedHTLCs.TlvType()]; ok {
		dp.MaxAcceptedHTLCs = tlv.SomeRecordT(maxHtlcs)
	}

	if _, ok := knownRecords[dp.ChannelFlags.TlvType()]; ok {
		dp.ChannelFlags = tlv.SomeRecordT(chanFlags)
	}

	dp.ExtraData = extraData

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynPropose on the wire.
//
// This is part of the lnwire.Message interface.
func (dp *DynPropose) MsgType() MessageType {
	return MsgDynPropose
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (dp *DynPropose) SerializedSize() (uint32, error) {
	return MessageSerializedSize(dp)
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (dp *DynPropose) TargetChanID() ChannelID {
	return dp.ChanID
}

// SerializeTlvData takes just the TLV data of DynPropose (which covers all of
// the parameters on deck for changing) and serializes just this component. The
// main purpose of this is to make it easier to validate the DynAck signature.
func (dp *DynPropose) SerializeTlvData() ([]byte, error) {
	producers := dynProposeRecords(dp)

	var extra ExtraOpaqueData
	err := extra.PackRecords(producers...)
	if err != nil {
		return nil, err
	}

	return extra, nil
}

func dynProposeRecords(dp *DynPropose) []tlv.RecordProducer {
	recordProducers := make([]tlv.RecordProducer, 0, 7)

	dp.DustLimit.WhenSome(
		func(dl tlv.RecordT[tlv.TlvType0,
			tlv.BigSizeT[btcutil.Amount]]) {

			recordProducers = append(recordProducers, &dl)
		},
	)
	dp.MaxValueInFlight.WhenSome(
		func(mvif tlv.RecordT[tlv.TlvType1, MilliSatoshi]) {
			recordProducers = append(recordProducers, &mvif)
		},
	)
	dp.HtlcMinimum.WhenSome(
		func(hm tlv.RecordT[tlv.TlvType2, MilliSatoshi]) {
			recordProducers = append(recordProducers, &hm)
		},
	)
	dp.ChannelReserve.WhenSome(
		func(reserve tlv.RecordT[tlv.TlvType3,
			tlv.BigSizeT[btcutil.Amount]]) {

			recordProducers = append(recordProducers, &reserve)
		},
	)
	dp.CsvDelay.WhenSome(
		func(wait tlv.RecordT[tlv.TlvType4, uint16]) {
			recordProducers = append(recordProducers, &wait)
		},
	)
	dp.MaxAcceptedHTLCs.WhenSome(
		func(mah tlv.RecordT[tlv.TlvType5, uint16]) {
			recordProducers = append(recordProducers, &mah)
		},
	)
	dp.ChannelFlags.WhenSome(
		func(cf tlv.RecordT[tlv.TlvType6, FundingFlag]) {
			recordProducers = append(recordProducers, &cf)
		},
	)

	return recordProducers
}
