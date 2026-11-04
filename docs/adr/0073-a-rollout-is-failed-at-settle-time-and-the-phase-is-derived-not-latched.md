# ADR-0073: A rollout reaches terminal `Failed` when every targeted robot has settled and at least one settled as a failure, and the phase is derived per reconcile rather than latched

- **Status:** accepted
- **Date:** 2026-09-21
- **Deciders:** Maintainers, Controller Owner, API Designer
- **Related:** ADR-0041 (a rollout resume excludes the robots that failed rather than retrying
  them), ADR-0045 (a `desiredState` hold is entered declaratively and lifted only by an operator);
  RFC-0001 `control-plane.md` (`#control-plane-ota-model-update-manager`),
  `crds/firmwarerollout.md`, `crds/modelrollout.md`;
  `internal/controller/firmwarerollout_controller.go`,
  `internal/controller/modelrollout_controller.go`, `internal/webhook/rollout_webhook.go`

## Context

`RolloutPhase` declares `Failed`, and until this decision neither rollout kind could reach it from
an install failure. The firmware controller wrote `Failed` in exactly one place — the pre-dispatch
signature-verification refusal (`internal/controller/firmwarerollout_controller.go:833`) — and the
model controller wrote it nowhere. Both phase computations ended at `InProgress`.

`pauseOnError: true` hides the gap, because a failure pauses the rollout and an operator resumes
past it (ADR-0041). With **`pauseOnError: false`** a rollout that accumulated install failures had
no terminal phase to reach. It sat `InProgress` for ever over robots that would never update.

The record could not be removed either. The delete webhook permits a rollout to be deleted only
from a terminal phase (`internal/webhook/rollout_webhook.go:48-51`, a positive permit-set so a
phase added later is refused by default). A rollout stranded `InProgress` therefore failed that
guard permanently. On `ModelRollout` the cost is not only tidiness: a rollout marks models
`Updating`, which suspends the capabilities those models grant, and the rollout's own status is
what restores them. A record that can neither complete nor be deleted leaves a robot with
suspended capabilities and no owner to restore them.

**A prerequisite defect had to be fixed first, and it is the reason the terminal phase was not
only missing but unreachable.** `paused` is derived from the failure list, and the failure list
evaporated. A robot was classified failed by `installFailedForRollout`, which keys on the robot's
entry in `status.currentBatch` to establish that the failure belongs to *this* rollout's dispatch —
but `buildRolloutBatch` rebuilds that batch from the *updating* robots, so the robot left the batch
on the very pass it was recorded failed. One reconcile later the failure was no longer recognised,
the robot was re-classified as an updater on its own leftover `pending-firmware-version`
annotation, and the pause released itself. Nothing could reach a terminal `Failed` while the
failure it would rest on disappeared between reconciles. The fix is to read
`status.failedRobots[]` back as the rollout's own record of an already-attributed failure
(`:186-196`), which re-establishes the fact without re-deciding it.

## Decision

**A rollout's phase is `Failed` when every targeted robot has settled — updated, failed, or
excluded by an operator's resume — and at least one settled as a failure that was neither repaired
nor excluded. The phase is recomputed from observed state on every reconcile and is not latched.**

### 1 · The branch and its order

`firmwarerollout_controller.go:959-981` and `modelrollout_controller.go:1034-1056` order the phase
switch the same way:

1. `Succeeded` — every targeted robot is done or excluded.
2. `Paused` — checked **before** `Failed`, and deliberately not terminal.
3. `Failed` — `total > 0 && len(failed) > 0 && done + len(failed) + len(excluded) == total`.
4. `InProgress`, then `Pending`.

Because `Paused` claims every failure under `pauseOnError: true`, the `Failed` branch is reachable
only with `pauseOnError: false`. That is the configuration that had no terminal phase.

### 2 · `Paused` stays non-terminal and keeps its precedence

`pauseOnError` halted the rollout so an operator could decide, and `swarmctl rollout resume` is the
exit (ADR-0041). Calling a paused rollout `Failed` would take that decision away and, since
`Failed` is deletable, would expose the state the operator needs to make it.

### 3 · Excluded robots count as settled

A resume moves the failed robots into `status.excludedRobots` and clears the failure list
(`firmwarerollout_controller.go:338-347`), so a resumed rollout converges and reaches `Succeeded`.
Under the `Failed` branch, exclusion likewise counts toward settlement, which is what lets a
partially-resumed rollout still terminate.

### 4 · Derived, not latched

`Failed` is recomputed each reconcile from the same inputs as every other phase, exactly as
`Paused` is. A robot repaired out of band classifies as done again, and the rollout resumes
converging rather than staying dead because it was once dead.

### 5 · A failed result keeps the previous version

