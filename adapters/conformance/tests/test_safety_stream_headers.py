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

"""The harness serves a SafetyStream as the control plane does: it sends its response headers
when it accepts the stream, before any estop.

A gRPC client's call returns before the server has accepted anything, so an adapter learns
that its estop path is served from the headers. The control plane sends them on accept. A
grpcio server sends them only with its first message unless the handler sends them itself,
so without that an adapter that waits for them would not be served here until the first
estop, and would refuse registration and motion for the whole run.
"""

from __future__ import annotations

import threading
from concurrent import futures

import grpc


def _serve(harness_mod, session):
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    harness_mod.pb_grpc.add_FleetAdapterServiceServicer_to_server(
        harness_mod._make_servicer(session), server)
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    return server, port


def _open(stub, stop: threading.Event):
    def requests():
        stop.wait()
        return
        yield  # a generator that sends nothing until the test ends

    return stub.SafetyStream(requests())


def _initial_metadata(call, timeout: float) -> bool:
    """Whether ``call.initial_metadata()`` returned within ``timeout`` seconds."""
    t = threading.Thread(target=call.initial_metadata, daemon=True)
    t.start()
    t.join(timeout=timeout)
    return not t.is_alive()


def test_an_accepted_safety_stream_sends_headers_before_any_estop(harness_mod, pb):
    session = harness_mod._Session()
    server, port = _serve(harness_mod, session)
    stop = threading.Event()
    try:
        with grpc.insecure_channel(f"127.0.0.1:{port}") as channel:
            call = _open(harness_mod.pb_grpc.FleetAdapterServiceStub(channel), stop)
            assert _initial_metadata(call, 3.0), (
                "no headers 3s after the harness accepted the SafetyStream: an adapter that waits "
                "for them is not served until the first estop")
            assert not call.done(), (
                "the headers came with the end of the call, not with its acceptance")
            assert session.safety_established.is_set()

            session.safety_out.put(
                pb.ControlPlaneSafetyMessage(robot_id="r1", estop=pb.Estop(estop_id="e-1")))
            first = next(call)
            assert first.estop.estop_id == "e-1", (
                "the first message on the stream is the estop sent after the headers")
            call.cancel()
    finally:
        stop.set()
        session.close_streams()
        server.stop(grace=0).wait(timeout=5.0)


def test_a_refused_second_safety_stream_gets_no_headers(harness_mod, pb):
    """The harness refuses a second live SafetyStream; that stream ends without being served."""
    session = harness_mod._Session()
    server, port = _serve(harness_mod, session)
    stop = threading.Event()
    try:
        with grpc.insecure_channel(f"127.0.0.1:{port}") as channel:
            stub = harness_mod.pb_grpc.FleetAdapterServiceStub(channel)
            first = _open(stub, stop)
            assert session.safety_established.wait(timeout=3.0)
            second = _open(stub, stop)
            assert _initial_metadata(second, harness_mod._SUPERSEDE_WAIT + 3.0)
            assert second.code() is grpc.StatusCode.FAILED_PRECONDITION
            assert not first.done(), "refusing the second stream ended the first"
            first.cancel()
    finally:
        stop.set()
        session.close_streams()
        server.stop(grace=0).wait(timeout=5.0)
