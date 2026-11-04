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

# Map a floor, get zones, drive in them, on the simulation adapter.
#
#   examples/floor-mapping/run.sh
#
# 1. examples/first-run brings up the control plane and admits the simulated robot.
# 2. The cluster starts the simulation adapter with an artifact directory, and the
#    zone-proposal worker (both Deployments; nothing here is started by hand).
# 3. FleetTask explore-floor runs: explore, then [charge, explore] up to three times.
#    Each segment saves its map at <namespace>/<action-name> and reports the
#    reference and its sha256; the worker writes one zone proposal per segment.
# 4. The operator reviews the proposal and applies it. That apply is the confirm step,
#    and the only way a proposed zone becomes a FleetZone.
# 5. The worker renders the adapter's keep-out mask from the applied zones and
#    publishes its reference (keepouts_ref) in ConfigMap floor-keepouts.
# 6. The operator places the robot in the zone it stands in, Patrol work addressed to
#    a generated zone runs, and work addressed to the keep-out is refused by the
#    control plane. Keeping a robot out of the keep-out itself is the adapter's, with
#    that mask; the simulation adapter enforces no geometry.
#
# Environment:
#   FLOOR_MAPPING_CLUSTER   kind cluster name (default swarmada-floor-mapping). An existing
#                           cluster is used when FLOOR_MAPPING_USE_EXISTING=1.
#   FLOOR_MAPPING_YES=1     apply the proposal without waiting for the operator's Enter.
#
# The simulation stands in for the robot: it moves nothing while it explores, runs no
# SLAM, and draws the map it reports. Every message it emits says "sim".

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${FLOOR_MAPPING_CLUSTER:-swarmada-floor-mapping}"
NS="warehouse-a"
RELEASE_NS="swarmada-system"
ADAPTER="sim-fleet-adapter"
ROBOT="sim-robot-001"
WORKER_IMG="swarmada-zone-proposal-worker:dev"
NODE_DIR="/var/lib/swarmada/floor-maps"
HERE="examples/floor-mapping"

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

wait_for() {
  local what="$1" timeout="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 3
  done
  info "$what"
}

jp() { kubectl get "$1" -n "$2" -o "jsonpath=$3" 2>/dev/null; }

step "0 — Preflight"
[[ -f tools/map-to-fleetzone/floor_to_member_zones.py ]] ||
  fail "tools/map-to-fleetzone/ is missing; the worker image is built from it"

step "1 — Control plane and robot (examples/first-run)"
if [[ "${FLOOR_MAPPING_USE_EXISTING:-0}" == 1 ]]; then
  FIRST_RUN_CLUSTER="$CLUSTER" FIRST_RUN_USE_EXISTING=1 examples/first-run/run.sh
else
  FIRST_RUN_CLUSTER="$CLUSTER" examples/first-run/run.sh
fi
KCFG="$(mktemp)"
trap 'rm -f "$KCFG"' EXIT
kind get kubeconfig --name "$CLUSTER" >"$KCFG"
export KUBECONFIG="$KCFG"

step "2 — The artifact directory on the node, the worker image, and both Deployments"
# The directory a PersistentVolume and the adapter share. On a robot run it is a kind
# extraMount of the adapter host's directory; here the in-cluster simulation writes it.
docker exec "$CLUSTER-control-plane" sh -c \
  "mkdir -p $NODE_DIR && chown 65532:65532 $NODE_DIR && chmod 0755 $NODE_DIR"
docker build -f "$HERE/worker/Dockerfile" -t "$WORKER_IMG" .
kind load docker-image "$WORKER_IMG" --name "$CLUSTER"
kubectl apply -k "$HERE/sim-adapter" >/dev/null
kubectl -n "$RELEASE_NS" rollout status "deploy/$ADAPTER" --timeout=180s >/dev/null
kubectl apply -k "$HERE/overlay" >/dev/null
kubectl -n "$NS" rollout status deploy/zone-proposal-worker --timeout=180s >/dev/null
robot_idle() { [[ "$(jp "robot/$ROBOT" "$NS" '{.status.phase}')" == "Idle" ]]; }
wait_for "Robot $ROBOT Idle again after the adapter restart" 180 robot_idle ||
  fail "Robot $ROBOT is $(jp "robot/$ROBOT" "$NS" '{.status.phase}')"
info "ArtifactStore floor-maps -> claim floor-maps -> $NODE_DIR"

step "3 — FleetTask explore-floor: explore, then [charge, explore] x3"
kubectl apply -f "$HERE/tasks/explore-floor.yaml" >/dev/null
task_terminal() {
  case "$(jp fleettask/explore-floor "$NS" '{.status.phase}')" in
    Succeeded | Failed | Cancelled | Compensated) return 0 ;;
    *) return 1 ;;
  esac
}
wait_for "FleetTask explore-floor reached a terminal phase" 900 task_terminal ||
  fail "explore-floor is $(jp fleettask/explore-floor "$NS" '{.status.phase}') after 900s"
[[ "$(jp fleettask/explore-floor "$NS" '{.status.phase}')" == "Succeeded" ]] ||
  fail "explore-floor ended $(jp fleettask/explore-floor "$NS" '{.status.phase}')"
kubectl get fleetactions -n "$NS" -o \
  custom-columns='ACTION:.metadata.name,TYPE:.spec.type,VERB:.spec.customType,PHASE:.status.phase,MESSAGE:.status.message' \
  | sed 's/^/    /'

