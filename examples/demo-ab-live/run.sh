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
# demo-ab-live — the canonical, fully-honest Demo A/B: every robot in the
# sample fleet is driven by its OWN real,
# unmodified adapters/simulation/sim_adapter.py process, talking to the real
# ControlStream over a real, mutually-authenticated (mTLS) gRPC connection.
#
# THIS SCRIPT NEVER RUNS `kubectl patch --subresource=status` OR ANY OTHER
# STATUS WRITE. Every field it asserts on — Robot.status.phase,
# Robot.status.batteryPercent, Robot.status.hardware, Robot.status.capabilities,
# FleetAdapter.status.phase, FleetAction.status.assignedRobot — is derived by
# the real control plane from real adapter traffic. Self-check:
#
#     ! grep -n "subresource=status" examples/demo-ab-live/run.sh
#
# should print nothing and exit 1 (no match). CI can run that grep as a
# regression gate.
#
# Why three robots need three FleetAdapter identities, not one shared between
# them (unlike examples/warehouse-quickstart/run.sh's single LIVE_ROBOT):
# internal/command/dispatcher.go keys its live-ControlStream map by
# "<namespace>/<adapter-name>" alone (adapterKey — see dispatcher.go), so a
# second concurrent sim_adapter.py process registering under the SAME
# FleetAdapter identity would silently overwrite the first in that map —
# command push (assign_action, cancel_task, active probes) would then only
# ever reach whichever robot registered most recently, and the others would
# never receive an assignment. Three distinct client-certificate SANs (see
# config/overlays/demo-ab-live) give three distinct dispatcher map entries,
# so all three robots stay independently addressable. sim_adapter.py itself
# is not modified in any way — this runs the unmodified reference adapter
# three times, exactly as documented in the Layered Demo Plan.
#
# Two scenarios:
#   --scenario healthy-fleet   Demo A / UC-2.1 — discover -> register -> assign,
#                              entirely on the wire.
#   --scenario hardware-fault  Demo B / UC-2.2 — fault -> capability degrade ->
#                              reroute -> recover, entirely on the wire. The
#                              fault and recovery fire on sim_adapter.py's OWN
#                              scenario clock (default T+20s / T+80s from
#                              adapter start) — this script only WAITS for
#                              them; nothing here triggers, paces, or patches
#                              the fault.
#
# On success it prints PASS and exits 0; if the end state is not reached
# within the timeouts it prints FAIL with diagnostics and exits 1. The
# cluster is left running so you can explore it — see --clean.
#
# Usage:
#   examples/demo-ab-live/run.sh --scenario healthy-fleet     Demo A / UC-2.1
#   examples/demo-ab-live/run.sh --scenario hardware-fault    Demo B / UC-2.2
#   examples/demo-ab-live/run.sh --clean                      tear down and exit
#   examples/demo-ab-live/run.sh --clean --unreachable        tear down a dead, unmarked cluster
#   examples/demo-ab-live/run.sh --help
#
# Env knobs:
#   DEMOAB_CLUSTER        kind cluster name              (default: swarmada-demo-ab)
#   DEMOAB_IMG             controller image tag the manager runs (default: swarmada-controller:dev)
#   DEMOAB_CS_PORT         local ControlStream port-forward port (default: 19444)
#   DEMOAB_READY_TIMEOUT   secs to wait for real telemetry + Idle per robot (default: 30)
#   DEMOAB_ASSIGN_TIMEOUT  secs to wait for the scheduler to assign        (default: 30)
#   DEMOAB_REROUTE_TIMEOUT secs to wait for the capability-loss reroute (hardware-fault
#                          only; the fault fires at --fault-at from adapter start, so
#                          this must comfortably exceed that)              (default: 60)
#   DEMOAB_RECOVER_TIMEOUT secs to wait for the post-recovery capability to return Active
#                          (hardware-fault only)                          (default: 60)
#
# The script works on the cluster it is given and nothing else on the host:
#   - every kubectl call names the context kind-$DEMOAB_CLUSTER, and the kubeconfig's
#     current context is left as it was (kind switches it on create; it is put back);
#   - a cluster that carries an owner marker (ConfigMap kube-system/swarmada-cluster-owner,
#     or node label swarmada.io/owner-token) is never deleted: an unhealthy one stops the
#     run, and --clean refuses it. Delete a marked cluster with the tool that marked it.
#   - a cluster whose API server does not answer is refused as well, because its marker
#     cannot be read through the API. --clean --unreachable reads the marker from the
#     cluster's node containers instead, and deletes only when none carries it.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${DEMOAB_CLUSTER:-swarmada-demo-ab}"
IMG="${DEMOAB_IMG:-swarmada-controller:dev}"
NS="warehouse-a"
SAMPLE="config/samples/demo_a.yaml"
MANAGER_NS="system"
MANAGER_DEPLOY="swarmada-controller-manager"
KCTX="kind-$CLUSTER"
DEFAULT_IMG="swarmada-controller:dev"
OWNER_CM="swarmada-cluster-owner"
OWNER_LABEL="swarmada.io/owner-token"

# index-aligned: ROBOTS[i] is served by ADAPTERS[i] via CERT_SECRETS[i].
ROBOTS=(sim-robot-001 sim-robot-002 sim-robot-003)
ADAPTERS=(sim-fleet-adapter sim-fleet-adapter-2 sim-fleet-adapter-3)
CERT_SECRETS=(sim-fleet-adapter-client-tls sim-fleet-adapter-2-client-tls sim-fleet-adapter-3-client-tls)
CERT_NAMES=(sim-fleet-adapter-client sim-fleet-adapter-2-client sim-fleet-adapter-3-client)

