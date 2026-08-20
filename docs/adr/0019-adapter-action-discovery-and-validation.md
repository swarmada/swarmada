# ADR-0019: Adapter action discovery and validation

- **Status:** accepted
- **Date:** 2026-07-23
- **Deciders:** Principal Software Architect, API Designer
- **Related:** RFC-0001 §9.2 (Fleet Adapter Protocol), ADR-0006 (north side is the Kubernetes API), RFC-0003 (planned, event-driven flows)

## Context

A `FleetAction` names an atomic unit of work by `spec.type` and carries an opaque
`spec.payload` of robot-native ("Robot OS") commands. By design, each Fleet Adapter
provider decides which Robot-OS commands it supports and at what level, so two
adapters for two vendors will serve different, overlapping sets of action types and
payload shapes. Two questions follow from this, and they are not the same question:

1. *What kinds of action can a given adapter serve at all?* — needed for operator and
   tooling discovery, for schedule-time pre-filtering, and for rejecting an
   unserviceable submission early rather than after a wasted scheduling cycle.
2. *Can a specific robot serve this specific action instance right now?* — needed for
   the contextual validity a static description cannot capture: a degraded
   capability, a robot-specific limit, the concrete payload.

RFC-0001 already added `ValidateAction` (a push RPC) to answer question 2. The open
decision is whether adapters should *also* expose a pulled catalog of supported
actions to answer question 1, or whether one mechanism should serve both. The forces
in tension: adapter implementation burden; the risk of a discovery catalog going
stale against live robot state; per-action round-trip latency at admission and
assignment; the value of upfront discoverability for a vendor-neutral ecosystem; and
the project goal of keeping "what is supported" declarative and poll-able rather than
hidden behind per-vendor validation code.

## Decision

Provide **both**, layered. Keep `ValidateAction` as the authoritative per-instance
check. Add a **pulled supported-action catalog**: each adapter advertises a
`repeated SupportedAction` list inside the `CapabilitiesSnapshot` it returns to
`Command.scan`. The control plane pulls the catalog at registration and on each
capability scan, projects it read-only onto `FleetAdapter.status` under the RA-1
status-write discipline, and uses it as a cheap
local pre-filter. Admission rejects an action whose `type` no connected adapter
advertises without a round-trip; the Scheduler never dispatches a `type` the
candidate robot's adapter cannot serve; `ValidateAction` remains the authoritative
per-instance confirmation at admission and at assignment. Initially the catalog carries
only action `type`, required capabilities, and a coarse parameter descriptor
(`ActionParam`: name, unit, kind, numeric range, enum values, required); rich payload
schemas are deferred to RFC-0003. Initially the projection materialises onto
`FleetAdapter.status` only; the per-robot resolution onto `Robot.status` (the adapter
catalog filtered to a robot's active capabilities) is a denormalisation the Scheduler
derives on demand and is deferred to RFC-0003 — nothing in the initial scope requires the
materialised copy, and it would otherwise add a status-projection staleness vector.

## Alternatives considered

- **Push-validate only (`ValidateAction` alone).** Authoritative and stateless for the
  control plane, but every admission/assignment pays a round-trip, nothing can be
  discovered or pre-filtered, operators and planners fly blind, and it couples the
  control plane to per-adapter validation logic — weaker for a neutral standard.
  Rejected as the sole mechanism; retained as the authoritative layer.
- **Pull-catalog only.** Enables discovery, schema-driven UX, and pre-filtering, but a
  static description cannot capture contextual validity (battery, a degraded
  capability, payload specifics), so a catalog-valid action can still fail at
  execution — the failure moves later, it does not disappear. Rejected as the sole
  mechanism.
- **Capability taxonomy + `AssignActionResult` rejection (status quo).** Simplest, no
  new surface: `requiredCapabilities` gate schedulability and the adapter can reject at
  assignment. But the capability taxonomy is coarse, the payload is opaque, discovery
  is absent, and failure surfaces only at assignment after a scheduling cycle is spent.
  Rejected as insufficient for action-level granularity.

## Consequences

- Mirrors the Kubernetes pattern the project already follows: a discovery surface
  (what resources/verbs exist, cacheable) plus admission control (is this instance
  valid). Catalog = discovery; `ValidateAction` = admission.
- The catalog's main weakness — staleness — is bounded by the layering: it is a
  pre-filter and a discovery/UX surface, never the source of truth, so a stale catalog
  degrades to at most one extra `ValidateAction` round-trip, never a wrong dispatch.
- New obligations: adapters MUST populate `supported_actions` in `CapabilitiesSnapshot`
  (conformance-checklist item); the control plane MUST project it under RA-1 and
  MUST NOT treat a catalog hit as authorization to skip assignment-time validation for
  adapters that implement it. `FleetAdapter.status` gains a read-only
  `supportedActions` projection (CRD field addition tracked with the `ValidateAction`
  runtime work); the per-robot `Robot.status` projection is deferred to RFC-0003.
- Accepted drawback: two mechanisms to implement and keep consistent, and a modest
  increase in adapter surface. Mitigated by keeping the initial descriptors coarse and
  deferring rich payload schemas to RFC-0003.
- Leaves a clean seam for RFC-0003: richer per-action parameter schemas and
  event/trigger semantics extend `SupportedAction` without changing the two-layer
  contract.
