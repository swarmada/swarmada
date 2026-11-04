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
"""Emit FleetZone manifests enclosing the free space of a ROS occupancy grid.

This is a PRIVATE planning tool. It is not `swarmctl` surface, it is not
documented under `docs/`, and it contacts no cluster: it writes a manifest to
stdout and the operator applies it.

Splitting a floor into per-ROOM zones is a SEPARATE tool and is deliberately out
of scope. What this produces is one honest over-approximation per CONNECTED free
component, annotated with enough provenance to trace it back to the map it came
from.

The default mode traces the free space, so the ring is concave where the floor
is. `bbox` and `hull` are kept because they are hand-verifiable and right for a
genuinely rectangular cell, but they are no longer the default: a convex fit to
a cross-shaped floor plan declares the walls and the neighbouring rooms
navigable, and the safe thing must be what happens when nobody
passes a flag.

Usage:
    map_to_fleetzone.py <map.yaml> [--name NAME] [--namespace NS]
                        [--mode traced|bbox|hull] [--frame-id map]
                        [--simplify-tolerance M] [--max-area-ratio R]
                        [--min-component-area M2]

The map image path is resolved relative to the YAML's own directory, as
`map_server` resolves it.
"""

from __future__ import annotations

import argparse
import hashlib
import sys
from collections.abc import Iterator
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path

import yaml

# Bumped whenever the geometry this tool produces from a given map would change.
# It is recorded in every manifest's annotations: a zone whose polygon cannot be
# reproduced from the map and the tool version that made it is a number with no
# evidence behind it.
TOOL_VERSION = "0.3.0"

# A private prefix, deliberately NOT bare `swarmada.io/`. That namespace is the
# control plane's: keys under it are read by controllers and webhooks, and a
# planning tool inventing neighbours there invites a collision with a key that
# means something.
ANN = "tools.swarmada.io/map-to-fleetzone"

# The defaults, chosen from a parameter sweep over three real robot captures,
# not from taste.
#
# DEFAULT_MODE is `traced` because the safe thing must be what happens when
# nobody passes a flag. `bbox` and `hull` remain available and remain right for
# a genuinely rectangular cell; they are wrong for every real floor plan
# measured so far, and they were the default.
DEFAULT_MODE = "traced"

# 0.10 m = two cells on a 0.05 m/px SLAM map. Measured on floor1, map_01 and
# map_02: at and below 0.10 every traced ring stays simple (189, 250 and 285
# vertices respectively, all within 0.8% of the unsimplified area); at 0.15 all
# three self-intersect. Above that the predicate passes and fails
# NON-MONOTONICALLY -- floor1 is simple at 0.30 and 0.50 but not at 0.15 or
# 0.20 -- which is exactly why the tolerance is checked rather than trusted.
DEFAULT_SIMPLIFY_TOLERANCE_M = 0.10

# A traced ring measures 1.05x-1.06x on the three real maps; the bbox measures
# 3.27x on floor1 and the hull 2.25x. 1.25 leaves the traced mode ~18% of
# headroom for a floor with more interior pillars than these have, and refuses
# both convex modes on any floor plan that is not a rectangle. That refusal is
# the finding, not a mis-set constant: see `_check_area_ratio`.
DEFAULT_MAX_AREA_RATIO = 1.25

# floor1 has 261 connected free components and exactly ONE is larger than 1 m2;
# the other 260 are sub-decimetre speckle. 0.25 m2 is about a MentorPi
# footprint: below it there is no zone worth writing, and emitting one per
# speck would bury the floor.
DEFAULT_MIN_COMPONENT_AREA_M2 = 0.25


class MapError(Exception):
    """A map pair that cannot be read, or cannot be read unambiguously."""


# --------------------------------------------------------------------------
# PGM
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class Grid:
    """A greyscale raster. `pixels` is row-major, row 0 being the TOP image row."""

    width: int
    height: int
    maxval: int
    pixels: list[int]

    def at(self, row: int, col: int) -> int:
        return self.pixels[row * self.width + col]


def _pgm_tokens(data: bytes, start: int) -> Iterator[tuple[bytes, int]]:
    """Yield (token, offset-just-past-it) over a PGM header, skipping # comments."""
    i = start
    n = len(data)
    while i < n:
        c = data[i : i + 1]
        if c.isspace():
            i += 1
            continue
        if c == b"#":
            while i < n and data[i : i + 1] not in (b"\n", b"\r"):
                i += 1
            continue
        j = i
        while j < n and not data[j : j + 1].isspace():
            j += 1
        yield data[i:j], j
        i = j


def read_pgm(path: Path) -> Grid:
    """Read a binary (P5) or ASCII (P2) PGM.

    SLAM stacks emit P5; P2 is accepted because hand-written fixtures are
    readable that way and the two differ only in how samples are encoded.
    """
    data = path.read_bytes()
    toks = _pgm_tokens(data, 0)

    def take() -> tuple[bytes, int]:
        try:
            return next(toks)
        except StopIteration:
            raise MapError(f"{path}: truncated PGM header") from None

    magic, _ = take()
    if magic not in (b"P5", b"P2"):
        raise MapError(f"{path}: not a PGM (magic {magic!r}); expected P5 or P2")

    def take_int(what: str) -> tuple[int, int]:
        tok, end = take()
        try:
            return int(tok), end
        except ValueError:
            raise MapError(f"{path}: bad PGM {what} {tok!r}") from None

    width, _ = take_int("width")
    height, _ = take_int("height")
    maxval, hdr_end = take_int("maxval")

    if width <= 0 or height <= 0:
        raise MapError(f"{path}: PGM dimensions {width}x{height} are not positive")
    if not 0 < maxval < 65536:
        raise MapError(f"{path}: PGM maxval {maxval} out of range")

    count = width * height

    if magic == b"P5":
        # Exactly ONE whitespace byte separates the header from the raster.
        body = data[hdr_end + 1 :]
        if maxval < 256:
            if len(body) < count:
                raise MapError(f"{path}: PGM raster short: {len(body)} bytes for {width}x{height}")
            pixels = list(body[:count])
        else:
            if len(body) < count * 2:
                raise MapError(
                    f"{path}: PGM raster short: {len(body)} bytes for {width}x{height} at 16bpp"
                )
            pixels = [
                (body[2 * i] << 8) | body[2 * i + 1] for i in range(count)
            ]  # PGM is big-endian
    else:
        pixels = []
        for tok, _ in toks:
            pixels.append(int(tok))
            if len(pixels) == count:
                break
        if len(pixels) != count:
            raise MapError(f"{path}: PGM raster short: {len(pixels)} samples for {width}x{height}")

    return Grid(width=width, height=height, maxval=maxval, pixels=pixels)


# --------------------------------------------------------------------------
# Map metadata
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class MapMeta:
    image: str
    resolution: float
    origin_x: float
    origin_y: float
    origin_theta: float
    negate: int
    occupied_thresh: float
    free_thresh: float


def read_map_yaml(path: Path) -> MapMeta:
    doc = yaml.safe_load(path.read_text())
    if not isinstance(doc, dict):
        raise MapError(f"{path}: map YAML is not a mapping")

    def need(key: str):
        if key not in doc:
            raise MapError(f"{path}: map YAML has no '{key}'")
        return doc[key]

    origin = need("origin")
    if not isinstance(origin, (list, tuple)) or len(origin) < 2:
        raise MapError(f"{path}: 'origin' must be [x, y, theta]")

    theta = float(origin[2]) if len(origin) > 2 else 0.0
    if abs(theta) > 1e-9:
        # A rotated grid means the axis-aligned box in MAP metres is not the
        # axis-aligned box in grid metres. Refusing beats emitting a polygon
        # that is wrong in a way nothing downstream can detect.
        raise MapError(
            f"{path}: origin theta is {theta}, not 0. This tool does not rotate "
            "the grid; a rotated map needs a transform this tool does not own."
        )

    resolution = float(need("resolution"))
    if resolution <= 0:
        raise MapError(f"{path}: resolution {resolution} is not positive")

    return MapMeta(
        image=str(need("image")),
        resolution=resolution,
        origin_x=float(origin[0]),
        origin_y=float(origin[1]),
        origin_theta=theta,
        negate=int(doc.get("negate", 0)),
        occupied_thresh=float(doc.get("occupied_thresh", 0.65)),
        free_thresh=float(doc.get("free_thresh", 0.25)),
    )


# --------------------------------------------------------------------------
# Occupancy + the transform
# --------------------------------------------------------------------------


def occupancy(pixel: int, maxval: int, negate: int) -> float:
    """Occupancy probability of one sample, per `map_server`'s convention.

    Dark is occupied unless `negate` flips it. The thresholds live in the map
    YAML precisely because images vary, so nothing here hardcodes 254/205/0.
    """
    if negate:
        return pixel / maxval
    return (maxval - pixel) / maxval


class Cell(StrEnum):
    """The three classes a trinary occupancy grid distinguishes.

    THREE, not two. A cell nobody has observed is not a free cell, and the
    difference is the whole safety content of the map: "no obstacle seen here"
    and "nothing has ever looked here" are the same byte only to a tool that
    has thrown the distinction away.
    """

    FREE = "free"
    OCCUPIED = "occupied"
    UNKNOWN = "unknown"


def classify(pixel: int, maxval: int, meta: MapMeta) -> Cell:
    """Classify one sample, per `map_server`'s trinary convention.

    Three bands, taken from the map YAML's own thresholds:

        occ > occupied_thresh   -> occupied
        occ < free_thresh       -> free
        otherwise               -> unknown

    The middle band is the one that matters here. Collapsing it into `free`
    is how a zone comes to declare unmapped floor safe to route into; see
    `check_thresholds_separate_unknown`.
    """
    occ = occupancy(pixel, maxval, meta.negate)
    if occ > meta.occupied_thresh:
        return Cell.OCCUPIED
    if occ < meta.free_thresh:
        return Cell.FREE
    return Cell.UNKNOWN


