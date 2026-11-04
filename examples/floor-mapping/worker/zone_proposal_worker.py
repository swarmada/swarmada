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

"""Turn a map a robot saved by reference into a FleetZone proposal an operator reviews.

The floor-mapping demo's in-cluster worker. It runs as a Deployment the demo overlay applies,
so nobody starts it by hand. It watches for a FleetAction of ``type: Custom``,
``customType: explore-floor`` that reached ``Succeeded``, and for each one:

1. reads ``map-ref=<store>/<namespace>/<name> map-sha256=<hex>`` from ``status.message``;
2. checks that the reference names this action's own path, in an ArtifactStore that declares
   the store this worker reads: the claim it mounts (kind VolumeClaim), or the object store and
   bucket it was started with (kind ObjectStore, for a robot whose adapter shares no directory
   with the cluster);
3. verifies ``MANIFEST.sha256`` against the reported digest and every listed file against
   the manifest, and copies the verified bytes out, so the tools read a copy that cannot
   change underneath them;
4. runs ``floor_to_member_zones.py`` (one composite floor, one member zone per room) and
   ``map_to_fleetzone.py --mode traced`` (the whole-floor outline, kept for review only);
5. turns each of the operator's keep-outs into an exclusion child zone in ADR-0069's
   three-object shape: the room becomes a container, ``<room>-open`` is where robots are,
   and ``<room>-<keepout>`` carries ``entry: Denied`` with the operator's reason. A keep-out
   over a room's edge, or in no room, is kept whole and attached to that room, or the
   nearest, and the summary says so; one across more than one room refuses the proposal;
6. writes ConfigMap ``zone-proposal-<action>`` with the manifests in ``zones.yaml``.

Once the operator has applied a proposal, it also renders the ADAPTER's keep-out mask from the
applied zone set, so the zones the control plane refuses work in and the cells the robot keeps
out of come from one source of geometry:

7. reads the applied FleetZones this worker proposed, and their exclusion child zones;
8. renders them, in the source map's frame, as the keep-out mask the adapter loads (PGM: 0 is
   keep-out, 205 is unknown; ``mode: trinary``, ``free_thresh: 0.10``) plus the keep-out set the
   adapter reads (``keepouts.json``: the robot's yaw on the mark in that frame when the source
   map saved one, the source map, the zone set's digest, and each name, reason and polygon);
9. saves both at ``<namespace>/keepouts-<zone set sha256, 16 hex>/`` in the store, with a
   ``MANIFEST.sha256``, so a changed zone set is a new artifact and never an overwrite;
10. publishes ``keepouts_ref`` -- ``<store>/<namespace>/<name>@sha256:<digest of the
    manifest>`` -- in ConfigMap ``floor-keepouts``: the value an explore payload carries as
    ``keepouts_ref``, and the one a robot adapter is started with to check Navigate goals.

It never applies a zone. Its Role can read FleetZones and write none, so it could not if it
tried; the
operator applies ``zones.yaml``, and that apply is the confirm step. A map it cannot verify
still produces a proposal ConfigMap, with ``verdict: refused`` and the reason, so a refusal is
something an operator can read rather than a log line.

Only the Python standard library and PyYAML: the Kubernetes API is reached over HTTPS with
the pod's service-account token, and an object store is reached with requests signed by AWS
Signature Version 4, with a credential from the pod's environment that no message carries.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import hmac
import json
import math
import os
import re
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from dataclasses import dataclass
from pathlib import Path

import yaml

OPERATION = "explore-floor"
MANIFEST = "MANIFEST.sha256"
PROPOSAL_LABEL = "swarmada.io/zone-proposal"
VERDICT_ANNOTATION = "swarmada.io/zone-proposal-verdict"
SOURCE_ANNOTATION = "swarmada.io/zone-proposal-source-action"
DIGEST_ANNOTATION = "swarmada.io/zone-proposal-map-sha256"
DROP_OFF_ANNOTATION = "tools.swarmada.io/map-to-fleetzone-drop-off-unverified"
KEEPOUT_ANNOTATION = "tools.swarmada.io/zone-proposal-keepout-source"
KEEPOUTS_CONFIGMAP = "floor-keepouts"

_DNS = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
_REF = re.compile(r"(?:^|\s)map-ref=(\S+)")
_SHA = re.compile(r"(?:^|\s)map-sha256=(\S+)")
_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_MANIFEST_LINE = re.compile(r"^([0-9a-f]{64})  ([A-Za-z0-9][A-Za-z0-9._-]*)$")


class RefusalError(Exception):
    """A map or a keep-out the worker will not turn into zones, with the reason."""


@dataclass(frozen=True)
class MapReference:
    store: str
    namespace: str
    name: str
    sha256: str


@dataclass(frozen=True)
class Keepout:
    name: str
    reason: str
    ring: list


@dataclass(frozen=True)
class MapGeometry:
    """The grid a mask is drawn on: the source map's size, resolution and origin."""

    width: int
    height: int
    resolution: float
    origin_x: float
    origin_y: float


# ── The reference ─────────────────────────────────────────────────────────────


def parse_map_reference(message: str) -> MapReference:
    refs = _REF.findall(message or "")
    shas = _SHA.findall(message or "")
    if not refs:
        raise RefusalError("the final message carries no map-ref token")
    if not shas:
        raise RefusalError("the final message carries no map-sha256 token")
    if len(refs) > 1 or len(shas) > 1:
        raise RefusalError("the final message carries more than one map-ref or map-sha256 token")
    parts = refs[0].split("/")
    if len(parts) != 3 or not all(_DNS.match(p) for p in parts):
        raise RefusalError(f"map-ref {refs[0]!r} is not <store>/<namespace>/<name> of DNS names")
    if not _HEX64.match(shas[0]):
        raise RefusalError(f"map-sha256 {shas[0]!r} is not 64 lower-case hex digits")
    return MapReference(parts[0], parts[1], parts[2], shas[0])


def check_reference_is_the_actions_own(ref: MapReference, namespace: str, name: str) -> None:
    """The path is keyed by the action id, so the only path an action may report is its own."""
    if (ref.namespace, ref.name) != (namespace, name):
        raise RefusalError(f"map-ref names {ref.namespace}/{ref.name}, which is not the path of "
                      f"action {namespace}/{name}")


