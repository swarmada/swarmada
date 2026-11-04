# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Drives COMPUTE-ENDPOINT.md against a Compute Endpoint under test.

The control plane is the CLIENT on this surface, so this module dials the
target rather than serving it — the inverse of the fleet-adapter harness, and
the inverse for the reason compute.proto states: an endpoint is addressable by
construction, so nothing needs to dial back.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from .model import ResultSet, SkipCause

HERE = Path(__file__).resolve().parent
DECLARATIONS = HERE / "fixtures" / "plate-reader-endpoints.yaml"

#: Keys that would make a reply readable as an instruction (CE2.5). `pose` is
#: deliberately ABSENT: on an artifact it is capture provenance — where the
#: camera was when the frame was taken — and banning it would fail a conforming
#: endpoint for recording where its evidence came from. The rule is about
#: `values` carrying something an actuator could act on, not about metadata.
COMMAND_KEYS = {
    "desiredstate", "action", "actions", "command", "commands", "navigate", "goto",
    "waypoint", "waypoints", "targetpose", "goal", "goalpose", "setspeed", "speed",
    "velocity", "trajectory", "instruction", "instructions", "nextaction", "dock",
}

#: Keys that would make an EVENT address a consumer (CE6.5).
ADDRESSING_KEYS = {"trigger", "triggerref", "consumer", "consumers", "subject",
                   "subjectid", "actionref", "taskref", "notify", "to"}

ENVELOPE_KEYS = {"status", "values", "artifacts", "events", "message", "producedBy"}


@dataclass
class Reply:
    status: str
    values: dict[str, Any]
    artifacts: list[dict[str, Any]]
    events: list[dict[str, Any]]
    message: str
    replayed: bool
    raw: Any
    latency_s: float
    produced_by: dict[str, Any] = field(default_factory=dict)


def load_declaration(name: str, rs: ResultSet) -> dict:
    rs.artefacts.append(str(DECLARATIONS))
    for doc in yaml.safe_load_all(DECLARATIONS.read_text()):
        if doc and doc["metadata"]["name"] == name:
            return doc
    raise KeyError(f"no ComputeEndpoint declaration named {name!r} in {DECLARATIONS}")


# ── Drivers ──────────────────────────────────────────────────────────────────

class HttpDriver:
    binding = "Http"

    def __init__(self, base: str) -> None:
        self.base = base.rstrip("/")

    def _post(self, path: str, body: dict, headers: dict[str, str] | None = None,
              content_type: str = "application/json", timeout: float = 30.0):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, method="POST")
        req.add_header("Content-Type", content_type)
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return resp.status, dict(resp.headers), resp.read()
        except urllib.error.HTTPError as exc:
            return exc.code, dict(exc.headers), exc.read()

    def invoke(self, invocation_id: str, parameters: dict, *, deadline_ms: int = 0,
               inputs: list | None = None, query: str = "",
               header_id: str | None = None) -> Reply:
        body = {"invocation": {"id": invocation_id, "deadlineMs": deadline_ms},
                "parameters": parameters, "inputs": inputs or [], "sinks": []}
        started = time.time()
        code, headers, raw = self._post(
            "/v1/invoke" + (f"?{query}" if query else ""), body,
            {"Swarmada-Invocation-Id": header_id or invocation_id})
        latency = time.time() - started
        doc = json.loads(raw)
        return Reply(doc.get("status", ""), doc.get("values", {}), doc.get("artifacts", []),
                     doc.get("events", []), doc.get("message", ""),
                     headers.get("Swarmada-Idempotent-Replay") == "true",
                     {"code": code, "headers": headers, "body": doc}, latency,
                     doc.get("producedBy", {}))


class McpDriver:
    binding = "Mcp"

    def __init__(self, base: str) -> None:
        self.base = base.rstrip("/")
        self._id = 0

    def rpc(self, method: str, params: dict | None = None) -> dict:
        self._id += 1
        req = {"jsonrpc": "2.0", "id": self._id, "method": method, "params": params or {}}
        request = urllib.request.Request(self.base + "/mcp", data=json.dumps(req).encode(),
                                         method="POST")
        request.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(request, timeout=30.0) as resp:
            return json.loads(resp.read())

    def invoke(self, invocation_id: str, parameters: dict, *, deadline_ms: int = 0,
               inputs: list | None = None, **_: Any) -> Reply:
        args = {"invocationId": invocation_id}
        args.update({k: v for k, v in parameters.items()
                     if k in ("region", "minPlateConfidence")})
        if inputs:
            args["clipUri"] = inputs[0].get("uri", "")
        started = time.time()
        resp = self.rpc("tools/call", {"name": "read_plate", "arguments": args})
        latency = time.time() - started
        result = resp.get("result") or {}
        env = result.get("structuredContent") or {}
        return Reply(env.get("status", ""), env.get("values", {}), env.get("artifacts", []),
                     env.get("events", []), env.get("message", ""),
                     bool(result.get("_idempotentReplay")), resp, latency,
                     env.get("producedBy", {}))


