# Full-surface sim harness

The integration / visual / metrics layer on top of the reference simulation
adapter. Where the conformance harness (`make conformance`, C1–C8) proves
per-endpoint correctness, this proves the whole surface flows end-to-end through a
real control plane: every consumed adapter field, every §9.3.8 alert, and all three
RPC services (ControlStream, SafetyStream, EdgeStream) in one run.

It reuses the existing pieces — the `full-surface` scenario preset, the one
`KinematicSim` value generator, the quickstart cluster bring-up, and swarmtop — and
adds nothing that forks a second mock.

## The three entry points

| Target | Purpose | Human? |
|---|---|---|
| `make demo` | Bring up kind + the live full-surface sim, then launch swarmtop so a human watches values flow through every view. Pick another scenario with `make demo SCENARIO=hardware-fault`. | yes |
| `make demo-test` | Headless CI gate: run full-surface, assert every targeted phase/estop-state in CRD status, assert the §9.3.8 alert expressions, assert RA-1, assert EdgeStream coverage, then delete the cluster. Exit code is the gate. | no |
| `make demo-test-walkthrough` | The same gate, narrated — explains each phase, what to expect, and how to watch it, pausing for Enter between phases. `DEMOTEST_NO_PROMPT=1` keeps the explanations but skips the pauses (e.g. to record). | guided |
| `adapters/scenarios/presets/full-surface.yaml` | The coverage scenario itself — touches every consumed field once on a compact timeline. Rides the existing `ScenarioEngine`. | — |

## What is REAL vs PROJECTED (honesty note)

Same convention as `examples/warehouse-quickstart`: the status sink writes only
battery + hardware from telemetry (RA-1), and several RobotPhases have no
control-plane writer from live signals. So the harness drives a mix and labels it:

- **Live-driven** (real control-plane code): `status.hardware`
  Healthy→Degraded→Failed→Healthy, `status.estopState` Stopping→Stopped, `Offline` +
  FleetTask `Revoking` (comms drop → lease expiry), `Assigned`/`InProgress`
  (scheduler), the adapter-connected gauge, the reconnect counter, and the RA-1
  write/frame ratio.
- **Projected via `kubectl`** (no live writer exists — same mechanism the quickstart
  uses to project `Idle`): RobotPhase `Idle`-bootstrap, `Charging`, `Error`,
  `Maintenance`; FleetTask `Succeeded`. `demo-test` asserts they were *observed* and
  the output marks them projected.

## The §9.3.8 alert fixtures

Three alerts fire naturally from full-surface; the other four need a deterministic
fault fixture (all driven by config/orchestration or an env-gated adapter delay — no
second value generator):

| Alert | Driven by |
|---|---|
| `SwarmadaFleetTaskStuckRevoking` | comms drop → lease-expiry Revoking (natural) |
| `SwarmadaFleetAdapterDisconnected` | comms drop → `connected==0` (natural) |
| `SwarmadaRobotEstopUncleared` | full-surface estop → `estop_state=Stopped` (natural) |
| `SwarmadaEstopLatencySLOBreach` | `SWARMADA_SIM_ESTOP_ACK_DELAY_MS` delays the estop ACK past 500ms |
| `SwarmadaTelemetryDroppedFrames` | `SwarmadaConfig` sink → unreachable remote-write endpoint |
| `SwarmadaTelemetryTSDBWriteErrors` | same failing sink |
| `SwarmadaSchedulerAssignmentLatencyHigh` | a Normal task held >60s with no capable robot, then made schedulable |

The three gauge alerts fire **and clear** within the run; the four range
(`increase`/`histogram_quantile`) alerts fire when their fixture triggers but only
decay after their `[5m]`/`[10m]` window — `demo-test` asserts fire for six and
clear for the gauges (`--require-clear gauge`).

### mTLS known-gap: `SwarmadaEstopLatencySLOBreach`

