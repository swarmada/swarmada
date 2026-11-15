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

"""The single-robot simulation adapter's wire traffic, pinned byte for byte.

The adapter serves several robots on one ControlStream when ``--robot-id`` is given more
than once. With ONE ``--robot-id`` -- every caller in this repository and in the scenarios
repository today: the ks pools, the examples, ``make conformance-sim`` -- it must say exactly
what it said before multi-robot support existed. This test is that guarantee. Its golden
files were recorded from the adapter at 2b099f7, before any multi-robot change, and every
later change must reproduce them.

What it drives, through the adapter's real stream entry points (``run_control_stream``,
``_telemetry_loop``, ``run_safety_stream``) against a scripted stub:

* two ControlStream sessions: the first registers through the provisioning loop
  (NOT_ADMITTED, discover, the loop's re-register), answers both heartbeat forms and a
  command of every arm the adapter serves, refuses a command for an unserved robot on
  each arm that names one, and ends in a transport drop; the second is the reconnect,
  with the authoritative action state of its RegisterAck;
* the telemetry loop on a fake clock, so the robot drives to its target and reports
  arrival, with the scenario's hardware fault and recovery in the telemetry;
* a SafetyStream session: an estop for the robot and one for an unserved robot, and
  clears for both.

It runs three times: as deployed (contract 1.3.0, no latch); as ``make conformance-sim``
runs it (``--latch``, 1.4.0), with ``--command-derived-plant`` too; and under the
``estop-drill`` scenario with a lease that lapses, for the scenario drill's confirmed stop
and the lease self-stop. The third golden file was recorded from 2b099f7 as well.

What is normalised, and only that: wall-clock timestamps (each is replaced by 1 when set,
so presence is still pinned), and the lease seconds an ActivateStream reply prints, which
are computed from the wall clock. Everything else -- every message, every field, the order
on each stream, and the stable stderr markers the scenarios tooling reads -- is compared
exactly.

Regenerate only on purpose, for a change that is MEANT to alter single-robot traffic:
``SWARMADA_REGEN_GOLDEN=1 python -m pytest tests/python/test_sim_adapter_golden_transcript.py``.
"""

from __future__ import annotations

import contextlib
import difflib
import hashlib
import io
import os
import pathlib
import re
import threading
import time

import pytest

pytest.importorskip("grpc")
pytest.importorskip("yaml")

import grpc  # noqa: E402
from google.protobuf import text_format  # noqa: E402

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.scenarios import HardwareFaultOverrides, ScenarioEngine, load_scenario  # noqa: E402
from adapters.simulation import sim_adapter  # noqa: E402
from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

ROBOT = "robot-conformance-1"
OTHER = "robot-not-served"
_GOLDEN = pathlib.Path(__file__).parent / "golden"
_REGEN = os.environ.get("SWARMADA_REGEN_GOLDEN") == "1"

# Every wall-clock field on the wire. Replaced by 1 when set, so whether it is set is pinned.
_TIME_FIELDS = frozenset({
    "timestamp_ms", "snapshot_ms", "confirmed_at_ms", "stop_initiated_at", "lifted_at_ms",
    "changed_at_ms", "received_at_ms", "sent_at_ms",
})


class _Dropped(grpc.RpcError):
    """A transport drop, as the gRPC response iterator raises it."""

    def code(self):
        return grpc.StatusCode.UNAVAILABLE


class _Clock:
    """A fake `time` module for the telemetry phase, advanced only by the test's own thread.

    Any other thread (the stream-lease watchdog, the edge loop) gets real time, so it can
    neither advance the clock nor spin on a sleep that returns at once.
    """

    def __init__(self, adapter: SimAdapter, ticks: int) -> None:
        self._adapter = adapter
        self._ticks = ticks
        self._owner = threading.get_ident()
        self._t = 1_800_000_000.0
        self._slept = 0

    def _mine(self) -> bool:
        return threading.get_ident() == self._owner

    def time(self) -> float:
        return self._t if self._mine() else time.time()

    def monotonic(self) -> float:
        return self._t if self._mine() else time.monotonic()

    def sleep(self, seconds: float) -> None:
        if not self._mine():
            time.sleep(seconds)
            return
        self._t += seconds
        self._slept += 1
        if self._slept >= self._ticks:
            self._adapter._shutdown.set()


