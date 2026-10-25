# 0001. Record architecture decisions

- Status: Accepted

## Context

`lnd` has no architecture-decision convention today. `docs/` is a flat pile of
topic-oriented documents (for example `watchtower.md`, `musig2.md`,
`zero_conf_channels.md`), each describing *how* a subsystem works, but there is
no durable record of *why* a given design was chosen over the alternatives that
were on the table at the time. As a result, the reasoning behind a decision
tends to live in a pull-request thread, a review comment, or a maintainer's
memory. Those sources are hard to find later, decay over time, and disappear
entirely when the surrounding code is rewritten.

This matters most for features that evolve over several years and pivot more
than once. Dynamic commitments is the concrete example that motivated this ADR
process: it has already changed shape several times (monolithic BOLT draft to a
consolidated params-only draft; a `protofsm` negotiator to a scoped modular
updater), and each pivot silently invalidated earlier design notes. Without a
record of what was decided and why, every new contributor re-litigates settled
questions.

An Architecture Decision Record (ADR) captures a single significant decision:
the context that forced it, the decision itself, and the consequences that
follow. Recording decisions as small, immutable, numbered documents that live
next to the code gives us a searchable, reviewable, version-controlled history
of the project's architecture — one that survives refactors because it is never
edited in place.

## Decision

Adopt lightweight ADRs for `lnd`, stored in `docs/adr/`, following a
Nygard/MADR-lite structure.

**File naming.** Each ADR is a Markdown file named `NNNN-slug.md`, where `NNNN`
is a zero-padded, monotonically increasing number and `slug` is a short
kebab-case title (for example `0004-dyn-commit-as-update-log-entry.md`). Numbers
are never reused. This bootstrap document is ADR 0001.

**Format.** Every ADR uses the following sections, in order:

- **Status** — one of `Proposed`, `Accepted`, or `Superseded-by-NNNN` (see
  lifecycle below).
- **Context** — the forces at play: the problem, constraints, and requirements
  that make a decision necessary. Written in the present tense, as the situation
  seen at decision time.
- **Decision** — the choice that was made, stated plainly and actively ("We
  will ...").
- **Consequences** — what becomes easier and what becomes harder as a result,
  including follow-on work and accepted trade-offs. Both positive and negative.
- **Alternatives considered** — the other options that were genuinely on the
  table and why they were not chosen.
- **Supersedes / Superseded-by** — cross-links to related ADRs, when
  applicable. Omit when there are none.

**Lifecycle.** An ADR moves through `Proposed` → `Accepted` →
`Superseded-by-NNNN`:

- A new ADR opens as **Proposed** while under review.
- It becomes **Accepted** when merged. An accepted ADR is immutable — it is a
  historical record of what was decided at a point in time.
- When a later decision overrides an accepted one, do **not** edit the original.
  Write a new ADR that captures the new decision, mark the old one
  `Superseded-by-NNNN`, and mark the new one as superseding the old
  (`Supersedes: NNNN`). This preserves the full lineage of the design.

Only edit an accepted ADR to fix a typo or a broken link, never to change its
meaning. Meaning changes go in a new, superseding ADR.

## Consequences

- The reasoning behind significant `lnd` design decisions becomes discoverable
  in-tree, reviewable through the normal pull-request process, and durable
  across refactors.
- Contributors gain a template that lowers the cost of proposing and evaluating
  a design change: reviewers can weigh the recorded alternatives instead of
  reconstructing them.
- The immutability rule means the ADR log grows monotonically and can contain
  decisions that no longer reflect the current code. This is intentional — a
  superseded ADR plus its successor together tell the story of how the design
  changed. Readers must check the `Status` line and follow `Superseded-by`
  links to find the currently-authoritative decision.
- There is a small ongoing discipline cost: authors of non-trivial architectural
  changes are expected to add or supersede an ADR alongside the code.
- This process is deliberately scoped to *architecture* decisions. It does not
  replace the per-feature `docs/*.md` overviews, release notes, or code
  comments; it complements them.

## Alternatives considered

- **Keep the status quo (no ADRs).** Continue recording rationale in PR threads
  and topic docs. Rejected: rationale stays scattered and ephemeral, and the
  dynamic-commitments pivots showed concretely how quickly undocumented design
  intent is lost.
- **A heavier ADR template (full MADR with decision drivers, scored options,
  pros/cons matrices).** Rejected as too much ceremony for a first adoption;
  the friction would discourage authors. The lightweight Nygard-style form
  captures what we need and can be extended later if it proves too thin.
- **A single living "design decisions" document.** Rejected: a mutable
  catch-all loses the per-decision review granularity and the immutable audit
  trail, and it re-introduces exactly the "edit history in place" problem that
  superseding avoids.
