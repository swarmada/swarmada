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

"""The control-plane-side test server and the C1–C12 scenario driver.

Topology mirrors production (RFC-0001 §5.3): the adapter-under-test is the gRPC
*client* and dials in; this harness is the *server*. The harness accepts the
adapter's ``ControlStream`` and ``SafetyStream``, then a driver thread sends
crafted control-plane messages and asserts the adapter's replies against
``adapters/CONFORMANCE.md``.

Scope: all of C1–C10. The driver exercises C1 (handshake), C2 (registration, when
the adapter registers), C3 (fencing-token ordering), C5 (confirmed estop), C7
(optional-command decline via ``unsupported=true``), C8 (edge stream: the
harness also serves ``EdgeService`` and advertises an edge endpoint on the
``RegisterAck``, then checks the adapter dials it and honours an edge-issued
estop with a confirmed ``EstopAck``), C9 (an adapter that answers ``verify_*``
returns ``actualMetrics``, §6.10) and C10 (an adapter that accepts
``push_firmware`` MAY emit advisory ``UpdateProgress``, §6.6), C11 (an adapter MAY
accept ``pause``/``resume`` and answer with a ``PauseResult``/``ResumeResult``) and
C12 (an adapter MAY answer ``scan`` with a ``CapabilitiesSnapshot``). C9–C12 are
MAY-level — a decline is conformant. C4 (lease self-stop, a timer-driven
on-robot behaviour) and C6 (telemetry) are recorded ``skip`` when the adapter
does not exercise them — as are C2/C8 for a minimal adapter that registers no
robot. A ``skip`` is not a failure: it records exactly what was and was not
verified for the adapter under test.
"""

from __future__ import annotations

import json
import os
import queue
import sys
import threading
import time
from concurrent import futures

import grpc

from .report import CONTRACT_VERSION, CheckStatus, Report, SkipCause

# Injected by __main__ after the generated stubs are importable, so this module
# can be imported and unit-inspected without the stubs present.
pb = None  # fleet_adapter_pb2
pb_grpc = None  # fleet_adapter_pb2_grpc

ROBOT = "robot-conformance-1"

# ── Fixture goal poses ───────────────────────────────────────────────────────────
#
# An assignment that is meant to EXECUTE carries a target, so the action is still IN
# FLIGHT while the check that depends on it runs. Without one, `goal_from_assign`
# resolves to the ORIGIN -- the robot is already standing there, every assignment in
# the suite completes in milliseconds, and the suite cannot tell a real Nav2 stack
# from a fake action server (it did not: ZERO checks differed between them).
#
# payload_json, NOT destination. fleet_adapter.proto marks
#     string destination = 3 [deprecated = true];  // control plane no longer sets this
# and payload coordinates are resolution path 1 in task.py::goal_from_assign, so this
# exercises the path a real control plane actually takes.
#
# CONFIGURABLE because the three reference adapters have entirely different geometry;
# a hard-coded warehouse coordinate is a landmine for -vda5050 and -mavlink. The
# default is warehouse.pgm's open x=0 corridor (racking occupies |x| in [2, 6]), and
# both poles are confirmed-reachable open floor.
_GOAL_POSES_ENV = "SWARMADA_CONFORMANCE_GOAL_POSES"
_GOAL_POSES_DEFAULT = "0,-6;0,6"
# Speed the fixture assumes when turning a required travel TIME into a required
# distance. Deliberately conservative -- a slower robot covers less ground in the
# window, so under-estimating speed would under-size the goal.
_FIXTURE_SPEED_MPS = 0.5


def _goal_poses() -> list:
    raw = os.environ.get(_GOAL_POSES_ENV, _GOAL_POSES_DEFAULT)
    poses = []
    for part in raw.split(";"):
        part = part.strip()
        if not part:
            continue
        x, y = (float(v) for v in part.split(",")[:2])
        poses.append((x, y))
    if len(poses) < 2:
        raise ValueError(
            f"{_GOAL_POSES_ENV} needs at least two poses so consecutive fixture goals are "
            f"far apart; got {raw!r}")
    return poses


class _FixtureGoals:
    """Hands out fixture targets that keep the action in flight while a check looks.

    Call sites declare the TRAVEL TIME they need -- roughly twice the longest window
    that check waits on -- not a distance. The requirement is "still moving when the
    check looks", and that is a time, not a length.

    Always picks the configured pose FARTHEST from the previous target. Sizing per site
    against a fixed list is not sufficient on its own: consecutive goals can land close
    to where the robot already is, and a zero-distance goal completes instantly however
    carefully the check was sized.
    """

    def __init__(self) -> None:
        self._poses = _goal_poses()
        self._last = None

    def payload(self, needs_seconds: float) -> bytes:
        if self._last is None:
            target = self._poses[0]
        else:
            target = max(self._poses, key=lambda p: (p[0] - self._last[0]) ** 2
                                                    + (p[1] - self._last[1]) ** 2)
            need_m = needs_seconds * _FIXTURE_SPEED_MPS
            got_m = ((target[0] - self._last[0]) ** 2 + (target[1] - self._last[1]) ** 2) ** 0.5
            if got_m < need_m:
                print(f"harness: WARNING fixture goal {target} is {got_m:.1f} m from the "
                      f"previous target but this check needs {need_m:.1f} m; the action may "
                      f"complete before the check looks. Widen {_GOAL_POSES_ENV}.",
                      file=sys.stderr, flush=True)
        self._last = target
        return json.dumps({"x": target[0], "y": target[1]}).encode()

_GET_TIMEOUT = 8.0  # seconds to wait for any single adapter reply
_STREAM_TIMEOUT = 10.0  # seconds to wait for the adapter to open both streams
_REGISTER_WAIT = 1.5  # seconds to wait for an adapter-initiated registration
# fleet-adapter-protocol.md performance table: Command.estop -> EstopAck is a HARD
# < 500 ms requirement. C5.5 measures it; nothing did before.
_ESTOP_ACK_BUDGET_MS = 500.0
# C5.6 — how long a STOPPING ack has to reach a terminal STOPPED/FAILED.
#
# A HARNESS CONSTANT, deliberately NOT a spec value. The right bound is a property of the
# base -- measured 0.588-0.876 s for this one -- and a drone, a floor scrubber and a 100 kg
# hospital cart cannot share one number, so it belongs in the adapter's own declaration
# (CapabilitiesSnapshot) once that field exists. Generous here so a slow CI box does not
# flake; it is a liveness bound, not a performance target. Every C5.6 detail says so.
_ESTOP_TERMINAL_BUDGET_S = 3.0
# Spec budget for the first TelemetryPayload after reconnect; must not be tighter.
_POST_RECONNECT_TELEMETRY_WAIT = 11.0
_EDGE_WAIT = 3.0  # seconds to wait for the adapter to dial the advertised edge node
_RECONNECT_WAIT = 5.0  # seconds to wait for the adapter to redial after a dropped ControlStream
# Redial alone can be quick while the hello/register handshake that follows is not.
# C6.1/C2.2 skipped on three runs in four at 5s; this covers the adapter's backoff
# plus a full re-handshake without making a genuine failure wait long.
_ACCEPTING_HANDSHAKE_WAIT = 15.0
_SENTINEL = object()
# _ABORT ends a stream with a transport ERROR rather than a clean close. C13.2 needs the adapter to
# RE-OPEN with a fresh Hello/Register, and a conformant adapter is only required to survive an
# unexpected drop — a clean server-side close legitimately means "we are done", and several adapters
# (correctly) shut down on it. Aborting is therefore the only drop that prompts a redial without
# demanding behaviour the contract never asked for.
_ABORT = object()


def _supports_contract(reported: str) -> tuple[bool, str]:
    """Is a reported contract_version inside the range this suite attests against (ADR-0032)?

    Deliberately the same rule as the control plane's internal/contract.Supports, and deliberately
    FAIL-CLOSED on everything it cannot read: an empty value (an adapter predating contract
    versioning), a non-numeric or short version, prerelease/build metadata, a different major in
    either direction, and a minor NEWER than this suite implements. Skew is backwards compatibility
    for older adapters, never a promise about a contract that does not exist yet.

    If this drifts from the Go implementation an adapter could pass here and be refused at runtime,
    which is the one outcome a conformance suite must never produce.
    """
    if not reported:
        return False, ("no contract_version reported (an adapter predating contract versioning); "
                       "treated as incompatible")
    parts = reported.split(".")
    if len(parts) != 3 or not all(p.isdigit() for p in parts):
        return False, (f"contract_version {reported!r} is not a plain semver major.minor.patch "
                       "(prerelease and build metadata are not accepted)")
    major, minor, _ = (int(p) for p in parts)
    ours = CONTRACT_VERSION.split(".")
    our_major, our_minor = int(ours[0]), int(ours[1])
    if major != our_major:
        return False, (f"contract_version {reported!r} is major {major}; this suite attests "
                       f"against major {our_major} (a major bump is breaking and requires "
                       "re-qualification)")
    low = max(0, our_minor - 1)
    if not (low <= minor <= our_minor):
        return False, (f"contract_version {reported!r} is outside the supported range "
                       f">={our_major}.{low}.0 <{our_major}.{our_minor + 1}.0 "
                       f"(this suite attests against {CONTRACT_VERSION})")
    return True, ""


class _Session:
    """Shared state between the two stream RPCs and the driver thread."""

    def __init__(self) -> None:
        self.control_established = threading.Event()
        self.safety_established = threading.Event()
        self.control_in: queue.Queue = queue.Queue()  # adapter -> harness
        self.control_out: queue.Queue = queue.Queue()  # harness -> adapter
        self.safety_in: queue.Queue = queue.Queue()
        self.safety_out: queue.Queue = queue.Queue()
        # EdgeService (C8): the harness plays the edge node the adapter dials.
        self.edge_established = threading.Event()
        self.edge_in: queue.Queue = queue.Queue()   # adapter -> edge node
        self.edge_out: queue.Queue = queue.Queue()  # edge node -> adapter
        # C13.2: the harness drops the ControlStream after C12 and waits for the adapter to redial,
        # so the version-refusal path can be exercised for real. Set on any ControlStream call after
        # the first; the counter distinguishes "never reconnected" from "reconnected".
        self.control_reconnected = threading.Event()
        self.control_opens = 0
        self._opens_lock = threading.Lock()

    def drop_control(self) -> None:
        """Fault the ControlStream only, leaving SafetyStream up (C13.2/C14.1: a version refusal
        must not cost the operator the ability to stop the robot)."""
        self.control_out.put(_ABORT)

    def close_streams(self) -> None:
        self.control_out.put(_SENTINEL)
        self.safety_out.put(_SENTINEL)
        self.edge_out.put(_SENTINEL)