class _Stub:
    """The FleetAdapterService stub the adapter dials: scripted sessions, recorded queues."""

    def __init__(self, adapter: SimAdapter, sessions: list, safety: list) -> None:
        self._adapter = adapter
        self._sessions = list(sessions)
        self._safety = list(safety)
        self.control: list = []   # (hello, the session's outbox queue)
        self.safety_queues: list = []

    def ControlStream(self, requests):  # noqa: N802 -- the gRPC method name
        queue_ = self._adapter._outbox
        hello = next(requests)
        self.control.append((hello, queue_))
        return self._sessions.pop(0)()

    def SafetyStream(self, requests):  # noqa: N802 -- the gRPC method name
        self.safety_queues.append(self._adapter._safety_outbox)
        return iter(self._safety)


def _is_repeated(fd) -> bool:
    """Whether `fd` is a repeated field, across the protobuf range the repo allows (>=3.20.3).

    protobuf 7 has no FieldDescriptor.label any more (CI installs 7.x); releases before
    FieldDescriptor.is_repeated have only the label. Use is_repeated where it exists.
    """
    is_repeated = getattr(fd, "is_repeated", None)
    if is_repeated is not None:
        return bool(is_repeated() if callable(is_repeated) else is_repeated)
    return fd.label == fd.LABEL_REPEATED


def _scrub(msg) -> None:
    for fd, value in msg.ListFields():
        if fd.type == fd.TYPE_MESSAGE:
            if fd.message_type.GetOptions().map_entry:
                continue  # this proto's maps hold scalars only
            if _is_repeated(fd):
                for item in value:
                    _scrub(item)
            else:
                _scrub(value)
        elif fd.name in _TIME_FIELDS:
            setattr(msg, fd.name, 1)


def _line(tag: str, msg) -> str:
    copy = type(msg)()
    copy.CopyFrom(msg)
    _scrub(copy)
    kind = copy.WhichOneof("payload") if copy.DESCRIPTOR.oneofs_by_name.get("payload") else ""
    # as_utf8: protobuf before 4.x escapes non-ASCII text as octal by default and later
    # releases print it as is; asking for UTF-8 makes every release print the same.
    text = text_format.MessageToString(copy, as_one_line=True, as_utf8=True)
    return _normalise(f"[{tag}] {kind}: {text}")


def _normalise(text: str) -> str:
    text = re.sub(r"lease_s=[0-9.]+", "lease_s=<t>", text)
    text = re.sub(r"for [0-9]+\.[0-9]{3}s", "for <t>s", text)
    return re.sub(r"\bat=[0-9]+\.[0-9]+", "at=<t>", text)


def _drain(tag: str, queue_) -> list:
    out = []
    while not queue_.empty():
        msg = queue_.get_nowait()
        out.append(f"[{tag}] <close sentinel>" if msg is None else _line(tag, msg))
    return out


def _cp(**payload):
    return pb.ControlPlaneMessage(**payload)


def _command(cid: int, robot: str = ROBOT, term: int = 1, **arm):
    return _cp(command=pb.Command(command_id=cid, robot_id=robot, leader_term=term, **arm))


def _assign(action_id: str, token: int | None, x: float = 3.0, **extra):
    fields = dict(action_id=action_id, action_type="Navigate",
                  payload_json=f'{{"x": {x}, "y": 0.0}}'.encode(),
                  assignment_id=f"asg-{action_id}", lease_duration_ms=30000,
                  lease_generation=1)
    if token is not None:
        fields["fencing_token"] = token
    fields.update(extra)
    return pb.AssignAction(**fields)


def _firmware(version: str, good: bool):
    digest = hashlib.sha256(f"firmware:{version}".encode()).hexdigest()
    return pb.PushFirmware(target_version=version, firmware_uri=f"sim://fw/{version}",
                           firmware_checksum=f"sha256:{digest if good else '0' * 64}")