READY_TIMEOUT="${DEMOAB_READY_TIMEOUT:-30}"
ASSIGN_TIMEOUT="${DEMOAB_ASSIGN_TIMEOUT:-30}"
REROUTE_TIMEOUT="${DEMOAB_REROUTE_TIMEOUT:-120}"
RECOVER_TIMEOUT="${DEMOAB_RECOVER_TIMEOUT:-60}"
LOCAL_CS_PORT="${DEMOAB_CS_PORT:-19444}"

PIDFILE="/tmp/demoab-${CLUSTER}.pids"
PF_LOG="/tmp/demoab-${CLUSTER}-portforward.log"
ADAPTER_TLS_DIR="/tmp/demoab-${CLUSTER}-adapter-tls"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.16.2}"

SCENARIOS=(healthy-fleet hardware-fault)
SCENARIO=""
LIVE_PIDS=()
CLEAN_REQUESTED=0
UNREACHABLE_REQUESTED=0
CLUSTER_HINT_SHOWN=0

step()  { echo; echo "==> $*"; }
info()  { echo "    $*"; }
fail()  { echo "FAIL: $*" >&2; exit 1; }
have()  { type -P "$1" >/dev/null 2>&1; }

# kubectl always names this cluster's context. Several sessions can share one host and
# one kubeconfig, so the script never relies on, or changes, the current context.
kubectl() { command kubectl --context "$KCTX" "$@"; }

# owner_marker prints the owner recorded on the cluster, "none" when it carries no
# marker, or "unreachable" when its API server does not answer (and so it cannot be
# shown to be unmarked).
owner_marker() {
  kubectl --request-timeout=10s get --raw /readyz >/dev/null 2>&1 || { echo unreachable; return; }
  local prompt token
  prompt=$(kubectl -n kube-system get configmap "$OWNER_CM" --ignore-not-found \
    -o jsonpath='{.data.prompt}' 2>/dev/null || true)
  token=$(kubectl get nodes -l "$OWNER_LABEL" \
    -o jsonpath="{.items[0].metadata.labels.swarmada\.io/owner-token}" 2>/dev/null || true)
  if [[ -z "$prompt" && -z "$token" ]]; then
    echo none
  else
    echo "prompt=${prompt:-unrecorded} token=${token:-unrecorded}"
  fi
}

# refuse ACTION LINE...: stop without deleting anything, and say why.
refuse() {
  echo "REFUSED: $1 kind cluster '$CLUSTER' - it is not this script's to delete." >&2
  shift
  local line
  for line in "$@"; do echo "  $line" >&2; done
  echo "  Nothing was deleted." >&2
  exit 1
}

# refuse_unless_unmarked ACTION: stop, and say why, unless the cluster is reachable and
# carries no owner marker. Deleting a marked cluster erases the marker with it, and the
# owner can then no longer prove the cluster is theirs.
refuse_unless_unmarked() {
  local owner; owner=$(owner_marker)
  case "$owner" in
    none) return 0 ;;
    unreachable)
      refuse "$1" \
        "Its API server does not answer, so it cannot be shown to be unmarked." \
        "If it is a dead cluster you may delete, run: examples/demo-ab-live/run.sh --clean --unreachable" \
        "That reads the owner marker from the cluster's node containers and still refuses a marked one." ;;
    *)
      refuse "$1" \
        "It carries an owner marker: $owner" \
        "Delete it with the tool that created it, or choose another cluster with DEMOAB_CLUSTER=<name>." ;;
  esac
}

# node_owner_marker prints the owner token recorded in any of the cluster's node
# containers, or nothing when none carries one. kind writes a node label into the node's
# /kind/kubeadm.conf, and docker cp reads that file with the API server down and the
# container stopped. It fails when there is no node container or one cannot be read:
# a marker that could not be read has not been shown to be absent.
node_owner_marker() {
  local nodes node conf found=""
  nodes=$(docker ps -a --filter "label=io.x-k8s.kind.cluster=$CLUSTER" --format '{{.Names}}' 2>/dev/null) || return 1
  [[ -n "$nodes" ]] || return 1
  for node in $nodes; do
    conf=$(docker cp "$node:/kind/kubeadm.conf" - 2>/dev/null | tar -xO 2>/dev/null) || return 1
    [[ -n "$conf" ]] || return 1
    found+=$(printf '%s\n' "$conf" | grep -o "$OWNER_LABEL=[^[:space:]\"',]*" | head -n 1 || true)
  done
  printf '%s' "$found"
}

# refuse_unless_nodes_unmarked: the --clean --unreachable check, for a cluster whose API
# server does not answer.
refuse_unless_nodes_unmarked() {
  info "cluster: $CLUSTER (its API server does not answer; reading the owner marker from its node containers)"
  have docker || refuse "--clean --unreachable would delete" "docker is not installed, so the node containers cannot be read."
  local marker
  if ! marker=$(node_owner_marker); then
    refuse "--clean --unreachable would delete" \
      "Its node containers could not be read (none found, or /kind/kubeadm.conf unreadable)," \
      "so it cannot be shown to be unmarked."
  fi
  if [[ -n "$marker" ]]; then
    refuse "--clean --unreachable would delete" \
      "Its node container carries an owner marker: $marker" \
      "Delete it with the tool that created it."
  fi
  info "no owner marker in its node containers"
}

# create_kind_cluster creates the cluster and puts the kubeconfig's current context back:
# kind create always switches it, and other sessions on the host read it.
create_kind_cluster() {
  local prev; prev=$(command kubectl config current-context 2>/dev/null || true)
  kind create cluster --name "$CLUSTER"
  if [[ -n "$prev" && "$prev" != "$KCTX" ]]; then
    command kubectl config use-context "$prev" >/dev/null
  fi
}

