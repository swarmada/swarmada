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

"""Reference simulation Fleet Adapter.

A feature-basic but **safety-complete** adapter (ADR-0005) that drives a simulated
robot instead of real hardware. It registers a robot, streams live telemetry, and
implements the safety MUSTs — fencing-token ordering (C3), assignment-lease
self-stop (C4), and confirmed emergency stop (C5) — using the pure logic in
``safety.py`` against a pluggable ``Simulator`` (``KinematicSim`` by default, at
$0 with no external simulator or licence). The zone-capacity hold/admit command
(``zone_admission``, RFC-0001 #control-plane-zone-controller) is honoured — a hold
really stops the robot, since the control plane cannot; the remaining optional
commands are declined with
``unsupported=true`` (C7). If a zone advertises an edge node it dials the
EdgeStream and honours edge-issued estops with the same confirmed discipline (C8).

With ``--latch`` it keeps the stop latch of contract 1.4.0 (RFC-0001
#fleet-adapter-protocol-the-stop-latch) and reports 1.4.0; without it, it reports 1.3.0,
which the latch does not bind. ``CONTRACT_VERSION`` below says why the default is 1.3.0.

Run at $0:  python3 adapters/simulation/sim_adapter.py --endpoint localhost:9090
Validate:   make conformance-sim
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import queue
import re
import secrets
import signal
import sys
import threading
import time
from collections.abc import Iterator

import grpc

# Make the in-tree packages importable when this file is run directly as a script
# (the conformance harness spawns it with only proto/ on PYTHONPATH). A real
# deployment pip-installs the adapter instead of relying on this.
_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(_REPO_ROOT))
# The safety primitives come from the SDK, not from a copy beside this file. This
# adapter is the REFERENCE adapter: ADR-0005 makes it the thing every vendor adapter
# is read against, and `swarmada_sdk/__init__.py` states these primitives exist "so
# every adapter -- reference and vendor -- shares one audited implementation". A local
# copy froze before `FenceGuard.adopt()` was added and C3.8 failed here while passing
# for anything generated from adapters/template/, which has always imported the SDK.
# The SDK is a separate distribution (sdk/python/pyproject.toml) and is not part of
# the root editable install, so put its source root on the path the same way the repo
# root goes on above -- the conformance harness spawns this file with only proto/ on
# PYTHONPATH. A real deployment pip-installs both instead of relying on either line.
sys.path.insert(1, str(_REPO_ROOT / "sdk" / "python" / "src"))

from swarmada_sdk.safety import (  # noqa: E402  (after the sys.path bootstrap)
    ESTOP_STOPPED,
    FenceDecision,
    FenceGuard,
    LeaseMonitor,
    confirm_estop,
)

from adapters.simulation import floor_mapping  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

_TELEMETRY_INTERVAL = 0.2  # seconds between telemetry payloads
_TICK_DT = 0.2             # sim advance per loop pass, in step with wall-clock time
# How close to its target a robot at rest must be for the action to count as arrived. The
# kinematic sim snaps the pose onto the target on arrival, so this only absorbs float noise;
# a robot brought to rest short of the target (estop, pause, zone hold, lease self-stop)
# is metres away and never completes.
_ARRIVAL_TOLERANCE_M = 1e-6

# The action catalog this reference sim advertises (RFC-0001 §9.2 SupportedAction) and
# the ONE source for what it accepts: `validate_action` and `assign_action` both judge an
# action against this table, so the catalog cannot say one thing and the robot do another.
# (type, required_capabilities, moves). A type that moves reads its target from the payload
# as optional `x` / `y` in metres; with none, it drives to _NOMINAL_TARGET. `Charge`, and
# `Custom` explore-floor (named by custom_type, or by the payload's `operation`), move nothing
# and end on their own
# (adapters/simulation/floor_mapping.py). The coordinates
# are declared OPTIONAL because that is what this sim does: it runs a Navigate with no target.
# That is honest, and it is also why the sim cannot establish the conformance row for
# refusing an assignment missing a required parameter: it declares none.
_SUPPORTED_ACTIONS = [
    ("Navigate", ["navigation.2d"], True),
    ("PickUp",   ["navigation.2d", "transport.payload"], True),
    ("DropOff",  ["navigation.2d", "transport.payload"], True),
    ("Patrol",   ["navigation.2d"], True),
    ("Charge",   [], False),
    ("Custom",   [], False),
]
_NOMINAL_TARGET = (10.0, 0.0)

# The provisioning loop's interval I when the DiscoverAck carries no TTL (RFC-0001
# #fleet-adapter-protocol-connection-lifecycle); the reference control plane sends none.
_PROVISIONING_INTERVAL_S = 60.0


def _provisioning_interval(ttl_seconds: int, override_s: float | None) -> float:
    """The provisioning loop's interval I: min(ttl_seconds/2, 300), or 60 s when the TTL
    is disabled (0). A deployment may set a shorter I (--provisioning-interval); it never
    exceeds TTL/2, or the DiscoveredRobot would expire while the robot is connected."""
    rule = min(ttl_seconds / 2.0, 300.0) if ttl_seconds > 0 else _PROVISIONING_INTERVAL_S
    if override_s is None:
        return rule
    return min(override_s, rule) if ttl_seconds > 0 else override_s


def _positive_seconds(text: str) -> float:
    try:
        value = float(text)
    except ValueError:
        value = 0.0
    if not value > 0:
        raise argparse.ArgumentTypeError("must be a positive number of seconds")
    return value


def _start_timer(delay_s: float, fn) -> None:
    timer = threading.Timer(delay_s, fn)
    timer.daemon = True
    timer.start()


# Sensor streams this simulated robot declares on CapabilitiesSnapshot.sensor_streams,
# which the registrar projects read-only to Robot.status.sensorStreams[] and the
# scheduler's SensorStreamNotOffered gate reads before committing any robot.
#
# (stream_id, media_type, native_rate_hz, required_capabilities, always_on,
#  supports_clip, clip_pre_seconds_max)
#
# The set is deliberately mixed rather than uniform, because a uniform set makes three
# distinct rules untestable at once: `front-rgb` is ON_DEMAND and clip-capable (the
# duty cycle has something real to switch and a pre-roll to bound), `mic-array` is
# ON_DEMAND without clip support (so a CLIP request against it MUST be refused rather
# than silently served), and `odom-pose` is ALWAYS_ON (activation costs nothing there,
# and an implementation that skipped activation for cheap streams would be caught by
# the refcount rules rather than passing unnoticed).
_SENSOR_STREAMS = [
    ("front-rgb",  "image/jpeg",                      15.0, ["navigation.2d"], False, True,  10),
    ("mic-array",  "audio/l16",                       16000.0, [],             False, False, 0),
    ("odom-pose",  "application/vnd.swarmada.pose+json", 20.0, [],             True,  False, 0),
]

# How often the stream-lease watchdog checks for expiry. It is NOT the telemetry
# interval (1 s), and that is the point: the interval an operator is promised is
# "the lease duration", and a 1 s poll would add up to a second of camera-on time to
# every partition. 20 ms bounds the overshoot to something smaller than the jitter of
# the network the renewal would have arrived over.
_STREAM_LEASE_TICK_S = 0.02


def _demo_estop_ack_delay() -> None:
    """DEMO/TEST ONLY: sleep ``SWARMADA_SIM_ESTOP_ACK_DELAY_MS`` before EMITTING a
    control-plane estop ack, so examples/full-surface-demo can drive the RFC-0001
    metrics-contract estop-latency SLO-breach alert (ACK latency > 500ms). Unset/0 — the
    default, and every real deployment — is a no-op. The delay is applied to the ACK
    only, AFTER the confirmed physical stop (C5), so the safe-hold itself is never
    delayed; only the acknowledgement the control plane times is."""
    ms = os.environ.get("SWARMADA_SIM_ESTOP_ACK_DELAY_MS")
    if not ms:
        return
    try:
        time.sleep(float(ms) / 1000.0)
    except ValueError:
        pass


# The fleet-adapter CONTRACT version this adapter implements (ADR-0032). Distinct from
# adapter_version (this build) and protocol_version (the wire package identity, not a semver, which
# is why it cannot express compatibility). A control plane refuses REGISTRATION for an adapter that
# reports nothing or reports out of range, so this is not optional -- telemetry, heartbeat and
# emergency stop keep working either way. Bump it only when this adapter is re-qualified against a
# new contract (`make conformance`), and update the adapters/REGISTRY.md row with it.
#
# AT N WITH --latch, N-1 WITHOUT (below). The control plane implements 1.4.0 and accepts
# ">=1.3.0 <1.5.0" (N and N-1 minor, ADR-0032). A later minor release moves that
# window, and it is not widened. The N-1 half is exercised in-tree by this adapter without --latch
# and by adapters/example-noop, which declares 1.3.0.
#
# It moved 1.0.0 -> 1.1.0 when the contract moved 1.1.0 -> 1.2.0, and that move was NOT
# cosmetic. An N/N-1 window is two minors wide: at contract 1.2.0 a 1.0.0 adapter was out of
# range, as a 1.2.0 adapter is at 1.4.0, and an adapter out of range is refused
# RegisterRobot/DiscoverRobot with
# REGISTRATION_REJECTION_VERSION_MISMATCH -- connected, streaming telemetry, still receiving
# estop, and unable to bring a single robot under management. ADR-0032 Amendment 1 (A1.2)
# records that the ADR's own "minor releases never break an adapter" is false as written, and
# that this is the shape of the break.
#
# Every change since 1.0.0 was additive, so an adapter that ignores what it does not know speaks
# them by construction; what needs code is what each minor obliges an adapter to DO:
#
#   1.2.0  the FA-R5/FA-R6 DECLINE below: it honours no operating subject and declares none
#          (honoured_operating_constraints stays empty, which FA-R1 says is not a breach), so
#          every operating rule that reaches it is refused rather than accepted and ignored.
#   1.3.0  the zone_constraints push (FA-R7), an OPTIONAL Command: declined unsupported=true
#          through the same branch as every other optional command, which is conformant.
#   1.4.0  one catalog entry per Custom verb (SupportedAction.custom_type), and the verb read
#          from AssignAction/ValidateAction.custom_type before the payload's `operation`, with
#          a verb it does not serve, or one the payload contradicts, refused INVALID_ACTION;
#          and, with --command-derived-plant, CANCEL_DISPOSITION_STOP_UNCONFIRMED on a cancel to a
#          control plane that negotiated 1.4.0 (acknowledged=false to an older one);
#          and the stop latch (RFC-0001 #fleet-adapter-protocol-the-stop-latch): a stop
#          latches its ID, work is refused ROBOT_UNAVAILABLE until the stop's EstopClear
#          lifts it, and so on, as the latch section of SimAdapter states.
#
# 1.3.0 BY DEFAULT, 1.4.0 WITH --latch. A 1.4.0 adapter that keeps no latch is not a 1.4.0
# adapter, so the claim follows the latch. The latch is off by default because every
# deployment in this repository runs this adapter against the reference control plane, which
# at v0.4 reads no report of the local latch, so it never sends the EstopClear that ends a
# local set (RFC-0001, safety, the edge-stop report's Not-met note). A latching adapter that
# took an edge stop there refuses all work until it restarts. Until the control plane ends a
# local set, the deployments run this adapter at 1.3.0, which the latch does not bind.
# `make conformance-sim` runs it with --latch, so the suite attests the 1.4.0 adapter.
#
# The N-1 skew is exercised by adapters/example-noop, which declares 1.3.0.
CONTRACT_VERSION = "1.3.0"
# What the adapter reports with --latch.
LATCH_CONTRACT_VERSION = "1.4.0"

# The operating subjects this adapter can honour while a robot is in a zone that restricts
# one (FA-R1). EMPTY, and that is a conformant answer, not a gap: FA-R1 states that an
# adapter declaring none is not in breach -- its robots are simply withheld from zones
# restricting a subject, by candidate filter 12. The simulation models motion and hardware
# health; it models no camera shutter, no water pump and no UV lamp, so there is no subject
# it could honestly claim. Claiming one it does not enforce would be a FALSE ATTESTATION,
# which ADR-0058 makes both a conformance failure and a security event.
_HONOURED_SUBJECTS: frozenset[str] = frozenset()


def _at_least(version: str, major: int, minor: int) -> bool:
    """Whether a negotiated contract version is major.minor or later (same major)."""
    parts = version.split(".")
    try:
        got_major, got_minor = int(parts[0]), int(parts[1])
    except (IndexError, ValueError):
        return False
    return got_major == major and got_minor >= minor


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


# The Command arms that act on the robot (its motion, its lease, a hold, an install or a
# stream) or vouch that it can take work (validate_action). Each is refused when
# Command.robot_id is not the robot this adapter serves (`_refuse_unserved`). `scan` and the
# three `verify_*` arms are not listed: they inspect and move nothing, and a scan names its
# own robot in CapabilitiesSnapshot.robot_id. Arms this adapter declines (`unsupported`) are
# declined whatever robot they name.
_ROBOT_ACTING_ARMS = frozenset({
    "assign_action", "renew_lease", "cancel_action", "pause", "resume", "zone_admission",
    "validate_action", "activate_stream", "deactivate_stream", "push_firmware", "model_update",
})


# Attributes carried on HardwareComponent (fleet_adapter.v1 tags 6-13). Listed once so the two
# emission sites cannot drift.
_HW_ATTRS = (
    "max_payload_kg", "resolution_mp", "range_m", "horizontal_fov_deg",
    "depth_capable", "frame_rate_fps", "platform_length_mm", "platform_width_mm",
)


def _hw_component(pb, hs, status_enum):
    """Build a HardwareComponent, OMITTING every attribute the manifest leaves unset.

    proto3 explicit presence: assigning None raises, and assigning 0.0 would put a measured zero
    on the wire. An attribute this robot does not know about must be ABSENT, so the control plane
    records it as unknown rather than as a 0 kg payload ceiling or a 0 m sensing range.
    """
    kw = {
        "name": hs.name, "type": hs.type, "model": hs.model,
        "status": status_enum, "degradation_reason": hs.reason,
    }
    for f in _HW_ATTRS:
        v = getattr(hs, f, None)
        if v is not None:
            kw[f] = v
    return pb.HardwareComponent(**kw)


class SimAdapter:
    """Speaks ``fleet_adapter.v1`` backed by a Simulator."""

    def __init__(self, pb, pb_grpc, sim, robot_id: str, zone: str, vendor: str,
                 engine=None, namespace: str = "default", model_artifacts=None,
                 artifact_dir: str | None = None, artifact_store: str = "sim-scratch",
                 provisioning_interval_s: float | None = None,
                 command_derived_plant: bool = False, latch: bool = False) -> None:
        self._pb = pb
        # The stop latch (RFC-0001 #fleet-adapter-protocol-the-stop-latch), kept with --latch
        # only, and the contract version that follows from it (CONTRACT_VERSION, above).
        self._latching = latch
        self._contract_version = LATCH_CONTRACT_VERSION if latch else CONTRACT_VERSION
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
        # A simulated plant with no motion channel independent of the command path (ADR-0071):
        # a cancel is commanded and its rest is not witnessed, so the cancel answer is
        # CANCEL_DISPOSITION_STOP_UNCONFIRMED (to a control plane at 1.4.0 or later) instead of
        # STOPPED_SAFELY. Off by default; only the cancel answer changes.
        self._command_derived_plant = command_derived_plant
        # HelloAck.negotiated_contract_version of the current session: which answers the
        # control plane can read, and whether the leader-term rule binds (_latch_command).
        self._negotiated_contract = ""
        self._pb_grpc = pb_grpc
        self._sim = sim
        self._sim_lock = threading.Lock()  # serialises all simulator access
        self._robot = robot_id
        self._zone = zone
        self._vendor = vendor
        # The namespace self-reported in AdapterHello, which Registrar.Register
        # uses verbatim to look up the admitted Robot (internal/registrar/
        # registrar.go). This MUST match the Robot's real namespace or every
        # Register call fails NOT_ADMITTED even though the robot exists — the
        # conformance harness's stub server doesn't care, but a real control
        # plane does. Defaults to "default" to preserve prior behavior for
        # anyone driving this against the conformance harness.
        self._namespace = namespace

        # Scenario engine (proto-free): decides hardware status over time. None ⇒
        # no scenario-driven hardware reporting (pure safety/telemetry as before).
        self._engine = engine
        self._hw_prev: dict[str, str] = {}   # component → last-reported status (delta base)
        # Install state the robot reports about ITSELF (ADR-0033). Held here because the
        # snapshot must be able to answer after the stream that carried the terminal outcome
        # is gone — that recovery path is the whole reason the snapshot carries it.
        self._firmware_version = ""          # "" until this sim has been flashed
        self._firmware_failure = ""          # reason for the last failed flash, if any
        self._firmware_attempted = ""        # the version that failed
        self._model_versions: dict[str, str] = {}   # model name → running version
        # SEEDED from the scenario, because this dict — not the manifest — is what every
        # report reads. Leaving it empty made a robot that boots with models declared in its
        # preset report NONE of them, in the scan reply (C12.1) as well as the snapshot.
        if engine is not None:
            for _m in engine.scenario.installed_models:
                self._model_versions[_m.name] = _m.version
        # Real-artifact mode (ModelArtifacts, below): None keeps the stand-in verification.
        self._model_artifacts = model_artifacts
        self._model_failures: dict[str, str] = {}   # model name → why its last install failed
        self._loaded_models: dict = {}              # model name → the running LoadedModel
        self._resident_models: dict = {}            # (name, version) → LoadedModel, for rollback
        self._after_ack: list = []                  # install work to start once the ack is queued
        self._telemetry_start = 0.0          # set when the telemetry loop begins
        self._estop_drilled = False          # estop-drill fires exactly once
        self._reconnect_pending = False      # comms-flaky: a stream drop is in progress
        self._active_call = None             # live ControlStream call, for cancel-on-drop

        self._fence = FenceGuard()                              # C3
        self._lease = LeaseMonitor(on_expiry=self._self_stop)   # C4
        # The sensor-stream duty cycle (C18) reuses the SAME lease primitive the
        # assignment lease already uses and C4 already verifies, keyed
        # "<robot_id>:<stream_id>" instead of by robot. That is the whole design claim:
        # a stream does not get a second, unproven self-stop implementation, and the
        # safety-reviewed code path is exercised unchanged.
        #
        # A SEPARATE monitor, not the same one, because the two leases point in
        # opposite directions on expiry — the assignment lease stops the ROBOT, the
        # stream lease stops a SENSOR — and sharing one would make a stream expiry
        # capable of reporting an action stopped.
        self._stream_lease = LeaseMonitor(on_expiry=self._stream_lease_expired)
        self._active_streams: dict = {}   # stream_id -> {"rate_hz", "window", "sinks"}
        self._stream_lock = threading.Lock()
        # Stream stops, newest last, as (stream_id, reason, wall_clock_seconds). The
        # measurement reads this back; the wire has no "the stream stopped" message and
        # this adapter does not invent one (see _stream_lease_expired).
        self._stream_stops: list = []
        self._current_action = ""
        self._current_generation = 0
        self._lease_action = ""
        # Where the sim was sent for _current_action (None while it is not driving toward
        # one: held, a non-motion action, or no action), the target the assignment named,
        # and the fencing token it was accepted under. Arrival at _move_target is the
        # action's completion; see _complete_on_arrival.
        self._move_target = None
        self._assigned_target = None
        self._action_fencing_token = 0
        self._snapshot_pending = True
        # C2.1 onboarding state. _admitted records that a Register was ACCEPTED, which is
        # the only evidence this robot is under management. While it is not, the
        # provisioning loop (RFC-0001 #fleet-adapter-protocol-connection-lifecycle) re-sends
        # register every _provisioning_interval_s, because nothing tells an adapter its
        # robot was admitted. _catalog_unseen marks that the snapshot sent at hello time
        # reached a control plane with no Robot for this id, which dropped it.
        # _provisioning_override is --provisioning-interval: None follows the RFC's rule.
        self._admitted = False
        self._catalog_unseen = False
        self._provisioning_override = provisioning_interval_s
        self._provisioning_interval_s = _provisioning_interval(0, provisioning_interval_s)
        self._schedule = _start_timer   # (delay_s, fn); replaced in tests
        self._telemetry_interval = _TELEMETRY_INTERVAL
        # Zone-capacity hold (RFC-0001 #control-plane-zone-controller): the zone name the
        # control plane is holding this robot out of via zone_admission(admit=false), or ""
        # when free to proceed. A hold suppresses motion —
        # honouring it is the whole enforcement mechanism; the control plane cannot stop the robot.
        self._zone_hold = ""
        # A non-motion action that ends on its own (Charge, Custom explore-floor), advanced
        # from the telemetry loop; None when the current action is not one. What a resumed
        # explore segment reloads is kept per bound, and the map is saved under the
        # artifact directory, created on the first segment that saves one.
        self._job = None
        self._explore_states: dict = {}
        self._artifact_dir = artifact_dir
        self._artifact_store = artifact_store
        self._artifacts = None

        self._outbox: queue.Queue = queue.Queue()   # → ControlStream
        self._safety_outbox: queue.Queue = queue.Queue()  # → SafetyStream (acks + drills)
        self._edge_addr: queue.Queue = queue.Queue()  # advertised edge endpoints
        self._shutdown = threading.Event()
        self._telemetry_started = False

    def shutdown(self) -> None:
        """Signal every loop to exit and unblock the two stream request generators, so
        a SIGTERM/SIGINT (or the quickstart's teardown) closes the ControlStream and
        SafetyStream cleanly instead of surfacing as an `UNAVAILABLE` drop + reconnect
        in the adapter log. Idempotent and signal-safe: it only sets an Event, enqueues
        the None sentinels, and cancels the in-flight call."""
        self._shutdown.set()
        # The None sentinel unblocks each request generator's queue .get(); the stream
        # then half-closes (CloseSend) and the server ends the response iterator cleanly.
        self._outbox.put(None)
        self._safety_outbox.put(None)
        # Nudge any in-flight ControlStream response iterator to return promptly; the
        # loops all check _shutdown first, so the resulting CANCELLED is not logged.
        call = self._active_call
        if call is not None:
            try:
                call.cancel()
            except Exception:  # noqa: BLE001 — best-effort during shutdown
                pass

    # ── ControlStream ────────────────────────────────────────────────────────

    def _control_requests(self) -> Iterator:
        """Outbound messages for ONE ControlStream session.

        The queue is captured per session rather than read from `self._outbox` on every
        iteration. A dying generator is not stopped the moment its stream faults — it can
        still be blocked in get() — and with a shared queue it consumes the RegisterRobot
        that the NEW session's hello_ack just enqueued, yielding it into a dead stream.
        The control plane then never sees a re-registration, which
        RFC-0001 #fleet-adapter-protocol-connection-lifecycle (b) requires and which the whole
        reconnect reconciliation
        (RFC-0001 #fleet-adapter-protocol-reconnect-action-state-reconciliation) is built on.
        Binding the queue here makes each session's traffic its own.
        """
        pb = self._pb
        outbox = self._outbox
        yield pb.AdapterMessage(hello=pb.AdapterHello(  # C1.2: hello first
            vendor=self._vendor, adapter_version="0.1.0-sim",
            protocol_version="fleet_adapter.v1", contract_version=self._contract_version,
            namespace=self._namespace))
        while True:
            msg = outbox.get()
            if msg is None:
                return
            yield msg

    def run_control_stream(self, stub) -> None:
        with self._sim_lock:
            self._sim.spawn(self._robot, 0.0, 0.0)
        # Telemetry starts only AFTER registration (below) — a robot streams
        # telemetry once the control plane knows it. The edge loop blocks until an
        # endpoint is advertised, so it is safe to start now.
        threading.Thread(target=self._edge_loop, daemon=True).start()
        # The stream-lease watchdog runs for the life of the adapter, independent of
        # the ControlStream. That independence IS the fail-safe: a stream must stop on
        # lease expiry whether the control plane is silent, partitioned or gone, and a
        # watchdog hosted inside the stream loop would die with the thing it guards
        # against.
        threading.Thread(target=self._stream_lease_loop, daemon=True).start()

        drop_window = self._engine.stream_drop_window() if self._engine else None
        if drop_window is None:
            # No scenario-driven drop: one logical ControlStream session, but
            # resilient like run_safety_stream. An UNEXPECTED transport drop (a demo
            # port-forward blip or a manager restart) raises RpcError from the
            # response iterator; log and RE-OPEN with a fresh Hello/Register rather
            # than crashing with a traceback. On shutdown, exit quietly; on a clean
            # server-side close, exit. The gRPC channel auto-heals once connectivity
            # returns. The conformance suite DOES fault this stream on purpose (the
            # accepting-reconnect checks C6.1/C2.2), so the re-open below is exercised.
            while not self._shutdown.is_set():
                try:
                    # Fresh outbox per session. The dying generator is not stopped the
                    # instant its stream faults — it can still be blocked in get() — and
                    # with a shared queue it swallows the RegisterRobot the NEW session
                    # enqueues on hello_ack, so the control plane never sees the
                    # re-registration
                    # RFC-0001 #fleet-adapter-protocol-connection-lifecycle (b) requires.
                    self._outbox = queue.Queue()
                    for cp in stub.ControlStream(self._control_requests()):
                        self._handle_control_plane(cp, stub)
                except grpc.RpcError as exc:
                    if self._shutdown.is_set():
                        break
                    print(f"control stream dropped ({exc.code()}); reconnecting", file=sys.stderr)
                    time.sleep(0.5)
                    continue
                break  # clean close (response iterator ended without error)
            self._shutdown.set()
            return

        # comms-flaky: reconnect across a scenario-driven stream drop. Each pass is a
        # full Hello/Register handshake, so this exercises the RegisterRobot reconnect
        # and the persisted fencing/lease state (C3/C4) across the outage.
        while not self._shutdown.is_set():
            self._reconnect_pending = False
            # Fresh outbox per session: anything still queued belonged to the stream that
            # just died, and replaying it on a new stream would be wrong anyway.
            self._outbox = queue.Queue()
            call = stub.ControlStream(self._control_requests())
            self._active_call = call
            dropped = False
            try:
                for cp in call:
                    self._handle_control_plane(cp, stub)
            except grpc.RpcError as exc:  # scenario drop OR an unexpected transport loss
                if self._shutdown.is_set():
                    self._active_call = None
                    break
                dropped = True
                print(f"control stream dropped ({exc.code()}); reconnecting", file=sys.stderr)
            finally:
                self._active_call = None
            if self._shutdown.is_set():
                break
            if self._reconnect_pending:
                # Scenario-driven drop: hold the outage open for its full window so the
                # un-renewed lease self-stops (C4), then reconnect with a fresh
                # Hello/Register (C3).
                self._await_reconnect()
                continue
            if dropped:
                # UNEXPECTED transport drop — e.g. the demo port-forward dying at script
                # exit, or a manager restart. Resilient like the single-session path and
                # run_safety_stream: brief backoff, then reconnect, rather than crashing
                # with a raw traceback. (On script exit the EXIT trap kills this adapter
                # within its grace window, so this cannot spin indefinitely.)
                time.sleep(0.5)
                continue
            break  # clean close (response iterator ended without error)
        self._shutdown.set()

    def _handle_control_plane(self, cp, stub) -> None:
        pb = self._pb
        kind = cp.WhichOneof("payload")
        if kind == "hello_ack":
            if not cp.hello_ack.accepted:
                raise RuntimeError(f"handshake rejected: {cp.hello_ack.message}")
            self._negotiated_contract = cp.hello_ack.negotiated_contract_version
            self._outbox.put(pb.AdapterMessage(  # C2.1: register the admitted robot
                register=pb.RegisterRobot(robot_id=self._robot)))
            # Surface the scenario's full hardware manifest once, up front, so the
            # control plane has the component inventory before the first fault delta.
            self._send_capabilities_snapshot()
            # Only now — registered — begin streaming telemetry (C6).
            # A fresh ControlStream: the next payload must be a full snapshot (C6.1).
            self._snapshot_pending = True
            if not self._telemetry_started:
                self._telemetry_started = True
                threading.Thread(target=self._telemetry_loop, args=(stub,), daemon=True).start()
        elif kind == "register_ack":
            # C2.1's fallback: register FIRST, and announce for admission only when the
            # control plane says this robot has no Robot object yet. Anything else that is
            # not accepted is a real refusal and is not answered by announcing — retrying as
            # a Discover would be an adapter reaching for a robot it was told it may not
            # touch.
            if not cp.register_ack.accepted:
                if cp.register_ack.rejection == pb.REGISTRATION_REJECTION_NOT_ADMITTED:
                    # One provisioning-loop iteration: announce (which keeps the
                    # DiscoveredRobot's TTL fresh), then register again after the interval.
                    self._admitted = False
                    self._catalog_unseen = True
                    self._outbox.put(pb.AdapterMessage(discover=self._discover_message()))
                    outbox = self._outbox
                    self._schedule(self._provisioning_interval_s,
                                   lambda: self._provisioning_iteration(outbox))
                    print(f"NOT_ADMITTED robot={self._robot}; announced for operator "
                          f"admission, next register in {self._provisioning_interval_s:.0f}s "
                          f"(C2.1)", file=sys.stderr)
                else:
                    if self._latching:
                        # The Stop Latch, A refused register: work held for the robot is
                        # interrupted work.
                        with self._latch_lock:
                            self._interrupt_work()
                    print(f"register refused robot={self._robot}: "
                          f"{cp.register_ack.message}", file=sys.stderr)
                return
            # Accepted: this robot IS admitted, so ordinary operation resumes.
            self._admitted = True
            if self._catalog_unseen:
                # Admission picked up by the provisioning loop: the snapshot sent at hello
                # time was dropped, so FleetAdapter.status.supportedActions has nothing yet.
                self._catalog_unseen = False
                self._send_capabilities_snapshot()
            for e in cp.register_ack.edge_endpoints:  # C8: dial each edge node
                self._edge_addr.put(e.address)
            self._reconcile_action_state(cp.register_ack)
            # C2.3 — adopt the per-robot cadence the control plane returned. Clamped to
            # the protocol's 1-30s bound; 0 means "not specified", keep the default.
            secs = cp.register_ack.telemetry_interval_seconds
            # Only a POSITIVE value is a cadence. 0 means "not specified" (the field is
            # optional and Robot.spec.telemetryIntervalSeconds is nilable), and a negative
            # value is malformed — clamping that to the 1s minimum would let a buggy
            # control plane force the fastest permitted telemetry rate, so it is ignored
            # in favour of the adapter's own default. Legitimate values are clamped to the
            # protocol's 1-30s range.
            if secs > 0:
                self._telemetry_interval = max(1.0, min(30.0, float(secs)))
        elif kind == "discover_ack":
            # Announced and staged. The robot is NOT under management yet: an operator has
            # to admit it, and the provisioning loop picks that up. The ack's TTL sets the
            # loop's interval (_provisioning_interval).
            self._provisioning_interval_s = _provisioning_interval(
                cp.discover_ack.ttl_seconds, self._provisioning_override)
            print(f"DISCOVERED robot={self._robot}: {cp.discover_ack.message}", file=sys.stderr)
        elif kind == "heartbeat":
            # Echo the request's robot_id whatever it is. An EMPTY robot_id is the
            # adapter-scoped probe ("is this adapter alive?"), which the control plane
            # pushes on a cadence of spec.heartbeatIntervalSeconds; a non-empty one is
            # the per-robot confirming exchange. Both are answered the same way, and the
            # echo is what the control plane correlates on -- so this must NOT be
            # rewritten to substitute self._robot when the field is empty.
            self._outbox.put(pb.AdapterMessage(
                heartbeat=pb.HeartbeatResponse(robot_id=cp.heartbeat.robot_id)))
        elif kind == "command":
            self._handle_command(cp.command)

    def _handle_command(self, command) -> None:
        pb = self._pb
        if self._latching and command.robot_id == self._robot and self._latch_command(command):
            return
        result = pb.CommandResult(command_id=command.command_id, robot_id=command.robot_id)
        which = command.WhichOneof("command")
        if command.robot_id != self._robot and which in _ROBOT_ACTING_ARMS:
            self._refuse_unserved(command, which, result)
            self._outbox.put(pb.AdapterMessage(command_result=result))
            return
        if which == "assign_action":
            self._on_assign(command.assign_action, result)
        elif which == "renew_lease":
            self._on_renew(command.renew_lease, result)
        elif which == "cancel_action":
            named = command.cancel_action.action_id
            if named and self._current_action and named != self._current_action:
                # A cancel for an action this robot is no longer running. It happens when
                # a higher-priority action preempts a running one in place: the robot
                # switches to the new action, and the control plane then asks it to stop
                # the old one. The old one is not executing here, so it is stopped already;
                # stopping the robot would abort the action that replaced it, which the
                # contract forbids (a stale cancel "cannot abort a newer assignment").
                result.cancel_action.CopyFrom(pb.CancelActionResult(
                    acknowledged=True,
                    disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY))
                print(f"CANCEL_NOT_RUNNING robot={self._robot} action={named} "
                      f"running={self._current_action} command_id={command.command_id}",
                      file=sys.stderr, flush=True)
                self._outbox.put(pb.AdapterMessage(command_result=result))
                return
            # Captured before the clear below: the marker and the status emit both
            # name the action being cancelled, and self._current_action is "" by then.
            cancelled_action = self._current_action
            with self._sim_lock:
                self._sim.command_stop(self._robot)
            self._lease.release(self._robot)
            self._emit_action_status(cancelled_action, "CANCELLED", "cancel_action honoured")
            self._current_action = ""
            self._move_target = None
            # The simulated robot carries no physical load, so a cancel always
            # reaches a safe stop — STOPPED_SAFELY (capability-loss reassignment
            # then requeues the task to a capable robot). A real adapter reports
            # COMPLETED or RECOVERED instead when the robot is mid-commitment.
            disposition, acknowledged = pb.CANCEL_DISPOSITION_STOPPED_SAFELY, True
            if self._command_derived_plant:
                # The stop was commanded and its rest cannot be witnessed. A control plane
                # below 1.4.0 has no value for that and reads any it does not know as a safe
                # stop, so it is not told one: the cancel is not acknowledged, and it holds the
                # robot until the lease is provably dead.
                # Both versions count: a 1.3.0 adapter's connection negotiates 1.3.0
                # (RFC-0001 #fleet-adapter-protocol-package-and-versioning), even where a
                # control plane reports 1.4.0 on every connection.
                if (_at_least(self._negotiated_contract, 1, 4)
                        and _at_least(self._contract_version, 1, 4)):
                    disposition = pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED
                else:
                    disposition, acknowledged = pb.CANCEL_DISPOSITION_UNSPECIFIED, False
            result.cancel_action.CopyFrom(pb.CancelActionResult(
                acknowledged=acknowledged,
                disposition=disposition))
            # Stable marker, emitted only after the stop, the lease release and the
            # status emit have all succeeded — so the line means "cancel honoured",
            # not "cancel received". A mishandled cancel surfaces three components
            # away, as the control plane simply not reassigning, which is equally
            # consistent with a scheduler, lease or capability fault. The disposition
            # is read back from the value that goes on the wire rather than written
            # a second time, so the log cannot disagree with the message.
            disposition_name = pb.CancelDisposition.Name(disposition).removeprefix(
                "CANCEL_DISPOSITION_")
            print(f"CANCEL_HONOURED robot={self._robot} action={cancelled_action} "
                  f"command_id={command.command_id} disposition={disposition_name}",
                  file=sys.stderr, flush=True)
        elif which in ("verify_hardware", "verify_capability", "verify_model"):
            self._on_verify(getattr(command, which), result)
        elif which == "push_firmware":
            self._on_firmware(command.push_firmware, result)
        elif which == "model_update":
            self._on_model_update(command.model_update, result)
        elif which == "pause":
            self._on_pause(command.pause, result)
        elif which == "resume":
            self._on_resume(command.resume, result)
        elif which == "scan":
            self._on_scan(result)
        elif which == "validate_action":
            self._on_validate(command.validate_action, result)
        elif which == "zone_admission":
            self._on_zone_admission(command.zone_admission, result)
        elif which == "activate_stream":
            self._on_activate_stream(command.activate_stream, result)
        elif which == "deactivate_stream":
            self._on_deactivate_stream(command.deactivate_stream, result)
        else:
            result.unsupported = True  # C7.1: decline every still-optional command
        self._outbox.put(pb.AdapterMessage(command_result=result))
        # Work that must follow the acknowledgement on the wire: the rollout records a robot as
        # updating only once its push is acknowledged, and correlates a FAILED install outcome
        # only for a robot it has recorded, so an install that is going to fail reports after
        # the ack, never before it.
        while self._after_ack:
            threading.Thread(target=self._after_ack.pop(0), daemon=True).start()

    # ── The stop latch (--latch) ─────────────────────────────────────────────────
    #
    # RFC-0001 #fleet-adapter-protocol-the-stop-latch, for a connection at 1.4.0:
    #   * a stop latches its stop ID, on whichever path it came: an Estop on SafetyStream or its
    #     fence (Command.estop) on ControlStream; an ID already cleared does not latch again;
    #   * an edge stop sets the robot's local latch under a fresh random set ID, and only a
    #     local reset at the robot lifts it (this simulation has none);
    #   * while anything is latched, every AssignAction is refused ROBOT_UNAVAILABLE and nothing
    #     starts, except a re-delivery of interrupted work, which is accepted with no motion;
    #   * each refusal is reported (an EstopAck for every latched ID, and a LocalStopReport),
    #     and both are sent on each SafetyStream that opens;
    #   * an EstopClear lifts the ID it names once that stop's fence has arrived (a clear that
    #     arrives first is kept), and is answered with an EstopClearAck; a clear for an ID
    #     already lifted is acknowledged again;
    #   * a Command other than a stop, or an EstopClear, whose leader term is lower than the
    #     highest seen is rejected; a stop is accepted whatever its term. A Command on a
    #     connection that negotiated less than 1.4.0 carries no term and is not rejected
    #     for it;
    #   * work a stop interrupts is stopped, reported FAILED with ACTION_END_CAUSE_ESTOP, and
    #     never resumed.
    # Not kept: the latched, fenced and cleared IDs live in this process only, which the
    # section allows ("An adapter process that restarts holds none of them").

    def _under_latch(self) -> bool:
        return bool(self._latched) or self._local_set or bool(self._local_set_id)

    def _interrupt_work(self) -> None:
        """Stop the action the robot holds, report it ended by the stop, and keep it stopped."""
        pb = self._pb
        action = self._current_action
        if not action:
            return
        self._interrupted[action] = self._action_fencing_token
        with self._sim_lock:
            self._sim.command_stop(self._robot)
        self._lease.release(self._robot)
        self._current_action = ""
        self._lease_action = ""
        self._move_target = None
        self._job = None
        self._emit_action_status(action, "STOPPED", "stopped by a stop")
        self._outbox.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=action, state=pb.ACTION_STATE_FAILED, message="ended by a stop",
            fencing_token=self._interrupted[action], end_cause=pb.ACTION_END_CAUSE_ESTOP)))

    def _stop_robot(self) -> tuple[int, int]:
        """Command the stop and confirm rest. Returns (stop_initiated_at ms, EstopState)."""
        pb = self._pb
        with self._sim_lock:
            initiated = int(time.time() * 1000)
            state = confirm_estop(self._sim, self._robot)
        return initiated, (pb.ESTOP_STATE_STOPPED if state == ESTOP_STOPPED
                           else pb.ESTOP_STATE_FAILED)

    def _report_latch(self, outbox: queue.Queue) -> None:
        """Every latched stop ID as an EstopAck, and the local latch as a LocalStopReport."""
        pb = self._pb
        for estop_id in sorted(self._latched):
            outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot, estop_ack=pb.EstopAck(
                estop_id=estop_id, state=pb.ESTOP_STATE_STOPPED,
                confirmed_at_ms=int(time.time() * 1000), message="latched (report)")))
        outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot, local_stop=pb.LocalStopReport(
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
        initiated, state = self._stop_robot()
        ack = pb.EstopAck(estop_id=estop_id, state=state, stop_initiated_at=initiated,
                          confirmed_at_ms=int(time.time() * 1000), message="fence")
        self._answer_fence(command, ack)
        clear = self._pending_clears.pop(estop_id, None)
        if clear is not None:
            self._take_clear(clear, self._safety_outbox)

    def _answer_fence(self, command, ack) -> None:
        """The EstopAck on SafetyStream, and the receipt on ControlStream that RFC-0001 permits."""
        pb = self._pb
        self._safety_outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot, estop_ack=ack))
        self._outbox.put(pb.AdapterMessage(command_result=pb.CommandResult(
            command_id=command.command_id, robot_id=command.robot_id, estop=ack)))

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

    def _latch_stop(self, estop_id: str) -> None:
        """An Estop on SafetyStream: latch its ID (unless cleared) and interrupt the work held."""
        with self._latch_lock:
            if estop_id not in self._cleared:
                self._latched.add(estop_id)
            self._interrupt_work()

    def _ack_id(self, estop_id: str) -> str:
        """The stop ID an EstopAck names: the stop's own."""
        return estop_id

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
        if estop_id in self._cleared:
            outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot, estop_clear_ack=(
                pb.EstopClearAck(estop_id=estop_id, lifted_at_ms=int(time.time() * 1000)))))
            return
        if estop_id not in self._fenced:
            self._pending_clears[estop_id] = clear  # takes effect when the fence arrives
            return
        self._latched.discard(estop_id)
        self._cleared.add(estop_id)
        if clear.ends_local_set and clear.ends_local_set == self._local_set_id:
            self._local_set_id = ""  # the refusal from that set ends; a reset lifts the latch
        outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot, estop_clear_ack=(
            pb.EstopClearAck(estop_id=estop_id, lifted_at_ms=int(time.time() * 1000)))))

    def _set_local_latch(self) -> None:
        """An edge stop sets the local latch, under a fresh 128-bit set ID, and is reported."""
        pb = self._pb
        with self._latch_lock:
            self._local_set = True
            self._local_set_id = secrets.token_hex(16)
            self._local_changed_ms = int(time.time() * 1000)
            self._interrupt_work()
            self._safety_outbox.put(pb.AdapterSafetyMessage(
                robot_id=self._robot, local_stop=pb.LocalStopReport(
                    latched=True, changed_at_ms=self._local_changed_ms, source="robot",
                    set_id=self._local_set_id)))

    def _refuse_unserved(self, command, which: str, result) -> None:
        """Refuse a command addressed to a robot this adapter does not serve.

        One ControlStream carries the commands for every Robot bound to this adapter, and
        Command.robot_id selects which one
        (RFC-0001 #fleet-adapter-protocol-the-controlstream-service). This adapter serves
        exactly one. Running a command addressed to any other id on it
        moves the wrong robot: an assignment for a second Robot object bound to the same
        adapter drove this one out of a stopped zone. So nothing here reaches the robot, the
        fence or the lease, and each arm is answered with the refusal the contract already
        defines for it. An assignment is declined ROBOT_UNAVAILABLE: the robot it names is
        not available through this adapter, and the control plane releases the assignment
        to reschedule it. INVALID_ACTION would say the action could not be served, which is
        not true of the action.
        """
        pb = self._pb
        why = (f"robot_id {command.robot_id!r} is not served by this adapter, which serves "
               f"{self._robot!r}; nothing was done")
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
        elif which == "zone_admission":
            result.zone_admission.CopyFrom(pb.ZoneAdmissionAck(acknowledged=False))
        elif which == "validate_action":
            result.validate_action.CopyFrom(pb.ValidateActionResult(servable=False, message=why))
        elif which == "activate_stream":
            result.activate_stream.CopyFrom(pb.ActivateStreamResult(activated=False, message=why))
        elif which == "deactivate_stream":
            result.deactivate_stream.CopyFrom(
                pb.DeactivateStreamResult(deactivated=False, message=why))
        elif which == "push_firmware":
            result.push_firmware.CopyFrom(pb.FirmwareResult(accepted=False, message=why))
        elif which == "model_update":
            result.model_update.CopyFrom(pb.ModelUpdateResult(acknowledged=False, message=why))
        print(f"UNSERVED_REFUSED robot={self._robot} addressed={command.robot_id!r} arm={which} "
              f"command_id={command.command_id}", file=sys.stderr, flush=True)

    def _on_pause(self, pause, result) -> None:
        # Pause halts the sim; require_stop_before_ack acks paused=true only once the
        # robot is CONFIRMED at rest (is_stopped) — never inferred.
        pb = self._pb
        with self._sim_lock:
            self._sim.command_stop(self._robot)
            stopped = (self._sim.is_stopped(self._robot)
                       if hasattr(self._sim, "is_stopped") else True)
        paused = stopped if pause.require_stop_before_ack else True
        result.pause.CopyFrom(pb.PauseResult(
            paused=paused,
            message="sim: paused" if paused else "sim: pausing (awaiting confirmed stop)"))

    def _on_resume(self, resume, result) -> None:
        result.resume.CopyFrom(self._pb.ResumeResult(resumed=True, message="sim: resumed"))

    def _on_zone_admission(self, za, result) -> None:
        """Honour a zone-capacity hold/admit (RFC-0001 #control-plane-zone-controller)
        instead of declining it (C7.1).

        `admit=false` holds the robot at the leaf-zone boundary because the zone is at
        `maxConcurrentRobots`; `admit=true` releases the hold when a slot frees. The control plane
        does not physically prevent entry — enforcement *is* the adapter honouring the hold
        (control-plane.md, Zone Controller capacity enforcement) — so this stops the sim and
        keeps it stopped: `_zone_hold` also suppresses motion in `_on_assign`, so a task
        assigned while held is accepted (it is not refused back to Pending — a capacity hold
        is not a rejection) but does not move until admitted.

        Release deliberately does NOT force motion when there is no current action, and cannot
        resurrect a task a stop dropped: a SafetyStream estop and the scenario drill both clear
        `_current_action`, so an admit after a stop resumes nothing.
        """
        pb = self._pb
        if za.admit:
            self._zone_hold = ""
            if self._current_action and self._assigned_target is not None:
                with self._sim_lock:
                    # proceed with the assigned task, to the target it named
                    self._sim.command_move(self._robot, *self._assigned_target)
                self._move_target = self._assigned_target
        else:
            self._zone_hold = za.zone_name
            with self._sim_lock:
                self._sim.command_stop(self._robot)
            self._move_target = None
        result.zone_admission.CopyFrom(pb.ZoneAdmissionAck(acknowledged=True))

    def _on_scan(self, result) -> None:
        # Answer a ScanCapabilities request with a full CapabilitiesSnapshot from the
        # scenario manifest (empty when no scenario is configured).
        pb = self._pb
        components = []
        if self._engine is not None:
            components = [
                _hw_component(pb, hs, self._hw_status_enum(hs.status))
                for hs in self._engine.hardware_at(0.0)
            ]
        result.scan.CopyFrom(pb.CapabilitiesSnapshot(
            robot_id=self._robot, hardware=components,
            supported_actions=self._supported_actions(),
            installed_models=self._installed_models(),
            sensor_streams=self._sensor_streams(),
            firmware=self._firmware_state(),
            snapshot_ms=int(time.time() * 1000)))

    def _firmware_state(self):
        """The robot's own firmware install state, or None when nothing has been installed.

        None rather than a zero-valued message: an adapter that has never flashed anything is
        not reporting "clean", and the control plane keeps those distinguishable.
        """
        pb = self._pb
        if not self._firmware_version and not self._firmware_failure:
            return None
        if self._firmware_failure:
            return pb.FirmwareState(
                running_version=self._firmware_version,
                status=pb.FIRMWARE_INSTALL_STATUS_FAILED,
                attempted_version=self._firmware_attempted,
                failure_reason=self._firmware_failure)
        return pb.FirmwareState(
            running_version=self._firmware_version,
            status=pb.FIRMWARE_INSTALL_STATUS_RUNNING)

    def _installed_models(self):
        pb = self._pb
        # A model whose last install failed is reported FAILED, with the version the robot is
        # still running and why. The snapshot is the robot's own account of itself (ADR-0033),
        # and reporting ACTIVE here would tell the control plane the failed install never
        # happened, so it would be pushed again instead of rolled back.
        return [pb.InstalledModel(name=name, version=ver, running_version=ver,
                                  status=(pb.MODEL_STATUS_FAILED if name in self._model_failures
                                          else pb.MODEL_STATUS_ACTIVE),
                                  failure_reason=self._model_failures.get(name, ""))
                for name, ver in sorted(self._model_versions.items())]

    def _supported_actions(self):
        pb = self._pb
        target = [pb.ActionParam(name=n, unit="m", kind="number", required=False)
                  for n in ("x", "y")]
        out = [pb.SupportedAction(action_type=t, required_capabilities=list(caps),
                                  params=target if moves else [],
                                  description="sim: " + t + (
                                      f"; optional target x/y in metres, default "
                                      f"{_NOMINAL_TARGET}" if moves else ""))
               for t, caps, moves in _SUPPORTED_ACTIONS]
        for entry in out:
            if entry.action_type == "Charge":
                entry.description = ("sim: Charge; ends when the battery reads full. The "
                                     "simulation has no cable, so cable-out is not observed")
            elif entry.action_type == "Custom":
                entry.description = ("sim: Custom that names no verb; runs until cancelled")
        # One entry per Custom verb (SupportedAction.custom_type, contract 1.4.0): explore-floor
        # declares its own parameters and results, so the verbless Custom above keeps none of
        # them. It is placed BEFORE the unkeyed entry, so a reader that takes the first Custom
        # entry finds the one with parameters, as it did when there was one entry.
        explore = pb.SupportedAction(
            action_type="Custom", custom_type=floor_mapping.OPERATION_EXPLORE,
            description=("sim: Custom explore-floor explores one segment and saves the map at "
                         "<namespace>/<name> in the artifact directory; the final message "
                         "carries map-ref and map-sha256"))
        # The verb stays legible to a reader below 1.4.0, which does not read custom_type: an
        # enum naming it alone. NOT required -- with custom_type set, a payload without
        # `operation` is served, and a catalog saying otherwise would be false.
        explore.params.append(pb.ActionParam(
            name="operation", kind="enum", required=False,
            allowed=[floor_mapping.OPERATION_EXPLORE]))
        for name, kind, unit, lo, hi in floor_mapping.EXPLORE_PARAMS:
            param = pb.ActionParam(name=name, kind=kind, unit=unit, required=False)
            if lo is not None:
                param.min = lo
            if hi is not None:
                param.max = hi
            explore.params.append(param)
        explore.results.extend(pb.ActionParam(name=n, kind=k, unit=u)
                               for n, k, u in floor_mapping.EXPLORE_RESULTS)
        at = next(i for i, e in enumerate(out) if e.action_type == "Custom")
        out.insert(at, explore)
        return out

    @staticmethod
    def _servable(action_type: str, payload_json: bytes, custom_type: str = ""):
        """(entry, target, reason) for an action judged against the catalog.

        ``entry`` is None when the type is not in the catalog or the payload cannot be read,
        and ``reason`` says which. RFC-0001: an adapter MUST NOT treat an action type it does
        not recognise as a nearest-neighbour verb; it rejects the assignment. The same holds
        for a Custom verb (``custom_type``, or the payload's ``operation``): one this adapter
        does not serve is refused, never run as the verbless Custom.
        """
        entry = next((e for e in _SUPPORTED_ACTIONS if e[0] == action_type), None)
        if entry is None:
            return None, None, "unsupported action type: " + (action_type or "(empty)")
        try:
            doc = json.loads(payload_json) if payload_json else {}
        except ValueError:
            return None, None, "payload_json is not JSON"
        if not isinstance(doc, dict):
            return None, None, "payload_json is not a JSON object"
        if action_type == "Custom":
            verb, reason = floor_mapping.verb_of(custom_type, doc)
            if reason:
                return None, None, reason
            if verb is not None and verb != floor_mapping.OPERATION_EXPLORE:
                return None, None, (f"Custom verb {verb!r} is not served; this adapter serves "
                                    f"{floor_mapping.OPERATION_EXPLORE!r}")
            if verb is not None:
                reason = floor_mapping.validate_explore(doc)
                if reason:
                    return None, None, reason
        target = _NOMINAL_TARGET
        if entry[2] and ("x" in doc or "y" in doc):
            try:
                target = (float(doc["x"]), float(doc["y"]))
            except (KeyError, TypeError, ValueError):
                return None, None, "a target needs both x and y, as numbers in metres"
        return entry, target, ""

    # ── Sensor streams and the duty cycle (C18) ──────────────────────────────

    def _sensor_streams(self):
        pb = self._pb
        return [
            pb.SensorStream(
                stream_id=sid,
                media_type=media,
                native_rate_hz=rate,
                required_capabilities=list(caps),
                activation_cost=(pb.ACTIVATION_COST_ALWAYS_ON if always_on
                                 else pb.ACTIVATION_COST_ON_DEMAND),
                supports_clip=clip,
                clip_pre_seconds_max=pre_max)
            for sid, media, rate, caps, always_on, clip, pre_max in _SENSOR_STREAMS
        ]

    def _declared_stream(self, stream_id: str):
        """The declaration tuple for stream_id, or None. An activation for a stream
        this robot never declared is refused rather than invented: the control plane's
        G2 gate is a PRE-filter over a projection that can be stale, so the adapter is
        the authority on what it can actually serve."""
        for entry in _SENSOR_STREAMS:
            if entry[0] == stream_id:
                return entry
        return None

    def _on_activate_stream(self, req, result) -> None:
        """Honour ActivateStream: start (or re-rate) a stream under a lease.

        THE SINKS ARE WHERE FRAMES GO, AND THEY ARE NOT THIS STREAM. `StreamSink.address`
        is a compute-runtime address the adapter dials on its own data path; a conforming
        adapter MUST NOT route sensor-stream bytes back through ControlStream, EdgeStream,
        or any other control-plane-terminated channel. This adapter therefore records the
        sink and emits NOTHING on the control plane for it — which is exactly what C18.2
        measures, by comparing ControlStream byte-rate before and during an activation.
        The simulated robot has no real camera, so no bytes are shipped anywhere; the
        conformance-relevant property is the absence, not the destination.

        RE-ISSUE, NOT A SECOND ACTIVATION. The control plane refcounts per
        (robot_id, stream_id) and re-issues a fresh ActivateStream at the new maximum rate
        when another consumer joins. An adapter that opened a second activation for the
        second command would defeat the refcount, so this replaces in place, keyed by
        stream_id.
        """
        pb = self._pb
        declared = self._declared_stream(req.stream_id)
        if declared is None:
            result.activate_stream.CopyFrom(pb.ActivateStreamResult(
                activated=False,
                message=f"sim: no such sensor stream {req.stream_id!r}"))
            return

        _sid, _media, native_rate, _caps, _always_on, supports_clip, clip_pre_max = declared

        # A CLIP against a stream that declares supports_clip=false asks for something
        # the stream cannot serve at all, so the activation is REFUSED. This is the
        # first half of the deliberate asymmetry in the two clip bounds.
        window = req.window
        mode = window.mode if req.HasField("window") else pb.CLIP_WINDOW_MODE_CONTINUOUS
        if mode == pb.CLIP_WINDOW_MODE_CLIP and not supports_clip:
            result.activate_stream.CopyFrom(pb.ActivateStreamResult(
                activated=False,
                message=f"sim: stream {req.stream_id!r} does not support clip windows"))
            return

        # The second half: a pre_seconds above the declared maximum asks for something
        # the stream CAN serve, only not for as long — so it is BOUNDED rather than
        # refused, because a shorter pre-roll is still evidence and refusing would
        # discard the post-roll too. ActivateStreamResult carries no structured window
        # echo, so the applied value MUST be reported in the free-text message; a
        # consumer that is not told has no way to discover it.
        bounded_note = ""
        pre_seconds = window.pre_seconds if req.HasField("window") else 0
        if mode == pb.CLIP_WINDOW_MODE_CLIP and pre_seconds > clip_pre_max:
            bounded_note = (f"; pre_seconds bounded {pre_seconds} -> {clip_pre_max} "
                            f"(clip_pre_seconds_max)")
            pre_seconds = clip_pre_max

        rate_hz = req.rate_hz if req.rate_hz > 0 else native_rate
        key = f"{self._robot}:{req.stream_id}"
        lease_s = max(0.0, (req.lease_expires_ms - int(time.time() * 1000)) / 1000.0)

        with self._stream_lock:
            reissue = req.stream_id in self._active_streams
            self._active_streams[req.stream_id] = {
                "rate_hz": rate_hz,
                "mode": mode,
                "pre_seconds": pre_seconds,
                "sinks": [sink.address for sink in req.sinks],
            }
        # grant() on a re-issue resets the deadline exactly as renew() would, and is
        # used for both so a re-issue under a NEW fencing token is not silently refused
        # as stale. The wire's monotonicity rule names only AssignAction/CancelAction,
        # so this adapter makes no stale-token claim about ActivateStream it could not
        # honour.
        self._stream_lease.grant(key, duration_s=lease_s, generation=req.fencing_token)

        verb = "REISSUED" if reissue else "ACTIVATED"
        print(f"STREAM_{verb} robot={self._robot} stream={req.stream_id} "
              f"rate_hz={rate_hz} lease_s={lease_s:.3f} sinks={len(req.sinks)}",
              file=sys.stderr, flush=True)
        result.activate_stream.CopyFrom(pb.ActivateStreamResult(
            activated=True,
            message=(f"sim: {req.stream_id} at {rate_hz} Hz for {lease_s:.3f}s"
                     + bounded_note)))

    def _on_deactivate_stream(self, req, result) -> None:
        """Honour DeactivateStream, and answer honestly when there is nothing to stop.

        deactivated=false for an inactive stream is NOT an error and is the only wire
        observable this contract gives for "the stream is off": there is no
        StreamStatusUpdate arm, and this adapter does not invent one. The conformance
        harness uses exactly this reply to observe the lease-expiry self-stop (C18.3).

        Control-plane-initiated ONLY. The adapter never infers a deactivation from
        anything except this message or its own lease expiring.
        """
        pb = self._pb
        with self._stream_lock:
            active = self._active_streams.pop(req.stream_id, None)
        if active is None:
            result.deactivate_stream.CopyFrom(pb.DeactivateStreamResult(
                deactivated=False,
                message=f"sim: stream {req.stream_id!r} is not active"))
            return
        self._stream_lease.release(f"{self._robot}:{req.stream_id}")
        self._stream_stops.append((req.stream_id, "deactivate_stream", time.time()))
        print(f"STREAM_OFF robot={self._robot} stream={req.stream_id} "
              f"reason=deactivate_stream", file=sys.stderr, flush=True)
        result.deactivate_stream.CopyFrom(pb.DeactivateStreamResult(
            deactivated=True, message=f"sim: {req.stream_id} stopped"))

    def _stream_lease_expired(self, key: str) -> None:
        """THE FAIL-SAFE. The lease lapsed without a renewal: stop the sensor.

        This is the designed behaviour of a control-plane partition, not an error path.
        A dead controller and a partitioned one are indistinguishable from here — both
        simply stop producing renewals — and that is precisely why the lease, rather
        than any liveness guess, is what stops the stream.

        It is the INVERSE of the assignment lease's direction and deliberately so: the
        failure that matters for a stream is "left on", because an unattended,
        still-powered sensor is itself the hazard, the cost and the privacy exposure.

        NOTHING IS SENT. There is no wire message for "the stream stopped", and the
        control plane that would receive one is by hypothesis not there. The stop is
        recorded locally and announced on stderr; the harness observes it by asking
        (DeactivateStream) once the control plane is back.
        """
        stream_id = key.split(":", 1)[1] if ":" in key else key
        with self._stream_lock:
            was_active = self._active_streams.pop(stream_id, None) is not None
        if not was_active:
            return
        self._stream_stops.append((stream_id, "lease_expired", time.time()))
        print(f"STREAM_OFF robot={self._robot} stream={stream_id} reason=lease_expired "
              f"at={time.time():.6f}", file=sys.stderr, flush=True)

    def _stream_lease_loop(self) -> None:
        """Tick the stream lease on its OWN cadence, not the telemetry loop's.

        The assignment lease is ticked from the telemetry loop at 1 s, which is fine for
        an action whose lease horizon is measured in tens of seconds. A stream lease is
        the thing an operator is promised a camera goes off at, so a 1 s poll would add
        up to a whole second of unattended camera time to every partition — and the
        overshoot would be reported as the mechanism's latency.
        """
        while not self._shutdown.is_set():
            self._stream_lease.tick()
            time.sleep(_STREAM_LEASE_TICK_S)

    def _on_validate(self, req, result) -> None:
        """Confirm whether the sim can serve an action type (RFC-0001 §9.2 ValidateAction)."""
        entry, _target, reason = self._servable(req.action_type, req.payload_json,
                                                req.custom_type)
        result.validate_action.CopyFrom(self._pb.ValidateActionResult(
            servable=entry is not None, message=reason))

    def _on_verify(self, sub, result) -> None:
        # Answer an active-verification probe (verify_hardware/capability/model) with
        # a healthy VerifyResult whose actualMetrics echo the requested expected
        # metrics — the sim always meets them; a real adapter measures the hardware
        # or model. Surfaces to RobotProbe.status.robotResults[].actualMetrics
        # (RFC-0001 #robotprobe-the-verify-probe-commands). C7-conformant: a genuine
        # VerifyResult is accepted like a decline.
        pb = self._pb
        expected = getattr(sub, "expected_metrics", {})
        result.verify.CopyFrom(pb.VerifyResult(
            status=pb.PROBE_STATUS_HEALTHY,
            actual_metrics={k: v for k, v in expected.items()},
            message="sim: verification passed"))

    def _on_firmware(self, fw, result) -> None:
        """C7.2 (MUST): re-verify the delivered firmware BODY before flashing, and fail closed.

        This is the robot-side half of the artifact chain. The control plane verified the signature
        over the checksum before dispatching; that proves the PUBLISHER vouched for a digest, not
        that the bytes this robot received are those bytes. A registry, mirror, proxy, or on-disk
        cache between the two can serve something else — so the adapter re-hashes what it actually
        got and compares. Flashing without that check makes every hop in the delivery path trusted.

        SIMULATED — read before copying. This adapter downloads nothing, so it hashes a
        deterministic stand-in for the artifact rather than real bytes.

        TODO(vendor): download firmware_uri, hash the RECEIVED BYTES, compare to firmware_checksum,
        and verify firmware_signature_ref against the trust roots — refusing on any mismatch. Report
        the refusal as accepted=false; the control plane records accepted=true as "these bytes were
        checked on the robot".
        """
        pb = self._pb
        ok, why = self._verify_firmware_artifact(fw)
        if not ok:
            result.push_firmware.CopyFrom(pb.FirmwareResult(
                accepted=False, message="sim: refusing unverified firmware artifact — " + why))
            # A refusal IS a terminal outcome. Declining in the command result alone leaves the
            # rollout waiting for an install that will never happen.
            self._firmware_failure = "refused: " + why
            self._firmware_attempted = fw.target_version
            self._emit_install_outcome(
                pb.UPDATE_KIND_FIRMWARE, False, self._firmware_version, self._firmware_failure)
            return

        # Only a verified artifact reaches the flash path. Advisory intra-update progress
        # (RFC-0001 #fleet-adapter-protocol-the-controlstream-service) surfaces to
        # FirmwareRollout.status.currentBatch[].updatePhase.
        self._emit_update_progress(
            pb.UPDATE_KIND_FIRMWARE, ("Pulling", "Installing", "Verifying", "Rebooting"))
        self._firmware_version = fw.target_version
        self._firmware_failure = ""       # a success clears the prior failure
        self._firmware_attempted = ""
        self._emit_install_outcome(pb.UPDATE_KIND_FIRMWARE, True, self._firmware_version)
        result.push_firmware.CopyFrom(pb.FirmwareResult(
            accepted=True, message="sim: firmware applied"))

    def _verify_firmware_artifact(self, fw) -> tuple[bool, str]:
        """Checksum gate for a firmware artifact. Returns (ok, reason); fail-closed by
        construction."""
        checksum = fw.firmware_checksum
        if not checksum:
            return False, "no firmware_checksum supplied"
        if not re.fullmatch(r"sha256:[a-f0-9]{64}", checksum):
            return False, f"malformed firmware_checksum {checksum!r} (want sha256:<64 hex>)"
        expected = "sha256:" + hashlib.sha256(
            f"firmware:{fw.target_version}".encode()).hexdigest()
        if checksum != expected:
            return False, "checksum mismatch (delivered bytes do not match firmware_checksum)"
        return True, ""

    def _on_model_update(self, upd, result) -> None:
        """C7.2 (MUST): verify the artifact before acknowledging, and FAIL CLOSED.

        SIMULATED VERIFICATION — read this before copying. This adapter downloads nothing, so it has
        no artifact bytes to hash. It therefore checks the two things it CAN check without a
        download: that a checksum was supplied in the documented sha256:<64 hex> form, and that it
        matches the digest of the synthetic payload this simulator stands in for. A mismatch or a
        malformed/absent checksum is refused.

        TODO(vendor): a real adapter MUST download the artifact, hash the RECEIVED BYTES, compare
        that digest to model_checksum, and verify model_signature_ref against the trust roots in
        SwarmadaConfig.spec.signing.trustRoots — refusing on any failure. Acknowledging an
        unverified model is a supply-chain compromise on the robot: the control plane treats
        acknowledged=true as "this artifact was checked".

        An Auto rollback (§6.7) carries no artifact — model_uri, checksum and signature are all
        empty because the version is already resident and was verified on first install — so it is
        accepted without a checksum, which is the one legitimate exception. It is signalled by
        ModelUpdate.rollback, the field the wire declares for it. DO NOT infer it from an empty
        model_uri: the empty URI is a CONSEQUENCE of a rollback, never the signal for one, and
        keying on it lets an ordinary install skip verification simply by omitting the URI.

        Setting rollback bypasses no check that protects anything: the rollback path downloads
        nothing and reactivates the already-resident version, which was verified on first install.
        """
        pb = self._pb
        if self._model_artifacts is not None:
            self._on_real_model_update(upd, result)
            return
        if upd.rollback:
            # Auto-rollback path: reactivate a retained version, nothing downloaded, nothing to
            # verify. Still an install, so it reports a terminal outcome like any other (C10.2).
            self._model_versions[upd.model_name] = upd.new_version
            self._emit_install_outcome(pb.UPDATE_KIND_MODEL, True, upd.new_version,
                                       model_name=upd.model_name)
            # ADR-0033 second carrier, re-sent on the transition so a lost terminal report
            # is recoverable. A revert needs this as much as a forward install:
            # the rollout does not settle until the robot reports the previous version.
            self._send_capabilities_snapshot()
            result.model_update.CopyFrom(pb.ModelUpdateResult(
                acknowledged=True, message="sim: rolled back to retained version"))
            return

        ok, why = self._verify_model_artifact(upd)
        if not ok:
            result.model_update.CopyFrom(pb.ModelUpdateResult(
                acknowledged=False, message="sim: refusing unverified model artifact — " + why))
            # A refusal IS a terminal outcome. Declining in the command result alone leaves the
            # rollout waiting for an install that will never happen.
            self._emit_install_outcome(
                pb.UPDATE_KIND_MODEL, False, self._model_versions.get(upd.model_name, ""),
                "refused: " + why, model_name=upd.model_name)
            return

        # Only a verified artifact reaches the install path.
        self._emit_update_progress(
            pb.UPDATE_KIND_MODEL, ("Downloading", "Verifying", "Installing", "HealthChecking"),
            model_name=upd.model_name)
        self._model_versions[upd.model_name] = upd.new_version
        self._emit_install_outcome(pb.UPDATE_KIND_MODEL, True, upd.new_version,
                                   model_name=upd.model_name)
        # ADR-0033 second carrier. The terminal outcome above is enqueued
        # BEFORE the acknowledgement below, so it can reach the control plane before the
        # control plane has recorded that this robot is being updated at all; this snapshot
        # is what repairs that, and it is why the rollout completes without it.
        self._send_capabilities_snapshot()
        result.model_update.CopyFrom(pb.ModelUpdateResult(
            acknowledged=True, message="sim: model updated",
            verified_signer=self._model_signer(upd)))

    def _on_real_model_update(self, upd, result) -> None:
        """model_update with a real artifact (--model-trust-root): fetch, verify, load, run.

        Verification happens BEFORE the acknowledgement and refuses on any doubt (C7.2):
        adapters/simulation/model_runtime.py checks the manifest digest against model_checksum,
        every layer against the manifest, and the cosign signature against the pinned trust
        root. A refusal is acknowledged=false plus a FAILED terminal outcome naming the version
        still running, exactly as on the stand-in path.

        Loading and the self-check happen AFTER the acknowledgement, because a model that is
        genuinely delivered and then fails on the robot is the case the rollback exists for, and
        the control plane only rolls back a robot it recorded as updating. A self-check failure
        leaves the previous model running and reports the new one FAILED; the switch happens
        only after the self-check passes. The fixed frame set is then run on the new model and
        the result logged as a MODEL_RUN line.

        A rollback reactivates a version this process verified and loaded earlier. A version it
        does not hold is refused: the stand-in's "already resident" is not true here.
        """
        pb = self._pb
        cfg = self._model_artifacts
        name, version = upd.model_name, upd.new_version
        if upd.rollback:
            resident = self._resident_models.get((name, version))
            if resident is None:
                result.model_update.CopyFrom(pb.ModelUpdateResult(
                    acknowledged=False,
                    message=f"sim: rollback target {name} {version} is not resident on this robot"))
                return
            result.model_update.CopyFrom(pb.ModelUpdateResult(
                acknowledged=True, message="sim: rolled back to retained version"))
            self._after_ack.append(lambda: self._switch_model(resident, "rollback"))
            return
        try:
            rt = cfg.runtime()
            art = rt.fetch_and_verify(upd.model_uri, upd.model_checksum, upd.model_signature_ref,
                                      cfg.trust_root, cfg.trust_root_name)
        except Exception as err:  # every failure to prove the artifact good is a refusal
            why = str(err) or type(err).__name__
            result.model_update.CopyFrom(pb.ModelUpdateResult(
                acknowledged=False, message="sim: refusing unverified model artifact — " + why))
            self._emit_install_outcome(
                pb.UPDATE_KIND_MODEL, False, self._model_versions.get(name, ""),
                "refused: " + why, model_name=name)
            print(f"MODEL_REFUSED robot={self._robot} model={name} version={version} "
                  f"reason={why}", file=sys.stderr, flush=True)
            return
        self._emit_update_progress(pb.UPDATE_KIND_MODEL, ("Downloading", "Verifying"),
                                   model_name=name)
        print(f"MODEL_VERIFIED robot={self._robot} model={name} version={version} "
              f"digest={art.manifest_digest} signer={art.signer}", file=sys.stderr, flush=True)
        result.model_update.CopyFrom(pb.ModelUpdateResult(
            acknowledged=True, message="sim: model verified; installing",
            verified_signer=art.signer))

        def install() -> None:
            self._emit_update_progress(pb.UPDATE_KIND_MODEL, ("Installing", "HealthChecking"),
                                       model_name=name)
            try:
                model = rt.load(art, name, version)
            except Exception as err:
                why = "self-check failed: " + (str(err) or type(err).__name__)
                self._model_failures[name] = why
                self._emit_install_outcome(pb.UPDATE_KIND_MODEL, False,
                                           self._model_versions.get(name, ""), why,
                                           model_name=name)
                self._send_capabilities_snapshot()
                print(f"MODEL_SELFCHECK_FAILED robot={self._robot} model={name} "
                      f"version={version} running={self._model_versions.get(name, '')} "
                      f"reason={why}", file=sys.stderr, flush=True)
                return
            self._resident_models[(name, version)] = model
            self._switch_model(model, "install")

        self._after_ack.append(install)

    def _switch_model(self, model, how: str) -> None:
        """Make a self-checked model the running one, report it, and run the fixed frames."""
        pb = self._pb
        name, version = model.name, model.version
        self._loaded_models[name] = model
        self._model_versions[name] = version
        self._model_failures.pop(name, None)
        self._emit_install_outcome(pb.UPDATE_KIND_MODEL, True, version, model_name=name)
        self._send_capabilities_snapshot()
        frames, truth = self._model_artifacts.frames
        run = model.run_frames(frames, truth)
        run.update(robot=self._robot, how=how)
        print("MODEL_RUN " + json.dumps(run, separators=(",", ":")), file=sys.stderr, flush=True)
        if self._model_artifacts.results_dir:
            out = pathlib.Path(self._model_artifacts.results_dir)
            out.mkdir(parents=True, exist_ok=True)
            (out / f"{self._robot}-{name}-{version}-{how}.json").write_text(json.dumps(run) + "\n")

    def _verify_model_artifact(self, upd) -> tuple[bool, str]:
        """Checksum (and, when supplied, signature) gate for a model artifact. Returns (ok, reason).

        Fail-closed by construction: every path that cannot PROVE the artifact good returns False.
        """
        if not upd.model_uri:
            # No artifact to fetch means no bytes to hash, so nothing here can be proved. Only a
            # rollback legitimately carries no URI, and that path never reaches this function.
            return False, "no model_uri supplied (only an Auto rollback may omit it)"
        checksum = upd.model_checksum
        if not checksum:
            return False, "no model_checksum supplied"
        if not re.fullmatch(r"sha256:[a-f0-9]{64}", checksum):
            return False, f"malformed model_checksum {checksum!r} (want sha256:<64 hex>)"

        expected = "sha256:" + hashlib.sha256(self._simulated_artifact_bytes(upd)).hexdigest()
        if checksum != expected:
            return False, "checksum mismatch (artifact digest does not match model_checksum)"
        return True, ""

    def _simulated_artifact_bytes(self, upd) -> bytes:
        """The bytes this simulator pretends to have downloaded, derived deterministically from the
        model identity so a control plane can compute the same digest. A real adapter hashes the
        bytes it actually received instead."""
        return f"{upd.model_name}:{upd.new_version}".encode()

    def _model_signer(self, upd) -> str:
        """The trust-root identity that verified the signature, echoed for the
        MODEL_SIGNATURE_VERIFIED audit event (DD-S1). Empty when the push carried no
        signature to verify."""
        return "sim-trust-root" if upd.model_signature_ref else ""

    def _emit_update_progress(self, kind, phases, model_name: str = "") -> None:
        """Advisory intra-update progress. model_name is set on MODEL reports and names the
        model the phase belongs to; without it the control plane has to guess which of a
        robot's concurrent rollouts a Downloading belongs to."""
        pb = self._pb
        total = len(phases)
        for idx, phase in enumerate(phases, start=1):
            self._outbox.put(pb.AdapterMessage(update_progress=pb.UpdateProgress(
                robot_id=self._robot, kind=kind, phase=phase, percent=idx * 100 // total,
                model_name=model_name)))

    def _emit_install_outcome(self, kind, succeeded: bool, resulting_version: str,
                              failure_reason: str = "", model_name: str = "") -> None:
        """Report the TERMINAL result of an install (ADR-0033).

        REQUIRED of any adapter that implements push_firmware / model_update: the command
        result acknowledged only that a download began, so without this the control plane
        cannot tell a slow install from a finished one and the rollout never completes.

        resulting_version is what the robot is ACTUALLY left running. On failure that is not
        the target — here the simulator stays on its previous version — and the control plane
        must not have to guess it.

        model_name is REQUIRED on a MODEL outcome. A robot can legitimately be in two model
        rollouts' batches at once, so a terminal outcome that does not name its model is one
        the control plane can only correlate by guessing.
        """
        pb = self._pb
        outcome = (pb.INSTALL_OUTCOME_SUCCEEDED if succeeded
                   else pb.INSTALL_OUTCOME_FAILED)
        self._outbox.put(pb.AdapterMessage(update_progress=pb.UpdateProgress(
            robot_id=self._robot, kind=kind, outcome=outcome,
            resulting_version=resulting_version, failure_reason=failure_reason,
            model_name=model_name)))

    def _on_assign(self, task, result) -> None:
        pb = self._pb
        decision = self._fence.check(
            self._robot, task.HasField("fencing_token"), task.fencing_token, task.assignment_id)
        if decision is FenceDecision.MISSING:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_MISSING_FENCING_TOKEN))
            return
        if decision is FenceDecision.STALE:
            result.assign_action.CopyFrom(pb.AssignActionResult(
                accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN))
            return
        # C17.1 / C17.2 (FA-R5, FA-R6) -- DECLINE an operating rule this adapter cannot
        # honour, never accept-and-ignore it. Placed AFTER the two fencing branches on
        # purpose: staleness is about ordering, and a superseded command must be refused
        # STALE so the control plane learns its assignment lost, not refused for a rule that
        # was never the reason it lost.
        # The catalog is the last gate: validate_action is optional and the control plane's
        # catalog pre-filter can be stale, so the assignment is judged against the same table
        # the catalog is built from. An action the sim does not serve is refused, not run as
        # the nearest thing it can do.
        entry, target, reason = self._servable(task.action_type, task.payload_json,
                                               task.custom_type)
        if entry is None:
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
        # ACCEPT or idempotent RE-ACK: dispatch to the sim and (re)grant the lease.
        # A zone-capacity hold (RFC-0001 #control-plane-zone-controller) is honoured here: the
        # assignment is still ACCEPTED — a hold is not a rejection, and refusing would bounce
        # the task back to Pending — but the robot stays
        # stopped at the boundary until zone_admission(admit=true) releases it.
        self._assigned_target = tuple(target) if entry[2] else None
        with self._sim_lock:
            if self._zone_hold or not entry[2]:
                self._sim.command_stop(self._robot)
            else:
                self._sim.command_move(self._robot, *target)
        self._move_target = None if self._zone_hold else self._assigned_target
        self._current_action = task.action_id
        self._action_fencing_token = task.fencing_token
        if task.lease_duration_ms:
            self._current_generation = task.lease_generation
            self._lease_action = task.action_id  # which action this lease belongs to
            self._lease.grant(self._robot, task.lease_duration_ms / 1000.0, task.lease_generation)
        result.assign_action.CopyFrom(pb.AssignActionResult(
            accepted=True, accepted_fencing_token=task.fencing_token))  # C3.5
        # adapter-compliance-checklist requires an
        # ActionStatusUpdate at every task phase transition. Accepting the assignment is
        # the first one: the control plane drives FleetAction Assigned -> InProgress off
        # this, and the echoed fencing token is how it detects a robot still executing a
        # superseded assignment.
        self._emit_action_status(task.action_id, "RUNNING", "assignment accepted")
        self._job = self._job_for(task)

    def _job_for(self, task):
        """The self-completing job for an accepted non-motion action, or None."""
        if task.action_type == "Charge":
            return floor_mapping.ChargeJob(task.action_id)
        if task.action_type != "Custom":
            return None
        doc = json.loads(task.payload_json) if task.payload_json else {}
        verb, _reason = floor_mapping.verb_of(task.custom_type, doc)
        if verb != floor_mapping.OPERATION_EXPLORE:
            return None
        if self._artifacts is None:
            self._artifacts = floor_mapping.ArtifactDir(self._artifact_dir,
                                                        self._artifact_store)
        return floor_mapping.ExploreJob(task.action_id, doc, self._explore_states,
                                        self._artifacts, namespace=self._namespace)

    def _advance_job(self, dt: float) -> None:
        """Advance the current non-motion job; report its phase changes and its end.

        A job whose action is no longer current (cancelled, estopped, superseded, lease
        self-stop) is dropped without a report: those paths have already reported the
        action's end, and a job must not report a success for work that was stopped.
        """
        job = self._job
        if job is None:
            return
        if job.action_id != self._current_action:
            self._job = None
            return
        if isinstance(job, floor_mapping.ChargeJob):
            charge = getattr(self._sim, "charge", None)
            with self._sim_lock:
                battery = self._sim.battery_percent(self._robot)
            updates = job.tick(dt, battery, (lambda pct: self._charge(charge, pct))
                               if charge is not None else None)
        else:
            updates = job.tick(dt)
        for u in updates:
            if job.action_id != self._current_action:
                return
            if u.state == "RUNNING":
                self._emit_action_status(job.action_id, "RUNNING", u.message,
                                         progress_pct=u.progress_pct)
                continue
            self._lease.release(self._robot)
            self._emit_action_status(job.action_id, u.state, u.message,
                                     fencing_token=self._action_fencing_token,
                                     progress_pct=u.progress_pct)
            print(f"ACTION_{u.state} robot={self._robot} action={job.action_id}",
                  file=sys.stderr, flush=True)
            self._current_action = ""
            self._lease_action = ""
            self._job = None
            return

    def _charge(self, charge, pct: float) -> None:
        with self._sim_lock:
            charge(self._robot, pct)

    def _reconcile_action_state(self, ack) -> None:
        """C2.2 / RFC-0001 #fleet-adapter-protocol-reconnect-action-state-reconciliation —
        adopt the control plane's authoritative action state.

        The control plane is the source of truth across a connectivity gap: a task may
        have been cancelled while the adapter was offline, and the adapter cannot know
        that from its own state. Resuming it would put two robots on the same work.
        """
        pb = self._pb
        if not ack.HasField("authoritative_action_state"):
            return
        st = ack.authoritative_action_state
        # C3.8 -- adopt the control plane's OWN statement of the current fencing token
        # before deciding anything about the phase. fleet_adapter.proto states the
        # obligation on the field directly: "the adapter MUST reject any command with a
        # lower token for this robot." Without this the mark only ever rises from
        # commands this adapter accepted (FenceGuard.check), so a token the adapter
        # never saw cannot raise it -- and after a control-plane FAILOVER, where the new
        # plane legitimately holds a higher token, this adapter goes on honouring
        # commands the DEAD plane authorised. Two executors, one robot.
        #
        # Unconditional, and ahead of every branch below including the IN_PROGRESS
        # early return: the token is authoritative whether or not the phase diverges
        # from local state, and a handshake that agrees about the action still carries
        # the plane's current mark. `adopt` is monotonic and returns False at or below
        # the current mark, so a replayed or reordered RegisterAck cannot lower it.
        if st.fencing_token:
            self._fence.adopt(self._robot, st.fencing_token)
        phase = st.phase
        if phase == pb.ROBOT_ACTION_PHASE_IN_PROGRESS:
            if st.action_id and st.action_id == self._current_action:
                return  # matches local state; keep executing under the same lease
            phase = pb.ROBOT_ACTION_PHASE_CANCELLED  # divergence → treat as cancelled
        if phase in (pb.ROBOT_ACTION_PHASE_CANCELLED, pb.ROBOT_ACTION_PHASE_UNKNOWN,
                     pb.ROBOT_ACTION_PHASE_IDLE):
            with self._sim_lock:
                self._sim.command_stop(self._robot)
            self._lease.release(self._robot)
            reported = st.action_id or self._current_action
            wire = ("CANCELLED" if phase == pb.ROBOT_ACTION_PHASE_CANCELLED
                    else "UNKNOWN" if phase == pb.ROBOT_ACTION_PHASE_UNKNOWN else "STOPPED")
            self._current_action = ""
            self._lease_action = ""
            self._move_target = None
            self._emit_action_status(reported, wire, "reconciled to authoritative state")

    def _emit_action_status(self, action_id: str, state: str, message: str = "",
                            fencing_token: int | None = None, progress_pct: int = 0) -> None:
        """Send an ActionStatusUpdate for a task phase transition.

        Terminal states are included: the control plane cannot distinguish "still
        running" from "finished and silent", so a task that ends without one of these
        is only resolved by lease expiry.
        """
        if not action_id:
            return
        pb = self._pb
        self._outbox.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=action_id,
            state=getattr(pb.ActionState, f"ACTION_STATE_{state}"),
            message=message,
            fencing_token=(self._fence.highest(self._robot) if fencing_token is None
                           else fencing_token),
            progress_pct=progress_pct,
        )))

    def _on_renew(self, renew, result) -> None:
        pb = self._pb
        renewed = self._lease.renew(
            self._robot, renew.lease_duration_ms / 1000.0, renew.lease_generation)  # C4.3
        if renewed:
            self._current_generation = renew.lease_generation
        running = self._current_action == renew.action_id and self._current_action != ""  # C4.4
        result.renew_lease.CopyFrom(pb.RenewActionLeaseResult(
            renewed=renewed, running=running, current_generation=self._current_generation))

    def _self_stop(self, robot: str) -> None:
        # C4.2 — lease expired without renewal: bring the task to a safe stop.
        #
        # The expiry callback carries only the robot, so the action whose lease lapsed is
        # recorded at grant time. Without that, a timer left over from a PREVIOUS
        # assignment reports the CURRENT action stopped — the control plane would then
        # believe live, renewed work had been abandoned (C4.1).
        expired = self._lease_action
        if not expired or expired != self._current_action:
            return  # superseded: the lease this timer belonged to is no longer held
        with self._sim_lock:
            self._sim.command_stop(robot)
        token = self._action_fencing_token   # the token the expired action was accepted under
        # STOPPED says the stop was commanded; it is not terminal. The terminal report follows
        # once rest is confirmed or the stopping budget runs out, and it is the report that
        # makes the self-stop verifiable at all: without it the control plane sees only
        # silence, which is indistinguishable from a robot that simply stopped reporting.
        self._emit_action_status(expired, "STOPPED", "assignment lease expired")
        with self._sim_lock:
            rest = confirm_estop(self._sim, robot)
        pb = self._pb
        self._outbox.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=expired, state=pb.ACTION_STATE_FAILED, fencing_token=token,
            message=("ended by lease expiry" if rest == ESTOP_STOPPED else
                     "ended by lease expiry; rest not confirmed within the stopping budget"),
            end_cause=pb.ACTION_END_CAUSE_LEASE_EXPIRED)))
        self._current_action = ""
        self._lease_action = ""
        self._move_target = None

    # ── Telemetry (C6) ───────────────────────────────────────────────────────

    def _complete_on_arrival(self) -> None:
        """Report SUCCEEDED once the robot is at rest AT the target it was sent to for the
        current action. The control plane moves a FleetAction to Succeeded only on that
        report, so without it every task ends by lease expiry, never by completion.

        Only arrival completes an action. Every mid-action path acts before it: an estop,
        pause, zone hold or lease self-stop brings the robot to rest short of the target
        (and the estop, cancel and self-stop paths also clear the action), so none of them
        can be read as arrival. An action that does not move the robot (Charge, Custom)
        sets no target and is not completed here.
        """
        action, target = self._current_action, self._move_target
        if not action or target is None:
            return
        with self._sim_lock:
            pose = self._sim.pose(self._robot)
            at_rest = self._sim.is_stopped(self._robot)
        if not at_rest or (abs(pose.x - target[0]) > _ARRIVAL_TOLERANCE_M
                           or abs(pose.y - target[1]) > _ARRIVAL_TOLERANCE_M):
            return
        if action != self._current_action:
            return  # cancelled or superseded while the pose was read
        self._lease.release(self._robot)
        # The token this action was accepted under, not the highest seen since: the update
        # is about this action, and a later adopted token would misattribute it.
        self._emit_action_status(action, "SUCCEEDED", "arrived at target",
                                 fencing_token=self._action_fencing_token)
        self._current_action = ""
        self._lease_action = ""
        self._move_target = None
        print(f"ACTION_SUCCEEDED robot={self._robot} action={action}",
              file=sys.stderr, flush=True)

    def _telemetry_loop(self, stub) -> None:
        # The simulator advances _TICK_DT per pass and a pass takes _TICK_DT of wall time,
        # so the robot moves at its real speed. Telemetry goes out once per
        # _telemetry_interval, which a RegisterAck may set to several seconds (C2.3): the
        # cadence the control plane hears must not set how fast the robot moves.
        pb = self._pb
        self._telemetry_start = time.time()
        last_sent = float("-inf")
        while not self._shutdown.is_set():
            self._lease.tick()  # C4 self-stop check (locks the sim itself if it fires)
            elapsed = time.time() - self._telemetry_start
            self._maybe_estop_drill(elapsed)  # estop-drill: confirmed stop on a timer
            self._maybe_stream_drop(elapsed)   # comms-flaky: tear the stream down on a timer
            with self._sim_lock:
                self._sim.tick(_TICK_DT)
                pose = self._sim.pose(self._robot)
                sim_battery = self._sim.battery_percent(self._robot)
            self._complete_on_arrival()
            self._advance_job(_TICK_DT)
            now = time.monotonic()
            if now - last_sent < self._telemetry_interval:
                time.sleep(_TICK_DT)
                continue
            last_sent = now
            # comms-flaky: suppress the telemetry stream during a scenario gap so the
            # control plane sees the outage. The sim (and lease/estop checks) keep
            # ticking — only the outbound TelemetryPayload is withheld.
            if self._scenario_telemetry_gap(elapsed):
                time.sleep(_TICK_DT)
                continue
            phase = (pb.RobotPhase.ROBOT_PHASE_IN_PROGRESS if self._current_action
                     else pb.RobotPhase.ROBOT_PHASE_IDLE)
            # C6.2: set explicit presence on safety-relevant scalars.
            self._outbox.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(
                robot_id=self._robot,
                timestamp_ms=int(time.time() * 1000),
                position=pb.RobotPosition(x=pose.x, y=pose.y, floor=pose.floor),
                battery=pb.BatteryStatus(percent=self._scenario_battery(elapsed, sim_battery)),
                phase=phase,
                current_action=self._current_action,
                hardware=self._hardware_updates(),  # scenario-driven, delta-compressed
            )))
            time.sleep(_TICK_DT)

    # ── Scenario-driven battery / comms / estop (proto mapping only) ─────────────

    def _scenario_battery(self, elapsed: float, sim_battery: int) -> int:
        """battery-edge: TelemetryPayload.battery.percent from the preset's curve;
        falls back to the simulator's own battery when no curve is declared."""
        if self._engine is None:
            return sim_battery
        curve = self._engine.battery_at(elapsed)
        return sim_battery if curve is None else curve

    def _scenario_telemetry_gap(self, elapsed: float) -> bool:
        """comms-flaky: whether telemetry is suppressed at this elapsed time."""
        return self._engine is not None and self._engine.telemetry_gap_at(elapsed)

    def _maybe_stream_drop(self, elapsed: float) -> None:
        """comms-flaky: at the drop time, end the current ControlStream request
        generator (the None sentinel) so the RPC tears down. run_control_stream then
        waits out the outage and reconnects with a fresh Hello/Register. Idempotent
        while a drop is already pending."""
        if self._engine is None or self._reconnect_pending:
            return
        if self._engine.stream_down_at(elapsed):
            self._reconnect_pending = True
            call = self._active_call
            if call is not None:
                call.cancel()           # tear the RPC down (raises CANCELLED in the loop)
            self._outbox.put(None)      # unblock the request generator so it returns

    def _await_reconnect(self) -> None:
        """Block until the scenario's reconnect time, then return so the control loop
        re-establishes the stream. Telemetry stays suppressed meanwhile (the stream is
        down), and the LeaseMonitor keeps ticking — so an un-renewed lease self-stops
        (C4) during the outage."""
        window = self._engine.stream_drop_window() if self._engine else None
        if window is None:
            return
        _, reconnect_at = window
        while not self._shutdown.is_set():
            if time.time() - self._telemetry_start >= reconnect_at:
                return
            time.sleep(self._telemetry_interval)

    def _maybe_estop_drill(self, elapsed: float) -> None:
        """estop-drill: once the timer is due, bring the robot to a REAL confirmed
        stop via safety.confirm_estop (ground truth, never faked) and report the
        EstopAck over the SafetyStream. Fires exactly once."""
        if self._engine is None or self._estop_drilled or not self._engine.estop_due(elapsed):
            return
        self._estop_drilled = True
        pb = self._pb
        with self._sim_lock:
            state = confirm_estop(self._sim, self._robot)  # C5 discipline: never inferred
        self._emit_action_status(self._current_action, "STOPPED", "estop safe-hold")
        self._current_action = ""  # safe-hold: the task is dropped, like a real estop
        self._move_target = None
        self._safety_outbox.put(pb.AdapterSafetyMessage(
            robot_id=self._robot,
            estop_ack=pb.EstopAck(
                estop_id=f"drill-{self._robot}",
                stop_initiated_at=int(time.time() * 1000),
                state=(pb.ESTOP_STATE_STOPPED if state == ESTOP_STOPPED
                       else pb.ESTOP_STATE_FAILED),
                message="estop drill (scenario)")))

    # ── Scenario-driven hardware reporting (proto mapping only) ──────────────────

    def _hw_status_enum(self, status: str):
        """Map a proto-free scenario status token onto the proto HardwareStatus."""
        pb = self._pb
        return {
            "HEALTHY": pb.HARDWARE_STATUS_HEALTHY,
            "DEGRADED": pb.HARDWARE_STATUS_DEGRADED,
            "FAILED": pb.HARDWARE_STATUS_FAILED,
        }.get(status, pb.HARDWARE_STATUS_UNSPECIFIED)

    def _hardware_updates(self) -> list:
        """TelemetryPayload.hardware for this tick: the components whose status
        changed since the last payload (all of them on the first payload), per the
        scenario's fault timeline. Empty when no scenario is configured."""
        if self._engine is None:
            return []
        pb = self._pb
        elapsed = time.time() - self._telemetry_start
        updates = []
        # C6.1 — the FIRST payload after a (re)connect MUST be a full snapshot. Clearing
        # the delta baseline makes every component look changed, so all of them are sent.
        # Without this a reconnected control plane inherits whatever hardware picture it
        # last had and can never detect that it is stale.
        if self._snapshot_pending:
            self._hw_prev.clear()
            self._snapshot_pending = False
        for hs in self._engine.hardware_delta(elapsed, self._hw_prev):
            updates.append(pb.HardwareStatusUpdate(
                component_name=hs.name,
                status=self._hw_status_enum(hs.status),
                degradation_reason=hs.reason,
            ))
            self._hw_prev[hs.name] = hs.status
        return updates

    def _provisioning_iteration(self, outbox) -> None:
        """Re-send register for a robot still unadmitted, on the stream that was refused.

        An iteration scheduled on a stream that has since been replaced does nothing: the
        new stream's hello_ack registers on its own.
        """
        if self._admitted or self._shutdown.is_set() or outbox is not self._outbox:
            return
        outbox.put(self._pb.AdapterMessage(register=self._pb.RegisterRobot(robot_id=self._robot)))

    def _discover_message(self):
        """The DiscoverRobot this adapter announces when the control plane answers a
        Register with NOT_ADMITTED (C2.1).

        It carries what an operator needs in order to decide the admission: the identity,
        the hardware inventory, and the capabilities the robot claims. Sourced from the
        same places the CapabilitiesSnapshot uses, so a robot does not describe itself one
        way at announce and another way once admitted.
        """
        pb = self._pb
        components = []
        if self._engine is not None:
            components = [
                _hw_component(pb, hs, self._hw_status_enum(hs.status))
                for hs in self._engine.hardware_at(0.0)
            ]
        caps = sorted({c for _, cs, _moves in _SUPPORTED_ACTIONS for c in cs})
        return pb.DiscoverRobot(
            robot_id=self._robot,
            manufacturer="SimBot",
            model="SimBot-250",
            firmware_version=self._firmware_version,
            hardware=components,
            installed_models=self._installed_models(),
            reported_capabilities=caps)

    def _send_capabilities_snapshot(self) -> None:
        """Emit the full hardware/model manifest: once after registration, and again
        whenever an install changes what this robot is running.

        ADR-0033 designates CapabilitiesSnapshot the SECOND carrier of install state,
        precisely so a terminal UpdateProgress that was lost — stream dropped, control
        plane restarted mid-install, or the report simply overtaking the acknowledgement
        it rides behind — is repaired on the next snapshot. Three things had to be true
        for that to work and none of them were:

          * it read the STATIC scenario preset instead of what the adapter had installed,
            so under `healthy-fleet` (installed_models: []) it reported an empty list no
            matter what was running;
          * it built InstalledModel with name/version only, dropping `running_version` and
            `status` — the two fields classifyModel actually keys on, so even a correct
            list projected as UNSPECIFIED with no running version; and
          * it was sent ONCE, at registration, before any install could have happened.

        It now reports self._model_versions through _installed_models(), the same source
        the ScanCapabilities reply uses, and is re-sent on every install transition. The
        control plane writes status only when the projection CHANGES
        (applyInstallState → reflect.DeepEqual), so a repeated identical snapshot costs
        nothing and RA-1 holds: this is an install-transition event, not a telemetry tick.

        Hardware is reported only when a scenario is configured; the model half is
        reported either way, since the recovery path must not depend on one. It is sent
        even with neither a scenario nor an installed model: the snapshot also carries the
        action catalog, and at v0.4 no control plane issues `scan`, so a snapshot pushed at
        registration is the only way the catalog reaches the plane (conformance C12.2).
        """
        pb = self._pb
        components = []
        if self._engine is not None:
            components = [
                _hw_component(pb, hs, self._hw_status_enum(hs.status))
                for hs in self._engine.hardware_at(0.0)
            ]
        self._outbox.put(pb.AdapterMessage(capabilities=pb.CapabilitiesSnapshot(
            robot_id=self._robot,
            hardware=components,
            supported_actions=self._supported_actions(),
            installed_models=self._installed_models(),
            sensor_streams=self._sensor_streams(),
            firmware=self._firmware_state(),
            snapshot_ms=int(time.time() * 1000),
        )))

    # ── SafetyStream (C5) ────────────────────────────────────────────────────

    def run_safety_stream(self, stub) -> None:
        pb = self._pb

        def requests(outbox: queue.Queue) -> Iterator:
            while True:
                msg = outbox.get()
                if msg is None:
                    return
                yield msg

        # Resilient like run_control_stream: a comms-flaky stream drop or a channel
        # close raises an RpcError from the response iterator. On an unexpected drop,
        # log and RE-OPEN SafetyStream (an adapter must always have a live estop path,
        # {{ref:safety}}); on shutdown, exit quietly. The gRPC channel auto-heals, so
        # the reconnect succeeds once connectivity returns.
        while not self._shutdown.is_set():
            # Fresh outbox per session, as for the ControlStream. The dying generator is
            # not stopped the instant its stream faults: it can still be blocked in get(),
            # and with a shared queue it takes the EstopAck for the next estop on the NEW
            # stream and drops it with the dead call, so that estop is never confirmed.
            # The estop drill and shutdown() write to self._safety_outbox, which is always
            # the live session's.
            outbox = self._safety_outbox = queue.Queue()
            if self._shutdown.is_set():
                return  # shutdown() put its sentinel on the previous session's queue
            if self._latching:
                with self._latch_lock:
                    self._report_latch(outbox)  # on each SafetyStream that opens
            try:
                for cp in stub.SafetyStream(requests(outbox)):
                    if cp.WhichOneof("payload") == "estop" and cp.robot_id != self._robot:
                        # An estop for a robot this adapter does not serve. It cannot confirm
                        # that robot is at rest, so the only true answer is FAILED, which
                        # escalates; a STOPPED would confirm a stop nobody made. This robot
                        # is not stopped by it: the command was addressed to another one.
                        print(f"ESTOP_UNSERVED robot={self._robot} addressed={cp.robot_id!r} "
                              f"id={cp.estop.estop_id}", file=sys.stderr, flush=True)
                        outbox.put(pb.AdapterSafetyMessage(
                            robot_id=cp.robot_id,
                            estop_ack=pb.EstopAck(
                                estop_id=cp.estop.estop_id, state=pb.ESTOP_STATE_FAILED,
                                message=(f"robot_id {cp.robot_id!r} is not served by this "
                                         f"adapter, which serves {self._robot!r}; no stop was "
                                         f"commanded and none can be confirmed"))))
                    elif cp.WhichOneof("payload") == "estop":
                        outbox.put(self._on_safety_estop(cp))
                    elif cp.WhichOneof("payload") == "estop_clear" and cp.robot_id == self._robot:
                        if self._latching:
                            self._on_estop_clear(cp.estop_clear, outbox)
                        else:
                            outbox.put(self._on_safety_clear(cp))
            except grpc.RpcError as exc:
                outbox.put(None)  # release the dead stream's generator
                if self._shutdown.is_set():
                    return
                print(f"safety stream dropped ({exc.code()}); reconnecting", file=sys.stderr)
                time.sleep(0.5)
                continue
            # The request generator returned its None sentinel: a clean close → exit.
            return

    def _on_safety_estop(self, cp):
        """Stop the robot on a SafetyStream Estop and return its EstopAck.

        The stop drops the work it interrupts: the current action, its target and the move
        target, as the scenario drill does. So a later zone_admission with admit = true moves
        nothing (RFC-0001 #safety-robot-stop-table). The capacity hold (_zone_hold) is not motion
        and stays. Without --latch this is not the full stop latch: the adapter still accepts a
        later AssignAction. With it, the stop's ID is latched first (_latch_stop).
        """
        pb = self._pb
        if self._latching:
            self._latch_stop(cp.estop.estop_id)
        with self._sim_lock:
            # C5.3: stamp the moment the hardware stop is COMMANDED,
            # before waiting for confirmation. An adapter that inferred
            # the stop has no such moment to report.
            initiated_ms = int(time.time() * 1000)
            # C5: confirmed, never inferred
            state = confirm_estop(self._sim, self._robot)
            self._current_action = ""
            self._assigned_target = None
            self._move_target = None
        # demo-test fixture: delays the ACK only, not the stop
        _demo_estop_ack_delay()
        return pb.AdapterSafetyMessage(
            robot_id=cp.robot_id,
            estop_ack=pb.EstopAck(
                estop_id=self._ack_id(cp.estop.estop_id),
                stop_initiated_at=initiated_ms,
                confirmed_at_ms=int(time.time() * 1000),
                state=(pb.ESTOP_STATE_STOPPED if state == ESTOP_STOPPED
                       else pb.ESTOP_STATE_FAILED)))

    def _on_safety_clear(self, cp):
        """Answer an EstopClear with its EstopClearAck, without --latch. This adapter then keeps
        no stop latch, so the clear takes effect at once, and an EstopClear for an ID already
        lifted is answered the same way (fleet adapter protocol, *The clear*). Without the
        answer the control plane keeps the cleared stop's record on the Robot."""
        pb = self._pb
        return pb.AdapterSafetyMessage(
            robot_id=cp.robot_id,
            estop_clear_ack=pb.EstopClearAck(estop_id=cp.estop_clear.estop_id,
                                             lifted_at_ms=int(time.time() * 1000)))

    # ── EdgeStream (C8) ──────────────────────────────────────────────────────

    def _edge_loop(self) -> None:
        pb = self._pb
        addr = self._edge_addr.get()  # blocks until an edge endpoint is advertised
        estub = self._pb_grpc.EdgeServiceStub(grpc.insecure_channel(addr))
        outbox: queue.Queue = queue.Queue()
        # C8.2: tee a position frame so the edge node has a pose to evaluate.
        with self._sim_lock:
            pose = self._sim.pose(self._robot)
        outbox.put(pb.AdapterEdgeMessage(position=pb.PositionFrame(
            robot_id=self._robot,
            position=pb.RobotPosition(x=pose.x, y=pose.y, floor=pose.floor))))

        def requests() -> Iterator:
            while True:
                msg = outbox.get()
                if msg is None:
                    return
                yield msg

        for ec in estub.EdgeStream(requests()):
            if ec.WhichOneof("msg") == "estop":  # C8.3: same confirmed discipline as C5
                if self._latching:
                    self._set_local_latch()
                with self._sim_lock:
                    state = confirm_estop(self._sim, self._robot)
                ack_state = (pb.ESTOP_STATE_STOPPED if state == ESTOP_STOPPED
                             else pb.ESTOP_STATE_FAILED)
                # Stable marker so the demo-test harness can assert that the third RPC
                # service (EdgeStream) was exercised end-to-end — the edge-issued estop
                # is confirmed here directly with the edge node, bypassing the control
                # plane, so it is not observable in CRD status or control-plane metrics.
                print(f"EDGE_ESTOP_CONFIRMED robot={self._robot} id={ec.estop.estop_id} "
                      f"state={'STOPPED' if state == ESTOP_STOPPED else 'FAILED'}",
                      file=sys.stderr, flush=True)
                outbox.put(pb.AdapterEdgeMessage(estop_ack=pb.EstopAck(
                    estop_id=ec.estop.estop_id, state=ack_state)))


