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

"""C19.5 — a Custom action with no custom_type is accepted when its payload names a served verb.

RFC-0001 declares ``AssignAction.custom_type`` "set only for a Custom action that names one",
and an adapter takes the verb "from `AssignAction.custom_type` ... when it is set, and from the
payload's `operation` otherwise" (fleet adapter protocol, *One entry per Custom verb*). Every
Custom action a control plane below 1.4.0 sends carries none. No row sent one and required it
accepted, so an adapter that refuses a Custom action without custom_type passed the suite.
"""

from __future__ import annotations

import json
import queue

from adapters.conformance.report import CheckStatus, SkipCause


def _sent_assigns(session):
    out = []
    while True:
        try:
            m = session.control_out.get_nowait()
        except queue.Empty:
            return out
        if hasattr(m, "WhichOneof") and m.WhichOneof("payload") == "command" \
                and m.command.WhichOneof("command") == "assign_action":
            out.append(m.command.assign_action)


def _catalog(pb, *, keyed=True):
    custom = pb.SupportedAction(action_type="Custom", params=[
        pb.ActionParam(name="operation", kind="enum", allowed=["explore-floor"])])
    if keyed:
        custom.custom_type = "explore-floor"
    return custom


def _answer(pb, session, cid, accepted, rejection=0):
    session.control_in.put(pb.AdapterMessage(command_result=pb.CommandResult(
        command_id=cid, assign_action=pb.AssignActionResult(
            accepted=accepted, rejection=rejection))))


def _row(report):
    (row,) = [r for r in report.results if r.check_id == "C19.5"]
    return row


def test_the_probe_carries_no_custom_type_and_names_the_verb_in_operation(pb, driver):
    drv, session, report = driver
    drv._catalog = [drv._served, _catalog(pb)]
    _answer(pb, session, drv._next_command_id, True)
    drv._check_custom_without_type(9)
    (assign, *_) = _sent_assigns(session)
    assert assign.action_type == "Custom" and assign.custom_type == ""
    assert json.loads(assign.payload_json)["operation"] == "explore-floor"
    assert assign.fencing_token == 9
    assert _row(report).status is CheckStatus.PASS


def test_an_unkeyed_entry_names_its_verb_by_its_operation_enum(pb, driver):
    drv, session, report = driver
    drv._catalog = [_catalog(pb, keyed=False)]
    _answer(pb, session, drv._next_command_id, True)
    drv._check_custom_without_type(9)
    (assign, *_) = _sent_assigns(session)
    assert json.loads(assign.payload_json)["operation"] == "explore-floor"
    assert _row(report).status is CheckStatus.PASS


def test_c19_5_fails_an_adapter_that_requires_custom_type(pb, driver):
    drv, session, report = driver
    drv._catalog = [drv._served, _catalog(pb)]
    _answer(pb, session, drv._next_command_id, False,
            pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION)
    drv._check_custom_without_type(9)
    row = _row(report)
    assert row.status is CheckStatus.FAIL, row.detail


def test_no_custom_verb_declared_is_recorded_with_its_cause(pb, driver):
    drv, _session, report = driver
    drv._catalog = [drv._served]
    drv._check_custom_without_type(9)
    row = _row(report)
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ADAPTER_DECLARES_NO_CUSTOM_VERB.value)


def test_c19_5_runs_when_no_enum_parameter_is_declared(pb, driver, harness_mod, monkeypatch):
    """C19.4's skip path returned before anything after it could run."""
    drv, _session, report = driver
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.05)
    drv._catalog = [drv._served]
    drv._check_catalog_enforced()
    ids = {r.check_id for r in report.results}
    assert {"C19.4", "C19.5"} <= ids, sorted(ids)