def _session_one(scheduled: list, latch: bool):
    """Provisioning, every served arm, every unserved arm that names a robot, then a drop."""
    def session():
        yield _cp(hello_ack=pb.HelloAck(accepted=True,
                                        negotiated_protocol_version="fleet_adapter.v1",
                                        negotiated_contract_version="1.4.0"))
        yield _cp(register_ack=pb.RegisterAck(
            accepted=False, rejection=pb.REGISTRATION_REJECTION_NOT_ADMITTED,
            message="not admitted"))
        yield _cp(discover_ack=pb.DiscoverAck(accepted=True, discovered_robot_name="dr-1",
                                              ttl_seconds=600))
        # The provisioning loop's next iteration, run now rather than after its interval
        # (which the NOT_ADMITTED marker on stderr records).
        assert len(scheduled) == 1
        for _delay, fn in scheduled:
            fn()
        scheduled.clear()
        yield _cp(register_ack=pb.RegisterAck(
            accepted=True, telemetry_interval_seconds=1,
            authoritative_action_state=pb.RobotActionState(
                phase=pb.ROBOT_ACTION_PHASE_IDLE, fencing_token=3)))
        yield _cp(heartbeat=pb.HeartbeatRequest(robot_id="", sent_at_ms=5))
        yield _cp(heartbeat=pb.HeartbeatRequest(robot_id=ROBOT, sent_at_ms=6))
        yield _command(10, scan=pb.ScanCapabilities())
        yield _command(11, validate_action=pb.ValidateAction(
            action_type="Navigate", payload_json=b'{"x": 1, "y": 2}'))
        yield _command(12, validate_action=pb.ValidateAction(action_type="Teleport"))
        yield _command(13, assign_action=_assign("a-1", 5, x=50.0))
        yield _command(14, assign_action=_assign("a-0", 4))
        yield _command(15, assign_action=_assign("a-x", None))
        yield _command(16, renew_lease=pb.RenewActionLease(action_id="a-1", lease_generation=1,
                                                           lease_duration_ms=30000))
        yield _command(17, renew_lease=pb.RenewActionLease(action_id="a-1", lease_generation=9,
                                                           lease_duration_ms=30000))
        yield _command(18, cancel_action=pb.CancelAction(action_id="a-old", fencing_token=5))
        yield _command(19, zone_admission=pb.ZoneAdmission(zone_name="z-1", admit=False))
        yield _command(20, zone_admission=pb.ZoneAdmission(zone_name="z-1", admit=True))
        yield _command(21, pause=pb.PauseCapabilities(require_stop_before_ack=True))
        yield _command(22, resume=pb.ResumeCapabilities())
        yield _command(23, verify_hardware=pb.VerifyHardware(component_name="lidar",
                                                             expected_metrics={"rpm": 10.0}))
        yield _command(24, verify_model=pb.VerifyModel(model_name="m"))
        lease_ms = int(time.time() * 1000) + 60000
        yield _command(25, activate_stream=pb.ActivateStream(
            robot_id=ROBOT, stream_id="front-rgb", rate_hz=5.0, fencing_token=1,
            lease_expires_ms=lease_ms, sinks=[pb.StreamSink(address="compute:1")]))
        yield _command(26, activate_stream=pb.ActivateStream(
            robot_id=ROBOT, stream_id="mic-array", rate_hz=5.0, fencing_token=1,
            lease_expires_ms=lease_ms,
            window=pb.ClipWindow(mode=pb.CLIP_WINDOW_MODE_CLIP, pre_seconds=2)))
        yield _command(27, deactivate_stream=pb.DeactivateStream(robot_id=ROBOT,
                                                                 stream_id="front-rgb"))
        yield _command(28, deactivate_stream=pb.DeactivateStream(robot_id=ROBOT,
                                                                 stream_id="odom-pose"))
        yield _command(29, push_firmware=_firmware("2.0.0", good=False))
        yield _command(30, push_firmware=_firmware("2.0.0", good=True))
        yield _command(31, assign_action=_assign(
            "a-c", 6, action_type="Custom", custom_type="paint-walls", payload_json=b"{}"))
        yield _command(32, assign_action=_assign(
            "a-r", 7, operating_constraints=[pb.EffectiveOperatingConstraint(
                subject="camera.capture", declaring_zone="z-1")]))
        yield _command(33, reservation_granted=pb.ReservationGranted())
        yield _command(34, zone_constraints=pb.ZoneConstraints())
        # Every arm that names a robot, addressed to one this adapter does not serve.
        yield _command(40, robot=OTHER, assign_action=_assign("a-o", 8))
        yield _command(41, robot=OTHER, renew_lease=pb.RenewActionLease(
            action_id="a-1", lease_generation=1, lease_duration_ms=30000))
        yield _command(42, robot=OTHER, cancel_action=pb.CancelAction(action_id="a-1",
                                                                      fencing_token=5))
        yield _command(43, robot=OTHER, pause=pb.PauseCapabilities())
        yield _command(44, robot=OTHER, resume=pb.ResumeCapabilities())
        yield _command(45, robot=OTHER, zone_admission=pb.ZoneAdmission(zone_name="z-1"))
        yield _command(46, robot=OTHER, validate_action=pb.ValidateAction(
            action_type="Navigate"))
        yield _command(47, robot=OTHER, activate_stream=pb.ActivateStream(
            robot_id=OTHER, stream_id="front-rgb", lease_expires_ms=lease_ms))
        yield _command(48, robot=OTHER, deactivate_stream=pb.DeactivateStream(
            robot_id=OTHER, stream_id="front-rgb"))
        yield _command(49, robot=OTHER, push_firmware=_firmware("2.0.0", good=True))
        yield _command(50, robot=OTHER, model_update=pb.ModelUpdate())
        yield _command(51, robot=OTHER, scan=pb.ScanCapabilities())
        yield _command(52, robot=OTHER, estop=pb.Estop(estop_id="fence-other"))
        # The running action ends by cancel: STOPPED_SAFELY, or STOP_UNCONFIRMED for the
        # command-derived plant on a 1.4.0 connection.
        yield _command(53, cancel_action=pb.CancelAction(action_id="a-1", fencing_token=5))
        if latch:
            # A fence latches its stop; work is then refused; a lower term is rejected.
            yield _command(60, estop=pb.Estop(estop_id="stop-1", reason="fence"))
            yield _command(61, assign_action=_assign("a-l", 9))
            yield _command(62, term=0, assign_action=_assign("a-t", 10))
        raise _Dropped()
    return session


