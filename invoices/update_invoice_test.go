package invoices

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

type updateHTLCTest struct {
	name     string
	input    InvoiceHTLC
	invState ContractState
	setID    *[32]byte
	output   InvoiceHTLC
	expErr   error
}

// TestUpdateHTLC asserts the behavior of the updateHTLC method in various
// scenarios for MPP and AMP.
func TestUpdateHTLC(t *testing.T) {
	t.Parallel()

	testNow := time.Now()
	setID := [32]byte{0x01}
	ampRecord := record.NewAMP([32]byte{0x02}, setID, 3)
	preimage := lntypes.Preimage{0x04}
	hash := preimage.Hash()

	diffSetID := [32]byte{0x05}
	fakePreimage := lntypes.Preimage{0x06}
	testAlreadyNow := time.Now()

	tests := []updateHTLCTest{
		{
			name: "MPP accept",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			invState: ContractAccepted,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			expErr: nil,
		},
		{
			name: "MPP accept, copy custom records",
			input: InvoiceHTLC{
				Amt:          5000,
				MppTotalAmt:  5000,
				AcceptHeight: 100,
				AcceptTime:   testNow,
				ResolveTime:  time.Time{},
				Expiry:       40,
				State:        HtlcStateAccepted,
				CustomRecords: record.CustomSet{
					0x01:   []byte{0x02},
					0xffff: []byte{0x04, 0x05, 0x06},
				},
				WireCustomRecords: lnwire.CustomRecords{
					0x010101: []byte{0x02, 0x03},
					0xffffff: []byte{0x44, 0x55, 0x66},
				},
				AMP: nil,
			},
			invState: ContractAccepted,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:          5000,
				MppTotalAmt:  5000,
				AcceptHeight: 100,
				AcceptTime:   testNow,
				ResolveTime:  time.Time{},
				Expiry:       40,
				State:        HtlcStateAccepted,
				CustomRecords: record.CustomSet{
					0x01:   []byte{0x02},
					0xffff: []byte{0x04, 0x05, 0x06},
				},
				WireCustomRecords: lnwire.CustomRecords{
					0x010101: []byte{0x02, 0x03},
					0xffffff: []byte{0x44, 0x55, 0x66},
				},
				AMP: nil,
			},
			expErr: nil,
		},
		{
			name: "MPP settle",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			invState: ContractSettled,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			expErr: nil,
		},
		{
			name: "MPP cancel",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			invState: ContractCanceled,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			expErr: nil,
		},
		{
			name: "AMP accept missing preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			expErr: ErrHTLCPreimageMissing,
		},
		{
			name: "AMP accept invalid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			expErr: ErrHTLCPreimageMismatch,
		},
		{
			name: "AMP accept valid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "AMP accept valid preimage different htlc set",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &diffSetID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "AMP settle missing preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			expErr: ErrHTLCPreimageMissing,
		},
		{
			name: "AMP settle invalid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			expErr: ErrHTLCPreimageMismatch,
		},
		{
			name: "AMP settle valid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "AMP pending settle valid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractPendingSettle,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStatePendingSettle,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "AMP finalize pending settle",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStatePendingSettle,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			// With the newer AMP logic, this is now valid, as we
			// want to be able to accept multiple settle attempts
			// to a given pay_addr. In this case, the HTLC should
			// remain in the accepted state.
			name: "AMP settle valid preimage different htlc set",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &diffSetID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "accept invoice htlc already settled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: ErrHTLCAlreadySettled,
		},
		{
			name: "cancel invoice htlc already settled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractCanceled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: ErrHTLCAlreadySettled,
		},
		{
			name: "settle invoice htlc already settled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "cancel invoice",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractCanceled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "accept invoice htlc already canceled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "cancel invoice htlc already canceled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractCanceled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "settle invoice htlc already canceled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testUpdateHTLC(t, test, testNow)
		})
	}
}

