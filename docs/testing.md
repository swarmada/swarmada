# Testing Strategy

How Swarmada is tested. For contributors and automated assistants writing or
reviewing tests. Complements [coding-standards.md](coding-standards.md).

## Principle: simulation-first

Swarmada is developed and tested without physical hardware. Robots are simulated by the
in-tree reference simulation adapter (`adapters/simulation/`), whose default backend is
`KinematicSim` — a dependency-free 2-D kinematic model with a real decelerating stop,
requiring no external simulator or licence. The **same Fleet Adapter gRPC interface** is
used in simulation and in production, so a scenario that passes in simulation exercises
the real control-plane paths, not a mock of them. Hardware validation is additive; it is
never a prerequisite for CI.

`adapters/simulation/simulator.py` also defines `IsaacSimBackend`, the seam for NVIDIA
Isaac Sim and the pattern any other simulator follows. The seam is **unimplemented**: it
lazily imports the Isaac runtime and raises rather than faking physics. No test, gate or
demo in this repository runs against Isaac Sim.

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
3. **Integration / end-to-end (simulation on `kind`).** Scenario-driven runs against a
   real control plane and the live simulation adapter on a `kind` cluster. Two gates:
   - `make demo-test` (`examples/full-surface-demo/run.sh`) — the headless CI gate. It
     asserts every phase and estop transition, the §9.3.8 alert set, and RA-1
     status-write discipline, then deletes the cluster. It fails on any miss.
   - `make quickstart-test` (`examples/warehouse-quickstart/run.sh`) — the quickstart
     run end-to-end, asserted, then torn down.

   Scenarios are selected with `SCENARIO=`. `hardware-fault` drives the camera-fault →
   capability-degrade → recovery arc, with the fault injected via the simulation
   fault-injection tooling. The reroute leg depends on *Capability-loss reassignment*
   (RFC-0001 control-plane), which is not implemented, so the scenario exercises the
   degrade → recover arc and the *placement* filter (a new task avoids the degraded
   robot), not in-flight reroute.

   **Known gap.** The admission → assignment leg is not end-to-end. No component owns
   the `Discovered` → `Idle` transition (`crds/discoveredrobot.md:342` requires it; no
   ownership table in `control-plane.md` claims it), so the run projects
   `Robot.status.phase = Idle` directly to make the scheduler's `Idle` filter pass. The
   scheduling itself is real; robot readiness is asserted, not derived.
   `examples/warehouse-quickstart/run.sh` announces this at the step that performs it.

   `make demo-a` remains as an advanced target that applies `config/samples/`; it is not
   an end-to-end gate. The manual `demo-b-inject` / `demo-b-recover` targets were removed
   as an RA-1 anti-pattern — they wrote `Robot.status` by hand.
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
- `make conformance` — the Fleet Adapter conformance suite (C0–C16).
- `make demo-test` — headless end-to-end gate on `kind` (requires Docker).
- `make quickstart-test` — the quickstart end-to-end on `kind` (requires Docker).

Tests run on every pull request via GitHub Actions; the DCO check gates merges.

## Determinism

Tests must be deterministic. Sort collections before asserting, and do not depend on
map iteration order or on wall-clock timing beyond controlled fakes. A flaky test is
treated as a failing test.
