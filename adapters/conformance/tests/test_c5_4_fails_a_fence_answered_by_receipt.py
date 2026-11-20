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

"""C5.4 fails an adapter that answers a fence only on ControlStream.

``latching_fake.py --plant receipt-only`` answers a fence with a ``CommandResult.estop``
receipt and sends no ``EstopAck`` for it on SafetyStream: the stop's acknowledgement travels
on ControlStream in its place, which C5.4 forbids. The simulation adapter it is built on
answers a fence ``ESTOP_STATE_STOPPING`` on SafetyStream before it confirms rest, so the
plant must hold back that first answer as well as the last, or the harness sees an
``EstopAck`` and C5.4 passes. This runs the whole harness against the plant, as
``make conformance-sim`` does, and requires C5.4 to fail it.
"""

from __future__ import annotations

import json
import shlex
import socket
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
FAKE = ROOT / "adapters" / "conformance" / "tests" / "latching_fake.py"


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_c5_4_fails_a_fence_answered_only_by_its_receipt(tmp_path):
    report = tmp_path / "report.json"
    witness = tmp_path / "witness"
    adapter_cmd = " ".join(shlex.quote(a) for a in (
        sys.executable, str(FAKE), "--endpoint", "localhost:{port}",
        "--witness-file", str(witness), "--provisioning-interval", "1",
        "--plant", "receipt-only"))
    run = subprocess.run(
        [sys.executable, "-m", "adapters.conformance", "--stub-path", "proto",
         "--adapter-name", "latching-fake", "--port", str(_free_port()),
         "--witness-file", str(witness), "--json", str(report), "--adapter-cmd", adapter_cmd],
        cwd=ROOT, capture_output=True, text=True, timeout=300)
    assert report.exists(), (
        f"the harness wrote no report:\n{run.stdout[-3000:]}\n{run.stderr[-3000:]}")
    rows = {r["check_id"]: r for r in json.loads(report.read_text())["results"]}
    c54 = rows["C5.4"]
    assert c54["status"] == "fail", (
        f"C5.4 {c54['status']} an adapter that answers a fence only with its receipt: "
        f"{c54.get('detail')}")
    assert "CommandResult.estop" in c54["detail"], c54["detail"]
