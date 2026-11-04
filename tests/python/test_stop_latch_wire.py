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

# The stop latch's SafetyStream messages (contract 1.4.0; RFC-0001
# #fleet-adapter-protocol-the-stop-latch) round-trip through the Python stubs the reference
# adapters import, with the field numbers the specification fixes. Estop and EstopAck stay
# version-frozen.

from __future__ import annotations

import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "proto"))

from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # noqa: E402


def _numbers(msg) -> dict[str, int]:
    return {f.name: f.number for f in msg.DESCRIPTOR.fields}


def test_field_numbers_are_the_specified_ones():
    assert _numbers(pb.EstopClear) == {
        "estop_id": 1,
        "cleared_by": 2,
        "cleared_at_ms": 3,
        "ends_local_set": 4,
        "leader_term": 5,
    }
    assert _numbers(pb.EstopClearAck) == {"estop_id": 1, "lifted_at_ms": 2}
    assert _numbers(pb.LocalStopReport) == {
        "latched": 1,
        "changed_at_ms": 2,
        "source": 3,
        "set_id": 4,
        "unreadable": 5,
    }
    cp = pb.ControlPlaneSafetyMessage.DESCRIPTOR.fields_by_name
    assert cp["estop_clear"].number == 5 and cp["estop_clear"].containing_oneof.name == "payload"
    ad = pb.AdapterSafetyMessage.DESCRIPTOR.fields_by_name
    assert (
        ad["estop_clear_ack"].number == 5
        and ad["estop_clear_ack"].containing_oneof.name == "payload"
    )
    assert ad["local_stop"].number == 6 and ad["local_stop"].containing_oneof.name == "payload"
    # version-frozen
    assert _numbers(pb.Estop) == {"estop_id": 1, "reason": 2, "issued_by": 3, "issued_at_ms": 4}


def test_estop_clear_round_trips():
    sent = pb.ControlPlaneSafetyMessage(
        seq=7,
        robot_id="amr-1",
        estop_clear=pb.EstopClear(
            estop_id="6f1c2b1e-9a43-4f3e-8d55-0f3f1c2d9e10",
            cleared_by="alice",
            cleared_at_ms=1760000000000,
            ends_local_set="",
            leader_term=3,
        ),
    )
    got = pb.ControlPlaneSafetyMessage.FromString(sent.SerializeToString())
    assert got.WhichOneof("payload") == "estop_clear"
    assert got.estop_clear == sent.estop_clear


def test_clear_ack_and_local_stop_round_trip():
    ack = pb.AdapterSafetyMessage(
        seq=1,
        robot_id="amr-1",
        estop_clear_ack=pb.EstopClearAck(
            estop_id="6f1c2b1e-9a43-4f3e-8d55-0f3f1c2d9e10", lifted_at_ms=1760000000500
        ),
    )
    got = pb.AdapterSafetyMessage.FromString(ack.SerializeToString())
    assert (
        got.WhichOneof("payload") == "estop_clear_ack"
        and got.estop_clear_ack == ack.estop_clear_ack
    )

    rep = pb.AdapterSafetyMessage(
        seq=2,
        robot_id="amr-1",
        local_stop=pb.LocalStopReport(
            latched=False, changed_at_ms=0, source="unreadable", set_id="", unreadable=True
        ),
    )
    got = pb.AdapterSafetyMessage.FromString(rep.SerializeToString())
    assert got.WhichOneof("payload") == "local_stop"
    assert got.local_stop.unreadable is True and got.local_stop.latched is False
    # unreadable defaults to false: a report that does not set it is a reading of the latch
    assert pb.LocalStopReport(latched=True).unreadable is False
