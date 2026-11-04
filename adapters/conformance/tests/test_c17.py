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

"""C17 — zone operating constraints: the checks must be able to go RED.

Every test here drives the real ``_check_zone_restrictions`` against a scripted
adapter and asserts the recorded verdict. Half of them script an adapter that
gets it WRONG: a check that has only ever been run against something that passes
is a check nobody has tested, and the accept-and-ignore case in particular is the
one the whole obligation exists to catch.

The FA-R5/FA-R6 obligation is only testable because ``AssignActionResult``
carries ``unhonourable_subjects``. A decline that does not name the subject is
indistinguishable, on the wire, from a decline for any other reason — so the
tests below assert on the NAMES and not merely on the refusal.
"""

from __future__ import annotations

from adapters.conformance.report import CheckStatus, SkipCause


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got "
                         f"{[r.check_id for r in report.results]}")


class _Scripted:
    """A scripted adapter answering every assign_action with one canned reply."""

    def __init__(self, pb, session, reply):
        self.pb = pb
        self.session = session
        self.reply = reply
        self.seen = []

    def pump(self):
        import queue as _q

        while True:
            try:
                msg = self.session.control_out.get_nowait()
            except _q.Empty:
                return
            if msg.WhichOneof("payload") != "command":
                continue
            cmd = msg.command
            self.seen.append(cmd)
            if not cmd.HasField("assign_action"):
                continue  # a cancel_action cleanup draws no reply
            res = self.pb.CommandResult(command_id=cmd.command_id,
                                        robot_id=cmd.robot_id)
            res.assign_action.CopyFrom(self.reply(cmd.assign_action))
            self.session.control_in.put(
                self.pb.AdapterMessage(command_result=res))


def _drive(drv, session, pb, reply, declared):
    """Run the C17 family against a scripted adapter that declared `declared`."""
    drv._honoured_subjects = declared
    scripted = _Scripted(pb, session, reply)
    real_await = drv._await_result

    def _await(command_id):
        scripted.pump()
        return real_await(command_id)

    drv._await_result = _await
    drv._check_zone_restrictions()
    return scripted


def _declines(pb):
    """A conformant adapter: declines naming every subject it was sent."""
    def _mk(assign):
        return pb.AssignActionResult(
            accepted=False,
            rejection=pb.ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET,
            unhonourable_subjects=[c.subject for c in assign.operating_constraints])
    return _mk


def _accepts(pb):
    """The failure mode the obligation exists to catch: accept, and ignore."""
    def _mk(assign):
        return pb.AssignActionResult(accepted=True,
                                     accepted_fencing_token=assign.fencing_token)
    return _mk


# ── FA-R5 / C17.1 ─────────────────────────────────────────────────────────────

def test_c171_green_when_the_adapter_declines_naming_every_subject(driver, pb):
    drv, session, report = driver
    scripted = _drive(drv, session, pb, _declines(pb), [])

    assert _row(report, "C17.1").status is CheckStatus.PASS
    # The probe really carried all seven, in all three modes -- a fixture that only
    # sent DENIED would pass an adapter that looks at nothing else.
    core = [c for c in scripted.seen if c.HasField("assign_action")
            and c.assign_action.action_id == "t-operating-core"]
    assert len(core) == 1
    sent = core[0].assign_action.operating_constraints
    assert [c.subject for c in sent] == [
        "camera", "microphone", "water", "chemical", "uv", "speed", "noise"]
    assert {c.mode for c in sent} == {
        pb.OPERATING_CONSTRAINT_MODE_DENIED,
        pb.OPERATING_CONSTRAINT_MODE_LIMITED,
        pb.OPERATING_CONSTRAINT_MODE_REQUIRED}
    # RESOLVED, NEVER RAW: every rule names the zone that DECLARED it, and it is not
    # the target. A refusal has to send an operator to the object they must edit.
    assert all(c.declaring_zone and c.reason for c in sent)


