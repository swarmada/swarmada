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
"""Split one floor into a parent FleetZone and one member zone per room.

This is a PRIVATE planning tool. It contacts no cluster, it is not `swarmctl`
surface, and it writes manifests to stdout for an operator to apply.

`map_to_fleetzone.py` answers "where is the floor?" with one ring per connected
free component. This answers the next question -- "which room is the robot in?"
-- by segmenting that component and emitting `spec.memberZones` on a parent.

THE PIPELINE (design doc section 6, and the prompt that commissioned this):

  1. distance transform over `traversable union observed-free`, never over raw
     occupancy -- unknown space is not floor;
  2. candidate portals: the narrow necks, found as the SADDLES of that transform;
  3. watershed seeded away from the portals -- seeds are the transform's
     regional maxima, which is what "away from the necks" means on a
     distance-to-wall field: a room's centre is the point furthest from its
     walls, a doorway is the local minimum between two such centres;
  4. each basin becomes a member zone, the floor becomes the parent, and the
     portal is the shared boundary.

PORTAL OWNERSHIP IS DECIDED HERE, NOT ARBITRATED LATER. `internal/zone/
geometry.go:78` (`onSegment`) counts a point ON a boundary as inside, so a robot
standing in a doorway drawn as a shared edge is inside BOTH member zones, and
`DeriveCurrentZone` falls back to a vertex-average centroid tie-break that
is neither density-invariant nor necessarily inside a concave
ring. Relying on it would make "which room?" depend on how finely each ring
happens to be sampled.

So: **each member ring is drawn to the portal's edge, and the portal cells
belong to the parent and to neither member.** A robot in a doorway is in the
floor and in no room -- which is a true statement about a robot in a doorway,
decided by geometry, rather than an answer produced by a comparison that is
known to be wrong.

WHAT THIS TOOL DOES NOT KNOW. A neck is not a door. The design doc is explicit
that step 2 is where a camera earns its place: neck detection alone merges two
rooms through a wide opening and splits one room at a narrow point between
furniture, and both errors look plausible on a greyscale map. No semantic layer
exists in this repository, so every boundary here is geometry only and every
room NAME is hand-typed by the operator. The runbook says so in those words.
"""

from __future__ import annotations

import argparse
import math
import sys
from collections import defaultdict, deque
from dataclasses import dataclass, field
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import map_to_fleetzone as m2f  # noqa: E402
import traversable as trav  # noqa: E402

TOOL_VERSION = "0.1.0"
ANN = m2f.ANN

# A neck WIDER than this is not a door, so the two basins either side of it are
# one room. 1.6 m is wider than any residential or ward doorway (a hospital bed
# door is 1.2 m) and narrower than the openings that make two spaces one room.
# It is the single control that decides the room count, which is why it is a
# flag with a default rather than a constant, and why two tests pin its
# behaviour from both sides.
DEFAULT_PORTAL_MAX_WIDTH_M = 1.6

# A basin whose peak stands less than this above the saddle that separates it
# from its neighbour is not a room; it is a bump in the distance transform. On a
# real raster every wall irregularity produces one. This is the classic
# `h`-parameter of h-maxima suppression, in metres of clearance.
DEFAULT_MIN_PROMINENCE_M = 0.25

# Basins smaller than this are merged into their widest-necked neighbour rather
# than emitted. A 0.4 m2 "room" is a corner of a real one.
DEFAULT_MIN_ROOM_AREA_M2 = 1.0

DEFAULT_SIMPLIFY_TOLERANCE_M = m2f.DEFAULT_SIMPLIFY_TOLERANCE_M

WATERSHED = -1
UNLABELLED = 0


class SegmentationError(Exception):
    """The floor could not be segmented into zones that satisfy the invariants."""


# --------------------------------------------------------------------------
# 1. Distance transform
# --------------------------------------------------------------------------


def _edt_1d(f: list[float], n: int) -> list[float]:
    """Felzenszwalb & Huttenlocher's 1-D squared-distance transform, O(n).

    Lower envelope of the parabolas y = (x - q)^2 + f[q]. Used twice -- once
    down the columns, once across the rows -- which makes the 2-D result EXACT
    Euclidean rather than the chamfer approximation a two-pass mask would give.
    That matters here because the output is not a picture: the neck width read
    off this transform is the number that decides whether two rooms are one, and
    a chamfer metric is up to 8% wrong on a diagonal, which is the difference
    between a 1.5 m opening and a 1.62 m one.
    """
    d = [0.0] * n
    v = [0] * n
    z = [0.0] * (n + 1)
    k = 0
    v[0] = 0
    z[0] = -math.inf
    z[1] = math.inf
    for q in range(1, n):
        while True:
            s = ((f[q] + q * q) - (f[v[k]] + v[k] * v[k])) / (2 * q - 2 * v[k])
            if s <= z[k]:
                k -= 1
            else:
                break
        k += 1
        v[k] = q
        z[k] = s
        z[k + 1] = math.inf
    k = 0
    for q in range(n):
        while z[k + 1] < q:
            k += 1
        d[q] = (q - v[k]) ** 2 + f[v[k]]
    return d


