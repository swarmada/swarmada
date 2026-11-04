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

"""The zone-proposal worker reading a robot's maps from an object store.

A robot's adapter shares no directory with the cluster, so on a robot run ArtifactStore
floor-maps is of kind ObjectStore (examples/floor-mapping/object-store): the adapter uploads
each segment, the worker reads it from the bucket with the same verification as from a claim
(the manifest against the reported digest, every file against the manifest, nothing unlisted),
follows a resume chain through it, and saves the keep-out set it renders there, manifest last.
The two scoped policies and the overlay are pinned here too, and so is the threshold a map must
declare for the tools to accept it.

The store here is an in-process one that answers as the object-storage API does (ETag = md5,
404 for a missing object, ListObjectsV2); the signer is checked against AWS's published example.
"""

from __future__ import annotations

import hashlib
import http.server
import importlib.util
import json
import pathlib
import sys
import threading
import urllib.parse

import pytest
import yaml

_ROOT = pathlib.Path(__file__).resolve().parents[2]
_WORKER = _ROOT / "examples" / "floor-mapping" / "worker" / "zone_proposal_worker.py"
_STORE_DIR = _ROOT / "examples" / "floor-mapping" / "object-store"
_TOOLS = _ROOT / "tools" / "map-to-fleetzone"


def _load():
    spec = importlib.util.spec_from_file_location("zone_proposal_worker", _WORKER)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = mod
    spec.loader.exec_module(mod)
    return mod


w = _load()

NS, NAME = "warehouse-a", "explore-first"
AK, SK = "floor-maps-worker", "worker-secret-never-printed-000000"


def test_the_signature_is_awss_published_example():
    """AWS's "GET Object" example (AWS Signature Version 4, S3 API reference): the signature is
    AWS's own. It depends on the secret, the date, the scope and the request, not on the access
    key id, so a neutral id stands in for the example's (a key id is never written in this tree)."""
    empty = hashlib.sha256(b"").hexdigest()
    auth = w._sigv4("GET", "https://examplebucket.s3.amazonaws.com/test.txt",
                    {"host": "examplebucket.s3.amazonaws.com", "range": "bytes=0-9",
                     "x-amz-content-sha256": empty, "x-amz-date": "20130524T000000Z"}, empty,
                    access_key="EXAMPLE-KEY-ID",
                    secret_key="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
                    amz_date="20130524T000000Z")
    assert auth.startswith("AWS4-HMAC-SHA256 Credential=EXAMPLE-KEY-ID/20130524/us-east-1/s3/")
    assert auth.endswith(
        "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")


