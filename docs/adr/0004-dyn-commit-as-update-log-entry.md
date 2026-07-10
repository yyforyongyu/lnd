# 0004. Model `dyn_commit` as an update-log entry

- Status: Accepted

## Context

Once negotiation (ADR 0003) produces an agreed parameter change, that change has
to be applied atomically to both commitment transactions and persisted so it
survives restarts and reconnects. The question is *how* the agreed update is
represented inside the channel state machine on the way to being committed.

`lnd`'s commitment state machine already has a well-understood mechanism for
"a change that is proposed on one commitment and locks in as the dance
progresses": the **update log**. `update_add_htlc`, `update_fulfill_htlc`,
`update_fee`, and friends are all update-log entries that ride the ordinary
`commitment_signed` / `revoke_and_ack` exchange and take effect at
deterministic commitment heights on each side. `update_fee` in particular is a
close analogue: a single non-HTLC value that changes and locks in through the
dance.

The alternative would be to invent a bespoke persistence path for the pending
parameter change — a new bucket, new reestablish logic, new lock-in accounting —
running alongside the existing one.

## Decision

Model the agreed dynamic update (`dyn_commit`) as an **update-log entry**, in
the same spirit as `update_fee`.

The agreed parameter change is added to the commitment update log and rides the
existing commitment dance. It is proposed on a commitment, signed over as part
of the normal `commitment_signed` flow (bundled into `dyn_commit_sig`; see ADR
0005), and locks in at the corresponding commitment height on each side. The
commitment state machine records *when* the parameters changed relative to each
chain's commit height, which is exactly what the recovery epoch history (ADR
0006) needs.

## Consequences

- No new persistence bucket is required. The pending parameter change is carried
  by the same machinery that already carries HTLC and fee updates, and is
  persisted by the same commitment-state writes.
- Channel reestablish stays largely unchanged: the update rides the dance, so
  the existing retransmission and lock-in logic covers most of the reconnect
  behavior (the dyn-specific reconnect rules are additive; see ADR 0005).
- Because update-log entries lock in at deterministic per-side heights, the
  local and remote commitment chains adopt the new parameters at *different*
  heights. This is the correctness basis for the per-side (`Dual`) epoch history
  in ADR 0006.
- The update-log representation must faithfully encode the full parameter set and
  its proposer-owned semantics (ADR 0007), and the link must apply each side's
  config exactly when that side's chain locks in — so the lock-in accounting has
  to be precise.

## Alternatives considered

- **A dedicated persistence bucket and bespoke reestablish path for the pending
  parameter update.** Rejected: it duplicates the update-log machinery, adds a
  second lock-in accounting scheme to keep in sync with the first, and enlarges
  the reestablish surface — all to represent something the update log already
  represents well.
- **Applying parameters immediately on agreement, outside the commitment
  dance.** Rejected: it breaks atomicity with the committed state and leaves the
  two commitment chains transiently disagreeing about the active parameters,
  which is unsafe for recovery.

## Supersedes / Superseded-by

- Relates to ADR 0003 (modular updater), ADR 0005 (bundled dyn_commit_sig), and
  ADR 0006 (breach recovery via epoch history).
