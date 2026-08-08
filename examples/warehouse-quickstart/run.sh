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
# warehouse-quickstart — the 5-minute simulated-fleet quickstart.
#
# Brings up a real Swarmada control plane on a local kind cluster, applies the
# maintained sample fleet (config/samples/demo_a.yaml — one FleetZone, three
# simulated Robots, two FleetActions), drives the robots to a ready state, and lets
# the REAL scheduler assign the tasks. The end-state assertion (Step 7) then
# waits for that outcome and prints ✅/❌.
#
# Readiness note (honest): TWO paths exist, chosen automatically.
#
#   - healthy-fleet (default) or CI ($CI=true, e.g. `make quickstart-test`):
#     status-projected — robots are driven Ready by projecting their status
#     (kubectl patch phase=Idle), the same mechanism the maintained `make
#     demo-b` uses. Deterministic and fast; this is what CI gates on.
#
#   - any other scenario, chosen interactively or via --scenario, and NOT in
#     CI: LIVE — one robot (sim-robot-001) is driven by a real, running
#     adapters/simulation/sim_adapter.py process talking to the real
#     ControlStream over a port-forward, so the chosen scenario's fault/
#     battery/comms/estop behaviour actually happens, live, on that robot.
#     The other two robots stay status-projected so the 3-robot scheduler
#     story is unaffected. This deploys via config/overlays/quickstart-dev
#     (DEV/DEMO ONLY — disables ControlStream per-robot authorization; see
#     that overlay's README comment and cmd/manager/main.go's
#     --fleet-adapter-insecure-authz flag help) instead of config/default.
#
# Either way, capabilities are declared in the sample robots' spec and derived
# Active by the Capability Controller — that part is always fully real, and so
# is the scheduler's assignment.
#
# On success it prints ✅ and exits 0; if the end state is not reached within the
# timeouts it prints ❌ with diagnostics and exits 1 — this exit code is the
# assertion CI gates on (see `make quickstart-test`). The cluster is left running
# so you can explore it; a teardown command is printed at the end.
#
# Usage:
#   examples/warehouse-quickstart/run.sh                      bring up + assert; interactive
#                                                             scenario picker on a TTY
#   examples/warehouse-quickstart/run.sh --scenario NAME      non-interactive (CI / video);
#                                                             NAME is a simulated-fleet scenario
#   examples/warehouse-quickstart/run.sh --robots N           fleet size for the printed sim-adapter command
#   examples/warehouse-quickstart/run.sh --pace keypress|timed|off   pacing before the LIVE scenario
#                                                             actually starts (see below); default:
#                                                             keypress on the LIVE path, off otherwise
#   examples/warehouse-quickstart/run.sh --pause-seconds N    duration for --pace timed (default: 8)
#   examples/warehouse-quickstart/run.sh --help               full flag help
#
# Pacing (--pace): rolling straight through every step is fine for a quick
# check, but not for presenting live — a human needs a beat before the actual
# payoff to narrate it. So there is exactly ONE pause, right before the LIVE
# scenario's real payoff (the moment the live sim adapter actually starts, and
# the scenario clock begins counting down to --fault-at etc.): "keypress"
# (default on the LIVE path) waits for Enter; "timed" sleeps
# --pause-seconds/$QUICKSTART_PAUSE_SECONDS (default 8) instead; "off" skips
# it. Always off under CI=true, regardless of --pace. Deliberately not
# scattered across every step (0/7 .. 7/7) — that dilutes attention instead of
# focusing it on the one thing actually worth watching.
#
# Env knobs:
#   QUICKSTART_CLUSTER  kind cluster name        (default: swarmada-quickstart)
#   QUICKSTART_IMG      controller image tag     (default: swarmada-controller:dev)
#   QUICKSTART_READY_TIMEOUT   secs to wait for robots Idle    (default: 15)
#   QUICKSTART_ASSIGN_TIMEOUT  secs to wait for a task Assigned (default: 15)
#   QUICKSTART_CS_PORT  local port for the ControlStream port-forward (live path only, default: 19443)
#   QUICKSTART_PAUSE_SECONDS   duration for --pace timed        (default: 8)
#   CI=true             forces the status-projected path (and disables pacing) regardless of --scenario/--pace
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${QUICKSTART_CLUSTER:-swarmada-quickstart}"
# Must match the image tag config/default/kustomization.yaml pins (swarmada-controller:dev),
# so `make deploy` / the quickstart-dev overlay schedules the image we build and load below.
IMG="${QUICKSTART_IMG:-swarmada-controller:dev}"
NS="warehouse-a"
SAMPLE="config/samples/demo_a.yaml"
MANAGER_NS="system"
MANAGER_DEPLOY="swarmada-controller-manager"
ROBOTS=(sim-robot-001 sim-robot-002 sim-robot-003)
# 15s defaults are tuned for THIS quickstart's fleet, which is 100% simulated
# (status-projection, or adapters/simulation/sim_adapter.py on the LIVE path)
# — no real robot hardware, no real network, no real controller boot time in
# the loop. If this script (or a variant of it) is ever pointed at a real
# adapter (adapters/external/fleet-adapter-*), raise these back up via the
# env vars below rather than assuming 15s holds — real hardware handshake,
# comms, and startup latency will not fit in 15s.
#
# The same applies to CONFIGURATION, not just hardware. These timings are validated
# only at the simulation adapter's default telemetry cadence. Setting
# Robot.spec.telemetryIntervalSeconds (or RobotClass.spec.defaultTelemetry) makes the
# adapter stream at that rate instead — the control plane returns it on RegisterAck and
# the adapter honours it. At the CRD maximum of 30s the 20s first-telemetry wait below
# expires before the first payload arrives, and the script takes its soft-fail path.
# No sample here sets the field, which is why the defaults hold; raise the waits if you
# add one.
READY_TIMEOUT="${QUICKSTART_READY_TIMEOUT:-15}"
ASSIGN_TIMEOUT="${QUICKSTART_ASSIGN_TIMEOUT:-15}"
LOCAL_CS_PORT="${QUICKSTART_CS_PORT:-19443}"
LIVE_ROBOT="sim-robot-001"
PIDFILE="/tmp/swq-${CLUSTER}.pids"
PF_LOG="/tmp/swq-${CLUSTER}-portforward.log"
ADAPTER_LOG="/tmp/swq-${CLUSTER}-adapter.log"

# Simulated-fleet scenario (adapters/scenarios). Selected via the interactive picker
# or the --scenario flag; healthy-fleet is the default (and what CI runs).
SCENARIOS=(healthy-fleet battery-edge battery-handoff hardware-fault comms-flaky estop-drill full-surface)
SCENARIO=""              # set by --scenario or the picker
ROBOTS_N=3                # --robots: fleet size for the printed sim-adapter command (informational)
LIVE=0                    # set by determine_live_mode
LIVE_PIDS=()               # background PIDs started by this run (port-forward, adapter)
PACE=""                    # set by --pace; "" means "resolve the default in determine_live_mode"
PAUSE_SECONDS="${QUICKSTART_PAUSE_SECONDS:-8}"
CLEAN_REQUESTED=0          # set by --clean or the picker's "clean everything" choice
CLUSTER_HINT_SHOWN=0       # guards remind_to_clean_cluster against printing twice

step()  { echo; echo "==> $*"; }
info()  { echo "    $*"; }
fail()  { echo "❌ $*" >&2; exit 1; }
have()  { command -v "$1" >/dev/null 2>&1; }

# kill_forcefully PID: SIGTERM, then SIGKILL after a short grace period if the
# process is still alive — used for both this run's own cleanup and cleaning
# up a stale PID left by a previous invocation that didn't exit cleanly.
# Plain SIGTERM alone can leave a hung/ignoring process behind, which is
# exactly the kind of leak that would corrupt the NEXT run regardless of
# which scenario it picks.
kill_forcefully() {
  local pid="$1" waited=0
  kill -0 "$pid" >/dev/null 2>&1 || return 0   # already gone
  kill "$pid" >/dev/null 2>&1 || true
  while kill -0 "$pid" >/dev/null 2>&1 && (( waited < 5 )); do
    sleep 1; waited=$((waited + 1))
  done
  kill -0 "$pid" >/dev/null 2>&1 && kill -9 "$pid" >/dev/null 2>&1 || true
}

