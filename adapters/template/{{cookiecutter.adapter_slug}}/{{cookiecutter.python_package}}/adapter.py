# Copyright {{cookiecutter.year}} {{cookiecutter.author}}
# SPDX-License-Identifier: Apache-2.0

"""{{cookiecutter.adapter_name}} — a Swarmada Fleet Adapter.

Feature-basic (ADR-0005): the CONFORMANCE.md safety MUSTs are wired from the
shared `swarmada_sdk` primitives; optional commands are declined
with `unsupported = true` (C7). A dropped ControlStream or SafetyStream is redialled
with bounded, jittered exponential backoff (C1.5). It keeps the stop latch of contract
1.4.0 (RFC-0001 #fleet-adapter-protocol-the-stop-latch; conformance C5.8). Bind
`RobotBinding` (see robot.py) to your fleet API — every `TODO(vendor)` marks a point where
you do so.

Run:  python -m {{cookiecutter.python_package}}.adapter --endpoint localhost:9090
"""

from __future__ import annotations

import argparse
import json
import pathlib
import queue
import random
import secrets
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

_TELEMETRY_INTERVAL = 0.2  # until a RegisterAck advertises one (C2.3)
_TICK_DT = 0.2

# C1.5: reconnect with bounded exponential backoff. The first redial comes within
# _BACKOFF_BASE seconds; each consecutive failure doubles the ceiling up to _BACKOFF_CAP.
# The delay is drawn uniformly below that ceiling ("full jitter"), so a fleet of adapters
# that lost the control plane together does not redial it together.
_BACKOFF_BASE = 0.5
_BACKOFF_CAP = 30.0


def _backoff_delay(attempt: int) -> float:
    """Seconds to wait before redial number `attempt` (0-based)."""
    return random.uniform(0.0, min(_BACKOFF_CAP, _BACKOFF_BASE * (2 ** attempt)))


# The fleet-adapter CONTRACT version this adapter implements (ADR-0032). Distinct from
# adapter_version (this build) and protocol_version (the wire package identity, not a semver, which
# is why it cannot express compatibility). A control plane refuses REGISTRATION for an adapter that
# reports nothing or reports out of range, so this is not optional -- telemetry, heartbeat and
# emergency stop keep working either way. Bump it only when this adapter is re-qualified against a
# new contract (`make conformance`), and update the adapters/REGISTRY.md row with it.
#
# THE TEMPLATE SHIPS AT THE CURRENT CONTRACT, AND A CHECK HOLDS IT THERE. A control plane
# accepts minor N and N-1 of the contract version it implements, within the major (ADR-0032).
# The window is relative to the control plane and nothing in it dates or expires: an adapter
# stays admissible until a control plane's version is two minors past the one it reports,
# and is then refused RegisterRobot/DiscoverRobot with REGISTRATION_REJECTION_VERSION_MISMATCH --
# connected, streaming telemetry, still receiving estop, and unable to bring a robot under
# management (ADR-0032 Amendment 1, A1.2). An adapter generated at N-1 would start one
# control-plane minor bump from that. `make spec-contract` fails if this value and
# contract.Version drift. (The in-tree simulation adapter declares the contract version with
# --latch and the previous minor without it; adapters/example-noop declares the previous minor
# on purpose, so the default `make conformance` run exercises the N-1 half of the window.)
#
# 1.4.0 is a TRUE claim. Every change since 1.0.0 was additive -- new fields on existing
# messages and new optional Command arms -- so an adapter that ignores what it does not
# know speaks them by construction. What 1.2.0 added that needs code is the FA-R5/FA-R6
# DECLINE below: an adapter that honours no operating subject must refuse every operating
# rule it is sent rather than accept and ignore it. It HONOURS no subject and declares none
# (honoured_operating_constraints stays empty), which FA-R1 says is not a breach.
# What 1.3.0 added is the zone_constraints push (FA-R7), an OPTIONAL Command that places
# its MUST on the control plane and adds no obligation to the adapter. This adapter
# declines it unsupported=true through the same branch it declines every other optional
# command (C7.1), which is conformant and is the reason the arm is optional.
# What 1.4.0 added is the Custom verb on the wire: a catalog entry per verb
# (SupportedAction.custom_type, the optional fourth element of a catalog() entry) and the
# verb read from AssignAction.custom_type before the payload's `operation` (_servable). The
# default binding serves no Custom verb, so it declares none. 1.4.0 also added
# CANCEL_DISPOSITION_STOP_UNCONFIRMED, for an adapter that cannot witness rest; the default
# binding confirms rest (is_stopped), so it answers STOPPED_SAFELY and never sends it.
# And 1.4.0 added the stop latch (RFC-0001 #fleet-adapter-protocol-the-stop-latch), which
# this adapter keeps: see "The stop latch" in the adapter class. An adapter that reports
# 1.4.0 and keeps no latch fails conformance C5.8.
#
# Only the control plane's EstopClear lifts a latched stop, and only the EstopClear that names a
# local set ends the refusal from an edge stop. The reference control plane ends no local set at
# v0.4 (RFC-0001, safety, the edge-stop report's Not-met note), so against it a robot this
# adapter took an edge stop for takes no work until the adapter restarts. Never report 1.4.0
# without the latch.
CONTRACT_VERSION = "1.4.0"

