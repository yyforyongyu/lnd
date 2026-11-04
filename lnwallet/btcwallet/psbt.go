package btcwallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2/hdkeychain"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	// PsbtKeyTypeInputSignatureTweakSingle is a custom/proprietary PSBT key
	// for an input that specifies what single tweak should be applied to
	// the key before signing the input. The value 51 is leet speak for
	// "si", short for "single".
	PsbtKeyTypeInputSignatureTweakSingle = []byte{0x51}

	// PsbtKeyTypeInputSignatureTweakDouble is a custom/proprietary PSBT key
	// for an input that specifies what double tweak should be applied to
	// the key before signing the input. The value d0 is leet speak for
	// "do", short for "double".
	PsbtKeyTypeInputSignatureTweakDouble = []byte{0xd0}

	// ErrInputMissingUTXOInfo is returned if a PSBT input is supplied that
	// does not specify the witness UTXO info.
	ErrInputMissingUTXOInfo = errors.New(
		"input doesn't specify any UTXO info",
	)

	// ErrScriptSpendFeeEstimationUnsupported is returned if a PSBT input is
	// of a script spend type.
	ErrScriptSpendFeeEstimationUnsupported = errors.New(
		"cannot estimate fee for script spend inputs",
	)

	// ErrUnsupportedScript is returned if a supplied pk script is not
	// known or supported.
	ErrUnsupportedScript = errors.New("unsupported or unknown pk script")
)

// SignPsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all unsigned inputs that have all required fields
// (UTXO information, BIP32 derivation information, witness or sig scripts) set.
// If no error is returned, the PSBT is ready to be given to the next signer or
// to be finalized if lnd was the last signer.
//
// NOTE: This method only signs inputs (and only those it can sign), it does not
// perform any other tasks (such as coin selection, UTXO locking or
// input/output/fee value validation, PSBT finalization). Any input that is
// incomplete will be skipped.
func (b *BtcWallet) SignPsbt(ctx context.Context,
	packet *psbt.Packet) ([]uint32, error) {
	// Proprietary Lightning fields choose the wallet's existing per-input
	// tweak hook. Wallet owns key selection, signing and PSBT mutation.
	tweakers := make(map[int]wallet.PrivKeyTweaker)
	for index, in := range packet.Inputs {
		for _, field := range in.Unknowns {
			if !bytes.Equal(field.Key,
				PsbtKeyTypeInputSignatureTweakSingle) &&
				!bytes.Equal(field.Key,
					PsbtKeyTypeInputSignatureTweakDouble) {

				continue
			}
			tweakers[index] = func(key *btcec.PrivateKey) (
				*btcec.PrivateKey, error) {

				tweaked := maybeTweakPrivKeyPsbt(
					in.Unknowns, key,
				)

				return tweaked, nil
			}

			break
		}
	}
	result, err := b.wallet.SignPsbt(
		ctx, &wallet.SignPsbtParams{
			Packet:        packet,
			InputTweakers: tweakers,
		},
	)
	if err != nil {
		return nil, err
	}

	return result.SignedInputs, nil
}

