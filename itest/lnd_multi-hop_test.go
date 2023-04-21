package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

const (
	finalCltvDelta  = routing.MinCLTVDelta // 18.
	thawHeightDelta = finalCltvDelta * 2   // 36.
)

var commitWithZeroConf = []struct {
	commitType lnrpc.CommitmentType
	zeroConf   bool
}{
	{
		commitType: lnrpc.CommitmentType_LEGACY,
		zeroConf:   false,
	},
	{
		commitType: lnrpc.CommitmentType_ANCHORS,
		zeroConf:   false,
	},
	{
		commitType: lnrpc.CommitmentType_ANCHORS,
		zeroConf:   true,
	},
	{
		commitType: lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		zeroConf:   false,
	},
	{
		commitType: lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		zeroConf:   true,
	},
	{
		commitType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
		zeroConf:   false,
	},
	{
		commitType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
		zeroConf:   true,
	},
}

// makeRouteHints creates a route hints that will allow Carol to be reached
// using an unadvertised channel created by Bob (Bob -> Carol). If the zeroConf
// bool is set, then the scid alias of Bob will be used in place.
func makeRouteHints(bob, carol *node.HarnessNode,
	zeroConf bool) []*lnrpc.RouteHint {

	carolChans := carol.RPC.ListChannels(
		&lnrpc.ListChannelsRequest{},
	)

	carolChan := carolChans.Channels[0]

	hopHint := &lnrpc.HopHint{
		NodeId: carolChan.RemotePubkey,
		ChanId: carolChan.ChanId,
		FeeBaseMsat: uint32(
			chainreg.DefaultBitcoinBaseFeeMSat,
		),
		FeeProportionalMillionths: uint32(
			chainreg.DefaultBitcoinFeeRate,
		),
		CltvExpiryDelta: chainreg.DefaultBitcoinTimeLockDelta,
	}

	if zeroConf {
		bobChans := bob.RPC.ListChannels(
			&lnrpc.ListChannelsRequest{},
		)

		// Now that we have Bob's channels, scan for the channel he has
		// open to Carol so we can use the proper scid.
		var found bool
		for _, bobChan := range bobChans.Channels {
			if bobChan.RemotePubkey == carol.PubKeyStr {
				hopHint.ChanId = bobChan.AliasScids[0]

				found = true

				break
			}
		}
		if !found {
			bob.Fatalf("unable to create route hint")
		}
	}

	return []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{hopHint},
		},
	}
}

// caseRunner defines a single test case runner.
type caseRunner func(ht *lntest.HarnessTest, alice, bob *node.HarnessNode,
	c lnrpc.CommitmentType, zeroConf bool)

// runMultiHopHtlcClaimTest is a helper method to build test cases based on
// different commitment types and zero-conf config and run them.
func runMultiHopHtlcClaimTest(ht *lntest.HarnessTest, tester caseRunner) {
	for _, typeAndConf := range commitWithZeroConf {
		typeAndConf := typeAndConf
		name := fmt.Sprintf("zeroconf=%v/committype=%v",
			typeAndConf.zeroConf, typeAndConf.commitType.String())

		// Create the nodes here so that separate logs will be created
		// for Alice and Bob.
		args := lntest.NodeArgsForCommitType(typeAndConf.commitType)
		if typeAndConf.zeroConf {
			args = append(
				args, "--protocol.option-scid-alias",
				"--protocol.zero-conf",
			)
		}

		s := ht.Run(name, func(t1 *testing.T) {
			st := ht.Subtest(t1)

			alice := st.NewNode("Alice", args)
			bob := st.NewNode("Bob", args)
			st.ConnectNodes(alice, bob)

			// Start each test with the default static fee estimate.
			st.SetFeeEstimate(12500)

			// Add test name to the logs.
			alice.AddToLogf("Running test case: %s", name)
			bob.AddToLogf("Running test case: %s", name)

			tester(
				st, alice, bob,
				typeAndConf.commitType, typeAndConf.zeroConf,
			)
		})
		if !s {
			return
		}
	}
}

// testMultiHopHtlcLocalTimeout tests that in a multi-hop HTLC scenario, if the
// outgoing HTLC is about to time out, then we'll go to chain in order to claim
// it using the HTLC timeout transaction. Any dust HTLC's should be immediately
// canceled backwards. Once the timeout has been reached, then we should sweep
// it on-chain, and cancel the HTLC backwards.
func testMultiHopHtlcLocalTimeout(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runMultiHopHtlcLocalTimeout)
}

func runMultiHopHtlcLocalTimeout(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, true, c, zeroConf,
	)

	// Now that our channels are set up, we'll send two HTLC's from Alice
	// to Carol. The first HTLC will be universally considered "dust",
	// while the second will be a proper fully valued HTLC.
	const (
		dustHtlcAmt = btcutil.Amount(100)
		htlcAmt     = btcutil.Amount(300_000)
	)

	// We'll create two random payment hashes unknown to carol, then send
	// each of them by manually specifying the HTLC details.
	carolPubKey := carol.PubKey[:]
	dustPayHash := ht.Random32Bytes()
	payHash := ht.Random32Bytes()

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	alice.RPC.SendPayment(&routerrpc.SendPaymentRequest{
		Dest:           carolPubKey,
		Amt:            int64(dustHtlcAmt),
		PaymentHash:    dustPayHash,
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		RouteHints:     routeHints,
	})

	alice.RPC.SendPayment(&routerrpc.SendPaymentRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		RouteHints:     routeHints,
	})

	// Verify that all nodes in the path now have two HTLC's with the
	// proper parameters.
	ht.AssertActiveHtlcs(alice, dustPayHash, payHash)
	ht.AssertActiveHtlcs(bob, dustPayHash, payHash)
	ht.AssertActiveHtlcs(carol, dustPayHash, payHash)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the HTLC is about to
	// timeout. With the default outgoing broadcast delta of zero, this will
	// be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	ht.MineBlocks(numBlocks)

	// Bob's force close transaction should now be found in the mempool. If
	// there are anchors, we also expect Bob's anchor sweep.
	expectedTxes := 1
	hasAnchors := lntest.CommitTypeHasAnchors(c)
	if hasAnchors {
		expectedTxes = 2
	}
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	op := ht.OutPointFromChannelPoint(bobChanPoint)
	closeTx := ht.Miner.AssertOutpointInMempool(op)

	// Mine a block to confirm the closing transaction.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// At this point, Bob should have canceled backwards the dust HTLC
	// that we sent earlier. This means Alice should now only have a single
	// HTLC on her channel.
	ht.AssertActiveHtlcs(alice, payHash)

	// With the closing transaction confirmed, we should expect Bob's HTLC
	// timeout transaction to be broadcast due to the expiry being reached.
	// If there are anchors, we also expect Carol's anchor sweep now.
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// We'll also obtain the expected HTLC timeout transaction hash.
	htlcOutpoint := wire.OutPoint{Hash: closeTx.TxHash(), Index: 0}
	commitOutpoint := wire.OutPoint{Hash: closeTx.TxHash(), Index: 1}
	if hasAnchors {
		htlcOutpoint.Index = 2
		commitOutpoint.Index = 3
	}
	htlcTimeoutTxid := ht.Miner.AssertOutpointInMempool(
		htlcOutpoint,
	).TxHash()

	// Mine a block to confirm the expected transactions.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// With Bob's HTLC timeout transaction confirmed, there should be no
	// active HTLC's on the commitment transaction from Alice -> Bob.
	ht.AssertNumActiveHtlcs(alice, 0)

	// At this point, Bob should show that the pending HTLC has advanced to
	// the second stage and is ready to be swept once the timelock is up.
	pendingChanResp := bob.RPC.PendingChannels()
	require.Equal(ht, 1, len(pendingChanResp.PendingForceClosingChannels))
	forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
	require.NotZero(ht, forceCloseChan.LimboBalance)
	require.Positive(ht, forceCloseChan.BlocksTilMaturity)
	require.Equal(ht, 1, len(forceCloseChan.PendingHtlcs))
	require.Equal(ht, uint32(2), forceCloseChan.PendingHtlcs[0].Stage)

	htlcTimeoutOutpoint := wire.OutPoint{Hash: htlcTimeoutTxid, Index: 0}
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Since Bob is the initiator of the script-enforced leased
		// channel between him and Carol, he will incur an additional
		// CLTV on top of the usual CSV delay on any outputs that he can
		// sweep back to his wallet.
		blocksTilMaturity := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(blocksTilMaturity)

		// Check that the sweep spends the expected inputs.
		ht.Miner.AssertOutpointInMempool(commitOutpoint)
		ht.Miner.AssertOutpointInMempool(htlcTimeoutOutpoint)
	} else {
		// Since Bob force closed the channel between him and Carol, he
		// will incur the usual CSV delay on any outputs that he can
		// sweep back to his wallet. We'll subtract one block from our
		// current maturity period to assert on the mempool.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity - 1)
		ht.MineBlocks(numBlocks)

		// Check that the sweep spends from the mined commitment.
		ht.Miner.AssertOutpointInMempool(commitOutpoint)

		// Mine a block to confirm Bob's commit sweep tx and assert it
		// was in fact mined.
		ht.MineBlocksAndAssertNumTxes(1, 1)

		// Mine one more block to trigger the timeout path.
		ht.MineEmptyBlocks(1)

		// Bob's sweeper should now broadcast his second layer sweep
		// due to the CSV on the HTLC timeout output.
		ht.Miner.AssertOutpointInMempool(htlcTimeoutOutpoint)
	}

	// Next, we'll mine a final block that should confirm the sweeping
	// transactions left.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Once this transaction has been confirmed, Bob should detect that he
	// no longer has any pending channels.
	ht.AssertNumPendingForceClose(bob, 0)

	// Coop close channel, expect no anchors.
	ht.CloseChannel(alice, aliceChanPoint)
}