def test_c171_red_when_the_adapter_accepts_and_ignores(driver, pb):
    """THE red run. An adapter that accepts a rule it cannot honour has left the
    control plane authorising an action it cannot police."""
    drv, session, report = driver
    _drive(drv, session, pb, _accepts(pb), [])

    row = _row(report, "C17.1")
    assert row.status is CheckStatus.FAIL
    assert row.level == "MUST"
    assert "ACCEPTED" in row.detail


def test_c171_red_when_the_decline_does_not_name_the_subject(driver, pb):
    """A decline that names nothing is not testable and does not satisfy FA-R5."""
    drv, session, report = driver

    def _silent(assign):
        return pb.AssignActionResult(
            accepted=False,
            rejection=pb.ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET)

    _drive(drv, session, pb, _silent, [])
    row = _row(report, "C17.1")
    assert row.status is CheckStatus.FAIL
    assert "does not name" in row.detail


def test_c171_red_when_it_names_only_the_first_subject(driver, pb):
    """The reason the seven travel in ONE message rather than seven.

    Seven separate probes are each satisfied by an adapter that names the one
    subject it was sent. One message carrying all seven is not.
    """
    drv, session, report = driver

    def _first_only(assign):
        subs = [c.subject for c in assign.operating_constraints]
        return pb.AssignActionResult(
            accepted=False,
            rejection=pb.ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET,
            unhonourable_subjects=subs[:1])

    _drive(drv, session, pb, _first_only, [])
    row = _row(report, "C17.1")
    assert row.status is CheckStatus.FAIL
    assert "microphone" in row.detail  # the ones it failed to name


def test_c171_red_when_declined_under_the_wrong_rejection_reason(driver, pb):
    """`INVALID_ACTION` tells the control plane something else entirely.

    Only OPERATING_CONSTRAINT_UNMET names the rule the robot cannot honour and
    carries the subject list; the other reasons are a different
    disposition and the operator is told the wrong thing.
    """
    drv, session, report = driver

    def _wrong_reason(assign):
        return pb.AssignActionResult(
            accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION,
            unhonourable_subjects=[c.subject for c in assign.operating_constraints])

    _drive(drv, session, pb, _wrong_reason, [])
    row = _row(report, "C17.1")
    assert row.status is CheckStatus.FAIL
    assert "rather than" in row.detail


def test_c171_probes_only_the_subjects_the_adapter_did_not_declare(driver, pb):
    """A subject the adapter DECLARED it honours is not one it cannot honour."""
    drv, session, report = driver
    scripted = _drive(drv, session, pb, _declines(pb), ["camera", "speed"])

    core = [c for c in scripted.seen if c.HasField("assign_action")
            and c.assign_action.action_id == "t-operating-core"][0]
    sent = [c.subject for c in core.assign_action.operating_constraints]
    assert sent == ["microphone", "water", "chemical", "uv", "noise"]
    assert _row(report, "C17.1").status is CheckStatus.PASS


def test_c171_skips_with_an_enumerated_cause_when_all_seven_are_declared(driver, pb):
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), [
        "camera", "microphone", "water", "chemical", "uv", "speed", "noise"])

    row = _row(report, "C17.1")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ALL_CORE_SUBJECTS_HONOURED.value)


# ── FA-R6 / C17.2 ─────────────────────────────────────────────────────────────

def test_c172_red_when_an_unknown_subject_is_silently_accepted(driver, pb):
    """The bypass. Treating an unrecognised subject as an absent one would let any
    vendor route around every operating rule by inventing a subject."""
    drv, session, report = driver
    _drive(drv, session, pb, _accepts(pb), [])

    row = _row(report, "C17.2")
    assert row.status is CheckStatus.FAIL
    assert row.level == "MUST"
    assert "example.com/leaf-blower" in row.detail


def test_c172_green_when_the_unknown_subject_is_declined_by_name(driver, pb):
    drv, session, report = driver
    scripted = _drive(drv, session, pb, _declines(pb), [])

    assert _row(report, "C17.2").status is CheckStatus.PASS
    ext = [c for c in scripted.seen if c.HasField("assign_action")
           and c.assign_action.action_id == "t-operating-extension"]
    assert len(ext) == 1, "the extension form is exercised EXACTLY once"
    assert [c.subject for c in ext[0].assign_action.operating_constraints] == [
        "example.com/leaf-blower"]


