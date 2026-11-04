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
"""The hardware-conformance desk tools: the Nav2 overlay and the fixture-pose picker.

THREE PROPERTIES CARRY THIS FILE.

1. **The overlay changes the footprint and the speed, and NOTHING else.** The whole
   argument for running an autonomous goal tomorrow is that exactly one class of change
   was made to a vendor stack nobody here wrote. `test_the_diff_is_only_the_footprint_and_
   the_speed` reads the diff the tool itself emits and fails on any other touched line,
   so a future edit that "also fixes" the turning radius fails here rather than at the
   robot.

2. **A pinned line that moved is a refusal, not a guess.** Every edit is anchored to the
   exact bytes it was reasoned about. A vendor file that drifted is a file the tool has not
   read, and editing it by pattern is how a footprint correction becomes a footprint that is
   quietly not in force.

3. **The pose picker refuses a floor that does not qualify.** It is cheaper to be told the
   run cannot happen than to discover it with a robot in a doorway, so the refusal cases get
   as much weight as the success case -- a clearance no pair meets, a separation no pair
   reaches, and a map whose free threshold cannot separate unknown from free.

4. **A ruled value is carried only when it is asked for, and it is carried as a FLOAT.** The
   turning radius went from "never changed" to "changed by a ruling, for one run". Property 1
   still holds at the default -- the line is not touched and the diff does not mention it --
   and a second set of assertions covers the ruled invocation: the radius moves, nothing else
   moves with it, and the value written is `0.0` and not `0`, because ROS 2 reads an unquoted
   `0` as an integer and refuses it as a double parameter, which on the day reads as "Nav2
   did not come up".

5. **An overlay that is valid YAML is not yet an overlay that launches.** A parameter file
   handed to a COMPOSABLE node is read by the launch system itself and sent to the container
   over its load-node service, and ROS 2 has no parameter type for an array of arrays. A
   polygon written as a nested YAML sequence parses, reads correctly, and then aborts the
   entire launch -- every node in the container, before any of them does any work -- with a
   message that names a float and no file, no key and no line. That is one powered cycle,
   spent. `test_no_array_in_either_overlay_is_an_array_of_arrays` is that reader's rule,
   applied to the bytes the tool writes.

6. **The boot gate measures drift, and says so when it measured nothing.** The gate is a
   least-squares slope over a full stationary minute, because a difference between two
   samples is one noisy reading against another. The refusals carry as much weight as the
   pass: a series that is too short, a series whose translation moved, and a series whose
   stamps are all identical -- which is what a STATIC transform prints, and which would
   otherwise report a tidy 0.000 deg/min off a reading that measured nothing at all.

The maps are synthetic and built in metres at the real capture's scale (0.05 m/px), because
a fixture at some other scale tests the arithmetic rather than its meaning.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
# The private tree's directory name is ASSEMBLED from segments rather than written out, for the
# reason tests/python/test_citation_gate_scope.py gives at its own _BOUNDARY: rule C04 of the
# boundary declaration forbids a public file from naming the private tree as content, and this
# file is public. Spelling it here would make the test that exercises the private tool the thing
# that breaks the boundary the tool sits behind.
TOOLS = REPO_ROOT.joinpath("-".join(("internal", "planning")), "tools", "hwc")
OVERLAY = TOOLS / "nav2_overlay.py"
POSES = TOOLS / "fixture_poses.py"
BOOT_GATE = TOOLS / "boot_gate.py"

# The two vendor parameter files, as BYTES BESIDE THE TOOL, not as a reach into a capture
# directory. A test that reads a dated capture is a test that a tidy-up breaks, and it is the
# capture's job to record what a session saw, not to be a fixture. The copies here are pinned
# by the same md5s the tool refuses on (see testdata/README.md), so a copy that drifted from
# the capture makes the tool refuse rather than making the test quietly test something else.
VENDOR = TOOLS / "testdata"

pytestmark = pytest.mark.skipif(
    not (VENDOR / "vendor_nav2_params.yaml").exists(),
    reason="the vendor parameter fixtures are not in this tree")


def run(script: Path, *args, expect=0):
    p = subprocess.run([sys.executable, str(script), *[str(a) for a in args]],
                       capture_output=True, text=True)
    assert p.returncode == expect, (
        f"exit {p.returncode}\n--- stdout\n{p.stdout}\n--- stderr\n{p.stderr}")
    return p


# ── the overlay ──────────────────────────────────────────────────────────────────────────

def _diff(*extra) -> str:
    return run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180,
               "--diff", *extra).stdout


def test_the_diff_is_only_the_footprint_and_the_speed():
    changed = [ln for ln in _diff().splitlines()
               if ln[:1] in "+-" and not ln.startswith(("+++", "---"))]
    removed = sorted(ln[1:].strip() for ln in changed if ln.startswith("-"))
    added = sorted(ln[1:].strip() for ln in changed if ln.startswith("+"))
    assert removed == sorted([
        "robot_radius: 0.05",                 # local_costmap
        "robot_radius: 0.05",                 # global_costmap
        "max_velocity: [0.35, 0.0, 1.0]",
        "min_velocity: [-0.35, 0.0, -1.0]",
        "max_vel_x: 0.4",
        'type: "circular"',
        'radius: 0.01 # for type "circular"',
    ]), "a line outside the footprint and the speed cap was removed"
    assert all(a.startswith(("footprint:", "footprint_padding:", "max_velocity:", "min_velocity:",
                             "max_vel_x:", 'type: "polygon"', "vertices:")) for a in added), \
        f"an unexpected line was added: {added}"


def _changed(body: str) -> list:
    """Only the +/- lines. A diff prints context too, and a context line is a line the
    overlay left alone -- which is exactly what these tests are asserting about."""
    return [ln for ln in body.splitlines()
            if ln[:1] in "+-" and not ln.startswith(("+++", "---"))]


def test_the_turning_radius_and_the_wheelbase_are_not_touched():
    changed = "\n".join(_changed(_diff()))
    for untouched in ("min_turning_radius", "wheelbase", "inflation_radius", "acc_lim_x",
                      "max_decel", "max_accel", "xy_goal_tolerance", "movement_time_allowance"):
        assert untouched not in changed, (
            f"{untouched} is added or removed by the overlay. Tomorrow tests the vendor plant "
            f"with the safety fix, not a re-tuned plant; list it as a finding instead.")


def test_the_polygon_is_the_measured_envelope_about_base_footprint():
    body = _diff()
    assert "[[0.12, 0.09], [-0.12, 0.09], [-0.12, -0.09], [0.12, -0.09]]" in body
    assert "footprint_padding: 0.05" in body


def test_both_costmaps_get_the_footprint_not_just_one():
    body = _diff()
    assert body.count("+      footprint: ") == 2 and body.count("+      footprint_padding: ") == 2


def test_the_speed_cap_reaches_the_smoother_and_the_controller():
    body = _diff()
    assert "+    max_velocity: [0.2, 0.0, 1.0]" in body      # velocity_smoother
    assert "+      max_vel_x: 0.2" in body                    # TEB


def test_the_reverse_cap_can_be_left_alone_and_the_diff_then_says_so():
    changed = _changed(_diff("--no-cap-reverse"))
    assert not any("min_velocity" in ln for ln in changed)
    assert "+    max_velocity: [0.2, 0.0, 1.0]" in changed


def test_a_vendor_file_that_moved_is_refused_rather_than_edited_by_pattern(tmp_path):
    for name in ("vendor_nav2_params.yaml", "vendor_nav2_controller_teb.yaml"):
        (tmp_path / name).write_text((VENDOR / name).read_text())
    p = tmp_path / "vendor_nav2_params.yaml"
    p.write_text("# a line the capture did not have\n" + p.read_text())
    out = run(OVERLAY, "--vendor-dir", tmp_path, "--length-mm", 240, "--width-mm", 180,
              "--diff", expect=1)
    assert "REFUSED" in out.stderr and "differs from the captured bytes" in out.stderr


def test_the_findings_name_the_turning_radius_and_say_it_is_not_changed():
    out = run(OVERLAY, "--findings").stdout
    assert "min_turning_radius: 1.0" in out and "NOT CHANGED" in out
    assert "wheelbase: 0.145" in out


def test_verify_live_reads_back_every_parameter_the_overlay_changed_and_aborts():
    out = run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180,
              "--verify-live").stdout
    for node_param in ("/local_costmap/local_costmap footprint",
                       "/global_costmap/global_costmap footprint",
                       "/local_costmap/local_costmap footprint_padding",
                       "/controller_server FollowPath.footprint_model.type",
                       "/controller_server FollowPath.max_vel_x",
                       "/velocity_smoother max_velocity",
                       "/velocity_smoother min_velocity"):
        assert node_param in out, f"verify-live does not read back {node_param}"
    assert "published_footprint" in out, "the footprint actually in force is never read"
    assert "ABORT" in out


def test_the_written_overlay_is_valid_yaml_with_the_footprint_in_force(tmp_path):
    yaml = pytest.importorskip("yaml")
    run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path,
        "--length-mm", 240, "--width-mm", 180)
    doc = yaml.safe_load((tmp_path / "overlay_nav2_params.yaml").read_text())
    for cm in ("local_costmap", "global_costmap"):
        params = doc[cm][cm]["ros__parameters"]
        assert "robot_radius" not in params, "the radius must be GONE, not merely shadowed"
        assert params["footprint_padding"] == 0.05
        assert yaml.safe_load(params["footprint"]) == [[0.12, 0.09], [-0.12, 0.09],
                                                       [-0.12, -0.09], [0.12, -0.09]]
    assert doc["velocity_smoother"]["ros__parameters"]["max_velocity"] == [0.2, 0.0, 1.0]
    teb = yaml.safe_load((tmp_path / "overlay_nav2_controller_teb.yaml").read_text())
    fp = teb["controller_server"]["ros__parameters"]["FollowPath"]
    assert fp["footprint_model"]["type"] == "polygon"
    assert "radius" not in fp["footprint_model"]
    assert fp["max_vel_x"] == 0.2
    assert fp["min_turning_radius"] == 1.0, "the vendor's tuning must survive the overlay"


# ── the overlay has to survive `ros2 launch`, not merely parse ─────────────────────
#
# Both overlay files are handed to COMPOSABLE nodes, whose parameter files the launch system
# reads ITSELF and sends to the container over its load-node service. That reader accepts an
# array only when every element is a scalar, because ROS 2 has no parameter type for an array
# of arrays; anything else raises before a single node has done any work, and takes every
# other node in the container down with it. The two assertions below are that rule and the
# spelling that satisfies it.

_CONTAINERS = (list, tuple, dict)


def _arrays_of_arrays(doc, path=()):
    """Dotted paths of every sequence value that itself contains a sequence or a mapping."""
    found = []
    if isinstance(doc, dict):
        for key, value in doc.items():
            found += _arrays_of_arrays(value, path + (str(key),))
    elif isinstance(doc, (list, tuple)):
        if any(isinstance(element, _CONTAINERS) for element in doc):
            found.append(".".join(path))
    return found


def _built(tmp_path, *extra):
    run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path,
        "--length-mm", 240, "--width-mm", 180, *extra)
    return tmp_path


def test_no_array_in_either_overlay_is_an_array_of_arrays(tmp_path):
    """The rule the launch system applies to a composable node's parameter file.

    A nested sequence here is not a style question. It aborts the launch with
    `Failed to normalize given item of type '<class 'float'>'` -- a message that names
    neither the file, nor the key, nor the line -- and it aborts it for every node in the
    container, so the symptom is "Nav2 did not come up" and the cause is one unquoted value.
    """
    yaml = pytest.importorskip("yaml")
    out = _built(tmp_path, "--min-turning-radius-m", "0.0")
    for name in ("overlay_nav2_params.yaml", "overlay_nav2_controller_teb.yaml"):
        doc = yaml.safe_load((out / name).read_text())
        assert _arrays_of_arrays(doc) == [], (
            f"{name} carries an array of arrays, which no ROS 2 parameter can hold and which "
            f"aborts the whole launch: {_arrays_of_arrays(doc)}")


def test_the_polygon_reaches_both_files_as_a_string_and_the_same_string(tmp_path):
    """The costmap footprint was always quoted; the controller's polygon must be too.

    They are the same measured envelope, so a test that only checked each one separately
    would pass on two different robots.
    """
    yaml = pytest.importorskip("yaml")
    out = _built(tmp_path)
    params = yaml.safe_load((out / "overlay_nav2_params.yaml").read_text())
    teb = yaml.safe_load((out / "overlay_nav2_controller_teb.yaml").read_text())
    vertices = teb["controller_server"]["ros__parameters"]["FollowPath"]["footprint_model"][
        "vertices"]
    assert isinstance(vertices, str), (
        "footprint_model.vertices must be a STRING; unquoted it is a nested sequence and the "
        "launch aborts before any node starts")
    assert yaml.safe_load(vertices) == [[0.12, 0.09], [-0.12, 0.09],
                                        [-0.12, -0.09], [0.12, -0.09]]
    for cm in ("local_costmap", "global_costmap"):
        footprint = params[cm][cm]["ros__parameters"]["footprint"]
        assert isinstance(footprint, str)
        assert footprint == vertices, (
            "the controller and the costmaps must describe ONE robot, character for character")


# ── the overlay, with --min-turning-radius-m 0.0 ─────────────────────────────────────────
#
# min_turning_radius 1.0 -> 0.0. The base is holonomic Mecanum and the car-like constraint
# never described it. These assert the requested value is carried EXACTLY, and that asking
# for the vendor's own value is not a change.

RULED = ("--min-turning-radius-m", "0.0")


def test_the_default_still_does_not_touch_the_turning_radius():
    """The ruling is a value somebody passes, not a new default. A tool that re-tunes a
    vendor plant when nobody asked is the failure this whole file exists to prevent."""
    assert "min_turning_radius" not in "\n".join(_changed(_diff()))


def test_the_ruled_turning_radius_is_carried_and_nothing_else_moves_with_it():
    changed = _changed(_diff(*RULED))
    removed = sorted(ln[1:].strip() for ln in changed if ln.startswith("-"))
    added = sorted(ln[1:].strip() for ln in changed if ln.startswith("+"))
    assert removed == sorted([
        "robot_radius: 0.05",                 # local_costmap
        "robot_radius: 0.05",                 # global_costmap
        "max_velocity: [0.35, 0.0, 1.0]",
        "min_velocity: [-0.35, 0.0, -1.0]",
        "max_vel_x: 0.4",
        "min_turning_radius: 1.0",
        'type: "circular"',
        'radius: 0.01 # for type "circular"',
    ]), "the ruling moved a line it was not about"
    assert "min_turning_radius: 0.0" in added
    assert all(a.startswith(("footprint:", "footprint_padding:", "max_velocity:", "min_velocity:",
                             "max_vel_x:", "min_turning_radius:", 'type: "polygon"', "vertices:"))
               for a in added), f"an unexpected line was added: {added}"


def test_the_wheelbase_survives_the_ruling():
    """Only the radius was ruled on. `wheelbase` is wrong by 7.6% and stays wrong: correcting
    it is a second change to a vendor plant in a first autonomous run."""
    changed = "\n".join(_changed(_diff(*RULED)))
    for untouched in ("wheelbase", "inflation_radius", "acc_lim_x", "max_decel", "max_accel",
                      "xy_goal_tolerance", "movement_time_allowance", "max_vel_theta"):
        assert untouched not in changed, f"{untouched} moved, and only the radius was ruled on"


def test_the_ruled_radius_is_written_as_a_float_not_an_integer(tmp_path):
    """`0` and `0.0` are the same number and different YAML. rclcpp reads the first as an
    integer and refuses it where a double is declared, so the node never configures."""
    run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path,
        "--length-mm", 240, "--width-mm", 180, *RULED)
    body = (tmp_path / "overlay_nav2_controller_teb.yaml").read_text()
    assert "min_turning_radius: 0.0" in body
    assert "min_turning_radius: 0\n" not in body


def test_the_written_overlay_carries_the_ruled_radius_and_the_polygon(tmp_path):
    yaml = pytest.importorskip("yaml")
    run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path,
        "--length-mm", 240, "--width-mm", 180, *RULED)
    teb = yaml.safe_load((tmp_path / "overlay_nav2_controller_teb.yaml").read_text())
    fp = teb["controller_server"]["ros__parameters"]["FollowPath"]
    assert fp["min_turning_radius"] == 0.0
    assert isinstance(fp["min_turning_radius"], float)
    assert fp["footprint_model"]["type"] == "polygon"
    assert fp["max_vel_x"] == 0.2
    assert fp["wheelbase"] == 0.145, "the vendor's wheelbase must survive the ruling"


def test_verify_live_reads_back_the_ruled_radius_and_the_polygon_vertices():
    """The overlay now replaces the vendor controller file on the robot, so verify-live is
    the only thing standing between a swap that silently did not happen and a goal."""
    out = run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180,
              "--verify-live", *RULED).stdout
    assert "/controller_server FollowPath.min_turning_radius" in out
    assert "/controller_server FollowPath.footprint_model.vertices" in out
    assert "[[0.12, 0.09], [-0.12, 0.09], [-0.12, -0.09], [0.12, -0.09]]" in out
    assert "0.0" in out.split("min_turning_radius")[-1][:200]
    assert "ros2 param list /controller_server" in out, (
        "verify-live must print the parameter NAMES this build exposes: a name this tool "
        "guessed wrong and a footprint that did not take read identically otherwise")


def test_verify_live_at_the_default_expects_the_vendors_own_radius():
    out = run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180,
              "--verify-live").stdout
    tail = out.split("FollowPath.min_turning_radius")[-1][:200]
    assert "1.0" in tail and "0.0" not in tail


def test_a_negative_turning_radius_is_refused():
    run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180, "--diff",
        "--min-turning-radius-m", "-1.0", expect=2)


def test_the_findings_record_the_ruling_and_what_it_makes_inert():
    out = run(OVERLAY, "--findings").stdout
    assert "RULED 2026-09-20" in out
    assert "weight_kinematics_turning_radius" in out, (
        "a weight that only the car-like edge reads is inert at radius 0, and a value left "
        "in a file reads as a value in force")
    assert "wheelbase: 0.145" in out


# ── the goal checker (2026-09-21) ────────────────────────────────────────────────────────
#
# Five goals SUCCEEDED on the previous site's floor that day and every one of them stopped
# 0.184-0.200 m from the point it was sent to -- all five within 0.017 m of `xy_goal_tolerance`.
# That is what a stateful SimpleGoalChecker does: it declares arrival at the first controller tick
# inside the circle, so the tolerance is not a bound on the error, it IS the error.
#
# The tolerance lives in the TEB CONTROLLER file, which is the file the run SWAPS IN PLACE.
# These assertions carry three things: that the tool reads the tolerance from the right file,
# that the derived numbers are arithmetic on the footprint / the cap / the grid rather than a
# preference, and that property 4 still holds -- at the vendor's own value the line is NOT
# touched, so the overlay driven on 2026-09-21 is byte-identical after this change.

GOAL_TOL = ("--xy-goal-tolerance-m", "0.10")


def test_the_goal_tolerance_lives_in_the_controller_file_and_not_in_nav2_params():
    """Read, not assumed. `nav2_params.yaml` names `nav2_goal_checker_selector_bt_node` in its
    bt_navigator plugin list, which is a behaviour-tree node and not a checker: a session that
    overlays that file expecting the tolerance to move has changed nothing."""
    teb = (VENDOR / "vendor_nav2_controller_teb.yaml").read_text()
    params = (VENDOR / "vendor_nav2_params.yaml").read_text()
    assert "xy_goal_tolerance: 0.2" in teb and "yaw_goal_tolerance: 0.5" in teb
    assert "xy_goal_tolerance" not in params and "yaw_goal_tolerance" not in params
    assert "nav2_goal_checker_selector_bt_node" in params


def test_the_default_leaves_both_goal_tolerance_lines_untouched():
    """The overlay that drove on 2026-09-21 must survive this change byte for byte. A tool
    that re-tunes a vendor plant when nobody asked is the failure this whole file prevents."""
    changed = "\n".join(_changed(_diff(*RULED)))
    assert "goal_tolerance" not in changed


def test_the_default_overlay_is_byte_identical_to_the_one_that_drove(tmp_path):
    """The md5 the run sheet gates on. `53ee2555` is the controller overlay every cycle of
    2026-09-21 ran, and adding a flag must not move it."""
    import hashlib
    run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path,
        "--length-mm", 240, "--width-mm", 180, *RULED)
    got = hashlib.md5(
        (tmp_path / "overlay_nav2_controller_teb.yaml").read_bytes()).hexdigest()
    assert got == "53ee2555168df3eb9265f02a148c19c8", (
        "the default controller overlay moved; the swap gate in the run sheet expects "
        "53ee2555 and would abort the run")


def test_the_asked_for_xy_tolerance_moves_only_that_line():
    changed = _changed(_diff(*RULED, *GOAL_TOL))
    removed = sorted(ln[1:].strip() for ln in changed if ln.startswith("-"))
    added = sorted(ln[1:].strip() for ln in changed if ln.startswith("+"))
    assert "xy_goal_tolerance: 0.2" in removed
    assert "xy_goal_tolerance: 0.1" in added
    assert not any("yaw_goal_tolerance" in ln for ln in changed), (
        "the yaw tolerance moved and only the xy tolerance was asked for")
    assert not any("progress_checker" in ln or "movement_time_allowance" in ln
                   for ln in changed), "the progress checker is the plant under test"


def test_the_written_overlay_carries_the_tolerance_as_a_float(tmp_path):
    """`0.1` and `.1` and `1e-1` are the same number; rclcpp reads the parameter as a double
    and a tolerance that fails to parse is a node that never configures."""
    yaml = pytest.importorskip("yaml")
    run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path,
        "--length-mm", 240, "--width-mm", 180, *RULED, *GOAL_TOL)
    teb = yaml.safe_load((tmp_path / "overlay_nav2_controller_teb.yaml").read_text())
    checker = teb["controller_server"]["ros__parameters"]["goal_checker"]
    assert checker["xy_goal_tolerance"] == 0.1
    assert isinstance(checker["xy_goal_tolerance"], float)
    assert checker["yaw_goal_tolerance"] == 0.5, "only the xy tolerance was asked for"
    assert checker["plugin"] == "nav2_controller::SimpleGoalChecker"
    assert checker["stateful"] is True


def test_the_derivation_is_arithmetic_on_the_footprint_the_grid_and_the_cap():
    """Every number in the derivation has to come from the footprint, the costmap grid or the
    cap. If one of them is a preference, this test is where it shows."""
    out = run(OVERLAY, "--length-mm", 240, "--width-mm", 180, "--goal-tolerances").stdout
    assert "half of 240 mm = 0.12 m" in out                 # ceiling: the footprint
    assert "0.05 m -> 2 whole cells = 0.1 m" in out         # step: the costmap cell
    assert "0.2/10 Hz = 0.0200 m per tick" in out           # floor: the cap
    assert "0.2^2/(2*2.5) = 0.0080 m stopping" in out
    assert "= 0.028 m" in out
    assert "xy_goal_tolerance 0.2 -> 0.1 m" in out


def test_the_derivation_quotes_the_five_arrivals_it_was_derived_from():
    out = run(OVERLAY, "--length-mm", 240, "--width-mm", 180, "--goal-tolerances").stdout
    for label, residual in (("c1 shakedown S1", "0.1836"), ("c1 shakedown S2", "0.1852"),
                            ("c4 return to mark", "0.2004"), ("c5 mark -> B", "0.1873"),
                            ("c5 B -> mark", "0.1946")):
        assert label in out and residual in out, f"{label} is not quoted with its residual"


def test_a_narrower_footprint_derives_a_narrower_tolerance():
    """The ceiling is half the MEASURED length, so a different robot derives a different
    number. A constant that does not move with the measurement is not a derivation."""
    out = run(OVERLAY, "--length-mm", 320, "--width-mm", 180, "--goal-tolerances").stdout
    assert "xy_goal_tolerance 0.2 -> 0.15 m" in out, (
        "half of 320 mm is 0.16 m, which is 3 whole costmap cells")


def test_the_derivation_says_what_the_tighter_tolerance_does_not_buy():
    """The honest half. AMCL and the tape disagreed by more than the whole new tolerance, so
    a reader must not take this as "it will now land on the mark"."""
    out = run(OVERLAY, "--length-mm", 240, "--width-mm", 180, "--goal-tolerances").stdout
    assert "0.14 m at the mark" in out
    assert "DECLARED arrival" in out
    assert "UNPROVEN ON HARDWARE" in out


def test_the_yaw_tolerance_is_reported_and_deliberately_not_derived_down():
    """The footprint gives 0.333 rad and the tool refuses to adopt it, because that is only
    1.75x an AMCL heading disagreement nobody has measured. A derivation that returns "hold"
    is a result; a derivation that quietly tightens into an unmeasured bias is a bug."""
    out = run(OVERLAY, "--length-mm", 240, "--width-mm", 180, "--goal-tolerances").stdout
    assert "HELD AT 0.5 rad" in out and "NOT DERIVED DOWN" in out
    assert "0.3333 rad" in out, "the footprint bound must still be shown, just not adopted"
    assert "11 deg" in out


def test_moving_the_yaw_tolerance_warns_that_the_derivation_says_hold(tmp_path):
    p = run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path, "--length-mm", 240,
            "--width-mm", 180, "--yaw-goal-tolerance-rad", "0.3")
    assert "WARNING" in p.stderr and "HOLD" in p.stderr
    assert "yaw_goal_tolerance: 0.3" in (
        tmp_path / "overlay_nav2_controller_teb.yaml").read_text(), (
        "the warning must not swallow the value: a ruled number still reaches the robot")


def test_a_tolerance_below_one_controller_tick_is_refused():
    """0.02 m is one tick at the cap; anything under the floor asks the controller to stop
    BETWEEN ticks, which it cannot, and the goal ends at the vendor progress checker instead
    -- which reads as "Nav2 aborted" and not as a bad number."""
    p = run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180, "--diff",
            "--xy-goal-tolerance-m", "0.01", expect=2)
    assert "below the floor" in p.stderr


def test_a_tolerance_past_the_footprint_warns_that_the_goal_is_outside_the_robot(tmp_path):
    p = run(OVERLAY, "--vendor-dir", VENDOR, "--out-dir", tmp_path, "--length-mm", 240,
            "--width-mm", 180, "--xy-goal-tolerance-m", "0.3")
    assert "exceeds half the measured length" in p.stderr


def test_verify_live_reads_the_goal_tolerances_back_off_the_running_stack():
    """A tolerance written to a file that the node never configured from is a robot that still
    stops 0.2 m short, and nothing about the run looks wrong."""
    out = run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180,
              *RULED, *GOAL_TOL, "--verify-live").stdout
    assert "goal_checker.xy_goal_tolerance^0.1" in out
    assert "goal_checker.yaw_goal_tolerance^0.5" in out, (
        "the yaw must be read back even though it did not move: the vendor value in force "
        "is a fact worth recording")


def test_verify_live_reads_the_tolerance_back_even_at_the_vendors_own_value():
    out = run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180,
              "--verify-live").stdout
    assert "goal_checker.xy_goal_tolerance^0.2" in out


# ── verify-live must terminate and must name what it stopped on ──────────────────────────
#
# The form this generator used to emit gave a deadline to one of its thirteen robot calls,
# ran for over three minutes on the Pi on 2026-09-21, and was killed without a verdict. The
# run sheet stopped calling it and carried its own bounded loop, which left the generator and
# the sheet disagreeing -- so the next sheet built from `--verify-live` would inherit the hang
# again. These are that defect, as assertions on the bytes the tool emits.

def _verify_live_body() -> str:
    """Just what is inside the single `~/mrun '...'` -- the part that runs on the robot."""
    out = run(OVERLAY, "--vendor-dir", VENDOR, "--length-mm", 240, "--width-mm", 180,
              *RULED, *GOAL_TOL, "--verify-live").stdout
    head = "~/mrun '"
    assert out.count(head) == 1, (
        "verify-live must emit ONE mrun block: each one is a fresh ssh, a fresh docker exec "
        "and three source lines before any ROS work starts")
    return out[out.index(head) + len(head):].rstrip().rstrip("'")


def test_every_robot_call_in_verify_live_carries_a_deadline():
    """`ros2 param get` and `ros2 param list` wait INDEFINITELY when the node's parameter
    service does not answer. One unanswered call hangs the whole gate."""
    body = _verify_live_body()
    calls = [ln for ln in body.splitlines() if "ros2 " in ln and not ln.lstrip().startswith("#")]
    assert calls, "no robot calls found at all"
    for call in calls:
        assert "timeout " in call, f"an unbounded robot call: {call.strip()}"


def test_verify_live_treats_a_timeout_as_its_own_outcome_and_fails_on_it():
    body = _verify_live_body()
    assert body.count("TIMEOUT") >= 3, "a read that never answers must be NAMED, not guessed at"
    assert "rc" in body and "124" in body, "the timeout exit status is never inspected"


def test_the_timeout_status_is_the_timeouts_and_not_tails():
    """The defect this is really about. `got=$(timeout 20 ... | tail -1); rc=$?` reads the
    exit of the LAST command in the pipeline -- `tail`, which succeeds on empty input -- so a
    read that timed out reported MISMATCH and 124 was never seen. Capturing with no pipe and
    trimming afterwards is portable to any POSIX shell, which PIPESTATUS is not, and the shell
    inside `docker exec` is not this tool's to choose."""
    body = _verify_live_body()
    import re
    assert not re.search(r"\|\s*tail[^\n]*\)\s*;?\s*rc=", body), (
        "a pipeline is captured and then its status read: that status is tail's")
    for capture in re.findall(r"got=\$\(timeout[^\n]*\)", body):
        assert "|" not in capture, f"the capture still contains a pipe: {capture}"


