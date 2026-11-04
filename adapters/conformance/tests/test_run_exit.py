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

"""A run ENDS: the harness process exits, and it exits within its own deadline.

A red run once printed its report and then sat for eleven minutes. ``main()`` had returned
and the interpreter could not finish: the gRPC server's worker threads are not daemon
threads, and two of them were parked inside a stream handler, waiting on a queue that
nothing would ever fill again. An unattended run in that state stalls until the job's own
timeout instead of failing, and the report stays invisible in a block-buffered stdout until
something signals the process.

Three properties are asserted here, and they fail for different reasons:

* ``close_streams`` must release EVERY open stream handler. One sentinel per queue frees
  exactly one handler, and a run can leave more than one parked on the same queue -- an
  adapter that redials leaves the previous stream's handler blocked, and gRPC cannot
  interrupt a handler that is blocked in harness code.
* ``--run-timeout`` must bound the whole PROCESS, not just the checks. When the driver is
  abandoned at its deadline nothing in the driver thread runs again, so whatever stops the
  server on the ordinary paths does not run either.
* stdout must not be block buffered, or a run says nothing until it ends.

The second is a subprocess test, because it is the process's own exit that is under test
and that cannot be observed from inside it.
"""

from __future__ import annotations

import os
import signal
import socket
import subprocess
import sys
import textwrap
import threading
import time
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parents[3]

#: What the harness is told to allow the run.
_RUN_TIMEOUT = 3.0
#: On top of the deadline: the server's stop grace, the adapter's terminate/kill wait and
#: interpreter shutdown. Generous, because the failure this guards against is unbounded.
_EXIT_GRACE = 20.0
#: What "bounded by the deadline" is allowed to cost on top of the deadline itself: the
#: interpreter's own start-up, the server's stop grace and the adapter's terminate wait.
_DEADLINE_GRACE = 8.0

#: An adapter that opens its streams and holds them open with every one of them silent -- the
#: shape a transport error mid-run leaves behind. It opens the ControlStream twice, which is
#: the shape a redial leaves behind: the first stream's handler is still parked in the harness
#: when the second one arrives. Run with ``--hello`` it says hello first, which carries the run
#: past its opening checks so that it is still working when a short deadline arrives.
_HOLDING_ADAPTER = '''\
"""A test double: opens streams, holds them, answers nothing (or only hello)."""

import sys
import threading
import time

import grpc

try:
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb
    from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc
except ImportError:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
    from proto.fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc


def hello_then_silence():
    """A request iterator that says hello, then keeps the stream open and never sends."""
    yield pb.AdapterMessage(hello=pb.AdapterHello(
        vendor="holding-adapter", adapter_version="0.0.0",
        protocol_version="fleet_adapter.v1", namespace="conformance",
        contract_version="1.3.0"))
    while True:
        time.sleep(3600)


def silence():
    """A request iterator that keeps the stream open and never sends."""
    while True:
        time.sleep(3600)
        yield None  # unreachable; this is what makes the function a generator


def hold(call):
    try:
        for _ in call:
            pass
    except grpc.RpcError:
        pass


def main():
    channel = grpc.insecure_channel(sys.argv[1])
    stub = pb_grpc.FleetAdapterServiceStub(channel)
    first = hello_then_silence if "--hello" in sys.argv else silence
    # One stream of each kind. This double used to open a second ControlStream as well, to
    # leave two handlers parked on one queue. The harness now refuses a second live stream
    # of a kind, so that stream would be refused. The parked-handler case is covered in
    # process by test_closing_the_streams_releases_every_open_handler.
    calls = [stub.ControlStream(first()),
             stub.SafetyStream(silence())]
    for call in calls:
        threading.Thread(target=hold, args=(call,), daemon=True).start()
    time.sleep(3600)


main()
'''


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


class _Ctx:
    """Stands in for a gRPC servicer context.

    Reports the RPC as ended. Several handlers end up parked on one queue only when an
    adapter drops its stream and redials, which leaves the dead stream's handler behind.
    The harness admits a new stream only while no other stream of its kind is live.
    """

    def is_active(self) -> bool:
        return False

    def send_initial_metadata(self, metadata) -> None:
        """The SafetyStream handler sends its headers on accept; there is no peer here."""

    def abort(self, code, details):  # pragma: no cover - only the stream-fault path aborts
        raise AssertionError(f"unexpected abort: {code} {details}")


def _drain_in_threads(handlers):
    """Run each handler in its own thread, as the server's worker pool does."""
    threads = []
    for handler in handlers:
        def drain(gen=handler) -> None:
            for _ in gen:
                pass

        thread = threading.Thread(target=drain, daemon=True)
        thread.start()
        threads.append(thread)
    return threads


@pytest.mark.parametrize("stream", ["ControlStream", "SafetyStream"])
def test_closing_the_streams_releases_every_open_handler(harness_mod, stream):
    """Three handlers on one queue, one close: all three must return.

    Parked handlers are what holds the process open after the report is printed, so a close
    that frees one handler out of three is the whole defect in miniature.
    """
    session = harness_mod._Session()
    servicer = harness_mod._make_servicer(session)
    threads = _drain_in_threads(
        [getattr(servicer, stream)(iter(()), _Ctx()) for _ in range(3)])

    time.sleep(0.3)
    assert all(t.is_alive() for t in threads), "a handler returned before the close"

    session.close_streams()

    for thread in threads:
        thread.join(timeout=3.0)
    parked = [t for t in threads if t.is_alive()]
    assert not parked, (
        f"{len(parked)} of 3 {stream} handlers were still parked 3s after the streams were "
        f"closed; a parked handler occupies a server worker thread, which is not a daemon "
        f"thread, so it holds the process open after the report is printed")


