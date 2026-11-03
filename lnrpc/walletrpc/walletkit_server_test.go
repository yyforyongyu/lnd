//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/btcutil/v2/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// TestWitnessTypeMapping tests that the two witness type enums in the `input`
// package and the `walletrpc` package remain equal.
func TestWitnessTypeMapping(t *testing.T) {
	t.Parallel()

	// Tests that both enum types have the same length except the
	// UNKNOWN_WITNESS type which is only present in the walletrpc
	// witness type enum.
	require.Equal(
		t, len(allWitnessTypes), len(WitnessType_name)-1,
		"number of witness types should match proto definition",
	)

	// Tests that the string representations of both enum types are
	// equivalent.
	for witnessType, witnessTypeProto := range allWitnessTypes {
		// Redeclare to avoid loop variables being captured
		// by func literal.

		t.Run(witnessType.String(), func(tt *testing.T) {
			tt.Parallel()

			witnessTypeName := witnessType.String()
			witnessTypeName = strings.ToUpper(witnessTypeName)
			witnessTypeProtoName := witnessTypeProto.String()
			witnessTypeProtoName = strings.ReplaceAll(
				witnessTypeProtoName, "_", "",
			)

			require.Equal(
				t, witnessTypeName, witnessTypeProtoName,
				"mapped witness types should be named the same",
			)
		})
	}
}

// TestLeaseOutputNoExpiry verifies the explicit flag acquires the wallet's
// no-timeout lifetime and reports that policy to clients.
func TestLeaseOutputNoExpiry(t *testing.T) {
	t.Parallel()

	// Arrange an exact wallet expectation and use the real coin-selection
	// mutex so only the optional wallet capability needs to be mocked. The
	// maintained no-expiry operation returns an absent deadline.
	id := wtxmgr.LockID{1}
	op := wire.OutPoint{Index: 1}
	controller := &mock.WalletController{}
	defer controller.AssertExpectations(t)
	controller.On(
		"LeaseOutput",
		testifymock.Anything,
		id,
		op,
		time.Duration(
			0,
		),
		testifymock.MatchedBy(func(opts []wallet.LeaseOption) bool {
			return len(opts) == 1
		}),
	).Return(time.Time{}, nil).Once()

	server := &WalletKit{cfg: &Config{
		Wallet:              controller,
		CoinSelectionLocker: &lnwallet.LightningWallet{},
	},
	}

	// Act with no duration: the flag alone chooses the
	// persisted reservation, without requiring a user-supplied count.
	resp, err := server.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: id[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   op.Hash[:],
			OutputIndex: op.Index,
		},
		NoExpiry: true,
	})

	// Assert the new response field acknowledges the installed policy;
	// deferred mock verification checks the exact owner and lifetime once.
	require.NoError(t, err)
	require.True(t, resp.NoExpiry)
	require.Zero(t, resp.Expiration)
}

// TestLeaseOutputRejectsNoExpiryConflicts verifies both lease RPCs reject
// competing lifetime settings before touching wallet state.
func TestLeaseOutputRejectsNoExpiryConflicts(t *testing.T) {
	t.Parallel()

	// Arrange without a wallet so validation must reject the conflicting
	// lifetimes before either handler can acquire a reservation.
	server := &WalletKit{cfg: &Config{}}

	// Act through both public request forms with a duration that cannot
	// apply to the explicitly requested no-expiry lifetime.
	_, leaseErr := server.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: bytes.Repeat([]byte{1}, 32),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes: make([]byte, 32),
		},
		NoExpiry:          true,
		ExpirationSeconds: 1,
	})
	_, fundErr := server.FundPsbt(t.Context(), &FundPsbtRequest{
		InputLeaseNoExpiry:    true,
		LockExpirationSeconds: 1,
	})

	// Assert both handlers expose the shared identity, so clients and
	// regression coverage do not depend on diagnostic wording.
	require.ErrorIs(t, leaseErr, errLeaseLifetimeConflict)
	require.ErrorIs(t, fundErr, errLeaseLifetimeConflict)
}

// TestLeaseOutputRejectsRetiredSpendConfs verifies old protobuf requests
// fail explicitly at both lease entry points instead of selecting timed locks.
func TestLeaseOutputRejectsRetiredSpendConfs(t *testing.T) {
	t.Parallel()

	// Arrange the old count value at each retired wire number. Ordinary
	// decoding retains these bytes even though the fields are now reserved;
	// no wallet is configured: rejection must need no acquisition.
	leaseReq := &LeaseOutputRequest{}
	leaseWire := protowire.AppendTag(nil, 4, protowire.VarintType)
	leaseWire = protowire.AppendVarint(leaseWire, 3)
	require.NoError(t, proto.Unmarshal(leaseWire, leaseReq))
	fundReq := &FundPsbtRequest{}
	fundWire := protowire.AppendTag(nil, 15, protowire.VarintType)
	fundWire = protowire.AppendVarint(fundWire, 3)
	require.NoError(t, proto.Unmarshal(fundWire, fundReq))
	server := &WalletKit{cfg: &Config{}}

	// Act through both public handlers with the decoded old requests, as
	// gRPC would deliver them from clients using the removed parameters.
	_, leaseErr := server.LeaseOutput(t.Context(), leaseReq)
	_, fundErr := server.FundPsbt(t.Context(), fundReq)

	// Assert the shared migration error before other validation or wallet
	// work; error identity keeps this independent of diagnostic wording.
	require.ErrorIs(t, leaseErr, errLeaseSpendConfsUnsupported)
	require.ErrorIs(t, fundErr, errLeaseSpendConfsUnsupported)
}

// TestLeaseOutputRenewal verifies a same-owner renewal
// acknowledges the timed or no-expiry lifetime preserved by the wallet.
func TestLeaseOutputRenewal(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		noExpiry bool
	}{
		{
			name:     "persistent",
			noExpiry: true,
		},
		{
			name:     "timed",
			noExpiry: false,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			// Arrange a renewal whose native expiration reports the
			// stored
			// lifetime, using the real coin-selection mutex.
			lockID := wtxmgr.LockID{1}
			outpoint := wire.OutPoint{Index: 1}
			controller := &mock.WalletController{}
			defer controller.AssertExpectations(t)
			controller.On(
				"LeaseOutput",
				testifymock.Anything,
				lockID,
				outpoint,
				time.Minute,
				[]wallet.LeaseOption(nil),
			).Return(func() time.Time {
				if testCase.noExpiry {
					return time.Time{}
				}

				return time.Unix(123, 0)
			}(), nil).Once()
			locker := &lnwallet.LightningWallet{}
			server := &WalletKit{cfg: &Config{
				Wallet:              controller,
				CoinSelectionLocker: locker,
			}}

			// Act without a new policy: the existing lease
			// must keep its persisted lifetime on this renewal.
			resp, err := server.LeaseOutput(
				t.Context(), &LeaseOutputRequest{
					Id: lockID[:],
					Outpoint: &lnrpc.OutPoint{
						TxidBytes:   outpoint.Hash[:],
						OutputIndex: outpoint.Index,
					},
					ExpirationSeconds: 60,
				},
			)

			// Assert the boolean describes the retained lifetime,
			// even though the renewal omitted the no-expiry flag.
			require.NoError(t, err)
			require.Equal(t, testCase.noExpiry, resp.NoExpiry)
		})
	}
}