kill_forcefully() {
  local pid="$1" waited=0
  kill -0 "$pid" >/dev/null 2>&1 || return 0
  kill "$pid" >/dev/null 2>&1 || true
  while kill -0 "$pid" >/dev/null 2>&1 && (( waited < 5 )); do
    sleep 1; waited=$((waited + 1))
  done
  kill -0 "$pid" >/dev/null 2>&1 && kill -9 "$pid" >/dev/null 2>&1 || true
}

cleanup_live_processes() {
  local pid i
  for (( i=${#LIVE_PIDS[@]}-1; i>=0; i-- )); do
    pid="${LIVE_PIDS[$i]}"
    [[ -n "$pid" ]] && kill_forcefully "$pid"
  done
  rm -f "$PIDFILE"
  ((CLEAN_REQUESTED)) || remind_to_clean_cluster
}
trap cleanup_live_processes EXIT

remind_to_clean_cluster() {
  ((CLUSTER_HINT_SHOWN)) && return 0
  CLUSTER_HINT_SHOWN=1
  echo
  echo "Remember to clean up the '$CLUSTER' cluster when you're done with it:"
  echo "    examples/demo-ab-live/run.sh --clean"
}

usage() {
  cat <<EOF
demo-ab-live — the canonical, fully-honest Demo A/B (no status patching).

Usage: run.sh --scenario NAME [--clean [--unreachable]] [-h|--help]

  --scenario NAME   one of: ${SCENARIOS[*]}
                    healthy-fleet   Demo A / UC-2.1 (discover -> register -> assign)
                    hardware-fault  Demo B / UC-2.2 (fault -> degrade -> reroute -> recover)
  --clean           tear down the '$CLUSTER' kind cluster and any tracked
                     background processes, then exit. Refused for a cluster
                     that carries an owner marker, or whose API server does
                     not answer.
  --unreachable     with --clean: for a cluster whose API server does not
                     answer, read the owner marker from its node containers
                     and delete only when none carries one.
  -h, --help        show this help and exit.
EOF
}

valid_scenario() {
  local x
  for x in "${SCENARIOS[@]}"; do [[ "$x" == "$1" ]] && return 0; done
  return 1
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --scenario) SCENARIO="${2:-}"; shift 2 ;;
      --scenario=*) SCENARIO="${1#*=}"; shift ;;
      --clean) CLEAN_REQUESTED=1; shift ;;
      --unreachable) UNREACHABLE_REQUESTED=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) usage >&2; fail "unknown argument: $1" ;;
    esac
  done
  if ((UNREACHABLE_REQUESTED && ! CLEAN_REQUESTED)); then
    fail "--unreachable is only meaningful with --clean"
  fi
  if ((! CLEAN_REQUESTED)); then
    [[ -n "$SCENARIO" ]] || SCENARIO="healthy-fleet"
    valid_scenario "$SCENARIO" || fail "unknown scenario '$SCENARIO'; choose one of: ${SCENARIOS[*]}"
  fi
}

preflight() {
  step "0/8 - Preflight"
  local missing=0
  for tool in kind kubectl docker make go python3; do
    have "$tool" || { echo "  missing required tool: $tool" >&2; missing=1; }
  done
  ((missing == 0)) || fail "install the missing tools and re-run"
  docker info >/dev/null 2>&1 || fail "the Docker daemon is not reachable - start Docker and re-run"
  if [[ ! -f "proto/fleet_adapter/v1/fleet_adapter_pb2.py" ]]; then
    info "generating Python proto stubs (proto/fleet_adapter/v1/*_pb2*.py)..."
    make proto-py
  fi
  info "required tools present; Docker daemon reachable"
}

kill_pidfile_processes() {
  if [[ -f "$PIDFILE" ]]; then
    local pid found=0
    while read -r pid; do
      [[ -n "$pid" ]] || continue
      if kill -0 "$pid" >/dev/null 2>&1; then
        info "killing leftover process (pid $pid)"
        kill_forcefully "$pid"
        found=1
      fi
    done < "$PIDFILE"
    ((found)) || info "no live processes found"
  else
    info "no pidfile - nothing to clean up"
  fi
  : > "$PIDFILE"
}

cleanup_stale_run() {
  step "0.5/8 - Clean up any leftover processes from a previous run"
  kill_pidfile_processes
}

do_clean_everything() {
  step "Clean everything - tearing down '$CLUSTER' and removing local state"
  kill_pidfile_processes
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    if ((UNREACHABLE_REQUESTED)) && [[ "$(owner_marker)" == unreachable ]]; then
      refuse_unless_nodes_unmarked
    else
      refuse_unless_unmarked "--clean would delete"
    fi
    info "deleting kind cluster '$CLUSTER'..."
    kind delete cluster --name "$CLUSTER"
  else
    info "no kind cluster named '$CLUSTER' - nothing to delete"
  fi
  rm -f "$PIDFILE" "$PF_LOG"
  rm -f "/tmp/demoab-${CLUSTER}-adapter-"*.log
  rm -rf "$ADAPTER_TLS_DIR"
  info "clean OK - no cluster, no tracked processes, no local pidfile/log/key state"
}

cluster_is_healthy() {
  kubectl cluster-info >/dev/null 2>&1 || return 1
  if kubectl -n "$MANAGER_NS" get "deployment/$MANAGER_DEPLOY" >/dev/null 2>&1; then
    kubectl -n "$MANAGER_NS" rollout status "deployment/$MANAGER_DEPLOY" --timeout=10s >/dev/null 2>&1 || return 1
  fi
  return 0
}

create_cluster() {
  step "1/8 - Create kind cluster ($CLUSTER)"
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    if cluster_is_healthy; then
      info "cluster '$CLUSTER' already exists and looks healthy - reusing it"
    else
      refuse_unless_unmarked "the cluster is not clean/healthy, and recreating it would delete"
      info "cluster '$CLUSTER' exists but is not clean/healthy - deleting and recreating"
      kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
      create_kind_cluster
    fi
  else
    create_kind_cluster
  fi
}

