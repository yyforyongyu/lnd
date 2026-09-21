//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestMarshallLeasesIncludesSpendProgress verifies that ListLeases exposes
// each confirmation-controlled lease's persisted spend progress.
func TestMarshallLeasesIncludesSpendProgress(t *testing.T) {
	t.Parallel()

	// Arrange the real adapter because the maintained lease DTO cannot
	// represent confirmation-depth state in an in-memory fixture. Request
	// that state through WalletKit before attempting to inspect its
	// progress.
	controller := &btcwallet.BtcWallet{}
	rpcServer, _, err := New(&Config{
		Wallet:              controller,
		CoinSelectionLocker: &lnwallet.LightningWallet{},
	})
	require.NoError(t, err)

	// Act by acquiring a confirmation-controlled lease through the public
	// request. Unsupported storage must remain a visible failure here.
	_, err = rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: bytes.Repeat([]byte{1}, 32),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   make([]byte, 32),
			OutputIndex: 2,
		},
		ReleaseAfterSpendConfs: 6,
	})

	// Assert the existing success contract before reading persisted
	// progress; a missing lease capability cannot be replaced by synthetic
	// DTO fields.
	require.NoError(t, err)
	leases, err := controller.ListLeasedOutputs()
	require.NoError(t, err)
	rpcLocks := marshallLeases(leases)
	require.Len(t, rpcLocks, 1)
	require.Equal(t, uint32(6), rpcLocks[0].ReleaseAfterSpendConfs)
	require.Zero(t, rpcLocks[0].ConfirmedSpendHeight)
}

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

type mockCoinSelectionLocker struct {
	fail bool
}

// renewalWallet records lease reads and mutations at the RPC boundary.
// Unarranged operations fail instead of simulating unavailable depth state.
type renewalWallet struct {
	lnwallet.WalletController
	testifymock.Mock
}

// ListLeasedOutputs returns the arranged persisted lease view.
func (w *renewalWallet) ListLeasedOutputs() (
	[]*wallet.ListLeasedOutputResult, error) {

	args := w.Called()
	leases, _ := args.Get(0).([]*wallet.ListLeasedOutputResult)

	return leases, args.Error(1)
}

// LeaseOutput records that a request reached the wallet mutation boundary.
func (w *renewalWallet) LeaseOutput(id wtxmgr.LockID, op wire.OutPoint,
	duration time.Duration) (time.Time, error) {

	args := w.Called(id, op, duration)
	expiration, _ := args.Get(0).(time.Time)

	return expiration, args.Error(1)
}

// WithCoinSelectLock runs the callback and optionally returns a test error.
func (m *mockCoinSelectionLocker) WithCoinSelectLock(f func() error) error {
	if err := f(); err != nil {
		return err
	}

	if m.fail {
		return fmt.Errorf("kek")
	}

	return nil
}

// TestLeaseOutputRejectsUnsupportedOptions verifies WalletKit does not
// silently downgrade an option-bearing request to a time-only lease.
func TestLeaseOutputRejectsUnsupportedOptions(t *testing.T) {
	t.Parallel()

	wallet := &legacyLeaseWallet{
		WalletController: &mock.WalletController{},
	}
	rpcServer, _, err := New(&Config{
		Wallet: &lnwallet.LightningWallet{
			WalletController: wallet,
		},
		CoinSelectionLocker: &mockCoinSelectionLocker{},
	})
	require.NoError(t, err)

	_, err = rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: bytes.Repeat([]byte{1}, 32),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   make([]byte, 32),
			OutputIndex: 1,
		},
		ExpirationSeconds:      60,
		ReleaseAfterSpendConfs: 6,
	})
	require.ErrorIs(t, err, errOutputLeaseOptionsUnsupported)
	require.Zero(t, wallet.leaseCalls,
		"unsupported options must not create a shorter legacy lease")
}

// TestLeaseOutputReturnsEffectiveRenewalDepth verifies that renewing an
// existing confirmation-controlled lease through the legacy zero-depth path
// reports the non-zero depth retained by the wallet.
func TestLeaseOutputReturnsEffectiveRenewalDepth(t *testing.T) {
	t.Parallel()

	lockID := wtxmgr.LockID{1}
	outpoint := wire.OutPoint{Index: 1}
	// Arrange the adapter's actual capability boundary. Installing a lease
	// through the public operation avoids inventing metadata absent from
	// btcwallet's maintained result type.
	controller := &btcwallet.BtcWallet{}
	_, err := controller.LeaseOutputWithOptions(
		lockID, outpoint, time.Minute, lnwallet.LeaseOutputOptions{
			ReleaseAfterSpendConfs: 6,
		},
	)
	require.NoError(t, err)
	rpcServer, _, err := New(&Config{
		Wallet:              controller,
		CoinSelectionLocker: &lnwallet.LightningWallet{},
	})
	require.NoError(t, err)

	// Act by renewing with zero depth, which must retain the stored policy.
	resp, err := rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   make([]byte, 32),
			OutputIndex: outpoint.Index,
		},
		ExpirationSeconds: 60,
	})
	// Assert the persisted depth survives renewal rather than being reset.
	require.NoError(t, err)
	require.Equal(t, uint32(6), resp.ReleaseAfterSpendConfs)
}