// TestFundPsbtRequiresCustomLockID verifies no-expiry input
// leases require an external, caller-specific owner ID.
func TestFundPsbtRequiresCustomLockID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		lockID   []byte
		expected error
	}{
		{
			name:     "missing",
			expected: errMissingCustomLockID,
		},
		{
			name:     "all zero",
			lockID:   make([]byte, 32),
			expected: errZeroCustomLockID,
		},
		{
			name:     "reserved internal",
			lockID:   chanfunding.LndInternalLockID[:],
			expected: errReservedCustomLockID,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			// Arrange a no-expiry request with an unsafe owner and
			// no wallet, requiring rejection before acquisition.
			server := &WalletKit{cfg: &Config{}}
			req := &FundPsbtRequest{
				InputLeaseNoExpiry: true,
				CustomLockId:       testCase.lockID,
			}

			// Act before template validation or funding can obscure
			// the owner-ID rejection.
			_, err := server.FundPsbt(t.Context(), req)

			// Assert each invalid identity is rejected, requiring a
			// caller-specific owner for a persistent reservation.
			// Compare identities without matching message text.
			require.ErrorIs(t, err, testCase.expected)
		})
	}
}

// TestFundPsbtCoinSelect tests that the coin selection for a PSBT template
// works as expected.
func TestFundPsbtCoinSelect(t *testing.T) {
	t.Parallel()

	const (
		fundAmt         = 50_000
		existingChange  = chanfunding.ExistingChangeAddress
		sigHashSingle   = txscript.SigHashSingle
		defaultFeeRatio = chanfunding.DefaultMaxFeeRatio
	)
	numsKey := schnorr.SerializePubKey(&input.TaprootNUMSKey)
	var (
		p2wkhDustLimit = lnwallet.DustLimitForSize(input.P2WPKHSize)
		p2trDustLimit  = lnwallet.DustLimitForSize(input.P2TRSize)
		p2wkhScript, _ = input.WitnessPubKeyHash([]byte{})
		p2trScript, _  = txscript.PayToTaprootScript(
			&input.TaprootNUMSKey,
		)
	)

	makePacket := func(outs ...*wire.TxOut) *psbt.Packet {
		p := &psbt.Packet{
			UnsignedTx: &wire.MsgTx{},
		}

		for _, out := range outs {
			p.UnsignedTx.TxOut = append(p.UnsignedTx.TxOut, out)
			p.Outputs = append(p.Outputs, psbt.POutput{})
		}

		return p
	}
	updatePacket := func(p *psbt.Packet,
		f func(*psbt.Packet) *psbt.Packet) *psbt.Packet {

		return f(p)
	}
	calcFee := func(p2trIn, p2wkhIn, p2trOut, p2wkhOut int,
		dust btcutil.Amount) btcutil.Amount {

		estimator := input.TxWeightEstimator{}
		for i := 0; i < p2trIn; i++ {
			estimator.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)
		}
		for i := 0; i < p2wkhIn; i++ {
			estimator.AddP2WKHInput()
		}
		for i := 0; i < p2trOut; i++ {
			estimator.AddP2TROutput()
		}
		for i := 0; i < p2wkhOut; i++ {
			estimator.AddP2WKHOutput()
		}

		weight := estimator.Weight()
		fee := chainfee.FeePerKwFloor.FeeForWeight(weight)

		return fee + dust
	}

	testCases := []struct {
		name        string
		utxos       []*wallet.Utxo
		packet      *psbt.Packet
		changeIndex int32
		changeType  chanfunding.ChangeAddressType
		feeRate     chainfee.SatPerKWeight

		// expectedUtxoIndexes is the list of utxo indexes that are
		// expected to be used for funding the psbt.
		expectedUtxoIndexes []int

		// expectChangeOutputIndex is the expected output index that is
		// returned from the tested method.
		expectChangeOutputIndex int32

		// expectedChangeOutputAmount is the expected final total amount
		// of the output marked as the change output. This will only be
		// checked if the expected amount is non-zero.
		expectedChangeOutputAmount btcutil.Amount

		// expectedFee is the total amount of fees paid by the funded
		// packet in bytes.
		expectedFee btcutil.Amount

		// maxFeeRatio is the maximum fee to total output amount ratio
		// that we consider valid.
		maxFeeRatio float64

		// expectedErr is the expected concrete error. If not nil, then
		// the error must match exactly.
		expectedErr error

		// expectedContainedErrStr is the expected string to be
		// contained in the returned error.
		expectedContainedErrStr string

		// expectedErrType is the expected error type. If not nil, then
		// the error must be of this type.
		expectedErrType error
	}{
		{
			name:  "no utxos",
			utxos: []*wallet.Utxo{},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}),
			changeIndex:     -1,
			feeRate:         chainfee.FeePerKwFloor,
			maxFeeRatio:     defaultFeeRatio,
			expectedErrType: &chanfunding.ErrInsufficientFunds{},
		},
		{
			name: "1 p2wpkh utxo, add p2wkh change",
			utxos: []*wallet.Utxo{
				{
					Amount:   100_000,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 1,
			expectedFee:             calcFee(0, 1, 1, 1, 0),
		},
		{
			name: "1 p2wpkh utxo, add p2tr change",
			utxos: []*wallet.Utxo{
				{
					Amount:   100_000,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              chanfunding.P2TRChangeAddress,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 1,
			expectedFee:             calcFee(0, 1, 2, 0, 0),
		},
		{
			name: "1 p2wpkh utxo, no change, exact amount",
			utxos: []*wallet.Utxo{
				{
					Amount:   fundAmt + 123,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: -1,
			expectedFee:             calcFee(0, 1, 1, 0, 0),
		},
		{
			name: "1 p2wpkh utxo, no change, p2wpkh change dust " +
				"to fee",
			utxos: []*wallet.Utxo{
				{
					Amount: fundAmt + calcFee(
						0, 1, 1, 0, p2wkhDustLimit-1,
					),
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              chanfunding.P2WKHChangeAddress,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: -1,
			expectedFee: calcFee(
				0,
				1,
				1,
				0,
				p2wkhDustLimit-1,
			),
		},
		{
			name: "1 p2wpkh utxo, no change, p2tr change dust to " +
				"fee",
			utxos: []*wallet.Utxo{
				{
					Amount: fundAmt + calcFee(
						0, 1, 1, 0, p2trDustLimit-1,
					),
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              chanfunding.P2TRChangeAddress,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: -1,
			expectedFee: calcFee(
				0,
				1,
				1,
				0,
				p2trDustLimit-1,
			),
		},
		{
			name: "1 p2wpkh utxo, existing p2tr change",
			utxos: []*wallet.Utxo{
				{
					Amount:   fundAmt + 50_000,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 0,
			expectedFee:             calcFee(0, 1, 1, 0, 0),
		},
		{
			name: "1 p2wpkh utxo, existing p2wkh change",
			utxos: []*wallet.Utxo{
				{
					Amount:   fundAmt + 50_000,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2wkhScript,
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 0,
			expectedFee:             calcFee(0, 1, 0, 1, 0),
		},
		{
			name: "1 p2wpkh utxo, existing p2wkh change, dust " +
				"change",
			utxos: []*wallet.Utxo{
				{
					Amount: fundAmt + calcFee(
						0,
						1,
						0,
						1,
						0,
					) + 50,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2wkhScript,
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 0,
			expectedFee:             calcFee(0, 1, 0, 1, 0),
		},
		{
			name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, " +
				"existing " +
				"p2tr change",
			utxos: []*wallet.Utxo{
				{
					Amount:   fundAmt / 2,
					PkScript: p2wkhScript,
				}, {
					Amount:   fundAmt / 2,
					PkScript: p2trScript,
				},
			},
			packet: updatePacket(makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}), func(p *psbt.Packet) *psbt.Packet {
				p.UnsignedTx.TxIn = append(
					p.UnsignedTx.TxIn, &wire.TxIn{
						PreviousOutPoint: wire.OutPoint{
							Hash: chainhash.Hash{
								1,
								2,
								3,
							},
						},
					},
				)
				derivations := []*psbt.TaprootBip32Derivation{
					{
						XOnlyPubKey: numsKey,
						Bip32Path:   []uint32{1, 2, 3},
					},
				}
				p.Inputs = append(p.Inputs, psbt.PInput{
					WitnessUtxo: &wire.TxOut{
						Value:    1000,
						PkScript: p2trScript,
					},
					SighashType:            sigHashSingle,
					TaprootBip32Derivation: derivations,
				})

				return p
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{0, 1},
			expectChangeOutputIndex: 0,
			expectedFee:             calcFee(2, 1, 1, 0, 0),
		},
		{
			name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, " +
				"add p2tr " +
				"change",
			utxos: []*wallet.Utxo{
				{
					Amount:   fundAmt / 2,
					PkScript: p2wkhScript,
				}, {
					Amount:   fundAmt / 2,
					PkScript: p2trScript,
				},
			},
			packet: updatePacket(makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}), func(p *psbt.Packet) *psbt.Packet {
				p.UnsignedTx.TxIn = append(
					p.UnsignedTx.TxIn, &wire.TxIn{
						PreviousOutPoint: wire.OutPoint{
							Hash: chainhash.Hash{
								1,
								2,
								3,
							},
						},
					},
				)
				derivations := []*psbt.TaprootBip32Derivation{
					{
						XOnlyPubKey: numsKey,
						Bip32Path:   []uint32{1, 2, 3},
					},
				}
				p.Inputs = append(p.Inputs, psbt.PInput{
					WitnessUtxo: &wire.TxOut{
						Value:    1000,
						PkScript: p2trScript,
					},
					SighashType:            sigHashSingle,
					TaprootBip32Derivation: derivations,
				})

				return p
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              chanfunding.P2TRChangeAddress,
			expectedUtxoIndexes:     []int{0, 1},
			expectChangeOutputIndex: 1,
			expectedFee:             calcFee(2, 1, 2, 0, 0),
		},
		{
			name: "large existing p2tr input, fee estimation " +
				"p2wpkh " +
				"change",
			utxos: []*wallet.Utxo{},
			packet: updatePacket(makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}), func(p *psbt.Packet) *psbt.Packet {
				p.UnsignedTx.TxIn = append(
					p.UnsignedTx.TxIn, &wire.TxIn{
						PreviousOutPoint: wire.OutPoint{
							Hash: chainhash.Hash{
								1,
								2,
								3,
							},
						},
					},
				)
				derivations := []*psbt.TaprootBip32Derivation{
					{
						XOnlyPubKey: numsKey,
						Bip32Path:   []uint32{1, 2, 3},
					},
				}
				p.Inputs = append(p.Inputs, psbt.PInput{
					WitnessUtxo: &wire.TxOut{
						Value:    fundAmt * 3,
						PkScript: p2trScript,
					},
					TaprootBip32Derivation: derivations,
				})

				return p
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              chanfunding.P2WKHChangeAddress,
			expectedUtxoIndexes:     []int{},
			expectChangeOutputIndex: 1,
			expectedChangeOutputAmount: fundAmt*3 - fundAmt -
				calcFee(1, 0, 1, 1, 0),
			expectedFee: calcFee(1, 0, 1, 1, 0),
		},
		{
			name: "large existing p2tr input, fee estimation no " +
				"change",
			utxos: []*wallet.Utxo{},
			packet: updatePacket(makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}), func(p *psbt.Packet) *psbt.Packet {
				p.UnsignedTx.TxIn = append(
					p.UnsignedTx.TxIn, &wire.TxIn{
						PreviousOutPoint: wire.OutPoint{
							Hash: chainhash.Hash{
								1,
								2,
								3,
							},
						},
					},
				)
				derivations := []*psbt.TaprootBip32Derivation{
					{
						XOnlyPubKey: numsKey,
						Bip32Path:   []uint32{1, 2, 3},
					},
				}
				p.Inputs = append(p.Inputs, psbt.PInput{
					WitnessUtxo: &wire.TxOut{
						Value: fundAmt +
							int64(
								calcFee(
									1,
									0,
									1,
									0,
									0,
								),
							),
						PkScript: p2trScript,
					},
					TaprootBip32Derivation: derivations,
				})

				return p
			}),
			changeIndex:             -1,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              chanfunding.P2TRChangeAddress,
			expectedUtxoIndexes:     []int{},
			expectChangeOutputIndex: -1,
			expectedFee:             calcFee(1, 0, 1, 0, 0),
		},
		{
			name: "1 p2wpkh utxo, existing p2wkh change, invalid " +
				"fee ratio",
			utxos: []*wallet.Utxo{
				{
					Amount:   250,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    50,
				PkScript: p2wkhScript,
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 0,
			expectedFee:             calcFee(0, 1, 0, 1, 0),

			expectedContainedErrStr: "fee 0.00000111 BTC exceeds " +
				"max fee " +
				"(0.00000027 BTC) on total output value",
		},
		{
			name: "1 p2wpkh utxo, existing p2wkh change, " +
				"negative feeratio",
			utxos: []*wallet.Utxo{
				{
					Amount:   250,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    50,
				PkScript: p2wkhScript,
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio * (-1),
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 0,
			expectedFee:             calcFee(0, 1, 0, 1, 0),

			expectedContainedErrStr: "maxFeeRatio must be " +
				"between 0.00 " +
				"and 5.00, got -0.20",
		},
		{
			name: "1 p2wpkh utxo, existing p2wkh change, big fee " +
				"ratio",
			utxos: []*wallet.Utxo{
				{
					Amount:   250,
					PkScript: p2wkhScript,
				},
			},
			packet: makePacket(&wire.TxOut{
				Value:    50,
				PkScript: p2wkhScript,
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             0.85,
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{0},
			expectChangeOutputIndex: 0,
			expectedFee:             calcFee(0, 1, 0, 1, 0),
		},
		{
			name: "large existing p2tr input, fee estimation " +
				"existing " +
				"change output",
			utxos: []*wallet.Utxo{},
			packet: updatePacket(makePacket(&wire.TxOut{
				Value:    fundAmt,
				PkScript: p2trScript,
			}), func(p *psbt.Packet) *psbt.Packet {
				p.UnsignedTx.TxIn = append(
					p.UnsignedTx.TxIn, &wire.TxIn{
						PreviousOutPoint: wire.OutPoint{
							Hash: chainhash.Hash{
								1,
								2,
								3,
							},
						},
					},
				)
				derivations := []*psbt.TaprootBip32Derivation{
					{
						XOnlyPubKey: numsKey,
						Bip32Path:   []uint32{1, 2, 3},
					},
				}
				p.Inputs = append(p.Inputs, psbt.PInput{
					WitnessUtxo: &wire.TxOut{
						Value:    fundAmt * 2,
						PkScript: p2trScript,
					},
					TaprootBip32Derivation: derivations,
				})

				return p
			}),
			changeIndex:             0,
			feeRate:                 chainfee.FeePerKwFloor,
			maxFeeRatio:             defaultFeeRatio,
			changeType:              existingChange,
			expectedUtxoIndexes:     []int{},
			expectChangeOutputIndex: 0,
			expectedChangeOutputAmount: fundAmt*2 - calcFee(
				1,
				0,
				1,
				0,
				0,
			),
			expectedFee: calcFee(1, 0, 1, 0, 0),
		},
	}

	for _, tc := range testCases {

		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		t.Run(tc.name, func(tt *testing.T) {
			// To avoid our packet being mutated, we'll make a deep
			// copy of it, so we can still use the original in the
			// test case to compare the results to.
			var buf bytes.Buffer
			err := tc.packet.Serialize(&buf)
			require.NoError(tt, err)

			copiedPacket, err := psbt.NewFromRawBytes(&buf, false)
			require.NoError(tt, err)

			// Arrange native snapshots and exactly the operations
			// this existing coin-selection case reaches. The real
			// lnd mutex is reused; no mock implements a wallet
			// selection algorithm.
			walletMock := &mock.WalletController{Utxos: tc.utxos}
			tt.Cleanup(func() { walletMock.AssertExpectations(tt) })
			var inputSum, outputSum int64
			for _, in := range copiedPacket.Inputs {
				if in.WitnessUtxo != nil {
					inputSum += in.WitnessUtxo.Value
				}
			}
			for _, out := range copiedPacket.UnsignedTx.TxOut {
				outputSum += out.Value
			}
			listCount := 1
			if outputSum > inputSum {
				listCount++
			}
			walletMock.On("ListUnspent", testifymock.Anything,
				wallet.UtxoQuery{
					Account:  lnwallet.DefaultAccountName,
					MinConfs: 0, MaxConfs: defaultMaxConf,
				},
			).Return(tc.utxos, nil).Times(listCount)
			decorateCount := 1
			success := tc.expectedErr == nil &&
				tc.expectedErrType == nil &&
				tc.expectedContainedErrStr == ""
			if success && len(tc.expectedUtxoIndexes) > 0 {
				decorateCount++
			}
			walletMock.On(
				"DecorateInputs",
				testifymock.Anything,
				copiedPacket,
				true,
			).
				Return(copiedPacket, nil).
				Times(decorateCount)
			if success && tc.changeIndex < 0 &&
				tc.expectChangeOutputIndex >= 0 {

				scope := waddrmgr.KeyScopeBIP0084
				addrType := waddrmgr.WitnessPubKey
				if tc.changeType ==
					chanfunding.P2TRChangeAddress {

					scope = waddrmgr.KeyScopeBIP0086
					addrType = waddrmgr.TaprootPubKey
				}
				addr, err := addrType.AddrFromPubKeyBytes(
					privKey.PubKey().SerializeCompressed(),
					&chaincfg.MainNetParams,
				)
				require.NoError(tt, err)
				info := wallet.AddressInfo{
					Addr:     addr,
					AddrType: addrType,
					PubKey:   privKey.PubKey(),
					Derivation: &wallet.AddressDerivation{
						KeyScope: scope,
					},
				}
				walletMock.On(
					"NewAddress",
					testifymock.Anything,
					wallet.NewAccountSelectorByName(
						scope,
						lnwallet.DefaultAccountName,
					),
					true,
				).
					Return(info, nil).
					Once()
				if addrType == waddrmgr.TaprootPubKey {
					walletMock.On(
						"GetAddressInfo",
						testifymock.Anything,
						addr,
					).
						Return(info, nil).
						Once()
				}
			}
			// Leases read the same parent snapshot; arrange it once
			// so per-input expectations only describe
			// acquisition/readback.
			var outputs []wallet.Output
			for _, utxo := range tc.utxos {
				outputs = append(outputs, wallet.Output{
					Index:    int(utxo.OutPoint.Index),
					Amount:   utxo.Amount,
					PkScript: utxo.PkScript,
				})
			}
			detail := &wallet.TxDetail{Outputs: outputs}
			lockID := chanfunding.LndInternalLockID
			duration := chanfunding.DefaultLockDuration
			for _, index := range tc.expectedUtxoIndexes {
				// Rejected fees return before reserving any
				// selected coin.
				if !success {
					break
				}
				for _, utxo := range tc.utxos {
					if utxo.OutPoint.Index != uint32(
						index,
					) {

						continue
					}
					walletMock.On(
						"GetUtxo",
						testifymock.Anything,
						utxo.OutPoint,
					).
						Return(utxo, nil).
						Once()
					walletMock.On(
						"LeaseOutput",
						testifymock.Anything,
						lockID,
						utxo.OutPoint,
						duration,
						[]wallet.LeaseOption(nil),
					).
						Return(time.Unix(123, 0), nil).
						Once()

					walletMock.On(
						"GetTx",
						testifymock.Anything,
						utxo.OutPoint.Hash,
					).
						Return(detail, nil).
						Once()
				}
			}
			cfg := &Config{
				Wallet:      walletMock,
				ChainParams: &chaincfg.MainNetParams,
			}
			cfg.CoinSelectionLocker = &lnwallet.LightningWallet{}
			cfg.CoinSelectionStrategy = wallet.CoinSelectionLargest
			rpcServer, _, err := New(cfg)
			require.NoError(tt, err)

			// Act through the existing advanced funding request
			// composition.
			resp, err := rpcServer.fundPsbtCoinSelect(
				tt.Context(),
				lnwallet.DefaultAccountName,
				tc.changeIndex,
				copiedPacket,
				0,
				tc.changeType,
				tc.feeRate,
				rpcServer.cfg.CoinSelectionStrategy,
				tc.maxFeeRatio,
				nil,
				0,
				0,
			)

			// Assert the original fee, change and selected-input
			// contract.
			switch {
			case tc.expectedErr != nil:
				require.Error(tt, err)
				require.ErrorIs(tt, err, tc.expectedErr)

				return

			case tc.expectedErrType != nil:
				require.Error(tt, err)
				require.ErrorAs(tt, err, &tc.expectedErr)

				return
			case tc.expectedContainedErrStr != "":
				require.ErrorContains(
					tt, err, tc.expectedContainedErrStr,
				)

				return
			}

			require.NoError(tt, err)
			require.NotNil(tt, resp)

			resultPacket, err := psbt.NewFromRawBytes(
				bytes.NewReader(resp.FundedPsbt), false,
			)
			require.NoError(tt, err)
			resultTx := resultPacket.UnsignedTx

			expectedNumInputs := len(tc.expectedUtxoIndexes) +
				len(tc.packet.Inputs)
			require.Len(tt, resultPacket.Inputs, expectedNumInputs)
			require.Len(tt, resultTx.TxIn, expectedNumInputs)
			require.Equal(
				tt, tc.expectChangeOutputIndex,
				resp.ChangeOutputIndex,
			)

			fee, err := resultPacket.GetTxFee()
			require.NoError(tt, err)
			require.EqualValues(tt, tc.expectedFee, fee)

			if tc.expectedChangeOutputAmount != 0 {
				changeIdx := resp.ChangeOutputIndex
				require.GreaterOrEqual(tt, changeIdx, int32(-1))
				require.Less(
					tt, changeIdx,
					int32(len(resultTx.TxOut)),
				)

				changeOut := resultTx.TxOut[changeIdx]

				require.EqualValues(
					tt, tc.expectedChangeOutputAmount,
					changeOut.Value,
				)
			}
		})
	}
}

// TestMarshallLeasesNoExpiry checks native lifetime and output RPC metadata.
func TestMarshallLeasesNoExpiry(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name       string
		noExpiry   bool
		expiration time.Time
		wireExpiry uint64
	}{
		{
			name:       "timed",
			expiration: time.Unix(123, 0),
			wireExpiry: 123,
		},
		{
			name:     "no expiry",
			noExpiry: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange a native lease and its parent output. History
			// remains available even when an unconfirmed spend
			// removes the UTXO.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			op := wire.OutPoint{Index: 2}
			w.On("GetTx", testifymock.Anything, op.Hash).
				Return(&wallet.TxDetail{
					Outputs: []wallet.Output{
						{
							Index:    2,
							Amount:   1000,
							PkScript: []byte{0x51},
						},
					},
				}, nil).
				Once()
			server := &WalletKit{cfg: &Config{Wallet: w}}

			// Act through the projection shared by funding and
			// lease listing.
			leases, err := server.marshallLeases(
				t.Context(),
				[]*wallet.LeasedOutput{
					{
						OutPoint:   op,
						LockID:     wtxmgr.LockID{1},
						Expiration: tc.expiration,
						NoExpiry:   tc.noExpiry,
					},
				},
			)

			// Assert only native facts are published, including
			// absent expiry.
			require.NoError(t, err)
			require.Len(t, leases, 1)
			require.Equal(t, tc.wireExpiry, leases[0].Expiration)
			require.Equal(t, tc.noExpiry, leases[0].NoExpiry)
			require.EqualValues(t, 1000, leases[0].Value)
			require.Equal(t, []byte{0x51}, leases[0].PkScript)
		})
	}
}

// TestFinalizePsbt checks native dispatch and script-based account constraints,
// including wallet-owned offline inputs absent from the current UTXO set.
func TestFinalizePsbt(t *testing.T) {
	t.Parallel()
	finalizeErr := errors.New("finalizer failed")
	testCases := []struct {
		name         string
		remote       bool
		account      string
		inputAccount uint32
		ambiguous    bool
		foreign      bool
		wantError    string
		finalizeErr  error
	}{
		{
			name: "local without PSBT derivation metadata",
		},
		{
			name:   "remote callback",
			remote: true,
		},
		{
			name:      "different requested account",
			account:   "other",
			wantError: "does not belong to account",
		},
		{
			name:         "offline input from another account",
			inputAccount: 1,
			wantError:    "does not belong to account",
		},
		{
			name:         "matching custom scope",
			account:      "other",
			inputAccount: 1,
		},
		{
			name:      "ambiguous custom name",
			account:   "other",
			ambiguous: true,
			wantError: "ambiguous",
		},
		{
			name:    "foreign input",
			foreign: true,
		},
		{
			name:        "finalizer error",
			finalizeErr: finalizeErr,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange an offline input without PSBT key origins.
			// Only
			// native script metadata establishes ownership; no UTXO
			// lookup or signing expectation can mask rejection.
			w := &mock.WalletController{}
			scope84 := waddrmgr.KeyScopeBIP0084
			scope86 := waddrmgr.KeyScopeBIP0086
			t.Cleanup(func() { w.AssertExpectations(t) })
			op := wire.OutPoint{
				Index: 1,
			}
			packet, err := psbt.New(
				[]*wire.OutPoint{&op},
				[]*wire.TxOut{
					{
						Value: 100,
						PkScript: []byte{
							txscript.OP_TRUE,
						},
					},
				},
				2, 0, []uint32{0},
			)
			require.NoError(t, err)
			script := append([]byte{0, 20}, make([]byte, 20)...)
			packet.Inputs[0].WitnessUtxo = &wire.TxOut{
				Value:    200,
				PkScript: script,
			}
			var buf bytes.Buffer
			require.NoError(t, packet.Serialize(&buf))
			num := wallet.AccountNumber(0)
			if tc.account != "" {
				num = 1
				matches := []wallet.AccountInfo{
					{
						KeyScope:      scope84,
						AccountNumber: &num,
					},
				}
				if tc.ambiguous {
					otherScope := wallet.AccountInfo{
						KeyScope: scope86,
					}
					matches = append(matches, otherScope)
				}
				w.On("ListAccountsByName",
					t.Context(), tc.account,
				).
					Return(matches, nil).Once()
			}
			if !tc.ambiguous {
				origin := &wallet.AddressDerivation{
					KeyScope: scope84,
					Account:  tc.inputAccount,
				}
				info := wallet.OutputScriptInfo{
					AddressInfo: wallet.AddressInfo{
						Derivation: origin,
					},
				}
				var ownershipErr error
				if tc.foreign {
					ownershipErr = wallet.ErrAddressNotFound
				}
				w.On("ScriptForOutput", t.Context(),
					*packet.Inputs[0].WitnessUtxo).
					Return(info, ownershipErr).Once()
				if !tc.foreign {
					name := tc.account
					if name == "" {
						name = "default"
					}
					w.On("GetAccount", t.Context(),
						waddrmgr.KeyScopeBIP0084, name).
						Return(&wallet.AccountInfo{
							AccountNumber: &num,
						}, nil).Once()
				}
			}
			finalizer := w
			if tc.remote {
				finalizer = &mock.WalletController{}
				t.Cleanup(func() {
					finalizer.AssertExpectations(t)
				})
			}
			if tc.wantError == "" {
				// A final witness exercises extraction without
				// copying
				// the maintained wallet's signing
				// implementation.
				finalize := func(args testifymock.Arguments) {
					p, _ := args.Get(1).(*psbt.Packet)
					p.Inputs[0].FinalScriptWitness = []byte{
						1, 1, 1,
					}
				}
				packetArg := testifymock.AnythingOfType(
					"*psbt.Packet",
				)
				finalizer.On(
					"FinalizePsbt", t.Context(), packetArg,
				).
					Return(tc.finalizeErr).
					Run(finalize).Once()
			}
			lw := &lnwallet.LightningWallet{
				Cfg: lnwallet.Config{
					FinalizePsbt: finalizer.FinalizePsbt,
				},
			}
			server := &WalletKit{
				cfg: &Config{
					Wallet:          w,
					LightningWallet: lw,
				},
			}

			// Act through the public RPC; account validation must
			// complete before either selected finalizer can run.
			resp, err := server.FinalizePsbt(t.Context(),
				&FinalizePsbtRequest{
					FundedPsbt: buf.Bytes(),
					Account:    tc.account,
				})

			// Assert rejection occurs before signing, valid offline
			// inputs still finalize, and callback errors keep
			// identity.
			// Cleanup verifies every strict expectation exactly
			// once.
			switch {
			case tc.wantError != "":
				require.ErrorContains(t, err, tc.wantError)
				require.Nil(t, resp)
			case tc.finalizeErr != nil:
				require.ErrorIs(t, err, tc.finalizeErr)
				require.Nil(t, resp)
			default:
				require.NoError(t, err)
				require.NotEmpty(t, resp.RawFinalTx)
			}
		})
	}
}

// TestRemoveTransaction checks deletion includes the requested root and errors.
func TestRemoveTransaction(t *testing.T) {
	t.Parallel()
	deleteErr := errors.New("delete failed")
	testCases := []struct {
		name string
		err  error
	}{
		{
			name: "requested root",
		},
		{
			name: "wallet deletion failure",
			err:  deleteErr,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange an unconfirmed root and require that exact
			// hash at the native deletion boundary; wallet owns
			// descendant traversal.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			hash := chainhash.Hash{1}
			w.On("GetTx", testifymock.Anything, hash).
				Return(&wallet.TxDetail{Hash: hash}, nil).Once()
			w.On("DeleteUnconfirmedTx", testifymock.Anything, hash).
				Return(tc.err).Once()
			server := &WalletKit{cfg: &Config{Wallet: w}}

			// Act on the public request rather than the native
			// graph algorithm.
			resp, err := server.RemoveTransaction(
				t.Context(),
				&GetTransactionRequest{
					Txid: hash.String(),
				},
			)

			// Assert a failure stays visible and cannot return a
			// success body.
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				require.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, resp.Status)
		})
	}
}

// TestFundPsbtLeaseOwner checks default native ownership and custom handoff.
func TestFundPsbtLeaseOwner(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name   string
		custom bool
	}{
		{
			name: "native owner retained",
		},
		{
			name:   "custom no expiry owner",
			custom: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange a confirmed input and the native funding
			// result. Strict mocks prohibit release/reacquisition
			// on the ordinary default path.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			op, nativeOwner := wire.OutPoint{
				Index: 1,
			}, wtxmgr.LockID{
				7,
			}
			pkScript := []byte{txscript.OP_TRUE}
			packet, err := psbt.New(
				[]*wire.OutPoint{&op},
				[]*wire.TxOut{
					{
						Value:    100,
						PkScript: pkScript,
					},
				},
				2,
				0,
				[]uint32{0},
			)
			require.NoError(t, err)
			var buf bytes.Buffer
			require.NoError(t, packet.Serialize(&buf))
			utxo := &wallet.Utxo{OutPoint: op, Confirmations: 1}
			w.On(
				"ListUnspent",
				testifymock.Anything,
				wallet.UtxoQuery{
					Account:  lnwallet.DefaultAccountName,
					MinConfs: 1,
					MaxConfs: defaultMaxConf,
				},
			).
				Return([]*wallet.Utxo{utxo}, nil).
				Once()
			w.On(
				"FundPsbt",
				testifymock.Anything,
				testifymock.AnythingOfType(
					"*wallet.FundIntent",
				),
			).Return(&wallet.FundResult{
				Packet:      packet,
				ChangeIndex: -1,
				LeaseID:     nativeOwner,
			}, nil).Once()
			owner := nativeOwner
			if tc.custom {
				owner = wtxmgr.LockID{9}
				w.On(
					"ReleaseOutput",
					testifymock.Anything,
					nativeOwner,
					op,
				).
					Return(nil).
					Once()
				w.On("GetUtxo", testifymock.Anything, op).
					Return(utxo, nil).
					Once()
				opts := testifymock.MatchedBy(
					func(opts []wallet.LeaseOption) bool {
						return len(opts) == 1
					},
				)
				w.On(
					"LeaseOutput",
					testifymock.Anything,
					owner,
					op,
					time.Duration(0),
					opts,
				).
					Return(time.Time{}, nil).
					Once()
			} else {
				w.On("ListLeasedOutputs", testifymock.Anything).
					Return([]*wallet.LeasedOutput{
						{
							LockID:   nativeOwner,
							OutPoint: op,
							Expiration: time.Unix(
								123,
								0,
							),
						},
					}, nil).Once()
			}
			w.On("GetTx", testifymock.Anything, op.Hash).
				Return(&wallet.TxDetail{
					Outputs: []wallet.Output{
						{
							Index:    int(op.Index),
							Amount:   200,
							PkScript: pkScript,
						},
					},
				}, nil).
				Once()
			cfg := &Config{Wallet: w}
			cfg.CoinSelectionLocker = &lnwallet.LightningWallet{}
			cfg.CoinSelectionStrategy = wallet.CoinSelectionLargest
			server := &WalletKit{cfg: cfg}
			req := &FundPsbtRequest{
				Template: &FundPsbtRequest_Psbt{
					Psbt: buf.Bytes(),
				},
				Fees: &FundPsbtRequest_SatPerVbyte{
					SatPerVbyte: 1,
				},
			}
			if tc.custom {
				req.CustomLockId = owner[:]
				req.InputLeaseNoExpiry = true
			}

			// Act through public funding so request normalization
			// participates.
			resp, err := server.FundPsbt(t.Context(), req)

			// Assert the response exposes the real owner and
			// effective lifetime.
			require.NoError(t, err)
			require.Len(t, resp.LockedUtxos, 1)
			require.Equal(t, owner[:], resp.LockedUtxos[0].Id)
			require.Equal(
				t,
				tc.custom,
				resp.LockedUtxos[0].NoExpiry,
			)
		})
	}
}

// TestFundPsbtRejectsAmbiguousAccount protects native scoped account names.
func TestFundPsbtRejectsAmbiguousAccount(t *testing.T) {
	t.Parallel()

	// Arrange two native scopes with the same custom name. No funding call
	// is expected because a name alone cannot select either spending
	// source.
	w := &mock.WalletController{}
	t.Cleanup(func() { w.AssertExpectations(t) })
	w.On("ListAccountsByName", t.Context(), "custom").
		Return([]wallet.AccountInfo{
			{
				AccountName: "custom",
				KeyScope:    waddrmgr.KeyScopeBIP0084,
			},
			{
				AccountName: "custom",
				KeyScope:    waddrmgr.KeyScopeBIP0086,
			},
		}, nil).
		Once()
	server := &WalletKit{cfg: &Config{
		Wallet: w, CoinSelectionStrategy: wallet.CoinSelectionLargest,
	},
	}

	// Act through funding's shared preflight, before either template mode.
	resp, err := server.FundPsbt(t.Context(), &FundPsbtRequest{
		Account: "custom",
		Fees:    &FundPsbtRequest_SatPerVbyte{SatPerVbyte: 1},
	})

	// Assert the ambiguity stays visible rather than selecting the first
	// scope.
	require.ErrorContains(t, err, "ambiguous")
	require.Nil(t, resp)
}

// TestListAccountsBIP44 checks that a fresh kvdb wallet's BIP44 account remains
// visible even though the RPC enum has no P2PKH address type.
func TestListAccountsBIP44(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		scope   waddrmgr.KeyScope
		wantErr bool
	}{
		{
			name:  "BIP44 metadata is visible",
			scope: waddrmgr.KeyScopeBIP0044,
		},
		{
			name: "unsupported scope remains an error",
			scope: waddrmgr.KeyScope{
				Purpose: 777,
				Coin:    0,
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange: Return a native account alongside a normal
			// payment account. Cleanup verifies mock expectations.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			zero := wallet.AccountNumber(0)
			paymentScope := waddrmgr.KeyScopeBIP0084
			w.On("ListAccounts", t.Context()).Return(
				[]wallet.AccountInfo{
					{
						AccountName:   "default",
						KeyScope:      tc.scope,
						AccountNumber: &zero,
						PublicKey: []byte(
							"stored account xpub",
						),
						ExternalKeyCount: 3,
					},
					{
						AccountName: "default",
						KeyScope:    paymentScope,
					},
				}, nil,
			).Once()
			server := &WalletKit{cfg: &Config{
				Wallet:      w,
				ChainParams: &chaincfg.MainNetParams,
			}}

			// Act: List all accounts through the public RPC without
			// filtering out metadata that Wallet actually returned.
			resp, err := server.ListAccounts(
				t.Context(), &ListAccountsRequest{},
			)

			// Assert: Both accounts retain their metadata, while
			// unsupported scope errors still reach the caller.
			if tc.wantErr {
				require.ErrorContains(
					t, err, "unsupported key scope",
				)
				require.Nil(t, resp)

				return
			}
			require.NoError(t, err)
			require.Len(t, resp.Accounts, 2)
			require.Equal(
				t, AddressType_UNKNOWN,
				resp.Accounts[0].AddressType,
			)
			require.Equal(
				t, "m/44'/0'/0'",
				resp.Accounts[0].DerivationPath,
			)
			require.Equal(
				t, "stored account xpub",
				resp.Accounts[0].ExtendedPublicKey,
			)
			require.EqualValues(
				t, 3, resp.Accounts[0].ExternalKeyCount,
			)
			require.Equal(t, AddressType_WITNESS_PUBKEY_HASH,
				resp.Accounts[1].AddressType)
		})
	}
}

// TestListAddressesNativeKeys preserves the custom-key listing using maintained
// public derivation, while payment accounts retain native address enumeration.
func TestListAddressesNativeKeys(t *testing.T) {
	t.Parallel()
	for _, showCustom := range []bool{false, true} {
		t.Run(fmt.Sprint(showCustom), func(t *testing.T) {
			// Arrange one payment account and one Lightning family
			// with an allocated key on each branch. Strict
			// selectors
			// prevent payment-scope lookup for the Lightning
			// family.
			w := &mock.WalletController{}
			t.Cleanup(func() { w.AssertExpectations(t) })
			_, pubKey := btcec.PrivKeyFromBytes([]byte{1})
			scope84 := waddrmgr.KeyScopeBIP0084
			schema := waddrmgr.ScopeAddrMap[scope84]
			keyBytes := pubKey.SerializeCompressed()
			keyHex := hex.EncodeToString(keyBytes)
			num := wallet.AccountNumber(123)
			scope := waddrmgr.KeyScope{
				Purpose: keychain.BIP0043Purpose,
				Coin:    chaincfg.MainNetParams.HDCoinType,
			}
			addr, err := address.NewAddressWitnessPubKeyHash(
				make([]byte, 20), &chaincfg.MainNetParams,
			)
			require.NoError(t, err)
			w.On("ListAccounts", t.Context()).Return(
				[]wallet.AccountInfo{
					{
						AccountName: "default",
						KeyScope:    scope84,
						AddrSchema:  schema,
					},
					{
						AccountName:      "act:123",
						KeyScope:         scope,
						AccountNumber:    &num,
						ExternalKeyCount: 1,
						InternalKeyCount: 1,
						NoChainSync:      true,
					},
				}, nil,
			).Once()
			w.On("ListAddresses", t.Context(), "default",
				waddrmgr.WitnessPubKey).
				Return([]wallet.AddressProperty{
					{
						Address: addr,
						Balance: 123,
					},
				}, nil).Once()
			w.On("GetAddressInfo", t.Context(), addr).
				Return(wallet.AddressInfo{
					PubKey: pubKey,
				}, nil).Once()
			if showCustom {
				selector := wallet.NewAccountSelectorByNumber(
					scope, num,
				)
				for branch := range uint32(2) {
					w.On("DerivePubKey", t.Context(),
						wallet.DerivePubKeyParams{
							Account: selector,
							Branch:  branch,
						}).Return(pubKey, nil).Once()
				}
			}
			server := &WalletKit{
				cfg: &Config{
					Wallet:      w,
					ChainParams: &chaincfg.MainNetParams,
				},
			}

			// Act through the public custom-account toggle, without
			// allocating children or accessing private signing
			// keys.
			resp, err := server.ListAddresses(t.Context(),
				&ListAddressesRequest{
					ShowCustomAccounts: showCustom,
				})

			// Assert payment balances pass through; custom children
			// retain hex keys and their actual native branch paths.
			require.NoError(t, err)
			count := 1
			if showCustom {
				count++
			}
			require.Len(t, resp.AccountWithAddresses, count)
			payment := resp.AccountWithAddresses[0].Addresses
			require.Len(t, payment, 1)
			require.Equal(t, addr.String(), payment[0].Address)
			require.EqualValues(t, 123, payment[0].Balance)
			if showCustom {
				keys := resp.AccountWithAddresses[1].Addresses
				require.Len(t, keys, 2)
				for branch, key := range keys {
					require.Equal(t, keyHex, key.Address)
					require.Equal(t,
						keyBytes, key.PublicKey,
					)
					require.Equal(t,
						branch == 1, key.IsInternal,
					)
					path := fmt.Sprintf(
						"%v/123'/%d/0", scope, branch,
					)
					require.Equal(t,
						path, key.DerivationPath,
					)
				}
			}
		})
	}
}

// TestListAccountsHybridEncoding verifies a generic native account key retains
// its BIP49 scope when exported for a later numberless account import.
func TestListAccountsHybridEncoding(t *testing.T) {
	t.Parallel()

	for _, params := range []*chaincfg.Params{
		&chaincfg.MainNetParams,
		&chaincfg.RegressionNetParams,
	} {
		t.Run(params.Name, func(t *testing.T) {
			t.Parallel()

			// Arrange a BIP49 key with SQL's generic HD version.
			// Export must preserve its known scope and key material
			// without declaring a numbered import identity.
			key, err := hdkeychain.NewMaster(
				bytes.Repeat([]byte{1}, 32), params,
			)
			require.NoError(t, err)
			for _, child := range []uint32{49, 0, 0} {
				key, err = key.Derive(
					child + hdkeychain.HardenedKeyStart,
				)
				require.NoError(t, err)
			}
			pub, err := key.Neuter()
			require.NoError(t, err)

			expectedKey, err := pub.ECPubKey()
			require.NoError(t, err)

			controller := &mock.WalletController{}
			defer controller.AssertExpectations(t)

			scope := waddrmgr.KeyScopeBIP0049Plus
			schema := waddrmgr.ScopeAddrMap[scope]
			encodedPub := []byte(pub.String())
			controller.On("ListAccounts", t.Context()).Return(
				[]wallet.AccountInfo{
					{
						AccountName: "default",
						KeyScope:    scope,
						AddrSchema:  schema,
						PublicKey:   encodedPub,
					},
				}, nil,
			).Once()
			kit := &WalletKit{
				cfg: &Config{
					Wallet:      controller,
					ChainParams: params,
				},
			}

			// Act through the public account export used for
			// imported funding and remote signing.
			resp, err := kit.ListAccounts(t.Context(),
				&ListAccountsRequest{},
			)

			// Assert encoding preserves BIP49 import selection
			// while the actual public key retains its native value.
			require.NoError(t, err)
			require.Len(t, resp.Accounts, 1)
			exported, err := hdkeychain.NewKeyFromString(
				resp.Accounts[0].ExtendedPublicKey,
			)
			require.NoError(t, err)
			version := waddrmgr.HDVersionMainNetBIP0049
			if params.Net != wire.MainNet {
				version = waddrmgr.HDVersionTestNetBIP0049
			}
			require.EqualValues(t, version,
				binary.BigEndian.Uint32(exported.Version()),
			)
			actualKey, err := exported.ECPubKey()
			require.NoError(t, err)
			require.Equal(t, expectedKey.SerializeCompressed(),
				actualKey.SerializeCompressed(),
			)
		})
	}
}

// TestImportAccountFingerprint preserves absent versus supplied-zero roots
// without binding a generic imported xpub to a numbered local account slot.
func TestImportAccountFingerprint(t *testing.T) {
	t.Parallel()

	// Arrange an account xpub shared by both metadata cases. Generic
	// imports retain their numberless native selection semantics.
	key, err := hdkeychain.NewMaster(bytes.Repeat([]byte{1}, 32),
		&chaincfg.RegressionNetParams)
	require.NoError(t, err)
	for _, child := range []uint32{49, 0, 0} {
		key, err = key.Derive(child + hdkeychain.HardenedKeyStart)
		require.NoError(t, err)
	}
	pub, err := key.Neuter()
	require.NoError(t, err)

	xpub := pub.String()
	params := chaincfg.RegressionNetParams
	walletType := waddrmgr.NestedWitnessPubKey

	for _, tc := range []struct {
		name        string
		fingerprint []byte
	}{
		{
			name: "omitted fingerprint",
		},
		{
			name:        "supplied zero fingerprint",
			fingerprint: []byte{0, 0, 0, 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange one native import and defer its expectation
			// check until the public RPC has completed.
			controller := &mock.WalletController{}
			defer controller.AssertExpectations(t)

			var fingerprint *wallet.MasterFingerprint
			if tc.fingerprint != nil {
				zero := wallet.MasterFingerprint(0)
				fingerprint = &zero
			}
			scope := waddrmgr.KeyScopeBIP0049Plus
			schema := waddrmgr.KeyScopeBIP0049AddrSchema
			controller.On("ImportAccount", t.Context(),
				testifymock.MatchedBy(func(
					p wallet.ImportAccountParams) bool {

					fp := p.MasterKeyFingerprint
					if (fp == nil) != (fingerprint == nil) {
						return false
					}
					if fp != nil && *fp != *fingerprint {
						return false
					}

					return p.Name == "external" &&
						p.XPub.String() == xpub &&
						p.Scope == nil &&
						p.AddrSchema == nil &&
						p.AddrType == walletType
				}), false,
			).Return(&wallet.AccountInfo{
				AccountName:          "external",
				KeyScope:             scope,
				AddrSchema:           schema,
				PublicKey:            []byte(pub.String()),
				MasterKeyFingerprint: fingerprint,
			}, nil).Once()
			kit := &WalletKit{
				cfg: &Config{
					Wallet:      controller,
					ChainParams: &params,
				},
			}

			// Act through the public RPC with the caller's optional
			// root fingerprint, including a valid supplied zero.
			addrType := AddressType_NESTED_WITNESS_PUBKEY_HASH
			resp, err := kit.ImportAccount(
				t.Context(), &ImportAccountRequest{
					Name:                 "external",
					ExtendedPublicKey:    pub.String(),
					AddressType:          addrType,
					MasterKeyFingerprint: tc.fingerprint,
				},
			)

			// Assert the response retains the optional fingerprint.
			// The mock also checks the native import parameters.
			require.NoError(t, err)
			require.Equal(t, "external", resp.Account.Name)
			require.Equal(t, tc.fingerprint,
				resp.Account.MasterKeyFingerprint)
		})
	}
}
