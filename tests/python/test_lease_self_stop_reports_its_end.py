# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""An in-tree adapter's lease self-stop sends the commanded stop, then the action's end.

`ACTION_STATE_STOPPED` says the stop was commanded and is not terminal (RFC-0001 safety
chapter, *terminal*; the proto's `ACTION_STATE_STOPPED` comment). The terminal report follows
it, once rest is confirmed or cannot be, and C4.2 grades that report
(`adapters/CONFORMANCE.md`, C4.2). The simulation adapter and the no-op example sent only
`STOPPED`, so the control plane was never told the action ended. The adapter template is
covered by `test_adapter_template_conformance.py`, which runs a generated adapter through the
harness.
"""

from __future__ import annotations

import importlib.util
import pathlib

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # pragma: no cover - depends on --stub-path layout
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

_ROOT = pathlib.Path(__file__).resolve().parents[2]


def _drain(outbox) -> list:
    out = []
    while not outbox.empty():
        msg = outbox.get()
        if msg.WhichOneof("payload") == "action_status":
            out.append(msg.action_status)
    return out


def test_the_simulation_adapter_ends_a_lapsed_lease_after_the_stop() -> None:
    from adapters.simulation.sim_adapter import SimAdapter
    from adapters.simulation.simulator import make_simulator

    robot = "sim-robot-001"
    a = SimAdapter(pb, None, make_simulator("kinematic"), robot, "warehouse-a", "sim")
    a._sim.spawn(robot, 0.0, 0.0)
    a._sim.command_move(robot, 10.0, 0.0)
    a._sim.tick(0.1)
    assert not a._sim.is_stopped(robot), "the robot is moving when its lease lapses"
    a._current_action = a._lease_action = "haul-lapsed"
    a._action_fencing_token = 11

    a._self_stop(robot)

    sent = _drain(a._outbox)
    assert [(u.action_id, u.state) for u in sent] == [
        ("haul-lapsed", pb.ACTION_STATE_STOPPED), ("haul-lapsed", pb.ACTION_STATE_FAILED)], sent
    end = sent[-1]
    assert end.end_cause == pb.ACTION_END_CAUSE_LEASE_EXPIRED
    assert end.fencing_token == 11, "the end echoes the token the action was accepted under"
    assert a._sim.is_stopped(robot), "the end is sent once the robot is at rest"
    assert a._current_action == "" and a._lease_action == ""


def test_the_simulation_adapter_ignores_a_superseded_lease_timer() -> None:
    from adapters.simulation.sim_adapter import SimAdapter
    from adapters.simulation.simulator import make_simulator

    robot = "sim-robot-001"
    a = SimAdapter(pb, None, make_simulator("kinematic"), robot, "warehouse-a", "sim")
    a._sim.spawn(robot, 0.0, 0.0)
    a._lease_action, a._current_action = "old", "live"

    a._self_stop(robot)

    assert _drain(a._outbox) == []
    assert a._current_action == "live"


def _noop():
    spec = importlib.util.spec_from_file_location(
        "noop_adapter_under_test", _ROOT / "adapters" / "example-noop" / "noop_adapter.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.NoopAdapter(pb, None, "noop", "default")


def test_the_noop_example_ends_a_lapsed_lease_after_the_stop() -> None:
    a = _noop()
    a._action["r1"], a._lease_epoch["r1"], a._fence["r1"] = "t-lapsed", 3, 9

    a._lease_expired("r1", "t-lapsed", 3)

    sent = _drain(a._outbox)
    assert [(u.action_id, u.state) for u in sent] == [
        ("t-lapsed", pb.ACTION_STATE_STOPPED), ("t-lapsed", pb.ACTION_STATE_FAILED)], sent
    assert sent[-1].end_cause == pb.ACTION_END_CAUSE_LEASE_EXPIRED
    assert sent[0].end_cause == pb.ACTION_END_CAUSE_UNSPECIFIED, "STOPPED carries no end cause"
    assert a._action["r1"] == ""
