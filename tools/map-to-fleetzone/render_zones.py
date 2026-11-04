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
"""Render a segmented floor to a PNG: rooms filled, outlined and labelled.

NO NEW DEPENDENCY. `map_to_fleetzone.py` reads PGM with its own parser and
imports nothing but `yaml`; this writes PNG with `zlib` and `struct` and a 5x7
bitmap font defined below. Pulling in Pillow or matplotlib to draw four hundred
rectangles would add an imaging stack to a repository that does not otherwise
have one -- AGENTS.md: no dependency without a stated reason -- and the venv the
Python gates run in has neither installed, so the render would be untestable
where the tests actually run.

The picture is the demo's visible artifact, so what it shows is a design
decision rather than a styling one:

* each member zone is filled in its own colour, so the room count is countable
  by eye without reading a manifest;
* PORTALS ARE DRAWN, in red, because a viewer who cannot see the doorways
  cannot tell a segmentation that found the rooms from one that cut the floor
  at arbitrary points;
* unknown space is NOT white. White reads as floor. It is the same grey the map
  stores it as, so the picture cannot flatter the capture.
"""

from __future__ import annotations

import struct
import zlib
from pathlib import Path

# A 5x7 font, one byte per column, low bit at the top. Enough for room labels:
# digits, uppercase, dash and space. Hand-drawn rather than imported, for the
# reason in the module docstring.
_FONT: dict[str, tuple[int, int, int, int, int]] = {
    " ": (0x00, 0x00, 0x00, 0x00, 0x00),
    "-": (0x08, 0x08, 0x08, 0x08, 0x08),
    ".": (0x00, 0x00, 0x40, 0x00, 0x00),
    "0": (0x3E, 0x51, 0x49, 0x45, 0x3E),
    "1": (0x00, 0x42, 0x7F, 0x40, 0x00),
    "2": (0x42, 0x61, 0x51, 0x49, 0x46),
    "3": (0x21, 0x41, 0x45, 0x4B, 0x31),
    "4": (0x18, 0x14, 0x12, 0x7F, 0x10),
    "5": (0x27, 0x45, 0x45, 0x45, 0x39),
    "6": (0x3C, 0x4A, 0x49, 0x49, 0x30),
    "7": (0x01, 0x71, 0x09, 0x05, 0x03),
    "8": (0x36, 0x49, 0x49, 0x49, 0x36),
    "9": (0x06, 0x49, 0x49, 0x29, 0x1E),
    "A": (0x7E, 0x11, 0x11, 0x11, 0x7E),
    "B": (0x7F, 0x49, 0x49, 0x49, 0x36),
    "C": (0x3E, 0x41, 0x41, 0x41, 0x22),
    "D": (0x7F, 0x41, 0x41, 0x22, 0x1C),
    "E": (0x7F, 0x49, 0x49, 0x49, 0x41),
    "F": (0x7F, 0x09, 0x09, 0x09, 0x01),
    "G": (0x3E, 0x41, 0x49, 0x49, 0x7A),
    "H": (0x7F, 0x08, 0x08, 0x08, 0x7F),
    "I": (0x00, 0x41, 0x7F, 0x41, 0x00),
    "J": (0x20, 0x40, 0x41, 0x3F, 0x01),
    "K": (0x7F, 0x08, 0x14, 0x22, 0x41),
    "L": (0x7F, 0x40, 0x40, 0x40, 0x40),
    "M": (0x7F, 0x02, 0x0C, 0x02, 0x7F),
    "N": (0x7F, 0x04, 0x08, 0x10, 0x7F),
    "O": (0x3E, 0x41, 0x41, 0x41, 0x3E),
    "P": (0x7F, 0x09, 0x09, 0x09, 0x06),
    "Q": (0x3E, 0x41, 0x51, 0x21, 0x5E),
    "R": (0x7F, 0x09, 0x19, 0x29, 0x46),
    "S": (0x46, 0x49, 0x49, 0x49, 0x31),
    "T": (0x01, 0x01, 0x7F, 0x01, 0x01),
    "U": (0x3F, 0x40, 0x40, 0x40, 0x3F),
    "V": (0x1F, 0x20, 0x40, 0x20, 0x1F),
    "W": (0x7F, 0x20, 0x18, 0x20, 0x7F),
    "X": (0x63, 0x14, 0x08, 0x14, 0x63),
    "Y": (0x03, 0x04, 0x78, 0x04, 0x03),
    "Z": (0x61, 0x51, 0x49, 0x45, 0x43),
}

