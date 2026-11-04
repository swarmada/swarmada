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

# make first-run — the secure first run, end to end, on a new kind cluster.
#
# Admission webhooks ON and ControlStream authorization ON, which is the posture the
# Helm chart installs by default. Every step below is run by this script; the adapter
# is a Deployment the cluster starts, so nothing depends on a process started by hand.
#
#   1. kind cluster                 (a new one; an existing cluster of the name is refused)
#   2. cert-manager                 (issues the webhook and ControlStream certificates)
#   3. images, built from source    (manager + simulation adapter), loaded into kind
#   4. helm install deploy/swarmada (asserts the rendered manager args first)
#   5. config/samples/first-run     (once admission serves: namespace; ConfigMap, SwarmadaConfig,
#                                    RobotClass; then FleetZone, FleetAdapter)
#   6. the in-cluster sim adapter   (examples/first-run/sim-adapter)
#   7. wait: FleetAdapter Connected/Passed, then the DiscoveredRobot
#   8. swarmctl admit --class       (the Robot is created through the admission gate)
#   9. the FleetTask, to Succeeded
#
# Environment:
#   FIRST_RUN_CLUSTER      kind cluster name (default: swarmada-first-run)
#   FIRST_RUN_USE_EXISTING 1 = use an existing, empty kind cluster of that name instead
#                          of creating one (for a cluster created and owned elsewhere)
#   FIRST_RUN_DELETE       1 = delete the cluster at the end if this script created it
#   CERT_MANAGER_VERSION   default v1.16.2, the version the other examples pin
#
# The cluster is left running on success, so the result can be inspected. Nothing
# here writes a status subresource.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${FIRST_RUN_CLUSTER:-swarmada-first-run}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.16.2}"
RELEASE="swarmada"
RELEASE_NS="swarmada-system"
NS="warehouse-a"
ADAPTER="sim-fleet-adapter"
ROBOT="sim-robot-001"
CLASS="simbot-v1"
TASK="first-run"
MANAGER_IMG_REPO="swarmada-controller"
MANAGER_IMG_TAG="dev"
ADAPTER_IMG="swarmada-sim-adapter:dev"

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# wait_for DESCRIPTION TIMEOUT_SECONDS COMMAND... — poll until COMMAND succeeds.
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

