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

"""The harness ENDS the adapter it started -- on every path out.

``--adapter-cmd`` is a shell command, so the harness starts it through a shell. Whether the
process the harness holds is the adapter or only the shell that launched it depends on the
shell: some ``/bin/sh`` implementations ``exec`` a simple ``-c`` command and become the
adapter, and some fork and wait for it. Signalling the held process alone therefore ends the
adapter on one host and, on another, ends the shell and leaves the adapter running,
re-parented, with the streams and the stdout pipe it inherited still open.

That is not a cosmetic difference. On a robot the survivor is a live node still holding an
action client after the run that started it has reported and gone. In a test it is worse than
a failure: the orphan holds the harness's stdout pipe open, so a reader waiting for EOF waits
for the orphan, and a regression that should fail hangs instead.

So the property asserted here is not "the harness signalled something". It is: after the
harness has exited, no process it started is still alive. The double writes its own pid to a
file, and the test reads that file and looks for the pid afterwards.

Every wait in this file is bounded, and the harness is started in its own process group so
that a timeout can end the whole group rather than the one process the test can see.
"""

from __future__ import annotations

import os
import signal
import socket
import subprocess
import sys
import textwrap
import time
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parents[3]

#: What the harness is told to allow the run.
_RUN_TIMEOUT = 3.0
#: On top of the deadline: the server's stop grace, the adapter group's terminate/kill wait
#: and interpreter shutdown.
_EXIT_GRACE = 20.0
#: How long after the harness's exit the adapter is still allowed to be alive. The harness
#: ends the group before it returns, so this is slack for the kernel, not for a shutdown.
_SURVIVOR_GRACE = 3.0
#: How long the double is given to write its pid file before a test gives up on it.
_PIDFILE_GRACE = 20.0

#: A test double that records which process it IS, and then holds its streams open and
#: answers nothing -- the shape a transport error mid-run leaves behind, which ends the run
#: by failing it rather than by the adapter exiting. The pid is written to a temporary name
#: and renamed, so a reader never sees half of it. Run with ``--hello`` it says hello first,
#: which carries the run past its opening checks so that it is still working when a short
#: deadline arrives, and the harness leaves by its deadline path instead.
_PID_ADAPTER = '''\
"""A test double: records its pid, opens streams, holds them, answers nothing."""

import os
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


def record_pid(path):
    """Write this process's pid where the test can read it, all at once."""
    tmp = path + ".part"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(str(os.getpid()))
    os.rename(tmp, path)


def hello_then_silence():
    """A request iterator that says hello, then keeps the stream open and never sends."""
    yield pb.AdapterMessage(hello=pb.AdapterHello(
        vendor="pid-adapter", adapter_version="0.0.0",
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
    endpoint, pid_path = sys.argv[1], sys.argv[2]
    record_pid(pid_path)
    channel = grpc.insecure_channel(endpoint)
    stub = pb_grpc.FleetAdapterServiceStub(channel)
    first = hello_then_silence if "--hello" in sys.argv else silence
    for call in (stub.ControlStream(first()), stub.SafetyStream(silence())):
        threading.Thread(target=hold, args=(call,), daemon=True).start()
    time.sleep(3600)


main()
'''


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _is_alive(pid: int) -> bool:
    """True while ``pid`` names a process that has not been reaped.

    A pid that has exited but whose parent has not reaped it still answers signal 0, so the
    process state is consulted as well: a zombie is a record, not a running adapter.
    """
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:  # pragma: no cover - the pid was recycled by another user
        return True
    state = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)],
                           capture_output=True, text=True, check=False).stdout.strip()
    return not state.startswith("Z")


def _end_group(proc: subprocess.Popen, grace: float = 5.0) -> None:
    """Kill ``proc`` AND everything it started, so a timed-out test leaves nothing behind.

    Killing only the harness leaves the adapter it started running, which is the very thing
    under test: a test that gives up must not itself add a survivor to the host.
    """
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
    except (ProcessLookupError, PermissionError):  # pragma: no cover - it ended on its own
        proc.kill()
    try:
        proc.wait(timeout=grace)
    except subprocess.TimeoutExpired:  # pragma: no cover - SIGKILL did not land
        pass


def _await_file(path: Path, grace: float, poll: float = 0.05) -> str:
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        if path.exists():
            text = path.read_text(encoding="utf-8").strip()
            if text:
                return text
        time.sleep(poll)
    return ""


