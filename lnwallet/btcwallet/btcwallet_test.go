package btcwallet

import (
	"context"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/stretchr/testify/mock"
)

// walletControllerMock records calls at the maintained adapter boundary;
// unused embedded operations have no implementation and fail if invoked.
type walletControllerMock struct {
	walletAPI
	mock.Mock
}

// SignPsbt captures the packet and Lightning callback translation.
func (w *walletControllerMock) SignPsbt(ctx context.Context,
	params *wallet.SignPsbtParams) (*wallet.SignPsbtResult, error) {

	args := w.Called(ctx, params)
	result, _ := args.Get(0).(*wallet.SignPsbtResult)

	return result, args.Error(1)
}

// ComputeRawSig supplies the maintained signature encoding for conversion.
func (w *walletControllerMock) ComputeRawSig(ctx context.Context,
	params *wallet.RawSigParams) (wallet.RawSignature, error) {

	args := w.Called(ctx, params)
	sig, _ := args.Get(0).(wallet.RawSignature)

	return sig, args.Error(1)
}

// SignDigest supplies the concrete signature selected by the request.
func (w *walletControllerMock) SignDigest(ctx context.Context,
	path wallet.BIP32Path, intent *wallet.SignDigestIntent) (
	wallet.Signature, error) {

	args := w.Called(ctx, path, intent)
	sig, _ := args.Get(0).(wallet.Signature)

	return sig, args.Error(1)
}

// GetAddressInfo resolves only public path metadata for signing.
func (w *walletControllerMock) GetAddressInfo(ctx context.Context,
	addr address.Address) (wallet.AddressInfo, error) {

	args := w.Called(ctx, addr)
	info, _ := args.Get(0).(wallet.AddressInfo)

	return info, args.Error(1)
}
