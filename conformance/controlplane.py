# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Drives CONTROL-PLANE.md against the artefacts a deployment actually ships.

No cluster, no API server, no network. The checks that can be established
statically -- containment, admission rules expressed as CEL, image provenance,
and the RBAC a deployment grants -- are established from the shipped CRD and
RBAC YAML, because those are what a deployment IS. A stranger reproduces every
result here from a clone.

The runtime checks (CP2, CP3.1, CP3.3, CP6) describe controller behaviour that
does not exist yet and record ``surface-not-implemented``. A skip is not a pass.
"""

from __future__ import annotations

import hashlib
from pathlib import Path

import yaml

from .model import ResultSet, SkipCause

REPO = Path(__file__).resolve().parent.parent
CRD_DIR = REPO / "config" / "crd" / "bases"
RBAC = [REPO / "config" / "rbac" / "role.yaml",
        REPO / "deploy" / "swarmada" / "templates" / "rbac-manager.yaml"]

#: Property names through which a task author could supply code or an address.
#: `podTemplate` is here too: it is legitimate on ComputeWorkloadTemplate, which
#: is an ADMIN-created object and is therefore not among the CRDs scanned.
CODE_BEARING = {"image", "images", "command", "args", "script", "scriptbody", "entrypoint",
                "containers", "podtemplate", "podspec", "url", "host", "hostport",
                "address", "endpointurl", "dsn", "shell", "exec"}

#: The CRDs a task author writes. Their SPEC subtrees are scanned; status is not,
#: because status is what the system reports, not what an author supplies -- a
#: `uri` on `status.result.artifacts` is an output reference and not a way in.
AUTHORED = ["swarmada.io_fleetactions.yaml", "swarmada.io_fleettasks.yaml",
            "swarmada.io_computeendpoints.yaml"]

WORKLOAD_KINDS = {"jobs", "deployments", "pods", "replicasets", "statefulsets",
                  "daemonsets", "persistentvolumeclaims"}
CREATE_VERBS = {"create", "*"}


def _spec_property_names(doc: dict) -> set[str]:
    schema = doc["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
    spec = (schema.get("properties") or {}).get("spec")
    names: set[str] = set()

    def walk(node) -> None:
        if isinstance(node, dict):
            for k, v in node.items():
                if k == "properties" and isinstance(v, dict):
                    names.update(v.keys())
                walk(v)
        elif isinstance(node, list):
            for item in node:
                walk(item)

    walk(spec)
    return names


def _validations(doc: dict, path: list[str]) -> list[str]:
    node = doc["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
    for seg in path:
        node = (node.get("properties") or {}).get(seg, {})
    return [v["rule"] for v in node.get("x-kubernetes-validations", [])]


def _status_node(doc: dict, path: list[str]) -> dict:
    """Walk into a CRD's status subtree. Returns {} when any segment is missing."""
    node = doc["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
    for seg in ["status"] + path:
        node = (node.get("properties") or {}).get(seg, {})
        if not node:
            return {}
    return node


def _load(rs: ResultSet, name: str) -> dict:
    path = CRD_DIR / name
    rs.artefacts.append(str(path))
    return yaml.safe_load(path.read_text())


def run(rs: ResultSet, *, deployment: str) -> dict[str, bool]:
    crds = {name: _load(rs, name) for name in AUTHORED}
    cwt = _load(rs, "swarmada.io_computeworkloadtemplates.yaml")

    digests = []
    for path in list(rs.artefacts):
        digests.append(f"{Path(path).name}="
                       f"{hashlib.sha256(Path(path).read_bytes()).hexdigest()[:12]}")
    rs.ctx("CP0.4", "Harness context: the artefacts read and their digests",
           "; ".join(digests))
    rs.ok("CP0.2", "The report names the deployment and the artefacts read",
          f"deployment={deployment!r}; read {len(rs.artefacts)} artefact(s) from "
          f"config/crd/bases and config/rbac")

    # ── CP1.1 — no field carries code or an address ──────────────────────────
    offenders: list[str] = []
    for name, doc in crds.items():
        for prop in _spec_property_names(doc):
            if prop.lower() in CODE_BEARING:
                offenders.append(f"{name}:{prop}")
    if not offenders:
        scanned = sum(len(_spec_property_names(d)) for d in crds.values())
        rs.ok("CP1.1", "No task-authored field can supply an image, a script, a command "
                       "or an address",
              f"{scanned} spec property name(s) across {', '.join(crds)}; none is one of "
              f"the {len(CODE_BEARING)} code- or address-bearing names. Status subtrees "
              "are excluded on purpose: a `uri` on status.result.artifacts is an output "
              "reference, not a way in", level="MUST NOT")
    else:
        rs.bad("CP1.1", "No task-authored field can supply an image, a script, a command "
                        "or an address",
               f"code- or address-bearing fields: {offenders}", level="MUST NOT")

    # ── CP1.2 — an executor is resolved by name ──────────────────────────────
    ce_props = _spec_property_names(crds["swarmada.io_computeendpoints.yaml"])
    fa_props = _spec_property_names(crds["swarmada.io_fleetactions.yaml"])
    by_name = {"service", "namespace", "port"} <= ce_props and "endpointRef" in fa_props
    if by_name:
        rs.ok("CP1.2", "An executor is resolved only by name, in a permitted namespace",
              "FleetAction.spec carries `endpointRef` (a name) and ComputeEndpoint's "
              "provision.existing carries service/namespace/port — a reference to an "
              "administrator-created object, with no field for an address")
    else:
        rs.bad("CP1.2", "An executor is resolved only by name, in a permitted namespace",
               f"endpointRef in FleetAction={('endpointRef' in fa_props)}; "
               f"provision.existing shape={sorted({'service','namespace','port'} & ce_props)}")

    rules = _validations(crds["swarmada.io_computeendpoints.yaml"], ["spec"])
    mcp_rule = [r for r in rules if "contract" in r and "Grpc" in r]
    pin_rule = [r for r in rules if "pinEndpoint" in r]
    if mcp_rule:
        rs.ok("CP1.3", "Admission rejects binding: Mcp with contract: Service",
              f"CEL on ComputeEndpoint.spec: {mcp_rule[0]!r} — Grpc is the only binding "
              "that carries a sensor stream, so every other binding is excluded by the "
              "same rule rather than by a list that could fall behind")
    else:
        rs.bad("CP1.3", "Admission rejects binding: Mcp with contract: Service",
               f"no CEL rule on ComputeEndpoint.spec constrains contract: Service to Grpc; "
               f"rules present: {rules}")
    if pin_rule:
        rs.ok("CP1.4", "contract: Service requires streamPolicy.pinEndpoint: true",
              f"CEL on ComputeEndpoint.spec: {pin_rule[0]!r}")
    else:
        rs.bad("CP1.4", "contract: Service requires streamPolicy.pinEndpoint: true",
               f"rules present: {rules}")

    # ── CP5.5 — the decline is verified, not inferred ────────────────────────
    granted: list[str] = []
    read: list[str] = []
    for path in RBAC:
        if not path.exists():
            continue
        rs.artefacts.append(str(path))
        read.append(path.name)
        for doc in yaml.safe_load_all(path.read_text().replace("{{", "#{{")):
            if not isinstance(doc, dict) or doc.get("kind") not in ("ClusterRole", "Role"):
                continue
            for rule in doc.get("rules") or []:
                resources = {str(r).lower() for r in (rule.get("resources") or [])}
                verbs = {str(v).lower() for v in (rule.get("verbs") or [])}
                hit = resources & WORKLOAD_KINDS
                if hit and (verbs & CREATE_VERBS):
                    granted.append(f"{sorted(hit)}:{sorted(verbs & CREATE_VERBS)}")
    if not granted:
        rs.ok("CP5.5", "A deployment granting no workload-create authority passes Core in full",
              f"read {', '.join(read) or '(none found)'}: no rule grants create on any of "
              f"{sorted(WORKLOAD_KINDS)}. The cluster-compute profile is therefore NOT "
              "APPLICABLE — a verified decline, which is a different fact from a profile "
              "that applied and went unverified")
    else:
        rs.ok("CP5.5", "A deployment granting no workload-create authority passes Core in full",
              f"workload-create authority IS granted ({granted}), so the cluster-compute "
              "profile applies and its Extended checks are exercised. This check does not "
              "fail on the grant; it establishes which branch the deployment is on")

    cluster_compute = bool(granted)

    # ── CP7 — zone restrictions (ADR-0058, amendment Part E) ─────────────────
    #
    # The ControlPlane row of the `zone-restrictions` profile. Candidate filter 11
    # reads FleetZone.status.restrictions.effective[] and the RestrictionsResolved
    # condition -- "the resolved projection, never spec.restrictions and never the raw
    # tree" (FZ-R4). A deployment whose FleetZone CRD does not carry that projection
    # cannot have a conforming filter 11, whatever its controller code says: there is
    # nowhere for the answer to live.
    #
    # STATIC, and honest about it. This profiler reads artefacts, not a cluster, so it
    # proves the SURFACE exists rather than that the filter behaves. The behavioural
    # half is Go: internal/controller/dispatch_zone_restrictions_test.go and its
    # envtest sibling. Reporting this as more than it is would be the over-claim the
    # rest of this file is careful to avoid.
    zone_doc = _load(rs, "swarmada.io_fleetzones.yaml")
    effective = _status_node(zone_doc, ["restrictions", "properties", "effective"]) or \
        (_status_node(zone_doc, ["restrictions"]).get("properties") or {}).get("effective", {})
    entry_props = set((effective.get("items", {}).get("properties") or {}).keys())
    required = {"axis", "mode", "inheritedFrom"}
    missing = sorted(required - entry_props)
    if not missing:
        rs.ok("CP7.1", "The resolved zone-restriction projection filters 11 and 12 read exists",
              "FleetZone.status.restrictions.effective[] carries "
              f"{', '.join(sorted(entry_props))}. inheritedFrom is the load-bearing one: a "
              "refusal must name the zone that DECLARED the rule, which is very often not "
              "the target")
    else:
        rs.bad("CP7.1", "The resolved zone-restriction projection filters 11 and 12 read exists",
               f"FleetZone.status.restrictions.effective[] is absent or incomplete "
               f"(missing: {missing}). Candidate filter 11 has nowhere to read the effective "
               "rule from, so it must either re-resolve the zone tree itself -- which FZ-R4 "
               "forbids and FZ-R9 says drifts to nearest-ancestor-wins -- or fail open")

    # ── CP7.3 — FA-R2's carry, and why this profiler cannot grade it ─────────
    #
    # The obligation is that the plane FILLS AssignAction.operating_constraints on a real
    # dispatch, from the resolved projection at the zone the robot is being sent to. That
    # is a property of a running control plane talking to a running adapter, and no such
    # pairing exists in any harness in this tree: the FleetAdapter harness IS the control
    # plane for its own C17 rows and fabricates the rules it sends.
    #
    # THE CHECK THIS PROFILER *COULD* RUN IS THE ONE THAT MUST NOT BE GREEN. Reading the
    # proto descriptor and confirming the field exists would establish that the schema has
    # a slot, which is precisely what a reader would mistake for "the adapter was told".
    # The unimplemented-surface skip beside it (CP2) sets the register: a row nothing can
    # exercise is recorded as unexercised, not as a result.
    #
    # NEEDS_LIVE_CLUSTER, not SURFACE_NOT_IMPLEMENTED: the note below says the surface IS
    # implemented and verified in the reference implementation. What is missing is a live
    # dispatch for this static profiler to watch. The distinction became a published
    # claim with ADR-0070: surface-not-implemented grades the row not-supported-by-platform,
    # which would state that the deployment lacks a carry it has.
    rs.skip("CP7.3", "The assignment carries the resolved operating rules the adapter must honour",
            SkipCause.NEEDS_LIVE_CLUSTER,
            "this profiler reads artefacts, and FA-R2's carry is a property of a live "
            "dispatch: whether the field is FILLED, from the resolved projection at the "
            "robot's leaf, cannot be read off a schema. Confirming the field exists would "
            "be a green line meaning 'the schema has a slot', which is the over-claim this "
            "row refuses to make. The behavioural half is verified in the reference "
            "implementation: "
            "internal/controller/zone_operating_constraints_dispatch_envtest_test.go "
            "drives the real reconciler and asserts the constraint set reaches the "
            "assignment, and internal/command/operating_constraints_wire_test.go asserts it "
            "reaches the AssignAction message. The ADAPTER half — that a running adapter "
            "acts on what arrives — is C17.1/C17.2/C17.4 on the FleetAdapter surface, and "
            "those are live")

    # The spec half: a projection with nothing to project is not the feature.
    spec_props = _spec_property_names(zone_doc)
    if "restrictions" in spec_props:
        rs.ok("CP7.2", "A zone can state a restriction in the fleet's own vocabulary",
              "FleetZone.spec.restrictions is present, so a site rule is expressible in the "
              "API rather than only inside whichever application happens to be dispatching")
    else:
        rs.bad("CP7.2", "A zone can state a restriction in the fleet's own vocabulary",
               "FleetZone.spec.restrictions is absent; a site policy cannot be stated at all")

    # ── CP2 — dispatch (runtime) ─────────────────────────────────────────────
    for cid, title in (
        ("CP2.1", "Derive the invocation key from (namespace, action, runGeneration, attempt)"),
        ("CP2.2", "Never block a reconcile on an endpoint call"),
        ("CP2.3", "Assign seq in reply order from 1 per invocation (E10.3)"),
        ("CP2.4", "Never re-record the events of an already-processed invocation (E10.4)"),
        ("CP2.5", "Reject an event type the endpoint does not declare (E10.1)"),
):
        rs.skip(cid, title, SkipCause.SURFACE_NOT_IMPLEMENTED,
                "no controller in the tree dispatches to a ComputeEndpoint yet, so the "
                "dispatch path cannot be driven; the endpoint's half of the same "
                "obligations is verified by CE1 and CE6")

    # ── CP3 — a result is data ───────────────────────────────────────────────
    # NEEDS_LIVE_CLUSTER for the reason CP7.3 gives: the note names a running controller as
    # what is missing, not the surface.
    rs.skip("CP3.1", "No path reaches an actuator without an author-written construct",
            SkipCause.NEEDS_LIVE_CLUSTER,
            "reaching the negative requires a running controller to exhaust; the "
            "structural half is CP3.2, which does run", level="MUST NOT")

    flow = _load(rs, "swarmada.io_fleettasks.yaml")
    schema = flow["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
    found_select: list[tuple[bool, list[str]]] = []

    def find_select(node) -> None:
        if isinstance(node, dict):
            props = node.get("properties")
            if isinstance(props, dict) and "select" in props:
                sel = props["select"]
                found_select.append(("default" in (sel.get("required") or []),
                                     sorted(sel.get("required") or [])))
            for v in node.values():
                find_select(v)
        elif isinstance(node, list):
            for item in node:
                find_select(item)

    find_select(schema)
    if found_select and all(req for req, _ in found_select):
        rs.ok("CP3.2", "An unenumerated value takes the default branch",
              f"every `select` in the FleetTask schema ({len(found_select)} occurrence(s)) "
              f"requires `default` ({found_select[0][1]}), so a select is TOTAL: a model "
              "returning something the author never enumerated selects the default rather "
              "than becoming a new instruction")
    elif not found_select:
        rs.skip("CP3.2", "An unenumerated value takes the default branch",
                SkipCause.SURFACE_NOT_IMPLEMENTED, "no `select` node in the shipped schema")
    else:
        rs.bad("CP3.2", "An unenumerated value takes the default branch",
               f"a `select` occurrence does not require `default`: {found_select}")

    rs.skip("CP3.3", "requireApproval defaults safer for responses committing physical work",
            SkipCause.SURFACE_NOT_IMPLEMENTED,
            "TaskTrigger is not in the shipped CRD set, so no default can be read off it",
            level="SHOULD")

    # ── CP4 — image provenance ───────────────────────────────────────────────
    rs.skip("CP4.1", "The resolved digest of the bytes serving a run is recorded on the run",
            SkipCause.SURFACE_NOT_IMPLEMENTED,
            "no status field records a resolved executor digest; the endpoint's half is "
            "CE8.1, which does run")

    mutable = sorted(p for d in list(crds.values()) + [cwt]
                     for p in _spec_property_names(d)
                     if "tagref" in p.lower() or "mutable" in p.lower())
    cfg = _load(rs, "swarmada.io_swarmadaconfigs.yaml")
    allow = sorted(p for p in _spec_property_names(cfg) if "mutableimage" in p.lower())
    if not mutable and not allow:
        rs.ok("CP4.2", "A mutable image reference is never admitted",
              "no field in the shipped schemas can express a mutable image reference "
              "(no `imageTagRef`, no `allowMutableImageRefs`), so one cannot be admitted "
              "and a run cannot resolve through one. Containment by absence, which is the "
              "strongest form this check takes — when the field is added, this check "
              "becomes a live assertion against it and a deployment enabling it is not "
              "Core-conformant", level="MUST NOT")
    else:
        rs.bad("CP4.2", "A mutable image reference is never admitted",
               f"mutable reference fields present: {mutable}; config gate: {allow}. A run "
               "resolving through a mutable tag fails Core, and no configuration rescues it",
               level="MUST NOT")

    # ── CP5.1-5.4 — only when the profile applies ────────────────────────────
    if cluster_compute:
        sa_rules = _validations(cwt, ["spec"])
        refuses_sa = [r for r in sa_rules if "serviceAccountName" in r]
        (rs.ok if refuses_sa else rs.bad)(
            "CP5.1", "A fixed low-privilege ServiceAccount is stamped and a template "
                     "naming its own is refused",
            f"CEL on ComputeWorkloadTemplate.spec: {refuses_sa[0][:80]!r}…" if refuses_sa
            else "no CEL rule refuses a template-named ServiceAccount")
        for cid, title in (
            ("CP5.2", "Workload creation is namespace-scoped, never cluster-wide"),
            ("CP5.3", "Workloads are created only from an admin template referenced by name"),
            ("CP5.4", "A created workload carries an ownerReference to the action"),
):
            rs.skip(cid, title, SkipCause.NEEDS_LIVE_CLUSTER,
                    "establishing what the controller creates needs a running API server")

    # ── CP6 — reactive dispatch ──────────────────────────────────────────────
    reactive_dispatch = "reactive" in _spec_property_names(
        crds["swarmada.io_fleetactions.yaml"])
    if reactive_dispatch:
        for cid, title in (
            ("CP6.1", "Refuse a block using a construct the declared grammar version lacks"),
            ("CP6.2", "Reject a predicate naming a control-plane source"),
            ("CP6.3", "Reject an effect outside the closed set of four"),
            ("CP6.4", "Refuse playCue when allowLocalCues is false"),
):
            rs.skip(cid, title, SkipCause.NEEDS_LIVE_CLUSTER,
                    "admission behaviour needs a running API server")

    # ── CP8 — status-write discipline (RA-1) ────────────────────────────────
    # The control plane's half of the adapter catalog's C6.3, graded here because the plane
    # holds the write. Nothing in the shipped artefacts shows whether status is written per
    # telemetry frame -- the manager needs robots/status write access for transitions, so the
    # RBAC cannot distinguish the two -- and establishing it needs a running control plane
    # fed telemetry. Recorded as not exercised rather than read off a schema.
    rs.skip("CP8.1", "No Robot status write per telemetry tick (RA-1)",
            SkipCause.NEEDS_LIVE_CLUSTER,
            "whether status is written per telemetry frame is a property of a running "
            "controller; the shipped RBAC grants robots/status for transitions and cannot "
            "tell the two apart", level="MUST NOT")

    trigger_consumer = (CRD_DIR / "swarmada.io_tasktriggers.yaml").exists()
    return {"cluster-compute": cluster_compute,
            "reactive-dispatch": reactive_dispatch,
            "trigger-consumer": trigger_consumer}
