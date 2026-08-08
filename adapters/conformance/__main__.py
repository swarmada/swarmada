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
import subprocess
import sys
import threading


def main() -> int:
    parser = argparse.ArgumentParser(description="Fleet Adapter conformance harness")
    parser.add_argument("--port", type=int, default=9090,
                        help="port the harness listens on and the adapter dials")
    parser.add_argument("--adapter-name", default="adapter-under-test")
    parser.add_argument("--adapter-cmd", required=True,
                        help="shell command that launches the adapter; '{port}' is "
                             "substituted with --port")
    parser.add_argument("--stub-path", default="proto",
                        help="directory holding the generated fleet_adapter.v1 stubs "
                             "(added to PYTHONPATH for the harness and the adapter)")
    parser.add_argument("--json", metavar="PATH", default=None,
                        help="also write the machine-readable report to PATH")
    parser.add_argument("--run-timeout", type=float, default=60.0,
                        help="seconds to allow the whole run before giving up")
    args = parser.parse_args()

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

    def _run() -> None:
        box["report"] = harness.run_conformance(args.adapter_name, args.port, ready)

    t = threading.Thread(target=_run, daemon=True)
    t.start()

    if not ready.wait(timeout=10.0):
        print("harness: server failed to start", file=sys.stderr)
        return 2

    env = dict(os.environ)
    env["PYTHONPATH"] = args.stub_path + os.pathsep + env.get("PYTHONPATH", "")
    cmd = args.adapter_cmd.format(port=args.port)
    proc = subprocess.Popen(cmd, shell=True, env=env)  # noqa: S602 (trusted, local)

    t.join(timeout=args.run_timeout)
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()

    report = box.get("report")
    if report is None:
        print("harness: run did not complete within "
              f"{args.run_timeout}s", file=sys.stderr)
        return 2

    print(report.to_text())
    if args.json:
        with open(args.json, "w", encoding="utf-8") as fh:
            fh.write(report.to_json())
        print(f"\nwrote {args.json}")

    return 0 if report.conformant else 1


if __name__ == "__main__":
    raise SystemExit(main())
