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

"""C7 — artefact verification: the checksum leg, and the signature leg.

``fleet-adapter-protocol.md`` "Firmware/model signature and checksum" has two
legs. The checksum leg was probed; the signature leg was not sent at all — the
probes set neither ``model_signature_ref`` nor ``firmware_signature_ref``, so
nothing in a passing report was evidence about signatures. These tests pin the
part that IS decidable on the wire: an adapter that ACCEPTS an artefact carrying
a signature reference must name the trust root it verified against
(``verified_signer``), which the proto marks "populated when accepted=true".
"""

from __future__ import annotations

from pathlib import Path

from adapters.conformance.report import CheckStatus

_HARNESS = Path(__file__).resolve().parents[1] / "harness.py"


def _status(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got "
                         f"{[r.check_id for r in report.results]}")


class _Scripted:
    """A scripted adapter: one CommandResult per Command the harness sends."""

    def __init__(self, pb, session, replies):
        self.pb = pb
        self.session = session
        self.replies = list(replies)
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
            self.session.control_in.put(self.pb.AdapterMessage(
                command_result=self.replies.pop(0)(cmd)))


def _reply(pb, *, model_ack=None, fw_accept=None, signer=""):
    def _mk(cmd):
        res = pb.CommandResult(command_id=cmd.command_id, robot_id=cmd.robot_id)
        if cmd.HasField("model_update"):
            res.model_update.acknowledged = bool(model_ack)
            res.model_update.verified_signer = signer
            res.model_update.message = "scripted"
        else:
            res.push_firmware.accepted = bool(fw_accept)
            res.push_firmware.verified_signer = signer
            res.push_firmware.message = "scripted"
        return res
    return _mk


def _drive(drv, session, pb, replies):
    scripted = _Scripted(pb, session, replies)
    real_await = drv._await_result

    def _await(command_id):
        scripted.pump()
        return real_await(command_id)

    drv._await_result = _await
    drv._check_artifact_verification()
    return scripted


def test_the_probes_carry_a_signature_reference(harness_mod, driver, pb):
    """The harness sent no signature reference at all.

    The two ``*_signature_ref`` fields are the only place the publisher-trust
    check appears on the wire. A probe that leaves them empty asks the adapter
    nothing about signatures, whatever the catalog says the check covers.
    """
    drv, session, report = driver
    scripted = _drive(drv, session, pb, [
        _reply(pb, model_ack=False), _reply(pb, fw_accept=False)])

    model = [c for c in scripted.seen if c.HasField("model_update")]
    firmware = [c for c in scripted.seen if c.HasField("push_firmware")]
    assert model and model[0].model_update.model_signature_ref
    assert firmware and firmware[0].push_firmware.firmware_signature_ref
    assert "signature_ref" in _HARNESS.read_text()


def test_c72_fails_an_accept_that_names_no_verified_signer(
        harness_mod, driver, pb):
    """An artefact accepted under a signature reference must name its signer.

    ``ModelUpdateResult.verified_signer`` is "populated when acknowledged=true".
    An empty one on an accepted artefact says the adapter installed something it
    did not verify — the supply-chain failure C7.2 exists to catch, and one that
    a checksum-only probe cannot see.
    """
    drv, session, report = driver
    _drive(drv, session, pb, [
        _reply(pb, model_ack=True, signer=""),
        _reply(pb, fw_accept=True, signer=""),
    ])

    for cid in ("C7.2", "C7.3"):
        row = _status(report, cid)
        assert row.status is CheckStatus.FAIL, row.detail
        assert "signature" in row.detail, row.detail


def test_c72_passes_an_adapter_that_fails_closed(harness_mod, driver, pb):
    """Refusing the unverifiable artefact is the conformant answer, as before."""
    drv, session, report = driver
    _drive(drv, session, pb, [
        _reply(pb, model_ack=False), _reply(pb, fw_accept=False)])

    assert _status(report, "C7.2").status is CheckStatus.PASS
    assert _status(report, "C7.3").status is CheckStatus.PASS
