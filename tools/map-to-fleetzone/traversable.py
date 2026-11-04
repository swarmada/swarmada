#!/usr/bin/env python3
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
"""The `traversable` layer: free space the robot actually drove over.

WHY THIS EXISTS. `map_to_fleetzone.py` learned that *unknown is
not free*. This file carries the same argument one step further: **observed-free
is not proven-floor.**

Three tiers, not two:

    driven over   >   observed free   >   unknown
    (the robot's own      (a lidar ray          (no ray ever
     wheels were here)     terminated beyond     reached it)
                           it, once)

Only the first is proof. A 2D lidar reports a descending staircase as open
floor, and this platform has no cliff sensor at all, so
`observed free` is a statement about photons, not about floor. `driven over` is
a statement about a robot that did not fall.

WHAT PRODUCES THE INPUT, AND WHAT DOES NOT. The trajectory lives in the SLAM
pose graph. slam_toolbox serialises its graph with cereal into a `.posegraph` /
`.data` pair, which is a C++ object graph, not a documented interchange format:
reading it needs slam_toolbox itself, and a hand-rolled parser for it would be a
guess dressed as a measurement. So this module reads an EXPORTED trajectory --
one pose per line, plain text -- and `read_pose_graph` refuses the serialised
form with instructions rather than attempting it. See `TRAJECTORY_FORMAT`.

NOTHING IN THIS REPOSITORY CURRENTLY PRODUCES EITHER FILE. As of 2026-09-17
neither `floor1_graph.posegraph` nor any exported trajectory exists in the tree,
so the driven tier cannot be computed for that capture and the tool says so
instead of inventing one.
"""

from __future__ import annotations

import argparse
import math
import sys
from collections import deque
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import map_to_fleetzone as m2f  # noqa: E402

TOOL_VERSION = "0.1.0"

# The MentorPi's footprint. Recorded here as a NUMBER WITH A SOURCE rather than
# a constant, because the swept area scales linearly with it and an operator
# reading "driven 11.4 m2" is entitled to know what width produced it.
#
# The platform's published specification gives the chassis as roughly
# 0.22 m across. The default here is the RADIUS of the circumscribed disc, and a
# disc is used rather than the true rectangle because the robot rotates in place
# and a rectangle swept through a spin would under-report the corners.
DEFAULT_FOOTPRINT_RADIUS_M = 0.11

TRAJECTORY_FORMAT = """\
One pose per line, whitespace- or comma-separated, `#` comments ignored:

    x y                 metres, in the map frame
    x y theta           theta in radians (ignored: the footprint is a disc)
    t x y theta         TUM-style, leading timestamp

Export it from a slam_toolbox pose graph with the SLAM stack that wrote it --
for example by replaying the serialised graph and dumping the node poses. This
tool does not parse the serialised form; see `read_pose_graph`.\
"""


class TraversableError(Exception):
    """A trajectory could not be read, or does not match its map."""


# --------------------------------------------------------------------------
# Input
# --------------------------------------------------------------------------


def read_pose_graph(path: Path) -> list[tuple[float, float]]:
    """Refuse a serialised slam_toolbox pose graph, with instructions.

    This is not a stub that will be filled in later and it is not an apology. A
    `.posegraph` is a cereal-serialised C++ object graph whose layout is the
    library's private business and changes with its version. A Python parser for
    it would be reverse-engineered, untestable against anything in this tree, and
    would fail SILENTLY on a version it did not expect -- producing a plausible
    trajectory and therefore a plausible `driven` figure that no robot drove.

    A refusal that names the export step is worth more than a parser that might
    be right.
    """
    raise TraversableError(
        f"{path}: serialised slam_toolbox pose graphs are not read by this tool.\n"
        "A .posegraph/.data pair is a cereal-serialised C++ object graph, not an "
        "interchange format; parsing it here would be a guess that fails silently "
        "on a version bump and reports a trajectory nobody drove.\n"
        "Export the node poses with the SLAM stack that wrote the graph, then pass "
        f"the result to --trajectory.\n\n{TRAJECTORY_FORMAT}"
    )


