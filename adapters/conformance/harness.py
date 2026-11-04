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

"""The control-plane-side test server and the C0–C19 scenario driver.

Topology mirrors production (RFC-0001 #fleet-adapter-protocol): the adapter-under-test is the gRPC
*client* and dials in; this harness is the *server*. The harness accepts the
adapter's ``ControlStream`` and ``SafetyStream``, then a driver thread sends
crafted control-plane messages and asserts the adapter's replies against
``adapters/CONFORMANCE.md``.

Scope: every id the file records, C0.1 through C19.4 — C0 (report integrity and
harness context), C1 (handshake), C2 (registration and the redial, when the
adapter registers), C3 (fencing-token ordering), C4 (assignment leases), C5
(confirmed estop), C6 (telemetry discipline), C7 (optional-command decline via
``unsupported=true``, and artefact verification), C8 (edge stream: the
harness also serves ``EdgeService`` and advertises an edge endpoint on the
``RegisterAck``, then checks the adapter dials it and honours an edge-issued
estop with a confirmed ``EstopAck``), C9 (an adapter that answers ``verify_*``
returns ``actualMetrics``, RFC-0001 #robotprobe-the-verify-probe-commands) and C10
(an adapter that accepts ``push_firmware`` or ``model_update`` MAY emit advisory
``UpdateProgress`` and MUST report a terminal outcome for each it answers,
RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist), C11 (an adapter MAY
accept ``pause``/``resume`` and answer with a ``PauseResult``/``ResumeResult``),
C12 (an adapter MAY answer ``scan`` with a ``CapabilitiesSnapshot``), C13
(contract-version negotiation), C14 (estop's invariance to it), C15 (the report
is version-bound), C16 (task status, terminal states included), C17 (zone
operating constraints: the FA-R1 declaration is well formed, and an assignment
carrying a rule the adapter cannot honour is DECLINED naming the subject rather
than accepted and ignored — FA-R5 and FA-R6) and C18 (sensor streams: the
declaration is well formed, no stream bytes on the control plane, a stream stops
on lease expiry, clip windows are bounded, and ``DeactivateStream`` stops a live
stream) and C19 (an assignment outside the adapter's declared action catalog is
declined INVALID_ACTION). C9–C12 are
MAY-level — a decline is conformant. C4 (lease self-stop, a timer-driven
on-robot behaviour) and C6 (telemetry) are recorded ``skip`` when the adapter
does not exercise them — as are C2/C8 for a minimal adapter that registers no
robot. A ``skip`` is not a failure: it records exactly what was and was not
verified for the adapter under test.

Keep this range in step with the ``r.add`` call sites: a docstring naming fewer
families than the file drives tells a reader auditing coverage that whole
families are out of scope when they are not.
"""

from __future__ import annotations

import json
import os
import queue
import re
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
# How often an open stream handler looks up from its queue to see whether the run has
# closed the streams. It costs nothing in latency -- a queued message still wakes the
# handler immediately -- and it is what lets EVERY open handler return, not just the one
# that happens to take the sentinel.
_STREAM_POLL = 0.1
# Seconds the server is given to finish in-flight RPCs when a run ends.
_SERVER_STOP_GRACE = 1.0
_STREAM_TIMEOUT = 10.0  # seconds to wait for the adapter to open both streams
_REGISTER_WAIT = 1.5  # seconds to wait for an adapter-initiated registration
# Seconds to wait, after the scan reply, for the CapabilitiesSnapshot an adapter pushes at
# registration (C12.2) when none has arrived yet.
_PUSH_WAIT = 3.0
# Action types offered as OUT-OF-CATALOG probes (C19.1): the first one the adapter does not
# declare is used. Two are real FleetAction types, so a catalog that happens to list one of
# them still leaves a real type undeclared; the last is not a type at all.
_UNDECLARED_TYPE_CANDIDATES = ("Clean", "External", "conformance-undeclared")
# Fencing tokens for C19. It runs BEFORE the fencing checks, and a refused assignment may still
# raise an adapter's high-water mark (a fence is checked before the type is), so each refusal
# and its control take their own token, rising in the order they are sent, and every one is
# below the fencing checks' first fresh token. The ninth is C19.5's.
_C19_TOKENS = (1, 2, 3, 4, 5, 6, 7, 8, 9)
# The fencing checks' first fresh token (C3.2's accept, C3.5, C3.4, and C3.6's cancel). It was
# 9; C19.5 took 9, and 10 was free below C4's 11.
_FENCING_FRESH_TOKEN = 10
# C3.9's in-place preemption: the action that is preempted, then the one that preempts it at a
# strictly higher token, as RFC-0001 #fleet-adapter-protocol-directive-supersession-on-the-wire
# has the control plane send it. Above C16's 14 and below C17's first probe; the C3.8 fixture
# was raised to make room (see there).
_PREEMPTED_TOKEN = 15
_PREEMPTOR_TOKEN = 16
# The lease both carry, long enough that neither lapses inside the check: C4 owns expiry, and
# a self-stop here would read as the cancel stopping the wrong action.
_PREEMPT_LEASE_MS = 60000
# Seconds to wait for the preemptor to reach a terminal state after the cancel. It drives to
# the fixture pole farthest from the preempted action's goal, about 12 m on the default poses:
# a liveness bound, not a performance target.
_PREEMPTOR_TERMINAL_WAIT = 30.0
# The value C19.4 sends for a declared enum parameter; lengthened if the catalog allows it.
_UNALLOWED_VALUE = "conformance-not-allowed"
# Seconds to wait, with a motion witness, for the robot to be seen moving before the C5
# estop is sent (C5.7). A plant that ticks on a telemetry interval moves on its next tick.
_WITNESS_MOTION_WAIT = 4.0
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
# The leader term on every Command and every EstopClear the harness sends. RFC-0001
# #fleet-adapter-protocol-the-stop-latch, *The leader term*: "The control plane MUST set the
# sending leader's term on every Command and every EstopClear." A run is one leader, so the
# term never changes. 0 would mean "no term".
_LEADER_TERM = 1
# Seconds to wait for the EstopClearAck that says a stop the harness cleared is lifted. The
# clear takes effect once the stop's fence has arrived and every Command ahead of it is
# answered, so this is a liveness bound, not a performance target.
_CLEAR_WAIT = 3.0
# The stop ID of C5.4's probe: a stop sent as its fence alone, on ControlStream.
_FENCE_PROBE_ID = "e-control-probe"
# C5.8's stop, and the two assignments it brackets: one while the stop is latched, one after
# its clear. Both tokens are above _CADENCE_TOKEN, the run's high-water mark when C5.8 runs,
# and below the 99 the refused session's C13.2 probe sends.
_LATCH_PROBE_ID = "e-latch"
_LATCH_HELD_TOKEN = 24
_LATCH_LIFTED_TOKEN = 25
# Spec budget for the first TelemetryPayload after reconnect; must not be tighter.
_POST_RECONNECT_TELEMETRY_WAIT = 11.0
_EDGE_WAIT = 3.0  # seconds to wait for the adapter to dial the advertised edge node
_RECONNECT_WAIT = 5.0  # seconds to wait for the adapter to redial after a dropped ControlStream
# Redial alone can be quick while the hello/register handshake that follows is not.
# C6.1/C2.2 skipped on three runs in four at 5s; this covers the adapter's backoff
# plus a full re-handshake without making a genuine failure wait long.
_ACCEPTING_HANDSHAKE_WAIT = 15.0

# ── C3.8 fencing-token ADOPTION fixture ───────────────────────────────────────
# Three values that must keep their order, or the check stops discriminating:
#
#   19  the high-water mark the run reaches before the reconnect (C17.4's probe, or 16
#       from C3.9's preemptor against an adapter that refuses every C17 probe before its
#       fence guard records them)
#   20  _ADOPTION_PROBE_TOKEN — issued AFTER the reconnect, strictly BETWEEN the two
#   22  _AUTHORITATIVE_TOKEN  — RobotActionState.fencing_token on the accepting RegisterAck
#
# An adapter that adopts 22 refuses 20 as STALE. One that ignores the field still holds 19
# and accepts it — the extra executor
# RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee
# exists to prevent, arriving through the one door C3.2 does not watch. The probe must be
# STRICTLY between: at 19 it is refused by the surviving mark alone (that is C3.1, not this),
# and at 22 an adapter that adopted would re-ack rather than refuse (C3.4).
#
# RAISED TWICE: once for C17.4, and again for C3.9's two assignments (15 and 16), which moved
# C17's probes to 17, 18 and 19. The gap below _ADOPTION_PROBE_TOKEN is fully consumed again:
# a new pre-reconnect assignment must raise this fixture rather than take the next number,
# or it silently converts C3.8 into a restatement of C3.1.
_AUTHORITATIVE_TOKEN = 22
_ADOPTION_PROBE_TOKEN = 20
# Above _AUTHORITATIVE_TOKEN so the C2.3 cadence drive is accepted whether or not the
# adapter adopted. It was 15, which a conformant adapter now correctly refuses.
_CADENCE_TOKEN = 23

# ── C17 zone-restriction fixture (ADR-0058 Part D, FA-R5 / FA-R6) ─────────────
# The normative core seven operating subjects. CLOSED at the core: a subject with no "/"
# MUST be one of these, and conformance covers these only. Ordered as the specification
# lists them so a reader can diff the two by eye.
_CORE_OPERATING_SUBJECTS = ("camera", "microphone", "water", "chemical", "uv", "speed", "noise")
# The FA-R6 probe subject: a namespaced extension in the RFC 2606 example domain, which is
# the amendment's own example. Exercised EXACTLY ONCE, which is all Part E asks for -- an
# adapter that treats an unrecognised subject as an absent one has made the extension form
# a universal bypass, and one probe establishes that as well as a hundred would.
_EXTENSION_OPERATING_SUBJECT = "example.com/leaf-blower"
# The zone the fixture rules are declared BY. Deliberately not the target: an effective
# rule is the most restrictive anywhere in the ancestor chain (FZ-R1), so declaring_zone is
# very often an ancestor, and a refusal has to name the object an operator must actually
# edit.
_DECLARING_ZONE = "ward-3"
# Three AssignActions, at 17, 18 and 19 -- the last free tokens below
# _ADOPTION_PROBE_TOKEN (see the C3.8 fixture above), and above C3.9's 15 and 16. Three,
# not nine, because ONE AssignAction carries the whole resolved set:
# `operating_constraints` is repeated, `unhonourable_subjects` is repeated, and requiring
# the decline to name EVERY subject it cannot honour is a stronger assertion than seven
# separate probes, each of which an adapter could satisfy by naming the first subject it
# saw and stopping.
_ZONE_CORE_TOKEN = 17
_ZONE_EXTENSION_TOKEN = 18
# C17.4's probe, which expects ACCEPTANCE rather than a decline. It is issued last because
# it is the only one that leaves a task running on a CONFORMANT adapter, and is cancelled
# immediately afterwards.
_ZONE_HONOURED_TOKEN = 19
# A namespaced extension subject, for C17.3. The prefix is a DNS domain the extender
# controls, which is the same split this specification already applies to capability
# identifiers -- so a bare label with no dot is NOT a domain, and neither is an uppercase
# one. Deliberately stricter than "contains a slash": the whole point of the namespace is
# that it is attributable to somebody.
_EXTENSION_SUBJECT_RE = re.compile(
    r"^(?=.{1,253}/)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}/[a-z][a-z0-9-]*$")
# How long to wait for the terminal ActionStatusUpdate after the harness cancels a task
# (C16.1). A cancel routed through a confirmed stop takes as long as the base takes to
# reach rest, which the adapter reports only afterwards.
_TERMINAL_STATUS_WAIT = 4.0
# How long C4.2 waits, from the accepted assignment, for the lapsed lease's terminal report.
# The adapter sends it only once the robot is observed at rest or its stopping budget runs
# out, so the window holds the 500 ms lease, the adapter's look at its timer, and that
# budget. Sized from the adapters it grades: the camera adapter confirms rest from fresh
# frames within 2.6 s and looks at its lease every 0.2 s, and MP1 confirms rest in at most 40
# looks 0.05 s apart (about 2 s) and looks every 0.05 s, so both report within about 3.3 s.
# Like _ESTOP_TERMINAL_BUDGET_S, a liveness bound and not a spec value; the
# assignment's travel must outlast it, or the task could end on its own inside the window.
_LEASE_END_WAIT = 4.0
# C18.3's lease horizon. Short enough that the suite does not spend a minute waiting for
# a camera to switch itself off, long enough that two renewals fit comfortably inside it
# and the measured interval is not dominated by one round trip.
_STREAM_LEASE_S = 2.0
# How far past the horizon a conformant stop may land. It is NOT slack for a slow
# adapter: it covers this harness's own poll round trip plus the adapter's watchdog
# granularity, both of which make the READING later than the truth.
_STREAM_LEASE_GRACE_S = 1.0
# The C18 rows, in report order. Listed once so the not-applicable paths above can skip
# the whole family without three copies of the titles drifting apart.
_C18_TITLES = [
    ("C18.1", "The sensor-stream declaration is well formed"),
    ("C18.2", "No sensor-stream bytes on the control plane while a stream runs"),
    ("C18.3", "A stream stops on lease expiry when the control plane goes silent"),
    ("C18.4", "Clip windows are refused or bounded, per the declaration"),
    ("C18.5", "DeactivateStream stops a live stream"),
]
# Levels that differ from MUST, for the loops that skip the family by table.
_C18_LEVELS = {"C18.2": "MUST NOT"}

# Every row whose check can only run on an ACCEPTED assignment, keyed by the check method
# that records it. An adapter that declares no SupportedAction offers no type an assignment
# could carry that it claims to serve, so these rows are recorded skipped with
# `adapter-declares-no-action` and the method is not run. Sending a type the adapter never
# declared would test something else: C19.1 is the row that does that on purpose.
_NEEDS_SERVED_ACTION = {
    "_check_catalog_enforced": [
        ("C19.1", "An action type the adapter did not declare is declined INVALID_ACTION", "MUST"),
        ("C19.2", "An assignment missing a declared required parameter is declined "
                  "INVALID_ACTION", "MUST"),
        ("C19.3", "An assignment that states no action type is declined INVALID_ACTION",
         "MUST"),
        ("C19.4", "A parameter value outside its declared allowed values is declined "
                  "INVALID_ACTION", "MUST"),
        ("C19.5", "A Custom action that names its verb only in the payload is accepted",
         "MUST"),
    ],
    "_check_fencing": [
        ("C3.3", "Reject AssignAction with absent fencing token (MISSING)", "MUST"),
        ("C0.4", "Harness context: a fresh (higher) fencing token is accepted", "MAY"),
        ("C3.5", "Echo the fenced token in AssignActionResult", "MUST"),
        ("C3.2", "Reject a stale (<= highest) fencing token (STALE)", "MUST"),
        ("C3.4", "Idempotently re-ack identical re-delivery", "MUST"),
    ],
    "_check_cancel_disposition": [
        ("C3.6", "cancel_action reports a valid CancelDisposition", "MUST"),
    ],
    "_check_cancel_names_its_action": [
        ("C3.9", "A cancel naming a preempted action leaves the preemptor running to completion",
         "MUST"),
    ],
    "_check_leases": [
        ("C4.2", "Self-stop when the assignment lease is not renewed", "MUST"),
        ("C4.1", "Reports the task still running across lease renewals", "MUST"),
        ("C0.3", "Harness context: a matching lease generation renews", "MAY"),
        ("C4.3", "Ignore a RenewActionLease with a stale lease_generation", "MUST"),
        ("C4.4", "Report running=false once the task is no longer executing", "MUST"),
    ],
    "_check_action_status": [
        ("C16.1", "ActionStatusUpdate at a task phase transition, terminal states included",
         "MUST"),
        ("C6.4", "Echo the assigned fencing_token in ActionStatusUpdate", "MUST"),
    ],
}


def _param_value(param):
    """A value inside what an ActionParam declares, for a parameter the probe must carry."""
    if param.kind == "number":
        return (param.min + param.max) / 2.0 if param.max > param.min else param.min
    if param.kind == "bool":
        return False
    if param.allowed:
        return param.allowed[0]
    return "conformance"


def _custom_type_of(action) -> str:
    """The ``custom_type`` a control plane sets on an AssignAction for ``action``.

    A ``Custom`` catalog entry keyed by ``SupportedAction.custom_type`` names its verb there,
    and the control plane copies ``FleetAction.spec.customType`` to ``AssignAction.custom_type``
    for it (contract 1.4.0). Any other entry, an unkeyed ``Custom`` one included, has none.
    """
    if action is None or action.action_type != "Custom":
        return ""
    return action.custom_type


def _custom_note(action) -> str:
    """The ``custom_type`` a C19 detail names beside an entry's type, when it carries one."""
    custom_type = _custom_type_of(action)
    return f" (custom_type={custom_type!r})" if custom_type else ""


def _catalog_key(actions) -> list:
    """What two CapabilitiesSnapshots must agree on for their catalogs to be the same."""
    return sorted((a.action_type, tuple(sorted(a.required_capabilities)),
                   tuple(sorted((p.name, p.required, p.kind) for p in a.params)))
                  for a in actions)


def _witnessed_rest(samples, since: float, noise: float, hold: float):
    """The start of the first interval after ``since``, at least ``hold`` long, in which every
    sample is within ``noise`` -- confirmed by a sample later than the hold. None otherwise."""
    after = [s for s in samples if s["t"] >= since]
    for i, s in enumerate(after):
        if s["speed"] > noise:
            continue
        window = [w for w in after[i:] if w["t"] <= s["t"] + hold]
        later = [w for w in after[i:] if w["t"] > s["t"] + hold]
        if all(w["speed"] <= noise for w in window) and (hold == 0.0 or later):
            return s["t"]
    return None


def _read_witness(path: str):
    """(metadata, samples) from a motion-witness file, or (None, []) when unreadable.

    The file is JSON lines. The first line describes the witness -- what it is, its rest
    noise, its hold time and its timing resolution -- and every following line is one
    sample: ``{"t": epoch seconds, "robot": id, "speed": m/s}``.
    """
    try:
        lines = [ln for ln in open(path, encoding="utf-8").read().splitlines() if ln.strip()]
    except OSError:
        return None, []
    if not lines:
        return None, []
    try:
        meta = json.loads(lines[0])
        samples = [json.loads(ln) for ln in lines[1:]]
    except ValueError:
        return None, []
    return meta, samples
_SENTINEL = object()

# Which optional command arm each check's `optional-declined` skip depends on. C0.1
# contradicts a skip whose arm the adapter never declined: `optional-declined` is the one
# cause whose truth the run can corroborate directly, and it had drifted to mean both "the
# implementation does not offer this" and "this was never exercised" -- opposite facts
# carrying opposite profile verdicts (ADR-0048). A check absent from this table is one
# whose decline this suite cannot corroborate, and its cause is left uncontradicted rather
# than assumed false.
_DECLINE_ARMS: dict[str, tuple[str, ...]] = {
    "C7.1":  ("verify_hardware",),
    "C7.2":  ("model_update",),
    "C7.3":  ("push_firmware",),
    "C9.1":  ("verify_hardware",),
    "C10.1": ("push_firmware",),
    "C10.2": ("push_firmware", "model_update"),
    "C12.1": ("scan",),
    "C18.2": ("activate_stream",),
    "C18.3": ("activate_stream",),
    "C18.4": ("activate_stream",),
    "C18.5": ("activate_stream",),
        }


def _TERMINAL_ACTION_STATES():  # noqa: N802 -- named as the constant it stands in for
    """The ActionStates that END a task the control plane drove.

    A function rather than a constant because ``pb`` is injected after import. PAUSED,
    RUNNING and STOPPED are not terminal (RFC-0001 safety chapter, *terminal*): STOPPED is
    the commanded stop, and the proto says the terminal report follows it. UNKNOWN is a
    reconnect reconciliation state and not the end of a task the control plane drove.
    """
    return (pb.ACTION_STATE_SUCCEEDED, pb.ACTION_STATE_FAILED, pb.ACTION_STATE_CANCELLED)


def _RFC_TERMINAL_ACTION_STATES():  # noqa: N802 -- named as the constant it stands in for
    """Every ActionState RFC-0001 calls terminal (safety chapter, *terminal*).

    C4.2 grades the lapsed lease's terminal report and RFC-0001 names no terminal state for
    a lease self-stop, so that row accepts any of these and requires none in particular.
    """
    return _TERMINAL_ACTION_STATES() + (pb.ACTION_STATE_UNKNOWN,)
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
                       f"(this suite attests against {CONTRACT_VERSION}); declare a contract "
                       "version in that range that the adapter implements, and run the suite again")
    return True, ""


def _negotiate(reported: str) -> str:
    """The contract version agreed for a connection whose adapter reported ``reported``.

    RFC-0001 #fleet-adapter-protocol-package-and-versioning: "A compatible connection negotiates
    1.4.0 when the adapter reported a 1.4 version, and the version the adapter reported otherwise,
    so a 1.3.x adapter's connection negotiates 1.3.x and is held to it: it is sent every stop, and
    no AssignAction." The control plane's internal/contract.Negotiate makes the same decision. An
    unsupported report agrees none (""). The agreed version is plain major.minor.patch.
    """
    if not _supports_contract(reported)[0]:
        return ""
    major, minor, patch = (int(p) for p in reported.split("."))
    ours = CONTRACT_VERSION.split(".")
    if (major, minor) == (int(ours[0]), int(ours[1])):
        return CONTRACT_VERSION
    return f"{major}.{minor}.{patch}"


class _Session:
    """Shared state between the two stream RPCs and the driver thread."""

    def __init__(self) -> None:
        # Set when the run is over. Every stream handler watches it, so a handler that is
        # parked on its outbound queue returns instead of waiting for a message that will
        # never come. See close_streams().
        self.closing = threading.Event()
        self.control_established = threading.Event()
        self.safety_established = threading.Event()
        self.control_in: queue.Queue = queue.Queue()  # adapter -> harness
        # Every unsolicited `capabilities` message the adapter sent, with its arrival time,
        # captured where it enters the harness. The driver's readers drop messages they are
        # not waiting for, and an adapter that pushes at registration pushes before any check
        # is listening (C12.2).
        self.capabilities_pushed: list = []
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
        # The ONE live stream of each kind, as its gRPC context. The harness plays the control
        # plane for a single adapter, and every handler of a kind drains the same outbound
        # queue. A second live ControlStream therefore competes for the run's commands
        # message by message, and whichever adapter it belongs to answers whatever it
        # receives. That happened, and it is why C3.8 recorded "accepted" for a probe the
        # adapter under test never saw: a second harness had bound the same port (the
        # default SO_REUSEPORT allows that), its adapter reached this server, and the probe
        # went to that adapter's stream. See _claim_stream.
        self.live_control = None
        self.live_safety = None
        # Kinds of streams refused because one was already open. Printed as they happen.
        self.refused_streams: list[str] = []

    def drop_control(self) -> None:
        """Fault the ControlStream only, leaving SafetyStream up (C13.2/C14.1: a version refusal
        must not cost the operator the ability to stop the robot)."""
        self.control_out.put(_ABORT)

    def close_streams(self) -> None:
        """End every open stream handler, however many are open.

        A sentinel per queue ends ONE handler per queue, and a run can leave more than one
        parked on the same queue: an adapter that redials leaves the previous stream's
        handler blocked on `get()`, and gRPC cannot interrupt a handler blocked in harness
        code. The ones left behind are not harmless. They occupy server worker threads,
        which are not daemon threads, so they hold the interpreter open after the report is
        printed -- a red run that stalls instead of failing. The flag ends all of them; the
        sentinels stay so a handler waiting on a message returns at once rather than at the
        next poll.
        """
        self.closing.set()
        self.control_out.put(_SENTINEL)
        self.safety_out.put(_SENTINEL)
        self.edge_out.put(_SENTINEL)


# How long a new stream waits for the one it would replace to finish closing. An adapter
# redials after it sees its old stream fail, so by then the harness side is normally
# closed. The wait covers the moment when it is not closed yet, so a legitimate redial is
# not refused as a second adapter.
_SUPERSEDE_WAIT = 1.0


def _claim_stream(session: _Session, kind: str, context) -> bool:
    """Make ``context`` the one live stream of ``kind``, or refuse it.

    A new stream replaces a stream that has ended: that is a redial, and the reconnect
    checks depend on it. A new stream that arrives while another is still ACTIVE is refused.
    The harness serves one adapter, and a second live stream would take the run's commands
    at random. Refusing is the only safe answer, because the harness cannot tell which of
    two adapters is the one under test.
    """
    attr = f"live_{kind}"
    deadline = time.monotonic() + _SUPERSEDE_WAIT
    while True:
        with session._opens_lock:
            live = getattr(session, attr)
            if live is None or not live.is_active():
                setattr(session, attr, context)
                return True
        if time.monotonic() >= deadline:
            session.refused_streams.append(kind)
            return False
        time.sleep(_STREAM_POLL)


def _may_send(session: _Session, kind: str, context) -> bool:
    """Whether this handler's stream is still the adapter's live stream of ``kind``.

    A handler whose stream ended without the harness ending it (the adapter dropped it) is
    left parked on the queue, because gRPC cannot interrupt harness code. It used to take
    the next message anyway and send it into a dead stream, so the message was lost. It
    now gives the message back, and the next live stream gets it first.
    """
    return getattr(session, f"live_{kind}") is context and context.is_active()