# Distinguishable at a glance, and distinguishable in greyscale too -- a demo
# still gets printed. Deliberately not a rainbow ramp: adjacent rooms must not
# read as adjacent values of one quantity, because they are not.
_PALETTE = [
    (0x4C, 0x8B, 0xD4),
    (0xE0, 0x9B, 0x3D),
    (0x5A, 0xB5, 0x6E),
    (0xB4, 0x6A, 0xC8),
    (0xD4, 0x5B, 0x5B),
    (0x3F, 0xAE, 0xB0),
    (0x9C, 0x8B, 0x4A),
    (0x8A, 0x8A, 0xC4),
]
_UNKNOWN = (0xC8, 0xC8, 0xC8)
_OCCUPIED = (0x1A, 0x1A, 0x1A)
_PORTAL = (0xE8, 0x1A, 0x1A)
_OUTLINE = (0x10, 0x10, 0x10)
_BACKDROP = (0xF2, 0xF2, 0xEE)
_INK = (0x10, 0x10, 0x10)


class _Canvas:
    def __init__(self, width: int, height: int, fill=_BACKDROP):
        self.w = width
        self.h = height
        self.px = bytearray(bytes(fill) * (width * height))

    def set(self, x: int, y: int, rgb) -> None:
        if 0 <= x < self.w and 0 <= y < self.h:
            i = (y * self.w + x) * 3
            self.px[i : i + 3] = bytes(rgb)

    def rect(self, x: int, y: int, w: int, h: int, rgb) -> None:
        for yy in range(y, y + h):
            for xx in range(x, x + w):
                self.set(xx, yy, rgb)

    def line(self, x0: int, y0: int, x1: int, y1: int, rgb) -> None:
        """Bresenham. Used for the ring outlines, which are the only non-axis
        aligned thing on the canvas once Douglas-Peucker has cut the corners."""
        dx, dy = abs(x1 - x0), -abs(y1 - y0)
        sx = 1 if x0 < x1 else -1
        sy = 1 if y0 < y1 else -1
        err = dx + dy
        while True:
            self.set(x0, y0, rgb)
            if x0 == x1 and y0 == y1:
                return
            e2 = 2 * err
            if e2 >= dy:
                err += dy
                x0 += sx
            if e2 <= dx:
                err += dx
                y0 += sy

    def text(self, x: int, y: int, s: str, rgb=_INK, scale: int = 2) -> int:
        """Draw `s` with its top-left at (x, y). Returns the width drawn."""
        cursor = x
        for ch in s.upper():
            glyph = _FONT.get(ch, _FONT["-"])
            for col, bits in enumerate(glyph):
                for row in range(7):
                    if bits & (1 << row):
                        self.rect(cursor + col * scale, y + row * scale, scale, scale, rgb)
            cursor += 6 * scale
        return cursor - x

    def text_width(self, s: str, scale: int = 2) -> int:
        return len(s) * 6 * scale

    def png(self) -> bytes:
        raw = bytearray()
        for y in range(self.h):
            raw.append(0)  # filter type 0, and 0 for every row: the image is
            # flat colour blocks, so a predictive filter buys nothing and a
            # single filter byte keeps this writer to one obvious path.
            raw += self.px[y * self.w * 3 : (y + 1) * self.w * 3]

        def chunk(tag: bytes, data: bytes) -> bytes:
            return (
                struct.pack(">I", len(data))
                + tag
                + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
            )

        return (
            b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", self.w, self.h, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
            + chunk(b"IEND", b"")
        )