def is_free(pixel: int, maxval: int, meta: MapMeta) -> bool:
    return classify(pixel, maxval, meta) is Cell.FREE


def check_thresholds_separate_unknown(grid: Grid, meta: MapMeta) -> None:
    """Refuse a map whose thresholds cannot tell `unknown` from `free`.

    THE DEFECT THIS EXISTS FOR, found on a real MentorPi map and not on any
    synthetic one. A ROS trinary map writes three grey levels -- observed-free,
    observed-occupied, and never-observed. `map_server`'s bands are meant to
    recover them, but that only works if `free_thresh` sits BELOW the unknown
    level's occupancy. The historical ROS value 0.196 is exactly the occupancy
    of the conventional unknown grey 205, chosen so that `occ < free_thresh` is
    false for it. A map saved with `free_thresh: 0.25` -- which nav2's saver
    defaults to -- puts that same grey INSIDE the free band, and every
    unobserved cell silently becomes floor.

    On `map_01` that was 70345 of 88429 "free" cells: 78% of the zone was
    space no sensor had ever seen.

    Detected STRUCTURALLY, not by looking for 205. The signature is:

        the free band holds more than one distinct grey level,
        and the unknown band holds none at all

    A well-formed trinary map puts one level in each band. A genuinely binary
    map (no unknown cells at all) has one level in the free band and trips
    nothing. Only the collapsed case matches, which is why this can refuse
    without knowing any conventional pixel value -- the offending level is
    discovered from the image and named back to the operator.
    """
    by_class: dict[Cell, set[int]] = {c: set() for c in Cell}
    for pixel in set(grid.pixels):
        by_class[classify(pixel, grid.maxval, meta)].add(pixel)

    free_levels = by_class[Cell.FREE]
    if len(free_levels) < 2 or by_class[Cell.UNKNOWN]:
        return

    # The most-occupied level in the free band is the suspect: the observed-free
    # level sits at the low-occupancy extreme, so anything above it is a plateau
    # that the free band has swallowed.
    suspect = max(free_levels, key=lambda p: occupancy(p, grid.maxval, meta.negate))
    occ = occupancy(suspect, grid.maxval, meta.negate)
    count = sum(1 for p in grid.pixels if p == suspect)
    total_free = sum(1 for p in grid.pixels if classify(p, grid.maxval, meta) is Cell.FREE)
    share = 100.0 * count / total_free if total_free else 0.0

    raise MapError(
        f"free_thresh={meta.free_thresh} cannot separate unknown from free.\n"
        f"  The free band holds {len(free_levels)} distinct grey levels and the "
        f"unknown band holds none, which means a level that should be UNKNOWN has "
        f"been classified FREE.\n"
        f"  Grey level {suspect} (occupancy {occ:.4f}) accounts for {count} of "
        f"{total_free} free cells ({share:.0f}%).\n"
        f"  A zone built from this would declare {count * meta.resolution**2:.2f} m2 "
        f"of never-observed space to be floor a robot may be routed across.\n"
        f"  Fix the map's free_thresh, or re-run with "
        f"--free-thresh {occ:.4f} (or lower) to place that level in the unknown band."
    )


def cell_to_metres(row: int, col: int, height: int, meta: MapMeta) -> tuple[float, float]:
    """Map a grid cell to its REFERENCE POINT in metres in the map frame.

    THE THING THIS TOOL IS EASIEST TO GET WRONG.

    Row 0 of a PGM is the TOP image row; the ROS map origin is the BOTTOM-LEFT
    corner of the grid. So the row index must be flipped and the column index
    must not:

        x = origin_x + col * resolution
        y = origin_y + (H - 1 - row) * resolution

    Reverse the y flip and every polygon is mirrored about the map's
    horizontal centre line. It still looks plausible, it still applies cleanly,
    and it only surfaces when a robot is judged outside a zone it is standing
    in. `tests/python/test_map_to_fleetzone.py` pins both halves of this
    independently of any polygon the tool computes.

    The point returned is the cell's BOTTOM-LEFT corner, which is what this
    formula yields and what `map_server` means by a cell index: the cell covers
    [x, x+resolution) x [y, y+resolution). `cell_corners` is what callers that
    need the cell's AREA should use.
    """
    x = meta.origin_x + col * meta.resolution
    y = meta.origin_y + (height - 1 - row) * meta.resolution
    return x, y


def cell_corners(row: int, col: int, height: int, meta: MapMeta) -> list[tuple[float, float]]:
    """The four metric corners of one cell's area.

    A cell is a square of side `resolution`, not a point. A polygon fitted to
    reference points alone stops half a cell short on the top and right edges,
    which would put a robot standing in the outermost free cell OUTSIDE the
    zone that is supposed to contain it -- the same class of silent error as a
    flipped row order, arrived at from the other direction.
    """
    x, y = cell_to_metres(row, col, height, meta)
    r = meta.resolution
    return [(x, y), (x + r, y), (x + r, y + r), (x, y + r)]


def free_cell_indices(grid: Grid, meta: MapMeta) -> list[tuple[int, int]]:
    """(row, col) of every free cell, in row-major order."""
    out: list[tuple[int, int]] = []
    for row in range(grid.height):
        base = row * grid.width
        for col in range(grid.width):
            if is_free(grid.pixels[base + col], grid.maxval, meta):
                out.append((row, col))
    return out


def free_cell_points(grid: Grid, meta: MapMeta) -> list[tuple[float, float]]:
    """Reference point of every free cell, in row-major order."""
    return [
        cell_to_metres(row, col, grid.height, meta) for row, col in free_cell_indices(grid, meta)
    ]


# --------------------------------------------------------------------------
# Polygons
# --------------------------------------------------------------------------


def bounding_box(
    cells: list[tuple[int, int]], height: int, meta: MapMeta
) -> list[tuple[float, float]]:
    """Axis-aligned bounding box over the free cells' AREAS, CCW from bottom-left.

    Deliberately the default. It is an over-approximation -- over a
    corridor-shaped map, a large one -- and the manifest says so rather than
    hiding it. It is also the only enclosing polygon an operator can check by
    hand against the map's own min/max free cell:

        x from  origin_x + min_col * res       to  origin_x + (max_col + 1) * res
        y from  origin_y + (H-1-max_row) * res to  origin_y + (H - min_row) * res

    The `+1` on each max side is the far cell's own width; see `cell_corners`.
    """
    rows = [c[0] for c in cells]
    cols = [c[1] for c in cells]
    r = meta.resolution
    x0 = meta.origin_x + min(cols) * r
    x1 = meta.origin_x + (max(cols) + 1) * r
    y0 = meta.origin_y + (height - 1 - max(rows)) * r
    y1 = meta.origin_y + (height - min(rows)) * r
    return [(x0, y0), (x1, y0), (x1, y1), (x0, y1)]


def convex_hull_of_cells(
    cells: list[tuple[int, int]], height: int, meta: MapMeta
) -> list[tuple[float, float]]:
    """Monotone-chain convex hull over the free cells' corners, CCW.

    A tighter over-approximation than the box for an L- or T-shaped floor.
    Hulls the CORNERS, not the reference points, for the reason `cell_corners`
    gives. Anything CONCAVE is the splitter's problem, not this tool's.
    """
    corners: set[tuple[float, float]] = set()
    for row, col in cells:
        corners.update(cell_corners(row, col, height, meta))
    pts = sorted(corners)
    if len(pts) < 3:
        raise MapError(f"convex hull needs 3 distinct corners, got {len(pts)}")

    def cross(o, a, b) -> float:
        return (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0])

    lower: list[tuple[float, float]] = []
    for p in pts:
        while len(lower) >= 2 and cross(lower[-2], lower[-1], p) <= 0:
            lower.pop()
        lower.append(p)
    upper: list[tuple[float, float]] = []
    for p in reversed(pts):
        while len(upper) >= 2 and cross(upper[-2], upper[-1], p) <= 0:
            upper.pop()
        upper.append(p)
    hull = lower[:-1] + upper[:-1]
    if len(hull) < 3:
        raise MapError("free cells are collinear; no polygon encloses them")
    return hull


def polygon_area(poly: list[tuple[float, float]]) -> float:
    """Shoelace area in m²."""
    total = 0.0
    for i, (x0, y0) in enumerate(poly):
        x1, y1 = poly[(i + 1) % len(poly)]
        total += x0 * y1 - x1 * y0
    return abs(total) / 2.0


# --------------------------------------------------------------------------
# Traced (non-convex) rings
#
# THE DEFECT THIS SECTION EXISTS FOR. `bbox` and `hull` are both
# CONVEX. A cross-, L- or T-shaped floor has no useful convex approximation, so
# the emitted zone declares the walls, the rooms behind them and whatever
# outdoor space falls inside the fit to be navigable. Measured on floor1:
# 54.57 m2 of observed free floor, 178.45 m2 of bbox, 122.64 m2 of hull.
#
# This is NOT the unknown-is-free defect again. That one was unknown cells counted
# as free, and its fix hardened `classify`. This is one layer up: not-floor enclosed by a
# convex fit, which no classifier change can touch.
# --------------------------------------------------------------------------


