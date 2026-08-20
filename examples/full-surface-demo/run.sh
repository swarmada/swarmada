#!/usr/bin/env bash
# Copyright 2026 The Swarmada Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# full-surface demo-test — the HEADLESS CI gate (make demo-test).
#
# Brings up a real Swarmada control plane on a throwaway kind cluster, drives the
# full-surface coverage scenario live through the reference sim adapter, applies
# the deterministic fault fixtures the four range-based §9.3.8 alerts need, runs the
# Python assertion driver (demo_test.py) against the CRD API + scraped /metrics, and
# ALWAYS deletes the cluster on exit. Prints ✅/❌; the exit code is the gate.
#
# Human-in-the-loop watching is `make demo` (examples/warehouse-quickstart). This is
# the CI counterpart: no swarmtop, no pauses, assertions only.
#
# What is REAL vs PROJECTED (honest, matching examples/warehouse-quickstart):
#   - REAL from the live adapter + control plane: status.hardware degrade→fail→recover,
#     status.estopState Stopping→Stopped, Offline + FleetAction Revoking (comms drop →
#     lease expiry), Assigned/InProgress (scheduler), the adapter-connected gauge, the
#     reconnect counter, and RA-1 (status_writes vs frames_received).
#   - PROJECTED via kubectl --subresource=status (the RA-1 anti-pattern; RFC-0001
#     crds/robot.md:312-314 reserves status to controllers): RobotPhase Idle-bootstrap,
#     Charging, Error, Maintenance, and FleetAction Succeeded. Idle-bootstrap is not a
#     cosmetic shortcut — no component owns the Discovered->Idle transition
#     (crds/discoveredrobot.md:342 requires it; no ownership table in control-plane.md
#     claims it), so no robot is schedulable without it. demo_test.py asserts these were
#     OBSERVED, which is weaker than asserting the control plane produced them; the run
#     output labels them projected. See docs/quickstart.md, Honest notes.
#   - Fault FIXTURES for the range alerts: a SwarmadaConfig pointing the TSDB sink at
#     an unreachable endpoint (dropped-frames + tsdb-write-errors), a delayed estop
#     ack via SWARMADA_SIM_ESTOP_ACK_DELAY_MS (estop-latency SLO breach), and a task
#     held >60s with no capable robot (scheduler assignment-latency).
#
# Env knobs (all optional):
#   DEMOTEST_CLUSTER      kind cluster name (default: swarmada-demotest)
#   DEMOTEST_METRICS_PORT local port for the /metrics port-forward (default: 18080)
#   DEMOTEST_KEEP=1       do NOT delete the cluster on exit (debugging)
#   ESTOP_ACK_DELAY_MS    delay injected into the live estop ack (default: 750)
#   SCHED_STALL_SECONDS   how long the no-capable-robot task is held (default: 65)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${DEMOTEST_CLUSTER:-swarmada-demotest}"
IMG="${DEMOTEST_IMG:-swarmada-controller:dev}"
NS="warehouse-a"
MANAGER_NS="system"
MANAGER_DEPLOY="swarmada-controller-manager"
LIVE_ROBOT="sim-robot-001"
METRICS_PORT="${DEMOTEST_METRICS_PORT:-18080}"
CS_PORT="${DEMOTEST_CS_PORT:-19444}"
ESTOP_ACK_DELAY_MS="${ESTOP_ACK_DELAY_MS:-750}"
SCHED_STALL_SECONDS="${SCHED_STALL_SECONDS:-65}"
EDGE_PORT="${DEMOTEST_EDGE_PORT:-9600}"
EDGE_DIR="$(mktemp -d)"           # edge config + safety-input file live here
EDGE_SAFETY_FILE="$EDGE_DIR/safety-input"
EDGE_LOG="/tmp/demotest-edge.log"
ADAPTER_LOG="/tmp/demotest-adapter.log"
PY="$(command -v python3 || command -v python)"
CTX="kind-${CLUSTER}"
PIDS=()

step() { echo; echo "==> $*"; }
info() { echo "    $*"; }
fail() { echo "❌ $*" >&2; exit 1; }

# ── Interactive walkthrough (opt-in) ─────────────────────────────────────────────
# DEMOTEST_WALKTHROUGH=1 turns demo-test into a guided tour: before each phase it
# explains WHAT it does, WHAT to expect, and HOW to see it yourself, then waits for
# Enter. DEMOTEST_NO_PROMPT=1 (or a non-TTY, e.g. CI) keeps the explanations but skips
# the pauses. Default (both unset) is the silent headless CI gate.
WALKTHROUGH="${DEMOTEST_WALKTHROUGH:-}"

