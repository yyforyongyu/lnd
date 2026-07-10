package lnwallet

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// hasOutputScript reports whether the given transaction has an output paying to
// the passed pkScript.
func hasOutputScript(tx *wire.MsgTx, pkScript []byte) bool {
	for _, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) {
			return true
		}
	}

	return false
}

// deriveDynViewKeyRing derives the commitment key ring the channel would use
// for the next commitment on the requested chain, mirroring the derivation done
// inside SignNextCommitment / restoreCommitState.
func deriveDynViewKeyRing(t *testing.T, lc *LightningChannel,
	whoseCommit lntypes.ChannelParty,
	chanType channeldb.ChannelType) *CommitmentKeyRing {

	t.Helper()

	var commitPoint *btcec.PublicKey
	if whoseCommit.IsRemote() {
		commitPoint = lc.channelState.RemoteNextRevocation
		require.NotNil(t, commitPoint, "no remote next revocation")
	} else {
		nextHeight := lc.commitChains.Local.tip().height + 1
		secret, err := lc.channelState.RevocationProducer.AtIndex(
			nextHeight,
		)
		require.NoError(t, err, "unable to derive commit secret")
		commitPoint = input.ComputeCommitmentPoint(secret[:])
	}

	return DeriveCommitmentKeys(
		commitPoint, whoseCommit, chanType,
		&lc.channelState.LocalChanCfg, &lc.channelState.RemoteChanCfg,
	)
}

// buildDynCommitView constructs the commitment view for the requested chain
// including all of the channel's currently staged updates.
func buildDynCommitView(t *testing.T, lc *LightningChannel,
	whoseCommit lntypes.ChannelParty,
	keyRing *CommitmentKeyRing) *commitment {

	t.Helper()

	remoteACKedIndex := lc.commitChains.Local.tail().messageIndices.Remote
	remoteHtlcIndex := lc.commitChains.Local.tail().theirHtlcIndex

	view, err := lc.fetchCommitmentView(
		whoseCommit, lc.updateLogs.Local.logIndex,
		lc.updateLogs.Local.htlcCounter, remoteACKedIndex,
		remoteHtlcIndex, keyRing,
	)
	require.NoError(t, err, "unable to build commitment view")

	return view
}

// TestDynCommitRendersNewParams asserts that a staged dyn_commit update-log
// entry re-renders the commitment transactions with the proposer's proposed
// parameters: the proposer's own dust limit governs the proposer's own
// commitment, while the proposed to_self_delay (CSV) governs the counterparty's
// to_local output. This mirrors the proposer-owned semantics enforced by
// ChannelParams.ApplyToConfigs.
func TestDynCommitRendersNewParams(t *testing.T) {
	t.Parallel()

	const chanType = channeldb.SingleFunderTweaklessBit

	// Alice is the channel funder and, here, the dynamic-commitments
	// proposer.
	aliceChannel, _, err := CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	oldRemoteCSV := aliceChannel.channelState.RemoteChanCfg.CsvDelay
	oldLocalCSV := aliceChannel.channelState.LocalChanCfg.CsvDelay
	oldRemoteDust := aliceChannel.channelState.RemoteChanCfg.DustLimit
	newCSV := oldRemoteCSV + 42
	newDust := aliceChannel.channelState.LocalChanCfg.DustLimit + 500

	params := ChannelParams{
		DustLimit: fn.Some(newDust),
		CsvDelay:  fn.Some(newCSV),
	}

	logIndex, err := aliceChannel.AddDynUpdate(params)
	require.NoError(t, err, "unable to stage dyn update")
	require.Equal(t, uint64(0), logIndex, "unexpected dyn log index")

	// The remote (Bob's) commitment must render Bob's to_local output with
	// Alice's proposed CSV, and must NOT retain the old CSV.
	remoteKeyRing := deriveDynViewKeyRing(
		t, aliceChannel, lntypes.Remote, chanType,
	)
	remoteView := buildDynCommitView(
		t, aliceChannel, lntypes.Remote, remoteKeyRing,
	)

	// Bob is not the funder, so initiator=false for his commitment; the type
	// has no lease, so leaseExpiry is unused.
	newScript, err := CommitScriptToSelf(
		chanType, false, remoteKeyRing.ToLocalKey,
		remoteKeyRing.RevocationKey, uint32(newCSV), 0,
		input.NoneTapLeaf(),
	)
	require.NoError(t, err, "unable to build new to_local script")
	require.True(t, hasOutputScript(remoteView.txn, newScript.PkScript()),
		"remote commitment missing to_local with proposed CSV")

	oldScript, err := CommitScriptToSelf(
		chanType, false, remoteKeyRing.ToLocalKey,
		remoteKeyRing.RevocationKey, uint32(oldRemoteCSV), 0,
		input.NoneTapLeaf(),
	)
	require.NoError(t, err, "unable to build old to_local script")
	require.False(t, hasOutputScript(remoteView.txn, oldScript.PkScript()),
		"remote commitment still uses old CSV")

	// Alice's dust proposal governs her own commitment, so the remote
	// (Bob's) commitment dust limit is unchanged.
	require.Equal(t, oldRemoteDust, remoteView.dustLimit,
		"remote commitment dust should be unchanged")

	// Alice's own (local) commitment must reflect her new dust limit, while
	// her own to_local CSV stays put (her proposal only constrains the
	// counterparty's to_local delay).
	localKeyRing := deriveDynViewKeyRing(
		t, aliceChannel, lntypes.Local, chanType,
	)
	localView := buildDynCommitView(
		t, aliceChannel, lntypes.Local, localKeyRing,
	)
	require.Equal(t, newDust, localView.dustLimit,
		"local commitment dust not updated to proposed value")

	localToLocal, err := CommitScriptToSelf(
		chanType, true, localKeyRing.ToLocalKey,
		localKeyRing.RevocationKey, uint32(oldLocalCSV), 0,
		input.NoneTapLeaf(),
	)
	require.NoError(t, err, "unable to build local to_local script")
	require.True(t, hasOutputScript(localView.txn, localToLocal.PkScript()),
		"proposer's own to_local CSV should be unchanged")
}

