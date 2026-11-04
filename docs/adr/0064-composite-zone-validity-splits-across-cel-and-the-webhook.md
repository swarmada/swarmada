# ADR-0064: Composite-zone validity splits across CEL and the webhook, and partial resolution is asymmetric

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, API Designer
- **Related:** ADR-0056 (containment split across CEL and a webhook), ADR-0058 (zone restrictions are one model across three axes), ADR-0059 (a robot is bound by every zone that contains it), ADR-0063 (a field this tree cannot evaluate is absent, not inert), RFC-0001 #fleetzone-composite-zones-and-zone-expansion / #fleetzone-zone-hierarchy-invariants-controller-enforced; `api/v1/fleetzone_types.go`, `docs/api-principles.md`

## Context

A `FleetZone` carries two different relations to other zones, and conflating them is
the failure mode the specification is emphatic about:

- **`spec.parentZone`** — bottom-up, asserts **physical containment**, forms a **tree**,
  and is traversed for targeting, `sharedResources` inheritance **and** e-stop
  propagation (ADR-0058, ADR-0059).
- **`spec.memberZones`** — top-down, asserts **nothing physical**, forms a **DAG**, and
  is traversed for **targeting only**.

Measured 2026-09-15: the geometry half of the zone work was already complete, and
the **composite half was not in the schema at all**. RFC-0001
#fleetzone-composite-zones-and-zone-expansion said so about itself, roughly a dozen times: `spec.memberZones` was
*"specified, not in the v0.4 schema — a write to this path is pruned by the API
server."* An operator applying a composite zone received a success response and a zone
with no members (see ADR-0063, measurement 1).

Implementing it raises two questions that the RFC specifies the *rules* for but not
the *placement*: where each invariant is enforced, and what a consumer does when a
composite resolves only partially.

RFC-0001 §9.1.6.8 states six constraints (FZ-C1…FZ-C6). They are not all of one
type: FZ-C4 (no `sharedResources` on a composite) and FZ-C6 (`maxConcurrentRobots`
must be 0 on a composite) are properties of **one object's own spec**; FZ-C1 (not both
a containment parent and a composite), member existence, cycle rejection and the FZ-C3
8-edge depth bound are properties of **the zone graph**.

## Decision

**Single-object invariants are CEL on the CRD; cross-object invariants stay in the
admission webhook, and both the Go comment and the generated CRD description say which
is which.**

- **FZ-C4 and FZ-C6 are CEL rules on `FleetZoneSpec`**, using RFC-0001's own specified
  message strings verbatim. This follows `docs/api-principles.md` ("invariants that
  span fields are enforced in the schema with CEL, not left to controllers") and
  ADR-0056's precedent of splitting a containment rule across the two mechanisms.
- **Both rules are conditioned on a non-empty `memberZones`.** An unguarded rule would
  reject every ordinary zone in every existing deployment, since a capacity cap plus a
  corridor is the normal case. `TestEnvtest_NonCompositeZoneKeepsCapacityAndSharedResources`
  is the regression guard and fails if either guard is dropped.
- **FZ-C1, member existence, cycle rejection and the FZ-C3 depth bound remain the
  webhook's.** CEL over one spec cannot express them. They are **not** silently
  presumed covered: the type comment and the generated description state that the
  schema does not cover them.
- **`memberZones` breadth is deliberately uncapped.** RFC-0001 bounds the expansion
  **graph** at 8 edges, not the member list; inventing a breadth cap the RFC does not
  state would put the schema and the normative specification into disagreement.
- **Derived composition is status, not spec.** `FleetZoneStatus.Composition` carries
  `IsComposite`, `ExpandedZones` (`+listType=set` — expansion is a set union, so a zone
  reachable by two paths appears once and a robot in it counts once against capacity)
  and `UnresolvedMembers` (`+listType=map`, key `name`), with a
  `MemberResolutionReason` enum of `NotFound` | `Cycle` | `DepthExceeded` — the three
  reasons the RFC's `expand()` returns.

**Partial resolution is not one behaviour, and this is the safety half of the
decision:**

- **Targeting consumers proceed against the resolved subset.** Refusing all work
  because one member vanished converts a bookkeeping error into a fleet-wide outage.
- **Safety and operational holds are all-or-nothing.** A pause that silently misses a
  room is a technician walking into a zone they were told was drained.

## Alternatives considered

- **Put every FZ-C rule in the webhook.** Uniform and simple to reason about. Rejected
  for FZ-C4/FZ-C6: admission is bypassable — a webhook can be uninstalled, set to
  `failurePolicy: Ignore`, raced, or skipped by an etcd restore (ADR-0058 §"Admission
  is bypassable") — and a rule that CEL *can* hold should be held where the API server
  always enforces it.
- **Put every FZ-C rule in CEL.** Rejected as impossible rather than undesirable: CEL
  on a CRD sees one object, and cycle detection, member existence and a depth bound are
  all questions about other objects.
- **Enforce the graph rules in the Zone Controller instead of the webhook.** Rejected:
  the controller runs *after* the write is accepted, so a cyclic composite would be
  stored and would have to be repaired rather than refused.
- **Cap `memberZones` breadth in the schema** (e.g. `MaxItems`). Rejected — see above;
  the RFC bounds the graph, not the list.
- **One partial-resolution rule for all consumers.** Rejected in both directions.
  "Always proceed on the subset" silently under-applies a safety hold. "Always refuse"
  turns one deleted member zone into a fleet-wide targeting outage. The asymmetry is the
  decision.
- **Model composition with `parentZone` and skip the second relation.** Rejected: it
  would make a grouping assert physical containment, which would drag e-stop
  propagation and `sharedResources` inheritance across edges that assert nothing
  physical — exactly what ADR-0058 forbids.

## Consequences

- A composite zone now round-trips: the field is stored and reads back. Seven new tests
  accompany the change, five of them **envtests against a real API server**, chosen
  deliberately because a field absent from the generated CRD is pruned on write and
  comes back empty with no error at either end — which a fake client, performing no
  admission and pruning nothing, cannot distinguish from a correct field. The same is
  true of a CEL rule: a typo, a marker on the wrong type, or a rule dropped by
  regeneration all behave exactly like a correct rule there.
- **A malformed composite that violates only the graph rules is still accepted today.**
  The webhook half is unimplemented. This is a strict improvement over the prior state
  (it is now *stored* rather than silently pruned) and it is not full FZ-C enforcement.
- **Nothing computes `status.composition` yet**, so the block is schema-present and
  unwritten. Acceptable under ADR-0063 because no operator intent is being discarded:
  an empty derived block reads as "not computed", not as "no members".
- **Zone expansion in the Scheduler is untouched.** It still matches
  `FleetAction.spec.zone` against `Robot.spec.zone` by exact string equality, so
  targeting a composite matches no robot — for the same reason targeting a containment
  parent matches none. RFC-0001's standing advice, *"target a terminal zone until
  expansion lands"*, is unchanged.
- **RFC-0001 #fleetzone-composite-zones-and-zone-expansion and #fleetzone-zone-hierarchy-invariants-controller-enforced now understate what the schema does.** The
  *"pruned by the API server"* qualifier is false for the schema half and still true
  for the controller and webhook halves, so the correction is not a find-and-replace —
  each of roughly a dozen occurrences needs deciding individually. This is the most
  likely thing for a later session to trip over.
- RA-1 holds: `status.composition` is written by the Zone Controller when the zone
  graph changes, never on a telemetry tick.
