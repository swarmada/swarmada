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
   - `FleetTask`: Pending → Running → Succeeded/Failed under each `completionPolicy`
     (`All`/`Any`/`Quorum`); `failurePolicy: Compensate` → Compensating →
     Compensated; `spec.desiredState: Cancelled` → Cancelled; and the `dependsOn`
     dependency graph gating when a member starts.
   - `Robot`: no heartbeat past the timeout → Offline; capability derivation from
     hardware status and overrides.
   Assert **idempotency**: reconciling a stable object twice produces no status write.
3. **Integration / end-to-end (simulation on `kind`).** Scenario-driven runs against a
   real control plane and the live simulation adapter on a `kind` cluster. Two targets,
   of which CI runs one:
   - `make quickstart-test` (`examples/warehouse-quickstart/run.sh`) — the quickstart
     run end-to-end, asserted, then torn down. The `quickstart` job in
     `.github/workflows/ci.yml` runs it on the LIVE path, as a local run does: the job
     installs Python, and a real sim adapter drives one robot.
   - `make demo-test` (`examples/full-surface-demo/run.sh`) — a headless gate that no
     CI job runs. It asserts every phase and estop transition, the RFC-0001 #control-plane-observability-prometheus-metrics-contract alert set, and RA-1
     status-write discipline, then deletes the cluster. It fails on any miss.

   Scenarios are selected with `SCENARIO=`. `hardware-fault` drives the camera-fault →
   capability-degrade → reroute → recovery arc. The in-flight reroute is real: the
   capability an assigned `FleetAction` needs going `Degraded` makes the controller
   safe-stop the action over the live ControlStream, requeue it with reason
   `CapabilityLost`, and reassign it to an idle spare
   (`reasonCapabilityLost` in `internal/controller/fleetaction_controller.go`). The
   camera fault comes from the live adapter's own clock, and the spare runs its own
   live adapter, which reports the rerouted task complete; the runner writes no
   status.

   The path that carries the adapter's `TelemetryPayload.hardware` into
   `Robot.status.hardware` is `mapHardware` (`internal/controlstream/translate.go`) →
   `mergeHardware` (`internal/telemetry/projector.go`) → `applyHardware`
   (`internal/controller/robot_status_sink.go`). No CI job runs this scenario — the
   `quickstart` job runs `healthy-fleet` — so the arc is
   asserted only when the runner is run locally. The runner fails with diagnostics if the
   live robot's `camera_front` capability never derives `Active`, or if the task is not
   rerouted within 60 s (`handle_hardware_fault` in `examples/warehouse-quickstart/run.sh`).

   **What is projected rather than derived.** On the default LIVE path the live robot's
   `Robot.status.phase` is advanced off `Discovered` by the Robot reconciler from fresh
   adapter liveness (ADR-0029), so its readiness is derived. The other two robots have
   no Fleet Adapter and nothing ever reports liveness for them, so the runner writes
   `status.phase = Idle` on those to make the scheduler's `Idle` filter pass; with
   `--projected` the runner takes the fully projected path and writes it on all three.
   `examples/warehouse-quickstart/run.sh` announces each such write at the step that
   performs it, and `docs/quickstart.md` ("Honest notes") carries the full list.

   `make demo-a` remains as an advanced target that applies `config/samples/`; it is not
   an end-to-end gate. The manual `demo-b-inject` / `demo-b-recover` targets were removed
   as an RA-1 anti-pattern — they wrote `Robot.status` by hand.
4. **Fleet Adapter conformance.** Every adapter must pass the conformance suite
   before it is considered Swarmada-compatible. The suite exercises the protocol
   contract, not any specific vendor. An executable harness lives in
   `adapters/conformance/` (`make conformance`) and covers checks C0–C19,
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
- `make test` — both, after `helm-verify-sync`, `check-error-strings` and
  `docs-cli-check`.
- `make conformance` — the Fleet Adapter conformance suite (C0–C19).
- `make demo-test` — headless end-to-end gate on `kind` (requires Docker; not run by CI).
- `make quickstart-test` — the quickstart end-to-end on `kind` (requires Docker).

GitHub Actions runs `.github/workflows/ci.yml` on every push to `main` and every pull
request against it, and `.github/workflows/dco.yml` checks the DCO sign-off on those pull
requests. CI runs `make test-go` and `make test-py`, not `make test`;
[Which gates run, and when](gate-invocation.md) lists every step.

## Determinism

Tests must be deterministic. Sort collections before asserting, and do not depend on
map iteration order or on wall-clock timing beyond controlled fakes. A flaky test is
treated as a failing test.
