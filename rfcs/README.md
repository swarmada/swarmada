# Swarmada RFCs

This directory holds Swarmada's specifications. RFC-0001, the core specification, is published as one
document, [`dist/RFC-0001-core-spec.md`](dist/RFC-0001-core-spec.md), with its overview
[`RFC-0001-overview.md`](RFC-0001-overview.md) and the two amendments accepted for it in
[`amendments/`](amendments/). RFC-0003 and RFC-0004 are published in [`dist/`](dist/) as drafts for
review. The chapter sources each document is assembled from, the
manifests and the assembler are not published.

## The nine mandatory sections

Abstract · Motivation · Goals · Non-Goals · Proposal · Design Details · Drawbacks ·
Alternatives Considered · References.

## Status lifecycle

```
Reserved  ->  Exploratory  ->  Draft  ->  Review  ->  Accepted
                                                    \->  Rejected
```

Each state is defined by an **entry gate**, not by judgement:

| Status | Entry gate |
| :----- | :--------- |
| `Reserved` | A number and a one-paragraph charter. **No document yet.** |
| `Exploratory` | A document exists but fails one or more Draft gates below, or meets them and is held below `Draft` until its content is reviewed. Not to be built against. |
| `Draft` | All nine mandatory sections present; the document assembles with every cross-reference resolved; RFC 2119 keywords declared and used; **zero hard-coded `§x.y` references**. |
| `Review` | Draft gates, plus an open comment window and at least one named external reviewer. |
| `Accepted` | Review closed; implementation path agreed; conformance clauses exist where the RFC defines a contract. |
| `Blocked` | A modifier, not a state. Applied to any of the above when progress depends on an unresolved core decision. **The decisions must be named.** |

`Blocked` is deliberate paused work with a written unblock list, and is not a defect.

RFC-0001's status is **Review**, declared in the front matter of its published document. Its revision is v0.4, the draft 4 release, and contract `1.4.0` is published with it.

## RFC index

RFC-0001 is the core specification. RFC-0003 and RFC-0004 are published with draft 4 for review. They are
not part of the draft 4 contract (RFC-0001 governs), and nothing in RFC-0001 depends on them.
RFC-0002, RFC-0005 and RFC-0007 exist as unpublished drafts, and RFC-0006 is superseded by RFC-0001; none is part of this release.

| RFC | Title | Status | Blocked on | Deferred from |
| :-- | :---- | :----- | :--------- | :------------ |
| 0001 | Core Specification | **Review** | — | — |
| 0003 | Semantic Events and Declarative Task Triggers | `Draft` · draft for review | — | RFC-0001, `FleetTask`, *Features outside this chapter* (semantic events, trigger binding) |
| 0004 | Task-Source Surface | `Exploratory` · draft for review | — (nearest to `Draft`) | ADR-0006 |

The drafts propose added resources, fields, enum values, annotations, RBAC verbs, lifecycle transitions and
`fleet_adapter.v1` messages. Each such addition would need a
contract change; none is part of RFC-0001 v0.4.

In RFC-0001, a **Maturity (v0.4)** label names the release. In RFC-0003 and RFC-0004, a **Maturity**
label names the draft's own document revision. The release a draft describes is the `swarmada-version`
in its front matter.

**RFC-0003 was withdrawn on 2026-08-13 and rebuilt, re-scoped, on 2026-08-19.**
The withdrawn draft contained zero RFC 2119 keywords and specified a wire change against a protocol
message that does not exist; the rebuilt draft repairs both. Of its four original features, `completionMode`
belongs to RFC-0001, which declares it as `FleetAction.spec.executor.completionMode`, time-synchronized
co-completion is not specified in this version, and the coherent remainder —
robot-raised semantic events and the declarative bindings that consume them — is what the
rebuilt draft specifies, in
[`dist/RFC-0003-semantic-events-and-task-triggers.md`](dist/RFC-0003-semantic-events-and-task-triggers.md). It is published with draft 4 for
review and is not part of the draft 4 contract (RFC-0001 governs); RFC-0001 cites it as
reference [34].

**RFC-0004 meets the structural `Draft` gates and is held at `Exploratory` until its content is reviewed.** It assembles
cleanly, with every cross-reference resolved and no hard-coded `§x.y` reference.

RFC numbers 0008 to 0011 are reserved for work RFC-0001 defers: RFC-0008 (Facility Infrastructure Actuation), RFC-0009 (Energy Management and Idle Disposition), RFC-0010 (Facility Model and Map Versioning) and RFC-0011 (Intervention and Custody). No document exists for any of them.

**Not separate RFCs:** **RobotClassRollout** (batch re-admission on class change,
sibling to `FirmwareRollout`/`ModelRollout`) and **SharedResource** (already embedded as
`FleetZone.spec.sharedResources[]`, RFC-0001 *SharedResource Types and Reservation Policy*). Neither is a CRD in RFC-0001 v0.4: adding one
would need a contract change (see "Amending an accepted RFC",
below).

## Amending an accepted RFC

RFC-0001's contract is **frozen at RFC-0001 v1.0**. The rule is stated in
[RFC-0001](dist/RFC-0001-core-spec.md), *Resource Model at a Glance*, *The contract
is frozen at RFC-0001 v1.0*, and this summary does not replace it. The freeze covers the fleet-adapter
wire contract version and the CRD API versions published with RFC-0001 v1.0; it does not mean wire
contract `1.0`. [ADR-0004](../docs/adr/0004-rfc-amendment-and-errata-policy.md) Amendment 1 records
this policy. Until v1.0, `0.x` revisions keep refining the contract and keep changes to existing
contracts to a minimum:

- **Editorial / errata** (typos, broken cross-references, wording that does not
  change meaning): fixed **in place** via a normal pull request.
- **Informative text** (maturity notes beyond their normative sentences, paragraphs that
  describe what holds "at v0.4", Known-gaps cells, examples, citations): changed **in place**;
  a note that records a gap can later record it closed.
- **Normative text, a CRD's rules and semantics, or the proto**: not changed in place. A change to the
  contract goes through the contract-change process: it is a new contract version, with release notes
  and migration notes, specified by an amendment or a new RFC that passes the gates above (`Draft`,
  `Review`, `Accepted`). A resource's `stage:` decides what may change. A change off the wire is a patch
  bump by default; a change that binds adapters is a minor bump, and the supported range moves. A change
  that binds adapters for safety also raises the lowest supported contract version to the new one.
- **A CRD's schema**: within `swarmada.io/v1` only backward-compatible changes are made. An incompatible
  schema change introduces `v2`, served alongside `v1` for at least one revision, with conversion and a
  migration note.
- **A safety rule** (stops, the action table, the robot stop table, the invariants): at every stage, a
  change goes through the contract-change process, with the conformance suite updated in the same
  version. A change that weakens a guarantee says so first, and every unmet rule keeps its Not-met note.

An in-place correction is still reviewed like any change.

**An amendment is a drafting tool.** Every release folds its accepted amendments into the chapters and
assembles one complete specification for that version. It is published with a "what changed since the
previous version" document.

**The two v0.4 amendments are not folded yet.** Zone restrictions and zone exclusions are normative for
v0.4 but published as separate files in [`amendments/`](amendments/). The next revision folds them into
the chapters.

## Chapter map

RFC-0001's section numbers are generated when the specification is assembled. The authoring tree, its
manifest and the assembler are not published. The published specification is
[`dist/RFC-0001-core-spec.md`](dist/RFC-0001-core-spec.md), and its table of contents is the section list.
