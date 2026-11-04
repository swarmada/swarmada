# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""`kubectl explain fleettask.spec.inputs` says what the FleetTask webhook checks on update.

Why this file exists. The field's description said a binding naming an undeclared input
"is rejected at admission on CREATE only" and that "an update is not checked the same
way". The webhook re-runs the create-time binding check on every update that changes
`spec.actions` (`bindingSpecChanged`, `internal/webhook/fleettask_webhook.go`), and the API
server refuses any update to `spec.inputs` or `spec.flow`, which are immutable after create.
Understating a check misleads as much as overclaiming one. The description is generated
into both the FleetTask CRD and the TaskTrigger CRD's `taskTemplate`.
"""

from __future__ import annotations

import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
WEBHOOK = ROOT / "internal" / "webhook" / "fleettask_webhook.go"
CRDS = {
    "fleettasks": ("spec", "inputs"),
    "tasktriggers": ("spec", "taskTemplate", "inputs"),
}


def _description(plural: str, path: tuple[str, ...]) -> str:
    path_ = ROOT / "config" / "crd" / "bases" / f"swarmada.io_{plural}.yaml"
    crd = yaml.safe_load(path_.read_text())
    node = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
    for key in path:
        node = node["properties"][key]
    return " ".join(node["description"].split())


def test_the_webhook_rechecks_bindings_when_inputs_flow_or_actions_change():
    src = WEBHOOK.read_text()
    body = re.search(r"func bindingSpecChanged\(.*?\n}", src, flags=re.S)
    assert body, "bindingSpecChanged not found; re-read the inputs description"
    for field in ("Flow", "Inputs", "Actions"):
        assert f"Spec.{field}" in body.group(0), (
            f"bindingSpecChanged no longer compares Spec.{field}")
    assert "validateBindings(newT)" in src, "the update path no longer runs validateBindings"


def test_the_inputs_description_says_updates_are_checked():
    for plural, path in CRDS.items():
        text = _description(plural, path)
        for wrong in ("on CREATE only", "an update is not checked"):
            assert wrong not in text, f"{plural} {'.'.join(path)} still says {wrong!r}"
        assert "every update that changes spec.actions" in text, (
            f"{plural} {'.'.join(path)} does not say which updates re-run the binding check")
        assert "spec.inputs and spec.flow are immutable after a FleetTask is created" in text, (
            f"{plural} {'.'.join(path)} does not say that inputs and flow cannot change")


def test_the_fleettask_crd_refuses_an_inputs_or_flow_change():
    crd = yaml.safe_load(
        (ROOT / "config" / "crd" / "bases" / "swarmada.io_fleettasks.yaml").read_text())
    spec = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]
    rules = " ".join(r["rule"] for r in spec.get("x-kubernetes-validations", []))
    for field in ("inputs", "flow"):
        assert f"oldSelf.{field}" in rules, f"FleetTask.spec carries no transition rule on {field}"
