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

"""Floor mapping for the reference simulation adapter: ``Custom`` explore-floor and ``Charge``.

This module is the simulation's stand-in for a robot that explores a floor segment by segment,
returns to charge between segments, and hands the map it built to the cluster **by reference**.
It moves no simulated robot and runs no SLAM. It exists so that the control-plane side of the
chain (a looped FleetTask, the map reference, the zone proposal, the operator's confirm) can be
run end to end without hardware. Every message it emits says ``sim``.

What reaches the control plane, and how
---------------------------------------

Nothing but ``ActionStatusUpdate.message`` and ``progress_pct``. The verb this module serves
reaches the adapter as ``AssignAction.custom_type`` (``FleetAction.spec.customType``, contract
1.4.0), or, from a control plane that does not send it, in the payload as
``operation: explore-floor``. ``verb_of`` reads the two and refuses them when they disagree. A
``Custom`` assignment that names no verb either way keeps the behaviour it always had.

The map is saved at a deterministic path keyed by the action id, ``<namespace>/<name>``, under a
directory an ``ArtifactStore`` declares. ``AssignAction.action_id`` carries the FleetAction's
name; the namespace is the adapter's own (``--namespace``). An id already of the form
``<namespace>/<name>`` is accepted as it is. The final ``SUCCEEDED`` message carries two
tokens a consumer parses::

    map-ref=<store>/<namespace>/<name> map-sha256=<64 hex>

``map-sha256`` is the digest of the directory's ``MANIFEST.sha256``, which lists the digest of
every other file in the directory, so one digest in the message verifies all of them.

Status writes
-------------

The control plane writes ``FleetAction.status`` whenever a ``RUNNING`` report changes its
message or its progress. A job therefore reports only at phase changes, a handful per segment,
and never per tick.
"""

from __future__ import annotations

import hashlib
import os
import re
import tempfile
from dataclasses import dataclass, field
from pathlib import Path

#: The one ``Custom`` operation this module serves.
OPERATION_EXPLORE = "explore-floor"

#: The speed ceiling for exploration. A payload above it is refused, never clamped.
MAX_SPEED_MPS = 0.20

#: The parameters an explore-floor payload may carry: (name, kind, unit, min, max).
#: ``min``/``max`` of None mean unbounded on that side. Declared in the catalog with the same
#: bounds this module enforces, so the catalog and the refusal cannot disagree.
EXPLORE_PARAMS: tuple[tuple[str, str, str, float | None, float | None], ...] = (
    ("bound_ref", "string", "", None, None),
    ("keepouts_ref", "string", "", None, None),
    ("max_speed_mps", "number", "m/s", 0.01, MAX_SPEED_MPS),
    ("battery_return_mv", "number", "mV", 1.0, None),
    ("exhausted_after_recoveries", "number", "", 1.0, None),
    ("time_box_s", "number", "s", 1.0, None),
    ("resume", "bool", "", None, None),
)

#: What a successful explore-floor segment reports, declared as SupportedAction.results.
EXPLORE_RESULTS: tuple[tuple[str, str, str], ...] = (
    ("map-ref", "string", ""),
    ("map-sha256", "string", ""),
    ("covered-m2", "number", "m2"),
)

#: The bound a payload that names none explores: the whole simulated world.
DEFAULT_BOUND = "sim-world"

#: Simulated seconds a segment spends exploring, and returning to the mark.
EXPLORE_SECONDS = 4.0
RETURN_SECONDS = 1.0

#: Simulated seconds with the cable in before the battery reading is checked.
CHARGE_DWELL_SECONDS = 2.0
#: How fast the simulated battery reading rises while charging, in percent per second.
CHARGE_RATE_PCT_PER_S = 20.0
#: The reading that counts as full. Not 100: the kinematic plant drains a hundredth of a
#: percent a second and reports a whole number rounded down, so a battery charged to 100.0
#: reads 99 on the next tick and would never read full again.
FULL_PCT = 99

MANIFEST = "MANIFEST.sha256"

_DNS_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")


@dataclass(frozen=True)
class Update:
    """One ActionStatusUpdate a job asks the adapter to send."""

    state: str  # RUNNING | SUCCEEDED | FAILED
    message: str
    progress_pct: int = 0


