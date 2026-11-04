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

"""Train and export the four versions of the demo's floor-hazard classifier.

The model is a small convolutional network (per-frame standardisation, two 3x3 conv + ReLU +
2x2 max-pool stages, one dense layer, softmax over 4 classes) trained from scratch with numpy
on frames from frames.py, and exported to ONNX. Its weights are produced here and carry
this file's licence (Apache-2.0); no pretrained weights are used.

Four versions, each for a reason the demo needs:

  1.0.0  baseline
  2.0.0  worse: trained on data whose cable labels were mostly written as "clear", so it
         misses cables. The ModelPolicy is expected to block it.
  3.0.0  better: more filters, trained on more data for longer.
  4.0.0  broken: 3.0.0 exported with a corrupted output bias. It loads, and every output
         is NaN, so the adapter's self-check refuses to run it.

Each version directory holds model.onnx, labels.json and selfcheck.npz: known-answer probe
frames with the answers the exporter's own model gave them. The adapter runs those probes
after loading and before it switches.

Needs numpy and onnx (build only; the adapter needs neither onnx nor this file):

    python train.py --out build/
"""

from __future__ import annotations

import argparse
import json
import pathlib
from dataclasses import dataclass
from typing import Any

import numpy as np
from frames import CLASSES, PROBE_SEED, TRAIN_SEED, make_set

MODEL_NAME = "floor-hazard"


@dataclass(frozen=True)
class Recipe:
    version: str
    channels: tuple[int, int]
    per_class: int
    epochs: int
    flip_cable_to_clear: float = 0.0  # fraction of cable labels mislabelled as clear
    augment: bool = False
    break_output: bool = False
    note: str = ""


RECIPES = (
    Recipe("1.0.0", channels=(4, 8), per_class=250, epochs=10, note="baseline"),
    Recipe(
        "2.0.0",
        channels=(4, 8),
        per_class=250,
        epochs=10,
        flip_cable_to_clear=0.7,
        note="training labels for 'cable' were mostly written as 'clear'",
    ),
    Recipe(
        "3.0.0",
        channels=(8, 16),
        per_class=1000,
        epochs=25,
        augment=True,
        note="more filters, more data, flip augmentation",
    ),
    Recipe(
        "4.0.0",
        channels=(8, 16),
        per_class=1000,
        epochs=25,
        augment=True,
        break_output=True,
        note="3.0.0 recipe exported with a NaN output bias: loads, fails its self-check",
    ),
)

Params = dict[str, np.ndarray[Any, Any]]


def _softmax(z: np.ndarray[Any, Any]) -> np.ndarray[Any, Any]:
    z = z - z.max(axis=1, keepdims=True)
    e = np.exp(z)
    return e / e.sum(axis=1, keepdims=True)


def _standardise(x: np.ndarray[Any, Any]) -> np.ndarray[Any, Any]:
    """Per-frame zero mean, unit variance. The exported graph does the same (see export)."""
    mean = x.mean(axis=(1, 2, 3), keepdims=True)
    var = ((x - mean) ** 2).mean(axis=(1, 2, 3), keepdims=True)
    return (x - mean) / np.sqrt(var + _EPS)


_EPS = 1e-3


def _conv(x: np.ndarray[Any, Any], w: np.ndarray[Any, Any], b: np.ndarray[Any, Any]):
    """3x3, stride 1, pad 1 cross-correlation (ONNX Conv). Returns (out, cols) for backward."""
    n, c, h, wd = x.shape
    xp = np.pad(x, ((0, 0), (0, 0), (1, 1), (1, 1)))
    win = np.lib.stride_tricks.sliding_window_view(xp, (3, 3), axis=(2, 3))  # n,c,h,w,3,3
    cols = win.transpose(0, 2, 3, 1, 4, 5).reshape(n, h * wd, c * 9)
    out = cols @ w.reshape(len(w), -1).T + b
    return out.reshape(n, h, wd, len(w)).transpose(0, 3, 1, 2), cols


