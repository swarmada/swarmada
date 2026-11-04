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
"""examples/warehouse-quickstart/run.sh, read by calling its own functions with kubectl stubbed.

The script is sourced, not run: sourced, it defines its functions and stops. A stub `kubectl`
records every call and every manifest piped to it, so a test sees exactly what a scenario would
apply to a cluster without one. The script's EXIT trap is cleared first; nothing is written
outside the test's temporary directory.

1. The estop drill's action must still be driving when the stop is sent. The drill waits for
   the action to land on the live robot, polling every `wait_until` interval, then pauses
   PAUSE_SECONDS before it triggers the stop. Detection can lag the assignment by a whole
   interval, so the robot can drive for interval + pause before the stop is even sent. On the
   sample's own target (10 m, 10 s at the simulator's 1 m/s) that is 3 + 8 = 11 s: the action
   could reach Succeeded before the estop existed, and the drill then failed "never paused".
   The bound is necessary, not sufficient: the stop still has to travel to the robot, and the
   robot may have started driving before the drill began polling. The long route leaves 21 s
   for those.
2. The comms-flaky payoff printed before the scenario gives the same reason as SCENARIOS.md for
   why the robot view does not move: the outage is shorter than the offline threshold.
"""

from __future__ import annotations

import ast
import base64
import json
import math
import os
import pathlib
import pty
import re
import shutil
import subprocess

import yaml

from adapters.simulation.simulator import KinematicSim

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
_RUN_SH = _REPO_ROOT / "examples/warehouse-quickstart/run.sh"
_SCENARIOS = _REPO_ROOT / "examples/warehouse-quickstart/SCENARIOS.md"
_SIM_ADAPTER = _REPO_ROOT / "adapters/simulation/sim_adapter.py"

# Every call is one line of NUL-separated arguments; every manifest piped to `apply -f -`
# is appended to the manifests file. Nothing else is emulated.
_STUB = r"""
# Sourcing installs run.sh's EXIT trap, which removes the run's pidfile under /tmp. A test
# must not touch a live quickstart's files, so the trap goes first.
trap - EXIT
kubectl() {
  printf '%s\0' "$@" >> "$KCALLS"; printf '\n' >> "$KCALLS"
  if [[ "$1" == apply && " $* " == *" -f - "* ]]; then
    cat >> "$KMANIFESTS"; echo '---' >> "$KMANIFESTS"
  fi
  return 0
}
# The FleetAdapter and its conformance report are not what these tests read, and producing
# the report runs the conformance harness against a live adapter.
apply_fleet_adapter() { :; }
"""


def _bash(tmp_path: pathlib.Path, body: str) -> tuple[str, list[list[str]], str]:
    bash = shutil.which("bash")
    assert bash, "bash is required: run.sh is a bash script"
    calls, manifests = tmp_path / "calls", tmp_path / "manifests"
    calls.write_text("")
    manifests.write_text("")
    script = f"source {_RUN_SH}\n{_STUB}\n{body}\n"
    env = dict(os.environ, KCALLS=str(calls), KMANIFESTS=str(manifests))
    out = subprocess.run(
        [bash, "-c", script], cwd=_REPO_ROOT, env=env, capture_output=True, text=True, timeout=60
    )
    assert out.returncode == 0, out.stderr
    rows = [ln.split("\0")[:-1] for ln in calls.read_text().splitlines() if ln]
    return out.stdout, rows, manifests.read_text()


def _sim_constants() -> tuple[tuple[float, float], tuple[float, float]]:
    """(the nominal target, the spawn point), read from sim_adapter.py without importing it."""
    tree = ast.parse(_SIM_ADAPTER.read_text(encoding="utf-8"))
    nominal = spawn = None
    for node in ast.walk(tree):
        if isinstance(node, ast.Assign) and any(
            isinstance(t, ast.Name) and t.id == "_NOMINAL_TARGET" for t in node.targets
        ):
            nominal = ast.literal_eval(node.value)
        if (
            isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "spawn"
            and len(node.args) == 3
        ):
            spawn = (ast.literal_eval(node.args[1]), ast.literal_eval(node.args[2]))
    assert nominal and spawn, "sim_adapter.py no longer states its nominal target or spawn point"
    return nominal, spawn


