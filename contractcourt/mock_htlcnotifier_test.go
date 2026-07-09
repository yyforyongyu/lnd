package contractcourt

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
)

type mockHTLCNotifier struct {
	HtlcNotifier

	// finalHtlcEvents records final HTLC events for assertions.
	finalHtlcEvents    []channeldb.FinalHtlcInfo
	finalHtlcEventKeys []models.CircuitKey
}

func (m *mockHTLCNotifier) NotifyFinalHtlcEvent(key models.CircuitKey,
	info channeldb.FinalHtlcInfo) {

	m.finalHtlcEventKeys = append(m.finalHtlcEventKeys, key)
	m.finalHtlcEvents = append(m.finalHtlcEvents, info)
}