def test_verify_live_carries_no_apostrophe_that_would_end_the_mrun_quote():
    """The whole block is handed to `~/mrun` inside single quotes. One apostrophe in one
    comment ends the string and feeds the rest of the gate to the local shell."""
    assert "'" not in _verify_live_body()


# ── verify-live, executed: what the gate PRINTS for what the robot answers ───────────────
#
# The assertions above read the emitted bytes. These run them. The block goes to `bash -s`
# on stdin, exactly as `~/mrun` hands it to the container, with two stand-ins first on PATH:
# a `ros2` that answers from a table, and a `timeout` that keeps the block's own deadlines
# but scales them down so a read that never answers costs a fraction of a second here.
#
# The answers are the SHAPES the real parameter service printed on the previous site's floor:
# `Double value is: 0.2`, `String value is: polygon`, and for an array
# `Double values are: array('d', [0.2, 0.0, 1.0])`. A matcher proven only against strings
# somebody imagined is proven against the wrong thing.

_POLY = "[[0.12, 0.09], [-0.12, 0.09], [-0.12, -0.09], [0.12, -0.09]]"

# What a stack running the RULED + GOAL_TOL overlay answers, read by read.
_IN_FORCE = {
    "/local_costmap/local_costmap footprint": f"String value is: {_POLY}",
    "/local_costmap/local_costmap footprint_padding": "Double value is: 0.05",
    "/global_costmap/global_costmap footprint": f"String value is: {_POLY}",
    "/global_costmap/global_costmap footprint_padding": "Double value is: 0.05",
    "/controller_server FollowPath.footprint_model.type": "String value is: polygon",
    "/controller_server FollowPath.footprint_model.vertices": f"String value is: {_POLY}",
    "/controller_server FollowPath.min_turning_radius": "Double value is: 0.0",
    "/controller_server FollowPath.max_vel_x": "Double value is: 0.2",
    "/controller_server FollowPath.max_vel_x_backwards": "Double value is: 0.2",
    "/controller_server goal_checker.xy_goal_tolerance": "Double value is: 0.1",
    "/controller_server goal_checker.yaw_goal_tolerance": "Double value is: 0.5",
    "/velocity_smoother max_velocity": "Double values are: array('d', [0.2, 0.0, 1.0])",
    "/velocity_smoother min_velocity": "Double values are: array('d', [-0.2, 0.0, -1.0])",
}

