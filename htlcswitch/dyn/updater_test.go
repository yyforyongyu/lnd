package dyn

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	// testCapacity is the channel capacity used across the tests. Its fifth
	// (the max reserve) is 200_000 sat.
	testCapacity = btcutil.Amount(1_000_000)

	// testMaxCSV is the maximum acceptable to_self_delay in the tests.
	testMaxCSV = uint16(2016)

	// testNextHeight is the next commitment number the dyn update binds to.
	testNextHeight = uint64(7)
)

// mkConfig builds a ChannelConfig whose dynamic parameters all satisfy BOLT #2
// and LND policy for a testCapacity channel.
func mkConfig(dust btcutil.Amount, csv uint16, reserve btcutil.Amount,
	maxInFlight, minHTLC lnwire.MilliSatoshi,
	maxAccepted uint16) channeldb.ChannelConfig {

	return channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			ChanReserve:      reserve,
			MaxPendingAmount: maxInFlight,
			MinHTLC:          minHTLC,
			MaxAcceptedHtlcs: maxAccepted,
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: dust,
			CsvDelay:  csv,
		},
	}
}

var (
	// baseLocal and baseRemote are fully valid baseline configs.
	baseLocal  = mkConfig(500, 144, 20_000, 500_000, 1_000, 100)
	baseRemote = mkConfig(600, 144, 20_000, 500_000, 1_000, 100)

	testChanID    = lnwire.ChannelID{0x01, 0x02, 0x03}
	testChainHash = chainhash.Hash{0xaa, 0xbb, 0xcc}
)

// fakeView is a configurable ChannelView for tests. Its zero value is not
// eligible; use newFakeView for a fully-eligible starting point.
type fakeView struct {
	chanID     lnwire.ChannelID
	chainHash  chainhash.Hash
	local      channeldb.ChannelConfig
	remote     channeldb.ChannelConfig
	capacity   btcutil.Amount
	nextHeight uint64

	bothReady       bool
	quiescent       bool
	initiator       fn.Result[lntypes.ChannelParty]
	htlcs           bool
	pendingUpdates  bool
	pendingShutdown bool
	pendingSplice   bool
}

// newFakeView returns a fully-eligible view whose quiescence initiator is the
// given proposer.
func newFakeView(proposer lntypes.ChannelParty) *fakeView {
	return &fakeView{
		chanID:     testChanID,
		chainHash:  testChainHash,
		local:      baseLocal,
		remote:     baseRemote,
		capacity:   testCapacity,
		nextHeight: testNextHeight,
		bothReady:  true,
		quiescent:  true,
		initiator:  fn.Ok(proposer),
	}
}

func (f *fakeView) ChanID() lnwire.ChannelID              { return f.chanID }
func (f *fakeView) ChainHash() chainhash.Hash             { return f.chainHash }
func (f *fakeView) LocalChanCfg() channeldb.ChannelConfig { return f.local }
func (f *fakeView) RemoteChanCfg() channeldb.ChannelConfig {
	return f.remote
}
func (f *fakeView) Capacity() btcutil.Amount { return f.capacity }
func (f *fakeView) NextCommitHeight() uint64 { return f.nextHeight }
func (f *fakeView) BothChannelReady() bool   { return f.bothReady }
func (f *fakeView) IsQuiescent() bool        { return f.quiescent }
func (f *fakeView) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	return f.initiator
}
func (f *fakeView) HasHTLCs() bool           { return f.htlcs }
func (f *fakeView) HasPendingUpdates() bool  { return f.pendingUpdates }
func (f *fakeView) HasPendingShutdown() bool { return f.pendingShutdown }
func (f *fakeView) HasPendingSpliceOrRBF() bool {
	return f.pendingSplice
}

// fakeSigner produces and verifies dyn_ack signatures with real keys, so the
// sign/verify round trip is exercised end to end.
type fakeSigner struct {
	signKey   *btcec.PrivateKey
	verifyKey *btcec.PublicKey
	signErr   error
}