// testMultiHopReceiverChainClaim tests that in the multi-hop setting, if the
// receiver of an HTLC knows the preimage, but wasn't able to settle the HTLC
// off-chain, then it goes on chain to claim the HTLC uing the HTLC success
// transaction. In this scenario, the node that sent the outgoing HTLC should
// extract the preimage from the sweep transaction found in the mempool, and
// finish settling the HTLC backwards into the route.
//
// NOTE: for neutrino, we will extract the preimage from blocks.
func testMultiHopReceiverChainClaim(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runMultiHopReceiverChainClaim)
}

func runMultiHopReceiverChainClaim(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertOutgoingHTLCActive(alice, aliceChanPoint, payHash[:])
	ht.AssertIncomingHTLCActive(bob, aliceChanPoint, payHash[:])
	ht.AssertOutgoingHTLCActive(bob, bobChanPoint, payHash[:])
	ht.AssertIncomingHTLCActive(carol, bobChanPoint, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We now advance the block height to the point where Carol will force
	// close her channel with Bob, broadcast the closing tx but keep it
	// unconfirmed.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))

	// Now we'll mine enough blocks to prompt carol to actually go to the
	// chain in order to sweep her HTLC since the value is high enough.
	ht.MineBlocks(numBlocks)

	// At this point, Carol should broadcast her active commitment
	// transaction in order to go to the chain and sweep her HTLC. If there
	// are anchors, Carol also sweeps hers.
	expectedTxes := 1
	hasAnchors := lntest.CommitTypeHasAnchors(c)
	if hasAnchors {
		expectedTxes = 2
	}
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	closingTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Confirm the commitment.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, a series of transactions
	// should be broadcast by Bob and Carol. When Bob notices Carol's second
	// level transaction in the mempool, he will extract the preimage and
	// settle the HTLC back off-chain.
	switch c {
	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a sweep tx to sweep his output in the channel with
	// Carol.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a sweep tx to sweep his output in the channel with
	// Carol, and another sweep tx to sweep his anchor output.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 3

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a sweep tx to sweep his anchor output. Bob's commit
	// output can't be swept yet as he's incurring an additional CLTV from
	// being the channel initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// All transactions should be spending from the commitment transaction.
	txes := ht.Miner.GetNumTxsFromMempool(expectedTxes)
	ht.AssertAllTxesSpendFrom(txes, closingTxid)

	// Mine a block to confirm the 2nd level success tx if this is
	// neutrino backend.
	if ht.IsNeutrinoBackend() {
		ht.MineBlocksAndAssertNumTxes(1, expectedTxes)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	// Once the second-level transaction enters the mempool, Bob should
	// have extracted the preimage and sent it back to Alice, clearing the
	// HTLC off-chain.
	ht.AssertHLTCNotActive(alice, aliceChanPoint, payHash[:])

	// Exit the test and clean the mempool.
	//
	// NOTE: for non-standby nodes there's no need to clean up the force
	// close as long as the mempool is cleaned.
	ht.CleanShutDown()
}

// testMultiHopLocalForceCloseOnChainHtlcTimeout tests that in a multi-hop HTLC
// scenario, if the node that extended the HTLC to the final node closes their
// commitment on-chain early, then it eventually recognizes this HTLC as one
// that's timed out. At this point, the node should timeout the HTLC using the
// HTLC timeout transaction, then cancel it backwards as normal.
func testMultiHopLocalForceCloseOnChainHtlcTimeout(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(
		ht, runMultiHopLocalForceCloseOnChainHtlcTimeout,
	)
}

func runMultiHopLocalForceCloseOnChainHtlcTimeout(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, true, c, zeroConf,
	)

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	const htlcAmt = btcutil.Amount(300_000)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// We'll now send a single HTLC across our multi-hop network.
	carolPubKey := carol.PubKey[:]
	payHash := ht.Random32Bytes()
	req := &routerrpc.SendPaymentRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		RouteHints:     routeHints,
	}
	alice.RPC.SendPayment(req)

	// Once the HTLC has cleared, all channels in our mini network should
	// have the it locked in.
	ht.AssertActiveHtlcs(alice, payHash)
	ht.AssertActiveHtlcs(bob, payHash)
	ht.AssertActiveHtlcs(carol, payHash)

	// blocksMined records how many blocks have mined after the creation of
	// the invoice so it can be used to calculate how many more blocks need
	// to be mined to trigger a force close later on.
	var blocksMined uint32

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// Now that all parties have the HTLC locked in, we'll immediately
	// force close the Bob -> Carol channel. This should trigger contract
	// resolution mode for both of them.
	hasAnchors := lntest.CommitTypeHasAnchors(c)
	stream, _ := ht.CloseChannelAssertPending(bob, bobChanPoint, true)
	closeTx := ht.AssertStreamChannelForceClosed(
		bob, bobChanPoint, hasAnchors, stream,
	)

	// Increase the blocks mined. At this step
	// AssertStreamChannelForceClosed mines one block.
	blocksMined++

	// If the channel closed has anchors, we should expect to see a sweep
	// transaction for Carol's anchor.
	htlcOutpoint := wire.OutPoint{Hash: *closeTx, Index: 0}
	bobCommitOutpoint := wire.OutPoint{Hash: *closeTx, Index: 1}
	if hasAnchors {
		htlcOutpoint.Index = 2
		bobCommitOutpoint.Index = 3
		ht.Miner.AssertNumTxsInMempool(1)
	}

	// Before the HTLC times out, we'll need to assert that Bob broadcasts a
	// sweep transaction for his commit output. Note that if the channel has
	// a script-enforced lease, then Bob will have to wait for an additional
	// CLTV before sweeping it.
	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// The sweep is broadcast on the block immediately before the
		// CSV expires and the commitment was already mined inside
		// AssertStreamChannelForceClosed(), so mine one block less
		// than defaultCSV in order to perform mempool assertions.
		ht.MineBlocks(defaultCSV - 1)

		commitSweepTx := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		)
		txid := commitSweepTx.TxHash()
		block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
		ht.Miner.AssertTxInBlock(block, &txid)

		blocksMined += defaultCSV
	}

	// We'll now mine enough blocks for the HTLC to expire. After this, Bob
	// should hand off the now expired HTLC output to the utxo nursery.
	numBlocks := padCLTV(uint32(finalCltvDelta) -
		lncfg.DefaultOutgoingBroadcastDelta)
	ht.MineBlocks(numBlocks - blocksMined)

	// Bob's pending channel report should show that he has a single HTLC
	// that's now in stage one.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 1)

	// We should also now find a transaction in the mempool, as Bob should
	// have broadcast his second layer timeout transaction.
	timeoutTx := ht.Miner.AssertOutpointInMempool(htlcOutpoint).TxHash()

	// Next, we'll mine an additional block. This should serve to confirm
	// the second layer timeout transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &timeoutTx)

	// With the second layer timeout transaction confirmed, Bob should have
	// canceled backwards the HTLC that carol sent.
	ht.AssertNumActiveHtlcs(bob, 0)

	// Additionally, Bob should now show that HTLC as being advanced to the
	// second stage.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 2)

	// Bob should now broadcast a transaction that sweeps certain inputs
	// depending on the commitment type. We'll need to mine some blocks
	// before the broadcast is possible.
	resp := bob.RPC.PendingChannels()

	require.Len(ht, resp.PendingForceClosingChannels, 1)
	forceCloseChan := resp.PendingForceClosingChannels[0]
	require.Len(ht, forceCloseChan.PendingHtlcs, 1)
	pendingHtlc := forceCloseChan.PendingHtlcs[0]
	require.Positive(ht, pendingHtlc.BlocksTilMaturity)
	numBlocks = uint32(pendingHtlc.BlocksTilMaturity)

	ht.MineBlocks(numBlocks)

	// Now that the CSV/CLTV timelock has expired, the transaction should
	// either only sweep the HTLC timeout transaction, or sweep both the
	// HTLC timeout transaction and Bob's commit output depending on the
	// commitment type.
	htlcTimeoutOutpoint := wire.OutPoint{Hash: timeoutTx, Index: 0}
	sweepTx := ht.Miner.AssertOutpointInMempool(
		htlcTimeoutOutpoint,
	).TxHash()
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		ht.Miner.AssertOutpointInMempool(bobCommitOutpoint)
	}

	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &sweepTx)

	// At this point, Bob should no longer show any channels as pending
	// close.
	ht.AssertNumPendingForceClose(bob, 0)

	// Coop close, no anchors.
	ht.CloseChannel(alice, aliceChanPoint)
}

