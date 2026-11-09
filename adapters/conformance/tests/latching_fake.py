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

"""The simulation adapter with the stop latch, and one rule broken: a fixture for the harness.

A test double, not a reference adapter. It runs the in-tree simulation adapter with
``latch=True`` (``sim_adapter.py --latch``), which keeps the rules RFC-0001
#fleet-adapter-protocol-the-stop-latch states for a connection at contract 1.4.0, and
``--plant`` breaks one of them, so a test can show the harness fails what it must:

* ``none``: nothing broken; the adapter is ``sim_adapter.py --latch``;
* ``no-latch``: the latch switched off, and contract 1.4.0 still reported. It latches
  nothing, refuses nothing after a stop and answers no clear;
* ``no-lift``: latches, and ignores every ``EstopClear``: no lift, no ``EstopClearAck``;
* ``ack-no-lift``: answers each ``EstopClear`` with an ``EstopClearAck`` and keeps the latch;
* ``receipt-only``: answers a fence with a ``CommandResult.estop`` receipt and sends no
  ``EstopAck`` for it on SafetyStream;
* ``c14-wrong-id``: after its registration is refused, answers an ``Estop`` with an
  ``EstopAck`` naming the last fence it latched instead of the stop it was sent, so C14.1's
  stop draws the fence's ack and none of its own;
* ``custom-needs-type``: declines ``INVALID_ACTION`` a ``Custom`` action that carries no
  ``custom_type``, whatever its payload names (not a latch rule; C19.5's proof);
* ``declares-unhonoured``: declares the ``--honour`` subjects on its CapabilitiesSnapshot and
  enforces none of them, so it declines an assignment carrying only declared subjects (not a
  latch rule; the false declaration C17.4 fails);
* ``keyed-custom``: declares one action, a ``Custom`` entry keyed by ``custom_type``
  (``conformance-patrol``, which drives to a required ``x``/``y``), with the verb also an
  ``operation`` enum that is not required. It takes the verb from ``custom_type``, or from the
  payload's ``operation`` when that is not set, and declines ``INVALID_ACTION`` an assignment
  that names no verb (not a latch rule; the convention a keyed adapter follows, which a harness
  that never sets ``custom_type`` cannot get past).

``--honour SUBJECT`` (repeatable) makes the fake honour an operating subject: it declares it on
the CapabilitiesSnapshot it answers a scan with, and accepts an assignment that carries only
honoured subjects. With none, it declares none, as the simulation adapter does, and C17.4 skips.

Run it as the harness's ``--adapter-cmd``, as ``adapters/simulation/sim_adapter.py`` is run:
``python adapters/conformance/tests/latching_fake.py --endpoint localhost:{port}``.
"""

from __future__ import annotations

import argparse
import json
import signal
import sys
import threading
import time
from pathlib import Path

import grpc

