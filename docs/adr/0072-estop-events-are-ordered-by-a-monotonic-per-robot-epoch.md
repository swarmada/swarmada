# ADR-0072: A trigger and a clear are ordered by a monotonic per-robot estop epoch, and only the release direction is gated

- **Status:** accepted
- **Date:** 2026-09-21
- **Deciders:** Maintainers, Safety Engineer, Spec Editor
- **Related:** ADR-0046 (attribution is stamped at admission and never blocks a stop), ADR-0043
  (estop fan-out is unbounded), ADR-0071 (a stop is confirmed by a channel that can see motion);
  RFC-0001 `safety.md` (`#safety-emergency-stop-protocol`, `#safety-estop-state-machine`),
  `control-plane.md` (`#control-plane-scheduler`); `internal/safety/dispatcher.go`,
  `internal/controller/adapter_readiness.go`

## Context

`Robot.status.estopState` is the control plane's record of whether a robot is stopped. It is
written by the estop dispatcher on two opposite occasions: a **trigger**, which moves the robot
into a stop (`Stopping`, `Stopped`, `Failed`), and a **clear**, which moves it out
(`Resuming`, then `Normal` once resumption is confirmed).

The write was a single unconditional status patch from a base read moments earlier. It carried no
lock, no fence and no ordering token. A trigger and a clear issued at about the same time for one
robot therefore resolved by **which patch reached the API server last**, not by which event was
issued last.

The consequence was larger than a lost record. `Normal` and `Resuming` are the two states a robot
must not be in while a hazard stands, and the scheduler reads them: candidate filter 10
(`internal/controller/adapter_readiness.go:170-176`) excludes exactly `Stopping`, `Stopped` and
`Failed`. A clear that happened to arrive last overwrote a live stop, the robot re-entered the
candidate set, and the next reconcile could dispatch work to a robot that had been stopped moments before.

Two constraints shape the answer.

**A stop must never be refused or delayed.** Estop deliberately carries no fence — no lease check,
no priority gate — precisely so that a stale token can never stand between an operator and a
stop (RFC-0001 `#safety-emergency-stop-protocol`). Any ordering mechanism that can make a stop
write fail, or that adds a retry loop to the stop path, is worse than the defect it fixes.

**"Later" has to mean the operator action.** A trigger and a clear each pass through an object
lookup and, for a trigger, a `SafetyStream` send and a wait for acknowledgement before any status
write happens. Ordering the two by anything measured at write time orders them by API and network
latency, which is not the question being asked.

## Decision

**Every estop event takes a monotonic per-robot epoch at the moment it is issued. A stop is
written unconditionally. A release is written only when no trigger has been issued for that robot
since the releasing event was issued.**

### 1 · The epoch, and when it is taken

`estopOrder` (`internal/safety/dispatcher.go:94-109`) holds two counters per robot: `issued`, the
last epoch handed out, and `lastStop`, the epoch of the most recent trigger. `issueEstopEpoch`
(`:578-586`) allocates the next epoch under the dispatcher's mutex and, for a trigger, advances
`lastStop` in the same critical section.

Both callers take the epoch **before any read or send that could block**:
`TriggerEstop` takes it as its first statement, ahead of the robot lookup
(`:208-213`), and `ClearEstop` does the same (`:369-373`). Taking an epoch cannot fail and cannot
block, so it is never in a stop's way.

### 2 · The gate is one-sided

`setEstopState` (`:627-648`) treats the two directions differently, and the asymmetry is the
decision:

- A **stop** is written unconditionally. No epoch is consulted, so no stale token can refuse or
  delay it.
- A **release** is written only when `supersededByLaterStop` (`:599-603`) reports no trigger
  issued after the releasing event — that is, `lastStop <= epoch`. A superseded clear is logged and
  dropped, and the stop stands.

### 3 · Both release states are gated, not only `Normal`

`isEstopRelease` (`:593-596`) returns true for `Resuming` **and** `Normal`. `Resuming` is the
first half of the release path (`Stopped → Resuming → Normal`), and letting it through ungated
would overwrite a confirmed `Stopped` — which RFC-0001 `#safety-estop-state-machine` forbids
outright: a confirmed stop is never discarded. It would also lift the robot out of filter 10,
which does not name `Resuming`. A `Resuming` robot is withheld from dispatch by a separate check
(`internal/controller/adapter_readiness.go:198-204`), and that check exists because `Resuming` is
not an active estop for the other purposes `robotUnderEstop` serves.

### 4 · Check and patch are serialised per robot

`estopOrder.write` (`:105-110`) is a per-robot mutex held across the ordering check and the status
patch (`:628-630`), so a trigger cannot register in the window between a clear's check and its
write. It covers the status write only — never the `SafetyStream` send — so it cannot add latency
to a stop.

### 5 · A clear reports the state that stands

`ClearEstop` returns the resulting state rather than asserting success
(`:363-368`, `:385-387`). When the clear is superseded, the state last read is returned and the
caller learns the stop still stands. A `nil` error does not mean the robot is clear, and `Resuming`
does not mean the robot is ready for work.

