# ADR-0071: A stop is confirmed by a channel that can see motion, any channel can veto it, and the estop receipt is never a stop

- **Status:** accepted
- **Date:** 2026-09-19
- **Deciders:** Maintainers, Safety Engineer, Spec Editor
- **Related:** ADR-0032 (the `Estop`/`EstopAck` schema is version-invariant), ADR-0048 (a verdict
  grades obligations), ADR-0070 (a report grades each row and names its unlock), ADR-0007
  (conformance is self-certified); `adapters/CONFORMANCE.md` C5; RFC-0001
  `fleet-adapter-protocol.md`, `safety.md`, `conformance-profiles.md`; `conformance/PLANT.md`;
  `proto/fleet_adapter/v1/fleet_adapter.proto` (`EstopState`)

## Context

`C5.3` forbids an adapter to report `ESTOP_STATE_STOPPED` by inferring it from absent motion, a
timeout or telemetry. The state must be adapter-confirmed. The catalog concedes that inference is
not decidable from the wire, and takes `stop_initiated_at` as necessary evidence, not proof.

Two gaps follow from the text as it stood.

**"Confirmed" did not say confirmed by what.** On at least one test platform, the only motion
reading that reaches software is odometry whose linear velocity is the commanded velocity,
clamped. Nothing on that platform measures linear motion. The platform's IMU can refute rest,
since a push or a collision registers as acceleration. It cannot confirm rest, because constant
velocity produces no reading. An adapter on that platform has two behaviours available:

1. **Honest.** It never confirms rest, so under `C5.2` it answers `ESTOP_STATE_FAILED` to every
   estop, and every estop reads as a failure.
2. **Passing.** It reports `STOPPED` from odometry, which echoes the stop command. The wire cannot
   tell the difference, and the suite grades `C5.3` as passed on the very inference `C5.3`
   forbids.

The suite had no way to tell the two apart. A reviewer reads the `safety` profile as "this robot
stops when told, and says so truthfully". On such a platform the profile could only be earned by the
behaviour it exists to rule out, and that result looked the same as an honest one.

**The first acknowledgement read as a stop.** `C5.5` bounds the first `EstopAck` at 500 ms, and
`C5.2` lets that ack be `ESTOP_STATE_STOPPING`, whose proto comment read "command received; robot
is decelerating". The name and the comment together read as a stop in progress that can be relied
on. Only `STOPPED` attests rest, and a `STOPPED` sent before rest was undetectable.

The layer boundary sets the scope of this decision. RFC-0001 `safety.md` claims Layer 3 only, the
control plane. The control plane cannot observe motion. It acts on `STOPPED` as an attestation, and
every decision it takes on a confirmed stop depends on the attestation being true. What may ground
the attestation is therefore an obligation on the adapter (Layer 2). This ADR states that
obligation and how a report grades it. It makes no claim about stopping distance or the physical
stop.

## Decision

### 1 · Confirmation needs a channel independent of the command path

An adapter MAY report `ESTOP_STATE_STOPPED` only when at least one motion channel that is
**independent of the command path**, and able to confirm rest, shows the robot at rest.

A channel is independent of the command path when both of these hold:

1. its reading is produced by a sensor that observes the robot's actual motion, or the world
   relative to the robot; and
2. no velocity command, setpoint, or quantity computed from one enters the computation of that
   reading, neither directly nor through a filter or estimator that fuses it with other inputs.

The test is counterfactual. If the drive did not do what it was commanded, would the channel show
the difference? Examples are a slipping wheel, a stalled or free-wheeling motor, or a person pushing
the robot. Wheel or motor encoders read from the drivetrain, a safety-rated scanner's motion output
and an external witness can qualify. Odometry whose velocity reproduces the commanded velocity does
not qualify, and neither does any estimate fused from it. A confirming channel must also be able to
observe motion at constant velocity, which an IMU cannot do.

The definition is written so that it cannot collapse back into the status quo. The status quo
reading of "confirmed" was "whatever the platform reports as motion". Condition 2 excludes a reading
that carries the command. The fusion clause excludes laundering the command through a filter. The
counterfactual test turns the question from "what is the channel called" into "what would it show".

### 2 · Refute wins

If **any** channel available to the adapter shows motion, the adapter MUST NOT report
`ESTOP_STATE_STOPPED` while it does. This includes channels that cannot confirm rest: an IMU, a
changed scan of a static scene, a bumper contact. A channel does not need to be able to confirm
rest in order to veto it. Disregarding an available channel, or setting its threshold above the
motion it exists to detect, is the inference `C5.3` forbids. The error falls in the safe direction.
A spurious refutation delays `STOPPED` and may end in `FAILED`. It never produces a `STOPPED` for a
moving robot.

### 3 · The precondition, and how a report shows it unmet