// validateSigningMethod attempts to detect the signing method that is required
// to sign for the given PSBT input and makes sure all information is available
// to do so.
func validateSigningMethod(in *psbt.PInput) (input.SignMethod, error) {
	script, err := txscript.ParsePkScript(in.WitnessUtxo.PkScript)
	if err != nil {
		return 0, fmt.Errorf("error detecting signing method, "+
			"couldn't parse pkScript: %v", err)
	}

	switch script.Class() {
	case txscript.WitnessV0PubKeyHashTy, txscript.ScriptHashTy,
		txscript.WitnessV0ScriptHashTy:

		return input.WitnessV0SignMethod, nil

	case txscript.WitnessV1TaprootTy:
		if len(in.TaprootBip32Derivation) == 0 {
			return 0, fmt.Errorf("cannot sign for taproot input " +
				"without taproot BIP0032 derivation info")
		}

		// Currently, we only support creating one signature per input.
		//
		// TODO(guggero): Should we support signing multiple paths at
		// the same time? What are the performance and security
		// implications?
		if len(in.TaprootBip32Derivation) > 1 {
			return 0, fmt.Errorf("unsupported multiple taproot " +
				"BIP0032 derivation info found, can only " +
				"sign for one at a time")
		}

		derivation := in.TaprootBip32Derivation[0]
		switch {
		// No leaf hashes means this is the internal key we're signing
		// with, so it's a key spend. And no merkle root means this is
		// a BIP0086 output we're signing for.
		case len(derivation.LeafHashes) == 0 &&
			len(in.TaprootMerkleRoot) == 0:

			return input.TaprootKeySpendBIP0086SignMethod, nil

		// A non-empty merkle root means we committed to a taproot hash
		// that we need to use in the tap tweak.
		case len(derivation.LeafHashes) == 0:
			// Getting here means the merkle root isn't empty, but
			// is it exactly the length we need?
			if len(in.TaprootMerkleRoot) != sha256.Size {
				return 0, fmt.Errorf("invalid taproot merkle "+
					"root length, got %d expected %d",
					len(in.TaprootMerkleRoot), sha256.Size)
			}

			return input.TaprootKeySpendSignMethod, nil

		// Currently, we only support signing for one leaf at a time.
		//
		// TODO(guggero): Should we support signing multiple paths at
		// the same time? What are the performance and security
		// implications?
		case len(derivation.LeafHashes) == 1:
			// If we're supposed to be signing for a leaf hash, we
			// also expect the leaf script that hashes to that hash
			// in the appropriate field.
			if len(in.TaprootLeafScript) != 1 {
				return 0, fmt.Errorf("specified leaf hash in " +
					"taproot BIP0032 derivation but " +
					"missing taproot leaf script")
			}

			leafScript := in.TaprootLeafScript[0]
			leaf := txscript.TapLeaf{
				LeafVersion: leafScript.LeafVersion,
				Script:      leafScript.Script,
			}
			leafHash := leaf.TapHash()
			if !bytes.Equal(leafHash[:], derivation.LeafHashes[0]) {
				return 0, fmt.Errorf("specified leaf hash in" +
					"taproot BIP0032 derivation but " +
					"corresponding taproot leaf script " +
					"was not found")
			}

			return input.TaprootScriptSpendSignMethod, nil

		default:
			return 0, fmt.Errorf("unsupported number of leaf " +
				"hashes in taproot BIP0032 derivation info, " +
				"can only sign for one at a time")
		}

	default:
		return 0, fmt.Errorf("unsupported script class for signing "+
			"PSBT: %v", script.Class())
	}
}

// EstimateInputWeight estimates the weight of a PSBT input and adds it to the
// passed in TxWeightEstimator. It returns an error if the input type is
// unknown or unsupported. Only inputs that have a known witness size are
// supported, which is P2WKH, NP2WKH and P2TR (key spend path).
func EstimateInputWeight(in *psbt.PInput, w *input.TxWeightEstimator) error {
	if in.WitnessUtxo == nil {
		return ErrInputMissingUTXOInfo
	}

	pkScript := in.WitnessUtxo.PkScript
	switch {
	case txscript.IsPayToScriptHash(pkScript):
		w.AddNestedP2WKHInput()

	case txscript.IsPayToWitnessPubKeyHash(pkScript):
		w.AddP2WKHInput()

	case txscript.IsPayToWitnessScriptHash(pkScript):
		return fmt.Errorf("P2WSH inputs are not supported, cannot "+
			"estimate witness size for script spend: %w",
			ErrScriptSpendFeeEstimationUnsupported)

	case txscript.IsPayToTaproot(pkScript):
		signMethod, err := validateSigningMethod(in)
		if err != nil {
			return fmt.Errorf("error determining p2tr signing "+
				"method: %w", err)
		}

		switch signMethod {
		// For p2tr key spend paths.
		case input.TaprootKeySpendBIP0086SignMethod,
			input.TaprootKeySpendSignMethod:

			w.AddTaprootKeySpendInput(in.SighashType)

		// For p2tr script spend path.
		case input.TaprootScriptSpendSignMethod:
			return fmt.Errorf("P2TR inputs are not supported, "+
				"cannot estimate witness size for script "+
				"spend: %w",
				ErrScriptSpendFeeEstimationUnsupported)

		default:
			return fmt.Errorf("unsupported signing method for "+
				"PSBT signing: %v", signMethod)
		}

	default:
		return fmt.Errorf("unknown input type for script %x: %w",
			pkScript, ErrUnsupportedScript)
	}

	return nil
}

