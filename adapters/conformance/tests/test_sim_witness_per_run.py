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

"""Each simulation run grades C5.7 against its own plant.

The motion witness is a file: the simulation adapter's plant model writes the robot's true
speed to it and the harness reads it back. The three targets that run the simulation adapter
pass one path to both halves of a run. When that path is a fixed name, two runs started
together -- in one tree and a copy of it, or in two sessions -- open the same file, and the
one that starts second truncates the first one's samples. C5.7 then grades one run's STOPPED
against a mix of two plants. Filtering by robot id does not separate them: every run's robot
carries the same id.

So the default must give each RUN its own path, and both halves of one run must still agree
on it. These tests read the recipes as make expands them, and then expand the path the way
the recipe's own shell will.
"""

from __future__ import annotations

import re
import shutil
import subprocess
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parents[3]

#: The targets that run the simulation adapter under the harness. Each passes the witness
#: path twice: once to the harness, which reads it, and once to the adapter, which writes it.
_SIM_TARGETS = ("conformance-sim", "conformance-sim-fault", "conformance-profiles")

_WITNESS_ARG = re.compile(r"--witness-file[= ]'?([^' \\]+)'?")


def _recipe(target: str, *overrides: str) -> str:
    """The commands make would run for a target, with every make variable expanded."""
    done = subprocess.run(["make", "--no-print-directory", "-n", target, *overrides],
                          cwd=_ROOT, capture_output=True, text=True, check=False)
    assert done.returncode == 0, f"make -n {target} failed:\n{done.stdout}\n{done.stderr}"
    return done.stdout


def _expand(fragment: str) -> str:
    """What the recipe's own shell makes of the path, in one shell, as the recipe runs it."""
    done = subprocess.run(["sh", "-c", f'printf "%s" "{fragment}"'],
                          cwd=_ROOT, capture_output=True, text=True, check=True)
    return done.stdout


pytestmark = pytest.mark.skipif(shutil.which("make") is None,
                                reason="make is not on PATH")


@pytest.mark.parametrize("target", _SIM_TARGETS)
def test_both_halves_of_a_run_are_given_the_same_witness_path(target):
    """The harness reads what this run's adapter writes, so the two must be the same file."""
    paths = _WITNESS_ARG.findall(_recipe(target))
    assert len(paths) == 2, (
        f"{target} names --witness-file {len(paths)} time(s); it must name it twice, once "
        f"for the harness that reads the witness and once for the adapter that writes it: "
        f"{paths}")
    assert paths[0] == paths[1], (
        f"{target} gives the harness {paths[0]} and the adapter {paths[1]}; the harness "
        f"would grade C5.7 against a file the adapter never wrote")


@pytest.mark.parametrize("target", _SIM_TARGETS)
def test_the_witness_path_leaves_nothing_for_a_shell_to_expand(target):
    """The two halves are given the path by two different shells, so it must be literal.

    The harness is handed the path inside the recipe's own quotes; the adapter is handed it
    inside --adapter-cmd, which the harness then runs through a shell of its own. Anything
    that resolves per shell -- a pid, a command substitution, an environment variable --
    becomes two different files, and the two halves agree on the TEXT while disagreeing on
    the file. C5.7 then reads a witness its own run never wrote and skips for want of one,
    which is a graded row quietly lost.
    """
    path = _WITNESS_ARG.findall(_recipe(target))[0]
    assert path == _expand(path), (
        f"{target} passes a witness path a shell still changes: {path!r} became "
        f"{_expand(path)!r}. The adapter's copy goes through a second shell, so the two "
        f"halves of the run would open different files")
    assert not set(path) & set("$`*?~"), (
        f"{target}'s witness path carries shell syntax: {path!r}")
    assert path.endswith(".jsonl"), path


@pytest.mark.parametrize("target", _SIM_TARGETS)
def test_two_runs_started_together_get_different_witness_paths(target):
    """Two runs, two witnesses. One path for both is one run grading the other's plant.

    Two runs cannot share a port -- the harness listens on it and the adapter dials it -- so
    naming the witness after the port is what makes it per-run. A second run is started by
    setting the port, and this asserts the witness follows it rather than staying put.
    """
    first = _WITNESS_ARG.findall(_recipe(target))[0]
    second = _WITNESS_ARG.findall(_recipe(target, "CONFORMANCE_PORT=19191"))[0]

    assert first != second, (
        f"{target} resolves to {first} whichever port the run listens on, so two runs "
        f"started together would write and read the same motion witness and C5.7 could be "
        f"graded against another process's plant")
    assert second.endswith(".jsonl"), second


@pytest.mark.parametrize("target", _SIM_TARGETS + ("conformance",))
def test_a_run_listens_on_the_port_its_witness_is_named_after(target):
    """A second run needs one setting, not two that can disagree."""
    recipe = _recipe(target, "CONFORMANCE_PORT=19191")
    assert "--port 19191" in recipe, (
        f"{target} does not pass CONFORMANCE_PORT to the harness, so a second run would "
        f"take a witness path of its own and then collide on the default port:\n{recipe}")


def test_the_witness_default_can_still_be_pinned():
    """An explicit SIM_WITNESS still wins, so a run can be pointed at a path to inspect."""
    paths = _WITNESS_ARG.findall(
        _recipe("conformance-sim", "SIM_WITNESS=/tmp/pinned-witness.jsonl"))
    assert paths == ["/tmp/pinned-witness.jsonl", "/tmp/pinned-witness.jsonl"], paths