// TestLeaseOutputDepthReadback permits timed leases only when their public
// depth response is known, rejecting ambiguous legacy renewals before mutation.
func TestLeaseOutputDepthReadback(t *testing.T) {
	t.Parallel()

	// Arrange a same-owner lease without inventing depth fields absent from
	// the maintained result. Cases distinguish known native-SQL policy,
	// fresh legacy creation and an existing or unreadable legacy policy.
	lockID := wtxmgr.LockID{1}
	outpoint := wire.OutPoint{Index: 1}
	expiration := time.Unix(123, 0)
	persisted := []*wallet.ListLeasedOutputResult{
		{
			LockedOutput: &wtxmgr.LockedOutput{
				LockID:   lockID,
				Outpoint: outpoint,
			},
		},
	}
	testCases := []struct {
		name      string
		nativeSQL bool
		existing  bool
		listErr   error
		wantErr   string
	}{
		{
			name:      "native SQL timed lease",
			nativeSQL: true,
		},
		{
			name: "fresh legacy timed lease",
		},
		{
			name:     "ambiguous legacy renewal",
			existing: true,
			wantErr:  "metadata is unavailable",
		},
		{
			name:    "legacy read failure",
			listErr: errors.New("list leases failed"),
			wantErr: "list leases failed",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange permitted reads and mutations. Rejected
			// requests have no mutation expectation.
			controller := &renewalWallet{}
			if !tc.nativeSQL {
				var leases []*wallet.ListLeasedOutputResult
				if tc.existing {
					leases = persisted
				}
				controller.On("ListLeasedOutputs").
					Return(leases, tc.listErr).Once()
			}
			if tc.wantErr == "" {
				controller.On("LeaseOutput", lockID, outpoint,
					time.Minute,
				).Return(expiration, nil).Once()
			}
			coinLocker := &lnwallet.LightningWallet{}
			rpcServer, _, err := New(&Config{
				Wallet:              controller,
				NativeSQLWallet:     tc.nativeSQL,
				CoinSelectionLocker: coinLocker,
			})
			require.NoError(t, err)

			// Act through the same zero-depth RPC used for creation
			// and renewal, under the real coin-selection lock.
			resp, err := rpcServer.LeaseOutput(
				t.Context(), &LeaseOutputRequest{
					Id: lockID[:],
					Outpoint: &lnrpc.OutPoint{
						TxidBytes:   outpoint.Hash[:],
						OutputIndex: outpoint.Index,
					},
					ExpirationSeconds: 60,
				},
			)

			// Assert success reports timed policy and expiration.
			// Failed reads must yield no response or mutation.
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, resp)
			} else {
				require.NoError(t, err)
				require.Equal(t, uint64(123), resp.Expiration)
				require.Zero(t, resp.ReleaseAfterSpendConfs)
			}
			controller.AssertExpectations(t)
		})
	}
}

// TestListLeases confines unavailable legacy depth reporting to its RPC,
// while retaining empty legacy lists and native-SQL timed lease results.
func TestListLeases(t *testing.T) {
	t.Parallel()

	// Arrange the same maintained lease view for native and legacy storage.
	// Only the RPC knows whether its zero depth is established by storage.
	lease := &wallet.ListLeasedOutputResult{
		LockedOutput: &wtxmgr.LockedOutput{},
		Value:        12345,
	}
	testCases := []struct {
		name      string
		nativeSQL bool
		empty     bool
		wantErr   bool
	}{
		{
			name:    "nonempty legacy",
			wantErr: true,
		},
		{
			name:  "empty legacy",
			empty: true,
		},
		{
			name:      "native SQL",
			nativeSQL: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange a lease read without mutation expectations.
			leases := []*wallet.ListLeasedOutputResult{lease}
			if tc.empty {
				leases = nil
			}
			controller := &renewalWallet{}
			controller.On("ListLeasedOutputs").Return(leases, nil).
				Once()
			rpcServer, _, err := New(&Config{
				Wallet:          controller,
				NativeSQLWallet: tc.nativeSQL,
			})
			require.NoError(t, err)

			// Act through the depth-reporting RPC. The underlying
			// lease values and reservations must remain intact.
			resp, err := rpcServer.ListLeases(
				t.Context(), &ListLeasesRequest{},
			)

			// Assert ambiguous legacy depth fails; supported
			// results retain the timed amount and zero depth.
			if tc.wantErr {
				require.ErrorContains(
					t, err, "lease spend-depth",
				)
				require.Nil(t, resp)
			} else {
				require.NoError(t, err)
				require.Len(t, resp.LockedUtxos, len(leases))
				if !tc.empty {
					rpcLease := resp.LockedUtxos[0]
					require.Equal(
						t, uint64(12345),
						rpcLease.Value,
					)
					require.Zero(
						t,
						rpcLease.ReleaseAfterSpendConfs,
					)
				}
			}
			controller.AssertExpectations(t)
		})
	}
}

