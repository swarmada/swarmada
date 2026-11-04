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

# The cluster side of a floor-mapping robot run: an object store the robot's adapter can
# reach, and the adapter's identity.
#
#   setup.sh install <cluster> <owner-token>   MinIO, the bucket, two scoped users, the
#                                              ObjectStore overlay, the ROS 2 adapter's
#                                              FleetAdapter and robot class, its client
#                                              certificate, and this run's robot credential
#   setup.sh forward <cluster> <owner-token>   serve the store and the ControlStream on this
#                                              machine's LAN address, for the robot to reach
#   setup.sh rotate  <cluster> <owner-token>   a new robot credential and client certificate
#   setup.sh stop                              end the forwards
#
# Needs a kind cluster made and marked by cluster-owner.sh (<owner-token> must be its token:
# nothing is installed into a cluster that is not yours), with examples/first-run already
# run in it (FIRST_RUN_USE_EXISTING=1).
#
# THE CREDENTIALS. MinIO's root credential is generated here, kept in a Secret in the
# cluster, used once inside the MinIO pod and never printed. Two users are made from it:
#   - the worker (in the cluster): read maps under warehouse-a/, list only there, write only
#     warehouse-a/keepouts-*; its credential is a Secret the worker's Deployment reads;
#   - the robot (its adapter, held at the edge): write under warehouse-a/ except keepouts-*,
#     read only warehouse-a/keepouts-*, list and delete nothing. Its name is
#     SWARMADA_STORE_ACCESS_KEY in the robot file (ROBOT_ENV, default
#     ~/.config/swarmada/robot.env), which the operator sets; its secret is made new on every
#     install and rotate and written there as SWARMADA_STORE_SECRET_KEY. The adapter is
#     given both in its environment at start, never as a file on the robot.
# The adapter's client certificate (SAN fleet-adapter-ros2.warehouse-a.svc.cluster.local) is
# issued by the chart's own CA through controlStream.tls.certManager.adapterClients, the
# path examples/first-run uses for the simulation adapter, and copied to RUN_DIR (default
# ~/.config/swarmada/robot-run, mode 0700, files 0600) with the CA that verifies the server.
#
# THE ROUTE. `forward` binds kubectl port-forwards to this machine's LAN address (BIND_ADDR,
# or the address of LAN_IF, default the interface of the default route), so a robot on the same network reaches the store on
# port 9000 and the ControlStream on port 9445. The address is written to
# RUN_DIR/endpoints.env and never printed. The store is plain HTTP on the LAN: every request
# is signed (the secret itself never crosses), and every map and keep-out set is proven by
# its sha256 at the other end, but the bytes are readable on the LAN.
#
# Values are never printed: only the names of the keys and files that hold them.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
HERE="$REPO_ROOT/examples/floor-mapping/object-store"
NS="warehouse-a"
STORE_NS="swarmada-store"
RELEASE="swarmada"
RELEASE_NS="swarmada-system"
BUCKET="floor-maps"
ADAPTER="fleet-adapter-ros2"
MINIO_IMG="swarmada-demo-minio:RELEASE.2025-09-07T16-13-09Z"
WORKER_IMG="swarmada-zone-proposal-worker:dev"
WORKER_USER="floor-maps-worker"
ROBOT_ENV="${ROBOT_ENV:-$HOME/.config/swarmada/robot.env}"
RUN_DIR="${RUN_DIR:-$HOME/.config/swarmada/robot-run}"
CONTROL_SVC="swarmada-controlstream"

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

