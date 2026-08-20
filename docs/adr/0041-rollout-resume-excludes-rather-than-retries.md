# ADR-0041: A rollout resume excludes the robots that failed rather than retrying them

- **Status:** accepted
- **Date:** 2026-08-19 (proposed) · decided 2026-08-20
- **Deciders:** Al — resume EXCLUDES the robots that failed; a fresh rollout is the retry path.
  Retry-once-then-exclude and auto-resume-after-timeout both ruled out: they weaken a halt whose
  whole purpose is to make an operator look before the fleet moves again
- **Related:** RFC-0001 §9.1.8 / §9.1.10 (FirmwareRollout, ModelRollout), §9.5.3 (RBAC custom verbs), §9.5.4 and §9.6.5.1 (required audit events), ADR-0021 (`policy-reset`: the operator-override precedent this mirrors), §6.7 (Auto model rollback, whose `rolledBackRobots` carries the same exclude-not-retry semantics)

## Context

`strategy.rollingUpdate.pauseOnError` defaults to **true**, for both rollout kinds. A single
robot failing its install therefore halts the rollout by default: no further robot enters the
batch while the failure stands.

Until this change there was no way out of that state.

1. **No resume existed.** `RollingUpdateStrategy.PauseOnError`'s own doc comment said *"The
   operator must resume with `swarmctl resume rollout`"*. That command had never existed, and
   RFC-0001 described no resume mechanism anywhere.
2. **No delete either.** The rollout delete webhook admits only terminal records
   (`Succeeded`/`Failed`), with `failurePolicy=fail`. `Paused` is refused.
3. **So the two guards closed on each other.** A paused rollout could be neither advanced nor
   removed. For a `ModelRollout` this was worse than an untidy record: robots in the batch have
   their models marked `Updating`, which suspends every capability those models grant, and the
   rollout's own status is what un-suspends them. A paused rollout held real fleet capability
   hostage.

The only exit was to repair every failed robot out of band until the failure set emptied,
because `paused` is derived per reconcile from `len(failed)` rather than latched.

So a resume path is not a feature request; it is the missing half of a guard that was already
shipping and already on by default. The open question is what "resume" should *mean*.

## Decision

**Resume excludes the robots that failed from further attempts by that rollout. It does not
retry them.** A fresh rollout is the retry path.

Concretely:

- Operator intake is the `swarmada.io/rollout-resume` annotation, written by
  `swarmctl rollout resume` after a `SelfSubjectAccessReview` on the **`rollout-resume`** custom
  verb. The CLI is the enforcement point and never edits `status` — the same split `estop`,
  `admit`/`reject` and `policy-reset` use.
- The controller moves the robots in `status.failedRobots` into a new
  **`status.excludedRobots`**, clears `status.failedRobots` and `status.pausedAt`, and seals a
  `ROLLOUT_RESUMED` audit entry naming the excluded set and the operator's reason.
- Excluded robots are removed from every progress bucket: they are not in `failed` (so the
  rollout cannot re-pause on them), not in `eligible` (so they are never dispatched to again),
  and they **count as settled** toward the terminal-phase test.

## Why exclude rather than retry

**Retrying re-dispatches the artifact that already failed.** The robot did not fail for a reason the
rollout can influence; it failed installing this specific image or model. Resuming into a retry
re-pushes the same bytes, the robot fails the same way, `paused` re-derives to true on the next
reconcile, and the operator is back where they started — having consumed a fleet-wide dispatch
slot to get there. That is a loop, not a recovery. It would also make the resume look like it did
nothing, which is precisely the failure mode `policy-reset` avoids by zeroing
`ConsecutiveRejections` rather than only clearing the condition.

**The platform already has this semantics, and calls it the same thing.** Auto model rollback
records `status.rolledBackRobots`, documented as robots *"excluded from further update attempts
by this rollout (a fixed model needs a fresh rollout), so a reverted robot is never pushed back
into an update loop."* Resume is the manual counterpart of the same decision, and modelling it
differently would leave two ways for a robot to leave a rollout with two different meanings.

**Exclusion is what lets the rollout finish at all.** This is the part that closes the deadlock,
not only the pause. If excluded robots stayed unaccounted for, the terminal test
(`done + rolledBack + newer + excluded == total`) could never be satisfied, the rollout would
never reach `Succeeded`, and the delete webhook would still refuse the record. Resume without
exclusion would move the wedge, not remove it.

**A fresh rollout is a better retry than an in-place one, and is already the supported shape.**
Retrying means a corrected artifact — a new version, a new URI, a new checksum — which is a new
`FirmwareRollout`/`ModelRollout` object. Re-using the paused one would mean mutating a rollout's
target mid-flight, which nothing else in the API permits and which would make the rollout's own
audit trail ambiguous about which artifact reached which robot.

## Consequences

- **A resumed rollout can report `Succeeded` while robots are not on the new version.** This is
  deliberate and visible: `status.excludedRobots` is non-empty and surfaces the fragmentation,
  exactly as a non-zero `robotsRolledBack` does. An operator or dashboard treating `Succeeded`
  as "the whole fleet converged" must read both lists. This is stated in the field's schema doc.
- **Resume is destructive of intent and is audited as such.** It abandons work rather than
  repeating it, so it is gated on its own verb rather than folded into `update`, and
  `ROLLOUT_RESUMED` records who did it, why, and which robots were dropped.
- **A stale annotation cannot resume a future pause.** The request is marked processed even when
  the rollout is not paused, so an annotation left on a healthy rollout is inert rather than
  armed.
- **Re-resuming works.** A *new* annotation value fires again, so a rollout that pauses a second
  time on different robots can be resumed a second time.
- **Excluded is not the same as failed.** `status.failedRobots` is cleared on resume, because it
  is the input `paused` is derived from. The record of what went wrong survives in the audit log
  (`FIRMWARE_ROLLOUT_PAUSED` / `MODEL_ROLLOUT_PAUSED` name the failed set) rather than on the
  live object.

## Alternatives considered

**Retry the failed robots once, then exclude on a second failure.** Rejected: it doubles the
state machine (a per-robot attempt counter that must survive reconciles) to buy one automatic
re-push of an artifact an operator has already been told is failing. If the artifact was the
problem, the retry is wasted; if it was not, the operator can create a fresh rollout targeting
that robot.

**Latch `paused` and clear the latch on resume, leaving `failedRobots` intact.** Rejected: it
makes `paused` a second source of truth about the same fact and leaves the terminal test
unsatisfiable, so the record still could not be deleted. It fixes the visible symptom and leaves
the deadlock.

**Make `Paused` a deletable phase in the webhook.** Rejected as the *primary* fix: it lets an
operator escape by deleting, but a deleted `ModelRollout` strands exactly the suspended
capabilities the guard exists to protect — which is why the delete guard refuses non-terminal
records in the first place. Resume settles the rollout properly; deleting a paused one would
discard the state that restores the fleet.

**Auto-resume after a timeout.** Rejected: `pauseOnError` is a safety halt on a fleet-wide
firmware or model dispatch. Something that undoes a safety halt without a human is not a pause.