def free_degree(cell: tuple[int, int], free: set[tuple[int, int]]) -> int:
    """How many of a cell's four edge-neighbours are free."""
    row, col = cell
    return sum(
        1
        for n in ((row - 1, col), (row + 1, col), (row, col - 1), (row, col + 1))
        if n in free
    )


def _pinch_windows(free: set[tuple[int, int]]) -> set[tuple[int, int]]:
    """Top-left cells of every 2x2 window holding a DIAGONAL free pair.

    A 2x2 window whose free cells sit on one diagonal and whose other diagonal
    is not free is the one configuration in which the boundary of the union of
    free squares passes through a single lattice corner TWICE. That corner is a
    pinch: two non-adjacent polygon edges meet there, and `self_intersects` --
    which is the consumer's own predicate, and counts a mere touch -- rejects
    the ring. See `remove_diagonal_pinches` for what is done about it.
    """
    candidates: set[tuple[int, int]] = set()
    for row, col in free:
        for drow in (-1, 0):
            for dcol in (-1, 0):
                candidates.add((row + drow, col + dcol))

    out: set[tuple[int, int]] = set()
    for row, col in candidates:
        tl = (row, col) in free
        tr = (row, col + 1) in free
        bl = (row + 1, col) in free
        br = (row + 1, col + 1) in free
        if (tl and br and not tr and not bl) or (tr and bl and not tl and not br):
            out.add((row, col))
    return out


def remove_diagonal_pinches(
    free: set[tuple[int, int]],
) -> tuple[set[tuple[int, int]], int]:
    """Drop free cells until no 2x2 window holds a diagonal-only free pair.

    Two cells touching only at a corner are not a floor a robot can drive
    across -- nothing fits through a zero-width gap -- so REMOVING one of them
    is the physically honest resolution as well as the safe one. The
    alternative, adding one of the two NON-free cells to close the diagonal,
    would make the ring simple by declaring a wall cell navigable, which is the
    exact class of error this whole section exists to stop. Under-claiming one
    cell (0.0025 m2 at 0.05 m/px) is not the same kind of mistake as
    over-claiming one.

    The victim is the cell with the fewer free edge-neighbours, ties broken
    row-major, so the result is deterministic and the spur is preferred over
    the corridor. Removal can create new pinches, so this iterates to a fixed
    point; each pass removes at least one cell, so it terminates.

    The count is returned, not swallowed: it is recorded in the manifest, and a
    map that loses a lot of cells here is a map whose free space is mostly
    diagonal hairline -- worth an operator's attention.
    """
    free = set(free)
    removed = 0
    while True:
        windows = _pinch_windows(free)
        if not windows:
            return free, removed
        gone = 0
        for row, col in sorted(windows):
            tl = (row, col) in free
            tr = (row, col + 1) in free
            bl = (row + 1, col) in free
            br = (row + 1, col + 1) in free
            if tl and br and not tr and not bl:
                pair = [(row, col), (row + 1, col + 1)]
            elif tr and bl and not tl and not br:
                pair = [(row, col + 1), (row + 1, col)]
            else:
                continue  # already resolved by an earlier removal in this pass
            victim = min(pair, key=lambda p: (free_degree(p, free), p))
            free.discard(victim)
            removed += 1
            gone += 1
        if not gone:
            return free, removed


def free_components(cells: list[tuple[int, int]]) -> list[list[tuple[int, int]]]:
    """4-connected components of the free cells, largest first.

    FOUR-connected, not eight. Two cells touching only at a corner are not
    reachable from one another by anything with width, and joining them would
    put the wall between them inside the ring. `remove_diagonal_pinches` has
    already made the two notions agree.

    Ordered by descending cell count, ties by the component's row-major first
    cell, so the ordering is a function of the map alone.
    """
    remaining = set(cells)
    out: list[list[tuple[int, int]]] = []
    for seed in sorted(remaining):
        if seed not in remaining:
            continue
        remaining.discard(seed)
        stack = [seed]
        comp = [seed]
        while stack:
            row, col = stack.pop()
            for n in ((row - 1, col), (row + 1, col), (row, col - 1), (row, col + 1)):
                if n in remaining:
                    remaining.discard(n)
                    stack.append(n)
                    comp.append(n)
        comp.sort()
        out.append(comp)
    out.sort(key=lambda c: (-len(c), c[0]))
    return out


def _collapse_collinear(ring: list[tuple[int, int]]) -> list[tuple[int, int]]:
    """Drop every vertex that lies in the middle of a straight run, cyclically."""
    n = len(ring)
    if n < 3:
        return list(ring)
    out: list[tuple[int, int]] = []
    for i in range(n):
        ax, ay = ring[i - 1]
        bx, by = ring[i]
        cx, cy = ring[(i + 1) % n]
        if (bx - ax) * (cy - by) - (by - ay) * (cx - bx) != 0:
            out.append((bx, by))
    return out


def _signed_area_units(ring: list[tuple[int, int]]) -> float:
    n = len(ring)
    total = 0
    for i in range(n):
        x0, y0 = ring[i]
        x1, y1 = ring[(i + 1) % n]
        total += x0 * y1 - x1 * y0
    return total / 2.0


def trace_component_rings(
    comp: list[tuple[int, int]], height: int
) -> tuple[list[tuple[int, int]], list[list[tuple[int, int]]]]:
    """Boundary of the union of one component's cell SQUARES, in cell units.

    Returns (outer ring CCW, interior hole rings). The unit lattice point
    (u, v) is the metre point (origin_x + u*res, origin_y + v*res), with
    u = col and v = height-1-row -- which is `cell_to_metres`' flip, carried
    onto the CORNER lattice so that the ring encloses the cells' AREA rather
    than their reference points. Half a cell is 2.5 cm on a 0.05 m/px map, and
    exactly enough to put a robot standing in the outermost free cell outside
    the zone that is meant to contain it.

    A lattice edge is on the boundary when exactly one of the two cells sharing
    it is free, and it is emitted with the free side on its LEFT, so the outer
    ring comes out counter-clockwise and holes clockwise. After
    `remove_diagonal_pinches` every lattice vertex has at most one outgoing
    boundary edge, so the edges chain into disjoint simple cycles with no
    choice to make -- which is why this needs no Moore-neighbour tie-breaking
    and no marching-squares saddle rule.

    INTERIOR HOLES ARE NOT REPRESENTABLE. `PhysicalBounds.Polygon` is a single
    ring (api/v1/fleetzone_types.go:227); there is no `Holes` field anywhere in
    api/ or internal/. A pillar inside a room therefore ends up INSIDE the
    emitted zone. The hole rings are returned so the caller can count that area
    and say so in the manifest rather than let it disappear.
    """
    free = {(col, height - 1 - row) for row, col in comp}
    edges: dict[tuple[int, int], tuple[int, int]] = {}

    def put(a: tuple[int, int], b: tuple[int, int]) -> None:
        if a in edges:
            # Unreachable once the pinches are gone; if it ever fires, the
            # de-pinch pass has a hole in it and the ring would be a lie.
            raise MapError(
                f"internal: lattice corner {a} has two outgoing boundary edges; "
                "remove_diagonal_pinches did not reach a fixed point"
            )
        edges[a] = b

    for u, v in free:
        if (u, v - 1) not in free:
            put((u, v), (u + 1, v))
        if (u + 1, v) not in free:
            put((u + 1, v), (u + 1, v + 1))
        if (u, v + 1) not in free:
            put((u + 1, v + 1), (u, v + 1))
        if (u - 1, v) not in free:
            put((u, v + 1), (u, v))

    rings: list[list[tuple[int, int]]] = []
    seen: set[tuple[int, int]] = set()
    for start in sorted(edges):
        if start in seen:
            continue
        ring: list[tuple[int, int]] = []
        p = start
        while p not in seen:
            seen.add(p)
            ring.append(p)
            p = edges[p]
        rings.append(_collapse_collinear(ring))

    outer: list[tuple[int, int]] | None = None
    holes: list[list[tuple[int, int]]] = []
    for ring in rings:
        if _signed_area_units(ring) > 0:
            if outer is not None:
                raise MapError(
                    "internal: one connected component produced two outer rings"
                )
            outer = ring
        else:
            holes.append(ring)
    if outer is None:
        raise MapError("internal: component produced no counter-clockwise outer ring")
    return outer, holes


def units_to_metres(
    ring: list[tuple[int, int]], meta: MapMeta
) -> list[tuple[float, float]]:
    return [
        (meta.origin_x + u * meta.resolution, meta.origin_y + v * meta.resolution)
        for u, v in ring
    ]


# --------------------------------------------------------------------------
# Simplification, and the consumer's own simplicity predicate
# --------------------------------------------------------------------------


def douglas_peucker(
    pts: list[tuple[float, float]], eps: float
) -> list[tuple[float, float]]:
    """Ramer-Douglas-Peucker on an OPEN polyline. Endpoints are always kept."""
    n = len(pts)
    if eps <= 0.0 or n < 3:
        return list(pts)
    keep = [False] * n
    keep[0] = keep[n - 1] = True
    stack = [(0, n - 1)]
    while stack:
        i, j = stack.pop()
        if j <= i + 1:
            continue
        ax, ay = pts[i]
        bx, by = pts[j]
        dx, dy = bx - ax, by - ay
        den = (dx * dx + dy * dy) ** 0.5
        best = -1.0
        idx = -1
        for k in range(i + 1, j):
            px, py = pts[k]
            if den == 0.0:
                d = ((px - ax) ** 2 + (py - ay) ** 2) ** 0.5
            else:
                d = abs(dx * (py - ay) - dy * (px - ax)) / den
            if d > best:
                best, idx = d, k
        if best > eps:
            keep[idx] = True
            stack.append((i, idx))
            stack.append((idx, j))
    return [p for p, k in zip(pts, keep) if k]


