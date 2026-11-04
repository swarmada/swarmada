# ADR-0019: Adapter action discovery and validation

- **Status:** accepted
- **Date:** 2026-07-23
- **Deciders:** Principal Software Architect, API Designer
- **Related:** RFC-0001 "Fleet Adapter Protocol", ADR-0006 (north side is the Kubernetes API), RFC-0003 (event-driven flows)

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

## Amendment 1 — the RFC-0003 seam moved forward into core, in two places

- **Date:** 2026-09-02
- **Basis:** ADR-0004 (RFC amendment and errata policy), second rule — a substantive
  change to an `alpha`, unreleased surface is corrected in place. `SupportedAction`,
  `CapabilitiesSnapshot` and the `Robot.status` projections are `alpha`, RFC-0001 is
  in `Draft`, and both changes below are purely additive, so no superseding ADR and no
  amendment RFC. Per `docs/adr/README.md` the original text above is left intact and
  this is appended rather than folded in.
- **Sources:** the Draft 4 design review, 2026-09-01.
- **What is unchanged:** the decision itself. The two-layer design stands — the pulled
  catalog remains a discovery and pre-filter surface, `ValidateAction` remains the
  authoritative per-instance check, and a catalog hit still MUST NOT be treated as
  authorization to skip assignment-time validation. What moved is the *deferral
  boundary* stated in the last paragraph of **Decision** and in the last two
  **Consequences** bullets.

Draft 4 crosses the reserved RFC-0003 seam **twice**. Both crossings are recorded here.

### Crossing 1 — `SupportedAction` gains a results descriptor

This ADR deferred "rich payload schemas" to RFC-0003 and closed by reserving
`SupportedAction` extension for that RFC. Draft 4 adds
`repeated ActionParam results = 5;` to `SupportedAction` as **core** work
(WS-B/B4 item 4), not RFC-0003 work.

**Why it moved.** `SupportedAction` declares what an action *takes* and nothing about
what it *produces*. That asymmetry was invisible while an action's only outputs were a
phase and a message. Draft 4 introduces parameter binding — a later step binding a
parameter to an earlier step's output — and tier-1 static checks (does the referenced
field exist; is its type compatible) cannot run against a producer that declares
nothing. Without the descriptor, *every* binding onto a robot-action producer falls
through to loose runtime resolution, so a mistyped field name surfaces when a patrol is
running rather than when the task is admitted. The compute side already declares its
outputs (`ComputeEndpoint.spec.results[]`), so leaving the robot side undeclared would make it
the weaker half of one mechanism for no principled reason.

**Why it could not wait for the RFC.** This is a proto field on the adapter contract,
and admission-time validation in core cannot depend on a descriptor that ships in a
later document. The descriptor has to be in the proto the Draft 4 adapters already
implement; deferring it would ship the validation and the thing it validates against in
different releases.

**Scope kept narrow.** It reuses the existing `ActionParam` shape rather than inventing
a second descriptor, is additive, and is optional — an empty `results[]` means "declares
no structured result", not "produces nothing". Rich payload *schemas* proper
(JSON-Schema-grade payload description) remain RFC-0003's and are not affected.

### Crossing 2 — per-robot materialisation onto `Robot.status`

This ADR deferred the per-robot projection onto `Robot.status` on two grounds: that
nothing in the initial scope required a materialised copy, and that it would add a
status-projection staleness vector. Draft 4's sensor-stream surface adds `SensorStream` to `CapabilitiesSnapshot` and projects it read-only onto
`Robot.status.sensorStreams[]`, written by the `CapabilitiesIngestor`. That is a
per-robot materialised projection of the same `CapabilitiesSnapshot` channel.

**Why the first ground no longer holds.** A `ComputeAction` names a stream on a robot.
Two things need the per-robot view *before any robot is committed*: a task author
discovering what a robot can see before writing the task, and the scheduler's
`SensorStreamNotOffered` gate refusing to schedule against a robot that
does not offer the named stream. Neither is derivable from `FleetAdapter.status` alone,
because stream availability is a function of the robot's active capabilities, not the
adapter's.

**The second ground is answered, not waived.** The staleness vector is bounded exactly
as the catalog's is, and by the same layering: the projection is written only by the
`CapabilitiesIngestor` from `CapabilitiesSnapshot`, at transition time under RA-1 and
never on a telemetry tick; and it is used as a pre-filter and discovery surface, never
as authority. A stale entry costs at most a refused or wasted scheduling cycle and an
adapter-side refusal — it cannot produce a wrong dispatch.

**What did not move.** The per-robot resolution of the *action catalog* — the adapter's
`supportedActions` filtered to a robot's active capabilities — is still not materialised
onto `Robot.status`; the Scheduler still derives it on demand, for the reasons the
original Decision gives. The deferral is narrowed to that case, not lifted wholesale.

### Consequences of this amendment

- The closing sentence of **Consequences** — "Leaves a clean seam for RFC-0003: richer
  per-action parameter schemas and event/trigger semantics extend `SupportedAction`
  without changing the two-layer contract" — now holds for its second half only. Read it
  as: event and trigger semantics remain RFC-0003's; the `SupportedAction` results
  descriptor and the per-robot sensor-stream projection are core. RFC-0003 inherits a
  narrower seam — payload schemas richer than the coarse `ActionParam` descriptor, event
  and trigger semantics, and the per-robot action-catalog resolution.
- Neither crossing changes the two-layer contract. Both new descriptors are declarative
  discovery surfaces projected under RA-1; `ValidateAction` remains authoritative for
  the per-instance question in both cases.
- Adapter obligations extend on the same MUST/MAY split: adapters that offer sensor
  streams MUST declare them in `sensor_streams`; `results[]` is optional per action.
  Both are additive proto changes, so they travel as a contract-version increment under
  ADR-0032, not a package-identity change.
- The forward references in the original text (`"deferred to RFC-0003"`, twice in
  **Decision** and twice in **Consequences**) are superseded by this section for these
  two surfaces. They are left in place as the record of what was decided on 2026-07-23.