def distance_transform(
    free: set[tuple[int, int]], height: int, width: int
) -> dict[tuple[int, int], float]:
    """Squared distance, in CELLS, from each free cell to the nearest non-free cell.

    Cells outside the grid count as non-free: a free cell against the map edge is
    against a wall as far as a robot is concerned, and treating the edge as open
    would make the outermost cells the deepest points on the map and seed a basin
    in every one of them.

    Returns squared distances. They are compared, sorted and bucketed but never
    added, so the square root is taken once, at the point of reporting, where it
    turns into metres.
    """
    big = float((height + width) ** 2)
    grid = [[big if (r, c) in free else 0.0 for c in range(width)] for r in range(height)]
    # columns
    for c in range(width):
        col = _edt_1d([grid[r][c] for r in range(height)], height)
        for r in range(height):
            grid[r][c] = col[r]
    # rows
    for r in range(height):
        row = _edt_1d(grid[r], width)
        grid[r] = row
    return {(r, c): grid[r][c] for (r, c) in free}


def _edge_clamped(
    dist2: dict[tuple[int, int], float], height: int, width: int
) -> dict[tuple[int, int], float]:
    """Clamp each cell's distance by its distance to the grid edge.

    The separable transform above only sees non-free cells that are IN the grid.
    A map whose free space runs to the border would otherwise report the border
    cells as infinitely deep.
    """
    out = {}
    for (r, c), d2 in dist2.items():
        edge = min(r + 1, height - r, c + 1, width - c)
        out[(r, c)] = min(d2, float(edge * edge))
    return out


# --------------------------------------------------------------------------
# 2 + 3. Watershed
# --------------------------------------------------------------------------

_N8 = ((-1, -1), (-1, 0), (-1, 1), (0, -1), (0, 1), (1, -1), (1, 0), (1, 1))


@dataclass
class Watershed:
    """Basin label per cell, plus the cells the basins meet on."""

    label: dict[tuple[int, int], int]
    watershed_cells: set[tuple[int, int]]
    peak: dict[int, float] = field(default_factory=dict)

    def basins(self) -> set[int]:
        return {v for v in self.label.values() if v > 0}


def watershed_by_flooding(
    dist2: dict[tuple[int, int], float], free: set[tuple[int, int]]
) -> Watershed:
    """Vincent-Soille flooding of the INVERTED distance transform.

    Flooding an inverted field is the same thing as flooding downhill from the
    maxima, and doing it that way round means the seeds never have to be chosen:
    the first cell reached in any region IS its regional maximum, so the basins
    are seeded at the room centres -- the points furthest from every wall --
    which is what the design doc means by "seeded away from the portals".

    LEVEL BY LEVEL, not cell by cell. Processing cells individually in sorted
    order would let an arbitrary tie-break on a flat plateau open a new basin in
    the middle of one room, because a plateau's cells have no gradient to follow.
    Each distance level is therefore flooded as a unit: existing labels spread
    across the level by breadth-first search first, and only the cells no label
    reached become new basins.

    A cell that two different labels reach is a WATERSHED cell. It is not
    assigned; it is the line between rooms, and it is what becomes the portal.
    """
    levels: dict[float, list[tuple[int, int]]] = defaultdict(list)
    for cell, d2 in dist2.items():
        levels[d2].append(cell)

    label: dict[tuple[int, int], int] = {}
    shed: set[tuple[int, int]] = set()
    peak: dict[int, float] = {}
    next_label = 1

    for d2 in sorted(levels, reverse=True):
        plateau = levels[d2]
        plateau_set = set(plateau)
        queue: deque[tuple[int, int]] = deque()
        # Seed the spread from cells of this level that already touch a label.
        for cell in plateau:
            r, c = cell
            for dr, dc in _N8:
                n = (r + dr, c + dc)
                if n in label and label[n] > 0:
                    queue.append(cell)
                    break
        in_queue = set(queue)
        while queue:
            cell = queue.popleft()
            in_queue.discard(cell)
            if cell in label or cell in shed:
                continue
            r, c = cell
            found: set[int] = set()
            for dr, dc in _N8:
                n = (r + dr, c + dc)
                lab = label.get(n)
                if lab is not None and lab > 0:
                    found.add(lab)
            if len(found) == 1:
                label[cell] = next(iter(found))
            elif len(found) > 1:
                shed.add(cell)
            else:
                continue
            for dr, dc in _N8:
                n = (r + dr, c + dc)
                if n in plateau_set and n not in label and n not in shed and n not in in_queue:
                    queue.append(n)
                    in_queue.add(n)
        # Whatever this level's flood did not reach is a new regional maximum.
        for cell in plateau:
            if cell in label or cell in shed:
                continue
            lab = next_label
            next_label += 1
            peak[lab] = d2
            comp = deque([cell])
            label[cell] = lab
            while comp:
                r, c = comp.popleft()
                for dr, dc in _N8:
                    n = (r + dr, c + dc)
                    if n in plateau_set and n not in label and n not in shed:
                        label[n] = lab
                        comp.append(n)

    for cell in free:
        label.setdefault(cell, UNLABELLED)
    return Watershed(label=label, watershed_cells=shed, peak=peak)


