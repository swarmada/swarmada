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

# The outside world raises events: signed webhooks become SemanticEvents, and
# TaskTriggers turn them into DRAFT FleetTasks that a person clears.
#
#   examples/event-bridge/run.sh
#
# 1. examples/first-run brings up the control plane (with cert-manager) and admits the
#    simulated robot.
# 2. One HMAC key per caller is generated and stored in Secret event-bridge-keys. No
#    key is written to a file or passed on a command line.
# 3. The cluster starts the bridge (a Deployment), with its sources, triggers and
#    admission policy (examples/event-bridge/overlay).
# 4. Signed events are sent through a port-forward: a door opening after hours and in
#    hours, a work order, a schedule start, a camera detection; the same door event
#    delivered twice; and the refusals (unknown caller, bad signature, replay, stale
#    timestamp, a type the caller may not send, a type outside the vocabulary, a zone
#    the bridge was not configured with).
# 5. The result: SemanticEvents, the drafts they produced, and no FleetAction. The
#    script stops there. It never clears a draft; it prints the command a person runs.
#
# Environment:
#   EVENT_BRIDGE_CLUSTER       kind cluster name (default swarmada-event-bridge). An
#                              existing cluster is used when EVENT_BRIDGE_USE_EXISTING=1.
#   EVENT_BRIDGE_SKIP_INSTALL  1 = the control plane is already installed in that
#                              cluster; skip examples/first-run.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${EVENT_BRIDGE_CLUSTER:-swarmada-event-bridge}"
NS="warehouse-a"
IMG="swarmada-event-bridge:dev"
HERE="examples/event-bridge"
PORT="${EVENT_BRIDGE_LOCAL_PORT:-18443}"
SERVER_NAME="event-bridge.$NS.svc"
CALLERS=(door-controller work-orders scheduler camera-adapter)

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

wait_for() {
  local what="$1" timeout="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 2
  done
  info "$what"
}

step "0 — Preflight"
command -v go >/dev/null || fail "go is required (the signer is built from this example)"
command -v openssl >/dev/null || fail "openssl is required (it generates the keys)"
command -v python3 >/dev/null || fail "python3 is required (it computes the event times)"

step "1 — Control plane and robot (examples/first-run)"
if [[ "${EVENT_BRIDGE_SKIP_INSTALL:-0}" != 1 ]]; then
  if [[ "${EVENT_BRIDGE_USE_EXISTING:-0}" == 1 ]]; then
    FIRST_RUN_CLUSTER="$CLUSTER" FIRST_RUN_USE_EXISTING=1 examples/first-run/run.sh
  else
    FIRST_RUN_CLUSTER="$CLUSTER" examples/first-run/run.sh
  fi
