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

"""C2 — robot lifecycle: register-before-discover, redial, telemetry cadence.

The obligation these pin is stated twice in the specification and was checked in
neither place: ``fleet-adapter-protocol.md`` "Required messages" says
``RegisterRobot`` MUST be the first attempt on connect AND reconnect and
``DiscoverRobot`` MUST NOT be attempted before it, and ``safety.md`` repeats the
reconnect half as its own MUST NOT.
"""

from __future__ import annotations

import pytest

from adapters.conformance.report import CheckStatus


def _status(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got "
                         f"{[r.check_id for r in report.results]}")


def _adapter_msg(pb, **kwargs):
    return pb.AdapterMessage(**kwargs)


# ── C2.1 — register is the FIRST lifecycle message ───────────────────────────
#
# The harness plays first contact with a robot that has no Robot yet: the first register is
# refused NOT_ADMITTED, the adapter MUST announce the robot with a discover, and the register of
# its provisioning loop's next iteration is accepted as the admission.


@pytest.fixture
def quick_c21(harness_mod, monkeypatch):
    monkeypatch.setattr(harness_mod, "_DISCOVER_WAIT", 0.2, raising=False)
    monkeypatch.setattr(harness_mod, "_ADMISSION_WAIT", 0.2, raising=False)


def _register(pb):
    return _adapter_msg(pb, register=pb.RegisterRobot(robot_id="robot-conformance-1",
                                                      adapter_version="1.0.0"))


def _discover(pb):
    return _adapter_msg(pb, discover=pb.DiscoverRobot(robot_id="robot-conformance-1",
                                                      manufacturer="acme", model="test"))


def _kinds(out):
    return [m.WhichOneof("payload") for m in out]


def test_c21_passes_register_then_discover_on_not_admitted(pb, driver, sent, quick_c21):
    """Register first; refused NOT_ADMITTED, the adapter announces; its next register is
    accepted as the admission, and advertises the edge node."""
    drv, session, report = driver
    for m in (_register(pb), _discover(pb), _register(pb)):
        session.control_in.put(m)

    assert drv._maybe_handle_registration() is True

    row = _status(report, "C2.1")
    assert row.status is CheckStatus.PASS, row.detail
    out = sent(session.control_out)
    assert _kinds(out) == ["register_ack", "discover_ack", "register_ack"]
    refused, staged, admitted = out[0].register_ack, out[1].discover_ack, out[2].register_ack
    assert refused.accepted is False
    assert refused.rejection == pb.REGISTRATION_REJECTION_NOT_ADMITTED
    assert staged.accepted is True and staged.ttl_seconds > 0
    assert admitted.accepted is True and len(admitted.edge_endpoints) == 1


def test_c21_fails_when_not_admitted_draws_no_discover(pb, driver, sent, quick_c21):
    """A NOT_ADMITTED refusal answered by nothing: the adapter never announces the robot."""
    drv, session, report = driver
    session.control_in.put(_register(pb))

    assert drv._maybe_handle_registration() is False

    row = _status(report, "C2.1")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "no discover" in row.detail
    assert _kinds(sent(session.control_out)) == ["register_ack"]


def test_c21_fails_when_not_admitted_draws_a_register(pb, driver, sent, quick_c21):
    """A NOT_ADMITTED refusal answered by another register: no announce. The second register
    is accepted, so the run goes on with the robot admitted."""
    drv, session, report = driver
    session.control_in.put(_register(pb))
    session.control_in.put(_register(pb))

    assert drv._maybe_handle_registration() is True

    assert _status(report, "C2.1").status is CheckStatus.FAIL
    out = sent(session.control_out)
    assert _kinds(out) == ["register_ack", "register_ack"]
    assert out[0].register_ack.rejection == pb.REGISTRATION_REJECTION_NOT_ADMITTED
    assert out[1].register_ack.accepted is True


def test_c21_fails_discover_before_register(pb, driver, sent, quick_c21):
    """``DiscoverRobot`` MUST NOT be attempted before ``register``. The robot has no Robot, so
    the announce is staged; the next register is accepted."""
    drv, session, report = driver
    session.control_in.put(_discover(pb))
    session.control_in.put(_register(pb))

    assert drv._maybe_handle_registration() is True

    row = _status(report, "C2.1")
    assert row.status is CheckStatus.FAIL, row.detail
    out = sent(session.control_out)
    assert _kinds(out) == ["discover_ack", "register_ack"]
    assert out[0].discover_ack.accepted is True
    assert out[1].register_ack.accepted is True


def test_c21_passes_when_no_register_follows_the_announce(pb, driver, sent, quick_c21):
    """The order is right; with no later register the run goes on with the robot not admitted."""
    drv, session, report = driver
    session.control_in.put(_register(pb))
    session.control_in.put(_discover(pb))

    assert drv._maybe_handle_registration() is False

    row = _status(report, "C2.1")
    assert row.status is CheckStatus.PASS, row.detail
    assert "not admitted" in row.detail


def test_c21_returns_other_messages_to_the_stream(pb, driver, sent, quick_c21):
    """Telemetry read while C2.1 waits goes back on the stream, in order, for later checks."""
    drv, session, report = driver
    beat = _adapter_msg(pb, telemetry=pb.TelemetryPayload(robot_id="robot-conformance-1"))
    for m in (_register(pb), beat, _discover(pb), _register(pb)):
        session.control_in.put(m)

    assert drv._maybe_handle_registration() is True

    assert [m.WhichOneof("payload") for m in drv._pushback] == ["telemetry"]