// testMultiHopRemoteForceCloseOnChainHtlcTimeout tests that if we extend a
// multi-hop HTLC, and the final destination of the HTLC force closes the
// channel, then we properly timeout the HTLC directly on *their* commitment
// transaction once the timeout has expired. Once we sweep the transaction, we
// should also cancel back the initial HTLC.
func testMultiHopRemoteForceCloseOnChainHtlcTimeout(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(
		ht, runMultiHopRemoteForceCloseOnChainHtlcTimeout,
	)
}

func runMultiHopRemoteForceCloseOnChainHtlcTimeout(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, true, c, zeroConf,
	)

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	const htlcAmt = btcutil.Amount(30000)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// We'll now send a single HTLC across our multi-hop network.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(htlcAmt),
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// blocksMined records how many blocks have mined after the creation of
	// the invoice so it can be used to calculate how many more blocks need
	// to be mined to trigger a force close later on.
	var blocksMined uint32

	// Once the HTLC has cleared, all the nodes in our mini network should
	// show that the HTLC has been locked in.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// At this point, we'll now instruct Carol to force close the
	// transaction. This will let us exercise that Bob is able to sweep the
	// expired HTLC on Carol's version of the commitment transaction.
	closeStream, _ := ht.CloseChannelAssertPending(
		carol, bobChanPoint, true,
	)

	// For anchor channels, the anchor won't be used for CPFP because
	// channel arbitrator thinks Carol doesn't have preimage for her
	// incoming HTLC on the commitment transaction Bob->Carol. Although
	// Carol created this invoice, because it's a hold invoice, the
	// preimage won't be generated automatically.
	hasAnchorSweep := false
	closeTx := ht.AssertStreamChannelForceClosed(
		carol, bobChanPoint, hasAnchorSweep, closeStream,
	)

	// Increase the blocks mined. At this step
	// AssertStreamChannelForceClosed mines one block.
	blocksMined++

	// At this point, Bob should have a pending force close channel as
	// Carol has gone directly to chain.
	ht.AssertNumPendingForceClose(bob, 1)

	var expectedTxes int
	switch c {
	// Bob can sweep his commit output immediately.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 1

	// Bob can sweep his commit and anchor outputs immediately. Carol will
	// also sweep her anchor.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 3

	// Bob can't sweep his commit output yet as he was the initiator of a
	// script-enforced leased channel, so he'll always incur the additional
	// CLTV. He can still sweep his anchor output however.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// We now mine a block to clear up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)
	blocksMined++

	// Next, we'll mine enough blocks for the HTLC to expire. At this
	// point, Bob should hand off the output to his internal utxo nursery,
	// which will broadcast a sweep transaction.
	numBlocks := padCLTV(uint32(finalCltvDelta) -
		lncfg.DefaultOutgoingBroadcastDelta)
	ht.MineBlocks(numBlocks - blocksMined)

	// If we check Bob's pending channel report, it should show that he has
	// a single HTLC that's now in the second stage, as it skipped the
	// initial first stage since this is a direct HTLC.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 2)

	// We need to generate an additional block to trigger the sweep.
	ht.MineBlocks(1)

	// Bob's sweeping transaction should now be found in the mempool at
	// this point.
	sweepTx := ht.Miner.AssertNumTxsInMempool(1)[0]

	// If we mine an additional block, then this should confirm Bob's
	// transaction which sweeps the direct HTLC output.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, sweepTx)

	// Now that the sweeping transaction has been confirmed, Bob should
	// cancel back that HTLC. As a result, Alice should not know of any
	// active HTLC's.
	ht.AssertNumActiveHtlcs(alice, 0)

	// Now we'll check Bob's pending channel report. Since this was Carol's
	// commitment, he doesn't have to wait for any CSV delays, but he may
	// still need to wait for a CLTV on his commit output to expire
	// depending on the commitment type.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		resp := bob.RPC.PendingChannels()

		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(numBlocks)

		bobCommitOutpoint := wire.OutPoint{Hash: *closeTx, Index: 3}
		bobCommitSweep := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		)
		bobCommitSweepTxid := bobCommitSweep.TxHash()
		block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
		ht.Miner.AssertTxInBlock(block, &bobCommitSweepTxid)
	}
	ht.AssertNumPendingForceClose(bob, 0)

	// While we're here, we assert that our expired invoice's state is
	// correctly updated, and can no longer be settled.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_CANCELED)

	// We'll close out the test by closing the channel from Alice to Bob,
	// and then shutting down the new node we created as its no longer
	// needed. Coop close, no anchors.
	ht.CloseChannel(alice, aliceChanPoint)
}

// testMultiHopHtlcLocalChainClaim tests that in a multi-hop HTLC scenario, if
// we force close a channel with an incoming HTLC, and later find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain using
// the HTLC success transaction in order to ensure we don't lose any funds.
func testMultiHopHtlcLocalChainClaim(ht *lntest.HarnessTest) {
	testRunner := runMultiHopHtlcLocalChainClaimMempool

	// For neutrino backend, we need to extract preimage from blocks
	// instead of mempool.
	if ht.IsNeutrinoBackend() {
		testRunner = runMultiHopHtlcLocalChainClaimBlock
	}

	runMultiHopHtlcClaimTest(ht, testRunner)
}