def check_store(ref: MapReference, store: dict | None, *, claim: str | None = None,
                endpoint: str | None = None) -> None:
    """The reference resolves through a declared ArtifactStore, to the store this pod reads:
    the claim it mounts, or (``endpoint``: ``<object store>/<bucket>``) the bucket it reads."""
    if store is None:
        raise RefusalError(f"no ArtifactStore {ref.store!r} is declared in namespace "
                           f"{ref.namespace}")
    spec = store.get("spec", {})
    if endpoint is not None:
        if spec.get("kind") != "ObjectStore":
            raise RefusalError(f"ArtifactStore {ref.store!r} is kind {spec.get('kind')!r}; this "
                               f"worker reads an ObjectStore store only")
        declared = spec.get("objectStore", {}).get("endpoint")
        if declared != endpoint:
            raise RefusalError(f"ArtifactStore {ref.store!r} declares endpoint {declared!r}, but "
                               f"this worker reads {endpoint!r}")
        return
    if spec.get("kind") != "VolumeClaim":
        raise RefusalError(f"ArtifactStore {ref.store!r} is kind {spec.get('kind')!r}; this worker "
                      f"reads a VolumeClaim store only")
    declared = spec.get("volumeClaim", {}).get("claimName")
    if declared != claim:
        raise RefusalError(f"ArtifactStore {ref.store!r} declares claim {declared!r}, but this "
                      f"worker mounts claim {claim!r}")


# ── The stores ────────────────────────────────────────────────────────────────


class DirStore:
    """A store that is a directory: a VolumeClaim mounted into this pod."""

    def __init__(self, root: Path) -> None:
        self.root = Path(root)

    def get(self, key: str) -> bytes | None:
        path = self.root / key
        return path.read_bytes() if path.is_file() else None

    def names(self, prefix: str) -> set[str] | None:
        """The names directly under ``prefix``; None when there is nothing there."""
        d = self.root / prefix
        return {p.name for p in d.iterdir()} if d.is_dir() else None

    def put_all(self, prefix: str, files: dict[str, bytes], manifest: bytes) -> None:
        """Write ``files`` and the manifest at ``prefix``, beside their place and renamed in,
        so a reader never sees half of them. A prefix already there is left as it is."""
        final = self.root / prefix
        if final.is_dir():
            return
        final.parent.mkdir(parents=True, exist_ok=True)
        staging = final.parent / f".{final.name}.staging-{os.getpid()}"
        staging.mkdir()
        for fname, data in {**files, MANIFEST: manifest}.items():
            (staging / fname).write_bytes(data)
        staging.rename(final)