`demo-test` gates on **6 of the 7** alerts. `SwarmadaEstopLatencySLOBreach` is a
documented known-gap because it cannot fire in the insecure demo: its counter
(`swarmada_estop_latency_violations_total`) is written only by the real control-plane
estop dispatcher, and routing a SafetyStream estop to a *named* adapter requires
mTLS. In insecure mode the adapter's stream carries an empty identity (namespace and
adapter-name both blank — visible as `namespace=""` on the telemetry counters), so
`Dispatcher.TriggerEstop` finds no stream for `sim-fleet-adapter` and the estop
resolves `Failed`, never recording a latency violation.

Consequently the estop **states** (`Stopping`/`Stopped`) and `RobotEstopUncleared`,
and the FleetAdapter **connectivity** (`AdapterDisconnected` fire+clear), are driven
by honest CRD-status **projection** (the metrics sweeper computes `robots_in_estop`
and `fleet_adapter_connected` from status), the same convention as the canary phase
walk. Only the latency-violation counter resists projection.

To gate on all 7, run the demo under **mTLS** (adapter presents a client cert whose
SAN names the FleetAdapter) and pass `--gate-known-gaps` to `demo_test.py`. That is a
larger change — `sim_adapter.py` dials `insecure_channel` — tracked as
follow-up work, not part of this harness.

## RA-1 end-to-end

`demo-test` asserts `swarmada_telemetry_status_writes_total` /
`swarmada_telemetry_frames_received_total` stays under a ceiling (default 0.5). A
compliant projector writes only on material transitions (ratio ≪ 1); a per-tick
writer (ratio ≈ 1) fails the gate. Tighten with `--ra1-max-ratio`.

## EdgeStream coverage

`cmd/edge` runs on the host; the zone advertises `spec.edgeNode.address` *before* the
adapter registers (the adapter dials the endpoint from its register-time
`RegisterAck`). `demo-test` trips a file-backed safety input (`1`→`0`, active-low),
which drives `Node.TriggerLocalEstop` → a zone-wide headless estop over EdgeStream →
the adapter confirms it (C8). Because an edge estop is confirmed directly between the
edge node and adapter (bypassing the control plane), it is not visible in CRD status
or control-plane metrics — the adapter emits an `EDGE_ESTOP_CONFIRMED` log marker and
`demo-test` asserts on it.

## kind validation checklist

Run `make demo-test` on a machine with Docker + kind + Go. It should end with a
single success line and exit 0. If it fails, confirm each of these (the parts that could not be
validated without a live cluster):

- [ ] `go build ./cmd/edge` succeeds and `bin/edge --insecure … --safety-input-file …` starts.
- [ ] `demo_a.yaml`'s FleetZone is named `warehouse-a` (the edge patch assumes it; adjust `run.sh` if not).
- [ ] The `SwarmadaConfig` sink fields are `spec.telemetry.sink.type` / `.endpoint` (per `api/v1/swarmadaconfig_types.go`); fix the heredoc in `apply_fleet_and_fixtures` if the CRD differs.
- [ ] The `demotest-slow-task` FleetTask spec (`spec.zone`, `spec.priority`, `spec.requiredCapabilities`) matches the real FleetTask CRD.
- [ ] Every projected phase actually lands (watch `kubectl get robots -n warehouse-a -w` during the run).
- [ ] The four fixture alerts fire (the run prints `alerts fired:` — expect all seven).
- [ ] The edge marker appears in `/tmp/demotest-adapter.log` after the safety trip.

Useful knobs while debugging: `DEMOTEST_KEEP=1` (leave the cluster up),
`DEMOTEST_DURATION`, `SCHED_STALL_SECONDS`, `ESTOP_ACK_DELAY_MS`, `EDGE_ESTABLISH_WAIT`.

## Offline tests (no cluster)

- `tests/python/test_promql_lite.py` — the §9.3.8 alert-expression evaluator (parse, histogram quantile, fire/clear for all seven).
- `tests/python/test_demo_test.py` — the driver's pure helpers (phase coverage, RA-1 ratio, edge marker).

Run with `python3 -m pytest tests/python/test_promql_lite.py tests/python/test_demo_test.py`.