class ModelArtifacts:
    """Real-artifact mode for model_update (--model-trust-root).

    Holds the pinned trust root and the fixed frame set, and imports the optional runtime
    (model_runtime.py: onnxruntime, numpy, cryptography) only when a model is delivered.
    """

    def __init__(self, trust_root_pem: bytes, trust_root_name: str, frames_path: str,
                 results_dir: str = "") -> None:
        self._pem = trust_root_pem
        self.trust_root_name = trust_root_name
        self._frames_path = frames_path
        self.results_dir = results_dir
        self._rt = None
        self._key = None
        self._frames = None

    def runtime(self):
        if self._rt is None:
            from adapters.simulation import model_runtime as rt
            rt.require_runtime()
            self._key = rt.load_trust_root(self._pem)
            self._frames = rt.load_frames(self._frames_path)
            self._rt = rt
        return self._rt

    @property
    def trust_root(self):
        self.runtime()
        return self._key

    @property
    def frames(self):
        self.runtime()
        return self._frames


def main() -> None:
    ap = argparse.ArgumentParser(description="Reference simulation Fleet Adapter")
    # 9445 is the control plane's ControlStream port (the manager's default, every overlay
    # and the chart). The conformance harness listens elsewhere and passes --endpoint.
    ap.add_argument("--endpoint", default="localhost:9445")
    ap.add_argument("--simulator", default="kinematic", choices=["kinematic", "isaac"],
                    help="simulator backend ($0 default: kinematic)")
    ap.add_argument("--robot-id", default="robot-conformance-1")
    ap.add_argument("--command-derived-plant", action="store_true",
                    help="simulate a plant with no motion channel independent of the command path: "
                         "a cancel is answered CANCEL_DISPOSITION_STOP_UNCONFIRMED to a control "
                         "plane "
                         "at contract 1.4.0 or later, and not acknowledged to an older one")
    ap.add_argument("--latch", action="store_true",
                    help="keep the stop latch of contract 1.4.0 and report 1.4.0 (default: no "
                         "latch, report 1.3.0; the reference control plane ends no local set "
                         "at v0.4, so a latching adapter there that took an edge stop takes no "
                         "work until it restarts)")
    ap.add_argument("--witness-file", default=None,
                    help="write the plant model's true speed to this file every tick, as a "
                         "motion witness for conformance C5.7 (simulated plant only)")
    ap.add_argument("--namespace", default="default",
                    help="namespace self-reported in AdapterHello; MUST match the admitted "+
                         "Robot's real namespace for Register to find it against a real "+
                         "control plane (the conformance harness's stub server ignores this; "+
                         "default: default)")
    ap.add_argument("--zone", default="warehouse-a")
    ap.add_argument("--artifact-dir", default=None,
                    help="directory an ArtifactStore declares; explore-floor saves each map at "
                         "<namespace>/<name> under it (default: a scratch directory no "
                         "cluster reads)")
    ap.add_argument("--artifact-store", default="sim-scratch",
                    help="the ArtifactStore name the map reference names (default: sim-scratch)")
    ap.add_argument("--vendor", default="simulation")
    ap.add_argument("--scenario", default="healthy-fleet",
                    help="scenario preset name or path (default: healthy-fleet)")
    ap.add_argument("--fault-component", default=None,
                    help="hardware-fault: component to degrade. Only applied when set; leaving "
                         "it unset keeps the preset's own fault_timeline (e.g. the multi-event "
                         "full-surface coverage preset). Preset default for hardware-fault: "
                         "camera_front.")
    ap.add_argument("--fault-at", type=float, default=None,
                    help="hardware-fault: seconds until the component degrades. Only applied when "
                         "set (preset default for hardware-fault: 30).")
    ap.add_argument("--recover-at", type=float, default=None,
                    help="hardware-fault: seconds until the component recovers. Only applied when "
                         "set (preset default for hardware-fault: 90).")
    # ControlStream mTLS (RFC-0001 #fleet-adapter-protocol-transport-security, C1.3).
    # All three are needed to build a client identity; supplying none keeps the
    # plaintext channel the conformance harness and the projected demo path rely on,
    # so this stays backward compatible.
    #
    # The control plane derives the adapter's NAME from this certificate's SAN
    # (<adapter>.<namespace>.svc.cluster.local) and registers the ControlStream
    # under it. Without a verified identity the stream is accepted but never
    # registered for command push, so validate_action/assign_action are
    # unreachable and no action is ever assigned.
    ap.add_argument("--tls-ca", default=None,
                    help="PEM CA bundle that signed the ControlStream server certificate.")
    ap.add_argument("--tls-cert", default=None,
                    help="PEM client certificate. Its SAN must be "
                         "<adapter>.<namespace>.svc.cluster.local — that is where the control "
                         "plane reads the FleetAdapter name from.")
    ap.add_argument("--tls-key", default=None,
                    help="PEM private key for --tls-cert.")
    ap.add_argument("--tls-server-name", default=None,
                    help="Override the TLS server name (SNI/verification). Needed when reaching "
                         "the ControlStream through a port-forward on 127.0.0.1, whose address "
                         "does not match the server certificate's SAN.")
    # Real model artifacts (optional; adapters/simulation/model_runtime.py). Without
    # --model-trust-root the adapter keeps its stand-in verification and needs no extra.
    ap.add_argument("--model-trust-root", default=None,
                    help="PEM public key that model signatures must verify against: the key "
                         "SwarmadaConfig.spec.signing.trustRoots pins, provisioned to this host. "
                         "Enables real-artifact mode: every model_update is fetched, verified, "
                         "self-checked and run. Needs requirements-model-runtime.txt.")
    ap.add_argument("--model-trust-root-name", default="trust-root",
                    help="name reported as the verified signer (the trustRoots[].name)")
    ap.add_argument("--model-frames", default=None,
                    help="fixed frame set (.npz: frames NCHW float32, optional labels) the robot "
                         "runs a newly switched model on. Required with --model-trust-root.")
    ap.add_argument("--model-results-dir", default=None,
                    help="also write each MODEL_RUN result as JSON into this directory")
    ap.add_argument("--provisioning-interval", type=_positive_seconds, default=None,
                    metavar="SECONDS",
                    help="seconds between register attempts while the robot is not admitted "
                         "(default: the RFC-0001 provisioning-loop rule, 60 s when the "
                         "DiscoverAck carries no TTL; never more than half the TTL)")
    args = ap.parse_args()
    if args.model_trust_root and not args.model_frames:
        ap.error("--model-trust-root needs --model-frames")

    _tls_flags = (args.tls_ca, args.tls_cert, args.tls_key)
    if any(_tls_flags) and not all(_tls_flags):
        ap.error("--tls-ca, --tls-cert and --tls-key must be supplied together "
                 "(mTLS needs a CA to trust, plus a client keypair to present).")

    from fleet_adapter.v1 import fleet_adapter_pb2 as pb
    from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc

    # Scenarios are additive tooling, not a safety requirement (ADR-0005): the
    # adapter must still start and pass conformance without them. If scenario support
    # is unavailable (e.g. PyYAML not installed in a minimal conformance env) degrade
    # to pure safety/telemetry. A bad --scenario *name* is still a hard error.
    engine = None
    try:
        from adapters.scenarios import HardwareFaultOverrides, ScenarioEngine, load_scenario
    except ImportError as exc:
        print(f"scenario support unavailable ({exc}); running without a scenario. "
              f"Install pyyaml to enable --scenario.", file=sys.stderr)
    else:
        # Apply the hardware-fault CLI overrides only when the operator actually set
        # one. With all three unset (the default) the preset's own fault_timeline is
        # used verbatim — required for multi-event coverage presets like full-surface,
        # whose DEGRADED->FAILED->HEALTHY timeline the single-shape override would
        # otherwise collapse (both non-HEALTHY events to --fault-at, HEALTHY to
        # --recover-at). On healthy-fleet the overrides are inert either way.
        overrides = None
        if any(v is not None for v in (args.fault_component, args.fault_at, args.recover_at)):
            overrides = HardwareFaultOverrides(
                component=args.fault_component, fault_at=args.fault_at, recover_at=args.recover_at)
        scenario = load_scenario(args.scenario, overrides)
        engine = ScenarioEngine(scenario)

    sim = make_simulator(args.simulator, args.witness_file)
    model_artifacts = None
    if args.model_trust_root:
        with open(args.model_trust_root, "rb") as fh:
            model_artifacts = ModelArtifacts(fh.read(), args.model_trust_root_name,
                                             args.model_frames, args.model_results_dir or "")
    adapter = SimAdapter(pb, pb_grpc, sim, args.robot_id, args.zone, args.vendor, engine,
                         namespace=args.namespace, model_artifacts=model_artifacts,
                         artifact_dir=args.artifact_dir, artifact_store=args.artifact_store,
                         provisioning_interval_s=args.provisioning_interval,
                         command_derived_plant=args.command_derived_plant,
                         latch=args.latch)

    # Graceful shutdown: on SIGTERM/SIGINT close both streams cleanly rather than
    # letting the transport drop surface as an UNAVAILABLE reconnect in the log. The
    # quickstart's teardown SIGTERMs this adapter BEFORE its port-forward, so a clean
    # stop here keeps the adapter log quiet on a normal finish; a genuinely unexpected
    # mid-run drop still logs and reconnects (that path is unaffected).
    def _handle_signal(_signum, _frame):
        adapter.shutdown()

    signal.signal(signal.SIGTERM, _handle_signal)
    signal.signal(signal.SIGINT, _handle_signal)

    # C1.3: present a client certificate and let that identity be authoritative.
    # Falls back to a plaintext channel when no TLS material is supplied, which is
    # what the conformance harness and the status-projected demo path use.
    if args.tls_cert:
        def _read(path):
            with open(path, "rb") as fh:
                return fh.read()

        creds = grpc.ssl_channel_credentials(
            root_certificates=_read(args.tls_ca),
            private_key=_read(args.tls_key),
            certificate_chain=_read(args.tls_cert),
        )
        # Reaching the ControlStream over a port-forward means dialing 127.0.0.1,
        # which no server SAN covers; override the authority so verification is
        # done against the name the certificate actually carries.
        options = ()
        if args.tls_server_name:
            options = (("grpc.ssl_target_name_override", args.tls_server_name),)
        channel = grpc.secure_channel(args.endpoint, creds, options=options)
    else:
        channel = grpc.insecure_channel(args.endpoint)
    stub = pb_grpc.FleetAdapterServiceStub(channel)
    # C1.1: both streams are opened together.
    threading.Thread(target=adapter.run_safety_stream, args=(stub,), daemon=True).start()
    adapter.run_control_stream(stub)


if __name__ == "__main__":
    main()
