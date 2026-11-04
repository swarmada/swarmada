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

"""Real model artifacts for the simulation adapter: fetch, verify, load, self-check, run.

OPTIONAL. The reference adapter imports this module only when it is started with
``--model-trust-root`` and a model_update arrives. Without that flag it keeps its dependency-free
stand-in verification, and nothing here is imported. The extra is installed with

    pip install -r adapters/simulation/requirements-model-runtime.txt

(onnxruntime, numpy and cryptography). A missing extra is a refusal, never a silent fallback to
the stand-in: see ``require_runtime``.

What "verified" means here, in the order it is checked (RFC-0001 C7.2, fail closed at each step):

1. The manifest named by ``model_uri`` is fetched, and the SHA-256 of its exact bytes must equal
   ``model_checksum``. The checksum therefore pins every byte of the artifact, transitively.
2. It must be a Swarmada model artifact (``artifactType``) with exactly one ONNX model, one label
   file and one self-check file, and each layer's bytes must hash to the digest the manifest
   declares for it.
3. ``model_signature_ref`` must name a cosign signature (``oci://<host>/<repo>:sha256-<hex>.sig``)
   whose simple-signing payload verifies against the pinned trust root, claims the manifest
   digest from step 1, and names the same repository as ``model_uri``. The key is the one
   SwarmadaConfig.spec.signing.trustRoots pins, provisioned to the adapter host: the control
   plane sends no trust root on the wire.

Only then is the model loaded, and before it is switched in it must pass the self-check that
travels in the artifact: known-answer probe frames, whose outputs must be finite, well formed
probabilities over the declared labels, and agree with the answers recorded at export time.

Transport: HTTPS, except to a loopback registry (the demo reaches its in-cluster registry through
``kubectl port-forward``). Integrity never rests on the transport; it rests on steps 1-3.
"""

from __future__ import annotations

import base64
import dataclasses
import hashlib
import io
import ipaddress
import json
import re
import urllib.error
import urllib.request
from typing import Any

ARTIFACT_TYPE = "application/vnd.swarmada.model.v1"
MEDIA_ONNX = "application/vnd.swarmada.model.onnx"
MEDIA_LABELS = "application/vnd.swarmada.model.labels+json"
MEDIA_SELFCHECK = "application/vnd.swarmada.model.selfcheck+npz"
_MANIFEST_TYPES = ("application/vnd.oci.image.manifest.v1+json",)
_COSIGN_LAYER = "application/vnd.dev.cosign.simplesigning.v1+json"
_COSIGN_SIG_ANNOTATION = "dev.cosignproject.cosign/signature"
_COSIGN_TYPE = "cosign container image signature"

MAX_MANIFEST_BYTES = 1 << 20
MAX_BLOB_BYTES = 64 << 20
_TIMEOUT_S = 10.0

_CHECKSUM_RE = re.compile(r"sha256:[a-f0-9]{64}")
_URI_RE = re.compile(
    r"oci://(?P<host>[A-Za-z0-9.\-]+(?::\d+)?|\[[0-9a-fA-F:]+\](?::\d+)?)"
    r"/(?P<repo>[a-z0-9]+(?:[._\-/][a-z0-9]+)*)"
    r"(?::(?P<tag>[A-Za-z0-9_][A-Za-z0-9._\-]{0,127})|@(?P<digest>sha256:[a-f0-9]{64}))$"
)


class ArtifactRefusedError(Exception):
    """The artifact could not be proved good. The adapter declines the push with this reason."""


class SignatureRefusedError(ArtifactRefusedError):
    """A refusal attributable to signature verification.

    The message starts with ``SignatureVerificationFailed``, the reason RFC-0001's compliance
    checklist names; the ModelRollout controller recognises a signature refusal by that word and
    records the robot failed instead of retrying the push.
    """

    def __init__(self, why: str) -> None:
        super().__init__("SignatureVerificationFailed: " + why)


class SelfCheckError(Exception):
    """The model loaded but did not pass its self-check, so it must not be switched in."""


class RuntimeUnavailableError(ArtifactRefusedError):
    """The optional extra is not installed, so nothing can be verified or run."""