def _target(payload_raw: str | None, nominal: tuple[float, float]) -> tuple[float, float]:
    if not payload_raw:
        return nominal
    doc = json.loads(base64.b64decode(payload_raw))
    return (float(doc["x"]), float(doc["y"])) if "x" in doc or "y" in doc else nominal


def test_the_estop_drill_action_is_still_driving_when_the_stop_is_sent(
    tmp_path: pathlib.Path,
) -> None:
    out, calls, manifests = _bash(
        tmp_path,
        "SCENARIO=estop-drill; apply_fleet >/dev/null\n"
        'echo "PAUSE=$PAUSE_SECONDS"; declare -f wait_until',
    )
    pause = int(re.search(r"PAUSE=(\d+)", out).group(1))
    interval = int(re.search(r"\binterval=(\d+)", out).group(1))

    applied = {
        d["metadata"]["name"]: (d.get("spec", {}).get("payload") or {}).get("raw")
        for d in yaml.safe_load_all(manifests)
        if isinstance(d, dict) and d.get("kind") == "FleetAction"
    }
    assert applied, "the drill applied no FleetAction; nothing to stop"
    for row in calls:
        if row[:1] == ["patch"] and row[1].startswith("fleetaction/") and "-p" in row:
            raw = json.loads(row[row.index("-p") + 1]).get("spec", {}).get("payload", {}).get("raw")
            if raw:
                applied[row[1].split("/", 1)[1]] = raw

    nominal, spawn = _sim_constants()
    budget = interval + pause
    for name, raw in sorted(applied.items()):
        x, y = _target(raw, nominal)
        drive = math.hypot(x - spawn[0], y - spawn[1]) / KinematicSim.SPEED
        assert drive > budget, (
            f"estop-drill's {name} drives {drive:.0f}s to ({x:g}, {y:g}), but the stop can be sent "
            f"{budget}s after it lands (poll {interval}s + pause {pause}s): the action can reach "
            f"Succeeded before the estop exists"
        )


def test_the_comms_flaky_payoff_gives_the_reason_scenarios_md_gives(
    tmp_path: pathlib.Path,
) -> None:
    out, _, _ = _bash(tmp_path, "scenario_payoff_description comms-flaky")
    payoff = " ".join(out.split())
    section = _SCENARIOS.read_text(encoding="utf-8").split("## 5. `comms-flaky`", 1)[1]
    section = " ".join(section.split("\n## ", 1)[0].split())

    for claim in ("offline threshold", "FleetAdapter.status.phase", "lastSeenAt"):
        assert claim in section, f"SCENARIOS.md §5 no longer says {claim!r}; re-read both texts"
        assert claim in payoff, f"run.sh's comms-flaky payoff does not say {claim!r}"
    for dropped in ("RA-1", "status write path", "nothing moving"):
        assert dropped not in payoff, (
            f"run.sh's comms-flaky payoff still says {dropped!r}, which SCENARIOS.md dropped"
        )


def _drill_drive_seconds(tmp_path: pathlib.Path) -> tuple[float, int]:
    """(the shortest drive among the drill's FleetActions, wait_until's poll interval)."""
    out, calls, manifests = _bash(
        tmp_path, "SCENARIO=estop-drill; apply_fleet >/dev/null\ndeclare -f wait_until"
    )
    interval = int(re.search(r"\binterval=(\d+)", out).group(1))
    applied = {
        d["metadata"]["name"]: (d.get("spec", {}).get("payload") or {}).get("raw")
        for d in yaml.safe_load_all(manifests)
        if isinstance(d, dict) and d.get("kind") == "FleetAction"
    }
    for row in calls:
        if row[:1] == ["patch"] and row[1].startswith("fleetaction/") and "-p" in row:
            raw = json.loads(row[row.index("-p") + 1]).get("spec", {}).get("payload", {}).get("raw")
            if raw:
                applied[row[1].split("/", 1)[1]] = raw
    nominal, spawn = _sim_constants()
    drives = [
        math.hypot(x - spawn[0], y - spawn[1]) / KinematicSim.SPEED
        for x, y in (_target(raw, nominal) for raw in applied.values())
    ]
    assert drives, "the drill applied no FleetAction; nothing to stop"
    return min(drives), interval


