# 0003. A modular `htlcswitch/dyn.Updater` owns negotiation

- Status: Accepted

## Context

A dynamic-commitments update has two distinct phases. First, **negotiation**:
the proposer sends `dyn_propose`, the responder replies with `dyn_ack` or
`dyn_reject`, and the two sides converge on an agreed set of parameter changes.
Second, **execution**: the agreed change is committed by running the ordinary
commitment dance (`commitment_signed` / `revoke_and_ack`) so that both
commitment transactions lock in the new parameters.

The prior design (#8756, `[4/n]`) modeled the whole flow as a `protofsm` state
machine — a full protocol finite-state machine driving both negotiation and
execution. That approach is powerful but heavy: it introduces its own state,
persistence, and event plumbing, and it partly duplicates the commitment-dance
machinery the link already owns. It also stalled and went stale, which is
evidence that the abstraction was more than the problem required.

The `link` and `LightningChannel` already know how to run the commitment dance
correctly, including retransmission and reestablish. The only genuinely new
logic is the negotiation handshake.

## Decision

Introduce a scoped `Updater` in a new `htlcswitch/dyn` package that **owns
negotiation only**.

The `Updater` validates preconditions (both peers have exchanged
`channel_ready`, the local node is the quiescence initiator when proposing,
no HTLCs are in flight, no pending update / shutdown / splice / RBF), drives the
`dyn_propose` / `dyn_ack` / `dyn_reject` exchange, signs and verifies the
`dyn_ack` signature, and produces an agreed parameter update. Its job ends
there.

Once negotiation yields an agreed update, the update is fed into the existing
commitment update log (see ADR 0004) and the link plus wallet perform the
ordinary commitment dance to execute it. The `Updater` does not re-implement
persistence, retransmission, or commitment-number accounting — those stay where
they already live.

## Consequences

- The new code is small and focused: a negotiation component, not a second
  protocol engine. Execution reuses the battle-tested commitment-dance paths.
- Negotiation is cleanly decoupled from execution, which makes each unit-testable
  in isolation (state-machine and race tests for the `Updater`; link-level tests
  for the dance).
- There is no separate FSM state to persist and reconcile at reestablish beyond
  what the update-log model (ADR 0004) and the bundled `dyn_commit_sig` (ADR
  0005) already imply.
- The `Updater` must integrate with the link's message routing (dyn messages
  route as `lnwire.LinkUpdater` via `TargetChanID()`), so the boundary between
  "negotiation done by the updater" and "execution done by the link" has to be
  drawn precisely — this is the main design cost of the split.

## Alternatives considered

- **A full `protofsm` negotiation state machine** (#8756). Rejected: it is
  heavier than the problem warrants, duplicates commitment-dance logic the link
  already owns, and the concrete attempt stalled. The modular updater captures
  the only novel part — negotiation — and delegates the rest.
- **Inlining negotiation directly into `link.go`.** Rejected: it would bloat an
  already-large file, entangle negotiation with forwarding logic, and make the
  handshake hard to test in isolation. A scoped package keeps the concern
  separable.

## Supersedes / Superseded-by

- Relates to ADR 0004 (dyn_commit as an update-log entry) and ADR 0005 (bundled
  dyn_commit_sig).
