# Copyright {{cookiecutter.year}} {{cookiecutter.author}}
# SPDX-License-Identifier: Apache-2.0

"""{{cookiecutter.adapter_name}} — a safety-complete Swarmada Fleet Adapter.

Feature-basic but safety-complete (ADR-0005): the CONFORMANCE.md safety MUSTs are
wired from the audited `swarmada_sdk` primitives; optional commands are declined
with `unsupported = true` (C7). Bind `RobotBinding` (see robot.py) to your fleet
API — every `TODO(vendor)` marks a point where you do so.

Run:  python -m {{cookiecutter.python_package}}.adapter --endpoint localhost:9090
"""

from __future__ import annotations

import argparse
import pathlib
import queue
import sys
import threading
import time
from collections.abc import Iterator

import grpc

# Make the package importable when run directly as a script (a real install
# `pip install -e .` makes this unnecessary).
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from swarmada_sdk.safety import (  # noqa: E402
    ESTOP_STOPPED,
    FenceDecision,
    FenceGuard,
    LeaseMonitor,
    confirm_estop,
)

from {{cookiecutter.python_package}}.robot import SimulatedRobot  # noqa: E402

_TELEMETRY_INTERVAL = 0.2
_TICK_DT = 0.2


# The fleet-adapter CONTRACT version this adapter implements (ADR-0032). Distinct from
# adapter_version (this build) and protocol_version (the wire package identity, not a semver, which
# is why it cannot express compatibility). A control plane refuses REGISTRATION for an adapter that
# reports nothing or reports out of range, so this is not optional -- telemetry, heartbeat and
# emergency stop keep working either way. Bump it only when this adapter is re-qualified against a
# new contract (`make conformance`), and update the adapters/REGISTRY.md row with it.
CONTRACT_VERSION = "1.0.0"


