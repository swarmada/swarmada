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
warehouse_demo.py — Isaac Sim warehouse scenario for Swarmada Demo A.

Spawns a configurable fleet of simulated robots in a warehouse layout,
applies a Swarmada KUBECONFIG, and watches them accept FleetActions generated
by the Kubernetes Job in config/samples/demo-a-tasks.yaml.

Usage (run from the Isaac Sim Python interpreter):
    from simulation.scenarios.warehouse_demo import WarehouseScenario
    scenario = WarehouseScenario(robot_count=4, zone="warehouse-a")
    scenario.setup()
    scenario.run()
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field

logger = logging.getLogger(__name__)


@dataclass
class RobotSpawnConfig:
    """Spawn parameters for a single simulated robot."""

    robot_id: str
    x: float
    y: float
    yaw: float = 0.0
    manufacturer: str = "SimBot"
    model: str = "SimBot-250"
    battery_percent: int = 90


@dataclass
class WarehouseScenario:
    """
    Top-level scenario orchestrator.

    Responsibilities:
    - Open the Isaac Sim warehouse USD stage.
    - Spawn robot prims at the configured positions.
    - Start a Fleet Adapter gRPC server for each robot.
    - Optionally apply Kubernetes manifests to trigger scheduling.
    """

    robot_count: int = 4
    zone: str = "warehouse-a"
    stage_usd: str = "omniverse://localhost/Swarmada/Warehouse.usd"
    fleet_adapter_port_base: int = 9090

    robots: list[RobotSpawnConfig] = field(default_factory=list)
    _running: bool = field(default=False, init=False)

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def setup(self) -> None:
        """Initialise Isaac Sim stage and spawn robots."""
        self._generate_spawn_configs()
        self._open_stage()
        for cfg in self.robots:
            self._spawn_robot(cfg)
        logger.info("Scenario ready: %d robots in zone '%s'", len(self.robots), self.zone)

    def run(self, duration_s: float | None = None) -> None:
        """
        Start the simulation loop.

        Args:
            duration_s: Run for this many seconds, then stop.  None = run
                        until KeyboardInterrupt.
        """
        self._running = True
        start = time.monotonic()
        try:
            while self._running:
                self._step()
                if duration_s and (time.monotonic() - start) >= duration_s:
                    break
        except KeyboardInterrupt:
            logger.info("Scenario interrupted by user")
        finally:
            self.teardown()

    def teardown(self) -> None:
        """Stop Fleet Adapters and close the Isaac Sim stage."""
        self._running = False
        logger.info("Scenario teardown complete")

    # ------------------------------------------------------------------
    # Private helpers
    # ------------------------------------------------------------------

    def _generate_spawn_configs(self) -> None:
        """Place robots in a grid along the east wall of the warehouse."""
        for i in range(self.robot_count):
            self.robots.append(
                RobotSpawnConfig(
                    robot_id=f"sim-robot-{i+1:03d}",
                    x=2.0 + i * 3.0,
                    y=1.0,
                    yaw=1.5708,  # facing north
                )
            )

    def _open_stage(self) -> None:
        """
        Open the warehouse USD stage in Isaac Sim.

        In a real run this calls omni.usd.get_context().open_stage().
        Stubbed here so the module loads without Isaac Sim installed.
        """
        logger.info("Opening stage: %s", self.stage_usd)
        # omni.usd.get_context().open_stage(self.stage_usd)

    def _spawn_robot(self, cfg: RobotSpawnConfig) -> None:
        """Add a robot prim to the stage at (cfg.x, cfg.y, cfg.yaw)."""
        logger.info(
            "Spawning %s at (%.1f, %.1f, yaw=%.2f)",
            cfg.robot_id, cfg.x, cfg.y, cfg.yaw,
        )
        # prim_path = f"/World/Robots/{cfg.robot_id}"
        # omni.kit.commands.execute("CreateReferenceCommand", ...)

    def _step(self) -> None:
        """Advance the simulation by one physics step (~20 ms)."""
        # simulation_app.update()
        time.sleep(0.02)
