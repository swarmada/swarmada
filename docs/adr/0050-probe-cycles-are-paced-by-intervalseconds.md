# ADR-0050: A probe cycle is issued once per `intervalSeconds`, so ADR-0012's debounce is also a duration

- **Status:** accepted
- **Date:** 2026-09-06
- **Deciders:** Controller Engineer
- **Related:** RFC-0001 [§9.1.7](../../rfcs/dist/RFC-0001-core-spec.md) (`RobotProbe`
  — `spec.intervalSeconds`, `failureThreshold`), ADR-0012 (probe status debounce — the
  decision this one paces and does not change), ADR-0024 (probe kill-switch), ADR-0026
  (robot liveness projection — the other clock this one runs alongside), the RA-1
  status-write discipline.

## Context

A verification run on 2026-09-06 measured what a probe cycle actually costs. A 22-second
outage of one adapter — expected to be about two cycles at `intervalSeconds: 10`, and so to
be swallowed by a `failureThreshold` of 3 — produced `lastProbeResult: Failed` and
`consecutiveFailures: 3`. The threshold had been crossed in under five seconds.

Two source facts produce that, and neither is in ADR-0012:

1. **The unreachable path returns immediately.** `Dispatcher.roundTrip` does not wait for
   a timeout when no ControlStream is registered for the robot's adapter
   (`internal/command/dispatcher.go:178`), and `probeRobot` binds any RPC error to
   `probe.StatusFailed` (`internal/controller/robotprobe_controller.go`). A failing cycle
   therefore costs microseconds, not `timeoutSeconds`.
2. **Each failing cycle re-triggered the next one.** The reconciler patches its own status
   whenever the computed status differs from the stored one, and `SetupWithManager` watches
   `RobotProbe` with no predicate — so a status write re-enqueues the object at once.
   During a failure ramp `consecutiveFailures` changes on every cycle (1, then 2, then 3),
   so every cycle wrote status and every write scheduled the next cycle immediately. The
   `RequeueAfter: intervalSeconds` at the end of `Reconcile` governed only cycles that
   changed nothing — which is to say, cycles after the streak had already clamped.

**Which of ADR-0012 and the implementation was wrong.** ADR-0012 is a **count**, and says
so in every quantity it states: *"it flips Healthy→Degraded/Failed only after
`failureThreshold` consecutive failing cycles"*; *"`lastProbeResult` now lags raw outcomes
by up to `failureThreshold`/`recoveryThreshold` cycles"*; *"a robot that is genuinely
failing is reported Healthy for up to `failureThreshold-1` extra cycles"*. The word
"seconds" does not appear in its Decision. So ADR-0012 is not a time-based debounce and
did not need changing.

But ADR-0012's count is a count of **cycles**, and what a cycle costs in time is not
ADR-0012's to define — it is RFC-0001 §9.1.7's, which defines `spec.intervalSeconds` as
*"how often the control plane issues the probe RPC to each targeted robot, in seconds"*,
and constrains `timeoutSeconds` to be *"less than intervalSeconds"* on the same
assumption. RFC-0001 governs where the two differ (AGENTS.md). **The implementation was
the thing that was wrong**: it issued probe RPCs faster than `intervalSeconds`, so a
count that RFC-0001 makes worth `failureThreshold × intervalSeconds` was worth
milliseconds instead.

The natural reading of the two fields together (`failureThreshold: 3` ×
`intervalSeconds: 10` = 30 seconds of sustained failure before anything acts) was therefore
**correct**, and was falsified by a defect rather than by a misreading.
`config/samples/robotprobe_demo.yaml` had said the same thing since it was written.

This matters well beyond a demo. Crossing the threshold demotes the component, which
demotes every capability derived from it, which takes the robot out of dispatch for those
capabilities. A five-second adapter restart — a rescheduled adapter pod, a dropped
ControlStream — was enough to cost a robot its capabilities, which is precisely the
transient ADR-0012 exists to absorb.

## Decision

**A probe cycle is issued at most once per effective `intervalSeconds`, per RobotProbe,
whatever enqueued the reconcile.** `Reconcile` gates on that before it issues any Verify
RPC and requeues the remainder of the interval when the cycle is not yet due
(`cycleDue`). No reconcile blocks and nothing sleeps: this is a rate limit on the probe,
not a delay.

Consequences of stating it that way:

- `failureThreshold` and `recoveryThreshold` are counts **and** durations. A failure must
  persist between `(failureThreshold-1) × intervalSeconds` and
  `failureThreshold × intervalSeconds` before the component is demoted. Both bounds are
  documented on the fields in `api/v1/robotprobe_types.go`, where an operator reads them.
