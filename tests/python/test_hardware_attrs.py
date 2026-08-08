# Copyright 2026 The Swarmada Authors. Apache-2.0.
#
# The physical hardware attributes (fleet_adapter.v1 HardwareComponent tags 6-13) travel
# manifest -> ScenarioEngine -> wire with EXPLICIT PRESENCE intact.
#
# The property that matters is the ABSENCE half. Asserting that a declared 25.0 m range arrives is
# easy and a broken implementation would pass it too; what distinguishes a correct adapter is that
# an attribute the manifest never declared is OMITTED on the wire rather than sent as 0. A control
# plane records an omitted attribute as unknown, but a 0 as a real reading — a 0 m sensing range or
# a 0 kg payload ceiling that a scheduler will act on.

from __future__ import annotations

import pathlib
import sys

import pytest

REPO = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO))
sys.path.insert(0, str(REPO / "proto"))

from adapters.scenarios import load_scenario  # noqa: E402
from adapters.scenarios.loader import ScenarioEngine  # noqa: E402
from adapters.simulation.sim_adapter import _HW_ATTRS, _hw_component  # noqa: E402

pb = pytest.importorskip("fleet_adapter.v1.fleet_adapter_pb2")


def _by_name(components):
    return {c.name: c for c in components}


def _emit(scenario_name: str, t: float = 0.0):
    """Run a preset through the engine and the sim's component builder — the real path."""
    eng = ScenarioEngine(load_scenario(scenario_name))
    return _by_name([
        _hw_component(pb, hs, pb.HARDWARE_STATUS_HEALTHY) for hs in eng.hardware_at(t)
    ])


# ── the manifest -> engine hop ────────────────────────────────────────────────

def test_manifest_attrs_parse_with_absence_preserved():
    sc = load_scenario("full-surface")
    hw = {h.name: h for h in sc.hardware}
    lidar, cam = hw["lidar_top"], hw["camera_front"]

    assert lidar.range_m == 25.0
    assert lidar.horizontal_fov_deg == 360.0
    # Not meaningful for a lidar and not declared -> stays None, never 0.
    assert lidar.resolution_mp is None
    assert lidar.frame_rate_fps is None
    assert lidar.depth_capable is None

    assert cam.resolution_mp == 12.0
    assert cam.frame_rate_fps == 30.0
    # A DECLARED false is data and must not be confused with "not declared".
    assert cam.depth_capable is False
    assert cam.range_m is None


def test_engine_carries_attrs_through_a_fault():
    """A fault changes status/reason only. The physical attributes must survive it — a degraded
    lidar has the same range it always had."""
    eng = ScenarioEngine(load_scenario("full-surface"))
    before = _by_name(eng.hardware_at(0.0))["camera_front"]
    after = _by_name(eng.hardware_at(13.0))["camera_front"]  # past the T+12 FAILED beat

    assert after.status != before.status, "precondition: the fault should have fired by T+13"
    assert after.resolution_mp == 12.0
    assert after.frame_rate_fps == 30.0
    assert after.depth_capable is False


# ── the engine -> wire hop ────────────────────────────────────────────────────

def test_declared_attrs_are_set_on_the_wire():
    comps = _emit("full-surface")
    lidar, cam = comps["lidar_top"], comps["camera_front"]

    assert lidar.HasField("range_m") and lidar.range_m == 25.0
    assert lidar.HasField("horizontal_fov_deg") and lidar.horizontal_fov_deg == 360.0
    assert cam.HasField("resolution_mp") and cam.resolution_mp == 12.0
    assert cam.HasField("frame_rate_fps") and cam.frame_rate_fps == 30.0


def test_undeclared_attrs_are_absent_on_the_wire():
    """THE point of the change. An attribute the manifest never declared must not appear at all."""
    comps = _emit("full-surface")
    lidar = comps["lidar_top"]

    for f in ("resolution_mp", "frame_rate_fps", "depth_capable", "max_payload_kg",
              "platform_length_mm", "platform_width_mm"):
        assert not lidar.HasField(f), (
            f"lidar_top sent {f} that the manifest never declared; an unknown attribute must be "
            f"OMITTED, never sent as {getattr(lidar, f)!r}"
        )


def test_declared_false_is_sent_not_omitted():
    """A declared false is a measurement. Omitting it would lose the fact that this camera was
    checked and found not depth-capable."""
    cam = _emit("full-surface")["camera_front"]
    assert cam.HasField("depth_capable"), "a declared depthCapable:false must be SENT"
    assert cam.depth_capable is False


def test_a_manifest_declaring_nothing_sends_nothing():
    """A preset with a bare hardware manifest emits components with no attributes at all — the
    pre-change behaviour must remain reachable, and must not become zeros."""
    comps = _emit("healthy-fleet")
    assert comps, "precondition: healthy-fleet declares some hardware"
    for name, c in comps.items():
        for f in _HW_ATTRS:
            assert not c.HasField(f), f"{name} sent {f} without the manifest declaring it"


def test_identity_and_health_fields_still_travel():
    """The attributes are additive — the identity/health subset must be untouched."""
    lidar = _emit("full-surface")["lidar_top"]
    assert lidar.name == "lidar_top"
    assert lidar.type == "Lidar"
    assert lidar.model == "sim-lidar-v1"
    assert lidar.status == pb.HARDWARE_STATUS_HEALTHY