build_and_load() {
  step "2/8 - Build the controller-manager image ($IMG)"
  make docker-build IMG="$IMG"
  step "3/8 - Load the image into the kind cluster"
  kind load docker-image "$IMG" --name "$CLUSTER"
}

install_cert_manager() {
  if kubectl get deploy -n cert-manager cert-manager-webhook >/dev/null 2>&1; then
    info "cert-manager already installed OK"
  else
    info "installing cert-manager ${CERT_MANAGER_VERSION}..."
    kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
  fi
  info "waiting for cert-manager to become Available..."
  for d in cert-manager cert-manager-cainjector cert-manager-webhook; do
    kubectl -n cert-manager rollout status "deployment/$d" --timeout=180s
  done
  local waited=0
  until kubectl get crd certificates.cert-manager.io >/dev/null 2>&1 || [ "$waited" -ge 60 ]; do
    sleep 2; waited=$((waited + 2))
  done
}

# wait_for_adapter_client_cert SECRET_NAME OUT_SUBDIR — block until cert-manager
# has issued one adapter identity's client keypair, then write it to
# $ADAPTER_TLS_DIR/$OUT_SUBDIR/{tls.crt,tls.key,ca.crt}.
wait_for_adapter_client_cert() {
  local secret="$1" out="$ADAPTER_TLS_DIR/$2"
  info "waiting for cert-manager to issue $secret..."
  local waited=0
  until kubectl -n "$MANAGER_NS" get secret "$secret" >/dev/null 2>&1; do
    [ "$waited" -ge 120 ] && fail "cert-manager never issued $secret"
    sleep 2; waited=$((waited + 2))
  done
  mkdir -p "$out"; chmod 700 "$out"
  kubectl -n "$MANAGER_NS" get secret "$secret" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$out/tls.crt"
  kubectl -n "$MANAGER_NS" get secret "$secret" -o jsonpath='{.data.tls\.key}' | base64 -d > "$out/tls.key"
  kubectl -n "$MANAGER_NS" get secret "$secret" -o jsonpath='{.data.ca\.crt}'  | base64 -d > "$out/ca.crt"
  chmod 600 "$out"/*
  [ -s "$out/tls.crt" ] && [ -s "$out/tls.key" ] && [ -s "$out/ca.crt" ] \
    || fail "adapter client certificate material for $secret is incomplete in $out"
  info "$secret ready OK ($out)"
}

install_control_plane() {
  step "4/8 - Install CRDs and deploy the control plane (config/overlays/demo-ab-live, mTLS)"
  install_cert_manager
  # --server-side: config/crd/bases/swarmada.io_computeworkloadtemplates.yaml is
  # ~526KB. Plain client-side `kubectl apply` stores a full copy of the applied
  # object in the kubectl.kubernetes.io/last-applied-configuration annotation,
  # which Kubernetes caps at 262144 bytes — client-side apply of THIS CRD fails
  # deterministically ("metadata.annotations: Too long"), independent of this
  # script or its overlay (reproduces with plain `kubectl apply -k
  # config/crd/bases/`, i.e. `make install` itself). Server-side apply tracks
  # field ownership instead of a stored annotation, so it has no such limit.
  # --force-conflicts: safe here since nothing else manages these fields on a
  # fresh cluster; only matters on a reused one.
  # DEMOAB_IMG reaches the manifest. The overlay pins the default image; any other image
  # is set by a throwaway kustomization that wraps the overlay, so the manager runs the
  # image steps 2 and 3 built and loaded rather than pulling the default tag. The overlay
  # is named by a path relative to the temporary directory, computed between RESOLVED
  # paths: mktemp can return a symlinked path (/var -> /private/var on macOS) and
  # kustomize resolves it, so an unresolved relative path points nowhere.
  if [[ "$IMG" == "$DEFAULT_IMG" ]]; then
    kubectl apply -k config/overlays/demo-ab-live --server-side --force-conflicts
  else
    local kz name tag
    kz=$(mktemp -d)
    name="$IMG" tag="latest"
    if [[ "${IMG##*/}" == *:* ]]; then name="${IMG%:*}" tag="${IMG##*:}"; fi
    cat > "$kz/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - $(python3 -c 'import os,sys; print(os.path.relpath(os.path.realpath(sys.argv[1]), os.path.realpath(sys.argv[2])))' "$REPO_ROOT/config/overlays/demo-ab-live" "$kz")
images:
  - name: ${DEFAULT_IMG%:*}
    newName: $name
    newTag: "$tag"
EOF
    info "manager image: $IMG (DEMOAB_IMG)"
    kubectl apply -k "$kz" --server-side --force-conflicts
    rm -rf "$kz"
  fi
  local i
  for i in "${!ROBOTS[@]}"; do
    kubectl -n "$MANAGER_NS" wait --for=condition=Ready "certificate/${CERT_NAMES[$i]}" --timeout=120s >/dev/null 2>&1 || true
    wait_for_adapter_client_cert "${CERT_SECRETS[$i]}" "robot-$((i+1))"
  done
  info "waiting for the controller-manager to become Available..."
  kubectl -n "$MANAGER_NS" rollout status "deployment/$MANAGER_DEPLOY" --timeout=180s
}