// runMultiHopHtlcLocalChainClaimMempool runs the test
// `testMuliHopHtlcLocalChainClaim` by extracting the preimage from mempool.
func runMultiHopHtlcLocalChainClaimMempool(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// If the channel commitment type is taproot channel, then we'll need
	// to make some manual route hints so Alice can actually find a route.
	//
	// NOTE: this is needed as taproot channels are private, so Carol won't
	// include Bob in her route hints as Bob is a private node.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		RouteHints:     routeHints,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// At this point, Bob decides that he wants to exit the channel
	// immediately, so he force closes his commitment transaction.
	hasAnchors := lntest.CommitTypeHasAnchors(c)
	closeStream, _ := ht.CloseChannelAssertPending(
		bob, aliceChanPoint, true,
	)
	bobForceClose := ht.AssertStreamChannelForceClosed(
		bob, aliceChanPoint, hasAnchors, closeStream,
	)

	var expectedTxes int
	switch c {
	// Alice will sweep her commitment output immediately.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 1

	// Alice will sweep her commitment and anchor output immediately.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 2

	// Alice will sweep her anchor output immediately. Her commitment
	// output cannot be swept yet as it has incurred an additional CLTV due
	// to being the initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 1

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Suspend Bob to force Carol to go to chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(invoiceReq.CltvExpiry-
		lncfg.DefaultIncomingBroadcastDelta) - 1)
	ht.MineBlocks(numBlocks)

	// Carol's commitment transaction should now be in the mempool. If
	// there is an anchor, Carol will sweep that too.
	if lntest.CommitTypeHasAnchors(c) {
		expectedTxes = 2
	}
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Look up the closing transaction. It should be spending from the
	// funding transaction,
	closingTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Mine a block that should confirm the commit tx, the anchor if
	// present and the coinbase.
	block := ht.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]
	ht.Miner.AssertTxInBlock(block, &closingTxid)

	// Restart Bob again and he will learn the preimage from mempool.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, transactions will be
	// broadcast by both Bob and Carol.
	switch c {
	// For legacy channels, there are three transactions,
	// 1. Carol's second level HTLC transaction
	// 2. Bob sweeps his commitment output.
	// 3. Bob's second level HTLC transaction for his incoming HTLC since he
	// has learnt the preimage from the mempool.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 3

	// For anchor channels, there are three transactions,
	// 1. Carol's second level HTLC transaction
	// 2. Bob sweeps his commitment output and his second level HTLC in the
	//   same sweeping tx.
	// 3. Bob's anchor transaction.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 3

	// For leased channels, there are three transactions and Bob can't
	// sweep his commitment output yet as it has incurred an additional
	// CLTV due to being the initiator of a script-enforced leased channel.
	// 1. Carol's second level HTLC transaction
	// 2. Bob's second level HTLC transaction for his incoming HTLC since he
	// has learnt the preimage from the mempool.
	// 3. Bob's anchor transaction.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 3

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Assert the transactions are in mempool.
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice := ht.SuspendNode(alice)

	// Mine a block to confirm the expected transactions, including Bob's
	// second level tx.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// For non-anchor channel types, the nursery will handle sweeping the
	// second level output, and it will wait one extra block before
	// sweeping it.
	secondLevelMaturity := uint32(defaultCSV)

	// If this is a channel of the anchor type, we will subtract one block
	// from the default CSV, as the Sweeper will handle the input, and the
	// Sweeper sweeps the input as soon as the lock expires.
	if hasAnchors {
		secondLevelMaturity = defaultCSV - 1
	}

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := secondLevelMaturity

	// At this point, Bob should have his second layer success transaction
	// confirmed.
	ht.AssertNumHTLCsAndStage(bob, aliceChanPoint, 1, 2)

	// The channel between Bob and Carol will still be pending force close
	// if this is a leased channel. In that case, we'd also check the HTLC
	// stages are correct in that channel.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		ht.AssertNumPendingForceClose(bob, 2)
		ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 1)
	} else {
		ht.AssertNumPendingForceClose(bob, 1)
	}

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	require.NoError(ht, restartAlice())

	// If we then mine additional blocks, both Bob and Carol's second level
	// txes will mature, and they should pull the funds.
	ht.MineBlocks(carolSecondLevelCSV)

	// We should now see 2 transactions,
	// 1. Carol's sweeping of her second level tx.
	// 2. Bob's sweeping of his second level tx.
	ht.Miner.AssertNumTxsInMempool(2)

	// Mine one more block to confirm both Bob and Carol's sweeps.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to
	// the additional CLTV. We'll need to mine enough blocks for the
	// timelock to expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		for _, node := range []*node.HarnessNode{alice, bob} {
			ht.AssertNumPendingForceClose(node, 1)
		}

		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := alice.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(numBlocks)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: *bobForceClose, Index: 3,
		}
		aliceCommitSweep := ht.Miner.AssertOutpointInMempool(
			aliceCommitOutpoint,
		).TxHash()
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		bobCommitSweep := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		).TxHash()

		// Confirm their sweeps.
		block := ht.MineBlocksAndAssertNumTxes(1, 2)[0]
		ht.Miner.AssertTxInBlock(block, &aliceCommitSweep)
		ht.Miner.AssertTxInBlock(block, &bobCommitSweep)
	}

	// All nodes should show zero pending and open channels.
	for _, node := range []*node.HarnessNode{alice, bob, carol} {
		ht.AssertNumPendingForceClose(node, 0)
		ht.AssertNodeNumChannels(node, 0)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// runMultiHopHtlcLocalChainClaimBlock runs the test
// `testMuliHopHtlcLocalChainClaim` by extracting the preimage from blocks.
func runMultiHopHtlcLocalChainClaimBlock(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// blocksMined records how many blocks have mined after the creation of
	// the invoice so it can be used to calculate how many more blocks need
	// to be mined to trigger a force close later on.
	var blocksMined uint32

	// At this point, Bob decides that he wants to exit the channel
	// immediately, so he force closes his commitment transaction.
	closeStream, _ := ht.CloseChannelAssertPending(
		bob, aliceChanPoint, true,
	)

	// For anchor channels, the anchor won't be used for CPFP as there's no
	// deadline pressure for Bob on the channel Alice->Bob at the moment.
	// For Bob's local commitment tx, there's only one incoming HTLC which
	// he doesn't have the preimage yet. Thus this anchor won't be
	// force-swept.
	hasAnchorSweep := false
	bobForceClose := ht.AssertStreamChannelForceClosed(
		bob, aliceChanPoint, hasAnchorSweep, closeStream,
	)

	// Increase the blocks mined. At this step
	// AssertStreamChannelForceClosed mines one block.
	blocksMined++

	var expectedTxes int
	switch c {
	// Alice will sweep her commitment output immediately.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 1

	// Alice will sweep her commitment and anchor output immediately. Bob
	// will also sweep his anchor.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 3

	// Alice will sweep her anchor output immediately. Her commitment
	// output cannot be swept yet as it has incurred an additional CLTV due
	// to being the initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Suspend Bob to force Carol to go to chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// We now advance the block height to the point where Carol will force
	// close her channel with Bob, broadcast the closing tx but keep it
	// unconfirmed.
	numBlocks := padCLTV(uint32(invoiceReq.CltvExpiry -
		lncfg.DefaultIncomingBroadcastDelta))
	ht.MineBlocks(numBlocks - blocksMined)

	// Carol's commitment transaction should now be in the mempool. If
	// there is an anchor, Carol will sweep that too.
	if lntest.CommitTypeHasAnchors(c) {
		expectedTxes = 2
	}
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Look up the closing transaction. It should be spending from the
	// funding transaction,
	closingTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Mine a block that should confirm the commit tx, the anchor if
	// present and the coinbase.
	block := ht.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]
	ht.Miner.AssertTxInBlock(block, &closingTxid)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, transactions will be
	// broadcast by both Bob and Carol.
	switch c {
	// For legacy channels, there are two transactions,
	// 1. Carol's second level HTLC transaction
	// 2. Bob sweeps his commitment output.
	// 3. Bob's second level HTLC transaction for his incoming HTLC since he
	// has learnt the preimage from the mempool.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2

	// Carol will broadcast her second level HTLC transaction and Bob will
	// sweep his commitment and anchor outputs.
	// For anchor channels, we'd expect to see three transactions,
	// - Carol's second level HTLC transaction
	// - Bob's sweep tx spending his commitment output
	// - Bob's sweep tx spending two anchor outputs, one from channel Alice
	//   to Bob and the other from channel Bob to Carol.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 3

	// For leased channels, there are two transactions and Bob can't
	// sweep his commitment output yet as it has incurred an additional
	// CLTV due to being the initiator of a script-enforced leased channel.
	// 1. Carol's second level HTLC transaction
	// 3. Bob's anchor transaction.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Assert transactions can be found in the mempool.
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice := ht.SuspendNode(alice)

	// Mine a block to confirm the expected transactions (+ the coinbase).
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// For non-anchor channel types, the nursery will handle sweeping the
	// second level output, and it will wait one extra block before
	// sweeping it.
	secondLevelMaturity := uint32(defaultCSV)

	// If this is a channel of the anchor type, we will subtract one block
	// from the default CSV, as the Sweeper will handle the input, and the
	// Sweeper sweeps the input as soon as the lock expires.
	if lntest.CommitTypeHasAnchors(c) {
		secondLevelMaturity = defaultCSV - 1
	}

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := secondLevelMaturity

	// When Bob notices Carol's second level transaction in the block, he
	// will extract the preimage and broadcast a second level tx to claim
	// the HTLC in his (already closed) channel with Alice.
	bobSecondLvlTx := ht.Miner.GetNumTxsFromMempool(1)[0]
	bobSecondLvlTxid := bobSecondLvlTx.TxHash()

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobSecondLvlTx, *bobForceClose)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation.
	ht.AssertNumHTLCsAndStage(bob, aliceChanPoint, 1, 1)

	// The channel between Bob and Carol will still be pending force close
	// if this is a leased channel. In that case, we'd also check the HTLC
	// stages are correct in that channel.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		ht.AssertNumPendingForceClose(bob, 2)
		ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 1)
	} else {
		ht.AssertNumPendingForceClose(bob, 1)
	}

	// We'll now mine a block which should confirm Bob's second layer
	// transaction.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &bobSecondLvlTxid)

	// Keep track of Bob's second level maturity, and decrement our track
	// of Carol's.
	bobSecondLevelCSV := secondLevelMaturity
	carolSecondLevelCSV--

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	require.NoError(ht, restartAlice())

	// If we then mine 3 additional blocks, Carol's second level tx should
	// mature, and she can pull the funds from it with a sweep tx.
	ht.MineBlocks(carolSecondLevelCSV)
	carolSweep := ht.Miner.AssertNumTxsInMempool(1)[0]

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output.
	bobSecondLevelCSV -= carolSecondLevelCSV
	block = ht.MineBlocksAndAssertNumTxes(bobSecondLevelCSV, 1)[0]
	ht.Miner.AssertTxInBlock(block, carolSweep)

	bobSweep := ht.Miner.GetNumTxsFromMempool(1)[0]
	bobSweepTxid := bobSweep.TxHash()

	// Make sure it spends from the second level tx.
	ht.AssertTxSpendFrom(bobSweep, bobSecondLvlTxid)

	// When we mine one additional block, that will confirm Bob's sweep.
	// Now Bob should have no pending channels anymore, as this just
	// resolved it by the confirmation of the sweep transaction.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &bobSweepTxid)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		for _, node := range []*node.HarnessNode{alice, bob} {
			ht.AssertNumPendingForceClose(node, 1)
		}

		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := alice.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(numBlocks)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: *bobForceClose, Index: 3,
		}
		aliceCommitSweep := ht.Miner.AssertOutpointInMempool(
			aliceCommitOutpoint,
		).TxHash()
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		bobCommitSweep := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		).TxHash()

		// Confirm their sweeps.
		block := ht.MineBlocksAndAssertNumTxes(1, 2)[0]
		ht.Miner.AssertTxInBlock(block, &aliceCommitSweep)
		ht.Miner.AssertTxInBlock(block, &bobCommitSweep)
	}

	// All nodes should show zero pending and open channels.
	for _, node := range []*node.HarnessNode{alice, bob, carol} {
		ht.AssertNumPendingForceClose(node, 0)
		ht.AssertNodeNumChannels(node, 0)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// testMultiHopHtlcRemoteChainClaim tests that in the multi-hop HTLC scenario,
// if the remote party goes to chain while we have an incoming HTLC, then when
// we found out the preimage via the witness beacon, we properly settle the
// HTLC directly on-chain using the preimage in order to ensure that we don't
// lose any funds.
func testMultiHopHtlcRemoteChainClaim(ht *lntest.HarnessTest) {
	testRunner := runMultiHopHtlcRemoteChainClaimMempool

	// For neutrino backend, we need to extract preimage from blocks
	// instead of mempool.
	if ht.IsNeutrinoBackend() {
		testRunner = runMultiHopHtlcRemoteChainClaimBlock
	}

	runMultiHopHtlcClaimTest(ht, testRunner)
}

// runMultiHopHtlcRemoteChainClaimMempool runs the test
// `testMuliHopHtlcRemoteChainClaim` by extracting the preimage from mempool.
func runMultiHopHtlcRemoteChainClaimMempool(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If the channel commitment type is taproot channel, then we'll need
	// to make some manual route hints so Alice can actually find a route.
	//
	// NOTE: this is needed as taproot channels are private, so Carol won't
	// include Bob in her route hints as Bob is a private node.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	hasAnchors := lntest.CommitTypeHasAnchors(c)
	closeStream, _ := ht.CloseChannelAssertPending(
		alice, aliceChanPoint, true,
	)
	aliceForceClose := ht.AssertStreamChannelForceClosed(
		alice, aliceChanPoint, hasAnchors, closeStream,
	)

	// Record how many blocks have mined. At this step
	// AssertStreamChannelForceClosed mines one block.
	blocksMined := uint32(1)

	// Wait for the channel to be marked pending force close.
	ht.AssertChannelPendingForceClose(alice, aliceChanPoint)

	// After AssertStreamChannelForceClosed returns, it has mined a block
	// so now bob will attempt to redeem his anchor commitment (if the
	// channel type is of that type).
	if hasAnchors {
		ht.Miner.AssertNumTxsInMempool(1)
	}

	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Mine enough blocks for Alice to sweep her funds from the
		// force closed channel. AssertStreamChannelForceClosed()
		// already mined a block containing the commitment tx and the
		// commit sweep tx will be broadcast immediately before it can
		// be included in a block, so mine one less than defaultCSV in
		// order to perform mempool assertions.
		ht.MineBlocks(defaultCSV - blocksMined)
		blocksMined = defaultCSV

		// Alice should now sweep her funds.
		ht.Miner.AssertNumTxsInMempool(1)
	}

	// Suspend bob, so Carol is forced to go on chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))
	ht.MineBlocks(numBlocks - blocksMined)

	expectedTxes := 1
	if hasAnchors {
		expectedTxes = 2
	}

	// Carol's commitment transaction should now be in the mempool. If
	// there are anchors, Carol also sweeps her anchor.
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// The closing transaction should be spending from the funding
	// transaction.
	closingTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Mine a block, which should contain: the commitment, possibly an
	// anchor sweep and the coinbase tx.
	block := ht.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]
	ht.Miner.AssertTxInBlock(block, &closingTxid)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, we should expect Bob and
	// Carol to broadcast some transactions depending on the channel
	// commitment type.
	switch c {
	// For legacy channels, there are three transactions,
	// 1. Carol's second level HTLC transaction
	// 2. Bob sweeps his commitment output.
	// 3. Bob's direct spend transaction for his incoming HTLC on Alice's
	// force close tx since he has learnt the preimage from the mempool.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 3

	// For anchor channels, there are three transactions,
	// 1. Carol's second level HTLC transaction
	// 2. Bob sweeps his commitment output.
	// 3. Bob's direct spend transaction for his incoming HTLC on Alice's
	// force close tx since he has learnt the preimage from the mempool.
	// 4. Bob's anchor transaction.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 4

	// For leased channels, there are three transactions and Bob can't
	// sweep his commitment output yet as it has incurred an additional
	// CLTV due to being the initiator of a script-enforced leased channel.
	// 1. Carol's second level HTLC transaction
	// 2. Bob's direct spend transaction for his incoming HTLC on Alice's
	// force close tx since he has learnt the preimage from the mempool.
	// 3. Bob's anchor transaction.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 3

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Assert the transactions are in mempool.
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Mine a block to confirm the above transactions (+ coinbase).
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := uint32(defaultCSV)

	// Now that the sweeping transaction has been confirmed, Bob should now
	// recognize that all contracts for the Bob-Carol channel have been
	// fully resolved
	aliceBobPendingChansLeft := 0
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		aliceBobPendingChansLeft = 1
	}
	for _, node := range []*node.HarnessNode{alice, bob} {
		ht.AssertNumPendingForceClose(
			node, aliceBobPendingChansLeft,
		)
	}

	// If we then mine additional blocks, Carol's second level tx will
	// mature, and she should pull the funds.
	ht.MineBlocks(carolSecondLevelCSV)
	carolSweep := ht.Miner.AssertNumTxsInMempool(1)[0]

	// When Carol's sweep gets confirmed, she should have no more pending
	// channels.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, carolSweep)
	ht.AssertNumPendingForceClose(carol, 0)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := alice.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(numBlocks)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: *aliceForceClose, Index: 3,
		}
		aliceCommitSweep := ht.Miner.AssertOutpointInMempool(
			aliceCommitOutpoint,
		)
		aliceCommitSweepTxid := aliceCommitSweep.TxHash()
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		bobCommitSweep := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		)
		bobCommitSweepTxid := bobCommitSweep.TxHash()

		// Confirm their sweeps.
		block := ht.MineBlocksAndAssertNumTxes(1, 2)[0]
		ht.Miner.AssertTxInBlock(block, &aliceCommitSweepTxid)
		ht.Miner.AssertTxInBlock(block, &bobCommitSweepTxid)

		ht.MineBlocks(1)

		// Alice and Bob should not show any pending channels anymore
		// as they have been fully resolved.
		for _, node := range []*node.HarnessNode{alice, bob} {
			ht.AssertNumPendingForceClose(node, 0)
		}
	}

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	invoice := ht.AssertInvoiceState(stream, lnrpc.Invoice_SETTLED)
	require.Equal(ht, int64(invoiceAmt), invoice.AmtPaidSat)

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// runMultiHopHtlcRemoteChainClaimBlock runs the test
// `testMuliHopHtlcRemoteChainClaim` by extracting the preimage from blocks.
func runMultiHopHtlcRemoteChainClaimBlock(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// blocksMined records how many blocks have mined after the creation of
	// the invoice so it can be used to calculate how many more blocks need
	// to be mined to trigger a force close later on.
	var blocksMined uint32

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	hasAnchors := lntest.CommitTypeHasAnchors(c)
	closeStream, _ := ht.CloseChannelAssertPending(
		alice, aliceChanPoint, true,
	)
	aliceForceClose := ht.AssertStreamChannelForceClosed(
		alice, aliceChanPoint, hasAnchors, closeStream,
	)

	// Increase the blocks mined. At this step
	// AssertStreamChannelForceClosed mines one block.
	blocksMined++

	// Wait for the channel to be marked pending force close.
	ht.AssertChannelPendingForceClose(alice, aliceChanPoint)

	// After AssertStreamChannelForceClosed returns, it has mined a block
	// so now bob will attempt to redeem his anchor commitment (if the
	// channel type is of that type).
	if hasAnchors {
		ht.Miner.AssertNumTxsInMempool(1)
	}

	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Mine enough blocks for Alice to sweep her funds from the
		// force closed channel. AssertStreamChannelForceClosed()
		// already mined a block containing the commitment tx and the
		// commit sweep tx will be broadcast immediately before it can
		// be included in a block, so mine one less than defaultCSV in
		// order to perform mempool assertions.
		ht.MineBlocks(defaultCSV - 1)
		blocksMined += (defaultCSV - 1)

		// Alice should now sweep her funds.
		ht.Miner.AssertNumTxsInMempool(1)
	}

	// Suspend bob, so Carol is forced to go on chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))
	ht.MineBlocks(numBlocks - blocksMined)

	expectedTxes := 1
	if hasAnchors {
		expectedTxes = 2
	}

	// Carol's commitment transaction should now be in the mempool. If
	// there are anchors, Carol also sweeps her anchor.
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// The closing transaction should be spending from the funding
	// transaction.
	closingTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Mine a block, which should contain: the commitment, possibly an
	// anchor sweep and the coinbase tx.
	block := ht.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]
	ht.Miner.AssertTxInBlock(block, &closingTxid)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, we should expect Bob and
	// Carol to broadcast some transactions depending on the channel
	// commitment type.
	switch c {
	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a transaction to sweep his commitment output.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a transaction to sweep his commitment output and
	// another to sweep his anchor output.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		expectedTxes = 3

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a transaction to sweep his anchor output. Bob can't
	// sweep his commitment output yet as he has incurred an additional CLTV
	// due to being the channel initiator of a force closed script-enforced
	// leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}
	txes := ht.Miner.GetNumTxsFromMempool(expectedTxes)

	// All transactions should be pending from the commitment transaction.
	ht.AssertAllTxesSpendFrom(txes, closingTxid)

	// Mine a block to confirm the two transactions (+ coinbase).
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := uint32(defaultCSV)

	// When Bob notices Carol's second level transaction in the block, he
	// will extract the preimage and broadcast a sweep tx to directly claim
	// the HTLC in his (already closed) channel with Alice.
	bobHtlcSweep := ht.Miner.GetNumTxsFromMempool(1)[0]
	bobHtlcSweepTxid := bobHtlcSweep.TxHash()

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobHtlcSweep, *aliceForceClose)

	// We'll now mine a block which should confirm Bob's HTLC sweep
	// transaction.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &bobHtlcSweepTxid)
	carolSecondLevelCSV--

	// Now that the sweeping transaction has been confirmed, Bob should now
	// recognize that all contracts for the Bob-Carol channel have been
	// fully resolved
	aliceBobPendingChansLeft := 0
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		aliceBobPendingChansLeft = 1
	}
	for _, node := range []*node.HarnessNode{alice, bob} {
		ht.AssertNumPendingForceClose(
			node, aliceBobPendingChansLeft,
		)
	}

	// If we then mine 3 additional blocks, Carol's second level tx will
	// mature, and she should pull the funds.
	ht.MineBlocks(carolSecondLevelCSV)
	carolSweep := ht.Miner.AssertNumTxsInMempool(1)[0]

	// When Carol's sweep gets confirmed, she should have no more pending
	// channels.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, carolSweep)
	ht.AssertNumPendingForceClose(carol, 0)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := alice.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(numBlocks)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: *aliceForceClose, Index: 3,
		}
		aliceCommitSweep := ht.Miner.AssertOutpointInMempool(
			aliceCommitOutpoint,
		)
		aliceCommitSweepTxid := aliceCommitSweep.TxHash()
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		bobCommitSweep := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		)
		bobCommitSweepTxid := bobCommitSweep.TxHash()

		// Confirm their sweeps.
		block := ht.MineBlocksAndAssertNumTxes(1, 2)[0]
		ht.Miner.AssertTxInBlock(block, &aliceCommitSweepTxid)
		ht.Miner.AssertTxInBlock(block, &bobCommitSweepTxid)

		// Alice and Bob should not show any pending channels anymore
		// as they have been fully resolved.
		for _, node := range []*node.HarnessNode{alice, bob} {
			ht.AssertNumPendingForceClose(node, 0)
		}
	}

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	invoice := ht.AssertInvoiceState(stream, lnrpc.Invoice_SETTLED)
	require.Equal(ht, int64(invoiceAmt), invoice.AmtPaidSat)

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// testMultiHopHtlcAggregation tests that in a multi-hop HTLC scenario, if we
// force close a channel with both incoming and outgoing HTLCs, we can properly
// resolve them using the second level timeout and success transactions. In
// case of anchor channels, the second-level spends can also be aggregated and
// properly feebumped, so we'll check that as well.
func testMultiHopHtlcAggregation(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runMultiHopHtlcAggregation)
}