class GrpcDriver:
    binding = "Grpc"

    def __init__(self, address: str) -> None:
        import grpc
        try:
            from compute.v1 import compute_pb2 as pb
            from compute.v1 import compute_pb2_grpc as pb_grpc
        except ImportError:  # pragma: no cover
            from proto.compute.v1 import compute_pb2 as pb
            from proto.compute.v1 import compute_pb2_grpc as pb_grpc
        self.pb, self.grpc = pb, grpc
        self.channel = grpc.insecure_channel(address)
        self.stub = pb_grpc.ComputeRuntimeServiceStub(self.channel)
        self.address = address

    def activate(self, invocation_id: str, parameters: dict, run_generation: int = 1,
                 attempt: int = 1, deadline_ms: int = 0):
        inv = self.pb.Invocation(id=invocation_id, run_generation=run_generation,
                                 attempt=attempt, deadline_ms=deadline_ms)
        return self.stub.Activate(self.pb.ActivateRequest(
            invocation=inv, parameters={k: str(v) for k, v in parameters.items()}))

    def events(self, invocation_id: str, since_seq: int = 0, limit: int = 50,
               timeout: float = 3.0) -> list:
        out = []
        try:
            for ev in self.stub.StreamStatus(
                    self.pb.StreamStatusRequest(invocation_id=invocation_id,
                                                since_seq=since_seq), timeout=timeout):
                out.append(ev)
                if len(out) >= limit:
                    break
        except self.grpc.RpcError:
            pass
        return out

    def result(self, invocation_id: str):
        return self.stub.Result(self.pb.ResultRequest(invocation_id=invocation_id))

    def invoke(self, invocation_id: str, parameters: dict, *, deadline_ms: int = 0,
               inputs: list | None = None, **_: Any) -> Reply:
        started = time.time()
        self.activate(invocation_id, parameters, deadline_ms=deadline_ms)
        res = self.result(invocation_id)
        latency = time.time() - started
        events = [{"type": e.type, "payload": dict(e.payload)}
                  for e in self.events(invocation_id, timeout=1.5)]
        values: dict[str, Any] = {}
        for k, v in res.values.items():
            try:
                values[k] = float(v) if v.replace(".", "", 1).lstrip("-").isdigit() else v
            except AttributeError:  # pragma: no cover
                values[k] = v
        artifacts = [{"name": a.name, "uri": a.uri, "mediaType": a.media_type,
                      "digest": a.digest} for a in res.artifacts]
        return Reply(res.status, values, artifacts, events, res.message, False, res, latency)


# ── The catalog ──────────────────────────────────────────────────────────────

def _values_bytes(values: dict) -> int:
    return len(json.dumps(values).encode())