func (s *fakeSigner) SignDynAck(chainHash chainhash.Hash,
	chanID lnwire.ChannelID, nextCommitHeight uint64,
	proposalTLVs []byte) (lnwire.Sig, error) {

	if s.signErr != nil {
		return lnwire.Sig{}, s.signErr
	}

	return lnwire.SignDynAck(
		s.signKey, chainHash, chanID, nextCommitHeight, proposalTLVs,
	)
}

func (s *fakeSigner) VerifyDynAck(sig lnwire.Sig, chainHash chainhash.Hash,
	chanID lnwire.ChannelID, nextCommitHeight uint64,
	proposalTLVs []byte) error {

	return lnwire.VerifyDynAck(
		s.verifyKey, sig, chainHash, chanID, nextCommitHeight,
		proposalTLVs,
	)
}

// fakeSender records the messages sent to the peer.
type fakeSender struct {
	mu   sync.Mutex
	sent []lnwire.Message
	err  error
}

func (s *fakeSender) SendMessage(_ context.Context,
	msgs ...lnwire.Message) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}

	s.sent = append(s.sent, msgs...)

	return nil
}

func (s *fakeSender) msgs() []lnwire.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]lnwire.Message, len(s.sent))
	copy(out, s.sent)

	return out
}

// testRig bundles an Updater with its injected fakes for assertions.
type testRig struct {
	u         *Updater
	view      *fakeView
	signer    *fakeSigner
	sender    *fakeSender
	persister *MemPersister
	clk       *clock.TestClock
}

// newRig constructs a rig whose quiescence initiator is the given proposer.
func newRig(t *testing.T, proposer lntypes.ChannelParty) *testRig {
	t.Helper()

	view := newFakeView(proposer)
	sender := &fakeSender{}
	persister := NewMemPersister()
	clk := clock.NewTestClock(time.Unix(0, 0))

	rig := &testRig{
		view:      view,
		signer:    &fakeSigner{},
		sender:    sender,
		persister: persister,
		clk:       clk,
	}

	rig.u = NewUpdater(Config{
		Signer:           rig.signer,
		ChannelView:      view,
		Persister:        persister,
		Sender:           sender,
		Clock:            clk,
		Timeout:          DefaultTimeout,
		MaxLocalCSVDelay: testMaxCSV,
	})

	return rig
}

// validProposal is a valid proposal that changes the proposer's own dust limit.
func validProposal() lnwallet.ChannelParams {
	return lnwallet.ChannelParams{
		DustLimit: fn.Some(btcutil.Amount(700)),
	}
}