cleanup_live_processes() {
  local pid i
  # Kill in REVERSE start order: the adapter (started last) before its port-forward
  # (started first), so a graceful adapter — which installs a SIGTERM handler that
  # closes both gRPC streams cleanly — shuts down while its transport is still up. That
  # keeps the adapter log quiet on a normal finish instead of emitting an UNAVAILABLE
  # "stream dropped; reconnecting" burst when the port-forward is torn out from under it.
  for (( i=${#LIVE_PIDS[@]}-1; i>=0; i-- )); do
    pid="${LIVE_PIDS[$i]}"
    [[ -n "$pid" ]] && kill_forcefully "$pid"
  done
  rm -f "$PIDFILE"
  # Fires on every exit path — success, ❌ failure, or Ctrl-C — not just the
  # happy path teardown_hint prints on. If do_clean_everything already ran
  # (CLEAN_REQUESTED), the cluster is already gone, so skip the reminder.
  ((CLEAN_REQUESTED)) || remind_to_clean_cluster
}
trap cleanup_live_processes EXIT

# remind_to_clean_cluster: the one place that tells the user how to tear the
# cluster down. Called from teardown_hint() on success, and from the EXIT trap
# on any other exit (failure, Ctrl-C) so the reminder shows up no matter how
# the script stopped. Guarded so it never prints twice in one run.
remind_to_clean_cluster() {
  ((CLUSTER_HINT_SHOWN)) && return 0
  CLUSTER_HINT_SHOWN=1
  echo
  echo "Remember to clean up the '$CLUSTER' cluster when you're done with it:"
  echo "    examples/warehouse-quickstart/run.sh --clean"
  echo "or:"
  echo "    kind delete cluster --name $CLUSTER"
}

# ── Scenario selection (front the choice before dialing the control plane) ──────

usage() {
  cat <<EOF
warehouse-quickstart — the 5-minute simulated-fleet quickstart.

Usage: run.sh [--scenario NAME] [--robots N] [--pace keypress|timed|off]
              [--pause-seconds N] [--clean] [-h|--help]

  --scenario NAME    simulated-fleet scenario; one of:
                       ${SCENARIOS[*]}
                     (default: healthy-fleet). Passing a non-default scenario
                     outside CI takes the LIVE path (see the header note) —
                     for CI or a fully scripted/deterministic run, set CI=true.
  --robots N         fleet size for the printed sim-adapter command (default: 3).
  --pace MODE        pacing before the LIVE scenario's real payoff starts:
                       keypress (default on the LIVE path) — wait for Enter
                       timed                                — sleep --pause-seconds
                       off                                  — no pause
                     Always off under CI=true regardless of this flag.
  --pause-seconds N  duration for --pace timed (default: 8, or \$QUICKSTART_PAUSE_SECONDS).
  --clean            tear down the '$CLUSTER' kind cluster, kill any live/stale
                     port-forward or adapter processes, remove local pidfile/log
                     state, and exit — does not bring anything up. Same as
                     picking "7" in the interactive picker.
  -h, --help         show this help and exit.

Env knobs: QUICKSTART_CLUSTER, QUICKSTART_IMG, QUICKSTART_CS_PORT,
           QUICKSTART_READY_TIMEOUT, QUICKSTART_ASSIGN_TIMEOUT,
           QUICKSTART_PAUSE_SECONDS, CI.
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
      --robots) ROBOTS_N="${2:-}"; shift 2 ;;
      --robots=*) ROBOTS_N="${1#*=}"; shift ;;
      --pace) PACE="${2:-}"; shift 2 ;;
      --pace=*) PACE="${1#*=}"; shift ;;
      --pause-seconds) PAUSE_SECONDS="${2:-}"; shift 2 ;;
      --pause-seconds=*) PAUSE_SECONDS="${1#*=}"; shift ;;
      --clean) CLEAN_REQUESTED=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) usage >&2; fail "unknown argument: $1" ;;
    esac
  done
  [[ "$ROBOTS_N" =~ ^[1-9][0-9]*$ ]] || fail "--robots must be a positive integer (got '$ROBOTS_N')"
  [[ -z "$PACE" || "$PACE" =~ ^(keypress|timed|off)$ ]] \
    || fail "--pace must be one of: keypress, timed, off (got '$PACE')"
  [[ "$PAUSE_SECONDS" =~ ^[0-9]+$ ]] || fail "--pause-seconds must be a non-negative integer (got '$PAUSE_SECONDS')"
}

# pick_scenario resolves SCENARIO: an explicit --scenario wins; otherwise, on a TTY,
# show the interactive numbered picker; with no TTY (CI / piped) fall back to the
# default. The picker is a convenience layer on top of the flag, never a second path.
pick_scenario() {
  if [[ -n "$SCENARIO" ]]; then
    valid_scenario "$SCENARIO" || fail "unknown scenario '$SCENARIO'; choose one of: ${SCENARIOS[*]}"
    return
  fi
  if [[ ! -t 0 ]]; then
    SCENARIO="healthy-fleet"   # non-interactive default (CI / recorded video)
    return
  fi
  echo
  echo "Pick a scenario to run against your local Swarmada cluster:"
  echo "  1) healthy-fleet     — 3 robots, nothing goes wrong (default)"
  echo "  2) battery-edge      — watch the scheduler avoid a dying robot"
  echo "  3) battery-handoff   — a working robot's battery drops mid-task; watch its"
  echo "                         task get safely handed off to a healthy robot"
  echo "  4) hardware-fault    — camera fails mid-task, watch it reroute and recover"
  echo "  5) comms-flaky       — a robot drops and reconnects"
  echo "  6) estop-drill       — trigger and confirm an emergency stop"
  echo "  7) clean everything  — tear down the cluster, kill any live processes, and exit"
  local choice=""
  read -rp "> " choice || true
  case "${choice:-1}" in
    1|"") SCENARIO="healthy-fleet" ;;
    2) SCENARIO="battery-edge" ;;
    3) SCENARIO="battery-handoff" ;;
    4) SCENARIO="hardware-fault" ;;
    5) SCENARIO="comms-flaky" ;;
    6) SCENARIO="estop-drill" ;;
    7|clean) CLEAN_REQUESTED=1; return ;;
    healthy-fleet|battery-edge|battery-handoff|hardware-fault|comms-flaky|estop-drill) SCENARIO="$choice" ;;
    *) fail "invalid choice '$choice' — pick 1–7 or a scenario name" ;;
  esac
}

# determine_live_mode: LIVE=1 drives $LIVE_ROBOT with a real sim_adapter.py
# process against the real ControlStream; LIVE=0 keeps the original
# status-projection path. CI always forces LIVE=0 regardless of scenario, so
# `make quickstart-test` stays exactly as deterministic as before this change.
#
# Also resolves PACE's default when --pace wasn't given: keypress on the LIVE
# path (there's an actual payoff worth a beat before it starts), off
# otherwise (nothing to pace on the status-projected path). CI forces off
# either way — see pause_for_demo.
determine_live_mode() {
  if [[ "$SCENARIO" != "healthy-fleet" && "${CI:-}" != "true" ]]; then
    LIVE=1
  fi
  if [[ -z "$PACE" ]]; then
    if [[ "$LIVE" == 1 ]]; then
      PACE="keypress"
    else
      PACE="off"
    fi
  fi
  # Explicit, unconditional tail: this function is called as a bare statement
  # under `set -e`, so its own return status is whatever its last command
  # returns. A trailing `[[ cond ]] && assignment` would make that status
  # depend on $cond — false, with no `else`, makes the function itself
  # "fail" and silently kills the whole script right here (this bit us: the
  # healthy-fleet/non-LIVE path hit exactly this and exited with no message
  # between the scenario picker and Step 0). `return 0` removes the hazard.
  return 0
}

# pause_for_demo MESSAGE: the one deliberate pacing point in this script,
# called only right before the LIVE scenario's real payoff (see
# run_live_scenario). Never blocks CI. "keypress" falls back to a short fixed
# sleep if stdin isn't a TTY (e.g. --pace keypress under a pipe) so the script
# can't hang forever waiting for input nobody can give it.
pause_for_demo() {
  local message="$1"
  [[ "${CI:-}" == "true" || "$PACE" == "off" ]] && return 0
  echo
  echo "    $message"
  if [[ "$PACE" == "timed" ]]; then
    echo "    continuing in ${PAUSE_SECONDS}s…"
    sleep "$PAUSE_SECONDS"
    return 0
  fi
  if [[ -t 0 ]]; then
    read -rp "    press Enter to continue... " _ || true
  else
    echo "    no TTY for --pace keypress; pausing ${PAUSE_SECONDS}s instead"
    sleep "$PAUSE_SECONDS"
  fi
}

# scenario_payoff_description NAME — one line explaining what actually happens
# once the live scenario starts, so the pause_for_demo prompt tells whoever's
# watching (presenter or solo operator) what to look for next instead of just
# "press Enter." Keep in sync with the picker's one-liners in pick_scenario
# and the README's Scenarios table — this is the same story, just phrased as
# "what happens after you continue" instead of "what this preset is."
scenario_payoff_description() {
  case "$1" in
    healthy-fleet)
      echo "$LIVE_ROBOT will report a steady battery and go Idle — nothing goes wrong." ;;
    battery-edge)
      echo "$LIVE_ROBOT's reported battery will drain on the scenario's curve — watch the" \
           "scheduler steer new task assignments away from it as it gets low." ;;
    battery-handoff)
      echo "$LIVE_ROBOT will drain and stay excluded, same as battery-edge. The real payoff" \
           "comes after: once things settle, watch whichever robot is holding the sample" \
           "task get its battery cut and its task safely handed off to the other one." ;;
    hardware-fault)
      echo "$LIVE_ROBOT comes online with its camera capability Inactive (no camera hardware yet), so" \
           "the camera task stays Pending. Its camera_front hardware is then marked Healthy — the" \
           "capability derives Active and the scheduler assigns it the camera task. You'll then be" \
           "prompted ONCE MORE to degrade the camera (this script drives the hardware status, same as" \
           "battery-handoff drives the battery): the capability degrades, and the control plane" \
           "automatically safe-stops the task and REROUTES it to the idle spare camera robot," \
           "sim-robot-002." ;;
    comms-flaky)
      echo "$LIVE_ROBOT's ControlStream will drop and reconnect on a timer. This is a" \
           "distributed-systems payoff, NOT a status-field one: the robot's Kubernetes status" \
           "does not change (per-robot liveness is kept off the status write path, RA-1), so" \
           "swarmtop correctly shows nothing moving. Watch the ADAPTER + manager LOGS instead" \
           "— the stream drops, the adapter self-stops its task on lease expiry (C4), reconnects" \
           "with a fresh Hello/Register, and its fencing token + lease survive the outage (C3)." ;;
    estop-drill)
      echo "$LIVE_ROBOT will trigger a real, confirmed emergency stop once — watch the" \
           "EstopAck and the task get dropped to a safe hold." ;;
    full-surface)
      echo "$LIVE_ROBOT runs the coverage timeline: camera degrades→fails→recovers (~8-16s)," \
           "then its ControlStream drops and reconnects (~22-30s), then a confirmed estop (~36s)" \
           "— watch every field move through swarmtop's views in one run." ;;
    *)
      echo "$LIVE_ROBOT will start streaming real telemetry over the live ControlStream." ;;
  esac
}