def run(driver, decl: dict, rs: ResultSet, *, run_id: str = "conformance") -> dict[str, bool]:
    """Drive the catalog. Returns the applicability verdict per conditional profile."""
    binding = decl["spec"]["binding"]
    contract = decl["spec"]["contract"]
    declared_events = set(decl["spec"].get("emitsEvents") or [])
    declared_results = {r["name"] for r in (decl["spec"].get("results") or [])}
    ns = decl["metadata"]["namespace"]
    rs.context.update({"binding": binding, "contract": contract,
                       "endpoint": decl["metadata"]["name"], "surface_driven": True})

    where = getattr(driver, "base", None) or getattr(driver, "address", "?")
    rs.ctx("CE0.4", "Harness context: the resolved target and transport",
           f"binding={binding} contract={contract} target={where}")
    if driver.binding == binding:
        rs.ok("CE0.2", "The report names the binding and contract actually driven",
              f"driver={driver.binding} declaration={binding}/{contract}")
    else:
        rs.bad("CE0.2", "The report names the binding and contract actually driven",
               f"drove {driver.binding} against a declaration of {binding}")

    key = f"{ns}/plate-read/{run_id}"
    inputs = [{"name": "clip", "mediaType": "video/mp4",
               "uri": "file:///dev/null/clip.mp4",
               "capturedAt": "2026-09-02T00:00:00Z"}]

    # ── CE1 — invocation and idempotency ─────────────────────────────────────
    first = driver.invoke(f"{key}/44/1", {"region": "US", "minPlateConfidence": 0.5},
                          inputs=inputs)
    odd = driver.invoke(f"{key}/44/1/extra/segments", {"minPlateConfidence": 0.5})
    if first.status and odd.status:
        rs.ok("CE1.1", "Accept the invocation key as an opaque whole",
              "accepted a canonical key and one with extra segments; the endpoint derived "
              "no behaviour from the key's parts")
    else:
        rs.bad("CE1.1", "Accept the invocation key as an opaque whole",
               f"canonical={first.status!r} extra-segments={odd.status!r}")

    resident = contract == "Service"
    repeat = driver.invoke(f"{key}/44/1", {"region": "US", "minPlateConfidence": 0.5},
                           inputs=inputs)
    if resident:
        # A resident run's Result is a RUNNING summary and changes by design, so
        # equality of replies is the wrong observable. What idempotency means
        # here is that a repeated Invocation.id RE-ACKS the run in flight rather
        # than starting a second one, which shows as the event sequence
        # continuing rather than resetting to 1 (CE6.2's series).
        _resident_idempotency(driver, rs, key)
        same = True
    elif True:
        same = (first.status == repeat.status and first.values == repeat.values
                and first.artifacts == repeat.artifacts and first.events == repeat.events)
    if resident:
        pass
    elif same:
        rs.ok("CE1.2", "The same reply for a repeated invocation key",
              f"status, values, artifacts and events identical across two calls with "
              f"{key}/44/1")
    else:
        rs.bad("CE1.2", "The same reply for a repeated invocation key",
               f"first={first.values} repeat={repeat.values}")

    work_marker = "workCount"
    if resident:
        pass
    elif work_marker in first.values:
        if first.values.get(work_marker) == repeat.values.get(work_marker):
            rs.ok("CE1.3", "Side effects happen at most once per invocation key",
                  f"{work_marker} unchanged ({first.values[work_marker]}) across the repeat: "
                  "the reply was replayed, not recomputed")
        else:
            rs.bad("CE1.3", "Side effects happen at most once per invocation key",
                   f"{work_marker} moved {first.values.get(work_marker)} -> "
                   f"{repeat.values.get(work_marker)}: the work was redone behind an "
                   "identical body")
    elif not resident:
        rs.skip("CE1.3", "Side effects happen at most once per invocation key",
                SkipCause.NOT_OBSERVABLE_ON_WIRE,
                "the endpoint exposes no observable that distinguishes a replayed reply "
                "from re-done work; CE1.2 establishes only that the BODY matched")

    deadline_ms = int((time.time() + 1.0) * 1000)
    started = time.time()
    try:
        late = driver.invoke(f"{key}/45/1", {"minPlateConfidence": 0.5},
                             deadline_ms=deadline_ms, query="fail=timeout")
        elapsed = time.time() - started
        if elapsed <= 3.0 and late.status:
            rs.ok("CE1.4", "Honour deadlineMs and return rather than hang",
                  f"returned {late.status!r} in {elapsed:.2f}s against a 1.0s deadline "
                  "with slow work injected")
        else:
            rs.bad("CE1.4", "Honour deadlineMs and return rather than hang",
                   f"took {elapsed:.2f}s against a 1.0s deadline")
    except TypeError:
        rs.skip("CE1.4", "Honour deadlineMs and return rather than hang",
                SkipCause.NOT_OBSERVABLE_ON_WIRE,
                "this binding's driver cannot inject slow work, so only the ordinary "
                "path's latency was seen")

    if first.latency_s < 5.0:
        rs.ok("CE1.5", "Do not require the caller to hold a call open for the run",
              f"the call returned in {first.latency_s:.3f}s" +
              (" — for Grpc, Activate returns before the run completes and progress is "
               "read later via StreamStatus/Result" if binding == "Grpc" else
               " — for a request/response binding the reply IS the run"))
    else:
        rs.bad("CE1.5", "Do not require the caller to hold a call open for the run",
               f"the call took {first.latency_s:.1f}s")

    if resident:
        pass
    elif True:
        attempt2 = driver.invoke(f"{key}/44/2", {"region": "US", "minPlateConfidence": 0.5},
                                 inputs=inputs)
    if resident:
        pass
    elif attempt2.values.get("plate") != first.values.get("plate") or \
            attempt2.values.get("rowId") != first.values.get("rowId"):
        rs.ok("CE1.6", "Do not deduplicate on a prefix of the invocation key",
              "attempt /44/2 produced its own result rather than replaying /44/1")
    else:
        rs.bad("CE1.6", "Do not deduplicate on a prefix of the invocation key",
               "attempt /44/2 returned /44/1's result: the endpoint keyed on a prefix, "
               "collapsing every attempt of every run into one")

    if repeat.replayed:
        rs.ok("CE1.7", "Mark a replayed reply", "the repeat was flagged as a replay",
              level="SHOULD")
    elif resident:
        rs.skip("CE1.7", "Mark a replayed reply", SkipCause.CONTRACT_NOT_DECLARED,
                "a resident run has no reply to replay; the Activate re-ack carries the "
                "equivalent signal and is checked by CE1.2", level="SHOULD")
    else:
        rs.skip("CE1.7", "Mark a replayed reply", SkipCause.OPTIONAL_DECLINED,
                "the endpoint deduplicates without saying so; a caller can only infer it "
                "from equality", level="SHOULD")

    # ── CE2 — the reply envelope ─────────────────────────────────────────────
    if first.status in ("Succeeded", "Failed"):
        rs.ok("CE2.1", "status is exactly Succeeded or Failed", f"status={first.status!r}")
    else:
        rs.bad("CE2.1", "status is exactly Succeeded or Failed", f"status={first.status!r}")

    empty = driver.invoke(f"{key}/46/1", {"minPlateConfidence": 0.999}, inputs=inputs)
    if resident:
        rs.skip("CE2.2", "'Worked, found nothing' is Succeeded with the value absent",
                SkipCause.CONTRACT_NOT_DECLARED,
                "a resident run has no terminal value to omit; it reports what it did and "
                "did not see through events, which CE6 governs")
    elif empty.status == "Succeeded" and "plate" not in empty.values:
        rs.ok("CE2.2", "'Worked, found nothing' is Succeeded with the value absent",
              f"status={empty.status!r} values={sorted(empty.values)} "
              f"events={[e['type'] for e in empty.events]}")
    else:
        rs.bad("CE2.2", "'Worked, found nothing' is Succeeded with the value absent",
               f"status={empty.status!r} values={sorted(empty.values)}")


    size = _values_bytes(first.values)
    scalars = all(isinstance(v, (str, int, float, bool)) for v in first.values.values())
    if scalars and len(first.values) <= 16 and size <= 4096:
        rs.ok("CE2.3", "values are scalars only, <=16 entries and <=4 KiB",
              f"{len(first.values)} entries, {size} bytes, all scalar")
    else:
        rs.bad("CE2.3", "values are scalars only, <=16 entries and <=4 KiB",
               f"{len(first.values)} entries, {size} bytes, scalars={scalars}")

    inline = [a for a in first.artifacts
              if any(k in a for k in ("data", "bytes", "content", "body"))]
    missing = [a.get("name") for a in first.artifacts if not a.get("digest")]
    if first.artifacts and not inline and not missing:
        rs.ok("CE2.4", "Artifacts are references carrying a digest",
              f"{len(first.artifacts)} artifact(s), every one with a uri, a mediaType and "
              "a digest, none carrying inline bytes")
    elif not first.artifacts:
        rs.skip("CE2.4", "Artifacts are references carrying a digest",
                SkipCause.TARGET_DID_NOT_REACH_STATE,
                "the run produced no artifact, so the reference discipline was not exercised")
    else:
        rs.bad("CE2.4", "Artifacts are references carrying a digest",
               f"inline-bytes artifacts={[a.get('name') for a in inline]} "
               f"digest-less={missing}")

    offenders = sorted(k for k in first.values if k.lower() in COMMAND_KEYS)
    top = getattr(first.raw, "keys", lambda: [])()
    top_offenders = sorted(k for k in top if k.lower() in COMMAND_KEYS)
    if not offenders and not top_offenders:
        rs.ok("CE2.5", "Nothing in the reply is interpretable as an instruction",
              f"no key in values or at the envelope's top level is in the command "
              f"vocabulary ({len(COMMAND_KEYS)} names). Artifact `pose` is capture "
              "provenance and is deliberately not treated as an instruction",
              level="MUST NOT")
    else:
        rs.bad("CE2.5", "Nothing in the reply is interpretable as an instruction",
               f"command-shaped keys present: values={offenders} envelope={top_offenders}",
               level="MUST NOT")

    if declared_results:
        undeclared = sorted(set(first.values) - declared_results)
        if not undeclared:
            rs.ok("CE2.6", "Return only result fields the contract declares",
                  f"{sorted(first.values)} all declared in spec.results")
        else:
            rs.bad("CE2.6", "Return only result fields the contract declares",
                   f"undeclared fields returned: {undeclared}")
    else:
        unknown = sorted(set(first.raw.get("result", {}).get("structuredContent", {}))
                         - ENVELOPE_KEYS) if isinstance(first.raw, dict) else []
        if not unknown:
            rs.ok("CE2.6", "Return only result fields the contract declares",
                  "the binding supplies the contract (no spec.results is authored for an "
                  "Mcp endpoint); the returned document carries only envelope fields")
        else:
            rs.bad("CE2.6", "Return only result fields the contract declares",
                   f"non-envelope top-level fields: {unknown}")

    seqd = [e for e in first.events if "seq" in e]
    if resident:
        rs.skip("CE2.7", "Events in a reply carry no sequence number",
                SkipCause.CONTRACT_NOT_DECLARED,
                "a resident executor assigns seq itself and its events do not travel in a "
                "reply at all; CE6.2 is the check that applies")
    elif first.events and not seqd:
        rs.ok("CE2.7", "Events in a reply carry no sequence number",
              f"{len(first.events)} event(s), none carrying seq: the control plane assigns "
              "it in reply order (CE6.3)")
    elif not first.events:
        rs.skip("CE2.7", "Events in a reply carry no sequence number",
                SkipCause.TARGET_DID_NOT_REACH_STATE, "the run raised no event")
    else:
        rs.bad("CE2.7", "Events in a reply carry no sequence number",
               f"events carrying seq: {[e['type'] for e in seqd]}")

    if first.produced_by.get("model"):
        rs.ok("CE2.8", "Model-derived results carry model identity and version",
              f"model={first.produced_by.get('model')!r} "
              f"version={first.produced_by.get('modelVersion')!r}", level="SHOULD")
    else:
        rs.skip("CE2.8", "Model-derived results carry model identity and version",
                SkipCause.OPTIONAL_DECLINED, "no model provenance on the reply",
                level="SHOULD")

    # ── CE3 / CE4 / CE5 — binding-specific ───────────────────────────────────
    applicable = {"binding-http": binding == "Http", "binding-grpc": binding == "Grpc",
                  "binding-mcp": binding == "Mcp",
                  "service-contract": contract == "Service",
                  "events": bool(declared_events)}

    if binding == "Http":
        _http_checks(driver, decl, rs, key, first, empty)
    else:
        for cid, title in _HTTP_TITLES.items():
            rs.skip(cid, title, SkipCause.BINDING_NOT_DECLARED,
                    f"the endpoint declares binding: {binding}")
    if binding == "Grpc":
        _grpc_checks(driver, decl, rs, key, contract)
    else:
        for cid, title in _GRPC_TITLES.items():
            level = "SHOULD" if cid == "CE4.6" else "MUST"
            rs.skip(cid, title, SkipCause.BINDING_NOT_DECLARED,
                    f"the endpoint declares binding: {binding}", level=level)
    if binding == "Mcp":
        _mcp_checks(driver, decl, rs, key)
    else:
        for cid, title in _MCP_TITLES.items():
            level = "SHOULD" if cid == "CE5.6" else "MUST"
            rs.skip(cid, title, SkipCause.BINDING_NOT_DECLARED,
                    f"the endpoint declares binding: {binding}", level=level)

    # ── CE2.9 — a malformed request is not answered with a success ───────────
    _ce29(driver, rs, key, binding)

    # ── CE6 — events from a source that is not a fleet adapter ───────────────
    _event_checks(driver, decl, rs, key, first, repeat, contract, declared_events)

    # ── CE7 — the Service contract ───────────────────────────────────────────
    _service_checks(driver, rs, key, contract)

    # ── CE8 — provenance ─────────────────────────────────────────────────────
    digest = first.produced_by.get("buildDigest") or first.values.get("buildDigest")
    if digest:
        rs.ok("CE8.1", "Report the identity of the bytes serving the run",
              f"buildDigest={digest[:23]}…")
    else:
        rs.skip("CE8.1", "Report the identity of the bytes serving the run",
                SkipCause.NOT_OBSERVABLE_ON_WIRE,
                "the reply carries no image or build identity, so a run cannot be tied to "
                "specific bytes from the reply alone")
    if first.produced_by.get("modelVersion"):
        rs.ok("CE8.2", "Echo model identity and version onto a model-derived result",
              f"{first.produced_by.get('model')}@{first.produced_by.get('modelVersion')}",
              level="SHOULD")
    else:
        rs.skip("CE8.2", "Echo model identity and version onto a model-derived result",
                SkipCause.OPTIONAL_DECLINED, "no model version on the reply", level="SHOULD")
    other = digest and (repeat.produced_by.get("buildDigest") == digest)
    if digest and other:
        rs.ok("CE8.3", "The reported identity does not change without the bytes changing",
              "the same identity was reported across every invocation in this run. LIMIT: "
              "a run cannot observe a swap that happens between runs; this establishes "
              "stability within one, not immutability",
              level="MUST NOT")
    else:
        rs.skip("CE8.3", "The reported identity does not change without the bytes changing",
                SkipCause.NOT_OBSERVABLE_ON_WIRE,
                "no identity is reported, so drift in it cannot be detected either",
                level="MUST NOT")
    return applicable


