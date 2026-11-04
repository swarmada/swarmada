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

"""An assignment for a ``Custom`` entry keyed by ``custom_type`` carries that ``custom_type``.

Contract 1.4.0 keys a ``Custom`` catalog entry by ``SupportedAction.custom_type``, and the
control plane carries the verb on ``AssignAction.custom_type``, from
``FleetAction.spec.customType``. With it set, a payload need not repeat the verb. The harness
never set it: every served-action assignment, and every C19 probe and control, named the verb
nowhere, so an adapter that keys its verbs and does not require ``operation`` refused them all
as naming no verb. C4.2, C3.9 and C16.1 then skipped, and C3.4, C3.5, C5.8, C17.1, C17.2 and
C19.1-C19.3 failed on the refusal rather than on what they grade.

The probes that name no declared entry -- C19.1's undeclared type and C19.3's missing one --
carry no ``custom_type``, and C19.5's carries none on purpose: it is the row for an action that
names its verb only in the payload.
"""

from __future__ import annotations

import json
import queue
import shlex
import socket
import subprocess
import sys
from pathlib import Path

import pytest

from adapters.conformance.report import CheckStatus

ROOT = Path(__file__).resolve().parents[3]
FAKE = ROOT / "adapters" / "conformance" / "tests" / "latching_fake.py"
_VERB = "conformance-patrol"
_NO_VERB = "the assignment names no Custom verb"


def _keyed(pb, verb=_VERB):
    entry = pb.SupportedAction(action_type="Custom", custom_type=verb)
    entry.params.extend([pb.ActionParam(name=n, unit="m", kind="number", required=True)
                         for n in ("x", "y")])
    entry.params.append(pb.ActionParam(name="operation", kind="enum", required=False,
                                       allowed=[verb]))
    return entry


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got {[r.check_id for r in report.results]}")


class _KeyedAdapter:
    """Answers commands as an adapter that keys its one ``Custom`` verb by ``custom_type``.

    The verb is ``custom_type``, or the payload's ``operation`` when that is not set; an
    assignment that names none, names another, or omits ``x``/``y`` is declined
    ``INVALID_ACTION``. Every assignment it was sent is kept in ``assigns``.
    """

    def __init__(self, pb, session):
        self.pb, self.session, self.assigns = pb, session, []

    def _judge(self, task) -> str:
        if task.action_type != "Custom":
            return "unsupported action type"
        doc = json.loads(task.payload_json or b"{}")
        op = doc.get("operation")
        if task.custom_type and op is not None and op != task.custom_type:
            return "custom_type and operation disagree"
        verb = task.custom_type or op
        if not verb:
            return _NO_VERB
        if verb != _VERB:
            return f"Custom verb {verb!r} is not served"
        if not all(isinstance(doc.get(n), (int, float)) for n in ("x", "y")):
            return "x and y are required"
        return ""

    def pump(self):
        pb = self.pb
        while True:
            try:
                msg = self.session.control_out.get_nowait()
            except queue.Empty:
                return
            if not hasattr(msg, "WhichOneof") or msg.WhichOneof("payload") != "command":
                continue
            cmd = msg.command
            res = pb.CommandResult(command_id=cmd.command_id, robot_id=cmd.robot_id)
            if cmd.WhichOneof("command") == "assign_action":
                task = cmd.assign_action
                self.assigns.append(task)
                why = self._judge(task)
                res.assign_action.CopyFrom(pb.AssignActionResult(
                    accepted=not why, accepted_fencing_token=0 if why else task.fencing_token,
                    rejection=pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION if why else 0,
                    message=why))
            elif cmd.WhichOneof("command") == "cancel_action":
                res.cancel_action.CopyFrom(pb.CancelActionResult(acknowledged=True))
            self.session.control_in.put(pb.AdapterMessage(command_result=res))


@pytest.fixture
def keyed(driver, pb, harness_mod, monkeypatch):
    """The driver over a catalog of one keyed ``Custom`` entry, answered by ``_KeyedAdapter``."""
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.2)
    drv, session, report = driver
    entry = _keyed(pb)
    drv._catalog = [entry]
    drv._served = entry
    adapter = _KeyedAdapter(pb, session)
    real = drv._await_result

    def _await(command_id):
        adapter.pump()
        return real(command_id)

    drv._await_result = _await
    return drv, report, adapter


def test_a_served_assignment_carries_the_keyed_entrys_custom_type(pb, driver):
    drv, _session, _report = driver
    drv._served = _keyed(pb)
    task = drv._assign(action_id="t-1", assignment_id="a-1", fencing_token=1)
    assert task.action_type == "Custom"
    assert task.custom_type == _VERB, "a keyed Custom assignment must name its verb"


def test_a_navigate_assignment_carries_no_custom_type(pb, driver):
    """``custom_type`` is set only for a Custom action that names one."""
    drv, _session, _report = driver
    task = drv._assign(action_id="t-1", assignment_id="a-1", fencing_token=1)
    assert task.action_type == "Navigate" and task.custom_type == ""


def test_custom_type_is_set_only_for_a_custom_action(pb, driver):
    """RFC-0001: set "only for a Custom action that names one". A catalog entry of another type
    that carries a custom_type anyway does not put one on the wire."""
    drv, _session, _report = driver
    drv._served = pb.SupportedAction(action_type="Navigate", custom_type=_VERB, params=[
        pb.ActionParam(name=n, unit="m", kind="number", required=True) for n in ("x", "y")])
    task = drv._assign(action_id="t-1", assignment_id="a-1", fencing_token=1)
    assert task.custom_type == ""