def _collapse_collinear_f(ring: list[tuple[float, float]]) -> list[tuple[float, float]]:
    n = len(ring)
    if n < 3:
        return list(ring)
    out: list[tuple[float, float]] = []
    for i in range(n):
        ax, ay = ring[i - 1]
        bx, by = ring[i]
        cx, cy = ring[(i + 1) % n]
        if (bx - ax) * (cy - by) - (by - ay) * (cx - bx) != 0.0:
            out.append((bx, by))
    return out


def simplify_ring(
    ring: list[tuple[float, float]], eps: float
) -> list[tuple[float, float]]:
    """Douglas-Peucker on a CLOSED ring.

    A ring has no endpoints, so two are manufactured: the lexicographically
    smallest vertex and the vertex farthest from it. Both are extreme points of
    the ring and both survive any tolerance, so the choice is stable under
    simplification as well as deterministic -- picking vertex 0 instead would
    make the output depend on where the tracer happened to start.
    """
    n = len(ring)
    if eps <= 0.0 or n < 4:
        return list(ring)

    i0 = min(range(n), key=lambda i: ring[i])
    best = -1.0
    i1 = i0
    for i in range(n):
        d = (ring[i][0] - ring[i0][0]) ** 2 + (ring[i][1] - ring[i0][1]) ** 2
        if d > best:
            best, i1 = d, i
    if i1 == i0:
        return list(ring)

    def arc(a: int, b: int) -> list[tuple[float, float]]:
        out = [ring[a]]
        i = a
        while i != b:
            i = (i + 1) % n
            out.append(ring[i])
        return out

    first = douglas_peucker(arc(i0, i1), eps)
    second = douglas_peucker(arc(i1, i0), eps)
    return _collapse_collinear_f(first[:-1] + second[:-1])


def on_segment(px: float, py: float, a, b) -> bool:
    """Port of internal/zone/geometry.go `onSegment`."""
    cross = (b[0] - a[0]) * (py - a[1]) - (b[1] - a[1]) * (px - a[0])
    if cross != 0:
        return False
    return (
        min(a[0], b[0]) <= px <= max(a[0], b[0])
        and min(a[1], b[1]) <= py <= max(a[1], b[1])
    )


def _orientation(p, q, r) -> float:
    """Port of internal/zone/simple_polygon.go `orientation`."""
    return (q[0] - p[0]) * (r[1] - p[1]) - (q[1] - p[1]) * (r[0] - p[0])


def segments_intersect(a1, a2, b1, b2) -> bool:
    """Port of internal/zone/simple_polygon.go `segmentsIntersect`.

    A PROPER CROSSING OR A MERE TOUCH. The four `d == 0` branches are the ones
    that matter here: a collinear endpoint touch counts, so a ring that pinches
    at a doorway is not simple even though it looks fine on paper.
    """
    d1 = _orientation(b1, b2, a1)
    d2 = _orientation(b1, b2, a2)
    d3 = _orientation(a1, a2, b1)
    d4 = _orientation(a1, a2, b2)

    if ((d1 > 0 and d2 < 0) or (d1 < 0 and d2 > 0)) and (
        (d3 > 0 and d4 < 0) or (d3 < 0 and d4 > 0)
    ):
        return True
    if d1 == 0 and on_segment(a1[0], a1[1], b1, b2):
        return True
    if d2 == 0 and on_segment(a2[0], a2[1], b1, b2):
        return True
    if d3 == 0 and on_segment(b1[0], b1[1], a1, a2):
        return True
    if d4 == 0 and on_segment(b2[0], b2[1], a1, a2):
        return True
    return False


def self_intersects(poly: list[tuple[float, float]]) -> bool:
    """Port of internal/zone/simple_polygon.go `SelfIntersects`.

    THE CONSUMER'S OWN PREDICATE, not an approximation of it. The admission
    webhook calls the Go original at internal/webhook/fleetzone_webhook.go:290;
    a tool that emits a polygon the webhook will reject has wasted the
    operator's time, and one that emits a polygon that passes by accident after
    an unrecorded tolerance change has done something worse.

    Same structure as the Go, including `edgesAdjacent` and the `n < 4` early
    return, so the two can be diffed by eye. The only addition is an
    axis-aligned bounding-box rejection before the orientation arithmetic: it
    changes no answer (segments whose boxes are disjoint cannot meet) and keeps
    the O(n^2) loop usable on a ring with a couple of thousand vertices.
    """
    n = len(poly)
    if n < 4:
        return False
    boxes = []
    for i in range(n):
        x0, y0 = poly[i]
        x1, y1 = poly[(i + 1) % n]
        boxes.append((min(x0, x1), min(y0, y1), max(x0, x1), max(y0, y1)))
    for i in range(n):
        a1, a2 = poly[i], poly[(i + 1) % n]
        alox, aloy, ahix, ahiy = boxes[i]
        for j in range(i + 1, n):
            if j == i + 1 or (i == 0 and j == n - 1):
                continue  # edgesAdjacent
            blox, bloy, bhix, bhiy = boxes[j]
            if alox > bhix or blox > ahix or aloy > bhiy or bloy > ahiy:
                continue
            if segments_intersect(a1, a2, poly[j], poly[(j + 1) % n]):
                return True
    return False


def point_in_polygon(x: float, y: float, poly: list[tuple[float, float]]) -> bool:
    """Port of internal/zone/geometry.go `PointInPolygon`.

    Even-odd ray casting, with a point exactly on an edge counted INSIDE, which
    is what makes containment deterministic on a shared boundary. Concavity is
    not a special case for this rule and never was -- which is the whole reason
    a traced ring is admissible at all, and why this tool spent two versions
    emitting convex fits for no reason a consumer required.

    Here so a test can ask the question the consumer will ask: is the point in
    the notch inside the zone? Cross-checked against the Go original.
    """
    n = len(poly)
    if n < 3:
        return False
    inside = False
    j = n - 1
    for i in range(n):
        if on_segment(x, y, poly[i], poly[j]):
            return True  # on the boundary -> treated as inside (deterministic)
        xi, yi = poly[i]
        xj, yj = poly[j]
        if (yi > y) != (yj > y):
            x_cross = (xj - xi) * (y - yi) / (yj - yi) + xi
            if x < x_cross:
                inside = not inside
        j = i
    return inside


def simplify_ring_safely(
    ring: list[tuple[float, float]], eps: float
) -> tuple[list[tuple[float, float]], float]:
    """Simplify as far as the tolerance asks, then BACK OFF until it is simple.

    Douglas-Peucker is not topology-preserving. On a concave boundary a
    tolerance large enough to be useful can make two simplified edges cross
    where the originals did not, and raising the tolerance to cut the vertex
    count raises that risk rather than lowering it. So the tolerance is a
    REQUEST: it is halved until the ring passes `self_intersects`, and the
    tolerance actually applied is returned so the manifest can record it beside
    the applied threshold, the way `--free-thresh`
    records its override.

    The traced ring itself is simple by construction (see
    `trace_component_rings`), so eps = 0 always terminates the loop.
    """
    applied = float(eps)
    while applied > 0.0:
        candidate = simplify_ring(ring, applied)
        if len(candidate) >= 3 and not self_intersects(candidate):
            return candidate, applied
        applied = applied / 2.0
        if applied < 1e-6:
            applied = 0.0
    return list(ring), 0.0


def _distance_to_ring(x: float, y: float, ring: list[tuple[float, float]]) -> float:
    """Shortest distance from a point to a closed polyline."""
    best = float("inf")
    n = len(ring)
    for i in range(n):
        ax, ay = ring[i]
        bx, by = ring[(i + 1) % n]
        dx, dy = bx - ax, by - ay
        length2 = dx * dx + dy * dy
        t = 0.0 if length2 == 0.0 else max(0.0, min(1.0, ((x - ax) * dx + (y - ay) * dy) / length2))
        best = min(best, ((x - ax - t * dx) ** 2 + (y - ay - t * dy) ** 2) ** 0.5)
    return best


def measure_clipping(
    comp: list[tuple[int, int]],
    ring: list[tuple[float, float]],
    height: int,
    meta: MapMeta,
    tolerance: float,
) -> tuple[int, float]:
    """Free cells of `comp` whose CENTRE falls outside `ring`, and how far out.

    SIMPLIFICATION CUTS BOTH WAYS, and the direction that matters is inward.
    Douglas-Peucker moves the boundary by up to the applied tolerance, and where
    it cuts a concave corner off it removes mapped floor from the zone -- so a
    robot standing on a cell this map calls free can be judged OUTSIDE the zone
    meant to contain it. That is the same failure the row-order flip produces,
    reached from a third direction, and it is the reason the manifest states a
    measured number instead of the word OVER-APPROXIMATION.

    Measured, not bounded: the cell centres that could have moved are tested
    against the ring the tool is about to write. On the three real maps that
    finds about 2% of cells clipped, by at most 0.075 m.
    """
    res = meta.resolution
    free = set(comp)

    # Only cells NEAR the traced boundary can have been clipped, so only those
    # are tested. Douglas-Peucker moves the boundary by at most `tolerance`
    # (every original vertex is within that distance of the simplified chain,
    # and distance to a line is affine along a segment, so every original point
    # is), which puts the simplified ring inside the tolerance-neighbourhood of
    # the traced one. A cell centre further than that from the traced boundary
    # is therefore on the same side of both. The restriction is an optimisation,
    # not an approximation: `test_the_clipping_count_matches_a_full_scan`
    # compares it against testing every cell.
    margin = int(tolerance / res) + 2
    candidates: set[tuple[int, int]] = set()
    for cell in comp:
        if free_degree(cell, free) == 4:
            continue  # interior: its whole neighbourhood is floor
        row, col = cell
        for drow in range(-margin, margin + 1):
            for dcol in range(-margin, margin + 1):
                near = (row + drow, col + dcol)
                if near in free:
                    candidates.add(near)

    outside: list[tuple[float, float]] = []
    for row, col in sorted(candidates):
        x, y = cell_to_metres(row, col, height, meta)
        cx, cy = x + res / 2.0, y + res / 2.0
        if not point_in_polygon(cx, cy, ring):
            outside.append((cx, cy))
    worst = max((_distance_to_ring(x, y, ring) for x, y in outside), default=0.0)
    return len(outside), worst