# ── The simulated world ───────────────────────────────────────────────────────
#
# Two rooms joined by one doorway, drawn as a ROS occupancy-grid pair (a YAML
# header and a binary PGM), which is what a real robot's map saver writes. The
# rooms are revealed one per segment: the first segment sees room A, the next
# sees room B through the doorway, and a segment after that finds no frontier.

_RES = 0.05
_W, _H = 180, 90  # 9.0 m x 4.5 m
_FREE, _OCC, _UNKNOWN = 254, 0, 205
# (x0, y0, x1, y1) in metres, map frame.
_ROOMS = ((0.5, 0.5, 5.0, 4.0), (5.2, 0.5, 8.5, 4.0))
_DOOR = (5.0, 1.8, 5.2, 2.6)


def room_count() -> int:
    return len(_ROOMS)


def _room_area_m2(i: int) -> float:
    x0, y0, x1, y1 = _ROOMS[i]
    return (x1 - x0) * (y1 - y0)


def world_area_m2() -> float:
    return sum(_room_area_m2(i) for i in range(len(_ROOMS)))


def render_map(revealed: int) -> tuple[bytes, bytes]:
    """The map YAML and PGM with the first ``revealed`` rooms observed.

    Unobserved space is UNKNOWN (205), not free and not occupied: a room the robot has not
    seen yet is not a room the map may claim. The doorway is observed only once the room
    beyond it is.
    """
    grid = [[_UNKNOWN] * _W for _ in range(_H)]

    def fill(box: tuple[float, float, float, float], value: int) -> None:
        x0, y0, x1, y1 = box
        for r in range(round(y0 / _RES), round(y1 / _RES)):
            for c in range(round(x0 / _RES), round(x1 / _RES)):
                grid[_H - 1 - r][c] = value

    for i in range(min(revealed, len(_ROOMS))):
        x0, y0, x1, y1 = _ROOMS[i]
        # The walls around an observed room are observed too.
        fill((x0 - 0.2, y0 - 0.2, x1 + 0.2, y1 + 0.2), _OCC)
    for i in range(min(revealed, len(_ROOMS))):
        fill(_ROOMS[i], _FREE)
    if revealed >= 2:
        fill(_DOOR, _FREE)
    pgm = b"P5\n%d %d\n255\n" % (_W, _H) + bytes(v for row in grid for v in row)
    yaml_text = (
        "image: map.pgm\n"
        f"resolution: {_RES}\n"
        "origin: [0.0, 0.0, 0.0]\n"
        "negate: 0\n"
        "occupied_thresh: 0.65\n"
        "free_thresh: 0.196\n"
    ).encode()
    return yaml_text, pgm


# ── Validation ────────────────────────────────────────────────────────────────


def operation_of(doc: dict) -> str | None:
    """The payload's ``operation``, or None when it names none."""
    op = doc.get("operation")
    return op if isinstance(op, str) else (None if op is None else repr(op))


def verb_of(custom_type: str, doc: dict) -> tuple[str | None, str]:
    """(verb, "") for a Custom assignment, or (None, reason) when it must be refused.

    ``custom_type`` names the verb when it is set; the payload's ``operation`` names it
    otherwise, and (None, "") means the assignment names no verb at all. When both are set
    they must agree: an operator reads ``spec.customType`` and the robot would act on the
    payload, so a disagreement is refused rather than resolved in favour of either.
    """
    op = operation_of(doc)
    if not custom_type:
        return op, ""
    if op is not None and op != custom_type:
        return None, (f"custom_type {custom_type!r} and the payload's operation {op!r} disagree; "
                      f"the verb must be named once, or the same way twice")
    return custom_type, ""