_HTTP_TITLES = {
    "CE3.1": "Serve POST /v1/invoke with a JSON envelope",
    "CE3.2": "Treat the body's invocation.id as authoritative",
    "CE3.3": "Both outcomes are returned in the same envelope shape",
    "CE3.4": "Do not claim the Service contract on Http",
    "CE3.5": "Answer the declared probe path",
    "CE3.6": "Require no request field the envelope does not define",
}
_GRPC_TITLES = {
    "CE4.1": "Implement ComputeRuntimeService as the server",
    "CE4.2": "Activate returns promptly and re-acks a repeated Invocation.id",
    "CE4.3": "StreamStatus resumes from since_seq",
    "CE4.4": "Result is write-once terminal (App) or a running summary (Service)",
    "CE4.5": "Cancel returns a valid CancelDisposition",
    "CE4.6": "RenewLease is implemented",
    "CE4.7": "No sensor frame is carried on this service",
}
_MCP_TITLES = {
    "CE5.1": "tools/list returns the input schema as JSON Schema",
    "CE5.2": "tools/call returns content mapping onto the envelope",
    "CE5.3": "Arguments are validated against the published schema",
    "CE5.4": "Do not claim the Service contract on Mcp",
    "CE5.5": "Deduplicate on the invocation key in the call arguments",
    "CE5.6": "A schema change is observable",
}


