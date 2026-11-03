package btcwallet

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/btcutil/v2/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	// seedBytes is the raw entropy of the aezeed:
	//   able promote dizzy mixture sword myth share public find tattoo
	//   catalog cousin bulb unfair machine alarm cool large promote kick
	//   shop rug mean year
	// Which corresponds to the master root key:
	//   xprv9s21ZrQH143K2KADjED57FvNbptdKLp4sqKzssegwEGKQMGoDkbyhUeCKe5m3A
	//   MU44z4vqkmGswwQVKrv599nFG16PPZDEkNrogwoDGeCmZ
	seedBytes, _ = hex.DecodeString("4a7611b6979ba7c4bc5c5cd2239b2973")

	// firstAddress is the first address that we should get from the wallet,
	// corresponding to the derivation path m/84'/0'/0'/0/0 (even on regtest
	// which is a special case for the BIP49/84 addresses in btcwallet).
	firstAddress = "bcrt1qgdlgjc5ede7fjv350wcjqat80m0zsmfaswsj9p"

	// firstAddressPubKey is the public key of the first address that we
	// should get from the wallet.
	firstAddressPubKey = "02b844aecf8250c29e46894147a7dae02de55a034a533b6" +
		"0c6a6469294ee356ce4"

	// firstAddressTaproot is the first address that we should get from the
	// wallet when deriving a taproot address.
	firstAddressTaproot = "bcrt1ps8c222fgysvnsj2m8hxk8khy6wthcrhv9va9z3t4" +
		"h3qeyz65sh4qqwvdgc"

	// firstAddressTaprootPubKey is the public key of the first address that
	// we should get from the wallet when deriving a taproot address.
	firstAddressTaprootPubKey = "03004113d6185c955d6e8f5922b50cc0ac3b64fa" +
		"0979402604c5b887f07e3b5388"

	testPubKeyBytes, _ = hex.DecodeString(
		"037a67771635344641d4b56aac33cd5f7a265b59678dce3aec31b89125e3" +
			"b8b9b2",
	)
	testPubKey, _          = btcec.ParsePubKey(testPubKeyBytes)
	testTaprootKeyBytes, _ = hex.DecodeString(
		"03f068684c9141027318eed958dccbf4f7f748700e1da53315630d82a362" +
			"d6a887",
	)
	testTaprootKey, _ = btcec.ParsePubKey(testTaprootKeyBytes)

	testTapscriptAddr = "bcrt1p7p5xsny3gyp8xx8wm9vdejl57lm5suqwrkjnx9trpk" +
		"p2xckk4zrs4xehl8"
	testTapscriptPkScript = append(
		[]byte{txscript.OP_1, txscript.OP_DATA_32},
		schnorr.SerializePubKey(testTaprootKey)...,
	)
)