func runMultiHopHtlcAggregation(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// For neutrino backend, we need one additional UTXO to create
	// the sweeping tx for the second-level success txes.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	}

	// First, we'll create a three hop network: Alice -> Bob -> Carol.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice+Carol can actually find a route.
	var (
		carolRouteHints []*lnrpc.RouteHint
		aliceRouteHints []*lnrpc.RouteHint
	)
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		carolRouteHints = makeRouteHints(bob, carol, zeroConf)
		aliceRouteHints = makeRouteHints(bob, alice, zeroConf)
	}

	// To ensure we have capacity in both directions of the route, we'll
	// make a fairly large payment Alice->Carol and settle it.
	const reBalanceAmt = 500_000
	invoice := &lnrpc.Invoice{
		Value:      reBalanceAmt,
		RouteHints: carolRouteHints,
	}
	resp := carol.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(alice, []string{resp.PaymentRequest})

	// Make sure Carol has settled the invoice.
	ht.AssertInvoiceSettled(carol, resp.PaymentAddr)

	// With the network active, we'll now add a new hodl invoices at both
	// Alice's and Carol's end. Make sure the cltv expiry delta is large
	// enough, otherwise Bob won't send out the outgoing htlc.
	const numInvoices = 5
	const invoiceAmt = 50_000

	var (
		carolInvoices       []*invoicesrpc.AddHoldInvoiceResp
		aliceInvoices       []*invoicesrpc.AddHoldInvoiceResp
		alicePreimages      []lntypes.Preimage
		payHashes           [][]byte
		invoiceStreamsCarol []rpc.SingleInvoiceClient
		invoiceStreamsAlice []rpc.SingleInvoiceClient
	)

	// Add Carol invoices.
	for i := 0; i < numInvoices; i++ {
		var preimage lntypes.Preimage
		copy(preimage[:], ht.Random32Bytes())
		payHash := preimage.Hash()
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Value:      invoiceAmt,
			CltvExpiry: finalCltvDelta,
			Hash:       payHash[:],
			RouteHints: carolRouteHints,
		}
		carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

		carolInvoices = append(carolInvoices, carolInvoice)
		payHashes = append(payHashes, payHash[:])

		// Subscribe the invoice.
		stream := carol.RPC.SubscribeSingleInvoice(payHash[:])
		invoiceStreamsCarol = append(invoiceStreamsCarol, stream)
	}

	// We'll give Alice's invoices a longer CLTV expiry, to ensure the
	// channel Bob<->Carol will be closed first.
	for i := 0; i < numInvoices; i++ {
		var preimage lntypes.Preimage
		copy(preimage[:], ht.Random32Bytes())
		payHash := preimage.Hash()
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Value:      invoiceAmt,
			CltvExpiry: thawHeightDelta - 4,
			Hash:       payHash[:],
			RouteHints: aliceRouteHints,
		}
		aliceInvoice := alice.RPC.AddHoldInvoice(invoiceReq)

		aliceInvoices = append(aliceInvoices, aliceInvoice)
		alicePreimages = append(alicePreimages, preimage)
		payHashes = append(payHashes, payHash[:])

		// Subscribe the invoice.
		stream := alice.RPC.SubscribeSingleInvoice(payHash[:])
		invoiceStreamsAlice = append(invoiceStreamsAlice, stream)
	}

	// Now that we've created the invoices, we'll pay them all from
	// Alice<->Carol, going through Bob. We won't wait for the response
	// however, as neither will immediately settle the payment.

	// Alice will pay all of Carol's invoices.
	for _, carolInvoice := range carolInvoices {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: carolInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		alice.RPC.SendPayment(req)
	}

	// And Carol will pay Alice's.
	for _, aliceInvoice := range aliceInvoices {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: aliceInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		carol.RPC.SendPayment(req)
	}

	// At this point, all 3 nodes should now the HTLCs active on their
	// channels.
	ht.AssertActiveHtlcs(alice, payHashes...)
	ht.AssertActiveHtlcs(bob, payHashes...)
	ht.AssertActiveHtlcs(carol, payHashes...)

	// Wait for Alice and Carol to mark the invoices as accepted. There is
	// a small gap to bridge between adding the htlc to the channel and
	// executing the exit hop logic.
	for _, stream := range invoiceStreamsCarol {
		ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)
	}

	for _, stream := range invoiceStreamsAlice {
		ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)
	}

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We want Carol's htlcs to expire off-chain to demonstrate bob's force
	// close. However, Carol will cancel her invoices to prevent force
	// closes, so we shut her down for now.
	restartCarol := ht.SuspendNode(carol)

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the Carol's HTLCs are
	// about to timeout. With the default outgoing broadcast delta of zero,
	// this will be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	ht.MineBlocks(numBlocks)

	// Bob's force close transaction should now be found in the mempool. If
	// there are anchors, we also expect Bob's anchor sweep.
	hasAnchors := lntest.CommitTypeHasAnchors(c)
	expectedTxes := 1
	if hasAnchors {
		expectedTxes = 2
	}
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	closeTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closeTxid := closeTx.TxHash()

	// Restart Bob to increase the batch window duration so the sweeper
	// will aggregate all the pending inputs.
	ht.RestartNodeWithExtraArgs(
		bob, []string{"--sweeper.batchwindowduration=15s"},
	)

	// Go through the closing transaction outputs, and make an index for
	// the HTLC outputs.
	successOuts := make(map[wire.OutPoint]struct{})
	timeoutOuts := make(map[wire.OutPoint]struct{})
	for i, txOut := range closeTx.TxOut {
		op := wire.OutPoint{
			Hash:  closeTxid,
			Index: uint32(i),
		}

		switch txOut.Value {
		// If this HTLC goes towards Carol, Bob will claim it with a
		// timeout Tx. In this case the value will be the invoice
		// amount.
		case invoiceAmt:
			timeoutOuts[op] = struct{}{}

		// If the HTLC has direction towards Alice, Bob will claim it
		// with the success TX when he learns the preimage. In this
		// case one extra sat will be on the output, because of the
		// routing fee.
		case invoiceAmt + 1:
			successOuts[op] = struct{}{}
		}
	}

	// Once bob has force closed, we can restart carol.
	require.NoError(ht, restartCarol())

	// Mine a block to confirm the closing transaction.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// Let Alice settle her invoices. When Bob now gets the preimages, he
	// has no other option than to broadcast his second-level transactions
	// to claim the money.
	for _, preimage := range alicePreimages {
		alice.RPC.SettleInvoice(preimage[:])
	}

	switch c {
	// With the closing transaction confirmed, we should expect Bob's HTLC
	// timeout transactions to be broadcast due to the expiry being reached.
	// We will also expect the success transactions, since he learnt the
	// preimages from Alice. We also expect Carol to sweep her commitment
	// output.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2*numInvoices + 1

	// In case of anchors, all success transactions will be aggregated into
	// one, the same is the case for the timeout transactions. In this case
	// Carol will also sweep her commitment and anchor output as separate
	// txs (since it will be low fee).
	case lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		lnrpc.CommitmentType_SIMPLE_TAPROOT:

		expectedTxes = 4

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}
	txes := ht.Miner.GetNumTxsFromMempool(expectedTxes)

	// Since Bob can aggregate the transactions, we expect a single
	// transaction, that have multiple spends from the commitment.
	var (
		timeoutTxs []*chainhash.Hash
		successTxs []*chainhash.Hash
	)
	for _, tx := range txes {
		txid := tx.TxHash()

		for i := range tx.TxIn {
			prevOp := tx.TxIn[i].PreviousOutPoint
			if _, ok := successOuts[prevOp]; ok {
				successTxs = append(successTxs, &txid)

				break
			}

			if _, ok := timeoutOuts[prevOp]; ok {
				timeoutTxs = append(timeoutTxs, &txid)

				break
			}
		}
	}

	// In case of anchor we expect all the timeout and success second
	// levels to be aggregated into one tx. For earlier channel types, they
	// will be separate transactions.
	if hasAnchors {
		require.Len(ht, timeoutTxs, 1)
		require.Len(ht, successTxs, 1)
	} else {
		require.Len(ht, timeoutTxs, numInvoices)
		require.Len(ht, successTxs, numInvoices)
	}

	// All mempool transactions should be spending from the commitment
	// transaction.
	ht.AssertAllTxesSpendFrom(txes, closeTxid)

	// Mine a block to confirm the all the transactions, including Carol's
	// commitment tx, anchor tx(optional), and the second-level timeout and
	// success txes.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation,
	// or to the sweeper for sweeping.
	ht.AssertNumPendingForceClose(bob, 1)

	// For this channel, we also check the number of HTLCs and the stage
	// are correct.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, numInvoices*2, 2)

	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// If we then mine additional blocks, Bob can sweep his
		// commitment output.
		ht.MineBlocks(defaultCSV - 2)

		// Find the commitment sweep.
		bobCommitSweep := ht.Miner.GetNumTxsFromMempool(1)[0]
		ht.AssertTxSpendFrom(bobCommitSweep, closeTxid)

		// Also ensure it is not spending from any of the HTLC output.
		for _, txin := range bobCommitSweep.TxIn {
			for _, timeoutTx := range timeoutTxs {
				require.NotEqual(ht, *timeoutTx,
					txin.PreviousOutPoint.Hash,
					"found unexpected spend of timeout tx")
			}

			for _, successTx := range successTxs {
				require.NotEqual(ht, *successTx,
					txin.PreviousOutPoint.Hash,
					"found unexpected spend of success tx")
			}
		}
	}

	// We now restart Bob with a much larger batch window duration since it
	// takes some time to aggregate all the 10 inputs below.
	ht.RestartNodeWithExtraArgs(
		bob, []string{"--sweeper.batchwindowduration=45s"},
	)

	switch c {
	// In case this is a non-anchor channel type, we must mine 2 blocks, as
	// the nursery waits an extra block before sweeping. Before the blocks
	// are mined, we should expect to see Bob's commit sweep in the mempool.
	case lnrpc.CommitmentType_LEGACY:
		ht.MineBlocksAndAssertNumTxes(2, 1)

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output. Before the blocks are mined, we should expect
	// to see Bob's commit sweep in the mempool.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		ht.MineBlocksAndAssertNumTxes(1, 1)

	// Since Bob is the initiator of the Bob-Carol script-enforced leased
	// channel, he incurs an additional CLTV when sweeping outputs back to
	// his wallet. We'll need to mine enough blocks for the timelock to
	// expire to prompt his broadcast.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		resp := bob.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)

		// Add debug log.
		_, height := ht.Miner.GetBestBlock()
		bob.AddToLogf("itest: now mine %d blocks at height %d",
			numBlocks, height)
		ht.MineBlocks(numBlocks)

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Make sure it spends from the second level tx.
	secondLevelSweep := ht.Miner.GetNumTxsFromMempool(1)[0]
	bobSweep := secondLevelSweep.TxHash()

	// It should be sweeping all the second-level outputs.
	var secondLvlSpends int
	for _, txin := range secondLevelSweep.TxIn {
		for _, timeoutTx := range timeoutTxs {
			if *timeoutTx == txin.PreviousOutPoint.Hash {
				secondLvlSpends++
			}
		}

		for _, successTx := range successTxs {
			if *successTx == txin.PreviousOutPoint.Hash {
				secondLvlSpends++
			}
		}
	}

	require.Equal(ht, 2*numInvoices, secondLvlSpends)

	// When we mine one additional block, that will confirm Bob's second
	// level sweep.  Now Bob should have no pending channels anymore, as
	// this just resolved it by the confirmation of the sweep transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &bobSweep)
	ht.AssertNumPendingForceClose(bob, 0)

	// THe channel with Alice is still open.
	ht.AssertNodeNumChannels(bob, 1)

	// Carol should have no channels left (open nor pending).
	ht.AssertNumPendingForceClose(carol, 0)
	ht.AssertNodeNumChannels(carol, 0)

	// Coop close, no anchors.
	ht.CloseChannel(alice, aliceChanPoint)
}