def test_c21_skips_when_the_adapter_registers_nothing(pb, driver, harness_mod, monkeypatch):
    """A minimal template registers nothing: the row is a skip, not a failure."""
    monkeypatch.setattr(harness_mod, "_REGISTER_WAIT", 0.1)
    drv, _session, report = driver

    assert drv._maybe_handle_registration() is False

    assert _status(report, "C2.1").status is CheckStatus.SKIP


# ── C2.4 — the redial re-registers; it never re-discovers ────────────────────


def _redial(drv, session, pb, monkeypatch, second):
    """Play a redial whose handshake is ``hello`` then ``second``."""
    # The driver drops the stream and waits for control_opens to move past what
    # it saw before the drop. There is no server here to answer the redial, so
    # the drop itself stands in for one.
    real_drop = session.drop_control

    def _drop():
        real_drop()
        session.control_opens += 1

    session.drop_control = _drop
    session.control_in.put(pb.AdapterMessage(hello=pb.AdapterHello(
        protocol_version="fleet_adapter.v1", contract_version="1.0.0")))
    session.control_in.put(second)
    # C6.1's telemetry wait is not what these tests are about, and it is 11 s.
    monkeypatch.setattr(drv, "_await_telemetry", lambda timeout=0.0: None)
    monkeypatch.setattr(drv, "_await_action_status",
                        lambda *a, **k: None)
    drv._check_accepting_reconnect()


def test_reconnect_discover_is_not_accepted_as_registration(
        pb, driver, sent, monkeypatch):
    """safety.md: on reconnect the adapter MUST send ``register``.

    The harness used to accept ``discover`` on the redial as if it were a
    registration — it answered a ``RegisterAck`` to a message the specification
    says MUST NOT be sent, and every downstream reconnect check then passed on
    the strength of it.
    """
    drv, session, report = driver
    _redial(drv, session, pb, monkeypatch,
            _adapter_msg(pb, discover=pb.DiscoverRobot(
                robot_id="robot-conformance-1")))

    row = _status(report, "C2.4")
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.level == "MUST NOT"

    kinds = [m.WhichOneof("payload") for m in sent(session.control_out)]
    assert "register_ack" not in kinds, (
        "a discover on the redial was answered as a registration")
    assert "discover_ack" in kinds


def test_reconnect_register_passes(pb, driver, sent, monkeypatch):
    """The conformant redial re-registers, and is acked as before."""
    drv, session, report = driver
    _redial(drv, session, pb, monkeypatch,
            _adapter_msg(pb, register=pb.RegisterRobot(
                robot_id="robot-conformance-1", adapter_version="1.0.0")))

    assert _status(report, "C2.4").status is CheckStatus.PASS
    kinds = [m.WhichOneof("payload") for m in sent(session.control_out)]
    assert "register_ack" in kinds


def test_c24_skips_when_the_adapter_never_redials(pb, driver, monkeypatch):
    """No redial is not a violation: the obligation is unestablished, not broken."""
    drv, session, report = driver
    monkeypatch.setattr(drv, "_await_telemetry", lambda timeout=0.0: None)
    monkeypatch.setattr(drv.__class__, "_RECONNECT_WAIT_OVERRIDE", 0.0, raising=False)
    monkeypatch.setattr("adapters.conformance.harness._RECONNECT_WAIT", 0.0)
    drv._check_accepting_reconnect()

    row = _status(report, "C2.4")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith("adapter-did-not-reach-state")


# ── C2.3 — the cadence window, and the motion-gating SHOULD it collides with ─


def test_c23_passes_motion_gated_adapter_with_stationary_robot(pb, driver):
    """A stationary robot cannot distinguish the two explanations.

    ``fleet-adapter-protocol.md`` "Motion-gated position telemetry" is a SHOULD:
    an adapter skips a payload while the robot has moved less than
    ``motionThresholdMeters``, and must send one only every
    ``maxIdleIntervalSeconds`` (recommended class default 30 s). Measured against
    a robot the harness never moved, a 30 s gap is exactly what a conformant
    motion-gated adapter produces — so FAILing it reports a satisfied SHOULD as a
    violated MUST.
    """
    drv, _session, report = driver
    drv._record_cadence([30.0, 30.0], in_motion=False)

    row = _status(report, "C2.3")
    assert row.status is CheckStatus.SKIP, row.detail
    assert row.detail.startswith("adapter-did-not-reach-state")
    assert "motion" in row.detail


def test_c23_fails_a_slow_cadence_while_the_robot_is_moving(pb, driver):
    """With the robot driven, motion gating cannot explain the gaps: a real FAIL."""
    drv, _session, report = driver
    drv._record_cadence([30.0, 30.0], in_motion=True)
    assert _status(report, "C2.3").status is CheckStatus.FAIL


def test_c23_passes_an_adopted_interval_either_way(pb, driver):
    """An in-window cadence is conclusive whether or not the robot moved."""
    drv, _session, report = driver
    drv._record_cadence([1.0, 1.02], in_motion=False)
    assert _status(report, "C2.3").status is CheckStatus.PASS
