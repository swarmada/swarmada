# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Published sentences that described behaviour the code does not have, held to the code.

Why this file exists. Each test below pins one published sentence -- a CRD description
that `kubectl explain` prints, an RFC-0001 chapter, `docs/operations.md`, the CHANGELOG
or the conformance catalog -- that once stated something the code does not do: a
check nothing performs, a field nothing reads, a notice nothing sends, a rule the code
states differently. Where the code fact can be read statically, the test reads it and
requires the text to agree with it, so the test fails again if the code changes and
the text does not. Where it cannot, the test pins the corrected text and says so.

Nothing here needs a cluster, an adapter or a build: CRD descriptions are read from the
generated `config/crd/bases/` YAML, which `make manifests` writes from `api/v1`.
"""

from __future__ import annotations

import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
CRDS = ROOT / "config" / "crd" / "bases"
SPEC = ROOT / "rfcs" / "dist" / "RFC-0001-core-spec.md"


def _read(rel: str) -> str:
    return (ROOT / rel).read_text()


def _spec() -> str:
    """The published RFC-0001: the assembled dist, not the chapter sources.

    Only `rfcs/dist/RFC-0001-core-spec.md` and the overview are promoted; the chapter
    sources under `rfcs/rfc-0001-core-spec/` are not, so a test that read them would fail
    on the published tree. The dist is assembled from the chapters, and the authoring tree's
    staleness check fails when it is older than them, so reading it tests the same text.
    """
    return SPEC.read_text()


def _has(text: str, phrase: str) -> bool:
    """`phrase in text`, as a call: a failing bare `in` on a whole document makes pytest diff
    megabytes of text, which turns a red into a hang."""
    return phrase in text


def _lacks(text: str, phrase: str) -> bool:
    """`phrase not in text`, as a call, for the same reason as `_has`."""
    return phrase not in text


def _flat(text: str) -> str:
    return " ".join(text.split())


def _crd_description(plural: str, path: str) -> str:
    """The description `kubectl explain <kind>.<path>` prints, from the generated CRD."""
    doc = yaml.safe_load((CRDS / f"swarmada.io_{plural}.yaml").read_text())
    node = doc["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
    for part in path.split("."):
        node = node["properties"][part]
        if part != path.split(".")[-1] and node.get("type") == "array":
            node = node["items"]
    return _flat(node.get("description", ""))


def _go_readers(pattern: str) -> list[str]:
    """Non-test Go under internal/ and cmd/ whose source matches `pattern`."""
    hits = []
    for top in ("internal", "cmd"):
        for p in (ROOT / top).rglob("*.go"):
            if not p.name.endswith("_test.go") and re.search(pattern, p.read_text()):
                hits.append(str(p.relative_to(ROOT)))
    return hits


# ── Fields no code reads say so where an operator reads them ─────────────────────────

_UNREAD = [
    ("edgenodes", "spec.identity.spiffeID", r"\.SPIFFEID\b"),
    ("artifactstores", "spec.objectStore.credentialRef", r"\.CredentialRef\b"),
    ("resultsinks", "spec.credentialRef", r"\.CredentialRef\b"),
]


def test_a_field_no_code_reads_says_so_in_kubectl_explain():
    wrong = []
    for plural, path, reader in _UNREAD:
        readers = _go_readers(reader)
        desc = _crd_description(plural, path)
        if readers:
            wrong.append(f"{plural} {path}: {readers} now read it; say what they do, then update "
                         "this test")
        elif "At v0.4" not in desc or not re.search(r"no code reads it|does not read it", desc):
            wrong.append(f"{plural} {path}: nothing reads it, and the description does not "
                         f"say so: {desc!r}")
    assert not wrong, "; ".join(wrong)


def test_edgenode_installed_models_says_no_rollout_reaches_a_node():
    ctrl = _read("internal/controller/modelrollout_controller.go")
    assert _has(ctrl, "RolloutTargetEdgeNode") and _has(ctrl, "TargetKindNotImplemented"), (
        "the ModelRollout controller no longer skips EdgeNode targets; update the description")
    desc = _crd_description("edgenodes", "spec.installedModels")
    assert "skips a rollout whose spec.targetKind is EdgeNode" in desc, desc
    assert "locates the entry by spec.modelName exactly as it does" not in desc, desc


def test_the_edgenode_chapter_example_does_not_present_the_identity_checks_as_live():
    text = _flat(_spec())
    assert _lacks(text, "# OPTIONAL. When set it is # checked IN ADDITION")
    assert _has(text, "Read by no code at # v0.4")


# ── The estop resume rule ────────────────────────────────────────────────────────────


def test_the_resume_rule_text_names_the_paused_exemption_the_code_has():
    code = _read("internal/safety/dispatcher.go")
    assert re.search(r"c\.Status == fleetv1\.CapabilityStatusActive \|\| c\.Paused", code), (
        "capabilityRefusal no longer exempts a Paused capability; update the texts below")
    desc = _crd_description("robots", "status.estopBaseline")
    assert "Paused" in desc and "every capability that is not Paused to be Active" in desc, desc
    stale = {
        "docs/operations.md": "requires every capability to be Active",
        "CHANGELOG.md": "while any declared capability is not Active",
    }
    wrong = [f"{f}: {s!r}" for f, s in stale.items() if s in _flat(_read(f))]
    assert not wrong, "the pre-exemption rule is still stated: " + "; ".join(wrong)
    spec = _flat(_spec())
    for s in ("every capability must be Active", "requires every capability to be Active"):
        assert _lacks(spec, s), f"the published RFC still states the pre-exemption rule: {s!r}"


# ── One field's description, and conditions that exist ─────────────────────────────


def test_trust_roots_description_is_its_own():
    desc = _crd_description("swarmadaconfigs", "spec.signing.trustRoots")
    assert desc.startswith("TrustRoots are the public-key anchors"), desc
    assert "A pointer because" not in desc and "deliberately kept out" not in desc, desc


def test_fleettask_crd_names_only_conditions_the_controller_sets():
    ctrl = "".join(p.read_text() for p in (ROOT / "internal" / "controller").glob("fleettask*.go")
                   if not p.name.endswith("_test.go"))
    set_types = set(re.findall(r'setCond\([^,]+,\s*"(\w+)"', ctrl))
    set_types |= set(re.findall(r'Condition\w+\s*=\s*"(\w+)"', ctrl))
    desc = _crd_description("fleettasks", "status.conditions")
    named = re.search(r"\(e\.g\. ([^)]*)\)", desc)
    assert named, desc
    unknown = [c.strip() for c in named.group(1).split(",") if c.strip() not in set_types]
    assert not unknown, f"the conditions description names types no controller sets: {unknown}"
    assert "start barrier" not in _crd_description("fleettasks", "spec.actions.dependsOn")


# ── Compute results: declared shape, and no egress ───────────────────────────────────


def test_computeendpoint_results_disclose_that_they_declare_no_value_domain():
    doc = yaml.safe_load((CRDS / "swarmada.io_computeendpoints.yaml").read_text())
    schema = doc["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
    spec = schema["properties"]["spec"]["properties"]
    assert "allowed" not in spec["results"]["items"]["properties"], (
        "results now declare allowed values; update the description")
    assert "declares no value domain" in _crd_description("computeendpoints", "spec.results")


def test_resultsink_spec_discloses_that_results_have_no_egress():
    assert not _go_readers(r"\bWriteResult\b"), "a result writer exists now; update the description"
    desc = _crd_description("resultsinks", "spec")
    assert "At v0.4 there is no egress path" in desc, desc
    assert "Principle 5" not in desc, desc


# ── Reconnect after an AfterTimeout reassignment ─────────────────────────────────────


def test_a_returning_robot_is_described_as_getting_idle_not_a_cancel_notice():
    registrar = _read("internal/registrar/registrar.go")
    assert re.search(r'actionName == ""\s*\{\s*return &fav1\.RobotActionState\{Phase: '
                     r"fav1\.RobotActionPhase_ROBOT_ACTION_PHASE_IDLE\}", registrar), (
        "a robot with no assigned action no longer gets IDLE on reconnect; update the chapter")
    text = _flat(_spec())
    assert _lacks(text, "notified on # reconnect that its action was cancelled")
    assert _has(text, "carries phase IDLE and no action id")


# ── Wording that narrated history or a site ──────────────────────────────────────────


def test_no_change_history_or_site_narration_in_these_sentences():
    desc = _crd_description("zonemaintenances", "spec.desiredState")
    assert "existed before" not in desc, desc
    fa = _spec()
    assert _lacks(fa, "S20-class") and _lacks(fa, "real today")


def test_an_unread_namespace_default_is_not_annotated_as_live():
    assert not _go_readers(r"\.DefaultAutoResumeMinutes\b"), (
        "a reader exists now; update the chapter")
    text = _spec()
    assert _lacks(text, "auto-resume 10 min after window closes")
    assert _lacks(text, "auto-resume 5 min after window closes")


def test_the_proto_carries_no_line_number_cross_references():
    """`:577 above` pointed at a lease comment and `:980 below` at a string field."""
    proto = _read("proto/fleet_adapter/v1/fleet_adapter.proto")
    assert not re.findall(r":\d+\s+(?:above|below)", proto)


def test_the_fleettask_chapter_does_not_claim_to_specify_spec_draft():
    text = _spec()
    assert _lacks(text, "which this chapter does specify")


# ── The conformance catalog cites the rule as the RFC states it ──────────────────────


def test_c39_describes_the_cancel_rule_the_cited_section_states():
    fap = _flat(_spec())
    assert _has(fap, "the control plane accepts either of two answers"), (
        "the fencing rule changed again; re-read the C3.9 entry against it")
    catalog = _flat(_read("adapters/CONFORMANCE.md"))
    c39 = catalog[catalog.index("**C3.9 (MUST)**"):catalog.index("## C4")]
    assert _has(c39, "accepts either of two answers")
    assert _lacks(c39, "the disagreement is open")
    assert _lacks(c39, "is rejected, `stale = true`")


# ── First-read docs ──────────────────────────────────────────────────────────────────


def test_first_read_docs_state_the_code():
    estop = _read("cmd/swarmctl/estop.go")
    assert re.search(r'if val == "" \{\s*val = "true"', estop), (
        "estop trigger no longer defaults its reason; update operations.md")
    ops = _flat(_read("docs/operations.md"))
    assert _lacks(ops, "required and non-empty on every `estop`")
    assert _lacks(ops, "the API rejects an empty or whitespace-only reason")

    action = _read("cmd/swarmctl/action.go")
    assert re.search(r'\[\]string\{"patch", "cancel"\}', action)
    assert _lacks(_flat(_read("CHANGELOG.md")), "still preflights `update`")

    assert _has(_read("api/v1/fleetaction_types.go"), "MinBatteryPct *int32")
    assert _lacks(_read("examples/warehouse-quickstart/SCENARIOS.md"), "that field doesn't exist")
    preset = _read("adapters/scenarios/presets/battery-edge.yaml")
    assert _lacks(preset, "FleetAction has no minBatteryPct")


# ── RFC-0001 core chapters ───────────────────────────────────────────────────────────


def _swarmctl_top_level_commands() -> set[str]:
    src = {p.name: p.read_text() for p in (ROOT / "cmd" / "swarmctl").glob("*.go")
           if not p.name.endswith("_test.go")}
    uses = {}
    for m in re.finditer(r"func (new\w+)\([^)]*\)[^{]*\{(.*?)\n\}", "\n".join(src.values()), re.S):
        use = re.search(r'Use:\s*"([\w-]+)', m.group(2))
        if use:
            uses[m.group(1)] = use.group(1)
    block = re.search(r"root\.AddCommand\((.*?)\n\t\)", src["root.go"], re.S)
    assert block, "cmd/swarmctl/root.go no longer registers commands in one AddCommand block"
    return {uses[c] for c in re.findall(r"(new\w+)\(", block.group(1)) if c in uses}


def test_rfc_chapters_name_only_swarmctl_commands_that_exist():
    top = _swarmctl_top_level_commands()
    assert {"get", "verify", "export", "estop"} <= top, top
    wrong = []
    lines = _spec().splitlines()
    assert sum("`swarmctl " in line for line in lines) > 10, "the scan would be vacuous"
    for n, line in enumerate(lines, 1):
        for m in re.finditer(r"`swarmctl ([\w-]+)", line):
            if m.group(1) not in top:
                wrong.append(f"{SPEC.relative_to(ROOT)}:{n} {m.group(0)}`")
    assert not wrong, "RFC-0001 names swarmctl commands that do not exist: " + "; ".join(wrong)


def test_the_offline_return_path_names_the_error_phase():
    ctrl = _read("internal/controller/robot_controller.go")
    assert re.search(r"func \(r \*RobotReconciler\) livePhase", ctrl)
    assert _has(ctrl, "RobotPhaseError")
    cp = _flat(_spec())
    m = re.search(r"is advanced on the next reconcile to (.*?)\(`isLivenessOwnedPhase`", cp)
    assert m and "`Error`" in m.group(1), m.group(0) if m else "sentence not found"


# ── The robots/status estop guard: mechanism and scope, as landed ────────────────────


def _estop_guard_row() -> str:
    """The admission-table row for the robots/status estop guard, in the published RFC."""
    rows = [line for line in _spec().splitlines()
            if line.startswith("| `Robot` (`status` subresource) |")]
    assert len(rows) == 1, f"expected one admission-table row for the guard, found {len(rows)}"
    return rows[0]


def _safety_guard_paragraph() -> str:
    """The safety chapter's sentences on the guard, from the published RFC."""
    spec = _flat(_spec())
    start = spec.find("The reference control plane enforces this at admission")
    assert start >= 0, "the safety chapter no longer describes the estop guard"
    end = spec.find("A conforming control plane MUST refuse the release", start)
    assert end > start, "the sentence after the guard's description has moved; re-anchor this"
    return spec[start:end]


