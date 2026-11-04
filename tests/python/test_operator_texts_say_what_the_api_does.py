# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Operator and specification sentences, each pinned to the code or generated file they describe.

1. No operator text says a FleetAction delete is refused while the action runs, as long as the
   FleetAction webhook does not see DELETE; known-limitations says what the delete leaves behind.
2. The specification quotes the composite-zone sharedResources refusal the CRD emits, and no
   specification, rule-index or proto text says resource names resolve up the zone tree.
3. The ParentZone explain text names both lookups: the TDE's namespace-wide one and the Robot
   admission walk over the zone's ancestors for a charging dock.
4. The audit table's ESTOP_TRIGGERED row lists the late-arrival entry's own field and actors.
5. The outage runbook gives the webhook configuration back to a Helm release.
6. The Helm install notes' CRD command selects the CRDs the chart installs.
"""

from __future__ import annotations

import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
DIST = ROOT / "rfcs" / "dist" / "RFC-0001-core-spec.md"


def _read(rel: str) -> str:
    return (ROOT / rel).read_text(encoding="utf-8")


def _flat(text: str) -> str:
    return " ".join(text.split())


def _webhook_operations(name: str) -> set[str]:
    docs = [d for d in yaml.safe_load_all(_read("config/webhook/manifests.yaml")) if d]
    for doc in docs:
        for w in doc.get("webhooks") or []:
            if w["name"] == name:
                return {op for r in w["rules"] for op in r["operations"]}
    raise AssertionError(f"no webhook {name} in config/webhook/manifests.yaml")


def _fleetzone_spec() -> dict:
    crd = yaml.safe_load(_read("config/crd/bases/swarmada.io_fleetzones.yaml"))
    return crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]


def test_no_text_says_a_running_fleetaction_delete_is_refused():
    if "DELETE" in _webhook_operations("vfleetaction.swarmada.io"):
        return  # a delete check exists; the texts may then say it refuses
    ops = _flat(_read("docs/operations.md"))
    for stale in ("delete a settled task (Pending or terminal only)",
                  "Pending or terminal (Succeeded/Failed/Cancelled) only",
                  "is accepted **only** when nothing is executing"):
        assert stale not in ops, f"operations.md says a FleetAction delete is refused: {stale!r}"
    assert "not refused in any phase" in ops, "operations.md does not say the delete is not refused"
    kl = _read("docs/known-limitations.md")
    assert ("### Deleting a running FleetAction holds its robot until the record is released"
            in kl), (
        "known-limitations has no entry for a FleetAction deleted while it runs")


def test_the_spec_quotes_the_composite_shared_resources_refusal_the_crd_emits():
    rules = _fleetzone_spec().get("x-kubernetes-validations", [])
    messages = [_flat(r["message"]) for r in rules
                if "spec.sharedResources is not permitted on a composite zone" in r["message"]]
    assert len(messages) == 1, f"expected one composite sharedResources rule, found {messages}"
    dist = _flat(DIST.read_text(encoding="utf-8"))
    assert messages[0] in dist, f"the RFC does not quote the refusal the CRD emits: {messages[0]!r}"
    proto = "proto/fleet_adapter/v1/fleet_adapter.proto"
    texts = {
        "rfcs/dist/RFC-0001-core-spec.md": dist,
        "docs/rule-index.md": _flat(_read("docs/rule-index.md")),
        proto: _read(proto),
    }
    for where, text in texts.items():
        for stale in ("walking up the zone tree", "walks containment edges only",
                      "inheritance down the containment tree", "resolved up the zone tree",
                      "resolution walks the containment tree, so"):
            assert stale not in text, f"{where} says resources resolve up the tree: {stale!r}"


def test_the_parentzone_explain_text_names_both_lookups():
    desc = _flat(_fleetzone_spec()["properties"]["memberZones"]["description"])
    assert "whole namespace" in desc, "the ParentZone explain text does not name the TDE's lookup"
    assert "dockName" in desc and "ancestors" in desc, (
        "the ParentZone explain text does not name the dock admission walk")
    webhook = _read("internal/webhook/robot_webhook.go")
    assert "is not a ChargingDock in zone %q or its ancestors" in webhook, (
        "Robot admission no longer walks the ancestors for a dock; re-read the ParentZone text")


def test_the_estop_triggered_row_lists_the_late_arrival_entry():
    writers = {"zoneestop-controller": "internal/controller/zoneestop_controller.go",
               "namespaceestop-controller": "internal/controller/namespaceestop_controller.go"}
    row = next((ln for ln in DIST.read_text(encoding="utf-8").splitlines()
                if ln.startswith("| `ESTOP_TRIGGERED` |") and "| emitted |" in ln), "")
    assert row, "the required-events table has no ESTOP_TRIGGERED row"
    for actor, src in writers.items():
        code = _read(src)
        if '"late_arrival"' not in code:
            continue
        assert f'"{actor}"' in code, f"{src} no longer names {actor}"
        assert "`late_arrival`" in row and f"`{actor}`" in row, (
            f"the ESTOP_TRIGGERED row does not list the late-arrival entry {src} writes")


def test_the_outage_runbook_gives_the_configuration_back_to_helm():
    ops = _read("docs/operations.md")
    sec = ops[ops.index("### Give the configuration back to Helm"):]
    block = re.search(r"```sh\n(.*?)```", sec, re.S)
    assert block, "the Helm section has no commands"
    assert "--show-managed-fields" in block.group(1), "it does not read how Helm writes the object"
    assert re.search(r"^helm upgrade .*--reuse-values --force-conflicts$", block.group(1), re.M), (
        "it does not upgrade the release with --force-conflicts")
    assert ops.count("(#give-the-configuration-back-to-helm)") == 2, (
        "both procedures that use kubectl replace must point to it")


def test_the_helm_notes_crd_command_selects_the_chart_crds():
    notes = _read("deploy/swarmada/templates/NOTES.txt")
    cmd = next(ln.strip() for ln in notes.splitlines() if "kubectl get crd" in ln)
    names = sorted(p.name for p in (ROOT / "config/crd/bases").glob("swarmada.io_*.yaml"))
    assert len(names) >= 21, f"expected the 21 CRDs, found {len(names)}"
    label = re.search(r"-l ([\w./-]+)=([\w.-]+)", cmd)
    if label:
        key, value = label.groups()
        for n in names:
            meta = yaml.safe_load(_read(f"config/crd/bases/{n}"))["metadata"]
            assert (meta.get("labels") or {}).get(key) == value, (
                f"NOTES selects CRDs by {key}={value}; {n} does not carry it, so it lists nothing")
    else:
        assert "grep swarmada.io" in cmd, f"NOTES CRD command selects nothing known: {cmd!r}"
