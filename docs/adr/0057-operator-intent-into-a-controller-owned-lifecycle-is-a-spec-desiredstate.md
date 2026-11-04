# ADR-0057: Operator intent into a controller-owned lifecycle is `spec.desiredState`, and it can only add a hold

- **Status:** accepted
- **Date:** 2026-09-06
- **Deciders:** API Engineer, Controller Engineer
- **Related:** ADR-0047 (control intent is level-triggered declarative state), ADR-0045 (a
  `desiredState` hold is entered declaratively and lifted only by an operator), ADR-0041 (a rollout
  resume excludes rather than retries), ADR-0021 (`policy-reset`: the operator-override precedent),
  ADR-0013 (bounded graceful drain), RFC-0001 "FirmwareRollout", "ModelRollout", "ZoneMaintenance"

## Context

Two resources had a phase an operator needs to reach and only the controller could reach.

**Rollouts.** `RolloutPhasePaused` is written on one path — `pauseOnError` — and `status.pausedAt`
with it. Neither rollout spec carried `desiredState`, `suspend`, or any equivalent, so an operator
watching a rollout they had decided was wrong could not stop it. The only exit was to delete the
object, which is a different operation: it does not unwind what the rollout did, and on `ModelRollout`
it strands the capabilities the rollout suspended.

**ZoneMaintenance.** `Completed` was reachable only through the auto-resume deadline. The controller
said so in its own comment — *"this is the only path that reaches Completed"* — and
`autoResumeAfterMinutes` **defaults to 0**, whose documentation reads *"0 means the operator must
manually resume."* No mechanism for that manual resume existed. A window created with defaults could
be activated and never closed; robots stayed paused until the object was deleted.

They are one defect: **the API modelled what the controller does and not what the operator needs.**

**What already existed, and what the items got wrong.** A re-derivation against the tree found the
rollout half is partly solved: `swarmctl rollout resume` writes `swarmada.io/rollout-resume`, gated on
a `rollout-resume` custom verb, and both controllers act on it (ADR-0041). The item's claim that "no
annotation verb exists either… none addresses a rollout" is false. What is missing is *pause* and
*stop*, not resume. ZoneMaintenance has no verb at all — `swarmada.io/zonemaintenance-resume` is a
**finalizer**, not an intake.

## Decision

**Operator intent into a controller-owned lifecycle is a `spec.desiredState` enum, matching
`FleetAction`/`FleetTask`.** Not a subresource, not an annotation verb, not a second CRD.

- `FirmwareRolloutSpec.desiredState` and `ModelRolloutSpec.desiredState`: `Running` | `Paused`,
  default `Running`.
- `ZoneMaintenanceSpec.desiredState`: `Active` | `Completed`, default `Active`.

Kubernetes answers this question with spec fields — `Deployment.spec.paused`, `CronJob.spec.suspend`
— and this project already answered it once. A second idiom for the same problem is worse than a
slightly imperfect fit with the first, and the imperfect fits here are small: a rollout's hold reads
naturally as `Paused`, and a window's close reads naturally as `Completed`, matching the phase each
is asking for.

**And the rule that makes it safe: a `desiredState` can only ADD a hold, never release one it did not
place.**

- Writing `Paused` on a rollout withholds dispatch of the next robot. Writing `Running` withdraws
  *that* hold and nothing else. A rollout halted by `pauseOnError` stays halted under `Running`; its
  exit remains the `rollout-resume` verb, because that verb also decides what happens to the robots
  that failed (ADR-0041), and a spec field cannot make that decision.
- Writing `Completed` on a `ZoneMaintenance` **requests** a close. The controller runs the same gated
  resume the deadline runs, through the same branch, and reaches `Completed` only when every robot it
  paused is actually resumable.

## Why this departs from ADR-0045, and why the departure is safe

ADR-0045 made `FleetAction.desiredState` asymmetric: writing `Running` onto a held action does **not**
resume it. The reason was that `Paused` had two sources — the estop path and `desiredState` — that an
operator could not tell apart, so one resume rule had to cover both, and it had to be the safe one.

Here the two sources are distinguishable **by construction**, and that is what changes the answer:

1. The operator's hold lives in `spec`, which the operator wrote and the controller never touches.
   "Did I pause this?" is answered by reading the object's own spec.
2. A `conditionOperatorHold` condition states which hold is in force, so `kubectl describe` answers
   it without cross-referencing.
