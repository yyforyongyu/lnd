//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
)

const (
	defaultMaxConf = math.MaxInt32
)

// verifyInputsUnspent checks that all inputs are contained in the list of
// known, non-locked UTXOs given.
func verifyInputsUnspent(inputs []*wire.TxIn, utxos []*base.Utxo) error {
	// TODO(guggero): Pass in UTXOs as a map to make lookup more efficient.
	for idx, txIn := range inputs {
		found := false
		for _, u := range utxos {
			if u.OutPoint == txIn.PreviousOutPoint {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("input %d not found in list of non-"+
				"locked UTXO", idx)
		}
	}

	return nil
}

// lockInputs requests lock leases for all inputs specified in a PSBT packet
// (the passed outpoints), using either the optional custom lock ID and duration
// or the wallet's internal static lock ID with the default 10-minute duration.
func lockInputs(ctx context.Context, w base.UtxoManager,
	outpoints []wire.OutPoint,
	customLockID *wtxmgr.LockID, customLockDuration time.Duration,
	releaseAfterSpendConfs uint32) (
	[]*base.LeasedOutput, error) {

	locks := make(
		[]*base.LeasedOutput, 0, len(outpoints),
	)

	for idx := range outpoints {
		lock := &base.LeasedOutput{OutPoint: outpoints[idx]}

		lock.LockID = chanfunding.LndInternalLockID
		if customLockID != nil {
			lock.LockID = *customLockID
		}

		lockDuration := chanfunding.DefaultLockDuration
		if customLockDuration != 0 {
			lockDuration = customLockDuration
		}

		// Get the details about this outpoint.
		_, err := w.GetUtxo(ctx, lock.OutPoint)
		if err != nil {
			cause := fmt.Errorf("fetch outpoint info: %w", err)

			return nil, rollbackInputLeases(ctx, w, locks, cause)
		}

		// Native NoExpiry is explicit and requires zero duration. Timed
		// requests keep the same caller-selected owner/default
		// lifetime.
		var opts []base.LeaseOption
		if releaseAfterSpendConfs > 0 {
			opts = append(opts, base.WithNoExpiry())
			lockDuration = 0
			lock.NoExpiry = true
		}
		expiration, err := w.LeaseOutput(ctx, lock.LockID,
			lock.OutPoint, lockDuration, opts...)

		if err != nil {
			cause := fmt.Errorf("could not lease UTXO: %w", err)

			return nil, rollbackInputLeases(ctx, w, locks, cause)
		}

		lock.Expiration = expiration
		lock.NoExpiry = expiration.IsZero()
		locks = append(locks, lock)
	}

	return locks, nil
}

// rollbackInputLeases releases every lease acquired before cause interrupted
// the current FundPsbt attempt. If a release fails, the returned error includes
// its outpoint and owner ID so the caller can identify and recover the lease.
func rollbackInputLeases(ctx context.Context, w base.UtxoManager,
	locks []*base.LeasedOutput, cause error) error {

	var releaseErrs []error
	for _, lock := range locks {
		err := w.ReleaseOutput(ctx, lock.LockID, lock.OutPoint)
		if err == nil {
			continue
		}

		releaseErr := fmt.Errorf(
			"could not release lease %v with lock ID %x: %w",
			lock.OutPoint, lock.LockID[:], err,
		)
		log.Errorf("%v", releaseErr)
		releaseErrs = append(releaseErrs, releaseErr)
	}

	if len(releaseErrs) == 0 {
		return cause
	}

	return errors.Join(append([]error{cause}, releaseErrs...)...)
}