def validate_explore(doc: dict) -> str:
    """The reason an explore-floor payload is refused, or "" when it is servable.

    Every bound is enforced here, including the speed ceiling: a payload asking for more than
    MAX_SPEED_MPS is refused with the reason, never run at a lower speed than it asked for.
    Parameters this module does not know are left alone, so a payload that also carries a
    target another consumer reads is not refused for it.
    """
    for name, kind, unit, lo, hi in EXPLORE_PARAMS:
        if name not in doc:
            continue
        value = doc[name]
        if kind == "string":
            if not isinstance(value, str) or not value:
                return f"{name} must be a non-empty string"
        elif kind == "bool":
            if not isinstance(value, bool):
                return f"{name} must be true or false"
        else:
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                return f"{name} must be a number"
            suffix = f" {unit}" if unit else ""
            if lo is not None and value < lo:
                return f"{name} {value}{suffix} is below the minimum of {lo:g}{suffix}"
            if hi is not None and value > hi:
                return (f"{name} {value}{suffix} is above the maximum of {hi:g}{suffix}; "
                        f"the adapter refuses rather than run slower than it was asked")
    return ""


def split_action_id(action_id: str, namespace: str) -> tuple[str, str] | None:
    """(namespace, name) for an AssignAction.action_id, or None if it is not one.

    The control plane sends the FleetAction's name, and the namespace is the adapter's own;
    an id of the form ``<namespace>/<name>`` is taken as it is. The path an artifact is saved
    at is built from these, so anything that is not a plain DNS name is refused rather than
    joined into a path.
    """
    parts = action_id.split("/") if "/" in action_id else [namespace, action_id]
    if len(parts) != 2 or not all(_DNS_LABEL.match(p) for p in parts):
        return None
    return parts[0], parts[1]


# ── Artifacts ─────────────────────────────────────────────────────────────────


def manifest_text(files: dict[str, bytes]) -> bytes:
    """``sha256sum``-format lines for every file, sorted by name."""
    lines = [f"{hashlib.sha256(files[n]).hexdigest()}  {n}\n" for n in sorted(files)]
    return "".join(lines).encode()


class ArtifactDir:
    """A directory an ArtifactStore declares, written at ``<namespace>/<name>/``.

    ``store`` is the ArtifactStore's name, which is what the map reference names. With no
    directory configured the adapter writes to a scratch directory of its own, which no
    cluster reads; the reference it reports then names a store that does not exist, and a
    consumer refuses it.
    """

    def __init__(self, root: str | None, store: str) -> None:
        self.store = store
        self.root = Path(root) if root else Path(tempfile.mkdtemp(prefix="sim-artifacts-"))

    def save(self, namespace: str, name: str, files: dict[str, bytes]) -> tuple[str, str]:
        """Write ``files`` plus the manifest; return (map-ref, map-sha256).

        The directory is written beside its final place and renamed into it, so a reader
        never sees a half-written map. A directory already at the path is replaced: the path
        is the action's, and a re-run of the same action id is the same artifact.
        """
        manifest = manifest_text(files)
        final = self.root / namespace / name
        final.parent.mkdir(parents=True, exist_ok=True)
        staging = Path(tempfile.mkdtemp(prefix=f".{name}-", dir=final.parent))
        for fname, data in {**files, MANIFEST: manifest}.items():
            (staging / fname).write_bytes(data)
            os.chmod(staging / fname, 0o644)
        os.chmod(staging, 0o755)
        if final.exists():
            for child in final.iterdir():
                child.unlink()
            final.rmdir()
        staging.rename(final)
        return f"{self.store}/{namespace}/{name}", hashlib.sha256(manifest).hexdigest()


# ── Jobs ──────────────────────────────────────────────────────────────────────


@dataclass
class ExploreState:
    """What a resumed segment reloads: how much of the bound has been observed."""

    revealed: int = 0