# narrate TITLE -- then heredoc body on stdin: printed only in walkthrough mode.
narrate() {
  [[ -n "$WALKTHROUGH" ]] || { cat >/dev/null; return 0; }
  echo
  echo "  ┌─ $1"
  sed 's/^/  │ /'
  echo "  └─"
}

# pause_step MESSAGE: wait for Enter in walkthrough mode, unless prompting is disabled.
pause_step() {
  [[ -n "$WALKTHROUGH" ]] || return 0
  if [[ "${DEMOTEST_NO_PROMPT:-}" == "1" || ! -t 0 ]]; then
    echo "  ▸ ${1:-continuing…}"
    return 0
  fi
  read -rp "  ▸ ${1:-press Enter to continue…} " _ || true
}

cleanup() {
  local pid
  for pid in "${PIDS[@]:-}"; do [[ -n "$pid" ]] && kill "$pid" >/dev/null 2>&1 || true; done
  if [[ "${DEMOTEST_KEEP:-}" == "1" ]]; then
    info "DEMOTEST_KEEP=1 — leaving cluster '$CLUSTER' up for debugging"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

preflight() {
  step "0 — Preflight"
  for t in kind kubectl docker make; do command -v "$t" >/dev/null || fail "missing tool: $t"; done
  [[ -n "$PY" ]] || fail "python3 not found"
  docker info >/dev/null 2>&1 || fail "Docker daemon not reachable"
  [[ -f proto/fleet_adapter/v1/fleet_adapter_pb2.py ]] || make proto-py
  info "ok"
}

bring_up() {
  step "1 — Create kind cluster + deploy control plane (quickstart-dev overlay)"
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  kind create cluster --name "$CLUSTER"
  kubectl config use-context "$CTX" >/dev/null
  # Prove the cluster is real and that kubectl is aimed at IT before applying anything. Without
  # this, a failed create leaves kubectl pointed at whatever context was current and `apply -k`
  # installs the control plane into an unrelated cluster — which has already happened once here.
  # It also fails in seconds instead of after the ~2min image build (the `no nodes found` case).
  actual="$(kubectl config current-context 2>&1 || true)"
  [ "$actual" = "$CTX" ] || fail "kubectl context is '$actual', expected '$CTX' — refusing to apply"
  kubectl get nodes >/dev/null 2>&1 || fail "cluster $CLUSTER has no reachable nodes after create"
  make docker-build IMG="$IMG"
  kind load docker-image "$IMG" --name "$CLUSTER"
  kubectl apply -k config/overlays/quickstart-dev
  kubectl -n "$MANAGER_NS" rollout status "deployment/$MANAGER_DEPLOY" --timeout=180s
}

apply_fleet_and_fixtures() {
  step "2 — Apply fleet, capability gating, and the range-alert fault fixtures"
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f config/samples/demo_a.yaml

  # Gate LIVE_ROBOT's camera capability on a camera_front hardware component so the
  # adapter's camera fault (full-surface T+8..16) degrades the capability → the
  # FleetAction controller's capability-loss reassignment drives Revoking. Mirrors
  # examples/warehouse-quickstart/run.sh's hardware-fault setup.
  kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --type=json -p '[
    {"op":"add","path":"/spec/hardware","value":[{"name":"camera_front","type":"Camera"}]},
    {"op":"replace","path":"/spec/capabilities","value":[
      {"name":"navigation","type":"hardware-native","pauseable":false},
      {"name":"camera_front","type":"hardware-native","pauseable":false,"requiredHardware":["camera_front"]}
    ]}]' || true

  # TSDB fixture: a real (non-Drop) sink pointed at an unreachable endpoint, so every
  # ingested telemetry frame's remote-write POST fails → IncTSDBWriteError +
  # IncTelemetryDroppedFrame with sink.type != Drop (fires alerts 2 and 3). A blackhole
  # endpoint is deterministic and needs no external service.
  cat <<EOF | kubectl apply -f -
apiVersion: swarmada.io/v1
kind: SwarmadaConfig
metadata:
  name: swarmada-config   # singleton — the CRD requires this exact name
  namespace: $NS
spec:
  # The CRD DEFAULTS signing.requireSignatureVerification to true, so a config that mentions only
  # telemetry still demands a detached signature on every conformance report and firmware/model
  # artifact. That silently failed this fixture's adapter to conformance=Failed, which the ADR-0032
  # assignment gate then turned into "no robot is dispatchable" — no InProgress, no assignment
  # latency, for three runs. Turned off explicitly: this scenario exercises digest verification and
  # the dispatch gate, not signature distribution, and a signed-report fixture deserves its own beat
  # rather than being an invisible precondition of this one.
  signing:
    requireSignatureVerification: false
  telemetry:
    sink:
      type: PrometheusRemoteWrite
      endpoint: http://127.0.0.1:1/api/v1/write
EOF
  # EdgeStream coverage: advertise a host-reachable edge node on the zone BEFORE the
  # adapter registers (the adapter dials the endpoint from its register-time
  # RegisterAck; a later patch would be missed until the comms-drop re-handshake).
  # physicalBounds strictly CONTAINS the origin the adapter tees (x=0,y=0), so the
  # headless estop comes from the tripped safety input — deterministically — not an
  # accidental zone-boundary breach.
  kubectl patch fleetzone/warehouse-a -n "$NS" --type=merge -p "{\"spec\":{
    \"physicalBounds\":{\"floor\":0,\"polygon\":[
      {\"x\":-5.0,\"y\":-5.0},{\"x\":25.0,\"y\":-5.0},{\"x\":25.0,\"y\":25.0},{\"x\":-5.0,\"y\":25.0}]},
    \"edgeNode\":{\"address\":\"127.0.0.1:${EDGE_PORT}\"}}}" 2>/dev/null || \
    info "note: no fleetzone/warehouse-a to patch (demo_a sample may name it differently)"

  # FleetAdapter resource for the sim adapter (the robots reference adapter.name
  # sim-fleet-adapter). The metrics sweeper emits swarmada_fleet_adapter_connected
  # per FleetAdapter (connected = status.phase==Connected), and the ControlStream
  # server's AdapterPresence drives status.phase Connected↔Disconnected on
  # connect/stream-loss — so with this resource present the full-surface comms drop
  # flips the gauge to 0 (SwarmadaFleetAdapterDisconnected fires) and back to 1 on
  # reconnect. Without it the sweeper produces no series and that alert can never fire.
  # ADR-0032 assignment gate: a robot is only dispatched to through a FleetAdapter that is
  # Connected AND conformance-Passed AND qualified against a supported contract version. The
  # conformance half is made REAL here rather than projected — a digest-verified report the
  # FleetAdapter controller actually validates — so the run exercises verifyConformance and
  # populates status.conformanceContractVersion by the production path. Without it the adapter
  # sits at conformance=Unknown, every robot is withheld from dispatch, and no task can ever
  # reach Assigned/InProgress (which is exactly how this gate failed before).
  local report_json report_digest
  report_json='{"adapter":"simulation","conformant":true,"contract_version":"1.0.0"}'
  report_digest="sha256:$(printf '%s' "$report_json" | shasum -a 256 | awk '{print $1}')"
  kubectl create configmap sim-adapter-conformance -n "$NS" \
    --from-literal=report.json="$report_json" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  cat <<EOF | kubectl apply -f -
apiVersion: swarmada.io/v1
kind: FleetAdapter
metadata:
  name: sim-fleet-adapter
  namespace: $NS
spec:
  vendor: simulation
  endpoint: "127.0.0.1:${CS_PORT}"
  # The run is ~2 min and NOTHING refreshes lastHeartbeat: this run is insecure, so ControlStream
  # sees no verified mTLS identity and AdapterPresence never fires. With the default interval the
  # controller's staleness backstop flips the projected Connected -> Disconnected mid-run and races
  # step 6.8's Connected->Disconnected->Connected walk, leaving fleet_adapter_connected at 0 so
  # AdapterDisconnected fires but never clears. A long interval makes 6.8 the ONLY thing that moves
  # this phase, which is exactly what that beat is for.
  heartbeatIntervalSeconds: 600
  conformanceReport:
    suiteVersion: "C1-C15"
    configMapRef: sim-adapter-conformance
    digest: "$report_digest"
EOF

  # A SECOND FleetAdapter that drives nothing. The AdapterDisconnected alert only needs SOME
  # adapter at swarmada_fleet_adapter_connected == 0, and walking the LIVE adapter to Disconnected
  # (as this beat used to) forbids dispatch for every robot bound to it — ADR-0032's assignment gate
  # withholds candidates whose adapter is not Connected. That collided with the concurrent scheduler
  # beat: the robot became eligible during the 18s Disconnected window, so no assignment happened,
  # and both RobotPhase InProgress and SchedulerAssignmentLatencyHigh were unreachable. Giving the
  # walk its own robot-less adapter removes the contradiction instead of tuning sleeps against it.
  cat <<EOF | kubectl apply -f -
apiVersion: swarmada.io/v1
kind: FleetAdapter
metadata:
  name: demotest-idle-adapter
  namespace: $NS
spec:
  vendor: simulation
  endpoint: "127.0.0.1:1"
  heartbeatIntervalSeconds: 600
EOF

  # The phase half CANNOT be real here: this run is insecure, so ControlStream sees no verified
  # mTLS identity and AdapterPresence.AdapterConnected never fires (internal/controlstream/
  # server.go). Project Connected with a fresh lastHeartbeat so the adapter is dispatch-eligible
  # for the timed fixtures; step 6.8 later walks it Connected->Disconnected->Connected for the
  # AdapterDisconnected fire+clear. Labeled projected, like every other mTLS-gated beat.
  kubectl patch fleetadapter/sim-fleet-adapter -n "$NS" --subresource=status --type=merge \
    -p "{\"status\":{\"phase\":\"Connected\",\"lastHeartbeat\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}" \
    >/dev/null 2>&1 || true

  # Taskless canary Robot + FleetAction for projected coverage of the phases with no
  # deterministic live writer here. The canary advertises an uncommon capability and
  # the canary task requires a DIFFERENT uncommon capability, so the scheduler never
  # touches either — nothing reverts the projected status walk (walk_canary_phases).
  cat <<EOF | kubectl apply -f -
apiVersion: swarmada.io/v1
kind: Robot
metadata: {name: demotest-canary, namespace: $NS}
spec:
  manufacturer: Simulated
  model: Canary
  adapter: {name: sim-fleet-adapter, version: "0.1.0"}
  zone: warehouse-a
  capabilities:
    - {name: demotest-canary-cap, type: hardware-native, pauseable: false}
---
apiVersion: swarmada.io/v1
kind: FleetAction
metadata: {name: demotest-canary-task, namespace: $NS}
spec:
  type: Navigate
  zone: warehouse-a
  priority: Low
  requiredCapabilities: [demotest-canary-cap-unmatched]
EOF

  info "fleet applied; camera gated; failing-TSDB SwarmadaConfig applied; edge node advertised; FleetAdapter + canary created"
}

walk_canary_phases() {
  # Project the RobotPhases / FleetAction phases with no deterministic live writer onto
  # the taskless canary (the scheduler ignores it, so nothing reverts these), dwelling
  # past the poll interval so each is observed. Discovered is covered for free (the
  # robot_controller sets phase=Discovered on creation); Idle/InProgress/Maintenance
  # come from the live robots. Backgrounded so it overlaps the assertion poll window.
  step "6.7 — Project the canary through the non-live-reachable phases (labeled projected)"
  (
    local ph
    for ph in Assigned Charging Error Offline Maintenance; do
      kubectl patch robot/demotest-canary -n "$NS" --subresource=status --type=merge \
        -p "{\"status\":{\"phase\":\"$ph\"}}" >/dev/null 2>&1 || true
      sleep "${CANARY_DWELL:-2}"
    done
    # Assigned is included because it is TRANSIENT on the live path (Idle->Assigned->InProgress
    # can complete between two 0.5s polls), so relying on a real dispatch to catch it would make
    # the gate flaky. Projected, like the robot walk above.
    local tp
    for tp in Pending Assigned InProgress Revoking; do
      kubectl patch fleetaction/demotest-canary-task -n "$NS" --subresource=status --type=merge \
        -p "{\"status\":{\"phase\":\"$tp\"}}" >/dev/null 2>&1 || true
      # Revoking must OUTLAST the 15s metrics-sweeper interval
      # (internal/controller/metrics_sweeper.go defaultSweepInterval), or
      # swarmada_fleetactions_by_phase{phase="Revoking"} is never sampled and
      # SwarmadaFleetActionStuckRevoking cannot fire. A 2s dwell is enough for the 0.5s CRD
      # poller to SEE the phase but never enough for the gauge — which is precisely how that
      # alert silently failed to fire before.
      if [ "$tp" = "Revoking" ]; then
        sleep "${REVOKING_DWELL:-20}"
      else
        sleep "${CANARY_DWELL:-2}"
      fi
    done
    kubectl patch fleetaction/demotest-canary-task -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"phase":"Succeeded"}}' >/dev/null 2>&1 || true
  ) &
  PIDS+=($!)
}

