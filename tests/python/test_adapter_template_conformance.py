# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""An adapter generated from `adapters/template/` passes the conformance harness.

Why this file exists. The template README promises that a generated adapter, run unmodified
through the harness, fails no check, and its own `make conformance` is the second command an
adapter author types. When C3.9 joined the suite (a cancel naming a preempted action must
leave the preemptor running to completion), the simulation adapter was fixed and the
template was not: its cancel stopped whatever was running, whatever the cancel named, and
every generated adapter was NON-CONFORMANT on C3.9 while the README said otherwise.
`make check-adapter-template` rendered the template and ran its unit tests, and none of them
drives the protocol, so nothing here could see it.

So this renders the template the way that check does, starts the rendered adapter under the
real harness, and reads the report. C3.9 is asserted by name, because it grades two things
the template had to learn together: the cancel reads `action_id`, and the simulated robot
arrives, so the preemptor can report SUCCEEDED. The whole-report assertion catches the next
check the template falls behind on.
"""

from __future__ import annotations

import importlib.util
import json
import os
import shlex
import socket
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def _render(dest: Path) -> Path:
    """Render the template with its defaults, as scripts/check-adapter-template.py does."""
    spec = importlib.util.spec_from_file_location(
        "check_adapter_template", ROOT / "scripts" / "check-adapter-template.py"
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mod.render(dest)
    (project,) = [p for p in dest.iterdir() if p.is_dir()]
    return project


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_a_generated_adapter_passes_the_harness_including_c3_9(tmp_path: Path) -> None:
    project = _render(tmp_path / "render")
    package = json.loads((ROOT / "adapters/template/cookiecutter.json").read_text())[
        "python_package"
    ]
    report = tmp_path / "report.json"
    path = os.pathsep.join(str(p) for p in (project, ROOT / "sdk/python/src", ROOT / "proto"))
    adapter_cmd = (
        f"PYTHONPATH={shlex.quote(path)} {shlex.quote(sys.executable)} "
        f"-m {package}.adapter --endpoint localhost:{{port}}"
    )
    run = subprocess.run(
        [
            sys.executable,
            "-m",
            "adapters.conformance",
            "--stub-path",
            "proto",
            "--adapter-name",
            "fleet-adapter-template",
            "--port",
            str(_free_port()),
            "--json",
            str(report),
            "--adapter-cmd",
            adapter_cmd,
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=300,
    )
    assert report.exists(), (
        f"the harness wrote no report:\n{run.stdout[-3000:]}\n{run.stderr[-3000:]}"
    )

    results = {r["check_id"]: r for r in json.loads(report.read_text())["results"]}
    c39 = results.get("C3.9")
    assert c39 is not None, "the harness recorded no C3.9 row"
    assert c39["status"] == "pass", (
        "a generated adapter fails C3.9: a cancel naming a preempted action stopped the "
        f"action that preempted it, or the preemptor never completed.\n  {c39.get('detail')}"
    )
    failed = sorted(
        k
        for k, r in results.items()
        if r["status"] == "fail" and r.get("level") in ("MUST", "MUST NOT")
    )
    assert not failed, (
        f"a generated adapter fails {failed}; the template README says it fails no check.\n"
        + "\n".join(f"  {k}: {results[k].get('detail')}" for k in failed)
    )
    assert run.returncode == 0, f"the harness exited {run.returncode}:\n{run.stdout[-2000:]}"