`firmwareFailedResults` (`:1015-1030`) carries the robot's prior version forward into the failure
record, so a `Failed` rollout still reports what each robot is actually running.

### 6 · The enum keeps `Failed`

`Failed` is not removed from `RolloutPhase`. The firmware path already writes it from the
signature-verification refusal (`:833`), and the delete webhook's permit-set names it
(`internal/webhook/rollout_webhook.go:48-51`).

## Alternatives considered

- **Auto-exclude failed robots, so the rollout converges to `Succeeded`.** Rejected.
  `excludedRobots` means "an operator resumed past this" (ADR-0041). Overloading it with
  "the controller gave up on this" would report `Succeeded` over robots that failed, which is the
  one answer a rollout must never give. It would also make the operator decision that ADR-0041
  exists to preserve invisible.
- **Drop `Failed` from the `RolloutPhase` enum and specify the non-termination.** Rejected. The
  firmware path already writes `Failed` from verification refusal, so the value has a live writer
  and removing it is a breaking API change for no gain. Specifying that a rollout never terminates
  would also leave the delete guard permanently unsatisfiable, which is the harm.
- **An explicit `failureThreshold` field on the spec.** Not taken here. It is the more expressive
  answer — an operator could say how many failures end the rollout — but it is an `api/v1` change
  with regenerated manifests and a defaulting decision of its own. Settle-time evaluation needs no
  new field and answers the stranding, so the field is the seam and not the fix. Recorded below.
- **Latch `Failed` once reached.** Rejected. It would contradict the way every other phase on these
  kinds is computed, and it would leave a rollout terminal after the condition that made it
  terminal has gone away. A robot repaired out of band should let the rollout converge.
- **Put `Failed` ahead of `Paused`.** Rejected; see decision 2. It would make `pauseOnError: true`
  terminate on the first failure, which is the deadlock ADR-0041 removed.
- **Fail on the first failed robot rather than at settle time.** Rejected. Robots still updating
  may yet succeed, and a rollout declared terminal while work is outstanding reports a convergence
  that has not been attempted. "Every robot has settled" is the point at which no further work is
  possible.
- **Let the delete webhook permit `InProgress` so the record can at least be removed.** Rejected.
  It treats the symptom and creates a worse one: deleting a live `ModelRollout` strands the
  suspended capabilities it owns, which is precisely what the guard exists to prevent.

## Consequences

**What a reader gains.** A rollout with `pauseOnError: false` over robots that will not update now
reaches a terminal phase that says so, and the record becomes deletable through the existing
guard. `status.failedRobots[]` names which robots failed and what version each is running.

**Terminal is not permanent.** Because the phase is derived, a `Failed` rollout whose robots are
repaired out of band returns to `InProgress` and can reach `Succeeded`. A reader who takes `Failed`
as an immutable end state will be surprised. That is the deliberate trade for not latching, and
`Succeeded` behaves the same way.

**Open: the already-stuck rollout is answered only by implication.** A rollout stranded
`InProgress` before this change carries no marker of having been stranded. Nothing migrates it and
nothing announces it. It is recovered purely as a side effect of decision 4: the next reconcile
recomputes its phase from current state, and if its robots have settled with at least one failure
it becomes `Failed` and deletable. That is the behaviour, and it is sound — but it is nowhere
stated, in this repository or in the specification, as an answer to the question "what happens to
the rollouts that are already wedged?". A reader looking for that answer has to derive it from the
derived-not-latched property. The honest statement is that the case is handled and not documented;
documenting it belongs with the CRD chapters for both kinds.

The implication also has a boundary worth naming: recovery requires the failure to be
re-established on the recomputing pass. For a firmware rollout whose `status.failedRobots[]` was
already lost to the batch-rebuild defect described above, the robots re-classify as updaters on
their leftover annotation rather than as failures, so such a rollout converges or stays
`InProgress` rather than reaching `Failed`. Nothing in the tree distinguishes the two outcomes for
an operator inspecting an old record.

**New obligations.** The `Failed` branch and the `Paused` branch derive from the same inputs and
must keep their order; a future edit that moves `Failed` above `Paused` re-opens ADR-0041's
deadlock. `status.failedRobots[]` is load-bearing for both the pause and the terminal phase, so
anything that clears it clears both, as the resume path does deliberately.

**What is tested.** `internal/controller/rollout_terminal_failure_test.go`: an unpaused failure
reaches `Failed` on both kinds (`:53`, `:165`), a `Failed` record is deletable (`:71`), `Failed` is
reached only once nothing is outstanding (`:89`), `Paused` still beats `Failed` (`:112`, `:182`),
a failed result keeps the previous version (`:150`), and the prerequisite — a failure surviving
across reconciles (`:127`).
