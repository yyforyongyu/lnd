package contractcourt

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func TestDeriveResolverSupplement(t *testing.T) {
	t.Parallel()

	delayPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	paymentPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	chanType := channeldb.AnchorOutputsBit |
		channeldb.LeaseExpirationBit |
		channeldb.SimpleTaprootFeatureBit

	state := &channeldb.OpenChannel{
		ChanType:     chanType,
		IsInitiator:  true,
		ThawHeight:   144,
		LocalChanCfg: channeldb.ChannelConfig{},
	}
	state.LocalChanCfg.DelayBasePoint = keychain.KeyDescriptor{
		PubKey: delayPriv.PubKey(),
	}
	state.LocalChanCfg.PaymentBasePoint = keychain.KeyDescriptor{
		PubKey: paymentPriv.PubKey(),
	}

	supplement := deriveResolverSupplement(state)
	require.NotNil(t, supplement)
	require.Equal(t, chanType, supplement.ChanType)
	require.True(t, supplement.ChannelInitiator)
	require.EqualValues(t, 144, supplement.LeaseExpiry)
	require.Equal(
		t, delayPriv.PubKey().SerializeCompressed(),
		supplement.LocalDelayPubKey[:],
	)
	require.Equal(
		t, paymentPriv.PubKey().SerializeCompressed(),
		supplement.LocalPaymentPubKey[:],
	)

	nonLease := deriveResolverSupplement(&channeldb.OpenChannel{
		ChanType: channeldb.AnchorOutputsBit,
	})
	require.NotNil(t, nonLease)
	require.Zero(t, nonLease.LeaseExpiry)
}

func TestResolverSupplementEncodeDecode(t *testing.T) {
	t.Parallel()

	supplement := &ResolverSupplement{
		ChanType:           channeldb.AnchorOutputsBit | channeldb.SimpleTaprootFeatureBit,
		ChannelInitiator:   true,
		LeaseExpiry:        144,
		LocalDelayPubKey:   [33]byte{1, 2, 3},
		LocalPaymentPubKey: [33]byte{4, 5, 6},
	}

	var buf bytes.Buffer
	require.NoError(t, encodeResolverSupplement(&buf, supplement))

	decoded, err := decodeResolverSupplement(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, supplement, decoded)
}

func TestResolverSupplementDecodeUnknownVersion(t *testing.T) {
	t.Parallel()

	_, err := decodeResolverSupplement(bytes.NewReader([]byte{99}))
	require.ErrorContains(t, err, "unknown resolver supplement version")
}