### 6 · The epoch is total only because there is one writer

`Robot.status.estopState` has a single production writer: the estop dispatcher. One instance is
constructed at `cmd/manager/main.go:501` and shared by the zone-estop reconciler (`:500`), the
robot-estop reconciler (`:517`), the zone-maintenance path (`:531`) and the control stream
(`:631`), all on the elected leader. A second writer would make the per-robot sequence partial and
this decision would no longer hold.

## Alternatives considered

- **An optimistic lock with retry on `resourceVersion`.** Rejected on both counts. It does not add
  event ordering: a clear that loses the compare-and-swap re-reads the object and re-applies its
  intent, so it still overrides a live estop — the race becomes slower, not correct. And a
  conflict can make a **stop** write fail, which requires a retry loop in the safety-critical
  path. A stop must never be refused or delayed by a concurrency artifact.
- **A fence on both directions, as the assignment path uses.** Rejected. A symmetric fence is
  exactly what `#safety-emergency-stop-protocol` excludes from estop: a stale token would be able
  to refuse a stop. The one-sidedness is not an omission, it is the property.
- **Gate `Normal` only and let `Resuming` through.** Rejected; see decision 3. `Resuming` is a
  release, it can overwrite a confirmed `Stopped`, and filter 10 does not exclude it.
- **Hold the epoch in `Robot.status`.** Rejected for this change. A status field is an `api/v1`
  change with regenerated manifests, and the ordering property is available without one. The
  durability this would buy is real and is recorded below as an open consequence, not as a denial
  that it matters.
- **Take the epoch after the object lookup, where the write path begins.** Rejected. That orders
  two operator actions by the latency of two API round trips. The question a reader asks is which
  action happened later, so the token is taken when the action is issued.
- **Order by a timestamp rather than a counter.** Rejected. Two events issued in the same
  millisecond do not order, and a wall-clock comparison invites the clock question on a path where
  it does not need to be asked. A counter under the same mutex is total by construction.
- **Let the clear keep reporting `Normal` unconditionally.** Rejected. Under the gate the clear can
  legitimately fail to release, so a return value that always claims success would misreport the
  state the caller is about to act on.

## Consequences

**What a reader gains.** A trigger and a clear racing on one robot resolve by the order the two
events were issued. A clear that happens to arrive last cannot return a stopped robot to the
scheduler's candidate set. RFC-0001 #safety-estop-state-machine states the rule, the one-sidedness and the
single-writer property that makes the epoch total.

**A stop that confirms late still wins, and that is deliberate.** A trigger issued before a clear,
whose `STOPPED` confirmation lands after the clear has been applied, re-asserts the stop: the
trigger advanced `lastStop` when it was issued, and its own state write is never gated. An operator
can therefore see a robot move to `Resuming` and back into a stop. That follows from "a stop is
never refused" and is the safe side of the error.

**The ordering record is process-local, and the specification now says so.** `estopOrder` lives in
the dispatcher's map, not in the API objects. A control-plane leader change loses it, and both
counters restart at zero on the new leader. The release path is safe at the boundary itself: a
robot found `Resuming` with no ordering record is adopted by `rearmResumeAfterFailover`
(`:472-484`), which re-establishes the resume baseline and never releases the stop, and the stop
direction was never gated. What is **not** established is ordering **across** the change: a clear
issued before the leader change and a trigger issued after it carry epochs drawn from two sequences
that both start at zero, so the rule that decides between them is no longer the rule stated above.

**This was an open consequence of this ADR and the specification half of it is closed.** When this
ADR was written, RFC-0001 #safety-estop-state-machine stated the single-writer property and said
nothing about what survives a failover, while the assignment path stated its own failover property
explicitly — `assignmentGeneration` and the per-robot fencing tokens are drawn from a persisted
store, and RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee says so. That
asymmetry is resolved: RFC-0001 #safety-estop-state-machine now carries a scope statement saying
that the guarantee holds within one leader's term, that a stop is ungated in every term, that a
robot found `Resuming` by a leader holding no record for it is re-armed rather than released, and
that ordering between a clear before the change and a trigger after it is not established by the
epoch. **The implementation half is unchanged and is not a defect**: the specification does not
require a persisted epoch, and this ADR's Decision stands as written. Persisting one remains an
option a later revision may take, and would be an `api/v1` change.

**A second writer would break it.** Any future path that writes `Robot.status.estopState` outside
the dispatcher — a webhook default, a migration, a second controller — makes the per-robot
sequence partial and silently removes the guarantee. Decision 6 is the invariant to check before
adding one.

**What is tested.** `internal/safety/estop_ordering_test.go` covers the interleavings the decision
turns on: a clear issued before a trigger does not release the stop (`:67`), a clear issued after
one does (`:108`), a stale epoch never blocks a stop (`:131`), the latest clear is not dropped by
the gate (`:152`), and a concurrent trigger and clear resolve cleanly (`:172`).