# --------------------------------------------------------------------------
# Merging: which necks are doors
# --------------------------------------------------------------------------


class _Union:
    def __init__(self, items):
        self.parent = {i: i for i in items}

    def find(self, a):
        while self.parent[a] != a:
            self.parent[a] = self.parent[self.parent[a]]
            a = self.parent[a]
        return a

    def union(self, a, b):
        ra, rb = self.find(a), self.find(b)
        if ra != rb:
            self.parent[rb] = ra
        return ra


@dataclass(frozen=True)
class Portal:
    """A neck that survived merging: the shared boundary of two member zones."""

    basin_a: int
    basin_b: int
    neck_width_m: float
    cells: frozenset[tuple[int, int]]

    def centroid(self, height: int, meta: m2f.MapMeta) -> tuple[float, float]:
        rs = sum(r for r, _ in self.cells) / len(self.cells)
        cs = sum(c for _, c in self.cells) / len(self.cells)
        return m2f.cell_to_metres(int(round(rs)), int(round(cs)), height, meta)


def saddles(
    ws: Watershed, dist2: dict[tuple[int, int], float]
) -> dict[tuple[int, int], tuple[float, set[tuple[int, int]]]]:
    """For each adjacent basin pair, the WIDEST point of the neck between them.

    The widest point, not the narrowest: the question a portal answers is "can
    this opening be driven through as one space?", and that is decided by the
    most generous point of the opening. Taking the minimum would split a room in
    half at the tightest corner of a wide archway.
    """
    out: dict[tuple[int, int], tuple[float, set[tuple[int, int]]]] = {}
    for cell in ws.watershed_cells:
        r, c = cell
        near = {ws.label[n] for dr, dc in _N8 if (n := (r + dr, c + dc)) in ws.label}
        near = {lab for lab in near if lab > 0}
        if len(near) < 2:
            continue
        labs = sorted(near)
        for i in range(len(labs)):
            for j in range(i + 1, len(labs)):
                key = (labs[i], labs[j])
                d2 = dist2.get(cell, 0.0)
                prev = out.get(key)
                if prev is None:
                    out[key] = (d2, {cell})
                else:
                    best, cells = prev
                    cells.add(cell)
                    out[key] = (max(best, d2), cells)
    return out


def merge_basins(
    ws: Watershed,
    dist2: dict[tuple[int, int], float],
    *,
    resolution: float,
    portal_max_width_m: float,
    min_prominence_m: float,
    min_room_area_m2: float,
) -> tuple[dict[int, int], list[tuple[int, int, str, float]]]:
    """Decide which basin pairs are one room. Returns (basin -> root, log).

    THREE REASONS TO MERGE, in this order of authority:

    1. **The neck is too wide to be a door.** `2 * saddle_distance >=
       portal_max_width`. This is the rule the room count actually turns on and
       the one the tests pin from both sides.
    2. **The basin has no prominence.** Its peak stands less than
       `min_prominence` above the saddle, so it is a bump in the transform
       rather than a room -- every wall irregularity on a real raster makes one.
    3. **The basin is too small to be a room** after 1 and 2 have settled.

    Widest necks merge first, so a chain of basins collapses from its most
    obviously-open joint inwards rather than in dictionary order.
    """
    basins = ws.basins()
    uf = _Union(basins)
    log: list[tuple[int, int, str, float]] = []

    def width(d2: float) -> float:
        return 2.0 * math.sqrt(d2) * resolution

    # Rules 1 and 2, widest neck first.
    pairs = saddles(ws, dist2)
    ordered = sorted(pairs.items(), key=lambda kv: -kv[1][0])
    for (a, b), (sad2, _cells) in ordered:
        ra, rb = uf.find(a), uf.find(b)
        if ra == rb:
            continue
        w = width(sad2)
        if w >= portal_max_width_m:
            uf.union(ra, rb)
            log.append((a, b, "neck wider than a door", w))
            continue
        peak_a = max(ws.peak.get(x, 0.0) for x in basins if uf.find(x) == ra)
        peak_b = max(ws.peak.get(x, 0.0) for x in basins if uf.find(x) == rb)
        clearance = math.sqrt(sad2) * resolution
        prom = min(math.sqrt(peak_a), math.sqrt(peak_b)) * resolution - clearance
        if prom < min_prominence_m:
            uf.union(ra, rb)
            log.append((a, b, "basin has no prominence above the saddle", prom))

    # Rule 3: absorb rooms too small to be rooms, widest neck first so a
    # fragment joins the space it is most open to.
    cell_area = resolution * resolution

    def group_area(root: int) -> float:
        return sum(1 for lab in ws.label.values() if lab > 0 and uf.find(lab) == root) * cell_area

    changed = True
    while changed:
        changed = False
        roots = {uf.find(b) for b in basins}
        if len(roots) <= 1:
            break
        for root in sorted(roots):
            if group_area(root) >= min_room_area_m2:
                continue
            best_key = None
            best_w = -1.0
            for (a, b), (sad2, _cells) in pairs.items():
                ra, rb = uf.find(a), uf.find(b)
                if ra == rb:
                    continue
                if root not in (ra, rb):
                    continue
                if sad2 > best_w:
                    best_w = sad2
                    best_key = (ra, rb)
            if best_key is None:
                continue
            uf.union(*best_key)
            log.append((best_key[0], best_key[1], "room below the minimum area", group_area(root)))
            changed = True
            break

    return {b: uf.find(b) for b in basins}, log