def _sigv4(method: str, url: str, headers: dict, payload_sha256: str, *, access_key: str,
           secret_key: str, amz_date: str, region: str = "us-east-1") -> str:
    """The Authorization header for a request, by AWS Signature Version 4."""
    parts = urllib.parse.urlsplit(url)
    path = urllib.parse.quote(parts.path or "/", safe="-_.~/")
    query = "&".join(
        f"{urllib.parse.quote(k, safe='-_.~')}={urllib.parse.quote(v, safe='-_.~')}"
        for k, v in sorted(urllib.parse.parse_qsl(parts.query, keep_blank_values=True)))
    canon = {k.lower().strip(): " ".join(str(v).split()) for k, v in headers.items()}
    signed = ";".join(sorted(canon))
    request = "\n".join([method, path, query,
                         "".join(f"{k}:{canon[k]}\n" for k in sorted(canon)),
                         signed, payload_sha256])
    scope = f"{amz_date[:8]}/{region}/s3/aws4_request"
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amz_date, scope,
                         hashlib.sha256(request.encode()).hexdigest()])
    key = ("AWS4" + secret_key).encode()
    for part in (amz_date[:8], region, "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    sig = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
    return (f"AWS4-HMAC-SHA256 Credential={access_key}/{scope}, SignedHeaders={signed}, "
            f"Signature={sig}")


class ObjectStore:
    """A store that is a bucket of an object store, reached path-style over HTTP(S). The
    credential is this pod's (``SWARMADA_STORE_ACCESS_KEY``, ``SWARMADA_STORE_SECRET_KEY``);
    no error it raises carries it."""

    def __init__(self, endpoint: str, bucket: str, access_key: str, secret_key: str, *,
                 timeout: float = 30.0, opener=urllib.request.urlopen, now=None) -> None:
        parts = urllib.parse.urlsplit(endpoint)
        if parts.scheme not in ("http", "https") or not parts.netloc or parts.path not in ("", "/"):
            raise ValueError("the store endpoint must be http[s]://host[:port] with no path")
        if not bucket or "/" in bucket:
            raise ValueError("the bucket must be one plain name")
        if not access_key or not secret_key:
            raise ValueError("the store credential is not set (SWARMADA_STORE_ACCESS_KEY, "
                             "SWARMADA_STORE_SECRET_KEY)")
        self.endpoint, self.bucket = f"{parts.scheme}://{parts.netloc}", bucket
        self._host, self._ak, self._sk = parts.netloc, access_key, secret_key
        self._timeout, self._open = timeout, opener
        self._now = now or (lambda: datetime.datetime.now(datetime.UTC))

    def declared(self) -> str:
        """What an ArtifactStore of kind ObjectStore declares for this store."""
        return f"{self.endpoint}/{self.bucket}"

    def _call(self, method: str, key: str = "", query: str = "", body: bytes = b""):
        url = f"{self.endpoint}/{self.bucket}"
        if key:
            url += "/" + urllib.parse.quote(key, safe="-_.~/")
        if query:
            url += "?" + query
        amz_date = self._now().strftime("%Y%m%dT%H%M%SZ")
        digest = hashlib.sha256(body).hexdigest()
        headers = {"host": self._host, "x-amz-content-sha256": digest, "x-amz-date": amz_date}
        auth = _sigv4(method, url, headers, digest, access_key=self._ak, secret_key=self._sk,
                      amz_date=amz_date)
        req = urllib.request.Request(url, data=body if method == "PUT" else None, method=method,
                                     headers={**headers, "Authorization": auth})
        try:
            with self._open(req, timeout=self._timeout) as resp:
                return resp.read(), (resp.headers.get("ETag") or "").strip('"')
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                return None, ""
            raise OSError(f"the object store refused {method} {self.bucket}/{key} "
                          f"(HTTP {exc.code})") from None
        except urllib.error.URLError as exc:
            raise OSError(f"the object store did not answer {method} {self.bucket}/{key} "
                          f"({exc.reason})") from None

    def get(self, key: str) -> bytes | None:
        return self._call("GET", key)[0]

    def names(self, prefix: str) -> set[str] | None:
        """The object names directly under ``prefix/``; None when there are none."""
        names: set[str] = set()
        token = ""
        while True:
            q = {"list-type": "2", "prefix": prefix.rstrip("/") + "/"}
            if token:
                q["continuation-token"] = token
            body, _ = self._call("GET", query=urllib.parse.urlencode(sorted(q.items())))
            if body is None:
                return None
            root = ET.fromstring(body)
            ns = root.tag[:root.tag.index("}") + 1] if root.tag.startswith("{") else ""
            for c in root.iter(f"{ns}Contents"):
                rest = c.findtext(f"{ns}Key", "")[len(q["prefix"]):]
                if rest and "/" not in rest:
                    names.add(rest)
            if root.findtext(f"{ns}IsTruncated") != "true":
                break
            token = root.findtext(f"{ns}NextContinuationToken", "")
        return names or None

    def put(self, key: str, data: bytes) -> None:
        _, etag = self._call("PUT", key, body=data)
        if etag != hashlib.md5(data).hexdigest():
            raise OSError(f"the object store holds md5 {etag or '(none)'} for {self.bucket}/{key}, "
                          f"not the md5 of the bytes sent")

    def put_all(self, prefix: str, files: dict[str, bytes], manifest: bytes) -> None:
        """Put ``files``, then the manifest last: a reader that finds the manifest finds
        everything it lists. A prefix whose manifest is already there is left as it is."""
        if self.get(f"{prefix}/{MANIFEST}") is not None:
            return
        for fname in sorted(files):
            self.put(f"{prefix}/{fname}", files[fname])
        self.put(f"{prefix}/{MANIFEST}", manifest)


def _as_store(store):
    """A directory given where a store is expected is the directory store over it."""
    return DirStore(store) if isinstance(store, Path) else store


# ── The artifact ──────────────────────────────────────────────────────────────


def verify_and_copy(store, ref: MapReference, dest: Path) -> Path:
    """Verify the artifact at ``<namespace>/<name>`` in ``store`` and copy it to ``dest``.

    Each file is read once, hashed, and the same bytes are written out, so what the tools
    read is exactly what was verified.
    """
    store = _as_store(store)
    prefix = f"{ref.namespace}/{ref.name}"
    present = store.names(prefix)
    if present is None:
        raise RefusalError(f"no artifact directory at {ref.namespace}/{ref.name} in store "
                           f"{ref.store}")
    manifest = store.get(f"{prefix}/{MANIFEST}")
    if manifest is None:
        raise RefusalError(f"{ref.namespace}/{ref.name} has no {MANIFEST}")
    got = hashlib.sha256(manifest).hexdigest()
    if got != ref.sha256:
        raise RefusalError(f"{MANIFEST} hashes to {got}, but the action reported {ref.sha256}")
    listed: dict[str, str] = {}
    for line in manifest.decode("utf-8", "replace").splitlines():
        m = _MANIFEST_LINE.match(line)
        if not m or m.group(2) == MANIFEST:
            raise RefusalError(f"{MANIFEST} line {line!r} is not '<sha256>  <file name>'")
        listed[m.group(2)] = m.group(1)
    unlisted = sorted(present - set(listed) - {MANIFEST})
    if unlisted:
        raise RefusalError(f"files not listed in {MANIFEST}: {', '.join(unlisted)}")
    dest.mkdir(parents=True, exist_ok=True)
    for fname, want in sorted(listed.items()):
        data = store.get(f"{prefix}/{fname}")
        if data is None:
            raise RefusalError(f"{MANIFEST} lists {fname}, which is not in the artifact")
        got = hashlib.sha256(data).hexdigest()
        if got != want:
            raise RefusalError(f"{fname} hashes to {got}, but {MANIFEST} lists {want}")
        (dest / fname).write_bytes(data)
    if "map.yaml" not in listed:
        raise RefusalError(f"{MANIFEST} lists no map.yaml")
    image = (yaml.safe_load((dest / "map.yaml").read_text()) or {}).get("image")
    if image not in listed:
        raise RefusalError(f"map.yaml names image {image!r}, which {MANIFEST} does not list")
    return dest


# ── Keep-outs ─────────────────────────────────────────────────────────────────


def load_keepouts(text: str | None) -> list[Keepout]:
    if not text:
        return []
    doc = yaml.safe_load(text) or {}
    out = []
    for i, k in enumerate(doc.get("keepouts") or []):
        name = k.get("name", "")
        if not isinstance(name, str) or not _DNS.match(name) or len(name) > 40:
            raise RefusalError(f"keepouts[{i}].name {name!r} must be a lower-case DNS name of at "
                          f"most 40 characters")
        reason = k.get("reason")
        if not isinstance(reason, str) or not reason.strip():
            raise RefusalError(f"keep-out {name!r} has no reason; the refusal an operator reads "
                          f"quotes it, and a refusal without one cannot say why")
        pts = k.get("polygon") or []
        try:
            ring = [(float(p["x"]), float(p["y"])) for p in pts]
        except (KeyError, TypeError, ValueError) as exc:
            raise RefusalError(f"keep-out {name!r}: every polygon point needs numeric x "
                               f"and y") from exc
        if len(ring) < 3:
            raise RefusalError(f"keep-out {name!r}: a polygon needs at least three points")
        out.append(Keepout(name, reason.strip(), ring))
    return out


def _ring_of(zone: dict) -> list:
    return [(float(p["x"]), float(p["y"]))
            for p in zone["spec"].get("physicalBounds", {}).get("polygon", [])]


def _on_segment(p, a, b) -> bool:
    cross = (b[0] - a[0]) * (p[1] - a[1]) - (b[1] - a[1]) * (p[0] - a[0])
    return (abs(cross) < 1e-9 and min(a[0], b[0]) - 1e-9 <= p[0] <= max(a[0], b[0]) + 1e-9
            and min(a[1], b[1]) - 1e-9 <= p[1] <= max(a[1], b[1]) + 1e-9)


def _inside(p, ring) -> bool:
    """Point in polygon, a point on the boundary counting as inside."""
    n = len(ring)
    for i in range(n):
        if _on_segment(p, ring[i], ring[(i + 1) % n]):
            return True
    hit = False
    for i in range(n):
        (x1, y1), (x2, y2) = ring[i], ring[(i + 1) % n]
        if (y1 > p[1]) != (y2 > p[1]):
            x = x1 + (p[1] - y1) * (x2 - x1) / (y2 - y1)
            if x > p[0]:
                hit = not hit
    return hit


def _proper_cross(a, b, c, d) -> bool:
    def orient(p, q, r):
        v = (q[0] - p[0]) * (r[1] - p[1]) - (q[1] - p[1]) * (r[0] - p[0])
        return 0 if abs(v) < 1e-9 else (1 if v > 0 else -1)
    o1, o2, o3, o4 = orient(a, b, c), orient(a, b, d), orient(c, d, a), orient(c, d, b)
    return o1 * o2 < 0 and o3 * o4 < 0


def _contains(outer, inner) -> bool:
    """Every vertex of ``inner`` in ``outer`` and no edge of one properly crossing the other."""
    if not all(_inside(p, outer) for p in inner):
        return False
    for i in range(len(inner)):
        for j in range(len(outer)):
            if _proper_cross(inner[i], inner[(i + 1) % len(inner)],
                             outer[j], outer[(j + 1) % len(outer)]):
                return False
    return True


def _strictly_inside(p, ring) -> bool:
    n = len(ring)
    return (not any(_on_segment(p, ring[i], ring[(i + 1) % n]) for i in range(n))
            and _inside(p, ring))


def _centroid(ring) -> tuple[float, float]:
    a = cx = cy = 0.0
    for (x1, y1), (x2, y2) in zip(ring, ring[1:] + ring[:1]):
        c = x1 * y2 - x2 * y1
        a, cx, cy = a + c, cx + (x1 + x2) * c, cy + (y1 + y2) * c
    if abs(a) < 1e-12:
        return sum(x for x, _ in ring) / len(ring), sum(y for _, y in ring) / len(ring)
    return cx / (3 * a), cy / (3 * a)


def _overlaps(a, b) -> bool:
    """The two rings share area: an edge of one properly crosses an edge of the other, or a
    vertex or the centroid of one lies strictly inside the other. Rings that only touch along
    an edge or at a corner do not overlap."""
    for i in range(len(a)):
        for j in range(len(b)):
            if _proper_cross(a[i], a[(i + 1) % len(a)], b[j], b[(j + 1) % len(b)]):
                return True
    return any(_strictly_inside(p, b) for p in [*a, _centroid(a)]) or \
        any(_strictly_inside(p, a) for p in [*b, _centroid(b)])


def _point_segment(p, a, b) -> float:
    dx, dy = b[0] - a[0], b[1] - a[1]
    t = 0.0 if dx == dy == 0 else max(0.0, min(1.0, ((p[0] - a[0]) * dx + (p[1] - a[1]) * dy)
                                                / (dx * dx + dy * dy)))
    return math.hypot(p[0] - a[0] - t * dx, p[1] - a[1] - t * dy)


def _gap(a, b) -> float:
    """The distance between two rings that do not overlap: nearest vertex to edge, both ways."""
    return min(_point_segment(p, q, r) for ring, other in ((a, b), (b, a)) for p in ring
               for q, r in zip(other, other[1:] + other[:1]))


def _home(rooms: list[dict], k: Keepout) -> tuple[str, str | None]:
    """The room a keep-out's exclusion belongs to, and why, when it is not simply inside it.

    Inside exactly one room: that room, nothing to say. Over one room's edge -- a hearth whose
    fireplace the map holds as occupied, a rug running into cells never seen: that room, the
    keep-out kept whole. In no room at all: the nearest room, the keep-out kept whole. Across
    more than one room: refused, since splitting it or picking one would put a denial where the
    operator did not draw it. A keep-out is never clipped (the part outside the room as this
    map traced it may be floor a later map shows, and a clipped denial would leave it open)
    and never dropped."""
    names = [r["metadata"]["name"] for r in rooms]
    rings = [_ring_of(r) for r in rooms]
    inside = [n for n, ring in zip(names, rings) if _contains(ring, k.ring)]
    if len(inside) == 1:
        return inside[0], None
    over = [n for n, ring in zip(names, rings) if _overlaps(ring, k.ring)]
    if len(over) > 1:
        raise RefusalError(f"keep-out {k.name!r} lies across more than one room "
                           f"({', '.join(over)}); redraw it as one keep-out in each")
    if over:
        return over[0], (f"extends outside {over[0]} as mapped (over cells the map holds "
                         f"occupied or unknown)")
    if not rooms:
        raise RefusalError(f"keep-out {k.name!r}: this map has no room to attach it to")
    gap, near = min((_gap(ring, k.ring), n) for n, ring in zip(names, rings))
    return near, f"lies in no room of this map; nearest room {near}, {gap:.2f} m away"


def apply_keepouts(manifests: list[dict], keepouts: list[Keepout],
                   notes: list[str] | None = None) -> list[dict]:
    """Rewrite each room that holds a keep-out into ADR-0069's three-object shape.

    The room keeps its name, so the composite floor's ``memberZones`` is unchanged; it becomes
    the container. ``<room>-open`` (same polygon) is the navigable remainder a robot is in,
    and ``<room>-<keepout>`` is the exclusion, with the keep-out's own polygon. Which room a
    keep-out belongs to is ``_home``'s rule; a keep-out not inside one room is attached and
    said in ``notes`` (and on the exclusion), and one across rooms refuses the proposal.
    """
    rooms = [m for m in manifests if not m["spec"].get("memberZones")]
    by_room: dict[str, list[Keepout]] = {}
    said: dict[str, str] = {}
    for k in keepouts:
        home, why = _home(rooms, k)
        if why:
            said[k.name] = why
            if notes is not None:
                notes.append(f"keep-out {k.name!r} {why}; kept whole, attached to {home}")
        by_room.setdefault(home, []).append(k)
    out = []
    for m in manifests:
        out.append(m)
        name = m["metadata"]["name"]
        if name not in by_room:
            continue
        meta = m["metadata"]
        bounds = m["spec"]["physicalBounds"]
        display = m["spec"].get("displayName", name)
        open_name = f"{name}-open"
        _check_name(open_name)
        out.append({
            "apiVersion": m["apiVersion"], "kind": "FleetZone",
            "metadata": {"name": open_name, "namespace": meta["namespace"],
                         "annotations": dict(meta.get("annotations", {}))},
            "spec": {"displayName": f"{display} (open)", "parentZone": name,
                     "physicalBounds": json.loads(json.dumps(bounds))},
        })
        for k in by_room[name]:
            excl = f"{name}-{k.name}"
            _check_name(excl)
            out.append({
                "apiVersion": m["apiVersion"], "kind": "FleetZone",
                "metadata": {"name": excl, "namespace": meta["namespace"], "annotations": {
                    DROP_OFF_ANNOTATION: "true",
                    KEEPOUT_ANNOTATION: "operator keep-out, from the floor-mapping ConfigMap"
                    + (f"; it {said[k.name]}, kept whole" if k.name in said else ""),
                }},
                "spec": {
                    "displayName": f"{display}: {k.name}",
                    "parentZone": name,
                    "physicalBounds": {
                        "floor": bounds.get("floor", 0),
                        "frame": json.loads(json.dumps(bounds["frame"])),
                        "polygon": [{"x": x, "y": y} for x, y in k.ring],
                    },
                    "restrictions": {"entry": {"mode": "Denied", "reason": k.reason}},
                },
            })
    return out


def _check_name(name: str) -> None:
    if len(name) > 63 or not _DNS.match(name):
        raise RefusalError(f"zone name {name!r} is not a DNS label of at most 63 characters; "
                      f"shorten the floor, room or keep-out name")


# ── The tools ─────────────────────────────────────────────────────────────────


class Tools:
    """The map-to-fleetzone tools, run as the operator would run them."""

    def __init__(self, tool_dir: Path, python: str = sys.executable) -> None:
        self.dir = tool_dir
        self.python = python

    def _run(self, args: list[str]) -> str:
        proc = subprocess.run([self.python, *args], capture_output=True, text=True,
                              timeout=300, check=False)
        if proc.returncode != 0:
            raise RefusalError(f"{Path(args[0]).name} exited {proc.returncode}: "
                          f"{proc.stderr.strip()[-600:]}")
        return proc.stdout

    def members(self, map_yaml: Path, *, floor: str, namespace: str, room_names: str) -> list:
        out = map_yaml.parent / "members.yaml"
        args = [str(self.dir / "floor_to_member_zones.py"), str(map_yaml), "--name", floor,
                "--namespace", namespace, "-o", str(out)]
        if room_names:
            args += ["--room-names", room_names]
        self._run(args)
        return [d for d in yaml.safe_load_all(out.read_text()) if d]

    def traced(self, map_yaml: Path, *, floor: str, namespace: str) -> list:
        text = self._run([str(self.dir / "map_to_fleetzone.py"), str(map_yaml), "--mode",
                          "traced", "--name", f"{floor}-traced", "--namespace", namespace])
        return [d for d in yaml.safe_load_all(text) if d]


# ── The proposal ──────────────────────────────────────────────────────────────


def wants(action: dict) -> bool:
    spec, status = action.get("spec", {}), action.get("status", {})
    return (spec.get("type") == "Custom" and spec.get("customType") == OPERATION
            and status.get("phase") == "Succeeded")


def proposal_name(action_name: str) -> str:
    name = f"zone-proposal-{action_name}"
    if len(name) <= 253:
        return name
    digest = hashlib.sha256(action_name.encode()).hexdigest()[:10]
    return f"zone-proposal-{action_name[:200]}-{digest}"


# Strings YAML 1.1 reads as booleans. kubectl parses YAML 1.1, so a bare `y:` key becomes
# `true:` and the API server refuses the polygon point; every such string is written quoted.
_YAML11_BOOLS = frozenset(
    w for base in ("y", "yes", "n", "no", "true", "false", "on", "off")
    for w in (base, base.capitalize(), base.upper()))


class _Dumper(yaml.SafeDumper):
    pass


def _represent_str(dumper: yaml.SafeDumper, value: str) -> yaml.ScalarNode:
    style = '"' if value in _YAML11_BOOLS else None
    return dumper.represent_scalar("tag:yaml.org,2002:str", value, style=style)


_Dumper.add_representer(str, _represent_str)


def _dump_all(docs: list[dict]) -> str:
    return "".join("---\n" + yaml.dump(d, Dumper=_Dumper, sort_keys=False, width=100)
                   for d in docs)


def build_proposal(action: dict, *, store: dict | None, config: dict, tools, mount,
                   claim: str | None, work: Path, endpoint: str | None = None) -> dict:
    """The ConfigMap for one succeeded explore-floor action: a proposal, or a refusal."""
    ns = action["metadata"]["namespace"]
    name = action["metadata"]["name"]
    message = action.get("status", {}).get("message", "")
    summary = [f"source action: {ns}/{name}", f"final message: {message}"]
    data: dict[str, str] = {}
    verdict = "proposed"
    digest = ""
    try:
        ref = parse_map_reference(message)
        digest = ref.sha256
        check_reference_is_the_actions_own(ref, ns, name)
        check_store(ref, store, claim=claim, endpoint=endpoint)
        copy = verify_and_copy(mount, ref, work / name)
        summary.append(f"map: {ref.store}/{ref.namespace}/{ref.name}, {MANIFEST} sha256 "
                       f"{ref.sha256}, verified")
        floor = config.get("floor-name") or "floor"
        rooms = config.get("room-names", "")
        keepouts = load_keepouts(config.get("keepouts.yaml"))
        members = tools.members(copy / "map.yaml", floor=floor, namespace=ns, room_names=rooms)
        notes: list[str] = []
        zones = apply_keepouts(members, keepouts, notes=notes)
        for z in zones:
            z["metadata"].setdefault("annotations", {}).update(
                {SOURCE_ANNOTATION: f"{ns}/{name}", DIGEST_ANNOTATION: ref.sha256})
        for z in zones:
            if z["metadata"].get("annotations", {}).get(DROP_OFF_ANNOTATION) != "true":
                raise RefusalError(f"zone {z['metadata']['name']} is not marked "
                                  f"drop-off-unverified")
        traced = tools.traced(copy / "map.yaml", floor=floor, namespace=ns)
        data["zones.yaml"] = (
            "# A PROPOSAL, generated from a map; nothing here has been applied.\n"
            "# Review it, then apply it yourself: that apply is the confirm step.\n"
            "# Containers are listed before their children; keep the order.\n"
            + _dump_all(zones))
        data["floor-traced.yaml"] = (
            "# The whole-floor outline (map_to_fleetzone --mode traced), for review only.\n"
            "# It is not part of the proposal: do not apply it.\n" + _dump_all(traced))
        summary.append(f"zones: {len(zones)} ({', '.join(z['metadata']['name'] for z in zones)})")
        summary.append(f"keep-outs: {len(keepouts)} "
                       f"({', '.join(k.name for k in keepouts) or 'none'})")
        summary.extend(notes)
        summary.append("every zone is marked drop-off-unverified: drop-off hazards were never "
                       "checked")
    except RefusalError as exc:
        verdict = "refused"
        summary.append(f"REFUSED: {exc}")
    summary.append(f"verdict: {verdict}")
    data["summary.txt"] = "\n".join(summary) + "\n"
    annotations = {VERDICT_ANNOTATION: verdict, SOURCE_ANNOTATION: f"{ns}/{name}"}
    if digest:
        annotations[DIGEST_ANNOTATION] = digest
    return {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": proposal_name(name), "namespace": ns,
                     "labels": {PROPOSAL_LABEL: "true"}, "annotations": annotations},
        "data": data,
    }