_FAKE_ROS2 = r'''#!/usr/bin/env python3
import json, os, sys, time
answers = json.load(open(os.environ["FAKE_ROS2_ANSWERS"]))
hang = set(json.load(open(os.environ["FAKE_ROS2_HANG"])))
drop = set(json.load(open(os.environ["FAKE_ROS2_DROP"])))
a = sys.argv[1:]
if a[:2] == ["param", "get"]:
    key = a[2] + " " + a[3]
    if key in hang:
        time.sleep(3600)
    if key in drop:  # the FIRST reply is lost, as on the robot; a second read is answered
        mark = os.path.join(os.environ["FAKE_ROS2_STATE"], key.replace("/", "_").replace(" ", "__"))
        if not os.path.exists(mark):
            open(mark, "w").close()
            time.sleep(3600)
    print(answers.get(key, "Parameter not set"))
elif a[:2] == ["param", "list"]:
    print("  FollowPath.max_vel_x")
elif a[:2] == ["topic", "echo"]:
    print("polygon:")
'''

# Keeps every deadline the block sets and scales it, so `timeout 20` still means "this read
# gets a deadline" and a hung read still ends in 124 -- in 0.1 s instead of 20.
_FAKE_TIMEOUT = r'''#!/usr/bin/env python3
import subprocess, sys
secs, cmd = float(sys.argv[1]), sys.argv[2:]
try:
    sys.exit(subprocess.run(cmd, timeout=secs * 0.005, stdin=subprocess.DEVNULL).returncode)
except subprocess.TimeoutExpired:
    sys.exit(124)
'''