# --------------------------------------------------------------------------
# Assembling the zones
# --------------------------------------------------------------------------


@dataclass
class Segmentation:
    """Everything the manifests and the render need, and nothing derived twice."""

    floor_cells: set[tuple[int, int]]
    members: dict[int, set[tuple[int, int]]]
    portals: list[Portal]
    portal_cells: set[tuple[int, int]]
    merge_log: list[tuple[int, int, str, float]]
    dist2: dict[tuple[int, int], float]
    resolution: float

    def member_area_m2(self, key: int) -> float:
        return len(self.members[key]) * self.resolution**2


def segment(
    free: set[tuple[int, int]],
    *,
    height: int,
    width: int,
    resolution: float,
    portal_max_width_m: float = DEFAULT_PORTAL_MAX_WIDTH_M,
    min_prominence_m: float = DEFAULT_MIN_PROMINENCE_M,
    min_room_area_m2: float = DEFAULT_MIN_ROOM_AREA_M2,
) -> Segmentation:
    """Run the whole pipeline on one connected free component.

    `free` must already be de-pinched (`remove_diagonal_pinches`): the tracer
    that turns these cells into rings requires it, and segmenting cells that the
    tracer would then refuse produces a room count that no manifest can express.
    """
    if not free:
        raise SegmentationError("no free cells to segment")

    dist2 = _edge_clamped(distance_transform(free, height, width), height, width)
    ws = watershed_by_flooding(dist2, free)
    mapping, log = merge_basins(
        ws,
        dist2,
        resolution=resolution,
        portal_max_width_m=portal_max_width_m,
        min_prominence_m=min_prominence_m,
        min_room_area_m2=min_room_area_m2,
    )

    members: dict[int, set[tuple[int, int]]] = defaultdict(set)
    for cell, lab in ws.label.items():
        if lab > 0:
            members[mapping[lab]].add(cell)

    # A watershed cell between two basins that MERGED is interior now, not a
    # portal: it is given to the room it ended up inside. Only the cells that
    # still separate two DIFFERENT members are portals, and those go to neither.
    portal_cells: set[tuple[int, int]] = set()
    for cell in ws.watershed_cells:
        r, c = cell
        roots = {
            mapping[lab]
            for dr, dc in _N8
            if (lab := ws.label.get((r + dr, c + dc), UNLABELLED)) > 0
        }
        if len(roots) > 1:
            portal_cells.add(cell)
        elif len(roots) == 1:
            members[next(iter(roots))].add(cell)
        # len(roots) == 0 is a watershed cell adrift with no labelled
        # neighbour; it stays with the parent, like a portal, because nothing
        # justifies putting it in a particular room.

    # Splitting at a portal can leave a member in two pieces (an L of corridor
    # with a door in the middle of it). A member zone is ONE ring -- FleetZone
    # carries a single polygon -- so a member that is not connected is split
    # into one zone per piece rather than emitted as a ring that spans a gap it
    # does not own.
    final: dict[int, set[tuple[int, int]]] = {}
    nxt = 1
    for key in sorted(members):
        for comp in m2f.free_components(sorted(members[key])):
            if len(comp) * resolution**2 < min_room_area_m2:
                continue
            final[nxt] = set(comp)
            nxt += 1

    if not final:
        raise SegmentationError(
            "segmentation produced no member zone above the minimum area; "
            "the floor is smaller than --min-room-area"
        )

    # PORTALS ARE DERIVED FROM THE FINAL MEMBERS, not from the pre-merge basin
    # ids. Merging and the connectivity split both renumber, and a portal
    # labelled with a basin id that no longer names a zone is a boundary an
    # operator cannot look up -- which is the whole failure this tool's
    # annotations exist to prevent.
    owner: dict[tuple[int, int], int] = {}
    for key, cells in final.items():
        for cell in cells:
            owner[cell] = key
    by_pair: dict[tuple[int, int], set[tuple[int, int]]] = defaultdict(set)
    for cell in portal_cells:
        r, c = cell
        touching = sorted({owner[n] for dr, dc in _N8 if (n := (r + dr, c + dc)) in owner})
        for i in range(len(touching)):
            for j in range(i + 1, len(touching)):
                by_pair[(touching[i], touching[j])].add(cell)
    portals = [
        Portal(
            basin_a=a,
            basin_b=b,
            neck_width_m=2.0 * math.sqrt(max(dist2.get(x, 0.0) for x in cells)) * resolution,
            cells=frozenset(cells),
        )
        for (a, b), cells in sorted(by_pair.items())
    ]

    return Segmentation(
        floor_cells=set(free),
        members=final,
        portals=portals,
        portal_cells=portal_cells,
        merge_log=log,
        dist2=dist2,
        resolution=resolution,
    )