launch_edge_node() {
  step "2.5 — Build + launch the edge node (cmd/edge) for EdgeStream coverage"
  if [ ! -d "$REPO_ROOT/cmd/edge" ]; then
    EDGE_SKIPPED=1
    info "⏭  cmd/edge not present in this checkout — skipping EdgeStream coverage (edge node held for a later release)"
    return 0
  fi
  ( cd "$REPO_ROOT" && go build -o bin/edge ./cmd/edge )
  # Edge config: a zone polygon containing the origin (≥3 verts required) and the
  # robot→zone assignment. For a safety-input estop the polygon is not consulted
  # (TriggerLocalEstop fans zone-wide), but a valid config needs one.
  cat >"$EDGE_DIR/config.json" <<EOF
{"zones":[{"name":"warehouse-a","floor":0,
  "polygon":[{"x":-5,"y":-5},{"x":25,"y":-5},{"x":25,"y":25},{"x":-5,"y":25}]}],
 "robotZone":{"$LIVE_ROBOT":"warehouse-a"}}
EOF
  echo "1" >"$EDGE_SAFETY_FILE"   # active-low default: "1" = SAFE, "0" = tripped
  "$REPO_ROOT/bin/edge" --insecure --namespace "$NS" \
    --config-file "$EDGE_DIR/config.json" --listen-address "127.0.0.1:${EDGE_PORT}" \
    --health-address "127.0.0.1:$((EDGE_PORT + 1))" \
    --safety-input-file "$EDGE_SAFETY_FILE" --safety-input-active-low \
    --kubeconfig "${KUBECONFIG:-$HOME/.kube/config}" >"$EDGE_LOG" 2>&1 &
  PIDS+=($!)
  info "edge node listening on 127.0.0.1:${EDGE_PORT}; log: $EDGE_LOG"
}