def _requeue_front(q: queue.Queue, item) -> None:
    """Put ``item`` back at the HEAD of ``q``, so the next reader gets it first.

    A superseded handler can already be blocked in ``get()`` when the adapter's new stream
    arrives, and can then receive a message meant for the new stream. Putting it back at the
    tail would reorder the run's commands, and ordering is what C3 grades. The
    ``mutex``/``not_empty`` attributes are the ones ``queue.Queue`` itself uses for this.
    """
    with q.mutex:
        q.queue.appendleft(item)
        q.unfinished_tasks += 1
        q.not_empty.notify()


def _make_servicer(session: _Session):
    class Servicer(pb_grpc.FleetAdapterServiceServicer):
        def ControlStream(self, request_iterator, context):
            if not _claim_stream(session, "control", context):
                print("harness: REFUSED a second ControlStream while one is open. This harness "
                      "serves one adapter; another adapter is dialling this port (often a "
                      "second conformance run on the same port).", file=sys.stderr, flush=True)
                context.abort(grpc.StatusCode.FAILED_PRECONDITION,
                              "harness: a ControlStream is already open; this harness serves "
                              "exactly one adapter")
                return
            with session._opens_lock:
                session.control_opens += 1
                if session.control_opens > 1:
                    session.control_reconnected.set()
            session.control_established.set()

            def reader() -> None:
                try:
                    for msg in request_iterator:
                        if msg.WhichOneof("payload") == "capabilities":
                            session.capabilities_pushed.append((time.time(), msg.capabilities))
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
            while not session.closing.is_set():
                try:
                    out = session.control_out.get(timeout=_STREAM_POLL)
                except queue.Empty:
                    continue
                if not _may_send(session, "control", context):
                    _requeue_front(session.control_out, out)
                    return
                if out is _SENTINEL:
                    return
                if out is _ABORT:
                    context.abort(grpc.StatusCode.UNAVAILABLE,
                                  "harness: contract-version negotiation probe (C13.2)")
                    return
                yield out

        def SafetyStream(self, request_iterator, context):
            if not _claim_stream(session, "safety", context):
                print("harness: REFUSED a second SafetyStream while one is open. This harness "
                      "serves one adapter.", file=sys.stderr, flush=True)
                context.abort(grpc.StatusCode.FAILED_PRECONDITION,
                              "harness: a SafetyStream is already open; this harness serves "
                              "exactly one adapter")
                return
            session.safety_established.set()
            # The control plane sends its headers when it accepts a SafetyStream, and an adapter
            # may wait for them before it counts its estop path as open. grpcio sends them only
            # with the first message unless the handler sends them, and the first message here is
            # an estop, so they are sent now.
            context.send_initial_metadata(())

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
            while not session.closing.is_set():
                try:
                    out = session.safety_out.get(timeout=_STREAM_POLL)
                except queue.Empty:
                    continue
                if not _may_send(session, "safety", context):
                    _requeue_front(session.safety_out, out)
                    return
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
            while not session.closing.is_set():
                try:
                    out = session.edge_out.get(timeout=_STREAM_POLL)
                except queue.Empty:
                    continue
                if out is _SENTINEL:
                    return
                yield out

    return EdgeServicer()


def _cause_is_checkable(cid: str, cause: str) -> bool:
    """Whether C0.1 has a test that can contradict `cause` recorded on row `cid`.

    Kept beside C0.1's tests and in step with them: a cause C0.1 cannot contradict must not
    be counted as checked.
    """
    if cause in (SkipCause.NO_TELEMETRY_IN_RUN.value, SkipCause.ADAPTER_DECLARES_NO_ACTION.value):
        return True
    if cause in (SkipCause.OPTIONAL_DECLINED.value, SkipCause.OPTIONAL_NOT_EXHIBITED.value):
        return cid in _DECLINE_ARMS
    return False