# --------------------------------------------------------------------------
# Rings, and the invariants they must satisfy
# --------------------------------------------------------------------------


@dataclass
class Ring:
    name: str
    polygon: list[tuple[float, float]]
    cells: set[tuple[int, int]]
    tolerance_applied: float
    holes_area_m2: float


def ring_for(
    cells: set[tuple[int, int]],
    *,
    name: str,
    height: int,
    meta: m2f.MapMeta,
    tolerance: float,
) -> Ring:
    """Trace, simplify and validate one ring with map_to_fleetzone's own machinery.

    Deliberately NOT a reimplementation. `trace_component_rings` is the tracer
    that replaced the convex fits, `simplify_ring_safely` is the back-off that keeps the
    result simple, and `self_intersects` is a port of the consumer's own
    predicate (`internal/zone/geometry.go`). Anything emitted here has passed
    the same checks as anything `map_to_fleetzone.py` emits.
    """
    depinched, _removed = m2f.remove_diagonal_pinches(cells)
    comps = m2f.free_components(sorted(depinched))
    if not comps:
        raise SegmentationError(f"{name}: no cells left after de-pinching")
    comp = max(comps, key=len)
    outer, holes = m2f.trace_component_rings(comp, height)
    metric = m2f.units_to_metres(outer, meta)
    poly, applied = m2f.simplify_ring_safely(metric, tolerance)
    if m2f.self_intersects(poly):
        raise SegmentationError(
            f"{name}: ring self-intersects even at tolerance 0 -- the consumer "
            "(internal/zone/geometry.go) would refuse it"
        )
    if len(poly) < 3:
        raise SegmentationError(f"{name}: ring has {len(poly)} vertices; the CRD requires 3")
    hole_area = sum(abs(m2f.polygon_area(m2f.units_to_metres(h, meta))) for h in holes)
    return Ring(
        name=name,
        polygon=poly,
        cells=set(comp),
        tolerance_applied=applied,
        holes_area_m2=hole_area,
    )


def overlap_check(
    rings: list[Ring], *, height: int, meta: m2f.MapMeta
) -> list[tuple[str, str, int]]:
    """Every pair of member rings, checked for shared area. Empty list = clean.

    Probes each ring's own cell CENTRES against every other ring with the
    consumer's even-odd test. Cell centres rather than the reference corners:
    a corner is shared by four cells and sits exactly ON a neighbouring ring's
    edge, where `point_in_polygon`'s behaviour is the boundary case this whole
    design exists to avoid relying on.
    """
    half = meta.resolution / 2.0
    out: list[tuple[str, str, int]] = []
    for i, a in enumerate(rings):
        for j, b in enumerate(rings):
            if i >= j:
                continue
            shared = 0
            for row, col in b.cells:
                x, y = m2f.cell_to_metres(row, col, height, meta)
                if m2f.point_in_polygon(x + half, y + half, a.polygon):
                    shared += 1
            if shared:
                out.append((a.name, b.name, shared))
    return out


# --------------------------------------------------------------------------
# Manifests
# --------------------------------------------------------------------------


def _sanitise(name: str) -> str:
    out = "".join(ch if ch.isalnum() or ch == "-" else "-" for ch in name.lower())
    return out.strip("-") or "zone"


