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

"""The latching fixture ends a stop as the simulation adapter does: a fence, then its EstopClear.

``latching_fake.py --plant none`` is the simulation adapter with ``--latch``, and the harness's
latch rows grade every other plant against it. The simulation adapter answers a fence from the
robot's worker, on the stream the fence came on, and records the fence once it is answered; a
clear that arrives first is kept until then. The fake overrides ``_answer_fence``. When its
override no longer matched the adapter's call, answering the fence failed, the fence was never
recorded, its clear was never taken or answered, and the robot stayed latched for good.
"""

from __future__ import annotations

import queue
import sys
from pathlib import Path

import pytest

pytest.importorskip("grpc")

_ROOT = Path(__file__).resolve().parents[3]
for _p in (str(_ROOT), str(_ROOT / "proto")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # noqa: E402

from adapters.conformance.tests import latching_fake  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

ROBOT = "robot-conformance-1"


def _drain(q: queue.Queue) -> list:
    out = []
    while True:
        try:
            msg = q.get_nowait()
        except queue.Empty:
            return out
        if msg is not None:
            out.append(msg)


@pytest.mark.parametrize("plant", ["none", "receipt-only"])
def test_a_fence_then_its_clear_lifts_the_latch_and_work_is_taken_again(plant: str) -> None:
    a = latching_fake.LatchingSimAdapter(pb, None, make_simulator("kinematic"), ROBOT,
                                         "warehouse-a", "simulation", plant=plant)
    a._sim.spawn(ROBOT, 0.0, 0.0)
    a._negotiated_contract = "1.4.0"
    a._handle_command(pb.Command(command_id=1, robot_id=ROBOT, leader_term=1,
                                 estop=pb.Estop(estop_id="e1", issued_by="harness")))
    a._clear_in_lane(pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop_clear=pb.EstopClear(
        estop_id="e1", leader_term=1)), a._safety_outbox)
    assert a._solo_lane.idle(5.0), "the fence and its clear were not answered within 5 s"
    answers = [(m.WhichOneof("payload"), m.estop_ack.state)
               for m in _drain(a._safety_outbox)
               if m.WhichOneof("payload") in ("estop_ack", "estop_clear_ack")]
    assert answers and answers[-1][0] == "estop_clear_ack", (
        f"the clear was never answered: the fence was not recorded ({answers})")
    if plant == "none":
        assert ("estop_ack", pb.ESTOP_STATE_STOPPED) in answers[:-1], (
            "the fence's final answer did not come before its clear's")
    assert not a._latched, "the clear did not lift the latch"
    _drain(a._outbox)
    a._handle_command(pb.Command(
        command_id=2, robot_id=ROBOT, leader_term=1, assign_action=pb.AssignAction(
            action_id="t1", assignment_id="a-t1", action_type="Navigate", fencing_token=2,
            lease_duration_ms=30_000, lease_generation=1, payload_json=b'{"x": 2.0, "y": 0.0}')))
    (res,) = [m.command_result.assign_action for m in _drain(a._outbox)
              if m.WhichOneof("payload") == "command_result"
              and m.command_result.command_id == 2]
    assert res.accepted, f"work was refused after the clear: {res.message}"
