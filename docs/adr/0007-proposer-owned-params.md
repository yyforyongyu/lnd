# 0007. Proposals update the proposer's own params

- Status: Accepted

## Context

Most channel parameters are inherently one-sided: they constrain what the
*remote* peer may do to the local peer. `channel_reserve_satoshis`,
`htlc_minimum_msat`, `to_self_delay`, `max_accepted_htlcs`, and
`max_htlc_value_in_flight_msat` are all declared by one side about the other in
the original `open_channel` / `accept_channel` handshake, producing two
independent sets of values (the funder's constraints on the fundee and vice
versa).

When a dynamic update renegotiates these values, the protocol must decide whose
values a single proposal changes. One option is a symmetric proposal that
rewrites both peers' values at once; the other is that each proposal only
touches the proposer's own declared parameters. The choice affects validation,
the meaning of `dyn_ack`, and how a "both sides change" outcome is reached.

`channel_flags` is different in kind: it expresses channel-wide announcement
intent (public vs. private), which is not a per-side constraint.

## Decision

A dynamic proposal updates the **proposer's own declared parameters**. The
responder accepts (`dyn_ack`) or rejects (`dyn_reject`) that one-sided change;
it does not simultaneously alter its own values in the same proposal.

Changing *both* peers' values therefore requires **two successful dynamic
updates**, one proposed by each side.

`channel_flags` is the exception: it is channel-wide announcement intent, not a
per-side constraint, so a proposal that changes it changes the shared channel
flag rather than the proposer's private value.

## Consequences

- Validation is simpler and local: the responder checks a single side's new
  values for BOLT-2 and LND-policy validity (and cross-coupling, e.g. reserve
  must not fall below dust after applying), rather than reconciling two
  simultaneous mutations.
- The semantics match the wire model, in which `dyn_propose` carries the
  proposer's declared values and `dyn_ack` acknowledges exactly those.
- Achieving a symmetric change (both sides lower their reserve, say) takes two
  rounds. This is an accepted cost; symmetric changes are expected to be rare and
  are cleanly expressible as two ordinary proposals.
- `channel_flags` needs distinct handling from the per-side parameters
  throughout the stack, since it is the one channel-wide value in the set; its
  public/private gossip effect is delivered as a dedicated subtrack.

## Alternatives considered

- **A single symmetric proposal that rewrites both peers' parameters at once.**
  Rejected: it complicates validation and the meaning of acknowledgement (the
  responder would have to both accept the proposer's changes and assert its own
  in one message), and it does not match the naturally one-sided structure of
  the parameters. Two one-sided updates express the same outcome with simpler
  semantics.

## Supersedes / Superseded-by

- Relates to ADR 0002 (params-only scope) and ADR 0005 (bundled
  `dyn_commit_sig`, which carries the accepted proposal TLVs).
