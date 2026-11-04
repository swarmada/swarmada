# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""RFC-0001 says what a robot is sent for a Revoking action when its adapter reconnects.

Why this file exists. The FleetAction chapter said that on entering Revoking the control
plane "queues `cancel_action` for next reconnect". Nothing is queued. The revoke branch of
the FleetAction reconciler stops renewing the lease and sends nothing, and on reconnect the
registrar projects a Revoking action into the `RegisterAck` as
`ROBOT_ACTION_PHASE_REVOKING`: the robot continues while its local lease is live and
self-stops when it expires. A reader of the old text expected a stop on reconnect; the
robot is told to keep going until its lease runs out.

These tests read the code's projection and the published text (the assembled dist, which
is what ships), and hold the two together.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DIST = ROOT / "rfcs" / "dist" / "RFC-0001-core-spec.md"
REGISTRAR = ROOT / "internal" / "registrar" / "registrar.go"
RECONCILER = ROOT / "internal" / "controller" / "fleetaction_controller.go"


def _row(lines: list[str], rid: str) -> str | None:
    """A row of RFC-0001's action table with its rules block: the one-line row, then the
    paragraph led `**A13 — ...**` and the bullets under it, blockquotes left out."""
    row = next((ln for ln in lines if ln.startswith(f"| {rid} |")), None)
    if row is None:
        return None
    out, inside = [row], False
    for ln in lines:
        if ln.startswith(f"**{rid} — "):
            inside = True
            continue
        if inside and ln.startswith(("**", "#")):
            break
        if inside and not ln.lstrip().startswith(">"):
            out.append(ln.strip())
    return " ".join(out)


def _func(src: str, name: str) -> str:
    m = re.search(rf"^func (?:\([^)]*\) )?{name}\(.*?^}}", src, flags=re.MULTILINE | re.DOTALL)
    assert m, f"func {name} not found"
    return m.group(0)


def test_the_registrar_projects_revoking_as_revoking():
    body = _func(REGISTRAR.read_text(), "mapActionPhase")
    m = re.search(r"case fleetv1\.ActionPhaseRevoking:\s*return fav1\.RobotActionPhase_(\w+)", body)
    assert m, "mapActionPhase has no case for ActionPhaseRevoking; re-read the reconnect text"
    assert m.group(1) == "ROBOT_ACTION_PHASE_REVOKING", (
        f"a Revoking action is projected as {m.group(1)}; RFC-0001's revoke text says REVOKING")


def test_a_revoking_action_a_stop_holds_is_projected_as_idle():
    body = _func(REGISTRAR.read_text(), "authoritativeActionState")
    m = re.search(r"if phase != fav1\.RobotActionPhase_ROBOT_ACTION_PHASE_CANCELLED"
                  r" && stopHoldsWork\(robot\) \{\s*phase = fav1\.RobotActionPhase_(\w+)", body)
    assert m, "authoritativeActionState does not override the phase of an action a stop holds"
    assert m.group(1) == "ROBOT_ACTION_PHASE_IDLE", (
        f"an action a stop holds is projected as {m.group(1)}; RFC-0001 row A13 says IDLE")
    a13 = _row(DIST.read_text().splitlines(), "A13")
    assert a13, "row A13 of the action table was not found"
    want = re.compile(r"for an action held by A6,? the control plane MUST send "
                      r"`ROBOT_ACTION_PHASE_IDLE`", re.IGNORECASE)
    assert want.search(a13), "row A13 does not say an action a stop holds is sent IDLE"


def test_the_revoke_branch_sends_no_cancel():
    src = RECONCILER.read_text()
    branch = src.split("case actionRevoke:", 1)[1].split("\n\t\tcase ", 1)[0]
    assert "Revoking" in branch, "the revoke branch was not found where this test looks"
    for call in ("PushCancelAction", "pushCancel", "CancelAction("):
        assert call not in branch, (
            f"the revoke branch now calls {call}; RFC-0001 says nothing is sent on revoke")


def test_the_published_text_says_nothing_is_queued_for_the_reconnect():
    text = " ".join(DIST.read_text().split())
    queued = re.findall(r"[^.|]*queues?\s+(?:a\s+)?`cancel_action`[^.|]*", text)
    assert not queued, f"RFC-0001 still says a cancel_action is queued: {queued}"

    lines = DIST.read_text().splitlines()
    a7 = _row(lines, "A7")
    assert a7, "row A7 of the action table was not found"
    assert "nothing is queued for the robot" in a7, "row A7 does not say nothing is queued"
    a13 = _row(lines, "A13")
    assert a13, "row A13 of the action table was not found"
    assert re.search(r"`Revoking`(?: →|,) `ROBOT_ACTION_PHASE_REVOKING`", a13), (
        "row A13 does not name what the robot is sent for a Revoking action")
    revoking = next((ln for ln in lines if ln.startswith("| `REVOKING` |")), None)
    assert revoking, "the REVOKING row of the adapter's RegisterAck table was not found"
    assert "self-stop" in revoking, (
        "the REVOKING row does not say the robot self-stops at lease expiry")