def require_runtime() -> None:
    """Import the optional extra, or refuse. Never falls back to a weaker check."""
    missing = []
    for mod in ("numpy", "onnxruntime", "cryptography"):
        try:
            __import__(mod)
        except ImportError:
            missing.append(mod)
    if missing:
        raise RuntimeUnavailableError(
            "model runtime unavailable (missing " + ", ".join(missing) + "); install "
            "adapters/simulation/requirements-model-runtime.txt"
        )


# ── references ──────────────────────────────────────────────────────────────────


@dataclasses.dataclass(frozen=True)
class OciRef:
    host: str
    repo: str
    reference: str  # a tag or a sha256 digest

    @property
    def repository(self) -> str:
        return f"{self.host}/{self.repo}"


def parse_ref(uri: str) -> OciRef:
    m = _URI_RE.match(uri or "")
    if not m:
        raise ArtifactRefusedError(
            f"unsupported model reference {uri!r} (want oci://host/repo:tag)"
        )
    return OciRef(m["host"], m["repo"], m["tag"] or m["digest"])


def _is_loopback(host: str) -> bool:
    name = host.rsplit(":", 1)[0] if not host.startswith("[") else host[1 : host.index("]")]
    if name == "localhost":
        return True
    try:
        return ipaddress.ip_address(name).is_loopback
    except ValueError:
        return False


# ── registry client (OCI distribution, read only) ───────────────────────────────


class Registry:
    """Minimal read-only OCI distribution client: GET manifest, GET blob. Anonymous only."""

    def __init__(self, opener: Any = None) -> None:
        self._open = opener or urllib.request.urlopen

    def _get(self, host: str, path: str, accept: str, limit: int) -> bytes:
        scheme = "http" if _is_loopback(host) else "https"
        req = urllib.request.Request(f"{scheme}://{host}/v2/{path}", headers={"Accept": accept})
        try:
            with self._open(req, timeout=_TIMEOUT_S) as resp:
                data = resp.read(limit + 1)
        except urllib.error.HTTPError as err:
            raise ArtifactRefusedError(f"registry answered HTTP {err.code} for {path}") from None
        except (urllib.error.URLError, OSError) as err:
            raise ArtifactRefusedError(f"registry unreachable for {path}: {err}") from None
        if len(data) > limit:
            raise ArtifactRefusedError(f"{path} exceeds {limit} bytes")
        return data

    def manifest(self, ref: OciRef, reference: str = "") -> bytes:
        return self._get(
            ref.host,
            f"{ref.repo}/manifests/{reference or ref.reference}",
            ", ".join(_MANIFEST_TYPES),
            MAX_MANIFEST_BYTES,
        )

    def blob(self, ref: OciRef, digest: str, size: int) -> bytes:
        if not _CHECKSUM_RE.fullmatch(digest):
            raise ArtifactRefusedError(f"malformed layer digest {digest!r}")
        if size < 0 or size > MAX_BLOB_BYTES:
            raise ArtifactRefusedError(f"layer {digest} declares size {size}, over the limit")
        data = self._get(ref.host, f"{ref.repo}/blobs/{digest}", "*/*", MAX_BLOB_BYTES)
        if len(data) != size or _sha256(data) != digest:
            raise ArtifactRefusedError(f"layer {digest} does not match its manifest descriptor")
        return data


def _sha256(data: bytes) -> str:
    return "sha256:" + hashlib.sha256(data).hexdigest()


# ── verification ────────────────────────────────────────────────────────────────


@dataclasses.dataclass(frozen=True)
class VerifiedArtifact:
    model: bytes
    labels: list[str]
    selfcheck: bytes
    manifest_digest: str
    signer: str


def load_trust_root(pem: bytes) -> Any:
    """Parse a PKIX public key (ECDSA or Ed25519), the form cosign and SwarmadaConfig use."""
    from cryptography.hazmat.primitives.asymmetric import ec, ed25519
    from cryptography.hazmat.primitives.serialization import load_pem_public_key

    key = load_pem_public_key(pem)
    if not isinstance(key, (ec.EllipticCurvePublicKey, ed25519.Ed25519PublicKey)):
        raise ValueError(f"unsupported trust root key type {type(key).__name__}")
    return key


