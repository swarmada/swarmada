# ADR-0004: RFC amendment and errata policy

- **Status:** accepted
- **Date:** 2026-07-06
- **Deciders:** Maintainers
- **Related:** ADR-0001, `rfcs/README.md`, `docs/api-principles.md`, `docs/release-process.md`

## Context

RFC-0001 is maintained as a **living normative specification**, not a frozen
proposal: it is chapter-sourced and assembled into a single rendered document,
carries `stage:`/`impl:` maturity badges, and is the current source of truth for
the standard. This differs from RFC systems where an accepted RFC is an immutable
historical record and every correction requires a new superseding document.

When an issue is found in an already-accepted RFC, contributors need a clear rule
for whether to correct it **in place** (edit the RFC) or via a **new RFC** (an
amendment that supersedes part of the previous one). Without a rule this is
relitigated per issue, and the risk runs both ways: treating every typo as a new
RFC is process theater, while editing a shipped, relied-upon contract in place
silently breaks consumers.

The right answer depends on two things: the nature of the issue, and how mature
and widely relied upon the affected surface is (its `stage:` badge and whether it
has shipped in a release).

## Decision

Correct issues according to the following rule, keyed on issue type and the
maturity of the affected surface — not on whether the RFC is "accepted."

- **Editorial / errata** (typos, broken cross-references, wording that does not
  change meaning, internal inconsistencies): corrected **in place** in the RFC
  via a normal pull request, at any maturity. No new RFC.

- **Substantive normative change to an `alpha` or unreleased surface** (a genuine
  design flaw, missing case, or semantics change where the affected surface is
  still `stage: alpha` and not depended upon in a release): corrected **in
  place**, with a re-review of the affected section when the blast radius is
  non-trivial. No superseding RFC. Pre-stabilization surfaces are expected to
  change; that is what `alpha` means.

- **Substantive normative change to a `beta`/`stable`, shipped surface**: made
  through a **new amendment RFC** that proposes the delta and any migration, runs
  the standard comment period (and a TSC vote when the change is API-breaking),
  and — on acceptance — is **folded back into the living spec text** so the spec
  remains the single current source of truth while the RFC serves as the
  proposal-and-history record.

- **Breaking change to a shipped contract** (`swarmada.io/v1`,
  `fleet_adapter.v1`): always a new RFC, and **never an in-place mutation** of the
  stable version. It introduces a new version (`…/v2`) with a conversion path per
  `docs/api-principles.md` and `docs/release-process.md`; the prior version's text
  remains valid for that version.

In-place corrections are still reviewed like any change; a normatively
significant in-place edit re-enters a review period for the affected section. The
distinction between "in place" and "new RFC" is the degree of ceremony and
historical record, not whether there is oversight.

## Alternatives considered

- **Immutable, append-only RFCs** (every correction is a new superseding RFC).
  Rejected: it contradicts the living-spec model, and would force new RFCs for
  typos and for routine churn on `alpha` surfaces that are explicitly still
  changing.
- **Always edit in place** (no amendment RFCs, even for stable surfaces).
  Rejected: it would allow silent changes to contracts that adapters and
  operators already depend on, with no proposal record or migration discipline.

## Consequences

- While the specification is pre-stabilization (most surfaces `alpha`, RFC-0001
  in `Draft`), the overwhelming majority of corrections are made in place; the
  amendment-RFC ceremony applies only after a surface is stabilized and shipped.
- The boundary at which ceremony increases is the maturity badge plus release
  status, which are already tracked, so the rule needs no new bookkeeping.
- A stable contract is never changed in place; consumers get a versioned surface
  and a migration path, consistent with the compatibility policy.
- The living spec always reflects the current accepted state, because accepted
  amendment RFCs are folded back into the text rather than left as separate
  documents to reconcile.
