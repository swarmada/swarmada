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

"""A run grades ONE adapter: no second stream, and no second run on its port.

C3.8 once recorded "a later AssignAction at 18 was NOT refused STALE (accepted=True
rejection=0)" against an adapter that had adopted token 20 and refused 18 in every other
run. The adapter was right. A second conformance run had bound the same port, which gRPC's
default SO_REUSEPORT allows without error. That run's adapter dialled this server, because
on macOS the first listener takes every connection, and its ControlStream drained the same
outbound queue as the adapter under test. The adoption probe went to the stranger, which had
never seen the RegisterAck, and it accepted.

Three rules close that, and each is asserted here:

* the harness will not share its port;
* a second LIVE stream of a kind is refused, and a redial after a stream ends is not;
* a handler whose stream is no longer the live one gives back any message it was handed,
  so nothing the run sends goes to a dead or superseded stream.
"""

from __future__ import annotations

import queue
import socket
import threading
import time
from concurrent import futures

import grpc
import pytest


class _Ctx:
    """A servicer context whose liveness the test controls, recording any abort."""

    def __init__(self, active: bool = True) -> None:
        self.active = active
        self.aborted = None

    def is_active(self) -> bool:
        return self.active

    def send_initial_metadata(self, metadata) -> None:
        """The SafetyStream handler sends its headers on accept; there is no peer here."""

    def abort(self, code, details):
        self.aborted = (code, details)
        self.active = False
        raise RuntimeError("aborted")  # what grpc's abort does: it does not return


def _drain(gen, sink):
    def run():
        try:
            for item in gen:
                sink.append(item)
        except RuntimeError:
            pass
    t = threading.Thread(target=run, daemon=True)
    t.start()
    return t


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


@pytest.mark.parametrize("stream", ["ControlStream", "SafetyStream"])
def test_a_second_live_stream_is_refused(harness_mod, stream):
    session = harness_mod._Session()
    servicer = harness_mod._make_servicer(session)
    first, second = _Ctx(), _Ctx()
    t1 = _drain(getattr(servicer, stream)(iter(()), first), [])
    time.sleep(0.2)
    t2 = _drain(getattr(servicer, stream)(iter(()), second), [])
    t2.join(timeout=harness_mod._SUPERSEDE_WAIT + 2.0)

    assert not t2.is_alive(), "the second live stream was admitted and is parked"
    assert second.aborted is not None
    assert second.aborted[0] is grpc.StatusCode.FAILED_PRECONDITION
    assert first.aborted is None
    if stream == "ControlStream":
        assert session.control_opens == 1, "a refused stream must not count as a redial"
    session.close_streams()
    t1.join(timeout=3.0)


def test_a_redial_after_the_stream_ended_is_admitted(harness_mod):
    session = harness_mod._Session()
    servicer = harness_mod._make_servicer(session)
    dead = _Ctx(active=False)
    t1 = _drain(servicer.ControlStream(iter(()), dead), [])
    time.sleep(0.2)
    redial = _Ctx()
    t2 = _drain(servicer.ControlStream(iter(()), redial), [])
    time.sleep(0.2)

    assert redial.aborted is None
    assert session.live_control is redial
    assert session.control_opens == 2
    assert session.control_reconnected.is_set()
    session.close_streams()
    for t in (t1, t2):
        t.join(timeout=3.0)


def test_a_superseded_handler_hands_back_what_it_took(harness_mod, pb):
    """Whichever handler the queue wakes, the message reaches the live stream, in order."""
    session = harness_mod._Session()
    servicer = harness_mod._make_servicer(session)
    dead = _Ctx(active=False)
    got_dead, got_live = [], []
    t1 = _drain(servicer.ControlStream(iter(()), dead), got_dead)
    time.sleep(0.2)
    live = _Ctx()
    t2 = _drain(servicer.ControlStream(iter(()), live), got_live)
    time.sleep(0.2)

    sent = [pb.ControlPlaneMessage(command=pb.Command(command_id=i)) for i in range(1, 21)]
    for msg in sent:
        session.control_out.put(msg)
    deadline = time.monotonic() + 5.0
    while len(got_live) < len(sent) and time.monotonic() < deadline:
        time.sleep(0.05)

    assert got_dead == [], "a message went to a stream that is no longer the adapter's"
    assert [m.command.command_id for m in got_live] == list(range(1, 21))
    session.close_streams()
    for t in (t1, t2):
        t.join(timeout=3.0)


def test_the_harness_will_not_share_its_port(harness_mod):
    """A port another server holds is refused at bind, not shared."""
    port = _free_port()
    other = grpc.server(futures.ThreadPoolExecutor(max_workers=1))  # gRPC defaults: reuseport on
    assert other.add_insecure_port(f"[::]:{port}") == port
    other.start()
    try:
        ready = threading.Event()
        with pytest.raises(RuntimeError, match="already in use"):
            harness_mod.run_conformance("port-holder", port, ready)
        assert not ready.is_set(), "a run that could not bind must not signal readiness"
    finally:
        other.stop(grace=0).wait(timeout=5.0)


def test_a_stray_on_the_queue_cannot_reorder_a_redial(harness_mod, pb):
    """The queue discipline itself: a hand-back goes to the head, not the tail."""
    q: queue.Queue = queue.Queue()
    q.put("second")
    harness_mod._requeue_front(q, "first")
    assert [q.get_nowait(), q.get_nowait()] == ["first", "second"]