def test_the_extension_form_is_exercised_exactly_once_in_the_whole_family(driver, pb):
    """Conformance covers the core seven only; the extension form appears once."""
    drv, session, report = driver
    scripted = _drive(drv, session, pb, _declines(pb), [])

    extension_rules = [c.subject
                       for cmd in scripted.seen if cmd.HasField("assign_action")
                       for c in cmd.assign_action.operating_constraints
                       if "/" in c.subject]
    assert extension_rules == ["example.com/leaf-blower"]


# ── FA-R1 / C17.3 ─────────────────────────────────────────────────────────────

def test_c173_green_when_nothing_is_declared(driver, pb):
    """FA-R1: declaring none is not a breach."""
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), [])

    row = _row(report, "C17.3")
    assert row.status is CheckStatus.PASS
    assert "not a breach" in row.detail


def test_c173_green_on_a_core_subject_and_a_namespaced_extension(driver, pb):
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), ["camera", "example.com/leaf-blower"])
    assert _row(report, "C17.3").status is CheckStatus.PASS


def test_c173_red_on_a_bare_word_that_is_not_one_of_the_core_seven(driver, pb):
    """The core is CLOSED. A subject with no `/` that is not one of the seven is
    dropped by the control plane, so the robot loses it and nothing says why."""
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), ["camera", "mop"])

    row = _row(report, "C17.3")
    assert row.status is CheckStatus.FAIL
    assert "'mop'" in row.detail


def test_c173_red_on_an_extension_whose_prefix_is_not_a_domain(driver, pb):
    """A bare label is not a namespace: the whole point of the prefix is that it
    is a DNS domain attributable to somebody."""
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), ["vendor/leaf-blower"])

    row = _row(report, "C17.3")
    assert row.status is CheckStatus.FAIL
    assert "dns-domain" in row.detail


def test_c173_skips_when_the_adapter_answered_no_scan(driver, pb):
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), None)

    row = _row(report, "C17.3")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)


# ── The report publishes the declaration, so a profile verdict is not prose ────

def test_the_declaration_is_published_on_the_report(driver, pb):
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), ["camera"])
    assert report.honoured_operating_constraints == ["camera"]
    assert "honoured_operating_constraints" in report.to_json()


def test_declared_none_and_never_asked_are_different_on_the_report(driver, pb):
    """`[]` is the adapter saying "none"; `None` is the run never having asked.
    They select different profile verdicts, so they must not collapse."""
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), [])
    assert report.honoured_operating_constraints == []

    drv2, session2, report2 = driver
    report2.results.clear()
    drv2._honoured_subjects = None
    _drive(drv2, session2, pb, _declines(pb), None)
    assert report2.honoured_operating_constraints is None


# ── The fixture must not disturb C3.8 ─────────────────────────────────────────

def test_the_probes_stay_below_the_c38_adoption_token(harness_mod):
    """C3.8 discriminates only while the pre-reconnect mark is strictly below
    _ADOPTION_PROBE_TOKEN. C17's three probes raise that mark -- C17.4's against
    every conformant adapter, the other two against one that wrongly accepts
    them -- so all three must stay under it, and the probe must stay strictly
    below _AUTHORITATIVE_TOKEN or C3.4 answers instead of C3.8."""
    probes = (harness_mod._ZONE_CORE_TOKEN,
              harness_mod._ZONE_EXTENSION_TOKEN,
              harness_mod._ZONE_HONOURED_TOKEN)
    assert len(set(probes)) == len(probes), "two C17 probes share a fencing token"
    for t in probes:
        assert t < harness_mod._ADOPTION_PROBE_TOKEN
    assert harness_mod._ADOPTION_PROBE_TOKEN < harness_mod._AUTHORITATIVE_TOKEN
    assert harness_mod._AUTHORITATIVE_TOKEN < harness_mod._CADENCE_TOKEN


# ── FA-R2 arrival / C17.4 ─────────────────────────────────────────────────────
#
# C17.1 and C17.2 require a DECLINE. An adapter that refuses every assignment carrying
# any operating rule — or every assignment at all — passes both without ever reading the
# message, and its green lines would mean "this adapter declines" rather than "this
# adapter was told". C17.4 is the row that separates the behaviour from the reflex, and it
# is the only one in the family that can establish FA-R2's carry ARRIVED.

