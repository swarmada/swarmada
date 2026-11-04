# Copyright {{cookiecutter.year}} {{cookiecutter.author}}
# SPDX-License-Identifier: Apache-2.0

"""After its SafetyStream drops and is redialled, the adapter answers the next estop on the
new stream.

grpcio reads an outgoing stream's messages from the request iterator on a thread of its own,
and that thread is not stopped when the call fails: it stays blocked in the iterator. If the
redialled stream's request generator reads the same queue as the dead one, the dead stream's
reader takes the next message, the EstopAck for the next estop, and drops it with the dead
call, so that estop is never confirmed. Each SafetyStream session reads its own queue.
"""

from __future__ import annotations

import threading
import time
from types import SimpleNamespace

import grpc
import pytest

from {{cookiecutter.python_package}}.adapter import {{cookiecutter.python_package}}Adapter
from {{cookiecutter.python_package}}.robot import SimulatedRobot

ROBOT = "r1"

# The messages this test exchanges, without the generated protobuf stubs.
_pb = SimpleNamespace(
    AdapterSafetyMessage=lambda **kw: SimpleNamespace(**kw),
    EstopAck=lambda **kw: SimpleNamespace(**kw),
    LocalStopReport=lambda **kw: SimpleNamespace(**kw),
    ESTOP_STATE_STOPPED=2,
    ESTOP_STATE_FAILED=3,
)


class _Dropped(grpc.RpcError):
    def code(self):
        return grpc.StatusCode.UNAVAILABLE


class _Estop:
    def __init__(self, estop_id: str) -> None:
        self.robot_id = ROBOT
        self.estop = SimpleNamespace(estop_id=estop_id)

    def WhichOneof(self, _name: str) -> str:  # noqa: N802 -- the protobuf method name
        return "estop"


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
        self.acks = []
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
                if hasattr(msg, "estop_ack"):  # the latch report precedes the ack
                    self._acked.set()

        threading.Thread(target=live_reader, daemon=True).start()

        def responses():
            yield _Estop("e-after-redial")
            self._acked.wait(timeout=3.0)

        return responses()


@pytest.mark.parametrize("late", [0.0, 1.5], ids=["reader-blocked", "reader-late"])
def test_the_first_estop_after_a_redial_is_answered_on_the_new_stream(late: float) -> None:
    robot = SimulatedRobot()
    robot.spawn(ROBOT)
    adapter = {{cookiecutter.python_package}}Adapter(_pb, None, robot, ROBOT)
    stub = _DropThenEstop(late)
    adapter.run_safety_stream(stub)

    assert stub.calls == 2
    # The adapter also reports its local latch on the new stream; only the acks are asserted here.
    acked = [m.estop_ack.estop_id for m in stub.acks if hasattr(m, "estop_ack")]
    assert acked == ["e-after-redial"], (
        "the EstopAck for the first estop after the redial did not arrive on the new stream: "
        "the dropped stream's request reader took it")
    assert stub.dead_reader_released.wait(timeout=late + 1.0), (
        "the dropped stream's request generator is still blocked; one is left per redial")