usage() { sed -n '17,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 2; }

wait_for() {
  local what="$1" timeout="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 2
  done
  info "$what"
}

new_secret() { openssl rand -hex 24; }

# The robot file: read one key's value, or replace (or add) one key's line, keeping the
# file's mode. Values pass through variables only.
env_get() { sed -n "s/^\(export \)\{0,1\}$1=//p" "$ROBOT_ENV" | tail -1 | sed "s/^['\"]//; s/['\"]$//"; }
env_put() {
  local key="$1" value="$2" tmp
  tmp="$(mktemp "$ROBOT_ENV.XXXXXX")"
  chmod 600 "$tmp"
  grep -v -E "^(export )?$key=" "$ROBOT_ENV" > "$tmp" || true
  printf '%s=%s\n' "$key" "$value" >> "$tmp"
  mv "$tmp" "$ROBOT_ENV"
}

cluster_kubeconfig() {
  mkdir -p "$RUN_DIR"; chmod 700 "$RUN_DIR"
  KCFG="$RUN_DIR/kubeconfig"
  kind get kubeconfig --name "$CLUSTER" > "$KCFG" 2>/dev/null ||
    fail "no kind cluster named '$CLUSTER'"
  chmod 600 "$KCFG"
  export KUBECONFIG="$KCFG"
}

# Nothing is installed into, or served from, a cluster that is not the caller's: the owner
# marker cluster-owner.sh wrote on it must carry the token given.
require_owner() {
  local token
  token="$(kubectl -n kube-system get configmap swarmada-cluster-owner \
             -o jsonpath='{.data.token}' 2>/dev/null || true)"
  [[ -n "$token" ]] || fail "'$CLUSTER' carries no owner marker (UNMARKED): not yours to install into"
  [[ "$token" == "$OWNER_TOKEN" ]] || fail "'$CLUSTER' is marked with another token: not yours"
  info "owner marker matches the token given"
}

mc_in_pod() {
  # The script (and every credential in it) goes on stdin, so none is on this machine's
  # command line; inside the pod mc reads them from its own arguments.
  kubectl -n "$STORE_NS" exec -i deploy/minio -- sh -s
}

make_user() {  # <user> <secret> <policy name> <policy file>
  local root_user root_pass
  root_user="$(kubectl -n "$STORE_NS" get secret minio-root -o jsonpath='{.data.user}' | base64 -d)"
  root_pass="$(kubectl -n "$STORE_NS" get secret minio-root -o jsonpath='{.data.password}' | base64 -d)"
  mc_in_pod <<EOF
set -e
export HOME=/tmp MC_CONFIG_DIR=/tmp/.mc
mc alias set local http://127.0.0.1:9000 '$root_user' '$root_pass' >/dev/null
mc mb --ignore-existing local/$BUCKET >/dev/null
mc anonymous set none local/$BUCKET >/dev/null
cat > /tmp/policy.json <<'POLICY'
$(cat "$4")
POLICY
mc admin policy create local $3 /tmp/policy.json >/dev/null
mc admin user add local '$1' '$2' >/dev/null
mc admin policy attach local $3 --user '$1' >/dev/null 2>&1 || true
mc admin policy entities local --user '$1' | grep -q '$3'
rm -f /tmp/policy.json
echo "user with policy $3: ready"
EOF
}

robot_credential() {
  local user secret
  [[ -w "$ROBOT_ENV" ]] || fail "the robot file ($ROBOT_ENV) is not writable"
  user="$(env_get SWARMADA_STORE_ACCESS_KEY)"
  [[ -n "$user" ]] || fail "the robot file does not set SWARMADA_STORE_ACCESS_KEY (the robot's store user name)"
  secret="$(new_secret)"
  make_user "$user" "$secret" floor-maps-robot "$HERE/policy-robot.json" | sed 's/^/    /'
  env_put SWARMADA_STORE_SECRET_KEY "$secret"
  info "robot credential: SWARMADA_STORE_ACCESS_KEY's user, new SWARMADA_STORE_SECRET_KEY written to the robot file (not printed)"
}

client_certificate() {
  local secret="$ADAPTER-$NS-client-tls" values
  values="$(helm -n "$RELEASE_NS" get values "$RELEASE" -o json)"
  if ! grep -q "\"$ADAPTER\"" <<<"$values"; then
    HELM_VALUES="$values" python3 - "$ADAPTER" "$NS" > "$RUN_DIR/values-adapter-clients.json" <<'PY'
import json, os, sys
v = json.loads(os.environ["HELM_VALUES"])
clients = v.setdefault("controlStream", {}).setdefault("tls", {}).setdefault(
    "certManager", {}).setdefault("adapterClients", [])
clients.append({"name": sys.argv[1], "namespace": sys.argv[2]})
print(json.dumps({"controlStream": {"tls": {"certManager": {"adapterClients": clients}}}}))
PY
    helm upgrade "$RELEASE" "$REPO_ROOT/deploy/swarmada" -n "$RELEASE_NS" --reuse-values \
      -f "$RUN_DIR/values-adapter-clients.json" --wait --timeout 6m >/dev/null
    info "chart: adapterClients now issues $ADAPTER.$NS"
  fi
  wait_for "Secret $RELEASE_NS/$secret issued" 180 \
    kubectl -n "$RELEASE_NS" get secret "$secret" -o jsonpath='{.data.tls\.key}' ||
    fail "cert-manager never issued $secret"
  for f in ca.crt tls.crt tls.key; do
    kubectl -n "$RELEASE_NS" get secret "$secret" -o "jsonpath={.data.${f//./\\.}}" | base64 -d \
      > "$RUN_DIR/$f"
    chmod 600 "$RUN_DIR/$f"
  done
  openssl x509 -in "$RUN_DIR/tls.crt" -noout -ext subjectAltName 2>/dev/null | sed -n 's/^ *//p' |
    grep -q "DNS:$ADAPTER.$NS.svc.cluster.local" || fail "the issued certificate's SAN is not $ADAPTER.$NS"
  openssl verify -CAfile "$RUN_DIR/ca.crt" "$RUN_DIR/tls.crt" >/dev/null ||
    fail "the issued certificate does not verify against the chart's CA"
  info "client certificate: SAN $ADAPTER.$NS.svc.cluster.local, verified against the chart CA; ca.crt, tls.crt, tls.key in RUN_DIR (0600)"
}

install() {
  step "0 — preflight: the cluster is yours, and first-run is in it"
  for tool in kind kubectl docker helm openssl python3; do
    command -v "$tool" >/dev/null || fail "missing required tool: $tool"
  done
  cluster_kubeconfig
  require_owner
  kubectl get namespace "$NS" "$RELEASE_NS" >/dev/null 2>&1 ||
    fail "namespaces $NS and $RELEASE_NS are not both there: run examples/first-run in '$CLUSTER' first"
  kubectl get crd artifactstores.swarmada.io >/dev/null || fail "the ArtifactStore CRD is not installed"

  step "1 — the images: MinIO from source (Dockerfile.minio), the worker; loaded into kind"
  docker image inspect "$MINIO_IMG" >/dev/null 2>&1 ||
    docker build -f "$HERE/Dockerfile.minio" -t "$MINIO_IMG" "$HERE"
  docker build -q -f "$REPO_ROOT/examples/floor-mapping/worker/Dockerfile" -t "$WORKER_IMG" "$REPO_ROOT" >/dev/null
  kind load docker-image "$MINIO_IMG" "$WORKER_IMG" --name "$CLUSTER" >/dev/null
  info "loaded $MINIO_IMG and $WORKER_IMG"

  step "2 — MinIO, its root credential generated here and kept in the cluster"
  kubectl apply -f "$HERE/minio.yaml" >/dev/null
  if ! kubectl -n "$STORE_NS" get secret minio-root >/dev/null 2>&1; then
    kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata: {name: minio-root, namespace: $STORE_NS}
stringData: {user: "root-$(openssl rand -hex 6)", password: "$(new_secret)"}
EOF
  fi
  kubectl -n "$STORE_NS" rollout status deploy/minio --timeout=180s >/dev/null ||
    fail "MinIO did not become ready"
  info "MinIO ready in $STORE_NS"

  step "3 — bucket $BUCKET and the worker's user (read maps, write keep-out sets)"
  local wsecret
  wsecret="$(kubectl -n "$NS" get secret "$WORKER_USER" -o jsonpath='{.data.secret-key}' 2>/dev/null | base64 -d || true)"
  [[ -n "$wsecret" ]] || wsecret="$(new_secret)"
  make_user "$WORKER_USER" "$wsecret" floor-maps-worker "$HERE/policy-worker.json" | sed 's/^/    /'
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata: {name: $WORKER_USER, namespace: $NS}
stringData: {access-key: "$WORKER_USER", secret-key: "$wsecret"}
EOF
  info "Secret $NS/$WORKER_USER (access-key, secret-key)"

  step "4 — the ObjectStore overlay (ArtifactStore $BUCKET, the worker reading the store)"
  kubectl delete pv swarmada-floor-maps --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pvc floor-maps --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl apply -k "$HERE" >/dev/null
  kubectl -n "$NS" rollout restart deploy/zone-proposal-worker >/dev/null
  kubectl -n "$NS" rollout status deploy/zone-proposal-worker --timeout=180s >/dev/null ||
    fail "the zone-proposal worker did not become ready"
  info "ArtifactStore $NS/$BUCKET: $(kubectl -n "$NS" get artifactstore "$BUCKET" -o jsonpath='{.spec.kind} {.spec.objectStore.endpoint}')"

  step "5 — the ROS 2 adapter's FleetAdapter and robot class (examples/floor-mapping/robot)"
  kubectl apply -k "$REPO_ROOT/examples/floor-mapping/robot" >/dev/null
  info "FleetAdapter $NS/$ADAPTER, RobotClass $NS/ros2-nav2-v1"

  step "6 — the adapter's client certificate, from the chart's CA"
  client_certificate

  step "7 — this run's robot credential (the robot's user, scoped; a new secret)"
  robot_credential

  step "Result"
  info "store: $BUCKET in MinIO ($STORE_NS); robot user: write under $NS/ except keepouts-*, read only $NS/keepouts-*, no list, no delete"
  info "next: setup.sh forward $CLUSTER <owner-token>, then start the adapter with the flags the operator sheet gives"
}

# The interface the default route leaves by, unless LAN_IF names one.
lan_interface() {
  if [[ -n "${LAN_IF:-}" ]]; then printf '%s' "$LAN_IF"; return 0; fi
  if command -v route >/dev/null 2>&1 && route -n get default >/dev/null 2>&1; then
    route -n get default | awk '/interface:/ {print $2; exit}'
  else
    ip route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}'
  fi
  return 0
}

