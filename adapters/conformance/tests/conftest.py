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

"""Stub wiring and the fake adapter these tests drive the harness against.

``harness.pb`` / ``harness.pb_grpc`` are injected by ``__main__`` at run time so
the module imports without the generated stubs present. The tests do the same
injection, from the same two candidate layouts, and then never touch gRPC: a
``_Session`` is a pair of queues, so a driver check can be run against messages
placed on them by hand.
"""

from __future__ import annotations

import queue
import sys
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parents[3]


def _load_stubs():
    for p in (str(_ROOT), str(_ROOT / "proto")):
        if p not in sys.path:
            sys.path.insert(0, p)
    try:
        from fleet_adapter.v1 import fleet_adapter_pb2 as pb
        from fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc
    except ImportError:  # pragma: no cover - depends on which `make proto` ran
        from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
        from proto.fleet_adapter.v1 import fleet_adapter_pb2_grpc as pb_grpc
    return pb, pb_grpc


@pytest.fixture(scope="session")
def pb():
    from adapters.conformance import harness

    _pb, _pb_grpc = _load_stubs()
    harness.pb = _pb
    harness.pb_grpc = _pb_grpc
    return _pb


@pytest.fixture
def harness_mod(pb):
    from adapters.conformance import harness

    return harness


@pytest.fixture
def driver(harness_mod):
    """A ``_Driver`` over an in-memory session, with no server behind it.

    Returns ``(driver, session, report)``. Put ``AdapterMessage``s on
    ``session.control_in`` / ``session.safety_in`` to play the adapter, and read
    ``session.control_out`` to see what the harness sent.
    """
    from adapters.conformance.report import Report

    session = harness_mod._Session()
    report = Report(adapter="test-double")
    drv = harness_mod._Driver(session, report, port=0)
    # A declared catalog, as a scan reply would give one: every acceptance probe carries a
    # type the adapter declared (C12.1 reads it before any assignment is sent). A test that
    # grades the empty-catalog path resets `_catalog`/`_served` itself.
    pb = harness_mod.pb
    navigate = pb.SupportedAction(action_type="Navigate", params=[
        pb.ActionParam(name="x", unit="m", kind="number", required=True),
        pb.ActionParam(name="y", unit="m", kind="number", required=True)])
    drv._catalog = [navigate]
    drv._served = navigate
    return drv, session, report


@pytest.fixture
def sent(harness_mod):
    """Drain a harness->adapter queue into a list of ``ControlPlaneMessage``."""

    def _drain(q: queue.Queue) -> list:
        out = []
        while True:
            try:
                item = q.get_nowait()
            except queue.Empty:
                return out
            # The stream-fault and stream-close sentinels are bare objects, not
            # messages: drop_control() puts one on this same queue.
            if hasattr(item, "WhichOneof"):
                out.append(item)

    return _drain
