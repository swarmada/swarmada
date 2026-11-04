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

# The ROS 2 adapter inside the robot's own container, as the robot run places it: rclpy and the
# robot's DDS graph are there, and the adapter runs the robot-side scripts locally
# (--shell local), reaching the cluster over mTLS and the object store over the LAN.
#
#   adapter-in-container.sh stage <adapter dir> <sdk dir> <bound dir>
#       the adapter's files, the SDK and one bound into the container's work directory, each
#       proven by an md5 manifest at both ends
#   adapter-in-container.sh start <robot id> [adapter flags...]
#       the adapter, its output in the foreground of this terminal (`stop` ends it; Ctrl-C here
#       ends only this terminal's docker exec, and the adapter runs on)
#   adapter-in-container.sh stop
#       the adapter, by the pid it wrote at start: SIGTERM, and up to 20 s for it to stop the
#       robot and exit
#
# How the container is reached: `ssh <user>@<host> sudo docker exec -i -u <user> <container>`,
# from the robot file's keys; or EXEC, a command prefix that runs its arguments in the
# container (a stand-in sets EXEC="docker exec -i <container>").
#
# What the adapter is given reaches the container on stdin, in the environment of the one bash
# that execs it: the work directory, environment script and overlay (the robot file,
# ROBOT_ENV, default ~/.config/swarmada/robot.env), this run's store credential (the same
# file, SWARMADA_STORE_ACCESS_KEY / _SECRET_KEY), and the endpoints and the client certificate
# and key (RUN_DIR, default ~/.config/swarmada/robot-run, written by
# examples/floor-mapping/object-store/setup.sh). None is on a command line, none is written to a
# file on the robot, none is printed.
#
# The adapter starts from the robot's own ROS environment and the explorer's overlay (it reads
# the explorer's status message type), with its own files appended to PYTHONPATH.

set -euo pipefail

ROBOT_ENV="${ROBOT_ENV:-$HOME/.config/swarmada/robot.env}"
RUN_DIR="${RUN_DIR:-$HOME/.config/swarmada/robot-run}"

