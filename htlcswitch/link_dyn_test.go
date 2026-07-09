package htlcswitch

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/dyn"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// dynTestDust is a dust limit that satisfies the dynamic-commitments validation
// bounds. The default test channel opens with dust limits below the dyn minimum
// (354 sat), so the dyn helpers bump both sides to this value before enabling
// the feature so proposals validate against the whole resulting channel.
const dynTestDust = btcutil.Amount(500)

// enableLinkDyn turns on the dynamic-commitments feature for the given link
// before it is started. It injects a concrete dyn_ack signer built over the
// provided node keys, rebuilds the quiescer with a non-zero timeout, mounts the
// dyn.Updater, and bumps both dust limits into the dyn-valid range so that
// whole-channel proposal validation can pass.
//
// It must be called before the link's htlcManager is started.
func enableLinkDyn(t *testing.T, link *channelLink,
	nodePriv *btcec.PrivateKey, peerPub *btcec.PublicKey) {

	t.Helper()

	link.cfg.DynAckSigner = newDynAckSigner(nodePriv, peerPub)
	link.cfg.MaxLocalCSVDelay = defaultMaxLinkCSVDelay
	link.cfg.QuiescenceTimeout = time.Hour

	// Rebuild the quiescer so it honors the non-zero timeout we just set
	// (it was built with the zero-value timeout inside NewChannelLink).
	link.quiescer = NewQuiescer(QuiescerCfg{
		chanID: lnwire.NewChanIDFromOutPoint(
			link.channel.ChannelPoint(),
		),
		channelInitiator: link.channel.Initiator(),
		sendMsg: func(s lnwire.Stfu) error {
			return link.cfg.Peer.SendMessage(false, &s)
		},
		timeoutDuration: link.cfg.QuiescenceTimeout,
		onTimeout: func() {
			link.cfg.Peer.Disconnect(ErrQuiescenceTimeout)
		},
	})

	// Bump both dust limits into the dyn-valid range. Only validation reads
	// these values in these tests (the dance is not executed), so mutating
	// the in-memory config is sufficient and safe here.
	st := link.channel.State()
	st.LocalChanCfg.DustLimit = dynTestDust
	st.RemoteChanCfg.DustLimit = dynTestDust

	link.updater = link.newDynUpdater()
}

// recvLinkMsg reads the next message the link sent to its peer, failing
// the test on timeout.
func recvLinkMsg(t *testing.T, msgs chan lnwire.Message) lnwire.Message {
	t.Helper()

	select {
	case m := <-msgs:
		return m

	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for link message")

		return nil
	}
}

// newDynLinkHarness spins up a single-link harness with the dynamic-commitments
// feature enabled on Alice's link, returning the started harness, the concrete
// link, and the channel Alice sends messages on.
func newDynLinkHarness(t *testing.T) (singleLinkTestHarness, *channelLink,
	chan lnwire.Message) {

	t.Helper()

	const (
		chanAmt     = btcutil.SatoshiPerBitcoin * 5
		chanReserve = btcutil.SatoshiPerBitcoin * 1
	)

	harness, err := newSingleLinkTestHarness(t, chanAmt, chanReserve)
	require.NoError(t, err)

	//nolint:forcetypeassert
	coreLink := harness.aliceLink.(*channelLink)

	aliceNodePriv, _ := btcec.PrivKeyFromBytes(alicePrivKey)
	_, bobNodePub := btcec.PrivKeyFromBytes(bobPrivKey)
	enableLinkDyn(t, coreLink, aliceNodePriv, bobNodePub)

	require.NoError(t, harness.start())
	t.Cleanup(func() { harness.aliceLink.Stop() })

	//nolint:forcetypeassert
	aliceMsgs := coreLink.cfg.Peer.(*mockPeer).sentMsgs

	return harness, coreLink, aliceMsgs
}

