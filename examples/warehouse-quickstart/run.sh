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
# Readiness note (honest): TWO paths exist. LIVE is the DEFAULT — this is what a
# reviewer or a first-time reader gets by just running the script, and a
# quickstart whose own south side is hand-typed status does not demonstrate the
# spec it ships with (B3).
#
#   - LIVE (default, any scenario, CI included): one robot (sim-robot-001) is
#     driven by a real, running adapters/simulation/sim_adapter.py process
#     talking to the real ControlStream over a port-forward, so the chosen
#     scenario's fault/battery/comms/estop behaviour actually happens, live, on
#     that robot — AND its readiness (Robot.status.phase, FleetAdapter.status.
#     phase) is real too: the Robot reconciler advances a live robot's phase
#     Discovered->{Idle,InProgress} on its own from fresh adapter liveness
# (ADR-0029, RFC-0001 #control-plane-robot-health-and-connectivity), and FleetAdapter.status.phase reaches
#     Connected from a verified mTLS identity (AdapterPresence). This script
#     WAITS for both rather than writing either. This deploys via
#     config/overlays/quickstart-mtls (DEV/DEMO ONLY — self-signed cert-manager
#     CA, no production certificate lifecycle) instead of config/default.
#     The other two robots have no Fleet Adapter at all, so nothing can ever
#     drive their readiness the real way; their Robot.status.phase is
#     projected (kubectl patch --subresource=status phase=Idle) — the same
#     honest stand-in-for-an-absent-adapter projection the fully-projected
#     path below uses, not a workaround for a missing transition. See
#     docs/quickstart.md, Honest notes.
#
#     The two scenarios that move work from one robot to another, battery-handoff
#     and hardware-fault, need a spare that can really take it, so there EVERY
#     robot is live: one sim_adapter.py process and one FleetAdapter identity per
#     robot, deployed via config/overlays/demo-ab-live (quickstart-mtls plus a
#     client certificate for each spare's adapter) — the pattern
#     examples/demo-ab-live/run.sh explains in its header. Nothing about any
#     robot's readiness is projected in those two.
#
#   - Projected (the explicit --projected flag): no adapter process runs at all, and EVERY
#     robot's readiness is projected the same way the two non-live robots
#     above are. Writing status is otherwise the RA-1 anti-pattern the spec
#     reserves to controllers (RFC-0001 §9.1.3, RFC-0001 Terminology); this
#     path exists for an environment that cannot run a live adapter process
#     (no python3, a sandboxed runner) but still wants the rest of the
#     quickstart. CI takes the LIVE path: its job installs Python so the
#     adapter can run. (The old `make demo-b`
#     targets that did the same thing unconditionally were removed for
#     exactly this reason — see the Makefile comment above the Demo section.)
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
#   examples/warehouse-quickstart/run.sh                      bring up + assert LIVE; interactive
#                                                             scenario picker on a TTY
#   examples/warehouse-quickstart/run.sh --scenario NAME      non-interactive LIVE (CI / video);
#                                                             NAME is a simulated-fleet scenario
#   examples/warehouse-quickstart/run.sh --projected          force the status-projected path
#                                                             (no live adapter process)
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
#   QUICKSTART_IMG      controller image tag     (default: swarmada-controller:dev). Only the
#                       default is supported: the deploy runs swarmada-controller:dev whatever
#                       this is set to. On a cluster this run creates, another value fails at
#                       step 4/7 (ImagePullBackOff). On a reused cluster that already holds :dev,
#                       it exits 0, but the manager runs that earlier :dev image, not the one
#                       built. Delete the cluster first (--clean) and leave this unset.
#   QUICKSTART_READY_TIMEOUT   secs to wait for robots Idle    (default: 15)
#   QUICKSTART_ASSIGN_TIMEOUT  secs to wait for a task Assigned (default: 15)
#   QUICKSTART_COMPLETE_TIMEOUT secs to wait for a task Succeeded (live healthy-fleet only, default: 45)
#   QUICKSTART_REROUTE_COMPLETE_TIMEOUT secs to wait for the moved task to be Succeeded on the spare
#                              (live battery-handoff / hardware-fault, default: 90)
#   QUICKSTART_CS_PORT  local port for the ControlStream port-forward (live path only, default: 19443)
#   QUICKSTART_PAUSE_SECONDS   duration for --pace timed        (default: 8)
#   CI=true             disables pacing regardless of --pace; the path stays LIVE
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${QUICKSTART_CLUSTER:-swarmada-quickstart}"
# Must match the image tag config/default/kustomization.yaml pins (swarmada-controller:dev),
# so `make deploy` / the quickstart-mtls or quickstart-projected overlay schedules the image
# we build and load below.
IMG="${QUICKSTART_IMG:-swarmada-controller:dev}"
NS="warehouse-a"
SAMPLE="config/samples/demo_a.yaml"
MANAGER_NS="system"
MANAGER_DEPLOY="swarmada-controller-manager"
ROBOTS=(sim-robot-001 sim-robot-002 sim-robot-003)
# Index-aligned with ROBOTS: the FleetAdapter identity that serves each robot when every
# robot is live (all_live). Otherwise only the first is used, by $LIVE_ROBOT.
ADAPTERS=(sim-fleet-adapter sim-fleet-adapter-2 sim-fleet-adapter-3)
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
COMPLETE_TIMEOUT="${QUICKSTART_COMPLETE_TIMEOUT:-45}"
REROUTE_COMPLETE_TIMEOUT="${QUICKSTART_REROUTE_COMPLETE_TIMEOUT:-90}"
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
PROJECTED_REQUESTED=0      # set by --projected: force the status-projected path (opt out of the LIVE default)
LIVE_PIDS=()               # background PIDs started by this run (port-forward, adapter)
PACE=""                    # set by --pace; "" means "resolve the default in determine_live_mode"
PAUSE_SECONDS="${QUICKSTART_PAUSE_SECONDS:-8}"
# The estop drill pauses after its action lands, and the robot drives on while it waits: on the
# sample's long route it reaches Succeeded about 32s after it starts, and a stop sent after that
# stops nothing. So that one pause lasts at most this long on every pace, keypress included.
ESTOP_DRILL_MAX_PAUSE=8
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
              [--pause-seconds N] [--projected] [--clean] [-h|--help]

  --scenario NAME    simulated-fleet scenario; one of:
                       ${SCENARIOS[*]}
                     (default: healthy-fleet). Every scenario takes the LIVE
                     path by default (see the header note), CI included;
                     CI=true only turns pacing off. To force the
                     status-projected path, pass --projected.
  --robots N         fleet size for the printed sim-adapter command (default: 3).
  --pace MODE        pacing before the LIVE scenario's real payoff starts:
                       keypress (default on the LIVE path) — wait for Enter
                       timed                                — sleep --pause-seconds
                       off                                  — no pause
                     Always off under CI=true regardless of this flag.
  --pause-seconds N  duration for --pace timed (default: 8, or \$QUICKSTART_PAUSE_SECONDS).
  --projected        force the status-projected path (no live sim_adapter.py
                     process) — for an environment that cannot run a live
                     adapter (no python3, a sandboxed runner, etc.).
                     Overrides the LIVE default for every scenario.
  --clean            tear down the '$CLUSTER' kind cluster, kill any live/stale
                     port-forward or adapter processes, remove local pidfile/log
                     state, and exit — does not bring anything up. Same as
                     picking "7" in the interactive picker.
  -h, --help         show this help and exit.