def test_an_unkeyed_custom_entry_gets_no_custom_type(pb, driver):
    drv, _session, _report = driver
    drv._served = pb.SupportedAction(action_type="Custom", params=[
        pb.ActionParam(name="operation", kind="enum", allowed=[_VERB])])
    task = drv._assign(action_id="t-1", assignment_id="a-1", fencing_token=1)
    assert task.custom_type == ""


def test_c19_against_a_keyed_adapter_grades_what_each_row_probes(keyed):
    drv, report, _adapter = keyed
    drv._check_catalog_enforced()
    for cid in ("C19.1", "C19.2", "C19.3", "C19.4", "C19.5"):
        row = _row(report, cid)
        assert row.status is CheckStatus.PASS, (cid, row.detail)


def test_c19_probes_carry_custom_type_only_where_they_name_a_declared_entry(keyed):
    drv, _report, adapter = keyed
    drv._check_catalog_enforced()
    sent = {t.action_id: t.custom_type for t in adapter.assigns}
    assert sent == {
        "t-undeclared-type": "",        # C19.1's probe: an undeclared type
        "t-declared-type": _VERB,       # C19.1's control
        "t-missing-param": _VERB,       # C19.2: refused for the missing x, not for no verb
        "t-complete-param": _VERB,
        "t-untyped": "",                # C19.3's probe: no type, so no entry
        "t-typed": _VERB,
        "t-unallowed-value": _VERB,     # C19.4
        "t-allowed-value": _VERB,
        "t-custom-no-type": "",         # C19.5: the verb in the payload only, by design
    }, sent


def test_c19_details_name_the_custom_type_sent(keyed):
    drv, report, _adapter = keyed
    drv._check_catalog_enforced()
    assert f"custom_type={_VERB!r}" in _row(report, "C19.1").detail
    assert f"custom_type={_VERB!r}" in _row(report, "C19.2").detail


def test_a_refusal_for_no_verb_is_still_not_a_pass(pb, driver, harness_mod, monkeypatch):
    """The control: a C19 probe declined for naming no verb, beside a control declined for
    the same reason, fails the row. Only the custom_type on the control makes it pass."""
    monkeypatch.setattr(harness_mod, "_custom_type_of", lambda action: "", raising=False)
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.2)
    drv, session, report = driver
    drv._catalog = [_keyed(pb)]
    drv._served = drv._catalog[0]
    adapter = _KeyedAdapter(pb, session)
    real = drv._await_result

    def _await(command_id):
        adapter.pump()
        return real(command_id)

    drv._await_result = _await
    drv._check_catalog_enforced()
    for cid in ("C19.1", "C19.2", "C19.3"):
        assert _row(report, cid).status is CheckStatus.FAIL, (cid, _row(report, cid).detail)


# -- the whole harness, against an adapter that keys its Custom verb -------------------------


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="module")
def keyed_run(tmp_path_factory):
    """One harness run against ``latching_fake.py --plant keyed-custom``: rows by check id."""
    tmp = tmp_path_factory.mktemp("keyed-custom")
    report = tmp / "report.json"
    witness = tmp / "witness"
    adapter_cmd = " ".join(shlex.quote(a) for a in (
        sys.executable, str(FAKE), "--endpoint", "localhost:{port}",
        "--witness-file", str(witness), "--plant", "keyed-custom"))
    run = subprocess.run(
        [sys.executable, "-m", "adapters.conformance", "--stub-path", "proto",
         "--adapter-name", "latching-fake-keyed-custom", "--port", str(_free_port()),
         "--witness-file", str(witness), "--json", str(report), "--adapter-cmd", adapter_cmd],
        cwd=ROOT, capture_output=True, text=True, timeout=300)
    assert report.exists(), (
        f"the harness wrote no report:\n{run.stdout[-3000:]}\n{run.stderr[-3000:]}")
    return {r["check_id"]: r for r in json.loads(report.read_text())["results"]}


def test_the_keyed_fake_declares_only_its_keyed_custom_entry(keyed_run):
    """The run is against the adapter this file is about, or it proves nothing: C19.5 names
    the keyed verb, and nothing but a Custom entry was served."""
    c195 = keyed_run["C19.5"]
    assert c195["status"] == "pass", c195.get("detail")
    assert f"operation={_VERB!r}" in c195["detail"], c195["detail"]
    assert "['Custom']" in keyed_run["C19.1"]["detail"], keyed_run["C19.1"]["detail"]


@pytest.mark.parametrize("cid", ["C4.2", "C3.9", "C16.1"])
def test_the_rows_that_need_an_accepted_custom_assignment_are_reached(keyed_run, cid):
    row = keyed_run[cid]
    assert row["status"] != "skip", (cid, row.get("detail"))
    assert row["status"] == "pass", (cid, row.get("detail"))


@pytest.mark.parametrize("cid", ["C3.4", "C3.5", "C5.8", "C17.1", "C17.2", "C19.1", "C19.2",
                                 "C19.3"])
def test_the_rows_that_failed_on_the_refusal_grade_their_own_subject(keyed_run, cid):
    row = keyed_run[cid]
    assert _NO_VERB not in (row.get("detail") or ""), (cid, row.get("detail"))
    assert row["status"] == "pass", (cid, row.get("detail"))


def test_no_row_is_refused_for_naming_no_verb(keyed_run):
    hit = sorted(cid for cid, r in keyed_run.items() if _NO_VERB in (r.get("detail") or ""))
    assert not hit, {cid: keyed_run[cid].get("detail") for cid in hit}
