package keychain

import (
	"context"
	"crypto/sha256"
	"fmt"

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

var (
	// lightningAddrSchema is the scope addr schema for all keys that we
	// derive. We'll treat them all as p2wkh addresses, as atm we must
	// specify a particular type.
	lightningAddrSchema = waddrmgr.ScopeAddrSchema{
		ExternalAddrType: waddrmgr.WitnessPubKey,
		InternalAddrType: waddrmgr.WitnessPubKey,
	}

	// waddrmgrNamespaceKey is the namespace key that the waddrmgr state is
	// stored within the top-level waleltdb buckets of btcwallet.
	waddrmgrNamespaceKey = []byte("waddrmgr")
)

// BtcWalletKeyRing is an implementation of both the KeyRing and SecretKeyRing
// interfaces backed by btcwallet's internal root waddrmgr. Internally, we'll
// be using a ScopedKeyManager to do all of our derivations, using the key
// scope and scope addr scehma defined above. Re-using the existing key scope
// construction means that all key derivation will be protected under the root
// seed of the wallet, making each derived key fully deterministic.
type BtcWalletKeyRing struct {
	// wallet is a pointer to the active instance of the btcwallet core. Key
	// derivation is performed through its bucket-free, role-based
	// DerivePubKey/DerivePrivKey API so that it works for both legacy kvdb
	// and SQL-backed wallets.
	wallet wallet.Interface

	// chainKeyScope defines the purpose and coin type to be used when generating
	// keys for this keyring.
	chainKeyScope waddrmgr.KeyScope
}

// NewBtcWalletKeyRing creates a new implementation of the
// keychain.SecretKeyRing interface backed by btcwallet.
//
// NOTE: The passed waddrmgr.Manager MUST be unlocked in order for the keychain
// to function.
func NewBtcWalletKeyRing(w wallet.Interface, coinType uint32) SecretKeyRing {
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

// bip32Path builds the full BIP32 derivation path for the given key family and
// index within lnd's custom key scope. lnd's keychain always derives from the
// external (receive) branch, and models each key family as an account whose
// number equals the family.
func (b *BtcWalletKeyRing) bip32Path(keyFam KeyFamily,
	index uint32) wallet.BIP32Path {

	return wallet.BIP32Path{
		KeyScope: b.chainKeyScope,
		DerivationPath: waddrmgr.DerivationPath{
			InternalAccount: uint32(keyFam),
			Account:         uint32(keyFam),
			Branch:          0,
			Index:           index,
		},
	}
}

// nextExternalIndex returns the next unused external (receive) key index for
// the given key family by locating the account whose number matches the family
// within lnd's custom key scope, and reading its external key count.
//
// GAP: this reads the account's recorded external key count but does not
// atomically reserve/advance it, and for SQL-backed wallets the custom-scope
// accounts are not present in the store (InitAccounts only populates the legacy
// kvdb bucket). See the port report.
func (b *BtcWalletKeyRing) nextExternalIndex(ctx context.Context,
	keyFam KeyFamily) (uint32, error) {

	accounts, err := b.wallet.ListAccountsByScope(ctx, b.chainKeyScope)
	if err != nil {
		return 0, err
	}

	for _, acct := range accounts {
		if acct.AccountNumber != nil &&
			*acct.AccountNumber == uint32(keyFam) {

			return acct.ExternalKeyCount, nil
		}
	}

	return 0, fmt.Errorf("account %d not found in key scope %v", keyFam,
		b.chainKeyScope)
}

// DeriveNextKey attempts to derive the *next* key within the key family
// (account in BIP43) specified. This method should return the next external
// child within this branch.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *BtcWalletKeyRing) DeriveNextKey(keyFam KeyFamily) (KeyDescriptor,
	error) {

	ctx := context.Background()

	// Determine the next unused external index for this key family, then
	// derive the corresponding public key via the wallet's bucket-free
	// DerivePubKey.
	//
	// GAP (not fully runtime-correct): unlike the legacy path, which called
	// NextExternalAddresses to atomically derive AND persist the next
	// address, DerivePubKey does not advance/persist the address chain. See
	// nextExternalIndex and the port report for the required by-number,
	// store-backed "next address" primitive.
	nextIndex, err := b.nextExternalIndex(ctx, keyFam)
	if err != nil {
		return KeyDescriptor{}, err
	}

	pubKey, err := b.wallet.DerivePubKey(ctx, b.bip32Path(keyFam, nextIndex))
	if err != nil {
		return KeyDescriptor{}, err
	}

	return KeyDescriptor{
		PubKey: pubKey,
		KeyLocator: KeyLocator{
			Family: keyFam,
			Index:  nextIndex,
		},
	}, nil
}

// DeriveKey attempts to derive an arbitrary key specified by the passed
// KeyLocator. This may be used in several recovery scenarios, or when manually
// rotating something like our current default node key.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *BtcWalletKeyRing) DeriveKey(keyLoc KeyLocator) (KeyDescriptor, error) {
	// The derivation path is fully specified by the key locator, so we can
	// derive the public key directly via the wallet's bucket-free
	// DerivePubKey (which transparently falls back to the store for
	// SQL-backed accounts).
	pubKey, err := b.wallet.DerivePubKey(
		context.Background(), b.bip32Path(keyLoc.Family, keyLoc.Index),
	)
	if err != nil {
		return KeyDescriptor{}, err
	}

	return KeyDescriptor{
		KeyLocator: keyLoc,
		PubKey:     pubKey,
	}, nil
}

// DerivePrivKey attempts to derive the private key that corresponds to the
// passed key descriptor.
//
// NOTE: This is part of the keychain.SecretKeyRing interface.
func (b *BtcWalletKeyRing) DerivePrivKey(keyDesc KeyDescriptor) (
	*btcec.PrivateKey, error) {

	ctx := context.Background()

	// If the exact derivation path is known (either there's no public key
	// to search for, or a specific non-zero index was given), we can derive
	// the private key directly via the wallet's bucket-free DerivePrivKey,
	// which transparently falls back to the store for SQL-backed accounts.
	if keyDesc.PubKey == nil || keyDesc.Index > 0 {
		return b.wallet.DerivePrivKey(
			ctx, b.bip32Path(keyDesc.Family, keyDesc.Index),
		)
	}

	// Otherwise we only know the public key and its key family, so we scan
	// forward through the family's external branch, deriving each public
	// key until we find the matching index, then return that private key.
	//
	// TODO(roasbeef): possibly move scanning into wallet to allow to be
	// parallelized
	for i := 0; i < MaxKeyRangeScan; i++ {
		path := b.bip32Path(keyDesc.Family, uint32(i))

		pubKey, err := b.wallet.DerivePubKey(ctx, path)
		if err != nil {
			return nil, err
		}

		// This wasn't the target key, so roll forward and try the next
		// one.
		if !pubKey.IsEqual(keyDesc.PubKey) {
			continue
		}

		// This is the target public key, so derive and return the
		// corresponding private key.
		return b.wallet.DerivePrivKey(ctx, path)
	}

	// If we reach this point, then we were unable to derive the private
	// key, so return an error back to the user.
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