Env knobs: QUICKSTART_CLUSTER, QUICKSTART_IMG, QUICKSTART_CS_PORT,
           QUICKSTART_READY_TIMEOUT, QUICKSTART_ASSIGN_TIMEOUT, QUICKSTART_COMPLETE_TIMEOUT,
           QUICKSTART_PAUSE_SECONDS, CI.
           QUICKSTART_IMG: only the default (swarmada-controller:dev) is supported; the
           deploy runs that tag whatever the knob says. On a cluster this run creates,
           another value fails at step 4/7. On a reused cluster that already holds :dev,
           it exits 0 but runs that earlier :dev image, not the one built. Delete the
           cluster first (--clean) and leave it unset.
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
      --projected) PROJECTED_REQUESTED=1; shift ;;
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
  echo "  2) battery-edge      — a robot's battery drains while it keeps its task"
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
# process against the real ControlStream; LIVE=0 keeps the status-projection
# path. LIVE is the DEFAULT for every scenario: this is the path a first-time
# reader (including a spec reviewer) hits by just running the script, and a
# quickstart that fakes the one thing — a real adapter — its own core spec
# exists to standardize does not demonstrate the spec (B3). Only the explicit
# --projected flag forces LIVE=0, for an environment that cannot run a live
# adapter process (no python3, a sandboxed runner, etc.) but still wants the
# rest of the quickstart. $CI=true does not: CI runs the LIVE path, because the
# projected path has no position telemetry, so no robot gets a status.currentZone
# and dispatch filter 2 assigns nothing. --projected is checked first: it beats even a
# scenario whose whole story IS the live wire (hardware-fault, comms-flaky,
# estop-drill) if a caller asks for it explicitly.
#
# Also resolves PACE's default when --pace wasn't given: keypress on the LIVE
# path (there's an actual payoff worth a beat before it starts), off
# otherwise (nothing to pace on the status-projected path). CI forces off
# either way — see pause_for_demo.
determine_live_mode() {
  if [[ "$PROJECTED_REQUESTED" != 1 ]]; then
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

# pause_for_demo MESSAGE [MAX_SECONDS]: the one deliberate pacing point in this
# script, called only right before the LIVE scenario's real payoff (see
# run_live_scenario). Never blocks CI. "keypress" falls back to a short fixed
# sleep if stdin isn't a TTY (e.g. --pace keypress under a pipe) so the script
# can't hang forever waiting for input nobody can give it. With MAX_SECONDS no
# pace waits longer than that: the keypress wait gives up and continues, and a
# longer --pause-seconds is cut to it.
pause_for_demo() {
  local message="$1" max="${2:-}" seconds="$PAUSE_SECONDS"
  [[ -n "$max" ]] && (( seconds > max )) && seconds="$max"
  [[ "${CI:-}" == "true" || "$PACE" == "off" ]] && return 0
  echo
  echo "    $message"
  if [[ "$PACE" == "timed" ]]; then
    echo "    continuing in ${seconds}s…"
    sleep "$seconds"
    return 0
  fi
  if [[ -t 0 && -n "$max" ]]; then
    read -rt "$max" -p "    press Enter to continue (continuing on its own in ${max}s)... " _ || echo
  elif [[ -t 0 ]]; then
    read -rp "    press Enter to continue... " _ || true
  else
    echo "    no TTY for --pace keypress; pausing ${seconds}s instead"
    sleep "$seconds"
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
      echo "$LIVE_ROBOT's reported battery will drain on the scenario's curve while it" \
           "works, and it keeps the task: the other two robots report no position, so it is" \
           "the only candidate the scheduler has, and this run compares no robots by battery." \
           "battery-handoff is the scenario that stages that comparison." ;;
    battery-handoff)
      echo "$LIVE_ROBOT comes online alone and takes the delivery task, while its reported battery" \
           "drains on the battery-edge curve. The two spares then come online, each driven by its" \
           "own live adapter. This script requests a safe hand-off; the control plane stops" \
           "$LIVE_ROBOT, and the scheduler gives the task to the spare with the most battery," \
           "which completes it." ;;
    hardware-fault)
      echo "All three robots come online, each driven by its own live adapter. $LIVE_ROBOT is the" \
           "only camera robot, so it takes the camera task. Its adapter reports camera_front" \
           "DEGRADED about 20s after it starts: the capability degrades, and the control plane" \
           "automatically safe-stops the task and REROUTES it to the spare camera robot," \
           "sim-robot-002, which completes it." ;;
    comms-flaky)
      echo "$LIVE_ROBOT's ControlStream will drop for about 15s and reconnect. The adapter" \
           "view moves: FleetAdapter.status.phase goes Connected → Disconnected → Connected" \
           "(swarmtop's adapter-health view, 'a'). The robot's own status does not: 15s is" \
           "shorter than the 30s default offline threshold, so the robot never goes Offline," \
           "and status.connectivity.lastSeenAt is written at most once per half that threshold." \
           "The rest is in the ADAPTER + manager LOGS — the stream drops, the adapter self-stops" \
           "its task on lease expiry (C4), reconnects with a fresh Hello/Register, and its" \
           "fencing token + lease survive the outage (C3)." ;;
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
# Battery/Action/Age, plus Class with -o wide (api/v1/robot_types.go) — status.hardware[],
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
# which is a LOGS story — the robot's status does not show the outage: it is
# shorter than the offline threshold, so the robot never goes Offline, and
# status.connectivity.lastSeenAt is written at most once per half that threshold.
# Only FleetAdapter.status.phase moves, and the payoff text names it; the watch
# command points at the adapter + manager logs. The remaining scenarios keep
# plain `watch kubectl get`, since Battery/Phase/Action already have printer
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
      # fencing/lease intact), which lives in the LOGS. The robot's status does not
      # show the outage (it is shorter than the offline threshold), so a robot view
      # or a `kubectl -o json` field would show nothing move.
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
    if all_live; then
      info "LIVE mode (default): every robot will be driven by its own real sim_adapter.py"
      info "process against the real ControlStream, under its own FleetAdapter identity, so"
      info "the spare that takes the work over is as real as the robot it leaves."
    else
      info "LIVE mode (default): $LIVE_ROBOT will be driven by a real sim_adapter.py process"
      info "against the real ControlStream (--scenario $SCENARIO). The other two"
      info "robots stay status-projected — they have no Fleet Adapter, so nothing can"
      info "ever drive their readiness the real way."
    fi
    info "$LIVE_ROBOT's connection, registration, telemetry, AND phase are all real:"
    info "the Robot reconciler advances a live robot Discovered->Idle on its own"
    info "(ADR-0029). See docs/quickstart.md, Honest notes."
    if all_live; then
      info "This deploys via config/overlays/demo-ab-live: quickstart-mtls (DEV/DEMO ONLY —"
      info "see that overlay's own header comment) plus the two spares' client certificates."
    else
      info "This deploys via config/overlays/quickstart-mtls (DEV/DEMO ONLY — see"
      info "that overlay's own header comment)."
    fi
  else
    info ""
    info "status-projected mode (--projected): readiness is projected"
    info "(kubectl patch), not adapter-driven — see the Honest notes in README.md."
    info "To watch '$SCENARIO' live yourself, run:"
    info "    python3 adapters/simulation/sim_adapter.py --endpoint <fleet-adapter-endpoint> \\"
    info "      --namespace $NS --robot-id sim-robot-001 --scenario $SCENARIO"
  fi
}