// createThreeHopNetwork creates a topology of `Alice -> Bob -> Carol`.
func createThreeHopNetwork(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, carolHodl bool, c lnrpc.CommitmentType,
	zeroConf bool) (*lnrpc.ChannelPoint,
	*lnrpc.ChannelPoint, *node.HarnessNode) {

	ht.EnsureConnected(alice, bob)

	// We'll create a new node "carol" and have Bob connect to her.
	// If the carolHodl flag is set, we'll make carol always hold onto the
	// HTLC, this way it'll force Bob to go to chain to resolve the HTLC.
	carolFlags := lntest.NodeArgsForCommitType(c)
	if carolHodl {
		carolFlags = append(carolFlags, "--hodl.exit-settle")
	}

	if zeroConf {
		carolFlags = append(
			carolFlags, "--protocol.option-scid-alias",
			"--protocol.zero-conf",
		)
	}
	carol := ht.NewNode("Carol", carolFlags)

	ht.ConnectNodes(bob, carol)

	// Make sure there are enough utxos for anchoring. Because the anchor
	// by itself often doesn't meet the dust limit, a utxo from the wallet
	// needs to be attached as an additional input. This can still lead to
	// a positively-yielding transaction.
	for i := 0; i < 2; i++ {
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, alice)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, bob)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)

		// Mine 1 block to get the above coins confirmed.
		ht.MineBlocks(1)
	}

	// We'll start the test by creating a channel between Alice and Bob,
	// which will act as the first leg for out multi-hop HTLC.
	const chanAmt = 1000000
	var aliceFundingShim *lnrpc.FundingShim
	var thawHeight uint32
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		_, minerHeight := ht.Miner.GetBestBlock()
		thawHeight = uint32(minerHeight + thawHeightDelta)
		aliceFundingShim, _ = deriveFundingShim(
			ht, alice, bob, chanAmt, thawHeight, true, c,
		)
	}

	var privateChan bool
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		privateChan = true
	}

	aliceParams := lntest.OpenChannelParams{
		Private:        privateChan,
		Amt:            chanAmt,
		CommitmentType: c,
		FundingShim:    aliceFundingShim,
		ZeroConf:       zeroConf,
	}

	// If the channel type is taproot, then use an explicit channel type to
	// open it.
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		aliceParams.CommitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT
	}

	// We'll create a channel from Bob to Carol. After this channel is
	// open, our topology looks like:  A -> B -> C.
	var bobFundingShim *lnrpc.FundingShim
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		bobFundingShim, _ = deriveFundingShim(
			ht, bob, carol, chanAmt, thawHeight, true, c,
		)
	}

	// Prepare params for Bob.
	bobParams := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        privateChan,
		CommitmentType: c,
		FundingShim:    bobFundingShim,
		ZeroConf:       zeroConf,
	}

	// If the channel type is taproot, then use an explicit channel type to
	// open it.
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		bobParams.CommitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT
	}

	var (
		acceptStreamBob   rpc.AcceptorClient
		acceptStreamCarol rpc.AcceptorClient
		cancelBob         context.CancelFunc
		cancelCarol       context.CancelFunc
	)

	// If a zero-conf channel is being opened, the nodes are signalling the
	// zero-conf feature bit. Setup a ChannelAcceptor for the fundee.
	if zeroConf {
		acceptStreamBob, cancelBob = bob.RPC.ChannelAcceptor()
		go acceptChannel(ht.T, true, acceptStreamBob)

		acceptStreamCarol, cancelCarol = carol.RPC.ChannelAcceptor()
		go acceptChannel(ht.T, true, acceptStreamCarol)
	}

	// Open channels in batch to save blocks mined.
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: aliceParams},
		{Local: bob, Remote: carol, Param: bobParams},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)
	aliceChanPoint := resp[0]
	bobChanPoint := resp[1]

	// Remove the ChannelAcceptor for Bob and Carol.
	if zeroConf {
		cancelBob()
		cancelCarol()
	}

	// Make sure alice and carol know each other's channels.
	//
	// We'll only do this though if it wasn't a private channel we opened
	// earlier.
	if !privateChan {
		ht.AssertTopologyChannelOpen(alice, bobChanPoint)
		ht.AssertTopologyChannelOpen(carol, aliceChanPoint)
	} else {
		// Otherwise, we want to wait for all the channels to be shown
		// as active before we proceed.
		ht.AssertChannelExists(alice, aliceChanPoint)
		ht.AssertChannelExists(carol, bobChanPoint)
	}

	return aliceChanPoint, bobChanPoint, carol
}