- The gate sits **after** the ADR-0024 kill-switch check, so a `disableAllProbes` flip
  still takes effect on the next reconcile. `reconcilePaused` issues no RPC and is never
  rate-limited.
- The pacing anchor is **in memory**, keyed by namespaced name. It is scratch state for
  one decision; putting it in `status` would cost a write per cycle and violate RA-1, which
  is the discipline this ADR is protecting. Losing it to a restart costs at most one early
  cycle per probe — the same trade `RobotReconciler.beats` makes for the confirming
  heartbeat exchange, and for the same reason. Leader election means one reconciler owns
  it.
- Nothing about ADR-0012 changes. `lastProbeResult` is still the debounced status, the raw
  per-robot outcome is still undebounced in `status.robotResults[]`, the streaks are still
  clamped at their thresholds, and the first observation is still adopted immediately.

## Alternatives considered

- **A `GenerationChangedPredicate` on the `RobotProbe` watch,** so a status-only write does
  not re-enqueue. Rejected as insufficient rather than wrong: it removes the one enqueue
  known source and leaves the invariant unstated, so any other early wake — the
  `SwarmadaConfig` watch, an informer resync, a rescheduled manager — reopens the defect.
  The rate limit holds regardless of what wakes the reconciler, and the wasted wake it absorbs is one
  cheap no-op reconcile.
- **A new `minFailureDurationSeconds` field beside the count.** The right shape *if* the
  count and the clock genuinely need tuning apart, and the wrong shape here: it adds
  published API surface to restore a guarantee
  `intervalSeconds` already makes and the implementation had broken. Adding a
  second knob would also leave `failureThreshold × intervalSeconds` still meaning
  something other than what it reads as.
- **Gate only the failure path on `intervalSeconds`.** Asymmetric for no reason: recovery
  is a count of cycles on the same clock, and a recovery ramp that can run at reconcile rate
  has the mirror-image defect — one good answer from a subsystem
  that has been failing for two minutes would restore it early.
- **Treat unreachability as its own class and let ADR-0026 liveness own it** — arguably the
  most correct framing. Not taken *now*, and not rejected: it needs ADR-0012 and ADR-0026
  reconciled rather than each extended, and it would leave the
  pacing defect in place for every other failure mode meanwhile — a probe that answers
  `Failed` in 20ms ramps as fast as an unreachable one. Recorded as a live option for
  a later ADR; this decision is a precondition for it, not a substitute.
- **Do nothing and document fail-fast as intended.** Rejected against ADR-0012's own
  Consequences, which put fast-fail out of scope for active probing: *"Safety-critical fast-fail is out of scope for active probing and remains the
  estop/lease path."* A debounce whose stated purpose is that a transient changes nothing
  cannot be documented as fail-fast on the most common transient there is.

## Consequences

- **Good.** A brief adapter outage no longer costs a robot its capabilities. An outage
  shorter than `(failureThreshold-1) × intervalSeconds` cannot cross the threshold at all,
  and the operator-facing thresholds mean what they read as. A demo of the debounce
  suppressing a flap becomes stageable, which it was not.
- **Cost — demotion latency.** A genuinely broken robot is demoted at
  `failureThreshold × intervalSeconds` rather than at reconcile rate. On
  `config/samples/robotprobe_demo.yaml`'s settings that is 30 seconds instead of about
  five. That is the trade ADR-0012 already declared ("a robot that is genuinely failing is
  reported Healthy for up to `failureThreshold-1` extra cycles"); what changes is that the
  extra cycles now cost what the ADR assumed they cost. Operators who want faster demotion
  lower `intervalSeconds` or `failureThreshold`, which is what those fields are for.
- **Cost — one no-op reconcile per status write.** The self-triggered wake still runs; it
  now finds the cycle not yet due and returns. No RPC, no API write.
- **Obligation — tests.** `internal/controller/robotprobe_pacing_test.go` covers both
  halves: the rate (two reconciles inside one interval issue one Verify RPC) and the
  behaviour (an unreachable adapter held 29 seconds sits at `consecutiveFailures: 2`, and
  crosses at 30). The existing debounce table tests drop the pacing anchor between cycles
  via `reconcileProbe`, so they keep testing the arithmetic rather than the clock.
- **Not addressed here.** The ADR-0026 liveness clock that runs alongside this one is *not*
  made deterministic by this decision, and the two are not ordered by construction: nothing
  refreshes `Robot.status.connectivity.lastSeenAt` on a cadence, so its latency to `Offline`
  spans a range that overlaps the probe's. This ADR paces the probe and says nothing about
  that; it is tracked separately.