fi
WORK="$(mktemp -d)"
PF_PID=""
cleanup() {
  if [[ -n "$PF_PID" ]]; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT
kind get kubeconfig --name "$CLUSTER" >"$WORK/kubeconfig"
export KUBECONFIG="$WORK/kubeconfig"

step "2 — One key per caller, in Secret event-bridge-keys"
# The keys live in shell variables and reach kubectl on stdin, so none is written to
# a file or appears in a process listing. Re-running generates new keys and the
# bridge is restarted to load them.
# key_var CALLER — the shell variable holding CALLER's key (bash 3.2 has no
# associative arrays).
key_var() { printf 'KEY_%s' "${1//-/_}"; }
for c in "${CALLERS[@]}"; do printf -v "$(key_var "$c")" '%s' "$(openssl rand -hex 32)"; done
{
  printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: event-bridge-keys\n  namespace: %s\ntype: Opaque\nstringData:\n' "$NS"
  for c in "${CALLERS[@]}"; do v="$(key_var "$c")"; printf '  %s: "%s"\n' "$c" "${!v}"; done
} | kubectl apply -f - >/dev/null
info "Secret event-bridge-keys: ${CALLERS[*]}"

step "3 — The bridge, its sources, triggers and admission policy"
docker build -q -f "$HERE/Dockerfile" -t "$IMG" . >/dev/null
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null
kubectl apply -k "$HERE/overlay" >/dev/null
kubectl -n "$NS" wait certificate/event-bridge-tls --for=condition=Ready --timeout=120s >/dev/null
kubectl -n "$NS" rollout restart deploy/event-bridge >/dev/null
kubectl -n "$NS" rollout status deploy/event-bridge --timeout=180s >/dev/null
info "bridge serving; its Role:"
kubectl -n "$NS" get role event-bridge -o jsonpath='{range .rules[*]}      {.verbs} {.resources}{"\n"}{end}'
kubectl -n "$NS" get secret event-bridge-tls -o jsonpath='{.data.ca\.crt}' | base64 -d >"$WORK/ca.crt"
go build -o "$WORK/event-bridge" "./$HERE"
kubectl -n "$NS" port-forward svc/event-bridge "$PORT:443" >/dev/null 2>&1 &
PF_PID=$!
bridge_up() { curl -sf --cacert "$WORK/ca.crt" --connect-to "$SERVER_NAME:$PORT:127.0.0.1:$PORT" "https://$SERVER_NAME:$PORT/healthz"; }
wait_for "port-forward up on localhost:$PORT" 30 bridge_up || fail "the bridge does not answer through the port-forward"

# send CALLER [flags] — signs with CALLER's key, which reaches the signer in its
# environment only. A caller with no key signs with a random one.
send() {
  local caller="$1" v; shift
  v="$(key_var "$caller")"
  EVENT_BRIDGE_KEY="${!v:-$(openssl rand -hex 32)}" "$WORK/event-bridge" send \
    -url "https://127.0.0.1:$PORT/v1/events" -ca "$WORK/ca.crt" -server-name "$SERVER_NAME" \
    -key-id "$caller" "$@" | tee -a "$WORK/sent.log" | sed 's/^/    /'
}
# The most recent HH:00 UTC at or before now, so the event is inside the bridge's
# 24-hour limit and on the intended side of the working hours (07:00-19:00 UTC).
last_utc() {
  python3 -c 'import datetime as d,sys; n=d.datetime.now(d.timezone.utc); t=n.replace(hour=int(sys.argv[1]),minute=0,second=0,microsecond=0); t-=d.timedelta(days=1) if t>n else d.timedelta(0); print(t.strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1"
}
RUN="$(date +%s)"

step "4a — Events the bridge maps"
info "door north-7 opened at $(last_utc 2) (after hours):"
send door-controller -type door.opened -id "door-north-7-$RUN" -occurred-at "$(last_utc 2)" -data door=north-7 -resend
info "door north-7 opened at $(last_utc 10) (in hours):"
send door-controller -type door.opened -id "door-north-7-day-$RUN" -occurred-at "$(last_utc 10)" -data door=north-7
info "work order naming zone warehouse-a:"
send work-orders -type workorder.created -id "WO-$RUN" -data "workOrder=WO-$RUN" -data zone=warehouse-a -data task=clean
info "the mapping schedule started:"
send scheduler -type schedule.started -id "mapping-$RUN" -data schedule=mapping-segment
info "a static camera saw a cable in warehouse-a:"
send camera-adapter -type camera.detection -id "cam-1-cable-$RUN" -data class=cable -data zone=warehouse-a \
  -data confidence=0.870 -data camera=cam-1 -data model=floor-hazards -data modelVersion=v3

step "4b — Refusals"
info "a caller the bridge holds no key for:"
send facilities-bot -type door.opened -id "x-$RUN" -data door=north-7
info "a body changed after signing:"
send door-controller -type door.opened -id "tamper-$RUN" -data door=north-7 -tamper
info "the identical signed request, twice (a replay):"
send door-controller -type door.opened -id "replay-$RUN" -occurred-at "$(last_utc 3)" -data door=north-7 -repeat 2
info "a timestamp ten minutes old:"
send door-controller -type door.opened -id "stale-$RUN" -data door=north-7 -timestamp-offset -10m
info "the door controller sending a work order:"
send door-controller -type workorder.created -id "wo-x-$RUN" -data workOrder=1 -data zone=warehouse-a -data task=clean
info "a type outside the vocabulary:"
send door-controller -type door.closed -id "closed-$RUN" -data door=north-7
info "a work order for a zone the bridge was not configured with:"
send work-orders -type workorder.created -id "WO-roof-$RUN" -data workOrder=1 -data zone=roof -data task=clean
info "a field the type does not declare:"
send door-controller -type door.opened -id "extra-$RUN" -data door=north-7 -data draft=false

step "5 — What reached the cluster"
# Only this run's events: the names the bridge answered "created" with.
EVENTS="$(grep -o '"result":"created","semanticEvent":"evb-[0-9a-f]*"' "$WORK/sent.log" |
  sed 's/.*"\(evb-[0-9a-f]*\)"/\1/' | sort -u)"
sleep 3
{
  printf 'EVENT\tSOURCE\tTYPE\tPHASE\tDRAFTED\n'
  for e in $EVENTS; do
    kubectl get semanticevent "$e" -n "$NS" \
      -o jsonpath='{.metadata.name}{"\t"}{.spec.source}{"\t"}{.spec.type}{"\t"}{.status.phase}{"\t"}{.status.draftedTasks}{"\n"}'
  done
} | column -t -s "$(printf '\t')" | sed 's/^/    /'
DRAFTS=""
for e in $EVENTS; do
  DRAFTS="$DRAFTS $(kubectl get fleettasks -n "$NS" -l "swarmada.io/drafted-from=$e" -o name)"
done
n=0
for t in $DRAFTS; do
  n=$((n + 1))
  [[ "$(kubectl get "$t" -n "$NS" -o jsonpath='{.status.phase}')" == Draft ]] || fail "$t is not held as a draft"
  [[ -z "$(kubectl get "$t" -n "$NS" -o jsonpath='{.status.actions}')" ]] || fail "$t has child actions"
done
# after-hours door x2 (the resent one and the replayed one, one draft each), work
# order, schedule, camera; the in-hours door drafts nothing.
[[ "$n" == 5 ]] || fail "expected 5 drafts from this run's 6 events, found $n"
info "6 events, 5 drafts, all held with no child action: nothing has been dispatched"

PATROL="$(for t in $DRAFTS; do echo "$t"; done | grep door-after-hours-patrol | head -1)"
step "Result"
echo "PASS: signed outside events became SemanticEvents and held drafts; the same event"
echo "      delivered twice made one draft; the bridge refused every request it could not map."
echo
echo "A person clears a draft; nothing here does. To run the after-hours patrol:"
echo "  kubectl patch ${PATROL} -n $NS --type=merge -p '{\"spec\":{\"draft\":false}}'"
