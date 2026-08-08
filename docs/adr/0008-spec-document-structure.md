# ADR-0008: Specification document structure — Proposal overview + Detailed Specification appendix

- **Status:** accepted
- **Date:** 2026-07-07
- **Deciders:** Maintainers
- **Related:** ADR-0004, `rfcs/README.md`, RFC-0001

## Context

RFC-0001 follows the nine-section KEP order (Abstract, Motivation, Goals,
Non-Goals, Proposal, Design Details, Drawbacks, Alternatives Considered,
References). Over time the **Proposal** section has accreted the full normative
detail of thirteen CRDs plus the Fleet Adapter protocol, control-plane components,
the Traffic Deconfliction Engine, the security model, and the safety
architecture — on the order of 8,000 lines — sitting in the middle of the
document. This pushes Drawbacks and Alternatives (the sections a reviewer uses to
judge the proposal) to the far end, and forces a reader of the *argument* through
the entire normative reference first.

Two forces are in tension:

- **Readability / reviewer experience.** A reader evaluating the proposal wants
  the case (motivation, goals, tradeoffs, alternatives) without an 8,000-line
  normative wall in the middle.
- **Convention.** The KEP order — Proposal *before* Design Details, Drawbacks,
  and Alternatives — is the shape a CNCF reviewer expects; Drawbacks and
  Alternatives are meant to be read in light of the Proposal.

A mechanical constraint also applies: the assembler generates section numbers
from manifest order, but chapter *bodies* currently carry hand-authored
subsection numbers (e.g. `5.2.4.1`), so any reorder desyncs those numbers and the
cross-references that point at them.

## Decision

Keep the KEP-conventional top-level order, and split the Proposal rather than
relocating it.

- The **Proposal** section stays in place but becomes a *concise overview* — the
  architecture overview and the resource model at a glance.
- The exhaustive per-CRD and per-RPC normative text moves into a new
  **Detailed Specification** section positioned at the **end of the document,
  immediately before Terminology**, as a clearly-labeled normative reference
  (appendix).
- **Prerequisite — numbering-model migration.** Before the reorder, migrate so
  the assembler owns *all* section numbers: strip hand-authored subsection
  numbers from chapter bodies and move cross-references to stable, name-based
  anchors. Only then perform the reorder, so numbering cannot desync.
- **Approved-chapter integrity.** Chapters already reviewed and approved
  (Abstract, Motivation) are **not changed in content** by this restructure.
  Their approved text is preserved verbatim; only document placement and the
  numbering mechanics may touch them.

## Alternatives considered

- **Move the entire Proposal to the end of the document.** Rejected: it inverts
  the Proposal → Drawbacks → Alternatives logic (a reader would meet the
  drawbacks of a design not yet described) and departs from the KEP shape a
  standards-body reviewer expects.
- **Leave the structure as-is.** Rejected: the mid-document normative wall harms
  readability and buries the evaluation sections at a length where reviewers may
  not reach them.

## Consequences

- Reading order becomes: Abstract → Motivation → Goals → Non-Goals →
  Proposal (overview) → Design Details → Drawbacks → Alternatives →
  **Detailed Specification** → Terminology → References.
- The numbering-model migration is a prerequisite and is tracked as an
  implementation item; the reorder is not performed until it is complete.
- Cross-references to CRD/RPC subsections must move to stable anchors and be
  verified after the reorder.
- Because the change is structural and preserves the normative content of
  approved chapters, it does not reopen approved review items.
