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
"""The simulation adapter at contract 1.4.0: one catalog entry per Custom verb, and the verb
read from ``custom_type`` on the assignment and on validate_action.

The reading, in order: ``custom_type`` when it is set, and the payload's ``operation``
otherwise (the convention stays accepted). Both set and different is refused, and so is a
verb the adapter does not serve: it is never run as the verbless Custom.
"""
import json

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

EXPLORE = {"bound_ref": "room-1", "max_speed_mps": 0.2}


def _adapter(tmp_path) -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim",
                   artifact_dir=str(tmp_path), artifact_store="floor-maps")
    a._sim.spawn("amr-1", 0.0, 0.0)
    return a


def _drain(a: SimAdapter) -> list:
    out = []
    while not a._outbox.empty():
        out.append(a._outbox.get_nowait())
    return out


def _results(a, kind):
    return [getattr(m.command_result, kind) for m in _drain(a)
            if m.WhichOneof("payload") == "command_result"]


def _assign(a, payload, token=1, custom_type=""):
    a._handle_command(pb.Command(command_id=token, robot_id="amr-1", assign_action=pb.AssignAction(
        action_id=f"ns/act-{token}", assignment_id=f"a{token}", action_type="Custom",
        custom_type=custom_type, fencing_token=token, lease_duration_ms=600_000,
        lease_generation=token, payload_json=json.dumps(payload).encode())))
    return _results(a, "assign_action")[-1]


def _validate(a, payload, custom_type=""):
    a._handle_command(pb.Command(command_id=99, robot_id="amr-1", validate_action=pb.ValidateAction(
        action_type="Custom", custom_type=custom_type, payload_json=json.dumps(payload).encode())))
    return _results(a, "validate_action")[-1]


def test_the_catalog_keys_explore_floor_and_keeps_an_unkeyed_custom_entry(tmp_path):
    custom = [s for s in _adapter(tmp_path)._supported_actions() if s.action_type == "Custom"]
    assert [s.custom_type for s in custom] == ["explore-floor", ""]
    keyed, unkeyed = custom
    params = {p.name: p for p in keyed.params}
    # The verb stays legible to a reader below 1.4.0, as an enum naming it alone -- and is
    # NOT required: with custom_type set, a payload without `operation` is served, and a
    # catalog that said otherwise would be false.
    assert list(params["operation"].allowed) == ["explore-floor"]
    assert params["operation"].required is False
    assert {r.name for r in keyed.results} == {"map-ref", "map-sha256", "covered-m2"}
    # The verbless Custom carries none of explore-floor's parameters or results.
    assert list(unkeyed.params) == [] and list(unkeyed.results) == []


def test_custom_type_alone_names_the_verb(tmp_path):
    a = _adapter(tmp_path)
    res = _assign(a, EXPLORE, custom_type="explore-floor")
    assert res.accepted, res.message
    assert a._job is not None and type(a._job).__name__ == "ExploreJob", \
        "custom_type explore-floor must run the explore job, not the verbless Custom"


def test_a_verb_the_adapter_does_not_serve_is_refused_not_run_verbless(tmp_path):
    a = _adapter(tmp_path)
    res = _assign(a, {"x": 1.0, "y": 1.0}, custom_type="capture-clip")
    assert not res.accepted
    assert res.rejection == pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION
    assert "capture-clip" in res.message
    assert a._current_action == ""


def test_a_custom_type_that_contradicts_the_payload_is_refused(tmp_path):
    a = _adapter(tmp_path)
    res = _assign(a, {**EXPLORE, "operation": "mow"}, custom_type="explore-floor")
    assert not res.accepted
    assert res.rejection == pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION
    assert "mow" in res.message and "explore-floor" in res.message


def test_a_custom_type_that_agrees_with_the_payload_is_served(tmp_path):
    res = _assign(_adapter(tmp_path), {**EXPLORE, "operation": "explore-floor"},
                  custom_type="explore-floor")
    assert res.accepted, res.message


def test_without_custom_type_the_payload_operation_still_names_the_verb(tmp_path):
    a = _adapter(tmp_path)
    assert _assign(a, {**EXPLORE, "operation": "explore-floor"}).accepted
    assert type(a._job).__name__ == "ExploreJob"


@pytest.mark.parametrize("custom_type,payload,servable", [
    ("explore-floor", EXPLORE, True),
    ("capture-clip", {}, False),
    ("explore-floor", {**EXPLORE, "operation": "mow"}, False),
    ("", {**EXPLORE, "operation": "explore-floor"}, True),
])
def test_validate_action_reads_the_verb_the_same_way(tmp_path, custom_type, payload, servable):
    assert _validate(_adapter(tmp_path), payload, custom_type).servable is servable
