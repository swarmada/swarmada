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

"""C19.3 and C19.4 — an assignment with no action type, or with a value its catalog does not
allow, is refused.

C19.1 probes an undeclared type by name. No row sent an assignment with no type at all, so an
adapter that ran an untyped assignment as its catalog's only type passed every row, exactly as
one that refused it did. C19.2 grades a missing required parameter, and nothing graded a
declared enum parameter carrying a value outside its `allowed` set.

Each refusal row is paired with a CONTROL -- the same assignment, corrected, must be accepted --
so an adapter that refuses everything fails the row instead of passing it.
"""

from __future__ import annotations

import json
import queue

from adapters.conformance.report import CheckStatus, SkipCause

MODES = ("eco", "fast")


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got {[r.check_id for r in report.results]}")


class _Adapter:
    """Answers assignments by a rule.

    'enforce'          refuses an undeclared or empty type, a missing required parameter and
                       an enum value outside `allowed`, INVALID_ACTION; accepts the rest.
    'untyped-as-sole'  as 'enforce', except that an empty type is run as the catalog's sole
                       declared type (the accommodation no row used to see).
    'ignore-values'    as 'enforce', except that enum values are never checked.
    'refuse-all'       refuses every assignment INVALID_ACTION.
    """

    def __init__(self, pb, session, rule, catalog):
        self.pb, self.session, self.rule = pb, session, rule
        self.catalog = {a.action_type: a for a in catalog}
        self.seen = []

    def _bad(self, action_type, payload):
        if not action_type and self.rule == "untyped-as-sole" and len(self.catalog) == 1:
            action_type = next(iter(self.catalog))
        action = self.catalog.get(action_type)
        if action is None:
            return True
        for p in action.params:
            if p.required and p.name not in payload:
                return True
            if (self.rule != "ignore-values" and p.kind == "enum" and p.name in payload
                    and payload[p.name] not in p.allowed):
                return True
        return False

    def pump(self):
        pb = self.pb
        while True:
            try:
                msg = self.session.control_out.get_nowait()
            except queue.Empty:
                return
            if msg.WhichOneof("payload") != "command":
                continue
            cmd = msg.command
            res = pb.CommandResult(command_id=cmd.command_id, robot_id=cmd.robot_id)
            if cmd.WhichOneof("command") == "assign_action":
                a = cmd.assign_action
                payload = json.loads(a.payload_json or b"{}")
                self.seen.append((a.action_type, payload))
                if self.rule != "refuse-all" and not self._bad(a.action_type, payload):
                    res.assign_action.CopyFrom(pb.AssignActionResult(
                        accepted=True, accepted_fencing_token=a.fencing_token))
                else:
                    res.assign_action.CopyFrom(pb.AssignActionResult(
                        accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION,
                        message="not served"))
            else:
                res.cancel_action.CopyFrom(pb.CancelActionResult(acknowledged=True))
            self.session.control_in.put(pb.AdapterMessage(command_result=res))


def _catalog(pb, *, with_enum=True):
    params = [pb.ActionParam(name="x", unit="m", kind="number", required=True),
              pb.ActionParam(name="y", unit="m", kind="number", required=True)]
    if with_enum:
        params.append(pb.ActionParam(name="mode", kind="enum", allowed=list(MODES)))
    return [pb.SupportedAction(action_type="Navigate", params=params)]


def _drive(drv, session, pb, rule, *, with_enum=True):
    drv._catalog = _catalog(pb, with_enum=with_enum)
    drv._served = drv._catalog[0]
    adapter = _Adapter(pb, session, rule, drv._catalog)
    real = drv._await_result

    def _await(command_id):
        adapter.pump()
        return real(command_id)

    drv._await_result = _await
    drv._check_catalog_enforced()
    return adapter


# ── C19.3: no action type ────────────────────────────────────────────────────

def test_c193_green_for_an_adapter_that_refuses_an_untyped_assignment(driver, pb):
    drv, session, report = driver
    adapter = _drive(drv, session, pb, "enforce")
    row = _row(report, "C19.3")
    assert row.status is CheckStatus.PASS, row.detail
    untyped = [p for t, p in adapter.seen if t == ""]
    assert len(untyped) == 1, "exactly one probe carries no action type"
    assert {"x", "y"} <= set(untyped[0]), "the untyped probe is otherwise complete"


def test_c193_red_for_an_adapter_that_runs_an_untyped_assignment_as_its_sole_type(driver, pb):
    """The accommodation: an empty type taken as the catalog's only type and driven."""
    drv, session, report = driver
    _drive(drv, session, pb, "untyped-as-sole")
    row = _row(report, "C19.3")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "ACCEPTED" in row.detail and row.is_blocking_failure
    assert _row(report, "C19.1").status is CheckStatus.PASS, "C19.1 alone cannot see this"


def test_c193_red_for_an_adapter_that_refuses_everything(driver, pb):
    drv, session, report = driver
    _drive(drv, session, pb, "refuse-all")
    row = _row(report, "C19.3")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "control" in row.detail


# ── C19.4: a value outside a declared enum's `allowed` ───────────────────────

def test_c194_green_for_an_adapter_that_refuses_a_value_it_does_not_allow(driver, pb):
    drv, session, report = driver
    adapter = _drive(drv, session, pb, "enforce")
    row = _row(report, "C19.4")
    assert row.status is CheckStatus.PASS, row.detail
    modes = [p.get("mode") for _t, p in adapter.seen if "mode" in p]
    assert len(modes) == 2, "one probe and one control carry the enum parameter"
    assert modes[0] not in MODES and modes[1] in MODES


def test_c194_red_for_an_adapter_that_ignores_declared_values(driver, pb):
    drv, session, report = driver
    _drive(drv, session, pb, "ignore-values")
    row = _row(report, "C19.4")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "ACCEPTED" in row.detail and row.is_blocking_failure


def test_c194_red_for_an_adapter_that_refuses_everything(driver, pb):
    drv, session, report = driver
    _drive(drv, session, pb, "refuse-all")
    row = _row(report, "C19.4")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "control" in row.detail


def test_c194_not_applicable_when_no_enum_parameter_is_declared(driver, pb):
    """With no declared closed set of values, there is no value to refuse."""
    from conformance.model import NOT_APPLICABLE_CAUSES
    from conformance.model import SkipCause as ModelCause

    drv, session, report = driver
    _drive(drv, session, pb, "enforce", with_enum=False)
    row = _row(report, "C19.4")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ADAPTER_DECLARES_NO_ENUM_PARAM.value)
    assert ModelCause.of(row.detail) in NOT_APPLICABLE_CAUSES


# ── the rows share one fence ─────────────────────────────────────────────────

def test_c19_probes_take_increasing_tokens_below_the_fencing_checks(driver, pb, harness_mod):
    """A refusal can still raise a fence, so each probe takes its own, higher token, and
    every one stays below the fencing checks' first fresh token."""
    drv, session, _report = driver
    sent = []
    real_send = drv._send_command

    def _send(**kw):
        if "assign_action" in kw:
            sent.append(kw["assign_action"].fencing_token)
        return real_send(**kw)

    drv._send_command = _send
    _drive(drv, session, pb, "enforce")
    assert sent == sorted(sent) and len(set(sent)) == len(sent) == 8, sent
    assert max(sent) < harness_mod._FENCING_FRESH_TOKEN
