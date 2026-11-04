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

"""C10 — update progress: the terminal outcome, for EVERY install answered.

The catalog says "the TERMINAL result of every install the adapter answers", and
``fleet-adapter-protocol.md`` says "for every install it accepts". The harness
sent ``push_firmware`` only, so an adapter that reports a terminal outcome for
firmware and stays silent on ``model_update`` held the MUST by never being asked.
"""

from __future__ import annotations

from adapters.conformance.report import CheckStatus


def _status(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got "
                         f"{[r.check_id for r in report.results]}")


class _Installer:
    """An adapter that accepts installs, with per-arm control of the outcome."""

    def __init__(self, pb, session, *, firmware_terminal, model_terminal,
                 unsupported=(), phase="installing"):
        self.pb = pb
        self.session = session
        self.firmware_terminal = firmware_terminal
        self.model_terminal = model_terminal
        self.unsupported = set(unsupported)
        # The terminal report's `phase`. Empty means the adapter sends the terminal outcome
        # and NO advisory progress, which is the shape C10.1 permits.
        self.phase = phase
        self.arms_seen = []

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
            arm = cmd.WhichOneof("command")
            self.arms_seen.append(arm)
            res = self.pb.CommandResult(command_id=cmd.command_id,
                                        robot_id=cmd.robot_id)
            if arm in self.unsupported:
                res.unsupported = True
                self.session.control_in.put(
                    self.pb.AdapterMessage(command_result=res))
                continue
            terminal = (self.firmware_terminal if arm == "push_firmware"
                        else self.model_terminal)
            if terminal:
                self.session.control_in.put(self.pb.AdapterMessage(
                    update_progress=self.pb.UpdateProgress(
                        robot_id=cmd.robot_id, phase=self.phase,
                        outcome=self.pb.INSTALL_OUTCOME_SUCCEEDED,
                        resulting_version="9.9.9")))
            if arm == "push_firmware":
                res.push_firmware.accepted = True
            else:
                res.model_update.acknowledged = True
            self.session.control_in.put(
                self.pb.AdapterMessage(command_result=res))


def _drive(drv, session, installer):
    real = drv._await_result_collecting

    def _collect(command_id, arm):
        installer.pump()
        return real(command_id, arm)

    drv._await_result_collecting = _collect
    drv._check_update_progress()


def test_c102_fails_model_update_without_terminal_outcome(
        harness_mod, driver, pb):
    """Firmware reports terminally; the model install does not.

    Under the old harness this adapter recorded ``[PASS] C10.2``: the check
    asked about ``push_firmware`` and nothing else, so half the obligation it
    published was held by never being exercised.
    """
    drv, session, report = driver
    installer = _Installer(pb, session, firmware_terminal=True,
                           model_terminal=False)
    _drive(drv, session, installer)

    assert "model_update" in installer.arms_seen, (
        "C10.2 never sent a model_update at all")
    row = _status(report, "C10.2")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "model_update" in row.detail


def test_c102_passes_when_both_installs_report_terminally(
        harness_mod, driver, pb):
    """Both arms answered and both reported terminally is the conformant shape."""
    drv, session, report = driver
    _drive(drv, session, _Installer(pb, session, firmware_terminal=True,
                                    model_terminal=True))
    assert _status(report, "C10.2").status is CheckStatus.PASS


def test_c102_skips_when_both_installs_are_declined(harness_mod, driver, pb):
    """Declining both is conformant (C7.1) and carries no terminal obligation."""
    drv, session, report = driver
    _drive(drv, session, _Installer(
        pb, session, firmware_terminal=False, model_terminal=False,
        unsupported=("push_firmware", "model_update")))

    row = _status(report, "C10.2")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith("optional-declined")


def test_c101_unreachable_path_is_not_recorded_as_a_decline(
        harness_mod, driver, pb):
    """``optional-declined`` must mean a decline and nothing else.

    A path made unreachable by an earlier correct refusal is not a declined
    feature. Recording it as one is what kept the cause out of
    ``NOT_APPLICABLE_CAUSES``: promoting it would have turned a feature the
    adapter never demonstrated into a ``pass``.
    """
    drv, session, report = driver

    class _Refuser(_Installer):
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
                arm = cmd.WhichOneof("command")
                self.arms_seen.append(arm)
                res = self.pb.CommandResult(command_id=cmd.command_id,
                                            robot_id=cmd.robot_id)
                # Answered, not declined — and refused on verification (C7.3).
                if arm == "push_firmware":
                    res.push_firmware.accepted = False
                    res.push_firmware.message = "checksum mismatch"
                else:
                    res.model_update.acknowledged = False
                    res.model_update.message = "checksum mismatch"
                self.session.control_in.put(
                    self.pb.AdapterMessage(update_progress=self.pb.UpdateProgress(
                        robot_id=cmd.robot_id,
                        outcome=self.pb.INSTALL_OUTCOME_FAILED,
                        failure_reason="checksum mismatch")))
                self.session.control_in.put(
                    self.pb.AdapterMessage(command_result=res))

    _drive(drv, session, _Refuser(pb, session, firmware_terminal=False,
                                  model_terminal=False))

    row = _status(report, "C10.1")
    assert row.status is CheckStatus.SKIP
    assert not row.detail.startswith("optional-declined"), (
        "an unreachable path was recorded as a declined optional feature")
    assert row.detail.startswith("adapter-did-not-reach-state")


# adapters/CONFORMANCE.md, C10.1 (MAY): "While applying a `PushFirmware` / `ModelUpdate` it
# accepted, emit advisory `UpdateProgress` messages (`robot_id`, `kind`, `phase`, optional
# `percent`) before the final result [...] Progress is advisory and emitting none is
# conformant."
#
# C10.2 (MUST, when implemented) is the row that DOES require a message: "Report the TERMINAL
# result of every install the adapter answers, as an `UpdateProgress` carrying `outcome`".
# So the adapter below sends exactly that and nothing more: one terminal UpdateProgress per
# install, with no `phase`.
def test_c101_accepting_an_install_without_progress_is_conformant(
        harness_mod, driver, pb):
    """Accept both installs, report each terminally, send no advisory progress.

    This is the shape the contract permits. The harness recorded C10.1 as
    ``optional-declined`` although nothing was declined, so C0.1 (a MUST) contradicted
    the cause and the verdict was NON-CONFORMANT.
    """
    drv, session, report = driver
    installer = _Installer(pb, session, firmware_terminal=True, model_terminal=True,
                           phase="")
    _drive(drv, session, installer)
    drv._record_unverified()

    c101 = _status(report, "C10.1")
    assert c101.status is CheckStatus.SKIP, c101.detail
    assert c101.detail.startswith("optional-not-exhibited"), c101.detail
    # Not a PASS: the profile layer reads a C10.1 pass as the update-progress feature
    # SUPPORTED, and this adapter does not send progress.
    assert _status(report, "C10.2").status is CheckStatus.PASS
    c01 = _status(report, "C0.1")
    assert c01.status is CheckStatus.PASS, c01.detail
    assert report.conformant, [r.check_id for r in report.blocking_failures()]


def test_c101_no_progress_still_owes_the_terminal_outcome(harness_mod, driver, pb):
    """The fix must not excuse C10.2. Silence on BOTH counts is still a failure."""
    drv, session, report = driver
    _drive(drv, session, _Installer(pb, session, firmware_terminal=False,
                                    model_terminal=False))
    assert _status(report, "C10.1").detail.startswith("optional-not-exhibited")
    assert _status(report, "C10.2").status is CheckStatus.FAIL
    assert not report.conformant