class _Driver:
    """Runs the checks and records results, given an established session."""

    def __init__(self, session: _Session, report: Report, port: int,
                 witness_path: str | None = None) -> None:
        # The motion witness for C5.7: a file of the robot's actual speed over time, written
        # by something other than the adapter's own confirmation path -- a simulator's plant
        # model, or an external reference on hardware. None when the run has none.
        self._witness_path = witness_path
        # The SupportedAction catalog the adapter declared on its scan reply, and the one entry
        # every acceptance probe carries. None until scan answers; an empty catalog leaves
        # `_served` None and the rows that need an accepted assignment are skipped.
        self._catalog = None
        self._served = None
        self._registered_robot = None
        # Set when a TelemetryPayload is observed. Read by the skip-cause
        # contradiction check: claiming no-telemetry-in-run while telemetry
        # WAS seen is a false reason, and the run says so.
        self.telemetry_seen = False
        self._hardware_baseline = 0
        self.s = session
        self.r = report
        self.port = port
        self._next_command_id = 1
        self._sent_arms: dict[int, str] = {}   # command_id -> the arm it carried
        self._goals = _FixtureGoals()
        # Component names from the CapabilitiesSnapshot that `scan` returns (C12.1). None
        # until scan answers. C6.1 cross-checks the first post-reconnect payload against it.
        self._scan_components = None
        # The operating subjects the CapabilitiesSnapshot declared honoured (FA-R1). None
        # until scan answers; an empty list is a DIFFERENT fact from None -- "declared
        # none", which FA-R1 says is not a breach, against "the adapter never answered
        # scan", which is a C12.1 failure. C17 reads both and says which it saw.
        self._honoured_subjects = None
        # The sensor streams the CapabilitiesSnapshot declared, read by C18. None until
        # scan answers; an empty list is a DIFFERENT fact from None — "this robot has no
        # streams to offer", which is conformant and makes the duty cycle inapplicable,
        # against "the adapter never answered scan", which is a C12.1 failure.
        self._sensor_streams = None
        self._pushback: list = []  # messages peeked-then-returned to the stream
        # The terminal ActionStatusUpdate for the task the harness cancels in C3.6, read
        # by C16.1. None until that cancel runs, and None afterwards if none arrived.
        self._terminal_status = None
        # Command arms the adapter DECLINED with unsupported=true. C0.1 reads this to
        # contradict an `optional-declined` skip recorded for a command that was never
        # declined -- the cause that meant two opposite things until it was split.
        self._declined_arms: set[str] = set()
        self._contract_ok = False  # set at the hello (C13.1)
        # the adapter's own contract_version (C13.1), for C3.8 grading
        self._reported_contract = ""
        # the contract version this run's HelloAck agreed for the connection (_negotiate)
        self._negotiated = ""
        # The Stop Latch (RFC-0001 #fleet-adapter-protocol-the-stop-latch). The harness clears
        # every stop it issues before the next check gives the robot work, as a 1.4.0 control
        # plane clears one: the stop's fence on ControlStream, an EstopClear, then the
        # EstopClearAck that says the latch is lifted. `_fences` maps each stop ID whose fence
        # was sent to the command_id that carried it; `_unlifted` lists the stop IDs whose
        # clear drew no EstopClearAck, which a later refusal names in its detail.
        self._fences: dict[str, int] = {}
        self._unlifted: list[str] = []
        # Set by _check_edge when the EdgeStream is up and C8.1/C8.2 are graded: C8.3 is then
        # owed, and runs last (_check_edge_stop).
        self._edge_ready = False

    # -- small helpers ---------------------------------------------------------

    def _recv_control(self):
        """One control-plane message, from the pushback list first and the queue second.

        THE SENTINEL CAN ARRIVE BY EITHER PATH, and testing only the queue is how an
        abandoned run used to die. A check that peeks a message and puts it back pushes the
        sentinel back too when that is what it peeked (`_check_register` does exactly this,
        so the fencing checks still see the stream close). The next caller then took the
        pushback branch, got a bare `object()` back as if it were a message, and called
        `WhichOneof` on it -- an AttributeError in the driver thread instead of the EOFError
        every caller here already handles.

        On the ordinary paths that surfaced as one odd row. On the `--run-timeout` path it
        was worse: the driver thread is a daemon the caller has already abandoned, so the
        AttributeError reached `threading`'s default excepthook, which wrote a traceback to
        stderr while the main thread was finalising the interpreter -- and finalisation that
        cannot take the stderr lock is a `Fatal Python error`, not an exit code. The harness
        aborted with SIGABRT where it had chosen to exit 2.
        """
        item = self._pushback.pop(0) if self._pushback else self.s.control_in.get(
            timeout=_GET_TIMEOUT)
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
        ESTOP_STATE_STOPPING ("received and braking; rest not yet confirmed -- never a stop").
        C5.2 and C14.1
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

    def _recv_estop_ack(self, estop_id: str, timeout: float | None = None):
        """The first EstopAck on SafetyStream that names ``estop_id``, and what came before it.

        The first SafetyStream message need not answer the stop just sent. A 1.4.0 adapter
        reports its local latch (a LocalStopReport) on each SafetyStream that opens, and every
        latched stop ID (an EstopAck naming it) with each refusal under the latch (RFC-0001
        #fleet-adapter-protocol-the-stop-latch, *Report*). Reading the first message as the
        answer graded such a report as the stop's acknowledgement, and timed it.

        Returns ``(ack, passed)``: the ack, or None when none came within ``timeout`` (default
        ``_GET_TIMEOUT``), and a list naming each message passed over. Raises EOFError when the
        stream closes.
        """
        deadline = time.monotonic() + (_GET_TIMEOUT if timeout is None else timeout)
        passed: list[str] = []
        while True:
            left = deadline - time.monotonic()
            if left <= 0:
                return None, passed
            try:
                msg = self.s.safety_in.get(timeout=min(left, 0.2))
            except queue.Empty:
                continue
            if msg is _SENTINEL:
                raise EOFError("safety stream closed by adapter")
            arm = msg.WhichOneof("payload")
            if arm == "estop_ack" and msg.estop_ack.estop_id == estop_id:
                return msg.estop_ack, passed
            named = getattr(msg, arm).estop_id if arm in ("estop_ack", "estop_clear_ack") else ""
            passed.append(f"{arm}({named!r})" if named else str(arm))

    def _latch_binds(self) -> bool:
        """Whether the Stop Latch binds this run's connection.

        RFC-0001 #fleet-adapter-protocol-the-stop-latch, *Version*: its rules bind a connection
        that negotiated contract 1.4.0, and the control plane sends no EstopClear to one below
        it. The harness agrees 1.4.0 with an adapter that reported a 1.4 version and the
        reported 1.3.x otherwise (_negotiate), so the version that counts is the adapter's own
        contract_version.
        """
        if not self._contract_ok:
            return False
        major, minor, _ = (int(p) for p in self._reported_contract.split("."))
        return (major, minor) >= (1, 4)

    def _hello_ack(self, reported: str):
        """The HelloAck that accepts a connection whose adapter reported ``reported``.

        negotiated_contract_version is set ONLY when the reported version is in range: an empty
        value on the wire always means "no compatible contract was agreed", never "compatible but
        unrecorded". It is the version agreed for this connection, as a control plane agrees it
        (_negotiate): 1.4.0 for a 1.4 adapter, and the 1.3.x a 1.3 adapter reported, which holds
        the connection to 1.3.x (_held_below_latch).
        """
        ack = pb.HelloAck(accepted=True, negotiated_protocol_version="fleet_adapter.v1")
        agreed = _negotiate(reported)
        if agreed:
            ack.negotiated_contract_version = agreed
        return ack

    def _held_below_latch(self) -> bool:
        """Whether this run's connection is held to a 1.3.x contract, and so sent no AssignAction.

        RFC-0001 #fleet-adapter-protocol-package-and-versioning: a 1.3.x adapter's connection
        negotiates 1.3.x and is held to it: "it is sent every stop, and no AssignAction"
        (candidate filter 9). The harness does as a 1.4.0 control plane does, so a row that needs
        an assignment does not bind such a connection, and records ``contract-predates-obligation``.
        A connection that agreed no version at all is graded as before (C13.1 fails it).
        """
        return self._contract_ok and not self._latch_binds()

    def _held_detail(self, needs: str = "an assignment") -> str:
        """The skip detail of a row that needs ``needs`` on a connection held to 1.3.x."""
        agreed = _negotiate(self._reported_contract)
        return (SkipCause.CONTRACT_PREDATES_OBLIGATION.value
                + f": the connection negotiated contract {agreed}, and a control plane "
                  f"sends a 1.3.x connection every stop and no AssignAction (RFC-0001 "
                  f"#fleet-adapter-protocol-package-and-versioning), so this row, which needs "
                  f"{needs}, does not bind it")

    def _fence(self, estop_id: str, reason: str) -> int:
        """Send a stop's fence: the same Estop, as Command.estop on ControlStream."""
        cid = self._send_command(estop=pb.Estop(estop_id=estop_id, reason=reason,
                                                issued_by="harness"))
        self._fences[estop_id] = cid
        return cid

    def _clear_stop(self, estop_id: str) -> bool:
        """Clear a stop the harness issued, and wait for the adapter to say it is lifted.

        What a 1.4.0 control plane does (RFC-0001 #fleet-adapter-protocol-the-stop-latch,
        *The clear*). A clear names one stop ID and lifts it only once the stop's fence has
        arrived on ControlStream, so the fence goes first if it has not been sent. The
        EstopClear carries the leader term, and the adapter answers it with an EstopClearAck
        naming the ID when the clear takes effect. Before the harness cleared its stops, every
        check after a stop graded an adapter that keeps the latch on refusals the latch
        requires.

        No clear is sent below 1.4.0: the control plane sends such a connection none. A clear
        that draws no EstopClearAck is recorded and grades nothing here; C5.8 grades the clear
        of its own stop. A later row that sees ROBOT_UNAVAILABLE is graded as before, and its
        detail names the stop. Returns whether the EstopClearAck came.
        """
        if not self._latch_binds():
            return False
        if estop_id not in self._fences:
            self._fence(estop_id, "conformance: the fence of a stop the harness clears")
        self.s.safety_out.put(pb.ControlPlaneSafetyMessage(
            robot_id=ROBOT, estop_clear=pb.EstopClear(
                estop_id=estop_id, cleared_by="harness",
                cleared_at_ms=int(time.time() * 1000), leader_term=_LEADER_TERM)))
        deadline = time.monotonic() + _CLEAR_WAIT
        while time.monotonic() < deadline:
            try:
                msg = self.s.safety_in.get(timeout=0.2)
            except queue.Empty:
                continue
            if msg is _SENTINEL:
                break
            if (msg.WhichOneof("payload") == "estop_clear_ack"
                    and msg.estop_clear_ack.estop_id == estop_id):
                return True
        self._unlifted.append(estop_id)
        print(f"harness: cleared stop {estop_id!r} (fence, then EstopClear) and received no "
              f"EstopClearAck within {_CLEAR_WAIT:.0f}s. A 1.4.0 adapter that keeps the stop "
              f"latch lifts it and says so; one that keeps no latch sends nothing.",
              file=sys.stderr, flush=True)
        return False

    def _unlifted_note(self, rejection) -> str:
        """A detail suffix for a ROBOT_UNAVAILABLE refusal while a cleared stop is not lifted."""
        if rejection != pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE or not self._unlifted:
            return ""
        return (f". The harness cleared stop(s) {self._unlifted} (fence, then EstopClear) and "
                f"received no EstopClearAck, so the robot may still be latched; a 1.4.0 adapter "
                f"lifts a cleared stop and acknowledges it (RFC-0001 The Stop Latch, The clear)")

    def _send_command(self, **command_kwargs):
        cid = self._next_command_id
        self._next_command_id += 1
        cmd = pb.Command(command_id=cid, robot_id=ROBOT, leader_term=_LEADER_TERM,
                         **command_kwargs)
        self._sent_arms[cid] = cmd.WhichOneof("command")
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
                    return self._note_decline(msg.command_result)
                if kind == "action_status":
                    held.append(msg)
        finally:
            self._pushback.extend(held)

    def _note_decline(self, result):
        """Record an `unsupported=true` answer against the arm it declined.

        The one observable that makes a decline a decline. C0.1 contradicts an
        `optional-declined` skip recorded for an arm that never appears here: an adapter
        that answered a command, or that said nothing at all, declined nothing, and a cause
        that cannot be read literally cannot be trusted with a profile verdict.
        """
        if result.unsupported:
            arm = self._sent_arms.get(result.command_id)
            if arm:
                self._declined_arms.add(arm)
        return result

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
                return collected, self._note_decline(msg.command_result)

    # -- checks ---------------------------------------------------------------

    def run(self) -> None:
        got_control = self.s.control_established.wait(_STREAM_TIMEOUT)
        got_safety = self.s.safety_established.wait(_STREAM_TIMEOUT)

        # C1 — Connection and handshake
        if got_control and got_safety:
            self.r.add("C1.1", "Open ControlStream and SafetyStream together",
                       CheckStatus.PASS, detail=(
                           f"ControlStream and SafetyStream both established within "
                           f"{_STREAM_TIMEOUT}s"))
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
                       detail=("first ControlStream message was AdapterHello, " if proto_ok
                               else "") + f"protocol_version={first.hello.protocol_version!r}")
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
        self._reported_contract = reported
        # RFC-0001's ConformanceReport schema defines `contractVersion` as "the contract
        # version the result was earned against", recorded BESIDE `suiteRevision` and
        # "never folded into it". That is the version the ADAPTER reported, not the
        # constant this suite attests against: stamping the suite's own value made the
        # published report attribute 1.1.0 to an adapter whose hello said 1.0.0, which is
        # also what adapters/REGISTRY.md records for all three reference adapters. The
        # suite's attestation basis is not lost — it is `suiteRevision` in the artefact.
        if contract_ok:
            self.r.contract_version = reported
        self.r.add("C13.1", "AdapterHello reports a supported contract_version",
                   CheckStatus.PASS if contract_ok else CheckStatus.FAIL,
                   detail=(f"contract_version={reported!r}" if contract_ok else why))

        # Accept the handshake so the adapter proceeds.
        ack = self._hello_ack(reported)
        self._negotiated = ack.negotiated_contract_version
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

        # C12 first: every assignment below carries an action type the adapter DECLARED, with
        # the parameters its catalog marks required, so the catalog is read before any is sent.
        self._check_scan()             # C12.1
        self._check_capabilities_push()  # C12.2
        self._when_served(self._check_catalog_enforced)  # C19
        self._when_served(self._check_fencing)           # C3
        self._when_served(self._check_cancel_disposition)  # C3.6
        self._when_served(self._check_leases)            # C4
        self._when_served(self._check_action_status)     # C16
        # C5 clears each stop it issues before it returns, so the checks below give work to a
        # robot whose latch is lifted (RFC-0001 #fleet-adapter-protocol-the-stop-latch).
        self._check_estop()            # C5
        self._check_telemetry()        # C6
        self._check_optional_decline() # C7
        self._check_edge(advertised_edge)  # C8.1, C8.2; C8.3 runs last
        self._check_verify_metrics()   # C9
        self._check_update_progress()  # C10
        self._check_pause_resume()     # C11
        # C3.9 after C5, not beside C3: C5.7 needs the robot C16 set moving, and C3.9 waits
        # for its preemptor to finish, leaving the robot at rest. Its tokens sit between
        # C16's and C17's, so it must still come before C17.
        self._when_served(self._check_cancel_names_its_action)  # C3.9
        self._check_zone_restrictions()  # C17 — MUST run after C12 (reads the declaration)
        self._check_sensor_streams()   # C18 — MUST run after C12 (reads the declaration)
        self._check_artifact_verification()  # C7.2 (model) + C7.3 (firmware body)
        self._check_version_bound()    # C15 (no adapter traffic; safe before the drop)
        # An ACCEPTING reconnect, before the refusing one below. C6.1 and C2.2 are both
        # about what happens on a SUCCESSFUL re-registration, which the version-refusal
        # path cannot exercise because it refuses by design.
        self._check_accepting_reconnect()  # C6.1 + C2.2
        # C5.8 after the reconnect: its tokens sit above the cadence drive's, and it lifts its
        # own stop before the refused session, where no clear may be sent.
        self._when_served(self._check_latch_holds_work)  # C5.8
        # C13.2/C14.1 drop the ControlStream, so nothing that needs it may follow.
        self._check_negotiation_refusal()  # C3.1 + C13.2 + C14.1
        # LAST — C8.3's edge stop sets the robot's local latch, which no harness message lifts
        # (RFC-0001 #fleet-adapter-protocol-the-stop-latch). It needs only the EdgeStream.
        self._check_edge_stop()        # C8.3
        self._record_unverified()      # documented-but-unexercised IDs, stated explicitly

    # -- the served action ------------------------------------------------------

    def _when_served(self, check) -> None:
        """Run `check`, or record its rows skipped when the adapter declared no action.

        Every check in _NEEDS_SERVED_ACTION puts an AssignAction on the wire. On a connection held
        to 1.3.x none is sent, and the check's rows record ``contract-predates-obligation``.
        """
        if self._held_below_latch() and check.__name__ in _NEEDS_SERVED_ACTION:
            for cid, title, level in _NEEDS_SERVED_ACTION[check.__name__]:
                self.r.add(cid, title, CheckStatus.SKIP, level=level, detail=self._held_detail())
            return
        if self._served is not None:
            check()
            return
        why = ("the adapter answered no scan, so no catalog was read (C12.1)"
               if self._catalog is None else
               "the adapter's CapabilitiesSnapshot declares no SupportedAction")
        for cid, title, level in _NEEDS_SERVED_ACTION[check.__name__]:
            self.r.add(cid, title, CheckStatus.SKIP, level=level,
                       detail=f"{SkipCause.ADAPTER_DECLARES_NO_ACTION.value}: {why}, so no "
                              f"assignment could carry a type it claims to serve")

    def _payload_for(self, action, needs_seconds: float | None = None, *,
                     drop: str | None = None) -> bytes:
        """A payload with a fixture target and every parameter `action` declares required.

        ``needs_seconds`` asks the fixture for a goal far enough away to keep the robot
        moving that long; None uses the first fixture pose and moves the fixture nowhere.
        ``drop`` removes one parameter, for the probe that omits it on purpose.
        """
        if needs_seconds is None:
            x, y = self._goals._poses[0]
            doc = {"x": x, "y": y}
        else:
            doc = json.loads(self._goals.payload(needs_seconds))
        for param in (action.params if action is not None else ()):
            if param.required and param.name not in doc:
                doc[param.name] = _param_value(param)
        if drop is not None:
            doc.pop(drop, None)
        return json.dumps(doc).encode()

    def _assign(self, *, needs_seconds: float | None = None, **fields):
        """An AssignAction carrying the served type and a complete payload.

        Every probe that could be accepted says what it asks for: an action type the adapter
        declared, with the parameters that type declares required. A probe with no type, or
        no target, is refused by a correct adapter for a reason no check was about. A
        ``Custom`` entry keyed by ``custom_type`` is asked for by that verb, as a control plane
        asks for it; without it, an adapter that keys its verbs refuses the assignment as
        naming none.
        """
        fields.setdefault("action_type", self._served.action_type)
        if fields["action_type"] == self._served.action_type:
            fields.setdefault("custom_type", _custom_type_of(self._served))
        if "payload_json" not in fields:
            fields["payload_json"] = self._payload_for(self._served, needs_seconds)
        return pb.AssignAction(**fields)

    def _cancel(self, action_id: str, token: int, reason: str) -> None:
        cid = self._send_command(cancel_action=pb.CancelAction(
            action_id=action_id, reason=reason, fencing_token=token))
        try:
            self._await_result(cid)
        except (EOFError, queue.Empty):
            pass

    def _check_capabilities_push(self) -> None:
        """C12.2 (MUST) — the snapshot pushed at registration carries the scan reply's catalog.

        At v0.4 no control plane issues `scan`, so the catalog reaches the plane only on a
        snapshot the adapter sends unasked, at registration and whenever it changes. An
        adapter that answers `scan` correctly and never pushes leaves the plane with no
        catalog, and every action against it is refused at admission; one that pushes a
        different catalog from the one it answers `scan` with admits work it then refuses.
        """
        title = ("A CapabilitiesSnapshot is pushed at registration, carrying the scan reply's "
                 "catalog")
        if not self._registered_robot:
            self.r.add("C12.2", title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: the adapter "
                              f"registered no robot, so there was no registration to push at")
            return
        if self._catalog is None:
            self.r.add("C12.2", title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: scan was not "
                              f"answered (C12.1), so there is no catalog to compare a push with")
            return
        deadline = time.monotonic() + _PUSH_WAIT
        while not self._pushes_for_robot() and time.monotonic() < deadline:
            time.sleep(0.1)
        pushes = self._pushes_for_robot()
        if not pushes:
            self.r.add("C12.2", title, CheckStatus.FAIL,
                       detail=f"no unsolicited `capabilities` message for "
                              f"{self._registered_robot!r} arrived, from registration to "
                              f"{_PUSH_WAIT:.0f}s after the scan reply. At v0.4 no control plane "
                              f"issues scan, so the plane would hold no catalog for this robot")
            return
        pushed = pushes[-1]
        want, got = _catalog_key(self._catalog), _catalog_key(pushed.supported_actions)
        types = [a.action_type for a in pushed.supported_actions]
        if got == want:
            self.r.add("C12.2", title, CheckStatus.PASS,
                       detail=f"{len(pushes)} snapshot(s) pushed for "
                              f"{self._registered_robot!r}; the latest declares {types}, "
                              f"identical to the scan reply (types, required capabilities, "
                              f"parameters)")
        else:
            self.r.add("C12.2", title, CheckStatus.FAIL,
                       detail=f"the pushed snapshot declares {got} and the scan reply {want}; "
                              f"the plane admits work against the pushed catalog")

    def _pushes_for_robot(self) -> list:
        robot = self._registered_robot
        return [snap for _t, snap in self.s.capabilities_pushed
                if not snap.robot_id or snap.robot_id == robot]

    def _check_catalog_enforced(self) -> None:
        """C19.1 - C19.4 (MUST) — an adapter refuses what its catalog says it cannot serve.

        RFC-0001: "An adapter MUST NOT treat an `action_type` it does not recognise as a
        nearest-neighbour verb; it rejects the assignment." An adapter that runs every
        assignment as navigation drives to the origin on a `Clean`, and on a `Navigate` with no
        target. Each row pairs the refusal with a CONTROL: the same assignment, corrected, must
        be accepted. An adapter that refuses everything cannot pass any row, and a refusal for
        a reason other than the one probed is a failure of the row, not a pass.

        C19.1 names an undeclared type; C19.3 states none, which an adapter can take as its
        catalog's sole type and still pass C19.1. C19.2 omits a required parameter; C19.4
        sends a declared enum parameter a value outside its `allowed` set.

        A probe or control that names a declared entry carries that entry's ``custom_type``
        when it is a keyed ``Custom`` entry, so the adapter refuses it for the reason probed
        and not for a verb left unnamed. C19.1's and C19.3's probes name no declared entry and
        carry none; C19.5's carries none on purpose.
        """
        declared = {a.action_type for a in self._catalog}
        undeclared = next(t for t in _UNDECLARED_TYPE_CANDIDATES if t not in declared)
        served = self._served
        tok = _C19_TOKENS

        # C19.1 -- an undeclared type, with a complete payload.
        title = "An action type the adapter did not declare is declined INVALID_ACTION"
        refused = self._probe_refusal(
            "t-undeclared-type", tok[0], undeclared, self._payload_for(served))
        control = self._probe_control(
            "t-declared-type", tok[1], served.action_type, self._payload_for(served),
            custom_type=_custom_type_of(served))
        self._grade_refusal_pair(
            "C19.1", title, refused, control,
            probe=f"action_type={undeclared!r} (declared: {sorted(declared)})",
            fixed=f"action_type={served.action_type!r}{_custom_note(served)}")

        # C19.2 -- a declared type, missing a parameter its catalog entry marks required.
        title = "An assignment missing a declared required parameter is declined INVALID_ACTION"
        with_required = [a for a in [served, *self._catalog]
                         if any(p.required for p in a.params)]
        if not with_required:
            self.r.add("C19.2", title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DECLARES_NO_REQUIRED_PARAM.value}: no "
                              f"declared action marks a parameter required "
                              f"({sorted(declared)}), so no assignment could omit one")
        else:
            action = with_required[0]
            names = [p.name for p in action.params if p.required]
            drop = "x" if "x" in names else names[0]
            refused = self._probe_refusal(
                "t-missing-param", tok[2], action.action_type,
                self._payload_for(action, drop=drop), custom_type=_custom_type_of(action))
            control = self._probe_control(
                "t-complete-param", tok[3], action.action_type, self._payload_for(action),
                custom_type=_custom_type_of(action))
            self._grade_refusal_pair(
                "C19.2", title, refused, control,
                probe=(f"{action.action_type}{_custom_note(action)} without {drop!r} "
                       f"(declared required: {names})"),
                fixed=f"{action.action_type}{_custom_note(action)} with every required parameter")

        # C19.3 -- no action type at all, with a complete payload.
        title = "An assignment that states no action type is declined INVALID_ACTION"
        refused = self._probe_refusal("t-untyped", tok[4], "", self._payload_for(served))
        control = self._probe_control(
            "t-typed", tok[5], served.action_type, self._payload_for(served),
            custom_type=_custom_type_of(served))
        self._grade_refusal_pair(
            "C19.3", title, refused, control,
            probe=f"no action_type (declared: {sorted(declared)})",
            fixed=f"action_type={served.action_type!r}{_custom_note(served)}")

        # C19.4 -- a declared enum parameter carrying a value outside its `allowed` set.
        title = ("A parameter value outside its declared allowed values is declined "
                 "INVALID_ACTION")
        enums = [(a, p) for a in [served, *self._catalog] for p in a.params
                 if p.kind == "enum" and p.allowed]
        if not enums:
            self.r.add("C19.4", title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DECLARES_NO_ENUM_PARAM.value}: no declared "
                              f"action has an enum parameter with allowed values "
                              f"({sorted(declared)}), so there is no value to refuse")
        else:
            self._probe_enum_value(enums[0], tok)
        self._check_custom_without_type(tok[8])  # C19.5

    def _probe_enum_value(self, enum, tok) -> None:
        """C19.4's probe and control, for the first declared enum parameter."""
        title = ("A parameter value outside its declared allowed values is declined "
                 "INVALID_ACTION")
        action, param = enum
        outside = _UNALLOWED_VALUE
        while outside in param.allowed:
            outside += "-x"
        doc = json.loads(self._payload_for(action))
        refused = self._probe_refusal(
            "t-unallowed-value", tok[6], action.action_type,
            json.dumps({**doc, param.name: outside}).encode(),
            custom_type=_custom_type_of(action))
        control = self._probe_control(
            "t-allowed-value", tok[7], action.action_type,
            json.dumps({**doc, param.name: param.allowed[0]}).encode(),
            custom_type=_custom_type_of(action))
        self._grade_refusal_pair(
            "C19.4", title, refused, control,
            probe=(f"{action.action_type}{_custom_note(action)} with {param.name}={outside!r} "
                   f"(declared allowed: {list(param.allowed)})"),
            fixed=f"{param.name}={param.allowed[0]!r}")

    def _check_custom_without_type(self, token: int) -> None:
        """C19.5 (MUST) — a Custom action with no ``custom_type`` is accepted when its payload's
        ``operation`` names a verb the adapter declared.

        ``custom_type`` is optional. RFC-0001's AssignAction declares it "set only for a Custom
        action that names one", and *One entry per Custom verb* says an adapter "takes a
        `Custom` action's verb from `AssignAction.custom_type` (and `ValidateAction.custom_type`)
        when it is set, and from the payload's `operation` otherwise"
        (rfcs/rfc-0001-core-spec/fleet-adapter-protocol.md, the `AssignAction` block and *One
        entry per Custom verb*). A control plane below 1.4.0 never sets it. An adapter that
        requires it refuses every Custom action such a control plane sends.

        The probe is a Custom assignment with no ``custom_type``, the verb in ``operation``,
        and every parameter the entry declares required. The verb is the first declared
        Custom entry's ``custom_type``, or the first allowed value of an unkeyed entry's
        ``operation`` enum. A Custom action that names no verb at all is not graded: no
        adapter obligation states what it must do with one.
        """
        title = "A Custom action that names its verb only in the payload is accepted"
        entry, verb = None, ""
        for action in self._catalog:
            if action.action_type != "Custom":
                continue
            operation = next((p for p in action.params
                              if p.name == "operation" and p.kind == "enum" and p.allowed), None)
            verb = action.custom_type or (operation.allowed[0] if operation else "")
            if verb:
                entry = action
                break
        if entry is None:
            self.r.add("C19.5", title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DECLARES_NO_CUSTOM_VERB.value}: no declared "
                              f"Custom entry names a verb (by custom_type or an operation "
                              f"enum), so there is no Custom action the adapter claims to serve")
            return
        doc = json.loads(self._payload_for(entry))
        doc["operation"] = verb
        res = self._probe_control("t-custom-no-type", token, "Custom", json.dumps(doc).encode())
        probe = f"Custom with no custom_type and operation={verb!r}"
        if res is None:
            self.r.add("C19.5", title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: no CommandResult "
                              f"for the {probe}")
        elif res.accepted:
            self.r.add("C19.5", title, CheckStatus.PASS,
                       detail=f"{probe}: accepted. The verb was read from the payload, as "
                              f"RFC-0001 states for an action with no custom_type")
        else:
            self.r.add("C19.5", title, CheckStatus.FAIL,
                       detail=f"{probe}: declined (rejection={res.rejection}, "
                              f"{res.message!r}). custom_type is optional: an adapter takes "
                              f"the verb from the payload's operation when it is not set, so "
                              f"a control plane that does not set it has its Custom work "
                              f"refused")

    def _probe_refusal(self, action_id: str, token: int, action_type: str, payload: bytes,
                       custom_type: str = ""):
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", fencing_token=token,
            action_type=action_type, payload_json=payload, custom_type=custom_type))
        try:
            res = self._await_result(cid).assign_action
        except (EOFError, queue.Empty):
            return None
        if res.accepted:
            self._cancel(action_id, token, "harness: C19 cleanup of a wrongly accepted probe")
        return res

    def _probe_control(self, action_id: str, token: int, action_type: str, payload: bytes,
                       custom_type: str = ""):
        cid = self._send_command(assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", fencing_token=token,
            action_type=action_type, payload_json=payload, custom_type=custom_type))
        try:
            res = self._await_result(cid).assign_action
        except (EOFError, queue.Empty):
            return None
        if res.accepted:
            self._cancel(action_id, token, "harness: C19 control cleanup")
        return res

    def _grade_refusal_pair(self, cid, title, refused, control, *, probe: str,
                            fixed: str) -> None:
        if refused is None or control is None:
            self.r.add(cid, title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: no "
                              f"CommandResult for the {'probe' if refused is None else 'control'}")
            return
        invalid = pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION
        refused_right = not refused.accepted and refused.rejection == invalid
        if refused_right and control.accepted:
            self.r.add(cid, title, CheckStatus.PASS,
                       detail=f"{probe}: declined INVALID_ACTION; control ({fixed}): accepted")
        elif refused.accepted:
            self.r.add(cid, title, CheckStatus.FAIL,
                       detail=f"{probe}: ACCEPTED. The adapter took an assignment its own "
                              f"catalog says it cannot serve")
        elif not refused_right:
            self.r.add(cid, title, CheckStatus.FAIL,
                       detail=f"{probe}: declined, but with rejection={refused.rejection}, not "
                              f"INVALID_ACTION ({refused.message!r})")
        else:
            self.r.add(cid, title, CheckStatus.FAIL,
                       detail=f"{probe}: declined INVALID_ACTION, but the control ({fixed}) "
                              f"was declined too (rejection={control.rejection}, "
                              f"{control.message!r}); a refusal that does not depend on what "
                              f"was wrong establishes nothing")

    def _check_fencing(self) -> None:
        # C3.3 — absent token must be rejected as MISSING (not treated as 0).
        cid = self._send_command(assign_action=self._assign(
            action_id="t-missing", assignment_id="a-missing"))
        res = self._await_result(cid).assign_action
        ok = (not res.accepted
              and res.rejection == pb.ASSIGN_ACTION_REJECTION_MISSING_FENCING_TOKEN)
        self.r.add("C3.3", "Reject AssignAction with absent fencing token (MISSING)",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail=("AssignAction with no fencing_token: accepted=False, rejection "
                           "MISSING_FENCING_TOKEN" if ok else
                           f"accepted={res.accepted} rejection={res.rejection}"))

        # C3.2 (accept) + C3.5 — accept a fresh token and echo it back.
        # ONE payload, reused BYTE-IDENTICALLY by the C3.4 re-delivery below: C3.4 asserts
        # that an identical re-delivery is re-acked rather than treated as stale, so the two
        # AssignActions must not differ in any field. ~2 s of window -> 4 s of travel.
        t1_payload = self._payload_for(self._served, 4.0)
        fresh = _FENCING_FRESH_TOKEN
        cid = self._send_command(assign_action=self._assign(
            action_id="t1", assignment_id="a1", fencing_token=fresh, payload_json=t1_payload))
        res = self._await_result(cid).assign_action
        accept_ok = res.accepted
        echo_ok = (res.HasField("accepted_fencing_token")
                   and res.accepted_fencing_token == fresh)
        # A PRECONDITION, not the catalog's C3.2 (which is the stale rejection only): a stale
        # token cannot be demonstrated without a high-water mark for it to be stale against.
        # Structurally identical to C0.3 for the lease. See CONFORMANCE.md "C0.x — harness
        # context".
        self.r.add("C0.4", "Harness context: a fresh (higher) fencing token is accepted",
                   CheckStatus.PASS if accept_ok else CheckStatus.FAIL, level="MAY",
                   detail="" if accept_ok else f"accepted={res.accepted}")
        self.r.add("C3.5", "Echo the fenced token in AssignActionResult",
                   CheckStatus.PASS if echo_ok else CheckStatus.FAIL,
                   detail=(f"AssignActionResult echoed accepted_fencing_token="
                           f"{res.accepted_fencing_token} for token {fresh}" if echo_ok else
                           "accepted_fencing_token not echoed"))

        # C3.2 (stale) — a token <= the highest accepted must be rejected STALE.
        cid = self._send_command(assign_action=self._assign(
            action_id="t-stale", assignment_id="a-stale", fencing_token=3))
        res = self._await_result(cid).assign_action
        stale_ok = (not res.accepted
                    and res.rejection == pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN)
        self.r.add("C3.2", "Reject a stale (<= highest) fencing token (STALE)",
                   CheckStatus.PASS if stale_ok else CheckStatus.FAIL,
                   detail=(f"token 3 after token {fresh} was accepted: accepted=False, "
                           f"rejection STALE_FENCING_TOKEN" if stale_ok else
                           f"accepted={res.accepted} rejection={res.rejection}"))

        # C3.4 — identical re-delivery of the current assignment must be
        # idempotently re-acked, NOT treated as stale.
        cid = self._send_command(assign_action=self._assign(
            action_id="t1", assignment_id="a1", fencing_token=fresh, payload_json=t1_payload))
        res = self._await_result(cid).assign_action
        redeliver_ok = res.accepted
        self.r.add("C3.4", "Idempotently re-ack identical re-delivery",
                   CheckStatus.PASS if redeliver_ok else CheckStatus.FAIL,
                   detail=(f"identical re-delivery of t1 at token {fresh}: accepted=True"
                           if redeliver_ok else
                       "identical re-delivery of the current assignment was "
                       "rejected instead of re-acked"))

    def _check_cancel_disposition(self) -> None:
        # C3.6 — cancel_action reports a CancelDisposition (capability-loss
        # reassignment). The field is additive: UNSPECIFIED is treated by the
        # control plane as STOPPED_SAFELY, so a minimal adapter is compliant; an
        # adapter that distinguishes safe-stop / completed / recovered / a stop whose
        # rest it could not witness reports the specific value. Every value the contract
        # defines is valid, STOP_UNCONFIRMED (contract 1.4.0) included.
        # Uses the token accepted in _check_fencing (t1 @ _FENCING_FRESH_TOKEN).
        cid = self._send_command(cancel_action=pb.CancelAction(
            action_id="t1", reason="capability-loss", fencing_token=_FENCING_FRESH_TOKEN))
        res = self._await_result(cid).cancel_action
        valid = {
            pb.CANCEL_DISPOSITION_UNSPECIFIED,
            pb.CANCEL_DISPOSITION_STOPPED_SAFELY,
            pb.CANCEL_DISPOSITION_COMPLETED,
            pb.CANCEL_DISPOSITION_RECOVERED,
            pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED,
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

        # C16.1's TERMINAL half, captured here because this is where the harness drives a
        # task to an end. The catalog says "every task phase transition, terminal states
        # included"; the acceptance RUNNING update proves only the first clause, and a task
        # that ends without a terminal update is ended only by a timeout or a cancel. Held on
        # the driver and recorded once, with the transition half, in _check_action_status.
        self._terminal_status = self._await_action_status(
            "t1", timeout=_TERMINAL_STATUS_WAIT, states=_TERMINAL_ACTION_STATES())

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
        title = "Skip reasons are consistent with the run"
        contradictions = []

        claimed = [cid for cid, cause in recorded
                   if cause == SkipCause.NO_TELEMETRY_IN_RUN.value]
        if self.telemetry_seen and claimed:
            contradictions.append(
                f"{', '.join(claimed)} recorded {SkipCause.NO_TELEMETRY_IN_RUN.value} "
                f"but telemetry WAS observed; the stated reason is false")

        # `optional-declined` must mean a decline. The observable is unsupported=true on
        # the arm the check depends on; anything else -- an answered command, or silence --
        # is a different fact and must not borrow this cause.
        undeclined = [cid for cid, cause in recorded
                      if cause == SkipCause.OPTIONAL_DECLINED.value
                      and cid in _DECLINE_ARMS
                      and not any(a in self._declined_arms for a in _DECLINE_ARMS[cid])]
        if undeclined:
            contradictions.append(
                f"{', '.join(undeclined)} recorded {SkipCause.OPTIONAL_DECLINED.value} but the "
                f"adapter declined none of the arms those checks depend on "
                f"(declined: {sorted(self._declined_arms) or 'nothing'}); a path that was not "
                f"reached is not a feature that was declined")

        # `optional-not-exhibited` says the arm was ANSWERED and the optional behaviour then
        # not taken. A decline of that arm contradicts it: the behaviour's state was never
        # reached, and the honest cause is the decline.
        declined_not_exhibited = [
            cid for cid, cause in recorded
            if cause == SkipCause.OPTIONAL_NOT_EXHIBITED.value
            and cid in _DECLINE_ARMS
            and any(a in self._declined_arms for a in _DECLINE_ARMS[cid])]
        if declined_not_exhibited:
            contradictions.append(
                f"{', '.join(declined_not_exhibited)} recorded "
                f"{SkipCause.OPTIONAL_NOT_EXHIBITED.value} but the adapter declined the arm "
                f"it depends on (declined: {sorted(self._declined_arms)}); an optional "
                f"behaviour on a declined command was never reached, so it cannot have been "
                f"passed up")

        no_action = [cid for cid, cause in recorded
                     if cause == SkipCause.ADAPTER_DECLARES_NO_ACTION.value]
        if no_action and self._catalog:
            contradictions.append(
                f"{', '.join(no_action)} recorded {SkipCause.ADAPTER_DECLARES_NO_ACTION.value} "
                f"but the scan reply declares {[a.action_type for a in self._catalog]}")

        if contradictions:
            self.r.add("C0.1", title, CheckStatus.FAIL, detail="; ".join(contradictions))
        else:
            # Say how many of the recorded causes the tests above can contradict, not only
            # how many were recorded. The rest are recorded, not checked, and a count that
            # folded them in would claim a cross-check this run did not make.
            checkable = [(cid, cause) for cid, cause in recorded
                         if _cause_is_checkable(cid, cause)]
            unchecked = sorted({cause for cid, cause in recorded
                                if not _cause_is_checkable(cid, cause)})
            observed = "telemetry observed" if self.telemetry_seen else "no telemetry in run"
            self.r.add("C0.1", title, CheckStatus.PASS,
                       detail=(f"{len(checkable)} of {len(recorded)} recorded skip cause(s) "
                               f"are of a kind this run can contradict, and none was "
                               f"contradicted ({observed}; declined arms "
                               f"{sorted(self._declined_arms) or 'none'})"
                               + (f". The other {len(recorded) - len(checkable)} are recorded, "
                                  f"not checked: {unchecked}" if unchecked else "")))

    def _check_leases(self) -> None:
        # C4.2 — the lease self-stop IS observable, because the checklist requires the
        # adapter to emit an action_status reflecting the stop. Assign under a short
        # lease, stop renewing, and wait for the robot to report it. This is the primary
        # dual-execution safeguard (RFC-0001
        # #safety-assignment-lease-and-the-single-executor-guarantee); certifying an adapter
        # without exercising it was the gap this check closes.
        #
        # WHAT IS GRADED. ACTION_STATE_STOPPED says the stop was commanded; it is not
        # terminal (RFC-0001 safety chapter, *terminal*; the proto's ACTION_STATE_STOPPED
        # comment), so it never satisfies this row. The adapter then sends the action's
        # terminal report, once the robot is observed at rest or rest cannot be confirmed
        # within its stopping budget, and that report is what is graded. RFC-0001 names no
        # terminal state for a lease self-stop, so any state it calls terminal passes.
        #
        # WHAT A PASS HERE MEANS, stated on the row rather than left to be assumed. This
        # harness sees the wire, not the robot. There is no field on ActionStatusUpdate that
        # attests confirmation the way stop_initiated_at does on EstopAck (C5.3), so a pass
        # shows the end was REPORTED within the window and nothing more. An adapter that
        # sends its terminal report at T+0 while the base decelerates for a further 2.5s
        # passes this and violates the requirement; that was measured, not hypothesised.
        aid = "t-lease-expiry"
        cid = self._send_command(assign_action=self._assign(
            action_id=aid, assignment_id="a-lease", fencing_token=11,
            needs_seconds=8.0,   # C4.2 waits _LEASE_END_WAIT (4 s) -> 8 s of travel
            lease_generation=1, lease_duration_ms=500))
        res = self._await_result(cid).assign_action
        if not res.accepted:
            self.r.add("C4.2", "Self-stop when the assignment lease is not renewed",
                       CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: adapter refused "
                              "the lease-bearing assignment; cannot exercise expiry")
            return
        # Deliberately send NO renew_lease. The lease must lapse on the robot's timer.
        upd = self._await_action_status(aid, timeout=_LEASE_END_WAIT,
                                        states=_RFC_TERMINAL_ACTION_STATES())
        # A STOPPED the wait passed over is held on _pushback; it is named in the detail,
        # never graded.
        stop_seen = any(m.WhichOneof("payload") == "action_status"
                        and m.action_status.action_id == aid
                        and m.action_status.state == pb.ACTION_STATE_STOPPED
                        for m in self._pushback)
        window = f"{_LEASE_END_WAIT:g}s"
        if upd is not None:
            detail = (f"reported {pb.ActionState.Name(upd.state)} within {window} of lease "
                      f"expiry"
                      + (", after ACTION_STATE_STOPPED (the commanded stop, not counted)"
                         if stop_seen else "")
                      + ". LIMIT: this establishes that the end was REPORTED, not that rest "
                        "was CONFIRMED before it was reported — nothing on ActionStatusUpdate "
                        "attests confirmation, so an announced end and a confirmed one are "
                        "the same trace here")
        elif stop_seen:
            detail = (f"reported only ACTION_STATE_STOPPED within {window} of lease expiry: "
                      f"STOPPED is the commanded stop and not terminal (RFC-0001 safety "
                      f"chapter, *terminal*), and no terminal report followed it, so the "
                      f"control plane cannot tell the action ended")
        else:
            detail = (f"no terminal action_status within {window} of lease expiry; the stop "
                      f"is unobservable and dual execution cannot be ruled out")
        self.r.add("C4.2", "Self-stop when the assignment lease is not renewed",
                   CheckStatus.PASS if upd is not None else CheckStatus.FAIL, detail=detail)

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
        cid = self._send_command(assign_action=self._assign(
            action_id=aid_live, assignment_id="a-held", fencing_token=12,
            needs_seconds=4.0,   # C4.1 renews for ~1 s -> 4 s of travel
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
                       detail=("renewed=True running=True after each of 4 renewals at "
                               "lease_generation 3, 0.2s apart" if held else detail))
        else:
            self.r.add("C4.1", "Reports the task still running across lease renewals",
                       CheckStatus.SKIP,
                       detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)

        # C4.3 — renew_lease reply semantics: a matching generation renews.
        aid2 = "t-renew"
        cid = self._send_command(assign_action=self._assign(
            action_id=aid2, assignment_id="a-renew", fencing_token=13,
            needs_seconds=4.0,   # C4.3/C4.4/C0.3 ~2 s -> 4 s of travel
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
                       detail=("RenewActionLease at lease_generation 1, below the current "
                               "7: renewed=False" if not rs.renewed else
                               "a stale generation was renewed"))
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
                       detail=("after CancelAction, RenewActionLease at the current "
                               "generation 7 reported running=False" if not re_.running else
                               "renew_lease still reported running=true after the action was "
                               "cancelled; a silent completion would be undetectable"))
        else:
            for cid_, name in (("C4.3", "renew_lease renews a matching lease generation"),
                               ("C4.4", "renew_lease rejects a stale lease generation")):
                self.r.add(cid_, name, CheckStatus.SKIP,
                           detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: adapter "
                                  "refused the lease-bearing assignment")

    def _check_action_status(self) -> None:
        # C16.1 — ActionStatusUpdate at a task phase transition. Required, and until now
        # untested: the control plane drives FleetAction Assigned -> InProgress from it.
        aid = "t-status"
        cid = self._send_command(assign_action=self._assign(
            action_id=aid, assignment_id="a-status", fencing_token=14,
            needs_seconds=6.0))  # C16.1 waits 3 s -> 6 s of travel
        if not self._await_result(cid).assign_action.accepted:
            self._record_task_status(transition=None, terminal=self._terminal_status,
                                     accepted=False)
            return
        upd = self._await_action_status(aid, timeout=3.0)
        self._record_task_status(transition=upd, terminal=self._terminal_status)
        # C6.4 — the update MUST echo the token the reported action was assigned with, so
        # the control plane can spot a robot still acting on a superseded assignment. The
        # token this action was assigned with is 14.
        if upd is None:
            self.r.add("C6.4", "Echo the assigned fencing_token in ActionStatusUpdate",
                       CheckStatus.SKIP,
                       detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)
        else:
            echoed = upd.fencing_token == 14
            self.r.add("C6.4", "Echo the assigned fencing_token in ActionStatusUpdate",
                       CheckStatus.PASS if echoed else CheckStatus.FAIL,
                       detail=(f"ActionStatusUpdate fencing_token={upd.fencing_token}, the "
                               f"accepted token" if echoed else
                           f"fencing_token={upd.fencing_token}, want 14 (the accepted token); "
                           f"without it a superseded assignment is undetectable"))

    def _record_task_status(self, *, transition, terminal, accepted: bool = True) -> None:
        """Record C16.1 from BOTH halves of what the catalog claims.

        "An ActionStatusUpdate at every task phase transition, terminal states included."
        The transition half is the acceptance update for the assignment C16 sends; the
        terminal half is the update for the task C3.6 cancelled — a task this harness drove
        to an end, so an adapter that reports nothing has left a rollout resolvable only by
        lease expiry, which is the failure the second clause names.

        Two halves, one row: a check id names exactly one check in a report.
        """
        title = "ActionStatusUpdate at a task phase transition, terminal states included"
        if not accepted:
            self.r.add("C16.1", title, CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": adapter refused the assignment"))
            return
        if transition is None:
            self.r.add("C16.1", title, CheckStatus.FAIL,
                       detail="no action_status observed after an accepted assignment")
            return
        if terminal is None:
            self.r.add("C16.1", title, CheckStatus.FAIL,
                       detail=("the acceptance update arrived (state="
                               + pb.ActionState.Name(transition.state)
                               + ") but the task the harness CANCELLED reported no terminal "
                                 "state; a task that ends without one is resolvable only by "
                                 "lease expiry"))
            return
        self.r.add("C16.1", title, CheckStatus.PASS,
                   detail=(f"transition state={pb.ActionState.Name(transition.state)}; "
                           f"terminal state={pb.ActionState.Name(terminal.state)} for the "
                           f"cancelled task {terminal.action_id!r}"))

    def _check_cancel_names_its_action(self) -> None:
        """C3.9 (MUST) — a cancel naming a preempted action does not stop the one preempting it.

        The control plane preempts in place: it pushes an AssignAction for the new action of
        the same type at a strictly higher token, with no cancel before it, and the robot
        switches (RFC-0001 #fleet-adapter-protocol-directive-supersession-on-the-wire). The
        preempted action's own reconcile then asks for a confirmed stop of IT, by name, with
        cancel_action (`confirmPreemptedStop` -> `pushCancel` in the control plane). That
        cancel is about an action the robot no longer runs. An adapter that ignores
        `action_id` and stops whatever is running aborts the preemptor, which then never
        completes; the fencing rule exists so that a cancel "cannot abort a newer assignment"
        (fleet_adapter.proto, CancelAction.fencing_token).

        Two cancels name the preempted action. The first carries its own token, below the
        mark the preemptor raised: that is what the reference control plane sends, since it
        draws every token from one counter per namespace. The second carries the mark
        itself, as a control plane that counted tokens per action could send, and then
        `action_id` is the only field that says the cancel is not about the running
        action. Without the second, an adapter that checks the token and never reads
        `action_id` would pass here and abort preemptors in the field.

        The row grades the preemptor: still running after both cancels (the adapter's own
        `running`, as C4.1 reads it) and then SUCCEEDED. What each CancelActionResult said is
        recorded in the detail and not graded; CONFORMANCE.md C3.9 says why.
        """
        title = "A cancel naming a preempted action leaves the preemptor running to completion"
        preempted, preemptor = "t-preempted", "t-preemptor"
        for action_id, token, needs in ((preempted, _PREEMPTED_TOKEN, 8.0),
                                        (preemptor, _PREEMPTOR_TOKEN, 4.0)):
            cid = self._send_command(assign_action=self._assign(
                action_id=action_id, assignment_id=f"a-{action_id}", fencing_token=token,
                needs_seconds=needs, lease_generation=1, lease_duration_ms=_PREEMPT_LEASE_MS))
            try:
                res = self._await_result(cid).assign_action
            except (EOFError, queue.Empty):
                res = None
            if res is None or not res.accepted:
                why = ("no CommandResult" if res is None else
                       f"refused it (rejection={res.rejection}, {res.message!r}"
                       + self._unlifted_note(res.rejection) + ")")
                self.r.add("C3.9", title, CheckStatus.SKIP,
                           detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: the adapter "
                                  f"answered the assignment of {action_id!r} at token {token} "
                                  f"with {why}, so there was no in-place preemption to cancel "
                                  f"around")
                return

        # The preemptor must be running when the cancels arrive. One that has already ended
        # proves nothing about them, and a correct adapter would then read as a faulty one.
        before = self._renew(preemptor)
        if before is None or not before.running:
            self.r.add("C3.9", title, CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: before any "
                              f"cancel, renew_lease for {preemptor!r} reported "
                              + ("no result" if before is None else "running=False")
                              + ", so the preemptor was not running to be aborted")
            return

        answers = []
        for token in (_PREEMPTED_TOKEN, _PREEMPTOR_TOKEN):
            cid = self._send_command(cancel_action=pb.CancelAction(
                action_id=preempted, reason="preempted", fencing_token=token,
                cause=pb.CANCEL_CAUSE_SUPERSEDED))
            try:
                ans = self._await_result(cid).cancel_action
                answers.append(f"token {token}: acknowledged={ans.acknowledged} "
                               f"stale={ans.stale} disposition="
                               f"{pb.CancelDisposition.Name(ans.disposition)}")
            except (EOFError, queue.Empty):
                answers.append(f"token {token}: no CancelActionResult")
        said = "; ".join(answers)

        after = self._renew(preemptor)
        end = self._await_action_status(preemptor, timeout=_PREEMPTOR_TERMINAL_WAIT,
                                        states=_TERMINAL_ACTION_STATES())
        running = after is not None and after.running
        sent = (f"assigned {preempted!r} at token {_PREEMPTED_TOKEN}, then {preemptor!r} in "
                f"place at {_PREEMPTOR_TOKEN}, then cancel_action({preempted!r}) at tokens "
                f"{_PREEMPTED_TOKEN} and {_PREEMPTOR_TOKEN}")
        if end is not None and end.state == pb.ACTION_STATE_SUCCEEDED:
            self.r.add("C3.9", title, CheckStatus.PASS,
                       detail=(f"{sent}: {preemptor!r} "
                               + ("still running=True after both cancels"
                                  if running else "had completed by the renewal after them")
                               + f", then SUCCEEDED. Cancel answers (not graded): {said}"))
        elif end is not None:
            self.r.add("C3.9", title, CheckStatus.FAIL,
                       detail=(f"{sent}: {preemptor!r} ended "
                               f"{pb.ActionState.Name(end.state)} ({end.message!r}) and "
                               f"renew_lease reported running={running}. A cancel naming "
                               f"{preempted!r} stopped the action that preempted it. Cancel "
                               f"answers: {said}"))
        else:
            self.r.add("C3.9", title, CheckStatus.FAIL,
                       detail=(f"{sent}: renew_lease for {preemptor!r} reported "
                               f"running={running} after the cancels, and no terminal "
                               f"ActionStatusUpdate for it arrived within "
                               f"{_PREEMPTOR_TERMINAL_WAIT:.0f}s, so it was not seen to "
                               f"complete. Cancel answers: {said}"))

    def _renew(self, action_id: str):
        """renew_lease for a C3.9 action at its lease; the result, or None if none came."""
        cid = self._send_command(renew_lease=pb.RenewActionLease(
            action_id=action_id, lease_generation=1, lease_duration_ms=_PREEMPT_LEASE_MS))
        try:
            return self._await_result(cid).renew_lease
        except (EOFError, queue.Empty):
            return None

    def _check_estop(self) -> None:
        # C5.1 / C5.2 — send Estop on SafetyStream, expect a confirmed EstopAck.
        # C5.5 times that exchange against the 500 ms hard requirement at
        # fleet-adapter-protocol.md's performance table. Measured from the moment the
        # Estop is queued to the moment the ack is dequeued, so the number includes the
        # harness's own transport and is therefore conservative (never flattering).
        # C5.7 needs a robot IN MOTION when the stop is sent: a stop issued to a robot at rest
        # cannot tell an early STOPPED from a timely one. The robot was last given work by C16,
        # whose acceptance can come before the plant has moved at all, so with a witness the
        # harness waits for the witness to show motion first. Without one nothing is waited for.
        #
        # The answer is read by its arm and its stop ID, not as the first SafetyStream message:
        # a 1.4.0 adapter may have a LocalStopReport, or a report of another latched stop,
        # waiting there (RFC-0001 #fleet-adapter-protocol-the-stop-latch, *Report*).
        self._await_witnessed_motion()
        _estop_sent_at = time.monotonic()
        _estop_sent_epoch = time.time()  # the witness samples on the host's epoch clock
        self.s.safety_out.put(pb.ControlPlaneSafetyMessage(
            robot_id=ROBOT,
            estop=pb.Estop(estop_id="e1", reason="conformance", issued_by="harness")))
        # Row S1 of RFC-0001's robot stop table: the control plane sends every stop on both
        # streams at once, and on a 1.4.0 connection the fence is what the clear waits for.
        if self._latch_binds():
            self._fence("e1", "conformance")
        try:
            ack, passed = self._recv_estop_ack("e1")
        except EOFError:
            ack, passed = None, ["the SafetyStream closed"]
        skipped = f"; passed over {passed} to reach it" if passed else ""
        if ack is None:
            none = ("no EstopAck for estop_id 'e1' within "
                    f"{_GET_TIMEOUT:.0f}s" + (f" (received {passed})" if passed else ""))
            self.r.add("C5.1", "Accept Estop on SafetyStream and act on it",
                       CheckStatus.FAIL, detail=none)
            self.r.add("C5.2", "Confirmed EstopAck (STOPPED) on SafetyStream",
                       CheckStatus.FAIL, detail=none)
            self.r.add("C5.5", "EstopAck within the 500 ms SafetyStream budget",
                       CheckStatus.FAIL, detail=f"{none}, so the budget cannot be met")
            self._grade_early_stopped(None, None, _estop_sent_epoch)
        else:
            self._grade_estop_ack(ack, skipped, _estop_sent_at, _estop_sent_epoch)
        self._clear_stop("e1")
        self._check_fence_ack()        # C5.4
        self._clear_stop(_FENCE_PROBE_ID)

    def _grade_estop_ack(self, ack, skipped: str, _estop_sent_at: float,
                         _estop_sent_epoch: float) -> None:
        """C5.1, C5.2, C5.3, C5.5, C5.6 and C5.7 from the first EstopAck naming 'e1'."""
        _first_ack_epoch = time.time()
        _estop_latency_ms = (time.monotonic() - _estop_sent_at) * 1000.0
        _within = _estop_latency_ms <= _ESTOP_ACK_BUDGET_MS
        self.r.add("C5.5", "EstopAck within the 500 ms SafetyStream budget",
                   CheckStatus.PASS if _within else CheckStatus.FAIL,
                   detail=(f"measured {_estop_latency_ms:.0f} ms to the first EstopAck naming "
                           f"'e1' (budget {_ESTOP_ACK_BUDGET_MS:.0f} ms, harness transport "
                           f"included){skipped}"))
        # C5.1 — the estop was accepted on SafetyStream and acted on: an ack for the
        # estop_id we sent came back on the same stream.
        self.r.add("C5.1", "Accept Estop on SafetyStream and act on it", CheckStatus.PASS,
                   detail=f"EstopAck for estop_id 'e1' returned on SafetyStream{skipped}")
        # C5.2 governs the TERMINAL ack. The FIRST one may legitimately be
        # ESTOP_STATE_STOPPING ("received and braking; rest not yet confirmed") -- C5.5
        # bounds that one, this bounds the confirmation that follows it.
        if ack.state == pb.ESTOP_STATE_STOPPING:
            term, saw_stopping, waited = self._recv_terminal_estop_ack("e1")
            saw_stopping = True
            _terminal_epoch = time.time()
        else:
            term, saw_stopping, waited = ack, False, 0.0
            _terminal_epoch = _first_ack_epoch
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
                       CheckStatus.SKIP,
                       detail=(SkipCause.LEGAL_ALTERNATIVE_PATH.value
                               + ": the adapter answered with a terminal ack directly and "
                                 "never sent STOPPING, which is equally conformant. C5.6 "
                                 "grades the two-phase shape; this adapter took the "
                                 "one-phase one, so the check was never an obligation of "
                                 "it (ADR-0048 rule B). did-not-reach-state would be "
                                 "false here -- a terminal state WAS reached"))
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
                   CheckStatus.PASS if attested else CheckStatus.FAIL, level="MUST NOT",
                   detail=("attested via stop_initiated_at (necessary evidence, not "
                           "proof that no inference occurred)") if attested else
                          ("EstopAck reported STOPPED with no stop_initiated_at, so "
                           "nothing attests a hardware stop was actually commanded"))
        self._grade_early_stopped(term, _terminal_epoch, _estop_sent_epoch)

    def _check_fence_ack(self) -> None:
        """C5.4 (MUST NOT) — a stop's acknowledgement is not carried on ControlStream in place
        of SafetyStream.

        The probe is a stop sent as its fence alone: Command.estop on ControlStream, for a stop
        ID no Estop carried. An active probe, because sending nothing there and seeing nothing
        come back is a tautology. On a 1.4.0 connection the fence is a stop, answered with an
        EstopAck for its ID on SafetyStream, and the adapter MAY also answer it with
        CommandResult.estop: "That answer is a receipt, and rows S2 to S4 never read it"
        (RFC-0001 #fleet-adapter-protocol-the-stop-latch). A 1.3.x connection may act on the
        fence or ignore it. So the row fails one shape: a receipt on ControlStream and no
        EstopAck for the ID on SafetyStream. It used to fail any CommandResult.estop, which
        failed an adapter for the receipt the specification permits.
        """
        title = "Estop traffic not carried on ControlStream"
        cid = self._fence(_FENCE_PROBE_ID, "conformance: a stop sent as its fence alone")
        receipt = acked = closed = False
        held = []
        # One second for an adapter to answer on either stream; a receipt with no
        # SafetyStream ack yet is given the full wait for the ack before it fails.
        window = 1.0
        start = time.monotonic()
        while time.monotonic() - start < window and not (receipt and acked):
            if not closed:
                try:
                    m = self.s.control_in.get(timeout=0.05)
                except queue.Empty:
                    m = None
                if m is _SENTINEL:
                    closed = True
                    held.append(m)
                elif m is not None:
                    if (m.WhichOneof("payload") == "command_result"
                            and m.command_result.command_id == cid
                            and m.command_result.WhichOneof("result") == "estop"):
                        receipt = True
                    else:
                        held.append(m)
            else:
                time.sleep(0.05)
            s = None
            if not acked:  # once acked, what follows on SafetyStream is not this probe's
                try:
                    s = self.s.safety_in.get_nowait()
                except queue.Empty:
                    pass
            if (s is not None and s is not _SENTINEL and s.WhichOneof("payload") == "estop_ack"
                    and s.estop_ack.estop_id == _FENCE_PROBE_ID):
                acked = True
            if receipt and not acked:
                window = _GET_TIMEOUT
        self._pushback.extend(held)
        if receipt and not acked:
            self.r.add("C5.4", title, CheckStatus.FAIL, level="MUST NOT",
                       detail=(f"a fence (Command.estop for {_FENCE_PROBE_ID!r}) was answered "
                               f"with CommandResult.estop on ControlStream, and no EstopAck "
                               f"for that stop came on SafetyStream within "
                               f"{_GET_TIMEOUT:.0f}s: the stop's acknowledgement travelled on "
                               f"ControlStream in place of SafetyStream"))
            return
        if acked:
            detail = (f"a fence (Command.estop for {_FENCE_PROBE_ID!r}) was acknowledged with "
                      f"an EstopAck on SafetyStream"
                      + (", and with a CommandResult.estop receipt on ControlStream, which "
                         "RFC-0001 permits (The Stop Latch: a receipt rows S2 to S4 never read)"
                         if receipt else ""))
        elif self._latch_binds():
            detail = (f"a fence (Command.estop for {_FENCE_PROBE_ID!r}) drew no "
                      f"acknowledgement on ControlStream. It drew no EstopAck on SafetyStream "
                      f"either, which RFC-0001 The Stop Latch requires of a 1.4.0 adapter and "
                      f"no C-row grades (CONFORMANCE.md, Adapter obligations with no C-row)")
        else:
            detail = (f"a fence (Command.estop for {_FENCE_PROBE_ID!r}) drew no "
                      f"acknowledgement on ControlStream; a connection below 1.4.0 may act on "
                      f"a fence or ignore it")
        self.r.add("C5.4", title, CheckStatus.PASS, level="MUST NOT", detail=detail)

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

    def _await_witnessed_motion(self, timeout: float = _WITNESS_MOTION_WAIT) -> bool:
        """Wait until the motion witness shows the robot moving. False when it never does."""
        if self._witness_path is None or self._served is None:
            return False
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            meta, samples = _read_witness(self._witness_path)
            robot = self._registered_robot
            mine = [x for x in samples if not robot or x.get("robot") in (robot, None)]
            if meta is not None and mine and mine[-1]["speed"] > float(
                    meta.get("rest_noise_mps", 0.0)):
                return True
            time.sleep(0.1)
        return False

    def _grade_early_stopped(self, term, received_epoch, sent_epoch: float) -> None:
        """C5.7 (MUST NOT) — a STOPPED is not sent before the witnessed moment of rest.

        C5.3 concedes that whether an adapter inferred rest is not decidable from the wire.
        It is decidable against a witness: something other than the adapter's own confirmation
        path that records the robot's actual speed. On hardware that is an external motion
        reference; in a simulated run it is the simulator's plant model, which the harness can
        be handed with `--witness-file`. With neither, the row is `not-testable-here`.

        The witnessed moment of rest is the start of the first interval of at least the
        witness's stated hold time, after the estop, in which every sample is within its stated
        rest noise. The time of STOPPED is the ack's `confirmed_at_ms`, or, when the adapter
        set none, the harness's receipt time, which makes the comparison lenient by the
        transport delay and says so.

        The witness places rest at its first sample within the noise; the robot came to rest
        somewhere after the sample before it, which still showed motion. A sparse witness can
        therefore place rest up to one sample gap late. A STOPPED earlier than the witnessed
        rest by more than the larger of the witness's declared resolution and that gap fails,
        so a STOPPED fails only when it precedes a sample that still showed the robot moving.
        """
        title = "An early STOPPED fails: STOPPED is not sent before the witnessed moment of rest"
        level = "MUST NOT"
        if self._witness_path is None:
            self.r.add("C5.7", title, CheckStatus.SKIP, level=level,
                       detail=(SkipCause.NO_MOTION_WITNESS.value
                               + ": the run was given no motion witness (--witness-file), so the "
                                 "time of STOPPED could not be compared with the robot's rest"))
            return
        if term is None or term.state != pb.ESTOP_STATE_STOPPED:
            self.r.add("C5.7", title, CheckStatus.SKIP, level=level,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": no terminal STOPPED was received, so there is no STOPPED "
                                 "to time against the witness"))
            return
        meta, samples = _read_witness(self._witness_path)
        robot = self._registered_robot
        samples = sorted((s for s in samples if not robot or s.get("robot") in (robot, None)),
                         key=lambda s: s["t"])
        if meta is None or not samples:
            self.r.add("C5.7", title, CheckStatus.SKIP, level=level,
                       detail=(SkipCause.NO_MOTION_WITNESS.value
                               + f": the witness file {self._witness_path} held no samples"
                                 + (f" for {robot!r}" if robot else "")))
            return
        noise = float(meta.get("rest_noise_mps", 0.0))
        hold = float(meta.get("hold_s", 0.0))
        resolution = float(meta.get("resolution_s", 0.0))
        source = meta.get("witness", "unnamed witness")
        before = [s for s in samples if s["t"] <= sent_epoch]
        speed_at_estop = before[-1]["speed"] if before else None
        if speed_at_estop is None or speed_at_estop <= noise:
            speed = "unknown" if speed_at_estop is None else f"{speed_at_estop:.3f}"
            agreed = _negotiate(self._reported_contract)
            held = (f"; the connection negotiated contract {agreed}, which is sent no "
                    f"AssignAction, so nothing put the robot in motion"
                    if self._held_below_latch() else "")
            self.r.add("C5.7", title, CheckStatus.SKIP, level=level,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + f": the witness ({source}) shows the robot at rest when the "
                                 f"estop was sent (speed {speed} m/s), so an early STOPPED "
                                 f"cannot be told from a timely one{held}"))
            return
        # The witness is still being written: a rest that has begun is confirmed only by a
        # sample later than its hold time, which may not exist yet when STOPPED arrives. Read
        # it again until the rest is confirmed or the wait runs out; deciding on a partial
        # record would fail an adapter whose STOPPED was on time.
        deadline = time.monotonic() + max(3.0, hold + 2 * resolution + 1.5)
        while True:
            rest_at = _witnessed_rest(samples, sent_epoch, noise, hold)
            if rest_at is not None or time.monotonic() >= deadline:
                break
            time.sleep(0.1)
            _meta, fresh = _read_witness(self._witness_path)
            samples = sorted((s for s in fresh if not robot or s.get("robot") in (robot, None)),
                             key=lambda s: s["t"])
        if term.confirmed_at_ms:
            stopped_at, basis = term.confirmed_at_ms / 1000.0, "confirmed_at_ms"
        else:
            stopped_at = received_epoch
            basis = ("the harness's receipt time (no confirmed_at_ms; lenient by the "
                     "transport delay)")
        bounds = (f"witness: {source}; rest noise {noise} m/s, hold {hold} s, resolution "
                  f"{resolution} s; STOPPED timed by {basis}")
        if rest_at is None:
            self.r.add("C5.7", title, CheckStatus.FAIL, level=level,
                       detail=f"STOPPED at +{stopped_at - sent_epoch:.3f}s after the estop, and "
                              f"the witness shows no rest in the samples that follow it "
                              f"(speed at the estop {speed_at_estop:.3f} m/s). {bounds}")
            return
        prior = [s["t"] for s in samples if s["t"] < rest_at]
        gap = rest_at - prior[-1] if prior else 0.0
        tolerance = max(resolution, gap)
        bounds += (f"; sample gap at the rest {gap:.3f} s, so the tolerance is "
                   f"{tolerance:.3f} s")
        early = rest_at - stopped_at
        if early > tolerance:
            self.r.add("C5.7", title, CheckStatus.FAIL, level=level,
                       detail=f"STOPPED at +{stopped_at - sent_epoch:.3f}s, the witnessed rest at "
                              f"+{rest_at - sent_epoch:.3f}s: {early:.3f}s EARLY, beyond the "
                              f"witness's resolution and its sample gap. The adapter reported "
                              f"rest the robot had not reached. {bounds}")
            return
        self.r.add("C5.7", title, CheckStatus.PASS, level=level,
                   detail=f"robot moving at {speed_at_estop:.3f} m/s when the estop was sent; "
                          f"witnessed rest at +{rest_at - sent_epoch:.3f}s, STOPPED at "
                          f"+{stopped_at - sent_epoch:.3f}s. {bounds}")

    def _check_telemetry(self) -> None:
        payload = self._await_telemetry()
        if payload is None:
            # A template that registers no robot streams nothing. Enumerated cause, so a
            # later contradiction check can catch this being claimed falsely.
            for cid, title, level in (
                    ("C6.3", "No Robot status write per telemetry tick (RA-1)", "MUST NOT"),
                    ("C6.2", "Preserve proto3 explicit presence on safety scalars", "MUST")):
                self.r.add(cid, title, CheckStatus.SKIP, level=level,
                           detail=SkipCause.NO_TELEMETRY_IN_RUN.value)
            return
        self.telemetry_seen = True
        self._hardware_baseline = len(payload.hardware)

        # C6.2 — explicit presence. `floor` is declared `optional int32` precisely because
        # 0 (ground) is a VALID value that must be distinguishable from "not reported": a
        # dropped field deserialising to 0 would silently claim the robot is on the ground.
        # Assert presence on the zero-valued scalar, which is the only case that proves it.
        # An aerial adapter reports `altitude` as well; `floor` is not frame-exclusive with
        # it (RFC-0001 FZ-Z1 compares floor in every frame), but this row grades presence,
        # not which scalars a frame carries. Prove presence on whichever of the two the
        # adapter actually reports; that floor is reported at all is not graded here.
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
        # recorded SKIP with an enumerated cause naming where it IS graded. It was a
        # bare PASS, which reported a MUST NOT as verified when nothing had tested it —
        # the single most misleading row a conformance report can carry. The obligation is
        # graded on the ControlPlane surface as CP8.1 (conformance/CONTROL-PLANE.md) and is
        # excluded from every FleetAdapter profile, so it no longer holds an adapter's
        # verdict for a property of the plane. The row stays because the catalog still
        # lists the check on this surface.
        self.r.add("C6.3", "No Robot status write per telemetry tick (RA-1)",
                   CheckStatus.SKIP, level="MUST NOT",
                   detail=(SkipCause.NOT_OBSERVABLE_ON_WIRE.value
                           + ": RA-1 governs the control plane's write policy, not adapter "
                             "output, and is graded on the ControlPlane surface as CP8.1. "
                             "Telemetry was observed, so the projection path is live."))

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
                       CheckStatus.SKIP,
                       detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: control stream "
                              "closed before registration")
            return False

        kind = msg.WhichOneof("payload")
        edge = pb.EdgeEndpoint(zone="zone-conformance", address=f"localhost:{self.port}")
        if kind == "register":
            self._registered_robot = msg.register.robot_id
            self.s.control_out.put(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                accepted=True, telemetry_interval_seconds=1, edge_endpoints=[edge])))  # 1s: the
                # suite now MEASURES adoption of this value (C2.3), and a 10s cadence
                # would starve every downstream check that waits on telemetry.
            self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                       CheckStatus.PASS,
                       detail="RegisterRobot for an admitted robot is acked")
            return True
        if kind == "discover":
            # fleet-adapter-protocol.md "Required messages": RegisterRobot MUST be the
            # FIRST attempt for every managed robot on connect and reconnect, and
            # DiscoverRobot MUST NOT be attempted before it — a NOT_ADMITTED rejection is
            # what routes an adapter into the discovery loop. This harness plays a control
            # plane that HOLDS a Robot CRD for this id: it acks a register, advertises edge
            # endpoints, and dispatches work for the rest of the run. A discover for it is
            # the reverse ordering the catalog forbids.
            #
            # Answered in band with ALREADY_EXISTS, the enum a real control plane returns
            # here, rather than by closing the stream: the in-band error model reserves
            # gRPC status codes for connection-level faults, and an adapter that corrects
            # itself deserves to be told what was wrong.
            self.s.control_out.put(pb.ControlPlaneMessage(discover_ack=pb.DiscoverAck(
                accepted=False, rejection=pb.REGISTRATION_REJECTION_ALREADY_EXISTS,
                message="a Robot already exists for this id; send register")))
            self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                       CheckStatus.FAIL,
                       detail="the first lifecycle message for an ADMITTED robot was "
                              "discover, not register; DiscoverRobot MUST NOT be attempted "
                              "before register, and the control plane answered "
                              "ALREADY_EXISTS")
            return False
        # Not a registration — return it to the stream for the fencing checks.
        self._pushback.append(msg)
        self.r.add("C2.1", "DiscoverRobot vs RegisterRobot used correctly",
                   CheckStatus.SKIP,
                   detail=f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: adapter initiated "
                          "no registration")
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
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": the adapter sent no CommandResult at all. That is NOT a "
                                 "decline — a decline is unsupported=true, which the adapter "
                                 "did not send — so this records what happened: the check "
                                 "never reached the state it needs (see C0.1)"))
            return
        declined = res.unsupported
        answered = res.WhichOneof("result") == "verify"
        ok = declined or answered
        self.r.add("C7.1", "Decline optional commands via unsupported=true",
                   CheckStatus.PASS if ok else CheckStatus.FAIL, level="MAY",
                   detail=(("verify_hardware declined with unsupported=true" if declined else
                            "verify_hardware answered with a VerifyResult") if ok else
                       "optional command was neither declined (unsupported=true) nor "
                       "answered with a VerifyResult"))

    def _check_verify_metrics(self) -> None:
        # C9.1 — an adapter that ANSWERS a verify_* probe SHOULD return actualMetrics
        # (RFC-0001 #robotprobe-the-verify-probe-commands), so
        # RobotProbe.status.robotResults[].actualMetrics is populated.
        # MAY-level: declining verify_* is conformant (C7), so a decline is a SKIP.
        cid = self._send_command(verify_hardware=pb.VerifyHardware(
            component_name="lidar-360", expected_metrics={"scanHz": 10.0}))
        try:
            res = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C9.1", "verify_* answers with actualMetrics", CheckStatus.SKIP,
                       level="SHOULD", detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": the adapter sent no CommandResult at all. That is NOT a "
                                 "decline — a decline is unsupported=true, which the adapter "
                                 "did not send — so this records what happened: the check "
                                 "never reached the state it needs (see C0.1)"))
            return
        if res.unsupported or res.WhichOneof("result") != "verify":
            self.r.add("C9.1", "verify_* answers with actualMetrics", CheckStatus.SKIP,
                       level="SHOULD",
                       detail=f"{SkipCause.OPTIONAL_DECLINED.value}: adapter declined verify_* "
                              "(conformant; see C7)")
            return
        ok = len(res.verify.actual_metrics) > 0
        self.r.add("C9.1", "verify_* answers with actualMetrics",
                   CheckStatus.PASS if ok else CheckStatus.FAIL, level="SHOULD",
                   detail=(f"VerifyResult actualMetrics={dict(res.verify.actual_metrics)}"
                           if ok else "VerifyResult answered but carried no actualMetrics"))

    def _check_update_progress(self) -> None:
        """C10.1 (MAY) and C10.2 (MUST, when implemented) — advisory progress, and the
        terminal outcome for EVERY install the adapter answers.

        Both installs are probed. The catalog says "the TERMINAL result of every install
        the adapter answers" and the protocol row says "for every install it accepts";
        sending only ``push_firmware`` let an adapter hold half a published MUST by never
        being asked about ``model_update``.
        """
        firmware = self._probe_install(
            "push_firmware",
            push_firmware=pb.PushFirmware(target_version="0.0.0-conformance"))
        model = self._probe_install(
            "model_update",
            # model_uri is REQUIRED on this probe. An Auto rollback (§6.7) is the one
            # ModelUpdate that legitimately carries no artifact, and it is signalled by an
            # empty model_uri, so a probe without one asks about the rollback branch rather
            # than about an ordinary install. Carrying no checksum makes a C7.2-compliant
            # adapter refuse — and a refusal is terminal, which is the obligation under test.
            model_update=pb.ModelUpdate(model_name="conformance-progress-probe",
                                        new_version="0.0.0-conformance",
                                        model_uri="oci://example.invalid/"
                                                  "conformance-progress-probe:0.0.0"))
        self._record_update_progress(firmware)
        self._record_terminal_outcome(firmware, model)

    def _probe_install(self, arm: str, **command_kwargs) -> dict:
        """Send one install command and collect the UpdateProgress it emits first.

        Returns ``{arm, answered, accepted, progress, message}``. ``answered`` is False for
        a decline (``unsupported=true``) or no result at all, which carries no obligation
        under C10.2 — declining an optional command is conformant (C7.1).
        """
        cid = self._send_command(**command_kwargs)
        progress, res = self._await_result_collecting(cid, "update_progress")
        if res is None or res.unsupported:
            # A decline (unsupported=true) and silence are DIFFERENT facts, and only the
            # first is a decline. Carried as separate causes so a reader — and C0.1 — can
            # tell an optional feature the adapter does not offer from one it never
            # answered at all.
            declined = res is not None
            return {"arm": arm, "answered": False, "accepted": False, "declined": declined,
                    "progress": progress, "message": "",
                    "cause": (SkipCause.OPTIONAL_DECLINED if declined
                              else SkipCause.ADAPTER_DID_NOT_REACH_STATE),
                    "why": ("adapter declined it with unsupported=true (conformant; see C7)"
                            if declined else
                            "the adapter sent no CommandResult at all, which is not a "
                            "decline")}
        inner = res.push_firmware if arm == "push_firmware" else res.model_update
        accepted = inner.accepted if arm == "push_firmware" else inner.acknowledged
        return {"arm": arm, "answered": True, "accepted": bool(accepted), "declined": False,
                "progress": progress, "message": inner.message, "cause": None, "why": ""}

    def _record_update_progress(self, firmware: dict) -> None:
        """C10.1 — an adapter that ACCEPTS push_firmware MAY emit advisory UpdateProgress
        before the FirmwareResult (RFC-0001 #fleet-adapter-protocol-the-controlstream-service),
        feeding
        ``FirmwareRollout.status.currentBatch[].updatePhase``. Fully optional: a decline
        (C7) or an accept with no progress are both conformant, so this only PASSES when
        progress is emitted and never FAILS a conformant adapter.

        Not a PASS when none is emitted, although that is conformant. The profile layer
        reads a pass on this row as `provisioning.update-progress` SUPPORTED, and an
        adapter that sends no progress does not support it. The row skips with
        `optional-not-exhibited`, which says what happened: the path was reached and the
        MAY was not taken. The terminal report is a different obligation, and C10.2 still
        holds the same adapter to it.
        """
        title = "push_firmware may emit UpdateProgress"
        emitted = [p for p in firmware["progress"] if p.robot_id == ROBOT and p.phase]
        if emitted:
            self.r.add("C10.1", title, CheckStatus.PASS, level="MAY",
                       detail=f"phases: {[p.phase for p in emitted]}")
            return
        if not firmware["answered"]:
            self.r.add("C10.1", title, CheckStatus.SKIP, level="MAY",
                       detail=f"{firmware['cause'].value}: {firmware['why']}")
            return
        if not firmware["accepted"]:
            # The probe carries no checksum, so a C7.3-compliant adapter REFUSES it and
            # never reaches the progress path. That is correct behaviour, and the harness
            # cannot supply a valid checksum here because only the adapter knows what bytes
            # it would receive.
            #
            # NOT `optional-declined`. Nothing was declined: the adapter ANSWERED the
            # command and refused the artifact, and the progress path was unreachable
            # because of that earlier refusal. Recording a decline here made the cause mean
            # two opposite things — a feature the implementation does not offer, and a
            # feature it was never asked for — and a cause that cannot be read literally
            # cannot be trusted with a verdict (ADR-0048).
            self.r.add("C10.1", title, CheckStatus.SKIP, level="MAY",
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": adapter ANSWERED push_firmware and refused the "
                                 "unverifiable probe artifact (C7.3 body re-verify), so the "
                                 "optional progress path was never reached. This is not a "
                                 "decline: the feature was not offered-and-refused, it was "
                                 "not exercised. Refusal message: " + firmware["message"]))
            return
        # NOT `optional-declined`. The adapter ACCEPTED the install, so nothing was declined,
        # and C0.1 rightly contradicts a decline cause the run cannot corroborate. Recording
        # one here failed that MUST against an adapter doing exactly what CONFORMANCE.md
        # C10.1 permits ("Progress is advisory and emitting none is conformant").
        self.r.add("C10.1", title, CheckStatus.SKIP, level="MAY",
                   detail=(SkipCause.OPTIONAL_NOT_EXHIBITED.value
                           + ": adapter accepted push_firmware and emitted no advisory "
                             "UpdateProgress before its result, which the MAY permits; the "
                             "terminal report is graded separately under C10.2"))

    def _record_terminal_outcome(self, *installs) -> None:
        """C10.2 (MUST, when implemented) — a terminal outcome per install ANSWERED.

        Advisory phases are optional; the terminal report is not (ADR-0033). The obligation
        is conditional on the capability, so an install the adapter declined carries none
        and an adapter that declines both records a skip.

        What is NOT excused: a REFUSAL is still terminal. The probes carry no checksum, so
        a C7.2/C7.3-compliant adapter refuses them — and that refusal must be reported as
        ``INSTALL_OUTCOME_FAILED``, because a rollout that only sees ``accepted=false`` in
        the command result is still waiting for an install that will never happen. This is
        the check that catches an adapter which refuses correctly but leaves the rollout
        wedged.
        """
        title = "Every install answered reports a terminal outcome"
        answered = [i for i in installs if i["answered"]]
        if not answered:
            all_declined = all(i["declined"] for i in installs)
            cause = (SkipCause.OPTIONAL_DECLINED if all_declined
                     else SkipCause.ADAPTER_DID_NOT_REACH_STATE)
            self.r.add("C10.2", title, CheckStatus.SKIP, level="MUST",
                       detail=(cause.value
                               + ": adapter answered neither push_firmware nor model_update"
                               + (" — both were declined with unsupported=true, which is "
                                  "conformant (C7.1), so no install carried a terminal "
                                  "obligation" if all_declined else
                                  "; at least one drew no CommandResult at all, which is not "
                                  "a decline and leaves the obligation unestablished")))
            return

        silent, reported = [], []
        for inst in answered:
            terminal = [p for p in inst["progress"]
                        if p.robot_id == ROBOT
                        and p.outcome != pb.INSTALL_OUTCOME_UNSPECIFIED]
            if not terminal:
                silent.append(inst["arm"])
                continue
            t = terminal[0]
            if t.outcome == pb.INSTALL_OUTCOME_SUCCEEDED and not t.resulting_version:
                silent.append(inst["arm"] + " (SUCCEEDED without a resulting_version)")
                continue
            name = ("SUCCEEDED" if t.outcome == pb.INSTALL_OUTCOME_SUCCEEDED else "FAILED")
            reported.append(f"{inst['arm']}={name}"
                            + (f"/{t.resulting_version}" if t.resulting_version else ""))

        if silent:
            self.r.add("C10.2", title, CheckStatus.FAIL, level="MUST", detail=(
                f"answered {', '.join(i['arm'] for i in answered)} but reported no usable "
                f"terminal outcome for {', '.join(silent)}. The CommandResult only "
                f"acknowledges that a download began, so the rollout cannot tell a slow "
                f"install from a finished one and will not complete; a SUCCEEDED without a "
                f"resulting_version leaves the control plane unable to record what the robot "
                f"is actually running."
                + (f" Reported: {', '.join(reported)}." if reported else "")))
            return
        self.r.add("C10.2", title, CheckStatus.PASS, level="MUST",
                   detail="terminal outcomes: " + ", ".join(reported))

    def _check_pause_resume(self) -> None:
        # C11.1 (MUST) — pause and resume are REQUIRED: they are in RFC-0001's
        # Required-message table and absent from its optional-command list, and
        # ZoneMaintenance drains a zone through them. The catalog says so in terms:
        # "Declining is NOT conformant". A decline or a silence is therefore a failed MUST,
        # graded exactly as C12.1 grades a declined scan. It was an `optional-declined` skip
        # at MAY, so an adapter that declined pause passed the harness verdict while a
        # maintenance window over its robots silently did nothing.
        title = "pause/resume answered with a result"
        cid = self._send_command(pause=pb.PauseCapabilities(
            reason="conformance", require_stop_before_ack=False))
        try:
            res = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C11.1", title, CheckStatus.FAIL,
                       detail="no CommandResult for pause; pause/resume is a required command "
                              "(RFC-0001 Required-message table)")
            return
        if res.unsupported or res.WhichOneof("result") != "pause":
            self.r.add("C11.1", title, CheckStatus.FAIL,
                       detail=("adapter declined pause with unsupported=true"
                               if res.unsupported else
                               f"pause answered with {res.WhichOneof('result')!r}, not a "
                               f"PauseResult")
                              + "; pause/resume is a required command (RFC-0001 "
                                "Required-message table), so declining is not conformant")
            return
        cid2 = self._send_command(resume=pb.ResumeCapabilities(reason="conformance"))
        try:
            res2 = self._await_result(cid2)
        except (EOFError, queue.Empty):
            res2 = None
        resume_ok = (res2 is not None and not res2.unsupported
                     and res2.WhichOneof("result") == "resume")
        self.r.add("C11.1", title,
                   CheckStatus.PASS if resume_ok else CheckStatus.FAIL,
                   detail=("pause answered with a PauseResult, resume with a ResumeResult"
                           if resume_ok else
                           "pause was answered but resume was not; resume is a required "
                           "command"))

    def _check_scan(self) -> None:
        # C12.1 (MUST) — scan is a required command (RFC-0001 Required-message table): an
        # unanswered or declined scan is a failed MUST, which is what the code below does.
        cid = self._send_command(scan=pb.ScanCapabilities())
        try:
            res = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C12.1", "scan answers with a CapabilitiesSnapshot", CheckStatus.FAIL,
                       detail="no CommandResult for scan; scan is a required command")
            return
        answered = not res.unsupported and res.WhichOneof("result") == "scan"
        if answered:
            # The declared catalog. Every acceptance probe carries one of these types: Navigate
            # when it is declared (the fixture drives it somewhere), otherwise the first one.
            self._catalog = list(res.scan.supported_actions)
            navigate = [a for a in self._catalog if a.action_type == "Navigate"]
            self._served = (navigate or self._catalog or [None])[0]
            # Kept for C6.1: the full inventory, by NAME, from an INDEPENDENT path. The
            # snapshot is full by definition -- the adapter builds the registration push and
            # this reply from one builder -- so C6.1 becomes an agreement invariant between
            # two paths rather than a self-comparison.
            self._scan_components = {c.name for c in res.scan.hardware}
            # FA-R1's declaration, read by C17. A list and not a set: the ORDER an adapter
            # declared them in is what C17.3 quotes back when one is malformed, and a set
            # would report the offender at an index the author cannot find.
            self._honoured_subjects = list(res.scan.honoured_operating_constraints)
            # The sensor-stream declaration, read by C18. A list, not a set: C18.1
            # grades the SHAPE of each entry and has to quote the offender back at the
            # index its author can find.
            self._sensor_streams = list(res.scan.sensor_streams)
        self.r.add("C12.1", "scan answers with a CapabilitiesSnapshot",
                   CheckStatus.PASS if answered else CheckStatus.FAIL,
                   detail=(f"CapabilitiesSnapshot returned, declaring "
                           f"{len(self._honoured_subjects or [])} honoured operating subject(s) "
                           f"and {len(self._sensor_streams or [])} sensor stream(s)"
                           if answered else
                           "adapter declined scan; scan is a required command "
                           "(RFC-0001 Required-message table)"))

    # ── C17 — zone operating constraints (ADR-0058 Part D) ────────────────────
    #
    # WHAT THESE ROWS DO AND DO NOT ESTABLISH. Declare, honour and attest are CLAIMS: the
    # control plane records what an adapter says and cannot verify any of it -- nothing in a
    # control plane can switch a camera off, and a check implying otherwise would launder an
    # unverifiable promise into a green line. DECLINE is different, and it is the only one of
    # the four these rows grade: it is an observable message on a wire. C17.1 and C17.2
    # dispatch a rule the adapter cannot honour and require a refusal that NAMES the subject.
    #
    # These are LIVE, not static. The adapter under test is a running process this harness
    # dials into, and every row below is graded on a message it actually sent.

    def _operating_rule(self, subject: str):
        """One resolved effective operating rule, in the wire form FA-R2 carries.

        RESOLVED, NEVER RAW: `declaring_zone` is an ANCESTOR of the target, because an
        effective rule is the most restrictive anywhere in the chain (FZ-R1) and a refusal
        has to name the object an operator must actually edit. The fixture covers all three
        modes -- DENIED, LIMITED with `max`, LIMITED with `allowed`, and REQUIRED -- so an
        adapter that only looks at one of them is not passing on a fixture that only sends
        one.
        """
        shape = {
            "camera":     (pb.OPERATING_CONSTRAINT_MODE_DENIED, "", (), "patient privacy"),
            "microphone": (pb.OPERATING_CONSTRAINT_MODE_DENIED, "", (), "patient privacy"),
            "water":      (pb.OPERATING_CONSTRAINT_MODE_DENIED, "", (),
                           "carpet that must not get wet"),
            "chemical":   (pb.OPERATING_CONSTRAINT_MODE_LIMITED, "", ("quat-ammonium-7",),
                           "food preparation surfaces"),
            "uv":         (pb.OPERATING_CONSTRAINT_MODE_REQUIRED, "", (),
                           "terminal clean; the lamp must be running"),
            "speed":      (pb.OPERATING_CONSTRAINT_MODE_LIMITED, "0.4", (),
                           "occupied ward at night"),
            "noise":      (pb.OPERATING_CONSTRAINT_MODE_LIMITED, "65", (),
                           "occupied ward at night"),
        }.get(subject, (pb.OPERATING_CONSTRAINT_MODE_DENIED, "", (),
                        "no powered garden equipment indoors"))
        mode, cap, allowed, reason = shape
        return pb.EffectiveOperatingConstraint(
            subject=subject, mode=mode, max=cap, allowed=list(allowed),
            reason=reason, declaring_zone=_DECLARING_ZONE)

    def _probe_operating_constraints(self, subjects, token: int, action_id: str):
        """Dispatch one AssignAction carrying `subjects`, and grade the decline.

        Returns ``(verdict, detail)`` where verdict is a CheckStatus. A wrongly ACCEPTED
        assignment is cancelled before returning: the adapter has just started a task the
        run never intended, and leaving it running would let one failure here corrupt the
        reconnect checks that come after it.
        """
        if self._served is None:
            return CheckStatus.SKIP, (
                SkipCause.ADAPTER_DECLARES_NO_ACTION.value
                + ": no declared action type to carry the operating rule on")
        cid = self._send_command(assign_action=self._assign(
            action_id=action_id, assignment_id=f"a-{action_id}",
            fencing_token=token, lease_duration_ms=0,
            operating_constraints=[self._operating_rule(s) for s in subjects]))
        named = ", ".join(subjects)
        try:
            result = self._await_result(cid)
        except (EOFError, queue.Empty):
            return CheckStatus.SKIP, (
                SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                + f": no CommandResult for the assignment carrying [{named}]")
        if result.unsupported:
            return CheckStatus.FAIL, (
                f"the adapter answered the assignment carrying [{named}] with "
                f"unsupported=true. assign_action is not an optional command (C7 governs "
                f"those), and an adapter that cannot honour a rule declines it under "
                f"FA-R5 rather than declining the whole message")
        res = result.assign_action
        if res.accepted:
            self._send_command(cancel_action=pb.CancelAction(
                action_id=action_id, reason="harness: C17 cleanup after a wrongly accepted "
                                            "assignment", fencing_token=token))
            return CheckStatus.FAIL, (
                f"the adapter ACCEPTED an assignment carrying operating constraint(s) "
                f"[{named}] that it never declared it honours. Accepting and ignoring a "
                f"rule is the one outcome FA-R5 forbids: the control plane has now "
                f"authorised an action it cannot police, and has no idea whether the "
                f"subject was running")
        if res.rejection != pb.ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET:
            return CheckStatus.FAIL, (
                f"the assignment carrying [{named}] was declined, but with "
                f"rejection={res.rejection} rather than "
                f"ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET. A decline under any "
                f"other reason tells the control plane the wrong thing: the five refusal "
                f"shapes are what separate 'cannot honour' from 'busy' and from "
                f"'unparseable', and only this one names the rule the robot cannot honour"
                + self._unlifted_note(res.rejection))
        missing = [s for s in subjects if s not in set(res.unhonourable_subjects)]
        if missing:
            return CheckStatus.FAIL, (
                f"the assignment carrying [{named}] was declined "
                f"OPERATING_CONSTRAINT_UNMET, but unhonourable_subjects="
                f"{list(res.unhonourable_subjects)} does not name {missing}. A decline "
                f"that does not name the subject is not testable and does not satisfy "
                f"FA-R5 -- naming it is the whole of what makes this obligation an "
                f"observable behaviour rather than a claim")
        return CheckStatus.PASS, (
            f"declined OPERATING_CONSTRAINT_UNMET naming "
            f"{list(res.unhonourable_subjects)}")

    def _check_zone_restrictions(self) -> None:
        declared = self._honoured_subjects
        # Published on the report itself, not only inside a check's prose. The profile layer
        # needs the DECLARATION to decide whether the `zone-restrictions` profile applied,
        # and parsing that back out of a detail string would couple a profile verdict to the
        # wording of a sentence.
        self.r.honoured_operating_constraints = declared
        self._record_honoured_declaration(declared)

        # An adapter that never answered scan declared nothing this run could read. Treated
        # as "declares none" and SAID SO in the detail, rather than silently: the two are
        # different facts and C12.1 already fails the first of them.
        honoured = set(declared or ())
        source = ("the CapabilitiesSnapshot" if declared is not None else
                  "no CapabilitiesSnapshot (scan unanswered, C12.1); treated as declaring none")

        # ── C17.1 — FA-R5, over the core seven ────────────────────────────────
        title17_1 = "Decline an assignment carrying an operating constraint it cannot honour"
        title17_2 = "Decline an assignment whose operating subject it does not recognise"
        if self._held_below_latch():
            # Each of C17.1, C17.2 and C17.4 grades an answer to an AssignAction, and a
            # connection held to 1.3.x is sent none. C17.3, the declaration, is graded above.
            self.r.add("C17.1", title17_1, CheckStatus.SKIP, detail=self._held_detail())
            self.r.add("C17.2", title17_2, CheckStatus.SKIP, detail=self._held_detail())
            self.r.add("C17.4",
                       "An assignment carrying only subjects it declared is ACCEPTED, not refused",
                       CheckStatus.SKIP, detail=self._held_detail())
            return
        probe = [s for s in _CORE_OPERATING_SUBJECTS if s not in honoured]
        if not probe:
            self.r.add("C17.1", title17_1, CheckStatus.SKIP,
                       detail=(SkipCause.ALL_CORE_SUBJECTS_HONOURED.value
                               + f": the adapter declared all seven core subjects "
                                 f"{sorted(honoured)}, so this run could construct no core "
                                 f"rule it cannot honour. Whether it ACTUALLY honours them "
                                 f"is FA-R3/FA-R4 and is not verifiable from the wire"))
        else:
            status, detail = self._probe_operating_constraints(
                probe, _ZONE_CORE_TOKEN, "t-operating-core")
            self.r.add("C17.1", title17_1, status,
                       detail=f"{detail}; probed {probe} (declared honoured: "
                              f"{sorted(honoured) or 'none'}, read from {source})")

        # ── C17.2 — FA-R6, the extension subject, exercised exactly once ──────
        status, detail = self._probe_operating_constraints(
            [_EXTENSION_OPERATING_SUBJECT], _ZONE_EXTENSION_TOKEN, "t-operating-extension")
        self.r.add("C17.2", title17_2, status,
                   detail=f"{detail}; probed {_EXTENSION_OPERATING_SUBJECT!r}. AN "
                          f"UNRECOGNISED SUBJECT IS NOT AN ABSENT SUBJECT: treating it as "
                          f"absent would make the namespaced extension form a universal "
                          f"bypass, since any vendor could then route around every "
                          f"operating rule by inventing a subject")

        # ── C17.4 — FA-R2 ARRIVAL: the decline is a function of what was sent ──
        self._check_constraints_arrive(sorted(honoured), source)

    def _check_constraints_arrive(self, honoured, source: str) -> None:
        """C17.4 — an assignment carrying ONLY subjects the adapter declared is ACCEPTED.

        WHAT THIS ADDS THAT C17.1 AND C17.2 CANNOT. Those two require a decline. An
        adapter that refuses EVERY assignment carrying any operating_constraints -- or
        that refuses every assignment at all -- passes both of them without ever having
        read the message. Its green lines would then mean "this adapter declines", not
        "this adapter was told". That is the difference between a check on a behaviour and
        a check on a reflex, and it is the whole of why this row exists.

        So this probe sends a set the adapter has DECLARED it honours and requires
        ACCEPTANCE. Passing C17.1, C17.2 and C17.4 together is only possible for an
        adapter whose answer DEPENDS ON THE SUBJECTS THAT ARRIVED -- which is the
        observable consequence of FA-R2's carry actually reaching it.

        LIVE, like the rest of the family: a running adapter process answered this.

        It CANNOT run against an adapter that declares nothing, because there is no set to
        construct, and it says so with an enumerated cause rather than passing vacuously.
        That is not a coverage hole in practice: Part E makes the whole `zone-restrictions`
        profile applicable only when the adapter declares at least one subject, so every
        adapter the profile applies to can be probed here.

        A conformant adapter ACCEPTS, which starts a task the run never intended to leave
        running. It is cancelled at the same fencing token immediately afterwards, exactly
        as C17.1 cancels a wrongly-accepted one.
        """
        title = "An assignment carrying only subjects it declared is ACCEPTED, not refused"
        if not honoured:
            self.r.add("C17.4", title, CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DECLARES_NO_OPERATING_SUBJECT.value
                               + f": this run could construct no rule the adapter claims to "
                                 f"honour, so there is no way to tell a decline that READ the "
                                 f"constraint set from one that refuses reflexively (declared "
                                 f"honoured: none, read from {source})"))
            return

        if self._served is None:
            self.r.add("C17.4", title, CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DECLARES_NO_ACTION.value
                               + ": no declared action type to carry the honoured rules on"))
            return
        # A type the adapter DECLARED, with its required parameters: the only thing left for
        # a decline to be about is the operating rules. The probe used to send `Custom`, which
        # a catalog-enforcing adapter that does not list it declines INVALID_ACTION -- and this
        # row then read that correct refusal as a false declaration.
        action_id = "t-operating-honoured"
        cid = self._send_command(assign_action=self._assign(
            action_id=action_id, assignment_id=f"a-{action_id}",
            fencing_token=_ZONE_HONOURED_TOKEN, lease_duration_ms=0,
            operating_constraints=[self._operating_rule(s) for s in honoured]))
        named = ", ".join(honoured)
        try:
            result = self._await_result(cid)
        except (EOFError, queue.Empty):
            self.r.add("C17.4", title, CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + f": no CommandResult for the assignment carrying [{named}]"))
            return
        if result.unsupported:
            self.r.add("C17.4", title, CheckStatus.FAIL,
                       detail=f"the adapter answered the assignment carrying [{named}] with "
                              f"unsupported=true; assign_action is not an optional command")
            return
        res = result.assign_action
        if res.accepted:
            self._send_command(cancel_action=pb.CancelAction(
                action_id=action_id, reason="harness: C17.4 cleanup after the expected "
                                            "acceptance", fencing_token=_ZONE_HONOURED_TOKEN))
            self.r.add("C17.4", title, CheckStatus.PASS,
                       detail=f"accepted an assignment carrying [{named}], every one of which "
                              f"it declared it honours -- so its refusals in C17.1/C17.2 are a "
                              f"function of the subjects that ARRIVED on the assignment and "
                              f"not a reflex. FA-R2's carry reached this adapter")
            return
        self.r.add("C17.4", title, CheckStatus.FAIL,
                   detail=f"the adapter DECLINED an assignment carrying [{named}], which it "
                          f"declared it honours (rejection={res.rejection}, "
                          f"unhonourable_subjects={list(res.unhonourable_subjects)}, read from "
                          f"{source}). Either the declaration is false -- FA-R1 asks for the "
                          f"subjects it CAN honour, and claiming one it cannot is a false "
                          f"attestation -- or the adapter refuses every assignment carrying an "
                          f"operating rule without reading it, in which case its C17.1 and "
                          f"C17.2 passes establish nothing about whether it was told"
                          + self._unlifted_note(res.rejection))

    def _record_honoured_declaration(self, declared) -> None:
        """C17.1's precondition and FA-R1's own obligation: the declaration is well formed.

        A subject with no ``/`` MUST be one of the core seven; a subject with a ``/`` is a
        namespaced extension whose prefix is a DNS domain the extender controls. The control
        plane DROPS an entry outside that vocabulary rather than rejecting the declaration,
        so a typo does not fail loudly anywhere -- it silently costs the robot the subject,
        and the robot is then withheld from zones restricting it by a filter that will never
        say why. This row is where that becomes visible.
        """
        title = "The honoured-operating-subject declaration is well formed"
        if declared is None:
            self.r.add("C17.3", title, CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": the adapter answered no scan, so there was no "
                                 "CapabilitiesSnapshot to read a declaration from (C12.1)"))
            return
        if not declared:
            self.r.add("C17.3", title, CheckStatus.PASS,
                       detail="declares no honoured operating subject, which FA-R1 states "
                              "is not a breach. Its robots are simply withheld from zones "
                              "restricting a subject. This row asserts the SHAPE of a "
                              "declaration and nothing about whether a declared subject is "
                              "honoured -- that is FA-R3/FA-R4, and it is not verifiable "
                              "from the wire")
            return
        bad = []
        for i, subject in enumerate(declared):
            if "/" in subject:
                if not _EXTENSION_SUBJECT_RE.match(subject):
                    bad.append(f"[{i}] {subject!r} is not <dns-domain>/<name>")
            elif subject not in _CORE_OPERATING_SUBJECTS:
                bad.append(f"[{i}] {subject!r} has no '/' and is not one of the core seven "
                           f"{list(_CORE_OPERATING_SUBJECTS)}")
        if bad:
            self.r.add("C17.3", title, CheckStatus.FAIL,
                       detail=("; ".join(bad) + ". The control plane DROPS an entry outside "
                               "the vocabulary rather than rejecting the whole declaration, "
                               "so a dropped subject is an undeclared subject and the robot "
                               "loses it without an error anywhere"))
        else:
            self.r.add("C17.3", title, CheckStatus.PASS,
                       detail=f"declared {list(declared)}; every entry is a core subject or "
                              f"a namespaced extension. This row asserts the SHAPE of the "
                              f"declaration only -- whether a declared subject is actually "
                              f"honoured is FA-R3/FA-R4 and is not verifiable from the wire")

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
            # C8.2 and C8.3 need the same EdgeStream. Recorded with the same cause rather
            # than left out: an absent Core row is graded a suite gap, and this is not one.
            for cid, title in (("C8.2", "Tee a PositionFrame per robot to the edge node"),
                               ("C8.3", "Confirmed EstopAck for an edge-issued estop")):
                self.r.add(cid, title, CheckStatus.SKIP, detail=(
                    SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                    + ": adapter registered no robot, so no EdgeStream was opened"))
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
        self.r.add("C8.1", "Adapter dials the advertised edge endpoint", CheckStatus.PASS,
                   detail=(f"EdgeStream established within {_EDGE_WAIT}s of the RegisterAck "
                           f"that advertised edge_endpoints"))

        # C8.2 — the adapter tees PositionFrames up the EdgeStream, fire-and-forget.
        deadline = time.monotonic() + _GET_TIMEOUT
        saw_position = False
        while time.monotonic() < deadline and not saw_position:
            try:
                msg = self.s.edge_in.get(timeout=max(0.0, deadline - time.monotonic()))
            except queue.Empty:
                break
            if msg is _SENTINEL:
                break
            saw_position = msg.WhichOneof("msg") == "position"

        # Exactly one C8.2 row per run that reached the EdgeStream: PASS when frames were
        # seen, FAIL when none were. Omitting the row on absence made a missing tee
        # invisible in the report rather than a failure.
        self.r.add("C8.2", "Tee a PositionFrame per robot to the edge node",
                   CheckStatus.PASS if saw_position else CheckStatus.FAIL,
                   detail=("PositionFrame observed on the EdgeStream" if saw_position else
                           "EdgeStream established but no PositionFrame was teed to it"))
        self._edge_ready = True

    def _check_edge_stop(self) -> None:
        """C8.3 — an edge-issued estop draws a confirmed EstopAck on the EdgeStream. Run LAST.

        An edge stop sets the robot's local latch, and only a local reset at the robot lifts
        it (RFC-0001 #fleet-adapter-protocol-the-stop-latch; safety, *Edge / Headless Emergency
        Stop*). No harness message can reset it, so an adapter that keeps the local latch
        refuses every assignment after this stop, as it must. Run before other checks, it
        graded each one that gives the robot work against that latch. It needs only the
        EdgeStream, so it runs after the refused session, when no check is left that needs
        the robot.

        The adapter also tees PositionFrames up the same stream; the read passes over them,
        and over any ack that names another stop. "Exactly as on SafetyStream" includes C5.2's
        two-phase answer: a STOPPING receipt, then the terminal ack. So a STOPPING ack is not
        the answer this row grades; it waits for the terminal one on the budget C5.6 gives the
        SafetyStream (the edge node resolves it the same way, internal/edge/node.go
        awaitConfirmed).
        """
        if not self._edge_ready:
            return  # _check_edge recorded C8.3 with the reason the EdgeStream was not reached
        self.s.edge_out.put(pb.EdgeControlMessage(
            estop=pb.Estop(estop_id="edge-e1", reason="conformance", issued_by="edge-harness")))
        deadline = time.monotonic() + _GET_TIMEOUT
        ack = None
        saw_stopping = False
        while time.monotonic() < deadline:
            try:
                msg = self.s.edge_in.get(timeout=max(0.0, deadline - time.monotonic()))
            except queue.Empty:
                break
            if msg is _SENTINEL:
                break
            if msg.WhichOneof("msg") != "estop_ack" or msg.estop_ack.estop_id != "edge-e1":
                continue
            if msg.estop_ack.state == pb.ESTOP_STATE_STOPPING and not saw_stopping:
                saw_stopping = True
                deadline = time.monotonic() + _ESTOP_TERMINAL_BUDGET_S
                continue
            ack = msg.estop_ack
            break
        if ack is None:
            self.r.add("C8.3", "Confirmed EstopAck for an edge-issued estop",
                       CheckStatus.FAIL,
                       detail=(f"STOPPING on the EdgeStream, then no terminal EstopAck within "
                               f"{_ESTOP_TERMINAL_BUDGET_S:.0f}s" if saw_stopping else
                               "no confirmed EstopAck on the EdgeStream"))
            return
        ok = ack.state == pb.ESTOP_STATE_STOPPED
        self.r.add("C8.3", "Confirmed EstopAck for an edge-issued estop",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail=(("EstopAck state STOPPED on the EdgeStream"
                            + (" after a STOPPING ack" if saw_stopping else "")) if ok else
                           f"state={ack.state} (not STOPPED)"))


    # The signature reference the probes carry. A well-formed URI that resolves nowhere:
    # no trust root in any deployment verifies it, so an adapter that accepts an artifact
    # under it has not verified what it says it verified.
    _PROBE_SIGNATURE_REF = "https://example.invalid/conformance-probe.sigstore.json"

    # ── C18 — sensor streams and the duty cycle ───────────────────────────────
    #
    # WHAT THESE ROWS CLOSE. The wire contract states four normative rules for the duty
    # cycle -- refcounted activation, a lease-bound stop, frames never on the control
    # plane, and absence-is-not-signal -- and until these checks existed NONE of them had
    # one. The `StreamSink` comment had to disclose that in its own text: "this is
    # NORMATIVE, and it is not yet conformance-checked ... a passing conformance report
    # does not evidence it". A non-conforming adapter relaying camera frames over
    # ControlStream would have passed, and the report would have said so.
    #
    # WHAT THEY STILL DO NOT CLOSE, stated here rather than left to be assumed. C18.2 is
    # a byte-rate comparison, so it detects RELAYING -- a stream's worth of traffic
    # appearing on a control-plane channel -- and not steganography; an adapter dribbling
    # frames through a field at a rate indistinguishable from its telemetry would pass.
    # Absence-is-not-signal is a CONSUMER rule, not an adapter one: it governs what a
    # compute run may conclude from a gap in frames, which is not a message on this wire,
    # so no row here grades it and none pretends to.

    def _stream_to_probe(self):
        """The declared stream C18 exercises, preferring an ON_DEMAND one.

        ON_DEMAND is the case the duty cycle exists for: activation genuinely switches
        something on, so a failure to stop it is a camera left running. An ALWAYS_ON
        stream is still activated and still leased -- the contract says an implementation
        MUST NOT skip activation because a stream is cheap -- but it is the weaker probe,
        so it is the fallback rather than the choice.
        """
        for st in (self._sensor_streams or []):
            if st.activation_cost == pb.ACTIVATION_COST_ON_DEMAND:
                return st
        return (self._sensor_streams or [None])[0]

    def _activate(self, stream_id: str, *, rate_hz: float, lease_s: float,
                  token: int, window=None, sinks=("127.0.0.1:59999",)):
        """Send one ActivateStream and return (result, sent_at_wall_clock).

        `lease_expires_ms` is absolute wall-clock milliseconds, as the wire defines it,
        so the adapter's stop deadline is computed on ITS clock from a value this
        harness set on its own. Both processes are on one machine, so the two
        `time.time()` readings are comparable -- which is what makes C18.3's interval a
        measurement rather than a self-report.
        """
        kwargs = dict(
            robot_id=ROBOT, stream_id=stream_id, rate_hz=rate_hz,
            sinks=[pb.StreamSink(address=a) for a in sinks],
            fencing_token=token,
            lease_expires_ms=int((time.time() + lease_s) * 1000))
        if window is not None:
            kwargs["window"] = window
        sent_at = time.time()
        cid = self._send_command(activate_stream=pb.ActivateStream(**kwargs))
        return self._await_result(cid), sent_at

    def _probe_stream_active_consuming(self, stream_id: str) -> bool:
        """Ask whether a stream is on — AND TURN IT OFF IN THE ASKING. Single-shot.

        THIS IS THE ONLY WIRE OBSERVABLE FOR "the stream is off", and its destructiveness
        is a property of the contract, not of this harness. `fleet_adapter.v1` has no
        StreamStatusUpdate arm, no active-stream field on any status message, and no
        read-only stream query: a stream stopping is not reported, it is simply not
        renewed. The one question that can be asked is `DeactivateStream`, whose answer
        distinguishes "there was a stream and I stopped it" (`deactivated=true`) from
        "there was nothing to stop" (`false`) — and asking it consumes the activation.

        So a POLLING loop is impossible: the first poll would report the stream active
        and switch it off, and the second would report it off, yielding an interval of
        one round trip that says nothing about the lease. C18.3 therefore uses two
        independent single-probe trials rather than a poll, and the wall-clock interval
        is measured out of band (`make stream-lease-measure`), on the adapter's own clock
        where the stop actually happens. Recorded as a gap in the wire, not worked
        around silently.
        """
        cid = self._send_command(deactivate_stream=pb.DeactivateStream(
            robot_id=ROBOT, stream_id=stream_id))
        res = self._await_result(cid)
        if res.unsupported:
            return False
        return res.deactivate_stream.deactivated

    def _check_sensor_streams(self) -> None:
        declared = self._sensor_streams
        if declared is None:
            for cid, title in _C18_TITLES:
                self.r.add(cid, title, CheckStatus.SKIP, level=_C18_LEVELS.get(cid, "MUST"),
                           detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                           + ": scan did not answer with a CapabilitiesSnapshot (C12.1), "
                             "so the sensor-stream declaration was never read")
            return

        # C18.1 — the declaration is well formed. A stream whose id is empty cannot be
        # refcounted (the key is (robot_id, stream_id)), and one claiming a clip pre-roll
        # it does not support is a contradiction an author will only discover when an
        # activation is refused in a building.
        malformed = []
        seen = set()
        for i, st in enumerate(declared):
            if not st.stream_id:
                malformed.append(f"[{i}] empty stream_id — an activation cannot be keyed on it")
            elif st.stream_id in seen:
                malformed.append(f"[{i}] duplicate stream_id {st.stream_id!r} — two declarations, "
                                 f"one refcount key")
            seen.add(st.stream_id)
            if st.clip_pre_seconds_max and not st.supports_clip:
                malformed.append(f"[{i}] {st.stream_id!r} declares clip_pre_seconds_max="
                                 f"{st.clip_pre_seconds_max} with supports_clip=false")
            if st.activation_cost == pb.ACTIVATION_COST_UNSPECIFIED:
                malformed.append(f"[{i}] {st.stream_id!r} leaves activation_cost UNSPECIFIED — "
                                 f"an author cannot tell whether switching it on costs anything")
        if not declared:
            self.r.add("C18.1", "The sensor-stream declaration is well formed",
                       CheckStatus.PASS, detail=(
                           "declares no sensor stream, which is conformant: a robot with "
                           "nothing to offer offers nothing, and the duty cycle below is "
                           "then not applicable to it"))
        else:
            self.r.add("C18.1", "The sensor-stream declaration is well formed",
                       CheckStatus.FAIL if malformed else CheckStatus.PASS,
                       detail="; ".join(malformed) if malformed else
                       f"declares {len(declared)} stream(s): "
                       + ", ".join(f"{st.stream_id}@{st.native_rate_hz:g}Hz" for st in declared))

        probe = self._stream_to_probe()
        if probe is None:
            for cid, title in _C18_TITLES[1:]:
                self.r.add(cid, title, CheckStatus.SKIP, level=_C18_LEVELS.get(cid, "MUST"),
                           detail=SkipCause.ADAPTER_DECLARES_NO_SENSOR_STREAM.value)
            return

        # Baseline the ControlStream's byte rate BEFORE any activation, so C18.2 has
        # something to compare against. Measured, not assumed: an adapter's idle traffic
        # is telemetry at its own cadence and differs per implementation.
        idle_bytes, idle_seconds = self._measure_control_bytes(1.5)

        res, _ = self._activate(probe.stream_id, rate_hz=min(5.0, probe.native_rate_hz or 5.0),
                                lease_s=30.0, token=101)
        if res.unsupported:
            self._declined_arms.add("activate_stream")
            for cid, title in _C18_TITLES[1:]:
                self.r.add(cid, title, CheckStatus.SKIP, level=_C18_LEVELS.get(cid, "MUST"),
                           detail=SkipCause.OPTIONAL_DECLINED.value
                           + ": the adapter declined activate_stream, which is conformant "
                             "(C7.1) and disables the duty cycle for its robots — nothing "
                             "activates, so nothing can be left on")
            return
        if not res.activate_stream.activated:
            for cid, title in _C18_TITLES[1:]:
                self.r.add(cid, title, CheckStatus.SKIP, level=_C18_LEVELS.get(cid, "MUST"),
                           detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                           + f": the adapter refused to activate {probe.stream_id!r} "
                             f"({res.activate_stream.message!r}), so no stream was running "
                             f"to observe")
            return

        # C18.2 — FRAMES NEVER ENTER THE CONTROL PLANE. With a stream running, the
        # ControlStream's byte rate must not have risen to carry it.
        live_bytes, live_seconds = self._measure_control_bytes(1.5)
        idle_rate = idle_bytes / idle_seconds if idle_seconds else 0.0
        live_rate = live_bytes / live_seconds if live_seconds else 0.0
        # A stream at 5 fps of anything a camera produces is kilobytes per frame at
        # minimum. The ceiling is generous on purpose: this row must fire on RELAYING,
        # not on an adapter that happened to emit one extra status message, so it is set
        # far above any plausible control-plane message rate and far below any plausible
        # frame rate. Absolute as well as relative, because an adapter that is silent
        # when idle has an idle rate of zero and no ratio is meaningful against it.
        relayed = live_rate > max(4 * idle_rate, 8192.0)
        self.r.add("C18.2", "No sensor-stream bytes on the control plane while a stream runs",
                   CheckStatus.FAIL if relayed else CheckStatus.PASS, level="MUST NOT",
                   detail=(f"ControlStream {idle_rate:.0f} B/s idle vs {live_rate:.0f} B/s with "
                           f"{probe.stream_id!r} active. "
                           + ("frames appear to be relayed through a control-plane-terminated "
                              "channel, which the contract states as a MUST NOT"
                              if relayed else
                              "LIMIT: this detects RELAYING — a stream's worth of traffic on a "
                              "control-plane channel — and not steganography; an adapter "
                              "dribbling frames through an existing field below this rate "
                              "would pass")))

        # C18.4 — a CLIP against a stream that does not support one MUST be refused, and
        # a pre-roll above the declared maximum MUST be bounded rather than refused.
        # Graded here, before the lease check tears the activation down.
        self._check_clip_bounds()

        # C18.3 — THE FAIL-SAFE. Renew for a while, then stop: a killed controller and a
        # partitioned one are the same thing from here, and the stream must go off.
        self._check_stream_lease_stop(probe)

    def _measure_control_bytes(self, seconds: float):
        """Bytes of AdapterMessage arriving on ControlStream over a window.

        Messages are PUSHED BACK, not consumed: every later check still reads its own
        replies. Returns (bytes, elapsed) — elapsed is measured rather than assumed
        because the drain loop's granularity is the queue's, not the caller's.
        """
        started = time.time()
        total = 0
        held = []
        while time.time() - started < seconds:
            try:
                msg = self.s.control_in.get(timeout=0.1)
            except queue.Empty:
                continue
            if msg is _SENTINEL:
                self._pushback.extend(held)
                self._pushback.append(msg)
                return total, max(1e-9, time.time() - started)
            total += msg.ByteSize()
            held.append(msg)
        self._pushback.extend(held)
        return total, max(1e-9, time.time() - started)

    def _check_clip_bounds(self) -> None:
        """C18.4 — the two clip bounds fail DIFFERENTLY, and the asymmetry is the check.

        A CLIP against a stream declaring supports_clip=false asks for something the
        stream cannot serve at all, so the activation is refused. A pre_seconds above the
        declared clip_pre_seconds_max asks for something it CAN serve, only not for as
        long — so it is bounded to the maximum rather than refused, because a shorter
        pre-roll is still evidence and refusing would discard the post-roll too. An
        adapter that refused both, or bounded both, is wrong in a way that only shows up
        as missing evidence after an incident.
        """
        clipless = next((st for st in self._sensor_streams if not st.supports_clip), None)
        clipful = next((st for st in self._sensor_streams if st.supports_clip), None)
        if clipless is None and clipful is None:
            self.r.add("C18.4", "Clip windows are refused or bounded, per the declaration",
                       CheckStatus.SKIP, level="MUST",
                       detail=SkipCause.ADAPTER_DECLARES_NO_SENSOR_STREAM.value)
            return

        findings = []
        if clipless is not None:
            res, _ = self._activate(
                clipless.stream_id, rate_hz=1.0, lease_s=5.0, token=110,
                window=pb.ClipWindow(mode=pb.CLIP_WINDOW_MODE_CLIP, pre_seconds=1, post_seconds=1))
            if res.unsupported or res.activate_stream.activated:
                findings.append(
                    f"{clipless.stream_id!r} declares supports_clip=false but accepted a CLIP "
                    f"activation; the request is for something the stream cannot serve and "
                    f"MUST be refused")
            else:
                findings.append(f"CLIP on {clipless.stream_id!r} (supports_clip=false) refused: "
                                f"{res.activate_stream.message!r}")

        if clipful is not None:
            over = (clipful.clip_pre_seconds_max or 0) + 7
            res, _ = self._activate(
                clipful.stream_id, rate_hz=1.0, lease_s=5.0, token=111,
                window=pb.ClipWindow(mode=pb.CLIP_WINDOW_MODE_CLIP,
                                     pre_seconds=over, post_seconds=1))
            if res.unsupported or not res.activate_stream.activated:
                findings.append(
                    f"{clipful.stream_id!r} refused a CLIP whose pre_seconds ({over}) merely "
                    f"exceeded its declared clip_pre_seconds_max ({clipful.clip_pre_seconds_max}); "
                    f"it MUST be bounded, not refused — refusing discards the post-roll too")
                self.r.add("C18.4", "Clip windows are refused or bounded, per the declaration",
                           CheckStatus.FAIL, detail="; ".join(findings))
                return
            # The result carries NO structured window echo, so an implementation that
            # bounds a pre-roll MUST report the applied value in the free-text message —
            # a consumer that is not told has no way to discover it got less.
            said = str(clipful.clip_pre_seconds_max) in res.activate_stream.message
            findings.append(
                f"over-long CLIP on {clipful.stream_id!r} bounded rather than refused; message "
                + (f"reports the applied bound: {res.activate_stream.message!r}" if said else
                   f"does NOT report the applied value ({res.activate_stream.message!r}) — "
                   f"ActivateStreamResult carries no window echo, so the consumer cannot "
                   f"discover it received less pre-roll than it asked for"))
            self._send_command(deactivate_stream=pb.DeactivateStream(
                robot_id=ROBOT, stream_id=clipful.stream_id))
            if not said:
                self.r.add("C18.4", "Clip windows are refused or bounded, per the declaration",
                           CheckStatus.FAIL, detail="; ".join(findings))
                return

        bad = [f for f in findings if "MUST" in f]
        self.r.add("C18.4", "Clip windows are refused or bounded, per the declaration",
                   CheckStatus.FAIL if bad else CheckStatus.PASS, detail="; ".join(findings))

    def _check_stream_lease_stop(self, probe) -> None:
        """C18.3 — THE FAIL-SAFE. Kill the controller; the camera goes off at the horizon.

        This is the row the whole mechanism rests on. The claim is not "a stream can be
        deactivated" — that is C18.5, and an adapter could satisfy it while leaving a
        camera running forever on a partition. The claim is that THE SAFE DIRECTION IS
        OFF: the control plane going away STOPS the sensor, and stops it AT the lease
        horizon rather than "eventually".

        It is the deliberate INVERSE of the emergency-stop lease direction. A stop must
        always be honoured, so nothing may gate it; a stream's failure mode is "left on",
        so everything gates it. Both fail toward their own safe state and those states
        point opposite ways.

        # TWO TRIALS, ONE PROBE EACH, AND WHY IT CANNOT BE A POLL

        The only wire observable for stream state consumes it
        (_probe_stream_active_consuming), so a poll would measure its own round trip. The
        two trials bracket the horizon instead, which is what the requirement actually
        says:

          * HELD — activate, wait well INSIDE the lease, probe once: the stream MUST
            still be running. Without this arm an adapter that stops on a fixed timer
            regardless of renewals would pass — a sensor abandoning live work, the same
            defect from the other side, and exactly what C4.1 is to C4.2.
          * PARTITIONED — activate, send NOTHING, wait PAST the horizon, probe once: the
            stream MUST be gone.

        The wall-clock interval between the two is not measurable from here for the
        reason above. It is measured out of band on the adapter's own clock by
        `make stream-lease-measure`, and that number — not this row — is the evidence
        for the fail-safe's latency. This row establishes the DIRECTION and the BRACKET.
        """
        stream_id = probe.stream_id
        title = "A stream stops on lease expiry when the control plane goes silent"

        # ── Trial 1: HELD. Renewals arrive; the stream must survive. ──
        res, _ = self._activate(stream_id, rate_hz=1.0, lease_s=_STREAM_LEASE_S, token=120)
        if res.unsupported or not res.activate_stream.activated:
            self.r.add("C18.3", title, CheckStatus.SKIP, level="MUST",
                       detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                       + f": could not activate {stream_id!r} under a short lease")
            return
        # A renewal IS a fresh ActivateStream carrying the same fencing token — the wire
        # gives streams no separate renew arm. Renew across more than one whole lease
        # duration, so surviving cannot be explained by the original grant still running.
        held_until = time.time() + _STREAM_LEASE_S * 1.5
        while time.time() < held_until:
            time.sleep(_STREAM_LEASE_S / 3)
            r2, _ = self._activate(stream_id, rate_hz=1.0, lease_s=_STREAM_LEASE_S, token=120)
            if r2.unsupported or not r2.activate_stream.activated:
                self.r.add("C18.3", title, CheckStatus.FAIL,
                           detail="the adapter refused a renewal (a re-issued ActivateStream under "
                                  "the same fencing token) while the stream was live; a stream "
                                  "that cannot be renewed cannot be held")
                return
        held = self._probe_stream_active_consuming(stream_id)
        if not held:
            self.r.add("C18.3", title, CheckStatus.FAIL,
                       detail=(f"{stream_id!r} was already off after "
                               f"{_STREAM_LEASE_S * 1.5:.1f}s of UNBROKEN renewals against a "
                               f"{_STREAM_LEASE_S:.1f}s lease — it stops on a timer rather than "
                               f"on the lease, which abandons live work"))
            return

        # ── Trial 2: PARTITIONED. The controller is dead; nothing is renewed. ──
        res, activated_at = self._activate(stream_id, rate_hz=1.0, lease_s=_STREAM_LEASE_S,
                                           token=121)
        if res.unsupported or not res.activate_stream.activated:
            self.r.add("C18.3", title, CheckStatus.SKIP, level="MUST",
                       detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                       + ": could not re-establish the activation for the partition trial")
            return
        # From here the harness issues NOTHING for this stream. Waiting past the horizon
        # plus the grace covers the adapter's own watchdog granularity; the probe that
        # follows is the single observation.
        time.sleep(_STREAM_LEASE_S + _STREAM_LEASE_GRACE_S)
        waited = time.time() - activated_at
        still_running = self._probe_stream_active_consuming(stream_id)

        # The two ends of the bracket, published as numbers rather than left inside a
        # sentence. They are not the partition-to-off interval — this row cannot measure
        # that — but they are what this row DID observe, and a reader should not have to
        # parse them back out of prose.
        self.r.measurements["stream_lease_seconds"] = round(_STREAM_LEASE_S, 3)
        self.r.measurements["stream_held_across_renewals_seconds"] = round(_STREAM_LEASE_S * 1.5, 3)
        self.r.measurements["stream_unrenewed_before_probe_seconds"] = round(waited, 3)
        for key in ("stream_lease_seconds", "stream_held_across_renewals_seconds",
                    "stream_unrenewed_before_probe_seconds"):
            self.r.measurement_checks[key] = "C18.3"

        self.r.add("C18.3", title,
                   CheckStatus.FAIL if still_running else CheckStatus.PASS,
                   detail=(f"{stream_id!r} was STILL RUNNING {waited:.2f}s after its last renewal, "
                           f"against a {_STREAM_LEASE_S:.1f}s lease. A control-plane partition has "
                           f"left a sensor on with nothing watching it — the failure the lease "
                           f"direction exists to prevent"
                           if still_running else
                           f"HELD across {_STREAM_LEASE_S * 1.5:.1f}s of renewals, then GONE "
                           f"{waited:.2f}s after the last one, against a {_STREAM_LEASE_S:.1f}s "
                           f"lease. Two single-probe trials, not a poll: the only wire observable "
                           f"for stream state is DeactivateStream, which consumes the activation "
                           f"it reports on, so a poll would measure its own round trip. This row "
                           f"establishes the DIRECTION and the bracket; the wall-clock "
                           f"partition-to-off INTERVAL is measured on the adapter's own clock by "
                           f"`make stream-lease-measure`"))

        # C18.5 — the complement: an explicit DeactivateStream also stops a live stream,
        # so the fail-safe above is not the ONLY way a stream ever goes off. An adapter
        # that ignored deactivate_stream would keep a camera on for a whole lease after
        # the last consumer settled, every time.
        res, _ = self._activate(stream_id, rate_hz=1.0, lease_s=30.0, token=130)
        if res.unsupported or not res.activate_stream.activated:
            self.r.add("C18.5", "DeactivateStream stops a live stream",
                       CheckStatus.SKIP, level="MUST",
                       detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                       + ": could not activate a stream to deactivate")
            return
        cid = self._send_command(deactivate_stream=pb.DeactivateStream(
            robot_id=ROBOT, stream_id=stream_id))
        dres = self._await_result(cid)
        if dres.unsupported:
            self.r.add("C18.5", "DeactivateStream stops a live stream", CheckStatus.FAIL,
                       detail="the adapter honoured activate_stream but declined "
                              "deactivate_stream; the contract requires an adapter declining one "
                              "to decline both, so this leaves every stream it starts running "
                              "until its lease lapses")
            return
        stopped = (dres.deactivate_stream.deactivated
                   and not self._probe_stream_active_consuming(stream_id))
        self.r.add("C18.5", "DeactivateStream stops a live stream",
                   CheckStatus.PASS if stopped else CheckStatus.FAIL,
                   detail=(f"{stream_id!r} reported stopped, and a second DeactivateStream "
                           f"confirms there is nothing left to stop" if stopped else
                           f"deactivated={dres.deactivate_stream.deactivated} and the stream "
                           f"still reports active; the last consumer leaving does not switch "
                           f"the sensor off"))

    def _check_artifact_verification(self) -> None:
        """C7.2 and C7.3 (MUST, when implemented) — verify the artifact and fail closed.

        C7.2 covers model_update: verify the artifact checksum, and the signature when one is
        required. C7.3 covers push_firmware and is a SEPARATE obligation, not a restatement —
        it is the robot-side re-verify of the delivered BYTES. The control plane checked the
        publisher's signature over the checksum, which says nothing about what arrived here.

        The two are independent: an adapter may decline one under C7.1 and implement the other,
        so a decline of model_update must not skip the firmware probe.

        Probed with a DELIBERATELY WRONG checksum AND an unresolvable signature reference. A
        conformant adapter answers acknowledged=false; acknowledging is a failed MUST, because
        the control plane records acknowledged=true as "this artifact was checked" and a robot
        then runs code nobody verified — a supply-chain compromise on the machine.

        THE SIGNATURE LEG, and what it can and cannot establish. ``model_signature_ref`` /
        ``firmware_signature_ref`` are the only wire fields carrying the publisher-trust check,
        and the probes were sending neither: nothing in a passing report was evidence about
        signatures at all. They are populated now, and the decidable obligation is the RESULT
        side — ``ModelUpdateResult.verified_signer`` / ``FirmwareResult.verified_signer`` are
        specified "populated when acknowledged=true", so an adapter that accepts an artifact
        carrying a signature reference and names no signer has installed something it did not
        verify, and that IS visible here. What is NOT visible is trust-root verification under
        ``SwarmadaConfig.spec.signing.requireSignatureVerification``: that flag is a
        control-plane resource, not a wire field, so this harness cannot assert the condition
        the catalog's "when signature verification is required" clause depends on. The catalog
        says so rather than implying the run covered it.

        An adapter that declines model_update (unsupported=true, C7.1) records a SKIP: declining
        is conformant, and a skip states plainly that verification was not observed rather than
        implying it passed.
        """
        cid = self._send_command(model_update=pb.ModelUpdate(
            model_name="conformance-probe",
            new_version="9.9.9",
            model_uri="oci://example.invalid/conformance-probe:9.9.9",
            # Well-formed but WRONG: it cannot match any artifact the adapter fetches.
            model_checksum="sha256:" + ("0" * 64),
            model_signature_ref=self._PROBE_SIGNATURE_REF,
        ))
        res = self._await_result(cid)
        if res.unsupported:
            self.r.add("C7.2", "Verify MODEL artifact checksum and signature, fail closed",
                       CheckStatus.SKIP, detail=(
                           SkipCause.OPTIONAL_DECLINED.value + ": " + "adapter declines "
                           "model_update (conformant, C7.1), so artifact verification was not "
                           "exercised"))
            # Do NOT return here. The firmware body re-verify below (C7.3) is a separate
            # requirement against a separate command, and an adapter may decline one while
            # implementing the other. An early return dropped C7.3 out of the report
            # entirely — no PASS, no FAIL, no SKIP, simply absent.
        else:
            mu = res.model_update
            status, detail = self._grade_artifact_answer(
                accepted=mu.acknowledged, signer=mu.verified_signer, message=mu.message,
                noun="model_update", verb="ACKNOWLEDGED", installed="an unverified model")
            self.r.add("C7.2", "Verify MODEL artifact checksum and signature, fail closed",
                       status, detail=detail)

        # C7.3 is the ROBOT-SIDE body re-verify, a separate requirement from C7.2: the
        # control plane already checked the publisher's signature over the checksum, but that says
        # nothing about the bytes this robot received. An adapter that flashes without re-hashing
        # trusts every hop of the delivery path.
        cid = self._send_command(push_firmware=pb.PushFirmware(
            target_version="9.9.9-conformance",
            firmware_uri="https://example.invalid/fw-9.9.9.bin",
            firmware_checksum="sha256:" + ("0" * 64),  # well-formed but cannot match any artifact
            firmware_signature_ref=self._PROBE_SIGNATURE_REF,
        ))
        res = self._await_result(cid)
        if res.unsupported:
            self.r.add("C7.3", "Verify FIRMWARE artifact body and signature before flashing, "
                               "fail closed",
                       CheckStatus.SKIP, detail=(
                           SkipCause.OPTIONAL_DECLINED.value
                           + ": adapter declines push_firmware (conformant, C7.1), so body "
                             "re-verify was not exercised"))
            return
        fw = res.push_firmware
        status, detail = self._grade_artifact_answer(
            accepted=fw.accepted, signer=fw.verified_signer, message=fw.message,
            noun="push_firmware", verb="ACCEPTED", installed="unverified firmware")
        self.r.add("C7.3", "Verify FIRMWARE artifact body and signature before flashing, "
                           "fail closed", status, detail=detail)

    @staticmethod
    def _grade_artifact_answer(*, accepted: bool, signer: str, message: str,
                               noun: str, verb: str, installed: str):
        """Grade one artifact answer against both legs of the fail-closed MUST.

        Failing closed is a PASS whichever leg triggered it — the requirement is that the
        artifact is not installed, not that the adapter says which check refused it. An
        ACCEPT is graded on the signature leg, which is where the two outcomes differ: an
        accept naming no ``verified_signer`` states, on the wire, that nothing verified the
        signature reference the probe carried.
        """
        if not accepted:
            return CheckStatus.PASS, (
                "failed closed on an artifact whose checksum cannot match and whose "
                "signature reference resolves nowhere: " + message)
        if not signer:
            return CheckStatus.FAIL, (
                f"adapter {verb} a {noun} carrying a signature reference and named no "
                f"verified_signer, which is specified populated on an accepted artifact — "
                f"{installed} would be installed with nothing having verified it")
        return CheckStatus.FAIL, (
            f"adapter {verb} a {noun} whose checksum cannot match the artifact and whose "
            f"signature reference resolves nowhere, reporting verified_signer={signer!r} — "
            f"{installed} would be installed")

    # ── C13.2 / C14 — the version-refusal path, and estop's invariance to it ──────
    #
    # These run LAST because they drop the ControlStream: the harness cannot make a conformant
    # adapter misreport its version, so the only honest way to exercise the refusal is to play a
    # control plane that refuses, and see what the adapter does next.

    def _check_accepting_reconnect(self) -> None:
        """C2.4 + C6.1 + C2.2 — fault the stream, then ACCEPT the re-registration.

        C2.4 grades the redial handshake itself: safety.md's connectivity-loss rules say
        the adapter MUST send a ``register`` first on reconnect and MUST NOT send a
        ``discover`` before that register is rejected NOT_ADMITTED, and the
        Required-message table says the same. The robot is admitted here, so the
        register is accepted and no discover is ever correct. The other two are about a
        SUCCESSFUL reconnect: the first telemetry
        afterwards must be a full snapshot (an adapter that resumed deltas leaves the
        control plane with a stale hardware picture it cannot detect), and the
        authoritative action state in the RegisterAck must be adopted (the window where a
        task was cancelled during the outage — the hazard the reconnect handshake exists
        to close).
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
            for cid, title in (("C2.4", "Re-register on reconnect; never re-discover"),
                               ("C6.1",
                                "First TelemetryPayload after reconnect is a full snapshot"),
                               ("C2.2", "Adopt authoritative_action_state from RegisterAck")):
                self.r.add(cid, title, CheckStatus.SKIP,
                           level="MUST NOT" if cid == "C2.4" else "MUST",
                           detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                                  + ": adapter did not redial within "
                                  + f"{_RECONNECT_WAIT}s")
            self._record_reconnect_unreached(f"adapter did not redial within {_RECONNECT_WAIT}s")
            return

        # Re-handshake: accept the hello, then accept the registration and hand back an
        # authoritative state saying the task the robot last held was CANCELLED.
        ghost = "t-status"   # the action the adapter accepted earlier in this run
        accepted_register = False
        redialled_with_discover = False
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
                # Agreed for this connection, as the first one was.
                self.s.control_out.put(pb.ControlPlaneMessage(
                    hello_ack=self._hello_ack(msg.hello.contract_version)))
            elif kind == "discover":
                # safety.md, connectivity loss item 3: no discover before a register is
                # rejected NOT_ADMITTED, and this robot is admitted. This branch used to
                # share the register arm and answer a
                # RegisterAck — the harness completed a handshake the specification says
                # MUST NOT be attempted, and C6.1/C2.2 then passed on the strength of it.
                # The refusal is in band (ALREADY_EXISTS), and the loop keeps waiting for
                # the register that MUST follow, so an adapter that corrects itself still
                # reaches the reconnect checks.
                redialled_with_discover = True
                self.s.control_out.put(pb.ControlPlaneMessage(discover_ack=pb.DiscoverAck(
                    accepted=False, rejection=pb.REGISTRATION_REJECTION_ALREADY_EXISTS,
                    message="a Robot already exists for this id; re-register on reconnect")))
            elif kind == "register":
                # fencing_token=22 is STRICTLY ABOVE the run's high-water mark (19, set by
                # C17.4's probe). It used to be 14 — equal to the mark — and that is why
                # the adoption gap stayed invisible for a whole draft: with the authoritative token
                # equal to the local one, an adapter that ADOPTS it and one that ignores it
                # emit byte-identical traces, so C2.2 and C3.1 both passed against an adapter
                # that never read the field. A fixture cannot detect a difference it does not
                # create. C3.8 below is the check this value exists to make possible.
                self.s.control_out.put(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                    accepted=True, telemetry_interval_seconds=1,
                    authoritative_action_state=pb.RobotActionState(
                        phase=pb.ROBOT_ACTION_PHASE_CANCELLED, action_id=ghost,
                        fencing_token=_AUTHORITATIVE_TOKEN, lease_generation=0))))
                accepted_register = True
                break
        if redialled_with_discover:
            self.r.add("C2.4", "Re-register on reconnect; never re-discover",
                       CheckStatus.FAIL, level="MUST NOT",
                       detail=("the redial sent discover; safety.md's connectivity-loss "
                               "rules make re-discovery a MUST NOT and require register. "
                               "The control plane answered ALREADY_EXISTS"
                               + ("; the adapter then registered correctly"
                                  if accepted_register else
                                  " and no register followed")))
        elif accepted_register:
            self.r.add("C2.4", "Re-register on reconnect; never re-discover",
                       CheckStatus.PASS, level="MUST NOT",
                       detail="the redial re-registered, with no discover attempt")
        else:
            self.r.add("C2.4", "Re-register on reconnect; never re-discover",
                   CheckStatus.SKIP, level="MUST NOT",
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + ": adapter re-opened the stream but attempted no "
                                 "lifecycle message"))
        if not accepted_register:
            for cid, title in (("C6.1",
                                "First TelemetryPayload after reconnect is a full snapshot"),
                               ("C2.2", "Adopt authoritative_action_state from RegisterAck")):
                self.r.add(cid, title, CheckStatus.SKIP,
                           detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                                  + ": adapter re-opened the stream but attempted no registration")
            self._record_reconnect_unreached(
                "adapter re-opened the stream but attempted no registration")
            return
        # What is held for a later reader now arrived before the RegisterAck; C2.2 names none
        # of it as the reconciliation.
        before_ack = {id(m) for m in self._pushback}

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

        if self._held_below_latch():
            # C2.2 reconciles the action the adapter accepted earlier in the run, and C3.8
            # probes with an AssignAction. A connection held to 1.3.x was given no action and is
            # sent no AssignAction, so neither binds it. C2.3 is still measured below, on a robot
            # nothing drove.
            self.r.add("C2.2", "Adopt authoritative_action_state from RegisterAck",
                       CheckStatus.SKIP,
                       detail=self._held_detail("an action the adapter accepted earlier in the "
                                                "run"))
            self.r.add("C3.8", "Adopt RobotActionState.fencing_token as the high-water mark",
                       CheckStatus.SKIP, detail=self._held_detail())
            self._measure_cadence()
            return

        # C2.2 — the adapter was told its last action is CANCELLED. It MUST reconcile:
        # halt and report, rather than resume executing a task the control plane dropped.
        # Taken BEFORE the cadence probe below, which puts a fresh assignment on the wire:
        # reconciliation of the ghost must be observed against the state the RegisterAck
        # established, not against one this harness then changed.
        #
        # WHAT IS GRADED, as C4.2 grades the lapsed lease: the ghost's terminal report, any
        # state RFC-0001 calls terminal. ACTION_STATE_STOPPED is the commanded stop and not an
        # end (RFC-0001 safety chapter, *terminal*), so it never satisfies this row; one that
        # comes first is named in the detail. A STOPPED held from before the reconnect is not
        # this reconciliation and is not named.
        upd = self._await_action_status(ghost, timeout=_TERMINAL_STATUS_WAIT,
                                        states=_RFC_TERMINAL_ACTION_STATES())
        stop_seen = any(id(m) not in before_ack and m.WhichOneof("payload") == "action_status"
                        and m.action_status.action_id == ghost
                        and m.action_status.state == pb.ACTION_STATE_STOPPED
                        for m in self._pushback if m is not _SENTINEL)
        window = f"{_TERMINAL_STATUS_WAIT:g}s"
        if upd is not None:
            detail = ("reported " + pb.ActionState.Name(upd.state)
                      + (", after ACTION_STATE_STOPPED (the commanded stop, not counted)"
                         if stop_seen else ""))
        elif stop_seen:
            detail = (f"reported only ACTION_STATE_STOPPED within {window} for the action the "
                      f"RegisterAck declared CANCELLED: STOPPED is the commanded stop and not "
                      f"terminal (RFC-0001 safety chapter, *terminal*), and no terminal report "
                      f"followed it, so the control plane cannot tell the action ended")
        else:
            detail = (f"no terminal report within {window} for the action the RegisterAck "
                      f"declared CANCELLED; a task cancelled during an outage would keep running")
        self.r.add("C2.2", "Adopt authoritative_action_state from RegisterAck",
                   CheckStatus.PASS if upd is not None else CheckStatus.FAIL, detail=detail)

        # C3.8 — fencing-token ADOPTION. The RegisterAck above declared the
        # robot's authoritative token to be _AUTHORITATIVE_TOKEN, strictly above the mark
        # the adapter reached on its own. `RobotActionState.fencing_token` is documented in
        # fleet_adapter.proto as the token the adapter should be executing under, against
        # which it applies the fencing rule (C3.2: an AssignAction or CancelAction at or
        # below it is rejected, except an identical re-delivery and a CancelAction naming
        # the executing action), so after this handshake an AssignAction at
        # _ADOPTION_PROBE_TOKEN is below the current token and must be refused.
        #
        # This is the ONE ordering hazard C3.2 cannot see. C3.2 refuses tokens below a mark
        # the adapter raised ITSELF, by accepting an assignment. Nothing raises the mark from
        # the control plane's own statement of it, so after a control-plane FAILOVER — where
        # the new plane legitimately holds a higher token than this adapter ever saw — the
        # adapter goes on honouring commands authorised by the plane that just died. Two
        # executors, one robot.
        #
        # Taken AFTER the C2.2 assertion for the reason stated there (a fresh assignment
        # changes the state C2.2 reads) and BEFORE the cadence drive, which raises the mark
        # past both values and would erase the difference this check exists to measure.
        if self._served is None:
            self.r.add("C3.8", "Adopt RobotActionState.fencing_token as the high-water mark",
                       CheckStatus.SKIP,
                       level="MUST" if self._reported_contract == CONTRACT_VERSION else "SHOULD",
                       detail=(SkipCause.ADAPTER_DECLARES_NO_ACTION.value
                               + ": no declared action type to carry the adoption probe"))
        else:
            cid = self._send_command(assign_action=self._assign(
                action_id="t-adopt", assignment_id="a-adopt",
                fencing_token=_ADOPTION_PROBE_TOKEN))
            # ADR-0032: supported skew is N and N-1 minor, and "patch and minor releases never
            # break an adapter, so they never invalidate its qualification". Grading this MUST
            # against an adapter that declares 1.0.0 would do exactly that — a previously
            # CONFORMANT adapter would go NON-CONFORMANT on a minor bump alone, and the control
            # plane's RobotAdmissionGate would then refuse it assignment. So it is advisory at
            # N-1 and binding at N; it becomes an unconditional MUST at the next MAJOR, which
            # requires re-qualification anyway. The check still RUNS and still reports FAIL at
            # N-1 — the adapter author sees the defect, the fleet does not stop.
            binding = self._reported_contract == CONTRACT_VERSION
            level = "MUST" if binding else "SHOULD"
            skew = ("" if binding else
                    f"; ADVISORY: adapter declares contract {self._reported_contract or 'none'} "
                    f"and this suite attests {CONTRACT_VERSION} (ADR-0032 N-1 skew), so the "
                    f"failure is reported but does not gate conformance")
            try:
                res = self._await_result(cid).assign_action
                stale = (not res.accepted
                         and res.rejection == pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN)
                self.r.add("C3.8", "Adopt RobotActionState.fencing_token as the high-water mark",
                           CheckStatus.PASS if stale else CheckStatus.FAIL, level=level,
                           detail=(f"a token of {_ADOPTION_PROBE_TOKEN}, below the "
                                   f"{_AUTHORITATIVE_TOKEN} the RegisterAck declared "
                                   f"authoritative, "
                                   f"was refused STALE" if stale else
                                   f"the RegisterAck declared fencing_token="
                                   f"{_AUTHORITATIVE_TOKEN}; a later AssignAction at "
                                   f"{_ADOPTION_PROBE_TOKEN} was NOT refused STALE "
                                   f"(accepted={res.accepted} rejection={res.rejection}). The "
                                   f"adapter never adopted the control plane's token, so after a "
                                   f"failover it keeps honouring commands the old plane "
                                   f"authorised{skew}" + self._unlifted_note(res.rejection)))
            except (EOFError, queue.Empty):
                self.r.add("C3.8", "Adopt RobotActionState.fencing_token as the high-water mark",
                           CheckStatus.SKIP, level=level,
                           detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                                   + ": adapter sent no result for the adoption probe"))

        # C2.3 — the RegisterAck above advertised telemetry_interval_seconds=1. Honouring
        # per-robot configuration is a WIRE effect: measure the spacing of the payloads
        # that follow. Bounds are wide (0.5-2.5s) because this is a cadence assertion, not
        # a clock test — the point is that the adapter adopted ~1s rather than ignoring the
        # field and keeping its own default.
        #
        # DRIVE THE ROBOT FIRST. fleet-adapter-protocol.md's motion-gated position
        # telemetry row is a SHOULD: an adapter may skip a payload while the robot has
        # moved less than motionThresholdMeters, sending one only every
        # maxIdleIntervalSeconds (recommended class default 30s). Measured against the
        # stationary robot this reconnect leaves behind, a conformant motion-gated adapter
        # and one ignoring the interval produce the SAME wire trace, and this check failed
        # the first. Neither field is on the wire — both are Robot.spec values — so the
        # harness cannot turn gating off; it can only remove the condition that triggers it.
        self._measure_cadence()

    def _measure_cadence(self) -> None:
        """C2.3: drive the robot when the connection may be given work, then time the payloads."""
        in_motion = self._drive_for_cadence()
        stamps = []
        for _ in range(3):
            t0 = time.monotonic()
            if self._await_telemetry(timeout=4.0) is None:
                break
            stamps.append(time.monotonic() - t0)
        # The first sample includes whatever was already queued.
        self._record_cadence(stamps[1:] if len(stamps) >= 3 else None, in_motion=in_motion)

    def _record_reconnect_unreached(self, why: str) -> None:
        """C3.8 and C2.3 for a run whose accepting reconnect never completed.

        Both are graded on the re-registered session, so neither was reached. They are
        recorded, with the adapter as the cause, instead of being left out: the profile
        layer grades an absent Core row as a suite gap, which would blame the suite for
        what the adapter did not do.
        """
        self.r.add("C3.8", "Adopt RobotActionState.fencing_token as the high-water mark",
                   CheckStatus.SKIP,
                   level="MUST" if self._reported_contract == CONTRACT_VERSION else "SHOULD",
                   detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value + ": " + why)
        self.r.add("C2.3", "Honour telemetry_interval_seconds from RegisterAck",
                   CheckStatus.SKIP,
                   detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value + ": " + why)

    def _check_latch_holds_work(self) -> None:
        """C5.8 (MUST at 1.4.0) — a stop holds the robot's work until its EstopClear lifts it.

        RFC-0001 #fleet-adapter-protocol-the-stop-latch, *While latched*: the adapter "MUST
        refuse every AssignAction for a robot while ... any stop ID is latched for the robot",
        "with accepted = false and ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE, whatever its
        fencing token". *The clear*: an EstopClear lifts the ID it names once the stop's fence
        has arrived, and "When a clear takes effect, the adapter sends an EstopClearAck naming
        the ID". *Version*: the rules bind a connection that negotiated 1.4.0.

        The probe stops the robot as row S1 does, with the Estop on SafetyStream and its fence
        on ControlStream. It assigns new work at _LATCH_HELD_TOKEN, clears the stop, and
        assigns new work again at _LATCH_LIFTED_TOKEN. It passes when the first is refused
        ROBOT_UNAVAILABLE, the clear draws its EstopClearAck, and the second is accepted. It
        cancels the cadence drive first, so the first refusal is not a busy robot's.

        Not graded here: the fence alone as a stop, the leader term, the reports, the local
        latch and interrupted work (CONFORMANCE.md, *Adapter obligations with no C-row*).
        """
        title = "A stop holds the robot's work until its EstopClear lifts it"
        if not self._contract_ok:
            self.r.add("C5.8", title, CheckStatus.SKIP, detail=(
                f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: the adapter reported no "
                f"supported contract version, so no connection was bound by the stop latch"))
            return
        if not self._latch_binds():
            self.r.add("C5.8", title, CheckStatus.SKIP, detail=(
                f"{SkipCause.CONTRACT_PREDATES_OBLIGATION.value}: the adapter reported "
                f"contract {self._reported_contract}. The stop latch binds a connection at "
                f"1.4.0, and a control plane sends an older one no EstopClear, so the probe "
                f"sent nothing"))
            return
        self._cancel("t-cadence", _CADENCE_TOKEN, "harness: C5.8 needs the robot idle")
        stop_id = _LATCH_PROBE_ID
        self.s.safety_out.put(pb.ControlPlaneSafetyMessage(
            robot_id=ROBOT,
            estop=pb.Estop(estop_id=stop_id, reason="conformance: C5.8", issued_by="harness")))
        self._fence(stop_id, "conformance: C5.8")
        try:
            ack, _passed = self._recv_estop_ack(stop_id)
        except EOFError:
            ack = None
        if ack is None:
            self._clear_stop(stop_id)
            self.r.add("C5.8", title, CheckStatus.SKIP, detail=(
                f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: no EstopAck named the probe's "
                f"stop {stop_id!r} within {_GET_TIMEOUT:.0f}s, so no stop was acknowledged "
                f"to hold work against (C5 grades the answer to a stop)"))
            return
        held = self._latch_probe("t-latch-held", _LATCH_HELD_TOKEN)
        lifted = self._clear_stop(stop_id)
        after = self._latch_probe("t-latch-lifted", _LATCH_LIFTED_TOKEN)
        for res, action_id, token in ((held, "t-latch-held", _LATCH_HELD_TOKEN),
                                      (after, "t-latch-lifted", _LATCH_LIFTED_TOKEN)):
            if res is not None and res.accepted:
                self._cancel(action_id, token, "harness: C5.8 cleanup")

        def why(res) -> str:
            return (f"{pb.AssignActionRejection.Name(res.rejection)} ({res.message!r})"
                    if res.rejection else f"with no rejection code ({res.message!r})")

        stopped = (f"the adapter acknowledged stop {stop_id!r} "
                   f"({pb.EstopState.Name(ack.state)})")
        if held is None or after is None:
            self.r.add("C5.8", title, CheckStatus.SKIP, detail=(
                f"{SkipCause.ADAPTER_DID_NOT_REACH_STATE.value}: no AssignActionResult for "
                f"the assignment at token "
                f"{_LATCH_HELD_TOKEN if held is None else _LATCH_LIFTED_TOKEN}"))
        elif held.accepted:
            self.r.add("C5.8", title, CheckStatus.FAIL, detail=(
                f"{stopped}, and an AssignAction at token {_LATCH_HELD_TOKEN} was then "
                f"accepted. While a stop ID is latched, a 1.4.0 adapter refuses every "
                f"AssignAction ROBOT_UNAVAILABLE and starts nothing (RFC-0001 The Stop Latch, "
                f"While latched); this one keeps no latch"))
        elif held.rejection != pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE:
            self.r.add("C5.8", title, CheckStatus.FAIL, detail=(
                f"{stopped}; the AssignAction at token {_LATCH_HELD_TOKEN} was refused "
                f"{why(held)}. Under the latch the refusal is ROBOT_UNAVAILABLE, whatever the "
                f"fencing token"))
        elif not lifted:
            self.r.add("C5.8", title, CheckStatus.FAIL, detail=(
                f"{stopped}; work at token {_LATCH_HELD_TOKEN} was refused ROBOT_UNAVAILABLE. "
                f"The harness then cleared the stop (its fence, then an EstopClear at leader "
                f"term {_LEADER_TERM}) and received no EstopClearAck naming it within "
                f"{_CLEAR_WAIT:.0f}s. A clear that takes effect is acknowledged (The clear)"))
        elif not after.accepted:
            self.r.add("C5.8", title, CheckStatus.FAIL, detail=(
                f"{stopped}; work at token {_LATCH_HELD_TOKEN} was refused ROBOT_UNAVAILABLE, "
                f"and the clear drew its EstopClearAck. New work at token "
                f"{_LATCH_LIFTED_TOKEN} was still refused {why(after)}: the EstopClearAck "
                f"says the stop is lifted, and a lifted robot takes new work"))
        else:
            self.r.add("C5.8", title, CheckStatus.PASS, detail=(
                f"{stopped}; work at token {_LATCH_HELD_TOKEN} was refused ROBOT_UNAVAILABLE "
                f"while it was latched; the clear (fence, then EstopClear at leader term "
                f"{_LEADER_TERM}) drew its EstopClearAck; work at token {_LATCH_LIFTED_TOKEN} "
                f"was then accepted"))

    def _latch_probe(self, action_id: str, token: int):
        """C5.8's assignment: the served action, to the first fixture pose. None: no answer."""
        cid = self._send_command(assign_action=self._assign(
            action_id=action_id, assignment_id=f"a-{action_id}", fencing_token=token))
        try:
            return self._await_result(cid).assign_action
        except (EOFError, queue.Empty):
            return None

    def _drive_for_cadence(self) -> bool:
        """Put the robot in motion for the C2.3 cadence window. True if it accepted.

        _CADENCE_TOKEN is above BOTH the run's own high-water mark (19, set by C17.4) and the
        _AUTHORITATIVE_TOKEN the reconnect RegisterAck declared, so a conformant adapter
        accepts it whether or not it adopted; C3.1 later probes with 6 and is unaffected.
        It was 15 — above the mark but BELOW the authoritative token — which an adapter that
        correctly adopts now refuses, silently costing C2.3 its telemetry window. The goal is
        long enough to outlast three telemetry waits at their 4s ceiling.
        """
        if self._served is None or self._held_below_latch():
            return False
        cid = self._send_command(assign_action=self._assign(
            action_id="t-cadence", assignment_id="a-cadence", fencing_token=_CADENCE_TOKEN,
            needs_seconds=15.0))
        try:
            return bool(self._await_result(cid).assign_action.accepted)
        except (EOFError, queue.Empty):
            return False

    def _record_cadence(self, gaps, *, in_motion: bool) -> None:
        """Record C2.3 from the measured inter-payload gaps.

        Three outcomes, because two different facts can produce a wide gap:

        * in window — conclusive either way. The adapter adopted the advertised interval.
        * out of window with the robot MOVING — motion gating cannot explain it, so the
          adapter kept its own default and the MUST is violated.
        * out of window with the robot STATIONARY — a conformant motion-gated adapter is
          indistinguishable from one ignoring the field, so nothing is established. A skip
          says that; a FAIL would report a satisfied SHOULD as a violated MUST.
        """
        title = "Honour telemetry_interval_seconds from RegisterAck"
        if not gaps:
            self.r.add("C2.3", title, CheckStatus.SKIP,
                           detail=SkipCause.NO_TELEMETRY_IN_RUN.value)
            return
        shown = [round(g, 2) for g in gaps]
        if all(0.5 <= g <= 2.5 for g in gaps):
            self.r.add("C2.3", title, CheckStatus.PASS,
                       detail=f"inter-payload gaps {shown}s against an advertised 1s")
        elif in_motion:
            self.r.add("C2.3", title, CheckStatus.FAIL,
                       detail=(f"gaps {shown}s do not reflect the advertised 1s interval "
                               f"while the robot was under an accepted assignment, so the "
                               f"motion-gating SHOULD does not explain them; the adapter "
                               f"kept its own default"))
        else:
            why = (f"the connection negotiated contract {_negotiate(self._reported_contract)}, "
                   f"which is sent no AssignAction" if self._held_below_latch() else
                   "the assignment was refused")
            self.r.add("C2.3", title, CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value
                               + f": gaps {shown}s exceed the advertised 1s, but the robot "
                                 f"was not driven ({why}), and the "
                                 f"motion-gated position telemetry SHOULD permits exactly "
                                 f"this cadence for a stationary robot. The two are "
                                 f"indistinguishable on the wire, so nothing is established"))

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
                    "version-refusal path was not exercised (expected for an adapter that does "
                    "not reconnect); the control-plane half is asserted in "
                    "internal/controlstream/server_contract_gate_test.go"))
            self._record_durability_unreached(
                f"adapter did not redial the ControlStream within {_RECONNECT_WAIT}s")
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
            self._record_durability_unreached(
                "adapter reconnected but sent no second AdapterHello")
            return

        # Accept the CONNECTION but negotiate no contract version — the control plane's real
        # behaviour for an incompatible adapter (ADR-0032: refuse work, never refuse observation).
        self.s.control_out.put(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
            accepted=True, negotiated_protocol_version="fleet_adapter.v1",
            message=("connected, but robot registration is refused: "
                     "contract version not supported"))))

        # Refuse whatever registration it attempts, with the enum a real control plane uses.
        refused = self._refuse_registration_version_mismatch()

        # C3.1 and C13.2 run BEFORE C14.1's stop, because that stop cannot be cleared: this
        # session negotiated no contract version, and the control plane sends no EstopClear to
        # a connection below 1.4.0 (RFC-0001 #fleet-adapter-protocol-the-stop-latch, *Version*).
        # Graded after it, an adapter that keeps the latch refuses their assignments
        # ROBOT_UNAVAILABLE, as the latch requires, and C3.1 read that as a lost mark.
        #
        # C3.1 — fencing-token DURABILITY. The stream was faulted and redialled above, so this is
        # a genuine across-reconnect test: a token at or below the pre-drop high-water mark (13,
        # set during C4/C16) must still be refused STALE. An adapter that kept the mark only in
        # the connection's memory accepts it here, and two robots can then execute the same task
        # after a control-plane failover — the hazard the persisted generation store exists for.
        if self._held_below_latch():
            # The mark C3.1 probes is raised by assignments the adapter accepted before the
            # reconnect, and a connection held to 1.3.x was sent none.
            self.r.add("C3.1", "Highest accepted fencing token survives a reconnect",
                       CheckStatus.SKIP, detail=self._held_detail())
        elif self._served is None:
            self.r.add("C3.1", "Highest accepted fencing token survives a reconnect",
                       CheckStatus.SKIP,
                       detail=(SkipCause.ADAPTER_DECLARES_NO_ACTION.value
                               + ": no declared action type to carry the durability probe"))
        else:
            cid = self._send_command(assign_action=self._assign(
                action_id="t-persist", assignment_id="a-persist", fencing_token=6))
            try:
                res = self._await_result(cid).assign_action
                stale = (not res.accepted
                         and res.rejection == pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN)
                self.r.add("C3.1", "Highest accepted fencing token survives a reconnect",
                           CheckStatus.PASS if stale else CheckStatus.FAIL,
                           detail=("token 6 after the reconnect, below the pre-reconnect "
                                   "high-water mark: rejection STALE_FENCING_TOKEN" if stale else
                               f"a token below the pre-reconnect high-water mark was not refused "
                               f"STALE (accepted={res.accepted} "
                               f"rejection={res.rejection}); the mark "
                               f"did not survive the reconnect"
                               + self._unlifted_note(res.rejection)))
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
        self._record_restart_unverified()
        self._check_no_work_after_refusal(refused)  # C13.2

        # C14.1 — the SafetyStream was deliberately left up. An adapter that lost the version gate
        # must still stop: this is the property that makes refusing work acceptable at all.
        self._check_estop_after_refusal()

    def _check_no_work_after_refusal(self, refused: bool) -> None:
        """C13.2 (SHOULD) — no work is accepted on a session whose registration was refused."""
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
        #
        # The probe is one the adapter would ACCEPT on a session that registered: a type it
        # declared, with every parameter that type declares required, at a fresh token. So a
        # refusal can only be the refused registration speaking. The probe used to carry no
        # type and no target, and an adapter that validates its payload refused it for missing
        # coordinates and went green on a property it did not have.
        if self._served is None:
            self.r.add("C13.2", "No work accepted after a VERSION_MISMATCH registration refusal",
                       CheckStatus.SKIP, level="SHOULD", detail=(
                           SkipCause.ADAPTER_DECLARES_NO_ACTION.value
                           + ": no declared action type to carry an otherwise acceptable probe"))
            return
        cid = self._send_command(assign_action=self._assign(
            action_id="t-refused", assignment_id="a-refused", fencing_token=99))
        accepted = None
        rejection = None
        try:
            res = self._await_result(cid).assign_action
            accepted = res.accepted
            rejection = res.rejection
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
                   detail=((f"declined a {self._served.action_type} assignment it would "
                            f"otherwise accept (rejection={rejection}) after a VERSION_MISMATCH "
                            f"refusal" if accepted is False else
                            "answered nothing to an otherwise acceptable assignment after a "
                            "VERSION_MISMATCH refusal")
                           if ok else
                           "adapter ACCEPTED an AssignAction after its registration was refused "
                           "VERSION_MISMATCH. Not blocking: C13.2 is a SHOULD, not a MUST, in the "
                           "contract, and a real control plane withholds the dispatch itself (the "
                           "assignment gate). Recorded as defence-in-depth that is absent"))

    def _record_durability_unreached(self, why: str) -> None:
        """C3.1 and C3.7 for a run whose refused-session reconnect never happened.

        C3.1 is graded on the redialled session, so the adapter's cause is recorded. C3.7 is
        a genuine suite gap and is recorded as one here exactly as on the full path.
        """
        self.r.add("C3.1", "Highest accepted fencing token survives a reconnect",
                   CheckStatus.SKIP,
                   detail=SkipCause.ADAPTER_DID_NOT_REACH_STATE.value + ": " + why)
        self._record_restart_unverified()

    def _record_restart_unverified(self) -> None:
        self.r.add("C3.7", "Highest accepted fencing token survives an adapter RESTART",
                   CheckStatus.SKIP,
                   detail=(SkipCause.NEEDS_ADAPTER_RESTART.value
                           + ": the suite spawns the adapter once and never restarts it, so a "
                             "passing report attests NOTHING about durability across a restart"))

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
        """C14.1 — an estop is honoured on the session whose registration was refused.

        The answer is read by its arm and its stop ID, as C5's is: reports of latched stops
        and of the local latch can wait on SafetyStream ahead of it (RFC-0001
        #fleet-adapter-protocol-the-stop-latch, *Report*).
        """
        self.s.safety_out.put(pb.ControlPlaneSafetyMessage(
            robot_id=ROBOT,
            estop=pb.Estop(estop_id="e-refused", reason="version-gate invariance",
                           issued_by="harness")))
        try:
            ack, passed = self._recv_estop_ack("e-refused")
        except EOFError:
            ack, passed = None, ["the SafetyStream closed"]
        if ack is None:
            self.r.add("C14.1", "Estop honored against an adapter that failed the version gate",
                       CheckStatus.FAIL, detail=(
                           "no EstopAck for 'e-refused' after a VERSION_MISMATCH refusal"
                           + (f" (received {passed})" if passed else "")
                           + " — estop MUST be version-invariant (ADR-0032)"))
            return
        # The first ack may be STOPPING (see C5.2); estop version-invariance is about the
        # CONFIRMED stop, so wait for the terminal one rather than failing on the first.
        if ack.state == pb.ESTOP_STATE_STOPPING:
            term, _saw, _w = self._recv_terminal_estop_ack("e-refused")
            if term is not None:
                ack = term
        ok = ack.estop_id == "e-refused" and ack.state == pb.ESTOP_STATE_STOPPED
        self.r.add("C14.1", "Estop honored against an adapter that failed the version gate",
                   CheckStatus.PASS if ok else CheckStatus.FAIL,
                   detail=("EstopAck for 'e-refused' reached STOPPED after the "
                           "VERSION_MISMATCH refusal" if ok else
                           f"estop_id={ack.estop_id!r} state={ack.state}"))

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
        # PRESENT and IN RANGE, not equal to the suite constant. RFC-0001 states what this
        # field is for: "the control plane copies it to
        # FleetAdapter.status.conformanceContractVersion and refuses assignment when it is
        # absent or out of range". An N-1 adapter earns a real result at its own version, and
        # requiring equality with the suite's version would have forced every report to
        # overstate the adapter it describes.
        in_range, why = _supports_contract(stamped)
        self.r.add("C15.1", "Report carries the contract version the result was earned against",
                   CheckStatus.PASS if in_range else CheckStatus.FAIL,
                   detail=(f"contract_version={stamped} (suite attests {CONTRACT_VERSION}; "
                           f"recorded beside it as suiteRevision, never folded into it)"
                           if in_range else f"report stamped {stamped!r}: {why}"))

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

