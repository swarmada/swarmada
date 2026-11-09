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

"""CLI entry point for the Fleet Adapter conformance harness.

Example (against the in-tree reference adapter)::

    python -m adapters.conformance \\
        --stub-path proto \\
        --adapter-name example-noop \\
        --adapter-cmd 'python adapters/example-noop/noop_adapter.py --endpoint localhost:{port}'

The harness starts the control-plane test server, launches the adapter-under-test
pointed at it, drives the CONFORMANCE.md checks, prints a report, and exits
non-zero if any MUST / MUST NOT check failed.
"""

from __future__ import annotations

import argparse
import os
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

#: The name a report carries when nobody said which adapter ran. Accepted for a run
#: written anywhere else, and REFUSED for a report written into the repository's
#: conformance-reports/: a stored report that cannot say which adapter it describes
#: cannot be matched to the registry row it supports.
DEFAULT_ADAPTER_NAME = "adapter-under-test"
REPORTS_DIR = Path(__file__).resolve().parents[2] / "conformance-reports"


def refuses_default_name(adapter_name: str, json_path: str | None) -> bool:
    """True when a report would be stored in conformance-reports/ under the default name."""
    if not json_path or adapter_name != DEFAULT_ADAPTER_NAME:
        return False
    target = Path(json_path).resolve()
    reports = REPORTS_DIR.resolve()
    return target.parent == reports or reports in target.parents


#: How long the adapter's process group is given to end on SIGTERM before it is killed.
_ADAPTER_TERM_GRACE = 5.0
#: How often the group is looked at while it is ending.
_GROUP_POLL = 0.05


def _signal_group(pgid: int, sig: int) -> bool:
    """Send ``sig`` to the process group. False when there is no such group left."""
    try:
        os.killpg(pgid, sig)
    except (ProcessLookupError, PermissionError):
        return False
    return True


def end_process_group(proc: subprocess.Popen, grace: float = _ADAPTER_TERM_GRACE) -> None:
    """End the adapter, and anything it started, before the harness returns.

    ``--adapter-cmd`` is a shell command, so ``proc`` is a shell, and whether that shell IS
    the adapter depends on the shell: some ``/bin/sh`` implementations ``exec`` a simple
    ``-c`` command and some fork and wait for it. Signalling ``proc`` alone therefore ends
    the adapter on one host and, on another, ends the shell and leaves the adapter running,
    re-parented, with the streams and the stdout it inherited still open -- on a robot, a
    live node outliving the run that started it.

    The shell is started in its own session, so the whole command is one process group with
    no one else in it, and the group is what is ended here: SIGTERM, a bounded wait, then
    SIGKILL for whatever is left. The group's id is the shell's pid, which the kernel does
    not hand out again while any member of the group is alive, so the signal cannot reach a
    stranger.
    """
    try:
        pgid = os.getpgid(proc.pid)
    except ProcessLookupError:  # pragma: no cover - reaped between the run and here
        return
    _signal_group(pgid, signal.SIGTERM)
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        # Reap the shell as we go: until it is reaped it is still a member of its own
        # group, and the group would look alive because of a process that has exited.
        proc.poll()
        if not _signal_group(pgid, 0):
            break
        time.sleep(_GROUP_POLL)
    else:
        _signal_group(pgid, signal.SIGKILL)
    # Reap the shell before returning, whichever way the loop above ended. "The group is
    # gone" is not the same statement as "the shell has been reaped": a group whose last
    # member is an exited-but-unreaped shell does not answer signal 0 the same way
    # everywhere -- one host says the group does not exist, another says it may not be
    # signalled -- and on the second reading the loop stops one reap early. A harness that
    # returns there leaves a child of its own behind for the rest of the run.
    try:
        proc.wait(timeout=grace)
    except subprocess.TimeoutExpired:  # pragma: no cover - SIGKILL did not land
        pass


def line_buffer_stdout() -> None:
    """Make stdout line buffered, whatever it is attached to.

    Python block buffers a stdout that is not a terminal, so a run redirected to a file or
    a pipe says nothing until it ends -- and a run that does not end says nothing at all.
    The report of one stalled run was written and then sat in the buffer for eleven
    minutes, until a signal let the interpreter flush it.
    """
    reconfigure = getattr(sys.stdout, "reconfigure", None)
    if reconfigure is not None:
        reconfigure(line_buffering=True)