lan_address() {
  if [[ -n "${BIND_ADDR:-}" ]]; then printf '%s' "$BIND_ADDR"; return 0; fi
  local ifc
  ifc="$(lan_interface)"
  [[ -n "$ifc" ]] || return 0
  if command -v ipconfig >/dev/null 2>&1; then
    ipconfig getifaddr "$ifc" 2>/dev/null || true
  else
    ip -4 -o addr show dev "$ifc" 2>/dev/null | awk '{sub("/.*", "", $4); print $4; exit}'
  fi
  return 0
}

forward() {
  cluster_kubeconfig
  require_owner
  stop_forwards quiet
  local addr
  addr="$(lan_address)"
  [[ -n "$addr" ]] || fail "no LAN address on interface '$(lan_interface)' (set LAN_IF or BIND_ADDR)"
  nohup kubectl -n "$STORE_NS" port-forward --address "$addr" svc/minio 9000:9000 \
    > "$RUN_DIR/forward-store.log" 2>&1 &
  echo $! > "$RUN_DIR/forward-store.pid"
  nohup kubectl -n "$RELEASE_NS" port-forward --address "$addr" "svc/$CONTROL_SVC" 9445:9445 \
    > "$RUN_DIR/forward-control.log" 2>&1 &
  echo $! > "$RUN_DIR/forward-control.pid"
  umask 077
  cat > "$RUN_DIR/endpoints.env" <<EOF
SWARMADA_STORE_ENDPOINT=http://$addr:9000
SWARMADA_CONTROL_ENDPOINT=$addr:9445
SWARMADA_CONTROL_SERVER_NAME=$CONTROL_SVC.$RELEASE_NS.svc
EOF
  store_live() { [[ "$(curl -s -o /dev/null -w '%{http_code}' "http://$addr:9000/minio/health/live")" == 200 ]]; }
  control_open() { (exec 3<>"/dev/tcp/$addr/9445") 2>/dev/null; }
  wait_for "the store answers on the LAN address, port 9000 (health: live)" 30 store_live ||
    fail "the store forward did not come up (see $RUN_DIR/forward-store.log)"
  wait_for "the ControlStream listens on the LAN address, port 9445" 30 control_open ||
    fail "the ControlStream forward did not come up (see $RUN_DIR/forward-control.log)"
  info "forwarding on the LAN address of ${BIND_ADDR:+BIND_ADDR}${BIND_ADDR:-interface $(lan_interface)}; the endpoints are in RUN_DIR/endpoints.env (not printed)"
}

stop_forwards() {
  local f
  for f in "$RUN_DIR"/forward-*.pid; do
    [[ -f "$f" ]] || continue
    kill "$(cat "$f")" 2>/dev/null || true
    rm -f "$f"
  done
  [[ "${1:-}" == quiet ]] || info "forwards stopped"
}

rotate() {
  cluster_kubeconfig
  require_owner
  step "a new robot credential"
  robot_credential
  step "a new client certificate"
  kubectl -n "$RELEASE_NS" delete secret "$ADAPTER-$NS-client-tls" --ignore-not-found >/dev/null
  client_certificate
}

cmd="${1:-}"
case "$cmd" in
  install|forward|rotate)
    [[ $# -eq 3 ]] || usage
    CLUSTER="$2"; OWNER_TOKEN="$3"
    "$cmd" ;;
  stop) stop_forwards ;;
  *) usage ;;
esac