# ── The adapter's keep-out mask, from the applied zone set ────────────────────


def _canonical(zone: dict) -> dict:
    """What makes two zone sets the same set: names, tree, geometry and restrictions."""
    spec = zone.get("spec", {})
    return {
        "name": zone["metadata"]["name"],
        "parentZone": spec.get("parentZone"),
        "memberZones": spec.get("memberZones"),
        "physicalBounds": spec.get("physicalBounds"),
        "restrictions": spec.get("restrictions"),
    }


def _canonical_set(zones: list[dict]) -> list[dict]:
    return sorted((_canonical(z) for z in zones), key=lambda c: c["name"])


def zone_set_digest(zones: list[dict]) -> str:
    """sha256 of the zone set; order and server-written fields do not change it."""
    text = json.dumps(_canonical_set(zones), sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(text.encode()).hexdigest()


def applied_zone_set(zones: list[dict]) -> list[dict]:
    """The applied FleetZones this worker proposed, all from one proposal."""
    mine = [z for z in zones
            if SOURCE_ANNOTATION in (z["metadata"].get("annotations") or {})]
    sources = sorted({z["metadata"]["annotations"][SOURCE_ANNOTATION] for z in mine})
    if len(sources) > 1:
        raise RefusalError(f"zones from more than one proposal are applied ({', '.join(sources)}); "
                           f"a keep-out mask is drawn from one map, so none is rendered")
    return mine


def exclusions_of(zones: list[dict]) -> list[Keepout]:
    """The exclusion child zones: a parent, and an entry that is denied."""
    out = []
    for z in zones:
        spec = z.get("spec", {})
        entry = (spec.get("restrictions") or {}).get("entry") or {}
        if spec.get("parentZone") and entry.get("mode") == "Denied":
            out.append(Keepout(z["metadata"]["name"], entry.get("reason", ""), _ring_of(z)))
    return sorted(out, key=lambda k: k.name)


def map_geometry(map_yaml: bytes, pgm: bytes) -> MapGeometry:
    meta = yaml.safe_load(map_yaml) or {}
    origin = meta.get("origin") or [0.0, 0.0, 0.0]
    tokens: list[bytes] = []
    for line in pgm.split(b"\n"):
        if line.startswith(b"#"):
            continue
        tokens += line.split()
        if len(tokens) >= 4:
            break
    if len(tokens) < 4 or tokens[0] != b"P5":
        raise RefusalError("the source map's image is not a binary PGM (P5)")
    return MapGeometry(width=int(tokens[1]), height=int(tokens[2]),
                       resolution=float(meta["resolution"]),
                       origin_x=float(origin[0]), origin_y=float(origin[1]))


def _even_odd(x: float, y: float, ring) -> bool:
    """The adapter's own point-in-polygon rule, so a cell is keep-out here iff it is there."""
    inside = False
    n = len(ring)
    for k in range(n):
        (x1, y1), (x2, y2) = ring[k], ring[(k + 1) % n]
        if (y1 > y) != (y2 > y) and x < x1 + (y - y1) * (x2 - x1) / (y2 - y1):
            inside = not inside
    return inside


def render_keepout_mask(geo: MapGeometry, keepouts: list[Keepout]) -> tuple[bytes, bytes]:
    """The mask YAML and PGM: a cell whose centre is inside a keep-out is 0, every other 205.

    205 is UNKNOWN and, with free_thresh 0.10, the keep-out filter leaves it untouched; a free
    value would erase the costmap's unknown cells, so none is written. PGM row 0 is the top of
    the map.
    """
    res, ox, oy = geo.resolution, geo.origin_x, geo.origin_y
    body = bytearray()
    for row in range(geo.height):
        y = oy + (geo.height - 1 - row + 0.5) * res
        for ix in range(geo.width):
            x = ox + (ix + 0.5) * res
            body.append(0 if any(_even_odd(x, y, k.ring) for k in keepouts) else 205)
    pgm = b"P5\n%d %d\n255\n" % (geo.width, geo.height) + bytes(body)
    yaml_text = (f"image: keepout-mask.pgm\nmode: trinary\nresolution: {res}\n"
                 f"origin: [{float(ox)}, {float(oy)}, 0.0]\nnegate: 0\noccupied_thresh: 0.65\n"
                 f"free_thresh: 0.10\n").encode()
    return yaml_text, pgm


def _segment_doc(path: Path, label: str) -> dict:
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError) as exc:
        raise RefusalError(f"{label}'s segment.json cannot be read ({exc})") from exc


