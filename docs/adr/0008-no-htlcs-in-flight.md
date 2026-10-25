# 0008. No dynamic update while HTLCs are in flight

- Status: Accepted

## Context

A dynamic update commits a new channel state with different parameters. Several
of those parameters shape how HTLCs are rendered on the commitment transaction:
`dust_limit` decides whether an HTLC is trimmed, `to_self_delay` sets the CSV in
second-level HTLC scripts, and the value/count limits constrain what may be
outstanding. If HTLCs were present on either commitment while parameters
changed, the new commitment would have to re-render existing in-flight HTLCs
under the new parameters, and the two commitment chains — which lock in the
change at different heights (ADR 0004) — could transiently disagree about how a
given HTLC should be trimmed or scripted. That is a correctness minefield for
both the live state machine and later historical reconstruction.

Quiescence (`stfu`) freezes *new* updates from being added, and the proposer is
required to be the quiescence initiator. But quiescence on its own does not
guarantee the commitments are *empty*: HTLCs that were already committed —
including **trimmed** HTLCs that do not appear as outputs but still exist as
commitment-log entries — can remain in flight when quiescence is reached.

## Decision

A dynamic update may proceed only when **no HTLC exists in either commitment**,
including trimmed HTLCs, on both the local and remote chains. Quiescence is
necessary but **not sufficient**; the no-HTLC precondition is an additional,
explicit gate.

The channel must be quiescent *and* have zero HTLCs (trimmed or not) in both
commitment states before negotiation is allowed to execute. The `Updater` (ADR
0003) enforces this precondition before sending or accepting a proposal.

## Consequences

- The committed state being signed by `dyn_commit_sig` is a **zero-HTLC**
  commitment, which is precisely what lets the bundled message carry a single
  commit signature with no HTLC signatures (ADR 0005).
- There is no need to re-render in-flight HTLCs under changed parameters, and no
  window in which the two commitment chains disagree about an HTLC's trimming or
  script during the parameter transition.
- A node that wants to run a dynamic update may have to wait for in-flight HTLCs
  to resolve first. Under automatic orchestration this manifests as the update
  waiting until the channel drains; it does not fail outright merely because the
  channel is momentarily busy, but it will fail safely if it cannot reach the
  no-HTLC state within the negotiation window.
- The check must count trimmed HTLCs and inspect both commitments; checking only
  visible outputs on one side would be an unsafe under-approximation.

## Alternatives considered

- **Gate on quiescence alone.** Rejected: quiescence stops new updates but does
  not empty the commitments, so trimmed or already-committed HTLCs could still be
  present when parameters change — exactly the case that produces mixed-parameter
  HTLC rendering.
- **Allow updates with HTLCs in flight, re-rendering them under the new
  parameters.** Rejected for this milestone: it adds substantial complexity to
  both the live state machine and historical reconstruction for little
  near-term benefit. The restriction is relaxable in a future iteration if a
  need arises.

## Supersedes / Superseded-by

- Relates to ADR 0005 (bundled `dyn_commit_sig`, which relies on the zero-HTLC
  commitment) and ADR 0003 (the `Updater` enforces this precondition).
