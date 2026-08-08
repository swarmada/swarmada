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

"""
camera_fault.py — Mark/clear a hardware-fault sentinel for the MANUAL workflow.

Writes/clears a /tmp/swarmada_fault_{robot_id}_{component} sentinel file that marks
an injected fault for the manual / status-projection workflow (pair it with a
`kubectl patch` of Robot.status — see docs/adapter-use-cases.md §3, Way 2). NO
adapter reads this file today — the adapter-driven fault path is
`--scenario hardware-fault` (Way 1). To wire the sentinel in, repoint it into a
live-override channel the adapter watches.

CLI usage:
    python -m simulation.fault_injection.camera_fault \\
        --robot sim-robot-002 --component camera_front

    python -m simulation.fault_injection.camera_fault \\
        --robot sim-robot-002 --component camera_front --clear
"""

from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path

logger = logging.getLogger(__name__)

FAULT_DIR = Path("/tmp")


def fault_path(robot_id: str, component: str) -> Path:
    return FAULT_DIR / f"swarmada_fault_{robot_id}_{component}"


def inject(robot_id: str, component: str) -> None:
    """Create the sentinel file that signals a hardware fault."""
    p = fault_path(robot_id, component)
    p.touch()
    logger.info("Fault injected: %s → %s", component, p)
    print(f"✓  Fault injected on {robot_id}/{component}")
    print(f"   Sentinel file: {p}")
    print("   Watch the Robot status with:")
    print(f"   kubectl get robot {robot_id} -w -o jsonpath='{{.status.hardware}}'")


def clear(robot_id: str, component: str) -> None:
    """Remove the sentinel file to recover the hardware fault."""
    p = fault_path(robot_id, component)
    if p.exists():
        p.unlink()
        logger.info("Fault cleared: %s → %s", component, p)
        print(f"✓  Fault cleared on {robot_id}/{component}")
    else:
        print(f"   No fault sentinel found at {p} (already cleared?)")


def list_faults(robot_id: str | None = None) -> None:
    """Print all active fault sentinels, optionally filtered by robot."""
    prefix = f"swarmada_fault_{robot_id}_" if robot_id else "swarmada_fault_"
    found = sorted(FAULT_DIR.glob(f"{prefix}*"))
    if not found:
        print("No active faults.")
        return
    print(f"Active faults ({len(found)}):")
    for p in found:
        parts = p.name.removeprefix("swarmada_fault_").rsplit("_", 1)
        print(f"  robot={parts[0]}  component={parts[1] if len(parts) > 1 else '?'}")


# ── CLI entry point ────────────────────────────────────────────────────────────

def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="Inject or clear hardware faults on Swarmada simulated robots."
    )
    p.add_argument("--robot", required=True, help="Robot ID (e.g. sim-robot-002)")
    p.add_argument("--component", required=True, help="Component name (e.g. camera_front)")
    p.add_argument("--clear", action="store_true", help="Clear the fault instead of injecting it")
    p.add_argument("--list", action="store_true", help="List all active faults for --robot")
    return p


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    args = _build_parser().parse_args(argv)

    if args.list:
        list_faults(args.robot)
        return 0

    if args.clear:
        clear(args.robot, args.component)
    else:
        inject(args.robot, args.component)
    return 0


if __name__ == "__main__":
    sys.exit(main())
