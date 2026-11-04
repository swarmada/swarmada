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

"""The simulation adapter's optional model runtime (adapters/simulation/model_runtime.py).

Verification is exercised against an in-memory OCI registry holding an artifact and a
cosign-format signature made with a throwaway key, so every refusal is a real cryptographic or
digest failure, not a mocked one. onnxruntime is replaced by a fake session so the self-check
rules can be driven with exact outputs.

These need the optional extra (numpy, cryptography) and skip without it; `make test` stays
green on an interpreter that does not carry it.
"""

from __future__ import annotations

import base64
import hashlib
import io
import json
import sys
import types
import urllib.error

import pytest

np = pytest.importorskip("numpy")
pytest.importorskip("cryptography")

from cryptography.hazmat.primitives import hashes, serialization  # noqa: E402
from cryptography.hazmat.primitives.asymmetric import ec  # noqa: E402

from adapters.simulation import model_runtime as mr  # noqa: E402

HOST = "localhost:5001"
REPO = "swarmada/floor-hazard"
LABELS = ["clear", "cable", "ledge", "obstacle"]


def _sha(b: bytes) -> str:
    return "sha256:" + hashlib.sha256(b).hexdigest()


class _Resp(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class FakeRegistry:
    """Serves /v2/<repo>/manifests/<ref> and /v2/<repo>/blobs/<digest> from dicts."""

    def __init__(self) -> None:
        self.manifests: dict[str, bytes] = {}
        self.blobs: dict[str, bytes] = {}
        self.urls: list[str] = []

    def add_blob(self, data: bytes) -> dict:
        self.blobs[_sha(data)] = data
        return {"digest": _sha(data), "size": len(data)}

    def open(self, req, timeout=None):
        url = req.full_url
        self.urls.append(url)
        path = url.split("/v2/", 1)[1]
        repo_ref, _, ref = path.rpartition("/")
        repo, _, kind = repo_ref.rpartition("/")
        store = self.manifests if kind == "manifests" else self.blobs
        key = f"{repo}:{ref}" if kind == "manifests" else ref
        if key not in store:
            raise urllib.error.HTTPError(url, 404, "not found", {}, None)
        return _Resp(store[key])


def _artifact(reg: FakeRegistry, tag: str = "3.0.0", model: bytes = b"onnx-bytes") -> str:
    probe = io.BytesIO()
    np.savez(probe, frames=np.zeros((2, 3, 4, 4), np.float32), expected=np.array([0, 1]))
    layers = [
        dict(reg.add_blob(model), mediaType=mr.MEDIA_ONNX),
        dict(reg.add_blob(json.dumps({"labels": LABELS}).encode()), mediaType=mr.MEDIA_LABELS),
        dict(reg.add_blob(probe.getvalue()), mediaType=mr.MEDIA_SELFCHECK),
    ]
    manifest = json.dumps(
        {
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "artifactType": mr.ARTIFACT_TYPE,
            "layers": layers,
        }
    ).encode()
    reg.manifests[f"{REPO}:{tag}"] = manifest
    reg.manifests[f"{REPO}:{_sha(manifest)}"] = manifest
    return _sha(manifest)


def _sign(
    reg: FakeRegistry,
    key,
    digest: str,
    identity: str = f"{HOST}/{REPO}",
    kind: str = "cosign container image signature",
) -> str:
    payload = json.dumps(
        {
            "critical": {
                "identity": {"docker-reference": identity},
                "image": {"docker-manifest-digest": digest},
                "type": kind,
            },
            "optional": None,
        }
    ).encode()
    sig = key.sign(payload, ec.ECDSA(hashes.SHA256()))
    layer = dict(
        reg.add_blob(payload),
        mediaType="application/vnd.dev.cosign.simplesigning.v1+json",
        annotations={"dev.cosignproject.cosign/signature": base64.b64encode(sig).decode()},
    )
    tag = "sha256-" + digest.split(":")[1] + ".sig"
    reg.manifests[f"{REPO}:{tag}"] = json.dumps({"schemaVersion": 2, "layers": [layer]}).encode()
    return f"oci://{HOST}/{REPO}:{tag}"


@pytest.fixture
def keys():
    priv = ec.generate_private_key(ec.SECP256R1())
    pem = priv.public_key().public_bytes(
        serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo
    )
    return priv, mr.load_trust_root(pem)


def _verify(reg, digest, sig_ref, pub, uri=f"oci://{HOST}/{REPO}:3.0.0"):
    return mr.fetch_and_verify(uri, digest, sig_ref, pub, "demo-root", mr.Registry(reg.open))


def test_verified_artifact_is_returned_with_its_parts(keys) -> None:
    priv, pub = keys
    reg = FakeRegistry()
    digest = _artifact(reg)
    art = _verify(reg, digest, _sign(reg, priv, digest), pub)
    assert art.model == b"onnx-bytes"
    assert art.labels == LABELS
    assert art.manifest_digest == digest
    assert art.signer == "demo-root"


def test_checksum_must_equal_the_manifest_digest(keys) -> None:
    priv, pub = keys
    reg = FakeRegistry()
    digest = _artifact(reg)
    other = _artifact(reg, tag="1.0.0", model=b"another model")
    with pytest.raises(mr.ArtifactRefusedError, match="checksum mismatch") as err:
        _verify(reg, other, _sign(reg, priv, other), pub)
    # A checksum refusal must not read as a signature refusal: the controller treats the word
    # "signature" as the cue to stop pushing to the robot.
    assert "signature" not in str(err.value).lower()
    assert not isinstance(err.value, mr.SignatureRefusedError)
    del digest


def test_a_layer_that_does_not_match_its_descriptor_is_refused(keys) -> None:
    priv, pub = keys
    reg = FakeRegistry()
    digest = _artifact(reg)
    sig = _sign(reg, priv, digest)
    onnx_digest = _sha(b"onnx-bytes")
    reg.blobs[onnx_digest] = b"onnx-bytez"  # a registry serving different bytes
    with pytest.raises(mr.ArtifactRefusedError, match="does not match its manifest descriptor"):
        _verify(reg, digest, sig, pub)


@pytest.mark.parametrize(
    "case", ["missing", "wrong-key", "other-digest", "other-repo", "not-cosign", "not-oci"]
)
def test_signature_failures_refuse_as_signature_failures(keys, case) -> None:
    priv, pub = keys
    reg = FakeRegistry()
    digest = _artifact(reg)
    if case == "missing":
        ref = ""
    elif case == "wrong-key":
        ref = _sign(reg, ec.generate_private_key(ec.SECP256R1()), digest)
    elif case == "other-digest":
        other = _artifact(reg, tag="9.9.9", model=b"x")
        ref = _sign(reg, priv, other)
        # present it as the signature for our digest
        reg.manifests[f"{REPO}:sha256-{digest.split(':')[1]}.sig"] = reg.manifests[
            f"{REPO}:sha256-{other.split(':')[1]}.sig"
        ]
        ref = f"oci://{HOST}/{REPO}:sha256-{digest.split(':')[1]}.sig"
    elif case == "other-repo":
        ref = _sign(reg, priv, digest, identity=f"{HOST}/someone/else")
    elif case == "not-cosign":
        ref = _sign(reg, priv, digest, kind="something else")
    else:
        ref = "bundle:AAAA"
    with pytest.raises(mr.SignatureRefusedError) as err:
        _verify(reg, digest, ref, pub)
    assert str(err.value).startswith("SignatureVerificationFailed")


def test_the_signature_is_checked_before_any_layer_is_downloaded(keys) -> None:
    _priv, pub = keys
    reg = FakeRegistry()
    digest = _artifact(reg)
    with pytest.raises(mr.SignatureRefusedError):
        _verify(reg, digest, "", pub)
    assert not any("/blobs/" in u for u in reg.urls)


def test_not_a_model_artifact_is_refused(keys) -> None:
    priv, pub = keys
    reg = FakeRegistry()
    manifest = json.dumps(
        {
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "artifactType": "application/vnd.example.other",
            "layers": [],
        }
    ).encode()
    reg.manifests[f"{REPO}:3.0.0"] = manifest
    with pytest.raises(mr.ArtifactRefusedError, match="not a model artifact"):
        _verify(reg, _sha(manifest), _sign(reg, priv, _sha(manifest)), pub)


def test_plain_http_only_to_a_loopback_registry() -> None:
    seen = []

    def opener(req, timeout=None):
        seen.append(req.full_url)
        raise urllib.error.HTTPError(req.full_url, 404, "nf", {}, None)

    reg = mr.Registry(opener)
    for host in ("localhost:5001", "127.0.0.1:5000", "registry.example.com"):
        with pytest.raises(mr.ArtifactRefusedError):
            reg.manifest(mr.OciRef(host, "r", "1"))
    assert seen[0].startswith("http://localhost:5001/")
    assert seen[1].startswith("http://127.0.0.1:5000/")
    assert seen[2].startswith("https://registry.example.com/")


@pytest.mark.parametrize(
    "uri", ["https://x/y", "oci://host", "oci://host/Repo:1", "oci://h/r:t;rm"]
)
def test_unsupported_references_are_refused(uri) -> None:
    with pytest.raises(mr.ArtifactRefusedError):
        mr.parse_ref(uri)


# ── self-check, with onnxruntime replaced by a fake session ─────────────────────


def _fake_ort(monkeypatch, outputs):
    class Session:
        def __init__(self, model, providers=None):
            if model == b"unloadable":
                raise RuntimeError("bad model")

        def get_inputs(self):
            return [types.SimpleNamespace(name="frames")]

        def run(self, _names, feeds):
            return [outputs(feeds["frames"])]

    monkeypatch.setitem(sys.modules, "onnxruntime", types.SimpleNamespace(InferenceSession=Session))


def _art(model: bytes = b"m", expected=(0, 1)) -> mr.VerifiedArtifact:
    probe = io.BytesIO()
    np.savez(probe, frames=np.zeros((2, 3, 4, 4), np.float32), expected=np.array(expected))
    return mr.VerifiedArtifact(model, LABELS, probe.getvalue(), "sha256:" + "0" * 64, "root")


def _onehot(rows):
    out = np.zeros((len(rows), len(LABELS)), np.float32)
    out[np.arange(len(rows)), rows] = 1.0
    return out


def test_self_check_passes_on_agreeing_finite_probabilities(monkeypatch) -> None:
    _fake_ort(monkeypatch, lambda x: _onehot([0, 1]))
    m = mr.load(_art(), "floor-hazard", "3.0.0")
    run = m.run_frames(np.zeros((2, 3, 4, 4), np.float32), np.array([0, 2]))
    assert run["predictions"] == [["clear", 1.0], ["cable", 1.0]]
    assert run["correct"] == 1


@pytest.mark.parametrize(
    "outputs,match",
    [
        (lambda x: np.full((2, 4), np.nan, np.float32), "non-finite output on 2/2"),
        (lambda x: np.ones((2, 3), np.float32) / 3, "output shape"),
        (lambda x: np.ones((2, 4), np.float32), "not class probabilities"),
        (lambda x: _onehot([1, 1]), "1/2 agree"),
    ],
)
def test_self_check_refuses(monkeypatch, outputs, match) -> None:
    _fake_ort(monkeypatch, outputs)
    with pytest.raises(mr.SelfCheckError, match=match):
        mr.load(_art(), "floor-hazard", "4.0.0")


def test_a_model_that_does_not_load_fails_its_self_check(monkeypatch) -> None:
    _fake_ort(monkeypatch, lambda x: _onehot([0, 1]))
    with pytest.raises(mr.SelfCheckError, match="does not load"):
        mr.load(_art(b"unloadable"), "floor-hazard", "4.0.0")


def test_frame_files_are_read_without_pickle(tmp_path) -> None:
    path = tmp_path / "f.npz"
    np.savez(path, frames=np.array([object()], dtype=object))
    with pytest.raises(ValueError):
        mr.load_frames(str(path))