# scenario_watch_command NAME — a suggested command to run in a SEPARATE
# terminal while this scenario plays out, so the field that actually matters
# is visible instead of buried. Robot's printer columns are Phase/Zone/
# Battery/Task/Class/Age only (api/v1/robot_types.go) — status.hardware[],
# status.capabilities[], status.estopState, and status.connectivity are NOT
# columns, so `kubectl get robots` alone can't show them.
#
# For the scenarios whose payoff lives in a field kubectl can't column-ize
# (hardware-fault → status.hardware/capabilities, estop-drill →
# status.estopState, full-surface → both), this prefers tools/swarmtop's robot
# detail view (`--robot $LIVE_ROBOT`) when a swarmtop binary is available (on
# PATH, or built at tools/swarmtop/bin/swarmtop via `make -C tools/swarmtop
# build`), and otherwise falls back to the field-specific `-o json | jq`
# one-liner. comms-flaky is deliberately NOT in that set: its payoff is stream
# lifecycle (drop/reconnect, lease self-stop C4, fencing-token survival C3),
# which is a LOGS story — the robot's status does not change (per-robot liveness
# is kept off the status write path, RA-1), so status.connectivity is never
# written and neither swarmtop nor a kubectl field would show anything move; it
# points at the adapter + manager logs instead. The remaining scenarios keep
# plain `watch kubectl get`, since Battery/Phase/Task already have printer
# columns. swarmtop is strictly OPTIONAL — nothing here requires it to be installed.

# swarmtop_bin — absolute path to a usable swarmtop binary, or empty when none
# is installed (PATH first, then the in-tree build).
swarmtop_bin() {
  if command -v swarmtop >/dev/null 2>&1; then
    command -v swarmtop
  elif [ -x "$REPO_ROOT/tools/swarmtop/bin/swarmtop" ]; then
    echo "$REPO_ROOT/tools/swarmtop/bin/swarmtop"
  fi
}

scenario_watch_command() {
  case "$1" in
    healthy-fleet)
      echo "watch -n1 kubectl get robots,fleetactions -n $NS" ;;
    battery-edge)
      echo "watch -n1 kubectl get robots -n $NS" ;;
    battery-handoff)
      echo "watch -n1 kubectl get robots,fleetactions -n $NS" ;;
    comms-flaky)
      # comms-flaky's payoff is stream lifecycle (drop→reconnect→re-register with
      # fencing/lease intact), which lives in the LOGS, not a status field —
      # status.connectivity is never written (RA-1), so neither swarmtop nor a
      # `kubectl -o json` field would show anything move.
      echo "tail -f $ADAPTER_LOG   # control plane: kubectl logs -n $MANAGER_NS deploy/$MANAGER_DEPLOY -f" ;;
    hardware-fault|estop-drill|full-surface)
      local st; st="$(swarmtop_bin)"
      if [ -n "$st" ]; then
        echo "$st -n $NS --robot $LIVE_ROBOT"
      else
        case "$1" in
          hardware-fault)
            echo "watch -n1 'kubectl get robot $LIVE_ROBOT -n $NS -o json | jq" \
                 "\"{phase: .status.phase, hardware: .status.hardware, capabilities: .status.capabilities}\"'" ;;
          estop-drill)
            echo "watch -n1 'kubectl get robot $LIVE_ROBOT -n $NS -o json | jq" \
                 "\"{phase: .status.phase, estopState: .status.estopState}\"'" ;;
          full-surface)
            echo "watch -n1 'kubectl get robot $LIVE_ROBOT -n $NS -o json | jq" \
                 "\"{phase: .status.phase, hardware: .status.hardware, connectivity: .status.connectivity, estopState: .status.estopState}\"'" ;;
        esac
      fi ;;
    *)
      echo "watch -n1 kubectl get robots -n $NS" ;;
  esac
}

# adapter_scenario_preset NAME — the adapters/scenarios preset to actually pass
# to sim_adapter.py's --scenario for $LIVE_ROBOT. Almost always identical to
# the script's own $SCENARIO, EXCEPT battery-handoff: there is no
# adapters/scenarios/presets/battery-handoff.yaml (sim_adapter.py hard-errors
# on an unknown --scenario name) — battery-handoff is script-level
# orchestration (see handle_battery_handoff), layered on top of $LIVE_ROBOT
# just running the real battery-edge preset underneath, so it stays drained
# and excluded exactly like the battery-edge scenario.
adapter_scenario_preset() {
  case "$1" in
    battery-handoff) echo "battery-edge" ;;
    *) echo "$1" ;;
  esac
}

# announce_scenario prints the resolved choice — and, in LIVE mode, exactly what
# is about to run live — before any cluster work.
announce_scenario() {
  step "Scenario — $SCENARIO"
  info "the control plane, CRDs, and scheduler are identical for every scenario."
  if [[ "$ROBOTS_N" -ne "${#ROBOTS[@]}" ]]; then
    info "note: this quickstart uses the fixed ${#ROBOTS[@]}-robot sample fleet; --robots"
    info "      $ROBOTS_N is otherwise unused."
  fi
  if [[ "$LIVE" == 1 ]]; then
    info ""
    info "LIVE mode: $LIVE_ROBOT will be driven by a real sim_adapter.py process"
    info "against the real ControlStream (--scenario $SCENARIO). The other two"
    info "robots stay status-projected so the scheduler still has a 3-robot fleet."
    info "This deploys via config/overlays/quickstart-dev (DEV/DEMO ONLY — see"
    info "that overlay and cmd/manager/main.go's --fleet-adapter-insecure-authz)."
  else
    info ""
    info "status-projected mode: readiness is projected (kubectl patch), not"
    info "adapter-driven — see the Honest notes in README.md. To watch '$SCENARIO'"
    info "live yourself, run:"
    info "    python3 adapters/simulation/sim_adapter.py --endpoint <fleet-adapter-endpoint> \\"
    info "      --namespace $NS --robot-id sim-robot-001 --scenario $SCENARIO"
  fi
}

preflight() {
  step "0/7 — Preflight"
  local missing=0
  for tool in kind kubectl docker make go; do
    if ! have "$tool"; then echo "  missing required tool: $tool" >&2; missing=1; fi
  done
  if [[ "$LIVE" == 1 ]] && ! have python3 && ! have python; then
    echo "  missing required tool: python3 (needed for the LIVE scenario path)" >&2
    missing=1
  fi
  ((missing == 0)) || fail "install the missing tools and re-run (see examples/warehouse-quickstart/README.md)"
  if ! docker info >/dev/null 2>&1; then
    fail "the Docker daemon is not reachable — start Docker and re-run"
  fi
  if [[ "$LIVE" == 1 && ! -f "proto/fleet_adapter/v1/fleet_adapter_pb2.py" ]]; then
    info "generating Python proto stubs (proto/fleet_adapter/v1/*_pb2*.py) — needed for the LIVE scenario path…"
    make proto-py
  fi
  info "required tools present; Docker daemon reachable"
}

# cleanup_stale_run: kill any port-forward/adapter processes left running by a
# previous invocation of this script against the same cluster that didn't exit
# cleanly (e.g. killed with SIGKILL, terminal closed, machine slept).
#
# kill_pidfile_processes does the actual work; cleanup_stale_run (used at the
# top of a normal run) and do_clean_everything (the explicit --clean /
# picker-"6" path) both call it so there is exactly one place that knows how
# to find and kill this script's tracked background processes.
kill_pidfile_processes() {
  if [[ -f "$PIDFILE" ]]; then
    local pid found=0
    while read -r pid; do
      [[ -n "$pid" ]] || continue
      if kill -0 "$pid" >/dev/null 2>&1; then
        info "killing process (pid $pid)"
        kill_forcefully "$pid"
        found=1
      fi
    done < "$PIDFILE"
    ((found)) || info "no live processes found"
  else
    info "no pidfile — nothing to clean up"
  fi
  : > "$PIDFILE"
}

cleanup_stale_run() {
  step "0.5/7 — Clean up any leftover processes from a previous run"
  kill_pidfile_processes
}

# do_clean_everything: the explicit reset — --clean, or "6" in the picker.
# Tears down the kind cluster entirely (which takes every namespace, CRD, and
# resource in it with it — the most thorough clean available) plus anything
# this script itself left running or on disk. Does not bring anything back up.
do_clean_everything() {
  step "Clean everything — tearing down '$CLUSTER' and removing local state"
  kill_pidfile_processes
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    info "deleting kind cluster '$CLUSTER'…"
    kind delete cluster --name "$CLUSTER"
  else
    info "no kind cluster named '$CLUSTER' — nothing to delete"
  fi
  rm -f "$PIDFILE" "$PF_LOG" "$ADAPTER_LOG"
  # The adapter's client PRIVATE KEY was extracted here; do not leave it in /tmp.
  rm -rf "/tmp/swq-${CLUSTER}-adapter-tls"
  info "clean ✓ — no cluster, no tracked processes, no local pidfile/log state"
}