def _make_servicer(session: _Session):
    class Servicer(pb_grpc.FleetAdapterServiceServicer):
        def ControlStream(self, request_iterator, context):
            with session._opens_lock:
                session.control_opens += 1
                if session.control_opens > 1:
                    session.control_reconnected.set()
            session.control_established.set()

            def reader() -> None:
                try:
                    for msg in request_iterator:
                        session.control_in.put(msg)
                except grpc.RpcError:
                    # The RPC terminated — the peer went away, or the harness aborted this stream on
                    # purpose (C13.2). Either way the read side is simply over: the sentinel below
                    # tells the driver so. Letting this propagate would kill the thread with a
                    # traceback that looks like a harness failure in an otherwise clean run.
                    pass
                finally:
                    session.control_in.put(_SENTINEL)

            threading.Thread(target=reader, daemon=True).start()
            while True:
                out = session.control_out.get()
                if out is _SENTINEL:
                    return
                if out is _ABORT:
                    context.abort(grpc.StatusCode.UNAVAILABLE,
                                  "harness: contract-version negotiation probe (C13.2)")
                    return
                yield out

        def SafetyStream(self, request_iterator, context):
            session.safety_established.set()

            def reader() -> None:
                try:
                    for msg in request_iterator:
                        session.safety_in.put(msg)
                except grpc.RpcError:
                    # The RPC terminated — the peer went away, or the harness aborted this stream on
                    # purpose (C13.2). Either way the read side is simply over: the sentinel below
                    # tells the driver so. Letting this propagate would kill the thread with a
                    # traceback that looks like a harness failure in an otherwise clean run.
                    pass
                finally:
                    session.safety_in.put(_SENTINEL)

            threading.Thread(target=reader, daemon=True).start()
            while True:
                out = session.safety_out.get()
                if out is _SENTINEL:
                    return
                yield out

    return Servicer()


def _make_edge_servicer(session: _Session):
    """The EdgeService the harness serves so an adapter can dial it (C8)."""
    class EdgeServicer(pb_grpc.EdgeServiceServicer):
        def EdgeStream(self, request_iterator, context):
            session.edge_established.set()

            def reader() -> None:
                try:
                    for msg in request_iterator:
                        session.edge_in.put(msg)
                except grpc.RpcError:
                    # The RPC terminated — the peer went away, or the harness aborted this stream on
                    # purpose (C13.2). Either way the read side is simply over: the sentinel below
                    # tells the driver so. Letting this propagate would kill the thread with a
                    # traceback that looks like a harness failure in an otherwise clean run.
                    pass
                finally:
                    session.edge_in.put(_SENTINEL)

            threading.Thread(target=reader, daemon=True).start()
            while True:
                out = session.edge_out.get()
                if out is _SENTINEL:
                    return
                yield out

    return EdgeServicer()