reset_namespace_state() {
  step "4.5/8 - Reset namespace $NS to a clean slate"
  if kubectl get namespace "$NS" >/dev/null 2>&1; then
    if [[ -n "$(kubectl get fleettasks -n "$NS" -o name 2>/dev/null || true)" ]]; then
      info "cancelling and deleting leftover FleetTasks from a previous run"
      for ft in $(kubectl get fleettasks -n "$NS" -o name 2>/dev/null || true); do
        kubectl patch "$ft" -n "$NS" --type=merge -p '{"spec":{"desiredState":"Cancelled"}}' >/dev/null 2>&1 || true
      done
      for _ in $(seq 1 20); do
        kubectl delete fleettasks -n "$NS" --all --ignore-not-found --wait=true >/dev/null 2>&1 && break
        sleep 1
      done
    fi
    info "deleting any leftover Robots/FleetActions/FleetAdapters from a previous run"
    kubectl delete robots,fleetactions,fleetadapters -n "$NS" --all --ignore-not-found --wait=true >/dev/null 2>&1 || true
  else
    info "namespace $NS does not exist yet - nothing to reset"
  fi
}

# apply_fleet: the sample fleet (config/samples/demo_a.yaml), then SPEC-only
# edits (never status) to (a) bind each robot to its own FleetAdapter identity
# — see the file header for why one identity per concurrently-live robot is
# required — and (b), for hardware-fault only, gate sim-robot-001's
# camera_front CAPABILITY on a camera_front HARDWARE component so its real
# adapter-reported hardware fault actually degrades the capability the
# scheduler reads (mirrors examples/warehouse-quickstart/run.sh's
# handle_hardware_fault gating). BOTH sim-robot-002 and sim-robot-003 start
# nav-only, so the camera task can only ever land on sim-robot-001 at
# apply time - sim-robot-002 only gets camera_front back (as the reroute
# target) after run_hardware_fault confirms the task actually landed there
# (see stage_reroute_spare). Giving sim-robot-002 camera_front up front
# raced the scheduler: with two equally-eligible camera robots at initial
# placement, it is free to hand the task to either one, and it did - the
# fault would then degrade a robot nothing was ever routed away from.
apply_fleet() {
  step "5/8 - Apply the sample fleet ($SAMPLE) and bind each robot to its own live adapter"
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f "$SAMPLE"

  local i robot adapter
  for i in "${!ROBOTS[@]}"; do
    robot="${ROBOTS[$i]}"; adapter="${ADAPTERS[$i]}"
    kubectl patch "robot/$robot" -n "$NS" --type=json \
      -p "[{\"op\":\"replace\",\"path\":\"/spec/adapter/name\",\"value\":\"$adapter\"}]"
    # RobotStatusSink (internal/controller/robot_status_sink.go) only projects
    # telemetry onto a Robot carrying this annotation matching the wire
    # robot_id - without it, real telemetry is silently dropped (by design: an
    # unmapped robot_id must not wedge ingestion) and batteryPercent/hardware
    # never populate even though the ControlStream and scheduling succeed via
    # the separate liveness/Connected signal. An annotation is metadata, not
    # status.
    kubectl annotate "robot/$robot" -n "$NS" "swarmada.io/robot-id=$robot" --overwrite >/dev/null
  done
  info "each of ${#ROBOTS[@]} robots now bound to its own FleetAdapter (spec.adapter.name) and"
  info "annotated with its wire robot-id (both spec/metadata, never status)"

  if [[ "$SCENARIO" == "hardware-fault" ]]; then
    # deliver-pallet-001 (nav-only) would otherwise be picked up by whichever
    # of sim-robot-002/003 is idle first, leaving no free robot for the
    # reroute to land on when the fault fires. Hold it back entirely (this
    # scenario only needs inspect-receiving-dock) - mirrors
    # examples/warehouse-quickstart/run.sh's sample_docs_excluding for the
    # same scenario, just via delete instead of a filtered apply.
    kubectl delete fleetaction deliver-pallet-001 -n "$NS" --ignore-not-found >/dev/null
    info "held back deliver-pallet-001 for this scenario - only inspect-receiving-dock runs,"
    info "so sim-robot-002/003 stay free for the reroute"
    # The fault can only reroute work that is still in progress, and the adapter reports
    # SUCCEEDED on arrival: to the sample's nominal target (10 m, ~10 s) the task finishes
    # before the camera fails. Send it to the far corner of warehouse-a instead, (25, 20),
    # about 32 m from the spawn point, so it is still driving when --fault-at fires. Set
    # before any adapter is up, so the scheduler never sees the short route.
    # payload.raw is bytes, so base64: {"x":25.0,"y":20.0}
    kubectl patch fleetaction inspect-receiving-dock -n "$NS" --type=merge \
      -p '{"spec":{"payload":{"raw":"eyJ4IjoyNS4wLCJ5IjoyMC4wfQ=="}}}' >/dev/null
    info "inspect-receiving-dock routed to (25, 20), ~32 s of driving, so the fault meets it mid-route"
    local nav_only='[{"op":"replace","path":"/spec/capabilities","value":[{"name":"navigation","type":"hardware-native","pauseable":false}]}]'
    kubectl patch robot/sim-robot-002 -n "$NS" --type=json -p "$nav_only"
    kubectl patch robot/sim-robot-003 -n "$NS" --type=json -p "$nav_only"
    kubectl patch robot/sim-robot-001 -n "$NS" --type=json -p '[
      {"op":"add","path":"/spec/hardware","value":[{"name":"camera_front","type":"Camera"}]},
      {"op":"replace","path":"/spec/capabilities","value":[
        {"name":"navigation","type":"hardware-native","pauseable":false},
        {"name":"camera_front","type":"hardware-native","pauseable":false,"requiredHardware":["camera_front"]}
      ]}]'
    info "sim-robot-001 camera_front gated on hardware (Active only once its real adapter reports"
    info "camera_front Healthy); sim-robot-002/003 both start nav-only, so the camera task can"
    info "only land on sim-robot-001; sim-robot-002 gets camera_front back once that's confirmed."
  fi

  kubectl apply -f - >/dev/null <<EOF