// TestLocalInitToReadyToCommit walks the proposer happy path: propose, receive
// a valid ack, and reach the ready-to-commit handoff, then hand off the dance.
func TestLocalInitToReadyToCommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// The responder holds respKey; as proposer we verify with its pubkey.
	respKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rig := newRig(t, lntypes.Local)
	rig.signer.verifyKey = respKey.PubKey()

	// Kick off the proposal.
	tr, err := rig.u.InitProposal(ctx, ProposalRequest{
		Params: validProposal(),
	})
	require.NoError(t, err)
	require.Equal(t, StateAwaitingAck, rig.u.State())
	require.Equal(t, StateIdle, tr.From)
	require.Equal(t, StateAwaitingAck, tr.To)

	// A single dyn_propose must have been sent.
	sent := rig.sender.msgs()
	require.Len(t, sent, 1)
	dp, ok := sent[0].(*lnwire.DynPropose)
	require.True(t, ok)
	require.Equal(t, tr.Messages, sent)

	// The proposal context must be persisted with us as the proposer.
	stored, err := rig.persister.FetchAcceptedProposal(ctx, testChanID)
	require.NoError(t, err)
	require.True(t, stored.IsSome())
	require.Equal(t, lntypes.Local, stored.UnsafeFromSome().Proposer)

	// The responder signs the exact proposal TLVs bound to the height.
	tlvs, err := dp.SerializeTlvData()
	require.NoError(t, err)
	ackSig, err := lnwire.SignDynAck(
		respKey, testChainHash, testChanID, testNextHeight, tlvs,
	)
	require.NoError(t, err)

	// Feed the ack; we should reach Accepted with a ready-to-commit handoff.
	tr, err = rig.u.RecvAck(ctx, &lnwire.DynAck{
		ChanID: testChanID,
		Sig:    ackSig,
	})
	require.NoError(t, err)
	require.Equal(t, StateAccepted, rig.u.State())
	require.False(t, tr.Disconnect)
	require.True(t, tr.Handoff.IsSome())

	handoff := tr.Handoff.UnsafeFromSome()
	require.Equal(t, lntypes.Local, handoff.Proposer)
	require.Equal(t, testNextHeight, handoff.NextCommitHeight)
	require.Equal(t, ackSig, handoff.AckSig)
	require.True(t, handoff.Params.DustLimit.IsSome())
	require.True(t, handoff.ReceivedCommit.IsNone())

	// Finally, hand off the dance, recording the sent ECDSA commit sig.
	sentSig := testSig(t)
	tr, err = rig.u.MarkCommitSent(
		ctx, fn.Some(sentSig), fn.None[lnwire.PartialSigWithNonce](),
	)
	require.NoError(t, err)
	require.Equal(t, StateCommitting, rig.u.State())
	require.True(t, rig.u.State().IsTerminal())

	// The sent commitment signature must have been persisted so a reconnect
	// resolves to retransmit rather than forget.
	stored, err = rig.persister.FetchAcceptedProposal(ctx, testChanID)
	require.NoError(t, err)
	require.True(t, stored.IsSome())
	require.True(t, stored.UnsafeFromSome().HasCommitSig())
	require.Equal(t, fn.Some(sentSig), stored.UnsafeFromSome().CommitSig)
}

// TestRemoteProposalAccept walks the responder happy path: validate an incoming
// proposal, sign and send a dyn_ack, then receive the bundled commit.
func TestRemoteProposalAccept(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// As responder we sign with ourKey.
	ourKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rig := newRig(t, lntypes.Remote)
	rig.signer.signKey = ourKey

	// The peer proposes a change to its own dust limit.
	params := lnwallet.ChannelParams{DustLimit: fn.Some(btcutil.Amount(700))}
	dp := params.ToDynPropose(testChanID)

	tr, err := rig.u.RecvPropose(ctx, dp)
	require.NoError(t, err)
	require.Equal(t, StateAckSent, rig.u.State())

	// A dyn_ack must have been sent, and it must verify under our pubkey.
	sent := rig.sender.msgs()
	require.Len(t, sent, 1)
	ack, ok := sent[0].(*lnwire.DynAck)
	require.True(t, ok)

	tlvs, err := dp.SerializeTlvData()
	require.NoError(t, err)
	require.NoError(t, lnwire.VerifyDynAck(
		ourKey.PubKey(), ack.Sig, testChainHash, testChanID,
		testNextHeight, tlvs,
	))

	// Our acceptance must be persisted with the ack signature.
	stored, err := rig.persister.FetchAcceptedProposal(ctx, testChanID)
	require.NoError(t, err)
	require.True(t, stored.IsSome())
	require.Equal(t, lntypes.Remote, stored.UnsafeFromSome().Proposer)
	require.True(t, stored.UnsafeFromSome().AckSig.IsSome())

	// Now receive the bundled dyn_commit_sig for the same proposal.
	commit := &lnwire.DynCommit{
		DynPropose: *dp,
		CommitSig:  testSig(t),
		AckSig:     ack.Sig,
	}
	tr, err = rig.u.RecvCommit(ctx, commit)
	require.NoError(t, err)
	require.Equal(t, StateCommitting, rig.u.State())
	require.True(t, tr.Handoff.IsSome())

	handoff := tr.Handoff.UnsafeFromSome()
	require.Equal(t, lntypes.Remote, handoff.Proposer)
	require.True(t, handoff.ReceivedCommit.IsSome())
	require.Equal(t, commit, handoff.ReceivedCommit.UnsafeFromSome())

	// The link records the received commitment signature before it sends its
	// revoke_and_ack. Simulate that here and confirm it is persisted so a
	// reconnect resolves to resume rather than forget.
	require.NoError(t, rig.u.RecordCommitReceived(
		ctx, fn.Some(commit.CommitSig),
		fn.None[lnwire.PartialSigWithNonce](),
	))

	stored, err = rig.persister.FetchAcceptedProposal(ctx, testChanID)
	require.NoError(t, err)
	require.True(t, stored.IsSome())
	require.True(t, stored.UnsafeFromSome().HasCommitSig())
	require.Equal(
		t, fn.Some(commit.CommitSig), stored.UnsafeFromSome().CommitSig,
	)
}