# The operating subjects this adapter can honour while a robot is in a zone that restricts
# one (FA-R1). EMPTY in the generated skeleton, and that is a conformant answer, not a gap:
# FA-R1 states that an adapter declaring none is not in breach -- its robots are simply
# withheld from zones restricting a subject, by candidate filter 12. List a subject here only
# once the adapter really holds it off, and declare the same set on the CapabilitiesSnapshot.
_HONOURED_SUBJECTS: frozenset[str] = frozenset()


def _unhonourable(constraints, honoured) -> list[str]:
    """The subjects in `constraints` this adapter cannot honour (FA-R5 and FA-R6).

    ONE PREDICATE COVERS BOTH OBLIGATIONS, and that is the point. A subject the adapter does
    not RECOGNISE is not in `honoured` either, so it lands here exactly as a recognised
    subject it cannot honour does. Writing the unknown case separately is how it comes to be
    written as "ignore it": an unrecognised constraint is NOT an absent constraint, and
    treating it as absent would make the namespaced extension form a universal bypass --
    any vendor could route around every operating rule by inventing a subject.
    """
    return [c.subject for c in constraints if c.subject not in honoured]


def _predates(version: str, major: int, minor: int) -> bool:
    """Whether a negotiated contract version reads as one of this major below major.minor.

    False for a version that cannot be read, or of another major: neither tells the adapter
    that a rule of major.minor does not bind the connection.
    """
    parts = version.split(".")
    try:
        got_major, got_minor = int(parts[0]), int(parts[1])
    except (IndexError, ValueError):
        return False
    return got_major == major and got_minor < minor


# The Command arms this adapter carries out on the robot: its motion, its lease, a hold. Each
# is refused when Command.robot_id is not the robot this adapter serves (`_refuse_unserved`).
# `scan` is not listed: it moves nothing, and the snapshot names its own robot. An optional
# arm this adapter declines (`unsupported`) is declined whatever robot it names.
# TODO(vendor): add every arm you implement that acts on the robot.
_ROBOT_ACTING_ARMS = frozenset({"assign_action", "renew_lease", "cancel_action", "pause", "resume"})


def _custom_type(entry) -> str:
    """The Custom verb a catalog entry is keyed by; "" for an unkeyed entry."""
    return entry[3] if len(entry) > 3 else ""


def _servable(catalog, action_type: str, payload_json: bytes, custom_type: str = ""):
    """((x, y), "") when the assignment is one the catalog serves, else (None, reason).

    The target is read from the payload (RFC-0001: an adapter reads the payload from
    `payload_json` on assignment). TODO(vendor): extend for types that carry more than a
    target, and read every parameter your types declare.

    A Custom action's verb is ``custom_type`` (AssignAction.custom_type, contract 1.4.0) when
    it is set and the payload's ``operation`` otherwise; both set and different is refused.
    It is judged against the entry keyed by that verb, or the unkeyed Custom entry when no
    entry is keyed by it, so a verb the robot does not serve is refused, never run as
    another Custom verb.
    """
    try:
        doc = json.loads(payload_json) if payload_json else {}
    except ValueError:
        return None, "payload_json is not JSON"
    if not isinstance(doc, dict):
        return None, "payload_json is not a JSON object"
    verb = None
    if action_type == "Custom":
        op = doc.get("operation")
        if custom_type and op is not None and op != custom_type:
            return None, (f"custom_type {custom_type!r} and the payload's operation {op!r} "
                          f"disagree")
        verb = custom_type or op
    entries = [e for e in catalog if e[0] == action_type]
    entry = next((e for e in entries if verb and _custom_type(e) == verb), None) or \
        next((e for e in entries if not _custom_type(e)), None)
    if entry is None:
        if entries and verb:
            return None, f"Custom verb {verb!r} is not in this adapter's catalog"
        return None, f"action type {action_type!r} is not in this adapter's catalog"
    missing = [name for name, _unit, _kind in entry[2] if name not in doc]
    if missing:
        return None, f"{action_type} requires {missing}"
    try:
        return (float(doc.get("x", 0.0)), float(doc.get("y", 0.0))), ""
    except (TypeError, ValueError):
        return None, "x and y must be numbers in metres"