// testHtlcTimeoutResolverExtractPreimageRemote tests that in the multi-hop
// setting, Alice->Bob->Carol, when Bob's outgoing HTLC is swept by Carol using
// the 2nd level success tx2nd level success tx, Bob's timeout resolver will
// extract the preimage from the sweep tx found in mempool or blocks(for
// neutrino). The 2nd level success tx is broadcast by Carol and spends the
// outpoint on her commit tx.
func testHtlcTimeoutResolverExtractPreimageRemote(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runExtraPreimageFromRemoteCommit)
}

// runExtraPreimageFromRemoteCommit checks that Bob's htlc timeout resolver
// will extract the preimage from the 2nd level success tx broadcast by Carol
// which spends the htlc output on her commitment tx.
func runExtraPreimageFromRemoteCommit(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      100_000,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	eveInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: eveInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the payment sent, Alice should have one outgoing HTLC active.
	ht.AssertOutgoingHTLCActive(alice, aliceChanPoint, payHash[:])

	// Bob should have two HTLCs active. One incoming HTLC from Alice, and
	// one outgoing to Carol.
	ht.AssertIncomingHTLCActive(bob, aliceChanPoint, payHash[:])
	htlc := ht.AssertOutgoingHTLCActive(bob, bobChanPoint, payHash[:])

	// Carol should have one incoming HTLC from Bob.
	ht.AssertIncomingHTLCActive(carol, bobChanPoint, payHash[:])

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Bob now goes offline so the link between Bob and Carol is broken.
	restartBob := ht.SuspendNode(bob)

	// Carol now settles the invoice, since her link with Bob is broken,
	// Bob won't know the preimage.
	carol.RPC.SettleInvoice(preimage[:])

	// We'll now mine enough blocks to trigger Carol's broadcast of her
	// commitment transaction due to the fact that the HTLC is about to
	// timeout. With the default incoming broadcast delta of 10, this
	// will be the htlc expiry height minus 10.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))
	ht.MineBlocks(numBlocks)

	// Carol's force close transaction should now be found in the mempool.
	// If there are anchors, we also expect Carol's anchor sweep. We now
	// mine a block to confirm Carol's closing transaction.
	ht.MineClosingTx(bobChanPoint, c)

	// With the closing transaction confirmed, we should expect Carol's
	// HTLC success transaction to be broadcast.
	ht.Miner.AssertNumTxsInMempool(1)

	// Restart Bob. Once he finishes syncing the channel state, he should
	// notice the force close from Carol.
	require.NoError(ht, restartBob())

	// Get the current height to compute number of blocks to mine to
	// trigger the htlc timeout resolver from Bob.
	_, height := ht.Miner.GetBestBlock()

	// We'll now mine enough blocks to trigger Bob's timeout resolver.
	numBlocks = htlc.ExpirationHeight - uint32(height) -
		lncfg.DefaultOutgoingBroadcastDelta

	// We should now have Carol's htlc success tx in the mempool.
	numTxesMempool := 1
	ht.Miner.AssertNumTxsInMempool(numTxesMempool)

	// For neutrino backend, the timeout resolver needs to extract the
	// preimage from the blocks.
	if ht.IsNeutrinoBackend() {
		// Mine a block to confirm Carol's 2nd level success tx.
		ht.MineBlocksAndAssertNumTxes(1, 1)
		numBlocks--
	}

	// Mine empty blocks so Carol's htlc success tx stays in mempool. Once
	// the height is reached, Bob's timeout resolver will resolve the htlc
	// by extracing the preimage from the mempool.
	ht.MineEmptyBlocks(int(numBlocks))

	// Finally, check that the Alice's payment is marked as succeeded as
	// Bob has settled the htlc using the preimage extracted from Carol's
	// 2nd level success tx.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	switch c {
	// For non-anchor channel type, we should expect to see Bob's commit
	// sweep in the mempool.
	case lnrpc.CommitmentType_LEGACY:
		numTxesMempool++

	// For anchor channel type, we should expect to see Bob's commit sweep
	// and his anchor sweep tx in the mempool.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		numTxesMempool += 2

	// For script-enforced leased channel, we should expect to see Bob's
	// anchor sweep tx in the mempool.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		numTxesMempool++
	}

	// Mine a block to clean the mempool.
	ht.MineBlocksAndAssertNumTxes(1, numTxesMempool)

	// NOTE: for non-standby nodes there's no need to clean up the force
	// close as long as the mempool is cleaned.
	ht.CleanShutDown()
}