# cluster_is_healthy: the API server responds, and — if the manager Deployment
# already exists from a previous run — it is Available. A cluster with no
# manager yet (first run against a fresh kind cluster) counts as healthy;
# there is nothing to be unclean about yet.
cluster_is_healthy() {
  kubectl cluster-info >/dev/null 2>&1 || return 1
  if kubectl -n "$MANAGER_NS" get "deployment/$MANAGER_DEPLOY" >/dev/null 2>&1; then
    kubectl -n "$MANAGER_NS" rollout status "deployment/$MANAGER_DEPLOY" --timeout=10s >/dev/null 2>&1 || return 1
  fi
  return 0
}

create_cluster() {
  step "1/7 — Create kind cluster ($CLUSTER)"
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    kubectl config use-context "kind-$CLUSTER" >/dev/null 2>&1 || true
    if cluster_is_healthy; then
      info "cluster '$CLUSTER' already exists and looks healthy — reusing it"
    else
      info "cluster '$CLUSTER' exists but is not clean/healthy — deleting and recreating"
      kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
      kind create cluster --name "$CLUSTER"
    fi
  else
    kind create cluster --name "$CLUSTER"
  fi
  kubectl config use-context "kind-$CLUSTER" >/dev/null
}

build_and_load() {
  step "2/7 — Build the controller-manager image ($IMG)"
  make docker-build IMG="$IMG"
  step "3/7 — Load the image into the kind cluster"
  kind load docker-image "$IMG" --name "$CLUSTER"
}

# cert-manager issues the ControlStream server certificate AND the adapter client
# certificate the LIVE path depends on. Pinned rather than "latest" so a demo does
# not change under you between runs.
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.16.2}"
ADAPTER_TLS_DIR="/tmp/swq-${CLUSTER}-adapter-tls"

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

# wait_for_adapter_client_cert: block until cert-manager has issued the adapter's
# client keypair, then write it where the adapter can read it. The SAN in this
# certificate is what the control plane treats as the adapter's name.
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

install_control_plane() {
  step "4/7 — Install CRDs and deploy the control plane"
  if [[ "$LIVE" == 1 ]]; then
    # LIVE needs a MUTUALLY-AUTHENTICATED ControlStream, not merely an encrypted
    # one. The adapter's identity is read from its client certificate's SAN
    # (<adapter>.<namespace>.svc.cluster.local); with no verified identity
    # internal/command/dispatcher.go refuses to register the stream, so the
    # manager can ingest telemetry but never push a command back — ADR-0019's
    # validate_action then fails unreachable for every robot and nothing is ever
    # assigned. That is why this path cannot use the plaintext quickstart-dev
    # overlay.
    install_cert_manager
    info "LIVE mode — deploying via config/overlays/quickstart-mtls (DEV/DEMO ONLY)"
    kubectl apply -k config/overlays/quickstart-mtls
    wait_for_adapter_client_cert
  else
    # Projected mode runs NO adapter process, so the manager must run with
    # ControlStream OFF. config/default (what `make deploy` applies) leaves it
    # on, which makes cmd/manager wire up a non-nil ActionValidator; ADR-0019's
    # validate_action probe then asks the fabricated sim-fleet-adapter whether
    # it can serve each action, finds no stream behind it, and drops every
    # candidate robot — so nothing is ever assigned and this runner times out.
    # config/overlays/quickstart-projected is config/default plus that one flag;
    # it carries the same CRDs + RBAC + manager and still needs no cert-manager.
    info "projected mode — deploying via config/overlays/quickstart-projected (ControlStream off)"
    kubectl apply -k config/overlays/quickstart-projected
  fi
  info "waiting for the controller-manager to become Available…"
  kubectl -n "$MANAGER_NS" rollout status "deployment/$MANAGER_DEPLOY" --timeout=180s
}

# reset_namespace_state: always start from a clean slate for $NS, regardless of
# what a previous run (possibly a different scenario, possibly LIVE vs not) left
# behind — a reused cluster must never let stale Robot/FleetAction status leak
# into this run's assertion.
reset_namespace_state() {
  step "4.5/7 — Reset namespace $NS to a clean slate"
  if kubectl get namespace "$NS" >/dev/null 2>&1; then
    info "deleting any leftover Robots/FleetActions from a previous run"
    kubectl delete robots,fleetactions -n "$NS" --all --ignore-not-found --wait=true >/dev/null 2>&1 || true
  else
    info "namespace $NS does not exist yet — nothing to reset"
  fi
}

# sample_docs_excluding KIND NAME — every doc in $SAMPLE except the one
# matching KIND/NAME, printed with its '---' separator restored. Plain text
# splitting on the doc separator (no PyYAML dependency — LIVE mode requires
# python3 but not the yaml package, see sim_adapter.py's graceful fallback).
# Used only by battery-handoff, to keep one sample robot genuinely spare —
# see apply_fleet and handle_battery_handoff.
sample_docs_excluding() {
  local kind="$1" name="$2"
  python3 - "$SAMPLE" "$kind" "$name" <<'PY'
import sys
path, kind, name = sys.argv[1], sys.argv[2], sys.argv[3]
for doc in open(path).read().split("\n---\n"):
    if f"kind: {kind}" in doc and f"name: {name}" in doc:
        continue
    print("---")
    print(doc.strip())
PY
}

apply_fleet() {
  step "5/7 — Apply the sample fleet (reuses $SAMPLE)"
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
  if [[ "$SCENARIO" == "battery-handoff" ]]; then
    # Apply only ONE of the two sample FleetActions. With both applied, both
    # non-dying robots (sim-robot-002/003) claim one task each immediately —
    # leaving no spare healthy robot for the mid-task hand-off to land on.
    # See handle_battery_handoff for where this plays out.
    sample_docs_excluding FleetAction inspect-receiving-dock | kubectl apply -f -
    info "applied 1 FleetZone, ${#ROBOTS[@]} Robots, and 1 of 2 sample FleetActions"
    info "(inspect-receiving-dock is intentionally not applied this scenario — see Step 6)"
  elif [[ "$SCENARIO" == "estop-drill" ]]; then
    # The drill's whole point is that THE ROBOT BEING STOPPED is the one doing the
    # work, so both must be unambiguous on screen. Applying both sample actions
    # left the other one running on a different robot, so a viewer watching
    # swarmtop saw "the task" on the wrong robot and nothing appeared to line up
    # with the narration. Apply only inspect-receiving-dock, and steer it onto
    # $LIVE_ROBOT with spec.preferredRobot (ADR-0034 — a soft preference the
    # scheduler honours when that robot is eligible), so the one task on screen
    # is the one that gets stopped.
    sample_docs_excluding FleetAction deliver-pallet-001 | kubectl apply -f -
    kubectl patch fleetaction/inspect-receiving-dock -n "$NS" --type=merge \
      -p "{\"spec\":{\"preferredRobot\":\"$LIVE_ROBOT\"}}"
    info "applied 1 FleetZone, ${#ROBOTS[@]} Robots, and 1 of 2 sample FleetActions"
    info "(deliver-pallet-001 is intentionally not applied; inspect-receiving-dock prefers $LIVE_ROBOT)"
  elif [[ "$SCENARIO" == "hardware-fault" ]]; then
    # Capability-loss reroute setup (all scenario-specific, so config/samples is
    # untouched): apply only the camera task (inspect-receiving-dock) and hold
    # deliver-pallet-001 back, so the spare robot stays idle for the reroute to
    # land on. Then make sim-robot-002/003 nav-only by stripping camera_front
    # from spec.capabilities — so the camera task can ONLY be scheduled onto the
    # live camera robot ($LIVE_ROBOT), deterministically. handle_hardware_fault
    # re-adds camera_front to sim-robot-002 as the idle spare once $LIVE_ROBOT
    # holds the task.
    sample_docs_excluding FleetAction deliver-pallet-001 | kubectl apply -f -
    local nav_only='[{"op":"replace","path":"/spec/capabilities","value":[{"name":"navigation","type":"hardware-native","pauseable":false}]}]'
    kubectl patch robot/sim-robot-002 -n "$NS" --type=json -p "$nav_only"
    kubectl patch robot/sim-robot-003 -n "$NS" --type=json -p "$nav_only"

    # Gate $LIVE_ROBOT's camera_front CAPABILITY on a camera_front HARDWARE
    # component. The sample's capabilities are ungated (always Active), so a
    # camera fault would degrade only status.hardware and never the capability the
    # Scheduler reads — no reroute. Declaring the hardware component and pointing
    # the capability's requiredHardware at it makes the derivation degrade the
    # capability when the adapter reports camera_front DEGRADED (T+30s), which is
    # what triggers Capability-loss reassignment. The adapter reports camera_front
    # HEALTHY before the fault, so the capability is Active pre-fault (the task can
    # land on $LIVE_ROBOT) and Degraded only during the window.
    kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --type=json -p '[
      {"op":"add","path":"/spec/hardware","value":[{"name":"camera_front","type":"Camera"}]},
      {"op":"replace","path":"/spec/capabilities","value":[
        {"name":"navigation","type":"hardware-native","pauseable":false},
        {"name":"camera_front","type":"hardware-native","pauseable":false,"requiredHardware":["camera_front"]}
      ]}]'
    info "applied 1 FleetZone, ${#ROBOTS[@]} Robots (002/003 nav-only; $LIVE_ROBOT camera gated on hardware), and 1 of 2 FleetActions"
    info "(deliver-pallet-001 held back; camera restored to sim-robot-002 mid-scenario — see Step 6.5)"
  else
    kubectl apply -f "$SAMPLE"
    info "applied 1 FleetZone, ${#ROBOTS[@]} Robots, and the sample FleetActions"
  fi
  apply_fleet_adapter
}