# all_live: true when EVERY robot runs its own live adapter — the LIVE path of the two
# scenarios that move work to a spare (battery-handoff, hardware-fault). A spare with no
# adapter reports no position, so the Zone Controller never derives its
# status.currentZone and dispatch filter 2 excludes it: the work would have nowhere to
# go. Everywhere else only $LIVE_ROBOT is live.
all_live() {
  [[ "$LIVE" == 1 && ( "$SCENARIO" == "battery-handoff" || "$SCENARIO" == "hardware-fault" ) ]]
}

# adapter_tls_dir INDEX / adapter_log INDEX — where ROBOTS[INDEX]'s adapter keeps its
# client keypair and its log. Index 0 is $LIVE_ROBOT's, at the paths it always used.
adapter_tls_dir() { if (($1 == 0)); then echo "$ADAPTER_TLS_DIR"; else echo "${ADAPTER_TLS_DIR}-$(($1 + 1))"; fi; }
adapter_log() { if (($1 == 0)); then echo "$ADAPTER_LOG"; else echo "/tmp/swq-${CLUSTER}-adapter-$(($1 + 1)).log"; fi; }

# preflight checks the tools every scenario needs, regardless of which one
# gets picked — kind/kubectl/docker/make/go and a reachable Docker daemon.
# Deliberately called from main() BEFORE pick_scenario: a stranger on a
# machine missing one of these used to see this failure only after already
# answering the interactive "Pick a scenario" prompt, which read as the
# scenario picker itself being broken rather than a missing tool
# (swarmada#26). The LIVE-scenario-only half (python3, proto stub
# generation) can't move here — it depends on LIVE, which pick_scenario's
# answer determines — so it stays in preflight_live, called once LIVE is
# known.
preflight() {
  step "0/7 — Preflight"
  local missing=0
  for tool in kind kubectl docker make go; do
    if ! have "$tool"; then echo "  missing required tool: $tool" >&2; missing=1; fi
  done
  ((missing == 0)) || fail "install the missing tools and re-run (see examples/warehouse-quickstart/README.md)"
  if ! docker info >/dev/null 2>&1; then
    fail "the Docker daemon is not reachable — start Docker and re-run"
  fi
  info "required tools present; Docker daemon reachable"
}

# preflight_live: the LIVE-scenario-only checks split out of preflight()
# above. Called from main() right after determine_live_mode, once LIVE is
# known — still well before any cluster work starts.
preflight_live() {
  if [[ "$LIVE" == 1 ]] && ! have python3 && ! have python; then
    fail "missing required tool: python3 (needed for the LIVE scenario path) — install it and re-run (see examples/warehouse-quickstart/README.md)"
  fi
  if [[ "$LIVE" == 1 && ! -f "proto/fleet_adapter/v1/fleet_adapter_pb2.py" ]]; then
    info "generating Python proto stubs (proto/fleet_adapter/v1/*_pb2*.py) — needed for the LIVE scenario path…"
    make proto-py
  fi
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
  # The CRD existing does not mean the webhook answers: on a fresh cluster the
  # step-4 apply was often refused with "failed calling webhook
  # webhook.cert-manager.io … connection refused". Ask the webhook itself, with a
  # server-side dry run of an Issuer, until it answers. The dry run creates nothing.
  info "waiting for the cert-manager webhook to answer (a Certificate applied before it does is refused)…"
  waited=0
  until printf 'apiVersion: cert-manager.io/v1\nkind: Issuer\nmetadata:\n  name: swq-webhook-probe\n  namespace: cert-manager\nspec:\n  selfSigned: {}\n' \
      | kubectl create --dry-run=server -f - >/dev/null 2>&1; do
    [ "$waited" -ge 180 ] && fail "the cert-manager webhook did not answer in 180s; re-run the same command, which reuses the cluster"
    sleep 2; waited=$((waited + 2))
  done
  info "cert-manager webhook answering after ${waited}s ✓"
}