project_estop_states() {
  # Project estopState Stopping→Stopped→Normal on the live robot. A REAL control-plane
  # estop (annotate swarmada.io/estop-triggered → Dispatcher.TriggerEstop over
  # SafetyStream) CANNOT work here: insecure mode gives the adapter stream an empty
  # identity, so the dispatcher can't route a command to the named adapter
  # (sim-fleet-adapter) — the estop resolves Failed, never Stopped. The estop states +
  # RobotEstopUncleared are therefore projected (honestly labeled), driving the sweeper's
  # robots_in_estop gauge for real. The one thing projection can't reach is
  # swarmada_estop_latency_violations_total (EstopLatencySLOBreach) — that counter needs
  # the real mTLS estop path; it is a documented known-gap (see README, demo_test.py).
  # Dwell each metric-driving state LONGER than the 15s metrics-sweeper interval
  # (internal/controller/metrics_sweeper.go defaultSweepInterval) — the sweeper
  # recomputes robots_in_estop every 15s, so a shorter window is never sampled and the
  # gauge (hence RobotEstopUncleared) never moves even though the CRD state is observed.
  step "6.6 — Project estopState Stopping→Stopped→Normal on $LIVE_ROBOT (mTLS-gated real path; see README)"
  (
    sleep "${ESTOP_TRIGGER_AFTER:-4}"
    kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"estopState":"Stopping"}}' >/dev/null 2>&1 || true
    sleep 3
    kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"estopState":"Stopped"}}' >/dev/null 2>&1 || true
    sleep "${ESTOP_STOPPED_HOLD:-18}"   # > sweep interval → robots_in_estop{Stopped} fires
    kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"estopState":"Normal"}}' >/dev/null 2>&1 || true
    # Normal persists for the rest of the run, so a later sweep samples it → the
    # RobotEstopUncleared gauge returns to 0 and the alert clears.
  ) &
  PIDS+=($!)
}