// testHtlcTimeoutResolverExtractPreimage tests that in the multi-hop setting,
// Alice->Bob->Carol, when Bob's outgoing HTLC is swept by Carol using the
// direct preimage spend, Bob's timeout resolver will extract the preimage from
// the sweep tx found in mempool or blocks(for neutrino). The direct spend tx
// is broadcast by Carol and spends the outpoint on Bob's commit tx.
func testHtlcTimeoutResolverExtractPreimageLocal(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runExtraPreimageFromLocalCommit)
}

// runExtraPreimageFromLocalCommit checks that Bob's htlc timeout resolver will
// extract the preimage from the direct spend broadcast by Carol which spends
// the htlc output on Bob's commitment tx.
func runExtraPreimageFromLocalCommit(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      100_000,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	eveInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: eveInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the payment sent, Alice should have one outgoing HTLC active.
	ht.AssertOutgoingHTLCActive(alice, aliceChanPoint, payHash[:])

	// Bob should have two HTLCs active. One incoming HTLC from Alice, and
	// one outgoing to Carol.
	ht.AssertIncomingHTLCActive(bob, aliceChanPoint, payHash[:])
	htlc := ht.AssertOutgoingHTLCActive(bob, bobChanPoint, payHash[:])

	// Carol should have one incoming HTLC from Bob.
	ht.AssertIncomingHTLCActive(carol, bobChanPoint, payHash[:])

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Bob now goes offline so the link between Bob and Carol is broken.
	restartBob := ht.SuspendNode(bob)

	// Carol now settles the invoice, since her link with Bob is broken,
	// Bob won't know the preimage.
	carol.RPC.SettleInvoice(preimage[:])

	// Stop Carol so it's easier to check the mempool's state since she
	// will broadcast the anchor sweeping once Bob force closes.
	restartCarol := ht.SuspendNode(carol)

	// Restart Bob to force close the channel.
	require.NoError(ht, restartBob())

	// Bob force closes the channel, which gets his commitment tx into the
	// mempool.
	ht.CloseChannelAssertPending(bob, bobChanPoint, true)

	// Mine Bob's force close tx.
	closeTx := ht.MineClosingTx(bobChanPoint, c)

	// We'll now mine enough blocks to trigger Carol's sweeping of the htlc
	// via the direct spend. With the default incoming broadcast delta of
	// 10, this will be the htlc expiry height minus 10.
	//
	// NOTE: we need to mine 1 fewer block as we've already mined one to
	// confirm Bob's force close tx.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta - 1,
	))

	// Mine empty blocks so it's easier to check Bob's sweeping txes below.
	ht.MineEmptyBlocks(int(numBlocks))

	// Increase the fee rate used by the sweeper so Carol's direct spend tx
	// won't be replaced by Bob's timeout tx.
	ht.SetFeeEstimate(30000)

	// Restart Carol to sweep the htlc output.
	require.NoError(ht, restartCarol())

	// Construct the htlc output on Bob's commitment tx, and decide its
	// index based on the commit type below.
	htlcOutpoint := wire.OutPoint{Hash: closeTx.TxHash()}

	// Check the current mempool state and we should see,
	// - Carol's direct spend tx.
	// - Bob's local output sweep tx, if this is NOT script enforced lease.
	// - Carol's anchor sweep tx, if the commitment type is anchor.
	switch c {
	case lnrpc.CommitmentType_LEGACY:
		htlcOutpoint.Index = 0
		ht.Miner.AssertNumTxsInMempool(2)

	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		htlcOutpoint.Index = 2
		ht.Miner.AssertNumTxsInMempool(3)

	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		htlcOutpoint.Index = 2
		ht.Miner.AssertNumTxsInMempool(2)
	}

	// Get the current height to compute number of blocks to mine to
	// trigger the timeout resolver from Bob.
	_, height := ht.Miner.GetBestBlock()

	// We'll now mine enough blocks to trigger Bob's htlc timeout resolver
	// to act. Once his timeout resolver starts, it will extract the
	// preimage from Carol's direct spend tx found in the mempool.
	numBlocks = htlc.ExpirationHeight - uint32(height) -
		lncfg.DefaultOutgoingBroadcastDelta

	// Decrease the fee rate used by the sweeper so Bob's timeout tx will
	// not replace Carol's direct spend tx.
	ht.SetFeeEstimate(1000)

	// Mine empty blocks so Carol's direct spend tx stays in mempool. Once
	// the height is reached, Bob's timeout resolver will resolve the htlc
	// by extracing the preimage from the mempool.
	ht.MineEmptyBlocks(int(numBlocks))

	// For neutrino backend, the timeout resolver needs to extract the
	// preimage from the blocks.
	if ht.IsNeutrinoBackend() {
		// Make sure the direct spend tx is still in the mempool.
		ht.Miner.AssertOutpointInMempool(htlcOutpoint)

		// Mine a block to confirm Carol's direct spend tx.
		ht.MineBlocks(1)
	}

	// Finally, check that the Alice's payment is marked as succeeded as
	// Bob has settled the htlc using the preimage extracted from Carol's
	// direct spend tx.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	// NOTE: for non-standby nodes there's no need to clean up the force
	// close as long as the mempool is cleaned.
	ht.CleanShutDown()
}
