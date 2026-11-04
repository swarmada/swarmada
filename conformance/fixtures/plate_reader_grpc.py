# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""A conforming Grpc-binding Compute Endpoint: compute.v1.ComputeRuntimeService.

The topology is the INVERSE of fleet_adapter.v1 — the control plane is the
CLIENT and this fixture is the SERVER (compute.proto's topology note). A robot
sits behind NAT and dials out; an endpoint is addressable by construction, so
nothing dials back.

Serves the App contract by default. ``--contract service`` makes it resident so
the Service-contract checks (CE7) have something to drive: it holds a lease, it
self-stops when renewal ceases, and it emits TaskEvents on StreamStatus as it
observes them rather than in a reply.

Run:  python3 -m conformance.fixtures.plate_reader_grpc --port 8082
"""

from __future__ import annotations

import argparse
import queue
import threading
import time

import grpc

from .plate_reader_http import MISBEHAVIOURS, Executor

# Import style mirrors adapters/conformance/__main__.py: tolerate both stub
# layouts so a stranger who ran either `make proto` invocation can run this.
try:
    from compute.v1 import compute_pb2 as pb
    from compute.v1 import compute_pb2_grpc as pb_grpc
    from fleet_adapter.v1 import fleet_adapter_pb2 as fa
except ImportError:  # pragma: no cover - layout fallback
    from proto.compute.v1 import compute_pb2 as pb
    from proto.compute.v1 import compute_pb2_grpc as pb_grpc
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as fa

#: A resident run with no renewal self-stops after this long. Short because the
#: safe direction is OFF and a fixture should demonstrate that in a test's
#: lifetime, not a deployment's.
DEFAULT_LEASE_MS = 1500


class Run:
    def __init__(self, invocation_id: str, run_generation: int) -> None:
        self.invocation_id = invocation_id
        self.run_generation = run_generation
        self.seq = 0
        self.events: list[fa.TaskEvent] = []
        self.subscribers: list[queue.Queue] = []
        self.lease_expires_ms = int(time.time() * 1000) + DEFAULT_LEASE_MS
        self.stopped_reason = ""
        self.observations = 0
        self.started_ms = int(time.time() * 1000)

    def raise_event(self, event_type: str, payload: dict[str, str],
                    terminal: bool = False) -> fa.TaskEvent:
        # CE6.2 / E10.2: a RESIDENT executor assigns seq itself, strictly
        # monotonic per run, never reset, never reused.
        self.seq += 1
        ev = fa.TaskEvent(type=event_type, seq=self.seq,
                          time_ms=int(time.time() * 1000), terminal=terminal)
        for k, v in payload.items():
            ev.payload[k] = v
        self.events.append(ev)
        for q in list(self.subscribers):
            q.put(ev)
        return ev


class Service(pb_grpc.ComputeRuntimeServiceServicer):
    def __init__(self, contract: str = "app", misbehave: str = "") -> None:
        self.contract = contract
        self.misbehave = misbehave
        self.executor = Executor(misbehave)
        self.runs: dict[str, Run] = {}
        self.results: dict[str, pb.ComputeResult] = {}
        self.cancelled: set[str] = set()
        self.lock = threading.Lock()
        self._stop = threading.Event()
        if contract == "service":
            threading.Thread(target=self._reap, daemon=True).start()

    # ── lease reaper: the safe direction is off ──────────────────────────────
    def _reap(self) -> None:
        while not self._stop.wait(0.1):
            now = int(time.time() * 1000)
            with self.lock:
                for run in list(self.runs.values()):
                    if run.stopped_reason or run.lease_expires_ms > now:
                        continue
                    run.stopped_reason = "lease-expired"
                    run.raise_event("RunSelfStopped", {"reason": "lease-expired"},
                                    terminal=True)
                    for q in list(run.subscribers):
                        q.put(None)

    # ── RPCs ─────────────────────────────────────────────────────────────────
    def Activate(self, request, context):  # noqa: N802
        inv = request.invocation
        with self.lock:
            existing = self.runs.get(inv.id)
            if existing is not None:
                # CE4.2 — idempotently RE-ACK; do not start a second run, and do
                # not let a lower run_generation supersede the one in flight.
                return pb.ActivateResponse(
                    accepted=True,
                    message=f"idempotent re-ack of invocation {inv.id} "
                            f"(run_generation {existing.run_generation})")
            run = Run(inv.id, inv.run_generation)
            self.runs[inv.id] = run

        if self.contract == "service":
            threading.Thread(target=self._observe, args=(run,), daemon=True).start()
        else:
            body = {"invocation": {"id": inv.id, "deadlineMs": inv.deadline_ms},
                    "parameters": dict(request.parameters), "inputs": []}
            env, _ = self.executor.invoke(body)
            result = pb.ComputeResult(
                status=env["status"], message=env["message"],
                produced_at_ms=int(time.time() * 1000))
            for k, v in env["values"].items():
                result.values[k] = str(v)
            for art in env["artifacts"]:
                result.artifacts.append(pb.ComputeArtifact(
                    name=art["name"], uri=art["uri"] or "",
                    media_type=art["mediaType"], digest=art["digest"]))
            with self.lock:
                self.results[inv.id] = result
                for ev in env["events"]:
                    run.raise_event(ev["type"], {k: str(v) for k, v in
                                                 (ev.get("payload") or {}).items()})
                run.stopped_reason = "completed"
                for q in list(run.subscribers):
                    q.put(None)
        # CE1.5 / CE4.2 — return PROMPTLY. Progress is observed on a later
        # reconcile via StreamStatus/Result, never by holding this call open.
        return pb.ActivateResponse(accepted=True, message="activated")

    def _observe(self, run: Run) -> None:
        while not self._stop.is_set():
            with self.lock:
                if run.stopped_reason:
                    return
                run.observations += 1
                n = run.observations
            run.raise_event("FrameObserved", {"n": str(n)})
            time.sleep(0.2)

    def StreamStatus(self, request, context):  # noqa: N802
        with self.lock:
            run = self.runs.get(request.invocation_id)
            if run is None:
                context.abort(grpc.StatusCode.NOT_FOUND, "unknown invocation")
            # CE4.3 — resume from since_seq. Re-sending an already-delivered
            # (invocation, seq) is correct and safe (E3.2).
            backlog = [e for e in run.events if e.seq > request.since_seq]
            q: queue.Queue = queue.Queue()
            run.subscribers.append(q)
            done = bool(run.stopped_reason)
        try:
            for ev in backlog:
                yield ev
            if done:
                return
            while True:
                ev = q.get()
                if ev is None:
                    return
                yield ev
        finally:
            with self.lock:
                if q in run.subscribers:
                    run.subscribers.remove(q)

    def Result(self, request, context):  # noqa: N802
        with self.lock:
            run = self.runs.get(request.invocation_id)
            if run is None:
                context.abort(grpc.StatusCode.NOT_FOUND, "unknown invocation")
            if self.contract == "app":
                result = self.results.get(request.invocation_id)
                if result is None:
                    context.abort(grpc.StatusCode.FAILED_PRECONDITION, "no terminal result yet")
                return result
            # CE4.4 / CE7.5 — a Service run reports a SUMMARY: counts, duration,
            # last observation. Never the observations themselves; this is not a
            # per-tick channel.
            summary = pb.ComputeResult(
                status="Succeeded" if not run.stopped_reason or
                       run.stopped_reason == "completed" else "Succeeded",
                message=f"resident summary ({run.stopped_reason or 'running'})",
                produced_at_ms=int(time.time() * 1000))
            summary.values["observations"] = str(run.observations)
            summary.values["durationMs"] = str(int(time.time() * 1000) - run.started_ms)
            summary.values["lastEventSeq"] = str(run.seq)
            return summary

    def Cancel(self, request, context):  # noqa: N802
        with self.lock:
            run = self.runs.get(request.invocation_id)
            if run is None:
                return pb.CancelResponse(
                    acknowledged=False, message="unknown invocation",
                    disposition=fa.CANCEL_DISPOSITION_UNSPECIFIED)
            already = run.stopped_reason == "completed"
            run.stopped_reason = run.stopped_reason or "cancelled"
            for q in list(run.subscribers):
                q.put(None)
        return pb.CancelResponse(
            acknowledged=True, message=request.reason or "cancelled",
            disposition=(fa.CANCEL_DISPOSITION_COMPLETED if already
                         else fa.CANCEL_DISPOSITION_STOPPED_SAFELY))

    def RenewLease(self, request, context):  # noqa: N802
        with self.lock:
            run = self.runs.get(request.invocation_id)
            if run is None or run.stopped_reason:
                return pb.RenewLeaseResponse(renewed=False, message="run is not live")
            run.lease_expires_ms = request.lease_expires_ms
        return pb.RenewLeaseResponse(renewed=True, message="renewed")


def serve(port: int, contract: str = "app", misbehave: str = ""):
    server = grpc.server(__import__("concurrent.futures", fromlist=["ThreadPoolExecutor"])
                         .ThreadPoolExecutor(max_workers=8))
    service = Service(contract, misbehave)
    pb_grpc.add_ComputeRuntimeServiceServicer_to_server(service, server)
    server.add_insecure_port(f"127.0.0.1:{port}")
    server.start()
    return server, service


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--port", type=int, default=8082)
    ap.add_argument("--contract", default="app", choices=["app", "service"])
    ap.add_argument("--misbehave", default="", choices=[""] + sorted(MISBEHAVIOURS))
    args = ap.parse_args()
    server, _ = serve(args.port, args.contract, args.misbehave)
    print(f"plate-reader-grpc on 127.0.0.1:{args.port} (contract {args.contract})", flush=True)
    server.wait_for_termination()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