def _verified_segment(mount, ref: str) -> dict:
    """segment.json of the saved segment ``<ns>/<name>``, read only if its manifest lists it
    with the same sha256."""
    parts = ref.split("/")
    if len(parts) != 2 or not all(_DNS.match(p) for p in parts):
        raise RefusalError(f"resumed_from {ref!r} is not a segment <namespace>/<name>")
    store = _as_store(mount)
    try:
        raw_manifest = store.get(f"{ref}/{MANIFEST}")
        data = store.get(f"{ref}/segment.json")
    except OSError as exc:
        raise RefusalError(f"the segment {ref} this map resumed from is gone ({exc})") from exc
    if raw_manifest is None or data is None:
        raise RefusalError(f"the segment {ref} this map resumed from is gone (no "
                           f"{MANIFEST if raw_manifest is None else 'segment.json'})")
    manifest = raw_manifest.decode("utf-8", "replace")
    got = hashlib.sha256(data).hexdigest()
    listed = {m.group(2): m.group(1) for m in map(_MANIFEST_LINE.match, manifest.splitlines()) if m}
    if listed.get("segment.json") != got:
        raise RefusalError(f"{ref}'s segment.json hashes to {got}, not what its {MANIFEST} lists")
    return json.loads(data)


def source_mark_yaw(copy: Path, mount) -> float | None:
    """The robot's yaw on the mark in the source map's frame, as its FRESH segment saved it.

    A resumed map is in the frame its fresh segment started, and only there was the robot put on
    the mark by hand facing the heading line; a resumed segment starts wherever the return to
    the mark left it facing. So the chain of ``resumed_from`` is followed back to the fresh
    segment, each segment.json proven by its own manifest. None when the map records no segment
    (the simulation adapter writes none). A chain that cannot be followed is refused: a keep-out
    set turned by a wrong angle would keep the robot out of the wrong place.
    """
    if not (copy / "segment.json").is_file():
        return None
    doc, seen = _segment_doc(copy / "segment.json", "the source map"), set()
    while doc.get("resumed_from") if isinstance(doc, dict) else False:
        ref = str(doc["resumed_from"])
        if ref in seen:
            raise RefusalError(f"the resume chain loops back to {ref}")
        seen.add(ref)
        doc = _verified_segment(mount, ref)
    try:
        yaw = doc["mark"][2]
    except (KeyError, IndexError, TypeError) as exc:
        raise RefusalError(f"the fresh segment's segment.json gives no mark yaw ({exc})") from exc
    if isinstance(yaw, bool) or not isinstance(yaw, (int, float)) or not math.isfinite(yaw):
        raise RefusalError(f"the fresh segment's segment.json mark yaw {yaw!r} is not a number")
    return float(yaw)


