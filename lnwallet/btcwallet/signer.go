package btcwallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// signerKeyPath translates lnd's locator or address-bound public key into
// the explicit HD path required by the maintained signing methods.
func (b *BtcWallet) signerKeyPath(ctx context.Context,
	keyDesc *keychain.KeyDescriptor) (
	base.BIP32Path, error) {

	// The ambiguous zero locator historically falls back to family zero
	// only when the supplied public key has never belonged to the wallet.
	path := base.BIP32Path{
		KeyScope: b.chainKeyScope,
		DerivationPath: waddrmgr.DerivationPath{
			InternalAccount: uint32(keyDesc.Family),
			Account:         uint32(keyDesc.Family),
			Index:           keyDesc.Index,
		},
	}
	if !keyDesc.KeyLocator.IsEmpty() || keyDesc.PubKey == nil {
		return path, nil
	}
	hash := address.Hash160(keyDesc.PubKey.SerializeCompressed())
	addr, err := address.NewAddressWitnessPubKeyHash(hash, b.netParams)
	if err != nil {
		return base.BIP32Path{}, err
	}
	info, err := b.wallet.GetAddressInfo(ctx, addr)
	if errors.Is(err, base.ErrAddressNotFound) {
		return path, nil
	}
	if err != nil {
		return base.BIP32Path{}, err
	}
	if info.Derivation == nil {
		return base.BIP32Path{}, base.ErrNoAssocPrivateKey
	}
	path.KeyScope = info.Derivation.KeyScope
	path.DerivationPath = waddrmgr.DerivationPath{
		InternalAccount: info.Derivation.Account,
		Account:         info.Derivation.Account,
		Branch:          info.Derivation.Branch,
		Index:           info.Derivation.Index,
	}

	return path, nil
}

// fetchPrivKey attempts to retrieve the raw private key corresponding to the
// passed public key if populated, or the key descriptor path (if non-empty).
func (b *BtcWallet) fetchPrivKey(
	keyDesc *keychain.KeyDescriptor) (*btcec.PrivateKey, error) {

	// MuSig still consumes private keys, including legacy imported keys
	// without an HD path. Keep its address lookup while native raw/message
	// signing uses only the path resolver and never exports key material.
	path, err := b.signerKeyPath(
		context.Background(), &keychain.KeyDescriptor{
			KeyLocator: keyDesc.KeyLocator,
		},
	)
	if err != nil {
		return nil, err
	}

	// If the key locator within the descriptor *isn't* empty, then we can
	// directly derive the keys raw.
	emptyLocator := keyDesc.KeyLocator.IsEmpty()
	if !emptyLocator || keyDesc.PubKey == nil {
		return b.wallet.DerivePrivKey(
			context.Background(), path,
		)
	}

	hash160 := address.Hash160(keyDesc.PubKey.SerializeCompressed())
	addr, err := address.NewAddressWitnessPubKeyHash(hash160, b.netParams)
	if err != nil {
		return nil, err
	}

	// Otherwise, we'll attempt to derive the key based on the address.
	// This will only work if we've already derived this address in the
	// past, since the wallet relies on a mapping of addr -> key.
	key, err := b.wallet.GetPrivKeyForAddress(context.Background(), addr)
	switch {
	// If we didn't find this key in the wallet, then there's a chance that
	// this is actually an "empty" key locator. The legacy KeyLocator
	// format failed to properly distinguish an empty key locator from the
	// very first in the index (0, 0).IsEmpty() == true.
	case errors.Is(err, base.ErrNoAssocPrivateKey) && emptyLocator:
		// The maintained signer uses the same error for unknown and
		// watch-only addresses. Only an unknown address permits the
		// legacy (0,0) fallback; never replace an owned public key.
		_, addrErr := b.wallet.GetAddressInfo(
			context.Background(), addr,
		)
		if errors.Is(addrErr, base.ErrAddressNotFound) {
			return b.wallet.DerivePrivKey(
				context.Background(), path,
			)
		}

		return nil, err

	case err != nil:
		return nil, err

	default:
		return key, nil
	}
}

