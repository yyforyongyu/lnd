package itest

import (
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testDyn(ht *lntest.HarnessTest) {
	// Prepare params.
	cfg := []string{
		"--protocol.anchors",
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
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		respChan <- resp
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
