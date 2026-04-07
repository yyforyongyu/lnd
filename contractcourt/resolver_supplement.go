package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
)

const resolverSupplementVersion byte = 1

// ResolverSupplement stores channel-scoped resolver state that needs to
// survive restarts but doesn't belong in each resolver's individual encoding.
type ResolverSupplement struct {
	// ChanType is the channel type for the closing channel.
	ChanType channeldb.ChannelType

	// ChannelInitiator records whether we initiated the channel.
	ChannelInitiator bool

	// LeaseExpiry is the lease thaw height for leased channels. For all other
	// channels this is zero.
	LeaseExpiry uint32

	// LocalDelayPubKey is the local delay basepoint used to identify the local
	// taproot commitment output on restart.
	LocalDelayPubKey [33]byte

	// LocalPaymentPubKey is the local payment basepoint used to identify the
	// remote taproot commitment output on restart.
	LocalPaymentPubKey [33]byte
}

// deriveResolverSupplement normalizes the restart-only resolver state from the
// historical channel record. This should be called once per channel close.
func deriveResolverSupplement(state *channeldb.OpenChannel) *ResolverSupplement {
	if state == nil {
		return nil
	}

	supplement := &ResolverSupplement{
		ChanType:         state.ChanType,
		ChannelInitiator: state.IsInitiator,
	}

	if state.ChanType.HasLeaseExpiration() {
		supplement.LeaseExpiry = state.ThawHeight
	}

	if pubKey := state.LocalChanCfg.DelayBasePoint.PubKey; pubKey != nil {
		copy(supplement.LocalDelayPubKey[:], pubKey.SerializeCompressed())
	}

	if pubKey := state.LocalChanCfg.PaymentBasePoint.PubKey; pubKey != nil {
		copy(supplement.LocalPaymentPubKey[:], pubKey.SerializeCompressed())
	}

	return supplement
}

// encodeResolverSupplement writes the resolver supplement using a versioned
// format to allow future extensions.
func encodeResolverSupplement(w io.Writer, supplement *ResolverSupplement) error {
	if supplement == nil {
		return fmt.Errorf("resolver supplement is nil")
	}

	if err := binary.Write(w, endian, resolverSupplementVersion); err != nil {
		return err
	}

	if err := binary.Write(w, endian, uint64(supplement.ChanType)); err != nil {
		return err
	}

	if err := binary.Write(w, endian, supplement.ChannelInitiator); err != nil {
		return err
	}

	if err := binary.Write(w, endian, supplement.LeaseExpiry); err != nil {
		return err
	}

	if _, err := w.Write(supplement.LocalDelayPubKey[:]); err != nil {
		return err
	}

	if _, err := w.Write(supplement.LocalPaymentPubKey[:]); err != nil {
		return err
	}

	return nil
}

// decodeResolverSupplement reads a resolver supplement written by
// encodeResolverSupplement.
func decodeResolverSupplement(r io.Reader) (*ResolverSupplement, error) {
	var version byte
	if err := binary.Read(r, endian, &version); err != nil {
		return nil, err
	}

	if version != resolverSupplementVersion {
		return nil, fmt.Errorf("unknown resolver supplement version %d", version)
	}

	var chanType uint64
	if err := binary.Read(r, endian, &chanType); err != nil {
		return nil, err
	}

	supplement := &ResolverSupplement{
		ChanType: channeldb.ChannelType(chanType),
	}

	if err := binary.Read(r, endian, &supplement.ChannelInitiator); err != nil {
		return nil, err
	}

	if err := binary.Read(r, endian, &supplement.LeaseExpiry); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, supplement.LocalDelayPubKey[:]); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, supplement.LocalPaymentPubKey[:]); err != nil {
		return nil, err
	}

	return supplement, nil
}
