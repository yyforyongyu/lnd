# 0006. Breach recovery via local epoch history

- Status: Accepted

## Context

When a dynamic update changes a parameter that affects on-chain **script or
output rendering** — chiefly `to_self_delay` (CSV), and also `dust_limit` for
output presence — the recovery paths that reconstruct *historical* commitment
transactions and scripts must use the parameter values that were in effect **at
that historical commitment height**, not the current ones.

Today this is fully static. Breach reconstruction in `lnwallet` (for example
`NewBreachRetribution`, `createHtlcRetribution`, and
`findOutputIndexesFromRemote`) rebuilds a revoked state's witness scripts from
the *live* channel config CSV. The `RevocationLog` entry stores only output
indices, the commit-tx hash, balances, and HTLCs — no CSV, no channel type.

The failure mode is concrete and costs funds: change CSV via a dynamic update,
then have a peer breach with a *pre-change* revoked state. The justice
transaction's witness scripts get rebuilt with the *new* CSV, produce the wrong
witness script, and the justice transaction is unspendable — funds are lost.

Which parameters actually need history is narrow. CSV is embedded in the
`to_local` and HTLC second-level witness scripts, so it is the hard requirement.
Dust decides output *presence* (trimming); it is cheap to retain so historical
commitment rendering stays fully specified, but note that
`NewBreachRetribution` works from the actual broadcast transaction and the
stored output indices and does not re-run trimming, so dust is retained for
completeness rather than as a breach blocker. `channel_type` is static under the
params-only scope (ADR 0002), so it needs no history.

## Decision

Maintain a **per-side local epoch history** of script/output-rendering
parameters, keyed by commitment height, and route every historical
reconstruction through it.

Adopt a `CommitChainEpoch` / `CommitChainEpochHistory` structure (reusing the
concept from #9158, re-implemented on the modular design) storing
`{DustLimit, CsvDelay}` per epoch. It is tracked per side with `lntypes.Dual`,
because the local and remote commitment chains lock in new parameters at
*different* heights (the responder chain at the height signed by
`dyn_commit_sig`; the proposer chain at the height signed by the responder's
`commitment_signed` — see ADR 0004 and ADR 0005). On execution, the current
epoch is closed at the per-side lock-in height and a new one is opened. The
history is persisted as optional channel TLV data in `channeldb`, and existing
channels migrate to a single "epoch 0" covering all prior heights with their
current static params.

This decision **gates** a required audit: every CSV/dust read in `lnwallet` must
be classified as either "reconstructs a historical state → MUST use epoch
lookup" or "operates on the current/latest state → current config is correct."
Because *any* historical-reconstruction site that reads current config is a
funds-loss risk once CSV/dust are mutable, this is a codebase-wide audit, not a
single-site fix. No mutable CSV/dust may ship until that audit lands.

Epoch history lives in **local on-disk channel state only** — it is **not**
placed in SCBs. SCBs drive DLP, and DLP recovers only the `to_remote` output,
whose script does not depend on `to_self_delay`; so CSV changes do not affect
DLP-based recovery. Instead, a fresh SCB is re-emitted on successful execution so
the backup reflects the current params.

## Consequences

- Breach and force-close recovery remain correct across parameter changes: each
  historical script is rebuilt with the CSV/dust that were live at that height.
- Space cost is proportional to the number of *upgrades* (rare), not the number
  of *states* (millions): roughly 18 bytes per epoch. Storing CSV per revoked
  state would have undone the revocation-log space optimization.
- The revocation-log format is left unchanged unless tests prove more per-state
  data is genuinely needed.
- The historical-param audit is a hard prerequisite and a safety gate: the
  feature (or per-parameter, CSV specifically) stays disabled until it lands
  together with the reconnect hardening.
- SCBs stay small and portable; the only backup change is re-emission on
  execution, matching #9158's minimal `chanbackup` touch. The trade-off is that
  a stale SCB after an upgrade is the ordinary "use your latest backup" problem,
  which epochs-in-the-SCB would only have masked.

## Alternatives considered

- **Store CSV (and dust) per revoked state in the revocation log.** Rejected:
  space blows up with the number of states and defeats the revocation-log size
  optimization, to record a value that only changes on rare upgrades.
- **Embed epoch history in the SCB.** Rejected: DLP recovers only the
  CSV-independent `to_remote` output, so the SCB does not need it; adding it
  would bloat a deliberately tiny, portable artifact and would merely mask the
  restoration of an out-of-date backup.

## Supersedes / Superseded-by

- Relates to ADR 0002 (params-only scope, which keeps `channel_type` static),
  ADR 0004 (per-side lock-in heights), and ADR 0005 (which height each chain
  locks in at).
