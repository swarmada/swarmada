# ADR-0042: Measure estop fan-out end to end, not per robot

- **Status:** accepted
- **Date:** 2026-08-19 (proposed) · decided 2026-08-20
- **Deciders:** Al — do BOTH: plumb the `scope` label AND add `swarmada_estop_fanout_duration_seconds`,
  because a per-robot latency histogram structurally cannot see the interval that matters. The
  scope-label-alone option was ruled out as precisely the trap this ADR documents
- **Related:** RFC-0001 §9.3.8 (metrics contract), §9.6.2.1 (estop scopes — the parallel-dispatch requirement, sequential when this ADR was written), §9.6.2.2 (timing requirements), §9.6.5.1 (required events), ADR-0016 (per-adapter estop delivery timeout), `drawbacks.md` item 15

## Context

`swarmada_estop_command_latency_seconds` is the SLA instrument for emergency stop. It is a
histogram of the round trip from an `estop` Command send on `SafetyStream` to the `EstopAck`,
with a 500ms SLA boundary and a companion counter, `swarmada_estop_latency_violations_total`,
whose non-zero rate is documented as an SLO breach that MUST alert.

For a **robot-scoped** estop that is the right measurement. For a **zone- or namespace-scoped**
one it measures the wrong interval, and does so in a way that is structurally undetectable from
the metric itself.

The dispatcher stamps `sentAt` per robot immediately before that robot's send and computes
`latency := clock() - sentAt` after its ack. Zone and namespace estops fan out **sequentially**
through that same primitive — one robot at a time, each robot's outcome awaited before the next
is sent (a disclosed gap, §9.6.2.1 and `drawbacks.md` item 15). Sequential dispatch therefore
delays the **send**, not the round trip.

The consequence is that the instrument reports health precisely when the fleet is least safe:

- Every robot in a 50-robot zone estop observes a healthy sub-500ms round trip.
- `swarmada_estop_latency_violations_total` stays at zero.
- The last robot is commanded tens of seconds after the operator hit the trigger.
- Nothing in the metrics surface says so.

A second, smaller defect compounded it. All five emit sites hard-coded `scope="robot"`, so the
`scope` label documented in §9.3.8 as `robot | zone | namespace` had exactly one value in
practice — a zone estop was indistinguishable from a single-robot one even in principle.

Fixing the label alone does not fix the measurement. It relabels healthy observations: the
p99 of a correctly-labelled `scope="zone"` series is still the p99 of *per-robot round trips*,
which stays green under sequential dispatch. The gap needs a different number, not a better
breakdown of the existing one.

## Decision

Add **`swarmada_estop_fanout_duration_seconds{namespace, scope}`**, a histogram observed **once
per zone- or namespace-scoped estop episode**, timing the operator's trigger to the last robot
in scope resolving.

- The clock starts in the fanning-out reconciler before the first send and stops after the loop,
  so it spans the sequential waits the per-robot metric cannot see.
- **Once per episode, not once per robot.** Per-robot observation would make one wide slow
  fan-out look like many fast episodes — the same averaging-away that hid the gap.
- **Robot scope is not observed.** A robot-scoped estop has no fan-out; admitting it would fill
  the histogram with near-zero episodes and drag every quantile down, so the metric would
  under-report exactly what it exists to expose. `scope` therefore takes only `zone` or
  `namespace` on this series.
- **An episode that confirms nothing is still observed.** A zone whose robots all fail to stop is
  the episode an operator most needs timed.
- Buckets run to 300s, far past the per-send 500ms SLA, because that is the range a sequential
  fan-out over a real fleet occupies. A histogram bounded at the SLA would saturate its top
  bucket and lose the shape of the very problem it measures.

`scope` is also plumbed properly: `TriggerEstop` takes a typed `metrics.EstopScope` supplied by
each of the three reconcilers, so the per-robot series is correctly attributed too.

§9.6.2.2 gains a fleet-scope timing row naming this metric, and states explicitly that the
per-send row does not imply it.

## Consequences

- **The sequential-dispatch gap became a measured breach rather than a paragraph an operator has
  to read and mentally apply.** It converted a disclosure into a signal — and the fan-out was
  parallelised immediately afterwards, in the change that follows this one. That does not retire
  the metric: the blind spot documented above is a property of the per-robot *measurement*, not
  of that particular bug, so this histogram is what would catch a regression to serialised
  dispatch. It is now a guard rather than an indictment.
- **Two estop latency metrics now exist and mean different things.** The risk is an operator
  alerting on the wrong one. Mitigated in the metric help text, in §9.3.8's row, and by the
  §9.6.2.2 note stating that a green `swarmada_estop_command_latency_seconds` is not evidence
  that a zone or namespace estop met its bound.
- **`scope` is a signature change across an interface** (`TriggerEstop`) and its test doubles.
  Carrying it as a parameter rather than inferring it is deliberate: below the fan-out every
  estop looks identical, so the information genuinely only exists at the call site.
- **It measures the reconciler's fan-out, not the operator's wall clock.** The interval begins
  when the reconciler starts the episode, which excludes admission, watch latency and requeue
  delay before it. That is the half the control plane can act on; the excluded part would make
  the series depend on informer timing rather than on dispatch strategy.
- **Parallelising the fan-out moves this metric, it does not retire it.** It remains the
  regression guard that the fan-out stayed parallel.

## Alternatives considered

**Add the `scope` label and alert on `scope="zone"` p99.** Rejected — this is the trap the
finding names. Those are still per-robot round trips; sequential dispatch leaves them green.

**Move the `sentAt` stamp to the start of the fan-out and keep one metric.** Rejected: it would
overload one series with two meanings, and every robot in an episode would then report roughly
the episode duration, destroying the per-robot signal that legitimately detects a slow adapter.
Two intervals are genuinely being measured; two metrics is the honest modelling.

**Derive the fan-out duration from the existing audit entries.** The zone controller already
seals `worstLatency` and `robots_in_scope`. Rejected: `worstLatency` is the worst *round trip*,
which has the identical blind spot, and an audit log is not an alerting surface. The number
belongs where SLOs are evaluated.

**A counter of episodes exceeding a fixed budget.** Rejected: it needs the budget baked in at
emit time, so it cannot be re-evaluated per fleet size. A histogram lets the bound live in the
alerting rule, where fleet size is known.