# handle_estop_drill with the cluster stubbed out: every wait succeeds at once, and `sleep` and
# `read` report how long the drill's pause could last instead of waiting.
_DRILL = r"""
wait_until() { return 0; }
live_robot_action() { echo drill-action; }
step() { :; }; info() { :; }
sleep() { echo "WAIT sleep $1"; }
read() { echo "WAIT read $*"; return 142; }
"""


def _drill_pause(tmp_path: pathlib.Path, pace: str, pause_seconds: int, tty: bool) -> str:
    """The longest the drill's pause can last, in seconds, or "unbounded".

    With `tty`, stdin is a pseudo-terminal, which is what makes --pace keypress wait for Enter.
    """
    bash = shutil.which("bash")
    assert bash, "bash is required: run.sh is a bash script"
    calls = tmp_path / "calls"
    calls.write_text("")
    script = (
        f"source {_RUN_SH}\n{_STUB}\n{_DRILL}\n"
        f"PACE={pace}; PAUSE_SECONDS={pause_seconds}; CI=; handle_estop_drill\n"
    )
    env = dict(os.environ, KCALLS=str(calls), KMANIFESTS=str(tmp_path / "manifests"))
    main, secondary = pty.openpty() if tty else (None, None)
    try:
        out = subprocess.run(
            [bash, "-c", script], cwd=_REPO_ROOT, env=env, capture_output=True, text=True,
            timeout=60, stdin=secondary if tty else subprocess.DEVNULL,
        )
    finally:
        for fd in (main, secondary):
            if fd is not None:
                os.close(fd)
    assert out.returncode == 0, out.stderr
    waits = re.findall(r"^WAIT (sleep|read) ?(.*)$", out.stdout, flags=re.M)
    assert len(waits) == 1, f"expected the drill to pause once, saw {waits}:\n{out.stdout}"
    kind, args = waits[0]
    if kind == "sleep":
        return args
    bound = re.search(r"(?:^|\s)-[a-z]*t\s+(\d+)", args)
    return bound.group(1) if bound else "unbounded"


def test_the_estop_drill_pause_ends_while_the_robot_is_still_driving(
    tmp_path: pathlib.Path,
) -> None:
    # The drill pauses after its action lands, and the robot drives on while it waits. A
    # presenter who waits at the keypress prompt, or a long --pause-seconds, let the action
    # reach Succeeded before the stop was sent, and the drill failed "never paused".
    drive, interval = _drill_drive_seconds(tmp_path)
    budget = drive - interval
    for pace, pause_seconds, tty in (
        ("keypress", 8, True),
        ("keypress", 60, False),
        ("timed", 60, False),
    ):
        got = _drill_pause(tmp_path, pace, pause_seconds, tty)
        assert got != "unbounded", (
            f"--pace {pace}: the estop drill waits for Enter with no bound while "
            f"the robot drives a {drive:.0f}s route"
        )
        assert float(got) < budget, (
            f"--pace {pace} --pause-seconds {pause_seconds}: the estop drill can wait {got}s after "
            f"its action lands, but the action reaches Succeeded {budget:.0f}s after that "
            f"(a {drive:.0f}s drive less the {interval}s poll)"
        )


def test_the_battery_edge_text_says_what_the_run_shows(tmp_path: pathlib.Path) -> None:
    # SCENARIOS.md §2: the draining robot is the only candidate, so this run stages no
    # comparison between robots and the robot keeps its task. The printed text said the
    # scheduler would steer assignments away from it.
    section = _SCENARIOS.read_text(encoding="utf-8").split("## 2. `battery-edge`", 1)[1]
    section = " ".join(section.split("\n## ", 1)[0].split())
    for claim in ("the only candidate", "`battery-handoff`", "keeps its task"):
        assert claim in section, f"SCENARIOS.md §2 no longer says {claim!r}; re-read both texts"
    # §2 opens with what the run shows, and the table's picker text is the one run.sh prints.
    for text, where in ((section.split(" - ", 1)[0], "§2's opening"),
                        (_scenarios_picker_texts()["2"], "picker text in the table")):
        for claim in ("avoid", "keeps a low-battery robot from winning", "dying robot"):
            assert claim not in text, (
                f"SCENARIOS.md's battery-edge {where} says {claim!r}; the run gives the draining "
                f"robot the task because it is the only candidate"
            )

    out, _, _ = _bash(tmp_path, "scenario_payoff_description battery-edge")
    payoff = " ".join(out.split())
    picker = next(
        ln for ln in _RUN_SH.read_text(encoding="utf-8").splitlines() if "2) battery-edge" in ln
    )
    for text, where in ((payoff, "payoff"), (picker, "picker line")):
        for claim in ("steer", "away from it", "avoid"):
            assert claim not in text, (
                f"run.sh's battery-edge {where} still says {claim!r}; SCENARIOS.md §2 says the "
                f"run does not stage that"
            )
    for claim in ("only candidate", "keeps the task", "battery-handoff"):
        assert claim in payoff, f"run.sh's battery-edge payoff does not say {claim!r}"