def _conv_back(dout, cols, w, x_shape):
    n, c, h, wd = x_shape
    f = len(w)
    d = dout.transpose(0, 2, 3, 1).reshape(n, h * wd, f)
    dw = np.einsum("npf,npk->fk", d, cols).reshape(w.shape)
    db = d.sum(axis=(0, 1))
    dcols = (d @ w.reshape(f, -1)).reshape(n, h, wd, c, 3, 3)
    dxp = np.zeros((n, c, h + 2, wd + 2), np.float32)
    for i in range(3):
        for j in range(3):
            dxp[:, :, i : i + h, j : j + wd] += dcols[..., i, j].transpose(0, 3, 1, 2)
    return dxp[:, :, 1:-1, 1:-1], dw, db


def _pool(x):
    n, c, h, w = x.shape
    r = x.reshape(n, c, h // 2, 2, w // 2, 2)
    out = r.max(axis=(3, 5))
    mask = r == out[:, :, :, None, :, None]
    return out, mask


def _pool_back(dout, mask):
    n, c, h2, _, w2, _ = mask.shape
    d = mask * dout[:, :, :, None, :, None]
    return d.reshape(n, c, h2 * 2, w2 * 2)


def forward(p: Params, x: np.ndarray[Any, Any]):
    """Class probabilities, plus the activations backward() needs."""
    x = _standardise(x)
    a1, c1 = _conv(x, p["c1w"], p["c1b"])
    r1 = np.maximum(a1, 0)
    p1, m1 = _pool(r1)
    a2, c2 = _conv(p1, p["c2w"], p["c2b"])
    r2 = np.maximum(a2, 0)
    p2, m2 = _pool(r2)
    flat = p2.reshape(len(x), -1)
    probs = _softmax(flat @ p["fw"] + p["fb"])
    return probs, (x, c1, r1, m1, p1, c2, r2, m2, p2, flat)


def backward(p: Params, cache, g: np.ndarray[Any, Any]) -> Params:
    x, c1, r1, m1, p1, c2, r2, m2, p2, flat = cache
    grads = {"fw": flat.T @ g, "fb": g.sum(0)}
    d = (g @ p["fw"].T).reshape(p2.shape)
    d = _pool_back(d, m2) * (r2 > 0)
    d, grads["c2w"], grads["c2b"] = _conv_back(d, c2, p["c2w"], p1.shape)
    d = _pool_back(d, m1) * (r1 > 0)
    _, grads["c1w"], grads["c1b"] = _conv_back(d, c1, p["c1w"], x.shape)
    return grads


def train(r: Recipe, seed: int) -> Params:
    x, y = make_set(TRAIN_SEED, r.per_class)
    y = y.copy()
    rng = np.random.default_rng(seed)
    if r.flip_cable_to_clear:
        cable = np.flatnonzero(y == CLASSES.index("cable"))
        flip = rng.choice(cable, size=int(len(cable) * r.flip_cable_to_clear), replace=False)
        y[flip] = CLASSES.index("clear")
    f1, f2 = r.channels
    flat_n = f2 * 8 * 8

    def he(*shape: int, fan_in: int) -> np.ndarray[Any, Any]:
        return rng.normal(0, np.sqrt(2 / fan_in), size=shape).astype(np.float32)

    p: Params = {
        "c1w": he(f1, 3, 3, 3, fan_in=27),
        "c1b": np.zeros(f1, np.float32),
        "c2w": he(f2, f1, 3, 3, fan_in=f1 * 9),
        "c2b": np.zeros(f2, np.float32),
        "fw": he(flat_n, len(CLASSES), fan_in=flat_n),
        "fb": np.zeros(len(CLASSES), np.float32),
    }
    m = {k: np.zeros_like(v) for k, v in p.items()}
    v = {k: np.zeros_like(val) for k, val in p.items()}
    lr, beta1, beta2, eps, batch, step = 2e-3, 0.9, 0.999, 1e-8, 32, 0
    for _epoch in range(r.epochs):
        order = rng.permutation(len(y))
        for i in range(0, len(y), batch):
            idx = order[i : i + batch]
            xb = x[idx]
            if r.augment:
                flip_h = rng.random(len(idx)) < 0.5
                xb = np.where(flip_h[:, None, None, None], xb[..., ::-1], xb)
                flip_v = rng.random(len(idx)) < 0.5
                xb = np.where(flip_v[:, None, None, None], xb[:, :, ::-1, :], xb)
            probs, cache = forward(p, xb)
            g = probs.copy()
            g[np.arange(len(idx)), y[idx]] -= 1
            g /= len(idx)
            step += 1
            for k, grad in backward(p, cache, g.astype(np.float32)).items():
                m[k] = beta1 * m[k] + (1 - beta1) * grad
                v[k] = beta2 * v[k] + (1 - beta2) * grad * grad
                mh = m[k] / (1 - beta1**step)
                vh = v[k] / (1 - beta2**step)
                p[k] = (p[k] - lr * mh / (np.sqrt(vh) + eps)).astype(np.float32)
    return p


def export(p: Params, r: Recipe, path: pathlib.Path) -> None:
    import onnx
    from onnx import TensorProto, helper, numpy_helper

    fb = p["fb"].copy()
    if r.break_output:
        fb[:] = np.nan
    arrays = dict(p, fb=fb, eps=np.array(_EPS, np.float32))
    inits = [numpy_helper.from_array(a, n) for n, a in arrays.items()]
    axes = [1, 2, 3]
    nodes = [
        helper.make_node("ReduceMean", ["frames"], ["mean"], axes=axes, keepdims=1),
        helper.make_node("Sub", ["frames", "mean"], ["centred"]),
        helper.make_node("Mul", ["centred", "centred"], ["sq"]),
        helper.make_node("ReduceMean", ["sq"], ["var"], axes=axes, keepdims=1),
        helper.make_node("Add", ["var", "eps"], ["var_eps"]),
        helper.make_node("Sqrt", ["var_eps"], ["std"]),
        helper.make_node("Div", ["centred", "std"], ["x"]),
        helper.make_node("Conv", ["x", "c1w", "c1b"], ["a1"], pads=[1, 1, 1, 1]),
        helper.make_node("Relu", ["a1"], ["r1"]),
        helper.make_node("MaxPool", ["r1"], ["p1"], kernel_shape=[2, 2], strides=[2, 2]),
        helper.make_node("Conv", ["p1", "c2w", "c2b"], ["a2"], pads=[1, 1, 1, 1]),
        helper.make_node("Relu", ["a2"], ["r2"]),
        helper.make_node("MaxPool", ["r2"], ["p2"], kernel_shape=[2, 2], strides=[2, 2]),
        helper.make_node("Flatten", ["p2"], ["flat"], axis=1),
        helper.make_node("Gemm", ["flat", "fw", "fb"], ["logits"]),
        helper.make_node("Softmax", ["logits"], ["probs"], axis=1),
    ]
    graph = helper.make_graph(
        nodes,
        f"{MODEL_NAME}-{r.version}",
        [helper.make_tensor_value_info("frames", TensorProto.FLOAT, ["n", 3, 32, 32])],
        [helper.make_tensor_value_info("probs", TensorProto.FLOAT, ["n", len(CLASSES)])],
        inits,
    )
    model = helper.make_model(
        graph,
        producer_name="swarmada-governed-detector",
        opset_imports=[helper.make_opsetid("", 17)],
    )
    model.ir_version = 8
    for k, val in (("name", MODEL_NAME), ("version", r.version), ("license", "Apache-2.0")):
        model.metadata_props.add(key=k, value=val)
    onnx.checker.check_model(model)
    path.write_bytes(model.SerializeToString())


def main() -> None:
    ap = argparse.ArgumentParser(description="Train and export the demo classifier versions")
    ap.add_argument("--out", required=True, help="build directory; one subdirectory per version")
    ap.add_argument("--only", default="", help="comma-separated versions to build (default all)")
    args = ap.parse_args()
    out = pathlib.Path(args.out)
    wanted = set(filter(None, args.only.split(",")))
    probes_x, _ = make_set(PROBE_SEED, 2)
    for r in RECIPES:
        if wanted and r.version not in wanted:
            continue
        d = out / r.version
        d.mkdir(parents=True, exist_ok=True)
        p = train(r, seed=int(r.version.split(".")[0]) if not r.break_output else 3)
        export(p, r, d / "model.onnx")
        (d / "labels.json").write_text(json.dumps({"labels": list(CLASSES)}) + "\n")
        # Known answers are what the exporter's own, unbroken weights say about the probes. A
        # broken export therefore disagrees with its own self-check, which is the point.
        probs, _ = forward(p, probes_x)
        np.savez_compressed(d / "selfcheck.npz", frames=probes_x, expected=probs.argmax(1))
        print(f"{MODEL_NAME} {r.version}: {r.note} -> {d}")


if __name__ == "__main__":
    main()