class _Driver:
    """Runs the checks and records results, given an established session."""

    def __init__(self, session: _Session, report: Report, port: int) -> None:
        # Set when a TelemetryPayload is observed. Read by the skip-cause
        # contradiction check: claiming no-telemetry-in-run while telemetry
        # WAS seen is a false reason, and the run says so.
        self.telemetry_seen = False
        self._hardware_baseline = 0
        self.s = session
        self.r = report
        self.port = port
        self._next_command_id = 1
        self._goals = _FixtureGoals()
        # Component names from the CapabilitiesSnapshot that `scan` returns (C12.1). None
        # until scan answers. C6.1 cross-checks the first post-reconnect payload against it.
        self._scan_components = None
        self._pushback: list = []  # messages peeked-then-returned to the stream
        self._contract_ok = False  # set at the hello (C13.1)

    # -- small helpers ---------------------------------------------------------

    def _recv_control(self):
        if self._pushback:
            return self._pushback.pop(0)
        item = self.s.control_in.get(timeout=_GET_TIMEOUT)
        if item is _SENTINEL:
            raise EOFError("control stream closed by adapter")
        return item

    def _await_action_status(self, action_id: str, timeout: float = _GET_TIMEOUT,
                             states=None):
        """Drain the control stream for an ActionStatusUpdate on action_id.

        `states`, when given, restricts the match to those ActionState values. A task
        emits RUNNING on acceptance and a terminal state later, both under the same
        action_id, so a check that wants the terminal one MUST say so — otherwise it
        matches the acceptance update and concludes the robot stopped when it did not.

        Returns the update, or None if none arrives. Unrelated traffic (telemetry,
        heartbeat replies) is pushed back so a later check still sees it.
        """
        import queue as _queue
        import time as _time
        deadline = _time.monotonic() + timeout
        held = []
        found = None
        while _time.monotonic() < deadline:
            try:
                msg = self._recv_control()
            except _queue.Empty:
                # A quiet gap is not the end of the stream: the adapter may emit the
                # update after a short delay. Keep waiting until OUR deadline, not the
                # queue's — collapsing the two is why this check first reported a false
                # negative against an adapter that does send the update.
                continue
            except EOFError:
                break
            if msg.WhichOneof("payload") == "action_status" \
                    and msg.action_status.action_id == action_id \
                    and (states is None or msg.action_status.state in states):
                found = msg.action_status
                break
            held.append(msg)
        self._pushback.extend(held)
        return found

    def _recv_terminal_estop_ack(self, estop_id: str, timeout: float = _ESTOP_TERMINAL_BUDGET_S):
        """The TERMINAL EstopAck for `estop_id`, skipping any STOPPING acks before it.

        C5.5 bounds the FIRST acknowledgement, which the contract permits to be
        ESTOP_STATE_STOPPING ("command received; robot is decelerating"). C5.2 and C14.1
        govern the TERMINAL one, so they must not read the first ack and conclude the
        robot never stopped. Returns (terminal_ack, saw_stopping, elapsed_s); terminal_ack
        is None if none arrived within `timeout`.
        """
        import time as _time
        t0 = _time.monotonic()
        saw_stopping = False
        while _time.monotonic() - t0 < timeout:
            try:
                msg = self.s.safety_in.get(timeout=0.2)
            except queue.Empty:
                continue
            if msg is _SENTINEL:
                break
            if msg.WhichOneof("payload") != "estop_ack":
                continue
            ack = msg.estop_ack
            if ack.estop_id != estop_id:
                continue
            if ack.state == pb.ESTOP_STATE_STOPPING:
                saw_stopping = True
                continue
            return ack, saw_stopping, _time.monotonic() - t0
        return None, saw_stopping, _time.monotonic() - t0

    def _recv_safety(self):
        item = self.s.safety_in.get(timeout=_GET_TIMEOUT)
        if item is _SENTINEL:
            raise EOFError("safety stream closed by adapter")
        return item

    def _send_command(self, **command_kwargs):
        cid = self._next_command_id
        self._next_command_id += 1
        cmd = pb.Command(command_id=cid, robot_id=ROBOT, **command_kwargs)
        self.s.control_out.put(pb.ControlPlaneMessage(command=cmd))
        return cid

    def _await_result(self, command_id: int):
        """Read control-plane messages until the matching CommandResult."""
        # Heartbeat replies and telemetry are noise while waiting and are dropped. An
        # action_status is NOT: a conformant adapter emits it on accepting an assignment,
        # i.e. BEFORE the command_result this loop waits for, so discarding it would make
        # a correctly-behaving adapter look silent to C16.1. Held aside and re-queued only
        # on return — appending to _pushback inside the loop would be re-read immediately
        # by _recv_control and spin forever.
        held = []
        try:
            while True:
                msg = self._recv_control()
                kind = msg.WhichOneof("payload")
                if kind == "command_result" and msg.command_result.command_id == command_id:
                    return msg.command_result
                if kind == "action_status":
                    held.append(msg)
        finally:
            self._pushback.extend(held)

    def _await_result_collecting(self, command_id: int, collect_arm: str):
        """Await the matching CommandResult, collecting any messages of collect_arm
        (e.g. update_progress) that arrive before it. Returns (collected, result);
        result is None if the stream closes first."""
        collected = []
        while True:
            try:
                msg = self._recv_control()
            except (EOFError, queue.Empty):
                return collected, None
            kind = msg.WhichOneof("payload")
            if kind == collect_arm:
                collected.append(getattr(msg, collect_arm))
            elif kind == "command_result" and msg.command_result.command_id == command_id:
                return collected, msg.command_result

    # -- checks ---------------------------------------------------------------

    def run(self) -> None:
        got_control = self.s.control_established.wait(_STREAM_TIMEOUT)
        got_safety = self.s.safety_established.wait(_STREAM_TIMEOUT)

        # C1 — Connection and handshake
        if got_control and got_safety:
            self.r.add("C1.1", "Open ControlStream and SafetyStream together",
                       CheckStatus.PASS)
        else:
            self.r.add("C1.1", "Open ControlStream and SafetyStream together",
                       CheckStatus.FAIL, detail=(
                           f"control={got_control} safety={got_safety} within "
                           f"{_STREAM_TIMEOUT}s"))
            return  # nothing else is testable without both streams

        try:
            first = self._recv_control()
        except (EOFError, queue.Empty):
            self.r.add("C1.2", "AdapterHello is the first message",
                       CheckStatus.FAIL, detail="no message received")
            return
        if first.WhichOneof("payload") == "hello":
            proto_ok = first.hello.protocol_version == "fleet_adapter.v1"
            self.r.add("C1.2", "AdapterHello is the first message",
                       CheckStatus.PASS if proto_ok else CheckStatus.FAIL,
                       detail="" if proto_ok else
                       f"protocol_version={first.hello.protocol_version!r}")
        else:
            self.r.add("C1.2", "AdapterHello is the first message",
                       CheckStatus.FAIL,
                       detail=f"first payload was {first.WhichOneof('payload')!r}")
            return

        # C13.1 — the hello must report a contract_version this suite attests against (ADR-0032).
        # An adapter that reports none, or one out of range, would connect against a real control
        # plane and then have every registration refused VERSION_MISMATCH, so failing here is the
        # suite telling the author about that outage BEFORE they ship.
        reported = first.hello.contract_version
        contract_ok, why = _supports_contract(reported)
        self._contract_ok = contract_ok
        self.r.add("C13.1", "AdapterHello reports a supported contract_version",
                   CheckStatus.PASS if contract_ok else CheckStatus.FAIL,
                   detail=(f"contract_version={reported!r}" if contract_ok else why))

        # Accept the handshake so the adapter proceeds. negotiated_contract_version is set ONLY when
        # the reported version is in range — an empty value on the wire always means "no compatible
        # contract was agreed", never "compatible but unrecorded".
        ack = pb.HelloAck(accepted=True, negotiated_protocol_version="fleet_adapter.v1")
        if contract_ok:
            ack.negotiated_contract_version = CONTRACT_VERSION
        self.s.control_out.put(pb.ControlPlaneMessage(hello_ack=ack))
        # NOT an adapter result: negotiated_contract_version is a field the HARNESS just set
        # on its own HelloAck, so asserting it asserts the harness. Recorded as harness-side
        # context under a C0 id so the report still shows what was negotiated, without
        # counting a self-fulfilling row toward the adapter's verdict.
        self.r.add("C0.2", "Harness context: negotiated contract version on HelloAck",
                   CheckStatus.PASS if ack.negotiated_contract_version else CheckStatus.SKIP,
                   level="MAY",
                   detail=(f"negotiated_contract_version={ack.negotiated_contract_version!r} "
                           f"(harness-set)" if ack.negotiated_contract_version else
                           SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                           + ": the adapter reported no supported contract_version, so "
                           "there was no version to negotiate"))

        # C2 — Robot lifecycle: adapter-initiated. If the adapter registers, the
        # harness advertises an edge endpoint in the RegisterAck (feeds C8).
        advertised_edge = self._maybe_handle_registration()

        self._check_fencing()          # C3
        self._check_cancel_disposition()  # C3.6
        self._check_leases()           # C4
        self._check_action_status()    # C16
        self._check_estop()            # C5
        self._check_telemetry()        # C6
        self._check_optional_decline() # C7
        self._check_edge(advertised_edge)  # C8
        self._check_verify_metrics()   # C9
        self._check_update_progress()  # C10
        self._check_pause_resume()     # C11
        self._check_scan()             # C12
        self._check_artifact_verification()  # C7.2 (model) + C7.3 (firmware body)
        self._check_version_bound()    # C15 (no adapter traffic; safe before the drop)
        # An ACCEPTING reconnect, before the refusing one below. C6.1 and C2.2 are both
        # about what happens on a SUCCESSFUL re-registration, which the version-refusal
        # path cannot exercise because it refuses by design.
        self._check_accepting_reconnect()  # C6.1 + C2.2
        # LAST — C13.2/C14.1 drop the ControlStream, so nothing that needs it may follow.
        self._check_negotiation_refusal()  # C13.2 + C14.1
        self._record_unverified()      # documented-but-unexercised IDs, stated explicitly

    def _check_fencing(self) -> None:
        # C3.3 — absent token must be rejected as MISSING (not treated as 0).
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id="t-missing", assignment_id="a-missing"))
        res = self._await_result(cid).assign_action
        ok = (not res.accepted
              and res.rejection == pb.ASSIGN_ACTION_REJECTION_MISSING_FENCING_TOKEN)
        self.r.add("C3.3", "Reject AssignAction with absent fencing token (MISSING)",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail="" if ok else f"accepted={res.accepted} rejection={res.rejection}")

        # C3.2 (accept) + C3.5 — accept a fresh token and echo it back.
        # ONE payload, reused BYTE-IDENTICALLY by the C3.4 re-delivery below: C3.4 asserts
        # that an identical re-delivery is re-acked rather than treated as stale, so the two
        # AssignActions must not differ in any field. ~2 s of window -> 4 s of travel.
        t1_payload = self._goals.payload(4.0)
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id="t1", assignment_id="a1", fencing_token=5, payload_json=t1_payload))
        res = self._await_result(cid).assign_action
        accept_ok = res.accepted
        echo_ok = res.HasField("accepted_fencing_token") and res.accepted_fencing_token == 5
        # A PRECONDITION, not the catalog's C3.2 (which is the stale rejection only): a stale
        # token cannot be demonstrated without a high-water mark for it to be stale against.
        # Structurally identical to C0.3 for the lease. See CONFORMANCE.md "C0.x — harness
        # context".
        self.r.add("C0.4", "Harness context: a fresh (higher) fencing token is accepted",
                   CheckStatus.PASS if accept_ok else CheckStatus.FAIL, level="MAY",
                   detail="" if accept_ok else f"accepted={res.accepted}")
        self.r.add("C3.5", "Echo the fenced token in AssignActionResult",
                   CheckStatus.PASS if echo_ok else CheckStatus.FAIL,
                   detail="" if echo_ok else "accepted_fencing_token not echoed")

        # C3.2 (stale) — a token <= the highest accepted must be rejected STALE.
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id="t-stale", assignment_id="a-stale", fencing_token=3))
        res = self._await_result(cid).assign_action
        stale_ok = (not res.accepted
                    and res.rejection == pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN)
        self.r.add("C3.2", "Reject a stale (<= highest) fencing token (STALE)",
                   CheckStatus.PASS if stale_ok else CheckStatus.FAIL,
                   detail="" if stale_ok else f"accepted={res.accepted} rejection={res.rejection}")

        # C3.4 — identical re-delivery of the current assignment must be
        # idempotently re-acked, NOT treated as stale.
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id="t1", assignment_id="a1", fencing_token=5, payload_json=t1_payload))
        res = self._await_result(cid).assign_action
        redeliver_ok = res.accepted
        self.r.add("C3.4", "Idempotently re-ack identical re-delivery",
                   CheckStatus.PASS if redeliver_ok else CheckStatus.FAIL,
                   detail="" if redeliver_ok else (
                       "identical re-delivery of the current assignment was "
                       "rejected instead of re-acked"))

    def _check_cancel_disposition(self) -> None:
        # C3.6 — cancel_action reports a CancelDisposition (capability-loss
        # reassignment). The field is additive: UNSPECIFIED is treated by the
        # control plane as STOPPED_SAFELY, so a minimal adapter is compliant; an
        # adapter that distinguishes safe-stop / completed / recovered reports the
        # specific value. Uses the token accepted in _check_fencing (t1 @ 5).
        cid = self._send_command(cancel_action=pb.CancelAction(
            action_id="t1", reason="capability-loss", fencing_token=5))
        res = self._await_result(cid).cancel_action
        valid = {
            pb.CANCEL_DISPOSITION_UNSPECIFIED,
            pb.CANCEL_DISPOSITION_STOPPED_SAFELY,
            pb.CANCEL_DISPOSITION_COMPLETED,
            pb.CANCEL_DISPOSITION_RECOVERED,
        }
        ok = res.acknowledged and res.disposition in valid
        if ok and res.disposition == pb.CANCEL_DISPOSITION_UNSPECIFIED:
            detail = "disposition UNSPECIFIED (treated as STOPPED_SAFELY — compliant)"
        elif ok:
            detail = f"disposition={pb.CancelDisposition.Name(res.disposition)}"
        else:
            detail = f"acknowledged={res.acknowledged} disposition={res.disposition}"
        self.r.add("C3.6", "cancel_action reports a valid CancelDisposition",
                   CheckStatus.PASS if ok else CheckStatus.FAIL, detail=detail)

    # Documented in adapters/CONFORMANCE.md but not exercised by this suite. Recorded as
    # explicit skips with the reason, so a CONFORMANT verdict enumerates what it did NOT
    # show rather than staying silent about it. Silence is what let a third of the catalog
    # go unverified without anyone noticing.
    _UNVERIFIED = [
        ("C1.3", "Present a client certificate and treat that identity as authoritative", "MUST",
         SkipCause.DEPLOYMENT_PROPERTY,
         "the harness calls add_insecure_port; mTLS identity is asserted in the control "
         "plane's own ControlStream tests"),
        ("C1.4", "Do not declare healthy when SafetyStream cannot be established", "MUST NOT",
         SkipCause.NOT_OBSERVABLE_ON_WIRE,
         "an adapter's own health signal is not a protocol concept"),
        ("C1.5", "Reconnect with bounded exponential backoff on stream loss", "SHOULD",
         SkipCause.NEEDS_EXTENDED_RUN,
         "one faulted stream shows reconnect (C13.2) but not the backoff curve"),
    ]

    def _record_unverified(self) -> None:
        for cid, title, level, cause, note in self._UNVERIFIED:
            assert isinstance(cause, SkipCause), f"{cid}: skip cause must be enumerated"
            detail = cause.value if not note else f"{cause.value}: {note}"
            self.r.add(cid, title, CheckStatus.SKIP, level=level, detail=detail)
        # A cause is only worth enumerating if it can be contradicted. NO_TELEMETRY_IN_RUN
        # is the one that is checkable from inside the run, so check it: recording it while
        # telemetry WAS observed would mean the reason is false.
        # Record the OUTCOME either way. A check that only ever speaks on failure is
        # indistinguishable, in a clean report, from a check that never ran — and this one's
        # own failure mode is silence: it is guarded on a flag whose absence would disable it
        # rather than fail it. A reader auditing what the suite verified needs the pass.
        # Read the causes actually RECORDED on the report, not the static _UNVERIFIED
        # declarations. NO_TELEMETRY_IN_RUN is recorded dynamically by _check_telemetry and
        # never appears in _UNVERIFIED at all, so the old contradiction test could not fire.
        recorded = self.r.skip_causes()
        claimed = [cid for cid, cause in recorded
                   if cause == SkipCause.NO_TELEMETRY_IN_RUN.value]
        title = "Skip reasons are consistent with the run"
        if self.telemetry_seen and claimed:
            self.r.add("C0.1", title, CheckStatus.FAIL,
                       detail=(f"{', '.join(claimed)} recorded {SkipCause.NO_TELEMETRY_IN_RUN.value} "
                               f"but telemetry WAS observed; the stated reason is false"))
        else:
            observed = "telemetry observed" if self.telemetry_seen else "no telemetry in run"
            self.r.add("C0.1", title, CheckStatus.PASS,
                       detail=(f"{len(recorded)} recorded skip cause(s) checked against what the run "
                               f"observed ({observed}); none contradicted"))

    def _check_leases(self) -> None:
        # C4.2 — the lease self-stop IS observable, because the checklist requires the
        # adapter to emit an action_status reflecting the stop. Assign under a short
        # lease, stop renewing, and wait for the robot to report it. This is the primary
        # dual-execution safeguard (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee);
        # certifying an adapter without exercising it was the gap this check closes.
        aid = "t-lease-expiry"
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id=aid, assignment_id="a-lease", fencing_token=11,
            payload_json=self._goals.payload(8.0),   # C4.2 waits 4 s -> 8 s of travel
            lease_generation=1, lease_duration_ms=500))
        res = self._await_result(cid).assign_action
        if not res.accepted:
            self.r.add("C4.2", "Self-stop when the assignment lease is not renewed",
                       CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: adapter refused the lease-bearing assignment; cannot exercise expiry")
            return
        # Deliberately send NO renew_lease. The lease must lapse on the robot's timer.
        terminal = (pb.ACTION_STATE_STOPPED, pb.ACTION_STATE_FAILED, pb.ACTION_STATE_CANCELLED)
        upd = self._await_action_status(aid, timeout=4.0, states=terminal)
        stopped = upd is not None
        self.r.add("C4.2", "Self-stop when the assignment lease is not renewed",
                   CheckStatus.PASS if stopped else CheckStatus.FAIL,
                   detail=("" if stopped else
                           "no terminal action_status within 4s of lease expiry; the stop is "
                           "unobservable and dual execution cannot be ruled out"))

        # C4.1 — the complement of C4.2: a lease that KEEPS being renewed is HELD, and
        # the robot goes on executing. C4.2 alone would pass on an adapter that stops on a
        # fixed timer regardless of renewals — a robot abandoning live work, the same
        # dual-execution hazard from the other side.
        #
        # Asserted through the adapter's DECLARED state (`running`) rather than through
        # the absence of a stop message. Absence-of-signal is a race against the lease
        # clock: an observation window wide enough to be meaningful is also wide enough
        # for the lease to lapse legitimately inside it, which reports a correct adapter
        # as faulty. `running` is a positive, timing-independent claim by the adapter that
        # it still holds the task.
        aid_live = "t-lease-held"
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id=aid_live, assignment_id="a-held", fencing_token=12,
            payload_json=self._goals.payload(4.0),   # C4.1 renews for ~1 s -> 4 s of travel
            lease_generation=3, lease_duration_ms=2000))
        if self._await_result(cid).assign_action.accepted:
            held = True
            for _ in range(4):
                time.sleep(0.2)
                rc = self._send_command(renew_lease=pb.RenewActionLease(
                    action_id=aid_live, lease_generation=3, lease_duration_ms=2000))
                rr = self._await_result(rc).renew_lease
                if not (rr.renewed and rr.running):
                    held = False
                    detail = (f"after a renewal the adapter reported renewed={rr.renewed} "
                              f"running={rr.running}; it no longer considers the renewed "
                              f"task its own")
                    break
            self.r.add("C4.1", "Reports the task still running across lease renewals",
                       CheckStatus.PASS if held else CheckStatus.FAIL,
                       detail="" if held else detail)
        else:
            self.r.add("C4.1", "Reports the task still running across lease renewals",
                       CheckStatus.SKIP,
                       detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)

        # C4.3 — renew_lease reply semantics: a matching generation renews.
        aid2 = "t-renew"
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id=aid2, assignment_id="a-renew", fencing_token=13,
            payload_json=self._goals.payload(4.0),   # C4.3/C4.4/C0.3 ~2 s -> 4 s of travel
            lease_generation=7, lease_duration_ms=5000))
        if self._await_result(cid).assign_action.accepted:
            cid = self._send_command(renew_lease=pb.RenewActionLease(
                action_id=aid2, lease_generation=7, lease_duration_ms=5000))
            rr = self._await_result(cid).renew_lease
            # Renewing a MATCHING generation is the precondition for the two catalog
            # checks below, not a catalog check itself — recorded as harness context.
            self.r.add("C0.3", "Harness context: a matching lease generation renews",
                       CheckStatus.PASS if rr.renewed else CheckStatus.FAIL, level="MAY",
                       detail="" if rr.renewed else f"renewed={rr.renewed} running={rr.running}")
            # C4.3 (catalog): IGNORE a RenewActionLease whose lease_generation is stale.
            # A superseded lease must not be revivable.
            cid = self._send_command(renew_lease=pb.RenewActionLease(
                action_id=aid2, lease_generation=1, lease_duration_ms=5000))
            rs = self._await_result(cid).renew_lease
            self.r.add("C4.3", "Ignore a RenewActionLease with a stale lease_generation",
                       CheckStatus.PASS if not rs.renewed else CheckStatus.FAIL,
                       detail="" if not rs.renewed else "a stale generation was renewed")
            # C4.4 (catalog): report running=false once the robot is no longer executing,
            # so a silent completion during a renewal gap is detectable. End the action,
            # then renew at the CURRENT generation and require running=false.
            self._send_command(cancel_action=pb.CancelAction(action_id=aid2))
            time.sleep(0.5)
            cid = self._send_command(renew_lease=pb.RenewActionLease(
                action_id=aid2, lease_generation=7, lease_duration_ms=5000))
            re_ = self._await_result(cid).renew_lease
            self.r.add("C4.4", "Report running=false once the task is no longer executing",
                       CheckStatus.PASS if not re_.running else CheckStatus.FAIL,
                       detail=("" if not re_.running else
                               "renew_lease still reported running=true after the action was "
                               "cancelled; a silent completion would be undetectable"))
        else:
            for cid_, name in (("C4.3", "renew_lease renews a matching lease generation"),
                               ("C4.4", "renew_lease rejects a stale lease generation")):
                self.r.add(cid_, name, CheckStatus.SKIP,
                           detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: adapter refused the lease-bearing assignment")

    def _check_action_status(self) -> None:
        # C16.1 — ActionStatusUpdate at a task phase transition. Required, and until now
        # untested: the control plane drives FleetAction Assigned -> InProgress from it.
        aid = "t-status"
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id=aid, assignment_id="a-status", fencing_token=14,
            payload_json=self._goals.payload(6.0)))  # C16.1 waits 3 s -> 6 s of travel
        if not self._await_result(cid).assign_action.accepted:
            self.r.add("C16.1", "ActionStatusUpdate sent at a task phase transition",
                       CheckStatus.SKIP, detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: adapter refused the assignment")
            return
        upd = self._await_action_status(aid, timeout=3.0)
        self.r.add("C16.1", "ActionStatusUpdate sent at a task phase transition",
                   CheckStatus.PASS if upd is not None else CheckStatus.FAIL,
                   detail=("state=" + pb.ActionState.Name(upd.state)) if upd
                          else "no action_status observed after an accepted assignment")
        # C6.4 — the update MUST echo the token the robot is executing under, so the
        # control plane can spot a robot still acting on a superseded assignment. The
        # token accepted for this action was 13.
        if upd is None:
            self.r.add("C6.4", "Echo the executing fencing_token in ActionStatusUpdate",
                       CheckStatus.SKIP,
                       detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)
        else:
            echoed = upd.fencing_token == 14
            self.r.add("C6.4", "Echo the executing fencing_token in ActionStatusUpdate",
                       CheckStatus.PASS if echoed else CheckStatus.FAIL,
                       detail="" if echoed else (
                           f"fencing_token={upd.fencing_token}, want 14 (the accepted token); "
                           f"without it a superseded assignment is undetectable"))

    def _check_estop(self) -> None:
        # C5.1 / C5.2 — send Estop on SafetyStream, expect a confirmed EstopAck.
        # C5.5 times that exchange against the 500 ms hard requirement at
        # fleet-adapter-protocol.md's performance table. Measured from the moment the
        # Estop is queued to the moment the ack is dequeued, so the number includes the
        # harness's own transport and is therefore conservative (never flattering).
        _estop_sent_at = time.monotonic()
        self.s.safety_out.put(pb.ControlPlaneSafetyMessage(
            robot_id=ROBOT,
            estop=pb.Estop(estop_id="e1", reason="conformance", issued_by="harness")))
        try:
            msg = self._recv_safety()
        except (EOFError, queue.Empty):
            self.r.add("C5.2", "Confirmed EstopAck (STOPPED) on SafetyStream",
                       CheckStatus.FAIL, detail="no EstopAck received")
            self.r.add("C5.5", "EstopAck within the 500 ms SafetyStream budget",
                       CheckStatus.FAIL, detail="no EstopAck arrived, so the budget cannot be met")
            return
        _estop_latency_ms = (time.monotonic() - _estop_sent_at) * 1000.0
        _within = _estop_latency_ms <= _ESTOP_ACK_BUDGET_MS
        self.r.add("C5.5", "EstopAck within the 500 ms SafetyStream budget",
                   CheckStatus.PASS if _within else CheckStatus.FAIL,
                   detail=(f"measured {_estop_latency_ms:.0f} ms "
                           f"(budget {_ESTOP_ACK_BUDGET_MS:.0f} ms, harness transport included)"))
        if msg.WhichOneof("payload") == "estop_ack":
            ack = msg.estop_ack
            # C5.1 — the estop was accepted on SafetyStream and acted on: an ack for the
            # estop_id we sent came back on the same stream.
            self.r.add("C5.1", "Accept Estop on SafetyStream and act on it",
                       CheckStatus.PASS if ack.estop_id == "e1" else CheckStatus.FAIL,
                       detail="" if ack.estop_id == "e1" else f"estop_id={ack.estop_id!r}")
            # C5.2 governs the TERMINAL ack. The FIRST one may legitimately be
            # ESTOP_STATE_STOPPING ("command received; robot is decelerating") -- C5.5
            # bounds that one, this bounds the confirmation that follows it.
            if ack.state == pb.ESTOP_STATE_STOPPING:
                term, saw_stopping, waited = self._recv_terminal_estop_ack("e1")
                saw_stopping = True
            else:
                term, saw_stopping, waited = ack, False, 0.0
            ok = term is not None and term.estop_id == "e1" \
                and term.state == pb.ESTOP_STATE_STOPPED
            self.r.add("C5.2", "Confirmed EstopAck (STOPPED) on SafetyStream",
                       CheckStatus.PASS if ok else CheckStatus.FAIL,
                       detail=("terminal ack STOPPED"
                               + (f" after a STOPPING ack, {waited * 1000:.0f} ms later"
                                  if saw_stopping else " on the first ack")) if ok else
                              (f"no terminal EstopAck within {_ESTOP_TERMINAL_BUDGET_S:.0f}s "
                               f"of a STOPPING ack" if term is None else
                               f"estop_id={term.estop_id!r} state={term.state}"))

            # C5.6 — a STOPPING ack MUST reach a terminal state. Acking STOPPING and never
            # following up satisfies C5.5 while telling the control plane nothing: the robot
            # is left in an unresolved emergency and dual execution cannot be ruled out.
            # Only meaningful when the adapter actually used the two-phase shape.
            if saw_stopping:
                reached = term is not None and term.state in (pb.ESTOP_STATE_STOPPED,
                                                              pb.ESTOP_STATE_FAILED)
                self.r.add("C5.6", "A STOPPING EstopAck reaches a terminal state",
                           CheckStatus.PASS if reached else CheckStatus.FAIL,
                           detail=(f"terminal {pb.EstopState.Name(term.state)} "
                                   f"{waited * 1000:.0f} ms after STOPPING. BOUND: "
                                   f"{_ESTOP_TERMINAL_BUDGET_S:.0f}s is a HARNESS constant, "
                                   f"not a contract figure -- the real bound is a property of "
                                   f"the base and belongs in the adapter's own declaration")
                                  if reached else
                                  (f"STOPPING was acked but no terminal STOPPED/FAILED "
                                   f"followed within {_ESTOP_TERMINAL_BUDGET_S:.0f}s; the "
                                   f"control plane is left in an unresolved emergency"))
            else:
                self.r.add("C5.6", "A STOPPING EstopAck reaches a terminal state",
                           CheckStatus.SKIP, level="MAY",
                           detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                                   + ": the adapter answered with a terminal ack directly and "
                                     "never sent STOPPING, which is equally conformant"))
            ack = term if term is not None else ack
            # C5.3 — STOPPED must be adapter-CONFIRMED, never inferred from absent
            # motion or a timeout. Whether an adapter inferred it is a property of its
            # internal logic and is not decidable from the wire, so this asserts the
            # strongest available evidence: stop_initiated_at, the timestamp at which
            # the adapter issued the hardware stop. An adapter that inferred the stop
            # has no such moment to report. NECESSARY, NOT SUFFICIENT — recorded here
            # so a CONFORMANT verdict does not imply more than was actually shown.
            attested = ack.HasField("stop_initiated_at")
            self.r.add("C5.3", "STOPPED is adapter-confirmed, not inferred",
                       CheckStatus.PASS if attested else CheckStatus.FAIL,
                       detail=("attested via stop_initiated_at (necessary evidence, not "
                               "proof that no inference occurred)") if attested else
                              ("EstopAck reported STOPPED with no stop_initiated_at, so "
                               "nothing attests a hardware stop was actually commanded"))
        else:
            self.r.add("C5.2", "Confirmed EstopAck (STOPPED) on SafetyStream",
                       CheckStatus.FAIL,
                       detail=f"safety payload was {msg.WhichOneof('payload')!r}")

        # C5.4 — estop MUST NOT be carried on ControlStream. Actively probe it: push a
        # Command.estop down the control path and require the adapter to ignore it. The
        # previous version sent nothing there and then confirmed nothing came back, which
        # is a tautology — it passed for an adapter that would happily answer an estop on
        # the wrong stream.
        self._send_command(estop=pb.Estop(
            estop_id="e-control-probe", reason="conformance: estop on the WRONG stream",
            issued_by="harness"))
        time.sleep(1.0)  # give a non-conformant adapter time to answer on the control path
        leaked = False
        try:
            while True:
                m = self.s.control_in.get_nowait()
                if m is _SENTINEL:
                    break
                if m.WhichOneof("payload") == "command_result" and \
                        m.command_result.WhichOneof("result") == "estop":
                    leaked = True
        except queue.Empty:
            pass
        self.r.add("C5.4", "Estop traffic not carried on ControlStream",
                   CheckStatus.FAIL if leaked else CheckStatus.PASS, level="MUST NOT",
                   detail=("adapter answered a Command.estop pushed down ControlStream; estop is "
                           "SafetyStream-only" if leaked else
                           "a Command.estop pushed down ControlStream drew no response"))

    def _await_telemetry(self, timeout: float = 6.0):
        """Drain for a TelemetryPayload. Non-matching traffic is pushed back."""
        import time as _time
        deadline = _time.monotonic() + timeout
        held, found = [], None
        while _time.monotonic() < deadline:
            try:
                msg = self._recv_control()
            except queue.Empty:
                continue
            except EOFError:
                break
            if msg.WhichOneof("payload") == "telemetry":
                found = msg.telemetry
                break
            held.append(msg)
        self._pushback.extend(held)
        return found

    def _check_telemetry(self) -> None:
        payload = self._await_telemetry()
        if payload is None:
            # A template that registers no robot streams nothing. Enumerated cause, so a
            # later contradiction check can catch this being claimed falsely.
            for cid, title in (("C6.3", "No Robot status write per telemetry tick (RA-1)"),
                               ("C6.2", "Preserve proto3 explicit presence on safety scalars")):
                self.r.add(cid, title, CheckStatus.SKIP,
                           detail=SkipCause.NO_TELEMETRY_IN_RUN.value)
            return
        self.telemetry_seen = True
        self._hardware_baseline = len(payload.hardware)

        # C6.2 — explicit presence. `floor` is declared `optional int32` precisely because
        # 0 (ground) is a VALID value that must be distinguishable from "not reported": a
        # dropped field deserialising to 0 would silently claim the robot is on the ground.
        # Assert presence on the zero-valued scalar, which is the only case that proves it.
        # `floor` and `altitude` are FRAME-EXCLUSIVE (fleet-adapter-protocol.md:280): a
        # ground robot reports floor, an aerial one reports altitude and no floor at all.
        # Prove presence on whichever frame scalar the adapter actually reports; failing an
        # aerial adapter for the absence of `floor` would contradict the spec's own rule.
        pos = payload.position
        floor_present = pos.HasField("floor")
        alt_present = pos.HasField("altitude")
        if floor_present and pos.floor == 0:
            detail = "floor=0 sent WITH explicit presence (absent != ground is preserved)"
            status = CheckStatus.PASS
        elif alt_present and pos.altitude == 0:
            detail = "altitude=0 sent WITH explicit presence (absent != sea level is preserved)"
            status = CheckStatus.PASS
        elif floor_present:
            detail = f"floor={pos.floor} present, but non-zero — presence is only proven at 0"
            status = CheckStatus.PASS
        elif alt_present:
            detail = f"altitude={pos.altitude} present, but non-zero — presence is only proven at 0"
            status = CheckStatus.PASS
        else:
            detail = ("position carries neither an explicit floor nor an explicit altitude; "
                      "a dropped frame scalar is indistinguishable from ground/sea level")
            status = CheckStatus.FAIL
        self.r.add("C6.2", "Preserve proto3 explicit presence on safety scalars", status,
                   detail=detail)

        # C6.3 — RA-1 ("no Robot status write per telemetry tick") is a CONTROL-PLANE
        # discipline. Nothing an adapter puts on the wire can demonstrate it, so this is
        # recorded SKIP with an enumerated cause naming where it IS asserted. It was a
        # bare PASS, which reported a MUST NOT as verified when nothing had tested it —
        # the single most misleading row a conformance report can carry.
        self.r.add("C6.3", "No Robot status write per telemetry tick (RA-1)",
                   CheckStatus.SKIP, level="MUST NOT",
                   detail=(SkipCause.NOT_OBSERVABLE_ON_WIRE.value
                           + ": RA-1 governs the control plane's write policy, not adapter "
                             "output; asserted in internal/controller's telemetry projection "
                             "tests. Telemetry was observed, so the projection path is live."))

    def _maybe_handle_registration(self) -> bool:
        # C2.1 — peek for an adapter-initiated Register/Discover. If one arrives,
        # ack it and advertise an edge endpoint (so an edge-capable adapter reaches
        # C8). A minimal template registers nothing → C2.1 skip; anything else read
        # here is pushed back so the fencing checks still see it.
        try:
            msg = self.s.control_in.get(timeout=_REGISTER_WAIT)
        except queue.Empty:
            self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                       CheckStatus.SKIP, detail=(
                           SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                           + ": adapter initiated no registration in this run "
                           "(expected for a minimal/template adapter)"))
            return False
        if msg is _SENTINEL:
            self._pushback.append(_SENTINEL)
            self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                       CheckStatus.SKIP, detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: control stream closed before registration")
            return False

        kind = msg.WhichOneof("payload")
        edge = pb.EdgeEndpoint(zone="zone-conformance", address=f"localhost:{self.port}")
        if kind == "register":
            self.s.control_out.put(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                accepted=True, telemetry_interval_seconds=1, edge_endpoints=[edge])))  # 1s: the
                # suite now MEASURES adoption of this value (C2.3), and a 10s cadence
                # would starve every downstream check that waits on telemetry.
            self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                       CheckStatus.PASS,
                       detail="RegisterRobot for an admitted robot is acked")
            return True
        if kind == "discover":
            self.s.control_out.put(pb.ControlPlaneMessage(discover_ack=pb.DiscoverAck(
                accepted=True, discovered_robot_name="dr-conformance")))
            self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                       CheckStatus.PASS,
                       detail="DiscoverRobot for an unadmitted robot is acked; edge endpoints "
                              "are advertised on RegisterAck, not DiscoverAck")
            return False
        # Not a registration — return it to the stream for the fencing checks.
        self._pushback.append(msg)
        self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                   CheckStatus.SKIP, detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: adapter initiated no registration")
        return False

    def _check_optional_decline(self) -> None:
        # C7.1 — an optional command the adapter does not implement MUST be declined
        # with unsupported=true (never a faked result). An adapter that DOES
        # implement it returns a real VerifyResult; either is conformant.
        cid = self._send_command(verify_hardware=pb.VerifyHardware(component_name="lidar-360"))
        try:
            res = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C7.1", "Decline optional commands via unsupported=true",
                       CheckStatus.SKIP, level="MAY",
                       detail=f"{SkipCause.OPTIONAL_DECLINED.value}: adapter sent no CommandResult for the optional command")
            return
        declined = res.unsupported
        answered = res.WhichOneof("result") == "verify"
        ok = declined or answered
        self.r.add("C7.1", "Decline optional commands via unsupported=true",
                   CheckStatus.PASS if ok else CheckStatus.FAIL, level="MAY",
                   detail="" if ok else (
                       "optional command was neither declined (unsupported=true) nor "
                       "answered with a VerifyResult"))

    def _check_verify_metrics(self) -> None:
        # C9.1 — an adapter that ANSWERS a verify_* probe SHOULD return actualMetrics
        # (§6.10), so RobotProbe.status.robotResults[].actualMetrics is populated.
        # MAY-level: declining verify_* is conformant (C7), so a decline is a SKIP.
        cid = self._send_command(verify_hardware=pb.VerifyHardware(
            component_name="lidar-360", expected_metrics={"scanHz": 10.0}))
        try:
            res = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C9.1", "verify_* answers with actualMetrics", CheckStatus.SKIP,
                       level="MAY", detail=f"{SkipCause.OPTIONAL_DECLINED.value}: no CommandResult for the optional verify command")
            return
        if res.unsupported or res.WhichOneof("result") != "verify":
            self.r.add("C9.1", "verify_* answers with actualMetrics", CheckStatus.SKIP,
                       level="MAY", detail=f"{SkipCause.OPTIONAL_DECLINED.value}: adapter declined verify_* (conformant; see C7)")
            return
        ok = len(res.verify.actual_metrics) > 0
        self.r.add("C9.1", "verify_* answers with actualMetrics",
                   CheckStatus.PASS if ok else CheckStatus.FAIL, level="MAY",
                   detail="" if ok else "VerifyResult answered but carried no actualMetrics")

    def _check_update_progress(self) -> None:
        # C10.1 — an adapter that ACCEPTS push_firmware MAY emit advisory
        # UpdateProgress before the FirmwareResult (§6.6), feeding
        # FirmwareRollout.status.currentBatch[].updatePhase. Fully optional: a decline
        # (C7) or an accept with no progress are both conformant, so this only PASSES
        # when progress is emitted and never FAILS a conformant adapter.
        cid = self._send_command(push_firmware=pb.PushFirmware(target_version="0.0.0-conformance"))
        progress, res = self._await_result_collecting(cid, "update_progress")
        if res is None:
            self.r.add("C10.1", "push_firmware may emit UpdateProgress", CheckStatus.SKIP,
                       level="MAY", detail=f"{SkipCause.OPTIONAL_DECLINED.value}: no CommandResult for the optional firmware command")
            return
        if res.unsupported:
            self.r.add("C10.1", "push_firmware may emit UpdateProgress", CheckStatus.SKIP,
                       level="MAY", detail=f"{SkipCause.OPTIONAL_DECLINED.value}: adapter declined push_firmware (conformant; see C7)")
            return
        emitted = [p for p in progress if p.robot_id == ROBOT and p.phase]
        if emitted:
            self.r.add("C10.1", "push_firmware may emit UpdateProgress", CheckStatus.PASS,
                       level="MAY", detail=f"phases: {[p.phase for p in emitted]}")
        elif not res.push_firmware.accepted:
            # The probe carries no checksum, so a C7.3-compliant adapter REFUSES it and never
            # reaches the progress path. That is correct behaviour, not a missing feature — and the
            # harness cannot supply a valid checksum here, because only the adapter knows what bytes
            # it would receive. Recorded distinctly so this never reads as "accepted but silent".
            self.r.add("C10.1", "push_firmware may emit UpdateProgress", CheckStatus.SKIP,
                       level="MAY", detail=(
                           SkipCause.OPTIONAL_DECLINED.value
                           + ": adapter refused the unverifiable probe artifact (C7.3 body "
                           "re-verify), so the optional progress path was not reachable: "
                           + res.push_firmware.message))
        else:
            self.r.add("C10.1", "push_firmware may emit UpdateProgress", CheckStatus.SKIP,
                       level="MAY", detail=(SkipCause.OPTIONAL_DECLINED.value
                                    + ": accepted firmware without advisory progress, which the MAY permits"))

        # C10.2 (MUST, when implemented) — an adapter that ANSWERS push_firmware must report a
        # terminal outcome (ADR-0033). Advisory phases are optional; the terminal report is not.
        #
        # The obligation is conditional on the capability, so a decline SKIPs above and never
        # reaches here. But note what is NOT excused: a REFUSAL is still terminal. The probe
        # carries an unmatchable checksum, so a C7.3-compliant adapter refuses it — and that
        # refusal must be reported as INSTALL_OUTCOME_FAILED, because a rollout that only sees
        # "accepted=false" in the command result is still waiting for an install that will never
        # happen. This is the check that catches an adapter which declines correctly but leaves
        # the rollout wedged.
        terminal = [p for p in progress
                    if p.robot_id == ROBOT and p.outcome != pb.INSTALL_OUTCOME_UNSPECIFIED]
        if terminal:
            t = terminal[0]
            name = ("SUCCEEDED" if t.outcome == pb.INSTALL_OUTCOME_SUCCEEDED else "FAILED")
            ok = bool(t.resulting_version) or t.outcome == pb.INSTALL_OUTCOME_FAILED
            self.r.add("C10.2", "push_firmware reports a terminal install outcome",
                       CheckStatus.PASS if ok else CheckStatus.FAIL, level="MUST",
                       detail=(f"outcome={name} resulting_version={t.resulting_version!r}"
                               if ok else
                               "reported SUCCEEDED without a resulting_version — the control "
                               "plane cannot record what the robot is actually running"))
        else:
            self.r.add("C10.2", "push_firmware reports a terminal install outcome",
                       CheckStatus.FAIL, level="MUST", detail=(
                           "adapter answered push_firmware but never reported a terminal outcome "
                           "(UpdateProgress.outcome). The CommandResult only acknowledges that a "
                           "download began, so the rollout cannot tell a slow install from a "
                           "finished one and will not complete."))

    def _check_pause_resume(self) -> None:
        # C11.1 — an adapter MAY accept pause / resume. An answered pause returns a
        # PauseResult (and resume a ResumeResult); a decline (C7) is conformant → SKIP.
        cid = self._send_command(pause=pb.PauseCapabilities(
            reason="conformance", require_stop_before_ack=False))
        try:
            res = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C11.1", "pause/resume answered with a result", CheckStatus.SKIP,
                       level="MAY", detail=f"{SkipCause.OPTIONAL_DECLINED.value}: no CommandResult for the optional pause command")
            return
        if res.unsupported or res.WhichOneof("result") != "pause":
            self.r.add("C11.1", "pause/resume answered with a result", CheckStatus.SKIP,
                       level="MAY", detail=f"{SkipCause.OPTIONAL_DECLINED.value}: adapter declined pause (conformant; see C7)")
            return
        cid2 = self._send_command(resume=pb.ResumeCapabilities(reason="conformance"))
        try:
            res2 = self._await_result(cid2)
        except (EOFError, queue.Empty):
            res2 = None
        resume_ok = res2 is not None and not res2.unsupported and res2.WhichOneof("result") == "resume"
        self.r.add("C11.1", "pause/resume answered with a result",
                   CheckStatus.PASS if resume_ok else CheckStatus.FAIL, level="MAY",
                   detail="" if resume_ok else "pause was answered but resume was not")

    def _check_scan(self) -> None:
        # C12.1 — an adapter MAY answer a ScanCapabilities request with a full
        # CapabilitiesSnapshot; a decline (C7) is conformant → SKIP.
        cid = self._send_command(scan=pb.ScanCapabilities())
        try:
            res = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C12.1", "scan answers with a CapabilitiesSnapshot", CheckStatus.FAIL,
                       detail="no CommandResult for scan; scan is a required command")
            return
        answered = not res.unsupported and res.WhichOneof("result") == "scan"
        if answered:
            # Kept for C6.1: the full inventory, by NAME, from an INDEPENDENT path. The
            # snapshot is full by definition -- the adapter builds the registration push and
            # this reply from one builder -- so C6.1 becomes an agreement invariant between
            # two paths rather than a self-comparison.
            self._scan_components = {c.name for c in res.scan.hardware}
        self.r.add("C12.1", "scan answers with a CapabilitiesSnapshot",
                   CheckStatus.PASS if answered else CheckStatus.FAIL,
                   detail="" if answered else
                   "adapter declined scan; scan is a required command (RFC-0001 Required-message table)")

    def _check_edge(self, advertised_edge: bool) -> None:
        # C8 — the harness advertised an edge endpoint (on RegisterAck). An
        # edge-capable adapter dials it and honours an edge-issued estop with the
        # same confirmed-EstopAck discipline as SafetyStream.
        if not advertised_edge:
            self.r.add("C8.1", "Adapter dials the advertised edge endpoint",
                       CheckStatus.SKIP, detail=(
                           SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                           + ": adapter registered no robot, so no edge_endpoints were "
                           "advertised (edge is exercised for registering adapters)"))
            return
        if not self.s.edge_established.wait(_EDGE_WAIT):
            # The harness advertised a non-empty edge_endpoints on RegisterAck, which is
            # the "a zone declares an edge node" case in which dialling is REQUIRED. A SKIP
            # here would let an adapter that ignores the advertisement report CONFORMANT.
            self.r.add("C8.1", "Adapter dials the advertised edge endpoint",
                       CheckStatus.FAIL, detail=(
                           f"edge_endpoints was advertised on RegisterAck and the adapter did "
                           f"not dial within {_EDGE_WAIT}s"))
            self.r.add("C8.2", "Tee a PositionFrame per robot to the edge node",
                       CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": the adapter never established the EdgeStream (C8.1)"))
            self.r.add("C8.3", "Confirmed EstopAck for an edge-issued estop",
                       CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": the adapter never established the EdgeStream (C8.1)"))
            return
        self.r.add("C8.1", "Adapter dials the advertised edge endpoint", CheckStatus.PASS)

        # C8.3 — an edge-issued estop must return a confirmed EstopAck. The adapter
        # also tees PositionFrames (C8.2) up the same stream, fire-and-forget; drain
        # past them to find the ack.
        self.s.edge_out.put(pb.EdgeControlMessage(
            estop=pb.Estop(estop_id="edge-e1", reason="conformance", issued_by="edge-harness")))
        deadline = time.monotonic() + _GET_TIMEOUT
        ack = None
        saw_position = False
        while time.monotonic() < deadline:
            try:
                msg = self.s.edge_in.get(timeout=_GET_TIMEOUT)
            except queue.Empty:
                break
            if msg is _SENTINEL:
                break
            which = msg.WhichOneof("msg")
            if which == "estop_ack":
                ack = msg.estop_ack
                break
            if which == "position":
                saw_position = True  # C8.2 — position tee observed

        # Exactly one C8.2 row per run that reached the EdgeStream: PASS when frames were
        # seen, FAIL when none were. Omitting the row on absence made a missing tee
        # invisible in the report rather than a failure.
        self.r.add("C8.2", "Tee a PositionFrame per robot to the edge node",
                   CheckStatus.PASS if saw_position else CheckStatus.FAIL,
                   detail="" if saw_position else
                          "EdgeStream established but no PositionFrame was teed to it")
        if ack is None:
            self.r.add("C8.3", "Confirmed EstopAck for an edge-issued estop",
                       CheckStatus.FAIL, detail="no confirmed EstopAck on the EdgeStream")
            return
        ok = ack.state == pb.ESTOP_STATE_STOPPED
        self.r.add("C8.3", "Confirmed EstopAck for an edge-issued estop",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail="" if ok else f"state={ack.state} (not STOPPED)")


    def _check_artifact_verification(self) -> None:
        """C7.2 and C7.3 (MUST, when implemented) — verify the artifact and fail closed.

        C7.2 covers model_update: verify the artifact checksum, and the signature when one is
        required. C7.3 covers push_firmware and is a SEPARATE obligation, not a restatement —
        it is the robot-side re-verify of the delivered BYTES. The control plane checked the
        publisher's signature over the checksum, which says nothing about what arrived here.

        The two are independent: an adapter may decline one under C7.1 and implement the other,
        so a decline of model_update must not skip the firmware probe.

        Probed with a DELIBERATELY WRONG checksum. A conformant adapter answers acknowledged=false;
        acknowledging is a failed MUST, because the control plane records acknowledged=true as "this
        artifact was checked" and a robot then runs code nobody verified — a supply-chain compromise
        on the machine.

        An adapter that declines model_update (unsupported=true, C7.1) records a SKIP: declining is
        conformant, and a skip states plainly that verification was not observed rather than
        implying it passed.
        """
        cid = self._send_command(model_update=pb.ModelUpdate(
            model_name="conformance-probe",
            new_version="9.9.9",
            model_uri="oci://example.invalid/conformance-probe:9.9.9",
            # Well-formed but WRONG: it cannot match any artifact the adapter fetches.
            model_checksum="sha256:" + ("0" * 64),
        ))
        res = self._await_result(cid)
        if res.unsupported:
            self.r.add("C7.2", "Verify MODEL artifact checksum/signature, fail closed",
                       CheckStatus.SKIP, detail=(
                           SkipCause.OPTIONAL_DECLINED.value + ": " + "adapter declines model_update (conformant, C7.1), so artifact "
                           "verification was not exercised"))
            # Do NOT return here. The firmware body re-verify below (C7.3) is a separate
            # requirement against a separate command, and an adapter may decline one while
            # implementing the other. An early return dropped C7.3 out of the report
            # entirely — no PASS, no FAIL, no SKIP, simply absent.
        else:
            mu = res.model_update
            ok = not mu.acknowledged
            self.r.add("C7.2", "Verify MODEL artifact checksum/signature, fail closed",
                       CheckStatus.PASS if ok else CheckStatus.FAIL,
                       detail=("refused a bad checksum: " + mu.message if ok else
                               "adapter ACKNOWLEDGED a model_update whose checksum cannot match "
                               "the artifact — an unverified model would be installed"))

        # C7.3 is the ROBOT-SIDE body re-verify, a separate requirement from C7.2: the
        # control plane already checked the publisher's signature over the checksum, but that says
        # nothing about the bytes this robot received. An adapter that flashes without re-hashing
        # trusts every hop of the delivery path.
        cid = self._send_command(push_firmware=pb.PushFirmware(
            target_version="9.9.9-conformance",
            firmware_uri="https://example.invalid/fw-9.9.9.bin",
            firmware_checksum="sha256:" + ("0" * 64),  # well-formed but cannot match any artifact
        ))
        res = self._await_result(cid)
        if res.unsupported:
            self.r.add("C7.3", "Verify FIRMWARE artifact body before flashing, fail closed",
                       CheckStatus.SKIP, detail=(
                           SkipCause.OPTIONAL_DECLINED.value
                           + ": adapter declines push_firmware (conformant, C7.1), so body "
                             "re-verify was not exercised"))
            return
        fw = res.push_firmware
        ok = not fw.accepted
        self.r.add("C7.3", "Verify FIRMWARE artifact body before flashing, fail closed",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail=("refused a bad checksum: " + fw.message if ok else
                           "adapter ACCEPTED a push_firmware whose checksum cannot match the "
                           "delivered bytes — unverified firmware would be flashed"))

    # ── C13.2 / C14 — the version-refusal path, and estop's invariance to it ──────
    #
    # These run LAST because they drop the ControlStream: the harness cannot make a conformant
    # adapter misreport its version, so the only honest way to exercise the refusal is to play a
    # control plane that refuses, and see what the adapter does next.

    def _check_accepting_reconnect(self) -> None:
        """C6.1 + C2.2 — fault the stream, then ACCEPT the re-registration.

        Both checks are about a successful reconnect: the first telemetry afterwards must
        be a full snapshot (an adapter that resumed deltas leaves the control plane with a
        stale hardware picture it cannot detect), and the authoritative action state in the
        RegisterAck must be adopted (the window where a task was cancelled during the
        outage — the hazard the reconnect handshake exists to close).
        """
        print("harness: faulting the ControlStream to exercise the ACCEPTING reconnect "
              "(C6.1/C2.2). Adapter transport errors after this line are EXPECTED.",
              file=sys.stderr, flush=True)
        opens_before = self.s.control_opens
        self.s.drop_control()
        deadline = time.monotonic() + _RECONNECT_WAIT
        while time.monotonic() < deadline and self.s.control_opens <= opens_before:
            time.sleep(0.1)
        if self.s.control_opens <= opens_before:
            for cid, title in (("C6.1", "First TelemetryPayload after reconnect is a full snapshot"),
                               ("C2.2", "Adopt authoritative_action_state from RegisterAck")):
                self.r.add(cid, title, CheckStatus.SKIP,
                           detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                                  + ": adapter did not redial within "
                                  + f"{_RECONNECT_WAIT}s")
            return

        # Re-handshake: accept the hello, then accept the registration and hand back an
        # authoritative state saying the task the robot last held was CANCELLED.
        ghost = "t-status"   # the action the adapter accepted earlier in this run
        accepted_register = False
        deadline = time.monotonic() + _ACCEPTING_HANDSHAKE_WAIT
        while time.monotonic() < deadline:
            try:
                msg = self.s.control_in.get(timeout=0.5)
            except queue.Empty:
                continue
            if msg is _SENTINEL:
                # End of the OLD stream, not the new one. Breaking here aborts the
                # handshake before the redialled adapter has said anything.
                continue
            kind = msg.WhichOneof("payload")
            if kind == "hello":
                self.s.control_out.put(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
                    accepted=True, negotiated_protocol_version="fleet_adapter.v1",
                    negotiated_contract_version=CONTRACT_VERSION)))
            elif kind in ("register", "discover"):
                self.s.control_out.put(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                    accepted=True, telemetry_interval_seconds=1,
                    authoritative_action_state=pb.RobotActionState(
                        phase=pb.ROBOT_ACTION_PHASE_CANCELLED, action_id=ghost,
                        fencing_token=14, lease_generation=0))))
                accepted_register = True
                break
        if not accepted_register:
            for cid, title in (("C6.1", "First TelemetryPayload after reconnect is a full snapshot"),
                               ("C2.2", "Adopt authoritative_action_state from RegisterAck")):
                self.r.add(cid, title, CheckStatus.SKIP,
                           detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                                  + ": adapter re-opened the stream but attempted no registration")
            return

        # C6.1 — the first payload after reconnect must carry the FULL hardware set, not a
        # delta. Compared against the snapshot size seen before the drop.
        # fleet-adapter-protocol.md performance table: first telemetry after reconnect is
        # "< 10 s after RegisterAck". Waiting 8 s failed adapters the spec permits.
        payload = self._await_telemetry(timeout=_POST_RECONNECT_TELEMETRY_WAIT)
        if payload is None:
            self.r.add("C6.1", "First TelemetryPayload after reconnect is a full snapshot",
                       CheckStatus.SKIP, detail=SkipCause.NO_TELEMETRY_IN_RUN.value)
        elif not self._scan_components:
            # No independent inventory to compare against: either scan was never answered,
            # or it answered with an empty hardware list. NON-EMPTINESS alone cannot tell a
            # full snapshot from a PARTIAL one, which is the whole point of this check, so
            # say so rather than pass on the weaker property.
            self.r.add("C6.1", "First TelemetryPayload after reconnect is a full snapshot",
                       CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": scan returned no components, so there is no independent "
                                 "inventory to compare the first post-reconnect payload "
                                 "against"))
        else:
            # Compare by NAME against the inventory `scan` reported (C12.1), not by COUNT and
            # not against the pre-drop payload. A count says nothing about WHICH components
            # arrived, and the pre-drop sample is produced by the same possibly-broken path --
            # a run that re-sent one of two components passed exactly that way.
            got = {u.component_name for u in payload.hardware}
            missing = sorted(self._scan_components - got)
            extra = sorted(got - self._scan_components)
            ok = not missing
            detail = (f"all {len(self._scan_components)} component(s) named by scan were "
                      f"re-sent: {sorted(self._scan_components)}"
                      + (f"; also present but not in scan: {extra}" if extra else "")
                      + ". LIMIT: if scan itself under-reports, both paths agree and this "
                        "check is blind -- an empty snapshot cannot say whether it means "
                        "'no hardware' or 'none observed yet'.")
            self.r.add("C6.1", "First TelemetryPayload after reconnect is a full snapshot",
                       CheckStatus.PASS if ok else CheckStatus.FAIL,
                       detail=detail if ok else (
                           f"first post-reconnect payload OMITTED {missing} of the "
                           f"{len(self._scan_components)} component(s) scan reported "
                           f"({sorted(self._scan_components)}); a PARTIAL snapshot leaves the "
                           f"control plane with a stale picture it cannot detect"))

        # C2.3 — the RegisterAck above advertised telemetry_interval_seconds=1. Honouring
        # per-robot configuration is a WIRE effect: measure the spacing of the payloads
        # that follow. Bounds are wide (0.5-2.5s) because this is a cadence assertion, not
        # a clock test — the point is that the adapter adopted ~1s rather than ignoring the
        # field and keeping its own default.
        stamps = []
        for _ in range(3):
            t0 = time.monotonic()
            if self._await_telemetry(timeout=4.0) is None:
                break
            stamps.append(time.monotonic() - t0)
        if len(stamps) < 3:
            self.r.add("C2.3", "Honour telemetry_interval_seconds from RegisterAck",
                       CheckStatus.SKIP, detail=SkipCause.NO_TELEMETRY_IN_RUN.value)
        else:
            gaps = stamps[1:]  # the first sample includes whatever was already queued
            ok = all(0.5 <= g <= 2.5 for g in gaps)
            self.r.add("C2.3", "Honour telemetry_interval_seconds from RegisterAck",
                       CheckStatus.PASS if ok else CheckStatus.FAIL,
                       detail=(f"inter-payload gaps {[round(g, 2) for g in gaps]}s against an "
                               f"advertised 1s") if ok else
                              (f"gaps {[round(g, 2) for g in gaps]}s do not reflect the "
                               f"advertised 1s interval; the adapter kept its own default"))

        # C2.2 — the adapter was told its last action is CANCELLED. It MUST reconcile:
        # halt and report, rather than resume executing a task the control plane dropped.
        upd = self._await_action_status(
            ghost, timeout=4.0,
            states=(pb.ACTION_STATE_CANCELLED, pb.ACTION_STATE_STOPPED, pb.ACTION_STATE_UNKNOWN))
        self.r.add("C2.2", "Adopt authoritative_action_state from RegisterAck",
                   CheckStatus.PASS if upd is not None else CheckStatus.FAIL,
                   detail=("reported " + pb.ActionState.Name(upd.state)) if upd else
                          ("no reconciliation reported for the action the RegisterAck declared "
                           "CANCELLED; a task cancelled during an outage would keep running"))

    def _check_negotiation_refusal(self) -> None:
        # C14.2 first: a static schema check that cannot flake, and does not depend on the probe
        # below succeeding. This is the strongest available form of "the Estop/EstopAck schema is
        # unchanged across the supported contract versions" — a renumbered or renamed field fails
        # it, which is exactly what would silently break an older adapter's estop path.
        self._check_estop_schema_pinned()

        # The abort below surfaces in the ADAPTER's own stderr, which is inherited by this process.
        # A non-resilient adapter will print a transport error or a traceback there. Say so first,
        # or a reader sees a traceback in a CONFORMANT run and reasonably concludes it failed.
        print("harness: C13.2 — faulting the ControlStream on purpose to exercise the "
              "contract-version refusal path. Adapter output after this line (a transport error, "
              "possibly a traceback) is EXPECTED and is not a conformance failure.",
              file=sys.stderr, flush=True)
        self.s.drop_control()
        if not self.s.control_reconnected.wait(_RECONNECT_WAIT):
            for cid, title, level in (
                ("C13.2", "No work accepted after a VERSION_MISMATCH registration refusal",
                 "SHOULD"),
                ("C14.1", "Estop honored against an adapter that failed the version gate", "MUST"),
            ):
                self.r.add(cid, title, CheckStatus.SKIP, level=level, detail=(
                    SkipCause.ADAPTER_DID_NOT_REACH_STATE.value + ": "
                    + f"adapter did not redial the ControlStream within {_RECONNECT_WAIT}s, so the "
                    "version-refusal path was not exercised (expected for a one-shot or template "
                    "adapter); the control-plane half is asserted in "
                    "internal/controlstream/server_contract_gate_test.go"))
            return

        # Drain the first stream's end-of-iteration sentinel, then read the second hello.
        try:
            while True:
                item = self.s.control_in.get(timeout=_GET_TIMEOUT)
                if item is _SENTINEL:
                    continue
                if item.WhichOneof("payload") == "hello":
                    break
        except queue.Empty:
            self.r.add("C13.2", "No work accepted after a VERSION_MISMATCH registration refusal",
                       CheckStatus.SKIP, level="SHOULD",
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": adapter reconnected but sent no second AdapterHello"))
            return

        # Accept the CONNECTION but negotiate no contract version — the control plane's real
        # behaviour for an incompatible adapter (ADR-0032: refuse work, never refuse observation).
        self.s.control_out.put(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
            accepted=True, negotiated_protocol_version="fleet_adapter.v1",
            message=("connected, but robot registration is refused: "
                     "contract version not supported"))))

        # Refuse whatever registration it attempts, with the enum a real control plane uses.
        refused = self._refuse_registration_version_mismatch()

        # C14.1 — the SafetyStream was deliberately left up. An adapter that lost the version gate
        # must still stop: this is the property that makes refusing work acceptable at all.
        self._check_estop_after_refusal()

        # C3.1 — fencing-token DURABILITY. The stream was faulted and redialled above, so this is
        # a genuine across-reconnect test: a token at or below the pre-drop high-water mark (13,
        # set during C4/C16) must still be refused STALE. An adapter that kept the mark only in
        # the connection's memory accepts it here, and two robots can then execute the same task
        # after a control-plane failover — the hazard the persisted generation store exists for.
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id="t-persist", assignment_id="a-persist", fencing_token=6))
        try:
            res = self._await_result(cid).assign_action
            stale = (not res.accepted
                     and res.rejection == pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN)
            self.r.add("C3.1", "Highest accepted fencing token survives a reconnect",
                       CheckStatus.PASS if stale else CheckStatus.FAIL,
                       detail="" if stale else (
                           f"a token below the pre-reconnect high-water mark was not refused "
                           f"STALE (accepted={res.accepted} rejection={res.rejection}); the mark "
                           f"did not survive the reconnect"))
        except (EOFError, queue.Empty):
            self.r.add("C3.1", "Highest accepted fencing token survives a reconnect",
                       CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": adapter sent no result on the refused session"))

        # C3.7 — the OTHER half of the catalog's C3.1: durability across a process RESTART.
        # C3.1 above faults and redials the STREAM, which does not touch process memory, so
        # an adapter holding the mark in a plain dict passes it. This suite spawns the
        # adapter once and never restarts it, so the restart half is not verified -- recorded
        # rather than left to a reader comparing the catalog against the harness.
        #
        # Deliberately NOT implemented here: a real restart turns this red for all three
        # reference adapters at once (none persists the mark), and that is an SDK decision,
        # not a harness change.
        self.r.add("C3.7", "Highest accepted fencing token survives an adapter RESTART",
                   CheckStatus.SKIP,
                   detail=(SkipCause.NEEDS_ADAPTER_RESTART.value
                           + ": the suite spawns the adapter once and never restarts it, so a "
                             "passing report attests NOTHING about durability across a restart"))

        if not refused:
            self.r.add("C13.2", "No work accepted after a VERSION_MISMATCH registration refusal",
                       CheckStatus.SKIP, level="SHOULD", detail=(
                           SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                           + ": adapter attempted no registration on the refused session, so "
                             "there was nothing to refuse"))
            return

        # The refusal must not be read as success: the adapter must not go on to accept work. A
        # command sent now is answered either with a rejection or not at all; accepting it would
        # mean the adapter believes it manages a robot the control plane refused it.
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id="t-refused", assignment_id="a-refused", fencing_token=99))
        accepted = None
        try:
            res = self._await_result(cid).assign_action
            accepted = res.accepted
        except (EOFError, queue.Empty):
            accepted = None  # silence is a correct answer here
        ok = accepted is not True
        # SHOULD, not MUST, and deliberately so: no existing check requires this. CONFORMANCE.md
        # C1.2 only requires an adapter not to proceed until HelloAck.accepted is true — and the
        # accept is what a version-refused connection gets (the refusal lands on the register,
        # not
        # the hello). ADR-0032 goes further and says an incompatible adapter "remains able to
        # register … the gate is on work dispatch only", so refusing work is defence in depth rather
        # than a stated obligation. Making it blocking would retroactively fail adapters that were
        # qualified against a contract that never asked for it — a new MUST needs to be written into
        # CONFORMANCE.md, and per ADR-0032 that is a contract bump, not a harness change.
        self.r.add("C13.2", "No work accepted after a VERSION_MISMATCH registration refusal",
                   CheckStatus.PASS if ok else CheckStatus.FAIL, level="SHOULD",
                   detail=("accepted no work after a VERSION_MISMATCH refusal"
                           if ok else
                           "adapter ACCEPTED an AssignAction after its registration was refused "
                           "VERSION_MISMATCH. Not blocking: the contract does not yet require an "
                           "adapter to withhold work on a refused registration, and a real control "
                           "plane withholds the dispatch itself (the assignment gate). Recorded as "
                           "defence-in-depth that is absent"))

    def _refuse_registration_version_mismatch(self) -> bool:
        """Refuse a Register/Discover on the reconnected session. Returns whether one arrived."""
        deadline = time.monotonic() + _REGISTER_WAIT
        while time.monotonic() < deadline:
            try:
                msg = self.s.control_in.get(timeout=0.2)
            except queue.Empty:
                continue
            if msg is _SENTINEL:
                return False
            kind = msg.WhichOneof("payload")
            if kind == "register":
                self.s.control_out.put(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                    accepted=False,
                    rejection=pb.REGISTRATION_REJECTION_VERSION_MISMATCH,
                    message="VERSION_MISMATCH: contract version not supported")))
                return True
            if kind == "discover":
                self.s.control_out.put(pb.ControlPlaneMessage(discover_ack=pb.DiscoverAck(
                    accepted=False,
                    rejection=pb.REGISTRATION_REJECTION_VERSION_MISMATCH,
                    message="VERSION_MISMATCH: contract version not supported")))
                return True
            # Telemetry/heartbeat on a refused session is CORRECT (observation is never gated) —
            # keep reading past it rather than treating it as the registration.
        return False

    def _check_estop_after_refusal(self) -> None:
        self.s.safety_out.put(pb.ControlPlaneSafetyMessage(
            robot_id=ROBOT,
            estop=pb.Estop(estop_id="e-refused", reason="version-gate invariance",
                           issued_by="harness")))
        try:
            msg = self._recv_safety()
        except (EOFError, queue.Empty):
            self.r.add("C14.1", "Estop honored against an adapter that failed the version gate",
                       CheckStatus.FAIL, detail=(
                           "no EstopAck after a VERSION_MISMATCH refusal — estop MUST be "
                           "version-invariant (ADR-0032)"))
            return
        if msg.WhichOneof("payload") != "estop_ack":
            self.r.add("C14.1", "Estop honored against an adapter that failed the version gate",
                       CheckStatus.FAIL,
                       detail=f"safety payload was {msg.WhichOneof('payload')!r}")
            return
        ack = msg.estop_ack
        # The first ack may be STOPPING (see C5.2); estop version-invariance is about the
        # CONFIRMED stop, so wait for the terminal one rather than failing on the first.
        if ack.state == pb.ESTOP_STATE_STOPPING:
            term, _saw, _w = self._recv_terminal_estop_ack("e-refused")
            if term is not None:
                ack = term
        ok = ack.estop_id == "e-refused" and ack.state == pb.ESTOP_STATE_STOPPED
        self.r.add("C14.1", "Estop honored against an adapter that failed the version gate",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail="" if ok else f"estop_id={ack.estop_id!r} state={ack.state}")

    def _check_estop_schema_pinned(self) -> None:
        """C14.2 — Estop/EstopAck field numbers and names are pinned.

        ADR-0032 states estop's version-invariance as a CONTRACT guarantee, which only holds if the
        two messages never change shape: an adapter built against an older contract minor must be
        able to parse an Estop from a newer control plane byte-for-byte. Field NUMBERS are what the
        wire actually carries, so they are what is pinned; names are pinned too because a rename
        is a source-breaking change for every adapter author.
        """
        expected = {
            "Estop": {"estop_id": 1, "reason": 2, "issued_by": 3, "issued_at_ms": 4},
            "EstopAck": {"estop_id": 1, "state": 2, "message": 3, "confirmed_at_ms": 4,
                         "stop_initiated_at": 5},
        }
        drift = []
        for msg_name, fields in expected.items():
            desc = getattr(pb, msg_name).DESCRIPTOR
            actual = {f.name: f.number for f in desc.fields}
            if actual != fields:
                drift.append(f"{msg_name}: expected {fields}, got {actual}")
        self.r.add("C14.2", "Estop/EstopAck schema is unchanged across supported contract versions",
                   CheckStatus.PASS if not drift else CheckStatus.FAIL,
                   detail=("; ".join(drift) if drift else
                           "field numbers and names match the pinned estop schema"))

    # ── C15 — version-bound conformance ──────────────────────────────────────────

    def _check_version_bound(self) -> None:
        """The report must carry the contract version its result was earned against, and must keep
        carrying it when the result is a FAILURE.

        This is the seam ADR-0032 depends on: the control plane copies this value to
        FleetAdapter.status.conformanceContractVersion and refuses assignment when it is absent
        or out of range. A report that omits it is not cosmetic — it makes the adapter unassignable.
        """
        stamped = self.r.contract_version
        self.r.add("C15.1", "Report carries the contract version the result was earned against",
                   CheckStatus.PASS if stamped == CONTRACT_VERSION else CheckStatus.FAIL,
                   detail=(f"contract_version={stamped}" if stamped == CONTRACT_VERSION else
                           f"report stamped {stamped!r}, suite attests {CONTRACT_VERSION!r}"))

        # C15.2 — "not assignable, but still observable", in the only terms the harness owns: a
        # NON-conformant result must still be version-stamped and still enumerate every check, so a
        # control plane can withhold work while an operator can still see why. The harness is not a
        # scheduler, so assignability itself is asserted control-plane-side; this checks the report
        # contract that decision reads.
        probe = Report(adapter="probe")
        probe.add("probe", "a failed MUST", CheckStatus.FAIL, level="MUST")
        payload = json.loads(probe.to_json())
        ok = (payload["conformant"] is False
              and payload["contract_version"] == CONTRACT_VERSION
              and len(payload["results"]) == 1
              and payload["counts"]["fail"] == 1)
        self.r.add("C15.2", "A non-conformant report stays version-bound and fully observable",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail=("a failed MUST yields conformant=false while keeping contract_version "
                           "and the per-check detail; assignability itself is asserted in "
                           "internal/controller/assignment_gate_envtest_test.go"
                           if ok else f"report contract broken: {payload}"))

def run_conformance(adapter_name: str, port: int, ready: threading.Event) -> Report:
    """Start the server, signal readiness, run the driver, return the report.

    The caller starts the adapter-under-test (pointing it at ``port``) after
    ``ready`` is set.
    """
    report = Report(adapter=adapter_name)
    session = _Session()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    pb_grpc.add_FleetAdapterServiceServicer_to_server(_make_servicer(session), server)
    # The EdgeService (C8) is served on the same port; advertised via RegisterAck.
    pb_grpc.add_EdgeServiceServicer_to_server(_make_edge_servicer(session), server)
    server.add_insecure_port(f"[::]:{port}")  # mTLS is C1.3; see README note.
    server.start()
    ready.set()
    try:
        _Driver(session, report, port).run()
    except (EOFError, queue.Empty) as exc:
        report.add("run", "Harness completed the scenario", CheckStatus.FAIL,
                   detail=f"aborted: {exc}")
    finally:
        session.close_streams()
        server.stop(grace=1.0)
    return report