def _signature_ok(key: Any, payload: bytes, sig: bytes) -> bool:
    from cryptography.exceptions import InvalidSignature
    from cryptography.hazmat.primitives import hashes
    from cryptography.hazmat.primitives.asymmetric import ec

    try:
        if isinstance(key, ec.EllipticCurvePublicKey):
            key.verify(sig, payload, ec.ECDSA(hashes.SHA256()))
        else:
            key.verify(sig, payload)
    except InvalidSignature:
        return False
    return True


def verify_signature(
    reg: Registry, sig_ref: str, model_ref: OciRef, manifest_digest: str, key: Any
) -> None:
    """Refuse unless a cosign signature at sig_ref, made by key, claims manifest_digest."""
    if not sig_ref:
        raise SignatureRefusedError("no model_signature_ref supplied")
    ref = parse_ref(sig_ref) if sig_ref.startswith("oci://") else None
    if ref is None:
        raise SignatureRefusedError(
            f"unsupported signature reference {sig_ref!r} " "(want an oci:// cosign signature)"
        )
    try:
        sig_manifest = json.loads(reg.manifest(ref))
    except ArtifactRefusedError as err:
        raise SignatureRefusedError(str(err)) from None
    except ValueError:
        raise SignatureRefusedError("signature manifest is not JSON") from None
    reasons = []
    for layer in sig_manifest.get("layers") or []:
        if layer.get("mediaType") != _COSIGN_LAYER:
            continue
        try:
            payload = reg.blob(ref, layer.get("digest", ""), int(layer.get("size", -1)))
            sig = base64.b64decode(
                (layer.get("annotations") or {}).get(_COSIGN_SIG_ANNOTATION, ""), validate=True
            )
        except (ArtifactRefusedError, ValueError) as err:
            reasons.append(str(err))
            continue
        if not sig or not _signature_ok(key, payload, sig):
            reasons.append("signature does not verify against the trust root")
            continue
        try:
            critical = json.loads(payload)["critical"]
            claimed = critical["image"]["docker-manifest-digest"]
            identity = critical["identity"]["docker-reference"]
            kind = critical["type"]
        except (ValueError, KeyError, TypeError):
            reasons.append("signed payload is not a cosign simple-signing claim")
            continue
        if kind != _COSIGN_TYPE:
            reasons.append(f"signed payload type {kind!r}")
        elif claimed != manifest_digest:
            reasons.append(f"signature is for {claimed}, not {manifest_digest}")
        elif identity != model_ref.repository:
            reasons.append(f"signature names repository {identity!r}, not {model_ref.repository!r}")
        else:
            return
    raise SignatureRefusedError("; ".join(reasons) or "no cosign signature layer at " + sig_ref)


def fetch_and_verify(
    model_uri: str, checksum: str, sig_ref: str, key: Any, signer: str, reg: Registry | None = None
) -> VerifiedArtifact:
    """Steps 1-3 of the module docstring. Returns the artifact only if every step holds."""
    reg = reg or Registry()
    if not checksum or not _CHECKSUM_RE.fullmatch(checksum):
        raise ArtifactRefusedError(f"malformed model_checksum {checksum!r} (want sha256:<64 hex>)")
    ref = parse_ref(model_uri)
    raw = reg.manifest(ref)
    digest = _sha256(raw)
    if digest != checksum:
        raise ArtifactRefusedError(
            f"checksum mismatch (manifest digest {digest} does not match model_checksum)"
        )
    try:
        manifest = json.loads(raw)
    except ValueError:
        raise ArtifactRefusedError("manifest is not JSON") from None
    if manifest.get("mediaType") not in _MANIFEST_TYPES:
        raise ArtifactRefusedError(f"unsupported manifest media type {manifest.get('mediaType')!r}")
    if manifest.get("artifactType") != ARTIFACT_TYPE:
        raise ArtifactRefusedError(
            f"not a model artifact (artifactType {manifest.get('artifactType')!r})"
        )
    # The signature is checked before any layer is downloaded: an unsigned artifact costs a
    # manifest fetch, not a model download.
    verify_signature(reg, sig_ref, ref, digest, key)
    by_type: dict[str, list[dict[str, Any]]] = {}
    for layer in manifest.get("layers") or []:
        by_type.setdefault(layer.get("mediaType", ""), []).append(layer)
    parts = {}
    for media in (MEDIA_ONNX, MEDIA_LABELS, MEDIA_SELFCHECK):
        found = by_type.get(media, [])
        if len(found) != 1:
            raise ArtifactRefusedError(
                f"artifact must carry exactly one {media} layer, has {len(found)}"
            )
        parts[media] = reg.blob(ref, found[0].get("digest", ""), int(found[0].get("size", -1)))
    try:
        labels = json.loads(parts[MEDIA_LABELS])["labels"]
    except (ValueError, KeyError, TypeError):
        raise ArtifactRefusedError('labels layer is not {"labels": [...]}') from None
    if not labels or not all(isinstance(x, str) and x for x in labels):
        raise ArtifactRefusedError("labels layer declares no labels")
    return VerifiedArtifact(parts[MEDIA_ONNX], labels, parts[MEDIA_SELFCHECK], digest, signer)


