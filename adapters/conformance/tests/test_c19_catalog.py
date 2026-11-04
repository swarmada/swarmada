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

"""C12.2 and C19 — an adapter serves what it declares, and declares it where the plane reads.

Twelve of the harness's fourteen assignment probes carried no action type, and several no
target. So no row graded an adapter refusing a type it never declared or a Navigate with no
target, and an adapter that ran every assignment as a drive to the origin passed every row.
Nothing read the snapshot pushed at registration either, which at v0.4 is the only way a
catalog reaches the plane.

Every refusal row here is paired with a CONTROL -- the same assignment, corrected, must be
accepted -- so an adapter that refuses everything fails the row instead of passing it.
"""

from __future__ import annotations

import json
import queue

from adapters.conformance.report import CheckStatus, SkipCause


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got {[r.check_id for r in report.results]}")


class _Adapter:
    """Answers assignments by a rule: 'enforce' (a correct catalog), 'accept-all',
    'refuse-all', or 'wrong-code' (refuses the bad ones with ROBOT_UNAVAILABLE)."""

    def __init__(self, pb, session, rule, declared=("Navigate",), required=("x", "y")):
        self.pb, self.session, self.rule = pb, session, rule
        self.declared, self.required = set(declared), set(required)
        self.seen = []

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
                self.seen.append((a.action_type, json.loads(a.payload_json or b"{}")))
                bad = (a.action_type not in self.declared
                       or not self.required <= set(json.loads(a.payload_json or b"{}")))
                if self.rule == "accept-all" or (self.rule in ("enforce", "wrong-code")
                                                 and not bad):
                    res.assign_action.CopyFrom(pb.AssignActionResult(
                        accepted=True, accepted_fencing_token=a.fencing_token))
                else:
                    code = (pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE
                            if self.rule == "wrong-code" else
                            pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION)
                    res.assign_action.CopyFrom(pb.AssignActionResult(
                        accepted=False, rejection=code, message="not served"))
            else:
                res.cancel_action.CopyFrom(pb.CancelActionResult(acknowledged=True))
            self.session.control_in.put(pb.AdapterMessage(command_result=res))


def _drive(drv, adapter, method):
    real = drv._await_result

    def _await(command_id):
        adapter.pump()
        return real(command_id)

    drv._await_result = _await
    getattr(drv, method)()


# ── C19.1 / C19.2 ────────────────────────────────────────────────────────────

def test_c19_green_for_an_adapter_that_enforces_its_catalog(driver, pb):
    drv, session, report = driver
    adapter = _Adapter(pb, session, "enforce")
    _drive(drv, adapter, "_check_catalog_enforced")
    assert _row(report, "C19.1").status is CheckStatus.PASS, _row(report, "C19.1").detail
    assert _row(report, "C19.2").status is CheckStatus.PASS, _row(report, "C19.2").detail
    types = [t for t, _ in adapter.seen]
    assert "Clean" in types and "Navigate" in types
    assert {"y"} == set(adapter.seen[2][1]), "C19.2's probe omits exactly the dropped parameter"


def test_c19_red_for_an_adapter_that_runs_whatever_it_is_sent(driver, pb):
    """An adapter that runs every assignment it is sent, whatever its type or target."""
    drv, session, report = driver
    _drive(drv, _Adapter(pb, session, "accept-all"), "_check_catalog_enforced")
    for cid in ("C19.1", "C19.2"):
        row = _row(report, cid)
        assert row.status is CheckStatus.FAIL, row.detail
        assert "ACCEPTED" in row.detail
        assert row.is_blocking_failure


def test_c19_red_for_an_adapter_that_refuses_everything(driver, pb):
    """The control: refusing every assignment must not satisfy a refusal row."""
    drv, session, report = driver
    _drive(drv, _Adapter(pb, session, "refuse-all"), "_check_catalog_enforced")
    for cid in ("C19.1", "C19.2"):
        row = _row(report, cid)
        assert row.status is CheckStatus.FAIL, row.detail
        assert "control" in row.detail


def test_c19_red_when_the_refusal_names_the_wrong_reason(driver, pb):
    drv, session, report = driver
    _drive(drv, _Adapter(pb, session, "wrong-code"), "_check_catalog_enforced")
    row = _row(report, "C19.1")
    assert row.status is CheckStatus.FAIL
    assert "not INVALID_ACTION" in row.detail