def _scenarios_picker_texts() -> dict[str, str]:
    """SCENARIOS.md's "At a glance" table: row number -> its "Picker text" cell."""
    out = {}
    for line in _SCENARIOS.read_text(encoding="utf-8").splitlines():
        cells = [c.strip() for c in line.split("|")]
        if len(cells) > 3 and cells[1].isdigit():
            out[cells[1]] = cells[3]
    return out


def _run_sh_picker_texts() -> dict[str, str]:
    """run.sh's interactive picker: choice number -> the text it prints for that choice."""
    text = _RUN_SH.read_text(encoding="utf-8")
    body = text.split("Pick a scenario", 1)[1].split("read -rp", 1)[0]
    out: dict[str, str] = {}
    last = None
    for line in re.findall(r'^\s*echo "(.*)"\s*$', body, flags=re.M):
        m = re.match(r"\s*(\d)\) \S+(?: \S+)?\s+— (.*)", line)
        if m:
            last = m.group(1)
            out[last] = m.group(2).strip()
        elif last is not None:
            out[last] += " " + line.strip()
    return out


def test_scenarios_md_picker_text_is_what_run_sh_prints() -> None:
    # A reader picks from run.sh's list; the table must not promise a different scenario.
    table, printed = _scenarios_picker_texts(), _run_sh_picker_texts()
    assert set(table) == set(printed), (
        f"SCENARIOS.md lists rows {sorted(table)}, run.sh's picker offers {sorted(printed)}")
    for n, text in sorted(table.items()):
        assert text in printed[n], (
            f"SCENARIOS.md row {n}'s picker text {text!r} is not what run.sh prints: "
            f"{printed[n]!r}")


def test_every_battery_edge_text_says_the_robot_keeps_its_task() -> None:
    # The sibling of the test above, over the rest of the scenario's texts: the whole of
    # SCENARIOS.md §2 (its bullets included), the scenarios README's preset row, and the
    # preset's own header and description. Each once said the run shows the scheduler
    # picking around the low robot; the run gives it the task as the only candidate.
    section = _SCENARIOS.read_text(encoding="utf-8").split("## 2. `battery-edge`", 1)[1]
    section = " ".join(section.split("\n## ", 1)[0].split())
    readme = _REPO_ROOT / "adapters/scenarios/README.md"
    row = next(ln for ln in readme.read_text(encoding="utf-8").splitlines()
               if ln.startswith("| `battery-edge`"))
    preset_path = _REPO_ROOT / "adapters/scenarios/presets/battery-edge.yaml"
    preset_text = preset_path.read_text(encoding="utf-8")
    header = " ".join(ln.lstrip("#").strip() for ln in preset_text.splitlines()
                      if ln.startswith("#"))
    description = " ".join(yaml.safe_load(preset_text)["description"].split())
    texts = (("SCENARIOS.md §2", section), ("the scenarios README row", row),
             ("the preset header", " ".join(header.split())),
             ("the preset description", description))
    for where, text in texts:
        for claim in ("tie-break the scenario shows", "the scheduler avoids it",
                      "never getting picked", "never selected", "loses every new-task selection",
                      "shows the scheduler's"):
            assert claim not in text.lower(), (
                f"{where} says {claim!r}; the run gives the draining robot the task because it "
                f"is the only candidate, and does not exercise the tie-break")
    for where, text in texts[1:]:
        assert "only candidate" in text and "keeps its task" in text, (
            f"{where} does not say the robot keeps its task as the only candidate: {text[:200]!r}")
