package invoices

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// finalTestDB is a minimal InvoiceDB implementation for pending-settle
// finalization tests.
type finalTestDB struct {
	hash              lntypes.Hash
	invoice           Invoice
	updateTime        time.Time
	updateErr         error
	updateRef         InvoiceRef
	updateSetID       *SetID
	updateSignal      chan struct{}
	fetchPendingCalls int
}

// AddInvoice satisfies the InvoiceDB interface. These tests do not add
// invoices through the test DB.
func (f *finalTestDB) AddInvoice(context.Context, *Invoice,
	lntypes.Hash) (uint64, error) {

	return 0, nil
}

// InvoicesAddedSince satisfies the InvoiceDB interface. These tests do not
// scan added invoice events.
func (f *finalTestDB) InvoicesAddedSince(context.Context,
	uint64) ([]Invoice, error) {

	return nil, nil
}

// LookupInvoice returns the test invoice.
func (f *finalTestDB) LookupInvoice(context.Context, InvoiceRef) (Invoice,
	error) {

	return f.invoice, nil
}

// FetchPendingInvoices returns the test invoice and records the scan count.
func (f *finalTestDB) FetchPendingInvoices(context.Context) (
	map[lntypes.Hash]Invoice, error) {

	f.fetchPendingCalls++

	return map[lntypes.Hash]Invoice{
		f.hash: f.invoice,
	}, nil
}

// QueryInvoices satisfies the InvoiceDB interface. These tests do not query
// arbitrary invoice ranges.
func (f *finalTestDB) QueryInvoices(context.Context,
	InvoiceQuery) (InvoiceSlice, error) {

	return InvoiceSlice{}, nil
}

// UpdateInvoice applies the callback to the in-memory test invoice.
func (f *finalTestDB) UpdateInvoice(_ context.Context, ref InvoiceRef,
	setID *SetID, callback InvoiceUpdateCallback) (*Invoice, error) {

	if f.updateErr != nil {
		return nil, f.updateErr
	}
	f.updateRef = ref
	f.updateSetID = setID

	updater := finalTestUpdater{}
	invoice, err := UpdateInvoice(
		&f.hash, &f.invoice, f.updateTime, callback, updater,
	)
	if err != nil {
		return nil, err
	}

	f.invoice = *invoice
	if f.updateSignal != nil {
		select {
		case f.updateSignal <- struct{}{}:
		default:
		}
	}

	return &f.invoice, nil
}

// InvoicesSettledSince satisfies the InvoiceDB interface. These tests do not
// scan settled invoice events.
func (f *finalTestDB) InvoicesSettledSince(context.Context,
	uint64) ([]Invoice, error) {

	return nil, nil
}

// DeleteInvoice satisfies the InvoiceDB interface. These tests do not delete
// invoices.
func (f *finalTestDB) DeleteInvoice(context.Context,
	[]InvoiceDeleteRef) error {

	return nil
}

// DeleteCanceledInvoices satisfies the InvoiceDB interface. These tests do not
// garbage collect canceled invoices.
func (f *finalTestDB) DeleteCanceledInvoices(context.Context) error {
	return nil
}

// newPendingSettleTestInvoice creates a non-AMP invoice with one pending-settle
// HTLC.
func newPendingSettleTestInvoice() (lntypes.Hash, CircuitKey, Invoice) {
	preimage := lntypes.Preimage{1, 2, 3}
	hash := preimage.Hash()
	circuitKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 2,
	}

	invoice := Invoice{
		Terms: ContractTerm{
			PaymentPreimage: &preimage,
			Features:        lnwire.EmptyFeatureVector(),
			Value:           1000,
		},
		State:   ContractPendingSettle,
		AmtPaid: 1000,
		Htlcs: map[CircuitKey]*InvoiceHTLC{
			circuitKey: {
				Amt:   1000,
				State: HtlcStatePendingSettle,
			},
		},
	}

	return hash, circuitKey, invoice
}

// newPendingSettleAMPTestInvoice creates an AMP invoice with one
// pending-settle HTLC.
func newPendingSettleAMPTestInvoice() (lntypes.Hash, [32]byte, SetID,
	CircuitKey, Invoice) {

	preimage := lntypes.Preimage{4, 5, 6}
	hash := preimage.Hash()
	payAddr := [32]byte{7, 8, 9}
	setID := SetID{10, 11, 12}
	ampRecord := record.NewAMP(
		[32]byte{13, 14, 15}, [32]byte(setID), 3,
	)
	circuitKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 2,
	}
	features := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.AMPRequired),
		lnwire.Features,
	)

	invoice := Invoice{
		Terms: ContractTerm{
			PaymentAddr: payAddr,
			Features:    features,
		},
		State:   ContractOpen,
		AmtPaid: 1000,
		AMPState: AMPInvoiceState{
			setID: {
				State: HtlcStatePendingSettle,
				InvoiceKeys: map[CircuitKey]struct{}{
					circuitKey: {},
				},
				AmtPaid: 1000,
			},
		},
		Htlcs: map[CircuitKey]*InvoiceHTLC{
			circuitKey: {
				Amt:   1000,
				State: HtlcStatePendingSettle,
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
		},
	}

	return hash, payAddr, setID, circuitKey, invoice
}