# wait_for_adapter_client_cert: block until cert-manager has issued the adapter's
# client keypair, then write it where the adapter can read it. The SAN in this
# certificate is what the control plane treats as the adapter's name.
wait_for_adapter_client_cert() {
  # ADAPTER and DIR default to $LIVE_ROBOT's adapter and $ADAPTER_TLS_DIR.
  local adapter="${1:-sim-fleet-adapter}" dir="${2:-$ADAPTER_TLS_DIR}"
  local secret="${adapter}-client-tls"
  info "waiting for cert-manager to issue the $adapter client certificate…"
  local waited=0
  until kubectl -n "$MANAGER_NS" get secret "$secret" >/dev/null 2>&1; do
    [ "$waited" -ge 120 ] && fail "cert-manager never issued $secret"
    sleep 2; waited=$((waited + 2))
  done
  kubectl -n "$MANAGER_NS" wait --for=condition=Ready "certificate/${adapter}-client" --timeout=120s >/dev/null 2>&1 || true
  rm -rf "$dir"; mkdir -p "$dir"; chmod 700 "$dir"
  kubectl -n "$MANAGER_NS" get secret "$secret" \
    -o jsonpath='{.data.tls\.crt}' | base64 -d > "$dir/tls.crt"
  kubectl -n "$MANAGER_NS" get secret "$secret" \
    -o jsonpath='{.data.tls\.key}' | base64 -d > "$dir/tls.key"
  kubectl -n "$MANAGER_NS" get secret "$secret" \
    -o jsonpath='{.data.ca\.crt}' | base64 -d > "$dir/ca.crt"
  chmod 600 "$dir"/*
  [ -s "$dir/tls.crt" ] && [ -s "$dir/tls.key" ] && [ -s "$dir/ca.crt" ] \
    || fail "adapter client certificate material is incomplete in $dir"
  info "$adapter client certificate ready ✓ ($dir)"
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
    # --server-side: these overlays include config/crd/bases/, whose
    # computeworkloadtemplates.swarmada.io CRD (~526KB) breaks client-side apply's
    # last-applied-configuration annotation (262144-byte cap).
    if all_live; then
      # One client certificate per robot's adapter: demo-ab-live is quickstart-mtls plus
      # the two spares' certificates. One adapter identity per live robot, because the
      # command dispatcher keys live streams by adapter name alone.
      info "LIVE mode, every robot live — deploying via config/overlays/demo-ab-live (DEV/DEMO ONLY)"
      kubectl apply -k config/overlays/demo-ab-live --server-side --force-conflicts
      local i
      for i in "${!ADAPTERS[@]}"; do
        wait_for_adapter_client_cert "${ADAPTERS[$i]}" "$(adapter_tls_dir "$i")"
      done
    else
      info "LIVE mode — deploying via config/overlays/quickstart-mtls (DEV/DEMO ONLY)"
      kubectl apply -k config/overlays/quickstart-mtls --server-side --force-conflicts
      wait_for_adapter_client_cert
    fi
  else
    # Projected mode runs NO adapter process, so the manager must run with
    # ControlStream OFF. config/default (what `make deploy-minimal` applies) leaves it
    # on, which makes cmd/manager wire up a non-nil ActionValidator; ADR-0019's
    # validate_action probe then asks the fabricated sim-fleet-adapter whether
    # it can serve each action, finds no stream behind it, and drops every
    # candidate robot — so nothing is ever assigned and this runner times out.
    # config/overlays/quickstart-projected is config/default plus that one flag;
    # it carries the same CRDs + RBAC + manager and still needs no cert-manager.
    info "projected mode — deploying via config/overlays/quickstart-projected (ControlStream off)"
    # --server-side: same oversized-CRD issue as the LIVE branch above.
    kubectl apply -k config/overlays/quickstart-projected --server-side --force-conflicts
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
    # FleetTasks FIRST, and only after cancelling them: the validating webhook
    # refuses to delete a LIVE composite (it owns its child FleetActions and their
    # assignment leases), so an uncancelled task survives this reset and the
    # controller re-creates its children into the next run's assertion — exactly
    # the stale-state leak this function exists to prevent. Deleting the task also
    # GCs its children via ownerReferences, which is why it runs before the
    # fleetactions delete below.
    if [[ -n "$(kubectl get fleettasks -n "$NS" -o name 2>/dev/null || true)" ]]; then
      info "cancelling and deleting leftover FleetTasks from a previous run"
      for ft in $(kubectl get fleettasks -n "$NS" -o name 2>/dev/null || true); do
        kubectl patch "$ft" -n "$NS" --type=merge \
          -p '{"spec":{"desiredState":"Cancelled"}}' >/dev/null 2>&1 || true
      done
      # The controller needs a beat to drive them terminal before delete is admitted.
      for _ in $(seq 1 20); do
        kubectl delete fleettasks -n "$NS" --all --ignore-not-found --wait=true \
          >/dev/null 2>&1 && break
        sleep 1
      done
      if [[ -n "$(kubectl get fleettasks -n "$NS" -o name 2>/dev/null || true)" ]]; then
        # Hard failure, not a warning: continuing would assert against state left
        # by a previous run, which is the failure mode this reset exists to stop.
        fail "leftover FleetTasks could not be cancelled and deleted within 20s — refusing to run against stale state"
      fi
    fi
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

# long_route: send FleetAction $1 to the far corner of warehouse-a, (25, 20), about 32 m from
# the spawn point. The live adapter reports SUCCEEDED on arrival, and to the sample's nominal
# target (10 m, ~10 s) a task finishes before a mid-task event can act on it: the battery
# hand-off, the capability-loss reroute and the estop drill all need work still in progress. Called from
# apply_fleet, before any adapter is up, so the scheduler never sees the short route.
# payload.raw is bytes, so base64: {"x":25.0,"y":20.0}
long_route() {
  kubectl patch "fleetaction/$1" -n "$NS" --type=merge \
    -p '{"spec":{"payload":{"raw":"eyJ4IjoyNS4wLCJ5IjoyMC4wfQ=="}}}' >/dev/null
  info "$1 routed to (25, 20), ~32 s of driving, so the mid-task event meets it en route"
}

apply_fleet() {
  step "5/7 — Apply the sample fleet (reuses $SAMPLE)"
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
  if [[ "$SCENARIO" == "battery-handoff" ]]; then
    # Apply only ONE of the two sample FleetActions. With both applied, the second
    # task would take one of the spares, leaving fewer for the mid-task hand-off to
    # land on. See handle_battery_handoff for where this plays out.
    sample_docs_excluding FleetAction inspect-receiving-dock | kubectl apply -f -
    long_route deliver-pallet-001
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
    #
    # The long route, too: the stop has to meet the task en route. On the sample's own
    # 10 m target the robot arrives in about 10 s, and the drill pauses 8 s (or waits for
    # a keypress) after assignment before it triggers, so the action could reach
    # Succeeded before the estop existed, and the drill then failed with "never
    # paused".
    sample_docs_excluding FleetAction deliver-pallet-001 | kubectl apply -f -
    kubectl patch fleetaction/inspect-receiving-dock -n "$NS" --type=merge \
      -p "{\"spec\":{\"preferredRobot\":\"$LIVE_ROBOT\"}}"
    long_route inspect-receiving-dock
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
    long_route inspect-receiving-dock
    local nav_only='[{"op":"replace","path":"/spec/capabilities","value":[{"name":"navigation","type":"hardware-native","pauseable":false}]}]'
    kubectl patch robot/sim-robot-002 -n "$NS" --type=json -p "$nav_only"
    kubectl patch robot/sim-robot-003 -n "$NS" --type=json -p "$nav_only"

    # Gate $LIVE_ROBOT's camera_front CAPABILITY on a camera_front HARDWARE
    # component. The sample's capabilities are ungated (always Active), so a
    # camera fault would degrade only status.hardware and never the capability the
    # Scheduler reads — no reroute. Declaring the hardware component and pointing
    # the capability's requiredHardware at it makes the derivation degrade the
    # capability when the adapter reports camera_front DEGRADED (T+20s on the LIVE
    # path, see run_live_scenario), which is what triggers Capability-loss
    # reassignment. The adapter reports camera_front
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
  elif [[ "$SCENARIO" == "healthy-fleet" ]]; then
    kubectl apply -f "$SAMPLE"
    # The composite view needs a composite to show. One FleetTask with
    # a single member, alongside the sample's two standalone FleetActions, so
    # swarmtop's `t` screen renders BOTH shapes at once — a task with its member
    # nested beneath it, and actions that no task owns. Kept here rather than in
    # config/samples so every other scenario stays composite-free (and so the
    # zero-composite path keeps being exercised somewhere).
    # Deliberately duplicated rather than applying config/samples/fleettask_sample.yaml:
    # that sample is a three-member chain needing three free robots, which would
    # saturate this scenario's fleet and leave no idle spare.
    kubectl apply -n "$NS" -f - <<'YAML'
apiVersion: swarmada.io/v1
kind: FleetTask
metadata:
  name: receiving-round-001
spec:
  completionPolicy: All
  failurePolicy: FailFast
  desiredState: Running
  actions:
    - name: inspect-dock
      action:
        type: Navigate
        zone: warehouse-a
        priority: Normal
        requiredCapabilities:
          - navigation
YAML
    info "applied 1 FleetZone, ${#ROBOTS[@]} Robots, 2 standalone FleetActions, and 1 FleetTask (1 member)"
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

  # A digest-verified conformance report the conformance harness wrote: the FleetAdapter
  # controller hashes the ConfigMap body and compares it to spec.conformanceReport.digest, so
  # status.conformance comes from the production path and the verdict from a harness run.
  # hack/sim-conformance-report.sh runs the harness when it can and otherwise uses the report
  # recorded in test/samples/testdata/recorded-sim-adapter-report.json, and says which on stderr.
  local report digest
  report="$(mktemp)"
  bash hack/sim-conformance-report.sh "$report" || fail "could not produce a conformance report for the simulation adapter"
  digest="sha256:$(shasum -a 256 <"$report" | awk '{print $1}')"
  kubectl create configmap sim-adapter-conformance -n "$NS" \
    --from-file=report.json="$report" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  rm -f "$report"

  # One FleetAdapter per live adapter process: all three when every robot is live, with
  # each spare bound to its own (spec.adapter.name, and the robot-id annotation
  # RobotStatusSink maps telemetry by — spec and metadata, never status).
  local n=1 i adapter
  all_live && n=${#ADAPTERS[@]}
  for ((i = 0; i < n; i++)); do
    adapter="${ADAPTERS[$i]}"
    if ((i > 0)); then
      kubectl patch "robot/${ROBOTS[$i]}" -n "$NS" --type=json \
        -p "[{\"op\":\"replace\",\"path\":\"/spec/adapter/name\",\"value\":\"$adapter\"}]" >/dev/null
      kubectl annotate "robot/${ROBOTS[$i]}" -n "$NS" "swarmada.io/robot-id=${ROBOTS[$i]}" --overwrite >/dev/null
    fi
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
  # Where the live sim adapter connects on the live path; inert on the projected path. Required by
  # the CRD, so it is always set.
  endpoint: "127.0.0.1:${LOCAL_CS_PORT}"
  # Long: in projected mode nothing ever refreshes lastHeartbeat, so a short interval would let
  # the staleness backstop demote the projected Connected mid-run and silently stop dispatch. In
  # LIVE mode a real heartbeat refreshes this on its own well inside 600s, so the long ceiling
  # costs nothing there — it only widens how long a genuine disconnect takes to notice.
  heartbeatIntervalSeconds: 600
  conformanceReport:
    suiteVersion: "C1-C15"
    configMapRef: sim-adapter-conformance
    digest: "$digest"
EOF
  done

  if all_live; then
    info "FleetAdapters ${ADAPTERS[*]} ready (digest-verified conformance; each becomes"
    info "Connected for real once its own live adapter registers below)"
  elif [[ "$LIVE" == 1 ]]; then
    # phase=Connected is reached FOR REAL on the LIVE path: config/overlays/quickstart-mtls
    # gives the live sim adapter a verified mTLS client identity, and AdapterPresence derives
    # Connected from that identity registering on the real ControlStream. run_live_scenario
    # waits for the real transition once the adapter process is up — nothing here writes it.
    info "FleetAdapter sim-fleet-adapter ready (digest-verified conformance; phase becomes"
    info "Connected for real once the live adapter registers below)"
  else
    # phase=Connected cannot be reached the real way in projected mode — no adapter process
    # ever runs here — so it is projected, the same honest projection this runner already
    # uses for the fully-projected robots (project_readiness, below).
    kubectl patch fleetadapter/sim-fleet-adapter -n "$NS" --subresource=status --type=merge \
      -p "{\"status\":{\"phase\":\"Connected\",\"lastHeartbeat\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}" >/dev/null
    info "FleetAdapter sim-fleet-adapter ready (digest-verified conformance; phase projected Connected)"
  fi
}

project_readiness() {
  step "6/7 — Project status.phase=Idle for the adapter-less robots (simulation stand-in)"
  info "these robots have NO Fleet Adapter, so nothing ever reports liveness for them. The Robot"
  info "reconciler advances Discovered->Idle from fresh adapter liveness (ADR-0029, RFC-0001"
  info "RFC-0001 #control-plane-robot-health-and-connectivity) — with no adapter that never fires, and scheduler filter 1 admits only Idle"
  info "robots. Writing status directly is reserved to controllers (RFC-0001 §9.1.3, RA-1 in"
  info "RFC-0001 Terminology); this patch substitutes for an adapter, not for a missing"
  info "control-plane feature. See docs/quickstart.md, Honest notes."
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
# process against the real ControlStream (port-forwarded from
# config/overlays/quickstart-mtls's Service); the other two robots stay
# status-projected (with a healthy baseline battery, so a battery-edge/
# hardware-fault story on the live robot reads as a real contrast, not an
# artifact of the other two having no battery reported at all).
#
# Phase honesty note: connection, registration, telemetry, AND phase are all
# genuinely real for $LIVE_ROBOT. internal/controller/robot_controller.go's
# reconciler advances a live robot's phase Discovered->{Idle,InProgress} from
# fresh adapter liveness on its own (ADR-0029, RFC-0001 #control-plane-robot-health-and-connectivity) — this was a
# real product gap as of an earlier revision of this runner, closed since. So
# below, once real telemetry from $LIVE_ROBOT is observed (proof the live
# handshake actually worked), this script WAITS for that real transition
# rather than projecting it; only the other two robots (no adapter at all,
# so nothing can ever drive them) are projected.
run_live_scenario() {
  step "6/7 — Drive the robots ready: $LIVE_ROBOT is LIVE (scenario '$SCENARIO')"
  local r
  # When every robot is live (all_live) nothing is projected: each spare's readiness comes
  # from its own adapter.
  if ! all_live; then
    for r in "${ROBOTS[@]}"; do
      [[ "$r" == "$LIVE_ROBOT" ]] && continue
      kubectl patch "robot/$r" -n "$NS" --subresource=status --type=merge \
        -p '{"status":{"phase":"Idle","batteryPercent":85}}'
    done
  fi

  # RobotStatusSink (internal/controller/robot_status_sink.go) only projects
  # telemetry onto a Robot that carries this annotation matching the wire
  # robot_id — without it, real telemetry from $LIVE_ROBOT is silently
  # dropped (by design: an unmapped robot_id must not wedge ingestion) and
  # nothing observable ever lands in status, live_robot_telemetry_seen below
  # included.
  kubectl annotate "robot/$LIVE_ROBOT" -n "$NS" "swarmada.io/robot-id=$LIVE_ROBOT" --overwrite >/dev/null

  info "port-forwarding to the ControlStream Service (127.0.0.1:$LOCAL_CS_PORT -> service/swarmada-controlstream:9445)…"
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
  info "port-forward ready ✓"

  local py
  py="$(command -v python3 || command -v python || true)"
  [[ -n "$py" ]] || fail "python3 (or python) not found — needed to run the live sim adapter"

  local adapter_scenario
  adapter_scenario="$(adapter_scenario_preset "$SCENARIO")"
  # hardware-fault's camera fault fires on the adapter's own clock. At the preset's T+30s it
  # would meet a task that is nearly done; T+20s meets it mid-route (long_route), and T+80s
  # keeps the preset's 60s window before recovery.
  local fault_args=()
  if [[ "$SCENARIO" == "hardware-fault" ]]; then
    fault_args=(--fault-component camera_front --fault-at 20 --recover-at 80)
  fi

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
      --tls-server-name "swarmada-controlstream.${MANAGER_NS}.svc" \
      ${fault_args[@]+"${fault_args[@]}"}
  ) >"$ADAPTER_LOG" 2>&1 &
  local adapter_pid=$!
  LIVE_PIDS+=("$adapter_pid")
  echo "$adapter_pid" >> "$PIDFILE"
  # hardware-fault: the spares start with $LIVE_ROBOT, so every clock starts together.
  # battery-handoff starts them later, in its handler, once $LIVE_ROBOT holds the task.
  if all_live && [[ "$SCENARIO" == "hardware-fault" ]]; then
    launch_spare_adapter 1
    launch_spare_adapter 2
  fi

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
  if ! wait_until "$LIVE_ROBOT real telemetry" 20 live_robot_telemetry_seen; then
    diagnostics
    fail "$LIVE_ROBOT never reported real telemetry within 20s — check $ADAPTER_LOG and the port-forward log"
  fi
  info "real telemetry confirmed (battery reported by the live adapter) ✓"

  info "waiting up to ${READY_TIMEOUT}s for $LIVE_ROBOT to leave Discovered for real (ADR-0029,"
  info "RFC-0001 #control-plane-robot-health-and-connectivity — not patched: the Robot reconciler advances a live robot's phase"
  info "Discovered->{Idle,InProgress} on its own once fresh liveness arrives)…"
  if ! wait_until "$LIVE_ROBOT left Discovered" "$READY_TIMEOUT" live_robot_left_discovered; then
    diagnostics
    fail "$LIVE_ROBOT never left Discovered for real within ${READY_TIMEOUT}s"
  fi
  info "$LIVE_ROBOT phase=$(kubectl get "robot/$LIVE_ROBOT" -n "$NS" -o jsonpath='{.status.phase}') (real) ✓"

  info "waiting up to ${READY_TIMEOUT}s for FleetAdapter sim-fleet-adapter to reach Connected"
  info "for real (verified mTLS identity — not patched)…"
  if ! wait_until "sim-fleet-adapter Connected" "$READY_TIMEOUT" adapter_connected; then
    diagnostics
    fail "FleetAdapter sim-fleet-adapter never reached Connected for real within ${READY_TIMEOUT}s"
  fi
  info "FleetAdapter sim-fleet-adapter phase=Connected (real) ✓"

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

# launch_spare_adapter INDEX — start the real, unmodified sim_adapter.py for spare
# ROBOTS[INDEX], under its own FleetAdapter identity and client certificate, on the
# healthy-fleet preset. exec'd for the same reason as $LIVE_ROBOT's adapter above.
launch_spare_adapter() {
  local i="$1" py robot dir log
  robot="${ROBOTS[$i]}"; dir="$(adapter_tls_dir "$i")"; log="$(adapter_log "$i")"
  py="$(command -v python3 || command -v python)"
  : > "$log"
  (
    cd "$REPO_ROOT"
    exec env PYTHONUNBUFFERED=1 PYTHONPATH="proto${PYTHONPATH:+:${PYTHONPATH}}" \
    "$py" adapters/simulation/sim_adapter.py \
      --endpoint "127.0.0.1:${LOCAL_CS_PORT}" --namespace "$NS" \
      --robot-id "$robot" --zone warehouse-a --vendor simulation \
      --scenario healthy-fleet \
      --tls-ca "$dir/ca.crt" --tls-cert "$dir/tls.crt" --tls-key "$dir/tls.key" \
      --tls-server-name "swarmada-controlstream.${MANAGER_NS}.svc"
  ) >"$log" 2>&1 &
  local pid=$!
  LIVE_PIDS+=("$pid")
  echo "$pid" >> "$PIDFILE"
  info "launched the live adapter for $robot (FleetAdapter ${ADAPTERS[$i]}, log: $log)"
}

robot_battery_seen() {
  [[ -n "$(kubectl get "robot/$1" -n "$NS" -o jsonpath='{.status.batteryPercent}' 2>/dev/null || true)" ]]
}
robot_left_discovered() {
  local p
  p=$(kubectl get "robot/$1" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ -n "$p" && "$p" != "Discovered" && "$p" != "Offline" ]]
}
named_adapter_connected() {
  [[ "$(kubectl get "fleetadapter/$1" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)" == "Connected" ]]
}

# wait_spares_live: every spare's readiness, awaited and never written: real telemetry,
# the Robot reconciler's own Discovered->Idle (ADR-0029), and its FleetAdapter Connected
# from a verified identity.
wait_spares_live() {
  local i r
  for ((i = 1; i < ${#ROBOTS[@]}; i++)); do
    r="${ROBOTS[$i]}"
    wait_until "$r telemetry" 30 robot_battery_seen "$r" ||
      { diagnostics; fail "$r never reported real telemetry within 30s"; }
    wait_until "$r left Discovered" "$READY_TIMEOUT" robot_left_discovered "$r" ||
      { diagnostics; fail "$r never left Discovered for real within ${READY_TIMEOUT}s"; }
    wait_until "${ADAPTERS[$i]} Connected" "$READY_TIMEOUT" named_adapter_connected "${ADAPTERS[$i]}" ||
      { diagnostics; fail "FleetAdapter ${ADAPTERS[$i]} never reached Connected for real within ${READY_TIMEOUT}s"; }
    info "$r live ✓ (phase=$(kubectl get "robot/$r" -n "$NS" -o jsonpath='{.status.phase}'), battery=$(kubectl get "robot/$r" -n "$NS" -o jsonpath='{.status.batteryPercent}')%, reported by its own adapter)"
  done
}

# action_succeeded_on ACTION ROBOT — ACTION is Succeeded and was completed by ROBOT. Only
# the adapter's report of arrival moves a FleetAction to Succeeded; nothing here writes it.
action_succeeded_on() {
  local phase robot
  phase=$(kubectl get "fleetaction/$1" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  robot=$(kubectl get "fleetaction/$1" -n "$NS" -o jsonpath='{.status.assignedRobot}' 2>/dev/null || true)
  [[ "$phase" == "Succeeded" && "$robot" == "$2" ]]
}

# wait_moved_task_succeeded ACTION HOLDER — the task a reroute scenario moved completes on
# the spare that took it, and the spare's own adapter log says so.
wait_moved_task_succeeded() {
  local action="$1" holder="$2" i log=""
  info "waiting up to ${REROUTE_COMPLETE_TIMEOUT}s for $holder to complete $action (its adapter"
  info "reports SUCCEEDED on arrival at (25, 20))…"
  if ! wait_until "$action Succeeded on $holder" "$REROUTE_COMPLETE_TIMEOUT" action_succeeded_on "$action" "$holder"; then
    diagnostics
    fail "$action was moved to $holder but did not reach Succeeded there within ${REROUTE_COMPLETE_TIMEOUT}s"
  fi
  for i in "${!ROBOTS[@]}"; do [[ "${ROBOTS[$i]}" == "$holder" ]] && log="$(adapter_log "$i")"; done
  info "$action Succeeded on $holder ✓ — its adapter: $(grep -m1 "ACTION_SUCCEEDED robot=$holder action=$action" "$log" 2>/dev/null || echo "(no ACTION_SUCCEEDED line in $log)")"
  MOVED_TASK="$action"
  MOVED_TO="$holder"
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

# live_robot_left_discovered: true once ADR-0029's real Discovered->{Idle,InProgress}
# transition has happened, from EITHER resulting phase. Checking for literal "Idle"
# alone races the real scheduler: assignment can land in the same reconcile tick real
# liveness arrives, moving phase straight through Idle into InProgress/Assigned before
# a 3s poll ever samples it. Any phase other than the pre-liveness set is equally valid
# proof the real transition already fired.
live_robot_left_discovered() {
  local phase
  phase=$(kubectl get "robot/$LIVE_ROBOT" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ -n "$phase" && "$phase" != "Discovered" && "$phase" != "Offline" ]]
}

# BATTERY_HANDOFF_ORIGINAL_ROBOT: set by handle_battery_handoff, read by its
# wait predicate battery_handoff_reassigned.
BATTERY_HANDOFF_ORIGINAL_ROBOT=""
# MOVED_TASK / MOVED_TO: set by wait_moved_task_succeeded once the task a reroute scenario
# moved has completed on the spare; read by assert_end_state.
MOVED_TASK=""
MOVED_TO=""

# handle_battery_handoff: battery-handoff scenario only. Every robot is live
# (all_live). $LIVE_ROBOT came online alone, on the battery-edge preset, so it is the only
# robot that can take deliver-pallet-001 (the only sample task applied this scenario, on
# long_route), and its adapter reports its battery draining toward 0 while it drives. The
# spares then come online, each on its own live adapter, at full battery.
#
# The hand-off itself uses the REAL, existing, safe mechanism ZoneMaintenance's Immediate
# mode uses: annotating the FleetAction with swarmada.io/requeue-requested makes
# fleetaction_controller.go push a real cancel_task down the holder's live ControlStream
# and wait for a CONFIRMED stop (the sim adapter acks STOPPED_SAFELY) before releasing the
# task. The scheduler then assigns it like any Pending task, and its battery-descending
# order sends it to a spare rather than back to the drained robot.
#
# Honesty note: deciding WHEN to hand off is this script's call — there is no controller
# anywhere that watches battery and triggers a hand-off on its own (that would be real,
# unbuilt product work). Everything from the annotation onward — the cancel push, the
# confirmed stop, the release, the reassignment, the spare's completion — is real,
# unmodified control-plane code, and every battery value shown is reported by an adapter.
handle_battery_handoff() {
  step "6.5/7 — battery-handoff: hand a working robot's task to a live spare"
  local holder new_holder
  info "waiting up to 20s for deliver-pallet-001 to land on $LIVE_ROBOT (the only live robot so far)…"
  if ! wait_until "deliver-pallet-001 assigned" 20 deliver_pallet_assigned; then
    diagnostics
    fail "deliver-pallet-001 was never assigned — nothing to hand off"
  fi
  holder=$(kubectl get fleetaction deliver-pallet-001 -n "$NS" -o jsonpath='{.status.assignedRobot}')
  BATTERY_HANDOFF_ORIGINAL_ROBOT="$holder"
  info "deliver-pallet-001 is held by $holder ✓"

  info "bringing the spares online, each on its own live adapter…"
  launch_spare_adapter 1
  launch_spare_adapter 2
  wait_spares_live

  info "$holder's battery is now $(kubectl get "robot/$holder" -n "$NS" -o jsonpath='{.status.batteryPercent}')%, reported by its adapter"
  info "requesting a safe hand-off (swarmada.io/requeue-requested) — the same real,"
  info "confirmed-stop mechanism ZoneMaintenance's Immediate mode uses…"
  kubectl annotate fleetaction deliver-pallet-001 -n "$NS" \
    "swarmada.io/requeue-requested=battery low (hand-off requested by this script)" --overwrite >/dev/null

  info "waiting up to 40s for the confirmed stop and the reassignment to a spare…"
  if ! wait_until "deliver-pallet-001 reassigned off $holder" 40 battery_handoff_reassigned; then
    diagnostics
    fail "deliver-pallet-001 was never reassigned off $holder within 40s"
  fi
  new_holder=$(kubectl get fleetaction deliver-pallet-001 -n "$NS" -o jsonpath='{.status.assignedRobot}')
  info "hand-off confirmed ✓ — deliver-pallet-001 moved from $holder to $new_holder"
  info "  $holder's adapter: $(grep -m1 "CANCEL_HONOURED robot=$holder" "$ADAPTER_LOG" 2>/dev/null || echo "(no CANCEL_HONOURED line in $ADAPTER_LOG)")"
  wait_moved_task_succeeded deliver-pallet-001 "$new_holder"
}

# handle_hardware_fault: hardware-fault scenario only (defined further down). Unlike
# battery-handoff, the reroute here is NOT script-triggered — the control plane
# detects it. The live robot ($LIVE_ROBOT) holds the camera task
# (inspect-receiving-dock; it is the only camera-capable robot, apply_fleet having
# stripped 002/003). When its camera degrades at ~20s (real adapter fault), the
# Capability Controller marks status.capabilities[camera_front]=Degraded, and
# fleetaction_controller.go's Capability-loss reassignment (RFC-0001) automatically
# pushes a real cancel_task, the sim adapter acks STOPPED_SAFELY, and the task
# requeues. The function only stages the reroute target: it re-adds camera_front to
# sim-robot-002 (a live, idle spare) so the automatic reassignment has somewhere to
# land. Everything from the capability degrading onward is real, unmodified
# control-plane code.
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
# (RFC-0001 #safety-fleetaction-behaviour-during-estop — estop takes precedence over the lease).
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

  pause_for_demo "$LIVE_ROBOT is executing $ESTOP_ACTION. Press Enter to TRIGGER an emergency stop — the control plane sends it over the live ControlStream, the robot CONFIRMS it from simulator ground truth, and $ESTOP_ACTION is paused. The stop goes out on its own after ${ESTOP_DRILL_MAX_PAUSE}s, while $LIVE_ROBOT is still driving." "$ESTOP_DRILL_MAX_PAUSE"

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
    info "safe hold confirmed ✓ — $ESTOP_ACTION was paused by the estop (RFC-0001 #safety-fleetaction-behaviour-during-estop)"
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
  step "6.5/7 — hardware-fault: the camera fails on the adapter's clock; the control plane reroutes"
  # Every robot is live (all_live); nothing below writes any status. $LIVE_ROBOT's
  # camera_front CAPABILITY is gated on its camera_front HARDWARE (apply_fleet), and its
  # adapter reports that hardware HEALTHY until --fault-at, then DEGRADED.
  wait_spares_live

  info "waiting up to ${READY_TIMEOUT}s for $LIVE_ROBOT's camera_front capability to derive Active"
  info "from its own pre-fault hardware telemetry…"
  if ! wait_until "$LIVE_ROBOT camera_front Active" "$READY_TIMEOUT" camera_capability_is "$LIVE_ROBOT" Active; then
    diagnostics
    fail "$LIVE_ROBOT's camera_front capability never derived Active"
  fi

  info "waiting up to 20s for the camera task to land on ${LIVE_ROBOT}…"
  if ! wait_until "inspect-receiving-dock assigned to $LIVE_ROBOT" 20 inspect_assigned_to_live; then
    diagnostics
    fail "inspect-receiving-dock never landed on $LIVE_ROBOT — cannot demonstrate the reroute"
  fi
  info "inspect-receiving-dock is held by $LIVE_ROBOT ✓"

  info "restoring camera_front on sim-robot-002 (spec, ungated so Active) as the reroute target…"
  kubectl patch robot/sim-robot-002 -n "$NS" --type=json -p \
    '[{"op":"add","path":"/spec/capabilities/-","value":{"name":"camera_front","type":"hardware-native","pauseable":false}}]'

  info "waiting up to 60s for the REAL camera fault (adapter clock, ~T+20s from $LIVE_ROBOT's"
  info "adapter start) to degrade the capability and for the control plane to AUTOMATICALLY"
  info "safe-stop and reroute the task — no annotation, no script trigger, no patch…"
  if ! wait_until "inspect-receiving-dock reassigned off $LIVE_ROBOT" 60 inspect_reassigned_off_live; then
    diagnostics
    fail "inspect-receiving-dock was never rerouted off $LIVE_ROBOT within 60s"
  fi
  local new_holder
  new_holder=$(kubectl get fleetaction inspect-receiving-dock -n "$NS" -o jsonpath='{.status.assignedRobot}')
  info "reroute confirmed ✓ — inspect-receiving-dock moved from $LIVE_ROBOT to $new_holder on capability loss"
  info "  $LIVE_ROBOT capabilities[camera_front]=$(kubectl get "robot/$LIVE_ROBOT" -n "$NS" -o jsonpath="{.status.capabilities[?(@.name=='camera_front')].status}")"
  wait_moved_task_succeeded inspect-receiving-dock "$new_holder"
}

# camera_capability_is ROBOT STATUS — ROBOT's derived camera_front capability status.
camera_capability_is() {
  [[ "$(kubectl get "robot/$1" -n "$NS" -o jsonpath="{.status.capabilities[?(@.name=='camera_front')].status}" 2>/dev/null || true)" == "$2" ]]
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
# i.e. the REAL scheduler bound a task to a robot. On the LIVE path the run then
# also waits for the live robot to complete it (the sim adapter reports completion).
task_assigned() {
  local vals
  vals=$(kubectl get fleetactions -n "$NS" -o jsonpath='{.items[*].status.assignedRobot}' 2>/dev/null || true)
  [[ -n "${vals// /}" ]]
}

# task_succeeded: some FleetAction reached Succeeded. Only the adapter's own report of
# arrival moves a FleetAction there; nothing in this script writes that phase.
task_succeeded() {
  local phases
  phases=$(kubectl get fleetactions -n "$NS" -o jsonpath='{.items[*].status.phase}' 2>/dev/null || true)
  [[ " $phases " == *" Succeeded "* ]]
}

# wait_until DESC TIMEOUT PREDICATE [ARGS...] — poll PREDICATE ARGS until true or TIMEOUT secs.
wait_until() {
  local desc="$1" timeout="$2" pred="$3" elapsed=0 interval=3
  shift 3
  while true; do
    if "$pred" "$@"; then return 0; fi
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
    if all_live; then
      local i
      for ((i = 1; i < ${#ROBOTS[@]}; i++)); do
        echo "---- live adapter log for ${ROBOTS[$i]} ($(adapter_log "$i")) ----" >&2
        tail -n 30 "$(adapter_log "$i")" >&2 2>&1 || true
      done
    fi
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

  # Assignment is half the loop. On the live healthy-fleet path nothing interrupts the
  # work, so the robot must also finish it: the adapter reports arrival, and the control
  # plane moves the FleetAction to Succeeded. The other scenarios interrupt work on
  # purpose and assert their own payoff below; the projected path has no adapter.
  if [[ "$LIVE" == 1 && "$SCENARIO" == "healthy-fleet" ]]; then
    info "waiting up to ${COMPLETE_TIMEOUT}s for the live robot to complete its FleetAction…"
    if ! wait_until "task succeeded" "$COMPLETE_TIMEOUT" task_succeeded; then
      diagnostics
      echo "❌ Quickstart FAILED: no FleetAction reached Succeeded within ${COMPLETE_TIMEOUT}s" >&2
      echo "   (the live adapter reports SUCCEEDED when the robot arrives at its target)." >&2
      exit 1
    fi
    info "a FleetAction was completed by the live robot ✓"
  fi

  # The reroute scenarios' own payoff, re-asserted: the moved task completed on the spare.
  if all_live && ! action_succeeded_on "$MOVED_TASK" "$MOVED_TO"; then
    diagnostics
    echo "❌ Quickstart FAILED: ${MOVED_TASK:-the moved task} is not Succeeded on ${MOVED_TO:-a spare}." >&2
    exit 1
  fi

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
  # Composites are printed separately: a child FleetAction appears above under its
  # generated "<task>-<action>" name, which on its own gives no sign a parent
  # exists. Without this the end state would show the work but not the shape of it.
  if [[ -n "$(kubectl get fleettasks -n "$NS" -o name 2>/dev/null || true)" ]]; then
    kubectl get fleettasks -n "$NS"
    echo
  fi
  if [[ "$SCENARIO" == "estop-drill" && "$LIVE" == 1 ]]; then
    echo "✅ Quickstart end state reached: $LIVE_ROBOT was emergency-stopped while holding"
    echo "   work, CONFIRMED the stop from simulator ground truth, and its action was"
    echo "   paused; the rest of the fleet stayed healthy and scheduling continued."
  elif [[ "$LIVE" == 1 && "$SCENARIO" == "healthy-fleet" ]]; then
    echo "✅ Quickstart end state reached: all robots Idle, the real scheduler"
    echo "   assigned a FleetAction in warehouse-a, and the live robot completed one."
  elif all_live; then
    echo "✅ Quickstart end state reached: every robot live on its own adapter; $MOVED_TASK"
    echo "   was taken off $LIVE_ROBOT mid-route, reassigned by the real scheduler to"
    echo "   $MOVED_TO, and completed there (its adapter reported SUCCEEDED)."
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
  preflight
  pick_scenario
  if ((CLEAN_REQUESTED)); then
    do_clean_everything
    exit 0
  fi
  determine_live_mode
  preflight_live
  announce_scenario
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

# Executed, run. Sourced, only define the functions: a test calls them with
# kubectl stubbed (tests/python/test_warehouse_quickstart_run_sh.py).
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
