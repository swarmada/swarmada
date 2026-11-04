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


def test_c21_fails_discover_before_register(pb, driver, sent):
    """``DiscoverRobot`` MUST NOT be attempted before ``register``.

    The harness plays a control plane holding a Robot CRD for this id: it acks a
    RegisterRobot, advertises edge endpoints, and dispatches work to it for the
    rest of the run. A ``discover`` for that robot is the reverse ordering the
    catalog forbids, and the control plane answers it in band with
    ``ALREADY_EXISTS`` rather than pretending the robot was unknown.
    """
    drv, session, report = driver
    session.control_in.put(_adapter_msg(pb, discover=pb.DiscoverRobot(
        robot_id="robot-conformance-1", manufacturer="acme", model="test")))

    drv._maybe_handle_registration()

    row = _status(report, "C2.1")
    assert row.status is CheckStatus.FAIL, row.detail

    out = sent(session.control_out)
    assert [m.WhichOneof("payload") for m in out] == ["discover_ack"]
    ack = out[0].discover_ack
    assert ack.accepted is False
    assert ack.rejection == pb.REGISTRATION_REJECTION_ALREADY_EXISTS


def test_c21_passes_register_first(pb, driver, sent):
    """The conformant ordering still passes, and still advertises the edge node."""
    drv, session, report = driver
    session.control_in.put(_adapter_msg(pb, register=pb.RegisterRobot(
        robot_id="robot-conformance-1", adapter_version="1.0.0")))

    advertised = drv._maybe_handle_registration()

    assert advertised is True
    assert _status(report, "C2.1").status is CheckStatus.PASS
    out = sent(session.control_out)
    assert [m.WhichOneof("payload") for m in out] == ["register_ack"]
    assert len(out[0].register_ack.edge_endpoints) == 1


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