class {{cookiecutter.python_package}}Adapter:  # noqa: N801  (generated class name)
    def __init__(self, pb, pb_grpc, robot, robot_id: str, namespace: str = "") -> None:
        self._pb = pb
        self._pb_grpc = pb_grpc
        self._robot = robot
        self._robot_lock = threading.Lock()
        self._robot_id = robot_id
        self._namespace = namespace
        self._fence = FenceGuard()                              # C3
        self._lease = LeaseMonitor(on_expiry=self._self_stop)   # C4
        self._current_action = ""
        self._lease_action = ""   # the action the held lease belongs to (C4.2)
        self._current_generation = 0
        self._outbox: queue.Queue = queue.Queue()
        self._shutdown = threading.Event()
        self._telemetry_started = False
        self._telemetry_interval = _TELEMETRY_INTERVAL
        # True only between an ACCEPTED RegisterAck and the end of that session. Work
        # arriving on a session whose registration was refused is declined (C13.2).
        self._registered = False
        # True once this session's HelloAck accepted it. Telemetry waits for it, so a
        # redialled stream carries RegisterRobot before any TelemetryPayload.
        self._session_ready = False
        # Set on every accepted RegisterAck: the next TelemetryPayload carries the FULL
        # hardware picture, never a delta, so a reconnected control plane cannot keep a
        # stale one (C6.1).
        self._full_snapshot_due = False
        self._edge_outboxes: dict[str, queue.Queue] = {}  # one per dialled edge node (C8)
        self._edge_lock = threading.Lock()
        # The stop latch (see "The stop latch", below). Kept in this process only: an adapter
        # process that restarts holds none of these IDs.
        self._safety_outbox: queue.Queue = queue.Queue()  # the live SafetyStream's
        self._latch_lock = threading.RLock()
        self._latched: set[str] = set()      # stop IDs latched for the robot
        self._fenced: set[str] = set()       # stop IDs whose fence has arrived
        self._cleared: set[str] = set()      # stop IDs a clear has lifted
        self._pending_clears: dict = {}      # stop ID -> an EstopClear that came before its fence
        self._leader_term = 0                # the highest leader term seen
        self._local_set = False              # the robot's local latch; an edge stop sets it
        self._local_set_id = ""              # the latest set whose clear has not taken effect
        self._local_changed_ms = 0
        self._interrupted: dict[str, int] = {}  # action_id -> fencing token, of interrupted work
        self._action_fencing_token = 0       # the token the current action was accepted under
        # HelloAck.negotiated_contract_version of the current session: whether the leader-term
        # rule binds (_latch_command). Set by each session's HelloAck, before any Command.
        self._negotiated_contract = ""

    # ── ControlStream ────────────────────────────────────────────────────────

    def _control_requests(self, outbox: queue.Queue) -> Iterator:
        pb = self._pb
        yield pb.AdapterMessage(hello=pb.AdapterHello(  # C1.2
            vendor="{{cookiecutter.vendor}}", adapter_version="0.1.0",
            protocol_version="fleet_adapter.v1", contract_version=CONTRACT_VERSION,
            namespace=self._namespace))
        while True:
            msg = outbox.get()
            if msg is None:
                return
            yield msg

    def run_control_stream(self, stub) -> None:
        with self._robot_lock:
            self._robot.spawn(self._robot_id)
        attempt = 0
        try:
            while not self._shutdown.is_set():
                # A fresh outbox per session. The generator of a stream that just died can
                # still be blocked in get(); on a shared queue it would swallow the
                # RegisterRobot the NEW session enqueues, and the control plane would never
                # see the re-registration it requires on reconnect (C2.4).
                outbox: queue.Queue = queue.Queue()
                self._outbox = outbox
                self._registered = False
                self._session_ready = False
                try:
                    for cp in stub.ControlStream(self._control_requests(outbox)):
                        if cp.WhichOneof("payload") == "hello_ack" and cp.hello_ack.accepted:
                            attempt = 0  # a handshake completed: the next outage starts over
                        self._handle_control_plane(cp, stub)
                except grpc.RpcError as exc:
                    if self._shutdown.is_set():
                        break
                    delay = _backoff_delay(attempt)
                    attempt += 1
                    print(f"control stream lost ({exc.code()}); redialling in {delay:.1f}s",
                          file=sys.stderr)
                    self._shutdown.wait(delay)
                    continue
                finally:
                    outbox.put(None)  # release the old session's request generator
                break  # the control plane closed the stream cleanly: stop
        finally:
            self._shutdown.set()

    def _handle_control_plane(self, cp, stub) -> None:
        pb = self._pb
        kind = cp.WhichOneof("payload")
        if kind == "hello_ack":
            if not cp.hello_ack.accepted:
                raise RuntimeError(f"handshake rejected: {cp.hello_ack.message}")
            self._negotiated_contract = cp.hello_ack.negotiated_contract_version
            self._outbox.put(pb.AdapterMessage(  # C2.1: register
                register=pb.RegisterRobot(
                    robot_id=self._robot_id,
                    reported_hardware=self._reported_hardware())))
            # C12.2: push the catalog at registration. No v0.4 control plane issues `scan`, so
            # this snapshot is the only way the plane learns what this robot serves.
            self._outbox.put(pb.AdapterMessage(capabilities=self._snapshot()))
            self._session_ready = True
            if not self._telemetry_started:  # C6: stream telemetry once registered
                self._telemetry_started = True
                threading.Thread(target=self._telemetry_loop, args=(stub,), daemon=True).start()
        elif kind == "register_ack":
            ack = cp.register_ack
            if not ack.accepted:
                # Refused (for example VERSION_MISMATCH): the robot is not under management
                # on this session, so no work is accepted on it (C13.2). Telemetry and the
                # SafetyStream carry on; estop is honoured regardless (C14.1).
                print(f"registration refused ({ack.rejection}): {ack.message}", file=sys.stderr)
                # The Stop Latch, A refused register: work held for the robot is interrupted
                # work. It is stopped and never resumed.
                with self._latch_lock:
                    self._interrupt_work()
                return
            self._registered = True
            self._full_snapshot_due = True  # C6.1
            self._reconcile(ack)            # C3.8, C2.2
            # C2.3: adopt the cadence the control plane advertised. Only a positive value is
            # one (0 means "not specified"); it is clamped to the protocol's 1-30 s range.
            if ack.telemetry_interval_seconds > 0:
                self._telemetry_interval = max(
                    1.0, min(30.0, float(ack.telemetry_interval_seconds)))
            # C8.1: an accepted registration that lists edge endpoints REQUIRES a dial to
            # each one. The EdgeStream carries headless estop from the zone's edge node,
            # which keeps working while the control plane is unreachable.
            for e in ack.edge_endpoints:
                self._dial_edge(e.address)
        elif kind == "heartbeat":
            self._outbox.put(pb.AdapterMessage(
                heartbeat=pb.HeartbeatResponse(robot_id=cp.heartbeat.robot_id)))
        elif kind == "command":
            self._handle_command(cp.command)

    def _reconcile(self, ack) -> None:
        """Adopt the control plane's authoritative state on (re-)registration.

        The control plane is the source of truth across a connectivity gap: a task may have
        been cancelled while this adapter was offline, and nothing local can tell it so.
        """
        pb = self._pb
        if not ack.HasField("authoritative_action_state"):
            return
        st = ack.authoritative_action_state
        # C3.8: raise the fencing high-water mark to the control plane's own statement of
        # it, before looking at the phase. After a control-plane failover the new plane can
        # hold a token this adapter never saw; without adopting it, the adapter would keep
        # honouring commands the old plane authorised. `adopt` never lowers the mark.
        if st.fencing_token:
            self._fence.adopt(self._robot_id, st.fencing_token)
        phase = st.phase
        if phase == pb.ROBOT_ACTION_PHASE_IN_PROGRESS:
            if st.action_id and st.action_id == self._current_action:
                return  # agrees with local state: keep executing under the same lease
            phase = pb.ROBOT_ACTION_PHASE_CANCELLED  # divergence: treat as cancelled
        if phase in (pb.ROBOT_ACTION_PHASE_CANCELLED, pb.ROBOT_ACTION_PHASE_UNKNOWN,
                     pb.ROBOT_ACTION_PHASE_IDLE):
            # C2.2: halt and report, never resume a task the control plane dropped.
            with self._robot_lock:
                self._robot.command_stop(self._robot_id)
            self._lease.release(self._robot_id)
            reported = st.action_id or self._current_action
            state = ("CANCELLED" if phase == pb.ROBOT_ACTION_PHASE_CANCELLED
                     else "UNKNOWN" if phase == pb.ROBOT_ACTION_PHASE_UNKNOWN else "STOPPED")
            self._current_action = ""
            self._lease_action = ""
            self._emit_action_status(reported, state, "reconciled to the authoritative state")

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

    def _snapshot(self):
        """The CapabilitiesSnapshot: the robot's catalog, as the binding declares it."""
        pb = self._pb
        with self._robot_lock:
            catalog = self._robot.catalog()
        return pb.CapabilitiesSnapshot(
            robot_id=self._robot_id, snapshot_ms=int(time.time() * 1000),
            hardware=self._reported_hardware(),  # ALL components (C12.1)
            supported_actions=[
                pb.SupportedAction(action_type=e[0], required_capabilities=list(e[1]), params=[
                    pb.ActionParam(name=n, unit=u, kind=k, required=True) for n, u, k in e[2]],
                    custom_type=_custom_type(e))  # one entry per Custom verb (1.4.0)
                for e in catalog])

    def _handle_command(self, command) -> None:
        pb = self._pb
        if command.robot_id == self._robot_id and self._latch_command(command):
            return
        result = pb.CommandResult(command_id=command.command_id, robot_id=command.robot_id)
        which = command.WhichOneof("command")
        if command.robot_id != self._robot_id and which in _ROBOT_ACTING_ARMS:
            self._refuse_unserved(command, which, result)
            self._outbox.put(pb.AdapterMessage(command_result=result))
            return
        # Every arm below acts for self._robot_id, which is the robot_id the command was
        # addressed to: an accepted action keeps that id for its whole life.
        if which == "assign_action":
            self._on_assign(command.assign_action, result)
        elif which == "renew_lease":
            self._on_renew(command.renew_lease, result)
        elif which == "scan":
            # C12.1 — REQUIRED: answer with a full CapabilitiesSnapshot, never decline.
            result.scan.CopyFrom(self._snapshot())
        elif which == "pause":
            # C11.1 — REQUIRED: pause and resume are answered, never declined.
            with self._robot_lock:
                paused = self._robot.pause(
                    self._robot_id, command.pause.require_stop_before_ack)
            result.pause.CopyFrom(pb.PauseResult(
                paused=paused, message="" if paused else "the robot could not be paused"))
        elif which == "resume":
            with self._robot_lock:
                resumed = self._robot.resume(self._robot_id)
            result.resume.CopyFrom(pb.ResumeResult(
                resumed=resumed, message="" if resumed else "the robot could not be resumed"))
        elif which == "cancel_action":
            named = command.cancel_action.action_id
            if named and self._current_action and named != self._current_action:
                # C3.9: a cancel for an action this robot is no longer running. The control
                # plane sends one after an in-place preemption: the robot has switched to
                # the preempting action, and the preempted one is then cancelled by name.
                # It is stopped already, and stopping the robot would abort the action
                # that replaced it, which the contract forbids (a stale cancel "cannot
                # abort a newer assignment": RFC-0001
                # #fleet-adapter-protocol-the-controlstream-service).
                result.cancel_action.CopyFrom(pb.CancelActionResult(
                    acknowledged=True, disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY))
                self._outbox.put(pb.AdapterMessage(command_result=result))
                return
            with self._robot_lock:
                self._robot.command_stop(self._robot_id)
            self._lease.release(self._robot_id)
            # C16.1: a task that ends reports its terminal state; without it the control
            # plane can resolve the task only by lease expiry.
            self._emit_action_status(self._current_action, "CANCELLED", "cancel_action honoured")
            self._current_action = ""
            self._lease_action = ""
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

    # ── The stop latch (contract 1.4.0) ──────────────────────────────────────
    #
    # RFC-0001 #fleet-adapter-protocol-the-stop-latch:
    #   * a stop latches its stop ID, on whichever path it came: an Estop on SafetyStream or its
    #     fence (Command.estop) on ControlStream; an ID already cleared does not latch again;
    #   * an edge stop sets the robot's local latch under a fresh random set ID;
    #   * while anything is latched, every AssignAction is refused ROBOT_UNAVAILABLE and nothing
    #     starts, except a re-delivery of interrupted work, which is accepted with no motion;
    #   * each refusal is reported (an EstopAck for every latched ID, and a LocalStopReport),
    #     and both are sent on each SafetyStream that opens;
    #   * an EstopClear lifts the ID it names once that stop's fence has arrived (a clear that
    #     arrives first is kept), and is answered with an EstopClearAck;
    #   * a Command other than a stop, or an EstopClear, whose leader term is lower than the
    #     highest seen is rejected; a stop is accepted whatever its term. A Command on a
    #     connection that negotiated less than 1.4.0 carries no term and is not rejected
    #     for it;
    #   * work a stop interrupts is stopped, reported FAILED with ACTION_END_CAUSE_ESTOP, and
    #     never resumed.
    #
    # TODO(vendor): the local latch is at the robot (an e-stop button, a safety relay). Read it
    # from your fleet API: set _local_set when it is set, mint a fresh set ID, and report it;
    # clear _local_set only when your robot reports a local reset. Only a local reset lifts it.

    def _under_latch(self) -> bool:
        return bool(self._latched) or self._local_set or bool(self._local_set_id)

    def _interrupt_work(self) -> None:
        """Stop the action the robot holds, report it ended by the stop, and keep it stopped."""
        pb = self._pb
        action = self._current_action
        if not action:
            return
        self._interrupted[action] = self._action_fencing_token
        with self._robot_lock:
            self._robot.command_stop(self._robot_id)
        self._lease.release(self._robot_id)
        self._current_action = ""
        self._lease_action = ""
        self._emit_action_status(action, "STOPPED", "stopped by a stop")
        self._outbox.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=action, state=pb.ACTION_STATE_FAILED, message="ended by a stop",
            fencing_token=self._interrupted[action], end_cause=pb.ACTION_END_CAUSE_ESTOP)))

    def _report_latch(self, outbox: queue.Queue) -> None:
        """Every latched stop ID as an EstopAck, and the local latch as a LocalStopReport."""
        pb = self._pb
        for estop_id in sorted(self._latched):
            outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot_id, estop_ack=pb.EstopAck(
                estop_id=estop_id, state=pb.ESTOP_STATE_STOPPED,
                confirmed_at_ms=int(time.time() * 1000), message="latched (report)")))
        outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot_id, local_stop=pb.LocalStopReport(
            latched=self._local_set, changed_at_ms=self._local_changed_ms, source="robot",
            set_id=self._local_set_id)))

    def _latch_command(self, command) -> bool:
        """The leader term, a fence, and work under the latch. True when the command is answered."""
        pb = self._pb
        which = command.WhichOneof("command")
        with self._latch_lock:
            # The term is checked before the latch and the fencing token, on a connection that
            # negotiated 1.4.0. One that negotiated less sends no term, and is not rejected for
            # it (The Stop Latch, Version, and Note 2); a version that cannot be read is checked.
            if (which != "estop" and command.leader_term < self._leader_term
                    and not _predates(self._negotiated_contract, 1, 4)):
                self._outbox.put(pb.AdapterMessage(command_result=pb.CommandResult(
                    command_id=command.command_id, robot_id=command.robot_id,
                    stale_leader_term=True)))
                return True
            self._leader_term = max(self._leader_term, command.leader_term)
            if which == "estop":
                self._on_fence(command)
                return True
            if which == "assign_action" and self._under_latch():
                self._refuse_under_latch(command)
                return True
        return False

    def _on_fence(self, command) -> None:
        """The fence is a stop: latch its ID, stop the robot, answer on SafetyStream."""
        pb = self._pb
        estop_id = command.estop.estop_id
        if estop_id in self._cleared:
            return  # a fence for an ID already cleared is ignored
        self._latched.add(estop_id)
        self._fenced.add(estop_id)
        self._interrupt_work()
        ack = self._estop_ack(estop_id)
        self._safety_outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot_id, estop_ack=ack))
        # A receipt RFC-0001 permits; the control plane never reads it.
        self._outbox.put(pb.AdapterMessage(command_result=pb.CommandResult(
            command_id=command.command_id, robot_id=command.robot_id, estop=ack)))
        clear = self._pending_clears.pop(estop_id, None)
        if clear is not None:
            self._take_clear(clear, self._safety_outbox)

    def _refuse_under_latch(self, command) -> None:
        pb = self._pb
        task = command.assign_action
        result = pb.CommandResult(command_id=command.command_id, robot_id=command.robot_id)
        if self._interrupted.get(task.action_id) == task.fencing_token:
            # A re-delivery of interrupted work: accepted, and the robot does not move.
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=True, accepted_fencing_token=task.fencing_token))
        else:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE,
                message=f"stop latched: {sorted(self._latched)}"
                        + (f", local set {self._local_set_id}" if self._local_set_id else "")))
            self._report_latch(self._safety_outbox)
        self._outbox.put(pb.AdapterMessage(command_result=result))

    def _on_estop_clear(self, clear, outbox: queue.Queue) -> None:
        with self._latch_lock:
            if clear.leader_term < self._leader_term:
                return  # a stale clear lifts nothing
            self._leader_term = max(self._leader_term, clear.leader_term)
            self._take_clear(clear, outbox)

    def _take_clear(self, clear, outbox: queue.Queue) -> None:
        """Lift the ID once its fence has arrived, and say so; keep a clear that came first."""
        pb = self._pb
        estop_id = clear.estop_id
        if estop_id not in self._cleared and estop_id not in self._fenced:
            self._pending_clears[estop_id] = clear  # takes effect when the fence arrives
            return
        if estop_id not in self._cleared:
            self._latched.discard(estop_id)
            self._cleared.add(estop_id)
            if clear.ends_local_set and clear.ends_local_set == self._local_set_id:
                self._local_set_id = ""  # the refusal from that set ends; a reset lifts the latch
        # Sent again for an ID already lifted.
        outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot_id, estop_clear_ack=(
            pb.EstopClearAck(estop_id=estop_id, lifted_at_ms=int(time.time() * 1000)))))

    def _refuse_unserved(self, command, which: str, result) -> None:
        """Refuse a command addressed to a robot this adapter does not serve.

        One ControlStream carries the commands for every Robot bound to this adapter, and
        Command.robot_id selects which one
        (RFC-0001 #fleet-adapter-protocol-the-controlstream-service). This adapter serves one.
        Carrying out a command addressed to another id would drive or stop the wrong robot, so
        nothing here reaches the robot, the fence or the lease. Each arm is answered with the
        refusal the contract defines for it, and the result keeps the id it was addressed to.
        An assignment is declined ROBOT_UNAVAILABLE: the robot it names is not available
        through this adapter, and the control plane can give the action to another robot.
        """
        pb = self._pb
        why = (f"robot_id {command.robot_id!r} is not served by this adapter, which serves "
               f"{self._robot_id!r}; nothing was done")
        if which == "assign_action":
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE,
                message=why))
        elif which == "renew_lease":
            result.renew_lease.CopyFrom(pb.RenewActionLeaseResult(
                renewed=False, running=False, message=why))
        elif which == "cancel_action":
            result.cancel_action.CopyFrom(pb.CancelActionResult(acknowledged=False, message=why))
        elif which == "pause":
            result.pause.CopyFrom(pb.PauseResult(paused=False, message=why))
        elif which == "resume":
            result.resume.CopyFrom(pb.ResumeResult(resumed=False, message=why))
        print(f"UNSERVED_REFUSED robot={self._robot_id} addressed={command.robot_id!r} "
              f"arm={which} command_id={command.command_id}", file=sys.stderr, flush=True)

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
        # C13.2: a session whose registration was refused manages no robot, so it runs no
        # work. After the fencing branches, so a stale token is still refused as STALE.
        if not self._registered:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE,
                message="this robot's registration was not accepted on this session"))
            return
        # C17.1 / C17.2 (FA-R5, FA-R6) -- DECLINE an operating rule this adapter cannot
        # honour, never accept-and-ignore it. Placed AFTER the two fencing branches on
        # purpose: staleness is about ordering, and a superseded command must be refused
        # STALE so the control plane learns its assignment lost, not refused for a rule that
        # was never the reason it lost.
        #
        # TODO(vendor): once your fleet API can actually hold a subject off for the duration
        # of a task -- shutter a camera, keep a pump dry, cap a speed -- add it to
        # _HONOURED_SUBJECTS, declare it on the CapabilitiesSnapshot, and ENFORCE it here.
        # Declining costs nothing (the action returns to Pending and the Scheduler tries the
        # next candidate); declaring a subject you do not enforce is a false attestation.
        # C19.1 / C19.2: judge the assignment against the catalog this adapter declares. A type
        # it does not serve, or one missing a parameter the type requires, is refused
        # INVALID_ACTION -- never run as the nearest thing the robot can do.
        with self._robot_lock:
            catalog = self._robot.catalog()
        target, reason = _servable(catalog, task.action_type, task.payload_json, task.custom_type)
        if target is None:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION,
                message=reason))
            return
        unhonourable = _unhonourable(task.operating_constraints, _HONOURED_SUBJECTS)
        if unhonourable:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False,
                rejection=pb.ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET,
                unhonourable_subjects=unhonourable))
            return
        # One step under the lock, so _complete_on_arrival never sees half an assignment.
        with self._robot_lock:
            self._robot.command_move(self._robot_id, *target)
            self._current_action = task.action_id
            self._action_fencing_token = task.fencing_token
            if task.lease_duration_ms:
                self._current_generation = task.lease_generation
                self._lease_action = task.action_id  # which action this lease belongs to
                self._lease.grant(
                    self._robot_id, task.lease_duration_ms / 1000.0, task.lease_generation)
        result.assign_action.CopyFrom(pb.AssignActionResult(
            accepted=True, accepted_fencing_token=task.fencing_token))  # C3.5
        # C16.1: report the phase transition. SUCCEEDED follows from _complete_on_arrival.
        # TODO(vendor): emit FAILED when your robot gives up on the action -- a terminal
        # state is required, not optional.
        self._emit_action_status(task.action_id, "RUNNING", "assignment accepted")

    def _complete_on_arrival(self) -> None:
        """C16.1: report SUCCEEDED once the robot is at rest at the target it was sent to.

        Only arrival completes an action. A stop, pause, estop or lease self-stop brings the
        robot to rest short of its target, and `arrived` is False for each of them.
        """
        with self._robot_lock:
            action = self._current_action
            if not action or not self._robot.arrived(self._robot_id):
                return
            self._lease.release(self._robot_id)
            self._current_action = ""
            self._lease_action = ""
        self._emit_action_status(action, "SUCCEEDED", "arrived at target")

    def _emit_action_status(self, action_id: str, state: str, message: str = "") -> None:
        """Send an ActionStatusUpdate (C16.1), echoing the executing fencing token (C6.4)."""
        if not action_id:
            return
        pb = self._pb
        self._outbox.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=action_id,
            state=getattr(pb.ActionState, f"ACTION_STATE_{state}"),
            message=message,
            fencing_token=self._fence.highest(self._robot_id))))

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
        # C4.2: the lease lapsed without renewal. Stop, and REPORT the end: without a terminal
        # ActionStatusUpdate the control plane sees only silence. STOPPED says the stop was
        # commanded and is not terminal; the terminal report follows once rest is confirmed
        # or the stopping budget runs out. A timer left over from a superseded assignment
        # must not report the current action stopped.
        expired = self._lease_action
        if not expired or expired != self._current_action:
            return
        with self._robot_lock:
            self._robot.command_stop(robot_id)
        token = self._action_fencing_token   # the token the expired action was accepted under
        self._emit_action_status(expired, "STOPPED", "assignment lease expired")
        with self._robot_lock:
            rest = confirm_estop(self._robot, robot_id)  # confirmed, never inferred
        pb = self._pb
        self._outbox.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=expired, state=pb.ACTION_STATE_FAILED, fencing_token=token,
            message=("ended by lease expiry" if rest == ESTOP_STOPPED else
                     "ended by lease expiry; rest not confirmed within the stopping budget"),
            end_cause=pb.ACTION_END_CAUSE_LEASE_EXPIRED)))
        self._current_action = ""
        self._lease_action = ""

    def _telemetry_loop(self, stub) -> None:
        # The robot advances _TICK_DT per pass and a pass takes _TICK_DT of wall time, so it
        # moves at its own speed. Telemetry goes out once per _telemetry_interval, which a
        # RegisterAck may set to several seconds (C2.3): that cadence must not set how fast
        # the robot moves, or how late its arrival is reported.
        pb = self._pb
        last_sent = float("-inf")
        while not self._shutdown.is_set():
            self._lease.tick()  # C4 self-stop check
            with self._robot_lock:
                self._robot.tick(_TICK_DT)
                x, y, floor = self._robot.pose(self._robot_id)
                battery = self._robot.battery_percent(self._robot_id)
            self._complete_on_arrival()
            phase = (pb.RobotPhase.ROBOT_PHASE_IN_PROGRESS if self._current_action
                     else pb.RobotPhase.ROBOT_PHASE_IDLE)
            # Between sessions there is nothing to send it on.
            if not self._session_ready or time.monotonic() - last_sent < self._telemetry_interval:
                self._shutdown.wait(_TICK_DT)
                continue
            last_sent = time.monotonic()
            now_ms = int(time.time() * 1000)
            hardware = []
            if self._full_snapshot_due:  # C6.1: every component, once per registration
                self._full_snapshot_due = False
                hardware = [pb.HardwareStatusUpdate(component_name=c.name, status=c.status)
                            for c in self._reported_hardware()]
            self._outbox.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(  # C6
                robot_id=self._robot_id, timestamp_ms=now_ms,
                position=pb.RobotPosition(x=x, y=y, floor=floor),  # C6.2 explicit presence
                battery=pb.BatteryStatus(percent=battery), phase=phase,
                current_action=self._current_action, hardware=hardware)))
            with self._edge_lock:  # C8.2: tee a PositionFrame to every edge node, per tick
                for edge_out in self._edge_outboxes.values():
                    edge_out.put(pb.AdapterEdgeMessage(position=pb.PositionFrame(
                        robot_id=self._robot_id, ts_ms=now_ms,
                        position=pb.RobotPosition(x=x, y=y, floor=floor))))
            self._shutdown.wait(_TICK_DT)

    # ── SafetyStream (C5) ────────────────────────────────────────────────────

    def run_safety_stream(self, stub) -> None:
        pb = self._pb

        def requests(outbox: queue.Queue) -> Iterator:
            while True:
                msg = outbox.get()
                if msg is None:
                    return
                yield msg

        # The estop path must always be up (C1.1), so a dropped SafetyStream is redialled on
        # the same backoff as the ControlStream, and independently of it.
        attempt = 0
        while not self._shutdown.is_set():
            # A fresh outbox per session, as for the ControlStream. The generator of a stream
            # that just died can still be blocked in get(); on a shared queue it would take
            # the EstopAck for the next estop on the NEW stream and drop it with the dead
            # call, and that estop would never be confirmed.
            outbox: queue.Queue = queue.Queue()
            with self._latch_lock:
                self._safety_outbox = outbox
                self._report_latch(outbox)  # on each SafetyStream that opens
            try:
                for cp in stub.SafetyStream(requests(outbox)):
                    attempt = 0
                    if cp.WhichOneof("payload") == "estop" and cp.robot_id != self._robot_id:
                        # An estop for a robot this adapter does not serve. It cannot confirm
                        # that robot is at rest, so the only true answer is FAILED, which
                        # escalates; a STOPPED would confirm a stop nobody made. This robot
                        # is not stopped by it: the estop was addressed to another one.
                        print(f"ESTOP_UNSERVED robot={self._robot_id} addressed={cp.robot_id!r} "
                              f"id={cp.estop.estop_id}", file=sys.stderr, flush=True)
                        outbox.put(pb.AdapterSafetyMessage(
                            robot_id=cp.robot_id, estop_ack=pb.EstopAck(
                                estop_id=cp.estop.estop_id, state=pb.ESTOP_STATE_FAILED,
                                message=(f"robot_id {cp.robot_id!r} is not served by this "
                                         f"adapter, which serves {self._robot_id!r}; no stop "
                                         f"was commanded and none can be confirmed"))))
                    elif cp.WhichOneof("payload") == "estop":
                        with self._latch_lock:
                            if cp.estop.estop_id not in self._cleared:
                                self._latched.add(cp.estop.estop_id)
                            self._interrupt_work()
                        outbox.put(pb.AdapterSafetyMessage(
                            robot_id=cp.robot_id, estop_ack=self._estop_ack(cp.estop.estop_id)))
                    elif cp.WhichOneof("payload") == "estop_clear":
                        self._on_estop_clear(cp.estop_clear, outbox)
            except grpc.RpcError as exc:
                outbox.put(None)  # release the dead stream's generator
                if self._shutdown.is_set():
                    return
                delay = _backoff_delay(attempt)
                attempt += 1
                print(f"safety stream lost ({exc.code()}); redialling in {delay:.1f}s",
                      file=sys.stderr)
                self._shutdown.wait(delay)
                continue
            return  # closed cleanly

    def _estop_ack(self, estop_id: str):
        """Stop the robot and build a CONFIRMED EstopAck (C5.2, C5.3; C8.3 on the edge)."""
        pb = self._pb
        with self._robot_lock:
            # C5.3: stamp the moment the hardware stop is COMMANDED, before waiting for
            # confirmation. An adapter that inferred the stop has no such moment to report.
            initiated_ms = int(time.time() * 1000)
            state = confirm_estop(self._robot, self._robot_id)  # C5: confirmed, never inferred
        return pb.EstopAck(
            estop_id=estop_id, stop_initiated_at=initiated_ms,
            confirmed_at_ms=int(time.time() * 1000),
            state=(pb.ESTOP_STATE_STOPPED if state == ESTOP_STOPPED
                   else pb.ESTOP_STATE_FAILED))

    # ── EdgeStream (C8) ──────────────────────────────────────────────────────

    def _set_local_latch(self) -> None:
        """An edge stop sets the local latch, under a fresh 128-bit set ID, and is reported.

        Only a local reset at the robot lifts it, and the EstopClear that names the set ends
        the refusal from it (RFC-0001, The Stop Latch, The clear).
        """
        pb = self._pb
        with self._latch_lock:
            self._local_set = True
            self._local_set_id = secrets.token_hex(16)
            self._local_changed_ms = int(time.time() * 1000)
            self._interrupt_work()
            self._safety_outbox.put(pb.AdapterSafetyMessage(
                robot_id=self._robot_id, local_stop=pb.LocalStopReport(
                    latched=True, changed_at_ms=self._local_changed_ms, source="robot",
                    set_id=self._local_set_id)))

    def _dial_edge(self, address: str) -> None:
        with self._edge_lock:
            if address in self._edge_outboxes:
                return  # one EdgeStream per edge node
            outbox: queue.Queue = queue.Queue()
            self._edge_outboxes[address] = outbox
        threading.Thread(target=self._run_edge_stream, args=(address, outbox),
                         daemon=True).start()

    def _run_edge_stream(self, address: str, outbox: queue.Queue) -> None:
        pb = self._pb

        def requests() -> Iterator:
            while True:
                msg = outbox.get()
                if msg is None:
                    return
                yield msg

        # TODO(vendor): use grpc.secure_channel with mTLS credentials. A lost EdgeStream is
        # not redialled here: the next accepted RegisterAck re-advertises the edge endpoints
        # and _dial_edge opens it again.
        estub = self._pb_grpc.EdgeServiceStub(grpc.insecure_channel(address))
        try:
            for ec in estub.EdgeStream(requests()):
                if ec.WhichOneof("msg") == "estop":  # C8.3: same confirmed discipline as C5
                    self._set_local_latch()
                    outbox.put(pb.AdapterEdgeMessage(estop_ack=self._estop_ack(ec.estop.estop_id)))
        except grpc.RpcError:
            pass
        finally:
            with self._edge_lock:
                self._edge_outboxes.pop(address, None)