def write_keepouts(mount, store: str, ns: str, zones: list[dict], geo: MapGeometry, *,
                   source: str, theta0_deg: float | None) -> tuple[str, str, str]:
    """Save the keep-out set for ``zones`` under its digest; return (keepouts_ref, manifest sha,
    set sha).

    ``keepouts.json`` is the set the robot's adapter reads: ``theta0_deg`` (the robot's yaw on
    the mark in the zones' map frame, from the fresh segment that started that frame; left out
    when the source map records none),
    ``source_map``, ``zone_set_sha256`` and each exclusion's name, reason and polygon as
    ``{"x", "y"}`` points. ``keepouts_ref`` is ``<store>/<ns>/<name>@sha256:<sha256 of
    MANIFEST.sha256>``, so the reference names the exact bytes. The directory is named by the
    zone set's digest, so the same set is the same artifact and a changed set is a new one. It
    is written beside its place and renamed in, so a reader never sees half of it.
    """
    zset = zone_set_digest(zones)
    keepouts = exclusions_of(zones)
    mask_yaml, mask_pgm = render_keepout_mask(geo, keepouts)
    files = {
        "keepout-mask.yaml": mask_yaml,
        "keepout-mask.pgm": mask_pgm,
        "keepouts.json": (json.dumps({
            **({"theta0_deg": theta0_deg} if theta0_deg is not None else {}),
            "source_map": source,
            "zone_set_sha256": zset,
            "keepouts": [{"name": k.name, "reason": k.reason,
                          "polygon": [{"x": x, "y": y} for x, y in k.ring]} for k in keepouts],
        }, indent=2) + "\n").encode(),
        "zone-set.json": (json.dumps(_canonical_set(zones), indent=2, sort_keys=True)
                          + "\n").encode(),
    }
    manifest = "".join(f"{hashlib.sha256(files[n]).hexdigest()}  {n}\n"
                       for n in sorted(files)).encode()
    name = f"keepouts-{zset[:16]}"
    _as_store(mount).put_all(f"{ns}/{name}", files, manifest)
    manifest_sha = hashlib.sha256(manifest).hexdigest()
    return f"{store}/{ns}/{name}@sha256:{manifest_sha}", manifest_sha, zset