def _execute(tmp_path, body, answers, hang=(), drop=()):
    import json
    import os
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir(exist_ok=True)
    for name, src in (("ros2", _FAKE_ROS2), ("timeout", _FAKE_TIMEOUT)):
        (bin_dir / name).write_text(src.replace("/usr/bin/env python3", sys.executable, 1))
        (bin_dir / name).chmod(0o755)
    (tmp_path / "answers.json").write_text(json.dumps(answers))
    (tmp_path / "hang.json").write_text(json.dumps(list(hang)))
    (tmp_path / "drop.json").write_text(json.dumps(list(drop)))
    (tmp_path / "state").mkdir(exist_ok=True)
    env = dict(os.environ, PATH=f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
               FAKE_ROS2_ANSWERS=str(tmp_path / "answers.json"),
               FAKE_ROS2_HANG=str(tmp_path / "hang.json"),
               FAKE_ROS2_DROP=str(tmp_path / "drop.json"),
               FAKE_ROS2_STATE=str(tmp_path / "state"))
    p = subprocess.run(["bash", "-s"], input=body, capture_output=True, text=True,
                       env=env, timeout=60)
    return p.stdout + p.stderr


def _line(out, name):
    """The one verdict line for a parameter, matched on the whole name so that
    `FollowPath.max_vel_x` does not also find `FollowPath.max_vel_x_backwards`. The reverse cap
    prints under its own label, `reverse cap`, and the vertices check under `vertices`."""
    def names(ln):
        if name in ("reverse cap", "vertices"):
            return name in ln
        if " " in name:  # "node param": both halves, and the whole parameter name
            return f"{name}:" in ln or f"{name} --" in ln
        return name in ln.replace(":", " ").split()
    hits = [ln for ln in out.splitlines()
            if ln.startswith(("ok ", "MISMATCH", "TIMEOUT")) and names(ln)]
    assert len(hits) == 1, f"expected one verdict line for {name}, got {hits}\n{out}"
    return hits[0]