// TestRejectInvalidParam checks that a proposal with an out-of-policy parameter
// is rejected with the correct rejection bit set.
func TestRejectInvalidParam(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Remote)

	// A CSV delay above our maximum must be rejected on the CSV bit.
	params := lnwallet.ChannelParams{CsvDelay: fn.Some(uint16(5000))}
	dp := params.ToDynPropose(testChanID)

	tr, err := rig.u.RecvPropose(ctx, dp)
	require.NoError(t, err)
	require.Equal(t, StateRejected, rig.u.State())
	require.True(t, rig.u.State().IsTerminal())

	sent := rig.sender.msgs()
	require.Len(t, sent, 1)
	dr, ok := sent[0].(*lnwire.DynReject)
	require.True(t, ok)
	require.True(t, dr.IsRejected(lnwire.DynRejectCsvDelay))
	require.False(t, dr.IsRejected(lnwire.DynRejectDustLimit))
	require.Equal(t, tr.Messages, sent)
}

// TestInvalidAckSignature checks that a dyn_ack that fails verification fails
// the negotiation and signals disconnect.
func TestInvalidAckSignature(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// The proposer verifies against wrongKey, but the ack is produced with
	// respKey, so verification must fail.
	respKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	wrongKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rig := newRig(t, lntypes.Local)
	rig.signer.verifyKey = wrongKey.PubKey()

	_, err = rig.u.InitProposal(ctx, ProposalRequest{Params: validProposal()})
	require.NoError(t, err)

	dp := rig.sender.msgs()[0].(*lnwire.DynPropose)
	tlvs, err := dp.SerializeTlvData()
	require.NoError(t, err)
	ackSig, err := lnwire.SignDynAck(
		respKey, testChainHash, testChanID, testNextHeight, tlvs,
	)
	require.NoError(t, err)

	tr, err := rig.u.RecvAck(ctx, &lnwire.DynAck{
		ChanID: testChanID,
		Sig:    ackSig,
	})
	require.NoError(t, err)
	require.Equal(t, StateFailed, rig.u.State())
	require.True(t, tr.Disconnect)

	// A failed negotiation must forget its persisted context.
	stored, err := rig.persister.FetchAcceptedProposal(ctx, testChanID)
	require.NoError(t, err)
	require.True(t, stored.IsNone())
}

// TestProposerPreconditions checks that every unmet precondition blocks a local
// proposal with the right typed error and leaves the machine Idle.
func TestProposerPreconditions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name    string
		mutate  func(v *fakeView)
		wantErr error
	}{
		{
			name:    "channel not ready",
			mutate:  func(v *fakeView) { v.bothReady = false },
			wantErr: ErrChannelNotReady,
		},
		{
			name:    "not quiescent",
			mutate:  func(v *fakeView) { v.quiescent = false },
			wantErr: ErrNotQuiescent,
		},
		{
			name: "not quiescence initiator",
			mutate: func(v *fakeView) {
				v.initiator = fn.Ok(lntypes.Remote)
			},
			wantErr: ErrNotQuiescenceInitiator,
		},
		{
			name:    "htlcs active",
			mutate:  func(v *fakeView) { v.htlcs = true },
			wantErr: ErrHTLCsActive,
		},
		{
			name:    "pending updates",
			mutate:  func(v *fakeView) { v.pendingUpdates = true },
			wantErr: ErrPendingUpdates,
		},
		{
			name:    "pending shutdown",
			mutate:  func(v *fakeView) { v.pendingShutdown = true },
			wantErr: ErrPendingShutdown,
		},
		{
			name:    "pending splice",
			mutate:  func(v *fakeView) { v.pendingSplice = true },
			wantErr: ErrPendingSpliceOrRBF,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rig := newRig(t, lntypes.Local)
			tc.mutate(rig.view)

			_, err := rig.u.InitProposal(ctx, ProposalRequest{
				Params: validProposal(),
			})
			require.ErrorIs(t, err, tc.wantErr)
			require.Equal(t, StateIdle, rig.u.State())
			require.Empty(t, rig.sender.msgs())
		})
	}
}