def _session_two(lease_ms: int = 30000):
    """The reconnect: a RegisterAck whose authoritative state diverges, then new work."""
    def session():
        yield _cp(hello_ack=pb.HelloAck(accepted=True,
                                        negotiated_protocol_version="fleet_adapter.v1",
                                        negotiated_contract_version="1.4.0"))
        yield _cp(register_ack=pb.RegisterAck(
            accepted=True, telemetry_interval_seconds=1,
            authoritative_action_state=pb.RobotActionState(
                phase=pb.ROBOT_ACTION_PHASE_IN_PROGRESS, action_id="a-1", fencing_token=10,
                lease_generation=1)))
        yield _command(70, assign_action=_assign("a-2", 11, x=3.0, lease_duration_ms=lease_ms))
        yield _command(71, assign_action=_assign(  # idempotent re-ack
            "a-2", 11, x=3.0, lease_duration_ms=lease_ms))
    return session


def _safety_script() -> list:
    return [
        pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop=pb.Estop(estop_id="stop-2")),
        pb.ControlPlaneSafetyMessage(robot_id=OTHER, estop=pb.Estop(estop_id="stop-o")),
        pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop_clear=pb.EstopClear(
            estop_id="stop-1", leader_term=1)),
        pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop_clear=pb.EstopClear(
            estop_id="stop-2", leader_term=1)),
        pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop_clear=pb.EstopClear(
            estop_id="stop-2", leader_term=1)),
        pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop_clear=pb.EstopClear(
            estop_id="stop-9", leader_term=1)),
        pb.ControlPlaneSafetyMessage(robot_id=OTHER, estop_clear=pb.EstopClear(
            estop_id="stop-o", leader_term=1)),
    ]