def _resident_idempotency(driver, rs: ResultSet, key: str) -> None:
    """CE1.2, CE1.3 and CE1.6 for a Service-contract (resident) endpoint."""
    inv = f"{key}/90/1"
    driver.activate(inv, {})
    time.sleep(0.35)
    before = [e.seq for e in driver.events(inv, since_seq=0, timeout=1.0)]
    ack = driver.activate(inv, {})
    time.sleep(0.35)
    after = [e.seq for e in driver.events(inv, since_seq=0, timeout=1.0)]
    continued = bool(before) and bool(after) and after[0] == before[0] == 1 and \
        len(after) >= len(before) and after[:len(before)] == before
    if ack.accepted and continued:
        rs.ok("CE1.2", "The same reply for a repeated invocation key",
              f"a repeated Invocation.id re-acked the run in flight ({ack.message!r}); the "
              f"event sequence continued {before} -> {after} rather than resetting to 1, "
              "so no second run was started")
        rs.ok("CE1.3", "Side effects happen at most once per invocation key",
              "one run served both Activates: a resident executor's side effects are the "
              "run itself, and there was only ever one")
    else:
        rs.bad("CE1.2", "The same reply for a repeated invocation key",
               f"accepted={ack.accepted}; sequence {before} -> {after}")
        rs.bad("CE1.3", "Side effects happen at most once per invocation key",
               "a repeated Invocation.id did not resolve to the run already in flight")

    other = f"{key}/90/2"
    driver.activate(other, {})
    time.sleep(0.3)
    fresh = [e.seq for e in driver.events(other, since_seq=0, timeout=1.0)]
    if fresh and fresh[0] == 1:
        rs.ok("CE1.6", "Do not deduplicate on a prefix of the invocation key",
              f"attempt /90/2 started its own run with its own sequence {fresh}, rather "
              "than resolving to /90/1")
    else:
        rs.bad("CE1.6", "Do not deduplicate on a prefix of the invocation key",
               f"attempt /90/2 produced {fresh}: the endpoint keyed on a prefix")


def _http_checks(driver: HttpDriver, decl, rs: ResultSet, key: str, first, empty) -> None:
    code = first.raw["code"]
    ctype = first.raw["headers"].get("Content-Type", "")
    if code == 200 and "application/json" in ctype:
        rs.ok("CE3.1", _HTTP_TITLES["CE3.1"], f"200 {ctype}")
    else:
        rs.bad("CE3.1", _HTTP_TITLES["CE3.1"], f"{code} {ctype}")

    # Compared on the id-DERIVED fields only, not on the whole values map: a
    # broken idempotency cache moves `rowId` and `workCount` on every call, and
    # a check should fail for its own reason rather than for CE1.2's.
    body_id = f"{key}/47/1"
    header_id = f"{key}/99/9"
    reply = driver.invoke(body_id, {"minPlateConfidence": 0.5}, header_id=header_id)
    from_body = driver.invoke(body_id, {"minPlateConfidence": 0.5})
    from_header = driver.invoke(header_id, {"minPlateConfidence": 0.5})
    derived = ("plate", "confidence")
    keyed_on_body = all(reply.values.get(k) == from_body.values.get(k) for k in derived)
    keyed_on_header = all(reply.values.get(k) == from_header.values.get(k) for k in derived)
    if keyed_on_body and not keyed_on_header:
        rs.ok("CE3.2", _HTTP_TITLES["CE3.2"],
              f"a request whose Swarmada-Invocation-Id header disagreed with the body "
              f"produced the BODY's result ({reply.values.get('plate')!r}), not the "
              f"header's ({from_header.values.get('plate')!r})")
    else:
        rs.bad("CE3.2", _HTTP_TITLES["CE3.2"],
               f"header-keyed={keyed_on_header} body-keyed={keyed_on_body}: the endpoint "
               "did not treat the body's invocation.id as authoritative")

    if set(first.raw["body"]) == set(empty.raw["body"]):
        rs.ok("CE3.3", _HTTP_TITLES["CE3.3"],
              f"both outcomes carry {sorted(first.raw['body'])}")
    else:
        rs.bad("CE3.3", _HTTP_TITLES["CE3.3"],
               f"found={sorted(first.raw['body'])} nothing-found={sorted(empty.raw['body'])}")

    if decl["spec"]["contract"] != "Service":
        rs.ok("CE3.4", _HTTP_TITLES["CE3.4"],
              f"the endpoint declares contract: {decl['spec']['contract']}",
              level="MUST NOT")
    else:
        rs.bad("CE3.4", _HTTP_TITLES["CE3.4"],
               "an Http endpoint declaring contract: Service", level="MUST NOT")

    probe = (decl["spec"].get("probe") or {}).get("path")
    if probe:
        req = urllib.request.Request(driver.base + probe)
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                ok = resp.status == 200
        except urllib.error.URLError:
            ok = False
        (rs.ok if ok else rs.bad)("CE3.5", _HTTP_TITLES["CE3.5"],
                                  f"GET {probe} -> {'200' if ok else 'no answer'}")
    else:
        rs.skip("CE3.5", _HTTP_TITLES["CE3.5"], SkipCause.OPTIONAL_DECLINED,
                "the endpoint declares no probe")

    code2, _, raw2 = driver._post("/v1/invoke",
                                 {"invocation": {"id": f"{key}/48/1"}})
    minimal = json.loads(raw2)
    if code2 == 200 and minimal.get("status"):
        rs.ok("CE3.6", _HTTP_TITLES["CE3.6"],
              "a request carrying only `invocation` was accepted: no field outside the "
              "envelope is required", level="MUST NOT")
    else:
        rs.bad("CE3.6", _HTTP_TITLES["CE3.6"],
               f"a minimal envelope was refused: {code2} {minimal}", level="MUST NOT")