class {{cookiecutter.python_package}}Adapter:  # noqa: N801  (generated class name)
    def __init__(self, pb, pb_grpc, robot, robot_id: str) -> None:
        self._pb = pb
        self._pb_grpc = pb_grpc
        self._robot = robot
        self._robot_lock = threading.Lock()
        self._robot_id = robot_id
        self._fence = FenceGuard()                              # C3
        self._lease = LeaseMonitor(on_expiry=self._self_stop)   # C4
        self._current_action = ""
        self._current_generation = 0
        self._outbox: queue.Queue = queue.Queue()
        self._shutdown = threading.Event()
        self._telemetry_started = False

    # ── ControlStream ────────────────────────────────────────────────────────

    def _control_requests(self) -> Iterator:
        pb = self._pb
        yield pb.AdapterMessage(hello=pb.AdapterHello(  # C1.2
            vendor="{{cookiecutter.vendor}}", adapter_version="0.1.0",
            protocol_version="fleet_adapter.v1", contract_version=CONTRACT_VERSION,
            namespace="default"))
        while True:
            msg = self._outbox.get()
            if msg is None:
                return
            yield msg

    def run_control_stream(self, stub) -> None:
        with self._robot_lock:
            self._robot.spawn(self._robot_id)
        try:
            for cp in stub.ControlStream(self._control_requests()):
                self._handle_control_plane(cp, stub)
        finally:
            self._shutdown.set()

    def _handle_control_plane(self, cp, stub) -> None:
        pb = self._pb
        kind = cp.WhichOneof("payload")
        if kind == "hello_ack":
            if not cp.hello_ack.accepted:
                raise RuntimeError(f"handshake rejected: {cp.hello_ack.message}")
            self._outbox.put(pb.AdapterMessage(  # C2.1: register
                register=pb.RegisterRobot(
                    robot_id=self._robot_id,
                    reported_hardware=self._reported_hardware())))
            if not self._telemetry_started:  # C6: stream telemetry once registered
                self._telemetry_started = True
                threading.Thread(target=self._telemetry_loop, args=(stub,), daemon=True).start()
        elif kind == "heartbeat":
            self._outbox.put(pb.AdapterMessage(
                heartbeat=pb.HeartbeatResponse(robot_id=cp.heartbeat.robot_id)))
        elif kind == "command":
            self._handle_command(cp.command)

    def _reported_hardware(self):
        """The robot's hardware inventory, sent once at registration.

        TODO(vendor): replace this example with your robot's real components. Report the physical
        attributes you can actually read from the hardware, and OMIT the rest — do not substitute
        0 or False for a value you do not know. The fields are proto3 `optional` precisely so
        "unknown" stays distinguishable from "measured zero": the control plane records an omitted
        attribute as unknown, while a 0 it treats as a real reading (a 0 kg payload ceiling, a 0 m
        sensing range) and schedules on it.

        Available attributes: max_payload_kg, resolution_mp, range_m, horizontal_fov_deg,
        depth_capable, frame_rate_fps, platform_length_mm, platform_width_mm.
        """
        pb = self._pb
        return [
            # A lidar: range and field of view are known; camera-only attributes are omitted.
            pb.HardwareComponent(
                name="lidar_front", type="Lidar", model="TODO-vendor-model",
                status=pb.HARDWARE_STATUS_HEALTHY,
                range_m=25.0, horizontal_fov_deg=360.0,
            ),
        ]

    def _handle_command(self, command) -> None:
        pb = self._pb
        result = pb.CommandResult(command_id=command.command_id, robot_id=command.robot_id)
        which = command.WhichOneof("command")
        if which == "assign_action":
            self._on_assign(command.assign_action, result)
        elif which == "renew_lease":
            self._on_renew(command.renew_lease, result)
        elif which == "cancel_action":
            with self._robot_lock:
                self._robot.command_stop(self._robot_id)
            self._lease.release(self._robot_id)
            self._current_action = ""
            # On a capability-loss cancel, set disposition from the robot's state:
            # STOPPED_SAFELY (safe stop → task requeued), COMPLETED (finished; cancel
            # moot), or RECOVERED (mid-commitment, could not hand off → task Failed).
            result.cancel_action.CopyFrom(pb.CancelActionResult(
                acknowledged=True,
                disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY))
        else:
            # C7.1: decline every optional command not implemented.
            # TODO(vendor): implement any optional commands your robots support.
            result.unsupported = True
        self._outbox.put(pb.AdapterMessage(command_result=result))

    def _on_assign(self, task, result) -> None:
        pb = self._pb
        decision = self._fence.check(
            self._robot_id, task.HasField("fencing_token"), task.fencing_token, task.assignment_id)
        if decision is FenceDecision.MISSING:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_MISSING_FENCING_TOKEN))
            return
        if decision is FenceDecision.STALE:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN))
            return
        with self._robot_lock:
            self._robot.command_move(self._robot_id, 10.0, 0.0)  # TODO(vendor): real destination
        self._current_action = task.action_id
        if task.lease_duration_ms:
            self._current_generation = task.lease_generation
            self._lease.grant(self._robot_id, task.lease_duration_ms / 1000.0, task.lease_generation)
        result.assign_action.CopyFrom(pb.AssignActionResult(
            accepted=True, accepted_fencing_token=task.fencing_token))  # C3.5

    def _on_renew(self, renew, result) -> None:
        pb = self._pb
        renewed = self._lease.renew(
            self._robot_id, renew.lease_duration_ms / 1000.0, renew.lease_generation)  # C4.3
        if renewed:
            self._current_generation = renew.lease_generation
        running = self._current_action == renew.action_id and self._current_action != ""  # C4.4
        result.renew_lease.CopyFrom(pb.RenewActionLeaseResult(
            renewed=renewed, running=running, current_generation=self._current_generation))

    def _self_stop(self, robot_id: str) -> None:
        with self._robot_lock:  # C4.2
            self._robot.command_stop(robot_id)
        self._current_action = ""

    def _telemetry_loop(self, stub) -> None:
        pb = self._pb
        while not self._shutdown.is_set():
            self._lease.tick()  # C4 self-stop check
            with self._robot_lock:
                self._robot.tick(_TICK_DT)
                x, y, floor = self._robot.pose(self._robot_id)
                battery = self._robot.battery_percent(self._robot_id)
            phase = (pb.RobotPhase.ROBOT_PHASE_IN_PROGRESS if self._current_action
                     else pb.RobotPhase.ROBOT_PHASE_IDLE)
            self._outbox.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(  # C6
                robot_id=self._robot_id, timestamp_ms=int(time.time() * 1000),
                position=pb.RobotPosition(x=x, y=y, floor=floor),  # C6.2 explicit presence
                battery=pb.BatteryStatus(percent=battery), phase=phase,
                current_action=self._current_action)))
            time.sleep(_TELEMETRY_INTERVAL)

    # ── SafetyStream (C5) ────────────────────────────────────────────────────

    def run_safety_stream(self, stub) -> None:
        pb = self._pb
        outbox: queue.Queue = queue.Queue()

        def requests() -> Iterator:
            while True:
                msg = outbox.get()
                if msg is None:
                    return
                yield msg

        for cp in stub.SafetyStream(requests()):
            if cp.WhichOneof("payload") == "estop":
                with self._robot_lock:
                    state = confirm_estop(self._robot, self._robot_id)  # C5: confirmed, never inferred
                outbox.put(pb.AdapterSafetyMessage(
                    robot_id=cp.robot_id, estop_ack=pb.EstopAck(
                        estop_id=cp.estop.estop_id,
                        state=(pb.ESTOP_STATE_STOPPED if state == ESTOP_STOPPED
                               else pb.ESTOP_STATE_FAILED))))


def main() -> None:
    ap = argparse.ArgumentParser(description="{{cookiecutter.adapter_name}}")
    ap.add_argument("--endpoint", default="localhost:9090")
    ap.add_argument("--robot-id", default="robot-conformance-1")
    args = ap.parse_args()

    from fleet_adapter.v1 import fleet_adapter_pb2 as pb
    from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc

    robot = SimulatedRobot()  # TODO(vendor): replace with your RobotBinding
    adapter = {{cookiecutter.python_package}}Adapter(pb, pb_grpc, robot, args.robot_id)

    # TODO(vendor): use grpc.secure_channel with mTLS credentials (C1.3).
    channel = grpc.insecure_channel(args.endpoint)
    stub = pb_grpc.FleetAdapterServiceStub(channel)
    threading.Thread(target=adapter.run_safety_stream, args=(stub,), daemon=True).start()  # C1.1
    adapter.run_control_stream(stub)


if __name__ == "__main__":
    main()