project_adapter_phase() {
  # Project FleetAdapter connectivity Connected→Disconnected→Connected so
  # swarmada_fleet_adapter_connected goes 1→0→1 (SwarmadaFleetAdapterDisconnected fires
  # AND clears). Presence-driven connectivity needs mTLS (insecure streams carry no
  # adapter identity, so AdapterConnected never fires and the gauge would sit at 0,
  # firing-but-never-clearing). Setting phase=Connected WITH a fresh lastHeartbeat keeps
  # the FleetAdapter controller from reverting it (it only demotes on heartbeat staleness).
  # Each state must outlast the 15s metrics-sweeper interval so a sweep samples it —
  # otherwise fleet_adapter_connected never reflects the transition and the alert can't
  # fire OR clear (the bug that left it firing-but-never-cleared).
  step "6.8 — Walk demotest-idle-adapter Connected→Disconnected→Connected (AdapterDisconnected fire+clear)"
  (
    local now
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    kubectl patch fleetadapter/demotest-idle-adapter -n "$NS" --subresource=status --type=merge \
      -p "{\"status\":{\"phase\":\"Connected\",\"lastHeartbeat\":\"$now\"}}" >/dev/null 2>&1 || true
    sleep "${ADAPTER_STATE_HOLD:-18}"    # sweep sees connected=1 (baseline)
    kubectl patch fleetadapter/demotest-idle-adapter -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"phase":"Disconnected"}}' >/dev/null 2>&1 || true
    sleep "${ADAPTER_STATE_HOLD:-18}"    # sweep sees connected=0 → AdapterDisconnected fires
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    kubectl patch fleetadapter/demotest-idle-adapter -n "$NS" --subresource=status --type=merge \
      -p "{\"status\":{\"phase\":\"Connected\",\"lastHeartbeat\":\"$now\"}}" >/dev/null 2>&1 || true
    # final Connected persists → a later sweep sees connected=1 → the alert clears
  ) &
  PIDS+=($!)
}

