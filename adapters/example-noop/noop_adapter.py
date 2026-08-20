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

"""Reference no-op Fleet Adapter.

Demonstrates the ``fleet_adapter.v1`` stream topology and the safety-critical
behaviors required by ``adapters/CONFORMANCE.md`` with no real hardware behind
it. Every ``TODO(vendor)`` marks a point where a real adapter calls into a
manufacturer's fleet API. This file is meant to be read and copied, not
deployed.

Requires the generated gRPC stubs on the Python path (``make proto``):
``fleet_adapter.v1.fleet_adapter_pb2`` and ``...fleet_adapter_pb2_grpc``.
"""

from __future__ import annotations

import argparse
import queue
import sys
import threading
import time
from collections.abc import Iterator

import grpc

# The import path matches the proto package ``fleet_adapter.v1``. How the
# generated package is laid out on disk depends on ``make proto``; adjust
# PYTHONPATH accordingly. Imported lazily in main() so the module can be read
# and syntax-checked without the generated code present.


# The fleet-adapter CONTRACT version this adapter implements (ADR-0032). Distinct from
# adapter_version (this build) and protocol_version (the wire package identity, not a semver, which
# is why it cannot express compatibility). A control plane refuses REGISTRATION for an adapter that
# reports nothing or reports out of range, so this is not optional -- telemetry, heartbeat and
# emergency stop keep working either way. Bump it only when this adapter is re-qualified against a
# new contract (`make conformance`), and update the adapters/REGISTRY.md row with it.
CONTRACT_VERSION = "1.0.0"