# ── the model ───────────────────────────────────────────────────────────────────


def _load_npz(data: bytes, *keys: str) -> dict[str, Any]:
    import numpy as np

    try:
        with np.load(io.BytesIO(data), allow_pickle=False) as z:
            return {k: z[k] for k in keys}
    except (ValueError, KeyError, OSError) as err:
        raise SelfCheckError(f"unreadable frame file: {err}") from None


class LoadedModel:
    """An ONNX classifier that has passed its self-check. Construct with ``load``."""

    def __init__(self, session: Any, labels: list[str], name: str, version: str) -> None:
        self._session = session
        self.labels = labels
        self.name = name
        self.version = version

    def predict(self, frames: Any) -> Any:
        import numpy as np

        inp = self._session.get_inputs()[0].name
        return np.asarray(self._session.run(None, {inp: frames.astype(np.float32)})[0])

    def run_frames(self, frames: Any, truth: Any = None) -> dict[str, Any]:
        """Classify a frame set; the result the adapter reports for the switch."""
        probs = self.predict(frames)
        top = probs.argmax(axis=1)
        out: dict[str, Any] = {
            "model": self.name,
            "version": self.version,
            "frames": len(top),
            "predictions": [
                [self.labels[int(k)], round(float(probs[i, k]), 3)] for i, k in enumerate(top)
            ],
        }
        if truth is not None:
            out["correct"] = int((top == truth).sum())
        return out


def load(art: VerifiedArtifact, name: str, version: str) -> LoadedModel:
    """Load a verified artifact and run its self-check. Raises SelfCheckError."""
    import numpy as np
    import onnxruntime as ort

    try:
        session = ort.InferenceSession(art.model, providers=["CPUExecutionProvider"])
    except Exception as err:  # onnxruntime raises its own exception types
        raise SelfCheckError(f"model does not load: {err}") from None
    model = LoadedModel(session, art.labels, name, version)
    probe = _load_npz(art.selfcheck, "frames", "expected")
    try:
        probs = model.predict(probe["frames"])
    except Exception as err:
        raise SelfCheckError(f"model does not run on its probe frames: {err}") from None
    n = len(probe["frames"])
    if probs.shape != (n, len(art.labels)):
        raise SelfCheckError(f"output shape {probs.shape}, want ({n}, {len(art.labels)})")
    if not np.isfinite(probs).all():
        bad = int((~np.isfinite(probs)).any(axis=1).sum())
        raise SelfCheckError(f"non-finite output on {bad}/{n} probe frames")
    if not np.allclose(probs.sum(axis=1), 1.0, atol=1e-3) or (probs < 0).any():
        raise SelfCheckError("outputs are not class probabilities")
    agree = int((probs.argmax(axis=1) == probe["expected"]).sum())
    if agree != n:
        raise SelfCheckError(f"known-answer probes: {agree}/{n} agree with the exported answers")
    return model


def load_frames(path: str) -> tuple[Any, Any]:
    """The fixed frame set the adapter runs after a switch: (frames NCHW, labels or None)."""
    with open(path, "rb") as fh:
        data = fh.read()
    import numpy as np

    with np.load(io.BytesIO(data), allow_pickle=False) as z:
        return z["frames"], (z["labels"] if "labels" in z.files else None)