# --------------------------------------------------------------------------
# Which holes can a robot actually get into
# --------------------------------------------------------------------------
#
# An exclusion over a region no robot can occupy enforces nothing. It is an
# object with a name, a spec, a status and a reconcile loop, and a containment
# test that can never fire. ADR-0069 measured the object count honestly and
# declined to choose a threshold, because at the time "nothing in this tree
# measures which holes matter". The robot has since been measured:
# 170 mm across the wheels, half-width 0.085 m.
#
# AREA IS THE WRONG TEST, and ADR-0069's own table is on area. A 0.05 x 1.80 m
# slot and a 0.30 x 0.30 m blob both cover 0.09 m2; the slot admits nothing and
# the blob admits a MentorPi. What decides entry is the largest circle that
# fits inside the hole.

# Candidate circle centres per cell edge. The search is a maximisation over a
# lattice, so it can only UNDER-state the radius -- and under-stating drops a
# hole a robot fits in, which is the unsafe direction. The lattice error is
# therefore bounded and ADDED back: see `max_inscribed_radius`.
HOLE_RADIUS_SAMPLES_PER_CELL = 4


def hole_cells(ring: list[tuple[int, int]]) -> set[tuple[int, int]]:
    """The cells enclosed by one interior ring, in (u, v) lattice cell coords.

    `trace_component_rings` returns holes as rings on the CORNER lattice, and
    every edge of such a ring is a lattice edge, so the region a hole ring
    bounds is a union of whole cells. A cell is in it exactly when its centre
    -- never on a lattice line, being a half-integer -- is inside the ring.

    `len(hole_cells(r)) * resolution**2` is therefore the same number the
    signed-area formula gives for that ring, which is asserted against all
    three real captures rather than assumed.

    A free component nested INSIDE a hole counts as part of the hole here, as
    it already does in the signed area. That over-states the hole, which keeps
    it, which is the safe direction.
    """
    poly = [(float(u), float(v)) for u, v in ring]
    us = [u for u, _ in ring]
    vs = [v for _, v in ring]
    return {
        (u, v)
        for u in range(min(us), max(us))
        for v in range(min(vs), max(vs))
        if point_in_polygon(u + 0.5, v + 0.5, poly)
    }


def _segment_distance(px: float, py: float, seg: tuple[int, int, int, int]) -> float:
    ax, ay, bx, by = seg
    dx, dy = bx - ax, by - ay
    length2 = dx * dx + dy * dy
    t = 0.0 if length2 == 0.0 else max(0.0, min(1.0, ((px - ax) * dx + (py - ay) * dy) / length2))
    return ((px - ax - t * dx) ** 2 + (py - ay - t * dy) ** 2) ** 0.5


@dataclass(frozen=True)
class HoleRadius:
    """The largest circle that fits inside one hole, with its error bounded.

    `sampled_m` is the maximum over the candidate lattice, which is a LOWER
    bound on the true radius. `upper_bound_m` adds the lattice's own worst-case
    error, so it is an upper bound. The FILTER COMPARES `upper_bound_m`,
    because a hole kept in error costs one `FleetZone` object and a hole
    dropped in error is a region a robot can drive into that nothing models.
    """

    sampled_m: float
    upper_bound_m: float
    samples_per_cell: int


def max_inscribed_radius(
    cells: set[tuple[int, int]],
    resolution: float,
    *,
    samples: int = HOLE_RADIUS_SAMPLES_PER_CELL,
) -> HoleRadius:
    """Radius of the largest circle contained in a union of cell squares.

    The distance-to-boundary function f(p) = dist(p, complement) is maximised
    over candidate centres on a lattice of spacing `1/samples` cells. Two
    things make the answer trustworthy rather than merely plausible:

    * **The error is bounded, not hoped about.** The candidates are the centres
      of sub-squares that TILE the region, so the true maximiser lies within
      (1/2)*sqrt(2)/samples cells of some candidate; f is 1-Lipschitz, so the
      true radius exceeds the sampled one by at most that. `upper_bound_m` adds
      it back. At samples=4 on a 0.05 m grid that is 8.8 mm.
    * **The pruning is exact.** `depth` is the 4-connected cell distance to the
      nearest cell OUTSIDE the hole, and f(p) <= depth(cell containing p) --
      an outside cell at index offset (a, b) with |a| + |b| = depth is at most
      sqrt(a^2 + b^2) <= depth away from any point of the cell. So a cell whose
      depth is already below the best radius found cannot contain a better
      centre, and the scan stops there. Without it floor1's largest hole alone
      would be 7 808 candidates against 300-odd boundary segments.
    """
    if samples < 1:
        raise MapError(f"internal: samples per cell must be >= 1, got {samples}")
    if not cells:
        return HoleRadius(0.0, 0.0, samples)

    boundary: list[tuple[int, int, int, int]] = []
    for u, v in cells:
        if (u, v - 1) not in cells:
            boundary.append((u, v, u + 1, v))
        if (u, v + 1) not in cells:
            boundary.append((u, v + 1, u + 1, v + 1))
        if (u - 1, v) not in cells:
            boundary.append((u, v, u, v + 1))
        if (u + 1, v) not in cells:
            boundary.append((u + 1, v, u + 1, v + 1))

    depth: dict[tuple[int, int], int] = {}
    frontier = [
        c
        for c in cells
        if any(
            n not in cells
            for n in ((c[0] + 1, c[1]), (c[0] - 1, c[1]), (c[0], c[1] + 1), (c[0], c[1] - 1))
        )
    ]
    for c in frontier:
        depth[c] = 1
    d = 1
    while frontier:
        nxt = []
        for u, v in frontier:
            for n in ((u + 1, v), (u - 1, v), (u, v + 1), (u, v - 1)):
                if n in cells and n not in depth:
                    depth[n] = d + 1
                    nxt.append(n)
        frontier = nxt
        d += 1

    step = 1.0 / samples
    best = 0.0
    for c in sorted(cells, key=lambda c: -depth[c]):
        if depth[c] <= best:
            break
        u, v = c
        for i in range(samples):
            px = u + (i + 0.5) * step
            for j in range(samples):
                py = v + (j + 0.5) * step
                near = min(_segment_distance(px, py, seg) for seg in boundary)
                if near > best:
                    best = near
    slack = 2.0**0.5 / (2.0 * samples)
    return HoleRadius(best * resolution, (best + slack) * resolution, samples)


@dataclass(frozen=True)
class HoleFilter:
    """What a fleet-minimum footprint did to one component's holes.

    `half_width_m` is recorded because the answer is only valid for the
    footprint it was computed against. A SMALLER robot joining the fleet makes
    holes enterable that were correctly dropped here, and nothing re-emits this
    manifest by itself -- the annotation is what makes that detectable.
    """

    half_width_m: float
    samples_per_cell: int
    enterable: int
    enterable_area_m2: float
    dropped: int
    dropped_area_m2: float
    largest_radius_m: float


def filter_holes(
    holes: list[list[tuple[int, int]]],
    meta: MapMeta,
    *,
    half_width_m: float,
    samples: int = HOLE_RADIUS_SAMPLES_PER_CELL,
) -> HoleFilter:
    """Partition one component's holes by whether a robot fits inside them.

    A hole is ENTERABLE when the largest circle inscribed in it has radius at
    least `half_width_m` -- i.e. the robot's body fits wholly within the hole.

    **The limit this test has, stated rather than discovered later.** It asks
    whether the robot fits INSIDE the hole, not whether its centre can be over
    the hole while its body straddles the boundary. A robot half on the floor
    and half over a 5 cm sliver of never-observed cells has its reported
    position inside that sliver. No threshold above zero excludes that case, so
    this rule does not claim to; it claims that a hole no robot can occupy is
    not worth an object. See ADR-0069's amendment.
    """
    if half_width_m <= 0.0:
        raise MapError(
            f"fleet minimum half-width {half_width_m} is not positive. It is a "
            "measurement of the smallest robot in the fleet, in metres."
        )
    cell_area = meta.resolution**2
    enterable = dropped = 0
    enterable_area = dropped_area = 0.0
    largest = 0.0
    for ring in holes:
        cells = hole_cells(ring)
        radius = max_inscribed_radius(cells, meta.resolution, samples=samples)
        largest = max(largest, radius.upper_bound_m)
        area = len(cells) * cell_area
        if radius.upper_bound_m >= half_width_m:
            enterable += 1
            enterable_area += area
        else:
            dropped += 1
            dropped_area += area
    return HoleFilter(
        half_width_m=half_width_m,
        samples_per_cell=samples,
        enterable=enterable,
        enterable_area_m2=enterable_area,
        dropped=dropped,
        dropped_area_m2=dropped_area,
        largest_radius_m=largest,
    )