_ROOT = Path(__file__).resolve().parents[3]
for _p in (str(_ROOT), str(_ROOT / "proto")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import adapters.simulation.sim_adapter as sim_adapter  # noqa: E402  (after the sys.path bootstrap)
from adapters.simulation import floor_mapping  # noqa: E402
from adapters.simulation.sim_adapter import (  # noqa: E402
    LATCH_CONTRACT_VERSION,
    SimAdapter,
    make_simulator,
)

PLANTS = ("none", "no-latch", "no-lift", "ack-no-lift", "receipt-only", "c14-wrong-id",
          "custom-needs-type", "declares-unhonoured", "keyed-custom")
# The one verb ``keyed-custom`` serves, and the catalog row it drives as: it moves.
KEYED_VERB = "conformance-patrol"
_KEYED_ENTRY = ("Custom", ["navigation.2d"], True)


class LatchingSimAdapter(SimAdapter):
    """``sim_adapter.py --latch``, with ``plant`` breaking one rule."""

    def __init__(self, *args, plant: str = "none", honoured=(), **kwargs) -> None:
        if plant not in PLANTS:
            raise ValueError(f"unknown plant {plant!r}; one of {PLANTS}")
        super().__init__(*args, latch=plant != "no-latch", **kwargs)
        self._plant = plant
        self._declared = frozenset(honoured)  # the subjects the scan answer declares
        # The planted shape the latch row exists for: 1.4.0 reported, no latch kept.
        self._contract_version = LATCH_CONTRACT_VERSION
        self._last_fence = ""                # the stop ID of the last fence latched
        self._register_refused = False       # the current session's register was refused

    def _handle_control_plane(self, cp, stub) -> None:
        if cp.WhichOneof("payload") == "register_ack":
            self._register_refused = not cp.register_ack.accepted
        super()._handle_control_plane(cp, stub)

    def _handle_command(self, command) -> None:
        pb = self._pb
        if (self._plant == "custom-needs-type" and command.WhichOneof("command") == "assign_action"
                and command.assign_action.action_type == "Custom"
                and not command.assign_action.custom_type):
            self._outbox.put(pb.AdapterMessage(command_result=pb.CommandResult(
                command_id=command.command_id, robot_id=command.robot_id,
                assign_action=pb.AssignActionResult(
                    accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION,
                    message="planted: a Custom action must carry custom_type"))))
            return
        if command.WhichOneof("command") == "estop" and command.estop.estop_id not in self._cleared:
            self._last_fence = command.estop.estop_id
        super()._handle_command(command)

    def _on_scan(self, result) -> None:
        super()._on_scan(result)
        result.scan.honoured_operating_constraints.extend(sorted(self._declared))

    def _supported_actions(self):
        if self._plant != "keyed-custom":
            return super()._supported_actions()
        pb = self._pb
        entry = pb.SupportedAction(
            action_type="Custom", custom_type=KEYED_VERB,
            required_capabilities=list(_KEYED_ENTRY[1]),
            description=f"fake: Custom {KEYED_VERB} drives to x/y; the verb is keyed by "
                        f"custom_type")
        entry.params.extend([pb.ActionParam(name=n, unit="m", kind="number", required=True)
                             for n in ("x", "y")])
        # Legible below 1.4.0, as the simulation adapter's explore-floor is: an enum naming the
        # verb alone, NOT required, because with custom_type set a payload need not repeat it.
        entry.params.append(pb.ActionParam(name="operation", kind="enum", required=False,
                                           allowed=[KEYED_VERB]))
        return [entry]

    def _servable(self, action_type: str, payload_json: bytes, custom_type: str = ""):
        if self._plant != "keyed-custom":
            return super()._servable(action_type, payload_json, custom_type)
        if action_type != "Custom":
            return None, None, "unsupported action type: " + (action_type or "(empty)")
        try:
            doc = json.loads(payload_json) if payload_json else {}
        except ValueError:
            return None, None, "payload_json is not JSON"
        if not isinstance(doc, dict):
            return None, None, "payload_json is not a JSON object"
        verb, reason = floor_mapping.verb_of(custom_type, doc)
        if reason:
            return None, None, reason
        if verb is None:
            return None, None, ("the assignment names no Custom verb (custom_type, or the "
                                "payload's operation)")
        if verb != KEYED_VERB:
            return None, None, (f"Custom verb {verb!r} is not served; this adapter serves "
                                f"{KEYED_VERB!r}")
        try:
            target = (float(doc["x"]), float(doc["y"]))
        except (KeyError, TypeError, ValueError):
            return None, None, "x and y are required, as numbers in metres"
        return _KEYED_ENTRY, target, ""

    def _answer_fence(self, command, ack) -> None:
        if self._plant != "receipt-only":
            super()._answer_fence(command, ack)
            return
        pb = self._pb
        self._outbox.put(pb.AdapterMessage(command_result=pb.CommandResult(
            command_id=command.command_id, robot_id=command.robot_id, estop=ack)))

    def _take_clear(self, clear, outbox) -> None:
        pb = self._pb
        if self._plant == "no-lift":
            return
        if self._plant == "ack-no-lift":
            outbox.put(pb.AdapterSafetyMessage(robot_id=self._robot, estop_clear_ack=(
                pb.EstopClearAck(estop_id=clear.estop_id, lifted_at_ms=int(time.time() * 1000)))))
            return
        super()._take_clear(clear, outbox)

    def _ack_id(self, estop_id: str) -> str:
        if self._plant == "c14-wrong-id" and self._register_refused and self._last_fence:
            return self._last_fence
        return estop_id


def main() -> None:
    ap = argparse.ArgumentParser(description="The simulation adapter with the stop latch "
                                             "(a test fixture for the conformance harness)")
    ap.add_argument("--endpoint", default="localhost:9090")
    ap.add_argument("--robot-id", default="robot-conformance-1")
    ap.add_argument("--plant", default="none", choices=PLANTS,
                    help="break one rule (default: none)")
    ap.add_argument("--witness-file", default=None,
                    help="write the plant model's true speed here, for C5.7")
    ap.add_argument("--honour", action="append", default=[], metavar="SUBJECT",
                    help="honour this operating subject and declare it (repeatable)")
    ap.add_argument("--provisioning-interval", type=float, default=None,
                    help="the provisioning loop's interval in seconds (default: the RFC's rule), "
                         "so a run need not wait 60 s for the register that admits the robot")
    args = ap.parse_args()
    if args.plant != "declares-unhonoured":
        # The simulation adapter judges an assignment's operating rules against this module
        # constant. One fake runs per process, so setting it here is what "honours" means.
        sim_adapter._HONOURED_SUBJECTS = frozenset(args.honour)

    from fleet_adapter.v1 import fleet_adapter_pb2 as pb
    from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc

    engine = None
    try:
        from adapters.scenarios import ScenarioEngine, load_scenario
    except ImportError as exc:
        print(f"scenario support unavailable ({exc}); running without a scenario",
              file=sys.stderr)
    else:
        engine = ScenarioEngine(load_scenario("healthy-fleet", None))

    sim = make_simulator("kinematic", args.witness_file)
    adapter = LatchingSimAdapter(pb, pb_grpc, sim, args.robot_id, "warehouse-a", "simulation",
                                 engine, plant=args.plant, honoured=args.honour,
                                 provisioning_interval_s=args.provisioning_interval)

    def _handle_signal(_signum, _frame):
        adapter.shutdown()

    signal.signal(signal.SIGTERM, _handle_signal)
    signal.signal(signal.SIGINT, _handle_signal)
    stub = pb_grpc.FleetAdapterServiceStub(grpc.insecure_channel(args.endpoint))
    threading.Thread(target=adapter.run_safety_stream, args=(stub,), daemon=True).start()
    adapter.run_control_stream(stub)


if __name__ == "__main__":
    main()