// maybeTweakPrivKey examines the single and double tweak parameters on the
// passed sign descriptor and may perform a mapping on the passed private key
// in order to utilize the tweaks, if populated. If both tweak parameters are
// set, then both are applied in the following order:
//
//	a) double tweak
//	b) single tweak
func maybeTweakPrivKey(signDesc *input.SignDescriptor,
	privKey *btcec.PrivateKey) (*btcec.PrivateKey, error) {

	var retPriv *btcec.PrivateKey

	switch {
	// If both tweak parameters are set, apply the double tweak first
	// (revocation), then the single tweak (HTLC index).
	case signDesc.DoubleTweak != nil && signDesc.SingleTweak != nil:
		retPriv = input.DeriveRevocationPrivKey(
			privKey, signDesc.DoubleTweak,
		)
		retPriv = input.TweakPrivKey(retPriv, signDesc.SingleTweak)

	case signDesc.SingleTweak != nil:
		retPriv = input.TweakPrivKey(privKey, signDesc.SingleTweak)

	case signDesc.DoubleTweak != nil:
		retPriv = input.DeriveRevocationPrivKey(
			privKey, signDesc.DoubleTweak,
		)

	default:
		retPriv = privKey
	}

	return retPriv, nil
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignDescriptor.
//
// Native wallet signing supplies the key material and signature algorithm.
func (b *BtcWallet) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	// Resolve only public path metadata. Wallet owns private-key admission,
	// the Lightning tweak callback and the actual signing operation.
	path, err := b.signerKeyPath(context.Background(), &signDesc.KeyDesc)
	if err != nil {
		return nil, err
	}
	params := &base.RawSigParams{
		Tx:         tx,
		InputIndex: signDesc.InputIndex,
		Output:     signDesc.Output,
		SigHashes:  signDesc.SigHashes,
		HashType:   signDesc.HashType,
		Path:       path,
		Tweaker: func(key *btcec.PrivateKey) (
			*btcec.PrivateKey, error) {

			return maybeTweakPrivKey(signDesc, key)
		},
		Details: base.SegwitV0SpendDetails{
			WitnessScript: signDesc.WitnessScript,
		},
	}
	isTaproot := txscript.IsPayToTaproot(signDesc.Output.PkScript)
	if isTaproot {
		params.SigHashes = txscript.NewTxSigHashes(
			tx, signDesc.PrevOutputFetcher,
		)
		switch signDesc.SignMethod {
		case input.TaprootKeySpendBIP0086SignMethod,
			input.TaprootKeySpendSignMethod:

			params.Details = base.TaprootSpendDetails{
				SpendPath: base.KeyPathSpend,
				Tweak:     signDesc.TapTweak,
			}
		case input.TaprootScriptSpendSignMethod:
			params.Details = base.TaprootSpendDetails{
				SpendPath:     base.ScriptPathSpend,
				WitnessScript: signDesc.WitnessScript,
			}
		default:
			return nil, fmt.Errorf("unknown sign method: %v",
				signDesc.SignMethod)
		}
	}
	sig, err := b.wallet.ComputeRawSig(context.Background(), params)
	if err != nil {
		return nil, err
	}

	// Taproot may include its sighash byte; the lnd signature interface
	// stores only the Schnorr signature. V0 already omits its sighash byte.
	if isTaproot {
		return schnorr.ParseSignature(sig[:schnorr.SignatureSize])
	}

	return ecdsa.ParseDERSignature(sig)
}

// ComputeInputScript generates a complete InputScript for the passed
// transaction with the signature as defined within the passed SignDescriptor.
// This method is capable of generating the proper input script for both
// regular p2wkh output and p2wkh outputs nested within a regular p2sh output.
//
// Native wallet signing supplies the key material and signature algorithm.
func (b *BtcWallet) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	// If a tweak (single or double) is specified, then we'll need to use
	// this tweak to derive the final private key to be used for signing
	// this output.
	privKeyTweaker := func(k *btcec.PrivateKey) (*btcec.PrivateKey, error) {
		return maybeTweakPrivKey(signDesc, k)
	}

	// Let the wallet compute the input script now.
	script, err := b.wallet.ComputeUnlockingScript(
		context.Background(), &base.UnlockingScriptParams{
			Tx:         tx,
			Output:     signDesc.Output,
			InputIndex: signDesc.InputIndex,
			SigHashes:  signDesc.SigHashes,
			HashType:   signDesc.HashType,
			Tweaker:    privKeyTweaker,
		},
	)
	if err != nil {
		return nil, err
	}

	return &input.Script{
		Witness:   script.Witness,
		SigScript: script.SigScript,
	}, nil
}

// A compile time check to ensure that BtcWallet implements the Signer
// interface.
var _ input.Signer = (*BtcWallet)(nil)

// SignMessage attempts to sign a target message with the private key that
// corresponds to the passed key locator. If the target private key is unable to
// be found, then an error will be returned. The actual digest signed is the
// double SHA-256 of the passed message.
//
// NOTE: This is a part of the MessageSigner interface.
func (b *BtcWallet) SignMessage(keyLoc keychain.KeyLocator,
	msg []byte, doubleHash bool) (*ecdsa.Signature, error) {

	// lnd defines message hashing; Wallet owns key access and ECDSA
	// signing.
	path, err := b.signerKeyPath(
		context.Background(), &keychain.KeyDescriptor{
			KeyLocator: keyLoc,
		},
	)
	if err != nil {
		return nil, err
	}
	digest := chainhash.HashB(msg)
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	}
	result, err := b.wallet.SignDigest(
		context.Background(), path, &base.SignDigestIntent{
			Digest:  digest,
			SigType: base.SigTypeECDSA,
		},
	)
	if err != nil {
		return nil, err
	}
	sig, ok := result.(base.ECDSASignature)
	if !ok {
		return nil, fmt.Errorf("unexpected signature type %T", result)
	}

	return sig.Signature, nil
}

// A compile time check to ensure that BtcWallet implements the MessageSigner
// interface.
var _ lnwallet.MessageSigner = (*BtcWallet)(nil)
