"""The floor-mapping demo's task samples say what their payloads are, and the adapter serves them.

Every payload in examples/floor-mapping/tasks/ is written as ``raw:`` (base64 of the JSON).
A plain-key payload would reach the adapter as well; the samples use the ``raw`` form. A base64
string is unreadable, so each one carries the JSON in the comment above it, and this test keeps
the two from drifting apart.

Every robot action in them is ``onFailure: Alert``: the default, Requeue, dispatches a failed
action again with no operator, and a robot action that failed -- a segment the robot refused
before motion, say, which the operator then charges -- must not move again until one has looked.
"""

from __future__ import annotations

import base64
import json
import pathlib

import pytest
import yaml

from adapters.simulation import floor_mapping as fm

_TASKS = pathlib.Path(__file__).resolve().parents[2] / "examples" / "floor-mapping" / "tasks"


def _raw_payloads():
    """(file, commented JSON, decoded raw) for every ``raw:`` line."""
    for path in sorted(_TASKS.glob("*.yaml")):
        lines = path.read_text().splitlines()
        for i, line in enumerate(lines):
            if line.strip().startswith("raw:"):
                comment = lines[i - 1].strip()
                assert comment.startswith("# {"), (
                    f"{path.name}:{i + 1} raw payload has no JSON comment")
                decoded = base64.b64decode(line.split("raw:", 1)[1].strip()).decode()
                yield path.name, json.loads(comment[2:]), json.loads(decoded)


def _actions(doc):
    if doc["kind"] == "FleetAction":
        yield doc["spec"]
    else:
        for member in doc["spec"]["actions"]:
            yield member["action"]


def test_every_raw_payload_is_the_json_its_comment_says():
    found = list(_raw_payloads())
    assert len(found) == 5
    for name, commented, decoded in found:
        assert decoded == commented, name


def test_no_sample_payload_uses_plain_keys():
    for path in sorted(_TASKS.glob("*.yaml")):
        doc = yaml.safe_load(path.read_text())
        for spec in _actions(doc):
            if "payload" in spec:
                assert set(spec["payload"]) == {"raw"}, f"{path.name}: {spec['payload']}"


@pytest.mark.parametrize("name", ["explore-first", "explore-resume"])
def test_the_explore_payloads_are_ones_the_adapter_serves(name):
    doc = yaml.safe_load((_TASKS / "explore-floor.yaml").read_text())
    [member] = [m for m in doc["spec"]["actions"] if m["name"] == name]
    payload = json.loads(base64.b64decode(member["action"]["payload"]["raw"]))
    assert member["action"]["customType"] == payload["operation"] == fm.OPERATION_EXPLORE
    assert payload["max_speed_mps"] <= fm.MAX_SPEED_MPS
    assert fm.validate_explore(payload) == ""
    assert payload["resume"] is (name == "explore-resume")


def test_every_robot_action_alerts_and_is_never_requeued():
    found = []
    for path in sorted(_TASKS.glob("*.yaml")):
        doc = yaml.safe_load(path.read_text())
        for spec in _actions(doc):
            if "executor" in spec:
                continue                      # not a robot action: never scheduled to one
            found.append(path.name)
            assert spec.get("onFailure") == "Alert", (
                f"{path.name}: a {spec['type']} action has onFailure "
                f"{spec.get('onFailure', 'unset, which is Requeue')}")
    assert len(found) == 6, found