// TestDynCommitStateTransition drives a full commitment dance across a staged
// dyn_commit update and asserts that (a) both commitment heights advance exactly
// as they would for a normal commitment_signed, and (b) the committed
// transactions are rendered with the new parameters. A successful transition is
// itself proof that both peers render the dyn commitment identically, since
// otherwise signature verification inside ReceiveNewCommitment would fail.
func TestDynCommitStateTransition(t *testing.T) {
	t.Parallel()

	const chanType = channeldb.SingleFunderTweaklessBit

	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	newCSV := aliceChannel.channelState.RemoteChanCfg.CsvDelay + 42
	newDust := aliceChannel.channelState.LocalChanCfg.DustLimit + 500
	params := ChannelParams{
		DustLimit: fn.Some(newDust),
		CsvDelay:  fn.Some(newCSV),
	}

	aliceHeightBefore := aliceChannel.channelState.LocalCommitment.
		CommitHeight
	bobHeightBefore := bobChannel.channelState.LocalCommitment.CommitHeight

	// Alice is the proposer; Bob mirrors the agreed update into his remote
	// log.
	_, err = aliceChannel.AddDynUpdate(params)
	require.NoError(t, err, "alice unable to stage dyn update")
	_, err = bobChannel.ReceiveDynUpdate(params)
	require.NoError(t, err, "bob unable to stage dyn update")

	// Execute the bundled dyn_commit dance as a standard commitment/revoke
	// round trip.
	require.NoError(t, ForceStateTransition(aliceChannel, bobChannel),
		"dyn commitment dance failed")

	// The dyn commitment advances the height like any commitment_signed.
	require.Equal(t, aliceHeightBefore+1,
		aliceChannel.channelState.LocalCommitment.CommitHeight,
		"alice commit height did not advance")
	require.Equal(t, bobHeightBefore+1,
		bobChannel.channelState.LocalCommitment.CommitHeight,
		"bob commit height did not advance")

	// Bob's freshly committed local commitment must render his to_local
	// output with the CSV Alice proposed.
	bobHeight := bobChannel.channelState.LocalCommitment.CommitHeight
	secret, err := bobChannel.channelState.RevocationProducer.AtIndex(
		bobHeight,
	)
	require.NoError(t, err, "unable to derive bob commit secret")
	bobPoint := input.ComputeCommitmentPoint(secret[:])
	bobKeyRing := DeriveCommitmentKeys(
		bobPoint, lntypes.Local, chanType,
		&bobChannel.channelState.LocalChanCfg,
		&bobChannel.channelState.RemoteChanCfg,
	)

	wantScript, err := CommitScriptToSelf(
		chanType, bobChannel.channelState.IsInitiator,
		bobKeyRing.ToLocalKey, bobKeyRing.RevocationKey,
		uint32(newCSV), 0, input.NoneTapLeaf(),
	)
	require.NoError(t, err, "unable to build expected to_local script")
	require.True(t, hasOutputScript(
		bobChannel.channelState.LocalCommitment.CommitTx,
		wantScript.PkScript(),
	), "bob's committed to_local missing proposed CSV")
}

// TestDynCommitRejectsHtlcs asserts the no-HTLC precondition of the dynamic-
// commitments protocol is enforced at two layers: the staging API refuses to
// add a dyn update while an HTLC is in flight, and the commitment renderer
// refuses to build a commitment that carries both a dyn update and an HTLC even
// if an entry is force-staged.
func TestDynCommitRejectsHtlcs(t *testing.T) {
	t.Parallel()

	const chanType = channeldb.SingleFunderTweaklessBit

	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	// Put an HTLC in flight on both sides.
	htlcAmt := lnwire.NewMSatFromSatoshis(btcutil.Amount(30000))
	htlc, _ := createHTLC(0, htlcAmt)
	addAndReceiveHTLC(t, aliceChannel, bobChannel, htlc, nil)

	params := ChannelParams{CsvDelay: fn.Some(uint16(46))}

	// The staging API rejects the update on both the proposer and responder
	// side.
	_, err = aliceChannel.AddDynUpdate(params)
	require.ErrorIs(t, err, ErrDynHtlcsPresent,
		"AddDynUpdate should reject with HTLC in flight")
	_, err = bobChannel.ReceiveDynUpdate(params)
	require.ErrorIs(t, err, ErrDynHtlcsPresent,
		"ReceiveDynUpdate should reject with HTLC in flight")

	// Force-stage a dyn entry alongside the HTLC to bypass the API check,
	// then confirm the renderer still refuses to sign the commitment.
	staged := params
	aliceChannel.updateLogs.Local.appendUpdate(&paymentDescriptor{
		ChanID:    aliceChannel.ChannelID(),
		LogIndex:  aliceChannel.updateLogs.Local.logIndex,
		EntryType: DynCommit,
		DynParams: &staged,
	})

	_, err = aliceChannel.SignNextCommitment(ctxb)
	require.ErrorIs(t, err, ErrDynHtlcsPresent,
		"renderer should reject dyn update with HTLC present")
}