def _label(node_param):
    return "reverse cap" if node_param.endswith("max_vel_x_backwards") else node_param


def test_verify_live_passes_a_stack_that_answers_exactly_what_the_overlay_wrote(tmp_path):
    out = _execute(tmp_path, _verify_live_body(), _IN_FORCE)
    assert "VERIFY-LIVE OK" in out, out
    assert not [ln for ln in out.splitlines() if ln.startswith(("MISMATCH", "TIMEOUT"))], out


@pytest.mark.parametrize("node_param, answer", [
    ("/controller_server FollowPath.max_vel_x", "Double value is: 0.25"),
    ("/controller_server FollowPath.max_vel_x", "Double value is: 10.2"),
    ("/controller_server FollowPath.min_turning_radius", "Double value is: 0.05"),
    ("/local_costmap/local_costmap footprint_padding", "Double value is: 0.055"),
    ("/controller_server goal_checker.xy_goal_tolerance", "Double value is: 0.15"),
    ("/controller_server goal_checker.yaw_goal_tolerance", "Double value is: 0.55"),
    ("/controller_server FollowPath.footprint_model.type", "String value is: polygons"),
    ("/velocity_smoother max_velocity", "Double values are: array('d', [0.2, 0.0, 1.05])"),
    ("/controller_server FollowPath.max_vel_x_backwards", "Double value is: 0.25"),
    ("/controller_server FollowPath.max_vel_x_backwards", "Double value is: 0.35"),
])
def test_a_value_that_merely_contains_the_expected_one_is_a_mismatch(tmp_path, node_param,
                                                                    answer):
    """A 0.25 m/s cap read against an approved 0.20 envelope is the case this gate exists
    for: a partly-applied overlay or a vendor default reads back a number that is NOT the one
    written, and a substring test calls it `ok` because `0.2` occurs inside `0.25`."""
    out = _execute(tmp_path, _verify_live_body(), dict(_IN_FORCE, **{node_param: answer}))
    assert _line(out, _label(node_param)).startswith("MISMATCH"), out
    assert "VERIFY-LIVE FAILED" in out


