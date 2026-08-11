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

"""The simulation adapter reports the disposition when it honours a cancel.

`CancelDisposition` is the adapter's OWN determination of whether the robot
reached a safe stop; the control plane cannot derive it. When a cancel is
mishandled the observable symptom is that the control plane does not reassign,
which is equally consistent with a scheduler, lease or capability fault -- none
of which are in the adapter. So the adapter has to say what it did.

Level note: `test_demo_test.py` tests the demo harness's PARSER of these markers.
This file tests the ADAPTER that emits one. Those are opposite sides of the same
contract, so extending that file would have tested the wrong side.
"""

from __future__ import annotations

import pathlib
import sys

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # pragma: no cover - depends on --stub-path layout
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

_ROBOT = "sim-robot-001"
_ADAPTER_SRC = (
    pathlib.Path(__file__).resolve().parents[2]
    / "adapters" / "simulation" / "sim_adapter.py"
)


def _adapter() -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), _ROBOT, "warehouse-a", "sim")
    # The adapter spawns itself into the simulator on connect (sim_adapter.py:230);
    # these tests drive _handle_command directly, so do it here.
    a._sim.spawn(_ROBOT, 0.0, 0.0)
    return a


def _cancel(command_id: int = 77):
    return pb.Command(
        command_id=command_id,
        robot_id=_ROBOT,
        cancel_action=pb.CancelAction(),
    )


def _marker_lines(captured: str, marker: str) -> list[str]:
    return [ln for ln in captured.splitlines() if ln.startswith(marker)]


def _boom(*_args: object, **_kwargs: object) -> None:
    raise RuntimeError("step failed")


def test_cancel_action_emits_the_marker(capsys: pytest.CaptureFixture[str]) -> None:
    a = _adapter()
    a._current_action = "haul-8846"

    a._handle_command(_cancel(command_id=77))

    lines = _marker_lines(capsys.readouterr().err, "CANCEL_HONOURED")
    assert len(lines) == 1, (
        "honouring a cancel produced no CANCEL_HONOURED line; the adapter's own "
        "disposition determination is unobservable, so a mishandled cancel can only "
        "be diagnosed from the control plane failing to reassign"
    )
    line = lines[0]
    for field in (
        f"robot={_ROBOT}",
        "action=haul-8846",
        "command_id=77",
        "disposition=STOPPED_SAFELY",
    ):
        assert field in line, f"marker missing {field!r}: {line!r}"


def test_marker_names_the_cancelled_action_not_an_empty_string(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """The branch clears self._current_action; the name must be captured first.

    Guarded separately because this failure is silent: the line is still emitted
    and still parses, it simply names nothing -- worse than no line, since a reader
    concludes the adapter cancelled an action it could not identify. Asserting the
    name AND the cleared field together is what pins the ordering: the marker can
    only carry the name if it was read before the clear.
    """
    a = _adapter()
    a._current_action = "inspect-dock"

    a._handle_command(_cancel())

    line = _marker_lines(capsys.readouterr().err, "CANCEL_HONOURED")[0]
    assert "action=inspect-dock" in line, f"marker does not name the action: {line!r}"
    assert "action= " not in line and not line.rstrip().endswith("action="), (
        f"marker names an empty action -- the name was read after the clear: {line!r}"
    )
    assert a._current_action == "", "the branch must still clear the current action"


def test_disposition_is_read_back_from_the_wire_value(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """The logged disposition must be derived from the enum, not written twice.

    A hard-coded string stays put when the value on the wire changes, and a log
    that contradicts the message is worse than no log at all.
    """
    a = _adapter()
    a._current_action = "haul-1"

    a._handle_command(_cancel())

    line = _marker_lines(capsys.readouterr().err, "CANCEL_HONOURED")[0]
    on_the_wire = pb.CancelDisposition.Name(
        pb.CANCEL_DISPOSITION_STOPPED_SAFELY
    ).removeprefix("CANCEL_DISPOSITION_")
    assert f"disposition={on_the_wire}" in line


def test_marker_means_honoured_not_merely_received(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """The line must be downstream of the stop, the lease release and the emit.

    This is the edge the whole marker exists for. A line printed on RECEIPT would
    be actively harmful: it would read as confirmation while the cancel silently
    failed, which is worse than the silence it replaced. Proven by making each step
    raise in turn -- if the print were above them it would still appear.
    """
    for failing_step in ("release", "emit"):
        a = _adapter()
        a._current_action = "haul-1"
        a._lease.grant(_ROBOT, 30.0, 1)

        if failing_step == "release":
            a._lease.release = _boom  # type: ignore[method-assign]
        else:
            a._emit_action_status = _boom  # type: ignore[method-assign]

        with pytest.raises(RuntimeError):
            a._handle_command(_cancel())

        assert not _marker_lines(capsys.readouterr().err, "CANCEL_HONOURED"), (
            f"CANCEL_HONOURED was printed even though the {failing_step} step failed; "
            f"the marker claims the cancel was honoured when it was not"
        )


def test_cancel_with_nothing_assigned_still_reports(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """A cancel arriving with no action assigned is honoured, and says so.

    The adapter still stops and still replies acknowledged, so it must still
    report. No ActionStatusUpdate is emitted (there is no action to transition),
    which is the behaviour _emit_action_status's empty-id guard already had --
    pinned here so the marker's presence is not mistaken for one.
    """
    a = _adapter()
    a._current_action = ""
    while not a._outbox.empty():
        a._outbox.get()

    a._handle_command(_cancel(command_id=5))

    line = _marker_lines(capsys.readouterr().err, "CANCEL_HONOURED")[0]
    assert "command_id=5" in line and "disposition=STOPPED_SAFELY" in line

    kinds = []
    while not a._outbox.empty():
        kinds.append(a._outbox.get().WhichOneof("payload"))
    assert "action_status" not in kinds, (
        f"an idle cancel emitted an action status for a nonexistent action: {kinds}"
    )


def test_edge_estop_marker_is_unchanged() -> None:
    """Pin EDGE_ESTOP_CONFIRMED, which another file matches as a literal.

    `examples/full-surface-demo/demo_test.py` greps for this exact string to prove
    the EdgeStream service was exercised -- an edge estop is confirmed directly with
    the edge node, bypassing the control plane, so no CRD status or metric shows it.
    Renaming or reformatting the adapter's line breaks that harness silently. This
    makes the cross-file dependency a test rather than a convention.
    """
    demo = pathlib.Path(__file__).resolve().parents[2] / "examples" / "full-surface-demo"
    sys.path.insert(0, str(demo))
    from demo_test import EDGE_ESTOP_MARKER  # noqa: PLC0415

    assert EDGE_ESTOP_MARKER == "EDGE_ESTOP_CONFIRMED"

    src = _ADAPTER_SRC.read_text(encoding="utf-8")
    assert f'print(f"{EDGE_ESTOP_MARKER} robot=' in src, (
        "the adapter no longer emits the estop marker in the shape "
        "full-surface-demo/demo_test.py matches"
    )
    for field in ("robot={self._robot}", "id={ec.estop.estop_id}", "state="):
        assert field in src, f"estop marker lost its {field!r} field"