def parse_trajectory(text: str, *, source: str = "<text>") -> list[tuple[float, float]]:
    """Parse an exported trajectory into (x, y) metres in the map frame.

    Accepts 2, 3 or 4 columns (see TRAJECTORY_FORMAT). The column count is fixed
    by the FIRST data line and then enforced: a file that changes shape halfway
    through is more likely to be two files concatenated than a trajectory, and
    guessing per-line would silently read a timestamp as an x coordinate.
    """
    poses: list[tuple[float, float]] = []
    width: int | None = None
    for lineno, raw in enumerate(text.splitlines(), start=1):
        line = raw.split("#", 1)[0].strip()
        if not line:
            continue
        parts = line.replace(",", " ").split()
        if width is None:
            if len(parts) not in (2, 3, 4):
                raise TraversableError(
                    f"{source}:{lineno}: {len(parts)} columns; expected 2, 3 or 4.\n\n"
                    f"{TRAJECTORY_FORMAT}"
                )
            width = len(parts)
        elif len(parts) != width:
            raise TraversableError(
                f"{source}:{lineno}: {len(parts)} columns, but line 1 had {width}. "
                "A trajectory that changes shape partway is not read as one file."
            )
        try:
            nums = [float(p) for p in parts]
        except ValueError as exc:
            raise TraversableError(f"{source}:{lineno}: {exc}") from exc
        # 4 columns is TUM (t x y theta); 2 and 3 start at x.
        x, y = (nums[1], nums[2]) if width == 4 else (nums[0], nums[1])
        poses.append((x, y))
    if not poses:
        raise TraversableError(f"{source}: no poses. An empty trajectory proves nothing.")
    return poses


def read_trajectory(path: Path) -> list[tuple[float, float]]:
    if not path.is_file():
        raise TraversableError(f"{path}: no such trajectory file.\n\n{TRAJECTORY_FORMAT}")
    return parse_trajectory(path.read_text(), source=str(path))


# --------------------------------------------------------------------------
# The sweep
# --------------------------------------------------------------------------


def metres_to_cell(x: float, y: float, height: int, meta: m2f.MapMeta) -> tuple[int, int]:
    """Inverse of `map_to_fleetzone.cell_to_metres`, to the containing cell.

    The row flip is the one that matters and it is the same flip, spelled
    backwards: PGM row 0 is the TOP image row, the ROS origin is the BOTTOM-LEFT
    corner. Getting it wrong mirrors the whole trajectory about the map's
    horizontal centre line and still looks plausible -- which is precisely the
    error `test_row_order_mutation_is_caught` exists to catch on the forward
    transform, so the inverse is pinned against the forward one by
    `test_metres_to_cell_round_trips`.
    """
    col = int(math.floor((x - meta.origin_x) / meta.resolution))
    row_from_bottom = int(math.floor((y - meta.origin_y) / meta.resolution))
    return height - 1 - row_from_bottom, col


def sweep_footprint(
    poses: list[tuple[float, float]],
    *,
    meta: m2f.MapMeta,
    width: int,
    height: int,
    radius_m: float = DEFAULT_FOOTPRINT_RADIUS_M,
) -> set[tuple[int, int]]:
    """Rasterise the robot footprint swept along the trajectory.

    Same grid, same origin, same resolution as the occupancy map -- the whole
    point is that the result can be counted against `observed free` cell for
    cell.

    The trajectory is sampled along each segment at half-cell steps rather than
    at its recorded poses only. A pose graph node every 0.3 m with a 0.11 m
    footprint would otherwise leave gaps between discs and report a dotted line
    as the driven area.
    """
    if radius_m < 0:
        raise TraversableError(f"footprint radius {radius_m} is negative")
    res = meta.resolution
    step = res / 2.0
    r_cells = radius_m / res
    r_ceil = int(math.ceil(r_cells))
    driven: set[tuple[int, int]] = set()

    def stamp(x: float, y: float) -> None:
        row, col = metres_to_cell(x, y, height, meta)
        for dr in range(-r_ceil, r_ceil + 1):
            for dc in range(-r_ceil, r_ceil + 1):
                # Cell centre against the disc: the cell's own centre is half a
                # cell from its (row, col) reference corner in both axes, and
                # both offsets cancel in the difference, so comparing integer
                # offsets is exact here.
                if dr * dr + dc * dc > r_cells * r_cells:
                    continue
                rr, cc = row + dr, col + dc
                if 0 <= rr < height and 0 <= cc < width:
                    driven.add((rr, cc))

    if len(poses) == 1:
        stamp(*poses[0])
        return driven

    for (x0, y0), (x1, y1) in zip(poses, poses[1:]):
        seg = math.hypot(x1 - x0, y1 - y0)
        n = max(1, int(math.ceil(seg / step)))
        for i in range(n + 1):
            t = i / n
            stamp(x0 + (x1 - x0) * t, y0 + (y1 - y0) * t)
    return driven