def keepouts_configmap(ns: str, *, ref: str, manifest_sha: str, zset: str, source: str,
                       keepouts: list[Keepout], theta0_deg: float | None) -> dict:
    return {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": KEEPOUTS_CONFIGMAP, "namespace": ns},
        "data": {
            "keepouts_ref": ref,
            "keepouts_sha256": manifest_sha,
            "zone_set_sha256": zset,
            "source_action": source,
            "keepouts": ",".join(k.name for k in keepouts) or "none",
            "theta0_deg": (f"{theta0_deg:g}" if theta0_deg is not None else
                           "not recorded: the source map saved no mark, so an adapter that "
                           "turns keep-outs into its bound's frame refuses this set"),
            "summary.txt": (
                f"The keep-out mask for the applied zone set {zset}, drawn from the map of "
                f"{source}. Put keepouts_ref in an explore payload, or start a robot adapter "
                f"with it to check Navigate goals; the adapter "
                f"verifies {MANIFEST} against keepouts_sha256 before loading it.\n"),
        },
    }


def render_applied(kube, ns: str, *, store: str, mount, work: Path) -> str | None:
    """Render the mask for the applied zone set if it changed; return its digest, or None."""
    zones = applied_zone_set(kube.fleetzones(ns))
    if not zones:
        return None
    ann = zones[0]["metadata"]["annotations"]
    src_ns, src_name = ann[SOURCE_ANNOTATION].split("/", 1)
    ref = MapReference(store, src_ns, src_name, ann.get(DIGEST_ANNOTATION, ""))
    copy = verify_and_copy(mount, ref, work / f"keepouts-source-{src_name}")
    geo = map_geometry((copy / "map.yaml").read_bytes(),
                       (copy / yaml.safe_load((copy / "map.yaml").read_text())["image"])
                       .read_bytes())
    theta0 = source_mark_yaw(copy, mount)
    keepouts_ref, manifest_sha, zset = write_keepouts(mount, store, ns, zones, geo,
                                                      source=ann[SOURCE_ANNOTATION],
                                                      theta0_deg=theta0)
    cm = keepouts_configmap(ns, ref=keepouts_ref, manifest_sha=manifest_sha, zset=zset,
                            source=ann[SOURCE_ANNOTATION], keepouts=exclusions_of(zones),
                            theta0_deg=theta0)
    current = kube.configmap(ns, KEEPOUTS_CONFIGMAP)
    if current is None:
        kube.create_configmap(cm)
    elif (current.get("data") or {}).get("zone_set_sha256") != zset:
        kube.replace_configmap(cm)
    return zset


