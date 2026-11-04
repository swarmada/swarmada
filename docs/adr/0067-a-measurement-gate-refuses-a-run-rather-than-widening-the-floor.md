# ADR-0067: A measurement gate refuses a run rather than widening the floor, and the kinematic constants belong to the plant under test

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, Test Engineer
- **Related:** ADR-0048 (a verdict grades obligations, not observations), ADR-0068 (a capability-loss reroute is a confirmed-stop requeue); the R1/R2 validation runbook and its recorded results

## Context

R1 (confirmed-stop deceleration interval) and R2 (cancel → confirmed-rest latency) are
the two numbers the project quotes about stopping. Both are produced by a harness that
must decide, per run, whether a sample **counts**. That decision rests on two
constants: a **cruise floor** (the robot must have reached a known speed, or the stop
interval is not comparable) and a **deceleration rate** (which sets the kinematic floor
a sample is measured against).

Two WS-B measurements, taken four days apart on different plants, bear on the same
decision.

**Simulated robot, measured 2026-09-15.** Three R2 sessions
were run; the middle one was **refused by the harness**. Run 3 returned **14.453 s —
39.3× its kinematic floor** — and runs 4 and 5 recorded peak **0.000 m/s**.
`run_verdict.py`'s 4× gate printed `THIS RUN MUST NOT BE QUOTED` and exited 1.
`/tf` was healthy at both ends (49.5 → 58.3 Hz), so this was not the known
transform-starvation signature. The refusal was **reported, not hidden**, and the
published figure rests on 12 samples across the two surviving
sessions rather than three.

**Physical robot, measured 2026-09-15.** No motion was commanded — the hardware
take was blocked by the room, then by localisation, then by
battery — but two properties of the *harness* were measured, and they do not depend on
the room:

```
speed_gate.py:53   CRUISE_FLOOR  = 0.35 m/s
$ ros2 param get /velocity_smoother max_velocity
  array('d', [0.35, 0.0, 1.0])          <- hard cap at EXACTLY the floor

run_verdict.py:75  DEFAULT_DECEL = 1.0 m/s^2
$ ros2 param get /velocity_smoother max_decel
  array('d', [-2.5, 0.0, -3.2])         <- the real robot decelerates 2.5x harder
```

Both constants are the **fake plant's**, and on this robot both are wrong **in opposite
directions**. The controller asks for 0.4 m/s and the smoother clips it to 0.35, so a
sample counts only when odometry noise reads at or above the cap — yielding nothing, or
a set biased toward favourable noise. And at `DEFAULT_DECEL` the kinematic floor is
understated **2.5×**, so every sample clears a bar set too low. The harness's own
comment names this: a mismatch *"makes the floor wrong in the permissive direction,
which is the silent one."*

The tempting response to a gate that refuses runs is to widen it until runs pass. The
harness pre-emptively forbids it (`speed_gate.py:50-52`): a floor *"NOT widened when a
run skips. A floor that is lowered until every run produces a sample returns a biased
set instead of an honest gap."*

**R1 and R2 differ here, and the contrast is the lesson.** `r1_smoke` records the speed
either way and **labels** a sub-cruise halt a lower bound rather than discarding it. R1's
design ports to slower hardware; R2's does not.

## Decision

**A measurement gate refuses a run; it is never widened to make a run pass.** A refused
run is reported as a refusal, with its numbers, and is excluded from the quoted figure.
A gap in the data is an honest result; a lowered bar is a biased dataset wearing the
same numbers.

**The kinematic constants belong to the plant under test, not to the harness.** A
cruise floor and a deceleration rate hardcoded from the simulated robot are wrong on any
other plant, and — as measured — wrong in both directions at once. They must be supplied
per plant (or read from the running stack), and a run whose declared constants do not
match the plant's is not quotable. This is recorded here rather than left in a source
comment, so that the prediction is on a record someone reads before the next run.

**Where a sample cannot meet the gate, prefer R1's shape: record the condition and label
the result a bound, rather than discarding it.** A labelled lower bound is usable
evidence and is honest about what it is. A discarded sample is invisible, and a series
of discarded samples looks like a clean run.

**Quote the distribution, not the median.** Two significant figures; the third is not
supported by this data.

## Alternatives considered

- **Lower `CRUISE_FLOOR` until the hardware produces samples.** The obvious fix, and it
  would have produced hardware numbers this week. Rejected: on this robot the cap *is*
  the floor, so the samples that cleared a lowered bar would be selected by odometry
  noise — a biased set that is indistinguishable, in the published number, from an
  honest one.
- **Keep `DEFAULT_DECEL = 1.0` and note the mismatch in the report.** Rejected. The
  error is in the permissive direction, so every sample passes and the report says the
  path is 2.5× better than a correct floor would show. A footnote does not undo a number
  that has already been quoted.
- **Widen the `run_verdict` 4× gate so the degraded session would have passed.**
  Rejected: that session recorded a 39.3× excursion and two runs at peak 0.000 m/s. A
  gate that admits those is measuring something other than the stopping path.
- **Average the refused session in with the others.** Rejected for the same reason, and
  it is worse than widening the gate because it hides the excursion inside a mean.
- **Adapt R2's goals to the 5.83 m corridor available on the hardware** rather than the
  6 m it asks for. Rejected as a silent change: shortening the travel changes whether
  the robot reaches cruise at all, which is the very gate in question. If it is done, it
  is a recorded decision, not a convenience applied at run time.
- **Publish the simulated numbers as the project's stopping figures.** Rejected —
  `validation/README.md` already governs this: *"R2 is a simulated robot. Nothing before
  a physical robot justifies"* moving an end-to-end maturity grade. WS-B produced no
  hardware measurement, so no grade moved.

## Consequences

- The published R2 figure rests on **two** sessions, not three, and says so: median
  0.62 s, 12 pooled samples, range 0.55–0.93 s, from a robot braking at 0.35–0.40 m/s
  under a 10 Hz controller — explicitly **not** an upper bound on the path, because no
  run reached the configured 0.5 m/s cruise.
- Agreement between the two surviving sessions is **not** treated as evidence that one
  would have sufficed. Their medians agree to 0.001 s while S1's own standard deviation
  (0.136 s) is the largest on record, driven by the largest single sample ever seen on
  this path. Two medians agreeing while one parent distribution is the widest yet
  observed is a coincidence of the median, not a tightening of the path.
- Hardware R1/R2 remain **unmeasured**, and this ADR is part of why: the honest route is
  slower than the expedient one. The blockers are recorded rather than worked around —
  the room, a seeded AMCL pose with zero covariance and a 20% scan-to-map match, and
  R2's 6 m requirement against a 5.83 m best corridor.
- The harness has no per-plant parameter surface, so a
  hardware run must state its constants and a reviewer must check them against the
  plant; that is a manual obligation and manual obligations decay.
- This ADR governs the **validation harness**, not the conformance suite. ADR-0048
  already draws the parallel line there: a verdict grades obligations, not observations,
  and a refusal is a result.
