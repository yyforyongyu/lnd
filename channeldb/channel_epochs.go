package channeldb

import (
	"io"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
)

// Byte sizes of the fixed-width fields that make up a serialized
// CommitChainEpochHistory. They are used by the TLV size function, which MUST
// return the exact encoded length, and double as documentation of the wire
// layout.
const (
	// commitmentParamsSize is the encoded size of a CommitmentParams: an
	// 8-byte dust limit (btcutil.Amount serialized as a uint64) followed by
	// a 2-byte CSV delay (uint16).
	commitmentParamsSize = 8 + 2

	// commitChainEpochSize is the encoded size of a CommitChainEpoch: an
	// 8-byte last height (uint64) followed by the epoch's CommitmentParams.
	commitChainEpochSize = 8 + commitmentParamsSize

	// epochListLenSize is the size of the uint16 length prefix that
	// precedes each per-side epoch list.
	epochListLenSize = 2
)

// CommitChainEpoch refers to a single period of time during which a particular
// set of commitment parameters were in effect on one commitment chain.
type CommitChainEpoch struct {
	// LastHeight is the last commit height, inclusive, that this epoch's
	// parameters applied to on the commitment chain it belongs to.
	LastHeight uint64

	// Params are the script/output-rendering commitment parameters (CSV and
	// dust) that were in effect for this epoch. They are "normalized" in the
	// sense that they are the parameters that actually apply to the party's
	// commitment transaction, irrespective of which party specified them.
	Params CommitmentParams
}

// encode serializes a CommitChainEpoch to the given writer.
func (c *CommitChainEpoch) encode(w io.Writer) error {
	return WriteElements(
		w, c.LastHeight, c.Params.DustLimit, c.Params.CsvDelay,
	)
}

// decode deserializes a CommitChainEpoch from the given reader.
func (c *CommitChainEpoch) decode(r io.Reader) error {
	return ReadElements(
		r, &c.LastHeight, &c.Params.DustLimit, &c.Params.CsvDelay,
	)
}

// CommitChainEpochHistory maintains, per side, the history of the commitment
// parameters that govern how each commitment chain's transactions and scripts
// are rendered. The two commitment chains lock in new parameters at different
// heights, so the history is tracked independently for each side via
// lntypes.Dual.
//
// This is needed so that historical (revoked/old-height) commitment and HTLC
// scripts can be reconstructed with the parameters that were actually in effect
// at that height, rather than the current parameters, once dynamic commitments
// allow those parameters to change mid-channel.
type CommitChainEpochHistory struct {
	// Current holds the commitment parameters presently in effect for each
	// side. They are kept separate from Historical because the height that
	// will eventually close them out is not yet known.
	Current lntypes.Dual[CommitmentParams]

	// Historical holds, for each side, the list of closed epochs sorted in
	// ascending order by LastHeight.
	Historical lntypes.Dual[[]CommitChainEpoch]
}

// NewCommitChainEpochHistory constructs a CommitChainEpochHistory whose current
// parameters are the given per-side values and whose history is empty. This is
// the state of every channel prior to its first parameter change, and is also
// the read-time default synthesized for channels persisted before this field
// existed (using their current channel configs).
func NewCommitChainEpochHistory(
	current lntypes.Dual[CommitmentParams]) CommitChainEpochHistory {

	return CommitChainEpochHistory{
		Current: current,
	}
}

// ParamsAt returns the commitment parameters in effect for the given party's
// commitment chain at the given commit height. It walks that side's historical
// epochs for the first one that still covers the height, falling back to the
// current parameters when the height is beyond all recorded epochs.
func (c *CommitChainEpochHistory) ParamsAt(party lntypes.ChannelParty,
	height uint64) CommitmentParams {

	// Epochs are stored in ascending order by LastHeight, and are
	// contiguous. The first epoch whose LastHeight is greater than or equal
	// to the query height is therefore the one that encloses it, since the
	// previous epoch's LastHeight is strictly less than the query height.
	epochs := c.Historical.GetForParty(party)
	for i := range epochs {
		if height <= epochs[i].LastHeight {
			return epochs[i].Params
		}
	}

	// The height is newer than any closed epoch, so the current parameters
	// apply.
	return c.Current.GetForParty(party)
}

