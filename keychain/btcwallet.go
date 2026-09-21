package keychain

import (
	"context"
	"crypto/sha256"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
)

const (
	// CoinTypeBitcoin specifies the BIP44 coin type for Bitcoin key
	// derivation.
	CoinTypeBitcoin uint32 = 0

	// CoinTypeTestnet specifies the BIP44 coin type for all testnet key
	// derivation.
	CoinTypeTestnet = 1
)

// BtcWalletKeyRing derives deterministic purpose-1017 keys through the managed
// wallet. Wallet startup creates the family accounts; derivation here neither
// creates accounts nor owns a second database transaction or scope cache.
type BtcWalletKeyRing struct {
	wallet *wallet.Wallet

	// chainKeyScope supplies the purpose and network coin for every
	// locator.
	chainKeyScope waddrmgr.KeyScope
}

var _ SecretKeyRing = (*BtcWalletKeyRing)(nil)

// NewBtcWalletKeyRing creates a new implementation of the
// keychain.SecretKeyRing interface backed by btcwallet.
//
// NOTE: The managed wallet must be unlocked and its family accounts
// initialized.
func NewBtcWalletKeyRing(w *wallet.Wallet, coinType uint32) SecretKeyRing {
	// Construct the key scope that will be used within the waddrmgr to
	// create an HD chain for deriving all of our required keys. A different
	// scope is used for each specific coin type.
	chainKeyScope := waddrmgr.KeyScope{
		Purpose: BIP0043Purpose,
		Coin:    coinType,
	}

	return &BtcWalletKeyRing{
		wallet:        w,
		chainKeyScope: chainKeyScope,
	}
}

// DeriveNextKey persists the next external child in the requested family.
// Allocation belongs to btcwallet so restart cannot reuse a returned locator.
func (b *BtcWalletKeyRing) DeriveNextKey(keyFam KeyFamily) (KeyDescriptor, error) {
	key, err := b.wallet.AllocateNextKey(
		context.Background(), wallet.NewAccountSelectorByNumber(
			b.chainKeyScope, wallet.AccountNumber(keyFam),
		), false,
	)
	if err != nil {
		return KeyDescriptor{}, err
	}
	return KeyDescriptor{
		PubKey: key.PubKey,
		KeyLocator: KeyLocator{
			Family: keyFam,
			Index:  key.Index,
		},
	}, nil
}

// DeriveKey reads the public key at an exact locator without consuming another
// allocation index, preserving recovery and explicit node-key selection.
func (b *BtcWalletKeyRing) DeriveKey(keyLoc KeyLocator) (KeyDescriptor, error) {
	pubKey, err := b.wallet.DerivePubKey(
		context.Background(), wallet.DerivePubKeyParams{
			Account: wallet.NewAccountSelectorByNumber(
				b.chainKeyScope,
				wallet.AccountNumber(keyLoc.Family),
			),
			Branch: 0,
			Index:  keyLoc.Index,
		},
	)
	if err != nil {
		return KeyDescriptor{}, err
	}

	return KeyDescriptor{PubKey: pubKey, KeyLocator: keyLoc}, nil
}

// DerivePrivKey resolves a known locator or scans its family's public keys when
// only a public key was supplied. Private material is derived only for the
// matching path, retaining lnd's existing bounded lookup and signing behavior.
func (b *BtcWalletKeyRing) DerivePrivKey(keyDesc KeyDescriptor) (
	*btcec.PrivateKey, error) {

	path := wallet.BIP32Path{
		KeyScope: b.chainKeyScope,
		DerivationPath: waddrmgr.DerivationPath{
			InternalAccount: uint32(keyDesc.Family),
			Account:         uint32(keyDesc.Family),
			Branch:          0,
			Index:           keyDesc.Index,
		},
	}
	if keyDesc.PubKey == nil || keyDesc.Index > 0 {
		return b.wallet.DerivePrivKey(context.Background(), path)
	}

	// The zero index with a public key means the caller does not know its
	// locator. Scan the same external range as before, without allocating.
	for i := 0; i < MaxKeyRangeScan; i++ {
		candidate, err := b.DeriveKey(KeyLocator{
			Family: keyDesc.Family,
			Index:  uint32(i),
		})
		if err != nil {
			return nil, err
		}
		if candidate.PubKey.IsEqual(keyDesc.PubKey) {
			path.DerivationPath.Index = uint32(i)
			return b.wallet.DerivePrivKey(
				context.Background(),
				path,
			)
		}
	}

	return nil, ErrCannotDerivePrivKey
}

