"""The reference simulation adapter serves ``Custom`` explore-floor and ``Charge`` to completion.

Before this, both types were in the catalog and neither ever finished: an action that does not
move the robot set no target, and only arrival completed an action, so a Charge or a Custom ran
until its lease expired. A looped FleetTask of explore segments and charges could not advance.

The map leaves the adapter by reference: saved at ``<namespace>/<name>`` under the artifact
directory, with the reference and the manifest's digest in the final SUCCEEDED message.
"""

from __future__ import annotations

import hashlib
import json
import re

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation import floor_mapping as fm  # noqa: E402
from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

_REF = re.compile(r"map-ref=(\S+) map-sha256=([0-9a-f]{64})$")


def _adapter(tmp_path) -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim",
                   artifact_dir=str(tmp_path), artifact_store="floor-maps")
    a._sim.spawn("amr-1", 0.0, 0.0)
    return a


def _drain(a: SimAdapter) -> list:
    out = []
    while not a._outbox.empty():
        out.append(a._outbox.get_nowait())
    return out


def _assign(a, action_id, action_type, payload, token):
    a._handle_command(pb.Command(command_id=token, robot_id="amr-1", assign_action=pb.AssignAction(
        action_id=action_id, assignment_id=f"a{token}", action_type=action_type,
        fencing_token=token, lease_duration_ms=600_000, lease_generation=token,
        payload_json=json.dumps(payload).encode())))
    results = [m.command_result.assign_action for m in _drain(a)
               if m.WhichOneof("payload") == "command_result"]
    return results[-1]


def _run(a, seconds: float) -> list:
    """Advance the adapter's non-motion work by ``seconds`` and return the status updates."""
    updates = []
    steps = int(round(seconds / 0.2))
    for _ in range(steps):
        with a._sim_lock:
            a._sim.tick(0.2)
        a._advance_job(0.2)
        updates += [m.action_status for m in _drain(a)
                    if m.WhichOneof("payload") == "action_status"]
    return updates


def _terminal(updates):
    return [u for u in updates if u.state != pb.ACTION_STATE_RUNNING]


EXPLORE = {"operation": "explore-floor", "bound_ref": "room-1", "max_speed_mps": 0.2}


def test_the_catalog_declares_explore_floor_with_its_bounds_and_results(tmp_path):
    custom = next(s for s in _adapter(tmp_path)._supported_actions() if s.action_type == "Custom")
    params = {p.name: p for p in custom.params}
    assert list(params["operation"].allowed) == ["explore-floor"]
    assert params["operation"].kind == "enum"
    assert params["max_speed_mps"].max == pytest.approx(0.20)
    assert params["resume"].kind == "bool"
    assert {r.name for r in custom.results} == {"map-ref", "map-sha256", "covered-m2"}


@pytest.mark.parametrize("speed", [0.21, 0.5, 5.0])
def test_a_speed_above_the_ceiling_is_refused_not_clamped(tmp_path, speed):
    a = _adapter(tmp_path)
    res = _assign(a, "ns/act-1", "Custom", {**EXPLORE, "max_speed_mps": speed}, 1)
    assert not res.accepted
    assert res.rejection == pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION
    assert "above the maximum of 0.2" in res.message


def test_an_operation_the_adapter_does_not_serve_is_refused(tmp_path):
    res = _assign(_adapter(tmp_path), "ns/act-1", "Custom", {"operation": "mow"}, 1)
    assert not res.accepted and res.rejection == pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION
    assert "explore-floor" in res.message


def test_custom_with_no_operation_keeps_its_old_behaviour(tmp_path):
    a = _adapter(tmp_path)
    assert _assign(a, "ns/act-1", "Custom", {"x": 1.0, "y": 1.0}, 1).accepted
    assert _terminal(_run(a, 10.0)) == [], "a Custom with no operation runs until cancelled"