trip_edge_safety() {
  step "6.5 — Trip the edge safety input → zone-wide headless estop over EdgeStream (C8)"
  info "waiting ${EDGE_ESTABLISH_WAIT:-8}s for the adapter to establish its EdgeStream…"
  sleep "${EDGE_ESTABLISH_WAIT:-8}"
  echo "0" >"$EDGE_SAFETY_FILE"   # trip: safe→tripped drives Node.TriggerLocalEstop
  info "safety input tripped; waiting ${EDGE_ACK_WAIT:-5}s for the confirmed edge estop…"
  sleep "${EDGE_ACK_WAIT:-5}"
}

launch_live_adapter() {
  step "3 — Launch the live full-surface adapter (with estop-ack delay fixture)"
  kubectl annotate "robot/$LIVE_ROBOT" -n "$NS" "swarmada.io/robot-id=$LIVE_ROBOT" --overwrite >/dev/null
  # Non-live robots: healthy baseline so the scheduler has a real 3-robot fleet.
  # SHORTCUT — status.phase is controller-owned (RFC-0001 crds/robot.md:312-314, RA-1).
  # Patched here because nothing transitions a Robot Discovered->Idle; scheduler filter 1
  # admits only Idle robots. See docs/quickstart.md, Honest notes.
  info "projecting status.phase=Idle on sim-robot-002/003 (SHORTCUT — the control plane does not do this)"
  for r in sim-robot-002 sim-robot-003; do
    kubectl patch "robot/$r" -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"phase":"Idle","batteryPercent":85}}'
  done
  local pflog; pflog="$(mktemp)"
  kubectl -n "$MANAGER_NS" port-forward service/swarmada-controlstream "${CS_PORT}:9443" >"$pflog" 2>&1 &
  PIDS+=($!)
  for _ in $(seq 1 15); do grep -q "Forwarding from" "$pflog" && break; sleep 1; done
  ( cd "$REPO_ROOT"
    exec env PYTHONPATH="proto${PYTHONPATH:+:$PYTHONPATH}" \
      SWARMADA_SIM_ESTOP_ACK_DELAY_MS="$ESTOP_ACK_DELAY_MS" \
      "$PY" adapters/simulation/sim_adapter.py \
      --endpoint "127.0.0.1:${CS_PORT}" --namespace "$NS" --robot-id "$LIVE_ROBOT" \
      --zone warehouse-a --vendor simulation --scenario full-surface
  ) >"$ADAPTER_LOG" 2>&1 &
  PIDS+=($!)
  info "adapter launched (estop-ack delay ${ESTOP_ACK_DELAY_MS}ms); log: $ADAPTER_LOG"
}

port_forward_metrics() {
  step "4 — Port-forward /metrics"
  kubectl -n "$MANAGER_NS" port-forward "deployment/$MANAGER_DEPLOY" "${METRICS_PORT}:8080" \
    >/tmp/demotest-metrics-pf.log 2>&1 &
  PIDS+=($!)
  for _ in $(seq 1 15); do
    curl -sf "http://127.0.0.1:${METRICS_PORT}/metrics" >/dev/null 2>&1 && break; sleep 1
  done
}