def build_manifests(*,
    parent_name: str,
    namespace: str,
    frame_id: str,
    yaml_path: Path,
    image_path: Path,
    meta: m2f.MapMeta,
    map_free_thresh: float,
    grid: m2f.Grid,
    seg: Segmentation,
    parent_ring: Ring,
    member_rings: dict[int, Ring],
    room_names: dict[int, str],
    tier_note: str,
    portal_max_width_m: float,
    driven_cells: int | None,
) -> list[dict]:
    cell_area = meta.resolution**2
    member_names = {
        key: f"{parent_name}-{_sanitise(room_names.get(key, f'room-{key}'))}"
        for key in sorted(member_rings)
    }

    shared = {
        f"{ANN}-tool": f"floor_to_member_zones.py {TOOL_VERSION}",
        f"{ANN}-source-map": yaml_path.name,
        f"{ANN}-source-image": image_path.name,
        f"{ANN}-source-map-sha256": m2f.sha256_file(yaml_path),
        f"{ANN}-source-image-sha256": m2f.sha256_file(image_path),
        f"{ANN}-resolution-m-per-px": repr(meta.resolution),
        f"{ANN}-free-thresh-applied": repr(meta.free_thresh),
        f"{ANN}-free-thresh-in-map": repr(map_free_thresh),
        f"{ANN}-portal-max-width-m": repr(portal_max_width_m),
        # The three tiers, on EVERY zone. An operator approving a room is
        # entitled to the same provenance as one approving the floor, and the
        # driven tier is the one that says whether a wheel ever tested it.
        f"{ANN}-tier-observed-free-m2": f"{len(seg.floor_cells) * cell_area:.4f}",
        f"{ANN}-tier-driven-m2": (
            "unavailable" if driven_cells is None else f"{driven_cells * cell_area:.4f}"
        ),
        f"{ANN}-tier-note": tier_note,
        # Two hazard classes that were never checked, stated on the object
        # rather than only in the runbook. AGENTS.md: do not interpret a
        # defaulted value where the true reading is unknown.
        f"{ANN}-drop-off-unverified": "true",
        f"{ANN}-semantic-layer": (
            "absent: every boundary is geometry only; room names are hand-typed"
        ),
    }

    def zone(
        name: str,
        ring: Ring,
        *,
        display: str,
        extra: dict[str, str],
        members: list[str] | None = None,
    ) -> dict:
        area = abs(m2f.polygon_area(ring.polygon))
        covered = len(ring.cells) * cell_area
        annotations = dict(shared)
        annotations.update(
            {
                f"{ANN}-polygon-area-m2": f"{area:.4f}",
                f"{ANN}-polygon-covers-free-area-m2": f"{covered:.4f}",
                f"{ANN}-polygon-vertices": str(len(ring.polygon)),
                f"{ANN}-area-ratio": f"{area / covered:.4f}" if covered else "n/a",
                f"{ANN}-simplify-tolerance-applied-m": repr(ring.tolerance_applied),
                f"{ANN}-enclosed-holes-m2": f"{ring.holes_area_m2:.4f}",
            }
        )
        annotations.update(extra)
        spec: dict = {
            "displayName": display,
            "physicalBounds": {
                "floor": 0,
                "frame": {
                    "id": frame_id,
                    # FrameLocal, for the same reason map_to_fleetzone.py uses
                    # it: this is the SLAM map frame and no one has verified it
                    # coincides with the namespace coordinate system.
                    "alignment": "FrameLocal",
                    "source": f"floor_to_member_zones {TOOL_VERSION}: {yaml_path.name}",
                },
                "polygon": [
                    {m2f._QuotedKey("x"): m2f._num(x), m2f._QuotedKey("y"): m2f._num(y)}
                    for x, y in ring.polygon
                ],
            },
        }
        if members is not None:
            spec["memberZones"] = members
        return {
            "apiVersion": "swarmada.io/v1",
            "kind": "FleetZone",
            "metadata": {"name": name, "namespace": namespace, "annotations": annotations},
            "spec": spec,
        }

    portal_area = len(seg.portal_cells) * cell_area
    members_total = sum(len(r.cells) for r in member_rings.values()) * cell_area
    parent_extra = {
        f"{ANN}-member-count": str(len(member_rings)),
        f"{ANN}-portal-count": str(len(seg.portals)),
        f"{ANN}-portal-area-m2": f"{portal_area:.4f}",
        f"{ANN}-member-area-total-m2": f"{members_total:.4f}",
        # The accounting an operator can check with a calculator: members plus
        # portals should be the floor. A gap here is unattributed floor.
        f"{ANN}-area-unaccounted-m2": (
            f"{len(seg.floor_cells) * cell_area - members_total - portal_area:.4f}"
        ),
        f"{ANN}-portal-ownership": (
            "portal cells belong to this parent and to NEITHER member: onSegment "
            "(internal/zone/geometry.go:78) counts a boundary point as inside, so a "
            "shared edge would put a robot in a doorway inside two zones and hand the "
            "answer to the centroid tie-break, which is not "
            "density-invariant. A robot in a doorway is in this floor and in no room."
        ),
    }
    manifests = [
        zone(
            parent_name,
            parent_ring,
            display=f"{yaml_path.stem} (floor)",
            extra=parent_extra,
            members=[member_names[k] for k in sorted(member_rings)],
        )
    ]

    portals_by_member: dict[int, list[Portal]] = defaultdict(list)
    for p in seg.portals:
        portals_by_member[p.basin_a].append(p)
        portals_by_member[p.basin_b].append(p)

    for key in sorted(member_rings):
        ring = member_rings[key]
        extra = {
            f"{ANN}-parent-zone": parent_name,
            f"{ANN}-room-name-source": "hand-typed by the operator (no semantic layer exists)",
        }
        # Where a boundary came from a portal, the neck that produced it, and
        # which zone is on the other side. A boundary an operator cannot
        # interrogate is a boundary they cannot approve.
        mine = portals_by_member.get(key, [])
        if mine:
            extra[f"{ANN}-portal-count"] = str(len(mine))
            extra[f"{ANN}-portal-neck-widths-m"] = ",".join(f"{p.neck_width_m:.2f}" for p in mine)
            extra[f"{ANN}-portal-neighbours"] = ",".join(
                member_names[p.basin_b if p.basin_a == key else p.basin_a] for p in mine
            )
        manifests.append(
            zone(
                member_names[key],
                ring,
                display=room_names.get(key, f"room {key}"),
                extra=extra,
            )
        )
    return manifests


