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
