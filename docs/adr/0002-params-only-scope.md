# 0002. Params-only scope for dynamic commitments

- Status: Accepted

## Context

Dynamic commitments lets two channel peers renegotiate "static" channel
parameters after the channel is open, executed **solely by committing a new
channel state** — no on-chain close and reopen. This preserves the channel's
identity, age, and graph continuity.

The original design lineage (the `DynComms [0/n]…[4/n]` series and the
monolithic BOLT draft #1117) bundled two very different kinds of change under
one banner: renegotiating commitment parameters, and mutating the funding output
itself (`funding_pubkey` rotation, simple-taproot conversion, other channel-type
transitions). The latter requires a new funding transaction or a kickoff /
reanchor transaction and a wholesale change to the on-chain output structure.
Combining both into one milestone produced a large, deeply-coupled surface with
a correspondingly large funds-loss blast radius.

A clean line can be drawn: some updates are executable purely by signing a new
commitment over the *same* funding output; others are not.

## Decision

The first production milestone of dynamic commitments is **params-only**.

**In scope** — parameters that can change by committing a new state over the
existing funding output: `dust_limit_satoshis`,
`max_htlc_value_in_flight_msat`, `channel_reserve_satoshis`,
`htlc_minimum_msat`, `to_self_delay`, `max_accepted_htlcs`, and `channel_flags`
(announcement intent; the public/private gossip effect is delivered as a
dedicated subtrack layered on top of the core commitment update).

**Out of scope** — anything that requires a new funding transaction,
funding-output change, capacity change, or channel-type transition:
`funding_pubkey` rotation, funding-output changes, channel-type upgrades
(including same-funding-output ones such as anchors / static-remote-key /
zero-fee), simple-taproot conversion, and kickoff / reanchor transactions. These
belong to splicing or a future funding-update protocol.

The governing rule: **if an update needs a new funding transaction, it is not
dynamic-commitments work.**

## Consequences

- The implementation surface shrinks dramatically. All kickoff / reanchor
  machinery is removed from the effort, and the on-chain output structure never
  changes during a dynamic update, which bounds the recovery-correctness problem
  to script/output-rendering parameters (see ADR 0006).
- Channel identity, age, and graph continuity are preserved by construction,
  because the funding output is untouched.
- Because parameters are proposer-owned (see ADR 0007), changing *both* peers'
  values requires two successful dynamic updates.
- `channel_type` remains static for the life of a channel under this milestone,
  which lets recovery paths keep reading `chanState.ChanType` directly.
- Type upgrades and funding-output mutations are deferred, not abandoned; they
  are re-homed to splicing / a future funding-update protocol, where the
  funding-transaction machinery already lives.

## Alternatives considered

- **Full dynamic commitments including channel-type and funding-output
  changes** (the two-document #1117 structure with
  `ext-dynamic-funding-outputs.md`). Rejected for this milestone: it couples
  parameter renegotiation to funding-transaction construction, kickoff
  transactions, and channel-type-aware recovery, producing a much larger and
  riskier change than the params-only core needs.
- **An even narrower CSV-only or dust-only scope.** Rejected as arbitrary: the
  in-scope parameter set is exactly what a single new commitment over the same
  funding output can express, which is a principled boundary rather than a
  convenience cut.

## Supersedes / Superseded-by

- Relates to ADR 0006 (breach recovery via epoch history), ADR 0007
  (proposer-owned params), and ADR 0008 (no HTLCs in flight).