# apply_fleet_adapter creates the FleetAdapter the sample robots are bound to
# (spec.adapter.name = sim-fleet-adapter).
#
# Required since ADR-0032's assignment gate: dispatch withholds any robot whose bound FleetAdapter
# is absent, not Connected, not conformance-Passed, or qualified against an unsupported contract
# version. Without this the scheduler correctly refuses every robot and no FleetAction is ever
# assigned — which is exactly how this runner failed ("FleetAdapter \"sim-fleet-adapter\" not found").
apply_fleet_adapter() {
  # The CRD DEFAULTS signing.requireSignatureVerification to true, so a namespace config that does
  # not mention signing still demands a SIGNED conformance report and fails the adapter closed.
  # This demo exercises the digest path and the dispatch gate, not signature distribution.
  kubectl apply -f - >/dev/null <<EOF
apiVersion: swarmada.io/v1
kind: SwarmadaConfig
metadata: {name: swarmada-config, namespace: $NS}   # singleton — the CRD requires this exact name
spec:
  signing:
    requireSignatureVerification: false
  telemetry:
    sink:
      # Leaving this unset is an OBSERVED-DEGRADED state, not a neutral default: the controller
      # raises a TelemetrySinkUnconfigured warning because unset is indistinguishable from a sink
      # that was meant to be configured and was forgotten. This cluster has no metrics store, so
      # Drop is the honest answer — discarding high-cadence telemetry ON PURPOSE. Robot status
      # projection (RobotStatusSink) is a separate path and is unaffected.
      type: Drop
EOF

  # A REAL digest-verified conformance report: the FleetAdapter controller hashes the ConfigMap
  # body and compares it to spec.conformanceReport.digest, so status.conformance reaches Passed
  # through the production path rather than being projected.
  local report digest
  report='{"adapter":"simulation","conformant":true,"contract_version":"1.0.0"}'
  digest="sha256:$(printf '%s' "$report" | shasum -a 256 | awk '{print $1}')"
  kubectl create configmap sim-adapter-conformance -n "$NS" \
    --from-literal=report.json="$report" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  kubectl apply -f - >/dev/null <<EOF
apiVersion: swarmada.io/v1
kind: FleetAdapter
metadata: {name: sim-fleet-adapter, namespace: $NS}
spec:
  vendor: simulation
  # Where the live sim adapter connects on the live path; inert on the projected path. Required by
  # the CRD, so it is always set.
  endpoint: "127.0.0.1:${LOCAL_CS_PORT}"
  # Long, because nothing refreshes lastHeartbeat here: this runner is insecure, so ControlStream
  # sees no verified mTLS identity and AdapterPresence never fires. A short interval would let the
  # staleness backstop demote the projected Connected mid-run and silently stop dispatch.
  heartbeatIntervalSeconds: 600
  conformanceReport:
    suiteVersion: "C1-C15"
    configMapRef: sim-adapter-conformance
    digest: "$digest"
EOF

  # phase=Connected CANNOT be reached the real way here (see the heartbeat note above), so it is
  # projected — the same honest projection this runner already uses for the non-live robots.
  kubectl patch fleetadapter/sim-fleet-adapter -n "$NS" --subresource=status --type=merge \
    -p "{\"status\":{\"phase\":\"Connected\",\"lastHeartbeat\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}" >/dev/null
  info "FleetAdapter sim-fleet-adapter ready (digest-verified conformance; phase projected Connected)"
}

project_readiness() {
  step "6/7 — Drive the robots Ready (status projection — see header note)"
  # phase=Idle with status.connectivity left nil is stable: the reconciler's
  # heartbeat-timeout→Offline branch only fires when connectivity.lastSeenAt is
  # set, so it never overwrites this. Capabilities are derived Active from the
  # robots' spec.capabilities by the Capability Controller.
  for r in "${ROBOTS[@]}"; do
    kubectl patch "robot/$r" -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"phase":"Idle"}}'
  done
}

# portforward_ready: true once kubectl's port-forward has printed its
# "Forwarding from" line for our local port.
portforward_ready() {
  grep -q "Forwarding from 127.0.0.1:${LOCAL_CS_PORT}" "$PF_LOG" 2>/dev/null
}

# run_live_scenario: LIVE path. $LIVE_ROBOT is driven by a real sim_adapter.py
# process against the real ControlStream (port-forwarded from the
# quickstart-dev overlay's Service); the other two robots stay
# status-projected (with a healthy baseline battery, so a battery-edge/
# hardware-fault story on the live robot reads as a real contrast, not an
# artifact of the other two having no battery reported at all).
#
# Phase honesty note: connection, registration, and telemetry for $LIVE_ROBOT
# are all genuinely real. Phase itself is NOT — as of this writing the control
# plane has no code path anywhere that moves a Robot from Discovered to Idle
# on its own. internal/controller/robot_controller.go's reconciler only ever
# writes Discovered (default) or Offline (heartbeat timeout); the Health
# Monitor (RFC-0001 §9.3.3) is documented to write phase only to Offline
# ("no further phase transition"); internal/controller/robot_status_sink.go
# explicitly refuses to write Phase at all. The only writer of Idle anywhere
# in the codebase is fleetaction_controller.go, on task completion — which
# presupposes the robot was already Idle. So below, once real telemetry from
# $LIVE_ROBOT is observed (proof the live handshake actually worked), this
# script projects Idle itself, exactly like the other two robots — there is
# currently no other way to get there. This is a real product gap (a
# first-registration/heartbeat → Idle admission transition), not a
# quickstart shortcut; track it before calling the live path "real".
run_live_scenario() {
  step "6/7 — Drive the robots ready: $LIVE_ROBOT is LIVE (scenario '$SCENARIO')"
  local r
  for r in "${ROBOTS[@]}"; do
    [[ "$r" == "$LIVE_ROBOT" ]] && continue
    kubectl patch "robot/$r" -n "$NS" --subresource=status --type=merge \
      -p '{"status":{"phase":"Idle","batteryPercent":85}}'
  done

  # RobotStatusSink (internal/controller/robot_status_sink.go) only projects
  # telemetry onto a Robot that carries this annotation matching the wire
  # robot_id — without it, real telemetry from $LIVE_ROBOT is silently
  # dropped (by design: an unmapped robot_id must not wedge ingestion) and
  # nothing observable ever lands in status, live_robot_telemetry_seen below
  # included.
  kubectl annotate "robot/$LIVE_ROBOT" -n "$NS" "swarmada.io/robot-id=$LIVE_ROBOT" --overwrite >/dev/null

  info "port-forwarding to the ControlStream Service (127.0.0.1:$LOCAL_CS_PORT -> service/swarmada-controlstream:9443)…"
  : > "$PF_LOG"
  kubectl -n "$MANAGER_NS" port-forward service/swarmada-controlstream "${LOCAL_CS_PORT}:9443" \
    >"$PF_LOG" 2>&1 &
  local pf_pid=$!
  LIVE_PIDS+=("$pf_pid")
  echo "$pf_pid" >> "$PIDFILE"
  if ! wait_until "port-forward ready" 15 portforward_ready; then
    cat "$PF_LOG" >&2 || true
    fail "port-forward to the ControlStream Service never became ready"
  fi
  info "port-forward ready ✓"

  local py
  py="$(command -v python3 || command -v python || true)"
  [[ -n "$py" ]] || fail "python3 (or python) not found — needed to run the live sim adapter"

  local adapter_scenario
  adapter_scenario="$(adapter_scenario_preset "$SCENARIO")"

  # The one deliberate pause in this script (see pause_for_demo): everything
  # before this point is setup: cluster, image, control plane, fleet. This is
  # the actual payoff — the moment $LIVE_ROBOT starts talking to the real
  # control plane and the '$SCENARIO' clock begins. Worth a beat to narrate.
  # Printed BEFORE the pause (not folded into its message) so there's time to
  # actually open the second terminal before continuing.
  if [[ "${CI:-}" != "true" && "$PACE" != "off" ]]; then
    echo
    echo "    optional — open a second terminal and run:"
    echo "        $(scenario_watch_command "$SCENARIO")"
    if ! command -v swarmtop >/dev/null 2>&1 && [ ! -x "$REPO_ROOT/tools/swarmtop/bin/swarmtop" ]; then
      echo "    (tip: 'make -C tools/swarmtop build' enables the richer swarmtop view above)"
    fi
  fi
  pause_for_demo "About to start the live '$SCENARIO' scenario on $LIVE_ROBOT: $(scenario_payoff_description "$SCENARIO")"

  info "launching the live sim adapter for $LIVE_ROBOT (scenario '$adapter_scenario')…"
  : > "$ADAPTER_LOG"
  # `exec` here makes the subshell BECOME the python process instead of
  # forking it as a child — without it, $! below would be the subshell's PID,
  # not the actual adapter's, and kill_forcefully would terminate an empty
  # shell while the real python process (and its gRPC streams) kept running
  # as an orphan. That orphan is exactly the kind of leak that would corrupt
  # the NEXT run, whatever scenario it picks.
  (
    cd "$REPO_ROOT"
    # sim_adapter.py imports fleet_adapter.v1 (the generated proto stubs under
    # proto/). Normally adapters/conformance/__main__.py sets this PYTHONPATH
    # for every adapter subprocess it launches (--stub-path, default "proto");
    # launching sim_adapter.py directly here bypasses the harness, so it must
    # be set the same way here or the import fails with ModuleNotFoundError.
    # --tls-server-name: the port-forward means we dial 127.0.0.1, which no server
    # SAN covers. Verify against the name the server certificate actually carries
    # instead of disabling verification.
    # PYTHONUNBUFFERED: without it Python block-buffers stdout when it is a file,
    # so $ADAPTER_LOG stays EMPTY until the process exits — and the teardown
    # SIGTERMs it, discarding the buffer. Every diagnostic this runner prints on
    # failure was therefore blank, which is worse than no log at all.
    exec env PYTHONUNBUFFERED=1 PYTHONPATH="proto${PYTHONPATH:+:${PYTHONPATH}}" \
    "$py" adapters/simulation/sim_adapter.py \
      --endpoint "127.0.0.1:${LOCAL_CS_PORT}" --namespace "$NS" \
      --robot-id "$LIVE_ROBOT" --zone warehouse-a --vendor simulation \
      --scenario "$adapter_scenario" \
      --tls-ca "$ADAPTER_TLS_DIR/ca.crt" \
      --tls-cert "$ADAPTER_TLS_DIR/tls.crt" \
      --tls-key "$ADAPTER_TLS_DIR/tls.key" \
      --tls-server-name "swarmada-controlstream.${MANAGER_NS}.svc"
  ) >"$ADAPTER_LOG" 2>&1 &
  local adapter_pid=$!
  LIVE_PIDS+=("$adapter_pid")
  echo "$adapter_pid" >> "$PIDFILE"

  info ""
  info "watch it live, in another terminal:"
  info "    kubectl -n $NS get robot $LIVE_ROBOT -w"
  info "    tail -f $ADAPTER_LOG"

  # Wait for genuine proof of life before projecting Idle: batteryPercent is
  # written by RobotStatusSink only from a real ingested TelemetryPayload (see
  # this function's header note), so seeing it means the Hello/Register/
  # telemetry handshake actually happened over the real ControlStream — not
  # just that the process started. If it never shows up within the window,
  # project anyway (so the scheduler story isn't blocked on this known gap)
  # but say so plainly; check $ADAPTER_LOG for why the handshake didn't land.
  info "waiting up to 20s for real telemetry from $LIVE_ROBOT (proves the live handshake worked)…"
  if wait_until "$LIVE_ROBOT real telemetry" 20 live_robot_telemetry_seen; then
    info "real telemetry confirmed (battery reported by the live adapter) ✓"
  else
    info "no telemetry observed from $LIVE_ROBOT within 20s — projecting Idle anyway;"
    info "check $ADAPTER_LOG and the port-forward log if this persists"
  fi
  kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --subresource=status --type=merge \
    -p '{"status":{"phase":"Idle"}}'

  if [[ "$SCENARIO" == "battery-handoff" ]]; then
    handle_battery_handoff
  elif [[ "$SCENARIO" == "hardware-fault" ]]; then
    handle_hardware_fault
  elif [[ "$SCENARIO" == "estop-drill" ]]; then
    handle_estop_drill
  elif [[ "$SCENARIO" == "comms-flaky" ]]; then
    handle_comms_flaky
  fi
}