def _transcript(latch: bool, drill: bool = False) -> str:
    if drill:
        # estop-drill fires its confirmed stop at 20 s; the 1 ms lease of the reconnect's
        # work lapses on the telemetry loop's first pass, so the lease self-stop runs too.
        engine = ScenarioEngine(load_scenario("estop-drill"))
    else:
        overrides = HardwareFaultOverrides(component=None, fault_at=1.0, recover_at=3.0)
        engine = ScenarioEngine(load_scenario("hardware-fault", overrides))
    adapter = SimAdapter(pb, None, make_simulator("kinematic"), ROBOT, "warehouse-a",
                         "simulation", engine, latch=latch, command_derived_plant=latch)
    scheduled: list = []
    adapter._schedule = lambda delay, fn: scheduled.append((delay, fn))
    adapter._telemetry_started = True   # the telemetry phase below runs it on a fake clock
    stub = _Stub(adapter, [_session_one(scheduled, latch),
                           _session_two(lease_ms=1 if drill else 30000)], _safety_script())
    pre_safety = adapter._safety_outbox
    lines: list = []
    err = io.StringIO()
    with contextlib.redirect_stderr(err):
        adapter.run_control_stream(stub)
        for n, (hello, queue_) in enumerate(stub.control, start=1):
            lines.append(_line(f"control {n}", hello))
            lines.extend(_drain(f"control {n}", queue_))
        lines.extend(_drain("safety before its stream", pre_safety))
        time.sleep(0.1)   # the stream-lease watchdog sees the shutdown and exits
        adapter._shutdown.clear()
        real_time = sim_adapter.time
        sim_adapter.time = _Clock(adapter, ticks=110 if drill else 25)
        try:
            adapter._telemetry_loop(stub)
        finally:
            sim_adapter.time = real_time
        lines.extend(_drain("telemetry", stub.control[-1][1]))
        lines.extend(_drain("safety during telemetry", pre_safety))
        adapter._shutdown.clear()
        adapter.run_safety_stream(stub)
        for queue_ in stub.safety_queues:
            lines.extend(_drain("safety", queue_))
        lines.extend(_drain("control after safety", stub.control[-1][1]))
        adapter.shutdown()
    lines.extend(f"[stderr] {_normalise(s)}" for s in err.getvalue().splitlines())
    return "\n".join(lines) + "\n"


@pytest.mark.parametrize("name, latch, drill", [
    ("default", False, False), ("latch", True, False), ("drill", False, True)])
def test_single_robot_wire_traffic_is_unchanged(name: str, latch: bool, drill: bool) -> None:
    got = _transcript(latch, drill)
    path = _GOLDEN / f"sim_adapter_single_robot_{name}.txt"
    if _REGEN:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(got, encoding="utf-8")
        pytest.skip(f"regenerated {path}")
    want = path.read_text(encoding="utf-8")
    if got != want:
        diff = "".join(difflib.unified_diff(want.splitlines(True), got.splitlines(True),
                                            fromfile=f"golden/{path.name}", tofile="this run"))
        pytest.fail("single-robot traffic changed:\n" + diff)


def test_transcript_is_deterministic() -> None:
    """Two runs agree, so a golden difference is a behaviour change and not noise."""
    assert _transcript(latch=True) == _transcript(latch=True)


class _Descriptor:
    """A FieldDescriptor of one protobuf release or the other, for _is_repeated."""

    LABEL_OPTIONAL = 1
    LABEL_REPEATED = 3

    def __init__(self, repeated: bool, shape: str) -> None:
        if shape == "label":         # before is_repeated (the Mini's 5.29.x)
            self.label = self.LABEL_REPEATED if repeated else self.LABEL_OPTIONAL
        elif shape == "property":    # protobuf 7: is_repeated, and no label
            self.is_repeated = repeated
        else:                        # a method, should a release expose it as one
            self.is_repeated = lambda: repeated


@pytest.mark.parametrize("shape", ["label", "property", "method"])
@pytest.mark.parametrize("repeated", [True, False])
def test_is_repeated_reads_either_protobuf_api(shape: str, repeated: bool) -> None:
    assert _is_repeated(_Descriptor(repeated, shape)) is repeated


def test_is_repeated_agrees_with_the_installed_protobuf() -> None:
    fields = pb.CapabilitiesSnapshot.DESCRIPTOR.fields_by_name
    assert _is_repeated(fields["hardware"]) is True
    assert _is_repeated(fields["robot_id"]) is False