class RunHandle:
    """A handle on a run's server, so it can be stopped from outside the run's own thread.

    ``run_conformance`` stops its server on every path it takes itself. It cannot stop it on
    the one path it never reaches: a run abandoned at the caller's deadline leaves the
    driver thread where it stands, so nothing in that thread runs again. The caller holds
    this and stops the server itself, which is what makes the deadline bound the whole
    process rather than just the checks.

    ``stop`` is safe from any thread and safe to call more than once.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._session: _Session | None = None
        self._server = None
        self._pool: futures.ThreadPoolExecutor | None = None
        self._stopped = False

    def _serving(self, session: _Session, server, pool: futures.ThreadPoolExecutor) -> None:
        with self._lock:
            self._session, self._server, self._pool = session, server, pool

    def stop(self, grace: float = _SERVER_STOP_GRACE) -> None:
        """Release the streams, stop the server, and let the worker threads finish."""
        with self._lock:
            if self._stopped:
                return
            self._stopped = True
            session, server, pool = self._session, self._server, self._pool
        if session is not None:
            session.close_streams()
        if server is not None:
            # stop() returns at once and terminates in the background; wait for it, bounded,
            # so the worker pool is idle before the pool itself is shut down.
            server.stop(grace=grace).wait(timeout=grace + _SERVER_STOP_GRACE)
        if pool is not None:
            pool.shutdown(wait=False, cancel_futures=True)


def run_conformance(adapter_name: str, port: int, ready: threading.Event,
                    witness_path: str | None = None,
                    handle: RunHandle | None = None) -> Report:
    """Start the server, signal readiness, run the driver, return the report.

    The caller starts the adapter-under-test (pointing it at ``port``) after
    ``ready`` is set. It may pass a ``RunHandle`` to be able to stop the server
    itself, which is the only way to end a run abandoned at a deadline.
    """
    report = Report(adapter=adapter_name)
    session = _Session()
    # The pool is named and held rather than left anonymous: its threads are not daemon
    # threads, so an unshut pool is what a stalled run is made of.
    pool = futures.ThreadPoolExecutor(max_workers=8, thread_name_prefix="harness-rpc")
    # SO_REUSEPORT OFF. gRPC turns it on by default, and with it a second run binds a port
    # this one holds without any error. Its adapter then dials THIS server (the first
    # listener takes every connection on macOS), and the two runs grade each other's
    # adapters. The second bind now fails at once instead.
    server = grpc.server(pool, options=[("grpc.so_reuseport", 0)])
    pb_grpc.add_FleetAdapterServiceServicer_to_server(_make_servicer(session), server)
    # The EdgeService (C8) is served on the same port; advertised via RegisterAck.
    pb_grpc.add_EdgeServiceServicer_to_server(_make_edge_servicer(session), server)
    try:
        bound = server.add_insecure_port(f"[::]:{port}")  # mTLS is C1.3; see README note.
    except RuntimeError:
        bound = 0
    if not bound:
        pool.shutdown(wait=False)
        print(f"harness: cannot listen on port {port}: it is already in use, most often by "
              f"another conformance run. Give this run its own port (--port, or "
              f"CONFORMANCE_PORT=... for the make targets).", file=sys.stderr, flush=True)
        raise RuntimeError(f"port {port} is already in use")
    server.start()
    handle = handle or RunHandle()
    handle._serving(session, server, pool)
    ready.set()
    try:
        _Driver(session, report, port, witness_path=witness_path).run()
    except (EOFError, queue.Empty) as exc:
        report.add("run", "Harness completed the scenario", CheckStatus.FAIL,
                   detail=f"aborted: {exc}")
    finally:
        # Pass, fail, exception alike: the server stops here. A run that raised something
        # this does not catch still leaves through this block.
        handle.stop()
    return report