# live_robot_telemetry_seen: true once $LIVE_ROBOT's status carries a
# batteryPercent — the one field RobotStatusSink projects from real ingested
# telemetry (see run_live_scenario's header note on why phase itself can't be
# used as this signal).
live_robot_telemetry_seen() {
  local pct
  pct=$(kubectl get "robot/$LIVE_ROBOT" -n "$NS" -o jsonpath='{.status.batteryPercent}' 2>/dev/null || true)
  [[ -n "$pct" ]]
}

# BATTERY_HANDOFF_ORIGINAL_ROBOT: set by handle_battery_handoff, read by its
# wait predicate battery_handoff_reassigned.
BATTERY_HANDOFF_ORIGINAL_ROBOT=""

# handle_battery_handoff: battery-handoff scenario only. deliver-pallet-001
# (the only sample task applied this scenario — see apply_fleet) is already
# held by one of sim-robot-002/sim-robot-003; the other is healthy, Idle, and
# genuinely spare (that's the point of holding inspect-receiving-dock back).
#
# This simulates that working robot's battery collapsing and triggers the
# REAL, existing, safe hand-off mechanism — the same one ZoneMaintenance's
# Immediate mode uses: annotating the FleetAction with
# swarmada.io/requeue-requested makes fleetaction_controller.go push a real
# cancel_task and wait for a CONFIRMED stop before reassigning. Since the
# robot being cut is status-projected (no live wire), "confirmed" here falls
# back to the assignment lease provably expiring — leaseDuration is a fixed
# 30s in internal/controller/fleetaction_controller.go, so this genuinely
# takes up to ~30s, same as it would for an actually-disconnected robot.
#
# Honesty note: deciding WHICH robot's battery to cut, and WHEN, is this
# script's call — there is no controller anywhere that watches battery and
# triggers a hand-off on its own (that would be real, unbuilt product work).
# Patching the battery number is purely cosmetic for this demo; it is the
# requeue-requested annotation that does the actual work, and everything
# from that annotation onward — the cancel push, the confirmed-stop wait,
# the release, the re-assignment — is real, unmodified control-plane code.
handle_battery_handoff() {
  step "6.5/7 — battery-handoff: simulate a working robot's battery collapsing"
  local holder
  holder=$(kubectl get fleetaction deliver-pallet-001 -n "$NS" -o jsonpath='{.status.assignedRobot}' 2>/dev/null || true)
  if [[ -z "$holder" ]]; then
    info "deliver-pallet-001 has no assignedRobot yet — waiting up to 15s for the scheduler…"
    if ! wait_until "deliver-pallet-001 assigned" 15 deliver_pallet_assigned; then
      diagnostics
      fail "deliver-pallet-001 was never assigned — nothing to hand off"
    fi
    holder=$(kubectl get fleetaction deliver-pallet-001 -n "$NS" -o jsonpath='{.status.assignedRobot}')
  fi
  BATTERY_HANDOFF_ORIGINAL_ROBOT="$holder"
  info "deliver-pallet-001 is currently held by $holder"

  # No second pause here on purpose — this script keeps to exactly ONE
  # deliberate keypress (see pause_for_demo's header note and the one already
  # taken before $LIVE_ROBOT's adapter launched). Once you've pressed Enter,
  # everything through the hand-off runs on its own.
  info "cutting $holder's battery mid-task and requesting a safe hand-off…"
  info "patching $holder's battery down (simulated — $holder is not the live robot)…"
  kubectl patch "robot/$holder" -n "$NS" --subresource=status --type=merge \
    -p '{"status":{"batteryPercent":8}}'

  info "requesting a safe hand-off (swarmada.io/requeue-requested) — this is the same"
  info "real, confirmed-stop mechanism ZoneMaintenance's Immediate mode uses; the battery"
  info "value above does NOT itself trigger anything — this annotation does…"
  kubectl annotate fleetaction deliver-pallet-001 -n "$NS" \
    "swarmada.io/requeue-requested=battery critically low (simulated by this script)" --overwrite >/dev/null

  info "waiting up to 40s for the confirmed stop and reassignment — $holder has no live"
  info "wire, so this waits out the real ~30s assignment-lease expiry, the same safe"
  info "fallback path a genuinely disconnected robot goes through…"
  if wait_until "deliver-pallet-001 reassigned off $holder" 40 battery_handoff_reassigned; then
    local new_holder
    new_holder=$(kubectl get fleetaction deliver-pallet-001 -n "$NS" -o jsonpath='{.status.assignedRobot}')
    info "hand-off confirmed ✓ — deliver-pallet-001 moved from $holder to $new_holder"
  else
    diagnostics
    fail "deliver-pallet-001 was never reassigned off $holder within 40s"
  fi
}

# handle_hardware_fault: hardware-fault scenario only. Unlike battery-handoff,
# the reroute here is NOT script-triggered — the control plane detects it. The
# live robot ($LIVE_ROBOT) holds the camera task (inspect-receiving-dock; it is
# the only camera-capable robot, apply_fleet having stripped 002/003). When its
# camera degrades at ~30s (real adapter fault), the Capability Controller marks
# status.capabilities[camera_front]=Degraded, and fleetaction_controller.go's
# Capability-loss reassignment (RFC-0001) automatically pushes a real cancel_task,
# the sim adapter acks STOPPED_SAFELY, and the task requeues. This function only
# stages the reroute target: it re-adds camera_front to sim-robot-002 (an idle
# spare) so the automatic reassignment has somewhere to land. Everything from the
# capability degrading onward is real, unmodified control-plane code.
# handle_comms_flaky: the option-5 payoff — the adapter's ControlStream really
# drops and really re-registers, and the control plane NOTICES both.
#
# Without this the scenario silently never happened: the preset drops the stream
# at T+20s and reconnects at T+35s (from adapter start), but nothing held the run
# open that long, so assert_end_state passed on the generic checks and tore the
# adapter down first — the adapter log came back 0 bytes.
#
# This is now observable in status, not only in logs. FleetAdapter.status.phase
# flips Connected → Disconnected → Connected because the LIVE path presents a
# VERIFIED mTLS identity (config/overlays/quickstart-mtls); the adapter-health
# view is keyed on that identity, so with the older plaintext overlay it could
# never move. That is why SCENARIOS.md used to call this a logs-only story.
handle_comms_flaky() {
  step "6.5/7 — comms-flaky: watch the ControlStream drop and re-register"
  info "the adapter drops its streams at T+20s and reconnects at T+35s (preset comms.drop)."
  info "watch FleetAdapter.status.phase — or swarmtop's adapter-health view ('a')."

  info "waiting up to 75s for the ControlStream to DROP…"
  if ! wait_until "adapter Disconnected" 75 adapter_disconnected; then
    diagnostics
    fail "the ControlStream never dropped — comms-flaky did not actually run"
  fi
  info "stream drop observed ✓ — FleetAdapter went Disconnected (the outage is real)"

  info "waiting up to 90s for the adapter to RE-REGISTER (fresh Hello/Register)…"
  if ! wait_until "adapter Connected again" 90 adapter_connected; then
    diagnostics
    fail "the adapter never re-registered after the outage"
  fi
  info "reconnect confirmed ✓ — FleetAdapter back to Connected after a real stream teardown"
}

