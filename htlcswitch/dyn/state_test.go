package dyn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAdvanceTable exercises the pure transition function directly, checking
// both the legal edges and that every other (state, event) pair is rejected as
// an invalid transition.
func TestAdvanceTable(t *testing.T) {
	t.Parallel()

	allStates := []State{
		StateIdle, StateAwaitingAck, StateAckSent, StateAccepted,
		StateCommitting, StateRejected, StateFailed,
	}
	allEvents := []eventType{
		eventPropose, eventProposeOK, eventProposeBad, eventAckOK,
		eventReject, eventCommitSent, eventCommitRecv, eventTimeout,
		eventFail,
	}

	// The complete set of legal edges, mirroring the transitions table.
	legal := map[State]map[eventType]State{
		StateIdle: {
			eventPropose:    StateAwaitingAck,
			eventProposeOK:  StateAckSent,
			eventProposeBad: StateRejected,
		},
		StateAwaitingAck: {
			eventAckOK:   StateAccepted,
			eventReject:  StateRejected,
			eventTimeout: StateFailed,
			eventFail:    StateFailed,
		},
		StateAckSent: {
			eventCommitRecv: StateCommitting,
			eventTimeout:    StateFailed,
			eventFail:       StateFailed,
		},
		StateAccepted: {
			eventCommitSent: StateCommitting,
			eventFail:       StateFailed,
		},
	}

	for _, from := range allStates {
		for _, e := range allEvents {
			want, ok := legal[from][e]

			to, err := advance(from, e)
			if !ok {
				require.ErrorIs(t, err, ErrInvalidTransition)
				require.Equal(t, from, to)

				continue
			}

			require.NoError(t, err)
			require.Equal(t, want, to)
		}
	}
}

// TestStateStrings checks that every state and event has a distinct,
// non-default string form.
func TestStateStrings(t *testing.T) {
	t.Parallel()

	states := []State{
		StateIdle, StateAwaitingAck, StateAckSent, StateAccepted,
		StateCommitting, StateRejected, StateFailed,
	}
	seen := make(map[string]struct{})
	for _, s := range states {
		str := s.String()
		require.NotContains(t, str, "Unknown")
		require.NotContains(t, seen, str)
		seen[str] = struct{}{}
	}

	require.True(t, StateCommitting.IsTerminal())
	require.True(t, StateRejected.IsTerminal())
	require.True(t, StateFailed.IsTerminal())
	require.False(t, StateIdle.IsTerminal())
	require.False(t, StateAwaitingAck.IsTerminal())
}