HEADER = """# Generated by floor_to_member_zones.py
# DO NOT hand-edit: regenerate from the map instead.
#
# One PARENT FleetZone for the floor, one MEMBER zone per room. The parent's
# spec.memberZones names every member; no API field was added for this -- the
# field is api/v1/fleetzone_types.go:90 and predates the tool.
#
# PORTAL CELLS BELONG TO THE PARENT AND TO NEITHER MEMBER. Each member ring is
# drawn to the portal's edge and stops. onSegment (internal/zone/geometry.go:78)
# counts a point on a boundary as INSIDE, so a doorway drawn as a shared edge
# would put a robot inside both rooms and hand the answer to DeriveCurrentZone's
# centroid tie-break, which is not density-invariant and can sit
# outside a concave ring. A robot in a doorway is in the floor and in no room.
#
# EVERY BOUNDARY HERE IS GEOMETRY. A neck is not a door: no semantic layer
# exists in this repository, so a wide opening between two rooms reads as one
# room and a narrow gap between furniture reads as two. Room names are hand-typed.
#
# DROP-OFF HAZARDS WERE NEVER CHECKED. The capture platform has no cliff sensor
# and a 2D lidar reports a descending staircase as open floor.
#
# spec.physicalBounds.frame.alignment is FrameLocal: promote it to SiteAligned
# only once the SLAM frame has been verified against the namespace frame.
"""


def emit(manifests: list[dict]) -> str:
    out = HEADER
    for manifest in manifests:
        out += "---\n" + m2f.yaml.dump(
            manifest,
            Dumper=m2f._Dumper,
            sort_keys=False,
            default_flow_style=False,
            width=100,
        )
    return out


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def _load_free(args, meta, grid) -> tuple[set[tuple[int, int]], int | None, str]:
    """The free set to segment, and what it is made of."""
    observed = set(m2f.free_cell_indices(grid, meta))
    driven_cells = None
    tier_note = (
        "observed-free only: no trajectory was supplied, so the driven tier is "
        "unavailable and no cell in these zones is proven floor"
    )
    if args.trajectory is not None:
        poses = trav.read_trajectory(args.trajectory)
        driven = trav.sweep_footprint(
            poses,
            meta=meta,
            width=grid.width,
            height=grid.height,
            radius_m=args.footprint_radius,
        )
        inside = driven & observed
        driven_cells = len(inside)
        observed = observed | inside
        tier_note = (
            f"traversable union observed-free: {len(inside)} cells driven of {len(observed)} free"
        )
    return observed, driven_cells, tier_note


