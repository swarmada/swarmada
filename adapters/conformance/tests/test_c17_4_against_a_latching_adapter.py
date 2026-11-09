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

"""C17.4 against an adapter that keeps the stop latch and declares an operating subject.

Why this file exists. C17.4 sends an assignment that carries only operating subjects the
adapter declared it honours, and requires it ACCEPTED. The harness runs it on the robot it
stopped in C5. An adapter that keeps the stop latch (RFC-0001
#fleet-adapter-protocol-the-stop-latch) refuses every assignment ``ROBOT_UNAVAILABLE`` until
the stop's EstopClear lifts it, so before the harness cleared its stops it failed C17.4 for
an adapter that met the latch. Every in-tree latching adapter declares no subject, so C17.4
skipped (``adapter-declares-no-operating-subject``) and no run reached the row.

So this runs the whole harness, as ``make conformance-sim`` does, against the latching fake
with ``--honour camera``. The first test fails on the harness before it cleared its stops
(C17.4: declined ``ROBOT_UNAVAILABLE``, "stop latched"). The second shows the row still
fails a latching adapter whose declaration is false.
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


def _run(tmp_path: Path, *fake_args: str) -> tuple[dict, dict, subprocess.CompletedProcess]:
    """Run the harness against the fake. Returns (rows by check id, the report, the run)."""
    report = tmp_path / "report.json"
    witness = tmp_path / "witness"
    adapter_cmd = " ".join(shlex.quote(a) for a in (
        sys.executable, str(FAKE), "--endpoint", "localhost:{port}",
        "--witness-file", str(witness), "--provisioning-interval", "1", *fake_args))
    run = subprocess.run(
        [sys.executable, "-m", "adapters.conformance", "--stub-path", "proto",
         "--adapter-name", "latching-fake", "--port", str(_free_port()),
         "--witness-file", str(witness), "--json", str(report), "--adapter-cmd", adapter_cmd],
        cwd=ROOT, capture_output=True, text=True, timeout=300)
    assert report.exists(), (
        f"the harness wrote no report:\n{run.stdout[-3000:]}\n{run.stderr[-3000:]}")
    doc = json.loads(report.read_text())
    return {r["check_id"]: r for r in doc["results"]}, doc, run


def test_a_latching_adapter_that_declares_a_subject_passes_c17_4(tmp_path):
    rows, doc, run = _run(tmp_path, "--honour", "camera")
    assert doc.get("honoured_operating_constraints") == ["camera"], doc.get(
        "honoured_operating_constraints")
    for cid in ("C17.4", "C17.1", "C17.2"):
        assert rows[cid]["status"] == "pass", (cid, rows[cid].get("detail"))
    # The run is against an adapter that keeps the latch, or it proves nothing: C5.8 passes
    # only when a stop held the robot's work until its EstopClear lifted it.
    assert rows["C5.8"]["status"] == "pass", rows["C5.8"].get("detail")
    failed = sorted(k for k, r in rows.items()
                    if r["status"] == "fail" and r.get("level") in ("MUST", "MUST NOT"))
    assert not failed, "\n".join(f"{k}: {rows[k].get('detail')}" for k in failed)
    assert run.returncode == 0, f"the harness exited {run.returncode}:\n{run.stdout[-2000:]}"


def test_c17_4_still_fails_a_latching_adapter_whose_declaration_is_false(tmp_path):
    rows, doc, _ = _run(tmp_path, "--honour", "camera", "--plant", "declares-unhonoured")
    assert doc.get("honoured_operating_constraints") == ["camera"]
    assert rows["C5.8"]["status"] == "pass", rows["C5.8"].get("detail")
    c174 = rows["C17.4"]
    assert c174["status"] == "fail", c174.get("detail")
    assert "DECLINED" in c174["detail"] and "camera" in c174["detail"], c174["detail"]