@dataclass(frozen=True)
class TracedRing:
    """One connected free component, as the ring that will be emitted for it."""

    polygon: list[tuple[float, float]]
    cells: int
    applied_tolerance: float
    raw_vertices: int
    holes: int
    hole_area_m2: float
    clipped_cells: int
    max_clip_m: float
    # None when the caller supplied no fleet minimum half-width. NOT a default
    # footprint standing in for one. An unverified default
    # costs -- 0.22 m carried for weeks, 2.6x the measured robot, unchecked
    # because nothing ever printed it. No footprint means no filtering, and the
    # manifest and the summary both say so.
    hole_filter: HoleFilter | None

    @property
    def area(self) -> float:
        return polygon_area(self.polygon)


@dataclass(frozen=True)
class TraceResult:
    rings: list[TracedRing]
    pinch_cells_removed: int
    dropped_components: int
    dropped_cells: int


def trace_free_space(
    cells: list[tuple[int, int]],
    height: int,
    meta: MapMeta,
    *,
    tolerance: float,
    min_component_area: float,
    fleet_min_half_width: float | None = None,
) -> TraceResult:
    """One simple ring per connected free component, largest component first.

    ONE RING PER COMPONENT, not one ring over all of them. Two free regions
    joined only through unmapped space are two places, and a single ring around
    both would declare the unmapped space between them navigable -- the same
    error as the convex fit, arrived at from a different direction.

    `min_component_area` drops the speckle a real SLAM raster is full of:
    floor1 has 261 components of which exactly one is larger than 1 m2, and
    emitting 260 sub-decimetre zones would bury the floor. Dropped area is
    counted and reported; it is free space the zones do not cover, which is the
    safe direction but still a fact the operator is owed.
    """
    kept, pinched = remove_diagonal_pinches(set(cells))
    cell_area = meta.resolution**2

    rings: list[TracedRing] = []
    dropped_components = 0
    dropped_cells = pinched
    for comp in free_components(sorted(kept)):
        if len(comp) * cell_area < min_component_area:
            dropped_components += 1
            dropped_cells += len(comp)
            continue
        outer_u, holes_u = trace_component_rings(comp, height)
        raw = units_to_metres(outer_u, meta)
        poly, applied = simplify_ring_safely(raw, tolerance)
        hole_area = sum(
            abs(_signed_area_units(h)) * cell_area for h in holes_u
        )
        holes_filtered = (
            None
            if fleet_min_half_width is None
            else filter_holes(holes_u, meta, half_width_m=fleet_min_half_width)
        )
        clipped, worst = measure_clipping(comp, poly, height, meta, applied)
        rings.append(
            TracedRing(
                polygon=poly,
                cells=len(comp),
                applied_tolerance=applied,
                raw_vertices=len(raw),
                holes=len(holes_u),
                hole_area_m2=hole_area,
                clipped_cells=clipped,
                max_clip_m=worst,
                hole_filter=holes_filtered,
            )
        )

    if not rings:
        raise MapError(
            f"no connected free component reaches --min-component-area "
            f"{min_component_area} m2. The largest is "
            f"{max((len(c) for c in free_components(sorted(kept))), default=0) * cell_area:.4f}"
            " m2. Lower the threshold, or accept that this map has no floor worth a zone."
        )
    return TraceResult(
        rings=rings,
        pinch_cells_removed=pinched,
        dropped_components=dropped_components,
        dropped_cells=dropped_cells,
    )


# --------------------------------------------------------------------------
# Manifest
# --------------------------------------------------------------------------


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


class _QuotedKey(str):
    """A mapping key that must be QUOTED in the emitted YAML.

    THE `y` KEY IS A BOOLEAN IF YOU LET IT BE. Under YAML 1.1 scalar
    resolution -- which gopkg.in/yaml.v2, and therefore sigs.k8s.io/yaml and
    therefore every Kubernetes client-side decode, still implements -- a bare
    `y` resolves to the boolean true and a bare `n` to false. So

        - x: -12.5
          y: 5.56

    converts to the JSON object {"x": -12.5, "true": 5.56}: the y value does
    not land in `Point.Y`, which keeps its zero value, and a 189-vertex ring
    collapses onto the line y = 0. Measured, not theorised: decoded that way,
    floor1's zone is rejected by the FleetZone admission webhook as
    self-intersecting -- 189 collinear vertices intersect almost everywhere --
    while the identical polygon passes `SelfIntersects` when it is decoded
    correctly. A strict decoder rejects it as an unknown field instead. Either
    way the manifest is not the polygon the tool computed.

    `config/samples/fleetzone_edge.yaml` and `config/samples/demo_a.yaml`
    already quote `"y"` for this reason and say so in a comment. This tool did
    not, which made it the one producer of FleetZone geometry in the tree that
    got it wrong.
    """


class _Dumper(yaml.SafeDumper):
    """SafeDumper plus the quoted-key representer.

    A subclass rather than a representer registered on `yaml.SafeDumper`
    itself: this module is imported by the test suite, and a global
    registration would change how every other `yaml.safe_dump` in the process
    behaves.
    """


_Dumper.add_representer(
    _QuotedKey,
    lambda dumper, data: dumper.represent_scalar(
        "tag:yaml.org,2002:str", str(data), style='"'
    ),
)


def _num(value: float) -> float:
    """Round a metre value to micrometres.

    Keeps the YAML readable and keeps float noise out of a field an operator
    reads. Well below any resolution a SLAM map carries.
    """
    return round(value + 0.0, 6)


@dataclass(frozen=True)
class EmittedZone:
    """One polygon and the free space it is accountable for.

    `free_cells` is the count the polygon COVERS, not the map's total: for
    `bbox` and `hull` those are the same number, but a traced ring answers for
    its own component only, and dividing its area by the whole map's free area
    would flatter it.
    """

    polygon: list[tuple[float, float]]
    free_cells: int
    name_suffix: str
    display_suffix: str
    extra: dict[str, str]

    def area(self) -> float:
        return polygon_area(self.polygon)

    def free_area(self, meta: MapMeta) -> float:
        return self.free_cells * meta.resolution**2

    def ratio(self, meta: MapMeta) -> float:
        free = self.free_area(meta)
        return self.area() / free if free else float("inf")


def _hole_filter_annotations(filt: HoleFilter | None, total_holes: int) -> dict[str, str]:
    """The hole-filter half of a traced ring's annotations.

    ALWAYS present, including the `none` case. An operator reading a manifest
    has to be able to tell "no footprint was supplied, so every hole is in this
    count" from "a footprint was supplied and 58 holes were dropped" -- and the
    absence of a key says neither.
    """
    if filt is None:
        return {
            f"{ANN}-fleet-min-half-width-m-applied": "none",
            f"{ANN}-hole-filter": (
                f"NOT APPLIED: no --fleet-min-half-width was supplied, so nothing was "
                f"filtered and all {total_holes} enclosed hole(s) are counted above. "
                "There is deliberately no default footprint."
            ),
        }
    return {
        f"{ANN}-fleet-min-half-width-m-applied": repr(filt.half_width_m),
        f"{ANN}-enclosed-holes-enterable": str(filt.enterable),
        f"{ANN}-enclosed-hole-area-enterable-m2": f"{filt.enterable_area_m2:.4f}",
        f"{ANN}-enclosed-holes-dropped": str(filt.dropped),
        f"{ANN}-enclosed-hole-area-dropped-m2": f"{filt.dropped_area_m2:.4f}",
        f"{ANN}-largest-hole-inscribed-radius-m": f"{filt.largest_radius_m:.4f}",
        f"{ANN}-hole-radius-samples-per-cell": str(filt.samples_per_cell),
        f"{ANN}-hole-filter": (
            f"{filt.enterable} of {total_holes} enclosed hole(s) admit a circle of "
            f"radius >= {filt.half_width_m} m and could therefore contain a robot; "
            f"{filt.dropped} ({filt.dropped_area_m2:.4f} m2) could not and are NOT "
            f"counted as needing an exclusion. The test is the maximum INSCRIBED "
            f"RADIUS, not area: a long thin slot has area and admits nothing. "
            f"{filt.half_width_m} m is a claim about the SMALLEST robot in the fleet. "
            "Add a smaller one and this manifest must be re-emitted against its "
            "half-width: holes dropped here become enterable and nothing re-emits "
            "them by itself."
        ),
    }