# ── The Kubernetes API ────────────────────────────────────────────────────────


class Kube:
    """The calls this worker makes, over HTTPS with the pod's service-account token."""

    SA = Path("/var/run/secrets/kubernetes.io/serviceaccount")

    def __init__(self) -> None:
        host = os.environ["KUBERNETES_SERVICE_HOST"]
        port = os.environ.get("KUBERNETES_SERVICE_PORT", "443")
        self.base = f"https://{host}:{port}"
        self.ctx = ssl.create_default_context(cafile=str(self.SA / "ca.crt"))

    def _call(self, method: str, path: str, body: dict | None = None):
        token = (self.SA / "token").read_text().strip()
        req = urllib.request.Request(
            self.base + path, method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json",
                     "Accept": "application/json"})
        try:
            with urllib.request.urlopen(req, context=self.ctx, timeout=30) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                return None
            raise

    def explore_actions(self, ns: str) -> list[dict]:
        doc = self._call("GET", f"/apis/swarmada.io/v1/namespaces/{ns}/fleetactions")
        return (doc or {}).get("items", [])

    def artifact_store(self, ns: str, name: str) -> dict | None:
        return self._call("GET", f"/apis/swarmada.io/v1/namespaces/{ns}/artifactstores/{name}")

    def configmap(self, ns: str, name: str) -> dict | None:
        return self._call("GET", f"/api/v1/namespaces/{ns}/configmaps/{name}")

    def create_configmap(self, cm: dict) -> None:
        ns = cm["metadata"]["namespace"]
        self._call("POST", f"/api/v1/namespaces/{ns}/configmaps", cm)

    def replace_configmap(self, cm: dict) -> None:
        ns, name = cm["metadata"]["namespace"], cm["metadata"]["name"]
        self._call("PUT", f"/api/v1/namespaces/{ns}/configmaps/{name}", cm)

    def fleetzones(self, ns: str) -> list[dict]:
        doc = self._call("GET", f"/apis/swarmada.io/v1/namespaces/{ns}/fleetzones")
        return (doc or {}).get("items", [])


def run(args: argparse.Namespace) -> None:
    kube = Kube()
    tools = Tools(Path(args.tool_dir))
    work = Path(args.work)
    endpoint = None
    if args.store_endpoint:
        mount = ObjectStore(args.store_endpoint, args.bucket,
                            os.environ.get("SWARMADA_STORE_ACCESS_KEY", ""),
                            os.environ.get("SWARMADA_STORE_SECRET_KEY", ""))
        endpoint = mount.declared()
        where = f"object store {endpoint}"
    else:
        mount = DirStore(Path(args.mount))
        where = f"store claim {args.claim} at {args.mount}"
    done: set[str] = set()
    rendered: str | None = None
    print(f"zone-proposal-worker: watching {args.namespace} for {OPERATION} actions every "
          f"{args.interval}s; {where}", flush=True)
    while True:
        try:
            for action in kube.explore_actions(args.namespace):
                name = action["metadata"]["name"]
                if name in done or not wants(action):
                    continue
                cm_name = proposal_name(name)
                if kube.configmap(args.namespace, cm_name) is not None:
                    done.add(name)
                    continue
                ref_store = None
                try:
                    ref_store = parse_map_reference(action["status"].get("message", "")).store
                except RefusalError:
                    pass
                store = kube.artifact_store(args.namespace, ref_store) if ref_store else None
                config = (kube.configmap(args.namespace, args.config) or {}).get("data", {})
                cm = build_proposal(action, store=store, config=config, tools=tools,
                                    mount=mount, claim=args.claim, work=work,
                                    endpoint=endpoint)
                kube.create_configmap(cm)
                done.add(name)
                print(f"zone-proposal-worker: {args.namespace}/{name} -> ConfigMap {cm_name} "
                      f"({cm['metadata']['annotations'][VERDICT_ANNOTATION]})", flush=True)
        except (urllib.error.URLError, OSError, KeyError, ValueError) as exc:
            print(f"zone-proposal-worker: pass failed, retrying: {exc}", file=sys.stderr,
                  flush=True)
        try:
            zset = render_applied(kube, args.namespace, store=args.store, mount=mount,
                                  work=work)
            if zset and zset != rendered:
                rendered = zset
                print(f"zone-proposal-worker: keep-out mask for applied zone set {zset[:16]} "
                      f"-> ConfigMap {KEEPOUTS_CONFIGMAP}", flush=True)
        except RefusalError as exc:
            print(f"zone-proposal-worker: no keep-out mask rendered: {exc}", file=sys.stderr,
                  flush=True)
        except (urllib.error.URLError, OSError, KeyError, ValueError) as exc:
            print(f"zone-proposal-worker: keep-out pass failed, retrying: {exc}",
                  file=sys.stderr, flush=True)
        time.sleep(args.interval)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--claim", default=None,
                    help="the PersistentVolumeClaim mounted at --mount; the ArtifactStore a "
                         "map reference names must declare it (kind VolumeClaim)")
    ap.add_argument("--mount", default=None)
    ap.add_argument("--store-endpoint", default=None, metavar="URL",
                    help="instead of --claim/--mount: an object store (http[s]://host[:port]); "
                         "the ArtifactStore must be kind ObjectStore and declare "
                         "<URL>/<--bucket>. The credential is read from "
                         "SWARMADA_STORE_ACCESS_KEY and SWARMADA_STORE_SECRET_KEY")
    ap.add_argument("--bucket", default=None)
    ap.add_argument("--store", default="floor-maps",
                    help="the ArtifactStore that declares the store; keep-out references name it")
    ap.add_argument("--tool-dir", required=True,
                    help="directory holding map_to_fleetzone.py and floor_to_member_zones.py")
    ap.add_argument("--config", default="floor-mapping",
                    help="ConfigMap with floor-name, room-names and keepouts.yaml")
    ap.add_argument("--work", default="/tmp/zone-proposal-work")
    ap.add_argument("--interval", type=float, default=5.0)
    args = ap.parse_args()
    if bool(args.store_endpoint) == bool(args.claim or args.mount):
        ap.error("give either --claim and --mount, or --store-endpoint and --bucket")
    if args.store_endpoint and not args.bucket:
        ap.error("--store-endpoint needs --bucket")
    if not args.store_endpoint and not (args.claim and args.mount):
        ap.error("--claim and --mount go together")
    run(args)


if __name__ == "__main__":
    main()