// TestFundPsbtRequiresCustomLockID verifies confirmation-controlled input
// leases require an external, caller-specific owner ID.
func TestFundPsbtRequiresCustomLockID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		lockID   []byte
		expected string
	}{
		{
			name: "missing",
			expected: "custom lock ID required for " +
				"confirmation-controlled",
		},
		{
			name:     "all zero",
			lockID:   make([]byte, 32),
			expected: "custom lock ID must not be all zeros",
		},
		{
			name:     "reserved internal",
			lockID:   chanfunding.LndInternalLockID[:],
			expected: "reserved custom lock ID cannot be used",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			server := &WalletKit{cfg: &Config{}}
			req := &FundPsbtRequest{
				InputReleaseAfterSpendConfs: 6,
			}
			req.CustomLockId = testCase.lockID

			_, err := server.FundPsbt(t.Context(), req)
			require.ErrorContains(t, err, testCase.expected)
		})
	}
}

// TestFundPsbtCoinSelect tests that the coin selection for a PSBT template
// works as expected.
func TestFundPsbtCoinSelect(t *testing.T) {
	t.Parallel()

	const fundAmt = 50_000
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
		utxos       []*lnwallet.Utxo
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
	}{{
		name:  "no utxos",
		utxos: []*lnwallet.Utxo{},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:     -1,
		feeRate:         chainfee.FeePerKwFloor,
		maxFeeRatio:     chanfunding.DefaultMaxFeeRatio,
		expectedErrType: &chanfunding.ErrInsufficientFunds{},
	}, {
		name: "1 p2wpkh utxo, add p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 1, 1, 0),
	}, {
		name: "1 p2wpkh utxo, add p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 2, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, exact amount",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 123,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, p2wpkh change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
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
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2wkhDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, no change, p2tr change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
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
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2trDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, existing p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, dust change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + calcFee(0, 1, 0, 1, 0) + 50,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, existing " +
			"p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
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
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(2, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, add p2tr " +
			"change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
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
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(2, 1, 2, 0, 0),
	}, {
		name: "large existing p2tr input, fee estimation p2wpkh " +
			"change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 3,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: 1,
		expectedChangeOutputAmount: fundAmt*3 - fundAmt -
			calcFee(1, 0, 1, 1, 0),
		expectedFee: calcFee(1, 0, 1, 1, 0),
	}, {
		name:  "large existing p2tr input, fee estimation no change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value: fundAmt +
						int64(calcFee(1, 0, 1, 0, 0)),
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(1, 0, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, invalid fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "fee 0.00000111 BTC exceeds max fee " +
			"(0.00000027 BTC) on total output value",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, negative feeratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio * (-1),
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "maxFeeRatio must be between 0.00 " +
			"and 1.00 got -0.20",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, big fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
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
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "large existing p2tr input, fee estimation existing " +
			"change output",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 2,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:                0,
		feeRate:                    chainfee.FeePerKwFloor,
		maxFeeRatio:                chanfunding.DefaultMaxFeeRatio,
		changeType:                 chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:        []int{},
		expectChangeOutputIndex:    0,
		expectedChangeOutputAmount: fundAmt*2 - calcFee(1, 0, 1, 0, 0),
		expectedFee:                calcFee(1, 0, 1, 0, 0),
	}}

	for _, tc := range testCases {

		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		walletMock := &mock.WalletController{
			RootKey: privKey,
			Utxos:   tc.utxos,
		}
		rpcServer, _, err := New(&Config{
			Wallet:                walletMock,
			CoinSelectionLocker:   &mockCoinSelectionLocker{},
			CoinSelectionStrategy: wallet.CoinSelectionLargest,
		})
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

			resp, err := rpcServer.fundPsbtCoinSelect(
				"", tc.changeIndex, copiedPacket, 0,
				tc.changeType, tc.feeRate,
				rpcServer.cfg.CoinSelectionStrategy,
				tc.maxFeeRatio, nil, 0, 0,
			)

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