def test_a_segment_succeeds_with_a_verifiable_map_reference(tmp_path):
    a = _adapter(tmp_path)
    assert _assign(a, "warehouse-a/explore-1", "Custom", EXPLORE, 1).accepted
    updates = _run(a, 8.0)
    [done] = _terminal(updates)
    assert done.state == pb.ACTION_STATE_SUCCEEDED
    assert done.fencing_token == 1
    m = _REF.search(done.message)
    assert m, done.message
    assert m.group(1) == "floor-maps/warehouse-a/explore-1"
    d = tmp_path / "warehouse-a" / "explore-1"
    manifest = (d / fm.MANIFEST).read_bytes()
    assert hashlib.sha256(manifest).hexdigest() == m.group(2)
    for line in manifest.decode().splitlines():
        digest, name = line.split("  ")
        assert hashlib.sha256((d / name).read_bytes()).hexdigest() == digest
    assert {"map.yaml", "map.pgm"} <= {ln.split("  ")[1] for ln in manifest.decode().splitlines()}
    assert "frontier: remaining" in done.message
    assert a._current_action == "", "a finished segment frees the robot"


def test_a_bare_action_name_is_saved_under_the_adapters_namespace(tmp_path):
    # The control plane sends AssignAction.action_id as the FleetAction's name alone, and
    # the adapter knows its namespace from its own configuration.
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim",
                   namespace="warehouse-a", artifact_dir=str(tmp_path),
                   artifact_store="floor-maps")
    a._sim.spawn("amr-1", 0.0, 0.0)
    assert _assign(a, "explore-1", "Custom", EXPLORE, 1).accepted
    [done] = _terminal(_run(a, 8.0))
    assert done.state == pb.ACTION_STATE_SUCCEEDED, done.message
    assert _REF.search(done.message).group(1) == "floor-maps/warehouse-a/explore-1"
    assert (tmp_path / "warehouse-a" / "explore-1" / fm.MANIFEST).is_file()


def test_running_reports_are_phase_changes_not_ticks(tmp_path):
    a = _adapter(tmp_path)
    _assign(a, "warehouse-a/explore-1", "Custom", EXPLORE, 1)
    running = [u for u in _run(a, 8.0) if u.state == pb.ACTION_STATE_RUNNING]
    assert 1 <= len(running) <= 3, [u.message for u in running]


def test_resume_continues_and_a_segment_with_nothing_left_ends_at_once(tmp_path):
    a = _adapter(tmp_path)
    _assign(a, "ns/explore-first", "Custom", EXPLORE, 1)
    _run(a, 8.0)
    _assign(a, "ns/explore-resume-i1", "Custom", {**EXPLORE, "resume": True}, 2)
    [second] = _terminal(_run(a, 8.0))
    assert "frontier: none-left" in second.message and "(100%)" in second.message
    _assign(a, "ns/explore-resume-i2", "Custom", {**EXPLORE, "resume": True}, 3)
    [third] = _terminal(_run(a, 0.2))
    assert third.state == pb.ACTION_STATE_SUCCEEDED
    assert "frontier: none-left" in third.message


def test_resume_with_no_saved_graph_fails_and_says_why(tmp_path):
    a = _adapter(tmp_path)
    _assign(a, "ns/explore-resume-i1", "Custom", {**EXPLORE, "resume": True}, 1)
    [done] = _terminal(_run(a, 1.0))
    assert done.state == pb.ACTION_STATE_FAILED
    assert "no saved graph" in done.message


def test_cancel_mid_segment_ends_it_with_no_success_and_no_map(tmp_path):
    a = _adapter(tmp_path)
    _assign(a, "ns/explore-1", "Custom", EXPLORE, 1)
    _run(a, 1.0)
    a._handle_command(pb.Command(command_id=9, robot_id="amr-1",
                                 cancel_action=pb.CancelAction(action_id="ns/explore-1")))
    _drain(a)
    assert _terminal(_run(a, 10.0)) == []
    assert not (tmp_path / "ns" / "explore-1").exists()


def test_charge_waits_then_succeeds_when_full_and_never_moves(tmp_path):
    a = _adapter(tmp_path)
    a._sim._robots["amr-1"].battery = 40.0
    assert _assign(a, "ns/charge-1", "Charge", {}, 1).accepted
    updates = _run(a, 10.0)
    assert any("waiting for charge" in u.message for u in updates
               if u.state == pb.ACTION_STATE_RUNNING)
    [done] = _terminal(updates)
    assert done.state == pb.ACTION_STATE_SUCCEEDED
    assert "battery full" in done.message and "cable-out is not observed" in done.message
    assert a._sim.battery_percent("amr-1") >= fm.FULL_PCT
    assert a._sim.pose("amr-1").x == 0.0 and a._sim.pose("amr-1").y == 0.0