// TestScriptImport tests tapscript import with a full script tree and a
// partially revealed branch whose proof must yield the same address.
func TestScriptImport(t *testing.T) {
	// Arrange the existing funded wallet and tapscript tree, retaining the
	// expected addresses while switching metadata to wallet-owned DTOs.
	netParams := &chaincfg.RegressionNetParams
	w, miner := newTestWallet(t, netParams, seedBytes)

	firstDerivedAddr, err := w.InternalWallet().NewAddress(
		t.Context(), wallet.NewAccountSelectorByName(
			waddrmgr.KeyScopeBIP0086, lnwallet.DefaultAccountName,
		), false,
	)
	require.NoError(t, err)
	require.Equal(t, firstAddressTaproot, firstDerivedAddr.Addr.String())

	scope := waddrmgr.KeyScopeBIP0086
	_, err = w.InternalWallet().GetAccount(
		t.Context(), scope, lnwallet.DefaultAccountName,
	)
	require.NoError(t, err)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(address.Hash160([]byte("foobar")))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	script1, err := builder.Script()
	require.NoError(t, err)
	leaf1 := txscript.NewBaseTapLeaf(script1)

	// Let's add a second script output as well to test the partial reveal.
	builder = txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(testPubKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	script2, err := builder.Script()
	require.NoError(t, err)
	leaf2 := txscript.NewBaseTapLeaf(script2)

	// Our first test case is storing the script with all its leaves.
	tapscript1 := input.TapscriptFullTree(testPubKey, leaf1, leaf2)

	taprootKey1, err := tapscript1.TaprootKey()
	require.NoError(t, err)
	require.Equal(
		t, testTaprootKey.SerializeCompressed(),
		taprootKey1.SerializeCompressed(),
	)

	// Act by importing the complete tapscript through WalletController.
	addr1, err := w.InternalWallet().
		ImportTaprootScript(t.Context(), *tapscript1)
	require.NoError(t, err)

	// Assert the maintained address and script match the original vectors.
	require.Equal(t, testTapscriptAddr, addr1.Addr.String())
	pkScript, err := txscript.PayToAddrScript(addr1.Addr)
	require.NoError(t, err)
	require.Equal(t, testTapscriptPkScript, pkScript)

	// Send some coins to the taproot address now and wait until they are
	// seen as unconfirmed.
	_, err = miner.SendOutputs([]*wire.TxOut{{
		Value:    btcutil.SatoshiPerBitcoin,
		PkScript: pkScript,
	}}, 1)
	require.NoError(t, err)

	var utxos []*wallet.Utxo
	require.Eventually(t, func() bool {
		utxos, err = w.InternalWallet().ListUnspent(
			t.Context(), wallet.UtxoQuery{
				MinConfs: 0,
				MaxConfs: math.MaxInt32,
			},
		)
		require.NoError(t, err)

		return len(utxos) == 1
	}, time.Minute, 50*time.Millisecond)
	require.Equal(t, testTapscriptPkScript, utxos[0].PkScript)

	// Arrange a partial reveal of the same output. The maintained import
	// API retains the first representation and returns its stored metadata
	// for repeats, so the caller need not emulate a legacy duplicate error.
	inclusionProof := leaf2.TapHash()
	tapscript2 := input.TapscriptPartialReveal(
		testPubKey, leaf1, inclusionProof[:],
	)
	// Act by importing the equivalent output with less script information.
	addr2, err := w.InternalWallet().
		ImportTaprootScript(t.Context(), *tapscript2)

	// Assert that the duplicate returns the canonical existing metadata,
	// rather than replacing it with the partial reveal's representation.
	require.NoError(t, err)
	require.Equal(t, addr1, addr2)
	stored, err := w.InternalWallet().GetAddressInfo(
		t.Context(), addr1.Addr,
	)
	require.NoError(t, err)
	require.Equal(t, addr1, stored)
}

// TestInitialAccountExport verifies a fresh SQL wallet exposes every Lightning
// account before lnd starts, so remote signers can export complete public keys.
func TestInitialAccountExport(t *testing.T) {
	// Arrange a fresh SQLite wallet through the ordinary constructor and
	// real chain fixture, leaving BtcWallet.Start uncalled so its account
	// completion loop cannot hide missing creation-time exports.
	netParams := &chaincfg.RegressionNetParams
	chainBackend, _ := getChainBackend(t, netParams)
	config := Config{
		PrivatePass: []byte("some-pass"),
		HdSeed:      seedBytes,
		NetParams:   netParams,
		CoinType:    netParams.HDCoinType,
		ChainSource: chainBackend,
		ManagerConfig: wallet.ManagerConfig{
			Backend:    wallet.DBBackendSQLite,
			DataSource: filepath.Join(t.TempDir(), "wallet.sqlite"),
		},
	}
	w, err := New(config, blockcache.NewBlockCache(10000))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Stop()) })

	// Act by listing the public account snapshots the remote-signer export
	// consumes before startup can create accounts alongside scanning.
	accounts, err := w.InternalWallet().ListAccountsByScope(
		t.Context(), waddrmgr.KeyScope{
			Purpose: keychain.BIP0043Purpose,
			Coin:    netParams.HDCoinType,
		},
	)

	// Assert the complete 0..255 family set is immediately exportable as
	// public extended keys and remains excluded from payment-chain scans.
	require.NoError(t, err)
	require.Len(t, accounts, 256)
	seen := make(map[wallet.AccountNumber]bool, len(accounts))
	for _, account := range accounts {
		require.NotNil(t, account.AccountNumber)
		number := *account.AccountNumber
		require.LessOrEqual(t, number, wallet.AccountNumber(255))
		require.False(t, seen[number])
		seen[number] = true
		require.True(t, account.NoChainSync)
		xpub, err := hdkeychain.NewKeyFromString(
			string(account.PublicKey),
		)
		require.NoError(t, err)
		require.False(t, xpub.IsPrivate())
	}
}