// quiesceAsResponder drives the link into quiescence with the peer as the
// quiescence initiator, returning once the link has sent its own Stfu reply.
func quiesceAsResponder(t *testing.T, link *channelLink,
	msgs chan lnwire.Message) {

	t.Helper()

	link.HandleChannelUpdate(&lnwire.Stfu{
		ChanID:    link.ChanID(),
		Initiator: true,
	})

	msg := recvLinkMsg(t, msgs)
	stfu, ok := msg.(*lnwire.Stfu)
	require.Truef(t, ok, "expected Stfu, got %T", msg)
	require.False(t, stfu.Initiator, "we are not the quiescence initiator")

	require.Eventually(t, link.quiescer.IsQuiescent, 5*time.Second,
		10*time.Millisecond)
}

// TestDynAckSignerRoundTrip verifies the concrete dyn.Signer adapter signs with
// our node key and verifies against the peer's node key, and that a tampered
// payload fails verification.
func TestDynAckSignerRoundTrip(t *testing.T) {
	t.Parallel()

	alicePriv, alicePub := btcec.PrivKeyFromBytes([]byte("alice node key"))
	bobPriv, bobPub := btcec.PrivKeyFromBytes([]byte("bob node key"))

	// Alice signs with her key and encodes the peer (Bob) as verifier.
	aliceSigner := newDynAckSigner(alicePriv, bobPub)

	// Bob's adapter verifies signatures Alice made (peerPub == alicePub).
	bobSigner := newDynAckSigner(bobPriv, alicePub)

	var (
		chanID    lnwire.ChannelID
		chainHash chainhash.Hash
	)
	tlvs := []byte{0x01, 0x02, 0x03}
	height := uint64(7)

	sig, err := aliceSigner.SignDynAck(chainHash, chanID, height, tlvs)
	require.NoError(t, err)

	require.NoError(t, bobSigner.VerifyDynAck(
		sig, chainHash, chanID, height, tlvs,
	))

	// A tampered payload must fail verification.
	require.Error(t, bobSigner.VerifyDynAck(
		sig, chainHash, chanID, height, []byte{0x09},
	))

	// The wrong height must fail verification.
	require.Error(t, bobSigner.VerifyDynAck(
		sig, chainHash, chanID, height+1, tlvs,
	))
}

// TestDynChannelViewPredicates checks the concrete dyn.ChannelView adapter
// reports the channel state the state machine relies on.
func TestDynChannelViewPredicates(t *testing.T) {
	t.Parallel()

	_, coreLink, msgs := newDynLinkHarness(t)

	view := &dynChannelView{link: coreLink}

	require.Equal(t, coreLink.ChanID(), view.ChanID())
	require.Equal(t, coreLink.channel.State().ChainHash, view.ChainHash())
	require.Equal(t, coreLink.channel.State().Capacity, view.Capacity())
	require.Equal(t, dynTestDust, view.LocalChanCfg().DustLimit)
	require.Equal(t, dynTestDust, view.RemoteChanCfg().DustLimit)

	// A freshly opened, clean channel has no HTLCs and no pending updates,
	// and both peers are ready (once the link has finished reestablishing).
	require.Eventually(t, view.BothChannelReady, 5*time.Second,
		10*time.Millisecond)
	require.False(t, view.HasHTLCs())
	require.False(t, view.HasPendingUpdates())
	require.False(t, view.HasPendingShutdown())
	require.False(t, view.HasPendingSpliceOrRBF())

	// The next commitment height is one past the current local height.
	nextHeight := coreLink.channel.State().LocalCommitment.CommitHeight + 1
	require.Equal(t, nextHeight, view.NextCommitHeight())

	// Not quiescent yet; once we drive quiescence the view reflects it.
	require.False(t, view.IsQuiescent())
	quiesceAsResponder(t, coreLink, msgs)
	require.True(t, view.IsQuiescent())

	initiator, err := view.QuiescenceInitiator().Unpack()
	require.NoError(t, err)
	require.Equal(t, lntypes.Remote, initiator)
}