# adapter_disconnected / adapter_connected: the FleetAdapter's observed phase.
# Derived by the control plane from live stream presence, not written by this script.
adapter_phase() {
  kubectl get fleetadapter sim-fleet-adapter -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true
}
adapter_disconnected() { [[ "$(adapter_phase)" == "Disconnected" ]]; }
adapter_connected() { [[ "$(adapter_phase)" == "Connected" ]]; }

# handle_estop_drill: the option-6 payoff — a robot that is ACTUALLY DOING WORK is
# emergency-stopped, the stop is CONFIRMED by the robot (never inferred), and the
# control plane pauses the action it was holding.
#
# Why this drives the estop from an annotation rather than the preset's
# `estop.trigger: at_seconds` timer: the timer fires 20s after the adapter starts,
# which races the scheduler — the drill would fire before a task lands (proving
# nothing) or after this runner has already torn the adapter down. The
# `swarmada.io/estop-triggered` annotation is the real operator-facing trigger
# (internal/controller/robotestop_controller.go), so this is both deterministic
# AND the path a real operator uses. Everything after the annotation is
# unmodified control-plane code: the estop goes out over $LIVE_ROBOT's LIVE
# ControlStream, the sim adapter confirms it via safety.confirm_estop from
# simulator ground truth, and the FleetAction controller pauses the held action
# (§9.6.2.4 — estop takes precedence over the lease).
handle_estop_drill() {
  step "6.5/7 — estop-drill: stop a working robot and confirm the stop"

  info "waiting up to 45s for a FleetAction to land on ${LIVE_ROBOT} (a stop only means"
  info "something if the robot is actually holding work)…"
  if ! wait_until "a FleetAction assigned to $LIVE_ROBOT" 45 action_assigned_to_live; then
    diagnostics
    fail "no FleetAction ever landed on $LIVE_ROBOT — nothing to emergency-stop"
  fi
  ESTOP_ACTION=$(live_robot_action)
  info "$LIVE_ROBOT is holding $ESTOP_ACTION ✓"

  pause_for_demo "$LIVE_ROBOT is executing $ESTOP_ACTION. Press Enter to TRIGGER an emergency stop — the control plane sends it over the live ControlStream, the robot CONFIRMS it from simulator ground truth, and $ESTOP_ACTION is paused."

  info "triggering the estop (swarmada.io/estop-triggered on $LIVE_ROBOT — the operator-facing path)…"
  kubectl annotate "robot/$LIVE_ROBOT" -n "$NS" \
    "swarmada.io/estop-triggered=$(date +%s)" --overwrite

  # Stopped is reached only from an adapter-confirmed EstopAck (C5 discipline).
  # A robot that merely *looks* stopped never reports Stopped.
  if ! wait_until "$LIVE_ROBOT confirmed Stopped" 60 live_robot_estop_stopped; then
    diagnostics
    fail "$LIVE_ROBOT never reported a CONFIRMED estop (status.estopState=Stopped) within 60s"
  fi
  info "estop CONFIRMED ✓ — $LIVE_ROBOT reported estopState=Stopped from simulator ground truth"

  if wait_until "$ESTOP_ACTION paused by estop" 60 estop_action_paused; then
    info "safe hold confirmed ✓ — $ESTOP_ACTION was paused by the estop (§9.6.2.4)"
  else
    diagnostics
    fail "$ESTOP_ACTION was never paused after $LIVE_ROBOT was emergency-stopped"
  fi
}

# action_assigned_to_live: true once ANY FleetAction is held by $LIVE_ROBOT.
action_assigned_to_live() { [[ -n "$(live_robot_action)" ]]; }

# live_robot_action: the name of the FleetAction $LIVE_ROBOT currently holds ("" if none).
live_robot_action() {
  kubectl get fleetactions -n "$NS" \
    -o jsonpath="{range .items[?(@.status.assignedRobot=='$LIVE_ROBOT')]}{.metadata.name}{'\n'}{end}" \
    2>/dev/null | head -1
}

# live_robot_estop_stopped: true once $LIVE_ROBOT reports a CONFIRMED stop.
live_robot_estop_stopped() {
  local s
  s=$(kubectl get "robot/$LIVE_ROBOT" -n "$NS" -o jsonpath='{.status.estopState}' 2>/dev/null || true)
  [[ "$s" == "Stopped" ]]
}

# estop_action_paused: true once the action $LIVE_ROBOT held is no longer running
# on it — either explicitly paused by the estop or released off the robot.
estop_action_paused() {
  local phase msg
  phase=$(kubectl get "fleetaction/$ESTOP_ACTION" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  msg=$(kubectl get "fleetaction/$ESTOP_ACTION" -n "$NS" -o jsonpath='{.status.message}' 2>/dev/null || true)
  [[ "$msg" == *"paused by estop"* || "$phase" == "Paused" ]]
}

handle_hardware_fault() {
  step "6.5/7 — hardware-fault: drive the camera capability and let the control plane reroute"
  # $LIVE_ROBOT's camera_front CAPABILITY is gated on a camera_front HARDWARE
  # component (apply_fleet). In a status-projected quickstart the live adapter's
  # hardware telemetry does not reach status.hardware (battery doesn't either) —
  # so, exactly as battery-handoff scripts the battery, this drives status.hardware
  # directly: Healthy so the capability derives Active and the task lands on
  # $LIVE_ROBOT, then Degraded to make its capability degrade. Everything from the
  # capability degrading onward is real, unmodified control-plane code — including
  # a real cancel_task over $LIVE_ROBOT's LIVE ControlStream (the sim adapter acks
  # STOPPED_SAFELY), the requeue, and the reassignment.
  info "marking $LIVE_ROBOT's camera_front hardware Healthy so its capability derives Active…"
  kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --subresource=status --type=merge \
    -p '{"status":{"hardware":[{"name":"camera_front","status":"Healthy"}]}}'

  info "waiting up to 20s for the camera task to land on ${LIVE_ROBOT}…"
  if ! wait_until "inspect-receiving-dock assigned to $LIVE_ROBOT" 20 inspect_assigned_to_live; then
    diagnostics
    fail "inspect-receiving-dock never landed on $LIVE_ROBOT — cannot demonstrate the reroute"
  fi
  info "inspect-receiving-dock is held by $LIVE_ROBOT ✓"

  info "restoring camera_front on sim-robot-002 (idle spare, ungated so Active) as the reroute target…"
  kubectl patch robot/sim-robot-002 -n "$NS" --type=json -p \
    '[{"op":"add","path":"/spec/capabilities/-","value":{"name":"camera_front","type":"hardware-native","pauseable":false}}]'

  # Second, deliberate pause: everything is staged ($LIVE_ROBOT holds the camera
  # task, sim-robot-002 is an idle camera spare). Pressing Enter now degrades the
  # camera and triggers the reroute — so a presenter can show the before/after.
  # Honors --pace (timed/off) and CI like the first pause; the message is explicit
  # so it never reads as "hung".
  pause_for_demo "$LIVE_ROBOT now holds the camera task; sim-robot-002 is staged as the idle camera spare. Press Enter to DEGRADE $LIVE_ROBOT's camera — the control plane will safe-stop the task and reroute it to sim-robot-002."

  info "degrading $LIVE_ROBOT's camera_front → its capability degrades → the FleetAction controller"
  info "AUTOMATICALLY safe-stops and reroutes the task (no annotation, no script trigger — a real"
  info "cancel_task over the live ControlStream, acked STOPPED_SAFELY, then reassignment)…"
  kubectl patch "robot/$LIVE_ROBOT" -n "$NS" --subresource=status --type=merge \
    -p '{"status":{"hardware":[{"name":"camera_front","status":"Degraded"}]}}'

  if wait_until "inspect-receiving-dock reassigned off $LIVE_ROBOT" 60 inspect_reassigned_off_live; then
    local new_holder
    new_holder=$(kubectl get fleetaction inspect-receiving-dock -n "$NS" -o jsonpath='{.status.assignedRobot}')
    info "reroute confirmed ✓ — inspect-receiving-dock moved from $LIVE_ROBOT to $new_holder on capability loss"
  else
    diagnostics
    fail "inspect-receiving-dock was never rerouted off $LIVE_ROBOT within 60s"
  fi
}

# inspect_assigned_to_live: true once inspect-receiving-dock is held by $LIVE_ROBOT.
inspect_assigned_to_live() {
  local a
  a=$(kubectl get fleetaction inspect-receiving-dock -n "$NS" -o jsonpath='{.status.assignedRobot}' 2>/dev/null || true)
  [[ "$a" == "$LIVE_ROBOT" ]]
}

# inspect_reassigned_off_live: true once inspect-receiving-dock is held by a robot
# OTHER than $LIVE_ROBOT (the reroute landed on the spare).
inspect_reassigned_off_live() {
  local a
  a=$(kubectl get fleetaction inspect-receiving-dock -n "$NS" -o jsonpath='{.status.assignedRobot}' 2>/dev/null || true)
  [[ -n "$a" && "$a" != "$LIVE_ROBOT" ]]
}

# deliver_pallet_assigned: true once deliver-pallet-001 has any assignedRobot.
deliver_pallet_assigned() {
  local a
  a=$(kubectl get fleetaction deliver-pallet-001 -n "$NS" -o jsonpath='{.status.assignedRobot}' 2>/dev/null || true)
  [[ -n "$a" ]]
}

# battery_handoff_reassigned: true once deliver-pallet-001's assignedRobot is
# non-empty AND different from $BATTERY_HANDOFF_ORIGINAL_ROBOT.
battery_handoff_reassigned() {
  local a
  a=$(kubectl get fleetaction deliver-pallet-001 -n "$NS" -o jsonpath='{.status.assignedRobot}' 2>/dev/null || true)
  [[ -n "$a" && "$a" != "$BATTERY_HANDOFF_ORIGINAL_ROBOT" ]]
}

# ── Verifiable end state ──────────────────────────────────────────────────────

# robots_ready: every sample robot reports a phase that proves it became
# schedulable — Idle, or past it (Assigned/InProgress/Charging), once the
# scheduler has picked it up. NOT a strict "== Idle": the scheduler can and
# does move a robot Idle -> Assigned -> InProgress within seconds of it
# becoming ready (visible in the manager log as soon as ~10s after readiness),
# so requiring every robot to still show Idle at the moment this is checked
# is a race — it happened to pass before only because the status-projection
# path patches all three robots in one instant and the very first poll here
# (before any sleep) usually lands in the split second before the scheduler
# has assigned anything. Any extra latency before a robot reaches readiness
# (e.g. the LIVE path's real adapter handshake) blows that window and this
# would then wait out the full timeout and fail — even though the robot
# genuinely became ready. Discovered (not yet admitted-ready), Error, and
# Offline are excluded on purpose: those are not "ready" by any definition.
robots_ready() {
  local r phase
  for r in "${ROBOTS[@]}"; do
    # estop-drill deliberately leaves $LIVE_ROBOT emergency-stopped. Demanding it
    # be Idle would fail the run for doing exactly what the scenario proves, so
    # the stopped robot is checked against its estop state instead (below).
    if [[ "$SCENARIO" == "estop-drill" && "$r" == "$LIVE_ROBOT" ]]; then
      continue
    fi
    phase=$(kubectl get "robot/$r" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "$phase" in
      Idle|Assigned|InProgress|Charging) ;;
      *) return 1 ;;
    esac
  done
  return 0
}