// newTestWallet starts a fresh managed SQLite wallet for signing tests.
func newTestWallet(t *testing.T, netParams *chaincfg.Params,
	seedBytes []byte) (*BtcWallet, *rpctest.Harness) {

	t.Helper()

	// Arrange a real chain source and fresh native storage so lnd creates
	// its exact key families through the same account API as normal
	// startup.
	chainBackend, miner := getChainBackend(t, netParams)

	config := Config{
		PrivatePass: []byte("some-pass"),
		HdSeed:      seedBytes,
		NetParams:   netParams,
		CoinType:    netParams.HDCoinType,
		ChainSource: chainBackend,
		ManagerConfig: wallet.ManagerConfig{
			Backend:    wallet.DBBackendSQLite,
			DataSource: filepath.Join(t.TempDir(), "wallet.sqlite"),
		},
	}
	blockCache := blockcache.NewBlockCache(10000)

	// Act through the ordinary constructor and account initialization. No
	// legacy loader or private database setup participates in this fixture.
	w, err := New(config, blockCache)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Stop()) })

	err = w.Start()

	// Assert startup success before exposing keys or addresses to a test.
	require.NoError(t, err)

	return w, miner
}

// getChainBackend returns a simple btcd based chain backend to back the wallet.
func getChainBackend(t *testing.T, netParams *chaincfg.Params) (chain.Interface,
	*rpctest.Harness) {

	miningNode := unittest.NewMiner(
		t, netParams, []string{"--txindex"}, true, 25,
	)

	// Next, mine enough blocks in order for SegWit and the CSV package
	// soft-fork to activate on RegNet.
	numBlocks := netParams.MinerConfirmationWindow * 2
	_, err := miningNode.Client.Generate(numBlocks)
	require.NoError(t, err)

	rpcConfig := miningNode.RPCConfig()
	chainClient, err := chain.NewRPCClient(
		netParams, rpcConfig.Host, rpcConfig.User, rpcConfig.Pass,
		rpcConfig.Certificates, false, 20,
	)
	require.NoError(t, err)

	return chainClient, miningNode
}

// hardenedKey returns a key of a hardened derivation key path.
func hardenedKey(part uint32) uint32 {
	return part + hdkeychain.HardenedKeyStart
}

