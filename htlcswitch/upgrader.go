package htlcswitch

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

type Upgrader interface {
	Start()
	Stop()
	InitDyn(r *dynReq, l *channelLink)
	ReceiveMsg(msg lnwire.DynMsg)
	Active() bool
}

// TODO: use structure status with code and msg instead, this is ridiculas.
type upgraderStatus uint8

const (
	upgraderStatusCreated upgraderStatus = iota

	upgraderStatusReady
	upgraderStatusBusy
	upgraderStatusLocalProposed
	upgraderStatusLocalWaiting
	upgraderStatusRemoteProposed
	upgraderStatusLocalReplied
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

	case upgraderStatusLocalProposed:
		return "local proposed"

	case upgraderStatusLocalWaiting:
		return "local waiting"

	case upgraderStatusRemoteProposed:
		return "remote proposed"

	case upgraderStatusLocalReplied:
		return "local replied"

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

	upgradeReqs chan *dynReq

	quit chan struct{}

	status upgraderStatus

	// TODO: we should create a link manager, or a link machine on top of
	// the existing link, then `DynUpgrader` is parallel to channelLink, and
	// will be managed by the link machine.
	link *channelLink
	req  *dynReq

	receiveMsgChan chan lnwire.DynMsg

	propose lnwire.DynPropose
}

// Compile-time check to ensure DynUpgrader satisfies the Upgrader interface.
var _ Upgrader = (*DynUpgrader)(nil)