3. Most importantly, **the operator's field cannot release the safety halt at all.** The two holds
   compose — dispatch happens only when neither is in force — so the failure ADR-0045 was guarding
   against is not only discouraged here, it is unrepresentable.

The asymmetry was a consequence of ambiguity, not a value in itself. Remove the ambiguity and
symmetric intent is both safe and less surprising: a field called `desiredState` that ignores half its
values is the `requireSignatureVerification` trap (ADR-0051, ADR-0052) — a field that accepts a value
and does nothing with it.

## The interactions, decided explicitly

**The controller pauses on error and the operator then resumes.** Writing `desiredState: Running` does
nothing to it: the halt is derived from `status.failedRobots`, which `Running` does not touch, so the
rollout stays `Paused` and the error condition does not "reassert" — it never lifted. The exit is
`swarmctl rollout resume`, which moves the failed robots to `status.excludedRobots` and clears
`failedRobots`, so `pausedOnError` re-derives false and cannot re-pause on the same robots. If an
operator hold is *also* in force, the rollout stays `Paused` after the verb until `Running` is written
— correct composition, and visible in the condition.

**The operator pauses and the auto-resume deadline expires.** This is a rollout/maintenance crossover
that does not arise: rollouts have no auto-resume (ADR-0041 rejected it — *"something that undoes a
safety halt without a human is not a pause"*). For `ZoneMaintenance` the analogous case is an operator
close racing the deadline: both take the same branch, whichever fires first, and the branch is
idempotent — the terminal guard at the top of `Reconcile` returns before it once `Completed` is
written, so `completedAt` is still stamped exactly once.

**The operator closes a maintenance window while robots are still draining.** A draining robot is
`InProgress` and was never paused — it is in `status.windingDownRobots`, not
`status.pausedRobots` — so the close does not strand it: it finishes its action under no maintenance
hold, which is what "the window is over" means. The robots that *were* paused are resumed through the
gate, and a robot whose estop is not Clear **holds the close**: the window stays `Active`, reports
`ResumeBlockedByEstop`, and completes once the estop clears. An early close therefore cannot strand a
robot that the deadline would not also have stranded, because it is the same code.

**A window the operator closes before it ever activated** is not activated in order to be closed.
Activating a window to close it in the same reconcile would pause robots that are immediately resumed
— in `Immediate` mode requeuing a live action — in service of a window the operator has called off.

## Consequences

- Two writers converge on one phase, and the object says which. `phase: Paused` plus
  `OperatorHold: True` is "waiting for you"; `OperatorHold: False` is the error halt.
- `desiredState` is additive and defaulted, so a stored object acquires `Running`/`Active` and behaves
  exactly as before. No migration.
- **A `ZoneMaintenance` still cannot reach `Cancelled`.** That value is declared, read by the terminal
  guard, and written by nothing — the same defect as this ADR fixes, in the same file. It is
  deliberately out of scope: `Cancelled` needs a meaning ("closed without resuming"? "called off"?)
  that this change does not have to settle in order to make `Completed` reachable, and inventing one
  in passing is how a second idiom appears. It is reported, with a number to be filed.
- `swarmctl` gains no command here. Writing a spec field needs no verb — `kubectl patch` and `kubectl
  edit` already work, and gating it would require a webhook the operator's existing `update` RBAC on
  the kind already implies. The `rollout-resume` verb stays verb-gated because it is destructive of
  intent (it abandons robots), which a hold is not.

## Alternatives considered

**An imperative verb — a subresource, or `swarmctl` writing an annotation.** This is the shape
`rollout-resume`, `policy-reset` and `estop-clear` use, so it is not foreign. Rejected as the default:
those three are all *destructive or safety-gated* actions where an RBAC verb distinct from `update` is
the point. A hold is neither — it is ordinary desired state, it must survive a disconnect and
re-converge, and an edge-triggered annotation is exactly the "lost command" ADR-0047 exists to rule
out. Where an intake genuinely needs its own authorization, the verb remains the right instrument, and
this ADR does not disturb the three that use it.

**`suspend: bool`, matching `CronJob`.** Rejected on two counts: a `bool` cannot grow a third state
(a rollout may later want `Aborting`), and the project has a gate test forbidding a defaulted-true
`bool` with `omitempty` because such a field cannot be set to `false` (ADR-0052). An enum is the
shape that survives the next value.

**Reuse `RolloutPhasePaused` as an operator-writable status field.** Rejected outright: status is
controller-authored (`docs/api-principles.md`), and an operator writing status is the inversion this
whole family of defects is made of.
