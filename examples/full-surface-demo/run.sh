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
# TRANSPORT: this gate deploys config/overlays/quickstart-mtls, NOT the plaintext
# quickstart-dev overlay, and it requires cert-manager (installed automatically) plus
# network access to fetch it. This is not a hardening nicety — it is load-bearing. The
# adapter's identity comes only from its client-certificate SAN; over plaintext
# internal/command/dispatcher.go refuses to register the stream, so no command can be
# pushed back, ADR-0019's validate_action is unreachable for every candidate robot, and
# NOTHING is ever assigned. Under the old plaintext overlay this gate could not reach
# Assigned/InProgress or record an assignment-latency sample in any checkout.
#
# What is REAL vs PROJECTED (honest, matching examples/warehouse-quickstart):
#   - REAL from the live adapter + control plane: status.hardware degrade→fail→recover,
#     FleetAction Revoking (comms drop → lease expiry), Assigned/InProgress (scheduler),
#     the adapter-connected gauge, the reconnect counter, and RA-1 (status_writes vs
#     frames_received). RobotPhase Discovered and Offline are also real, produced by
#     discovered_offline_fixture — the Robot controller writes both. They need a fixture
#     because a HEALTHY fleet never sits in either phase; before the transport was fixed
#     they were supplied by the breakage itself, which is not coverage.
#   - PROJECTED via kubectl --subresource=status (the RA-1 anti-pattern; RFC-0001
#     §9.1.3 reserves status to controllers): RobotPhase Idle-bootstrap,
#     Charging, Error, Maintenance, FleetAction Succeeded, and status.estopState
#     Stopping→Stopped→Normal. The estop states are projected because this scenario
#     issues no real TriggerEstop, not because the transport cannot carry one — see
#     project_estop_states. Idle-bootstrap applies ONLY to robots with no Fleet
#     Adapter: the Robot reconciler owns Discovered->Idle and advances a robot from
#     fresh adapter liveness (ADR-0029, RFC-0001 §9.3.3), so a robot nothing is
#     connected to never goes live and never becomes schedulable. The live robot
#     advances on its own. demo_test.py asserts these were
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
#   CERT_MANAGER_VERSION  cert-manager release to install (default: v1.16.2, pinned so a
#                         run does not change under you between invocations)
#
# EDGE SURFACE: cmd/edge is not published in this repository, so a public checkout skips
# every EdgeStream (C8) beat — coherently: the zone advertises no spec.edgeNode, no edge
# node is launched, the safety input is not tripped, and the EdgeStream assertion is not
# run. The closing COVERAGE SUMMARY names each skipped assertion on EVERY run. Nothing is
# weakened to get green: the checks are skipped, and they run where cmd/edge exists.
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

# EDGE_AVAILABLE is resolved ONCE, in preflight, before anything advertises or
# consumes the edge surface. It gates every edge-dependent step in this run:
# the zone's spec.edgeNode (step 2), the edge node itself (step 2.5), the
# safety-input trip (step 6.5), and the EdgeStream assertion (step 7.5). One
# decision with four consumers, because the previous shape — step 2.5 deciding
# on its own — left steps 2 and 6.5 advertising and tripping an edge node that
# was never launched.
EDGE_AVAILABLE=0

# mTLS material for the LIVE adapter. cert-manager issues the client keypair;
# the SAN in it is what the control plane treats as the adapter's NAME, so it
# must match the FleetAdapter resource this run creates (sim-fleet-adapter).
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.16.2}"
ADAPTER_TLS_DIR="/tmp/demotest-${CLUSTER}-adapter-tls"

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

  # Resolve the edge surface ONCE, here, before any step advertises or consumes it.
  # cmd/edge is not published in this repository (it is held for a later release), so
  # a public checkout runs without it and the EdgeStream beats are skipped COHERENTLY
  # — see EDGE_AVAILABLE above and print_coverage_summary below, which names the
  # skipped assertions on every run.
  if [ -d "$REPO_ROOT/cmd/edge" ]; then
    EDGE_AVAILABLE=1
    info "cmd/edge present — EdgeStream coverage (C8) is IN scope for this run"
  else
    EDGE_AVAILABLE=0
    info "cmd/edge absent — EdgeStream coverage (C8) is OUT of scope for this run"
  fi
  info "ok"
}