def build_manifest(*,
    name: str,
    namespace: str,
    mode: str,
    frame_id: str,
    yaml_path: Path,
    image_path: Path,
    meta: MapMeta,
    map_free_thresh: float,
    grid: Grid,
    n_free: int,
    counts: dict,
    zone: EmittedZone,
    max_area_ratio: float,
    shared: dict[str, str],
) -> dict:
    poly_area = zone.area()
    covered_area = zone.free_area(meta)
    free_area = n_free * meta.resolution**2

    annotations = {
        f"{ANN}-tool-version": TOOL_VERSION,
        f"{ANN}-source-map": yaml_path.name,
        f"{ANN}-source-image": image_path.name,
        f"{ANN}-source-map-sha256": sha256_file(yaml_path),
        f"{ANN}-source-image-sha256": sha256_file(image_path),
        f"{ANN}-resolution-m-per-px": repr(meta.resolution),
        f"{ANN}-origin": f"[{meta.origin_x}, {meta.origin_y}, {meta.origin_theta}]",
        f"{ANN}-grid-size-px": f"{grid.width}x{grid.height}",
        f"{ANN}-free-cells": str(n_free),
        f"{ANN}-occupied-cells": str(counts.get(Cell.OCCUPIED, 0)),
        # Recorded because it is the number that makes the free count
        # meaningful: a zone drawn from a mostly-unobserved map is a
        # claim about space no sensor has seen.
        f"{ANN}-unknown-cells": str(counts.get(Cell.UNKNOWN, 0)),
        # The thresholds ACTUALLY applied, which are not necessarily the
        # map's own -- --free-thresh overrides it, and a manifest that
        # recorded the file's value would misattribute the geometry.
        f"{ANN}-free-thresh-applied": repr(meta.free_thresh),
        f"{ANN}-occupied-thresh-applied": repr(meta.occupied_thresh),
        f"{ANN}-free-thresh-in-map": repr(map_free_thresh),
        f"{ANN}-free-area-m2": f"{free_area:.4f}",
        # What THIS polygon answers for. Equal to the totals above in the convex
        # modes; the component's own share under --mode traced.
        f"{ANN}-polygon-covers-free-cells": str(zone.free_cells),
        f"{ANN}-polygon-covers-free-area-m2": f"{covered_area:.4f}",
        f"{ANN}-polygon-area-m2": f"{poly_area:.4f}",
        f"{ANN}-polygon-vertices": str(len(zone.polygon)),
        f"{ANN}-area-ratio": f"{poly_area / covered_area:.4f}" if covered_area else "n/a",
        # The ceiling that was enforced, not merely the ratio that resulted. A
        # manifest whose ratio is 1.05 says nothing about whether anything
        # would have stopped it at 3.27.
        f"{ANN}-max-area-ratio-applied": repr(max_area_ratio),
        f"{ANN}-polygon-mode": mode,
    }
    annotations.update(shared)
    annotations.update(zone.extra)
    # NOT the word OVER-APPROXIMATION unconditionally. A traced ring that has
    # been simplified can cover LESS than its own free space -- map_01's second
    # component measures 0.99x -- and a manifest that says otherwise is making a
    # claim nobody checked.
    direction = "OVER-APPROXIMATION" if poly_area >= covered_area else "UNDER-APPROXIMATION"
    detail = (
        f"{direction}: the {mode} polygon covers {poly_area:.2f} m2, the free space "
        f"it answers for {covered_area:.2f} m2. It encloses obstacles and unmapped "
        "cells too."
    )
    clipped = zone.extra.get(f"{ANN}-clipped-free-cells")
    if clipped is not None:
        detail += (
            f" {clipped} free cells ({zone.extra[f'{ANN}-clipped-free-area-m2']} m2) fall "
            f"OUTSIDE this ring, by at most "
            f"{zone.extra[f'{ANN}-max-clip-distance-m']} m: simplification cut the "
            "corners they sit in, so a robot standing on one is judged outside this zone."
        )
    annotations[f"{ANN}-approximation"] = detail

    manifest = {
        "apiVersion": "swarmada.io/v1",
        "kind": "FleetZone",
        "metadata": {
            "name": name + zone.name_suffix,
            "namespace": namespace,
            "annotations": annotations,
        },
        "spec": {
            "displayName": (
                f"{yaml_path.stem} (mapped free space, {mode}{zone.display_suffix})"
            ),
            "physicalBounds": {
                "floor": 0,
                "frame": {
                    "id": frame_id,
                    # The polygon is in the SLAM map frame. Whether that frame
                    # coincides with the namespace coordinate system is a claim
                    # this tool cannot make and must not guess: a wrong
                    # SiteAligned yields silently wrong containment, which the
                    # API doc calls worse than supplying no geometry at all.
                    # The operator promotes it to SiteAligned once they have
                    # verified the alignment.
                    "alignment": "FrameLocal",
                    "source": f"map-to-fleetzone {TOOL_VERSION}: {yaml_path.name}",
                },
                "polygon": [
                    {_QuotedKey("x"): _num(x), _QuotedKey("y"): _num(y)}
                    for x, y in zone.polygon
                ],
            },
        },
    }
    return manifest


HEADER = """# Generated by map_to_fleetzone.py
# DO NOT hand-edit: regenerate from the map instead.
#
# One FleetZone per CONNECTED free component of the map. The polygon is still an
# over-approximation -- see the {ann}-approximation
# annotation for by how much, and {ann}-area-ratio
# for the number the --max-area-ratio ceiling was applied to. Splitting a
# component into rooms is a separate tool.
#
# Under --mode traced (the default) the ring follows the free space, so it is
# CONCAVE where the floor is. Containment (internal/zone/geometry.go
# PointInPolygon) is even-odd ray casting and handles that; the ring is checked
# against the consumer's own simplicity predicate before it is written, and the
# tolerance actually applied is recorded in the annotations.
#
# INTERIOR HOLES ARE NOT REPRESENTABLE: PhysicalBounds.Polygon is one ring, so a
# pillar inside a room is inside the zone. See {ann}-enclosed-holes.
#
# spec.bounds.frame.alignment is FrameLocal: the polygon is in the SLAM map
# frame and this tool cannot verify that frame is the namespace coordinate
# system. Promote it to SiteAligned only once you have.
"""


def emit(manifests: list[dict]) -> str:
    out = HEADER.format(ann=ANN)
    for manifest in manifests:
        body = yaml.dump(
            manifest,
            Dumper=_Dumper,
            sort_keys=False,
            default_flow_style=False,
            width=100,
        )
        out += "---\n" + body
    return out


# --------------------------------------------------------------------------


def _check_area_ratio(zone: EmittedZone, meta: MapMeta, mode: str, ceiling: float) -> None:
    """Refuse a polygon that covers more than `ceiling` times its own free space.

    THE RATIO IS A CONTROL, NOT AN ANNOTATION. Before this, the number was
    printed to stderr, recorded in the manifest, and read by nothing: `--mode
    bbox` on floor1 announced 3.27x and emitted the zone anyway. A number
    nobody acts on is documentation.

    The default ceiling is deliberately low enough that the convex modes fail
    on a floor plan that is not a rectangle -- which is not a bug in the
    ceiling, it is the finding. `bbox` and `hull` remain correct for a genuinely
    rectangular cell, where their ratio is ~1.0, and an operator who has looked
    at the map and accepts the over-approximation raises the ceiling
    explicitly and leaves that decision in the shell history.
    """
    ratio = zone.ratio(meta)
    if ratio <= ceiling:
        return
    raise MapError(
        f"--mode {mode} polygon covers {zone.area():.2f} m2 for "
        f"{zone.free_area(meta):.2f} m2 of free space: ratio {ratio:.2f}x, "
        f"above the --max-area-ratio ceiling of {ceiling:.2f}x.\n"
        f"  {zone.area() - zone.free_area(meta):.2f} m2 of wall, neighbouring room and "
        f"unmapped space would be declared navigable.\n"
        f"  Use --mode traced (the default), or raise --max-area-ratio {ratio:.2f} "
        "if you have looked at the map and accept that."
    )