def _honours(pb, honoured):
    """A conformant adapter: declines only the subjects it did not declare."""
    def _mk(assign):
        missing = [c.subject for c in assign.operating_constraints
                   if c.subject not in set(honoured)]
        if missing:
            return pb.AssignActionResult(
                accepted=False,
                rejection=pb.ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET,
                unhonourable_subjects=missing)
        return pb.AssignActionResult(accepted=True,
                                     accepted_fencing_token=assign.fencing_token)
    return _mk


def harness_fencing_token_for_honoured_probe():
    from adapters.conformance import harness as _h
    return _h._ZONE_HONOURED_TOKEN


def test_c174_green_when_a_declared_set_is_accepted(driver, pb):
    drv, session, report = driver
    scripted = _drive(drv, session, pb, _honours(pb, ["camera", "speed"]), ["camera", "speed"])

    assert _row(report, "C17.4").status is CheckStatus.PASS
    # The probe carried ONLY subjects the adapter declared — otherwise a decline would be
    # correct and the row would grade the wrong thing.
    probe = [c for c in scripted.seen if c.HasField("assign_action")
             and c.assign_action.action_id == "t-operating-honoured"]
    assert len(probe) == 1
    assert sorted(c.subject for c in probe[0].assign_action.operating_constraints) == \
        ["camera", "speed"]
    # The accepted task is cancelled: a conformant adapter is left running nothing the run
    # never intended to start. The cleanup cancel is the last thing the family sends, and
    # _drive's scripted adapter only pumps when the harness AWAITS a reply -- a cancel
    # draws none -- so it is drained explicitly here.
    scripted.pump()
    cancels = [c for c in scripted.seen if c.HasField("cancel_action")
               and c.cancel_action.action_id == "t-operating-honoured"]
    assert len(cancels) == 1
    assert cancels[0].cancel_action.fencing_token == \
        harness_fencing_token_for_honoured_probe(), (
            "the cleanup must cancel at the token the assignment was accepted at, or a "
            "conformant adapter refuses it as stale and the task keeps running")


def test_c174_red_when_the_adapter_declines_everything_it_is_sent(driver, pb):
    """THE red run for this row. An adapter that refuses reflexively passes C17.1 and
    C17.2 while establishing nothing at all about whether it read the constraint set."""
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), ["camera", "speed"])

    # The reflex passes both decline rows …
    assert _row(report, "C17.1").status is CheckStatus.PASS
    assert _row(report, "C17.2").status is CheckStatus.PASS
    # … and this is the row that catches it.
    row = _row(report, "C17.4")
    assert row.status is CheckStatus.FAIL
    assert row.level == "MUST"
    assert "DECLINED" in row.detail


def test_c174_skips_with_an_enumerated_cause_when_nothing_is_declared(driver, pb):
    """An adapter that declares nothing cannot be probed here: there is no set it claims
    to honour. Recorded as a named cause, never as a vacuous pass — and it is not a
    coverage hole, because Part E makes the whole profile inapplicable to such an
    adapter."""
    drv, session, report = driver
    _drive(drv, session, pb, _declines(pb), [])

    row = _row(report, "C17.4")
    assert row.status is CheckStatus.SKIP
    assert SkipCause.ADAPTER_DECLARES_NO_OPERATING_SUBJECT.value in row.detail


def test_c174_probes_only_subjects_the_adapter_declared(driver, pb):
    """Including an undeclared subject would make a DECLINE correct, and the row would
    then grade the opposite of what it is for."""
    drv, session, report = driver
    scripted = _drive(drv, session, pb, _honours(pb, ["uv"]), ["uv"])

    assert _row(report, "C17.4").status is CheckStatus.PASS
    probe = [c for c in scripted.seen if c.HasField("assign_action")
             and c.assign_action.action_id == "t-operating-honoured"][0]
    assert [c.subject for c in probe.assign_action.operating_constraints] == ["uv"]
