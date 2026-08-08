# ADR-0001: Record architecture decisions

- **Status:** accepted
- **Date:** 2026-07-06
- **Deciders:** Maintainers
- **Related:** none

## Context

The project already records proposed *changes* to the standard as RFCs under
`rfcs/`. It does not have a place to record decisions that have *already been
made* — the many smaller architectural choices that shape the codebase but do
not each warrant a full RFC. Without such a record, the reasoning behind a
decision lives only in review threads and contributors' memory, and is lost as
the maintainer set grows. New contributors then re-litigate settled questions
because the rationale is not written down.

## Decision

Record architecture decisions in this repository as Architecture Decision
Records (ADRs), one Markdown file per decision under `docs/adr/`, following the
lightweight format described in `docs/adr/README.md`. ADRs are immutable once
accepted and are superseded rather than edited.

## Alternatives considered

- **Only RFCs.** RFCs are the right instrument for proposing changes to the
  standard, but they are heavyweight and forward-looking. Using them to record
  small, already-settled implementation decisions would dilute the RFC process
  and discourage writing decisions down at all.
- **A wiki or external doc.** Decisions kept outside the repository drift from
  the code, are not reviewed alongside the change they justify, and are not
  versioned with it.
- **Code comments only.** Comments explain local mechanics but cannot carry the
  context and rejected alternatives behind a cross-cutting decision.

## Consequences

- Every significant architectural decision has a durable, reviewed, versioned
  home that ships with the code.
- Authoring an ADR is a small, well-understood step in a pull request, lowering
  the barrier to documenting a decision.
- The index in `docs/adr/README.md` must be kept current; a stale index is the
  main maintenance risk and should be checked in review.