scheduler_stall_fixture() {
  # Assignment-latency fixture. A Normal task requires an uncommon capability; a
  # DEDICATED Idle robot lacks it, so the task stays Pending. After the stall we grant
  # the capability → the scheduler assigns it and records a >60s Pending→Assigned
  # latency (>60s bucket → SwarmadaSchedulerAssignmentLatencyHigh fires). A dedicated
  # robot is required because the three sample robots are busy/nondeterministically
  # placed — the task would otherwise never land on an Idle capable robot.
  step "6 — Scheduler assignment-latency fixture (dedicated robot, phase=Idle PROJECTED, ~${SCHED_STALL_SECONDS}s)"
  info "demotest-sched-robot's status.phase=Idle is patched by hand here and again after the"
  info "capability grant — same shortcut as the pool above (no Discovered->Idle owner exists)."
  kubectl apply -f - <<EOF >/dev/null 2>&1 || true
apiVersion: swarmada.io/v1
kind: Robot
metadata: {name: demotest-sched-robot, namespace: $NS}
spec:
  manufacturer: Simulated
  model: SchedCanary
  adapter: {name: sim-fleet-adapter, version: "0.1.0"}
  zone: warehouse-a
  capabilities: [{name: navigation, type: hardware-native, pauseable: false}]
EOF
  kubectl patch robot/demotest-sched-robot -n "$NS" --subresource=status --type=merge \
    -p '{"status":{"phase":"Idle","batteryPercent":90}}' >/dev/null 2>&1 || true
  kubectl apply -f - <<EOF >/dev/null 2>&1 || true
apiVersion: swarmada.io/v1
kind: FleetAction
metadata: {name: demotest-slow-task, namespace: $NS}
spec:
  type: Navigate
  zone: warehouse-a
  priority: Normal
  requiredCapabilities: [demotest-uncommon-cap]
EOF
  (
    sleep "$SCHED_STALL_SECONDS"
    kubectl patch robot/demotest-sched-robot -n "$NS" --type=json \
      -p '[{"op":"add","path":"/spec/capabilities/-","value":{"name":"demotest-uncommon-cap","type":"hardware-native","pauseable":false}}]' >/dev/null 2>&1 || true
    kubectl patch robot/demotest-sched-robot -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"phase":"Idle"}}' >/dev/null 2>&1 || true
  ) &
  PIDS+=($!)
}

run_assertions() {
  step "7 — Run the assertion driver (poll CRDs + evaluate §9.3.8 alerts + RA-1)"
  local dur="${DEMOTEST_DURATION:-$((SCHED_STALL_SECONDS + 30))}" rc=0
  "$PY" examples/full-surface-demo/demo_test.py \
    --namespace "$NS" --context "$CTX" \
    --metrics-url "http://127.0.0.1:${METRICS_PORT}/metrics" \
    --duration "$dur" --poll-interval "${DEMOTEST_POLL_INTERVAL:-0.5}" --require-clear gauge || rc=1

  step "7.5 — Assert EdgeStream coverage (third RPC service)"
  if [ "${EDGE_SKIPPED:-0}" = "1" ]; then
    info "⏭  EdgeStream coverage skipped (cmd/edge not in this checkout)"
  elif "$PY" -c "import sys; sys.path.insert(0,'examples/full-surface-demo'); \
from demo_test import edge_estop_confirmed; \
sys.exit(0 if edge_estop_confirmed(open('$ADAPTER_LOG').read(), '$LIVE_ROBOT') else 1)"; then
    info "✅ EdgeStream: confirmed edge-issued estop observed for $LIVE_ROBOT"
  else
    echo "❌ EdgeStream never confirmed an edge estop for $LIVE_ROBOT (see $ADAPTER_LOG / $EDGE_LOG)" >&2
    rc=1
  fi
  [[ "$rc" -ne 0 ]] && dump_diagnostics
  return "$rc"
}

# dump_diagnostics: printed before teardown when the gate fails, so a CI run (which
# deletes the cluster) still captures what to fix — task placement and which §9.3.8
# metrics are actually emitted with what labels.
dump_diagnostics() {
  step "DIAGNOSTICS (gate failed) — capture before teardown"
  # The dispatch gate's own inputs. Without these the failure "no robot reached InProgress" is
  # indistinguishable between "conformance never verified", "phase was not Connected at dispatch
  # time" and "the fixture's capabilities do not match" — three different fixes.
  echo "---- fleetadapter (ADR-0032 dispatch gate inputs) ----" >&2
  kubectl get fleetadapter -n "$NS" -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,CONF:.status.conformance,CONTRACT:.status.conformanceContractVersion,ROBOTS:.status.connectedRobots,MSG:.status.message >&2 2>/dev/null || true
  echo "---- why candidates were withheld from dispatch (adapter_readiness) ----" >&2
  kubectl logs -n swarmada-system deploy/swarmada-controller-manager --tail=500 2>/dev/null \
    | grep -E "not fit for dispatch|withheld|no spec.adapter.name|capability" | tail -20 >&2 || true
  echo "---- robots + fleetactions ----" >&2
  kubectl get robots,fleetactions -n "$NS" -o wide >&2 2>&1 || true
  echo "---- robot phases + estopState ----" >&2
  kubectl get robots -n "$NS" -o "custom-columns=NAME:.metadata.name,PHASE:.status.phase,ESTOP:.status.estopState,TASK:.status.assignedTask,BATT:.status.batteryPercent" >&2 2>&1 || true
  echo "---- §9.3.8 metrics actually emitted ----" >&2
  curl -s "http://127.0.0.1:${METRICS_PORT}/metrics" 2>/dev/null | grep -E \
    'fleet_adapter_connected|fleetactions_by_phase|robots_by_phase|robots_in_estop|frames_received|status_writes|dropped_frames|tsdb_write_errors|estop_latency_violations|assignment_latency_seconds_count' \
    | grep -v '^#' >&2 2>&1 || true
  echo "---- adapter log (estop/safety/edge lines) ----" >&2
  grep -iE "estop|safety|edge|EDGE_ESTOP" "$ADAPTER_LOG" 2>/dev/null | tail -n 20 >&2 2>&1 || true
  echo "---- manager log (estop / adapter-presence / dispatcher) ----" >&2
  kubectl -n "$MANAGER_NS" logs "deployment/$MANAGER_DEPLOY" --tail=400 2>/dev/null \
    | grep -iE "estop|TriggerEstop|SafetyStream|adapter.*connect|presence|dispatch|lease" \
    | tail -n 30 >&2 2>&1 || true
}