func NewDynUpgrader(logger btclog.Logger) *DynUpgrader {
	du := &DynUpgrader{
		log:            logger,
		quit:           make(chan struct{}),
		status:         upgraderStatusCreated,
		receiveMsgChan: make(chan lnwire.DynMsg, 1),
		upgradeReqs:    make(chan *dynReq),
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

func (d *DynUpgrader) InitDyn(r *dynReq, l *channelLink) {
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
	d.req = r

	// Send the request to the internal loop.
	fn.SendOrQuit(d.upgradeReqs, r, d.quit)
}

func (d *DynUpgrader) ReceiveMsg(msg lnwire.DynMsg) {
	fn.SendOrQuit(d.receiveMsgChan, msg, d.quit)
}

func (d *DynUpgrader) Active() bool {
	return d.status >= upgraderStatusBusy
}

func (d *DynUpgrader) mainLoop() {
	defer d.wg.Done()

	for {
		select {
		case <-d.quit:
			d.log.Debugf("DynUpgrader shutting down...")
			return

		case req := <-d.upgradeReqs:
			// TODO: rename to local init.
			d.handleUpgradeReq(req)

		case msg := <-d.receiveMsgChan:
			d.log.Debugf("Received msg %v", msg.MsgType())

			// TODO: rename to remote init.
			d.handlePeerMsg(msg)
		}
	}
}

func (d *DynUpgrader) handlePeerMsg(msg lnwire.DynMsg) {
	switch m := msg.(type) {
	case *lnwire.DynPropose:
		d.handleRemoteUpgrade(m)
		return

	case *lnwire.DynAck, *lnwire.DynReject:
		d.handleRemoteReply(m)
	}
}

func (d *DynUpgrader) handleRemoteReply(msg lnwire.DynMsg) {
	if d.status != upgraderStatusLocalProposed {
		// Unexpected state, Disconnect.
		return
	}

	switch m := msg.(type) {
	// Maybe the remote can send a commit sig instead to signal accept?
	case *lnwire.DynAck:
		d.handleRemoteAccept(m)

	case *lnwire.DynReject:
		d.handleRemoteReject(m)

	case *lnwire.RevokeAndAck:
		d.handleRemoteCommit(m)
	}
}

// Accept flow:
// 1. we propose
// 2. they send an ACK, which has the next commit_sig.
// 3. we send our revoke_and_ack and commit_sig.
// 4. they send their revoke_and_ack.
//
// received DynAck, which has their commit_sig.
// send our revoke_and_ack and commit_sig.
// now expect their revoke_and_ack.
func (d *DynUpgrader) handleRemoteAccept(msg *lnwire.DynAck) {
	// Validate the proposed params are the same and signed.
	//
	// Perform a round of commitment dance <- unnecessary if csv is not
	// changed?
	//
	auxSigBlob, err := msg.CustomRecords.Serialize()
	if err != nil {
		d.link.failf(
			LinkFailureError{code: ErrInvalidCommitment},
			"unable to serialize custom records: %v", err,
		)

		return
	}

	err = d.link.channel.ReceiveNewCommitment(&lnwallet.CommitSigs{
		CommitSig:  msg.CommitSig.CommitSig,
		HtlcSigs:   msg.HtlcSigs,
		PartialSig: msg.PartialSig,
		AuxSigBlob: auxSigBlob,
	})
	if err != nil {
		// If we were unable to reconstruct their proposed commitment,
		// then we'll examine the type of error. If it's an
		// InvalidCommitSigError, then we'll send a direct error.
		var sendData []byte
		switch err.(type) {
		case *lnwallet.InvalidCommitSigError:
			sendData = []byte(err.Error())
		case *lnwallet.InvalidHtlcSigError:
			sendData = []byte(err.Error())
		}
		d.link.failf(
			LinkFailureError{
				code:          ErrInvalidCommitment,
				FailureAction: LinkFailureForceClose,
				SendData:      sendData,
			},
			"ChannelPoint(%v): unable to accept new "+
				"commitment: %v",
			d.link.channel.ChannelPoint(), err,
		)
		return
	}

	// Create a new commitment based on the upgraded params.
	// As we've just accepted a new state, we'll now
	// immediately send the remote peer a revocation for our prior
	// state.
	nextRevocation, _, _, err :=
		d.link.channel.RevokeCurrentCommitment()
	if err != nil {
		d.link.log.Errorf("unable to revoke commitment: %v", err)

		// We need to fail the channel in case revoking our
		// local commitment does not succeed. We might have
		// already advanced our channel state which would lead
		// us to proceed with an unclean state.
		//
		// NOTE: We do not trigger a force close because this
		// could resolve itself in case our db was just busy
		// not accepting new transactions.
		d.link.failf(
			LinkFailureError{
				code:          ErrInternalError,
				Warning:       true,
				FailureAction: LinkFailureDisconnect,
			},
			"ChannelPoint(%v): unable to accept new "+
				"commitment: %v",
			d.link.channel.ChannelPoint(), err,
		)
		return
	}

	// As soon as we are ready to send our next revocation, we can
	// invoke the incoming commit hooks.
	d.link.RWMutex.Lock()
	d.link.incomingCommitHooks.invoke()
	d.link.Unlock()

	d.link.cfg.Peer.SendMessage(false, nextRevocation)

	// If the remote party initiated the state transition,
	// we'll reply with a signature to provide them with their
	// version of the latest commitment. Otherwise, both commitment
	// chains are fully synced from our PoV, then we don't need to
	// reply with a signature as both sides already have a
	// commitment with the latest accepted.
	if d.link.channel.OweCommitment() {
		if !d.link.updateCommitTxOrFail(context.TODO()) {
			return
		}
	}

	// The remote has agreed on the upgrade, we now need to wait for their
	// revoke_and_ack before persisting the changes in db and update the
	// link and channel structs.
}

func (d *DynUpgrader) handleRemoteCommit(msg *lnwire.RevokeAndAck) {
	// We now process the message and advance our remote commit
	// chain.
	_, _, err := d.link.channel.ReceiveRevocation(msg)
	if err != nil {
		// TODO(halseth): force close?
		d.link.failf(
			LinkFailureError{
				code:          ErrInvalidRevocation,
				FailureAction: LinkFailureDisconnect,
			},
			"unable to accept revocation: %v", err,
		)
		return
	}

	// If we have a tower client for this channel type, we'll
	// create a backup for the current state.
	if d.link.cfg.TowerClient != nil {
		state := d.link.channel.State()
		chanID := d.link.ChanID()

		err = d.link.cfg.TowerClient.BackupState(
			&chanID, state.RemoteCommitment.CommitHeight-1,
		)
		if err != nil {
			d.link.failf(LinkFailureError{
				code: ErrInternalError,
			}, "unable to queue breach backup: %v", err)
			return
		}
	}

	if d.link.channel.OweCommitment() {
		log.Errorf("-----------------> we should not owe!!!!")
	}

	log.Debugf("Updating local config using propose %v",
		lnutils.SpewLogClosure(d.propose))

	// Since the remote has committed this change, we can now persist the
	// new params in db and update the link configs.
	d.link.channel.State().UpdateLocalConfig(&d.propose)

	// Notify the caller this is now pending.
	resp := UpgradeLinkResponse{
		Status: UpdateLinkStatusSucceeded,
	}
	d.req.Resolve(resp)
}

func (d *DynUpgrader) handleRemoteReject(msg lnwire.DynMsg) {

}

func (d *DynUpgrader) handleRemoteUpgrade(msg *lnwire.DynPropose) {
	d.log.Debugf("handling remote upgrade request %v", msg)

	if !d.status.canAcceptReq() {
		d.log.Errorf("Rejected remote upgradde due to status(%v) not "+
			"allowed", d.status)

		// Disconnect? or Noop?
		return
	}

	result := d.link.quiescer.QuiescenceInitiator()
	initiator, err := result.Unpack()
	if err != nil {
		d.log.Errorf("%v", err)

		// Disconnect? or Noop?
		return
	}

	if initiator.IsLocal() {
		d.log.Errorf("Remote is not the initiator, cannot start the " +
			"dynamic commitment")

		// Disconnect? or Noop?
		return
	}

	d.log.Debugf("Upgrader status: %v => %v", d.status,
		upgraderStatusRemoteProposed)

	d.status = upgraderStatusRemoteProposed

	// TODO: validate the params, then accept or reject.

	d.acceptRemoteProposal(msg)

}

// Accept remote's propose flow:
// 1. they propose by sending DynPropose.
// 2. we accept by sending an DynAck.
// 3. they send their revoke_and_ack and commit_sig.
// 4. we send our revoke_and_ack.
func (d *DynUpgrader) acceptRemoteProposal(m *lnwire.DynPropose) {
}

func (d *DynUpgrader) rejectRemoteProposal(m *lnwire.DynPropose) {

}

func (d *DynUpgrader) handleUpgradeReq(req *dynReq) {
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

		d.status = upgraderStatusReady

		return
	}

	// Notify the caller this is now pending.
	resp := UpgradeLinkResponse{
		Status: UpdateLinkStatusPending,
	}
	req.Resolve(resp)

	d.status = upgraderStatusLocalProposed
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

	d.propose = msg

	return nil
}

func (d *DynUpgrader) handleChannelTypeUpgrade(msg lnwire.DynPropose) error {
	return nil
}

func (d *DynUpgrader) dynReqToWireMsg(req *dynReq) lnwire.DynPropose {
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