# install_cert_manager / wait_for_adapter_client_cert are ported from
# examples/warehouse-quickstart/run.sh (its LIVE path needs the identical material).
# Duplicated rather than shared, matching the convention config/overlays/quickstart-mtls
# already sets for its copied manifests: these examples are self-contained so a reader
# can follow one run.sh top to bottom. Keep the copies in step if the originals change.
install_cert_manager() {
  if kubectl get deploy -n cert-manager cert-manager-webhook >/dev/null 2>&1; then
    info "cert-manager already installed ✓"
  else
    info "installing cert-manager ${CERT_MANAGER_VERSION} (issues the ControlStream + adapter certs)…"
    kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
  fi
  info "waiting for cert-manager to become Available…"
  for d in cert-manager cert-manager-cainjector cert-manager-webhook; do
    kubectl -n cert-manager rollout status "deployment/$d" --timeout=180s
  done
  # The CA injector and webhook are Available before their APIService is reliably
  # serving; applying a Certificate too early fails with a webhook connection
  # refused. Retry briefly rather than racing.
  local waited=0
  until kubectl get crd certificates.cert-manager.io >/dev/null 2>&1 || [ "$waited" -ge 60 ]; do
    sleep 2; waited=$((waited + 2))
  done
}

wait_for_adapter_client_cert() {
  info "waiting for cert-manager to issue the adapter client certificate…"
  local waited=0
  until kubectl -n "$MANAGER_NS" get secret sim-fleet-adapter-client-tls >/dev/null 2>&1; do
    [ "$waited" -ge 120 ] && fail "cert-manager never issued sim-fleet-adapter-client-tls"
    sleep 2; waited=$((waited + 2))
  done
  kubectl -n "$MANAGER_NS" wait --for=condition=Ready certificate/sim-fleet-adapter-client --timeout=120s >/dev/null 2>&1 || true
  rm -rf "$ADAPTER_TLS_DIR"; mkdir -p "$ADAPTER_TLS_DIR"; chmod 700 "$ADAPTER_TLS_DIR"
  kubectl -n "$MANAGER_NS" get secret sim-fleet-adapter-client-tls \
    -o jsonpath='{.data.tls\.crt}' | base64 -d > "$ADAPTER_TLS_DIR/tls.crt"
  kubectl -n "$MANAGER_NS" get secret sim-fleet-adapter-client-tls \
    -o jsonpath='{.data.tls\.key}' | base64 -d > "$ADAPTER_TLS_DIR/tls.key"
  kubectl -n "$MANAGER_NS" get secret sim-fleet-adapter-client-tls \
    -o jsonpath='{.data.ca\.crt}' | base64 -d > "$ADAPTER_TLS_DIR/ca.crt"
  chmod 600 "$ADAPTER_TLS_DIR"/*
  [ -s "$ADAPTER_TLS_DIR/tls.crt" ] && [ -s "$ADAPTER_TLS_DIR/tls.key" ] && [ -s "$ADAPTER_TLS_DIR/ca.crt" ] \
    || fail "adapter client certificate material is incomplete in $ADAPTER_TLS_DIR"
  info "adapter client certificate ready ✓ ($ADAPTER_TLS_DIR)"
}

bring_up() {
  step "1 — Create kind cluster + deploy control plane (quickstart-mtls overlay)"
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

  # This gate REQUIRES a mutually-authenticated ControlStream, not merely an encrypted
  # one, and it cannot use the plaintext quickstart-dev overlay it used to deploy.
  #
  # The adapter's identity comes ONLY from its client certificate's SAN. Over plaintext
  # IdentityFromContext yields Verified=false, and internal/command/dispatcher.go's
  # RegisterStream refuses to record an unverified stream ("nothing may be pushed to an
  # unauthenticated stream"). The Dispatcher's stream table therefore stays EMPTY, so
  # ADR-0019's validate_action probe returns ErrUnreachable for every candidate robot and
  # actionServableBy drops each one — fail-closed and deliberate. Zero candidates means no
  # FleetAction ever reaches Assigned, so no robot reaches InProgress and
  # ObserveAssignmentLatency (its only call site is the Assigned transition) is never
  # called, which makes SwarmadaSchedulerAssignmentLatencyHigh unfirable.
  #
  # That is one cause for both of this gate's long-standing failures. It is structural,
  # not a timing flake: config/overlays/quickstart-mtls says so in its own header, and
  # examples/warehouse-quickstart/run.sh's LIVE path already deploys this overlay for
  # exactly this reason. Nothing here is projected to work around it.
  install_cert_manager
  info "deploying via config/overlays/quickstart-mtls (DEV/DEMO ONLY)"
  kubectl apply -k config/overlays/quickstart-mtls
  kubectl -n "$MANAGER_NS" rollout status "deployment/$MANAGER_DEPLOY" --timeout=180s
  wait_for_adapter_client_cert
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
  # physicalBounds strictly CONTAINS the origin the adapter tees (x=0,y=0), so the
  # headless estop comes from the tripped safety input — deterministically — not an
  # accidental zone-boundary breach. It is patched unconditionally: a leaf zone with no
  # polygon is NOT ZoneReady (internal/controller/zone_controller.go, computeZoneConditions).
  #
  # spec.edgeNode is advertised ONLY when an edge node will actually be launched. The zone
  # used to advertise it unconditionally, which left a public checkout pointing at an
  # endpoint nothing ever listened on: the adapter took the address from its register-time
  # RegisterAck, dialled it, and its _edge_loop thread died on connection-refused
  # (adapters/simulation/sim_adapter.py, EdgeStream). That crash is confined to the daemon
  # thread — telemetry, ControlStream and SafetyStream are unaffected — but advertising a
  # feed that cannot exist is a false statement about the zone, so it is not made.
  local zone_patch="{\"spec\":{
    \"physicalBounds\":{\"floor\":0,\"polygon\":[
      {\"x\":-5.0,\"y\":-5.0},{\"x\":25.0,\"y\":-5.0},{\"x\":25.0,\"y\":25.0},{\"x\":-5.0,\"y\":25.0}]}"
  if [ "$EDGE_AVAILABLE" = "1" ]; then
    # Advertised BEFORE the adapter registers: the adapter dials the endpoint from its
    # register-time RegisterAck, so a later patch would be missed until the comms-drop
    # re-handshake.
    zone_patch="${zone_patch},\"edgeNode\":{\"address\":\"127.0.0.1:${EDGE_PORT}\"}"
  fi
  zone_patch="${zone_patch}}}"
  kubectl patch fleetzone/warehouse-a -n "$NS" --type=merge -p "$zone_patch" 2>/dev/null || \
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

  if [ "$EDGE_AVAILABLE" = "1" ]; then
    info "fleet applied; camera gated; failing-TSDB SwarmadaConfig applied; edge node advertised; FleetAdapter + canary created"
  else
    info "fleet applied; camera gated; failing-TSDB SwarmadaConfig applied; edge node NOT advertised (cmd/edge absent); FleetAdapter + canary created"
  fi
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
  if [ "$EDGE_AVAILABLE" != "1" ]; then
    info "⏭  cmd/edge not present in this checkout — skipping EdgeStream coverage (edge node held for a later release)"
    info "   the zone advertises no spec.edgeNode and step 6.5 will not trip the safety input;"
    info "   the closing coverage summary names every assertion this costs."
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
  # Project estopState Stopping→Stopped→Normal on the live robot.
  #
  # This run now deploys the mTLS overlay, so the adapter HAS a verified identity and the
  # dispatcher can route a command to sim-fleet-adapter — the transport reason this beat
  # used to give ("insecure mode gives the adapter stream an empty identity") no longer
  # applies. What is still missing is a TRIGGER: nothing in this scenario annotates
  # swarmada.io/estop-triggered or applies a ZoneEstop, so no real TriggerEstop is ever
  # issued. The states are therefore still projected, and still honestly labeled.
  #
  # Converting this beat to the real SafetyStream path is now unblocked and is worth doing
  # on its own — it is the one change that would also reach
  # swarmada_estop_latency_violations_total (EstopLatencySLOBreach), which projection
  # cannot produce and which remains a documented known-gap (see README, demo_test.py).
  # It is deliberately NOT bundled into the overlay switch: this beat drives
  # robots_in_estop and RobotEstopUncleared today, and rewriting it in the same change
  # that moved the transport would leave two suspects if it broke.
  # Dwell each metric-driving state LONGER than the 15s metrics-sweeper interval
  # (internal/controller/metrics_sweeper.go defaultSweepInterval) — the sweeper
  # recomputes robots_in_estop every 15s, so a shorter window is never sampled and the
  # gauge (hence RobotEstopUncleared) never moves even though the CRD state is observed.
  step "6.6 — Project estopState Stopping→Stopped→Normal on $LIVE_ROBOT (no estop trigger in this scenario; see above)"
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
  # Guarded by the same EDGE_AVAILABLE decision as steps 2, 2.5 and 7.5. This used to trip
  # unconditionally: it slept ~13s and wrote the safety-input file whether or not an edge
  # node existed to read it, so a skipped edge node still cost the run its wall-clock and
  # still printed a step banner for work that could not happen.
  if [ "$EDGE_AVAILABLE" != "1" ]; then
    step "6.5 — Trip the edge safety input (SKIPPED — cmd/edge absent)"
    info "⏭  no edge node is running, so there is nothing to trip and no estop to confirm"
    return 0
  fi
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
  # status.phase is controller-owned (RFC-0001 §9.1.3, RA-1). These two robots have no
  # Fleet Adapter, so no liveness is ever reported for them and the Robot reconciler's
  # Discovered->Idle advance (ADR-0029) never fires. Scheduler filter 1 admits only Idle
  # robots. The patch substitutes for an adapter. See docs/quickstart.md, Honest notes.
  info "projecting status.phase=Idle on sim-robot-002/003 (no adapter — simulation stand-in)"
  for r in sim-robot-002 sim-robot-003; do
    kubectl patch "robot/$r" -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"phase":"Idle","batteryPercent":85}}'
  done
  local pflog; pflog="$(mktemp)"
  kubectl -n "$MANAGER_NS" port-forward service/swarmada-controlstream "${CS_PORT}:9443" >"$pflog" 2>&1 &
  PIDS+=($!)
  for _ in $(seq 1 15); do grep -q "Forwarding from" "$pflog" && break; sleep 1; done
  ( cd "$REPO_ROOT"
    # The adapter presents its cert-manager-issued CLIENT certificate. Its SAN
    # (sim-fleet-adapter.warehouse-a.svc.cluster.local) is the identity the control plane
    # reads — it must match the FleetAdapter resource this run creates, because that is the
    # key internal/command/dispatcher.go stores the stream under. Without it the stream is
    # never registered and validate_action is unreachable for every candidate robot.
    #
    # --tls-server-name: the port-forward means we dial 127.0.0.1, which no server SAN
    # covers. Verify against the name the server certificate actually carries rather than
    # disabling verification.
    #
    # PYTHONUNBUFFERED: without it Python block-buffers stdout when it is a file, so
    # $ADAPTER_LOG stays EMPTY until the process exits — and teardown SIGTERMs it,
    # discarding the buffer. Every diagnostic dump_diagnostics prints from the adapter log
    # would be blank exactly when it is needed.
    exec env PYTHONUNBUFFERED=1 PYTHONPATH="proto${PYTHONPATH:+:$PYTHONPATH}" \
      SWARMADA_SIM_ESTOP_ACK_DELAY_MS="$ESTOP_ACK_DELAY_MS" \
      "$PY" adapters/simulation/sim_adapter.py \
      --endpoint "127.0.0.1:${CS_PORT}" --namespace "$NS" --robot-id "$LIVE_ROBOT" \
      --zone warehouse-a --vendor simulation --scenario full-surface \
      --tls-ca "$ADAPTER_TLS_DIR/ca.crt" \
      --tls-cert "$ADAPTER_TLS_DIR/tls.crt" \
      --tls-key "$ADAPTER_TLS_DIR/tls.key" \
      --tls-server-name "swarmada-controlstream.${MANAGER_NS}.svc"
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

# discovered_offline_fixture: drive REAL Discovered and Offline transitions inside the
# assertion window. Both phases are written BY the control plane here — neither is projected.
#
# They need a fixture at all because fixing the transport removed the accident that used to
# supply them. Under the old plaintext overlay the adapter had no verified identity, so
# telemetry never resolved to a robot (its metrics carried EMPTY adapter/namespace labels) and
# the liveness prober could never reach anything: every robot sat in Discovered forever and
# then fell to Offline because no heartbeat could be confirmed. The gate observed both phases
# and called it coverage. It was observing a broken transport, not a lifecycle.
#
# With a working ControlStream the fleet advances Discovered→Idle→Assigned→InProgress and
# stays live, so both phases have to be produced deliberately and honestly:
#
#   Discovered — a Robot created INSIDE the poll window. The Robot controller writes
#     phase=Discovered on first observation (robot_controller.go, the phase=="" branch) and
#     nothing advances it, because no component owns Discovered→Idle without adapter liveness.
#
#   Offline — a Robot bound to demotest-idle-adapter (which has NO ControlStream, so nothing
#     projects liveness onto it) whose status.connectivity.lastSeenAt is stamped an hour in the
#     past. The controller then does the real §9.6.3.2 work: it sees stale telemetry, tries
#     three HeartbeatRequests five seconds apart, gets ErrUnreachable for each, and declares
#     Offline itself. The ONLY projected value is lastSeenAt — an INPUT. The decision, the
#     confirming exchange and the phase write are all the control plane's.
discovered_offline_fixture() {
  step "6.9 — Drive REAL Discovered + Offline transitions inside the assertion window"
  info "demotest-discovered-robot: created during the poll window; the Robot controller writes Discovered"
  info "demotest-offline-robot: stale lastSeenAt + an adapter with no stream → controller confirms and marks Offline"
  (
    # Created a few seconds INTO the poll window: a robot created earlier would already have
    # been observed (and, if live, advanced) before polling began.
    sleep "${DISCOVERED_FIXTURE_AFTER:-6}"
    kubectl apply -f - <<EOF >/dev/null 2>&1 || true
apiVersion: swarmada.io/v1
kind: Robot
metadata: {name: demotest-discovered-robot, namespace: $NS}
spec:
  manufacturer: Simulated
  model: DiscoveryCanary
  adapter: {name: demotest-idle-adapter, version: "0.1.0"}
  zone: warehouse-a
  capabilities: [{name: demotest-discovered-cap, type: hardware-native, pauseable: false}]
---
apiVersion: swarmada.io/v1
kind: Robot
metadata: {name: demotest-offline-robot, namespace: $NS}
spec:
  manufacturer: Simulated
  model: OfflineCanary
  adapter: {name: demotest-idle-adapter, version: "0.1.0"}
  zone: warehouse-a
  capabilities: [{name: demotest-offline-cap, type: hardware-native, pauseable: false}]
EOF
    # Let the controller write Discovered first, so the poller observes that phase before this
    # robot starts its walk to Offline.
    sleep "${OFFLINE_FIXTURE_DWELL:-4}"
    # An hour in the past beats any namespace connectivityOfflineThresholdSeconds, so the
    # fixture does not depend on the default staying what it is today. Computed with python
    # rather than `date -d`/`date -v`, whose relative-time flags differ between GNU and BSD —
    # this gate runs on both (Linux CI, macOS dev).
    local stale
    stale="$("$PY" -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ"))')"
    kubectl patch robot/demotest-offline-robot -n "$NS" --subresource=status --type=merge \
      -p "{\"status\":{\"connectivity\":{\"lastSeenAt\":\"$stale\"}}}" >/dev/null 2>&1 || true
    # 3 attempts x 5s of confirming HeartbeatRequests before the controller commits to Offline.
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
  if [ "$EDGE_AVAILABLE" != "1" ]; then
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
  # Printed on EVERY run, pass or fail, and printed LAST so it is the closing word. A gate
  # that passes by not running is indistinguishable from one that found nothing wrong.
  print_coverage_summary "$rc"
  return "$rc"
}

# print_coverage_summary: state what this run did NOT exercise. Unconditional by design —
# the failure mode it exists to prevent is a green gate whose green is partly silence.
# Print it whether the run passed or failed, and name the assertions the skip costs
# rather than a bare "skipped".
print_coverage_summary() {
  local rc="${1:-0}"
  step "COVERAGE SUMMARY — what this run did and did not exercise"
  if [ "$EDGE_AVAILABLE" = "1" ]; then
    echo "    ✅ every surface this gate covers was exercised, including EdgeStream (C8)."
  else
    echo "    ⏭  NOT EXERCISED in this checkout — cmd/edge is not published in this repository:"
    echo "         • C8 EdgeStream end-to-end (EdgeService / AdapterEdgeMessage), the third RPC service"
    echo "         • the edge-issued headless estop and its confirmed EstopAck (step 6.5 / 7.5)"
    echo "         • FleetZone.spec.edgeNode + status.edgeFeedUnavailable, and the Zone Controller's"
    echo "           EdgeFeedUnavailable / EdgeFeedRestored events"
    echo "       Everything else below the edge surface — dispatch, telemetry, estop projection,"
    echo "       the §9.3.8 alerts and RA-1 — WAS asserted. No assertion was removed to get here:"
    echo "       the EdgeStream checks are skipped, not weakened, and they run in a checkout"
    echo "       that has cmd/edge."
  fi
  # Also name the gap that persists even WITH an edge node, so a green run never reads as
  # "everything in §9.3.8 was proven".
  echo "    ⏭  NOT EXERCISED in any checkout: swarmada_estop_latency_violations_total"
  echo "       (EstopLatencySLOBreach) — the estop states are projected, so the counter that"
  echo "       needs a real SafetyStream estop is never incremented. Documented known-gap."
  if [ "$rc" -eq 0 ]; then
    echo "    verdict: PASS on what it asserted, with the gaps above stated."
  else
    echo "    verdict: FAIL — see the ❌ lines above; the gaps listed here are NOT the failure."
  fi
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
  # Why candidates were withheld. TWO independent gates can empty the candidate set and they
  # speak different vocabularies, so both must be grepped:
  #   ADR-0032 (adapter_readiness.go)      — "not fit for dispatch", "withheld", capability/adapter
  #   ADR-0019 (actionServableBy)          — "validate_action unreachable; skipping candidate"
  # This block used to match only the first set. When the gate failed because the ControlStream
  # was unregistered — the ADR-0019 path — it printed NOTHING, and the run looked like an
  # unexplained "no eligible robot". A diagnostic that is silent on a live failure mode is worse
  # than absent, because it reads as "checked, nothing there". Also grep the manager namespace
  # variable rather than a hardcoded one: this deployment lives in $MANAGER_NS.
  echo "---- why candidates were withheld from dispatch (ADR-0032 readiness + ADR-0019 validate_action) ----" >&2
  kubectl logs -n "$MANAGER_NS" "deploy/$MANAGER_DEPLOY" --tail=500 2>/dev/null \
    | grep -E "not fit for dispatch|withheld|no spec.adapter.name|capability|validate_action|skipping candidate|no ControlStream to adapter|no eligible robot" \
    | tail -20 >&2 || true
  # The ControlStream identity itself: an unverified adapter stream is never registered with the
  # command Dispatcher, which is what makes validate_action unreachable in the first place.
  echo "---- ControlStream identity (verified mTLS identity is required to push any command) ----" >&2
  kubectl logs -n "$MANAGER_NS" "deploy/$MANAGER_DEPLOY" --tail=500 2>/dev/null \
    | grep -E "ControlStream established|no verified mTLS identity|WITHOUT per-robot authorization" \
    | tail -10 >&2 || true
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
Creates a kind cluster, builds/loads the controller image, installs cert-manager,
deploys via the quickstart-mtls overlay, applies the sample fleet, and stands up
the failing-TSDB SwarmadaConfig, the FleetAdapter resource and the canary (plus
the edge node, in a checkout that has cmd/edge).
Why mTLS and not the plaintext quickstart-dev overlay: the adapter's identity comes
only from its client-certificate SAN, and the command Dispatcher refuses to register
an unverified stream. Over plaintext no command can be pushed back, so ADR-0019's
validate_action is unreachable for every candidate robot and NOTHING is ever
assigned — no InProgress, no assignment-latency sample, no latency alert.
Expect: ~3-5 min (image build dominates; cert-manager adds ~1 min). How to watch:
in another terminal,
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
  discovered_offline_fixture   # bg: REAL Discovered + Offline, written by the control plane
  trip_edge_safety &           # bg: headless estop over EdgeStream (C8)
  run_assertions               # combined gate (CRD/metrics/RA-1 + EdgeStream); trap deletes the cluster
}

main "$@"