step "4 — The zone proposal, and the operator's confirm"
explore_actions() {
  kubectl get fleetactions -n "$NS" -o \
    jsonpath='{range .items[?(@.spec.customType=="explore-floor")]}{.metadata.name}{"\n"}{end}'
}
proposals_written() {
  local a
  for a in $(explore_actions); do
    kubectl get configmap "zone-proposal-$a" -n "$NS" >/dev/null 2>&1 || return 1
  done
}
wait_for "a proposal ConfigMap for every explore-floor segment" 120 proposals_written ||
  fail "the worker did not write every proposal: $(kubectl -n "$NS" logs deploy/zone-proposal-worker --tail=20)"
kubectl get configmaps -n "$NS" -l swarmada.io/zone-proposal=true -o \
  custom-columns='PROPOSAL:.metadata.name,VERDICT:.metadata.annotations.swarmada\.io/zone-proposal-verdict' \
  | sed 's/^/    /'
# The segment that left no frontier is the whole floor.
FINAL=""
for a in $(explore_actions); do
  if [[ "$(jp "fleetaction/$a" "$NS" '{.status.message}')" == *"frontier: none-left"* ]]; then
    FINAL="$a"
    break
  fi
done
[[ -n "$FINAL" ]] || fail "no explore-floor segment reported frontier: none-left"
PROPOSAL="zone-proposal-$FINAL"
[[ "$(jp "configmap/$PROPOSAL" "$NS" '{.metadata.annotations.swarmada\.io/zone-proposal-verdict}')" == "proposed" ]] ||
  fail "$PROPOSAL was refused: $(jp "configmap/$PROPOSAL" "$NS" '{.data.summary\.txt}')"
jp "configmap/$PROPOSAL" "$NS" '{.data.summary\.txt}' | sed 's/^/    /'
kubectl get fleetzones -n "$NS" -o name | grep -q floor &&
  fail "a generated zone exists before the operator applied anything"
info "no generated FleetZone exists yet: the worker proposes, it does not apply"
if [[ "${FLOOR_MAPPING_YES:-0}" != 1 ]]; then
  read -r -p "    Apply $PROPOSAL? [Enter to apply, Ctrl-C to stop] " _
fi
jp "configmap/$PROPOSAL" "$NS" '{.data.zones\.yaml}' | kubectl apply -f -
bin/swarmctl get fleetzones -n "$NS"

step "5 — The adapter's keep-out mask, from the applied zones"
keepouts_published() {
  [[ -n "$(jp configmap/floor-keepouts "$NS" '{.data.keepouts_ref}')" ]]
}
wait_for "ConfigMap floor-keepouts published" 120 keepouts_published ||
  fail "the worker published no keep-out reference: $(kubectl -n "$NS" logs deploy/zone-proposal-worker --tail=20)"
info "keepouts_ref:    $(jp configmap/floor-keepouts "$NS" '{.data.keepouts_ref}')"
info "keepouts_sha256: $(jp configmap/floor-keepouts "$NS" '{.data.keepouts_sha256}')"
info "zone set:        $(jp configmap/floor-keepouts "$NS" '{.data.zone_set_sha256}')"
info "keep-outs:       $(jp configmap/floor-keepouts "$NS" '{.data.keepouts}')"

step "6 — Work in the generated zones"
kubectl patch robot "$ROBOT" -n "$NS" --type merge -p '{"spec":{"zone":"floor-room-a-open"}}' >/dev/null
in_zone() { [[ "$(jp "robot/$ROBOT" "$NS" '{.status.currentZone}')" == "floor-room-a-open" ]]; }
wait_for "Robot $ROBOT is in floor-room-a-open" 120 in_zone ||
  fail "Robot $ROBOT reports zone '$(jp "robot/$ROBOT" "$NS" '{.status.currentZone}')'"
kubectl apply -f "$HERE/tasks/patrol-generated-zones.yaml" >/dev/null
patrol_terminal() {
  case "$(jp fleettask/patrol-generated-zones "$NS" '{.status.phase}')" in
    Succeeded | Failed | Cancelled | Compensated) return 0 ;;
    *) return 1 ;;
  esac
}
wait_for "FleetTask patrol-generated-zones reached a terminal phase" 300 patrol_terminal ||
  fail "patrol-generated-zones is $(jp fleettask/patrol-generated-zones "$NS" '{.status.phase}')"
[[ "$(jp fleettask/patrol-generated-zones "$NS" '{.status.phase}')" == "Succeeded" ]] ||
  fail "patrol-generated-zones ended $(jp fleettask/patrol-generated-zones "$NS" '{.status.phase}')"
info "patrol-generated-zones Succeeded (work addressed to floor-room-a; goals are the payload's)"

kubectl apply -f "$HERE/tasks/goal-into-exclusion.yaml" >/dev/null
refused() { [[ "$(jp fleetaction/goal-into-keepout "$NS" '{.status.phase}')" == "Failed" ]]; }
wait_for "work addressed to the keep-out refused" 120 refused ||
  fail "goal-into-keepout is $(jp fleetaction/goal-into-keepout "$NS" '{.status.phase}')"
info "reason:  $(jp fleetaction/goal-into-keepout "$NS" '{.status.failureReason}')"
info "message: $(jp fleetaction/goal-into-keepout "$NS" '{.status.message}')"
[[ -z "$(jp fleetaction/goal-into-keepout "$NS" '{.status.assignedRobot}')" ]] ||
  fail "goal-into-keepout was assigned a robot"

step "Result"
echo "PASS: a map saved by reference became a zone proposal, the operator applied it, the"
echo "      keep-out mask was rendered from the applied zones, patrol work addressed to a"
echo "      generated zone ran, and work addressed to the keep-out was refused."
