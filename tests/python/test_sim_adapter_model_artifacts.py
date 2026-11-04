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

"""The simulation adapter's real-artifact mode (--model-trust-root), on the wire.

The runtime is replaced by a fake with the real module's exception types, so these tests need
none of the optional extra and run under `make test`. What they pin is the adapter's side of the
contract: verification before the acknowledgement, a refusal that is terminal, a self-check
failure that is reported after the acknowledgement and leaves the previous model running, a
switch only on success, and a rollback only to a version this robot actually holds.
"""

from __future__ import annotations

import time
import types

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation import model_runtime as mr  # noqa: E402
from adapters.simulation import sim_adapter  # noqa: E402
from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

GOOD, BROKEN = "3.0.0", "4.0.0"


class FakeModel:
    def __init__(self, name, version):
        self.name, self.version = name, version

    def run_frames(self, frames, truth=None):
        return {
            "model": self.name,
            "version": self.version,
            "frames": len(frames),
            "predictions": [["clear", 0.9]] * len(frames),
        }


class FakeRuntime:
    """model_runtime's surface, with scripted outcomes."""

    ArtifactRefusedError = mr.ArtifactRefusedError

    def __init__(self, refuse=None):
        self.refuse = refuse
        self.fetched = []

    def fetch_and_verify(self, uri, checksum, sig_ref, key, signer):
        self.fetched.append(uri)
        if self.refuse is not None:
            raise self.refuse
        return types.SimpleNamespace(manifest_digest=checksum, signer=signer, version=uri[-5:])

    def load(self, art, name, version):
        if version == BROKEN:
            raise mr.SelfCheckError("non-finite output on 8/8 probe frames")
        return FakeModel(name, version)


class FakeArtifacts:
    trust_root = object()
    trust_root_name = "demo-root"
    frames = ([0, 0, 0], None)
    results_dir = ""

    def __init__(self, runtime):
        self._rt = runtime

    def runtime(self):
        if isinstance(self._rt, Exception):
            raise self._rt
        return self._rt


def _adapter(runtime) -> SimAdapter:
    a = SimAdapter(
        pb,
        None,
        make_simulator("kinematic"),
        "amr-1",
        "z1",
        "sim",
        model_artifacts=FakeArtifacts(runtime),
    )
    return a


def _update(version, rollback=False):
    if rollback:
        return pb.ModelUpdate(model_name="floor-hazard", new_version=version, rollback=True)
    return pb.ModelUpdate(
        model_name="floor-hazard",
        new_version=version,
        model_uri=f"oci://localhost:5001/swarmada/floor-hazard:{version}",
        model_checksum="sha256:" + "a" * 64,
        model_signature_ref="oci://localhost:5001/swarmada/floor-hazard:s.sig",
    )


def _run(a, upd, cid=1, settle=True):
    a._handle_command(pb.Command(command_id=cid, robot_id="amr-1", model_update=upd))
    if settle:  # the install runs on a thread started after the ack is queued
        deadline = time.time() + 5
        while time.time() < deadline and not _terminal(list(a._outbox.queue)):
            time.sleep(0.01)
        time.sleep(0.05)
    out = []
    while not a._outbox.empty():
        out.append(a._outbox.get_nowait())
    return out


def _terminal(msgs):
    return [
        m.update_progress
        for m in msgs
        if m.WhichOneof("payload") == "update_progress"
        and m.update_progress.outcome != pb.INSTALL_OUTCOME_UNSPECIFIED
    ]


def _result(msgs):
    return [m.command_result for m in msgs if m.WhichOneof("payload") == "command_result"][-1]


def _snapshot_models(msgs):
    snaps = [m.capabilities for m in msgs if m.WhichOneof("payload") == "capabilities"]
    return {x.name: x for x in snaps[-1].installed_models} if snaps else {}


def test_verified_model_is_acknowledged_then_switched_and_run(capfd) -> None:
    a = _adapter(FakeRuntime())
    msgs = _run(a, _update(GOOD))
    cr = _result(msgs)
    assert cr.model_update.acknowledged and cr.model_update.verified_signer == "demo-root"
    [t] = _terminal(msgs)
    assert t.outcome == pb.INSTALL_OUTCOME_SUCCEEDED and t.resulting_version == GOOD
    kinds = [m.WhichOneof("payload") for m in msgs]
    assert kinds.index("command_result") < msgs.index(
        next(
            m
            for m in msgs
            if m.WhichOneof("payload") == "update_progress"
            and m.update_progress.outcome != pb.INSTALL_OUTCOME_UNSPECIFIED
        )
    )
    assert _snapshot_models(msgs)["floor-hazard"].status == pb.MODEL_STATUS_ACTIVE
    assert "MODEL_RUN" in capfd.readouterr().err