// ECDH performs a scalar multiplication (ECDH-like operation) between the
// target key descriptor and remote public key. The output returned will be
// the sha256 of the resulting shared point serialized in compressed format. If
// k is our private key, and P is the public key, we perform the following
// operation:
//
//	sx := k*P s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the keychain.ECDHRing interface.
func (b *BtcWalletKeyRing) ECDH(keyDesc KeyDescriptor,
	pub *btcec.PublicKey) ([32]byte, error) {

	privKey, err := b.DerivePrivKey(keyDesc)
	if err != nil {
		return [32]byte{}, err
	}

	var (
		pubJacobian btcec.JacobianPoint
		s           btcec.JacobianPoint
	)
	pub.AsJacobian(&pubJacobian)

	btcec.ScalarMultNonConst(&privKey.Key, &pubJacobian, &s)
	s.ToAffine()
	sPubKey := btcec.NewPublicKey(&s.X, &s.Y)
	h := sha256.Sum256(sPubKey.SerializeCompressed())

	return h, nil
}

// SignMessage signs the given message, single or double SHA256 hashing it
// first, with the private key described in the key locator.
//
// NOTE: This is part of the keychain.MessageSignerRing interface.
func (b *BtcWalletKeyRing) SignMessage(keyLoc KeyLocator,
	msg []byte, doubleHash bool) (*ecdsa.Signature, error) {

	privKey, err := b.DerivePrivKey(KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, err
	}

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}
	return ecdsa.Sign(privKey, digest), nil
}

// SignMessageCompact signs the given message, single or double SHA256 hashing
// it first, with the private key described in the key locator and returns
// the signature in the compact, public key recoverable format.
//
// NOTE: This is part of the keychain.MessageSignerRing interface.
func (b *BtcWalletKeyRing) SignMessageCompact(keyLoc KeyLocator,
	msg []byte, doubleHash bool) ([]byte, error) {

	privKey, err := b.DerivePrivKey(KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, err
	}

	var digest []byte
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	} else {
		digest = chainhash.HashB(msg)
	}

	return ecdsa.SignCompact(privKey, digest, true), nil
}

// SignMessageSchnorr uses the Schnorr signature algorithm to sign the given
// message, single or double SHA256 hashing it first, with the private key
// described in the key locator and the optional tweak applied to the private
// key.
//
// NOTE: This is part of the keychain.MessageSignerRing interface.
func (b *BtcWalletKeyRing) SignMessageSchnorr(keyLoc KeyLocator,
	msg []byte, doubleHash bool, taprootTweak []byte,
	tag []byte) (*schnorr.Signature, error) {

	privKey, err := b.DerivePrivKey(KeyDescriptor{
		KeyLocator: keyLoc,
	})
	if err != nil {
		return nil, err
	}

	if len(taprootTweak) > 0 {
		privKey = txscript.TweakTaprootPrivKey(*privKey, taprootTweak)
	}

	// If a tag was provided, we need to take the tagged hash of the input.
	var digest []byte
	switch {
	case len(tag) > 0:
		taggedHash := chainhash.TaggedHash(tag, msg)
		digest = taggedHash[:]
	case doubleHash:
		digest = chainhash.DoubleHashB(msg)
	default:
		digest = chainhash.HashB(msg)
	}
	return schnorr.Sign(privKey, digest)
}
