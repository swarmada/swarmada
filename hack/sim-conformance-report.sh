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

# sim-conformance-report.sh OUT — write a conformance report for the in-tree simulation
# adapter to OUT, for a demo's FleetAdapter to pin by digest.
#
# The report is always one the conformance harness wrote. A demo never writes the
# verdict itself: a hand-written {"conformant":true} makes the FleetAdapter's
# conformance gate pass on a result no run produced, and a hand-written
# contract_version drifts out of the window the control plane accepts, after which
# every robot bound to that adapter is withheld from dispatch.
#
#   1. The harness can run here (python3 with the gRPC runtime and the committed
#      stubs): run `make conformance-sim` with --json OUT, on a free port so it cannot
#      collide with another run. The report is whatever that run concluded; a
#      non-conformant result is written as-is and the FleetAdapter will be Failed.
#   2. It cannot (no python3, or no gRPC runtime, as on the status-projected CI path):
#      copy the harness report recorded in this repository,
#      test/samples/testdata/recorded-sim-adapter-report.json, and say so. That is a
#      real harness run of this adapter, not this checkout's run; test/samples checks
#      its contract version against the control plane's supported range, so it cannot
#      silently fall out, and runs this fallback to check it copies that file.
#      (conformance-reports/ is not it: that directory is the local, gitignored output
#      of `make conformance-profiles` and does not exist in a fresh clone.)
#
# SIM_CONFORMANCE_REPORT=recorded forces (2); =run forces (1) and fails if it cannot.
# Provenance goes to stderr; OUT holds only the report.
set -euo pipefail

out="${1:?usage: $0 OUT}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
recorded="$root/test/samples/testdata/recorded-sim-adapter-report.json"
mode="${SIM_CONFORMANCE_REPORT:-auto}"

can_run_harness() {
  command -v python3 >/dev/null 2>&1 &&
    (cd "$root" && python3 -c 'import grpc, google.protobuf, sys; sys.path[:0] = ["proto", "."];
from fleet_adapter.v1 import fleet_adapter_pb2' >/dev/null 2>&1)
}

use_recorded() {
  [[ -s "$recorded" ]] || {
    echo "sim-conformance-report: the recorded harness report $recorded is missing or empty." \
      "It is committed with the repository; restore it from your checkout's source. There is" \
      "no other recorded report to fall back to, and this script does not write a verdict itself." >&2
    exit 1; }
  cp "$recorded" "$out"
  echo "sim-conformance-report: using the recorded harness report $(basename "$recorded")" \
    "(the harness cannot run here: $1). It is a real run of this adapter, not a run of this checkout." >&2
}

run_harness() {
  local port log
  port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
  log="$(mktemp)"
  echo "sim-conformance-report: running the conformance harness against the simulation adapter (port $port)…" >&2
  # -o proto-py: use the committed stubs, as the adapter itself does, instead of
  # regenerating them into the working tree during a demo.
  set +e
  (cd "$root" && make -s -o proto-py conformance-sim CONFORMANCE_PORT="$port" \
    CONFORMANCE_ARGS="--json $out") >"$log" 2>&1
  local rc=$?
  set -e
  # The harness exits 1 for a completed non-conformant run and 2 when the run itself did
  # not complete; only a completed run wrote a report.
  if [[ ! -s "$out" ]]; then
    echo "sim-conformance-report: the harness run wrote no report (exit $rc); last lines:" >&2
    tail -n 20 "$log" >&2
    rm -f "$log"
    return 1
  fi
  # `counts` is the raw per-status tally, where a failing SHOULD/MAY row lands in "fail" too.
  # Printed as-is, a conformant run read "fail 1". Split it the way the harness's own summary
  # does (adapters/conformance/report.py, ADR-0048): a failing SHOULD or MAY row is an
  # advisory, and every other fail is a failure. Counted from the advisory side, so a row with
  # no level (the harness defaults it to MUST) or a report with no rows at all can only
  # overstate failures, never hide one.
  python3 - "$out" >&2 <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
c = r.get("counts", {})
advisory = sum(1 for x in r.get("results", [])
               if x.get("status") == "fail" and x.get("level") in ("SHOULD", "MAY"))
blocking = c.get("fail", 0) - advisory
print(f"sim-conformance-report: harness verdict conformant={str(r.get('conformant')).lower()} "
      f"contract_version={r.get('contract_version')} "
      f"(pass {c.get('pass')}, fail {blocking}, advisory {advisory}, skip {c.get('skip')})")
PY
  rm -f "$log"
}

case "$mode" in
  recorded) use_recorded "SIM_CONFORMANCE_REPORT=recorded" ;;
  run) run_harness ;;
  auto)
    if can_run_harness; then
      run_harness
    else
      use_recorded "no python3 with the gRPC runtime and the fleet_adapter.v1 stubs"
    fi ;;
  *) echo "sim-conformance-report: SIM_CONFORMANCE_REPORT must be auto, run or recorded (got '$mode')" >&2; exit 2 ;;
esac