// TestChannelLinkDynResponderAccept exercises the responder happy path: after
// quiescence the peer's dyn_propose is routed to the mounted updater, which
// validates it and replies with a signed dyn_ack.
func TestChannelLinkDynResponderAccept(t *testing.T) {
	t.Parallel()

	_, coreLink, msgs := newDynLinkHarness(t)
	quiesceAsResponder(t, coreLink, msgs)

	chanID := coreLink.ChanID()

	// The peer (proposer) proposes a new max_accepted_htlcs. Proposer-owned
	// semantics apply the change to our (the counterparty's) config.
	params := lnwallet.ChannelParams{
		MaxAcceptedHtlcs: fn.Some(uint16(20)),
	}
	dp := params.ToDynPropose(chanID)
	coreLink.HandleChannelUpdate(dp)

	// We should reply with a dyn_ack carrying a valid signature over the
	// proposal we accepted.
	msg := recvLinkMsg(t, msgs)
	ack, ok := msg.(*lnwire.DynAck)
	require.Truef(t, ok, "expected DynAck, got %T", msg)

	tlvs, err := dp.SerializeTlvData()
	require.NoError(t, err)

	_, alicePub := btcec.PrivKeyFromBytes(alicePrivKey)
	height := coreLink.channel.State().LocalCommitment.CommitHeight + 1
	require.NoError(t, lnwire.VerifyDynAck(
		alicePub, ack.Sig, coreLink.channel.State().ChainHash, chanID,
		height, tlvs,
	))

	require.Equal(t, dyn.StateAckSent, coreLink.updater.State())
}

// TestChannelLinkDynResponderReject exercises the responder unhappy path: a
// dyn_propose that fails validation is rejected with the offending parameter
// bit set.
func TestChannelLinkDynResponderReject(t *testing.T) {
	t.Parallel()

	_, coreLink, msgs := newDynLinkHarness(t)
	quiesceAsResponder(t, coreLink, msgs)

	chanID := coreLink.ChanID()

	// Propose a to_self_delay well above our configured maximum.
	params := lnwallet.ChannelParams{
		CsvDelay: fn.Some(uint16(5000)),
	}
	dp := params.ToDynPropose(chanID)
	coreLink.HandleChannelUpdate(dp)

	msg := recvLinkMsg(t, msgs)
	reject, ok := msg.(*lnwire.DynReject)
	require.Truef(t, ok, "expected DynReject, got %T", msg)
	require.True(t, reject.IsRejected(lnwire.DynRejectCsvDelay))

	require.Eventually(t, func() bool {
		return coreLink.updater.State() == dyn.StateRejected
	}, 5*time.Second, 10*time.Millisecond)
}