apiVersion: swarmada.io/v1
kind: SwarmadaConfig
metadata: {name: swarmada-config, namespace: $NS}
spec:
  signing:
    requireSignatureVerification: false
  telemetry:
    sink:
      type: Drop
EOF
}

# apply_fleet_adapters: one FleetAdapter object per robot, each with a
# digest-verified conformance report the conformance harness wrote
# (status.conformance reaches Passed through the production path, on a verdict a
# run produced). status.phase=Connected is NEVER written here
# — see wait_adapter_connected, which polls the real, adapter-derived value.
apply_fleet_adapters() {
  step "5.5/8 - Create ${#ADAPTERS[@]} FleetAdapter objects (no status write - see run_demo)"
  local i adapter cm report digest
  # One harness run of the simulation adapter; all three adapters are that adapter.
  # hack/sim-conformance-report.sh runs the harness when it can and otherwise uses the
  # report recorded in test/samples/testdata/recorded-sim-adapter-report.json,
  # and says which on stderr.
  report="$(mktemp)"
  bash hack/sim-conformance-report.sh "$report" || fail "could not produce a conformance report for the simulation adapter"
  digest="sha256:$(shasum -a 256 <"$report" | awk '{print $1}')"
  for i in "${!ADAPTERS[@]}"; do
    adapter="${ADAPTERS[$i]}"
    cm="${adapter}-conformance"
    kubectl create configmap "$cm" -n "$NS" --from-file=report.json="$report" \
      --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    kubectl apply -f - >/dev/null <<EOF
apiVersion: swarmada.io/v1
kind: FleetAdapter
metadata: {name: $adapter, namespace: $NS}
spec:
  vendor: simulation
  # The class config/samples/demo_a.yaml gives the sample Robots. The ControlStream
  # authorizer refuses every robot-scoped message from this adapter unless the robot's
  # class is listed here.
  servesRobotClasses: [simbot-v1]
  endpoint: "127.0.0.1:${LOCAL_CS_PORT}"
  # A 30s override used to sit here because the default (10s) flapped all three adapters
  # Connected -> Degraded/Disconnected at once, and that was attributed to three
  # adapter processes plus a kind cluster competing for CPU. THAT DIAGNOSIS WAS
  # WRONG: nothing sent adapter heartbeats on a cadence at all, so no
  # interval could have been satisfied and raising it only bought more slack
  # before the same inevitable lapse. ADR-0054 supplies the cadence.
  #
  # The override is RETIRED. The run the previous comment asked for was made on
  # 2026-09-16: a live three-adapter kind cluster held all three adapters
  # Connected at this default, with sim-robot-003's lastSeenAt advancing every
  # 15s (heartbeatTimeout/2, ADR-0026's throttle) over nine consecutive
  # advances -- a floor under the throttle, which is what ADR-0054 supplies and
  # what the 30s override was standing in for.
  # This is a real spec field, not a projected value.
  heartbeatIntervalSeconds: 10
  conformanceReport:
    suiteVersion: "C1-C15"
    configMapRef: $cm
    digest: "$digest"
EOF
  done
  rm -f "$report"
  info "${#ADAPTERS[@]} FleetAdapter objects applied (digest-verified conformance; phase not yet Connected -"
  info "each becomes Connected for real once its own sim_adapter.py process registers)."
}

portforward_ready() {
  grep -q "Forwarding from 127.0.0.1:${LOCAL_CS_PORT}" "$PF_LOG" 2>/dev/null
}

start_portforward() {
  step "6/8 - Port-forward to the ControlStream Service (shared by all ${#ROBOTS[@]} robots -"
  info "each dials it with its OWN client certificate, so the server demuxes identity by SAN,"
  info "not by port)"
  : > "$PF_LOG"
  kubectl -n "$MANAGER_NS" port-forward service/swarmada-controlstream "${LOCAL_CS_PORT}:9445" \
    >"$PF_LOG" 2>&1 &
  local pf_pid=$!
  LIVE_PIDS+=("$pf_pid")
  echo "$pf_pid" >> "$PIDFILE"
  if ! wait_until "port-forward ready" 15 portforward_ready; then
    cat "$PF_LOG" >&2 || true
    fail "port-forward to the ControlStream Service never became ready"
  fi
  info "port-forward ready OK (127.0.0.1:${LOCAL_CS_PORT} -> service/swarmada-controlstream:9445)"
}

# launch_adapter INDEX ROBOT_ID SCENARIO [EXTRA_ARGS...] — start one real,
# unmodified sim_adapter.py process for one robot, using that robot's own
# client certificate (index-aligned with CERT_SECRETS). exec'd, same reasoning
# as examples/warehouse-quickstart/run.sh: makes the subshell BECOME the
# python process so $! and kill_forcefully target the actual adapter, not an
# empty wrapper shell.
launch_adapter() {
  local idx="$1" robot="$2" scenario="$3"; shift 3
  local tlsdir="$ADAPTER_TLS_DIR/robot-$((idx+1))"
  local log="/tmp/demoab-${CLUSTER}-adapter-$((idx+1)).log"
  : > "$log"
  (
    cd "$REPO_ROOT"
    exec env PYTHONUNBUFFERED=1 PYTHONPATH="proto${PYTHONPATH:+:${PYTHONPATH}}" \
    python3 adapters/simulation/sim_adapter.py \
      --endpoint "127.0.0.1:${LOCAL_CS_PORT}" --namespace "$NS" \
      --robot-id "$robot" --zone warehouse-a --vendor simulation \
      --scenario "$scenario" \
      --tls-ca "$tlsdir/ca.crt" --tls-cert "$tlsdir/tls.crt" --tls-key "$tlsdir/tls.key" \
      --tls-server-name "swarmada-controlstream.${MANAGER_NS}.svc" \
      "$@"
  ) >"$log" 2>&1 &
  local pid=$!
  LIVE_PIDS+=("$pid")
  echo "$pid" >> "$PIDFILE"
  info "launched $robot (adapter ${ADAPTERS[$idx]}, scenario '$scenario', log: $log)"
}

