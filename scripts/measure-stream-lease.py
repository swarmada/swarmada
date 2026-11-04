#!/usr/bin/env python3
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

"""Measure the partition-to-stream-off interval: kill the controller, time the camera off.

    make stream-lease-measure

# WHAT THIS MEASURES, AND WHY IT IS NOT A CONFORMANCE ROW

The sensor-stream duty cycle claims that the safe direction is OFF: a control-plane
partition STOPS a sensor, and stops it at the lease horizon rather than eventually. That
claim is the inverse of the emergency-stop lease direction and the whole reason a stream
carries a lease at all. Without a number it is an intention.

`adapters/CONFORMANCE.md` C18.3 establishes the DIRECTION — held across renewals, gone
after the horizon — and deliberately does not report an interval, because it cannot. The
only wire observable for stream state is `DeactivateStream`, whose answer distinguishes
"there was a stream and I stopped it" from "there was nothing to stop" — and asking
CONSUMES the activation. A polling loop would therefore switch the stream off with its
first poll and measure its own round trip.

So the interval is measured here instead, on the clock where the stop actually happens.
Two things make it a measurement rather than a self-report:

  * THE CONTROLLER IS REALLY KILLED. This script stands up a control plane, lets the
    adapter register and hold a stream under a renewed lease, and then STOPS THE gRPC
    SERVER — the transport dies, every renewal path with it. It is not a withheld
    renewal over a live socket; the adapter is genuinely talking to nothing. The
    adapter's own reconnect loop keeps trying throughout, which is the realistic case
    and the one where "left on" would actually happen.
  * THE STOP IS TIMESTAMPED BY THE THING THAT STOPS. The adapter prints
    `STREAM_OFF ... reason=lease_expired at=<unix seconds>` from inside its lease
    watchdog, at the moment it drops the stream. Both processes read the same machine
    clock via `time.time()`, so the two readings subtract.

The interval reported is therefore (adapter's stop instant) − (the instant this process
stopped being able to renew), which is what an operator is promised.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
import threading
import time
from concurrent import futures

import grpc

# The stream the measurement drives. front-rgb is the sim adapter's ON_DEMAND,
# clip-capable camera — the case the duty cycle exists for, where activation genuinely
# switches something on and failing to stop it leaves a camera running.
STREAM_ID = "front-rgb"
ROBOT = "robot-lease-measure-1"

# The lease horizon under test. Short enough to run in a make target, long enough that
# several renewals fit inside it and the reading is not dominated by scheduling jitter.
LEASE_S = 2.0
# How long the "controller" stays alive renewing before it is killed. More than one whole
# lease, so a stream that survives to the kill cannot be explained by the original grant.
ALIVE_S = 3.0
# How long to wait for the adapter's STREAM_OFF line before declaring the fail-safe did
# not fire. Generously past the horizon: a miss here is a real finding, not a flake.
OFF_DEADLINE_S = LEASE_S + 10.0

_STREAM_OFF = re.compile(
    r"STREAM_OFF\s+robot=(?P<robot>\S+)\s+stream=(?P<stream>\S+)\s+"
    r"reason=lease_expired\s+at=(?P<at>[0-9.]+)"
)


class _Plane:
    """A minimal control plane: enough to register a robot and hold a stream."""

    def __init__(self, pb, pb_grpc):
        self.pb, self.pb_grpc = pb, pb_grpc
        self.inbox: list = []
        self._cond = threading.Condition()
        self.established = threading.Event()
        self._out = []
        self._out_cond = threading.Condition()
        self._closed = False

    # -- stream plumbing ----------------------------------------------------

    def push(self, msg) -> None:
        with self._out_cond:
            self._out.append(msg)
            self._out_cond.notify_all()

    def _next_out(self):
        with self._out_cond:
            while not self._out and not self._closed:
                self._out_cond.wait(0.1)
            return self._out.pop(0) if self._out else None

    def servicer(self):
        plane = self

        class Servicer(plane.pb_grpc.FleetAdapterServiceServicer):
            def ControlStream(
                self, request_iterator, context
            ):  # noqa: N802 — the name is the generated servicer's
                plane.established.set()

                def reader() -> None:
                    try:
                        for msg in request_iterator:
                            with plane._cond:
                                plane.inbox.append(msg)
                                plane._cond.notify_all()
                    except grpc.RpcError:
                        pass

                threading.Thread(target=reader, daemon=True).start()
                while not plane._closed:
                    out = plane._next_out()
                    if out is not None:
                        yield out

            def SafetyStream(self, request_iterator, context):  # noqa: N802 — as above
                # Held open and silent. The adapter opens both streams before it will
                # register at all, so refusing this one would prevent the run rather
                # than measure it.
                def reader() -> None:
                    try:
                        for _ in request_iterator:
                            pass
                    except grpc.RpcError:
                        pass

                threading.Thread(target=reader, daemon=True).start()
                while not plane._closed:
                    time.sleep(0.05)

        return Servicer()

    def await_msg(self, kind: str, timeout: float = 10.0):
        """Wait for the next inbound AdapterMessage of `kind`, consuming earlier ones."""
        deadline = time.time() + timeout
        with self._cond:
            while time.time() < deadline:
                for i, msg in enumerate(self.inbox):
                    if msg.WhichOneof("payload") == kind:
                        return self.inbox.pop(i)
                self._cond.wait(0.1)
        raise TimeoutError(f"no {kind} from the adapter within {timeout}s")

    def await_msg_any(self, kinds, timeout: float = 10.0):
        """Wait for the next inbound message of ANY of `kinds`, or None on timeout.

        The onboarding order is not this script's to decide: an adapter meeting a control
        plane for the first time announces itself (DiscoverRobot); one whose Robot CRD
        already exists re-registers. Both are conformant and the run must survive either.
        """
        deadline = time.time() + timeout
        with self._cond:
            while time.time() < deadline:
                for i, msg in enumerate(self.inbox):
                    if msg.WhichOneof("payload") in kinds:
                        return self.inbox.pop(i)
                self._cond.wait(0.1)
        return None

    def close(self) -> None:
        self._closed = True
        with self._out_cond:
            self._out_cond.notify_all()


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("--port", type=int, default=9097)
    ap.add_argument("--stub-path", default="proto")
    ap.add_argument(
        "--adapter-cmd",
        default=None,
        help="command launching the adapter; '{port}' is substituted. "
        "Defaults to the in-tree simulation adapter.",
    )
    ap.add_argument("--lease-seconds", type=float, default=LEASE_S)
    args = ap.parse_args()

    sys.path.insert(0, args.stub_path)
    sys.path.insert(0, ".")
    try:
        from fleet_adapter.v1 import fleet_adapter_pb2 as pb
        from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc
    except ImportError:
        from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
        from proto.fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc

    adapter_cmd = args.adapter_cmd or (
        f"{sys.executable} adapters/simulation/sim_adapter.py "
        f"--endpoint localhost:{{port}} --robot-id {ROBOT}"
    )

    plane = _Plane(pb, pb_grpc)
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    pb_grpc.add_FleetAdapterServiceServicer_to_server(plane.servicer(), server)
    server.add_insecure_port(f"[::]:{args.port}")
    server.start()

    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(
        [args.stub_path, ".", "sdk/python/src", env.get("PYTHONPATH", "")]
    )
    # stderr is CAPTURED, not inherited: the adapter's STREAM_OFF marker carries the
    # timestamp this whole measurement turns on, and a line printed to the terminal is a
    # line this process cannot read.
    proc = subprocess.Popen(
        adapter_cmd.format(port=args.port),
        shell=True,
        env=env,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )

    stop_line: dict = {}
    adapter_log: list = []

    def drain_stderr() -> None:
        for line in proc.stderr:
            adapter_log.append(line.rstrip())
            m = _STREAM_OFF.search(line)
            if m and m.group("stream") == STREAM_ID and "at" not in stop_line:
                stop_line["at"] = float(m.group("at"))
                stop_line["line"] = line.rstrip()

    threading.Thread(target=drain_stderr, daemon=True).start()

    rc = 1
    try:
        rc = _run(plane, server, pb, args, stop_line)
    except Exception as exc:  # noqa: BLE001 — the failure IS the result here
        print(f"\nMEASUREMENT FAILED: {exc}", file=sys.stderr)
        print("\n-- adapter output --", file=sys.stderr)
        for line in adapter_log[-40:]:
            print("   " + line, file=sys.stderr)
    finally:
        plane.close()
        server.stop(grace=0.5)
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
    return rc


def _run(plane, server, pb, args, stop_line: dict) -> int:
    lease_s = args.lease_seconds
    if not plane.established.wait(20.0):
        raise RuntimeError("the adapter never opened a ControlStream")

    hello = plane.await_msg("hello")
    plane.push(
        pb.ControlPlaneMessage(
            hello_ack=pb.HelloAck(
                accepted=True,
                negotiated_protocol_version="fleet_adapter.v1",
                negotiated_contract_version=hello.hello.contract_version,
            )
        )
    )

    # Onboarding. An adapter meeting a control plane for the first time announces itself
    # (DiscoverRobot) and registers once admitted; one whose robot is already known
    # registers straight away. Answer whichever arrives, then wait for the registration
    # — without it the adapter never starts its loops and there is nothing to switch on.
    first = plane.await_msg_any(("discover", "register"), timeout=20.0)
    if first is None:
        raise RuntimeError("the adapter neither announced nor registered a robot")
    if first.WhichOneof("payload") == "discover":
        plane.push(
            pb.ControlPlaneMessage(
                discover_ack=pb.DiscoverAck(accepted=True, discovered_robot_name=ROBOT)
            )
        )
        if plane.await_msg_any(("register",), timeout=20.0) is None:
            raise RuntimeError("the adapter was admitted but never registered")
    plane.push(
        pb.ControlPlaneMessage(
            register_ack=pb.RegisterAck(accepted=True, telemetry_interval_seconds=1)
        )
    )

    def activate(token: int):
        plane.push(
            pb.ControlPlaneMessage(
                command=pb.Command(
                    command_id=token,
                    robot_id=ROBOT,
                    activate_stream=pb.ActivateStream(
                        robot_id=ROBOT,
                        stream_id=STREAM_ID,
                        rate_hz=5.0,
                        sinks=[pb.StreamSink(address="127.0.0.1:59999")],
                        fencing_token=7,
                        lease_expires_ms=int((time.time() + lease_s) * 1000),
                    ),
                )
            )
        )

    activate(1000)
    res = plane.await_msg("command_result", timeout=15.0)
    if not res.command_result.activate_stream.activated:
        raise RuntimeError(
            f"the adapter refused to activate {STREAM_ID!r}: "
            f"{res.command_result.activate_stream.message!r}"
        )
    print(f"stream {STREAM_ID!r} ACTIVE under a {lease_s:.3f}s lease", file=sys.stderr)

    # ── The controller is alive: renew well inside the horizon. ──
    cid = 1001
    alive_until = time.time() + ALIVE_S
    while time.time() < alive_until:
        time.sleep(lease_s / 3)
        activate(cid)
        cid += 1
        plane.await_msg("command_result", timeout=15.0)
        if "at" in stop_line:
            raise RuntimeError(
                "the stream stopped WHILE renewals were arriving — it stops on a timer "
                "rather than on the lease, which abandons live work"
            )

    # ── KILL THE CONTROLLER. ──
    #
    # HARD, not clean, and the difference decides what is being measured. A graceful
    # server-side close is a legitimate "we are done": the sim adapter (correctly) shuts
    # itself down on it, and a measurement taken that way would be timing a process exit
    # — of course a dead process holds no camera on. `server.stop(0)` cancels the RPCs
    # instead, so the adapter sees UNAVAILABLE, enters its reconnect loop, and keeps
    # trying to reach a control plane that is not there. That is what a crashed
    # controller looks like from a robot, and it is the only state in which "left on"
    # could actually happen.
    partition_at = time.time()
    server.stop(0)
    print(f"CONTROLLER KILLED at {partition_at:.6f} (RPCs cancelled, port closed)", file=sys.stderr)

    deadline = partition_at + OFF_DEADLINE_S
    while time.time() < deadline and "at" not in stop_line:
        time.sleep(0.01)

    if "at" not in stop_line:
        raise RuntimeError(
            f"{STREAM_ID!r} was STILL RUNNING {OFF_DEADLINE_S:.1f}s after the controller "
            f"died, against a {lease_s:.3f}s lease. A control-plane partition has left a "
            f"sensor on with nothing watching it — the failure the lease direction exists "
            f"to prevent."
        )

    interval = stop_line["at"] - partition_at
    print()
    print("=" * 72)
    print("  MEASURED partition-to-stream-off interval")
    print("=" * 72)
    print(f"  controller killed at   {partition_at:.6f} (this process, time.time())")
    print(f"  stream stopped at      {stop_line['at']:.6f} (the adapter's own clock)")
    print(f"  INTERVAL               {interval:.3f}s")
    print(f"  lease duration         {lease_s:.3f}s")
    print(
        f"  overshoot              {interval - lease_s:+.3f}s "
        f"(the adapter's watchdog granularity)"
    )
    print(f"  adapter marker         {stop_line['line']}")
    print("=" * 72)

    # The stop must land AT the horizon, not merely somewhere before a timeout:
    # "eventually" is not a fail-safe. The lower bound matters as much as the upper —
    # stopping early would mean abandoning a stream a live controller was still renewing.
    if not (lease_s - 0.1 <= interval <= lease_s + 1.0):
        print(
            f"\nFAIL: {interval:.3f}s is outside [{lease_s - 0.1:.3f}, {lease_s + 1.0:.3f}]s — "
            f"the stream did not stop AT the lease horizon",
            file=sys.stderr,
        )
        return 1
    print("\nPASS: the stream powered off at its lease horizon after the controller died.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
