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

"""Synthesised floor-camera frames for the governed-detector demo.

Every frame is drawn by this file from a seed. No photograph, and no frame from any real
site, is involved, so the data set carries the licence of this file (Apache-2.0) and can be
regenerated bit for bit. The frames are 32x32 RGB views of a floor in one of four classes:

  clear     floor only (sometimes with tile grout lines, which look a little like a cable)
  cable     a thin dark curve lying across the floor
  ledge     a drop-off: the far side of an edge is much darker than the near side
  obstacle  a solid object standing on the floor

The classes are deliberately easy to tell apart for a person and not trivial for a small
model: floor colour, texture, lighting and grout vary, and noise is added to every frame.

    python frames.py --seed 7 --per-class 150 --out eval.npz
"""

from __future__ import annotations

import argparse
from typing import Any

import numpy as np

CLASSES = ("clear", "cable", "ledge", "obstacle")
SIZE = 32

# Seeds used by the demo. Distinct seeds give disjoint draws, so the evaluation set and the
# probe frames never overlap the training data.
TRAIN_SEED = 1001
EVAL_SEED = 2002
PROBE_SEED = 3003


def _floor(rng: np.random.Generator) -> np.ndarray[Any, Any]:
    base = rng.uniform(0.35, 0.85, size=3)
    img = np.ones((SIZE, SIZE, 3)) * base
    # Low-frequency texture (wood grain / tile mottling) plus a lighting gradient.
    yy, xx = np.mgrid[0:SIZE, 0:SIZE] / SIZE
    angle = rng.uniform(0, np.pi)
    phase = (xx * np.cos(angle) + yy * np.sin(angle)) * rng.uniform(6, 20)
    grain = np.sin(phase + rng.uniform(0, 6))
    img += (grain * rng.uniform(0.0, 0.06))[..., None]
    gx, gy = rng.uniform(-0.25, 0.25, size=2)
    img += (gx * (xx - 0.5) + gy * (yy - 0.5))[..., None]
    if rng.random() < 0.35:  # tile grout: thin straight lighter or darker lines
        step = int(rng.integers(8, 14))
        shade = rng.uniform(-0.18, 0.12)
        off = int(rng.integers(0, step))
        img[off::step, :, :] += shade
        img[:, off::step, :] += shade
    return img


def _cable(img: np.ndarray[Any, Any], rng: np.random.Generator) -> None:
    p0, p1, p2 = rng.uniform(-4, SIZE + 4, size=(3, 2))
    colour = rng.uniform(0.0, 0.25, size=3)
    width = rng.uniform(0.6, 1.4)
    t = np.linspace(0, 1, 200)[:, None]
    pts = (1 - t) ** 2 * p0 + 2 * (1 - t) * t * p1 + t**2 * p2
    yy, xx = np.mgrid[0:SIZE, 0:SIZE]
    d = np.min(np.hypot(xx[..., None] - pts[:, 0], yy[..., None] - pts[:, 1]), axis=-1)
    alpha = np.clip(width + 0.5 - d, 0, 1)[..., None]
    img[:] = img * (1 - alpha) + colour * alpha


def _ledge(img: np.ndarray[Any, Any], rng: np.random.Generator) -> None:
    yy, xx = np.mgrid[0:SIZE, 0:SIZE]
    angle = rng.uniform(-0.5, 0.5) + (np.pi / 2 if rng.random() < 0.3 else 0.0)
    offset = rng.uniform(8, 24)
    side = (np.sin(angle) * (xx - SIZE / 2) + np.cos(angle) * (yy - SIZE / 2)) + SIZE / 2 - offset
    far = side < 0 if rng.random() < 0.5 else side > 0
    img[far] = img[far] * rng.uniform(0.15, 0.4)


def _obstacle(img: np.ndarray[Any, Any], rng: np.random.Generator) -> None:
    yy, xx = np.mgrid[0:SIZE, 0:SIZE]
    cx, cy = rng.uniform(6, SIZE - 6, size=2)
    rx, ry = rng.uniform(3, 8, size=2)
    colour = rng.uniform(0.0, 1.0, size=3)
    if rng.random() < 0.5:
        mask = (np.abs(xx - cx) < rx) & (np.abs(yy - cy) < ry)
    else:
        mask = ((xx - cx) / rx) ** 2 + ((yy - cy) / ry) ** 2 < 1
    img[mask] = colour
    # A soft contact shadow on one side.
    shadow = ((np.abs(xx - cx - 1.5) < rx + 1) & (np.abs(yy - cy - 1.5) < ry + 1)) & ~mask
    img[shadow] *= 0.7


_DRAW = {"clear": None, "cable": _cable, "ledge": _ledge, "obstacle": _obstacle}


def frame(label: int, rng: np.random.Generator) -> np.ndarray[Any, Any]:
    """One HxWxC float32 frame in [0, 1] of class CLASSES[label]."""
    img = _floor(rng)
    draw = _DRAW[CLASSES[label]]
    if draw is not None:
        draw(img, rng)
    img += rng.normal(0, rng.uniform(0.02, 0.08), size=img.shape)
    return np.clip(img, 0, 1).astype(np.float32)


def make_set(seed: int, per_class: int) -> tuple[np.ndarray[Any, Any], np.ndarray[Any, Any]]:
    """(frames NCHW float32, labels int64), class-balanced, in a seed-determined order."""
    rng = np.random.default_rng(seed)
    labels = np.repeat(np.arange(len(CLASSES)), per_class)
    rng.shuffle(labels)
    frames = np.stack([frame(int(y), rng) for y in labels]).transpose(0, 3, 1, 2)
    return frames.astype(np.float32), labels.astype(np.int64)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--seed", type=int, required=True)
    ap.add_argument("--per-class", type=int, required=True)
    ap.add_argument("--out", required=True, help="output .npz (frames, labels, classes)")
    args = ap.parse_args()
    x, y = make_set(args.seed, args.per_class)
    np.savez_compressed(args.out, frames=x, labels=y, classes=np.array(CLASSES), seed=args.seed)
    print(f"{args.out}: {len(y)} frames, seed {args.seed}, classes {', '.join(CLASSES)}")


if __name__ == "__main__":
    main()
