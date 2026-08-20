# ADR-0043: Estop fan-out is unbounded — one goroutine per robot, not a worker pool

- **Status:** accepted
- **Date:** 2026-08-20 (proposed and decided)
- **Deciders:** Al — one goroutine per robot, unbounded, as implemented in
  `internal/controller/estop_fanout.go`. A bounded worker pool and the bound-by-adapter variant were
  ruled out as reintroducing send-behind-ack at a coarser granularity; splitting send from collect is
  acknowledged as the better shape at very large scale and left as the documented next step
- **Related:** RFC-0001 §9.6.2.1 (estop scopes — the parallel-dispatch requirement this implements), §9.6.2.2 (timing requirements, fleet-scope row), §9.3.8 (metrics contract), ADR-0042 (`swarmada_estop_fanout_duration_seconds` — the metric that made the gap visible), ADR-0016 (per-adapter estop delivery timeout)

## Context

RFC-0001 §9.6.2.1 requires the control plane to send an `estop` Command to every in-scope
robot's Fleet Adapter **in parallel — not sequentially — so that each command is issued
independently and a slow acknowledgement from one robot does not delay the estop signal to
others.**

The reference zone- and namespace-scoped reconcilers did not. Both looped over the robot set
awaiting each robot's full outcome before sending to the next. On a 50-robot zone estop the last
robot was commanded tens of seconds after the operator hit the trigger, and — because
`swarmada_estop_command_latency_seconds` is stamped per robot immediately before *that robot's*
send — every round trip still measured healthy and the violation counter stayed at zero
(ADR-0042).

Parallelising was therefore not a judgement call; the spec already mandated it. **The judgement
call is what "parallel" is implemented as.** The obvious safe-looking shape — a bounded worker
pool — turns out to reintroduce the defect it is meant to fix.

The relevant structure of `TriggerEstop` is that the send is cheap and the *wait* is not. Per
robot it performs a cache-backed robot read, one `SafetyStream` send, a bounded wait for the
first `EstopAck` (default 2s, namespace-tunable via `spec.estop.delivery.perAdapterTimeoutMs`,
ADR-0016), then for a `STOPPING` ack a further confirm wait (default 10s), and finally one
`Robot.status` patch. Wall-clock cost per robot is dominated entirely by the two waits.

## Decision

**Fan out with one goroutine per in-scope robot, unbounded.** No semaphore, no worker pool, no
batching. `estopFanout` launches every robot's `TriggerEstop` concurrently, joins on a
`sync.WaitGroup`, and returns per-robot outcomes in a pre-indexed slice so aggregation is
deterministic regardless of completion order.

## Why not a worker pool

**A pool of size N means robot N+1's SEND waits on some other robot's ACK.** That is the same
defect the change exists to remove, at a coarser granularity — it only needs a fleet larger than
N to expose it. With `N = 16` and a zone of 200 robots where the first 16 adapters are silent,
robot 17's stop is not even *issued* until a 2s delivery timeout expires; with several such
waves the last robot is again tens of seconds late, and again every measured round trip is
healthy. A bound would be a resource guard purchased with precisely the property §9.6.2.1
requires.

There is no bound that is both safe and useful here. Any N small enough to meaningfully cap
resources is small enough to serialise a real fleet; any N large enough not to serialise a real
fleet is not meaningfully capping anything.

## Why unbounded is affordable

**The wire is already protected.** Concurrent sends to one adapter converge on a single
`SafetyStream`, whose `safetyWriter.Send` is mutex-guarded and stamps the monotonic seq under
that lock — its own comment anticipated this exact usage: *"Multiple TriggerEstop calls for
different robots on the same adapter may send concurrently; the mutex keeps the gRPC Send safe."*
No individual stream is overrun, and per-adapter ordering is preserved.

**The rest of the path is concurrency-safe, and was already documented as such.** The
dispatcher's pending-ack registry is guarded by its own mutex; `audit.Log.Record` states it is
safe for concurrent use and chains per namespace under a lock; Prometheus collectors are
thread-safe; each goroutine patches a different `Robot`.

**The safety-critical work happens before the expensive work.** On the confirmed path every
goroutine issues its send *before* it performs any API-server write — the status patch follows
the ack. So all N stops are commanded essentially immediately, and the writes that follow are
naturally spread across the acks rather than queued ahead of them. Even under client-side rate
limiting, the thing that must not be serialised is not, and the thing that tolerates queueing is
governed by a limiter designed for it.

**Goroutines do not accumulate.** Each is bounded by the delivery timeout plus the confirm
timeout — roughly 12 seconds at the defaults — so an episode drains on a fixed schedule rather
than growing with fleet size. An estop fan-out is also not a steady-state workload: it is a rare,
operator- or safety-triggered burst.

**The cost is one goroutine and one status patch per robot, for one episode.** At a few KB of
stack each, a fleet large enough for that to matter is a fleet where a *serialised* fan-out would
already be catastrophic.

## Consequences

- **Episode wall clock becomes the SLOWEST robot rather than the SUM.** A fan-out now completes
  within roughly one confirm-timeout regardless of fleet size, which is what makes the §9.6.2.2
  fleet-scope row satisfiable at all.
- **Peak API-server write concurrency now scales with fleet size** during an episode: up to one
  `Robot.status` patch per in-scope robot. This is the real cost of the decision and the thing to
  watch. It is bounded per episode, not sustained.
- **Test doubles must be concurrency-safe.** `go test -race` immediately surfaced unguarded
  appends in the estop fakes — a genuine consequence, not incidental: any future double standing
  in for the estopper is now called concurrently.
- **Arrival order carries no information.** Anything asserting on the order robots were estopped
  in is asserting on the Go scheduler. `estopFanout` returns results positionally and the doubles
  normalise to sorted copies, so callers cannot accidentally depend on it.
- **`swarmada_estop_fanout_duration_seconds` becomes a regression guard.** It was added
  (ADR-0042) to expose the serialised fan-out; with this change it is what would catch a
  regression back to serialised dispatch, including one introduced by "adding a small
  semaphore".
- **If a fleet ever genuinely outgrows this, the fix is phase-splitting, not a pool.** Split
  `TriggerEstop` into a send phase and a collect phase — issue every send, then gather every ack
  — which preserves independent sends while collapsing the goroutine count. This is recorded at
  the call site so the next person to feel the urge to add a semaphore finds the alternative
  first.

## Alternatives considered

**Bounded worker pool (semaphore of N).** Rejected above: it reintroduces send-behind-ack
serialisation for any fleet larger than N, which is the defect being fixed.

**Bound by adapter rather than by robot** — one goroutine per adapter, robots sequential within
it. Rejected: it makes fan-out latency proportional to the largest robots-per-adapter fan-in.
A namespace where one adapter serves 200 robots — an ordinary VDA5050 or fleet-manager
deployment — would serialise exactly as before. It also mistakes the per-adapter *send* lock
(microseconds, already held) for a per-adapter *ack* constraint (seconds), which does not exist.

**Split the send and collect phases now.** This is the better shape at very large scale and is
named above as the escape hatch. Rejected at v0.3 as premature: it requires restructuring
`TriggerEstop`'s single-call contract — which owns register → send → wait → resolve →
patch as one unit with a `defer deregister` — and every caller and test double with it, to solve
a resource problem no deployment has yet reported. Doing it under the same change that fixes the
safety defect would have coupled a correctness fix to a much larger refactor.

**Cap the fan-out and let the remainder proceed on the next reconcile.** Rejected outright: it
makes an emergency stop eventually-consistent. A robot whose stop is deferred to a requeue is a
robot still moving, and the operator has no way to tell which.