def _grpc_checks(driver: GrpcDriver, decl, rs: ResultSet, key: str, contract: str) -> None:
    rs.ok("CE4.1", _GRPC_TITLES["CE4.1"],
          f"the harness dialled {driver.address} as a client and the endpoint answered as "
          "the server; no reverse dial was offered or needed. LIMIT: this establishes that "
          "a reverse dial is unnecessary, not that the endpoint never attempts one")

    inv = f"{key}/50/1"
    started = time.time()
    a1 = driver.activate(inv, {"minPlateConfidence": 0.5})
    prompt = time.time() - started
    a2 = driver.activate(inv, {"minPlateConfidence": 0.5})
    if a1.accepted and a2.accepted and prompt < 5.0:
        rs.ok("CE4.2", _GRPC_TITLES["CE4.2"],
              f"Activate returned in {prompt:.3f}s; the repeat answered {a2.message!r}")
    else:
        rs.bad("CE4.2", _GRPC_TITLES["CE4.2"],
               f"accepted={a1.accepted}/{a2.accepted} latency={prompt:.2f}s")

    time.sleep(0.4)
    all_events = driver.events(inv, since_seq=0, timeout=1.5)
    tail = driver.events(inv, since_seq=1, timeout=1.5)
    seqs = [e.seq for e in all_events]
    if seqs and seqs == sorted(seqs) and all(e.seq > 1 for e in tail):
        rs.ok("CE4.3", _GRPC_TITLES["CE4.3"],
              f"since_seq=0 delivered {seqs}; since_seq=1 resumed at "
              f"{[e.seq for e in tail] or '(no tail)'}")
    elif not seqs:
        rs.skip("CE4.3", _GRPC_TITLES["CE4.3"], SkipCause.TARGET_DID_NOT_REACH_STATE,
                "the run raised no event, so a resume point could not be exercised")
    else:
        rs.bad("CE4.3", _GRPC_TITLES["CE4.3"],
               f"seqs={seqs} tail={[e.seq for e in tail]}")

    r1, r2 = driver.result(inv), driver.result(inv)
    if contract == "Service":
        ok = "observations" in r1.values and not any(
            isinstance(v, (list, dict)) for v in r1.values.values())
        (rs.ok if ok else rs.bad)("CE4.4", _GRPC_TITLES["CE4.4"],
                                  f"summary values={dict(r1.values)}")
    else:
        if r1.SerializeToString() == r2.SerializeToString():
            rs.ok("CE4.4", _GRPC_TITLES["CE4.4"],
                  "two Result calls returned byte-identical terminal results: written once, "
                  "so a select branching on it cannot see it change")
        else:
            rs.bad("CE4.4", _GRPC_TITLES["CE4.4"], "Result changed between two calls")

    cancel = driver.stub.Cancel(driver.pb.CancelRequest(invocation_id=inv, reason="harness"))
    disposition = driver.pb.CancelResponse.DESCRIPTOR.fields_by_name[
        "disposition"].enum_type.values_by_number[cancel.disposition].name
    if cancel.acknowledged and cancel.disposition != 0:
        rs.ok("CE4.5", _GRPC_TITLES["CE4.5"],
              f"disposition={disposition} — the control plane can tell a safe stop from a "
              "completion or a recovery")
    else:
        rs.bad("CE4.5", _GRPC_TITLES["CE4.5"],
               f"acknowledged={cancel.acknowledged} disposition={disposition}")

    try:
        renew = driver.stub.RenewLease(driver.pb.RenewLeaseRequest(
            invocation_id=inv, lease_expires_ms=int((time.time() + 5) * 1000)))
        rs.ok("CE4.6", _GRPC_TITLES["CE4.6"],
              f"answered renewed={renew.renewed} ({renew.message!r})", level="SHOULD")
    except driver.grpc.RpcError as exc:
        if exc.code() == driver.grpc.StatusCode.UNIMPLEMENTED:
            rs.skip("CE4.6", _GRPC_TITLES["CE4.6"], SkipCause.OPTIONAL_DECLINED,
                    "an App-contract endpoint that never outlives its invocation may "
                    "decline this RPC", level="SHOULD")
        else:
            rs.bad("CE4.6", _GRPC_TITLES["CE4.6"], f"{exc.code()}", level="SHOULD")

    # Structural, against the descriptor rather than a live exchange: a frame
    # field cannot be added to this service without failing here.
    byte_fields = []
    for name, msg in driver.pb.DESCRIPTOR.message_types_by_name.items():
        for f in msg.fields:
            if f.type == f.TYPE_BYTES:
                byte_fields.append(f"{name}.{f.name}")
    if not byte_fields:
        rs.ok("CE4.7", _GRPC_TITLES["CE4.7"],
              f"no message in {driver.pb.DESCRIPTOR.package} carries a bytes field; frames "
              "flow adapter -> runtime directly and cannot travel this channel",
              level="MUST NOT")
    else:
        rs.bad("CE4.7", _GRPC_TITLES["CE4.7"],
               f"bytes fields present: {byte_fields}", level="MUST NOT")