// RecordParamChange closes out the current epoch on both commitment chains and
// installs a new set of current parameters reflecting a change made by the
// whoSpecified party. The lockInHeights give the last height that the outgoing
// parameters applied to on each chain (the local and remote chains lock in at
// different heights).
//
// The change is normalized per BOLT semantics: a party's declared dust limit
// governs its own commitment outputs, whereas the to_self_delay (CSV) it
// declares is imposed on its counterparty's commitment. Concretely, the
// specifying party's own dust limit is updated to params.DustLimit while its
// CSV is carried over unchanged, and the counterparty's CSV is updated to
// params.CsvDelay while its dust limit is carried over unchanged. Callers that
// only intend to change one field must therefore pass the unchanged value for
// the other.
func (c *CommitChainEpochHistory) RecordParamChange(
	whoSpecified lntypes.ChannelParty, params CommitmentParams,
	lockInHeights lntypes.Dual[uint64]) {

	// Close the outgoing epoch on both chains at their respective lock-in
	// heights and append them to the per-side history.
	c.Historical.Local = append(c.Historical.Local, CommitChainEpoch{
		LastHeight: lockInHeights.Local,
		Params:     c.Current.Local,
	})
	c.Historical.Remote = append(c.Historical.Remote, CommitChainEpoch{
		LastHeight: lockInHeights.Remote,
		Params:     c.Current.Remote,
	})

	// The specifying party keeps its old CSV (a CSV change it declares
	// applies to its counterparty, not itself) but adopts the new dust
	// limit.
	counterParty := whoSpecified.CounterParty()
	oldMain := c.Current.GetForParty(whoSpecified)
	oldCounter := c.Current.GetForParty(counterParty)

	c.Current.SetForParty(whoSpecified, CommitmentParams{
		DustLimit: params.DustLimit,
		CsvDelay:  oldMain.CsvDelay,
	})

	// The counterparty keeps its old dust limit but adopts the CSV imposed
	// on it by the specifying party.
	c.Current.SetForParty(counterParty, CommitmentParams{
		DustLimit: oldCounter.DustLimit,
		CsvDelay:  params.CsvDelay,
	})
}

// Size returns the exact number of bytes that encode produces for this
// CommitChainEpochHistory. It is used as the TLV size function, which must
// match the encoded length exactly.
//
// NOTE: This is a part of the RecordProducer size contract.
func (c *CommitChainEpochHistory) Size() uint64 {
	numEpochs := uint64(
		len(c.Historical.Local) + len(c.Historical.Remote),
	)

	// Two sets of current params, two per-side length prefixes, and one
	// fixed-width entry per epoch.
	return 2*commitmentParamsSize + 2*epochListLenSize +
		numEpochs*commitChainEpochSize
}

// encode serializes a CommitChainEpochHistory to the given writer. The layout
// is: the local then remote current params, followed by a
// uint16-length-prefixed list of local epochs, then a uint16-length-prefixed
// list of remote epochs.
func (c *CommitChainEpochHistory) encode(w io.Writer) error {
	err := WriteElements(
		w, c.Current.Local.DustLimit, c.Current.Local.CsvDelay,
		c.Current.Remote.DustLimit, c.Current.Remote.CsvDelay,
	)
	if err != nil {
		return err
	}

	if err := encodeEpochList(w, c.Historical.Local); err != nil {
		return err
	}

	return encodeEpochList(w, c.Historical.Remote)
}

// encodeEpochList writes a uint16 length prefix followed by each epoch in the
// list.
func encodeEpochList(w io.Writer, epochs []CommitChainEpoch) error {
	if err := WriteElement(w, uint16(len(epochs))); err != nil {
		return err
	}

	for i := range epochs {
		if err := epochs[i].encode(w); err != nil {
			return err
		}
	}

	return nil
}

// decode deserializes a CommitChainEpochHistory from the given reader.
func (c *CommitChainEpochHistory) decode(r io.Reader) error {
	err := ReadElements(
		r, &c.Current.Local.DustLimit, &c.Current.Local.CsvDelay,
		&c.Current.Remote.DustLimit, &c.Current.Remote.CsvDelay,
	)
	if err != nil {
		return err
	}

	c.Historical.Local, err = decodeEpochList(r)
	if err != nil {
		return err
	}

	c.Historical.Remote, err = decodeEpochList(r)

	return err
}

// decodeEpochList reads a uint16 length prefix and then that many epochs.
func decodeEpochList(r io.Reader) ([]CommitChainEpoch, error) {
	var numEpochs uint16
	if err := ReadElement(r, &numEpochs); err != nil {
		return nil, err
	}

	if numEpochs == 0 {
		return nil, nil
	}

	epochs := make([]CommitChainEpoch, numEpochs)
	for i := range epochs {
		if err := epochs[i].decode(r); err != nil {
			return nil, err
		}
	}

	return epochs, nil
}

// Record returns a TLV record that can be used to encode/decode a
// CommitChainEpochHistory to/from a TLV stream.
//
// NOTE: This is a part of the RecordProducer interface.
func (c *CommitChainEpochHistory) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, c, c.Size, eCommitChainEpochHistory,
		dCommitChainEpochHistory,
	)
}

// eCommitChainEpochHistory is a tlv.Encoder for CommitChainEpochHistory.
func eCommitChainEpochHistory(w io.Writer, val interface{}, _ *[8]byte) error {
	if hist, ok := val.(*CommitChainEpochHistory); ok {
		return hist.encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "*CommitChainEpochHistory")
}

// dCommitChainEpochHistory is a tlv.Decoder for CommitChainEpochHistory.
func dCommitChainEpochHistory(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if hist, ok := val.(*CommitChainEpochHistory); ok {
		return hist.decode(r)
	}

	return tlv.NewTypeForDecodingErr(val, "*CommitChainEpochHistory", l, l)
}

// Assert that CommitChainEpochHistory implements the RecordProducer interface.
var _ tlv.RecordProducer = (*CommitChainEpochHistory)(nil)