@dataclass
class ExploreJob:
    """One explore-floor segment.

    Phases: exploring -> returning to the mark -> saved. A segment observes one more room;
    a resumed segment that finds everything observed reports "no frontier left" and ends at
    once, which is what lets a loop of segments bounded by a count finish early in effect.
    """

    action_id: str
    doc: dict
    states: dict[str, ExploreState]
    artifacts: ArtifactDir
    namespace: str = "default"
    elapsed: float = 0.0
    phase: str = "start"
    frontier_left: bool = True
    _bound: str = field(init=False, default="")

    def __post_init__(self) -> None:
        self._bound = self.doc.get("bound_ref", DEFAULT_BOUND)

    def tick(self, dt: float) -> list[Update]:
        out: list[Update] = []
        if self.phase == "start":
            ids = split_action_id(self.action_id, self.namespace)
            if ids is None:
                self.phase = "done"
                return [Update("FAILED", f"explore-floor (sim): action id {self.action_id!r} "
                                         f"in namespace {self.namespace!r} is not a DNS name; "
                                         f"no artifact path can be built")]
            resume = bool(self.doc.get("resume", False))
            state = self.states.get(self._bound)
            if resume and state is None:
                self.phase = "done"
                return [Update("FAILED", f"explore-floor (sim): resume requested but no saved "
                                         f"graph exists for bound {self._bound!r}")]
            if not resume:
                state = self.states[self._bound] = ExploreState()
            assert state is not None
            if state.revealed >= room_count():
                self.frontier_left = False
                self.phase = "saving"
            else:
                self.phase = "exploring"
                out.append(Update("RUNNING", f"explore-floor (sim): bound {self._bound} proven; "
                                             f"exploring", 5))
            if self.phase == "exploring":
                return out
        if self.phase == "exploring":
            self.elapsed += dt
            if self.elapsed >= EXPLORE_SECONDS:
                self.states[self._bound].revealed += 1
                self.phase = "returning"
                self.elapsed = 0.0
                out.append(Update("RUNNING", "explore-floor (sim): segment threshold reached; "
                                             "explorer stopped; returning to the mark", 80))
            return out
        if self.phase == "returning":
            self.elapsed += dt
            if self.elapsed >= RETURN_SECONDS:
                self.phase = "saving"
            else:
                return out
        if self.phase == "saving":
            self.phase = "done"
            out.append(self._save())
        return out

    def _save(self) -> Update:
        namespace, name = split_action_id(self.action_id, self.namespace)  # type: ignore[misc]
        state = self.states[self._bound]
        if state.revealed >= room_count():
            self.frontier_left = False
        yaml_text, pgm = render_map(state.revealed)
        files = {
            "map.yaml": yaml_text,
            "map.pgm": pgm,
            # No SLAM runs here, so there is no pose graph to serialize and none is written.
            # The frame delta is zero because the simulated map frame IS the sim world frame.
            "frame-delta.json": b'{"dx_m": 0.0, "dy_m": 0.0, "dyaw_rad": 0.0, '
                                b'"source": "sim: the map frame is the sim world frame"}\n',
        }
        ref, digest = self.artifacts.save(namespace, name, files)
        covered = sum(_room_area_m2(i) for i in range(state.revealed))
        pct = round(100 * covered / world_area_m2())
        frontier = "remaining" if self.frontier_left else "none-left"
        return Update(
            "SUCCEEDED",
            f"explore-floor (sim): segment done; covered {covered:.1f} of "
            f"{world_area_m2():.1f} m2 ({pct}%); exhausted: none; frontier: {frontier}; "
            f"map-ref={ref} map-sha256={digest}",
            100,
        )


@dataclass
class ChargeJob:
    """``Charge``: RUNNING "waiting for charge" until the battery reads full.

    It never moves the robot. The simulation has no cable, so "the cable is out" cannot be
    observed; the job says so in its final message rather than claiming it.
    """

    action_id: str
    elapsed: float = 0.0
    phase: str = "start"

    def tick(self, dt: float, battery_pct: int, charge) -> list[Update]:
        if self.phase == "start":
            self.phase = "waiting"
            return [Update("RUNNING", f"waiting for charge (sim): battery {battery_pct}%", 0)]
        if self.phase != "waiting":
            return []
        if charge is None:
            self.phase = "done"
            return [Update("FAILED", "Charge (sim): this simulator backend does not model "
                                     "charging")]
        self.elapsed += dt
        if self.elapsed < CHARGE_DWELL_SECONDS:
            return []
        if battery_pct >= FULL_PCT:
            self.phase = "done"
            return [Update("SUCCEEDED", f"Charge (sim): battery full ({battery_pct}%); the "
                                        f"simulation has no cable, so cable-out is not "
                                        f"observed", 100)]
        charge(CHARGE_RATE_PCT_PER_S * dt)
        return []
