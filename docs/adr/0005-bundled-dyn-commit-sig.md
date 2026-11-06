# 0005. Bundle the proposer commit sig into `dyn_commit_sig`

- Status: Accepted

## Context

After the responder accepts a proposal with `dyn_ack`, the proposer must commit
the agreed change. An earlier design separated this into two messages: a
`dyn_commit` message that carried the accepted proposal, followed by an ordinary
`commitment_signed` carrying the commitment signature. That sequence has two
problems.

First, it costs an extra round trip: the proposer sends `dyn_commit`, then
`commitment_signed`, where the two could travel together since there are no
HTLCs in flight (ADR 0008) and the committed state is fully determined by the
accepted proposal.

Second, splitting the accepted-proposal payload from the signature opens a race:
the two peers can disagree about exactly which proposal the signature covers if
messages interleave with other state, and the responder's acceptance signature
(`dyn_ack`) is a separate artifact that has to be re-associated with the
commitment.

The `dyn_ack` signature itself is domain-separated: a 64-byte low-S ECDSA
signature over the **double-SHA256** digest of a domain-tagged payload
(`"lightning-dynamic-commitments-dyn-ack-v1"`) covering the chain hash, channel
id, next commitment number, and the exact proposal TLVs.

## Decision

Bundle everything the responder needs to commit into a single message,
`dyn_commit_sig` (wire type 117):

1. the **proposer's** commitment signature over the new commitment (a
   zero-HTLC commitment, since no HTLCs are in flight), computed over the
   domain-separated double-SHA256 digest;
2. the responder's `dyn_ack` signature; and
3. the accepted proposal TLVs.

Because there are no HTLCs, this is a single commit signature with no
accompanying HTLC signatures. `dyn_commit_sig` is treated as the
`commitment_signed`-equivalent for the update: it counts toward
`next_commitment_number` / `next_revocation_number` accounting, and it is
persisted (together with the accepted TLVs and the resulting state) before the
responder sends `revoke_and_ack`.

## Consequences

- One message replaces two: a full round trip is removed from the happy path.
- The race is closed: the signature, the accepted proposal it signs over, and
  the responder's acceptance signature travel as one atomic unit, so there is no
  window in which the peers disagree about what was signed.
- Reestablish must treat `dyn_commit_sig` as the corresponding
  `commitment_signed`: it is retransmitted when the peer's
  `channel_reestablish` expects it, and it is permitted before any other update
  even though quiescence has already ended.
- The terminology must stay precise: `dyn_commit_sig` carries the **proposer's**
  commitment signature. "Proposer" is a dynamic-commitments role and must not be
  conflated with the channel funder/opener or with the quiescence initiator,
  even though the proposer is required to be the quiescence initiator.
- Crash-safety hinges on ordering: a received `dyn_ack` must not be used after
  reconnect unless a `dyn_commit_sig` was persisted before disconnect, and a
  received `dyn_commit_sig` plus its TLVs must be persisted before
  `revoke_and_ack`.

## Alternatives considered

- **Separate `dyn_commit` followed by `commitment_signed`** (the earlier
  sequence). Rejected: it adds a round and admits the accepted-proposal /
  signature race, for no benefit given the no-HTLC precondition.
- **Sending the `dyn_ack` signature only in the ack and never echoing it.**
  Rejected: re-including it in `dyn_commit_sig` lets the responder verify, in a
  single step, that the commitment being signed corresponds exactly to the
  proposal it acknowledged.

## Supersedes / Superseded-by

- Relates to ADR 0004 (dyn_commit as an update-log entry) and ADR 0008 (no
  HTLCs in flight, which is what makes the single bundled commit sig sufficient).