main() {
  narrate "What this is" <<'EOF'
demo-test is the headless CI gate for the full-surface sim harness. It brings up a
real Swarmada control plane on a throwaway kind cluster, drives the full-surface
scenario live through the reference sim adapter, applies deterministic fault fixtures,
asserts every targeted RobotPhase / FleetAction phase / estop-state and the §9.3.8 alert
expressions + RA-1, then deletes the cluster. This is the WALKTHROUGH view — same run,
narrated. (Set DEMOTEST_NO_PROMPT=1 to skip the Enter prompts; unset DEMOTEST_WALKTHROUGH
for the silent CI view.)
EOF
  pause_step "Enter to begin"

  narrate "Step 0-4 — bring up the cluster + control plane" <<'EOF'
Creates a kind cluster, builds/loads the controller image, deploys via the
quickstart-dev overlay, applies the sample fleet, and stands up the failing-TSDB
SwarmadaConfig, the FleetAdapter resource, the canary, and the edge node.
Expect: ~2-4 min (image build dominates). How to watch: in another terminal,
  kubectl get pods -A -w
EOF
  pause_step "Enter to bring up the cluster"
  preflight
  bring_up
  apply_fleet_and_fixtures
  launch_edge_node          # before the adapter, so the endpoint is live when it dials
  launch_live_adapter
  port_forward_metrics

  narrate "Steps 6-7 — fixtures + assertions (concurrent)" <<EOF
Now the timed fixtures fire CONCURRENTLY with the assertion poller (they must overlap
polling — a fixture that finished before polling started would read as "never observed").
What runs and what to expect:
  • scheduler stall  — a Normal task stays Pending ~${SCHED_STALL_SECONDS}s, then a
                       dedicated Idle robot gains the capability → >60s assignment
                       latency → SchedulerAssignmentLatencyHigh fires.
  • estop walk       — $LIVE_ROBOT estopState Stopping→Stopped(hold >15s)→Normal →
                       robots_in_estop gauge → RobotEstopUncleared fires then clears.
  • adapter walk     — FleetAdapter Connected→Disconnected→Connected → fleet_adapter_
                       connected 1→0→1 → AdapterDisconnected fires then clears.
  • canary walk      — the taskless canary is walked through Charging/Error/Offline/
                       Assigned + task Pending/InProgress/Revoking (projected).
  • edge trip        — the safety-input file flips → headless estop over EdgeStream →
                       adapter confirms (EDGE_ESTOP_CONFIRMED).
How to watch live (another terminal):
  watch -n1 kubectl get robots,fleetactions -n $NS
  curl -s localhost:${METRICS_PORT}/metrics | grep -E 'by_phase|in_estop|adapter_connected'
Note: metrics gauges update on a 15s sweep, so allow a few seconds after each transition.
Runs ~$((SCHED_STALL_SECONDS + 30))s, then prints ✅/❌ and deletes the cluster.
EOF
  pause_step "Enter to run the fixtures + assertions"
  scheduler_stall_fixture      # bg
  project_estop_states         # bg: estopState walk (real path is mTLS-gated; see README)
  project_adapter_phase        # bg: FleetAdapter connectivity walk (AdapterDisconnected fire+clear)
  walk_canary_phases           # bg: projected phase/task-phase walk on the taskless canary
  trip_edge_safety &           # bg: headless estop over EdgeStream (C8)
  run_assertions               # combined gate (CRD/metrics/RA-1 + EdgeStream); trap deletes the cluster
}

main "$@"