// TestMaybeTweakPrivKey tests the maybeTweakPrivKey function to ensure it
// correctly applies single tweaks, double tweaks, both tweaks combined, and
// handles the case where no tweaks are applied.
func TestMaybeTweakPrivKey(t *testing.T) {
	// Create a test private key.
	privKeyBytes, err := hex.DecodeString(
		"e68abc8e2a7a5b9f0e4a3c7d8b9e6f5" +
			"a4b3c2d1e0f9e8d7c6b5a4938271605",
	)
	require.NoError(t, err)
	privKey, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)

	// Create test tweak values.
	singleTweakBytes, err := hex.DecodeString(
		"1234567890abcdef1234567890abcdef" +
			"1234567890abcdef1234567890abcdef",
	)
	require.NoError(t, err)

	doubleTweakBytes, err := hex.DecodeString(
		"fedcba0987654321fedcba0987654321" +
			"fedcba0987654321fedcba0987654321",
	)
	require.NoError(t, err)
	doubleTweak, _ := btcec.PrivKeyFromBytes(doubleTweakBytes)

	testCases := []struct {
		name        string
		singleTweak []byte
		doubleTweak *btcec.PrivateKey
		validate    func(*testing.T, *btcec.PrivateKey,
			*btcec.PublicKey)
	}{
		{
			name:        "no tweaks applied",
			singleTweak: nil,
			doubleTweak: nil,
			validate: func(t *testing.T, result *btcec.PrivateKey,
				_ *btcec.PublicKey) {

				// Should return the original private key
				// unchanged.
				require.Equal(
					t, privKey.Serialize(),
					result.Serialize(),
					"expected private key to be unchanged",
				)
			},
		},
		{
			name:        "single tweak only",
			singleTweak: singleTweakBytes,
			doubleTweak: nil,
			validate: func(t *testing.T, result *btcec.PrivateKey,
				_ *btcec.PublicKey) {

				// Manually apply single tweak to verify.
				expected := input.TweakPrivKey(
					privKey, singleTweakBytes,
				)
				require.Equal(
					t, expected.Serialize(),
					result.Serialize(),
					"single tweak not applied correctly",
				)
				// Ensure it's different from the original.
				require.NotEqual(
					t, privKey.Serialize(),
					result.Serialize(),
					"tweaked key should differ from "+
						"original",
				)
			},
		},
		{
			name:        "double tweak only",
			singleTweak: nil,
			doubleTweak: doubleTweak,
			validate: func(t *testing.T, result *btcec.PrivateKey,
				_ *btcec.PublicKey) {

				// Manually apply double tweak to verify.
				expected := input.DeriveRevocationPrivKey(
					privKey, doubleTweak,
				)
				require.Equal(
					t, expected.Serialize(),
					result.Serialize(),
					"double tweak not applied correctly",
				)
				// Ensure it's different from the original.
				require.NotEqual(
					t, privKey.Serialize(),
					result.Serialize(),
					"tweaked key should differ from "+
						"original",
				)
			},
		},
		{
			name:        "both tweaks combined",
			singleTweak: singleTweakBytes,
			doubleTweak: doubleTweak,
			validate: func(t *testing.T, result *btcec.PrivateKey,
				origPubKey *btcec.PublicKey) {

				// Manually apply both tweaks in order: double
				// first, then single.
				afterDouble := input.DeriveRevocationPrivKey(
					privKey, doubleTweak,
				)
				expected := input.TweakPrivKey(
					afterDouble, singleTweakBytes,
				)
				require.Equal(
					t, expected.Serialize(),
					result.Serialize(),
					"combined tweaks not applied correctly",
				)
				// Ensure it's different from single tweak only.
				singleOnly := input.TweakPrivKey(
					privKey, singleTweakBytes,
				)
				require.NotEqual(
					t, singleOnly.Serialize(),
					result.Serialize(),
					"combined tweak should differ from "+
						"single tweak only",
				)

				// Calculate the expected tweaked public key by
				// applying the same tweaks to the original
				// public key.
				doubleTweakPub := doubleTweak.PubKey()
				afterDoublePub := input.DeriveRevocationPubkey(
					origPubKey, doubleTweakPub,
				)
				finalPK := input.TweakPubKeyWithTweak(
					afterDoublePub, singleTweakBytes,
				)

				// Verify that the tweaked private key can sign
				// for the expected tweaked public key.
				testMsg := []byte("test message for signing")
				msgHash := sha256.Sum256(testMsg)
				signature := ecdsa.Sign(result, msgHash[:])

				// Verify the signature with the tweaked public
				// key.
				require.True(
					t,
					signature.Verify(msgHash[:], finalPK),
					"signature verification failed for "+
						"combined tweaked key",
				)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a sign descriptor with the test tweaks.
			signDesc := &input.SignDescriptor{
				SingleTweak: tc.singleTweak,
				DoubleTweak: tc.doubleTweak,
			}

			// Call the function under test.
			result, err := maybeTweakPrivKey(signDesc, privKey)
			require.NoError(t, err)
			require.NotNil(t, result)

			// Validate the result.
			tc.validate(t, result, pubKey)
		})
	}
}