CREATED_CLUSTER=0
KCFG=""
cleanup() {
  local rc=$?
  if ((rc != 0)) && [[ -n "$KCFG" ]]; then
    echo "" >&2
    echo "---- diagnostics (exit $rc) ----" >&2
    kubectl get fleetadapters,discoveredrobots,robots,fleettasks,fleetactions -n "$NS" 2>&1 | sed 's/^/    /' >&2 || true
    kubectl -n "$RELEASE_NS" get pods 2>&1 | sed 's/^/    /' >&2 || true
    kubectl -n "$RELEASE_NS" logs deploy/"$RELEASE"-controller-manager --tail=40 2>&1 | sed 's/^/    /' >&2 || true
    kubectl -n "$RELEASE_NS" logs deploy/"$ADAPTER" --tail=20 2>&1 | sed 's/^/    /' >&2 || true
  fi
  if [[ "${FIRST_RUN_DELETE:-0}" == 1 && "$CREATED_CLUSTER" == 1 ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  [[ -n "$KCFG" ]] && rm -f "$KCFG"
  exit "$rc"
}
trap cleanup EXIT

step "0 — Preflight"
for tool in kind kubectl docker helm go make; do
  have "$tool" || fail "missing required tool: $tool"
done
docker info >/dev/null 2>&1 || fail "the Docker daemon is not reachable — start Docker and re-run"
info "kind, kubectl, docker, helm, go and make present; Docker daemon reachable"

step "1 — kind cluster '$CLUSTER'"
# Every kind, kubectl, helm and swarmctl call below uses this kubeconfig file and
# nothing else, so no context in the caller's kubeconfig is read, added or switched.
KCFG="$(mktemp)"
# Here-strings, not pipes, into every `grep -q` below: under pipefail, grep -q exits on its
# first match, the writer is killed by SIGPIPE, and the pipeline reports failure for a match.
if grep -qx "$CLUSTER" <<<"$(kind get clusters 2>/dev/null)"; then
  [[ "${FIRST_RUN_USE_EXISTING:-0}" == 1 ]] ||
    fail "a kind cluster named '$CLUSTER' already exists. This script does not reuse or delete a cluster it did not create: delete it yourself, set FIRST_RUN_CLUSTER to another name, or set FIRST_RUN_USE_EXISTING=1 to run in it"
  info "using the existing cluster (FIRST_RUN_USE_EXISTING=1)"
  kind get kubeconfig --name "$CLUSTER" >"$KCFG"
else
  kind create cluster --name "$CLUSTER" --kubeconfig "$KCFG" --wait 120s
  CREATED_CLUSTER=1
fi
export KUBECONFIG="$KCFG"
if [[ "$CREATED_CLUSTER" == 0 ]] && kubectl get namespace "$RELEASE_NS" >/dev/null 2>&1; then
  fail "namespace $RELEASE_NS already exists in '$CLUSTER'; first-run needs an empty cluster"
fi

step "2 — cert-manager $CERT_MANAGER_VERSION"
kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" >/dev/null
for d in cert-manager cert-manager-cainjector cert-manager-webhook; do
  kubectl -n cert-manager rollout status "deploy/$d" --timeout=300s >/dev/null
done
# Available is not serving: a Certificate created before cert-manager's own webhook
# answers is refused. A server-side dry run of an Issuer goes through that webhook.
cm_webhook_serving() {
  kubectl create --dry-run=server -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: first-run-probe, namespace: default}
spec: {selfSigned: {}}
EOF
}
wait_for "cert-manager ready (its webhook answers)" 120 cm_webhook_serving ||
  fail "cert-manager's webhook never answered"

step "3 — Build the images from source and load them into kind"
make docker-build IMG="$MANAGER_IMG_REPO:$MANAGER_IMG_TAG"
docker build -f Dockerfile.sim-adapter -t "$ADAPTER_IMG" .
kind load docker-image "$MANAGER_IMG_REPO:$MANAGER_IMG_TAG" --name "$CLUSTER"
kind load docker-image "$ADAPTER_IMG" --name "$CLUSTER"
go build -o bin/swarmctl ./cmd/swarmctl
info "images loaded; bin/swarmctl built"

HELM_ARGS=(
  "$RELEASE" deploy/swarmada --namespace "$RELEASE_NS"
  --set "image.repository=$MANAGER_IMG_REPO" --set "image.tag=$MANAGER_IMG_TAG"
  --set image.pullPolicy=IfNotPresent
  --set "controlStream.tls.certManager.adapterClients[0].name=$ADAPTER"
  --set "controlStream.tls.certManager.adapterClients[0].namespace=$NS"
)

step "4 — helm install $RELEASE (webhooks on, ControlStream mTLS on)"
rendered="$(helm template "${HELM_ARGS[@]}")"
args="$(printf '%s\n' "$rendered" | grep -E '^\s+- --' || true)"
for flag in --fleet-adapter-tls-cert-file --fleet-adapter-tls-key-file --fleet-adapter-client-ca-file; do
  grep -q -- "$flag=" <<<"$args" || fail "the rendered manager args lack $flag"
done
if grep -q -- '--fleet-adapter-insecure-authz' <<<"$args"; then
  fail "the rendered manager args carry --fleet-adapter-insecure-authz"
fi
grep -q 'value: "true"' <<<"$(printf '%s\n' "$rendered" | grep -A1 'name: ENABLE_WEBHOOKS')" ||
  fail "the rendered manager does not set ENABLE_WEBHOOKS=true"
info "rendered: the three --fleet-adapter-tls-* flags present, --fleet-adapter-insecure-authz absent, ENABLE_WEBHOOKS=true"
helm install "${HELM_ARGS[@]}" --create-namespace --wait --timeout 6m >/dev/null
live_args="$(kubectl -n "$RELEASE_NS" get deploy "$RELEASE-controller-manager" -o jsonpath='{.spec.template.spec.containers[0].args}')"
if grep -q -- '--fleet-adapter-insecure-authz' <<<"$live_args"; then
  fail "the installed manager Deployment carries --fleet-adapter-insecure-authz"
fi
manager_serves_mtls() {
  grep -Eq 'ControlStream telemetry live feed enabled.*mtls.{0,4}true' \
    <<<"$(kubectl -n "$RELEASE_NS" logs "deploy/$RELEASE-controller-manager")"
}
wait_for "manager serves ControlStream with mTLS (its log says so)" 120 manager_serves_mtls ||
  fail "the manager never logged that it serves ControlStream with mTLS"
if grep -q 'NOT serving ControlStream' <<<"$(kubectl -n "$RELEASE_NS" logs deploy/"$RELEASE-controller-manager")"; then
  fail "the manager logged NOT serving ControlStream"
fi

step "5 — Apply config/samples/first-run"
kubectl create namespace "$NS" >/dev/null
# Apply only after admission is serving, and in two phases. The set's SwarmadaConfig and
# FleetZone go through webhooks that fail closed, and its FleetAdapter and RobotClass go
# through none, and kubectl applies the rest of a set when one object is refused. An
# apply that meets a refusal is therefore a PARTIAL apply: the FleetAdapter lands, the
# bootstrap controller creates a default swarmada-config for the namespace (signature
# verification required, no trust root), and the sample's own config arrives later as
# an overwrite. So wait for every condition admission needs, then apply the objects that
# cannot start the bootstrap, confirm the config exists, and only then apply the
# FleetZone and FleetAdapter, the two kinds the bootstrap controller watches.
WEBHOOK_SVC="$RELEASE-webhook-service"
kubectl wait --for=condition=Established --timeout=60s \
  crd/swarmadaconfigs.swarmada.io crd/robotclasses.swarmada.io \
  crd/fleetzones.swarmada.io crd/fleetadapters.swarmada.io >/dev/null ||
  fail "the swarmada.io CRDs the sample uses were not Established within 60s"
info "CRDs Established"
webhook_endpoint_ready() {
  [[ -n "$(kubectl -n "$RELEASE_NS" get endpointslices -l "kubernetes.io/service-name=$WEBHOOK_SVC" \
    -o jsonpath='{.items[*].endpoints[?(@.conditions.ready==true)].addresses[0]}' 2>/dev/null)" ]]
}
wait_for "Service $RELEASE_NS/$WEBHOOK_SVC has a ready endpoint" 120 webhook_endpoint_ready ||
  fail "Service $RELEASE_NS/$WEBHOOK_SVC has no ready endpoint after 120s"
# A ready endpoint is not enough: cert-manager's CA injector fills each webhook
# configuration's caBundle on its own schedule, and until it does the API server cannot
# verify the webhook's certificate and refuses every call.
webhook_ca_injected() {
  local kind
  for kind in mutatingwebhookconfiguration validatingwebhookconfiguration; do
    [[ -n "$(kubectl get "$kind/$RELEASE-${kind%webhookconfiguration}-webhook-configuration" \
      -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null)" ]] || return 1
  done
}
wait_for "both webhook configurations carry the CA bundle" 120 webhook_ca_injected ||
  fail "the webhook configurations have no caBundle after 120s (cert-manager's CA injector)"
# The end-to-end check: a server-side dry run of the sample's SwarmadaConfig goes through
# the mutating and validating webhooks and persists nothing (every webhook declares
# sideEffects: None).
admission_serving() {
  kubectl apply --dry-run=server -f config/samples/first-run/swarmadaconfig.yaml
}
wait_for "admission answers (server-side dry run of the SwarmadaConfig)" 120 admission_serving ||
  fail "admission never answered a server-side dry run: $(admission_serving 2>&1 | tail -3)"
info "admission serving at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
# One render, split by kind. The kustomization's sortOptions order stays for anyone who
# runs a plain `kubectl apply -k`; here the phases set the order.
PHASE1_KINDS="ConfigMap SwarmadaConfig RobotClass"
PHASE2_KINDS="FleetZone FleetAdapter"
SAMPLE_DIR="$(mktemp -d)"
kubectl kustomize config/samples/first-run > "$SAMPLE_DIR/all.yaml"
# split_kinds KIND... — print the rendered documents whose top-level kind is one of KIND.
split_kinds() {
  awk -v want=" $* " '
    function flush() { if (doc != "" && index(want, " " kind " ")) printf "---\n%s", doc; doc = ""; kind = "" }
    /^---$/ { flush(); next }
    /^kind: / { kind = $2 }
    { doc = doc $0 "\n" }
    END { flush() }' "$SAMPLE_DIR/all.yaml"
}
for k in $(grep '^kind: ' "$SAMPLE_DIR/all.yaml" | cut -d' ' -f2); do
  [[ " $PHASE1_KINDS $PHASE2_KINDS " == *" $k "* ]] ||
    fail "config/samples/first-run renders a $k, which neither apply phase lists"
done
split_kinds $PHASE1_KINDS > "$SAMPLE_DIR/phase1.yaml"
split_kinds $PHASE2_KINDS > "$SAMPLE_DIR/phase2.yaml"
apply_out="$(kubectl apply -f "$SAMPLE_DIR/phase1.yaml" 2>&1)" ||
  fail "phase 1 ($PHASE1_KINDS) was refused with admission serving (not retried):
$apply_out"
info "phase 1 applied ($PHASE1_KINDS) at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
kubectl -n "$NS" get swarmadaconfig swarmada-config >/dev/null ||
  fail "swarmada-config is not in $NS after phase 1; not applying the FleetZone or FleetAdapter"
info "config present at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
apply_out="$(kubectl apply -f "$SAMPLE_DIR/phase2.yaml" 2>&1)" ||
  fail "phase 2 ($PHASE2_KINDS) was refused with admission serving (not retried):
$apply_out"
info "phase 2 applied ($PHASE2_KINDS) at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
rm -rf "$SAMPLE_DIR"

step "6 — Start the in-cluster simulation adapter (a Deployment; nothing is run by hand)"
wait_for "adapter client certificate issued" 120 \
  kubectl -n "$RELEASE_NS" get secret "$ADAPTER-$NS-client-tls" ||
  fail "the chart never issued Secret $ADAPTER-$NS-client-tls"
kubectl apply -k examples/first-run/sim-adapter >/dev/null
kubectl -n "$RELEASE_NS" rollout status "deploy/$ADAPTER" --timeout=180s >/dev/null
info "Deployment $RELEASE_NS/$ADAPTER running"

step "7 — Wait for the adapter to be Connected with conformance Passed, and the robot discovered"
adapter_ready() {
  [[ "$(jp "fleetadapter/$ADAPTER" "$NS" '{.status.phase}/{.status.conformance}')" == "Connected/Passed" ]]
}
wait_for "FleetAdapter $ADAPTER Connected/Passed" 180 adapter_ready ||
  fail "FleetAdapter $ADAPTER is $(jp "fleetadapter/$ADAPTER" "$NS" '{.status.phase}/{.status.conformance}: {.status.message}')"
cv="$(jp "fleetadapter/$ADAPTER" "$NS" '{.status.conformanceContractVersion}')"
[[ -n "$cv" ]] || fail "FleetAdapter $ADAPTER has no status.conformanceContractVersion"
info "status.conformanceContractVersion=$cv"
wait_for "DiscoveredRobot $ROBOT announced" 120 kubectl get discoveredrobot "$ROBOT" -n "$NS" ||
  fail "no DiscoveredRobot $ROBOT appeared"

step "8 — swarmctl admit robot $ROBOT --class $CLASS"
bin/swarmctl admit robot "$ROBOT" -n "$NS" --zone "$NS" --adapter "$ADAPTER" --class "$CLASS" --yes
wait_for "Robot $ROBOT created by admission" 120 kubectl get robot "$ROBOT" -n "$NS" ||
  fail "Robot $ROBOT was not created"
robot_idle() { [[ "$(jp "robot/$ROBOT" "$NS" '{.status.phase}')" == "Idle" ]]; }
wait_for "Robot $ROBOT Idle" 180 robot_idle ||
  fail "Robot $ROBOT is $(jp "robot/$ROBOT" "$NS" '{.status.phase}'), not Idle"
[[ "$(jp "robot/$ROBOT" "$NS" '{.spec.robotClass}')" == "$CLASS" ]] ||
  fail "Robot $ROBOT has spec.robotClass '$(jp "robot/$ROBOT" "$NS" '{.spec.robotClass}')', want $CLASS"
info "Robot $ROBOT: class $CLASS, adapter $(jp "robot/$ROBOT" "$NS" '{.spec.adapter.name}')"

# The adapter's catalog reaches FleetAdapter.status once the robot is admitted: the
# adapter re-registers on its provisioning-loop interval and then sends it (5 s here, a
# demo setting in examples/first-run/sim-adapter; the RFC default is 60 s). Every
# capability an action in that catalog requires is one the admitted robot holds Active,
# or a client composing a task from the catalog builds one no robot can take.
# The check is by action type, not by entry: the catalog carries one entry per Custom verb
# (contract 1.4.0), so its entry count moves when the adapter gains a verb, and the six
# types do not.
catalog_types="Charge Custom DropOff Navigate Patrol PickUp"
catalog_projected() {
  [[ "$(jp "fleetadapter/$ADAPTER" "$NS" '{.status.supportedActions[*].actionType}' |
    tr ' ' '\n' | sed '/^$/d' | sort -u | tr '\n' ' ' | sed 's/ $//')" == "$catalog_types" ]]
}
wait_for "FleetAdapter $ADAPTER lists its six action types" 120 catalog_projected ||
  fail "FleetAdapter $ADAPTER lists [$(jp "fleetadapter/$ADAPTER" "$NS" '{.status.supportedActions[*].actionType}')], want the six action types $catalog_types"
active="$(jp "robot/$ROBOT" "$NS" '{range .status.capabilities[?(@.status=="Active")]}{.name}{" "}{end}')"
for cap in $(jp "fleetadapter/$ADAPTER" "$NS" '{.status.supportedActions[*].requiredCapabilities[*]}' | tr ' ' '\n' | sort -u); do
  [[ " $active " == *" $cap "* ]] ||
    fail "the catalog requires capability $cap; Robot $ROBOT holds Active only: $active"
done
info "catalog: $(jp "fleetadapter/$ADAPTER" "$NS" '{.status.supportedActions[*].actionType}'); every required capability Active on $ROBOT"

step "9 — FleetTask $TASK, to Succeeded"
kubectl apply -f config/samples/first-run/fleettask.yaml >/dev/null
# Stops polling on any terminal phase; the check after it decides pass or fail.
task_terminal() {
  case "$(jp "fleettask/$TASK" "$NS" '{.status.phase}')" in
    Succeeded | Failed | Cancelled | Compensated) return 0 ;;
    *) return 1 ;;
  esac
}
wait_for "FleetTask $TASK reached a terminal phase" 600 task_terminal ||
  fail "FleetTask $TASK is $(jp "fleettask/$TASK" "$NS" '{.status.phase} {.status.actionSummary}') after 600s"
phase="$(jp "fleettask/$TASK" "$NS" '{.status.phase}')"
[[ "$phase" == "Succeeded" ]] ||
  fail "FleetTask $TASK ended $phase: $(jp "fleettask/$TASK" "$NS" '{.status.actionSummary}')"
info "FleetTask $TASK Succeeded ($(jp "fleettask/$TASK" "$NS" '{.status.actionSummary}'))"

step "Result"
kubectl get fleetadapter,robot,fleettask,fleetaction -n "$NS" 2>&1 | sed 's/^/    /'
echo ""
echo "PASS: a robot admitted through the admission webhooks ran FleetTask $TASK to Succeeded"
echo "      over an mTLS ControlStream with per-robot authorization (class $CLASS)."
if [[ "$CREATED_CLUSTER" == 1 && "${FIRST_RUN_DELETE:-0}" != 1 ]]; then
  echo "      The cluster is still running: kind delete cluster --name $CLUSTER"
fi
