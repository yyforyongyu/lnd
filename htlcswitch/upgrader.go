package htlcswitch

import (
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
)

type Upgrader interface {
	Start()
	Stop()
	InitDyn(r dynReq)
}

type upgraderStatus uint8

const (
	upgraderStatusCreated upgraderStatus = iota

	upgraderStatusReady
	upgraderStatusBusy
	upgraderStatusStopped
)

func (u upgraderStatus) String() string {
	switch u {
	case upgraderStatusCreated:
		return "created"

	case upgraderStatusReady:
		return "ready"

	case upgraderStatusBusy:
		return "busy"

	case upgraderStatusStopped:
		return "stopped"

	default:
		return "unknown"
	}
}

func (u upgraderStatus) canAcceptReq() bool {
	if u == upgraderStatusReady {
		return true
	}

	return false
}

type DynUpgrader struct {
	// log is a link-specific logging instance.
	log btclog.Logger

	upgradeReqs chan dynReq

	quit chan struct{}

	status upgraderStatus
}

// Compile-time check to ensure DynUpgrader satisfies the Upgrader interface.
var _ Upgrader = (*DynUpgrader)(nil)

func NewDynUpgrader(logger btclog.Logger) *DynUpgrader {
	du := &DynUpgrader{
		log:    logger,
		quit:   make(chan struct{}),
		status: upgraderStatusCreated,
	}

	return du
}

func (d *DynUpgrader) Start() {
	d.status = upgraderStatusReady
}

func (d *DynUpgrader) Stop() {
	d.status = upgraderStatusStopped
}

func (d *DynUpgrader) InitDyn(r dynReq) {
	d.log.Debugf("Received dyn req %v", r)

	if !d.status.canAcceptReq() {
		err := fmt.Errorf("upgrade now allow due to status(%v)",
			d.status)
		resp := UpgradeLinkResponse{
			Status: UpdateLinkStatusFailed,
			Err:    err,
		}

		// TODO(yy): this resolve doesn't block on sending, and there's
		// no guarantee that the receiver will receive this resp if
		// a previous resp is not consumed by the receiver, need to use
		// a more customized req with lager buffer channels.
		r.Resolve(resp)
	}

	// Mark the machine as busy.
	d.status = upgraderStatusBusy

	// Send the request to the internal loop.
	fn.SendOrQuit(d.upgradeReqs, r, d.quit)
}