# --------------------------------------------------------------------------
# The three-tier account
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class TierReport:
    """What the three tiers measure, in m2, plus the number that decides them."""

    driven_cells: int
    observed_free_cells: int
    unknown_cells: int
    occupied_cells: int
    cell_area_m2: float
    largest_unproven_cells: int
    largest_unproven_centroid: tuple[float, float] | None
    driven_outside_free_cells: int

    @property
    def driven_m2(self) -> float:
        return self.driven_cells * self.cell_area_m2

    @property
    def observed_free_m2(self) -> float:
        return self.observed_free_cells * self.cell_area_m2

    @property
    def ratio(self) -> float:
        """driven / observed-free. The layer's whole claim, in one number."""
        return self.driven_cells / self.observed_free_cells if self.observed_free_cells else 0.0

    @property
    def largest_unproven_m2(self) -> float:
        return self.largest_unproven_cells * self.cell_area_m2

    def verdict(self) -> str:
        """Whether the third tier earned its place, stated rather than implied.

        The prompt that commissioned this layer asked for exactly this call to be
        made: *if driven is approximately observed-free, say so -- a layer that
        adds nothing is a finding, not a failure.*
        """
        if self.observed_free_cells == 0:
            return "NO OBSERVED FREE SPACE: the three-tier model has nothing to separate."
        if self.ratio >= 0.95:
            return (
                f"THE LAYER ADDS LITTLE HERE: driven covers {self.ratio:.1%} of observed-free, "
                f"and the largest unproven region is {self.largest_unproven_m2:.2f} m2. On this "
                "map the two tiers are nearly the same set, which is a finding about this run "
                "-- a robot that drove everywhere it saw -- not a defect in the model."
            )
        return (
            f"THE LAYER EARNS ITS PLACE: driven covers {self.ratio:.1%} of observed-free, "
            f"leaving {self.observed_free_m2 - self.driven_m2:.2f} m2 seen but never driven. "
            f"The largest single such region is {self.largest_unproven_m2:.2f} m2 -- that is "
            "the number to show an operator first, because it is floor the map calls open and "
            "no wheel has tested."
        )


def largest_region_without(
    region: set[tuple[int, int]], driven: set[tuple[int, int]]
) -> list[tuple[int, int]]:
    """The largest 4-connected component of `region - driven`.

    THE HEADLINE NUMBER. Total un-driven area is easy to shrug off when it is
    spread as a one-cell rind around every wall -- which is what a footprint
    sweep always leaves. A single contiguous 6 m2 of floor the map calls open and
    the robot never entered is a different statement: it is a room, or a
    staircase.
    """
    unproven = region - driven
    best: list[tuple[int, int]] = []
    seen: set[tuple[int, int]] = set()
    for start in unproven:
        if start in seen:
            continue
        comp: list[tuple[int, int]] = []
        queue = deque([start])
        seen.add(start)
        while queue:
            r, c = queue.popleft()
            comp.append((r, c))
            for nr, nc in ((r - 1, c), (r + 1, c), (r, c - 1), (r, c + 1)):
                if (nr, nc) in unproven and (nr, nc) not in seen:
                    seen.add((nr, nc))
                    queue.append((nr, nc))
        if len(comp) > len(best):
            best = comp
    return best