def test_a_parameter_the_node_does_not_have_is_a_mismatch(tmp_path):
    answers = dict(_IN_FORCE)
    del answers["/controller_server FollowPath.min_turning_radius"]
    out = _execute(tmp_path, _verify_live_body(), answers)
    assert _line(out, "FollowPath.min_turning_radius").startswith("MISMATCH"), out
    assert "VERIFY-LIVE FAILED" in out


def test_an_unset_reverse_cap_is_still_accepted(tmp_path):
    """The TEB reverse cap is absent from the vendor file; not set at all is the vendor
    state and is fine. Exactness must not turn that into a failure."""
    answers = dict(_IN_FORCE)
    del answers["/controller_server FollowPath.max_vel_x_backwards"]
    out = _execute(tmp_path, _verify_live_body(), answers)
    assert _line(out, "reverse cap").startswith("ok"), out
    assert "VERIFY-LIVE OK" in out, out


def test_either_rendering_of_the_vertices_is_accepted_and_nothing_else(tmp_path):
    flat = "[0.12, 0.09, -0.12, 0.09, -0.12, -0.09, 0.12, -0.09]"
    key = "/controller_server FollowPath.footprint_model.vertices"
    out = _execute(tmp_path, _verify_live_body(),
                   dict(_IN_FORCE, **{key: f"Double values are: array('d', {flat})"}))
    assert _line(out, "vertices").startswith("ok"), out
    out = _execute(tmp_path, _verify_live_body(),
                   dict(_IN_FORCE, **{key: f"String value is: {_POLY[:-1]}, [0.0, 0.0]]"}))
    assert _line(out, "vertices").startswith("MISMATCH"), out


def test_a_read_that_never_answers_is_named_and_the_block_still_finishes(tmp_path):
    """The hang itself, not the absence of a word in the bytes. One unanswered read must cost
    one deadline, print TIMEOUT against the parameter it was, fail the gate -- and every read
    after it must still happen."""
    hung = "/controller_server FollowPath.max_vel_x"
    out = _execute(tmp_path, _verify_live_body(), _IN_FORCE, hang=[hung])
    assert _line(out, "FollowPath.max_vel_x").startswith("TIMEOUT"), out
    assert _line(out, "goal_checker.xy_goal_tolerance").startswith("ok"), (
        "a read after the hung one never happened: one unanswered call still hangs the gate")
    assert "VERIFY-LIVE FAILED" in out
    assert out.index("VERIFY-LIVE will read") < out.index("TIMEOUT"), (
        "every read must be named before the first one is made, so a block killed anyway "
        "still says where it got to")