def run(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="floor_to_member_zones.py",
        description="Split a floor into a parent FleetZone and one member zone per room.",
    )
    ap.add_argument("map_yaml", type=Path)
    ap.add_argument("--name", help="parent metadata.name (default: the map's stem)")
    ap.add_argument("--namespace", default="default")
    ap.add_argument("--frame-id", default="map")
    ap.add_argument("--free-thresh", type=float)
    ap.add_argument(
        "--room-names",
        default="",
        help=(
            "comma-separated names for the member zones, in order. HAND-TYPED: no "
            "semantic layer exists, so nothing derives them. Short of the room count, "
            "the rest are numbered."
        ),
    )
    ap.add_argument(
        "--portal-max-width",
        type=float,
        default=DEFAULT_PORTAL_MAX_WIDTH_M,
        metavar="M",
        help=(
            f"an opening at least this wide is not a door, so the spaces either side "
            f"of it are one room (default {DEFAULT_PORTAL_MAX_WIDTH_M}). THE control "
            "that decides the room count."
        ),
    )
    ap.add_argument("--min-prominence", type=float, default=DEFAULT_MIN_PROMINENCE_M, metavar="M")
    ap.add_argument("--min-room-area", type=float, default=DEFAULT_MIN_ROOM_AREA_M2, metavar="M2")
    ap.add_argument(
        "--min-component-area",
        type=float,
        default=m2f.DEFAULT_MIN_COMPONENT_AREA_M2,
        metavar="M2",
        help="ignore free components smaller than this before segmenting (speckle)",
    )
    ap.add_argument(
        "--simplify-tolerance", type=float, default=DEFAULT_SIMPLIFY_TOLERANCE_M, metavar="M"
    )
    ap.add_argument(
        "--trajectory", type=Path, help="exported driven trajectory (see traversable.py)"
    )
    ap.add_argument(
        "--footprint-radius", type=float, default=trav.DEFAULT_FOOTPRINT_RADIUS_M, metavar="M"
    )
    ap.add_argument("--render", type=Path, help="write a labelled PNG of the segmentation here")
    ap.add_argument("-o", "--output", type=Path, help="write manifests here instead of stdout")
    args = ap.parse_args(argv)

    import dataclasses

    yaml_path: Path = args.map_yaml.resolve()
    meta = m2f.read_map_yaml(yaml_path)
    map_free_thresh = meta.free_thresh
    if args.free_thresh is not None:
        meta = dataclasses.replace(meta, free_thresh=args.free_thresh)
    image_path = (yaml_path.parent / meta.image).resolve()
    if not image_path.is_file():
        raise m2f.MapError(f"{yaml_path}: image '{meta.image}' not found at {image_path}")
    grid = m2f.read_pgm(image_path)
    m2f.check_thresholds_separate_unknown(grid, meta)

    free, driven_cells, tier_note = _load_free(args, meta, grid)

    # Speckle first, then de-pinch: the tracer requires de-pinched input, and
    # de-pinching 260 one-cell specks first would just be work.
    cell_area = meta.resolution**2
    comps = m2f.free_components(sorted(free))
    kept = [c for c in comps if len(c) * cell_area >= args.min_component_area]
    if not kept:
        raise SegmentationError(
            f"no free component reaches --min-component-area {args.min_component_area} m2"
        )
    floor = max(kept, key=len)
    skipped = len(free) - len(floor)
    depinched, pinches = m2f.remove_diagonal_pinches(set(floor))
    floor_set = max(m2f.free_components(sorted(depinched)), key=len)

    seg = segment(
        set(floor_set),
        height=grid.height,
        width=grid.width,
        resolution=meta.resolution,
        portal_max_width_m=args.portal_max_width,
        min_prominence_m=args.min_prominence,
        min_room_area_m2=args.min_room_area,
    )

    parent_name = _sanitise(args.name or yaml_path.stem)

    parent_ring = ring_for(
        seg.floor_cells,
        name=parent_name,
        height=grid.height,
        meta=meta,
        tolerance=args.simplify_tolerance,
    )
    names = [n.strip() for n in args.room_names.split(",") if n.strip()]
    room_names = {}
    member_rings: dict[int, Ring] = {}
    for i, key in enumerate(sorted(seg.members)):
        label = names[i] if i < len(names) else f"room-{i + 1}"
        room_names[key] = label
        member_rings[key] = ring_for(
            seg.members[key],
            name=f"{parent_name}-{_sanitise(label)}",
            height=grid.height,
            meta=meta,
            tolerance=args.simplify_tolerance,
        )

    overlaps = overlap_check(list(member_rings.values()), height=grid.height, meta=meta)
    if overlaps:
        detail = "; ".join(f"{a} and {b} share {n} cells" for a, b, n in overlaps)
        raise SegmentationError(
            "member rings overlap after simplification, so a robot could be inside two "
            f"rooms at once: {detail}. Lower --simplify-tolerance and re-run; the tool "
            "refuses rather than emit zones whose containment is ambiguous."
        )

    manifests = build_manifests(
        parent_name=parent_name,
        namespace=args.namespace,
        frame_id=args.frame_id,
        yaml_path=yaml_path,
        image_path=image_path,
        meta=meta,
        map_free_thresh=map_free_thresh,
        grid=grid,
        seg=seg,
        parent_ring=parent_ring,
        member_rings=member_rings,
        room_names=room_names,
        tier_note=tier_note,
        portal_max_width_m=args.portal_max_width,
        driven_cells=driven_cells,
    )

    text = emit(manifests)
    if args.output:
        args.output.write_text(text)
    else:
        sys.stdout.write(text)

    members_total = sum(len(r.cells) for r in member_rings.values()) * cell_area
    portal_area = len(seg.portal_cells) * cell_area
    print(
        f"floor {len(seg.floor_cells) * cell_area:.2f} m2 -> {len(member_rings)} member zones "
        f"({members_total:.2f} m2) + {len(seg.portals)} portals ({portal_area:.2f} m2); "
        f"unaccounted {len(seg.floor_cells) * cell_area - members_total - portal_area:.2f} m2",
        file=sys.stderr,
    )
    for p in seg.portals:
        print(f"  portal {p.basin_a}-{p.basin_b}: neck {p.neck_width_m:.2f} m", file=sys.stderr)
    if skipped:
        print(
            f"  skipped {skipped} speckle cells ({skipped * cell_area:.2f} m2) below "
            f"--min-component-area; {pinches} diagonal pinches removed",
            file=sys.stderr,
        )
    if driven_cells is None:
        print(
            "  NOTE: no trajectory supplied -- the driven tier is unavailable and these "
            "zones rest on observed-free alone.",
            file=sys.stderr,
        )

    if args.render:
        import render_zones

        render_zones.write_png(
            args.render,
            grid=grid,
            meta=meta,
            seg=seg,
            member_rings=member_rings,
            room_names=room_names,
        )
        print(f"  wrote {args.render}", file=sys.stderr)
    return 0


def main() -> int:
    try:
        return run()
    except (SegmentationError, m2f.MapError, trav.TraversableError) as exc:
        print(f"floor_to_member_zones.py: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
