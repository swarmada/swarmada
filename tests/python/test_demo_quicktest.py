# Copyright 2026 The Swarmada Authors.
#

"""Quicktest for the Demo B fault-recovery demo modules.

Fast, no-Isaac-Sim, no-gRPC checks a non-author (or CI) can run in well under a
second to confirm the demo path is wired end to end: the warehouse scenario
spins its fleet up and tears it down cleanly, and the camera-fault injector
round-trips inject → clear against an isolated fault directory — never the real
/tmp.

    pytest tests/python/test_demo_quicktest.py -q
"""

from __future__ import annotations

import pytest

from simulation.fault_injection import camera_fault
from simulation.scenarios.warehouse_demo import WarehouseScenario


# ── warehouse scenario lifecycle ────────────────────────────────────────────────

def test_setup_spawns_requested_fleet() -> None:
    scenario = WarehouseScenario(robot_count=4, zone="warehouse-a")
    scenario.setup()
    assert [r.robot_id for r in scenario.robots] == [
        "sim-robot-001",
        "sim-robot-002",
        "sim-robot-003",
        "sim-robot-004",
    ]


def test_run_honours_duration_and_tears_down() -> None:
    # A bounded run must exit on the duration and leave the scenario stopped,
    # all without an Isaac Sim stage attached.
    scenario = WarehouseScenario(robot_count=2)
    scenario.setup()
    scenario.run(duration_s=0.05)
    assert scenario._running is False


# ── camera-fault injector round-trip (isolated dir) ─────────────────────────────

@pytest.fixture()
def isolated_fault_dir(tmp_path, monkeypatch):
    """Point the injector at a temp dir so tests never touch the real /tmp."""
    monkeypatch.setattr(camera_fault, "FAULT_DIR", tmp_path)
    return tmp_path


def test_inject_creates_sentinel(isolated_fault_dir) -> None:
    camera_fault.inject("sim-robot-002", "camera_front")
    assert camera_fault.fault_path("sim-robot-002", "camera_front").exists()


def test_clear_removes_sentinel(isolated_fault_dir) -> None:
    camera_fault.inject("sim-robot-002", "camera_front")
    camera_fault.clear("sim-robot-002", "camera_front")
    assert not camera_fault.fault_path("sim-robot-002", "camera_front").exists()


def test_clear_missing_sentinel_is_safe(isolated_fault_dir) -> None:
    # Clearing with nothing injected must be a no-op, not an error.
    camera_fault.clear("sim-robot-999", "lidar_top")


def test_cli_inject_then_clear(isolated_fault_dir) -> None:
    p = camera_fault.fault_path("sim-robot-002", "camera_front")
    assert camera_fault.main(["--robot", "sim-robot-002", "--component", "camera_front"]) == 0
    assert p.exists()
    assert (
        camera_fault.main(
            ["--robot", "sim-robot-002", "--component", "camera_front", "--clear"]
        )
        == 0
    )
    assert not p.exists()