// TestSignOutputRawDelegates checks descriptor translation at the maintained
// signing boundary; the wallet owns signature generation and key admission.
func TestSignOutputRawDelegates(t *testing.T) {
	t.Parallel()

	// Arrange stable wallet encodings to check adapter conversion,
	// without reproducing private-key derivation or transaction signing.
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	hash := address.Hash160(key.PubKey().SerializeCompressed())
	addr, err := address.NewAddressWitnessPubKeyHash(hash, netParams)
	require.NoError(t, err)
	digest := chainhash.HashB([]byte("raw signing"))
	ecdsaSig := ecdsa.Sign(key, digest)
	schnorrSig, err := schnorr.Sign(key, digest)
	require.NoError(t, err)
	tests := []struct {
		name      string
		method    input.SignMethod
		byAddress bool
		walletErr error
	}{
		{
			name:   "segwit locator",
			method: input.WitnessV0SignMethod,
		},
		{
			name:      "taproot key metadata",
			method:    input.TaprootKeySpendSignMethod,
			byAddress: true,
		},
		{
			name:   "taproot script locator",
			method: input.TaprootScriptSpendSignMethod,
		},
		{
			name:      "wallet admission failure",
			walletErr: wallet.ErrNoAssocPrivateKey,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange an explicit locator, or public address
			// metadata,
			// and an encoded wallet signature. No private-key
			// lookup is
			// configured, so the adapter must use the native
			// signer.
			scope := waddrmgr.KeyScope{
				Purpose: keychain.BIP0043Purpose,
				Coin:    1,
			}
			path := wallet.BIP32Path{
				KeyScope: scope,
				DerivationPath: waddrmgr.DerivationPath{
					InternalAccount: 2,
					Account:         2,
					Index:           3,
				},
			}
			output := wire.NewTxOut(1000, []byte{0x51})
			desc := &input.SignDescriptor{
				KeyDesc: keychain.KeyDescriptor{
					KeyLocator: keychain.KeyLocator{
						Family: 2,
						Index:  3,
					},
				},
				Output:        output,
				WitnessScript: []byte{0x51},
				HashType:      txscript.SigHashAll,
				SignMethod:    test.method,
				SingleTweak:   testTweakSingle,
				TapTweak:      digest,
			}
			backend := &walletControllerMock{}
			if test.byAddress {
				desc.KeyDesc = keychain.KeyDescriptor{
					PubKey: key.PubKey(),
				}
				path.KeyScope = waddrmgr.KeyScopeBIP0086
				path.DerivationPath.Branch = 1

				origin := &wallet.AddressDerivation{
					KeyScope: path.KeyScope,
					Account:  2,
					Branch:   1,
					Index:    3,
				}
				backend.On(
					"GetAddressInfo", mock.Anything, addr,
				).
					Return(wallet.AddressInfo{
						Derivation: origin,
					}, nil).Once()
			}
			tx := wire.NewMsgTx(2)
			tx.AddTxIn(&wire.TxIn{})
			tx.AddTxIn(&wire.TxIn{})
			desc.InputIndex = 1
			expectedSig := ecdsaSig.Serialize()
			raw := wallet.RawSignature(expectedSig)
			var details wallet.SpendDetails
			details = wallet.SegwitV0SpendDetails{
				WitnessScript: desc.WitnessScript,
			}
			if test.method != input.WitnessV0SignMethod {
				desc.Output.PkScript = testTapscriptPkScript
				fetcher := txscript.NewCannedPrevOutputFetcher(
					desc.Output.PkScript, desc.Output.Value,
				)
				desc.PrevOutputFetcher = fetcher
				expectedSig = schnorrSig.Serialize()
				raw = wallet.RawSignature(expectedSig)
				raw = append(raw, byte(desc.HashType))
				details = wallet.TaprootSpendDetails{
					SpendPath: wallet.KeyPathSpend,
					Tweak:     desc.TapTweak,
				}
				if test.method ==
					input.TaprootScriptSpendSignMethod {

					spend := wallet.ScriptPathSpend
					script := desc.WitnessScript
					details = wallet.TaprootSpendDetails{
						SpendPath:     spend,
						WitnessScript: script,
					}
				}
			}
			var captured *wallet.RawSigParams
			backend.On(
				"ComputeRawSig", mock.Anything, mock.Anything,
			).
				Run(func(args mock.Arguments) {
					arg := args.Get(1)
					captured, _ = arg.(*wallet.RawSigParams)
				}).Return(raw, test.walletErr).Once()
			t.Cleanup(func() { backend.AssertExpectations(t) })
			controller := &BtcWallet{
				wallet:        backend,
				chainKeyScope: scope,
				netParams:     netParams,
			}

			// Act through SignOutputRaw so path, spend details and
			// the
			// returned encoding are checked at the public boundary.
			sig, err := controller.SignOutputRaw(tx, desc)

			// Assert lnd supplies its exact descriptor and tweak
			// policy,
			// while wallet errors and actual signature bytes
			// survive.
			require.NotNil(t, captured)
			require.Same(t, tx, captured.Tx)
			require.Same(t, desc.Output, captured.Output)
			require.Equal(t, path, captured.Path)
			require.Equal(t, details, captured.Details)
			require.Equal(t, desc.HashType, captured.HashType)
			require.Equal(t, desc.InputIndex, captured.InputIndex)
			if desc.PrevOutputFetcher != nil {
				require.Equal(t, txscript.NewTxSigHashes(
					tx, desc.PrevOutputFetcher,
				), captured.SigHashes)
			} else {
				hashes := captured.SigHashes
				require.Same(t, desc.SigHashes, hashes)
			}
			tweaked, tweakErr := captured.Tweaker(key)
			require.NoError(t, tweakErr)
			expected := input.TweakPrivKey(key, testTweakSingle)
			require.Equal(t, expected, tweaked)
			if test.walletErr != nil {
				require.ErrorIs(t, err, test.walletErr)
				require.Nil(t, sig)
				return
			}
			require.NoError(t, err)
			require.Equal(t, expectedSig, sig.Serialize())
		})
	}
}

