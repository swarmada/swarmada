# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""A conforming Http-binding, App-contract Compute Endpoint. Standard library only.

Implements the envelope in COMPUTE-ENDPOINT.md CE2 and CE3:

    POST /v1/invoke   {invocation:{id,deadlineMs}, parameters:{}, inputs:[], sinks:[]}
    ->                {status, values, artifacts, events, message}
    GET  /healthz     {"status":"ok"}

Run:  python3 -m conformance.fixtures.plate_reader_http --port 8080
"""

from __future__ import annotations

import argparse
import hashlib
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

_ALPHABET = "ABCDEFGHJKLMNPRSTUVWXYZ"  # no I, O, Q — as most real plate schemes omit them

#: Deliberate defects the fixture can be asked to exhibit, so the checks that
#: catch them can be seen to fail. Named for the check each one breaks.
MISBEHAVIOURS = {
    "prefix-dedup":  "CE1.6 — deduplicate on <namespace>/<action> instead of the whole key",
    "command":       "CE2.5 — return a field interpretable as an instruction",
    "oversize":      "CE2.3 — exceed the 4 KiB / 16-entry values bound",
    "empty-is-fail": "CE2.2 — report 'worked, found nothing' as Failed",
    "no-replay":     "CE1.2 — redo the work for a repeated invocation key",
}


def synth_plate(invocation_id: str) -> tuple[str, float]:
    digest = hashlib.sha256(invocation_id.encode("utf-8")).digest()
    letters = "".join(_ALPHABET[b % len(_ALPHABET)] for b in digest[:3])
    digits = "".join(str(b % 10) for b in digest[3:7])
    confidence = 0.55 + (digest[7] / 255.0) * 0.44  # 0.55 .. 0.99
    return f"{letters}{digits}", round(confidence, 3)


class Executor:
    """The envelope logic, transport-free so the MCP fixture can reuse it."""

    #: The identity of the bytes serving the run (CE8.1). A fixture has no image,
    #: so it reports the digest of its own source — which is the same property
    #: (a run is attributable to specific bytes) at fixture scale.
    def __init__(self, misbehave: str = "") -> None:
        self.misbehave = misbehave
        self._seen: dict[str, dict] = {}
        self._work_count: dict[str, int] = {}
        src = __file__
        with open(src, "rb") as fh:
            self.build_digest = "sha256:" + hashlib.sha256(fh.read()).hexdigest()

    def _key(self, invocation_id: str) -> str:
        if self.misbehave == "prefix-dedup":
            return "/".join(invocation_id.split("/")[:2])
        return invocation_id

    def invoke(self, body: dict, fail: str = "") -> tuple[dict, bool]:
        """Return (envelope, replayed)."""
        inv = body.get("invocation") or {}
        inv_id = inv.get("id", "")
        key = self._key(inv_id)

        if key in self._seen and self.misbehave != "no-replay":
            return self._seen[key], True

        if fail == "slow":
            time.sleep(5)
        elif fail == "timeout":
            deadline = inv.get("deadlineMs")
            # CE1.4: honour the deadline rather than hanging. The 'timeout' mode
            # simulates slow work that still RETURNS by the deadline.
            budget = 30.0
            if deadline:
                budget = max(0.0, deadline / 1000.0 - time.time())
            time.sleep(min(budget, 30.0))
            return {"status": "Failed", "values": {}, "artifacts": [], "events": [],
                    "message": "deadline reached before completion"}, False
        elif fail == "error":
            return {"status": "Failed", "values": {}, "artifacts": [], "events": [],
                    "message": "injected failure"}, False

        params = body.get("parameters") or {}
        threshold = float(params.get("minPlateConfidence", 0.7))
        plate, confidence = synth_plate(inv_id)
        self._work_count[key] = self._work_count.get(key, 0) + 1
        attempt = self._work_count[key]

        if confidence < threshold:
            status = "Failed" if self.misbehave == "empty-is-fail" else "Succeeded"
            env = {
                "status": status,
                "values": {"confidence": confidence, "workCount": attempt},
                "artifacts": [],
                "events": [{"type": "NoPlateFound", "payload": {"confidence": confidence}}],
                "message": f"below minPlateConfidence {threshold}",
                "producedBy": {"buildDigest": self.build_digest},
            }
            self._seen[key] = env
            return env, False

        clip = next((i for i in (body.get("inputs") or []) if i.get("name") == "clip"), {})
        values = {
            "plate": plate,
            "confidence": confidence,
            # rowId is the observable CE1.3 uses: it changes only when the work
            # is actually redone, so a cached BODY with a fresh row is visible.
            "rowId": "det-" + hashlib.sha256(f"{key}#{attempt}".encode()).hexdigest()[:8],
            "workCount": attempt,
        }
        if self.misbehave == "command":
            values["desiredState"] = "drive to the loading dock"
        if self.misbehave == "oversize":
            values.update({f"pad{i}": "x" * 400 for i in range(20)})

        env = {
            "status": "Succeeded",
            "values": values,
            "artifacts": [{
                "name": "frame",
                "uri": (clip.get("uri") or "") + "#frame-0",
                "mediaType": "image/jpeg",
                "digest": "sha256:" + hashlib.sha256(inv_id.encode()).hexdigest(),
                "capturedAt": clip.get("capturedAt"),
                "pose": clip.get("pose"),
            }],
            "events": [{"type": "PlateRead", "payload": {"plate": plate}}],
            "message": "",
            # CE2.8 / CE8.2: the result is model-derived, so it says which model.
            "producedBy": {"model": "plate-reader-synthetic", "modelVersion": "1.0.0",
                           "buildDigest": self.build_digest},
        }
        self._seen[key] = env
        return env, False


class Handler(BaseHTTPRequestHandler):
    executor: Executor

    def log_message(self, fmt, *args):  # noqa: A003 - quiet fixture
        pass

    def _json(self, code: int, body: dict, headers: dict | None = None) -> None:
        payload = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802
        if urlparse(self.path).path == "/healthz":
            self._json(200, {"status": "ok"})
        else:
            self._json(404, {"status": "Failed", "message": "no such path"})

    def do_POST(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        if parsed.path != "/v1/invoke":
            self._json(404, {"status": "Failed", "message": "no such path"})
            return
        if "application/json" not in (self.headers.get("Content-Type") or ""):
            self._json(415, {"status": "Failed", "message": "expected application/json"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self._json(400, {"status": "Failed", "message": "malformed body"})
            return
        fail = (parse_qs(parsed.query).get("fail") or [""])[0]
        env, replayed = self.executor.invoke(body, fail)
        headers = {"Swarmada-Idempotent-Replay": "true"} if replayed else {}
        self._json(200, env, headers)


def serve(port: int, misbehave: str = "") -> tuple[ThreadingHTTPServer, threading.Thread]:
    handler = type("BoundHandler", (Handler,), {"executor": Executor(misbehave)})
    httpd = ThreadingHTTPServer(("127.0.0.1", port), handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd, thread


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--port", type=int, default=8080)
    ap.add_argument("--misbehave", default="", choices=[""] + sorted(MISBEHAVIOURS))
    args = ap.parse_args()
    httpd, thread = serve(args.port, args.misbehave)
    print(f"plate-reader-http on 127.0.0.1:{args.port} "
          f"{'(misbehaving: ' + args.misbehave + ')' if args.misbehave else ''}",
          flush=True)
    try:
        thread.join()
    except KeyboardInterrupt:
        httpd.shutdown()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