def write_png(
    path: Path,
    *,
    grid,
    meta,
    seg,
    member_rings,
    room_names,
    scale: int = 4,
    title: str | None = None,
) -> Path:
    """Render the segmentation and write it to `path`.

    `scale` is pixels per map cell. At 0.05 m/px a scale of 4 makes one metre
    80 pixels, which is enough for a 0.8 m doorway to be visibly a doorway.
    """
    import map_to_fleetzone as m2f

    margin = 16
    legend_h = 30 + 22 * (len(member_rings) + 3)
    cw, ch = grid.width * scale, grid.height * scale
    canvas = _Canvas(cw + 2 * margin, ch + 2 * margin + legend_h)

    def cell_rect(row: int, col: int, rgb) -> None:
        canvas.rect(margin + col * scale, margin + row * scale, scale, scale, rgb)

    # Base raster: unknown grey, occupied near-black. Free space that belongs to
    # no member (speckle below the component floor) stays grey deliberately --
    # it is not part of any zone and must not look as though it is.
    for row in range(grid.height):
        for col in range(grid.width):
            cls = m2f.classify(grid.at(row, col), grid.maxval, meta)
            if cls is m2f.Cell.OCCUPIED:
                cell_rect(row, col, _OCCUPIED)
            else:
                cell_rect(row, col, _UNKNOWN)

    colours: dict[int, tuple[int, int, int]] = {}
    for i, key in enumerate(sorted(member_rings)):
        colours[key] = _PALETTE[i % len(_PALETTE)]
        for row, col in member_rings[key].cells:
            cell_rect(row, col, colours[key])

    # Portals last, so they are never painted over by a room.
    for row, col in seg.portal_cells:
        cell_rect(row, col, _PORTAL)

    def to_px(x: float, y: float) -> tuple[int, int]:
        col = (x - meta.origin_x) / meta.resolution
        row = (grid.height - 1) - (y - meta.origin_y) / meta.resolution
        return margin + int(round(col * scale)), margin + int(round((row + 1) * scale))

    for key in sorted(member_rings):
        poly = member_rings[key].polygon
        for i in range(len(poly)):
            x0, y0 = to_px(*poly[i])
            x1, y1 = to_px(*poly[(i + 1) % len(poly)])
            canvas.line(x0, y0, x1, y1, _OUTLINE)

    # Labels sit at each room's own cell centroid, not its polygon centroid: a
    # concave room's polygon centroid can fall outside it (the same fact that makes
    # the control plane's centroid tie-break unsafe to rely on), which would print a
    # room's name in a different room.
    for key in sorted(member_rings):
        cells = member_rings[key].cells
        rr = sum(r for r, _ in cells) / len(cells)
        cc = sum(c for _, c in cells) / len(cells)
        label = room_names.get(key, f"ROOM-{key}").upper()
        px = margin + int(cc * scale) - canvas.text_width(label) // 2
        py = margin + int(rr * scale) - 7
        canvas.rect(px - 4, py - 4, canvas.text_width(label) + 8, 22, (0xFF, 0xFF, 0xFF))
        canvas.text(px, py, label, _INK, scale=2)

    y = margin + ch + 14
    if title:
        canvas.text(margin, y, title[:60], _INK, scale=2)
        y += 24
    cell_area = meta.resolution**2
    canvas.text(
        margin,
        y,
        f"PARENT FLOOR {len(seg.floor_cells) * cell_area:.1f} M2 - "
        f"{len(member_rings)} MEMBER ZONES - {len(seg.portals)} PORTALS",
        _INK,
        scale=2,
    )
    y += 24
    for key in sorted(member_rings):
        canvas.rect(margin, y, 16, 14, colours[key])
        canvas.rect(margin, y, 16, 1, _OUTLINE)
        area = len(member_rings[key].cells) * cell_area
        canvas.text(
            margin + 24,
            y,
            f"{room_names.get(key, f'ROOM-{key}')}  {area:.1f} M2",
            _INK,
            scale=2,
        )
        y += 22
    canvas.rect(margin, y, 16, 14, _PORTAL)
    canvas.text(
        margin + 24,
        y,
        "PORTAL - PARENT ONLY, IN NO ROOM - "
        + " ".join(f"{p.neck_width_m:.2f}M" for p in seg.portals),
        _INK,
        scale=2,
    )
    y += 22
    canvas.text(
        margin,
        y,
        "GEOMETRY ONLY - NAMES HAND-TYPED - DROP-OFF UNVERIFIED",
        (0x90, 0x20, 0x20),
        scale=2,
    )

    path.write_bytes(canvas.png())
    return path