fail() { echo "FAIL: $*" >&2; exit 1; }
usage() { sed -n '19,28p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 2; }

[[ -r "$ROBOT_ENV" ]] || fail "the robot file ($ROBOT_ENV) cannot be read"
set -a
# shellcheck disable=SC1090
. "$ROBOT_ENV"
set +a
for k in SWARMADA_ROBOT_WORKDIR SWARMADA_ROBOT_ENV_SCRIPT SWARMADA_ROBOT_OVERLAY; do
  [[ -n "${!k:-}" ]] || fail "the robot file does not set $k"
done
W="$SWARMADA_ROBOT_WORKDIR"

if [[ -n "${EXEC:-}" ]]; then
  read -r -a PREFIX <<<"$EXEC"
else
  for k in SWARMADA_ROBOT_HOST SWARMADA_ROBOT_SSH_USER SWARMADA_ROBOT_CONTAINER_USER SWARMADA_ROBOT_CONTAINER; do
    [[ -n "${!k:-}" ]] || fail "the robot file does not set $k (or set EXEC)"
  done
  PREFIX=(ssh "$SWARMADA_ROBOT_SSH_USER@$SWARMADA_ROBOT_HOST" sudo docker exec -i
          -u "$SWARMADA_ROBOT_CONTAINER_USER" "$SWARMADA_ROBOT_CONTAINER")
fi

# Run "$@" in the container. Over ssh the remote side is one command line, so every word after
# the host is quoted once here; docker exec takes the words as they are.
in_container() {
  if [[ "${PREFIX[0]}" == ssh ]]; then
    local remote=() w
    for w in "${PREFIX[@]:2}" "$@"; do remote+=("$(printf '%q' "$w")"); done
    "${PREFIX[@]:0:2}" "${remote[*]}"
  else
    "${PREFIX[@]}" "$@"
  fi
}

md5_of() { if command -v md5sum >/dev/null 2>&1; then md5sum | cut -d' ' -f1; else md5 -q; fi; }

local_manifest() {
  (cd "$1" && find . -type f ! -path './.git/*' ! -name '*.pyc' ! -path '*/__pycache__/*' |
     LC_ALL=C sort | while IFS= read -r f; do printf '%s  %s\n' "$(md5_of < "$f")" "$f"; done) | md5_of
}

remote_manifest() {
  in_container bash -s <<EOF
cd $(printf '%q' "$1") && find . -type f ! -name '*.pyc' ! -path '*/__pycache__/*' | LC_ALL=C sort |
  while IFS= read -r f; do printf '%s  %s\n' "\$(md5sum < "\$f" | cut -d' ' -f1)" "\$f"; done | md5sum | cut -d' ' -f1
EOF
}

push() {  # <local dir> <remote dir> <label>
  in_container bash -c "rm -rf $(printf '%q' "$2") && mkdir -p $(printf '%q' "$2")"
  COPYFILE_DISABLE=1 tar -C "$1" --exclude .git --exclude '__pycache__' --exclude '*.pyc' -cf - . |
    in_container bash -c "tar -x -C $(printf '%q' "$2") 2>/dev/null"
  local here there
  here="$(local_manifest "$1")"; there="$(remote_manifest "$2")"
  [[ "$here" == "$there" ]] || fail "STAGE MISMATCH $3: manifest md5 $here here, $there in the container"
  echo "STAGED $3: manifest md5 $here, both ends"
}

stage() {
  [[ $# -eq 3 ]] || usage
  [[ -d "$1/fleet_adapter_ros2" ]] || fail "$1 is not the adapter's tree"
  [[ -d "$2/swarmada_sdk" ]] || fail "$2 does not hold swarmada_sdk"
  [[ -f "$3/bound.json" ]] || fail "$3 is not a bound (no bound.json)"
  push "$1" "$W/adapter" adapter
  push "$2" "$W/adapter-deps" sdk
  push "$3" "$W/bounds/$(basename "$3")" "bound $(basename "$3")"
}

start() {
  [[ $# -ge 1 ]] || usage
  local robot_id="$1"; shift
  [[ -r "$RUN_DIR/endpoints.env" ]] || fail "no endpoints in RUN_DIR: run setup.sh forward first"
  for f in ca.crt tls.crt tls.key; do [[ -r "$RUN_DIR/$f" ]] || fail "no $f in RUN_DIR: run setup.sh install first"; done
  for k in SWARMADA_STORE_ACCESS_KEY SWARMADA_STORE_SECRET_KEY; do
    [[ -n "${!k:-}" ]] || fail "the robot file does not set $k: run setup.sh install (or rotate)"
  done
  set -a
  # shellcheck disable=SC1091
  . "$RUN_DIR/endpoints.env"
  set +a
  local q=() a
  for a in "$@"; do q+=("$(printf '%q' "$a")"); done
  {
    echo "set -euo pipefail"
    for k in SWARMADA_ROBOT_WORKDIR SWARMADA_ROBOT_ENV_SCRIPT SWARMADA_ROBOT_OVERLAY \
             SWARMADA_STORE_ACCESS_KEY SWARMADA_STORE_SECRET_KEY; do
      printf 'export %s=%q\n' "$k" "${!k}"
    done
    printf 'export SWARMADA_TLS_CA=%q\n' "$(cat "$RUN_DIR/ca.crt")"
    printf 'export SWARMADA_TLS_CERT=%q\n' "$(cat "$RUN_DIR/tls.crt")"
    printf 'export SWARMADA_TLS_KEY=%q\n' "$(cat "$RUN_DIR/tls.key")"
    cat <<'INNER'
W="$SWARMADA_ROBOT_WORKDIR"
set +u; source "$SWARMADA_ROBOT_ENV_SCRIPT"; source "$SWARMADA_ROBOT_OVERLAY"; set -u
export PYTHONPATH="${PYTHONPATH:+$PYTHONPATH:}$W/adapter:$W/adapter-deps"
mkdir -p "$W/out" "$W/artifacts"; echo $$ > "$W/out/adapter.pid"
cd "$W/adapter"
INNER
    printf 'exec python3 -m fleet_adapter_ros2.adapter --binding ros2 --shell local --site-env env'
    printf ' --robot-id %q --namespace warehouse-a' "$robot_id"
    printf ' --endpoint %q --tls-server-name %q' "$SWARMADA_CONTROL_ENDPOINT" "$SWARMADA_CONTROL_SERVER_NAME"
    printf ' --tls-ca env:SWARMADA_TLS_CA --tls-cert env:SWARMADA_TLS_CERT --tls-key env:SWARMADA_TLS_KEY'
    printf ' --artifact-store floor-maps --artifact-root "$W/artifacts"'
    printf ' --store-endpoint %q --store-bucket floor-maps' "$SWARMADA_STORE_ENDPOINT"
    printf ' %s\n' "${q[*]}"
  } | in_container bash -s
}

stop() {
  in_container bash -s <<EOF
P=\$(cat $(printf '%q' "$W")/out/adapter.pid 2>/dev/null) || { echo "no adapter pid file"; exit 0; }
# A zombie has exited: a container whose first process reaps nothing keeps it, and kill -0 on it
# succeeds.
alive() { local s; s=\$(ps -o stat= -p "\$P" 2>/dev/null); [ -n "\$s" ] && [ "\${s#Z}" = "\$s" ]; }
alive || { echo "adapter stopped (it was not running)"; exit 0; }
# On SIGTERM the adapter holds the base, cancels its goal, stops the explorer and ends what its
# segment launched, then exits: 6.2 s on the stand-in with a segment running. A kill before
# that cuts its stop short, so it is given 20 s.
kill -TERM "\$P" 2>/dev/null
i=0; while alive && [ \$i -lt 200 ]; do sleep 0.1; i=\$((i + 1)); done
if ! alive; then echo "adapter stopped (in \$((i / 10)).\$((i % 10)) s)"; exit 0; fi
kill -9 "\$P" 2>/dev/null; sleep 0.5
alive && echo "THE ADAPTER IS STILL RUNNING (pid \$P)" || echo "THE ADAPTER WAS KILLED after 20 s: its own stop may not have finished"
EOF
}

cmd="${1:-}"; shift || true
case "$cmd" in
  stage|start|stop) "$cmd" "$@" ;;
  *) usage ;;
esac
