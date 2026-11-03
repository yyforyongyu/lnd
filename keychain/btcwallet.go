package keychain

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
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
// wallet. Wallet startup creates the usual family accounts; explicit requests
// provision missing families through Wallet without owning database state.
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
	// A requested family need not be in startup's initial range. Provision
	// that exact account and let Wallet own the allocation and its cursor.
	if errors.Is(err, wallet.ErrAccountNotFound) {
		if err := b.createFamilyAccount(keyFam); err != nil {
			return KeyDescriptor{}, err
		}
		key, err = b.wallet.AllocateNextKey(
			context.Background(), wallet.NewAccountSelectorByNumber(
				b.chainKeyScope, wallet.AccountNumber(keyFam),
			), false,
		)
	}
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
	// Reading a missing family requires explicit caller provisioning, not
	// a fallback to another account inside Wallet's derivation API.
	if errors.Is(err, wallet.ErrAccountNotInStore) {
		if err := b.createFamilyAccount(keyLoc.Family); err != nil {
			return KeyDescriptor{}, err
		}
		pubKey, err = b.wallet.DerivePubKey(
			context.Background(), wallet.DerivePubKeyParams{
				Account: wallet.NewAccountSelectorByNumber(
					b.chainKeyScope,
					wallet.AccountNumber(keyLoc.Family),
				),
				Branch: 0,
				Index:  keyLoc.Index,
			},
		)
	}
	if err != nil {
		return KeyDescriptor{}, err
	}

	return KeyDescriptor{PubKey: pubKey, KeyLocator: keyLoc}, nil
}

// createFamilyAccount provisions one exact off-chain family. Wallet serializes
// creation, so a concurrent caller that already created it permits the retry.
func (b *BtcWalletKeyRing) createFamilyAccount(family KeyFamily) error {
	number := wallet.AccountNumber(family)
	name := fmt.Sprintf("act:%d", family)
	if family == 0 {
		name = waddrmgr.DefaultAccountName
	}
	schema := waddrmgr.ScopeAddrSchema{
		ExternalAddrType: waddrmgr.WitnessPubKey,
		InternalAddrType: waddrmgr.WitnessPubKey,
	}
	_, err := b.wallet.NewAccount(
		context.Background(), wallet.NewAccountParams{
			Scope:         b.chainKeyScope,
			Name:          name,
			AccountNumber: &number,
			AddrSchema:    &schema,
			NoChainSync:   true,
		},
	)
	if errors.Is(err, wallet.ErrAccountAlreadyExists) {
		return nil
	}

	return err
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
	for i := range MaxKeyRangeScan {
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

	// Message hashing is lnd policy; Wallet owns key admission, signing and
	// key cleanup. The locator selects the existing purpose-1017 family.
	digest := chainhash.HashB(msg)
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	}
	path := wallet.BIP32Path{
		KeyScope: b.chainKeyScope,
		DerivationPath: waddrmgr.DerivationPath{
			InternalAccount: uint32(keyLoc.Family),
			Account:         uint32(keyLoc.Family),
			Index:           keyLoc.Index,
		},
	}
	result, err := b.wallet.SignDigest(
		context.Background(), path, &wallet.SignDigestIntent{
			Digest:  digest,
			SigType: wallet.SigTypeECDSA,
		},
	)
	if err != nil {
		return nil, err
	}
	sig, ok := result.(wallet.ECDSASignature)
	if !ok {
		return nil, fmt.Errorf("unexpected signature type %T", result)
	}

	return sig.Signature, nil
}

// SignMessageCompact signs the given message, single or double SHA256 hashing
// it first, with the private key described in the key locator and returns
// the signature in the compact, public key recoverable format.
//
// NOTE: This is part of the keychain.MessageSignerRing interface.
func (b *BtcWalletKeyRing) SignMessageCompact(keyLoc KeyLocator,
	msg []byte, doubleHash bool) ([]byte, error) {

	// Message hashing is lnd policy; Wallet owns key admission, signing and
	// key cleanup. The locator selects the existing purpose-1017 family.
	digest := chainhash.HashB(msg)
	if doubleHash {
		digest = chainhash.DoubleHashB(msg)
	}
	path := wallet.BIP32Path{
		KeyScope: b.chainKeyScope,
		DerivationPath: waddrmgr.DerivationPath{
			InternalAccount: uint32(keyLoc.Family),
			Account:         uint32(keyLoc.Family),
			Index:           keyLoc.Index,
		},
	}
	result, err := b.wallet.SignDigest(
		context.Background(), path, &wallet.SignDigestIntent{
			Digest:     digest,
			SigType:    wallet.SigTypeECDSA,
			CompactSig: true,
		},
	)
	if err != nil {
		return nil, err
	}
	sig, ok := result.(wallet.CompactSignature)
	if !ok {
		return nil, fmt.Errorf("unexpected signature type %T", result)
	}

	return []byte(sig), nil
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

	// Message hashing is lnd policy; Wallet owns key admission, signing and
	// key cleanup. The locator selects the existing purpose-1017 family.
	var digest []byte
	switch {
	case len(tag) > 0:
		hash := chainhash.TaggedHash(tag, msg)
		digest = hash[:]
	case doubleHash:
		digest = chainhash.DoubleHashB(msg)
	default:
		digest = chainhash.HashB(msg)
	}
	// Wallet treats a non-nil empty tweak as BIP86, while this lnd method
	// treats every empty tweak as absent. Preserve that caller contract.
	if len(taprootTweak) == 0 {
		taprootTweak = nil
	}

	path := wallet.BIP32Path{
		KeyScope: b.chainKeyScope,
		DerivationPath: waddrmgr.DerivationPath{
			InternalAccount: uint32(keyLoc.Family),
			Account:         uint32(keyLoc.Family),
			Index:           keyLoc.Index,
		},
	}
	result, err := b.wallet.SignDigest(
		context.Background(), path, &wallet.SignDigestIntent{
			Digest:       digest,
			SigType:      wallet.SigTypeSchnorr,
			TaprootTweak: taprootTweak,
		},
	)
	if err != nil {
		return nil, err
	}
	sig, ok := result.(wallet.SchnorrSignature)
	if !ok {
		return nil, fmt.Errorf("unexpected signature type %T", result)
	}

	return sig.Signature, nil
}
