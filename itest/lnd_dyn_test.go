package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testDynUpgradeChanParams(ht *lntest.HarnessTest) {
	// Prepare params.
	cfg := []string{
		"--protocol.anchors",
		// TODO: remove this once upgrader has its own msg flow.
		"--channel-commit-interval=1m",
	}
	cfgs := [][]string{cfg, cfg}

	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 10,
	}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)
	cp := chanPoints[0]
	alice, bob := nodes[0], nodes[1]

	bob.RPC.GetInfo()

	dustLimit := uint64(1000)
	csvDelay := uint32(100)

	req := &lnrpc.UpgradeChannelRequest{
		ChannelPoint: cp,
		DustLimit:    &dustLimit,
		CsvDelay:     &csvDelay,
	}
	stream, err := alice.RPC.LN.UpgradeChannel(ht.Context(), req)
	require.NoError(ht, err)

	respChan := make(chan *lnrpc.UpgradeChannelResponse, 1)
	errChan := make(chan error)
	go func() {
		for {
			// Consume one message. This will block until the
			// message is received.
			resp, err := stream.Recv()
			if err != nil {
				errChan <- err
				return
			}
			respChan <- resp
		}
	}()

	for {
		select {
		case <-time.After(time.Second * 5):
			ht.Logf("==========> timeout")
			return

		case err := <-errChan:
			ht.Logf("Received err %v", err)

		case resp := <-respChan:
			ht.Logf("Received resp %v", resp)
		}
	}
}

func testDynUpgradeChanType(ht *lntest.HarnessTest) {
	// Prepare params.
	cfg := []string{
		"--protocol.anchors",
		// TODO: remove this once upgrader has its own msg flow.
		"--channel-commit-interval=1s",
		"--protocol.simple-taproot-chans",
	}
	cfgs := [][]string{cfg, cfg}

	openChannelParams := lntest.OpenChannelParams{
		Amt:            invoiceAmt * 10,
		Private:        true,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
	}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)
	cp := chanPoints[0]
	alice, bob := nodes[0], nodes[1]

	newCommitType := lnrpc.CommitmentType_SIMPLE_TAPROOT

	req := &lnrpc.UpgradeChannelRequest{
		ChannelPoint:   cp,
		CommitmentType: &newCommitType,
	}
	stream, err := alice.RPC.LN.UpgradeChannel(ht.Context(), req)
	require.NoError(ht, err)

	respChan := make(chan *lnrpc.UpgradeChannelResponse, 1)
	errChan := make(chan error)
	go func() {
		for {
			// Consume one message. This will block until the
			// message is received.
			resp, err := stream.Recv()
			if err != nil {
				errChan <- err
				return
			}
			respChan <- resp
		}
	}()

	for {
		select {
		case <-time.After(time.Second * 5):
			ht.Logf("==========> timeout")
			return

		case err := <-errChan:
			ht.Logf("Received err %v", err)

			time.Sleep(2 * time.Second)

			alice.AddToLogf("------------------------------------------------")
			bob.AddToLogf("--------------------------------------------------")

			payAmt := btcutil.Amount(5)
			invoice := &lnrpc.Invoice{
				Memo:  "testing",
				Value: int64(payAmt),
			}
			resp := bob.RPC.AddInvoice(invoice)

			ht.CompletePaymentRequests(
				alice, []string{resp.PaymentRequest},
			)

			return

		case resp := <-respChan:
			ht.Logf("Received resp %v", resp)
		}
	}
}