def _run_with_pid_double(tmp_path, adapter_cmd_for, run_timeout=_RUN_TIMEOUT, hello=False):
    """Run the harness against the pid-recording double.

    ``adapter_cmd_for(argv)`` builds the ``--adapter-cmd``, so a test can wrap the double in a
    compound shell command. Returns ``(exit code, output, the double's pid)``.
    """
    double = tmp_path / "pid_adapter.py"
    double.write_text(textwrap.dedent(_PID_ADAPTER), encoding="utf-8")
    pid_file = tmp_path / "adapter.pid"
    log = tmp_path / "harness.out"
    port = _free_port()
    argv = (f"{sys.executable} {double} localhost:{{port}} {pid_file}"
            + (" --hello" if hello else ""))
    cmd = [sys.executable, "-m", "adapters.conformance",
           "--stub-path", "proto",
           "--port", str(port),
           "--adapter-name", "pid-adapter",
           "--run-timeout", str(run_timeout),
           "--adapter-cmd", adapter_cmd_for(argv)]

    # The harness's output goes to a FILE, not to a pipe this test then reads. A survivor
    # inherits the harness's stdout, so reading a pipe would wait for the survivor and the
    # test would report "the harness did not exit" -- true, but not the property under test.
    # Against a file the harness's own exit is visible the moment it happens, and what fails
    # is the survivor assertion, which is what this file is about.
    budget = run_timeout + _EXIT_GRACE
    with open(log, "w", encoding="utf-8") as fh:
        proc = subprocess.Popen(cmd, cwd=_ROOT, stdout=fh, stderr=subprocess.STDOUT,
                                start_new_session=True)
        recorded = _await_file(pid_file, _PIDFILE_GRACE)
        try:
            proc.wait(timeout=budget)
        except subprocess.TimeoutExpired:
            _end_group(proc)
            pytest.fail(
                f"the harness had not exited {budget:.0f}s after a {run_timeout:.0f}s "
                f"--run-timeout. What it had said by then:\n"
                f"{log.read_text(encoding='utf-8')}")
    out = log.read_text(encoding="utf-8")
    assert recorded, (
        f"the adapter never wrote its pid, so the run did not start it. Harness output:\n{out}")
    return proc.returncode, out, int(recorded)


def _assert_no_survivor(pid: int, out: str, grace: float = _SURVIVOR_GRACE) -> None:
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        if not _is_alive(pid):
            return
        time.sleep(0.05)
    try:
        os.killpg(os.getpgid(pid), signal.SIGKILL)
    except (ProcessLookupError, PermissionError):  # pragma: no cover
        pass
    pytest.fail(
        f"the adapter (pid {pid}) was still running {grace:.0f}s after the harness exited. "
        f"The harness started it through a shell and signalled the shell, so on a host whose "
        f"/bin/sh forks rather than execs, the adapter is re-parented and survives the run "
        f"that started it -- still holding its streams and the harness's stdout pipe. "
        f"Harness output:\n{out}")


def test_no_adapter_process_survives_a_failed_run(tmp_path):
    """The run ends by failing the adapter. The adapter must end with it."""
    code, out, pid = _run_with_pid_double(tmp_path, lambda argv: argv)
    assert code != 0, f"an adapter that answers nothing must not pass: exit {code}\n{out}"
    _assert_no_survivor(pid, out)


def test_no_adapter_process_survives_the_run_timeout_path(tmp_path):
    """A run abandoned at its deadline takes its adapter with it.

    With a deadline shorter than the run's own opening waits the driver is still working when
    the caller gives up on it, and the caller returns down a different path. The adapter is
    ended on that path too, or an unattended run that overran leaves a node on the robot.
    """
    code, out, pid = _run_with_pid_double(tmp_path, lambda argv: argv, run_timeout=1.0,
                                          hello=True)
    assert code == 2, f"a run that did not complete must fail, not pass: exit {code}\n{out}"
    _assert_no_survivor(pid, out)


def test_a_compound_adapter_command_still_runs_and_still_ends(tmp_path):
    """``--adapter-cmd`` is a shell command, and a compound one must keep working.

    Making the shell ``exec`` the adapter would also stop the survivor, and would break this:
    a command with a second stage is not a single program for the shell to become. Ending the
    whole process group works for both shapes, so both are asserted here -- the adapter runs,
    and nothing it or the shell left behind outlives the harness.
    """
    def compound(argv: str) -> str:
        return f"{sys.executable} -c 'pass' && {argv}"

    code, out, pid = _run_with_pid_double(tmp_path, compound)
    assert code != 0, f"an adapter that answers nothing must not pass: exit {code}\n{out}"
    _assert_no_survivor(pid, out)


def test_ending_the_group_ends_a_child_the_shell_did_not_exec(tmp_path):
    """The entry point's own teardown, against the shape that defeats terminate().

    A shell that forks leaves two processes: the shell, which is what ``Popen`` returns, and
    the command, which is what matters. This drives the teardown directly so that its failure
    is one assertion rather than a whole run.
    """
    from adapters.conformance.__main__ import end_process_group

    marker = tmp_path / "child.pid"
    child_script = tmp_path / "child.py"
    child_script.write_text(
        "import os, sys, time\n"
        "open(sys.argv[1] + '.part', 'w').write(str(os.getpid()))\n"
        "os.replace(sys.argv[1] + '.part', sys.argv[1])\n"
        "time.sleep(300)\n", encoding="utf-8")
    proc = subprocess.Popen(f"true && {sys.executable} {child_script} {marker}",
                            shell=True, start_new_session=True)
    recorded = _await_file(marker, _PIDFILE_GRACE)
    assert recorded, "the child never started, so there is nothing to end"

    end_process_group(proc)

    assert proc.poll() is not None, "the shell the harness holds was not reaped"
    deadline = time.monotonic() + _SURVIVOR_GRACE
    child = int(recorded)
    while time.monotonic() < deadline and _is_alive(child):
        time.sleep(0.05)
    assert not _is_alive(child), (
        f"the child (pid {child}) outlived the shell the harness signalled")