// TestChannelLinkDynResponderCommit drives the responder happy path end to end:
// the peer proposes, our link acks, the peer sends the bundled dyn_commit_sig,
// and our link runs the full commitment dance (revoke_and_ack + a normal
// commitment_signed) with the peer replying with its final revoke_and_ack. Once
// both chains lock in, the agreed params are applied to our config, both commit
// heights advance, and quiescence is exited.
func TestChannelLinkDynResponderCommit(t *testing.T) {
	t.Parallel()

	harness, coreLink, msgs := newDynLinkHarness(t)
	bob := harness.bobChannel
	quiesceAsResponder(t, coreLink, msgs)

	chanID := coreLink.ChanID()

	// The peer (Bob) is the proposer, so a max_accepted_htlcs change
	// constrains our (the counterparty's) local config.
	params := lnwallet.ChannelParams{
		MaxAcceptedHtlcs: fn.Some(uint16(20)),
	}
	require.NotEqual(t, uint16(20),
		coreLink.channel.State().LocalChanCfg.MaxAcceptedHtlcs)

	aliceHeightBefore := coreLink.channel.State().LocalCommitment.CommitHeight
	bobHeightBefore := bob.State().LocalCommitment.CommitHeight

	dp := params.ToDynPropose(chanID)
	coreLink.HandleChannelUpdate(dp)

	// Our link acks the proposal.
	msg := recvLinkMsg(t, msgs)
	ack, ok := msg.(*lnwire.DynAck)
	require.Truef(t, ok, "expected DynAck, got %T", msg)

	// Bob (the proposer) stages the agreed update into his own log and signs
	// our next commitment, producing the bundled dyn_commit_sig.
	_, err := bob.AddDynUpdate(params)
	require.NoError(t, err, "bob unable to stage dyn update")
	bobCommit, err := bob.SignNextCommitment(t.Context())
	require.NoError(t, err, "bob unable to sign dyn commitment")

	dc := &lnwire.DynCommit{
		DynPropose: *dp,
		CommitSig:  bobCommit.CommitSig,
		AckSig:     ack.Sig,
	}
	coreLink.HandleChannelUpdate(dc)

	// Our link validates the bundled sig, revokes, and replies with its own
	// revoke_and_ack followed by a normal commitment_signed for the
	// proposer's next commitment.
	aliceRev, ok := recvLinkMsg(t, msgs).(*lnwire.RevokeAndAck)
	require.True(t, ok, "expected RevokeAndAck from link")
	aliceSig, ok := recvLinkMsg(t, msgs).(*lnwire.CommitSig)
	require.True(t, ok, "expected CommitSig from link")

	// Bob processes our reply and sends his final revoke_and_ack.
	_, _, err = bob.ReceiveRevocation(aliceRev)
	require.NoError(t, err, "bob unable to receive revocation")
	err = bob.ReceiveNewCommitment(&lnwallet.CommitSigs{
		CommitSig: aliceSig.CommitSig,
	})
	require.NoError(t, err, "bob unable to receive new commitment")
	bobRev, _, _, err := bob.RevokeCurrentCommitment()
	require.NoError(t, err, "bob unable to revoke commitment")

	coreLink.HandleChannelUpdate(bobRev)

	// Wait for both chains to lock in. The negotiation state returning to
	// idle is set (under the updater lock) only after applyDynParams has run,
	// so it is a safe synchronization point for the config reads below.
	require.Eventually(t, func() bool {
		return coreLink.updater.State() == dyn.StateIdle
	}, 5*time.Second, 10*time.Millisecond, "dance did not complete")

	// The agreed params are applied with proposer-owned semantics: a remote
	// proposer's max_accepted_htlcs change lands in our local config.
	require.Equal(t, uint16(20),
		coreLink.channel.State().LocalChanCfg.MaxAcceptedHtlcs,
		"agreed params not applied after lock-in")

	// The dyn commit advanced both commitment heights like a normal
	// commitment_signed.
	require.Equal(t, aliceHeightBefore+1,
		coreLink.channel.State().LocalCommitment.CommitHeight,
		"our commit height did not advance")
	require.Equal(t, bobHeightBefore+1,
		bob.State().LocalCommitment.CommitHeight,
		"bob commit height did not advance")

	// Quiescence was exited so htlc traffic can resume.
	require.False(t, coreLink.quiescer.IsQuiescent(),
		"channel should no longer be quiescent")
}

