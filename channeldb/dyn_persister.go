package channeldb

import (
	"bytes"
	"fmt"
	"io"
	"math"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// dynAcceptedProposalBucket is the top-level bucket that stores the accepted
// dynamic-commitments proposal context for channels with an in-flight
// negotiation. It is keyed by the 32-byte lnwire.ChannelID of the channel and
// holds a single serialized DynAcceptedProposal per channel. The context only
// lives here between the persistence boundaries the extension BOLT calls out
// (from dyn_propose/dyn_ack up to a locked-in dyn_commit_sig), so at most one
// entry per channel exists and it is deleted once the negotiation is forgotten
// or fully committed.
var dynAcceptedProposalBucket = []byte("dyn-accepted-proposal")

// DynAcceptedProposal is the durable form of an accepted dynamic-commitments
// proposal. It mirrors the htlcswitch/dyn.AcceptedProposal context so that a
// reconnect after a crash can decide whether to resume or forget a negotiation.
// It is a channeldb-native type (channeldb must not depend on htlcswitch/dyn);
// the htlcswitch layer converts between the two.
type DynAcceptedProposal struct {
	// Proposer is the party that sent the dyn_propose. If it equals
	// lntypes.Local we are the proposer, otherwise we are the responder.
	Proposer lntypes.ChannelParty

	// Proposal is the accepted proposal message.
	Proposal *lnwire.DynPropose

	// NextCommitHeight is the commitment number the update binds to; it is
	// bound into the dyn_ack signature digest.
	NextCommitHeight uint64

	// AckSig is the responder's dyn_ack signature. It is set on the
	// responder once it signs its dyn_ack, and on the proposer once it
	// verifies the received dyn_ack. When absent, no dyn_ack has been
	// exchanged for this negotiation yet.
	AckSig fn.Option[lnwire.Sig]

	// CommitSig is the proposer's dyn_commit_sig commitment signature for a
	// non-taproot (ECDSA) channel. Its presence, or the presence of
	// PartialSig, is the persisted flag that a dyn_commit_sig has been sent
	// (by the proposer) or received (by the responder) before disconnect,
	// which is what promotes a negotiation from "forget on reconnect" to
	// "retain and retransmit". When absent, no ECDSA dyn_commit_sig has been
	// persisted.
	CommitSig fn.Option[lnwire.Sig]

	// PartialSig is the proposer's dyn_commit_sig commitment signature for a
	// taproot (musig2) channel, carried as a partial_signature_with_nonce.
	// It is the taproot counterpart of CommitSig: for a taproot channel the
	// dyn_commit_sig rides this field and CommitSig is absent, while for a
	// non-taproot channel this field is absent and CommitSig carries the
	// ECDSA signature. Its presence is likewise a persisted flag that a
	// dyn_commit_sig has crossed the wire.
	PartialSig fn.Option[lnwire.PartialSigWithNonce]
}

// serialize writes the DynAcceptedProposal to the given writer following the
// channeldb fixed-layout conventions: the proposer role and lock-in height, the
// optional dyn_ack signature, the optional ECDSA and taproot-partial
// dyn_commit_sig signatures each with a presence flag, and finally the accepted
// proposal encoded as a length-prefixed lnwire message.
func (p *DynAcceptedProposal) serialize(w io.Writer) error {
	if p.Proposal == nil {
		return fmt.Errorf("dyn accepted proposal has a nil proposal")
	}

	err := WriteElements(w, uint8(p.Proposer), p.NextCommitHeight)
	if err != nil {
		return err
	}

	if err := writeOptionalSig(w, p.AckSig); err != nil {
		return err
	}

	if err := writeOptionalSig(w, p.CommitSig); err != nil {
		return err
	}

	if err := writeOptionalPartialSig(w, p.PartialSig); err != nil {
		return err
	}

	// The accepted proposal message is variable length, so it is stored
	// with a uint16 byte-length prefix. An lnwire message never exceeds the
	// uint16 range, but we guard the conversion defensively.
	var buf bytes.Buffer
	if err := p.Proposal.Encode(&buf, 0); err != nil {
		return fmt.Errorf("encode dyn proposal: %w", err)
	}
	if buf.Len() > math.MaxUint16 {
		return fmt.Errorf("dyn proposal too large to persist: %d bytes",
			buf.Len())
	}

	if err := WriteElement(w, uint16(buf.Len())); err != nil {
		return err
	}

	_, err = w.Write(buf.Bytes())

	return err
}

// deserializeDynAcceptedProposal reads a DynAcceptedProposal from the given
// reader, inverting serialize.
func deserializeDynAcceptedProposal(r io.Reader) (*DynAcceptedProposal, error) {
	var (
		proposer   uint8
		nextHeight uint64
	)
	if err := ReadElements(r, &proposer, &nextHeight); err != nil {
		return nil, err
	}

	ackSig, err := readOptionalSig(r)
	if err != nil {
		return nil, err
	}

	commitSig, err := readOptionalSig(r)
	if err != nil {
		return nil, err
	}

	partialSig, err := readOptionalPartialSig(r)
	if err != nil {
		return nil, err
	}

	var propLen uint16
	if err := ReadElement(r, &propLen); err != nil {
		return nil, err
	}

	propBytes := make([]byte, propLen)
	if _, err := io.ReadFull(r, propBytes); err != nil {
		return nil, err
	}

	var proposal lnwire.DynPropose
	if err := proposal.Decode(bytes.NewReader(propBytes), 0); err != nil {
		return nil, fmt.Errorf("decode dyn proposal: %w", err)
	}

	return &DynAcceptedProposal{
		Proposer:         lntypes.ChannelParty(proposer),
		Proposal:         &proposal,
		NextCommitHeight: nextHeight,
		AckSig:           ackSig,
		CommitSig:        commitSig,
		PartialSig:       partialSig,
	}, nil
}

// writeOptionalSig writes an optional 64-byte signature: a presence flag
// followed, when present, by the raw signature bytes. dyn signatures are always
// 64-byte ECDSA.
func writeOptionalSig(w io.Writer, sig fn.Option[lnwire.Sig]) error {
	if sig.IsNone() {
		return WriteElement(w, false)
	}

	if err := WriteElement(w, true); err != nil {
		return err
	}

	s := sig.UnsafeFromSome()
	_, err := w.Write(s.RawBytes())

	return err
}

// readOptionalSig reads an optional 64-byte signature written by
// writeOptionalSig.
func readOptionalSig(r io.Reader) (fn.Option[lnwire.Sig], error) {
	var present bool
	if err := ReadElement(r, &present); err != nil {
		return fn.None[lnwire.Sig](), err
	}

	if !present {
		return fn.None[lnwire.Sig](), nil
	}

	var raw [64]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return fn.None[lnwire.Sig](), err
	}

	sig, err := lnwire.NewSigFromWireECDSA(raw[:])
	if err != nil {
		return fn.None[lnwire.Sig](), err
	}

	return fn.Some(sig), nil
}

