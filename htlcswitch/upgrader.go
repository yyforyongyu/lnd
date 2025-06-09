package htlcswitch

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

type Upgrader interface {
	Start()
	Stop()
	InitDyn(r dynReq, l *channelLink)
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
	wg sync.WaitGroup

	// log is a link-specific logging instance.
	log btclog.Logger

	upgradeReqs chan dynReq

	quit chan struct{}

	status upgraderStatus

	// TODO: we should create a link manager, or a link machine on top of
	// the existing link, then `DynUpgrader` is parallel to channelLink, and
	// will be managed by the link machine.
	link *channelLink
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

	d.wg.Add(1)
	go d.mainLoop()
}

func (d *DynUpgrader) Stop() {
	d.status = upgraderStatusStopped

	close(d.quit)
	d.wg.Wait()
}

func (d *DynUpgrader) InitDyn(r dynReq, l *channelLink) {
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

	// TODO: redesign.
	d.link = l

	// Send the request to the internal loop.
	fn.SendOrQuit(d.upgradeReqs, r, d.quit)
}

func (d *DynUpgrader) mainLoop() {
	defer d.wg.Done()

	for {
		select {
		case <-d.quit:
			d.log.Debugf("DynUpgrader shutting down...")

		case req := <-d.upgradeReqs:
			d.handleUpgradeReq(req)
		}
	}
}

func (d *DynUpgrader) handleUpgradeReq(req dynReq) {
	params := req.Request

	// Construct the TLV to be sent.
	msg := d.dynReqToWireMsg(req)

	// TODO: Create the signature.

	var err error
	// Depending on whether we want to upgrade the channel type or not, we
	// will go to the dedicated handlers.
	if params.ChanType.IsSome() {
		err = d.handleChannelTypeUpgrade(msg)
	} else {
		err = d.handleChannelParamsUpgrade(msg)
	}

	if err != nil {
		d.log.Errorf("%v", err)

		resp := UpgradeLinkResponse{
			Status: UpdateLinkStatusFailed,
			Err:    err,
		}
		req.Resolve(resp)

		return
	}

	// Notify the caller this is now pending.
	resp := UpgradeLinkResponse{
		Status: UpdateLinkStatusPending,
	}
	req.Resolve(resp)
}

// TODO: rename to sendDynPropose?
func (d *DynUpgrader) handleChannelParamsUpgrade(msg lnwire.DynPropose) error {
	d.log.Debugf("Starting to upgrade channel params")

	// Send the propose msg.
	err := d.link.cfg.Peer.SendMessage(true, &msg)
	if err != nil {
		return fmt.Errorf("failed to send DynPropose: %w", err)
	}

	// Wait for the peer's reply, either accept or reject.

	return nil
}

func (d *DynUpgrader) handleChannelTypeUpgrade(msg lnwire.DynPropose) error {
	return nil
}

func (d *DynUpgrader) dynReqToWireMsg(req dynReq) lnwire.DynPropose {
	chanID := req.Request.ChanID
	params := req.Request

	// Make a copy of the current channel config.
	cfg := d.link.channel.State().LocalChanCfg

	// Figure out which field is changed and overwrite.
	//
	// TODO: Check if the new value is sane?
	// TODO: add better loggings.
	msg := lnwire.DynPropose{
		ChanID: chanID,
	}

	params.DustLimit.WhenSome(func(a btcutil.Amount) {
		var r tlv.RecordT[tlv.TlvType0, btcutil.Amount]
		r.Val = a
		msg.DustLimit = tlv.SomeRecordT(r)
	})

	params.MaxPendingAmount.WhenSome(func(m lnwire.MilliSatoshi) {
		var r tlv.RecordT[tlv.TlvType2, lnwire.MilliSatoshi]
		r.Val = m
		msg.MaxValueInFlight = tlv.SomeRecordT(r)
	})

	params.ChanReserve.WhenSome(func(a btcutil.Amount) {
		var r tlv.RecordT[tlv.TlvType6, btcutil.Amount]
		r.Val = a
		msg.ChannelReserve = tlv.SomeRecordT(r)
	})

	params.MinHTLC.WhenSome(func(m lnwire.MilliSatoshi) {
		var r tlv.RecordT[tlv.TlvType4, lnwire.MilliSatoshi]
		r.Val = m
		msg.HtlcMinimum = tlv.SomeRecordT(r)
	})

	params.CsvDelay.WhenSome(func(v uint16) {
		var r tlv.RecordT[tlv.TlvType8, uint16]
		r.Val = v
		msg.CsvDelay = tlv.SomeRecordT(r)
	})

	params.MaxAcceptedHtlcs.WhenSome(func(v uint16) {
		var r tlv.RecordT[tlv.TlvType10, uint16]
		r.Val = v
		msg.MaxAcceptedHTLCs = tlv.SomeRecordT(r)
	})

	params.ChanType.WhenSome(func(ct lnwire.ChannelType) {
		var r tlv.RecordT[tlv.TlvType12, lnwire.ChannelType]
		r.Val = ct
		msg.ChannelType = tlv.SomeRecordT(r)
	})

	d.log.Infof("Old cfg %v, new cfg %v", cfg, params)

	return msg
}