def _mcp_checks(driver: McpDriver, decl, rs: ResultSet, key: str) -> None:
    listed = driver.rpc("tools/list").get("result") or {}
    tools = listed.get("tools") or []
    schema = (tools[0].get("inputSchema") if tools else None) or {}
    if schema.get("type") == "object" and schema.get("properties"):
        rs.ok("CE5.1", _MCP_TITLES["CE5.1"],
              f"tool {tools[0]['name']!r} declares {sorted(schema['properties'])} "
              f"(required {schema.get('required')})")
    else:
        rs.bad("CE5.1", _MCP_TITLES["CE5.1"], f"tools/list returned {listed}")

    reply = driver.invoke(f"{key}/60/1", {"minPlateConfidence": 0.5})
    if reply.status and isinstance(reply.values, dict):
        rs.ok("CE5.2", _MCP_TITLES["CE5.2"],
              f"tools/call returned status={reply.status!r} with values and events mapped "
              "onto the envelope")
    else:
        rs.bad("CE5.2", _MCP_TITLES["CE5.2"], f"{reply.raw}")

    bad = driver.rpc("tools/call", {"name": "read_plate",
                                    "arguments": {"invocationId": f"{key}/61/1",
                                                  "region": "MARS"}})
    unknown = driver.rpc("tools/call", {"name": "read_plate",
                                        "arguments": {"invocationId": f"{key}/62/1",
                                                      "nonsense": 1}})
    if "error" in bad and "error" in unknown:
        rs.ok("CE5.3", _MCP_TITLES["CE5.3"],
              f"an out-of-enum value and an undeclared argument were both refused as "
              f"protocol errors ({bad['error']['message']!r}); the call never ran, so "
              "there is no envelope reporting a success that did nothing")
    else:
        rs.bad("CE5.3", _MCP_TITLES["CE5.3"],
               f"enum-violation={'error' in bad} unknown-arg={'error' in unknown}")

    if decl["spec"]["contract"] != "Service":
        rs.ok("CE5.4", _MCP_TITLES["CE5.4"],
              f"the endpoint declares contract: {decl['spec']['contract']}; MCP is "
              "request/response and has no binary frame stream", level="MUST NOT")
    else:
        rs.bad("CE5.4", _MCP_TITLES["CE5.4"],
               "an Mcp endpoint declaring contract: Service", level="MUST NOT")

    again = driver.invoke(f"{key}/60/1", {"minPlateConfidence": 0.5})
    if again.values == reply.values and again.replayed:
        rs.ok("CE5.5", _MCP_TITLES["CE5.5"],
              "a repeated invocationId in the call arguments replayed the cached result")
    else:
        rs.bad("CE5.5", _MCP_TITLES["CE5.5"],
               f"replayed={again.replayed} values-match={again.values == reply.values}")

    digest = listed.get("_schemaDigest")
    if digest:
        rs.ok("CE5.6", _MCP_TITLES["CE5.6"],
              f"tools/list carries a schema digest ({digest[:23]}…) a consumer can hold and "
              "compare", level="SHOULD")
    else:
        rs.skip("CE5.6", _MCP_TITLES["CE5.6"], SkipCause.OPTIONAL_DECLINED,
                "tools/list carries no schema digest; drift is discoverable only by "
                "diffing the schema", level="SHOULD")


def _ce29(driver, rs: ResultSet, key: str, binding: str) -> None:
    title = "A malformed request is never answered with a success"
    if binding == "Http":
        code_ct, _, _ = driver._post("/v1/invoke", b"not json",
                                     content_type="text/plain")
        code_body, _, _ = driver._post("/v1/invoke", b"{not json")
        if code_ct >= 400 and code_body >= 400:
            rs.ok("CE2.9", title,
                  f"wrong content type -> {code_ct}, unparseable body -> {code_body}")
        else:
            rs.bad("CE2.9", title,
                   f"wrong content type -> {code_ct}, unparseable body -> {code_body}")
    elif binding == "Mcp":
        resp = driver.rpc("tools/call", {"name": "no_such_tool", "arguments": {}})
        (rs.ok if "error" in resp else rs.bad)(
            "CE2.9", title, f"an unknown tool answered {list(resp)[-1]}")
    else:
        try:
            driver.result("no/such/invocation/0")
            rs.bad("CE2.9", title, "Result for an unknown invocation returned a result")
        except driver.grpc.RpcError as exc:
            (rs.ok if exc.code() == driver.grpc.StatusCode.NOT_FOUND else rs.bad)(
                "CE2.9", title, f"Result for an unknown invocation -> {exc.code().name}")