// TestSignMessageDelegates checks lnd hashing and the maintained signer result
// without deriving or exposing a private key through the adapter.
func TestSignMessageDelegates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		doubleHash bool
		walletErr  error
	}{
		{
			name: "single digest",
		},
		{
			name:       "double digest",
			doubleHash: true,
		},
		{
			name:      "wallet error",
			walletErr: wallet.ErrNoAssocPrivateKey,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange the exact purpose-1017 path and digest
			// expected
			// by Wallet, including the caller's double-hash choice.
			msg := []byte("lightning message")
			digest := chainhash.HashB(msg)
			if test.doubleHash {
				digest = chainhash.DoubleHashB(msg)
			}
			key, _ := btcec.PrivKeyFromBytes([]byte{1})
			sig := ecdsa.Sign(key, digest)
			path := wallet.BIP32Path{
				KeyScope: waddrmgr.KeyScope{
					Purpose: keychain.BIP0043Purpose,
					Coin:    1,
				},
				DerivationPath: waddrmgr.DerivationPath{
					InternalAccount: 2,
					Account:         2,
					Index:           3,
				},
			}
			backend := &walletControllerMock{}
			backend.On("SignDigest", mock.Anything, path,
				&wallet.SignDigestIntent{
					Digest:  digest,
					SigType: wallet.SigTypeECDSA,
				}).Return(wallet.ECDSASignature{
				Signature: sig,
			}, test.walletErr).Once()
			t.Cleanup(func() { backend.AssertExpectations(t) })
			controller := &BtcWallet{
				wallet:        backend,
				chainKeyScope: path.KeyScope,
			}

			// Act through the public message signer; no private-key
			// method is configured on the mock.
			got, err := controller.SignMessage(keychain.KeyLocator{
				Family: 2,
				Index:  3,
			}, msg, test.doubleHash)

			// Assert the concrete ECDSA result is returned
			// unchanged,
			// or the original wallet error prevents a signature.
			if test.walletErr != nil {
				require.ErrorIs(t, err, test.walletErr)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Same(t, sig, got)
		})
	}
}