def test_the_executed_check_tells_the_old_pipe_capture_from_the_fix(tmp_path):
    """The control for the test above: put the pipe back into the capture -- the form the
    sheet carried before -- and the same hung read is reported as a MISMATCH with an empty
    value, because the status read is tail's. If this ever stops holding, the test above is
    no longer able to see the defect it guards."""
    body = _verify_live_body()
    fixed = 'got=$(timeout 20 ros2 param get "$1" "$2" 2>&1); rc=$?'
    assert body.count(fixed) == 2, "the read helper changed shape; update this control with it"
    old = body.replace(fixed, 'got=$(timeout 20 ros2 param get "$1" "$2" 2>&1 | tail -1); rc=$?')
    out = _execute(tmp_path, old, _IN_FORCE, hang=["/controller_server FollowPath.max_vel_x"])
    assert _line(out, "FollowPath.max_vel_x").startswith("MISMATCH"), out



# ── one more read after a dropped reply ──────────────────────────────────────────────────
#
# On the robot the controller server twice computed an answer and logged that it could not
# DELIVER it ("failed to send response ... (timeout)"): the read timed out and a whole charge
# was lost to a transport drop, not to a wrong setting. A read that gets no answer is read ONCE
# more, named on a RETRY line, and the verdict says so. A second silence is still a TIMEOUT and
# fails; a wrong value is never read again.

@pytest.mark.parametrize("key, label", [
    ("/controller_server FollowPath.max_vel_x", "FollowPath.max_vel_x"),
    ("/controller_server FollowPath.footprint_model.vertices", "vertices"),
    ("/controller_server FollowPath.max_vel_x_backwards", "reverse cap"),
])
def test_a_reply_dropped_once_is_read_again_named_and_the_gate_passes(tmp_path, key, label):
    out = _execute(tmp_path, _verify_live_body(), _IN_FORCE, drop=[key])
    retries = [ln for ln in out.splitlines() if ln.startswith("RETRY")]
    assert len(retries) == 1 and key.split()[1] in retries[0], out
    assert _line(out, label).startswith("ok"), out
    assert out.rstrip().endswith(
        "VERIFY-LIVE OK -- the overlay is in force (1 read retried after a dropped reply)"), out


def test_a_reply_dropped_twice_is_a_timeout_and_the_gate_fails(tmp_path):
    hung = "/controller_server FollowPath.max_vel_x"
    out = _execute(tmp_path, _verify_live_body(), _IN_FORCE, hang=[hung])
    assert [ln for ln in out.splitlines() if ln.startswith("RETRY")], out
    assert _line(out, "FollowPath.max_vel_x").startswith("TIMEOUT"), out
    assert "VERIFY-LIVE FAILED" in out


def test_a_wrong_value_is_never_read_again(tmp_path):
    wrong = {"/controller_server FollowPath.max_vel_x": "Double value is: 0.25"}
    out = _execute(tmp_path, _verify_live_body(), dict(_IN_FORCE, **wrong))
    assert not [ln for ln in out.splitlines() if ln.startswith("RETRY")], out
    assert _line(out, "FollowPath.max_vel_x").startswith("MISMATCH"), out
    assert "VERIFY-LIVE FAILED" in out


def test_a_clean_run_mentions_no_retry(tmp_path):
    out = _execute(tmp_path, _verify_live_body(), _IN_FORCE)
    assert out.rstrip().endswith("VERIFY-LIVE OK -- the overlay is in force"), out
    assert "RETRY" not in out

# ── the pose picker ──────────────────────────────────────────────────────────────────────

def _map(tmp_path: Path, rows: int, cols: int, paint, *, res=0.05, name="m") -> Path:
    """A synthetic .pgm/.yaml pair. `paint(r, c)` returns 254 free, 0 occupied, 205 unknown."""
    body = bytearray()
    for r in range(rows):
        for c in range(cols):
            body.append(paint(r, c))
    pgm = tmp_path / f"{name}.pgm"
    pgm.write_bytes(b"P5\n%d %d\n255\n" % (cols, rows) + bytes(body))
    yml = tmp_path / f"{name}.yaml"
    yml.write_text(f"image: {name}.pgm\nmode: trinary\nresolution: {res}\n"
                   f"origin: [0.0, 0.0, 0.0]\nnegate: 0\noccupied_thresh: 0.65\n"
                   f"free_thresh: 0.25\n")
    return yml


def _corridor(tmp_path: Path, *, length_m=14.0, hall_w_m=1.6, room_r_m=2.0,
              room_len_m=4.0, res=0.05):
    """Two open rooms joined by a hall, all free, everything else unknown.

    `room_r_m` is the room's HALF-height and `room_len_m` its length, so a disc of
    `min_turning_radius + half-diagonal + padding` fits inside a room and does not fit in
    the hall -- which is the distinction the picker exists to make.
    """
    margin = 20
    cols = int(length_m / res) + 2 * margin
    rows = int(2 * room_r_m / res) + 2 * margin
    mid = rows // 2
    hall_half = int(hall_w_m / 2 / res)
    room_half = int(room_r_m / res)
    room_len = int(room_len_m / res)
    x0, x1 = margin, cols - margin

    def paint(r, c):
        if not (x0 <= c <= x1):
            return 205
        near_end = c <= x0 + room_len or c >= x1 - room_len
        half = room_half if near_end else hall_half
        return 254 if abs(r - mid) <= half else 205
    return _map(tmp_path, rows, cols, paint, res=res)


def _poses(map_yaml, *args, expect=0):
    return run(POSES, "--map", map_yaml, "--length-mm", 240, "--width-mm", 180,
               "--padding-m", 0.05, *args, expect=expect)


def test_a_floor_with_two_open_ends_yields_an_env_value_and_a_png(tmp_path):
    pytest.importorskip("scipy")
    png = tmp_path / "out" / "poses.png"
    out = _poses(_corridor(tmp_path), "--free-thresh", 0.196, "--png", png)
    assert out.stdout.startswith('SWARMADA_CONFORMANCE_GOAL_POSES="')
    body = out.stdout.split('"')[1]
    a, b = body.split(";")
    ax, ay = (float(v) for v in a.split(","))
    bx, by = (float(v) for v in b.split(","))
    assert ((ax - bx) ** 2 + (ay - by) ** 2) ** 0.5 >= 7.5, "the poses are closer than the " \
        "harness's longest-needing check at its own fixture speed"
    assert png.exists() and png.stat().st_size > 0
    assert "OBSERVED-FREE" in out.stderr or "observed-free" in out.stderr


def test_a_floor_whose_ends_have_no_room_to_turn_around_is_refused(tmp_path):
    pytest.importorskip("scipy")
    # Wide enough to drive along, nowhere wide enough for a 1.0 m-radius reversal.
    out = _poses(_corridor(tmp_path, room_r_m=0.6, hall_w_m=1.0, room_len_m=1.2),
                 "--free-thresh", 0.196, expect=2)
    assert "REFUSED" in out.stderr
    assert "clearance a reversal needs" in out.stderr or "end clearance" in out.stderr


def test_a_floor_too_short_for_the_harness_is_refused_with_the_distance_it_reached(tmp_path):
    pytest.importorskip("scipy")
    out = _poses(_corridor(tmp_path, length_m=6.0), "--free-thresh", 0.196, expect=2)
    assert "REFUSED" in out.stderr and "7.5 m apart" in out.stderr


