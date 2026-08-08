# ADR-0017: Surface the namespace coordinate system as stamped annotations, not transformation

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** API Designer, Kubernetes Expert
- **Related:** RFC-0001 §9.1.11.11 (coordinate system), FleetZone/Robot schemas,
  ADR-0006 (north side is the Kubernetes API)

## Context

`SwarmadaConfig.spec.coordinateSystem` declares facility-wide spatial conventions —
`lengthUnit` (Meters|Millimeters), `angleUnit` (Radians|Degrees), `groundFloor`, and
an `origin` description. Its own doc is explicit that these describe conventions in
which every coordinate elsewhere (FleetZone `physicalBounds`/`waypoints`,
`Robot.status.position`, the edge `PositionFrame` stream) is **already expressed**:
"the control plane never transforms coordinates, only validates, annotates, and
informs." Nothing currently reads the block.

So the question is narrow: given that the control plane must not transform
coordinates, what is the useful, in-scope behavior? The fields are enum-constrained
already (the API server rejects a bad unit), and coordinates are consumer-interpreted
(adapters, edge nodes, tooling) — those consumers today have no on-object signal of
which convention applies; they would have to fetch the SwarmadaConfig separately.

## Decision

**Stamp the coordinate convention onto FleetZone and Robot as informational
annotations via a defaulting webhook; do not transform or hard-validate coordinates.**

1. A defaulting webhook (extending the existing Robot/FleetZone defaulters) reads the
   namespace `coordinateSystem` and stamps annotations —
   `swarmada.io/length-unit`, `swarmada.io/angle-unit`, `swarmada.io/ground-floor` —
   so any consumer reading the object knows the convention without a second lookup.
   Fail-safe: no config ⇒ the CRD defaults (Meters/Radians/0).
2. **No coordinate transformation and no rejection of coordinate magnitudes.** The
   spec forbids transformation, and unit-plausibility checks (e.g. "yaw looks like
   degrees") are heuristic and fragile; they are out of scope.
3. `origin.description` stays purely informational on the config; it is not stamped
   (free-text, not a machine convention).

## Alternatives considered

- **A validating webhook that checks coordinates against the declared units** (e.g.
  yaw within [0, 2π) when Radians). Rejected — magnitude checks are heuristic, produce
  false rejections (a robot legitimately at yaw 0 tells you nothing), and edge toward
  the transformation/interpretation the spec explicitly excludes.
- **A controller that writes the units into each object's status.** Rejected — a
  webhook stamp at admission is cheaper and needs no reconcile loop; the convention is
  set-once namespace policy, not evolving state. Annotations are the idiomatic place
  for advisory, non-spec metadata.
- **Do nothing (leave the block purely documentary).** Tempting given the low value,
  but consumers genuinely benefit from an on-object signal, and the stamp is cheap.
  Still, this is the lowest-priority item in the backlog and could be deferred without
  risk.

## Consequences

- **Good.** Adapters, edge nodes, and tooling get the coordinate convention directly
  from the FleetZone/Robot they already read — no second config fetch, no divergence
  risk. Stays strictly within "annotate and inform," never transforming.
- **Obligation.** Extend the existing defaulters to read `coordinateSystem` and stamp
  the annotations; tests for the stamp and the fail-safe defaults. If the defaulter
  webhooks do not currently read SwarmadaConfig, add that read (informer-cached).
- **RA-1 / safety.** Purely additive metadata at admission; no status writes, no
  behavioral change, no safety surface.
- **Drawback accepted.** Annotations can drift if an operator edits the
  `coordinateSystem` after objects are admitted (the stamp is set at admission time).
  Documented as a known limitation; a re-stamp controller is a possible future
  refinement but is not justified for informational metadata.
- **Priority.** This is the least load-bearing of the remaining items; reviewers may
  reasonably choose to defer it. The ADR records the decision so it need not be
  re-derived.