The `safety` profile is claimable only on a platform with at least one independent motion channel.
The `safety` profile is Core at every capability tier, so an adapter driving a platform without
such a channel is conformant there at no tier. A report for such an adapter grades the `C5.3` row
`not-supported-by-platform` (ADR-0070), with an `unlock` naming such a channel. That grade records
an obligation left unestablished, not one that never applied. It is **not** a cause in ADR-0048's
never-an-obligation set, so it keeps the verdict from reaching `pass`.

The precondition applies to a hardware run. A simulated run has no platform channels to judge, and
its `C5` rows stay `measured-simulation`.

The suite does not implement this yet. No check reads which channels a platform has, and a run on
such a platform today can still grade `C5.3` as measured. The catalog says so in its text. The
harness change requires a skip cause whose grade is `not-supported-by-platform` and that is
outside the never-an-obligation set. Until that change lands, a hardware `C5.3` result depends on
the adapter author's account of its confirming channel. `conformance/PLANT.md`'s guided motion test
is the intended evidence, and no check reads it yet.

### 4 · The receipt keeps its 500 ms bound, and its meaning is fixed on the existing value

The first acknowledgement stays. Without it, the control plane cannot tell "received, braking" from
"lost, still driving" within the budget, so it cannot escalate on the second.

`ESTOP_STATE_STOPPING` means *received and braking; rest not yet confirmed — never a stop*. The
meaning is carried on the existing value: the proto comment on `ESTOP_STATE_STOPPING` is rewritten
to say so, and `C5.2`, `C5.5` and the RFC's estop text use that meaning. No value is added,
renamed or renumbered.

A comment carries no wire semantics. The descriptor is unchanged, the breaking-change check stays
clean, and the `Estop`/`EstopAck` schema stays inside ADR-0032's version-invariance guarantee.
What the change fixes is what every reader of the value is told it means. The reference control
plane already treats `STOPPING` as "commanded, awaiting confirmation". It resolves a
`STOPPING` that is not followed by `STOPPED` to `FAILED`, so the new text states what the code
already does.

### 5 · Hardware qualification: an early `STOPPED` fails (specified, not implemented)

On a hardware run with an external motion witness, a `STOPPED` whose time precedes the witnessed
moment of rest, by more than the witness's timing resolution, is non-conformant. The row is
`failed`, and the report flags the robot as well as the adapter. An early `STOPPED` means one of two
things. Either the adapter inferred rest, or the channel it confirmed rest from does not measure the
robot's motion, and the second is a fact about the platform. The catalog defines how both times are
taken:

- **The witnessed moment of rest:** the start of a hold interval below the witness's own stated
  rest noise.
- **The time of `STOPPED`:** `confirmed_at_ms`, placed on the witness's clock by a measured offset.
  Without a measured offset, the harness's receipt time is used, and the result states that the
  comparison is lenient.

~~A simulated run cannot supply the witness, so there the check is `not-testable-here`, with the witness as its unlock. The check is specified in the catalog and is not in the harness. It carries no check identifier yet, and assigning one, together with its profile placement, is part of the harness change.~~ **Corrected by Amendment 1:** a simulator's plant model is a witness in a simulated run, and a run with no witness is `not-testable-here`. The check is in the suite as `C5.7`.

### 6 · Carried to draft 5

- **The confirmation source on the wire.** The adapter would attest whether its rest confirmation
  is measured or command-derived, and a profile could grade command-derived confirmation as not
  meeting `C5.3`, or under a separately named lower level. This is a `fleet_adapter.v1` change and
  needs a contract-version decision.
- **A distinct receipt value.** A new `EstopState` value would name the receipt on the wire and
  stop overloading `STOPPING`. Adding a value to `EstopAck` is a contract bump against ADR-0032's
  frozen estop schema, and every control plane in the supported range must parse it. It is
  recorded here as the durable fix, and section 4 is the draft-4 measure.
- **A hazard advisory from an external observer.** An operator, another robot or a fixed camera
  may see a hazard, for example a robot about to hit a wall. Today the standard can carry that
  observation only as a command: `estop`, a zone constraint, or the Zone Controller's local safety
  input. An advisory would ask the adapter to cross-check the observation against what it senses
  and to answer. It needs a new message and a new event subject kind.

## Alternatives considered

- **`FAILED` is the honest answer, and is graded conformant**, where rest cannot be confirmed.
  Rejected. Every platform could then conform, but every estop on such a platform would report
  failure. The control plane's handling of `FAILED` (operator escalation, the robot treated as
  possibly still moving, no automated exit) would become the normal path. An operator taught that
  `FAILED` is routine stops reading the one signal that must stay rare. Under this ADR, `FAILED`
  remains the honest answer on such a platform. Such a platform does not hold `safety`, though.