def main() -> int:
    line_buffer_stdout()
    parser = argparse.ArgumentParser(description="Fleet Adapter conformance harness")
    parser.add_argument("--port", type=int, default=9090,
                        help="port the harness listens on and the adapter dials")
    parser.add_argument("--adapter-name", default=DEFAULT_ADAPTER_NAME,
                        help="the adapter's name, as the registry row names it; required for "
                             "a report written into conformance-reports/")
    parser.add_argument("--adapter-cmd", required=True,
                        help="shell command that launches the adapter; '{port}' is "
                             "substituted with --port")
    parser.add_argument("--stub-path", default="proto",
                        help="directory holding the generated fleet_adapter.v1 stubs "
                             "(added to PYTHONPATH for the harness and the adapter)")
    parser.add_argument("--json", metavar="PATH", default=None,
                        help="also write the machine-readable report to PATH")
    parser.add_argument("--witness-file", metavar="PATH", default=None,
                        help="a motion witness for C5.7: JSON lines of the robot's actual speed, "
                             "written by something other than the adapter's confirmation path "
                             "(the simulation adapter's plant model writes one with its own "
                             "--witness-file). Without it C5.7 is not testable in the run")
    # 180 s: C2.1 waits up to 75 s for the provisioning loop's register that admits the robot
    # (60 s is the loop's interval while the adapter has no TTL), and the run continues after it.
    parser.add_argument("--run-timeout", type=float, default=180.0,
                        help="seconds to allow the whole run before giving up")
    args = parser.parse_args()
    if refuses_default_name(args.adapter_name, args.json):
        print(f"harness: refused: --json {args.json} writes into conformance-reports/ and "
              f"--adapter-name was not given, so the report would name "
              f"{DEFAULT_ADAPTER_NAME!r}. Pass the adapter's registry name",
              file=sys.stderr, flush=True)
        return 2

    # Make the generated stubs importable, then wire them into the harness. The
    # import is deferred so the package loads without stubs present. Tolerate both
    # stub layouts: `fleet_adapter.v1.*` (protoc -Iproto) and
    # `proto.fleet_adapter.v1.*` (protoc -I., the current `make proto`).
    sys.path.insert(0, args.stub_path)
    sys.path.insert(0, ".")
    try:
        from fleet_adapter.v1 import fleet_adapter_pb2 as pb
        from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc
    except ImportError:
        from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
        from proto.fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc

    from . import harness
    harness.pb = pb
    harness.pb_grpc = pb_grpc

    ready = threading.Event()
    box: dict = {}
    handle = harness.RunHandle()

    def _run() -> None:
        box["report"] = harness.run_conformance(args.adapter_name, args.port, ready,
                                                witness_path=args.witness_file,
                                                handle=handle)

    t = threading.Thread(target=_run, daemon=True)
    t.start()

    if not ready.wait(timeout=10.0):
        print("harness: server failed to start", file=sys.stderr, flush=True)
        handle.stop()
        return 2

    env = dict(os.environ)
    env["PYTHONPATH"] = args.stub_path + os.pathsep + env.get("PYTHONPATH", "")
    cmd = args.adapter_cmd.format(port=args.port)
    # start_new_session makes the whole adapter command one process group of its own, which
    # is what end_process_group ends. shell=True stays: an --adapter-cmd is a shell command
    # and a compound one ("a && b") must keep working, which making the shell exec the
    # adapter would break.
    proc = subprocess.Popen(cmd, shell=True, env=env,  # noqa: S602 (trusted, local)
                            start_new_session=True)

    try:
        t.join(timeout=args.run_timeout)
    finally:
        # Every way out of the run goes through here: pass, fail, the --run-timeout path
        # below, and an exception on the way.
        #
        # --run-timeout bounds the PROCESS, not just the checks. The run thread is a daemon
        # and is simply abandoned here, so the server it started must be stopped from
        # outside it: its worker threads are not daemon threads, and a handler still parked
        # on a stream holds the interpreter open long after this function returns. On the
        # ordinary paths the run has already stopped it and this is a no-op.
        handle.stop()
        end_process_group(proc)

    report = box.get("report")
    if report is None:
        print("harness: run did not complete within "
              f"{args.run_timeout}s", file=sys.stderr, flush=True)
        sys.stdout.flush()
        return 2

    print(report.to_text(), flush=True)
    if args.json:
        with open(args.json, "w", encoding="utf-8") as fh:
            fh.write(report.to_json())
        print(f"\nwrote {args.json}", flush=True)

    sys.stdout.flush()
    return 0 if report.conformant else 1


if __name__ == "__main__":
    raise SystemExit(main())
