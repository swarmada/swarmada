# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""A conforming Mcp-binding, App-contract Compute Endpoint. Standard library only.

JSON-RPC 2.0 over ``POST /mcp``, implementing the three methods the binding
needs: ``initialize``, ``tools/list`` and ``tools/call``.

The point of the binding is that the parameter and result contract is
DISCOVERABLE FROM THE SERVER: ``tools/list`` returns the input schema as JSON
Schema, so the declaration cannot drift from the implementation because it IS
the implementation's declaration (CE5.1, CE5.3). This fixture therefore
validates arguments against the very schema it advertises, rather than against a
second copy written out by hand.

Run:  python3 -m conformance.fixtures.plate_reader_mcp --port 8081
"""

from __future__ import annotations

import argparse
import hashlib
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

from .plate_reader_http import MISBEHAVIOURS, Executor

PROTOCOL_VERSION = "2025-06-18"

TOOL_SCHEMA = {
    "type": "object",
    "properties": {
        "invocationId": {"type": "string",
                         "description": "<namespace>/<action>/<runGeneration>/<attempt>"},
        "region": {"type": "string", "enum": ["US", "EU"]},
        "minPlateConfidence": {"type": "number", "minimum": 0.0, "maximum": 1.0},
        "clipUri": {"type": "string"},
    },
    "required": ["invocationId"],
    "additionalProperties": False,
}

TOOL = {
    "name": "read_plate",
    "title": "Read a plate from a clip",
    "description": "Synthetic plate reader. Recognises nothing; derives a plate from the "
                   "invocation id so a test can assert an exact string.",
    "inputSchema": TOOL_SCHEMA,
}


class SchemaError(ValueError):
    pass


def validate(args: dict, schema: dict) -> None:
    """The subset of JSON Schema this fixture's own schema uses.

    Deliberately not a general validator: CE5.3 asks whether the server refuses
    a call violating the schema it published, and a small checker that covers
    exactly the published schema answers that without adding a dependency the
    suite would then require a stranger to install.
    """
    if schema.get("type") == "object":
        if not isinstance(args, dict):
            raise SchemaError("arguments must be an object")
        props = schema.get("properties", {})
        for name in schema.get("required", []):
            if name not in args:
                raise SchemaError(f"missing required argument {name!r}")
        if schema.get("additionalProperties") is False:
            for name in args:
                if name not in props:
                    raise SchemaError(f"unknown argument {name!r}")
        for name, value in args.items():
            spec = props.get(name, {})
            kind = spec.get("type")
            if kind == "string" and not isinstance(value, str):
                raise SchemaError(f"{name} must be a string")
            if kind == "number" and not isinstance(value, (int, float)):
                raise SchemaError(f"{name} must be a number")
            if "enum" in spec and value not in spec["enum"]:
                raise SchemaError(f"{name} must be one of {spec['enum']}")
            if kind == "number":
                if "minimum" in spec and value < spec["minimum"]:
                    raise SchemaError(f"{name} below minimum")
                if "maximum" in spec and value > spec["maximum"]:
                    raise SchemaError(f"{name} above maximum")


class McpServer:
    def __init__(self, misbehave: str = "") -> None:
        self.executor = Executor(misbehave)
        self.misbehave = misbehave
        #: CE5.6 — a consumer holding this digest detects a schema change
        #: rather than discovering it in a failed call.
        self.schema_digest = "sha256:" + hashlib.sha256(
            json.dumps(TOOL_SCHEMA, sort_keys=True).encode()).hexdigest()

    def handle(self, req: dict) -> dict | None:
        method = req.get("method")
        rid = req.get("id")
        if method == "initialize":
            return self._ok(rid, {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {"tools": {"listChanged": True}},
                "serverInfo": {"name": "plate-reader-mcp", "version": "1.0.0"},
            })
        if method == "tools/list":
            return self._ok(rid, {"tools": [TOOL], "_schemaDigest": self.schema_digest})
        if method == "tools/call":
            params = req.get("params") or {}
            if params.get("name") != TOOL["name"]:
                return self._err(rid, -32602, f"unknown tool {params.get('name')!r}")
            args = params.get("arguments") or {}
            try:
                validate(args, TOOL_SCHEMA)
            except SchemaError as exc:
                # A schema violation is a PROTOCOL error, not a tool result: the
                # call never ran, so there is no envelope to report.
                return self._err(rid, -32602, str(exc))
            body = {
                "invocation": {"id": args["invocationId"]},
                "parameters": {k: v for k, v in args.items()
                               if k in ("region", "minPlateConfidence")},
                "inputs": ([{"name": "clip", "uri": args["clipUri"]}]
                           if "clipUri" in args else []),
            }
            env, replayed = self.executor.invoke(body)
            return self._ok(rid, {
                "content": [{"type": "text", "text": json.dumps(env)}],
                "structuredContent": env,
                "isError": env["status"] == "Failed",
                "_idempotentReplay": replayed,
            })
        if method and method.startswith("notifications/"):
            return None
        return self._err(rid, -32601, f"method not found: {method}")

    @staticmethod
    def _ok(rid, result):
        return {"jsonrpc": "2.0", "id": rid, "result": result}

    @staticmethod
    def _err(rid, code, message):
        return {"jsonrpc": "2.0", "id": rid, "error": {"code": code, "message": message}}


class Handler(BaseHTTPRequestHandler):
    server_impl: McpServer

    def log_message(self, fmt, *args):  # noqa: A003
        pass

    def do_POST(self) -> None:  # noqa: N802
        if urlparse(self.path).path != "/mcp":
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length") or 0)
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            payload = json.dumps({"jsonrpc": "2.0", "id": None,
                                  "error": {"code": -32700, "message": "parse error"}}).encode()
        else:
            resp = self.server_impl.handle(req)
            if resp is None:
                self.send_response(202)
                self.end_headers()
                return
            payload = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def serve(port: int, misbehave: str = "") -> tuple[ThreadingHTTPServer, threading.Thread]:
    handler = type("BoundHandler", (Handler,), {"server_impl": McpServer(misbehave)})
    httpd = ThreadingHTTPServer(("127.0.0.1", port), handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd, thread


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--port", type=int, default=8081)
    ap.add_argument("--misbehave", default="", choices=[""] + sorted(MISBEHAVIOURS))
    args = ap.parse_args()
    httpd, thread = serve(args.port, args.misbehave)
    print(f"plate-reader-mcp on 127.0.0.1:{args.port}/mcp", flush=True)
    try:
        thread.join()
    except KeyboardInterrupt:
        httpd.shutdown()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
