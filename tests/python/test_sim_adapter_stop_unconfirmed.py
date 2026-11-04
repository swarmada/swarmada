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
"""The simulation adapter at contract 1.4.0 (with --latch), and its command-derived plant.

With ``--command-derived-plant`` the simulated robot has no motion channel independent of the
command path (ADR-0071), so a cancel can be commanded but its rest not witnessed. The adapter
then answers CANCEL_DISPOSITION_STOP_UNCONFIRMED, acknowledged, to a control plane that
negotiated contract 1.4.0 or later, and acknowledged=false to an older one, which has no value
for the answer. Without the flag it answers STOPPED_SAFELY, as before.

The adapter reports 1.4.0 only with --latch, and 1.3.0 without it. A 1.3.0 adapter's connection
negotiates 1.3.0, so it does not send the 1.4.0 value even where a control plane reports 1.4.0.
"""
import json

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation import sim_adapter  # noqa: E402
from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402


def _adapter(tmp_path, *, command_derived: bool = False, negotiated: str = "",
             latch: bool = True) -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim",
                   artifact_dir=str(tmp_path), command_derived_plant=command_derived,
                   latch=latch)
    a._sim.spawn("amr-1", 0.0, 0.0)
    a._negotiated_contract = negotiated
    return a


def _drain(a):
    out = []
    while not a._outbox.empty():
        out.append(a._outbox.get_nowait())
    return out


def _cancel(a):
    a._handle_command(pb.Command(command_id=1, robot_id="amr-1", assign_action=pb.AssignAction(
        action_id="ns/act-1", assignment_id="a1", action_type="Navigate", fencing_token=1,
        lease_duration_ms=600_000, lease_generation=1,
        payload_json=json.dumps({"x": 5.0, "y": 0.0}).encode())))
    _drain(a)
    a._handle_command(pb.Command(command_id=2, robot_id="amr-1", cancel_action=pb.CancelAction(
        action_id="ns/act-1", reason="zone maintenance", fencing_token=1)))
    return [m.command_result.cancel_action for m in _drain(a)
            if m.WhichOneof("payload") == "command_result"][-1]


def test_the_adapter_declares_contract_1_4_0_only_with_the_latch(tmp_path):
    assert sim_adapter.LATCH_CONTRACT_VERSION == "1.4.0"
    assert sim_adapter.CONTRACT_VERSION == "1.3.0"
    assert _adapter(tmp_path)._contract_version == "1.4.0"
    assert _adapter(tmp_path, latch=False)._contract_version == "1.3.0"


def test_a_1_3_0_adapter_sends_no_1_4_0_disposition_whatever_was_negotiated(tmp_path):
    """A control plane that predates per-connection negotiation reports 1.4.0 on every
    compatible connection. A 1.3.0 adapter's connection negotiates 1.3.0 (RFC-0001, package and
    versioning), so the adapter sends no 1.4.0 disposition whatever it was told."""
    res = _cancel(_adapter(tmp_path, command_derived=True, negotiated="1.4.0", latch=False))
    assert not res.acknowledged
    assert res.disposition == pb.CANCEL_DISPOSITION_UNSPECIFIED


def test_without_the_flag_a_cancel_is_a_safe_stop(tmp_path):
    res = _cancel(_adapter(tmp_path, negotiated="1.4.0"))
    assert res.acknowledged and res.disposition == pb.CANCEL_DISPOSITION_STOPPED_SAFELY


def test_a_command_derived_plant_answers_stop_unconfirmed_to_a_1_4_control_plane(tmp_path):
    res = _cancel(_adapter(tmp_path, command_derived=True, negotiated="1.4.0"))
    assert res.acknowledged
    assert res.disposition == pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED


@pytest.mark.parametrize("negotiated", ["1.3.0", ""])
def test_to_an_older_control_plane_it_does_not_acknowledge(tmp_path, negotiated):
    """An older control plane reads every value it does not know as a safe stop, so the
    adapter does not send one it cannot claim: it does not acknowledge, and the control plane
    holds the robot until its lease is provably dead."""
    res = _cancel(_adapter(tmp_path, command_derived=True, negotiated=negotiated))
    assert not res.acknowledged
    assert res.disposition == pb.CANCEL_DISPOSITION_UNSPECIFIED


def test_hello_ack_records_the_negotiated_contract(tmp_path):
    a = _adapter(tmp_path)
    a._telemetry_started = True  # no telemetry thread in a unit test
    a._handle_control_plane(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
        accepted=True, negotiated_contract_version="1.4.0")), None)
    assert a._negotiated_contract == "1.4.0"
