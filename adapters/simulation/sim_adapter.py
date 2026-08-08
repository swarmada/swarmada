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
(``zone_admission``, §5.4.4) is honoured — a hold really stops the robot, since the
control plane cannot; the remaining optional commands are declined with
``unsupported=true`` (C7). If a zone advertises an edge node it dials the
EdgeStream and honours edge-issued estops with the same confirmed discipline (C8).

Run at $0:  python3 adapters/simulation/sim_adapter.py --endpoint localhost:9090
Validate:   make conformance-sim
"""

from __future__ import annotations

import argparse
import hashlib
import os
import pathlib
import queue
import re
import signal
import sys
import threading
import time
from collections.abc import Iterator

import grpc

# Make the in-tree packages importable when this file is run directly as a script
# (the conformance harness spawns it with only proto/ on PYTHONPATH). A real
# deployment pip-installs the adapter instead of relying on this.
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from adapters.simulation.safety import (  # noqa: E402  (after the sys.path bootstrap)
    ESTOP_STOPPED,
    FenceDecision,
    FenceGuard,
    LeaseMonitor,
    confirm_estop,
)
from adapters.simulation.simulator import make_simulator  # noqa: E402

_TELEMETRY_INTERVAL = 0.2  # seconds between telemetry payloads
_TICK_DT = 0.2             # sim advance per telemetry tick

# The action catalog this reference sim advertises (RFC-0001 §9.2 SupportedAction)
# and validates against: (type, required_capabilities).
_SUPPORTED_ACTIONS = [
    ("Navigate", ["navigation.2d"]),
    ("PickUp",   ["navigation.2d", "transport.payload"]),
    ("DropOff",  ["navigation.2d", "transport.payload"]),
    ("Patrol",   ["navigation.2d"]),
    ("Charge",   []),
    ("Custom",   []),
]


def _demo_estop_ack_delay() -> None:
    """DEMO/TEST ONLY: sleep ``SWARMADA_SIM_ESTOP_ACK_DELAY_MS`` before EMITTING a
    control-plane estop ack, so examples/full-surface-demo can drive the RFC-0001
    §9.3.8 estop-latency SLO-breach alert (ACK latency > 500ms). Unset/0 — the
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
CONTRACT_VERSION = "1.0.0"


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
                 engine=None, namespace: str = "default") -> None:
        self._pb = pb
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
        self._telemetry_start = 0.0          # set when the telemetry loop begins
        self._estop_drilled = False          # estop-drill fires exactly once
        self._reconnect_pending = False      # comms-flaky: a stream drop is in progress
        self._active_call = None             # live ControlStream call, for cancel-on-drop

        self._fence = FenceGuard()                              # C3
        self._lease = LeaseMonitor(on_expiry=self._self_stop)   # C4
        self._current_action = ""
        self._current_generation = 0
        self._lease_action = ""
        self._snapshot_pending = True
        self._telemetry_interval = _TELEMETRY_INTERVAL
        # Zone-capacity hold (§5.4.4): the zone name the control plane is holding this robot out of
        # via zone_admission(admit=false), or "" when free to proceed. A hold suppresses motion —
        # honouring it is the whole enforcement mechanism; the control plane cannot stop the robot.
        self._zone_hold = ""

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
        The control plane then never sees a re-registration, which §9.2.5(b) requires and
        which the whole reconnect reconciliation (§9.2.6) is built on. Binding the queue
        here makes each session's traffic its own.
        """
        pb = self._pb
        outbox = self._outbox
        yield pb.AdapterMessage(hello=pb.AdapterHello(  # C1.2: hello first
            vendor=self._vendor, adapter_version="0.1.0-sim",
            protocol_version="fleet_adapter.v1", contract_version=CONTRACT_VERSION,
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
                    # re-registration §9.2.5(b) requires.
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
        elif kind == "heartbeat":
            self._outbox.put(pb.AdapterMessage(
                heartbeat=pb.HeartbeatResponse(robot_id=cp.heartbeat.robot_id)))
        elif kind == "command":
            self._handle_command(cp.command)

    def _handle_command(self, command) -> None:
        pb = self._pb
        result = pb.CommandResult(command_id=command.command_id, robot_id=command.robot_id)
        which = command.WhichOneof("command")
        if which == "assign_action":
            self._on_assign(command.assign_action, result)
        elif which == "renew_lease":
            self._on_renew(command.renew_lease, result)
        elif which == "cancel_action":
            with self._sim_lock:
                self._sim.command_stop(self._robot)
            self._lease.release(self._robot)
            self._emit_action_status(self._current_action, "CANCELLED", "cancel_action honoured")
            self._current_action = ""
            # The simulated robot carries no physical load, so a cancel always
            # reaches a safe stop — STOPPED_SAFELY (capability-loss reassignment
            # then requeues the task to a capable robot). A real adapter reports
            # COMPLETED or RECOVERED instead when the robot is mid-commitment.
            result.cancel_action.CopyFrom(pb.CancelActionResult(
                acknowledged=True,
                disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY))
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
        else:
            result.unsupported = True  # C7.1: decline every still-optional command
        self._outbox.put(pb.AdapterMessage(command_result=result))

    def _on_pause(self, pause, result) -> None:
        # Pause halts the sim; require_stop_before_ack acks paused=true only once the
        # robot is CONFIRMED at rest (is_stopped) — never inferred.
        pb = self._pb
        with self._sim_lock:
            self._sim.command_stop(self._robot)
            stopped = self._sim.is_stopped(self._robot) if hasattr(self._sim, "is_stopped") else True
        paused = stopped if pause.require_stop_before_ack else True
        result.pause.CopyFrom(pb.PauseResult(
            paused=paused, message="sim: paused" if paused else "sim: pausing (awaiting confirmed stop)"))

    def _on_resume(self, resume, result) -> None:
        result.resume.CopyFrom(self._pb.ResumeResult(resumed=True, message="sim: resumed"))

    def _on_zone_admission(self, za, result) -> None:
        """Honour a zone-capacity hold/admit (§5.4.4) instead of declining it (C7.1).

        `admit=false` holds the robot at the leaf-zone boundary because the zone is at
        `maxConcurrentRobots`; `admit=true` releases the hold when a slot frees. The control plane
        does not physically prevent entry — enforcement *is* the adapter honouring the hold
        (control-plane.md, Zone Controller capacity enforcement) — so this stops the sim and keeps it
        stopped: `_zone_hold` also suppresses motion in `_on_assign`, so a task assigned while held
        is accepted (it is not refused back to Pending — a capacity hold is not a rejection) but does
        not move until admitted.

        Release deliberately does NOT force motion when there is no current action, and cannot
        resurrect a task the safe-hold dropped: an estop clears `_current_action`, so an admit after
        an estop resumes nothing.
        """
        pb = self._pb
        if za.admit:
            self._zone_hold = ""
            if self._current_action:
                with self._sim_lock:
                    self._sim.command_move(self._robot, 10.0, 0.0)  # proceed with the assigned task
        else:
            self._zone_hold = za.zone_name
            with self._sim_lock:
                self._sim.command_stop(self._robot)
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
        return [pb.InstalledModel(name=name, version=ver, running_version=ver,
                                  status=pb.MODEL_STATUS_ACTIVE)
                for name, ver in sorted(self._model_versions.items())]

    def _supported_actions(self):
        pb = self._pb
        return [pb.SupportedAction(action_type=t, required_capabilities=list(caps),
                                   description="sim: " + t)
                for t, caps in _SUPPORTED_ACTIONS]

    def _on_validate(self, req, result) -> None:
        """Confirm whether the sim can serve an action type (RFC-0001 §9.2 ValidateAction)."""
        types = {t for t, _ in _SUPPORTED_ACTIONS}
        servable = req.action_type in types
        result.validate_action.CopyFrom(self._pb.ValidateActionResult(
            servable=servable,
            message="" if servable else ("unsupported action type: " + req.action_type)))

    def _on_verify(self, sub, result) -> None:
        # Answer an active-verification probe (verify_hardware/capability/model) with
        # a healthy VerifyResult whose actualMetrics echo the requested expected
        # metrics — the sim always meets them; a real adapter measures the hardware
        # or model. Surfaces to RobotProbe.status.robotResults[].actualMetrics
        # (§6.10). C7-conformant: a genuine VerifyResult is accepted like a decline.
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

        # Only a verified artifact reaches the flash path. Advisory intra-update progress (§6.6)
        # surfaces to FirmwareRollout.status.currentBatch[].updatePhase.
        self._emit_update_progress(
            pb.UPDATE_KIND_FIRMWARE, ("Pulling", "Installing", "Verifying", "Rebooting"))
        self._firmware_version = fw.target_version
        self._firmware_failure = ""       # a success clears the prior failure
        self._firmware_attempted = ""
        self._emit_install_outcome(pb.UPDATE_KIND_FIRMWARE, True, self._firmware_version)
        result.push_firmware.CopyFrom(pb.FirmwareResult(
            accepted=True, message="sim: firmware applied"))

    def _verify_firmware_artifact(self, fw) -> tuple[bool, str]:
        """Checksum gate for a firmware artifact. Returns (ok, reason); fail-closed by construction."""
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
        accepted without a checksum, which is the one legitimate exception.
        """
        pb = self._pb
        if upd.model_uri == "":
            # Auto-rollback path: reactivate a retained version, nothing to verify.
            result.model_update.CopyFrom(pb.ModelUpdateResult(
                acknowledged=True, message="sim: rolled back to retained version"))
            return

        ok, why = self._verify_model_artifact(upd)
        if not ok:
            result.model_update.CopyFrom(pb.ModelUpdateResult(
                acknowledged=False, message="sim: refusing unverified model artifact — " + why))
            return

        # Only a verified artifact reaches the install path.
        self._emit_update_progress(
            pb.UPDATE_KIND_MODEL, ("Downloading", "Verifying", "Installing", "HealthChecking"))
        self._model_versions[upd.model_name] = upd.new_version
        self._emit_install_outcome(pb.UPDATE_KIND_MODEL, True, upd.new_version)
        result.model_update.CopyFrom(pb.ModelUpdateResult(
            acknowledged=True, message="sim: model updated",
            verified_signer=self._model_signer(upd)))

    def _verify_model_artifact(self, upd) -> tuple[bool, str]:
        """Checksum (and, when supplied, signature) gate for a model artifact. Returns (ok, reason).

        Fail-closed by construction: every path that cannot PROVE the artifact good returns False.
        """
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
        """The trust-root identity that verified the signature, echoed for the MODEL_SIGNATURE_VERIFIED
        audit event (DD-S1). Empty when the push carried no signature to verify."""
        return "sim-trust-root" if upd.model_signature_ref else ""

    def _emit_update_progress(self, kind, phases) -> None:
        pb = self._pb
        total = len(phases)
        for idx, phase in enumerate(phases, start=1):
            self._outbox.put(pb.AdapterMessage(update_progress=pb.UpdateProgress(
                robot_id=self._robot, kind=kind, phase=phase, percent=idx * 100 // total)))

    def _emit_install_outcome(self, kind, succeeded: bool, resulting_version: str,
                              failure_reason: str = "") -> None:
        """Report the TERMINAL result of an install (ADR-0033).

        REQUIRED of any adapter that implements push_firmware / model_update: the command
        result acknowledged only that a download began, so without this the control plane
        cannot tell a slow install from a finished one and the rollout never completes.

        resulting_version is what the robot is ACTUALLY left running. On failure that is not
        the target — here the simulator stays on its previous version — and the control plane
        must not have to guess it.
        """
        pb = self._pb
        outcome = (pb.INSTALL_OUTCOME_SUCCEEDED if succeeded
                   else pb.INSTALL_OUTCOME_FAILED)
        self._outbox.put(pb.AdapterMessage(update_progress=pb.UpdateProgress(
            robot_id=self._robot, kind=kind, outcome=outcome,
            resulting_version=resulting_version, failure_reason=failure_reason)))

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
        # ACCEPT or idempotent RE-ACK: dispatch to the sim and (re)grant the lease.
        # A zone-capacity hold (§5.4.4) is honoured here: the assignment is still ACCEPTED — a hold is
        # not a rejection, and refusing would bounce the task back to Pending — but the robot stays
        # stopped at the boundary until zone_admission(admit=true) releases it.
        with self._sim_lock:
            if self._zone_hold:
                self._sim.command_stop(self._robot)
            else:
                self._sim.command_move(self._robot, 10.0, 0.0)  # nominal destination
        self._current_action = task.action_id
        if task.lease_duration_ms:
            self._current_generation = task.lease_generation
            self._lease_action = task.action_id  # which action this lease belongs to
            self._lease.grant(self._robot, task.lease_duration_ms / 1000.0, task.lease_generation)
        result.assign_action.CopyFrom(pb.AssignActionResult(
            accepted=True, accepted_fencing_token=task.fencing_token))  # C3.5
        # RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist requires an
        # ActionStatusUpdate at every task phase transition. Accepting the assignment is
        # the first one: the control plane drives FleetAction Assigned -> InProgress off
        # this, and the echoed fencing token is how it detects a robot still executing a
        # superseded assignment.
        self._emit_action_status(task.action_id, "RUNNING", "assignment accepted")

    def _reconcile_action_state(self, ack) -> None:
        """C2.2 / §9.2.6 — adopt the control plane's authoritative action state.

        The control plane is the source of truth across a connectivity gap: a task may
        have been cancelled while the adapter was offline, and the adapter cannot know
        that from its own state. Resuming it would put two robots on the same work.
        """
        pb = self._pb
        if not ack.HasField("authoritative_action_state"):
            return
        st = ack.authoritative_action_state
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
            self._emit_action_status(reported, wire, "reconciled to authoritative state")

    def _emit_action_status(self, action_id: str, state: str, message: str = "") -> None:
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
            fencing_token=self._fence.highest(self._robot),
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
        # Required by the compliance checklist and the reason the lease self-stop is
        # verifiable at all: without this the control plane sees only silence, which is
        # indistinguishable from a robot that simply stopped reporting.
        self._emit_action_status(expired, "STOPPED", "assignment lease expired")
        self._current_action = ""
        self._lease_action = ""

    # ── Telemetry (C6) ───────────────────────────────────────────────────────

    def _telemetry_loop(self, stub) -> None:
        pb = self._pb
        self._telemetry_start = time.time()
        while not self._shutdown.is_set():
            self._lease.tick()  # C4 self-stop check (locks the sim itself if it fires)
            elapsed = time.time() - self._telemetry_start
            self._maybe_estop_drill(elapsed)  # estop-drill: confirmed stop on a timer
            self._maybe_stream_drop(elapsed)   # comms-flaky: tear the stream down on a timer
            with self._sim_lock:
                self._sim.tick(_TICK_DT)
                pose = self._sim.pose(self._robot)
                sim_battery = self._sim.battery_percent(self._robot)
            # comms-flaky: suppress the telemetry stream during a scenario gap so the
            # control plane sees the outage. The sim (and lease/estop checks) keep
            # ticking — only the outbound TelemetryPayload is withheld.
            if self._scenario_telemetry_gap(elapsed):
                time.sleep(self._telemetry_interval)
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
            time.sleep(self._telemetry_interval)

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

    def _send_capabilities_snapshot(self) -> None:
        """Emit the full hardware/model manifest once after registration. No-op when
        no scenario is configured."""
        if self._engine is None:
            return
        pb = self._pb
        components = [
            _hw_component(pb, hs, self._hw_status_enum(hs.status))
            for hs in self._engine.hardware_at(0.0)
        ]
        models = [
            pb.InstalledModel(name=m.name, version=m.version)
            for m in self._engine.scenario.installed_models
        ]
        self._outbox.put(pb.AdapterMessage(capabilities=pb.CapabilitiesSnapshot(
            robot_id=self._robot,
            hardware=components,
            supported_actions=self._supported_actions(),
            installed_models=models,
            snapshot_ms=int(time.time() * 1000),
        )))

    # ── SafetyStream (C5) ────────────────────────────────────────────────────

    def run_safety_stream(self, stub) -> None:
        pb = self._pb
        outbox = self._safety_outbox  # shared so the estop-drill can push an ack too

        def requests() -> Iterator:
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
            try:
                for cp in stub.SafetyStream(requests()):
                    if cp.WhichOneof("payload") == "estop":
                        with self._sim_lock:
                            # C5.3: stamp the moment the hardware stop is COMMANDED,
                            # before waiting for confirmation. An adapter that inferred
                            # the stop has no such moment to report.
                            initiated_ms = int(time.time() * 1000)
                            state = confirm_estop(self._sim, self._robot)  # C5: confirmed, never inferred
                        _demo_estop_ack_delay()  # demo-test fixture: delays the ACK only, not the stop
                        outbox.put(pb.AdapterSafetyMessage(
                            robot_id=cp.robot_id,
                            estop_ack=pb.EstopAck(
                                estop_id=cp.estop.estop_id,
                                stop_initiated_at=initiated_ms,
                                confirmed_at_ms=int(time.time() * 1000),
                                state=(pb.ESTOP_STATE_STOPPED if state == ESTOP_STOPPED
                                       else pb.ESTOP_STATE_FAILED))))
            except grpc.RpcError as exc:
                if self._shutdown.is_set():
                    return
                print(f"safety stream dropped ({exc.code()}); reconnecting", file=sys.stderr)
                time.sleep(0.5)
                continue
            # The request generator returned its None sentinel: a clean close → exit.
            return

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


def main() -> None:
    ap = argparse.ArgumentParser(description="Reference simulation Fleet Adapter")
    ap.add_argument("--endpoint", default="localhost:9090")
    ap.add_argument("--simulator", default="kinematic", choices=["kinematic", "isaac"],
                    help="simulator backend ($0 default: kinematic)")
    ap.add_argument("--robot-id", default="robot-conformance-1")
    ap.add_argument("--namespace", default="default",
                    help="namespace self-reported in AdapterHello; MUST match the admitted Robot's "+
                         "real namespace for Register to find it against a real control plane "+
                         "(the conformance harness's stub server ignores this; default: default)")
    ap.add_argument("--zone", default="warehouse-a")
    ap.add_argument("--vendor", default="simulation")
    ap.add_argument("--scenario", default="healthy-fleet",
                    help="scenario preset name or path (default: healthy-fleet)")
    ap.add_argument("--fault-component", default=None,
                    help="hardware-fault: component to degrade. Only applied when set; leaving "
                         "it unset keeps the preset's own fault_timeline (e.g. the multi-event "
                         "full-surface coverage preset). Preset default for hardware-fault: camera_front.")
    ap.add_argument("--fault-at", type=float, default=None,
                    help="hardware-fault: seconds until the component degrades. Only applied when "
                         "set (preset default for hardware-fault: 30).")
    ap.add_argument("--recover-at", type=float, default=None,
                    help="hardware-fault: seconds until the component recovers. Only applied when "
                         "set (preset default for hardware-fault: 90).")
    # ControlStream mTLS (RFC-0001 section 9.2.7, C1.3). All three are needed to
    # build a client identity; supplying none keeps the plaintext channel the
    # conformance harness and the projected demo path rely on, so this stays
    # backward compatible.
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
    args = ap.parse_args()

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

    sim = make_simulator(args.simulator)
    adapter = SimAdapter(pb, pb_grpc, sim, args.robot_id, args.zone, args.vendor, engine,
                         namespace=args.namespace)

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