// TestResponderPreconditionReject checks that a precondition failure on the
// accept side yields a bare dyn_reject and moves to Rejected.
func TestResponderPreconditionReject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Remote)
	rig.view.htlcs = true

	dp := validProposal().ToDynPropose(testChanID)
	tr, err := rig.u.RecvPropose(ctx, dp)
	require.NoError(t, err)
	require.Equal(t, StateRejected, rig.u.State())

	sent := rig.sender.msgs()
	require.Len(t, sent, 1)
	dr, ok := sent[0].(*lnwire.DynReject)
	require.True(t, ok)

	// A precondition rejection identifies no specific parameter.
	require.False(t, dr.IsRejected(lnwire.DynRejectDustLimit))
	require.False(t, dr.IsRejected(lnwire.DynRejectCsvDelay))
	require.Equal(t, tr.Messages, sent)
}

// TestTimeout checks that a waiting negotiation fails and signals disconnect
// once its deadline elapses, and is a no-op before then.
func TestTimeout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Local)

	_, err := rig.u.InitProposal(ctx, ProposalRequest{
		Params: validProposal(),
	})
	require.NoError(t, err)
	require.Equal(t, StateAwaitingAck, rig.u.State())

	// Before the deadline, ticking the timeout is a no-op.
	tr, err := rig.u.CheckTimeout(ctx)
	require.NoError(t, err)
	require.Equal(t, StateAwaitingAck, tr.To)
	require.False(t, tr.Disconnect)
	require.Equal(t, StateAwaitingAck, rig.u.State())

	// Advance past the deadline and tick again.
	rig.clk.SetTime(time.Unix(0, 0).Add(DefaultTimeout + time.Second))

	tr, err = rig.u.CheckTimeout(ctx)
	require.NoError(t, err)
	require.Equal(t, StateFailed, rig.u.State())
	require.True(t, tr.Disconnect)

	// The timed-out negotiation must be forgotten.
	stored, err := rig.persister.FetchAcceptedProposal(ctx, testChanID)
	require.NoError(t, err)
	require.True(t, stored.IsNone())
}

// TestDuplicateProposal checks that a second local proposal, and an incoming
// proposal while we are proposing, are both refused.
func TestDuplicateProposal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Local)

	_, err := rig.u.InitProposal(ctx, ProposalRequest{
		Params: validProposal(),
	})
	require.NoError(t, err)

	// A second local proposal is refused.
	_, err = rig.u.InitProposal(ctx, ProposalRequest{
		Params: validProposal(),
	})
	require.ErrorIs(t, err, ErrNegotiationInProgress)

	// An incoming proposal while we are the in-flight proposer is a
	// protocol violation.
	dp := validProposal().ToDynPropose(testChanID)
	_, err = rig.u.RecvPropose(ctx, dp)
	require.ErrorIs(t, err, ErrInvalidTransition)

	// State is unchanged.
	require.Equal(t, StateAwaitingAck, rig.u.State())
}

// TestConcurrentProposals checks that concurrent local proposals are serialized
// so that exactly one wins, with no data race (run with -race).
func TestConcurrentProposals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Local)

	const n = 12
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
	)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, err := rig.u.InitProposal(ctx, ProposalRequest{
				Params: validProposal(),
			})
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()

		// Concurrent readers exercise the read path under the race
		// detector.
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rig.u.State()
		}()
	}

	wg.Wait()

	require.Equal(t, 1, successes)
	require.Equal(t, StateAwaitingAck, rig.u.State())
	require.Len(t, rig.sender.msgs(), 1)
}

