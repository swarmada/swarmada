# Testing Strategy

How Swarmada is tested. For contributors and automated assistants writing or
reviewing tests. Complements [coding-standards.md](coding-standards.md).

## Principle: simulation-first

Swarmada is developed and tested without physical hardware. Robots are simulated
(NVIDIA Isaac Sim), and the **same Fleet Adapter gRPC interface** is used in
simulation and in production — so a scenario that passes in simulation exercises the
real control-plane paths, not a mock of them. Hardware validation is additive; it is
never a prerequisite for CI.

## Test layers

1. **Unit tests (Go).** Pure logic — the scheduler's selection policy, the
   health/capability derivation, validation helpers. Prefer table-driven tests. The
   `Scheduler` is an interface, so controllers are tested against a fake scheduler.
   Aim for meaningful coverage on `internal/scheduler` and the capability-derivation
   paths in `internal/controller`.
2. **Controller tests (envtest).** Reconcile logic against a real API server via
   controller-runtime's `envtest`. Cover the state machines:
   - `FleetTask`: Pending → Scheduled → Running → Succeeded/Failed; deadline exceeded
     → Failed; assigned robot Degraded/Offline → requeue to Pending.
   - `Robot`: no heartbeat past the timeout → Offline; capability derivation from
     hardware status and overrides.
   Assert **idempotency**: reconciling a stable object twice produces no status write.
3. **Integration / end-to-end (simulation).** The Isaac Sim harness drives full
   scenarios:
   - *Demo A* — discovery → admission → assignment.
   - *Demo B* — camera-fault → capability-degrade → task-reroute → recovery, with the
     fault injected via the simulation fault-injection tooling. The reroute/recovery
     steps depend on *Capability-loss reassignment* (RFC-0001 control-plane; **impl:
     planned**); until it lands, Demo B exercises the capability-degrade → recover arc
     and the *placement* filter (a new task avoids the degraded robot), not in-flight
     reroute.
   These are the reference scenarios; keep them runnable and green.
4. **Fleet Adapter conformance.** Every adapter must pass the conformance suite
   before it is considered Swarmada-compatible. The suite exercises the protocol
   contract, not any specific vendor. An executable harness lives in
   `adapters/conformance/` (`make conformance`) and covers checks C0–C16,
   including the optional-command (C7), edge (C8) and contract-version (C13–C15)
   checks. A `skip` is not a pass: every skip records an enumerated cause
   (`adapters/conformance/report.py`, `SkipCause`) naming why the check could not
   run, and C0.1 fails the suite if a run contradicts one of those causes.

## What to test when you change…

- **an API type** → regenerate, then add or adjust validation and controller tests
  for the new fields.
- **a controller** → cover the new transition in `envtest`; assert no spurious status
  writes.
- **the scheduler** → table-driven unit tests for the new policy; verify the
  interface contract still holds.
- **the proto** → conformance-suite coverage for the new RPC or behavior.

## Running tests

- `make test-go` — Go unit tests and `envtest`.
- `make test-py` — Python (`pytest`).
- `make test` — both.

Tests run on every pull request via GitHub Actions; the DCO check gates merges.

## Determinism

Tests must be deterministic. Sort collections before asserting, and do not depend on
map iteration order or on wall-clock timing beyond controlled fakes. A flaky test is
treated as a failing test.