// TestChannelLinkDynProposer drives the proposer path end to end at the link
// level: a local proposal request drives quiescence with us as the initiator,
// sends a dyn_propose, and a valid dyn_ack triggers the bundled dyn_commit_sig.
// The peer completes the dance with its revoke_and_ack + commitment_signed, our
// link replies with its final revoke_and_ack, and once both chains lock in the
// agreed params are applied, both heights advance, and quiescence is exited.
func TestChannelLinkDynProposer(t *testing.T) {
	t.Parallel()

	harness, coreLink, msgs := newDynLinkHarness(t)
	bob := harness.bobChannel

	chanID := coreLink.ChanID()

	// We are the proposer, so a max_accepted_htlcs change constrains the
	// counterparty (remote) config.
	params := lnwallet.ChannelParams{
		MaxAcceptedHtlcs: fn.Some(uint16(20)),
	}
	require.NotEqual(t, uint16(20),
		coreLink.channel.State().RemoteChanCfg.MaxAcceptedHtlcs)

	aliceHeightBefore := coreLink.channel.State().LocalCommitment.CommitHeight
	bobHeightBefore := bob.State().LocalCommitment.CommitHeight

	// Request a local proposal. This drives quiescence first.
	errCh := coreLink.initDynProposal(dyn.ProposalRequest{Params: params})

	// The link should send its Stfu as the quiescence initiator.
	msg := recvLinkMsg(t, msgs)
	stfu, ok := msg.(*lnwire.Stfu)
	require.Truef(t, ok, "expected Stfu, got %T", msg)
	require.True(t, stfu.Initiator, "we should be the quiescence initiator")

	// The peer completes quiescence by replying with its own Stfu.
	coreLink.HandleChannelUpdate(&lnwire.Stfu{
		ChanID:    chanID,
		Initiator: false,
	})

	// Now that the channel is quiescent, the pending proposal is initiated.
	msg = recvLinkMsg(t, msgs)
	dp, ok := msg.(*lnwire.DynPropose)
	require.Truef(t, ok, "expected DynPropose, got %T", msg)

	// Starting the negotiation should have reported success.
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for proposal result")
	}

	require.Equal(t, dyn.StateAwaitingAck, coreLink.updater.State())

	// Bob (the responder) stages the agreed update into his remote log so
	// that his rendering of both commitments matches ours, then accepts by
	// returning a valid dyn_ack over the exact proposal TLVs, bound to the
	// next commitment height and chain.
	_, err := bob.ReceiveDynUpdate(params)
	require.NoError(t, err, "bob unable to stage dyn update")

	tlvs, err := dp.SerializeTlvData()
	require.NoError(t, err)

	bobNodePriv, _ := btcec.PrivKeyFromBytes(bobPrivKey)
	height := coreLink.channel.State().LocalCommitment.CommitHeight + 1
	ackSig, err := lnwire.SignDynAck(
		bobNodePriv, coreLink.channel.State().ChainHash, chanID, height,
		tlvs,
	)
	require.NoError(t, err)

	coreLink.HandleChannelUpdate(&lnwire.DynAck{
		ChanID: chanID,
		Sig:    ackSig,
	})

	// The valid ack triggers the bundled dyn_commit_sig: our commitment
	// signature over the responder's next commitment, the echoed dyn_ack
	// signature, and the accepted proposal.
	dc, ok := recvLinkMsg(t, msgs).(*lnwire.DynCommit)
	require.True(t, ok, "expected DynCommit from link")
	require.Equal(t, ackSig, dc.AckSig, "dyn_commit_sig echoed wrong ack")

	// Bob validates the bundled sig against his newly rendered commitment,
	// revokes, and signs our next commitment (a normal commitment_signed).
	err = bob.ReceiveNewCommitment(&lnwallet.CommitSigs{
		CommitSig: dc.CommitSig,
	})
	require.NoError(t, err, "bob unable to receive dyn commitment")
	bobRev, _, _, err := bob.RevokeCurrentCommitment()
	require.NoError(t, err, "bob unable to revoke commitment")
	bobCommit, err := bob.SignNextCommitment(t.Context())
	require.NoError(t, err, "bob unable to sign next commitment")

	// Our link processes the peer's revoke_and_ack and commitment_signed,
	// then replies with its final revoke_and_ack.
	coreLink.HandleChannelUpdate(bobRev)
	coreLink.HandleChannelUpdate(&lnwire.CommitSig{
		ChanID:    chanID,
		CommitSig: bobCommit.CommitSig,
	})

	aliceRev, ok := recvLinkMsg(t, msgs).(*lnwire.RevokeAndAck)
	require.True(t, ok, "expected final RevokeAndAck from link")
	_, _, err = bob.ReceiveRevocation(aliceRev)
	require.NoError(t, err, "bob unable to receive final revocation")

	// Wait for both chains to lock in. The negotiation state returning to
	// idle is set (under the updater lock) only after applyDynParams has run,
	// so it is a safe synchronization point for the config reads below.
	require.Eventually(t, func() bool {
		return coreLink.updater.State() == dyn.StateIdle
	}, 5*time.Second, 10*time.Millisecond, "dance did not complete")

	// The agreed params are applied with proposer-owned semantics: a local
	// proposer's max_accepted_htlcs change lands in the remote config.
	require.Equal(t, uint16(20),
		coreLink.channel.State().RemoteChanCfg.MaxAcceptedHtlcs,
		"agreed params not applied after lock-in")

	// The dyn commit advanced both commitment heights like a normal
	// commitment_signed.
	require.Equal(t, aliceHeightBefore+1,
		coreLink.channel.State().LocalCommitment.CommitHeight,
		"our commit height did not advance")
	require.Equal(t, bobHeightBefore+1,
		bob.State().LocalCommitment.CommitHeight,
		"bob commit height did not advance")

	// Quiescence was exited so htlc traffic can resume.
	require.False(t, coreLink.quiescer.IsQuiescent(),
		"channel should no longer be quiescent")
}