// TestInvalidTransitions checks that messages arriving in the wrong state are
// rejected as invalid transitions.
func TestInvalidTransitions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Local)

	// From Idle, an ack/reject/commit/commit-sent are all illegal.
	_, err := rig.u.RecvAck(ctx, &lnwire.DynAck{ChanID: testChanID})
	require.ErrorIs(t, err, ErrInvalidTransition)

	_, err = rig.u.RecvReject(ctx, lnwire.NewDynReject(testChanID))
	require.ErrorIs(t, err, ErrInvalidTransition)

	_, err = rig.u.RecvCommit(ctx, &lnwire.DynCommit{})
	require.ErrorIs(t, err, ErrInvalidTransition)

	_, err = rig.u.MarkCommitSent(
		ctx, fn.None[lnwire.Sig](),
		fn.None[lnwire.PartialSigWithNonce](),
	)
	require.ErrorIs(t, err, ErrInvalidTransition)

	require.Equal(t, StateIdle, rig.u.State())
}

// TestRecvCommitMismatch checks that a bundled commit carrying a different
// proposal than the one we acked fails the negotiation.
func TestRecvCommitMismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	ourKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rig := newRig(t, lntypes.Remote)
	rig.signer.signKey = ourKey

	dp := validProposal().ToDynPropose(testChanID)
	_, err = rig.u.RecvPropose(ctx, dp)
	require.NoError(t, err)
	require.Equal(t, StateAckSent, rig.u.State())

	// Bundle a different proposal than the one we acked.
	other := lnwallet.ChannelParams{CsvDelay: fn.Some(uint16(288))}
	tr, err := rig.u.RecvCommit(ctx, &lnwire.DynCommit{
		DynPropose: *other.ToDynPropose(testChanID),
		CommitSig:  testSig(t),
	})
	require.NoError(t, err)
	require.Equal(t, StateFailed, rig.u.State())
	require.True(t, tr.Disconnect)
}

// TestEmptyProposalRejected checks that a proposal that changes nothing is
// refused at init.
func TestEmptyProposalRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Local)

	_, err := rig.u.InitProposal(ctx, ProposalRequest{})
	require.ErrorIs(t, err, lnwallet.ErrDynParamsNoChange)
	require.Equal(t, StateIdle, rig.u.State())
}

// TestReset returns a terminal machine to Idle and clears persisted state.
func TestReset(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rig := newRig(t, lntypes.Remote)

	// Reject a bad proposal to reach a terminal state.
	bad := lnwallet.ChannelParams{CsvDelay: fn.Some(uint16(5000))}
	_, err := rig.u.RecvPropose(ctx, bad.ToDynPropose(testChanID))
	require.NoError(t, err)
	require.Equal(t, StateRejected, rig.u.State())

	require.NoError(t, rig.u.Reset(ctx))
	require.Equal(t, StateIdle, rig.u.State())

	stored, err := rig.persister.FetchAcceptedProposal(ctx, testChanID)
	require.NoError(t, err)
	require.True(t, stored.IsNone())
}

// testSig returns an arbitrary well-formed signature for use where the content
// of a commitment signature is not under test.
func testSig(t *testing.T) lnwire.Sig {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sig, err := lnwire.SignDynAck(
		priv, testChainHash, testChanID, 0, []byte{0x00},
	)
	require.NoError(t, err)

	return sig
}

// testPartialSig returns an arbitrary well-formed musig2 partial signature with
// nonce for use where the content of a taproot commitment signature is not
// under test.
func testPartialSig(t *testing.T) lnwire.PartialSigWithNonce {
	t.Helper()

	priv1, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	priv2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var nonce [66]byte
	copy(nonce[:33], priv1.PubKey().SerializeCompressed())
	copy(nonce[33:], priv2.PubKey().SerializeCompressed())

	var s btcec.ModNScalar
	s.SetInt(7)

	return *lnwire.NewPartialSigWithNonce(nonce, s)
}
