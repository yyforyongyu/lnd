# Dynamic Commitments

Dynamic commitments let two channel peers renegotiate "static" channel
parameters *after* the channel is open, without an on-chain close and reopen.
The agreed change is applied **solely by committing a new channel state** over
the existing funding output, so the channel keeps its identity, age, and place
in the graph. Only the parameters change; the funding transaction is untouched.

This document describes *how* the feature works. The *why* — the design
decisions behind it — lives in the Architecture Decision Records under
[`docs/adr/`](./adr), linked from the "Design decisions" section below.

## Scope

Dynamic commitments is **params-only**. It covers exactly those parameters that
can be changed by signing a new commitment over the same funding output:

- `dust_limit_satoshis`
- `max_htlc_value_in_flight_msat`
- `channel_reserve_satoshis`
- `htlc_minimum_msat`
- `to_self_delay`
- `max_accepted_htlcs`
- `channel_flags` (announcement intent; the public/private gossip effect is
  handled as a dedicated subtrack)

Anything that requires a new funding transaction — `funding_pubkey` rotation,
funding-output changes, capacity changes, or channel-type transitions (including
simple-taproot conversion and other same-funding-output type upgrades) — is
**out of scope** and belongs to splicing or a future funding-update protocol.
The governing rule is simple: if an update needs a new funding transaction, it
is not dynamic-commitments work.

## Roles and terminology

Dynamic commitments has two roles:

- The **proposer** sends `dyn_propose` and, in `dyn_commit_sig`, the proposer's
  commitment signature over the new state.
- The **responder** accepts (`dyn_ack`) or rejects (`dyn_reject`) the proposal.

These are distinct from two other roles that appear in the flow. The
**initiator** is the *quiescence* initiator (the peer that sends `stfu`); the
proposer must be the quiescence initiator, but "proposer" is a
dynamic-commitments role, not a quiescence role. The **funder** / **opener** is
the peer that originally opened the channel. None of these should be conflated:
in particular, the `dyn_commit_sig` signature is the *proposer's* commitment
signature, which has nothing to do with who funded or opened the channel.

Parameters are **proposer-owned**: a proposal changes the proposer's own
declared parameters (`channel_flags` excepted — it is channel-wide). Changing
both peers' values therefore takes two successful updates, one from each side.

## Protocol flow

A dynamic update runs automatically under the hood once requested. The happy
path is:

1. **Quiescence.** The proposer initiates quiescence with `stfu`, freezing new
   updates on the channel. A dynamic update additionally requires that **no
   HTLC** — including trimmed HTLCs — exists in either commitment; quiescence
   alone is not sufficient.
2. **`dyn_propose`** (wire type 111). The proposer sends its desired new
   parameter values as a set of TLVs.
3. **`dyn_ack`** (113) or **`dyn_reject`** (115). The responder validates the
   proposal against BOLT 2 and local policy. It either acknowledges with a
   domain-separated `dyn_ack` signature, or rejects with a bitfield identifying
   the rejected parameters. A `dyn_reject` ends the current quiescence session;
   a retry needs fresh quiescence.
4. **`dyn_commit_sig`** (117). The proposer sends a single bundled message
   carrying the proposer's commitment signature over the new (zero-HTLC)
   commitment, the responder's `dyn_ack` signature, and the accepted proposal
   TLVs. The commitment signature is computed over a domain-separated
   double-SHA256 digest. This bundling removes a round trip and closes a race
   compared to a separate `dyn_commit` followed by `commitment_signed`.
5. **Commitment dance.** `dyn_commit_sig` is treated as the
   `commitment_signed`-equivalent for the update. The two peers exchange
   `revoke_and_ack` (and the responder's own `commitment_signed`) to lock the
   new parameters into both commitment chains. Each chain adopts the new
   parameters at its own lock-in height: the responder chain at the height
   signed by `dyn_commit_sig`, the proposer chain at the height signed by the
   responder's `commitment_signed`.
6. **Quiescence exit.** Once the dance completes at the BOLT-defined termination
   point, quiescence is released and the channel resumes normal operation with
   the new parameters in effect.

If the connection drops before a `dyn_commit_sig` has been sent or received, the
in-progress negotiation is aborted and forgotten; a fresh negotiation (and fresh
quiescence) is required. Once a `dyn_commit_sig` has been persisted, the update
is retransmitted and completed on reconnect like any other commitment-dance
step.

## Recovery

Some parameters affect on-chain script or output rendering — chiefly
`to_self_delay` (the CSV embedded in `to_local` and second-level HTLC scripts)
and `dust_limit` (which decides output presence). Breach and force-close
recovery must reconstruct *historical* commitments using the parameter values
that were in effect at that height, not the current ones. `lnd` keeps a
per-side, on-disk **epoch history** of these values, keyed by commitment height,
and every historical-reconstruction path looks up the epoch covering the target
state. This history is deliberately *not* placed in static channel backups
(SCBs); instead a fresh SCB is re-emitted after a successful update. See ADR
0006 for the full rationale.

## Design decisions

The architecture is recorded as ADRs. Start with the process bootstrap, then the
seven decisions specific to this feature:

- [ADR 0001 — Record architecture decisions](./adr/0001-record-architecture-decisions.md):
  establishes the ADR process itself.
- [ADR 0002 — Params-only scope](./adr/0002-params-only-scope.md): what is in
  and out, and why channel-type and funding-output changes are excluded.
- [ADR 0003 — Modular `htlcswitch/dyn.Updater`](./adr/0003-modular-updater.md):
  a scoped updater owns negotiation only, chosen over a `protofsm` negotiation
  state machine.
- [ADR 0004 — `dyn_commit` as an update-log entry](./adr/0004-dyn-commit-as-update-log-entry.md):
  the agreed update rides the existing commitment dance, like `update_fee`.
- [ADR 0005 — Bundled `dyn_commit_sig`](./adr/0005-bundled-dyn-commit-sig.md):
  one message carries the proposer commit sig, the responder `dyn_ack` sig, and
  the accepted TLVs, removing a round and a race.
- [ADR 0006 — Breach recovery via epoch history](./adr/0006-breach-recovery-epoch-history.md):
  per-side local epoch history for historical script reconstruction, not in
  SCBs.
- [ADR 0007 — Proposer-owned params](./adr/0007-proposer-owned-params.md): a
  proposal updates the proposer's own declared parameters; `channel_flags` is
  channel-wide.
- [ADR 0008 — No HTLCs in flight](./adr/0008-no-htlcs-in-flight.md): no dynamic
  update while any HTLC (including trimmed) exists in either commitment;
  quiescence alone is insufficient.