func TestUpdateInvoiceAMPPendingSettleState(t *testing.T) {
	t.Parallel()

	setID := SetID{1, 2, 3}
	preimage := lntypes.Preimage{4, 5, 6}
	hash := preimage.Hash()
	ampRecord := record.NewAMP([32]byte{7, 8, 9}, [32]byte(setID), 4)
	circuitKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 2,
	}

	invoice := &Invoice{
		Terms: ContractTerm{
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(lnwire.AMPRequired),
				lnwire.Features,
			),
		},
		State:   ContractOpen,
		AmtPaid: 1000,
		AMPState: AMPInvoiceState{
			setID: {
				State: HtlcStateAccepted,
				InvoiceKeys: map[CircuitKey]struct{}{
					circuitKey: {},
				},
				AmtPaid: 1000,
			},
		},
		Htlcs: map[CircuitKey]*InvoiceHTLC{
			circuitKey: {
				Amt:   1000,
				State: HtlcStateAccepted,
				AMP: &InvoiceHtlcAMPData{
					Record: *ampRecord,
					Hash:   hash,
				},
			},
		},
	}
	updater := &ampStateTestUpdater{}
	updateTime := time.Now()

	updatedInvoice, err := UpdateInvoice(
		&hash, invoice, updateTime, func(*Invoice) (*InvoiceUpdateDesc,
			error) {

			return &InvoiceUpdateDesc{
				UpdateType: AddHTLCsUpdate,
				State: &InvoiceStateUpdateDesc{
					NewState: ContractPendingSettle,
					HTLCPreimages: map[CircuitKey]lntypes.Preimage{
						circuitKey: preimage,
					},
					SetID: (*[32]byte)(&setID),
				},
			}, nil
		}, updater,
	)
	require.NoError(t, err)

	require.Equal(
		t, HtlcStatePendingSettle,
		updatedInvoice.Htlcs[circuitKey].State,
	)
	require.Equal(
		t, HtlcStatePendingSettle, updatedInvoice.AMPState[setID].State,
	)
	require.Equal(t, lnwire.MilliSatoshi(1000), updatedInvoice.AmtPaid)
	require.Equal(t, lnwire.MilliSatoshi(1000), updater.ampState.AmtPaid)
	require.Equal(t, HtlcStatePendingSettle, updater.ampState.State)
}

// finalTestUpdater is a no-op InvoiceUpdater for tests that exercise in-memory
// invoice transitions.
type finalTestUpdater struct{}

// AddHtlc satisfies InvoiceUpdater. The in-memory invoice is updated directly.
func (f finalTestUpdater) AddHtlc(CircuitKey, *InvoiceHTLC) error {
	return nil
}

// ResolveHtlc satisfies InvoiceUpdater. The in-memory invoice is updated
// directly.
func (f finalTestUpdater) ResolveHtlc(CircuitKey, HtlcState,
	time.Time) error {

	return nil
}

// AddAmpHtlcPreimage satisfies InvoiceUpdater. The in-memory invoice is
// updated directly.
func (f finalTestUpdater) AddAmpHtlcPreimage([32]byte, CircuitKey,
	lntypes.Preimage) error {

	return nil
}

// UpdateInvoiceState satisfies InvoiceUpdater. The in-memory invoice is updated
// directly.
func (f finalTestUpdater) UpdateInvoiceState(ContractState,
	*lntypes.Preimage) error {

	return nil
}

// UpdateInvoiceAmtPaid satisfies InvoiceUpdater. The in-memory invoice is
// updated directly.
func (f finalTestUpdater) UpdateInvoiceAmtPaid(lnwire.MilliSatoshi) error {
	return nil
}

// UpdateAmpState satisfies InvoiceUpdater. The in-memory invoice is updated
// directly.
func (f finalTestUpdater) UpdateAmpState([32]byte, InvoiceStateAMP,
	CircuitKey) error {

	return nil
}

// Finalize satisfies InvoiceUpdater. No flush is needed for the in-memory test
// updater.
func (f finalTestUpdater) Finalize(UpdateType) error {
	return nil
}

// ampStateTestUpdater records the latest AMP state update for assertions.
type ampStateTestUpdater struct {
	finalTestUpdater

	ampState InvoiceStateAMP
}

func (u *ampStateTestUpdater) UpdateAmpState(_ [32]byte,
	newState InvoiceStateAMP, _ CircuitKey) error {

	u.ampState = newState
	return nil
}

func testUpdateHTLC(t *testing.T, test updateHTLCTest, now time.Time) {
	htlc := test.input.Copy()
	stateChanged, state, err := getUpdatedHtlcState(
		htlc, test.invState, test.setID,
	)
	if stateChanged {
		htlc.State = state
		htlc.ResolveTime = now
	}

	require.Equal(t, test.expErr, err)
	require.Equal(t, test.output, *htlc)
}