- **Status quo, stated:** "confirmed" means whatever the platform reports as motion, whatever its
  provenance. Rejected. On a command-derived platform the statement is hollow, and the suite would
  go on certifying an echo as a confirmation.
- **The confirmation source declared on the wire, now.** Deferred, not rejected; see section 6.
  It is the precise answer, and it fits platforms of every kind, but it is a proto and attestation
  change of draft-5 size. The precondition and the refute rule deliver the safety property in
  catalog text alone.
- **An external physical witness as the confirmation rule.** Not adopted as the runtime rule,
  because a qualification fixture gives an adapter no help at run time. It is adopted as the
  hardware-qualification check in section 5, where it is the only evidence that can show that a
  `STOPPED` came early.
- **Drop the first acknowledgement, and make the first ack terminal.** The intuition is that an
  estop ack should never arrive before the robot has stopped. Rejected, for the reason section 4
  gives: a real base takes longer than 500 ms to reach confirmed rest. With no receipt, a control
  plane waiting for rest cannot tell a lost estop from a slow one until it is too late to escalate.
  The intuition is kept where it holds: `STOPPED` must not precede rest (section 5).
- **Rename `ESTOP_STATE_STOPPING`.** Rejected. `C14.2` and ADR-0032 pin the estop field numbers and
  names across the supported range. A rename would also change the generated identifiers and the
  JSON name in every binding. A documented meaning buys the same reader-facing correction at no
  wire cost.
- **A new check identifier in the catalog now.** Not taken. Every catalog check must be assigned
  to a profile (ADR-0066), and the specification's drift check fails a catalog check that the
  harness neither exercises nor records as an explicit skip. An identifier added to
  the catalog alone would be one the suite neither runs nor grades. It is assigned together with
  the harness change.

## Consequences

**What a reader gains.** A `STOPPED` in a conformant report now means that a channel able to see
motion saw none and no available channel saw any. `STOPPING` can no longer be read as a stop. A
platform that cannot confirm rest is named as such in the report, with what would change that,
instead of passing on an echo.

**What some platforms lose.** A platform whose only motion reading is command-derived cannot hold
`safety`, however good its adapter, until it gains an independent channel, such as encoders, a
scanner's motion output or an external witness. That is the intended outcome. A result on such a
platform may still be published, graded as it is.

**What is enforced.** A run profiled with a plant declaration (`--plant`) grades the precondition
from the declaration's motion-channel fields: a platform with no independent motion channel gets the
cause `no-independent-motion-channel`, which is outside the never-an-obligation set. The witnessed
early-`STOPPED` check is `C5.7`, run with a motion witness (`--witness-file`). A run with no plant
declaration cannot apply the precondition, and one with no witness skips `C5.7`; the catalog says
so.

**The proto.** One comment changes on one enum value, and the generated Go stub's copy of the
comment changes with it. There is no descriptor change and no contract-version movement.

## Amendment 1 — the early-`STOPPED` check is in the suite, and a simulator's plant model is a witness

- **Date:** 2026-09-19
- **Basis:** `docs/adr/README.md` — the decision for hardware is unchanged, so no superseding
  ADR. §5 is extended, and its sentence *"A simulated run cannot supply the witness"* is
  corrected rather than re-read.
- **What is unchanged:** the precondition (§3), refute-wins, the meaning of `STOPPING` (§4),
  and the hardware check as §5 specifies it.

**§5 is implemented as `C5.7`** (`adapters/CONFORMANCE.md`, `safety` profile, Core,
MUST NOT). The harness takes a motion witness (`--witness-file`, JSON lines of the robot's
speed over time with the witness's rest noise, hold time and resolution) and grades the
terminal `STOPPED` against the witnessed moment of rest exactly as §5 defines both times.
Without a witness the row is `not-testable-here`.

**A simulated run can supply a witness.** §5 said it cannot. What makes a witness a
witness is that its record does not come from the adapter's confirmation path, and a
simulator's plant model meets that: the reference simulation adapter's kinematic model
writes the robot's true speed each time it advances the physics, and the adapter's
`STOPPED` comes from a different path that reads the model's state. Graded against that
record, a simulated run establishes that the adapter waits for the rest its plant
reports. It still establishes nothing about a physical platform, and the result says so:
the report's `subject.fidelity` is `simulated`, and the witness names itself as a
simulated plant.

**The precondition is graded from the plant declaration.** A profiled run given a
`plant.yaml` whose odometry topics are all `command-derived`, with none `measured`, records
`C5.3` with the cause `no-independent-motion-channel`, graded `not-supported-by-platform`
and kept out of ADR-0048's never-an-obligation set (ADR-0070's cause table lists it).

**The `profiles.yaml` statement §3 required** is on the `safety` profile as `precondition:`.