// maybeTweakPrivKeyPsbt examines if there are any tweak parameters given in the
// custom/proprietary PSBT fields and may perform a mapping on the passed
// private key in order to utilize the tweaks, if populated.
func maybeTweakPrivKeyPsbt(unknowns []*psbt.Unknown,
	privKey *btcec.PrivateKey) *btcec.PrivateKey {

	// There can be other custom/unknown keys in a PSBT that we just ignore.
	// Key tweaking is optional and only one tweak (single _or_ double) can
	// ever be applied (at least for any use cases described in the BOLT
	// spec).
	for _, u := range unknowns {
		if bytes.Equal(u.Key, PsbtKeyTypeInputSignatureTweakSingle) {
			return input.TweakPrivKey(privKey, u.Value)
		}

		if bytes.Equal(u.Key, PsbtKeyTypeInputSignatureTweakDouble) {
			doubleTweakKey, _ := btcec.PrivKeyFromBytes(
				u.Value,
			)
			return input.DeriveRevocationPrivKey(
				privKey, doubleTweakKey,
			)
		}
	}

	return privKey
}

// Bip32DerivationFromKeyDesc returns the default and Taproot BIP-0032 key
// derivation information from the given key descriptor information.
func Bip32DerivationFromKeyDesc(keyDesc keychain.KeyDescriptor,
	coinType uint32) (*psbt.Bip32Derivation, *psbt.TaprootBip32Derivation,
	string) {

	bip32Derivation := &psbt.Bip32Derivation{
		PubKey: keyDesc.PubKey.SerializeCompressed(),
		Bip32Path: []uint32{
			keychain.BIP0043Purpose + hdkeychain.HardenedKeyStart,
			coinType + hdkeychain.HardenedKeyStart,
			uint32(keyDesc.Family) +
				uint32(hdkeychain.HardenedKeyStart),
			0,
			keyDesc.Index,
		},
	}

	derivationPath := fmt.Sprintf(
		"m/%d'/%d'/%d'/%d/%d", keychain.BIP0043Purpose, coinType,
		keyDesc.Family, 0, keyDesc.Index,
	)

	return bip32Derivation, &psbt.TaprootBip32Derivation{
		XOnlyPubKey:          bip32Derivation.PubKey[1:],
		MasterKeyFingerprint: bip32Derivation.MasterKeyFingerprint,
		Bip32Path:            bip32Derivation.Bip32Path,
		LeafHashes:           make([][]byte, 0),
	}, derivationPath
}

// Bip32DerivationFromAddress returns the default and Taproot BIP-0032 key
// derivation information from the given managed address.
func Bip32DerivationFromAddress(
	addr *wallet.AddressInfo) (*psbt.Bip32Derivation,
	*psbt.TaprootBip32Derivation, string, error) {

	// Imported keys may have no root origin. Do not invent a signing path
	// from an account display number when metadata is absent.
	if addr.PubKey == nil {
		return nil, nil, "", fmt.Errorf(
			"address is not a pubkey address",
		)
	}
	if addr.Derivation == nil {
		return nil, nil, "", fmt.Errorf(
			"address has no BIP32 derivation path",
		)
	}
	scope := addr.Derivation.KeyScope
	derivationInfo := addr.Derivation

	bip32Derivation := &psbt.Bip32Derivation{
		PubKey:               addr.PubKey.SerializeCompressed(),
		MasterKeyFingerprint: derivationInfo.MasterKeyFingerprint,
		Bip32Path: []uint32{
			scope.Purpose + hdkeychain.HardenedKeyStart,
			scope.Coin + hdkeychain.HardenedKeyStart,
			derivationInfo.Account +
				hdkeychain.HardenedKeyStart,
			derivationInfo.Branch,
			derivationInfo.Index,
		},
	}

	derivationPath := fmt.Sprintf(
		"m/%d'/%d'/%d'/%d/%d", scope.Purpose, scope.Coin,
		derivationInfo.Account, derivationInfo.Branch,
		derivationInfo.Index,
	)

	return bip32Derivation, &psbt.TaprootBip32Derivation{
		XOnlyPubKey:          bip32Derivation.PubKey[1:],
		MasterKeyFingerprint: bip32Derivation.MasterKeyFingerprint,
		Bip32Path:            bip32Derivation.Bip32Path,
		LeafHashes:           make([][]byte, 0),
	}, derivationPath, nil
}