def tier_report(*,
    driven: set[tuple[int, int]],
    observed_free: set[tuple[int, int]],
    unknown_cells: int,
    occupied_cells: int,
    meta: m2f.MapMeta,
    height: int,
) -> TierReport:
    """Account for the three tiers against one another.

    `driven` is intersected with `observed_free` before it is counted. A swept
    footprint can spill onto a cell the map calls occupied -- the trajectory is
    the robot's CENTRE and the map's walls are inflated by nothing -- and
    counting those would let the driven tier exceed the free tier, which is not a
    measurement but an artefact of two rasterisations disagreeing. The spill is
    reported separately rather than dropped silently: a large one means the
    trajectory and the map are not in the same frame.
    """
    inside = driven & observed_free
    biggest = largest_region_without(observed_free, inside)
    centroid = None
    if biggest:
        rs = sum(r for r, _ in biggest) / len(biggest)
        cs = sum(c for _, c in biggest) / len(biggest)
        centroid = m2f.cell_to_metres(int(round(rs)), int(round(cs)), height, meta)
    return TierReport(
        driven_cells=len(inside),
        observed_free_cells=len(observed_free),
        unknown_cells=unknown_cells,
        occupied_cells=occupied_cells,
        cell_area_m2=meta.resolution**2,
        largest_unproven_cells=len(biggest),
        largest_unproven_centroid=centroid,
        driven_outside_free_cells=len(driven - observed_free),
    )


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def run(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="traversable.py",
        description="Report the three free-space tiers of a map against a driven trajectory.",
    )
    ap.add_argument("map_yaml", type=Path, help="path to <name>.yaml of a ROS map pair")
    ap.add_argument(
        "--trajectory",
        type=Path,
        help=f"exported trajectory. Format:\n{TRAJECTORY_FORMAT}",
    )
    ap.add_argument(
        "--pose-graph",
        type=Path,
        help="a slam_toolbox .posegraph. Refused with instructions -- see --trajectory.",
    )
    ap.add_argument(
        "--footprint-radius",
        type=float,
        default=DEFAULT_FOOTPRINT_RADIUS_M,
        metavar="M",
        help=f"robot footprint radius in metres (default {DEFAULT_FOOTPRINT_RADIUS_M}, MentorPi)",
    )
    ap.add_argument("--free-thresh", type=float, help="override the map YAML's free_thresh")
    args = ap.parse_args(argv)

    if args.pose_graph is not None:
        read_pose_graph(args.pose_graph)  # always raises
    if args.trajectory is None:
        raise TraversableError(
            "--trajectory is required: the driven tier is the robot's own path and there is "
            "no way to derive it from an occupancy grid. Without one this tool would be "
            f"guessing, and a guessed 'driven' figure is worse than none.\n\n{TRAJECTORY_FORMAT}"
        )

    import dataclasses

    yaml_path: Path = args.map_yaml.resolve()
    meta = m2f.read_map_yaml(yaml_path)
    if args.free_thresh is not None:
        meta = dataclasses.replace(meta, free_thresh=args.free_thresh)
    image_path = (yaml_path.parent / meta.image).resolve()
    grid = m2f.read_pgm(image_path)
    m2f.check_thresholds_separate_unknown(grid, meta)

    observed = set(m2f.free_cell_indices(grid, meta))
    counts = {"unknown": 0, "occupied": 0}
    for px in grid.pixels:
        cell = m2f.classify(px, grid.maxval, meta)
        if cell is m2f.Cell.UNKNOWN:
            counts["unknown"] += 1
        elif cell is m2f.Cell.OCCUPIED:
            counts["occupied"] += 1

    poses = read_trajectory(args.trajectory)
    driven = sweep_footprint(
        poses,
        meta=meta,
        width=grid.width,
        height=grid.height,
        radius_m=args.footprint_radius,
    )
    rep = tier_report(
        driven=driven,
        observed_free=observed,
        unknown_cells=counts["unknown"],
        occupied_cells=counts["occupied"],
        meta=meta,
        height=grid.height,
    )

    print(
        f"map                  {yaml_path.name}  {grid.width}x{grid.height} @ {meta.resolution} m"
    )
    print(f"trajectory           {args.trajectory.name}  {len(poses)} poses")
    print(f"footprint radius     {args.footprint_radius} m")
    print()
    print(f"driven over          {rep.driven_m2:8.2f} m2   ({rep.driven_cells} cells)")
    print(
        f"observed free        {rep.observed_free_m2:8.2f} m2   ({rep.observed_free_cells} cells)"
    )
    print(f"unknown              {rep.unknown_cells * rep.cell_area_m2:8.2f} m2")
    print(f"driven / observed    {rep.ratio:8.3f}")
    print()
    print(
        f"largest observed-free region containing NO driven cell: {rep.largest_unproven_m2:.2f} m2"
    )
    if rep.largest_unproven_centroid:
        x, y = rep.largest_unproven_centroid
        print(f"  centred near ({x:.2f}, {y:.2f}) in the map frame")
    if rep.driven_outside_free_cells:
        print(
            f"  NOTE: {rep.driven_outside_free_cells} swept cells fall outside the free set "
            "(trajectory centre vs uninflated walls); excluded from the driven count."
        )
    print()
    print(rep.verdict())
    return 0


def main() -> int:
    try:
        return run()
    except (TraversableError, m2f.MapError) as exc:
        print(f"traversable.py: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