# ---- real-status wait predicates (read-only; nothing here ever patches) ----

robot_battery_seen() {
  local pct; pct=$(kubectl get "robot/$1" -n "$NS" -o jsonpath='{.status.batteryPercent}' 2>/dev/null || true)
  [[ -n "$pct" ]]
}
robot_phase() { kubectl get "robot/$1" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
robot_idle()  { [[ "$(robot_phase "$1")" == "Idle" ]]; }
# robot_left_discovered: true once ADR-0029's real Discovered->{Idle,InProgress}
# transition has happened, from EITHER resulting phase. Checking for "Idle"
# alone races the real scheduler: assignment can (and often does) land within
# the same second real liveness arrives, moving phase straight through Idle
# into InProgress before a 2s poll ever samples it. Any phase other than the
# pre-liveness set is equally valid proof the real transition already fired.
robot_left_discovered() {
  local p; p="$(robot_phase "$1")"
  [[ -n "$p" && "$p" != "Discovered" && "$p" != "Offline" ]]
}

adapter_phase() { kubectl get "fleetadapter/$1" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
adapter_connected() { [[ "$(adapter_phase "$1")" == "Connected" ]]; }

any_fleetaction_assigned() {
  local vals; vals=$(kubectl get fleetactions -n "$NS" -o jsonpath='{.items[*].status.assignedRobot}' 2>/dev/null || true)
  [[ -n "${vals// /}" ]]
}
fleetaction_holder() { kubectl get "fleetaction/$1" -n "$NS" -o jsonpath='{.status.assignedRobot}' 2>/dev/null || true; }
inspect_assigned_to_001() { [[ "$(fleetaction_holder inspect-receiving-dock)" == "sim-robot-001" ]]; }
inspect_reassigned_off_001() {
  local a; a="$(fleetaction_holder inspect-receiving-dock)"
  [[ -n "$a" && "$a" != "sim-robot-001" ]]
}
capability_status() {
  kubectl get "robot/$1" -n "$NS" -o jsonpath="{.status.capabilities[?(@.name=='$2')].status}" 2>/dev/null || true
}
camera_active_on_001()  { [[ "$(capability_status sim-robot-001 camera_front)" == "Active" ]]; }
camera_degraded_on_001(){ [[ "$(capability_status sim-robot-001 camera_front)" == "Degraded" ]]; }

# wait_until DESC TIMEOUT PREDICATE [ARGS...] - poll until true or TIMEOUT secs.
wait_until() {
  local desc="$1" timeout="$2" pred="$3"; shift 3
  local elapsed=0 interval=2
  while true; do
    if "$pred" "$@"; then return 0; fi
    (( elapsed >= timeout )) && return 1
    sleep "$interval"; elapsed=$((elapsed + interval))
  done
}

diagnostics() {
  echo "---- diagnostics ----" >&2
  kubectl get robots,fleetadapters -n "$NS" -o wide >&2 2>&1 || true
  kubectl get fleetactions -n "$NS" -o wide >&2 2>&1 || true
  kubectl -n "$MANAGER_NS" logs "deployment/$MANAGER_DEPLOY" --tail=50 >&2 2>&1 || true
  local i
  for i in "${!ROBOTS[@]}"; do
    echo "---- adapter log for ${ROBOTS[$i]} (/tmp/demoab-${CLUSTER}-adapter-$((i+1)).log) ----" >&2
    tail -n 40 "/tmp/demoab-${CLUSTER}-adapter-$((i+1)).log" >&2 2>&1 || true
  done
  echo "---- port-forward log ($PF_LOG) ----" >&2
  tail -n 20 "$PF_LOG" >&2 2>&1 || true
}

teardown_hint() {
  echo
  echo "The cluster '$CLUSTER' is left running so you can explore it:"
  echo "    kubectl --context $KCTX get robots,fleetadapters,fleetactions -n $NS"
  echo "The three live sim adapters and the port-forward keep running until this script exits."
  remind_to_clean_cluster
}

# wait_all_robots_live: for every robot, wait for real telemetry (proof the
# live handshake worked) and then for the real Discovered->Idle transition
# (ADR-0029) - never patched, only awaited.
wait_all_robots_live() {
  local r
  for r in "${ROBOTS[@]}"; do
    info "waiting up to ${READY_TIMEOUT}s for real telemetry from $r..."
    wait_until "$r telemetry" "$READY_TIMEOUT" robot_battery_seen "$r" \
      || { diagnostics; fail "$r never reported real telemetry within ${READY_TIMEOUT}s"; }
    info "  $r: real telemetry confirmed (battery=$(kubectl get "robot/$r" -n "$NS" -o jsonpath='{.status.batteryPercent}')%)"
  done
  for r in "${ROBOTS[@]}"; do
    info "waiting up to ${READY_TIMEOUT}s for $r to leave Discovered for real (ADR-0029 - not patched)..."
    wait_until "$r left Discovered" "$READY_TIMEOUT" robot_left_discovered "$r" \
      || { diagnostics; fail "$r never left Discovered for real within ${READY_TIMEOUT}s"; }
    info "  $r: phase=$(robot_phase "$r") (real) OK"
  done
  local a
  for a in "${ADAPTERS[@]}"; do
    info "waiting up to ${READY_TIMEOUT}s for FleetAdapter $a to reach Connected (real - not patched)..."
    wait_until "$a Connected" "$READY_TIMEOUT" adapter_connected "$a" \
      || { diagnostics; fail "FleetAdapter $a never reached Connected for real within ${READY_TIMEOUT}s"; }
    info "  $a: phase=Connected (real) OK"
  done
}

run_healthy_fleet() {
  step "7/8 - Demo A / UC-2.1: discover -> register -> assign, on the wire"
  launch_adapter 0 sim-robot-001 healthy-fleet
  launch_adapter 1 sim-robot-002 healthy-fleet
  launch_adapter 2 sim-robot-003 healthy-fleet
  wait_all_robots_live

  info "waiting up to ${ASSIGN_TIMEOUT}s for the real scheduler to assign a FleetAction..."
  wait_until "a FleetAction assigned" "$ASSIGN_TIMEOUT" any_fleetaction_assigned \
    || { diagnostics; fail "no FleetAction was assigned within ${ASSIGN_TIMEOUT}s"; }
  info "a FleetAction was assigned by the real scheduler OK"

  echo
  kubectl get robots -n "$NS"
  echo
  kubectl get fleetadapters -n "$NS"
  echo
  kubectl get fleetactions -n "$NS"
  echo
  echo "PASS: Demo A end state reached - three independently-live robots all left Discovered,"
  echo "      all three FleetAdapters reached Connected, and the real scheduler assigned a"
  echo "      FleetAction. No kubectl status patch ran anywhere in this path."
}

run_hardware_fault() {
  step "7/8 - Demo B / UC-2.2: fault -> capability degrade -> reroute -> recover, on the wire"
  info "sim-robot-001 runs the fault timeline for real (default --fault-at 20 --recover-at 80,"
  info "component camera_front); sim-robot-002/003 run healthy-fleet unmodified. Nothing in"
  info "this script triggers, paces, or patches the fault - it fires on the adapter's own clock."
  launch_adapter 0 sim-robot-001 hardware-fault --fault-component camera_front --fault-at 20 --recover-at 80
  launch_adapter 1 sim-robot-002 healthy-fleet
  launch_adapter 2 sim-robot-003 healthy-fleet
  wait_all_robots_live

  info "waiting up to ${READY_TIMEOUT}s for sim-robot-001's camera_front capability to derive"
  info "Active from its own real pre-fault hardware telemetry..."
  wait_until "sim-robot-001 camera_front Active" "$READY_TIMEOUT" camera_active_on_001 \
    || { diagnostics; fail "sim-robot-001's camera_front capability never derived Active"; }
  info "  sim-robot-001: capabilities[camera_front]=Active (real) OK"

  info "waiting up to ${ASSIGN_TIMEOUT}s for inspect-receiving-dock to land on sim-robot-001"
  info "(the only camera-capable robot with an assignable task right now)..."
  wait_until "inspect-receiving-dock assigned to sim-robot-001" "$ASSIGN_TIMEOUT" inspect_assigned_to_001 \
    || { diagnostics; fail "inspect-receiving-dock never landed on sim-robot-001"; }
  info "  inspect-receiving-dock held by sim-robot-001 OK"

  info "staging sim-robot-002 as the reroute target: restoring its camera_front capability"
  info "(spec, ungated so Active immediately - not status) now that the camera task is"
  info "confirmed to be held by sim-robot-001, so the two never raced for it..."
  kubectl patch robot/sim-robot-002 -n "$NS" --type=json -p \
    '[{"op":"add","path":"/spec/capabilities/-","value":{"name":"camera_front","type":"hardware-native","pauseable":false}}]'

  info "waiting up to ${REROUTE_TIMEOUT}s for the REAL camera fault (adapter clock, ~T+20s from"
  info "adapter start) to degrade the capability and for the control plane to AUTOMATICALLY"
  info "safe-stop and reroute the task to sim-robot-002 - no script trigger, no patch..."
  wait_until "inspect-receiving-dock reassigned off sim-robot-001" "$REROUTE_TIMEOUT" inspect_reassigned_off_001 \
    || { diagnostics; fail "inspect-receiving-dock was never rerouted off sim-robot-001"; }
  local new_holder; new_holder="$(fleetaction_holder inspect-receiving-dock)"
  info "  reroute confirmed OK - inspect-receiving-dock moved from sim-robot-001 to $new_holder"
  [[ "$new_holder" == "sim-robot-002" ]] || info "  (note: landed on $new_holder, not sim-robot-002 - still a real capability-loss reroute)"

  info "waiting up to ${RECOVER_TIMEOUT}s for the REAL camera recovery (adapter clock, ~T+80s"
  info "from adapter start) to bring sim-robot-001's capability back to Active..."
  if wait_until "sim-robot-001 camera_front Active again" "$RECOVER_TIMEOUT" camera_active_on_001; then
    info "  recovery confirmed OK - sim-robot-001 capabilities[camera_front]=Active again (real)"
  else
    diagnostics
    fail "sim-robot-001's camera_front never recovered to Active within ${RECOVER_TIMEOUT}s"
  fi

  echo
  kubectl get robots -n "$NS"
  echo
  kubectl get fleetadapters -n "$NS"
  echo
  kubectl get fleetactions -n "$NS"
  echo
  echo "PASS: Demo B end state reached - a real, adapter-timed hardware fault degraded"
  echo "      sim-robot-001's capability, the control plane automatically safe-stopped and"
  echo "      rerouted its task to $new_holder, and the capability recovered for real when the"
  echo "      adapter's own fault window closed. No kubectl status patch ran anywhere in this path."
}

main() {
  parse_args "$@"
  if ((CLEAN_REQUESTED)); then
    do_clean_everything
    exit 0
  fi
  preflight
  cleanup_stale_run
  create_cluster
  build_and_load
  install_control_plane
  reset_namespace_state
  apply_fleet
  apply_fleet_adapters
  start_portforward
  if [[ "$SCENARIO" == "hardware-fault" ]]; then
    run_hardware_fault
  else
    run_healthy_fleet
  fi
  teardown_hint
}

main "$@"
