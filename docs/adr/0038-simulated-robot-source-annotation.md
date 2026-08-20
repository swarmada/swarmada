# ADR-0038: Declare simulated Fleet Adapters and stamp robot-source onto Robot

- **Status:** accepted
- **Date:** 2026-08-15 (proposed) · decided 2026-08-19
- **Deciders:** Al — Option 1 (`spec.simulated` bool + `robot-source` annotation) approved now;
  Option 2 (`AdapterKind` enum, see Alternatives) explicitly deferred, to be revisited only if a
  genuine third category (digital twin) gets a real consumer
- **Related:** RFC-0001 §5.3 (Fleet Adapter), ADR-0005 (reference adapter policy — vendor adapters
  own their own repo), ADR-0017 (the coordinate-system annotation stamp this reuses the pattern of)

## Context

Nothing in the public API distinguishes a Fleet Adapter that drives simulated robots
(a simulator binding, a digital twin) from one that drives physical hardware. `FleetAdapterSpec`
has no such field, and neither `Robot` nor `DiscoveredRobot` carries any annotation, label, or
status field naming `simulated`/`virtual` (re-measured directly against this repo's `api/` and
`internal/` 2026-08-15: `grep -rni "simulated\|robot-source" --include="*.go" api/ internal/ |
grep -v _test` — zero matches).

This surfaced building a Fleet Adapter for a physics simulator (Isaac Sim): every robot it drives
is simulated, and there is no vendor-neutral way to say so. The gap is general, not
simulator-specific — the same need exists for any digital-twin or test-fixture adapter, and
several consumers benefit from an on-object answer without back-tracking through
`Robot.spec.adapter.name` to a `FleetAdapter` and reasoning about its `vendor` string: scheduler
policy that should never route a real customer's inventory task to a simulated robot, dashboards
and cross-tooling that should visually distinguish the two, and operators auditing a namespace who
want to tell at a glance which Robots are real.

## Decision

**Add `FleetAdapter.spec.simulated: bool`, and extend the existing Robot defaulting webhook
(`internal/webhook/robot_defaulter.go`'s `RobotDefaulter` — the same one ADR-0017 extends for
`coordinateSystem`) to stamp `swarmada.io/robot-source: Simulated|Physical` onto every `Robot` at
admission, read from the `FleetAdapter` it binds to.**

1. `spec.simulated` is a plain bool on `FleetAdapterSpec`, defaulting to `false` — an adapter that
   does not declare itself is assumed physical. This is the fail-safe direction: a physical-adapter
   default that gets misread as simulated only makes scheduling *more* conservative (a real robot
   momentarily treated as sim-only fails safe by doing nothing extra); the reverse — a simulated
   adapter defaulting to "physical" — is the one that could not go uncaught, and it doesn't, because
   `false` there is exactly what an undeclared adapter already means today (nothing distinguishes
   it), so this is purely additive.
2. `RobotDefaulter` resolves `robot.Spec.Adapter.Name` to its `FleetAdapter` (same namespace) and
   stamps `swarmada.io/robot-source` onto the `Robot`, mirroring `stampCoordinateAnnotations`'s
   fail-safe shape: an unresolvable adapter (unset name, not-found, read error) leaves the
   annotation unset — never a guessed value. An absent annotation means "unknown," not "assumed
   physical" — the bool's own default already carries that meaning at the `FleetAdapter` level; the
   annotation only records what could actually be resolved.
3. Purely informational, exactly like ADR-0017's stamps: **never used to gate admission**, never a
   safety signal, never transforms or validates anything. `RobotAdmissionGate`'s existing gate
   (Connected + Conformance == Passed) is unchanged — a simulated adapter is exactly as admissible
   as a physical one; this ADR only makes the distinction visible on the object.

## Alternatives considered

- **A first-class `AdapterKind` enum** (`Physical | Simulated | DigitalTwin`) instead of a bool.
  More future-proof if a third category (a genuine digital twin mirroring a physical robot 1:1,
  distinct from a pure simulator) becomes real — but that distinction has no consumer today, and a
  bool that later needs a third state can migrate to an enum additively (the annotation value space
  only grows) without a breaking change. Deferred, not rejected.
- **A label instead of an annotation.** Rejected — labels are for selection/grouping; nothing here
  selects Robots by source today, and ADR-0017 already established annotations as this codebase's
  idiom for admission-time informational stamps. Revisit if a selector use case (e.g. `swarmctl get
  robots -l swarmada.io/robot-source=Simulated`) materializes — annotations don't support label
  selectors, so that would force the migration.
- **Do nothing; let each consumer infer simulated-ness from `FleetAdapter.spec.vendor` or
  `endpoint`.** Rejected — vendor strings are free text with no enforced simulator convention, and
  every consumer would need to maintain its own guesswork (`vendor == "isaacsim"`,
  `endpoint contains "sim"`, …), which is exactly the divergence risk a shared, declared field
  exists to prevent.
- **A controller that reconciles the stamp on a watch, instead of a defaulting webhook at
  admission.** Rejected for the same reason ADR-0017 rejected it for coordinates: the source of a
  Robot's binding is set-once at admission, not evolving state that needs a reconcile loop; a
  webhook stamp is cheaper and needs no extra controller.

## Consequences

- **Good.** Every consumer (scheduler policy, dashboards, cross-tooling, operators) gets the
  simulated/physical distinction directly off the `Robot` object it already reads, with zero
  simulator-specific vocabulary anywhere in the public API — the field and the annotation are
  exactly as neutral as `coordinateSystem`'s.
- **Obligation.** Extend `RobotDefaulter.Default` to resolve the bound `FleetAdapter` and stamp
  `swarmada.io/robot-source`; add the `Simulated` field to `FleetAdapterSpec` with
  `make generate && make manifests`; tests for the stamp and its fail-safe (unresolvable-adapter)
  path, mirroring `robot_defaulter_coords_test.go`.
- **RA-1 / safety.** Purely additive metadata at admission, same as ADR-0017 — no status writes, no
  behavioral change, no safety surface. `RobotAdmissionGate` is untouched.
- **Drawback accepted.** Like the coordinate stamp, this can drift if an operator re-points
  `robot.spec.adapter.name` to a different-source adapter after admission without triggering a
  re-merge (`RobotDefaulter` only re-stamps coordinates unconditionally; the robot-source stamp
  follows the same admission-time-only semantics). Acceptable for informational metadata; documented
  as a known limitation, same as ADR-0017's.
- **Who proposes this.** This ADR originates from a private downstream adapter author (building a
  Fleet Adapter for a physics simulator) who confirmed the gap by reading the current `api/v1`,
  `internal/webhook`, and `docs/adr` sources directly rather than assuming, and is proposing it back
  through this project's normal intake process (`ITEM-0103`) — not merging it unreviewed.

## Numbering note

Filed originally as a draft "0037" against a stale read of `docs/adr/` that hadn't seen
`0037-protocol-profiles-record-not-enforce.md` (accepted 2026-08-13). Renumbered to 0038 on
2026-08-15 after re-checking the actual highest ADR in this checkout. A second collision surfaced
2026-08-19 when two ADRs copied over from the `swarmada` working tree were found already occupying
0037/0038 in this checkout; those two were renumbered to 0039/0040 instead of moving this one again.
This ADR is settled at **0038**.
