package dyn

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// mkAccepted builds an AcceptedProposal for the reconnect tests with the given
// role and optional dyn_ack / dyn_commit_sig signatures set.
func mkAccepted(t *testing.T, proposer lntypes.ChannelParty,
	ack, commit bool) AcceptedProposal {

	t.Helper()

	p := AcceptedProposal{
		Proposer:         proposer,
		Proposal:         validProposal().ToDynPropose(testChanID),
		NextCommitHeight: testNextHeight,
		AckSig:           fn.None[lnwire.Sig](),
		CommitSig:        fn.None[lnwire.Sig](),
	}
	if ack {
		p.AckSig = fn.Some(testSig(t))
	}
	if commit {
		p.CommitSig = fn.Some(testSig(t))
	}

	return p
}

// TestDecideTable exhaustively exercises the reconnect decision function across
// every meaningful combination of role, dyn_ack presence, dyn_commit_sig
// presence, and the peer's next commitment height, asserting the locked design
// decisions.
func TestDecideTable(t *testing.T) {
	t.Parallel()

	const (
		heightEq   = testNextHeight     // peer still expects the sig.
		heightPast = testNextHeight + 1 // peer already processed the sig.
		heightBack = testNextHeight - 1 // defensive: peer behind us.
	)

	testCases := []struct {
		name     string
		proposer lntypes.ChannelParty
		ack      bool
		commit   bool
		peerNext uint64
		want     ReconnectAction
	}{
		// Pre-dyn_commit_sig: everything is forgotten except a
		// responder that already sent its dyn_ack.
		{
			name:     "proposer pre-ack forgets",
			proposer: lntypes.Local,
			ack:      false,
			commit:   false,
			peerNext: heightEq,
			want:     ReconnectForget,
		},
		{
			name:     "proposer received ack no commitsig forgets",
			proposer: lntypes.Local,
			ack:      true,
			commit:   false,
			peerNext: heightEq,
			want:     ReconnectForget,
		},
		{
			name:     "responder pre-ack forgets",
			proposer: lntypes.Remote,
			ack:      false,
			commit:   false,
			peerNext: heightEq,
			want:     ReconnectForget,
		},
		{
			name:     "responder sent ack retains and waits",
			proposer: lntypes.Remote,
			ack:      true,
			commit:   false,
			peerNext: heightEq,
			want:     ReconnectRetainAndWait,
		},

		// dyn_commit_sig persisted by the proposer: retransmit while the
		// peer still expects it, otherwise resume.
		{
			name:     "proposer commitsig peer expects retransmits",
			proposer: lntypes.Local,
			ack:      true,
			commit:   true,
			peerNext: heightEq,
			want:     ReconnectRetransmitCommitSig,
		},
		{
			name:     "proposer commitsig peer advanced resumes",
			proposer: lntypes.Local,
			ack:      true,
			commit:   true,
			peerNext: heightPast,
			want:     ReconnectResume,
		},
		{
			name:     "proposer commitsig peer behind resumes",
			proposer: lntypes.Local,
			ack:      true,
			commit:   true,
			peerNext: heightBack,
			want:     ReconnectResume,
		},

		// dyn_commit_sig persisted by the responder (received it): the
		// responder never retransmits it, so it always resumes.
		{
			name:     "responder commitsig peer expects resumes",
			proposer: lntypes.Remote,
			ack:      true,
			commit:   true,
			peerNext: heightEq,
			want:     ReconnectResume,
		},
		{
			name:     "responder commitsig peer advanced resumes",
			proposer: lntypes.Remote,
			ack:      true,
			commit:   true,
			peerNext: heightPast,
			want:     ReconnectResume,
		},

		// Defensive: signatures in impossible combinations still resolve
		// to a total, sensible action.
		{
			name:     "proposer commitsig without ack retransmits",
			proposer: lntypes.Local,
			ack:      false,
			commit:   true,
			peerNext: heightEq,
			want:     ReconnectRetransmitCommitSig,
		},
		{
			name:     "responder commitsig without ack resumes",
			proposer: lntypes.Remote,
			ack:      false,
			commit:   true,
			peerNext: heightEq,
			want:     ReconnectResume,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := mkAccepted(t, tc.proposer, tc.ack, tc.commit)
			got := Decide(p, ReestablishState{
				PeerNextCommitHeight: tc.peerNext,
			})
			require.Equalf(t, tc.want, got,
				"want %s, got %s", tc.want, got)
		})
	}
}

// TestReconnectActionStrings checks that every action has a distinct,
// non-default string form.
func TestReconnectActionStrings(t *testing.T) {
	t.Parallel()

	actions := []ReconnectAction{
		ReconnectForget, ReconnectRetainAndWait,
		ReconnectRetransmitCommitSig, ReconnectResume,
	}
	seen := make(map[string]struct{})
	for _, a := range actions {
		str := a.String()
		require.NotContains(t, str, "Unknown")
		require.NotContains(t, seen, str)
		seen[str] = struct{}{}
	}
}

// TestRestoreState verifies that Restore rehydrates a freshly-constructed
// Updater into the correct state for each retained-negotiation shape, and that
// the negotiation timeout is (re)armed only for the still-waiting states.
func TestRestoreState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	testCases := []struct {
		name      string
		proposer  lntypes.ChannelParty
		ack       bool
		commit    bool
		wantState State
		wantArmed bool
	}{
		{
			name:      "proposer awaiting ack",
			proposer:  lntypes.Local,
			ack:       false,
			commit:    false,
			wantState: StateAwaitingAck,
			wantArmed: true,
		},
		{
			name:      "proposer accepted",
			proposer:  lntypes.Local,
			ack:       true,
			commit:    false,
			wantState: StateAccepted,
			wantArmed: false,
		},
		{
			name:      "responder ack sent",
			proposer:  lntypes.Remote,
			ack:       true,
			commit:    false,
			wantState: StateAckSent,
			wantArmed: true,
		},
		{
			name:      "committing after commitsig",
			proposer:  lntypes.Local,
			ack:       true,
			commit:    true,
			wantState: StateCommitting,
			wantArmed: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rig := newRig(t, tc.proposer)
			p := mkAccepted(t, tc.proposer, tc.ack, tc.commit)

			require.NoError(t, rig.u.Restore(ctx, p))
			require.Equal(t, tc.wantState, rig.u.State())

			// Advance the clock beyond the negotiation timeout and
			// tick. An armed timer must fire (Disconnect), a
			// disarmed one must not.
			rig.clk.SetTime(
				rig.clk.Now().Add(2 * DefaultTimeout),
			)
			tr, err := rig.u.CheckTimeout(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.wantArmed, tr.Disconnect)
		})
	}
}

// TestRestoreInvalid verifies Restore rejects the shapes that cannot represent
// a retained negotiation: a nil proposal and a responder without a dyn_ack.
func TestRestoreInvalid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("nil proposal", func(t *testing.T) {
		t.Parallel()

		rig := newRig(t, lntypes.Local)
		err := rig.u.Restore(ctx, AcceptedProposal{
			Proposer: lntypes.Local,
		})
		require.Error(t, err)
	})

	t.Run("responder without ack", func(t *testing.T) {
		t.Parallel()

		rig := newRig(t, lntypes.Remote)
		p := mkAccepted(t, lntypes.Remote, false, false)
		err := rig.u.Restore(ctx, p)
		require.Error(t, err)
		require.Equal(t, StateIdle, rig.u.State())
	})
}