# task_assigned: at least one FleetAction has a non-empty status.assignedRobot —
# i.e. the REAL scheduler bound a task to a robot. (A terminal Succeeded is not
# reachable here: the reference sim adapter never reports task completion, so
# assignment by the scheduler is the honest, verifiable end state.)
task_assigned() {
  local vals
  vals=$(kubectl get fleetactions -n "$NS" -o jsonpath='{.items[*].status.assignedRobot}' 2>/dev/null || true)
  [[ -n "${vals// /}" ]]
}

# wait_until DESC TIMEOUT PREDICATE — poll PREDICATE until true or TIMEOUT secs.
wait_until() {
  local desc="$1" timeout="$2" pred="$3" elapsed=0 interval=3
  while true; do
    if "$pred"; then return 0; fi
    (( elapsed >= timeout )) && return 1
    sleep "$interval"; elapsed=$((elapsed + interval))
  done
}

diagnostics() {
  echo "---- diagnostics ----" >&2
  kubectl get robots -n "$NS" >&2 2>&1 || true
  kubectl get fleetactions -n "$NS" -o wide >&2 2>&1 || true
  kubectl -n "$MANAGER_NS" logs "deployment/$MANAGER_DEPLOY" --tail=50 >&2 2>&1 || true
  if [[ "$LIVE" == 1 ]]; then
    echo "---- live adapter log ($ADAPTER_LOG) ----" >&2
    tail -n 50 "$ADAPTER_LOG" >&2 2>&1 || true
    echo "---- port-forward log ($PF_LOG) ----" >&2
    tail -n 20 "$PF_LOG" >&2 2>&1 || true
  fi
}

# launch_swarmtop: the `make demo` hook (DEMO_LAUNCH_SWARMTOP=1). After the end
# state is reached, run swarmtop in the FOREGROUND so the human watches values
# flow through every view. Foreground is deliberate: on the LIVE path the EXIT
# trap (cleanup_live_processes) tears down the live sim adapter and port-forward
# when this script returns — so blocking here on swarmtop keeps that live
# telemetry flowing until the operator quits swarmtop (q), at which point the
# normal teardown reminder prints. Builds the in-tree binary if none is on PATH;
# if swarmtop can't be found or built, falls back to teardown_hint with the watch
# command, so `make demo` degrades instead of failing.
launch_swarmtop() {
  local st; st="$(swarmtop_bin)"
  if [[ -z "$st" ]]; then
    info "building swarmtop (tools/swarmtop) for the demo view…"
    make -C "$REPO_ROOT/tools/swarmtop" build >/dev/null 2>&1 || true
    st="$(swarmtop_bin)"
  fi
  if [[ -z "$st" ]]; then
    info "swarmtop unavailable (build failed?) — skipping the auto-launch"
    teardown_hint
    return 0
  fi
  echo
  echo "==> Launching swarmtop against $NS (quit with 'q' to tear down the live demo)"
  teardown_hint
  "$st" -n "$NS" --robot "$LIVE_ROBOT"
}

teardown_hint() {
  echo
  echo "The cluster '$CLUSTER' is left running so you can explore it:"
  echo "    kubectl get robots,fleetactions -n $NS"
  echo "    $(scenario_watch_command "$SCENARIO")   # live fleet inspector"
  if [[ "$LIVE" == 1 ]]; then
    echo "The live sim adapter and its port-forward keep running until this script exits."
  fi
  remind_to_clean_cluster
}

assert_end_state() {
  step "7/7 — Wait for the verifiable end state"

  info "waiting up to ${READY_TIMEOUT}s for all ${#ROBOTS[@]} robots to reach Idle…"
  if ! wait_until "robots Idle" "$READY_TIMEOUT" robots_ready; then
    diagnostics
    echo "❌ Quickstart FAILED: not all robots reached Idle within ${READY_TIMEOUT}s." >&2
    exit 1
  fi
  info "all robots Idle ✓"

  info "waiting up to ${ASSIGN_TIMEOUT}s for the scheduler to assign a FleetAction…"
  if ! wait_until "task assigned" "$ASSIGN_TIMEOUT" task_assigned; then
    diagnostics
    echo "❌ Quickstart FAILED: no FleetAction was assigned within ${ASSIGN_TIMEOUT}s." >&2
    exit 1
  fi
  info "a FleetAction was assigned by the scheduler ✓"

  # A scenario that silently never ran must not report success. The generic
  # checks above ("robots healthy, something got assigned") are true whether or
  # not the drill happened — which is exactly how estop-drill passed green while
  # never firing an estop. Re-assert the scenario's OWN payoff here.
  if [[ "$SCENARIO" == "estop-drill" && "$LIVE" == 1 ]]; then
    if ! live_robot_estop_stopped; then
      diagnostics
      echo "❌ Quickstart FAILED: estop-drill finished but $LIVE_ROBOT is not in a confirmed" >&2
      echo "   estop (status.estopState != Stopped) — the drill did not actually happen." >&2
      exit 1
    fi
    info "estop-drill payoff re-confirmed at end state ✓ ($LIVE_ROBOT still Stopped)"
  fi

  echo
  kubectl get robots -n "$NS"
  echo
  kubectl get fleetactions -n "$NS"
  echo
  if [[ "$SCENARIO" == "estop-drill" && "$LIVE" == 1 ]]; then
    echo "✅ Quickstart end state reached: $LIVE_ROBOT was emergency-stopped while holding"
    echo "   work, CONFIRMED the stop from simulator ground truth, and its action was"
    echo "   paused; the rest of the fleet stayed healthy and scheduling continued."
  else
    echo "✅ Quickstart end state reached: all robots Idle and the real scheduler"
    echo "   assigned a FleetAction to a robot in warehouse-a."
  fi
}

main() {
  parse_args "$@"
  if ((CLEAN_REQUESTED)); then
    do_clean_everything
    exit 0
  fi
  pick_scenario
  if ((CLEAN_REQUESTED)); then
    do_clean_everything
    exit 0
  fi
  determine_live_mode
  announce_scenario
  preflight
  cleanup_stale_run
  create_cluster
  build_and_load
  install_control_plane
  reset_namespace_state
  apply_fleet
  if [[ "$LIVE" == 1 ]]; then
    run_live_scenario
  else
    project_readiness
  fi
  assert_end_state
  if [[ "${DEMO_LAUNCH_SWARMTOP:-}" == "1" ]]; then
    launch_swarmtop
  else
    teardown_hint
  fi
}

main "$@"