def _robot_status_webhook() -> dict:
    docs = yaml.safe_load_all((ROOT / "config" / "webhook" / "manifests.yaml").read_text())
    for doc in docs:
        for hook in (doc or {}).get("webhooks", []):
            if hook["name"] == "vrobotstatus.swarmada.io":
                return hook
    raise AssertionError("vrobotstatus.swarmada.io is no longer registered; update the texts")


def test_the_estop_guard_text_names_its_mechanism_and_failure_policy():
    hook = _robot_status_webhook()
    policy = hook["failurePolicy"]
    assert [r for rule in hook["rules"] for r in rule["resources"]] == ["robots/status"]
    row = _estop_guard_row()
    assert f"`failurePolicy: {policy}`" in row, row
    texts = {f: _flat(_read(f)) for f in ("docs/operations.md", "CHANGELOG.md")}
    texts["the safety chapter"] = _safety_guard_paragraph()
    for f, text in texts.items():
        assert _has(text, "vrobotstatus.swarmada.io"), f
        assert _has(text, f"`failurePolicy: {policy}`"), f
        assert _has(text, "deploy-minimal") or _has(text, "installed"), (
            f"{f} must say where it does not hold")
        # Where the webhook is not installed, RBAC still keeps the adapter out: its role
        # holds no robots/status write. Each text says so.
        assert _has(text, "`swarmada:robot`") or _has(text, "Fleet Adapter's role"), (
            f"{f} must say the adapter's role holds no robots/status write on any install")
    role = yaml.safe_load((ROOT / "config" / "rbac" / "swarmada_robot_role.yaml").read_text())
    assert not [r for r in role["rules"] if "robots/status" in r.get("resources", [])], (
        "swarmada:robot grants robots/status again; the texts above say it does not")