_SIG = "signature does not verify against the trust root"
_CK = "checksum mismatch (manifest digest x does not match model_checksum)"
_RT = "model runtime unavailable (missing onnxruntime)"


@pytest.mark.parametrize(
    "err,word",
    [
        (mr.SignatureRefusedError(_SIG), "SignatureVerificationFailed"),
        (mr.ArtifactRefusedError(_CK), "checksum"),
        (mr.RuntimeUnavailableError(_RT), "runtime unavailable"),
    ],
)
def test_unproven_artifacts_are_refused_before_the_ack(err, word) -> None:
    rt = err if isinstance(err, mr.RuntimeUnavailableError) else FakeRuntime(refuse=err)
    a = _adapter(rt)
    msgs = _run(a, _update(GOOD), settle=False)
    cr = _result(msgs)
    assert cr.model_update.acknowledged is False
    assert word in cr.model_update.message
    [t] = _terminal(msgs)
    assert t.outcome == pb.INSTALL_OUTCOME_FAILED and word in t.failure_reason
    assert "floor-hazard" not in a._model_versions


def test_self_check_failure_after_ack_keeps_the_previous_model_and_reports_failed() -> None:
    a = _adapter(FakeRuntime())
    _run(a, _update(GOOD))
    msgs = _run(a, _update(BROKEN), cid=2)
    assert _result(msgs).model_update.acknowledged is True  # verified: the bytes were right
    [t] = _terminal(msgs)
    assert t.outcome == pb.INSTALL_OUTCOME_FAILED
    assert t.resulting_version == GOOD  # still running the model it had
    assert "self-check failed" in t.failure_reason
    snap = _snapshot_models(msgs)["floor-hazard"]
    assert snap.status == pb.MODEL_STATUS_FAILED and snap.running_version == GOOD
    assert a._model_versions["floor-hazard"] == GOOD


def test_rollback_reactivates_a_resident_version_and_clears_the_failure() -> None:
    a = _adapter(FakeRuntime())
    _run(a, _update(GOOD))
    _run(a, _update(BROKEN), cid=2)
    msgs = _run(a, _update(GOOD, rollback=True), cid=3)
    assert _result(msgs).model_update.acknowledged is True
    [t] = _terminal(msgs)
    assert t.outcome == pb.INSTALL_OUTCOME_SUCCEEDED and t.resulting_version == GOOD
    assert _snapshot_models(msgs)["floor-hazard"].status == pb.MODEL_STATUS_ACTIVE


def test_rollback_to_a_version_this_robot_never_held_is_refused() -> None:
    rt = FakeRuntime()
    a = _adapter(rt)
    msgs = _run(a, _update("1.0.0", rollback=True), settle=False)
    cr = _result(msgs)
    assert cr.model_update.acknowledged is False and "not resident" in cr.model_update.message
    assert rt.fetched == []


@pytest.mark.parametrize("version", [GOOD, BROKEN], ids=["install succeeded", "self-check failed"])
def test_an_install_result_is_reported_once_and_nothing_is_scheduled_to_resend_it(
        monkeypatch, version) -> None:
    """The install outcome is streamed once and snapshotted once (RFC-0001
    #fleet-adapter-protocol, install-outcome reporting). Keeping a report that lands while the
    control plane marks the robot Updating is the control plane's job; a second snapshot on a
    timer was the adapter compensating for a race it cannot see."""
    started = []

    class _RecordingTimer:
        def __init__(self, delay, fn, *args, **kwargs):
            started.append((delay, getattr(fn, "__name__", repr(fn))))
            self.daemon = True

        def start(self) -> None:
            pass

    monkeypatch.setattr(sim_adapter.threading, "Timer", _RecordingTimer)
    a = _adapter(FakeRuntime())
    msgs = _run(a, _update(version))
    assert sum(m.WhichOneof("payload") == "capabilities" for m in msgs) == 1
    assert [name for _d, name in started if name == "_send_capabilities_snapshot"] == [], (
        f"an install result scheduled a delayed re-send of the snapshot: {started}")
