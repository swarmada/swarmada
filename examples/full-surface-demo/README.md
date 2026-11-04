# Full-surface sim harness

The integration / visual / metrics layer on top of the reference simulation
adapter. Where the conformance harness (`make conformance`, C0–C19) proves
per-endpoint correctness, this proves the whole surface flows end-to-end through a
real control plane: every consumed adapter field, every metrics-contract alert, and all three
RPC services (ControlStream, SafetyStream, EdgeStream) in one run.

It reuses the existing pieces — the `full-surface` scenario preset, the one
`KinematicSim` value generator, the quickstart cluster bring-up, and swarmtop — and
adds nothing that forks a second mock.

## The three entry points

| Target | Purpose | Human? |
|---|---|---|
| `make demo` | Bring up kind + the live full-surface sim, then launch swarmtop so a human watches values flow through every view. Pick another scenario with `make demo SCENARIO=hardware-fault`. | yes |
| `make demo-test` | Headless gate (no CI job runs it): run full-surface, assert every targeted phase/estop-state in CRD status, assert the metrics-contract alert expressions, assert RA-1, assert EdgeStream coverage, then delete the cluster. Exit code is the gate. | no |
| `make demo-test-walkthrough` | The same gate, narrated — explains each phase, what to expect, and how to watch it, pausing for Enter between phases. `DEMOTEST_NO_PROMPT=1` keeps the explanations but skips the pauses (e.g. to record). | guided |
| `adapters/scenarios/presets/full-surface.yaml` | The coverage scenario itself — touches every consumed field once on a compact timeline. Rides the existing `ScenarioEngine`. | — |

## What is REAL vs PROJECTED (honesty note)

Same convention as `examples/warehouse-quickstart`: the status sink writes only
battery + hardware from telemetry (RA-1), and several states have no control-plane
writer in this scenario. So the harness drives a mix and labels it:

- **Live-driven** (real control-plane code): `status.hardware`
  Healthy→Degraded→Failed→Healthy, FleetAction `Revoking` (comms drop → lease
  expiry), `Assigned`/`InProgress` (scheduler), `Succeeded` (the live robot's
  adapter reports arriving at its target), RobotPhase `Discovered` and
  `Offline` (written by the Robot controller from a dedicated fixture,
  `discovered_offline_fixture`), the reconnect counter, and the RA-1 write/frame
  ratio.
- **Projected via `kubectl --subresource=status`** (same mechanism the quickstart
  uses to project `Idle`): RobotPhase `Idle`-bootstrap for robots with no Fleet
  Adapter, `Charging`, `Error`, `Maintenance`; `status.estopState`
  Stopping→Stopped→Normal on the live robot; `sim-fleet-adapter`'s `Connected`
  phase, and the Connected→Disconnected→Connected walk of a second, robot-less
  `demotest-idle-adapter`; and a canary robot and canary FleetAction walked through
  `Assigned`, `Charging`, `Error`, `Offline`, `Maintenance`, `Pending`, `InProgress`,
  `Revoking`, then deleted so the stuck-`Revoking` alert can clear. `Succeeded` is
  never projected: if the live robot does not complete an action, `demo-test` fails.
  `demo-test` asserts the projected phases were *observed* and the output marks them
  projected.

## The metrics-contract alert fixtures

Three alerts are driven by projected CRD status, which the metrics sweeper turns
into gauges; the other four need a deterministic fault fixture (all driven by
config/orchestration or an env-gated adapter delay — no second value generator):

| Alert | Driven by |
|---|---|
| `SwarmadaFleetActionStuckRevoking` | the canary FleetAction projected to `Revoking` for 20s, longer than the 15s metrics sweep |
| `SwarmadaFleetAdapterDisconnected` | `demotest-idle-adapter` projected `Disconnected` → `connected==0` |
| `SwarmadaRobotEstopUncleared` | the live robot's `estopState` projected `Stopped` → `robots_in_estop` |
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
documented known-gap: its counter (`swarmada_estop_latency_violations_total`) is
written only by the real control-plane estop dispatcher, and this scenario issues no
real estop. The run deploys `config/overlays/quickstart-mtls` and `sim_adapter.py`
connects over mTLS (`--tls-ca`, `--tls-cert`, `--tls-key`), so the adapter has a
verified identity and the dispatcher could route a `SafetyStream` estop to it; but
nothing in the run writes the `swarmada.io/estop-triggered` annotation, so no
`TriggerEstop` is issued and no latency is measured.

Consequently the estop **states** (`Stopping`/`Stopped`) and `RobotEstopUncleared`,
and the FleetAdapter **connectivity** (`AdapterDisconnected` fire+clear), are driven
by CRD-status **projection** (the metrics sweeper computes `robots_in_estop` and
`fleet_adapter_connected` from status), the same convention as the canary phase
walk. Only the latency-violation counter resists projection.

`demo_test.py --gate-known-gaps` makes `SwarmadaEstopLatencySLOBreach` required to
fire. `run.sh` does not pass it, because no step of the run issues a real estop.

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
- [ ] The alerts fire (the run prints `alerts fired:` — expect six of the seven;
  `SwarmadaEstopLatencySLOBreach` is the known gap above).
- [ ] The edge marker appears in `/tmp/demotest-adapter.log` after the safety trip.

Useful knobs while debugging: `DEMOTEST_KEEP=1` (leave the cluster up),
`DEMOTEST_DURATION`, `SCHED_STALL_SECONDS`, `ESTOP_ACK_DELAY_MS`, `EDGE_ESTABLISH_WAIT`.

## Offline tests (no cluster)

- `tests/python/test_promql_lite.py` — the metrics-contract alert-expression evaluator (parse, histogram quantile, fire/clear for all seven).
- `tests/python/test_demo_test.py` — the driver's pure helpers (phase coverage, RA-1 ratio, edge marker).

Run with `python3 -m pytest tests/python/test_promql_lite.py tests/python/test_demo_test.py`.