// TestChannelLinkApplyDynParams verifies the apply-at-lock-in step: applying an
// agreed proposal mutates the underlying channel config with proposer-owned
// semantics.
func TestChannelLinkApplyDynParams(t *testing.T) {
	t.Parallel()

	_, coreLink, _ := newDynLinkHarness(t)

	// Sanity check the starting value differs from what we will apply.
	require.NotEqual(t, uint16(25),
		coreLink.channel.State().RemoteChanCfg.MaxAcceptedHtlcs)

	handoff := dyn.CommitHandoff{
		Proposer: lntypes.Local,
		Params: lnwallet.ChannelParams{
			MaxAcceptedHtlcs: fn.Some(uint16(25)),
		},
	}
	lockIn := lntypes.Dual[uint64]{Local: 0, Remote: 0}

	require.NoError(t, coreLink.applyDynParams(handoff, lockIn))

	// Proposer-owned semantics: a Local proposer's max_accepted_htlcs
	// change constrains the counterparty (remote) config.
	require.Equal(t, uint16(25),
		coreLink.channel.State().RemoteChanCfg.MaxAcceptedHtlcs)
}

// TestChannelLinkDynDisabled ensures a link without the feature enabled ignores
// incoming dyn messages and refuses local proposals, leaving non-dyn behavior
// untouched.
func TestChannelLinkDynDisabled(t *testing.T) {
	t.Parallel()

	const (
		chanAmt     = btcutil.SatoshiPerBitcoin * 5
		chanReserve = btcutil.SatoshiPerBitcoin * 1
	)

	harness, err := newSingleLinkTestHarness(t, chanAmt, chanReserve)
	require.NoError(t, err)
	require.NoError(t, harness.start())
	t.Cleanup(func() { harness.aliceLink.Stop() })

	//nolint:forcetypeassert
	coreLink := harness.aliceLink.(*channelLink)

	// No updater is mounted when the feature is disabled.
	require.Nil(t, coreLink.updater)

	// A local proposal request is rejected.
	select {
	case err := <-coreLink.initDynProposal(dyn.ProposalRequest{}):
		require.ErrorIs(t, err, errDynDisabled)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for proposal result")
	}

	// An incoming dyn message is ignored (no disconnect / link failure).
	coreLink.HandleChannelUpdate(&lnwire.DynPropose{
		ChanID: coreLink.ChanID(),
	})

	require.Never(t, func() bool {
		coreLink.RLock()
		defer coreLink.RUnlock()

		return coreLink.failed
	}, 200*time.Millisecond, 20*time.Millisecond)
}