def run(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="map_to_fleetzone.py",
        description="Emit FleetZone manifests enclosing a ROS map's free space.",
    )
    ap.add_argument("map_yaml", type=Path, help="path to <name>.yaml of a ROS map pair")
    ap.add_argument("--name", help="metadata.name (default: the map's stem, sanitised)")
    ap.add_argument("--namespace", default="default", help="metadata.namespace")
    ap.add_argument(
        "--mode",
        choices=("traced", "bbox", "hull"),
        default=DEFAULT_MODE,
        help=(
            "traced (default: follows the free space, concave, one ring per connected "
            "component), bbox (convex, hand-verifiable) or hull (convex, tighter)"
        ),
    )
    ap.add_argument("--frame-id", default="map", help="spec.physicalBounds.frame.id")
    ap.add_argument(
        "--free-thresh",
        type=float,
        help=(
            "override the map YAML's free_thresh. The documented escape hatch when a "
            "map was saved with a threshold that cannot separate unknown from free."
        ),
    )
    ap.add_argument(
        "--occupied-thresh",
        type=float,
        help="override the map YAML's occupied_thresh.",
    )
    ap.add_argument(
        "--simplify-tolerance",
        type=float,
        default=DEFAULT_SIMPLIFY_TOLERANCE_M,
        metavar="M",
        help=(
            f"Douglas-Peucker tolerance in metres for --mode traced "
            f"(default {DEFAULT_SIMPLIFY_TOLERANCE_M}). A REQUEST, not a promise: it is "
            "halved until the ring passes the consumer's simplicity predicate, and the "
            "value actually applied is recorded in the manifest."
        ),
    )
    ap.add_argument(
        "--max-area-ratio",
        type=float,
        default=DEFAULT_MAX_AREA_RATIO,
        metavar="R",
        help=(
            f"refuse any polygon covering more than R times the free space it answers "
            f"for (default {DEFAULT_MAX_AREA_RATIO}). The convex modes exceed this on "
            "any floor plan that is not a rectangle -- that is the point."
        ),
    )
    ap.add_argument(
        "--fleet-min-half-width",
        type=float,
        metavar="M",
        help=(
            "count only the enclosed holes a robot could be INSIDE: those whose "
            "largest inscribed circle has radius >= M metres. M is the SMALLEST "
            "half-width in the fleet, not the half-width of the robot you happen to "
            "have measured -- a smaller robot enters holes a larger one cannot. "
            "THERE IS NO DEFAULT AND THERE MUST NOT BE ONE: without this "
            "flag every hole is counted, and the manifest says that no footprint was "
            "supplied. The MentorPi measures 170 mm across the wheels, so 0.085."
        ),
    )
    ap.add_argument(
        "--min-component-area",
        type=float,
        default=DEFAULT_MIN_COMPONENT_AREA_M2,
        metavar="M2",
        help=(
            f"under --mode traced, skip connected free components smaller than this "
            f"(default {DEFAULT_MIN_COMPONENT_AREA_M2} m2). A real raster is full of "
            "single-cell speckle; the skipped area is reported."
        ),
    )
    args = ap.parse_args(argv)

    if args.simplify_tolerance < 0:
        raise MapError(f"--simplify-tolerance {args.simplify_tolerance} is negative")
    if args.fleet_min_half_width is not None:
        if args.fleet_min_half_width <= 0.0:
            raise MapError(
                f"--fleet-min-half-width {args.fleet_min_half_width} is not positive. "
                "It is a measured half-width in metres: the MentorPi's 170 mm across "
                "the wheels is 0.085."
            )
        if args.mode != "traced":
            raise MapError(
                f"--fleet-min-half-width has no meaning under --mode {args.mode}: only "
                "the traced ring has holes to filter. A flag that silently does nothing "
                "would repeat the unverified-default defect, so this refuses instead."
            )
    if args.max_area_ratio < 1.0:
        raise MapError(
            f"--max-area-ratio {args.max_area_ratio} is below 1.0, which no enclosing "
            "polygon can satisfy: a polygon that encloses N m2 of free space covers at "
            "least N m2."
        )

    yaml_path: Path = args.map_yaml.resolve()
    meta = read_map_yaml(yaml_path)
    image_path = (yaml_path.parent / meta.image).resolve()
    if not image_path.is_file():
        raise MapError(f"{yaml_path}: image '{meta.image}' not found at {image_path}")

    import dataclasses

    map_free_thresh = meta.free_thresh
    overrides = {}
    if args.free_thresh is not None:
        overrides["free_thresh"] = args.free_thresh
    if args.occupied_thresh is not None:
        overrides["occupied_thresh"] = args.occupied_thresh
    if overrides:
        meta = dataclasses.replace(meta, **overrides)

    grid = read_pgm(image_path)

    # Before any geometry: a map whose thresholds fold `unknown` into `free`
    # yields a polygon that is confidently, silently too big.
    check_thresholds_separate_unknown(grid, meta)

    counts: dict[Cell, int] = {c: 0 for c in Cell}
    for pixel in grid.pixels:
        counts[classify(pixel, grid.maxval, meta)] += 1

    cells = free_cell_indices(grid, meta)
    if not cells:
        raise MapError(
            f"{image_path}: no cell is free at free_thresh={meta.free_thresh} "
            f"negate={meta.negate}. Check the thresholds in {yaml_path.name}."
        )

    shared: dict[str, str] = {}
    zones: list[EmittedZone] = []

    if args.mode == "bbox":
        zones = [
            EmittedZone(
                polygon=bounding_box(cells, grid.height, meta),
                free_cells=len(cells),
                name_suffix="",
                display_suffix="",
                extra={},
            )
        ]
    elif args.mode == "hull":
        zones = [
            EmittedZone(
                polygon=convex_hull_of_cells(cells, grid.height, meta),
                free_cells=len(cells),
                name_suffix="",
                display_suffix="",
                extra={},
            )
        ]
    else:
        traced = trace_free_space(
            cells,
            grid.height,
            meta,
            tolerance=args.simplify_tolerance,
            min_component_area=args.min_component_area,
            fleet_min_half_width=args.fleet_min_half_width,
        )
        cell_area = meta.resolution**2
        shared = {
            f"{ANN}-simplify-tolerance-m-requested": repr(args.simplify_tolerance),
            f"{ANN}-traced-components": str(len(traced.rings)),
            f"{ANN}-pinch-cells-removed": str(traced.pinch_cells_removed),
            f"{ANN}-dropped-components": str(traced.dropped_components),
            f"{ANN}-dropped-free-cells": str(traced.dropped_cells),
            f"{ANN}-dropped-free-area-m2": f"{traced.dropped_cells * cell_area:.4f}",
            f"{ANN}-min-component-area-m2-applied": repr(args.min_component_area),
        }
        many = len(traced.rings) > 1
        for i, ring in enumerate(traced.rings, start=1):
            zones.append(
                EmittedZone(
                    polygon=ring.polygon,
                    free_cells=ring.cells,
                    name_suffix=f"-c{i}" if many else "",
                    display_suffix=f", component {i}/{len(traced.rings)}" if many else "",
                    extra={
                        f"{ANN}-component": f"{i}/{len(traced.rings)}",
                        # The tolerance that was actually APPLIED, which is not
                        # necessarily the one requested: see
                        # `simplify_ring_safely`. Recorded beside the applied
                        # threshold for the same reason -- a ring that cannot be
                        # reproduced from the map and the numbers in its own
                        # annotations is geometry with no evidence behind it.
                        f"{ANN}-simplify-tolerance-m-applied": repr(
                            ring.applied_tolerance
                        ),
                        f"{ANN}-traced-vertices-before-simplify": str(ring.raw_vertices),
                        f"{ANN}-enclosed-holes": str(ring.holes),
                        f"{ANN}-enclosed-hole-area-m2": f"{ring.hole_area_m2:.4f}",
                        # What was omitted, and on what basis. ADR-0069 makes
                        # disclosure the contract for holes that stay inside
                        # the ring; a FILTERED count without the footprint it
                        # was filtered against is a number an operator cannot
                        # check and a later, smaller robot cannot invalidate.
                        **_hole_filter_annotations(ring.hole_filter, ring.holes),
                        # Free cells this ring does NOT contain, because
                        # simplification cut the corner they sit in. The
                        # direction that can hurt someone: a robot standing on
                        # one of them is judged outside its own zone.
                        f"{ANN}-clipped-free-cells": str(ring.clipped_cells),
                        f"{ANN}-clipped-free-area-m2": (
                            f"{ring.clipped_cells * meta.resolution**2:.4f}"
                        ),
                        f"{ANN}-max-clip-distance-m": f"{ring.max_clip_m:.4f}",
                    },
                )
            )

    for z in zones:
        _check_area_ratio(z, meta, args.mode, args.max_area_ratio)
        if self_intersects(z.polygon):
            # Unreachable for a traced ring -- simplify_ring_safely has already
            # backed the tolerance off until it passed -- and a standing check
            # on the convex modes, which have never been run through it.
            raise MapError(
                f"--mode {args.mode} produced a self-intersecting polygon "
                f"({len(z.polygon)} vertices). The FleetZone admission webhook "
                "(internal/webhook/fleetzone_webhook.go) would reject it; emitting it "
                "would only move the failure."
            )

    name = args.name or "".join(
        c if (c.isalnum() or c == "-") else "-" for c in yaml_path.stem.lower()
    ).strip("-")

    manifests = [
        build_manifest(
            name=name,
            namespace=args.namespace,
            mode=args.mode,
            frame_id=args.frame_id,
            yaml_path=yaml_path,
            image_path=image_path,
            meta=meta,
            map_free_thresh=map_free_thresh,
            grid=grid,
            n_free=len(cells),
            counts=counts,
            zone=z,
            max_area_ratio=args.max_area_ratio,
            shared=shared,
        )
        for z in zones
    ]

    sys.stdout.write(emit(manifests))

    free_area = len(cells) * meta.resolution**2
    # Everything a human needs in order to disbelieve the manifest goes to
    # stderr, so a redirected stdout stays a clean, appliable manifest.
    lines = [
        f"map: {yaml_path}",
        f"grid: {grid.width}x{grid.height} px @ {meta.resolution} m/px, "
        f"origin ({meta.origin_x}, {meta.origin_y}, {meta.origin_theta})",
        f"cells: {counts[Cell.FREE]} free, {counts[Cell.OCCUPIED]} occupied, "
        f"{counts[Cell.UNKNOWN]} unknown (excluded)",
        f"free area {free_area:.2f} m2",
    ]
    if args.mode == "traced":
        lines.append(
            f"traced: {len(zones)} component(s) kept, "
            f"{shared[f'{ANN}-dropped-components']} dropped below "
            f"{args.min_component_area} m2 "
            f"({shared[f'{ANN}-dropped-free-area-m2']} m2), "
            f"{shared[f'{ANN}-pinch-cells-removed']} diagonal-pinch cells removed"
        )
    for i, z in enumerate(zones, start=1):
        tag = f"zone {i}/{len(zones)}" if len(zones) > 1 else "zone"
        eps = z.extra.get(f"{ANN}-simplify-tolerance-m-applied")
        holes_line = z.extra.get(f"{ANN}-hole-filter")
        lines.append(
            f"{tag} ({args.mode}, {len(z.polygon)} vertices"
            + (f", eps {eps} m" if eps is not None else "")
            + f") area {z.area():.2f} m2 for {z.free_area(meta):.2f} m2 free "
            f"-> ratio {z.ratio(meta):.2f}x "
            f"({'OVER' if z.area() >= z.free_area(meta) else 'UNDER'}-APPROXIMATION), "
            f"ceiling {args.max_area_ratio:.2f}x"
            + (
                f"; {z.extra[f'{ANN}-clipped-free-cells']} free cells clipped out "
                f"({z.extra[f'{ANN}-clipped-free-area-m2']} m2, max "
                f"{z.extra[f'{ANN}-max-clip-distance-m']} m)"
                if f"{ANN}-clipped-free-cells" in z.extra
                else ""
            )
        )
        if holes_line is not None:
            lines.append(
                f"  holes: {z.extra[f'{ANN}-enclosed-holes']} enclosed "
                f"({z.extra[f'{ANN}-enclosed-hole-area-m2']} m2); {holes_line}"
            )
    print("\n".join(lines), file=sys.stderr)
    return 0


def main() -> int:
    try:
        return run()
    except MapError as exc:
        print(f"map_to_fleetzone: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