def test_closing_the_streams_releases_the_edge_handlers_too(harness_mod):
    """The edge service is served on the same port, off the same pattern."""
    session = harness_mod._Session()
    servicer = harness_mod._make_edge_servicer(session)
    threads = _drain_in_threads([servicer.EdgeStream(iter(()), _Ctx()) for _ in range(3)])

    time.sleep(0.3)
    session.close_streams()
    for thread in threads:
        thread.join(timeout=3.0)
    assert not [t for t in threads if t.is_alive()], (
        "an edge stream handler was still parked 3s after the streams were closed")


def _end_group_and_read(proc, grace=5.0):
    """Kill the harness AND anything it started, then read what it said, bounded.

    Killing only the harness leaves an adapter it started holding the stdout pipe it
    inherited, so a read that waits for EOF waits for the adapter -- and a test that gives
    up on a stalled run would then stall itself, in an unbounded read, instead of failing.
    """
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
    except (ProcessLookupError, PermissionError):  # pragma: no cover - it ended on its own
        proc.kill()
    try:
        out, _ = proc.communicate(timeout=grace)
    except subprocess.TimeoutExpired:  # pragma: no cover - something still holds the pipe
        if proc.stdout is not None:
            proc.stdout.close()
        out = f"(nothing readable within {grace:.0f}s of the harness being killed)"
    return out


def _run_harness(tmp_path, run_timeout, budget, hello=False):
    """Run the harness against the holding adapter; return (exit code, output, seconds)."""
    adapter = tmp_path / "holding_adapter.py"
    adapter.write_text(textwrap.dedent(_HOLDING_ADAPTER), encoding="utf-8")
    port = _free_port()
    adapter_cmd = f"{sys.executable} {adapter} localhost:{{port}}" + (" --hello" if hello else "")
    cmd = [sys.executable, "-m", "adapters.conformance",
           "--stub-path", "proto",
           "--port", str(port),
           "--adapter-name", "holding-adapter",
           "--run-timeout", str(run_timeout),
           "--adapter-cmd", adapter_cmd]

    started = time.monotonic()
    # Its own session, so that giving up on it can end the adapter it started too.
    proc = subprocess.Popen(cmd, cwd=_ROOT, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, start_new_session=True)
    try:
        out, _ = proc.communicate(timeout=budget)
    except subprocess.TimeoutExpired:
        out = _end_group_and_read(proc)
        pytest.fail(
            f"the harness had not exited {budget:.0f}s after a {run_timeout:.0f}s "
            f"--run-timeout; the run stalled instead of ending. What it had said by "
            f"then:\n{out}")
    return proc.returncode, out, time.monotonic() - started


def test_the_process_exits_when_a_run_ends_with_streams_still_open(tmp_path):
    """A run that ends must END.

    The adapter opens three streams -- the ControlStream twice -- and then answers nothing.
    The run gives up on it and closes the streams, which frees one handler per queue; the
    ControlStream's other handler is left parked on a queue nothing will ever fill again,
    on a server worker thread, which is not a daemon thread. That is the stall: the report
    is printed and the process does not exit.
    """
    code, out, elapsed = _run_harness(tmp_path, _RUN_TIMEOUT, _RUN_TIMEOUT + _EXIT_GRACE)
    assert code != 0, f"an adapter that answers nothing must not pass: exit {code}\n{out}"
    assert elapsed < _RUN_TIMEOUT + _EXIT_GRACE


def test_the_run_timeout_bounds_the_process_not_only_the_checks(tmp_path):
    """A run abandoned at its deadline ends at its deadline.

    With a deadline shorter than the run's own opening waits, the driver is still working
    when the caller gives up on it. Nothing in the driver thread runs again after that, so
    whatever stops the server on the ordinary paths does not run either, and the deadline
    bounds only the checks unless the caller stops the server itself.
    """
    deadline = 1.0
    code, out, elapsed = _run_harness(tmp_path, deadline, deadline + _EXIT_GRACE, hello=True)
    assert code == 2, (
        f"a run that did not complete must fail, not pass: exit {code}\n{out}")
    assert "did not complete" in out, out
    assert elapsed < deadline + _DEADLINE_GRACE, (
        f"the process took {elapsed:.1f}s to exit after a {deadline:.1f}s deadline; the "
        f"deadline bounded the checks and not the process\n{out}")


def test_the_entry_point_line_buffers_stdout(tmp_path):
    """A run writing to a file or a pipe is readable while it runs, not only after it ends.

    Python block buffers a stdout that is not a terminal, which is why the report of the
    run that stalled was invisible for eleven minutes: it was written, but it sat in the
    buffer until a signal let the interpreter flush.
    """
    code = ("import sys;"
            "from adapters.conformance.__main__ import line_buffer_stdout;"
            "line_buffer_stdout();"
            "sys.stderr.write(str(sys.stdout.line_buffering))")
    done = subprocess.run([sys.executable, "-c", code], cwd=_ROOT,
                          capture_output=True, text=True, timeout=60, check=False)
    assert done.returncode == 0, done.stderr
    assert done.stderr.strip() == "True", (
        f"stdout was not line buffered when it is a pipe: {done.stderr!r}")