def test_the_status_guard_text_claims_exactly_the_fields_it_guards():
    guard = _read("internal/webhook/robot_status_estop_guard.go")
    compared = set(re.findall(r"oldRobot\.Status\.(\w+)", guard))
    # AssignedAction: the executor guard's record; a bound robot's Phase is guarded for the
    # same reason. Each text names both below.
    assert compared == {"EstopState", "EstopBaseline", "EstopHeldBy", "Stops", "CurrentZone",
                        "Phase", "Hardware", "Capabilities", "InstalledModels",
                        "ModelGrantedCapabilities", "AssignedAction"}, (
        f"the guard compares {sorted(compared)}; update the scope stated below")
    guarded = ("`status.estopHeldBy`", "`status.stops`", "`status.currentZone`", "`status.phase`",
               "`Error`",
               "`status.hardware`", "`Failed`", "`status.capabilities`", "`status.installedModels`",
               "`status.modelGrantedCapabilities`", "`status.assignedAction`", "`Assigned`",
               "`InProgress`")
    texts = {"the admission row": _estop_guard_row(),
             "the safety chapter": _safety_guard_paragraph(),
             "CHANGELOG.md": _flat(_read("CHANGELOG.md")),
             "docs/operations.md": _flat(_read("docs/operations.md"))}
    for f, text in texts.items():
        for field in guarded:
            assert _has(text, field), f"{f} does not name {field} among the guarded inputs"
    assert "The other `status` fields are not guarded" in texts["the admission row"]
    assert _has(texts["CHANGELOG.md"], "the other status fields are not guarded")
    assert _has(texts["docs/operations.md"], "It guards those fields only")


# ── No claim that the published CI tests the Python floor ────────────────────────────


def test_no_doc_claims_ci_tests_the_python_floor():
    """The published CI runs on Python 3.12 only.

    The workflow that ships is public `main`'s `ci.yml` plus the envtest patch; the
    `python-floor` job exists only in this tree's `ci.yml`, so a check that read the
    workflow here would see a job the published repository does not have. This is a text
    pin: the three sentences that claimed it are gone, and the floor itself (Python 3.10,
    protobuf 3.20.3, grpcio 1.64.1) is still stated as a declaration, not as a CI result.
    """
    claims = {
        "docs/adapter-authoring.md": ["CI builds the SDK and the rendered template",
                                      "CI imports them at that"],
        "CHANGELOG.md": ["CI tests both ends of the runtime"],
    }
    found = [f"{f}: {c!r}" for f, cs in claims.items() for c in cs if c in _flat(_read(f))]
    assert not found, (
        "a doc still says the published CI tests the Python floor: " + "; ".join(found))
    assert _has(_read("docs/adapter-authoring.md"), "**Python 3.10 or newer**")