def test_c192_skips_when_no_declared_action_requires_a_parameter(driver, pb):
    drv, session, report = driver
    bare = pb.SupportedAction(action_type="Navigate")
    drv._catalog, drv._served = [bare], bare
    _drive(drv, _Adapter(pb, session, "enforce", required=()), "_check_catalog_enforced")
    row = _row(report, "C19.2")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ADAPTER_DECLARES_NO_REQUIRED_PARAM.value)


def test_an_empty_catalog_skips_every_served_row_and_sends_nothing(driver, pb):
    drv, session, report = driver
    drv._catalog, drv._served = [], None
    drv._when_served(drv._check_catalog_enforced)
    drv._when_served(drv._check_fencing)
    for cid in ("C19.1", "C19.2", "C3.3", "C3.5", "C3.2", "C3.4"):
        row = _row(report, cid)
        assert row.status is CheckStatus.SKIP
        assert row.detail.startswith(SkipCause.ADAPTER_DECLARES_NO_ACTION.value)
    assert session.control_out.empty(), "nothing is sent to an adapter that serves nothing"


def test_c01_contradicts_a_no_action_skip_the_catalog_disproves(driver, pb):
    drv, _session, report = driver
    report.add("C3.5", "Echo the fenced token in AssignActionResult", CheckStatus.SKIP,
               detail=SkipCause.ADAPTER_DECLARES_NO_ACTION.value)
    drv._record_unverified()
    row = _row(report, "C0.1")
    assert row.status is CheckStatus.FAIL
    assert "declares ['Navigate']" in row.detail


# ── every acceptance probe says what it asks for ─────────────────────────────

def test_the_fencing_probes_carry_a_declared_type_and_a_target(driver, pb):
    drv, session, report = driver
    adapter = _Adapter(pb, session, "enforce")
    real = drv._await_result
    drv._await_result = lambda cid: (adapter.pump(), real(cid))[1]
    drv._check_fencing()
    assert adapter.seen, "the fencing checks sent no assignment"
    for action_type, payload in adapter.seen:
        assert action_type == "Navigate"
        assert {"x", "y"} <= set(payload)
    assert _row(report, "C3.5").status is CheckStatus.PASS


def test_c174_probe_carries_a_declared_type_not_custom(driver, pb):
    """A catalog-enforcing adapter that does not list `Custom` must not fail C17.4."""
    drv, session, report = driver
    adapter = _Adapter(pb, session, "enforce")
    real = drv._await_result
    drv._await_result = lambda cid: (adapter.pump(), real(cid))[1]
    drv._check_constraints_arrive(["speed"], "the CapabilitiesSnapshot")
    assert _row(report, "C17.4").status is CheckStatus.PASS, _row(report, "C17.4").detail
    assert adapter.seen[0][0] == "Navigate"


# ── C12.2 ────────────────────────────────────────────────────────────────────

def _snapshot(pb, robot, types):
    return pb.CapabilitiesSnapshot(robot_id=robot, supported_actions=[
        pb.SupportedAction(action_type=t, params=[
            pb.ActionParam(name="x", unit="m", kind="number", required=True),
            pb.ActionParam(name="y", unit="m", kind="number", required=True)])
        for t in types])


def test_c122_green_when_the_push_matches_the_scan_reply(driver, pb):
    drv, session, report = driver
    drv._registered_robot = "r1"
    session.capabilities_pushed.append((0.0, _snapshot(pb, "r1", ["Navigate"])))
    drv._check_capabilities_push()
    assert _row(report, "C12.2").status is CheckStatus.PASS, _row(report, "C12.2").detail


def test_c122_red_when_nothing_is_pushed(driver, pb, harness_mod, monkeypatch):
    monkeypatch.setattr(harness_mod, "_PUSH_WAIT", 0.05)
    drv, _session, report = driver
    drv._registered_robot = "r1"
    drv._check_capabilities_push()
    row = _row(report, "C12.2")
    assert row.status is CheckStatus.FAIL and row.is_blocking_failure


def test_c122_red_when_the_push_declares_a_different_catalog(driver, pb):
    drv, session, report = driver
    drv._registered_robot = "r1"
    session.capabilities_pushed.append((0.0, _snapshot(pb, "r1", ["Navigate", "Clean"])))
    drv._check_capabilities_push()
    assert _row(report, "C12.2").status is CheckStatus.FAIL


def test_c122_skips_for_an_adapter_that_registered_nothing(driver, pb):
    drv, _session, report = driver
    drv._check_capabilities_push()
    row = _row(report, "C12.2")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)