// writeOptionalPartialSig writes an optional musig2 partial signature with
// nonce: a presence flag followed, when present, by the 98-byte serialized
// partial_signature_with_nonce (32-byte scalar || 66-byte nonce).
func writeOptionalPartialSig(w io.Writer,
	sig fn.Option[lnwire.PartialSigWithNonce]) error {

	if sig.IsNone() {
		return WriteElement(w, false)
	}

	if err := WriteElement(w, true); err != nil {
		return err
	}

	s := sig.UnsafeFromSome()

	return s.Encode(w)
}

// readOptionalPartialSig reads an optional musig2 partial signature with nonce
// written by writeOptionalPartialSig.
func readOptionalPartialSig(r io.Reader) (
	fn.Option[lnwire.PartialSigWithNonce], error) {

	var present bool
	if err := ReadElement(r, &present); err != nil {
		return fn.None[lnwire.PartialSigWithNonce](), err
	}

	if !present {
		return fn.None[lnwire.PartialSigWithNonce](), nil
	}

	var sig lnwire.PartialSigWithNonce
	if err := sig.Decode(r); err != nil {
		return fn.None[lnwire.PartialSigWithNonce](), err
	}

	return fn.Some(sig), nil
}

// StoreDynAcceptedProposal persists (or overwrites) the accepted
// dynamic-commitments proposal context for the given channel.
func (c *ChannelStateDB) StoreDynAcceptedProposal(chanID lnwire.ChannelID,
	p DynAcceptedProposal) error {

	var b bytes.Buffer
	if err := p.serialize(&b); err != nil {
		return err
	}

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(dynAcceptedProposalBucket)
		if err != nil {
			return err
		}

		return bucket.Put(chanID[:], b.Bytes())
	}, func() {})
}

// FetchDynAcceptedProposal loads the persisted accepted dynamic-commitments
// proposal context for the given channel. It returns fn.None when no context is
// stored (either the bucket does not exist or the channel has no entry).
func (c *ChannelStateDB) FetchDynAcceptedProposal(chanID lnwire.ChannelID) (
	fn.Option[DynAcceptedProposal], error) {

	var proposal fn.Option[DynAcceptedProposal]
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(dynAcceptedProposalBucket)
		if bucket == nil {
			return nil
		}

		raw := bucket.Get(chanID[:])
		if raw == nil {
			return nil
		}

		p, err := deserializeDynAcceptedProposal(bytes.NewReader(raw))
		if err != nil {
			return err
		}

		proposal = fn.Some(*p)

		return nil
	}, func() {
		proposal = fn.None[DynAcceptedProposal]()
	})

	return proposal, err
}

// DeleteDynAcceptedProposal removes any persisted accepted dynamic-commitments
// proposal context for the given channel. It is a no-op when none exists.
func (c *ChannelStateDB) DeleteDynAcceptedProposal(
	chanID lnwire.ChannelID) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(dynAcceptedProposalBucket)
		if bucket == nil {
			return nil
		}

		return bucket.Delete(chanID[:])
	}, func() {})
}