class _Bucket:
    """GET/PUT one object and ListObjectsV2, as the object-storage API answers them."""

    def __init__(self):
        self.objects: dict[str, bytes] = {}
        self.puts: list[str] = []
        me = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_PUT(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                key = urllib.parse.unquote(self.path.split("?")[0].lstrip("/"))
                me.objects[key] = body
                me.puts.append(key)
                self.send_response(200)
                self.send_header("ETag", f'"{hashlib.md5(body).hexdigest()}"')
                self.end_headers()

            def do_GET(self):
                path, _, query = self.path.partition("?")
                key = urllib.parse.unquote(path.lstrip("/"))
                if query:
                    q = dict(urllib.parse.parse_qsl(query))
                    prefix = key + "/" + q["prefix"]
                    keys = sorted(k for k in me.objects if k.startswith(prefix))
                    xml = ("<ListBucketResult xmlns='http://s3.amazonaws.com/doc/2006-03-01/'>"
                           + "".join(f"<Contents><Key>{k.split('/', 1)[1]}</Key></Contents>"
                                     for k in keys) + "<IsTruncated>false</IsTruncated>"
                           "</ListBucketResult>").encode()
                    self.send_response(200)
                    self.send_header("Content-Length", str(len(xml)))
                    self.end_headers()
                    self.wfile.write(xml)
                    return
                data = me.objects.get(key)
                if data is None:
                    self.send_response(404)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.endpoint = f"http://127.0.0.1:{self.server.server_address[1]}"

    def store(self):
        return w.ObjectStore(self.endpoint, "floor-maps", AK, SK, timeout=5)

    def artifact(self, name, files):
        manifest = "".join(f"{hashlib.sha256(files[n]).hexdigest()}  {n}\n"
                           for n in sorted(files)).encode()
        for n, d in {**files, w.MANIFEST: manifest}.items():
            self.objects[f"floor-maps/{NS}/{name}/{n}"] = d
        return hashlib.sha256(manifest).hexdigest()


@pytest.fixture()
def bucket():
    b = _Bucket()
    yield b
    b.server.shutdown()


def _sim_map(free_thresh="0.196"):
    sys.path.insert(0, str(_ROOT / "adapters" / "simulation"))
    try:
        import floor_mapping
    finally:
        sys.path.pop(0)
    map_yaml, pgm = floor_mapping.render_map(2)
    return map_yaml.replace(b"free_thresh: 0.196", f"free_thresh: {free_thresh}".encode()), pgm


# ── the artifact, from the bucket ───────────────────────────────────────────

def test_a_verified_artifact_is_copied_out_of_the_bucket(tmp_path, bucket):
    map_yaml, pgm = _sim_map()
    sha = bucket.artifact(NAME, {"map.yaml": map_yaml, "map.pgm": pgm})
    out = w.verify_and_copy(bucket.store(), w.MapReference("floor-maps", NS, NAME, sha),
                            tmp_path / "copy")
    assert (out / "map.pgm").read_bytes() == pgm


def test_an_object_the_manifest_does_not_list_is_refused(tmp_path, bucket):
    map_yaml, pgm = _sim_map()
    sha = bucket.artifact(NAME, {"map.yaml": map_yaml, "map.pgm": pgm})
    bucket.objects[f"floor-maps/{NS}/{NAME}/extra.bin"] = b"x"
    with pytest.raises(w.RefusalError, match="not listed"):
        w.verify_and_copy(bucket.store(), w.MapReference("floor-maps", NS, NAME, sha),
                          tmp_path / "copy")


def test_a_manifest_that_is_not_the_one_reported_is_refused(tmp_path, bucket):
    map_yaml, pgm = _sim_map()
    bucket.artifact(NAME, {"map.yaml": map_yaml, "map.pgm": pgm})
    with pytest.raises(w.RefusalError, match="hashes to"):
        w.verify_and_copy(bucket.store(), w.MapReference("floor-maps", NS, NAME, "b" * 64),
                          tmp_path / "copy")


def test_an_artifact_the_bucket_does_not_hold_is_refused(tmp_path, bucket):
    with pytest.raises(w.RefusalError, match="no artifact"):
        w.verify_and_copy(bucket.store(), w.MapReference("floor-maps", NS, NAME, "b" * 64),
                          tmp_path / "copy")


def test_the_resume_chain_is_followed_through_the_bucket(tmp_path, bucket):
    fresh = json.dumps({"mark": [0.0, 0.0, -60.5], "resumed_from": ""}).encode()
    bucket.artifact("explore-first", {"segment.json": fresh})
    (tmp_path / "copy").mkdir()
    (tmp_path / "copy" / "segment.json").write_text(json.dumps(
        {"mark": [0.1, 0.0, 88.0], "resumed_from": f"{NS}/explore-first"}))
    assert w.source_mark_yaw(tmp_path / "copy", bucket.store()) == -60.5


def test_the_keep_out_set_is_saved_in_the_bucket_manifest_last(tmp_path, bucket):
    map_yaml, pgm = _sim_map()
    geo = w.map_geometry(map_yaml, pgm)
    zones = [{"metadata": {"name": "floor-room-a-hearth", "annotations": {}},
              "spec": {"parentZone": "floor-room-a", "physicalBounds": {"polygon": [
                  {"x": 1, "y": 1}, {"x": 1.5, "y": 1}, {"x": 1.5, "y": 1.5}, {"x": 1, "y": 1.5}]},
                       "restrictions": {"entry": {"mode": "Denied", "reason": "a hearth"}}}}]
    ref, manifest_sha, zset = w.write_keepouts(bucket.store(), "floor-maps", NS, zones, geo,
                                               source=f"{NS}/{NAME}", theta0_deg=-60.5)
    name = f"keepouts-{zset[:16]}"
    assert ref == f"floor-maps/{NS}/{name}@sha256:{manifest_sha}"
    assert bucket.puts[-1] == f"floor-maps/{NS}/{name}/{w.MANIFEST}"
    assert hashlib.sha256(bucket.objects[bucket.puts[-1]]).hexdigest() == manifest_sha
    before = len(bucket.puts)
    w.write_keepouts(bucket.store(), "floor-maps", NS, zones, geo, source=f"{NS}/{NAME}",
                     theta0_deg=-60.5)
    assert len(bucket.puts) == before               # the same set is the same artifact


# ── the declared store ──────────────────────────────────────────────────────

def _store(kind, **spec):
    return {"spec": {"kind": kind, **spec}}


def test_an_object_store_worker_reads_only_the_bucket_its_store_declares():
    ref = w.MapReference("floor-maps", NS, NAME, "a" * 64)
    ep = "http://minio.swarmada-store.svc.cluster.local:9000/floor-maps"
    w.check_store(ref, _store("ObjectStore", objectStore={"endpoint": ep}), endpoint=ep)
    with pytest.raises(w.RefusalError, match="declares endpoint"):
        w.check_store(ref, _store("ObjectStore", objectStore={"endpoint": ep + "x"}), endpoint=ep)
    with pytest.raises(w.RefusalError, match="ObjectStore store only"):
        w.check_store(ref, _store("VolumeClaim", volumeClaim={"claimName": "floor-maps"}),
                      endpoint=ep)


def test_a_store_without_a_credential_or_with_a_path_is_refused():
    with pytest.raises(ValueError, match="credential"):
        w.ObjectStore("http://h:9000", "floor-maps", "", "")
    with pytest.raises(ValueError, match="no path"):
        w.ObjectStore("http://h:9000/floor-maps", "floor-maps", AK, SK)


# ── what a map must declare for the tools to take it ────────────────────────

class _Config:
    data = {"floor-name": "floor", "room-names": "room-a,room-b"}


@pytest.mark.parametrize("free_thresh, verdict", [("0.196", "proposed"), ("0.25", "refused")])
def test_a_map_declaring_0196_is_proposed_and_0_25_is_refused(tmp_path, bucket, free_thresh,
                                                                verdict):
    """The adapter's saved maps now declare 0.196; map_saver's default 0.25 reads every cell
    nothing observed as free, and the tools refuse such a map."""
    map_yaml, pgm = _sim_map(free_thresh)
    sha = bucket.artifact(NAME, {"map.yaml": map_yaml, "map.pgm": pgm})
    action = {"metadata": {"namespace": NS, "name": NAME},
              "status": {"message": f"explore-floor: segment done; map-ref=floor-maps/{NS}/{NAME} "
                                    f"map-sha256={sha}"}}
    ep = bucket.store().declared()
    cm = w.build_proposal(action, store=_store("ObjectStore", objectStore={"endpoint": ep}),
                          config=_Config.data, tools=w.Tools(_TOOLS), mount=bucket.store(),
                          claim=None, work=tmp_path, endpoint=ep)
    assert cm["metadata"]["annotations"][w.VERDICT_ANNOTATION] == verdict, cm["data"]["summary.txt"]
    if verdict == "refused":
        assert "free_thresh" in cm["data"]["summary.txt"]


# ── the scoped users and the overlay ────────────────────────────────────────

def _policy(name):
    return json.loads((_STORE_DIR / name).read_text())["Statement"]


def _actions(statements, effect):
    return {(a, r) for s in statements if s["Effect"] == effect
            for a in s["Action"] for r in s["Resource"]}


def test_the_robot_writes_its_maps_reads_only_keep_outs_and_lists_and_deletes_nothing():
    st = _policy("policy-robot.json")
    allow, deny = _actions(st, "Allow"), _actions(st, "Deny")
    assert allow == {("s3:PutObject", f"arn:aws:s3:::floor-maps/{NS}/*"),
                     ("s3:GetObject", f"arn:aws:s3:::floor-maps/{NS}/keepouts-*")}
    assert deny == {("s3:PutObject", f"arn:aws:s3:::floor-maps/{NS}/keepouts-*")}


def test_the_worker_reads_maps_writes_only_keep_outs_and_deletes_nothing():
    st = _policy("policy-worker.json")
    allow = _actions(st, "Allow")
    assert ("s3:PutObject", f"arn:aws:s3:::floor-maps/{NS}/keepouts-*") in allow
    assert ("s3:GetObject", f"arn:aws:s3:::floor-maps/{NS}/*") in allow
    assert not any("Delete" in a for a, _ in allow)
    listing = [s for s in st if "s3:ListBucket" in s["Action"]]
    assert listing[0]["Condition"] == {"StringLike": {"s3:prefix": [f"{NS}/*"]}}


def test_the_overlay_declares_the_bucket_the_worker_reads():
    """Read from the overlay's own patches, so no tool is needed: the store the ArtifactStore
    declares is the endpoint and bucket the worker is started with, and the VolumeClaim store of
    ../overlay is removed."""
    k = yaml.safe_load((_STORE_DIR / "kustomization.yaml").read_text())
    assert k["resources"] == ["../overlay"]
    deleted = {(d["kind"], d["metadata"]["name"]) for d in
               (yaml.safe_load(p["patch"]) for p in k["patches"] if "target" not in p)
               if d.get("$patch") == "delete"}
    assert deleted == {("PersistentVolume", "swarmada-floor-maps"),
                       ("PersistentVolumeClaim", "floor-maps")}
    ops = {p["target"]["kind"]: yaml.safe_load(p["patch"]) for p in k["patches"] if "target" in p}
    store = next(o["value"] for o in ops["ArtifactStore"] if o["path"] == "/spec")
    args = next(o["value"] for o in ops["Deployment"]
                if o["path"] == "/spec/template/spec/containers/0/args")
    ep = next(a.split("=", 1)[1] for a in args if a.startswith("--store-endpoint="))
    bkt = next(a.split("=", 1)[1] for a in args if a.startswith("--bucket="))
    assert store["kind"] == "ObjectStore"
    assert store["objectStore"]["endpoint"] == f"{ep}/{bkt}"
    assert not any(a.startswith(("--claim=", "--mount=")) for a in args)