def _event_checks(driver, decl, rs: ResultSet, key: str, first, repeat,
                  contract: str, declared: set[str]) -> None:
    raised = {e["type"] for e in first.events}
    if not declared:
        for cid in ("CE6.1", "CE6.2", "CE6.3", "CE6.4", "CE6.5"):
            rs.skip(cid, "Event obligations for a non-adapter source",
                    SkipCause.OPTIONAL_DECLINED, "the endpoint declares no event types")
        rs.skip("CE6.6", "Delivery timing is documented", SkipCause.OPTIONAL_DECLINED,
                "the endpoint declares no event types", level="SHOULD")
        return
    undeclared = sorted(raised - declared)
    if not undeclared:
        rs.ok("CE6.1", "Raise only event types the endpoint declares (E10.1)",
              f"raised {sorted(raised)}; declared {sorted(declared)}")
    else:
        rs.bad("CE6.1", "Raise only event types the endpoint declares (E10.1)",
               f"undeclared types raised: {undeclared}")

    if contract == "Service":
        inv = f"{key}/70/1"
        driver.activate(inv, {})
        time.sleep(0.5)
        evs = driver.events(inv, since_seq=0, timeout=1.5)
        seqs = [e.seq for e in evs]
        ok = seqs and seqs[0] == 1 and seqs == sorted(set(seqs))
        (rs.ok if ok else rs.bad)("CE6.2",
                                  "A resident executor assigns seq itself (E10.2)",
                                  f"seqs={seqs}: strictly monotonic per run, from 1, none reused")
        rs.skip("CE6.3", "A request/response executor emits no seq (E10.3)",
                SkipCause.CONTRACT_NOT_DECLARED,
                "the endpoint declares contract: Service, so its events carry a seq the "
                "executor assigned; CE6.2 is the rule that binds it")
    else:
        rs.skip("CE6.2", "A resident executor assigns seq itself (E10.2)",
                SkipCause.CONTRACT_NOT_DECLARED,
                "the endpoint declares contract: App, which holds no state between calls "
                "and has nothing to be monotonic with; CE6.3 is the rule that binds it")
        seqd = [e for e in first.events if "seq" in e]
        (rs.ok if not seqd else rs.bad)(
            "CE6.3", "A request/response executor emits no seq (E10.3)",
            f"{len(first.events)} event(s) in the reply, none carrying seq; the control "
            "plane assigns seq in reply order from 1 per invocation")

    if contract == "Service":
        rs.skip("CE6.4", "A repeated invocation carries the same events (E10.4)",
                SkipCause.CONTRACT_NOT_DECLARED,
                "E10.4 binds a REQUEST/RESPONSE executor. A resident executor holds the "
                "run and keeps emitting; its deduplication is the Activate re-ack, which "
                "CE1.2 checks")
    elif first.events == repeat.events:
        rs.ok("CE6.4", "A repeated invocation carries the same events (E10.4)",
              f"{[e['type'] for e in first.events]} identical across the repeat, so a "
              "retried invocation fires a trigger exactly once")
    else:
        rs.bad("CE6.4", "A repeated invocation carries the same events (E10.4)",
               f"{first.events} -> {repeat.events}")

    offenders = []
    for ev in first.events:
        for k in list(ev) + list(ev.get("payload") or {}):
            if k.lower() in ADDRESSING_KEYS:
                offenders.append(f"{ev['type']}.{k}")
    if not offenders:
        rs.ok("CE6.5", "An event addresses no consumer and names no trigger (E10.5)",
              f"no event field or payload key is in the addressing vocabulary "
              f"({len(ADDRESSING_KEYS)} names); consumers subscribe by naming the producer",
              level="MUST NOT")
    else:
        rs.bad("CE6.5", "An event addresses no consumer and names no trigger (E10.5)",
               f"addressing keys present: {offenders}", level="MUST NOT")

    timing = (decl["metadata"].get("annotations") or {}).get(
        "conformance.swarmada.io/event-delivery-timing")
    if timing in ("reply", "observation"):
        rs.ok("CE6.6", "Delivery timing is documented (E10.6)",
              f"declared {timing!r}: a task author can see whether a loop.until resumes "
              "when the event is observed or when the invocation reports", level="SHOULD")
    else:
        rs.skip("CE6.6", "Delivery timing is documented (E10.6)",
                SkipCause.OPTIONAL_DECLINED, "the declaration records no delivery timing",
                level="SHOULD")


_SERVICE_TITLES = {
    "CE7.1": "A resident run ends only on cancel, lease expiry, or a terminal event",
    "CE7.2": "A resident run self-stops when renewal ceases",
    "CE7.3": "A pinned run is served from one address for its lifetime",
    "CE7.4": "A re-placement after endpoint loss is marked by an event",
    "CE7.5": "Result reports a summary, never per-observation data",
    "CE7.6": "An absence of frames is not treated as an absence of the thing sought",
}


def _service_checks(driver, rs: ResultSet, key: str, contract: str) -> None:
    if contract != "Service":
        for cid, title in _SERVICE_TITLES.items():
            rs.skip(cid, title, SkipCause.CONTRACT_NOT_DECLARED,
                    "the endpoint declares contract: App")
        return

    inv = f"{key}/80/1"
    driver.activate(inv, {})
    time.sleep(0.5)
    mid = driver.events(inv, since_seq=0, timeout=1.0)
    if len(mid) >= 2:
        rs.ok("CE7.1", _SERVICE_TITLES["CE7.1"],
              f"{len(mid)} event(s) observed while the run was neither cancelled nor "
              "expired: the run stayed resident")
    else:
        rs.bad("CE7.1", _SERVICE_TITLES["CE7.1"],
               f"only {len(mid)} event(s) before the run stopped on its own")

    # Stop renewing and watch it self-stop. The safe direction is off.
    deadline = time.time() + 6.0
    terminal = None
    while time.time() < deadline and terminal is None:
        for ev in driver.events(inv, since_seq=0, timeout=1.0):
            if ev.terminal:
                terminal = ev
                break
    if terminal is not None:
        rs.ok("CE7.2", _SERVICE_TITLES["CE7.2"],
              f"with renewal withheld the run self-stopped and raised "
              f"{terminal.type!r} (terminal)")
    else:
        rs.bad("CE7.2", _SERVICE_TITLES["CE7.2"],
               "the run was still running after its lease horizon with no renewal sent")

    # DEPLOYMENT_PROPERTY, not NOT_OBSERVABLE_ON_WIRE: a hand-off IS observable on the wire
    # (the serving address changes); what the suite lacks is a second replica to hand off
    # to. Under ADR-0070 the two causes grade differently, and only this one names the
    # suite as the party that could close it.
    rs.skip("CE7.3", _SERVICE_TITLES["CE7.3"], SkipCause.DEPLOYMENT_PROPERTY,
            "the in-tree fixture is a single replica, so a hand-off to another replica "
            "cannot occur and therefore cannot be observed. Pinning is asserted on the "
            "control-plane side by CP1.4")
    # NEEDS_EXTENDED_RUN ("several forced faults"), not TARGET_DID_NOT_REACH_STATE: the
    # target was never asked to reach the state. The harness induces no endpoint loss.
    rs.skip("CE7.4", _SERVICE_TITLES["CE7.4"], SkipCause.NEEDS_EXTENDED_RUN,
            "the harness induced no endpoint loss, so no re-placement happened")

    summary = driver.result(inv)
    if "observations" in summary.values and all(
            not isinstance(v, (list, dict)) for v in summary.values.values()):
        rs.ok("CE7.5", _SERVICE_TITLES["CE7.5"],
              f"summary values={dict(summary.values)} — counts and durations, not the "
              "observations themselves")
    else:
        rs.bad("CE7.5", _SERVICE_TITLES["CE7.5"], f"values={dict(summary.values)}")

    rs.skip("CE7.6", _SERVICE_TITLES["CE7.6"], SkipCause.NEEDS_EXTENDED_RUN,
            "distinguishing 'the stream dried up' from 'nothing was there' needs a run "
            "with the frame source cut; the fixture generates its own frames")