def test_a_map_whose_free_threshold_admits_unobserved_space_is_refused(tmp_path):
    pytest.importorskip("scipy")
    out = _poses(_corridor(tmp_path), expect=1)          # no --free-thresh: the yaml's 0.25
    assert "cannot separate unknown from free" in out.stderr
    assert "nobody observed" in out.stderr


def test_the_requirement_is_printed_with_its_terms_so_the_number_can_be_checked(tmp_path):
    pytest.importorskip("scipy")
    out = _poses(_corridor(tmp_path), "--free-thresh", 0.196)
    assert "half-width 0.090 + padding 0.05 + margin 0.3" in out.stderr
    assert "turning radius 1 + half-diagonal 0.150 + padding 0.05" in out.stderr


def test_a_trajectory_restricts_the_poses_to_cells_the_robot_actually_drove(tmp_path):
    pytest.importorskip("scipy")
    m = _corridor(tmp_path)
    traj = tmp_path / "traj.txt"
    # A trajectory along only the left half of the floor: no pair 7.5 m apart survives it.
    traj.write_text("\n".join(f"{x:.2f} {1.6:.2f}" for x in
                              [i * 0.1 for i in range(int(4.0 / 0.1))]))
    out = _poses(m, "--free-thresh", 0.196, "--trajectory", traj, expect=2)
    assert "REFUSED" in out.stderr
    assert "driven-eligible" in out.stderr, "the driven tier was not announced"


def test_without_a_trajectory_the_weaker_tier_is_named_rather_than_passed_off(tmp_path):
    pytest.importorskip("scipy")
    out = _poses(_corridor(tmp_path), "--free-thresh", 0.196)
    assert "DRIVEN TIER UNAVAILABLE" in out.stderr
    assert "tier observed-free" in out.stderr


# ── the boot gate ──────────────────────────────────────────────────────────
#
# The series is one stationary capture of odom -> base_footprint, and the gate is the
# least-squares slope of its yaw. The fixtures below are synthetic, in the exact text the
# transform echo prints, so the parser is tested against the format rather than against a
# recording of one particular minute.

_RECORD = """At time {t:.9f}
- Translation: [{x:.3f}, {y:.3f}, 0.000]
- Rotation: in Quaternion (xyzw) [0.000, 0.000, 0.935, -0.355]
- Rotation: in RPY (radian) [0.000, -0.000, -2.415]
- Rotation: in RPY (degree) [0.000, -0.000, {yaw:.3f}]
- Matrix:
 -0.748  0.664  0.000  0.000
 -0.664 -0.748  0.000  0.000
  0.000  0.000  1.000  0.000
  0.000  0.000  0.000  1.000
"""


def _echo(tmp_path, *, seconds=64, drift_per_min=0.0, moved=False, start_yaw=-138.380,
          stamp0=1789940895.915, name="raw.txt"):
    """A stationary capture at 1 Hz, with a yaw drift put in on purpose."""
    body = []
    for i in range(seconds):
        t = stamp0 + i
        yaw = start_yaw + drift_per_min * (i / 60.0)
        x = 0.004 * i if moved else 0.0
        body.append(_RECORD.format(t=t, x=x, y=0.0, yaw=yaw))
    path = tmp_path / name
    path.write_text("".join(body))
    return path


def test_the_gate_reads_the_angle_and_not_the_line_after_it(tmp_path):
    """The series must contain yaw degrees. A pipeline that took the line BELOW the angle
    produced a file of matrix rows, which still looked like a series and gated nothing."""
    out = run(BOOT_GATE, _echo(tmp_path), "--series-out", tmp_path / "series.txt").stdout
    series = (tmp_path / "series.txt").read_text().splitlines()
    assert len(series) == 64
    assert series[0].split()[3] == "-138.380"
    assert "yaw offset at the start of the series: -138.380" in out


def test_a_stationary_minute_within_the_limit_passes(tmp_path):
    out = run(BOOT_GATE, _echo(tmp_path, drift_per_min=1.3)).stdout
    assert "BOOT GATE PASSED" in out
    assert "+1.300 deg/min" in out


def test_a_drifting_minute_fails_with_the_slope_it_measured(tmp_path):
    """The reading this gate exists for: the robot is motionless and the yaw is not."""
    out = run(BOOT_GATE, _echo(tmp_path, drift_per_min=-5.04), expect=1).stdout
    assert "-5.040 deg/min" in out
    assert "BOOT GATE FAILED" in out
    assert "NO DRIVING THIS CYCLE" in out


def test_the_offset_is_recorded_and_never_gated(tmp_path):
    """Four of the seven boots on record started outside +/-2 deg and every one was healthy."""
    out = run(BOOT_GATE, _echo(tmp_path, start_yaw=-158.825, drift_per_min=0.2)).stdout
    assert "-158.825" in out
    assert "RECORDED, NOT GATED" in out
    assert "BOOT GATE PASSED" in out


def test_a_series_whose_translation_moved_is_void(tmp_path):
    out = run(BOOT_GATE, _echo(tmp_path, moved=True), expect=1).stdout
    assert "the robot MOVED and the series is void" in out


def test_twenty_seconds_is_not_a_minute(tmp_path):
    """A 20 s series cannot separate a slow drift from noise, and the short series is exactly
    what the earlier sheet would have collected."""
    out = run(BOOT_GATE, _echo(tmp_path, seconds=20), expect=1).stdout
    assert "BOOT GATE FAILED" in out
    assert "samples" in out


def test_a_static_transform_is_refused_rather_than_reported_as_no_drift(tmp_path):
    """A static transform prints `At time 0.0` for every sample. Fitting a slope to that
    gives a tidy zero off a reading that measured nothing."""
    raw = tmp_path / "static.txt"
    raw.write_text("".join(_RECORD.format(t=0.0, x=0.0, y=0.0, yaw=-138.381)
                           for _ in range(64)))
    out = run(BOOT_GATE, raw, expect=1).stdout
    assert "STATIC transform" in out
    assert "0.000 deg/min" not in out


def test_output_that_is_not_a_transform_echo_is_refused_not_passed(tmp_path):
    raw = tmp_path / "nothing.txt"
    raw.write_text("[INFO] [tf2_echo]: Waiting for transform odom -> base_footprint\n")
    p = run(BOOT_GATE, raw, expect=1)
    assert "no complete" in p.stderr


def test_a_parsed_series_can_be_regated_without_the_raw_capture(tmp_path):
    """The series is what the capture directory keeps; it has to stay enough to re-decide."""
    run(BOOT_GATE, _echo(tmp_path, drift_per_min=1.3), "--series-out", tmp_path / "s.txt")
    out = run(BOOT_GATE, "--series", tmp_path / "s.txt").stdout
    assert "+1.300 deg/min" in out and "BOOT GATE PASSED" in out
