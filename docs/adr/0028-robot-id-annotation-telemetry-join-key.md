# ADR-0028: `swarmada.io/robot-id` is the canonical telemetry↔Robot join key

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** Control Plane / CRD-controller capability
- **Related:** RFC-0001 §9.2.3 (globally-unique robot_id), §9.3.1 (telemetry status projection), ADR-0014 (auto-admit DiscoveredRobots), ADR-0027 (suggested robot class on discover)

## Context

Telemetry arrives on the Fleet Adapter ControlStream keyed by the wire
`robot_id` — a stable, globally-unique identifier (serial / MAC) chosen by the
robot, not by the operator. The material-projection sink
(`internal/controller/robot_status_sink.go`) must resolve that `robot_id` to the
one Robot CRD it belongs to before it can write `status.battery`/`status.hardware`.

Robot CRD names, by contrast, are Kubernetes object names: DNS-safe, namespace-scoped,
and frequently operator-chosen. They are not required to equal the wire `robot_id`.
So the sink cannot join on `metadata.name` in general; it needs an explicit,
indexable identity field carrying the wire id. `resolveRobot` already reads one —
the `swarmada.io/robot-id` annotation (the `RobotIDAnnotation` constant in `api/v1`) —
and refuses to guess when zero or multiple Robots claim the same id.

The gap that forced this decision: for **adapter-discovered** robots the annotation
was never populated. `buildAutoAdmitRobot` (ADR-0014) constructed the Robot from the
DiscoveredRobot with only `Name`/`Namespace`/`Spec` and no annotations, and no other
controller set it. Every telemetry frame for such a robot resolved to nil and was
dropped: `status.hardware` stayed empty and every hardware-gated capability was stuck
`Inactive`. Hand-created and pre-existing Robots had the same latent gap whenever an
operator omitted the annotation.

The constraint in tension is RA-1: the control plane must not write `Robot.status`
on a telemetry tick. Any fix that resolves `robot_id → Robot` must not itself become
a per-tick status write, and must not weaken the projector's write-coalescing.

## Decision

Adopt `swarmada.io/robot-id` (the single-sourced `api/v1.RobotIDAnnotation`) as the
**canonical, sole** join key from a wire `robot_id` to a Robot CRD, and guarantee it
is always present:

1. **Stamped at admission.** `buildAutoAdmitRobot` sets
   `metadata.annotations["swarmada.io/robot-id"] = dr.Name` when it constructs an
   auto-admitted Robot. For adapter-discovered robots the DiscoveredRobot name *is*
   the announced `robot_id`, so the annotation value equals the Robot name.
2. **Defaulted when absent.** The Robot mutating webhook (`RobotDefaulter.Default`)
   backfills `swarmada.io/robot-id = metadata.name` whenever the annotation is unset,
   so hand-created and pre-existing Robots converge without operator action. An
   explicitly-set value is always preserved — an operator whose wire `robot_id`
   differs from the object name may set it deliberately, and the defaulter must not
   clobber that.

The annotation is **identity, not status.** It lives in `metadata.annotations`, so
stamping and backfilling it are spec/metadata writes performed at admission — never
`status` writes and never on a telemetry tick. RA-1 is preserved unchanged: the sink
still writes status only on a projector-approved material transition, and the join
key it reads was established at admission time.

## Alternatives considered

- **Join on `metadata.name` directly.** Rejected: Robot names are operator/DNS-scoped
  and not guaranteed to equal the wire `robot_id`; it would forbid renaming a Robot
  independently of its hardware identity and break any deployment whose naming scheme
  differs from the robot's serial.
- **A typed `spec.robotId` field instead of an annotation.** A cleaner model long-term,
  but it is a CRD schema change requiring `make manifests`, CEL/validation, and a
  migration for existing objects. The annotation join already exists in `resolveRobot`
  and in `api/v1`; the defect was purely that it was unpopulated. Deferred — this ADR
  leaves the seam to promote it to a typed field later without changing the join
  semantics.
- **Stamp only at auto-admit, not in the defaulter.** Rejected: it would leave
  hand-created and pre-existing Robots silently unjoined — the same empty-status
  failure, just for a different provenance. Doing both closes the gap for every path
  a Robot can enter the cluster by.
- **Resolve `robot_id` by writing it onto status and matching there.** Rejected
  outright: it inverts the dependency (status would have to exist before the first
  telemetry frame could land) and it is a status write, violating RA-1.

## Consequences

- **New obligation:** every path that creates a Robot must leave it joinable. Two
  now do (auto-admit stamps; the defaulter backfills). A future third creation path
  must either set the annotation or rely on the defaulter — which runs on all
  `create`/`update` admissions, so the default holds automatically.
- **Uniqueness is load-bearing.** `resolveRobot` errors if two Robots carry the same
  `robot_id` rather than writing to an arbitrary one. Because the value defaults to
  `metadata.name` (unique within a namespace) and auto-admit uses the unique
  DiscoveredRobot name, collisions only arise from a deliberate, duplicated explicit
  value — a spec violation the sink correctly refuses.
- **Cluster-wide List on resolve.** `resolveRobot` lists Robots cluster-wide today.
  Acceptable because material writes are projector-gated and rare; the code already
  carries a TODO to back it with a field indexer if that volume grows. This ADR does
  not change that trade-off.
- **RA-1 untouched.** The fix adds no status write and no per-tick work. The only new
  writes are at admission (metadata), which happen once per Robot create/update.
- **Drawback accepted:** identity lives in an annotation, which is weakly typed and
  invisible to CRD schema validation. A malformed or hand-cleared annotation degrades
  silently to "telemetry dropped" rather than a hard error. The defaulter mitigates
  the common case; promoting to a typed `spec.robotId` (see alternatives) remains the
  path to eliminate the drawback entirely.