class NoopAdapter:
    """Speaks the full protocol, backed by canned safe responses."""

    def __init__(self, pb, pb_grpc, vendor: str, namespace: str) -> None:
        self._pb = pb
        self._pb_grpc = pb_grpc
        self._vendor = vendor
        self._namespace = namespace
        # Outbound ControlStream messages are enqueued here and drained by the
        # request generator, so any thread can push a reply.
        self._outbox: queue.Queue = queue.Queue()
        # C3.1: highest accepted fencing token per robot MUST be persisted in a
        # real adapter. In-memory here for illustration only.
        self._fence: dict[str, int] = {}
        # C3.4: the assignment_id accepted at the current token, so an identical
        # re-delivery can be told apart from a genuinely stale assignment.
        self._assignment: dict[str, str] = {}
        # C16.1 / C4.2: the task currently held per robot, and its lease timer. A real
        # adapter tracks these against the robot's actual execution state.
        self._action: dict[str, str] = {}
        self._lease_gen: dict[str, int] = {}
        self._lease_timer: dict[str, threading.Timer] = {}
        self._lease_epoch: dict[str, int] = {}

    # ── ControlStream ────────────────────────────────────────────────────────

    def _control_requests(self) -> Iterator:
        """Yield outbound AdapterMessages. C1.2: AdapterHello goes first."""
        pb = self._pb
        yield pb.AdapterMessage(
            seq=0,
            hello=pb.AdapterHello(
                vendor=self._vendor,
                adapter_version="0.0.0-noop",
                protocol_version="fleet_adapter.v1",
                contract_version=CONTRACT_VERSION,
                namespace=self._namespace,
            ),
        )
        while True:
            msg = self._outbox.get()
            if msg is None:  # shutdown sentinel
                return
            yield msg

    def run_control_stream(self, stub) -> None:
        # A transport drop is a normal event, not a crash: the control plane may restart, a
        # port-forward may blip, or a conformance harness may fault the stream on purpose. Exit
        # quietly with a one-line reason rather than a traceback — an example adapter's failure
        # output is read as guidance, so a stack trace here teaches the wrong shape.
        #
        # TODO(vendor): reconnect with bounded exponential backoff and jitter instead of exiting
        # (C1.5, SHOULD), re-sending AdapterHello and re-registering each robot on the new stream.
        # This skeleton exits so the control flow stays readable; a production adapter must not.
        try:
            for cp in stub.ControlStream(self._control_requests()):
                self._handle_control_plane_message(cp)
        except grpc.RpcError as exc:
            print(f"control stream ended ({exc.code().name}): {exc.details()}", file=sys.stderr)

    def _handle_control_plane_message(self, cp) -> None:
        pb = self._pb
        kind = cp.WhichOneof("payload")
        if kind == "hello_ack":
            if not cp.hello_ack.accepted:
                raise RuntimeError(f"handshake rejected: {cp.hello_ack.message}")
        elif kind == "heartbeat":
            # C-heartbeat: reply so the control plane sees the adapter as live.
            self._outbox.put(
                pb.AdapterMessage(
                    heartbeat=pb.HeartbeatResponse(robot_id=cp.heartbeat.robot_id)
                )
            )
        elif kind == "command":
            self._handle_command(cp.command)
        # register_ack / discover_ack / resource_response: reconcile local state
        # (C2.2). Omitted in the no-op.

    def _handle_command(self, command) -> None:
        pb = self._pb
        result = pb.CommandResult(command_id=command.command_id, robot_id=command.robot_id)
        which = command.WhichOneof("command")
        robot = command.robot_id

        if which == "assign_action":
            task = command.assign_action
            # C3.3: absent token -> MISSING; C3.4: identical re-delivery of the
            # current assignment -> idempotent re-ack; C3.2: stale token -> STALE.
            if not task.HasField("fencing_token"):
                result.assign_action.CopyFrom(
                    pb.AssignActionResult(
                        accepted=False,
                        rejection=pb.ASSIGN_ACTION_REJECTION_MISSING_FENCING_TOKEN,
                    )
                )
            elif (
                task.fencing_token == self._fence.get(robot, 0)
                and task.assignment_id == self._assignment.get(robot)
            ):
                # C3.4: the current assignment, re-delivered unchanged. Re-ack it
                # idempotently instead of treating it as stale.
                result.assign_action.CopyFrom(
                    pb.AssignActionResult(
                        accepted=True, accepted_fencing_token=task.fencing_token
                    )
                )
            elif task.fencing_token <= self._fence.get(robot, 0):
                result.assign_action.CopyFrom(
                    pb.AssignActionResult(
                        accepted=False,
                        rejection=pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN,
                    )
                )
            else:
                self._fence[robot] = task.fencing_token  # C3.1 (persist for real)
                self._assignment[robot] = task.assignment_id  # C3.4 identity
                self._action[robot] = task.action_id
                # TODO(vendor): dispatch the task to the robot here.
                result.assign_action.CopyFrom(
                    pb.AssignActionResult(
                        accepted=True, accepted_fencing_token=task.fencing_token
                    )
                )
                # C16.1 — REQUIRED: report every task phase transition. The control
                # plane drives FleetAction Assigned -> InProgress off this message, so
                # an adapter that stays silent leaves its tasks resolvable only by
                # lease expiry.
                self._emit_action_status(robot, task.action_id, "RUNNING")
                # C4.2 — REQUIRED: arm the self-stop. If no renew_lease arrives within
                # lease_duration_ms the task MUST be brought to a safe stop and the stop
                # MUST be reported. This is the primary dual-execution safeguard.
                if task.lease_duration_ms:
                    self._arm_lease(robot, task.action_id,
                                    task.lease_generation, task.lease_duration_ms)
        elif which == "cancel_action":
            # TODO(vendor): cancel on the robot.
            self._cancel_lease(robot)
            self._emit_action_status(robot, self._action.get(robot, ""), "CANCELLED")
            # C4.4: the action is no longer executing — clear it the same way
            # _lease_expired does, so a renew_lease after this point correctly
            # reports running=false instead of a silent (undetectable) completion.
            self._action[robot] = ""
            # A canned safe stop. On a capability-loss cancel a real adapter chooses
            # the disposition from the robot's physical state (STOPPED_SAFELY /
            # COMPLETED / RECOVERED); the no-op adapter always stops safely.
            result.cancel_action.CopyFrom(pb.CancelActionResult(
                acknowledged=True,
                disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY))
        elif which == "renew_lease":
            # C4.3/C4.4 — REQUIRED. A matching generation refreshes the self-stop timer;
            # a stale one is refused, or a superseded lease could be revived and two
            # robots could hold the same task.
            renew = command.renew_lease
            ok = self._renew_lease(robot, renew.lease_generation, renew.lease_duration_ms)
            result.renew_lease.CopyFrom(pb.RenewActionLeaseResult(
                renewed=ok, running=self._action.get(robot, "") == renew.action_id))
        elif which == "scan":
            # C12.1 — REQUIRED (RFC-0001 Required-message table): answer with a full
            # (non-delta) CapabilitiesSnapshot, never decline via unsupported=true.
            # TODO(vendor): populate hardware[]/installed_models[]/supported_actions[]
            # from the robot's real inventory. The no-op adapter has none to report, and
            # an empty list here is a true statement ("this robot has no hardware"), not
            # a withheld one — see ADR-0039 if a future revision needs to distinguish
            # "no hardware" from "not yet observed".
            result.scan.CopyFrom(pb.CapabilitiesSnapshot(
                robot_id=robot, snapshot_ms=int(time.time() * 1000)))
        else:
            # C7.1: decline every optional command we do not implement.
            result.unsupported = True

        self._outbox.put(pb.AdapterMessage(command_result=result))

    # ── Task status and the assignment lease (C16.1 / C4.2-C4.4) ─────────────

    def _emit_action_status(self, robot: str, action_id: str, state: str) -> None:
        """Report a task phase transition. REQUIRED at every transition, terminal ones
        included: silence is indistinguishable from a robot that stopped reporting."""
        if not action_id:
            return
        pb = self._pb
        self._outbox.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=action_id,
            state=getattr(pb.ActionState, f"ACTION_STATE_{state}"),
            fencing_token=self._fence.get(robot, 0),
        )))

    def _arm_lease(self, robot: str, action_id: str, generation: int, duration_ms: int) -> None:
        """Start (or restart) the self-stop timer for robot's current assignment.

        Each arming carries an EPOCH. Timer.cancel() is best-effort — a timer already
        past its deadline still runs — and every renewal of the same task shares an
        action_id, so an action-id guard alone lets a superseded timer stop live work.
        The epoch is what makes a stale firing identifiable.
        """
        self._cancel_lease(robot)
        self._lease_gen[robot] = generation
        epoch = self._lease_epoch.get(robot, 0) + 1
        self._lease_epoch[robot] = epoch
        t = threading.Timer(duration_ms / 1000.0, self._lease_expired,
                            args=(robot, action_id, epoch))
        t.daemon = True
        self._lease_timer[robot] = t
        t.start()

    def _cancel_lease(self, robot: str) -> None:
        t = self._lease_timer.pop(robot, None)
        if t is not None:
            t.cancel()

    def _renew_lease(self, robot: str, generation: int, duration_ms: int) -> bool:
        """A matching generation refreshes the timer; a stale one is refused."""
        if self._lease_gen.get(robot) != generation:
            return False  # C4.4: never revive a superseded lease
        self._arm_lease(robot, self._action.get(robot, ""), generation, duration_ms)
        return True

    def _lease_expired(self, robot: str, action_id: str, epoch: int) -> None:
        """The lease lapsed without renewal. TODO(vendor): bring the robot to a safe
        stop here. The control plane may now reassign this task, so continuing to
        execute it would put two robots on the same work."""
        if self._lease_epoch.get(robot) != epoch:
            return  # a renewal superseded this timer before it fired
        if self._action.get(robot) != action_id:
            return  # superseded; the timer is stale
        self._action[robot] = ""
        self._emit_action_status(robot, action_id, "STOPPED")

    # ── SafetyStream ─────────────────────────────────────────────────────────

    def run_safety_stream(self, stub) -> None:
        """C5: confirmed estop on a physically separate stream."""
        safety_outbox: queue.Queue = queue.Queue()

        def requests() -> Iterator:
            while True:
                msg = safety_outbox.get()
                if msg is None:
                    return
                yield msg

        try:
            self._safety_loop(stub, requests, safety_outbox)
        except grpc.RpcError as exc:
            # LOUD on purpose. Losing the control stream costs work; losing the SAFETY stream costs
            # the ability to stop the robot, so it must never look like an ordinary shutdown.
            # TODO(vendor): reconnect immediately and keep retrying — estop must be reachable
            # whenever the robot is powered, independent of the control stream (C1.1, C5).
            print(f"SAFETY STREAM LOST ({exc.code().name}): {exc.details()} — estop is "
                  "UNREACHABLE until it is re-established", file=sys.stderr)

    def _safety_loop(self, stub, requests, safety_outbox) -> None:
        pb = self._pb
        for cp in stub.SafetyStream(requests()):
            if cp.WhichOneof("payload") == "estop":
                # TODO(vendor): command a physical safe stop and WAIT for it.
                # Stamp the command moment BEFORE the wait: stop_initiated_at reports
                # when the stop was issued, which is the evidence that it was confirmed
                # rather than inferred (C5.3).
                initiated_ms = int(time.time() * 1000)
                stopped = True  # a real adapter confirms this; never infers it.
                safety_outbox.put(
                    pb.AdapterSafetyMessage(
                        robot_id=cp.robot_id,
                        estop_ack=pb.EstopAck(
                            estop_id=cp.estop.estop_id,
                            # C5.3: the moment the hardware stop was COMMANDED. An
                            # adapter that inferred the stop has no such moment.
                            stop_initiated_at=initiated_ms,
                            # C5.2 / C5.3: STOPPED only when confirmed halted.
                            state=(
                                pb.ESTOP_STATE_STOPPED
                                if stopped
                                else pb.ESTOP_STATE_FAILED
                            ),
                        ),
                    )
                )


def main() -> None:
    parser = argparse.ArgumentParser(description="Reference no-op Fleet Adapter")
    parser.add_argument("--endpoint", default="localhost:9090")
    parser.add_argument("--vendor", default="noop")
    parser.add_argument("--namespace", default="default")
    args = parser.parse_args()

    # Imported here so the file syntax-checks without the generated stubs.
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb
    from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc

    # TODO(vendor): use grpc.secure_channel with mTLS credentials (C1.3).
    channel = grpc.insecure_channel(args.endpoint)
    stub = pb_grpc.FleetAdapterServiceStub(channel)

    adapter = NoopAdapter(pb, pb_grpc, args.vendor, args.namespace)
    # C1.1: both streams are opened together.
    safety = threading.Thread(target=adapter.run_safety_stream, args=(stub,), daemon=True)
    safety.start()
    adapter.run_control_stream(stub)


if __name__ == "__main__":
    main()