def main() -> None:
    ap = argparse.ArgumentParser(description="{{cookiecutter.adapter_name}}")
    ap.add_argument("--endpoint", default="localhost:9090")
    ap.add_argument("--robot-id", default="robot-conformance-1")
    ap.add_argument(
        "--namespace", default="{{cookiecutter.namespace}}",
        help="namespace declared in AdapterHello. With a client certificate, the certificate "
             "binds the namespace: pass an empty value to be served in it, or its exact name; "
             "any other value is refused at the handshake. Without a certificate the value is "
             "used as declared, so it must name the namespace your Robots are admitted in "
             "(default: %(default)s)")
    args = ap.parse_args()

    from fleet_adapter.v1 import fleet_adapter_pb2 as pb
    from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc

    robot = SimulatedRobot()  # TODO(vendor): replace with your RobotBinding
    adapter = {{cookiecutter.python_package}}Adapter(
        pb, pb_grpc, robot, args.robot_id, namespace=args.namespace)

    # TODO(vendor): use grpc.secure_channel with mTLS credentials (C1.3).
    channel = grpc.insecure_channel(args.endpoint)
    stub = pb_grpc.FleetAdapterServiceStub(channel)
    threading.Thread(target=adapter.run_safety_stream, args=(stub,), daemon=True).start()  # C1.1
    adapter.run_control_stream(stub)


if __name__ == "__main__":
    main()