// registerPendingSettleTestInvoice indexes a test invoice's pending-settle
// HTLCs in the registry.
func registerPendingSettleTestInvoice(registry *InvoiceRegistry,
	hash lntypes.Hash, invoice *Invoice) {

	registry.Lock()
	registry.recordPendingSettleRefsLocked(
		hash, InvoiceRefByHash(hash), invoice,
	)
	registry.Unlock()
}

// startRegistryEventLoop starts the registry event loop for retry tests.
func startRegistryEventLoop(t *testing.T, registry *InvoiceRegistry) {
	t.Helper()

	registry.started.Store(true)
	registry.wg.Add(1)
	go registry.invoiceEventLoop()

	t.Cleanup(func() {
		close(registry.quit)
		registry.wg.Wait()
	})
}

func assertNoInvoiceEvent(t *testing.T, registry *InvoiceRegistry) {
	t.Helper()

	select {
	case event := <-registry.invoiceEvents:
		t.Fatalf("unexpected invoice event: %v", event)

	default:
	}
}

// TestShouldCancelPendingSettle asserts pending-settle invoices cannot be
// canceled by external cancellation paths.
func TestShouldCancelPendingSettle(t *testing.T) {
	t.Parallel()

	require.False(t, shouldCancel(&Invoice{
		State: ContractPendingSettle,
	}, false))
	require.False(t, shouldCancel(&Invoice{
		State: ContractPendingSettle,
	}, true))

	_, _, _, _, ampInvoice := newPendingSettleAMPTestInvoice()
	require.False(t, shouldCancel(&ampInvoice, true))
}

// TestInvoiceEventSuppressesAMPNonSettled verifies all-invoice subscribers do
// not see AMP set updates before final settlement.
func TestInvoiceEventSuppressesAMPNonSettled(t *testing.T) {
	t.Parallel()

	_, _, setID, _, invoice := newPendingSettleAMPTestInvoice()
	event := &invoiceEvent{
		invoice: &invoice,
		setID:   (*[32]byte)(&setID),
	}
	require.True(t, event.suppressedForAllClients())

	ampState := invoice.AMPState[setID]
	ampState.State = HtlcStateCanceled
	invoice.AMPState[setID] = ampState
	require.True(t, event.suppressedForAllClients())

	ampState.State = HtlcStateSettled
	ampState.SettleIndex = 1
	invoice.AMPState[setID] = ampState
	require.False(t, event.suppressedForAllClients())
}

// TestNotifyExitHopHtlcFinalizedSettle finalizes a pending-settle HTLC as
// settled and verifies the invoice reaches its terminal settled state.
func TestNotifyExitHopHtlcFinalizedSettle(t *testing.T) {
	t.Parallel()

	hash, circuitKey, invoice := newPendingSettleTestInvoice()
	db := &finalTestDB{
		hash:       hash,
		invoice:    invoice,
		updateTime: time.Now(),
	}
	registry := NewRegistry(db, nil, &RegistryConfig{})
	registerPendingSettleTestInvoice(registry, hash, &invoice)

	err := registry.NotifyExitHopHtlcFinalized(
		t.Context(), circuitKey, true,
	)
	require.NoError(t, err)
	require.Equal(t, ContractSettled, db.invoice.State)
	require.Equal(
		t, HtlcStateSettled, db.invoice.Htlcs[circuitKey].State,
	)
	require.Zero(t, db.fetchPendingCalls)
}

// TestNotifyExitHopHtlcFinalizedFail finalizes a pending-settle HTLC as failed
// and verifies the invoice is canceled.
func TestNotifyExitHopHtlcFinalizedFail(t *testing.T) {
	t.Parallel()

	hash, circuitKey, invoice := newPendingSettleTestInvoice()
	db := &finalTestDB{
		hash:       hash,
		invoice:    invoice,
		updateTime: time.Now(),
	}
	registry := NewRegistry(db, nil, &RegistryConfig{})
	registerPendingSettleTestInvoice(registry, hash, &invoice)

	err := registry.NotifyExitHopHtlcFinalized(
		t.Context(), circuitKey, false,
	)
	require.NoError(t, err)
	require.Equal(t, ContractCanceled, db.invoice.State)
	require.Equal(
		t, HtlcStateCanceled, db.invoice.Htlcs[circuitKey].State,
	)
	require.Zero(t, db.fetchPendingCalls)
}

