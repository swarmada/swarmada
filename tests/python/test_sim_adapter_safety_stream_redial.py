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

"""After its SafetyStream drops and is redialled, the simulation adapter answers the next
estop on the new stream.

grpcio reads an outgoing stream's messages from the request iterator on a thread of its own,
and that thread is not stopped when the call fails: it stays blocked in the iterator. The
adapter's request generator reads a queue. When the redialled stream's generator read the
same queue, the dead stream's reader was first in line for the next message, which is the
EstopAck for the next estop, and dropped it with the dead call: the first estop after every
reconnect resolved Failed although the robot stopped. Each session reads its own queue, as
the ControlStream's sessions do, and the dead session's generator is released.
"""

from __future__ import annotations

import threading
import time

import pytest

grpc = pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

ROBOT = "amr-1"


class _Dropped(grpc.RpcError):
    def code(self):
        return grpc.StatusCode.UNAVAILABLE


class _DropThenEstop:
    """SafetyStream 1 is read as grpcio reads it, then fails. SafetyStream 2 delivers one
    estop and stays open until an ack arrives on it, or 3 s pass.

    ``late`` is how long after the drop the dead stream's reader resumes reading: 0 when it
    is already blocked in the generator, as grpcio's reader usually is, and longer than the
    redial when it is not. A release written to a queue both sessions share then reaches the
    NEW session's generator, which closes the new stream's request side."""

    def __init__(self, late: float) -> None:
        self.late = late
        self.calls = 0
        self.acks = []  # what the adapter sent on stream 2
        self.dead_reader_released = threading.Event()
        self._acked = threading.Event()

    def SafetyStream(self, requests):  # noqa: N802 -- the gRPC method name
        self.calls += 1
        if self.calls == 1:
            def dead_reader():
                time.sleep(self.late)
                for _ in requests:
                    pass  # the call has failed: whatever it reads is lost
                self.dead_reader_released.set()

            threading.Thread(target=dead_reader, daemon=True).start()
            if not self.late:
                time.sleep(0.1)  # the reader is now blocked in the generator
            raise _Dropped()

        def live_reader():
            for msg in requests:
                self.acks.append(msg)
                self._acked.set()

        threading.Thread(target=live_reader, daemon=True).start()

        def responses():
            yield pb.ControlPlaneSafetyMessage(
                robot_id=ROBOT, estop=pb.Estop(estop_id="e-after-redial", reason="test"))
            self._acked.wait(timeout=3.0)

        return responses()


@pytest.mark.parametrize("late", [0.0, 1.5], ids=["reader-blocked", "reader-late"])
def test_the_first_estop_after_a_redial_is_answered_on_the_new_stream(late: float) -> None:
    a = SimAdapter(pb, None, make_simulator("kinematic"), ROBOT, "z1", "sim")
    a._sim.spawn(ROBOT, 0.0, 0.0)
    stub = _DropThenEstop(late)
    a.run_safety_stream(stub)

    assert stub.calls == 2
    ids = [m.estop_ack.estop_id for m in stub.acks if m.WhichOneof("payload") == "estop_ack"]
    assert ids == ["e-after-redial"], (
        "the EstopAck for the first estop after the redial did not arrive on the new stream: "
        "the dropped stream's request reader took it")
    assert stub.dead_reader_released.wait(timeout=late + 1.0), (
        "the dropped stream's request generator is still blocked; one is left per redial")