// TestNotifyExitHopHtlcFinalizedSettleWaitsForSibling verifies a settled HTLC
// does not finalize the invoice while another HTLC in the same set is still
// pending finality.
func TestNotifyExitHopHtlcFinalizedSettleWaitsForSibling(t *testing.T) {
	t.Parallel()

	hash, circuitKey, invoice := newPendingSettleTestInvoice()
	secondKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 3,
	}
	invoice.Htlcs[secondKey] = &InvoiceHTLC{
		Amt:   2000,
		State: HtlcStatePendingSettle,
	}
	invoice.AmtPaid += 2000
	db := &finalTestDB{
		hash:       hash,
		invoice:    invoice,
		updateTime: time.Now(),
	}
	registry := NewRegistry(db, nil, &RegistryConfig{})
	registerPendingSettleTestInvoice(registry, hash, &invoice)

	err := registry.NotifyExitHopHtlcFinalized(
		t.Context(), circuitKey, true,
	)
	require.NoError(t, err)
	require.Equal(t, ContractPendingSettle, db.invoice.State)
	require.Equal(
		t, HtlcStateSettled, db.invoice.Htlcs[circuitKey].State,
	)
	require.Equal(
		t, HtlcStatePendingSettle, db.invoice.Htlcs[secondKey].State,
	)
	assertNoInvoiceEvent(t, registry)

	registry.Lock()
	_, firstPending := registry.pendingSettleRefs[circuitKey]
	_, secondPending := registry.pendingSettleRefs[secondKey]
	registry.Unlock()
	require.False(t, firstPending)
	require.True(t, secondPending)

	err = registry.NotifyExitHopHtlcFinalized(
		t.Context(), secondKey, true,
	)
	require.NoError(t, err)
	require.Equal(t, ContractSettled, db.invoice.State)
	require.Equal(
		t, HtlcStateSettled, db.invoice.Htlcs[secondKey].State,
	)
	require.Equal(t, lnwire.MilliSatoshi(3000), db.invoice.AmtPaid)
}

// TestNotifyExitHopHtlcFinalizedFailWaitsForSibling verifies a failed HTLC does
// not cancel sibling pending-settle HTLCs, and a later settled sibling can
// still settle the invoice if it satisfies the invoice amount.
func TestNotifyExitHopHtlcFinalizedFailWaitsForSibling(t *testing.T) {
	t.Parallel()

	hash, circuitKey, invoice := newPendingSettleTestInvoice()
	secondKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 3,
	}
	invoice.Htlcs[secondKey] = &InvoiceHTLC{
		Amt:   2000,
		State: HtlcStatePendingSettle,
	}
	invoice.AmtPaid += 2000
	db := &finalTestDB{
		hash:       hash,
		invoice:    invoice,
		updateTime: time.Now(),
	}
	registry := NewRegistry(db, nil, &RegistryConfig{})
	registerPendingSettleTestInvoice(registry, hash, &invoice)

	err := registry.NotifyExitHopHtlcFinalized(
		t.Context(), circuitKey, false,
	)
	require.NoError(t, err)
	require.Equal(t, ContractPendingSettle, db.invoice.State)
	require.Equal(
		t, HtlcStateCanceled, db.invoice.Htlcs[circuitKey].State,
	)
	require.Equal(
		t, HtlcStatePendingSettle, db.invoice.Htlcs[secondKey].State,
	)
	assertNoInvoiceEvent(t, registry)

	err = registry.NotifyExitHopHtlcFinalized(
		t.Context(), secondKey, true,
	)
	require.NoError(t, err)
	require.Equal(t, ContractSettled, db.invoice.State)
	require.Equal(
		t, HtlcStateSettled, db.invoice.Htlcs[secondKey].State,
	)
	require.Equal(t, lnwire.MilliSatoshi(2000), db.invoice.AmtPaid)
}

// TestScanInvoicesOnStartIndexesPendingSettle verifies startup scans rebuild
// the pending-settle circuit index.
func TestScanInvoicesOnStartIndexesPendingSettle(t *testing.T) {
	t.Parallel()

	hash, circuitKey, invoice := newPendingSettleTestInvoice()
	db := &finalTestDB{
		hash:       hash,
		invoice:    invoice,
		updateTime: time.Now(),
	}
	registry := NewRegistry(db, nil, &RegistryConfig{})

	err := registry.scanInvoicesOnStart(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, db.fetchPendingCalls)

	registry.Lock()
	_, ok := registry.pendingSettleRefs[circuitKey]
	registry.Unlock()
	require.True(t, ok)

	err = registry.NotifyExitHopHtlcFinalized(
		t.Context(), circuitKey, true,
	)
	require.NoError(t, err)
	require.Equal(t, ContractSettled, db.invoice.State)
}
