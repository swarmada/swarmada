"""The floor-mapping demo's zone-proposal worker: a map reference in, a zone proposal out.

The worker reads a map a robot saved by reference, verifies it, turns it into FleetZone
manifests with the operator's keep-outs as exclusion child zones, and writes a PROPOSAL the
operator reviews. It applies nothing: the operator's apply is the confirm step, and the
overlay's Role gives the worker no verb on FleetZones at all.
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib
import sys

import pytest
import yaml

_ROOT = pathlib.Path(__file__).resolve().parents[2]
_WORKER = _ROOT / "examples" / "floor-mapping" / "worker" / "zone_proposal_worker.py"
_OVERLAY = _ROOT / "examples" / "floor-mapping" / "overlay"


def _load():
    spec = importlib.util.spec_from_file_location("zone_proposal_worker", _WORKER)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = mod  # dataclasses resolve their module through sys.modules
    spec.loader.exec_module(mod)
    return mod


w = _load()

SHA = "a" * 64
NS, NAME = "warehouse-a", "explore-floor-explore-first"
GOOD = (f"explore-floor (sim): segment done; covered 15.8 of 27.1 m2 (58%); exhausted: none; "
        f"frontier: remaining; map-ref=floor-maps/{NS}/{NAME} map-sha256={SHA}")


# ── The reference ─────────────────────────────────────────────────────────────


def test_a_reference_is_parsed_from_the_final_message():
    ref = w.parse_map_reference(GOOD)
    assert (ref.store, ref.namespace, ref.name, ref.sha256) == ("floor-maps", NS, NAME, SHA)


@pytest.mark.parametrize("message, why", [
    ("explore-floor (sim): segment done", "no map-ref"),
    (f"map-ref=floor-maps/{NS}/{NAME}", "no map-sha256"),
    (f"map-ref=floor-maps/{NS}/{NAME} map-sha256=abc", "not 64"),
    (f"map-ref=floor-maps/../etc map-sha256={SHA}", "<store>/<namespace>/<name>"),
    (f"map-ref=floor-maps/{NS}/{NAME}/../x map-sha256={SHA}", "<store>/<namespace>/<name>"),
    (f"map-ref=a/b/c map-ref=d/e/f map-sha256={SHA}", "more than one"),
])
def test_a_malformed_reference_is_refused_and_says_why(message, why):
    with pytest.raises(w.RefusalError, match=why):
        w.parse_map_reference(message)


def test_a_reference_to_another_actions_path_is_refused():
    ref = w.parse_map_reference(GOOD)
    with pytest.raises(w.RefusalError, match="not the path of action"):
        w.check_reference_is_the_actions_own(ref, NS, "some-other-action")


def test_a_reference_through_an_undeclared_store_is_refused():
    ref = w.parse_map_reference(GOOD)
    with pytest.raises(w.RefusalError, match="no ArtifactStore"):
        w.check_store(ref, None, claim="floor-maps")
    store = {"spec": {"kind": "ObjectStore", "objectStore": {"endpoint": "s3://x"}}}
    with pytest.raises(w.RefusalError, match="VolumeClaim"):
        w.check_store(ref, store, claim="floor-maps")
    store = {"spec": {"kind": "VolumeClaim", "volumeClaim": {"claimName": "other",
                                                             "accessMode": "ReadWriteOnce"}}}
    with pytest.raises(w.RefusalError, match="mounts claim"):
        w.check_store(ref, store, claim="floor-maps")


# ── The artifact ──────────────────────────────────────────────────────────────


def _write_artifact(root: pathlib.Path, files: dict[str, bytes], *, extra: dict | None = None,
                    tamper: str | None = None, name: str = NAME) -> str:
    d = root / NS / name
    d.mkdir(parents=True)
    manifest = "".join(f"{hashlib.sha256(files[n]).hexdigest()}  {n}\n" for n in sorted(files))
    for n, data in {**files, **(extra or {})}.items():
        (d / n).write_bytes(data)
    (d / "MANIFEST.sha256").write_text(manifest)
    if tamper:
        (d / tamper).write_bytes(b"changed after the manifest was written")
    return hashlib.sha256(manifest.encode()).hexdigest()


MAP_FILES = {"map.yaml": b"image: map.pgm\nresolution: 0.05\n", "map.pgm": b"P5\n1 1\n255\n\x00"}


def test_a_verified_artifact_is_copied_out_whole(tmp_path):
    sha = _write_artifact(tmp_path / "store", MAP_FILES)
    ref = w.MapReference("floor-maps", NS, NAME, sha)
    out = w.verify_and_copy(tmp_path / "store", ref, tmp_path / "copy")
    assert (out / "map.yaml").read_bytes() == MAP_FILES["map.yaml"]
    assert (out / "map.pgm").read_bytes() == MAP_FILES["map.pgm"]


def test_a_manifest_that_is_not_the_one_reported_is_refused(tmp_path):
    _write_artifact(tmp_path / "store", MAP_FILES)
    ref = w.MapReference("floor-maps", NS, NAME, "b" * 64)
    with pytest.raises(w.RefusalError, match="MANIFEST.sha256 hashes to"):
        w.verify_and_copy(tmp_path / "store", ref, tmp_path / "copy")


def test_a_file_changed_after_the_manifest_is_refused(tmp_path):
    sha = _write_artifact(tmp_path / "store", MAP_FILES, tamper="map.pgm")
    with pytest.raises(w.RefusalError, match="map.pgm hashes to"):
        w.verify_and_copy(tmp_path / "store", w.MapReference("floor-maps", NS, NAME, sha),
                          tmp_path / "copy")


def test_a_file_the_manifest_does_not_list_is_refused(tmp_path):
    sha = _write_artifact(tmp_path / "store", MAP_FILES, extra={"notes.txt": b"hello"})
    with pytest.raises(w.RefusalError, match="not listed"):
        w.verify_and_copy(tmp_path / "store", w.MapReference("floor-maps", NS, NAME, sha),
                          tmp_path / "copy")


def test_a_map_whose_image_is_not_listed_is_refused(tmp_path):
    files = {"map.yaml": b"image: other.pgm\nresolution: 0.05\n", "map.pgm": b"x"}
    sha = _write_artifact(tmp_path / "store", files)
    with pytest.raises(w.RefusalError, match="other.pgm"):
        w.verify_and_copy(tmp_path / "store", w.MapReference("floor-maps", NS, NAME, sha),
                          tmp_path / "copy")


# ── Keep-outs become exclusion child zones (ADR-0069's three-object shape) ────


def _zone(name, ring, members=None):
    spec = {"displayName": name, "physicalBounds": {
        "floor": 0, "frame": {"id": "map", "alignment": "FrameLocal"},
        "polygon": [{"x": x, "y": y} for x, y in ring]}}
    if members:
        spec["memberZones"] = members
    return {"apiVersion": "swarmada.io/v1", "kind": "FleetZone",
            "metadata": {"name": name, "namespace": NS, "annotations": {
                w.DROP_OFF_ANNOTATION: "true"}},
            "spec": spec}


def _box(x0, y0, x1, y1):
    return [(x0, y0), (x1, y0), (x1, y1), (x0, y1)]


def _floor():
    return [_zone("floor", _box(0, 0, 9, 4), ["floor-room-a", "floor-room-b"]),
            _zone("floor-room-a", _box(0, 0, 5, 4)),
            _zone("floor-room-b", _box(5.2, 0, 9, 4))]


KEEPOUT = w.Keepout("keepout-1", "operator keep-out: fragile edge", _box(1, 1, 1.6, 1.6))


def test_a_room_with_a_keepout_becomes_container_open_and_exclusion():
    out = w.apply_keepouts(_floor(), [KEEPOUT])
    names = [m["metadata"]["name"] for m in out]
    assert names == ["floor", "floor-room-a", "floor-room-a-open", "floor-room-a-keepout-1",
                     "floor-room-b"]
    by = {m["metadata"]["name"]: m for m in out}
    assert by["floor-room-a-open"]["spec"]["parentZone"] == "floor-room-a"
    assert by["floor-room-a-open"]["spec"]["physicalBounds"] == \
        by["floor-room-a"]["spec"]["physicalBounds"]
    excl = by["floor-room-a-keepout-1"]["spec"]
    assert excl["parentZone"] == "floor-room-a"
    assert excl["restrictions"]["entry"] == {"mode": "Denied",
                                             "reason": "operator keep-out: fragile edge"}
    assert excl["physicalBounds"]["frame"] == by["floor-room-a"]["spec"]["physicalBounds"]["frame"]
    assert "parentZone" not in by["floor-room-b"]["spec"], "a room with no keep-out is unchanged"
    assert by["floor"]["spec"]["memberZones"] == ["floor-room-a", "floor-room-b"], \
        "the composite still names the rooms, which are now containers"


def test_every_zone_in_the_proposal_is_marked_drop_off_unverified():
    for m in w.apply_keepouts(_floor(), [KEEPOUT]):
        assert m["metadata"]["annotations"][w.DROP_OFF_ANNOTATION] == "true", m["metadata"]["name"]


def test_a_parent_is_always_listed_before_its_children():
    out = w.apply_keepouts(_floor(), [KEEPOUT, w.Keepout("keepout-2", "r", _box(3, 3, 3.5, 3.5))])
    seen = set()
    for m in out:
        parent = m["spec"].get("parentZone")
        assert parent is None or parent in seen, f"{m['metadata']['name']} before {parent}"
        seen.add(m["metadata"]["name"])


def test_a_keepout_that_lies_across_two_rooms_refuses_the_proposal():
    # It spans the doorway: guessing a room would put the denial where the operator did not draw it.
    with pytest.raises(w.RefusalError,
                       match=r"lies across more than one room \(floor-room-a, floor-room-b\)"):
        w.apply_keepouts(_floor(), [w.Keepout("keepout-x", "r", _box(4.8, 1, 5.5, 1.5))])


# A hearth: the slab runs under a fireplace that the map holds as occupied, so the room traced
# from the map stops at the fireplace and the slab crosses the room's edge.
HEARTH = w.Keepout("hearth", "marble hearth below the lidar plane", _box(-0.4, 1, 0.8, 2.2))


def test_a_keepout_over_a_rooms_edge_is_kept_whole_attached_to_that_room_and_said():
    notes = []
    out = w.apply_keepouts(_floor(), [HEARTH], notes=notes)
    by = {m["metadata"]["name"]: m for m in out}
    excl = by["floor-room-a-hearth"]
    assert excl["spec"]["parentZone"] == "floor-room-a"
    assert excl["spec"]["restrictions"]["entry"] == {
        "mode": "Denied", "reason": "marble hearth below the lidar plane"}
    # Whole: the part outside the room as mapped is still denied, never clipped away.
    assert [(p["x"], p["y"]) for p in excl["spec"]["physicalBounds"]["polygon"]] == HEARTH.ring
    assert "extends outside floor-room-a" in excl["metadata"]["annotations"][w.KEEPOUT_ANNOTATION]
    assert notes == ["keep-out 'hearth' extends outside floor-room-a as mapped (over cells the "
                     "map holds occupied or unknown); kept whole, attached to floor-room-a"]


def test_a_keepout_in_no_room_is_kept_whole_and_attached_to_the_nearest_room_and_said():
    notes = []
    out = w.apply_keepouts(_floor(), [w.Keepout("keepout-x", "r", _box(9.5, 1, 10, 1.5))],
                           notes=notes)
    excl = {m["metadata"]["name"]: m for m in out}["floor-room-b-keepout-x"]
    assert excl["spec"]["parentZone"] == "floor-room-b"
    assert notes == ["keep-out 'keepout-x' lies in no room of this map; nearest room "
                     "floor-room-b, 0.50 m away; kept whole, attached to floor-room-b"]


@pytest.mark.parametrize("ring", [
    _box(1, 1, 1.6, 1.6), _box(-0.4, 1, 0.8, 2.2), _box(9.5, 1, 10, 1.5), _box(20, 20, 21, 21),
    _box(0, 0, 5, 4),
])
def test_a_keepout_is_never_dropped(ring):
    out = w.apply_keepouts(_floor(), [w.Keepout("keepout-x", "r", ring)], notes=[])
    excl = [m for m in out if m["metadata"]["name"].endswith("-keepout-x")]
    assert len(excl) == 1
    assert [(p["x"], p["y"]) for p in excl[0]["spec"]["physicalBounds"]["polygon"]] == ring


def test_keepouts_are_read_from_the_operators_configmap_text():
    text = yaml.safe_dump({"keepouts": [
        {"name": "keepout-1", "reason": "fragile edge",
         "polygon": [{"x": 1, "y": 1}, {"x": 2, "y": 1}, {"x": 2, "y": 2}]}]})
    [k] = w.load_keepouts(text)
    assert (k.name, k.reason, k.ring) == ("keepout-1", "fragile edge", [(1, 1), (2, 1), (2, 2)])


@pytest.mark.parametrize("doc, why", [
    ({"keepouts": [{"name": "Bad_Name", "reason": "r",
                    "polygon": [{"x": 0, "y": 0}, {"x": 1, "y": 0}, {"x": 1, "y": 1}]}]}, "name"),
    ({"keepouts": [{"name": "k", "polygon": [{"x": 0, "y": 0}, {"x": 1, "y": 0},
                                             {"x": 1, "y": 1}]}]}, "reason"),
    ({"keepouts": [{"name": "k", "reason": "r", "polygon": [{"x": 0, "y": 0}]}]}, "three"),
])
def test_a_malformed_keepout_is_refused(doc, why):
    with pytest.raises(w.RefusalError, match=why):
        w.load_keepouts(yaml.safe_dump(doc))


# ── The whole proposal, with the tools faked ──────────────────────────────────


class _FakeTools:
    def __init__(self):
        self.calls = []

    def members(self, map_yaml, *, floor, namespace, room_names):
        self.calls.append(("members", floor, namespace, room_names))
        return _floor()

    def traced(self, map_yaml, *, floor, namespace):
        self.calls.append(("traced", floor, namespace))
        return [_zone(f"{floor}-traced", _box(0, 0, 9, 4))]


def test_a_proposal_is_a_configmap_that_applies_nothing(tmp_path):
    sha = _write_artifact(tmp_path / "store", MAP_FILES)
    msg = f"segment done; map-ref=floor-maps/{NS}/{NAME} map-sha256={sha}"
    action = {"metadata": {"name": NAME, "namespace": NS},
              "spec": {"type": "Custom", "customType": "explore-floor"},
              "status": {"phase": "Succeeded", "message": msg}}
    store = {"spec": {"kind": "VolumeClaim", "volumeClaim": {"claimName": "floor-maps",
                                                             "accessMode": "ReadWriteOnce"}}}
    config = {"floor-name": "floor", "room-names": "room-a,room-b",
              "keepouts.yaml": yaml.safe_dump({"keepouts": [
                  {"name": "keepout-1", "reason": "fragile edge",
                   "polygon": [{"x": x, "y": y} for x, y in _box(1, 1, 1.6, 1.6)]}]})}
    tools = _FakeTools()
    cm = w.build_proposal(action, store=store, config=config, tools=tools,
                          mount=tmp_path / "store", claim="floor-maps", work=tmp_path / "work")
    assert cm["kind"] == "ConfigMap"
    assert cm["metadata"]["name"] == f"zone-proposal-{NAME}"
    assert cm["metadata"]["labels"][w.PROPOSAL_LABEL] == "true"
    assert cm["metadata"]["annotations"][w.VERDICT_ANNOTATION] == "proposed"
    zones = list(yaml.safe_load_all(cm["data"]["zones.yaml"]))
    assert [z["metadata"]["name"] for z in zones if z][:3] == ["floor", "floor-room-a",
                                                               "floor-room-a-open"]
    assert "floor-traced" in cm["data"]["floor-traced.yaml"]
    assert "floor-traced" not in cm["data"]["zones.yaml"], "the traced floor is for review only"
    assert sha in cm["data"]["summary.txt"]
    assert ("members", "floor", NS, "room-a,room-b") in tools.calls


def test_a_proposal_with_a_keepout_over_a_rooms_edge_is_proposed_and_says_so(tmp_path):
    sha = _write_artifact(tmp_path / "store", MAP_FILES)
    action = {"metadata": {"name": NAME, "namespace": NS},
              "spec": {"type": "Custom", "customType": "explore-floor"},
              "status": {"phase": "Succeeded",
                         "message": f"map-ref=floor-maps/{NS}/{NAME} map-sha256={sha}"}}
    store = {"spec": {"kind": "VolumeClaim", "volumeClaim": {"claimName": "floor-maps",
                                                             "accessMode": "ReadWriteOnce"}}}
    config = {"keepouts.yaml": yaml.safe_dump({"keepouts": [
        {"name": HEARTH.name, "reason": HEARTH.reason,
         "polygon": [{"x": x, "y": y} for x, y in HEARTH.ring]}]})}
    cm = w.build_proposal(action, store=store, config=config, tools=_FakeTools(),
                          mount=tmp_path / "store", claim="floor-maps", work=tmp_path / "work")
    assert cm["metadata"]["annotations"][w.VERDICT_ANNOTATION] == "proposed"
    names = [z["metadata"]["name"] for z in yaml.safe_load_all(cm["data"]["zones.yaml"]) if z]
    assert "floor-room-a-hearth" in names
    assert ("keep-out 'hearth' extends outside floor-room-a as mapped"
            in cm["data"]["summary.txt"])


def test_the_proposal_quotes_every_y_key(tmp_path):
    # kubectl parses YAML 1.1, where a bare `y` key is the boolean true: the API server then
    # refuses every polygon point with `unknown field "spec.physicalBounds.polygon[0].true"`.
    sha = _write_artifact(tmp_path / "store", MAP_FILES)
    action = {"metadata": {"name": NAME, "namespace": NS},
              "spec": {"type": "Custom", "customType": "explore-floor"},
              "status": {"phase": "Succeeded",
                         "message": f"map-ref=floor-maps/{NS}/{NAME} map-sha256={sha}"}}
    store = {"spec": {"kind": "VolumeClaim", "volumeClaim": {"claimName": "floor-maps",
                                                             "accessMode": "ReadWriteOnce"}}}
    config = {"keepouts.yaml": yaml.safe_dump({"keepouts": [
        {"name": "keepout-1", "reason": "r",
         "polygon": [{"x": x, "y": y} for x, y in _box(1, 1, 1.6, 1.6)]}]})}
    cm = w.build_proposal(action, store=store, config=config, tools=_FakeTools(),
                          mount=tmp_path / "store", claim="floor-maps", work=tmp_path / "work")
    for key in ("zones.yaml", "floor-traced.yaml"):
        text = cm["data"][key]
        assert '"y":' in text, key
        bare = [ln for ln in text.splitlines() if ln.strip().lstrip("- ").startswith("y:")]
        assert bare == [], f"{key}: {bare[:3]}"


def test_a_refused_map_still_produces_a_proposal_that_says_why(tmp_path):
    action = {"metadata": {"name": NAME, "namespace": NS},
              "spec": {"type": "Custom", "customType": "explore-floor"},
              "status": {"phase": "Succeeded", "message": "segment done; no reference"}}
    cm = w.build_proposal(action, store=None, config={}, tools=_FakeTools(),
                          mount=tmp_path, claim="floor-maps", work=tmp_path / "work")
    assert cm["metadata"]["annotations"][w.VERDICT_ANNOTATION] == "refused"
    assert "no map-ref" in cm["data"]["summary.txt"]
    assert "zones.yaml" not in cm["data"]


@pytest.mark.parametrize("action, wanted", [
    ({"spec": {"type": "Custom", "customType": "explore-floor"},
      "status": {"phase": "Succeeded"}}, True),
    ({"spec": {"type": "Custom", "customType": "explore-floor"},
      "status": {"phase": "InProgress"}}, False),
    ({"spec": {"type": "Custom", "customType": "mow"}, "status": {"phase": "Succeeded"}}, False),
    ({"spec": {"type": "Charge"}, "status": {"phase": "Succeeded"}}, False),
])
def test_only_a_succeeded_explore_floor_action_is_picked_up(action, wanted):
    assert w.wants(action) is wanted


def test_a_long_action_name_still_gives_a_valid_configmap_name():
    name = w.proposal_name("x" * 250)
    assert len(name) <= 253 and name.startswith("zone-proposal-")


# ── The overlay: the worker cannot apply a zone ───────────────────────────────


def _overlay_docs():
    for path in sorted(_OVERLAY.glob("*.yaml")):
        yield from (d for d in yaml.safe_load_all(path.read_text()) if d)


def test_the_workers_role_can_read_zones_and_write_none():
    # It reads the applied zone set to render the keep-out mask; it can never write a zone.
    roles = [d for d in _overlay_docs() if d["kind"] in ("Role", "ClusterRole")
             and "zone-proposal" in d["metadata"]["name"]]
    assert roles, "the overlay declares the worker's Role"
    for role in roles:
        for rule in role["rules"]:
            assert "*" not in rule.get("resources", []), rule
            assert "*" not in rule.get("verbs", []), rule
            if "fleetzones" in rule.get("resources", []):
                assert set(rule["verbs"]) <= {"get", "list"}, rule
            if "configmaps" in rule.get("resources", []) and "update" in rule["verbs"]:
                assert rule.get("resourceNames") == ["floor-keepouts"], rule


def test_the_worker_is_started_by_the_cluster_not_by_hand():
    deps = [d for d in _overlay_docs() if d["kind"] == "Deployment"
            and d["metadata"]["name"] == "zone-proposal-worker"]
    assert len(deps) == 1


# ── The adapter's keep-out mask, rendered from the APPLIED zone set ───────────
#
# One source of geometry: the exclusion child zones the operator applied are what the robot's
# adapter keeps out of. The worker renders them into the mask format the adapter loads, stores
# it under the zone set's digest, and publishes the reference the explore and navigate payloads
# carry as keepouts_ref.


def _applied(keepout_ring=_box(1, 1, 1.6, 1.6), source=f"{NS}/{NAME}"):
    zones = w.apply_keepouts(_floor(), [w.Keepout("keepout-1", "fragile edge", keepout_ring)])
    for z in zones:
        z["metadata"]["annotations"][w.SOURCE_ANNOTATION] = source
        z["metadata"]["annotations"][w.DIGEST_ANNOTATION] = SHA
    return zones


def test_a_changed_zone_set_yields_a_new_digest():
    base = w.zone_set_digest(_applied())
    assert w.zone_set_digest(_applied(keepout_ring=_box(1, 1, 1.7, 1.6))) != base
    renamed = _applied()
    renamed[3]["spec"]["restrictions"]["entry"]["reason"] = "another reason"
    assert w.zone_set_digest(renamed) != base
    assert w.zone_set_digest(_applied()[:-1]) != base, "a zone removed is a new set"


def test_the_digest_ignores_order_and_server_fields():
    zones = _applied()
    noisy = [json_copy(z) for z in reversed(zones)]
    for z in noisy:
        z["metadata"]["resourceVersion"] = "4711"
        z["metadata"]["uid"] = "abc"
        z["status"] = {"robotCount": 1}
    assert w.zone_set_digest(noisy) == w.zone_set_digest(zones)


def json_copy(d):
    import json
    return json.loads(json.dumps(d))


def test_only_exclusion_children_become_keepouts():
    [k] = w.exclusions_of(_applied())
    assert (k.name, k.reason) == ("floor-room-a-keepout-1", "fragile edge")
    assert k.ring == _box(1, 1, 1.6, 1.6)


def test_the_mask_marks_exactly_the_cells_inside_an_exclusion():
    geo = w.MapGeometry(width=20, height=10, resolution=0.1, origin_x=0.0, origin_y=0.0)
    yaml_text, pgm = w.render_keepout_mask(geo, [w.Keepout("k", "r", _box(0.5, 0.2, 1.0, 0.6))])
    meta = yaml.safe_load(yaml_text)
    assert meta == {"image": "keepout-mask.pgm", "mode": "trinary", "resolution": 0.1,
                    "origin": [0.0, 0.0, 0.0], "negate": 0, "occupied_thresh": 0.65,
                    "free_thresh": 0.10}
    header, body = pgm[:len(b"P5\n20 10\n255\n")], pgm[len(b"P5\n20 10\n255\n"):]
    assert header == b"P5\n20 10\n255\n" and len(body) == 200
    assert set(body) == {0, 205}, "0 is keep-out; 205 is unknown, never free"
    # PGM row 0 is the top of the map (highest y); cell (ix, iy) counts iy from the bottom.
    keep = {(ix, 9 - row) for row in range(10) for ix in range(20) if body[row * 20 + ix] == 0}
    assert keep == {(ix, iy) for ix in range(5, 10) for iy in range(2, 6)}


def test_map_geometry_is_read_from_the_source_map():
    geo = w.map_geometry(b"image: map.pgm\nresolution: 0.05\norigin: [-1.0, 2.5, 0.0]\n",
                         b"P5\n180 90\n255\n" + bytes(180 * 90))
    assert geo == w.MapGeometry(width=180, height=90, resolution=0.05, origin_x=-1.0,
                                origin_y=2.5)


def test_the_keepout_artifact_is_saved_under_the_zone_set_digest(tmp_path):
    zones = _applied()
    geo = w.MapGeometry(width=180, height=90, resolution=0.05, origin_x=0.0, origin_y=0.0)
    ref, manifest_sha, zset = w.write_keepouts(tmp_path, "floor-maps", NS, zones, geo,
                                               source=f"{NS}/{NAME}", theta0_deg=-73.85)
    assert zset == w.zone_set_digest(zones)
    assert ref == f"floor-maps/{NS}/keepouts-{zset[:16]}@sha256:{manifest_sha}"
    d = tmp_path / NS / f"keepouts-{zset[:16]}"
    manifest = (d / "MANIFEST.sha256").read_bytes()
    assert hashlib.sha256(manifest).hexdigest() == manifest_sha
    listed = {ln.split("  ")[1] for ln in manifest.decode().splitlines()}
    assert listed == {"keepout-mask.yaml", "keepout-mask.pgm", "keepouts.json", "zone-set.json"}
    import json
    doc = json.loads((d / "keepouts.json").read_text())
    # The keep-out set the robot's adapter reads: its format is the contract.
    assert doc == {"theta0_deg": -73.85, "source_map": f"{NS}/{NAME}", "zone_set_sha256": zset,
                   "keepouts": [{"name": "floor-room-a-keepout-1", "reason": "fragile edge",
                                 "polygon": [{"x": x, "y": y}
                                             for x, y in _box(1, 1, 1.6, 1.6)]}]}
    ref2, _, zset2 = w.write_keepouts(tmp_path, "floor-maps", NS,
                                      _applied(keepout_ring=_box(1, 1, 1.7, 1.6)), geo,
                                      source=f"{NS}/{NAME}", theta0_deg=-73.85)
    assert zset2 != zset and ref2 != ref, "a changed zone set is a new artifact, not an overwrite"
    assert d.is_dir()


def test_a_zone_set_from_two_proposals_is_refused():
    zones = _applied() + [dict(_applied(source=f"{NS}/other")[-1])]
    zones[-1]["metadata"] = dict(zones[-1]["metadata"], name="floor-room-c")
    with pytest.raises(w.RefusalError, match="more than one proposal"):
        w.applied_zone_set(zones)


def test_zones_the_worker_did_not_propose_are_not_part_of_the_set():
    other = _zone("warehouse-a", _box(0, 0, 1, 1))
    other["metadata"]["annotations"] = {}
    assert len(w.applied_zone_set(_applied() + [other])) == len(_applied())


def test_every_proposed_zone_names_its_source(tmp_path):
    sha = _write_artifact(tmp_path / "store", MAP_FILES)
    action = {"metadata": {"name": NAME, "namespace": NS},
              "spec": {"type": "Custom", "customType": "explore-floor"},
              "status": {"phase": "Succeeded",
                         "message": f"map-ref=floor-maps/{NS}/{NAME} map-sha256={sha}"}}
    store = {"spec": {"kind": "VolumeClaim", "volumeClaim": {"claimName": "floor-maps",
                                                             "accessMode": "ReadWriteOnce"}}}
    cm = w.build_proposal(action, store=store, config={}, tools=_FakeTools(),
                          mount=tmp_path / "store", claim="floor-maps", work=tmp_path / "work")
    for z in yaml.safe_load_all(cm["data"]["zones.yaml"]):
        if z:
            ann = z["metadata"]["annotations"]
            assert ann[w.SOURCE_ANNOTATION] == f"{NS}/{NAME}"
            assert ann[w.DIGEST_ANNOTATION] == sha


def test_a_keepout_set_without_a_recorded_mark_carries_no_theta0(tmp_path):
    # theta0 is the robot's yaw on the mark in the zones' map frame. A map that records no mark
    # (the simulation adapter's) gives none, and none is invented: a reader that needs it
    # refuses the set by name.
    geo = w.MapGeometry(width=40, height=40, resolution=0.05, origin_x=0.0, origin_y=0.0)
    ref, _, zset = w.write_keepouts(tmp_path, "floor-maps", NS, _applied(), geo,
                                    source=f"{NS}/{NAME}", theta0_deg=None)
    import json
    doc = json.loads((tmp_path / NS / f"keepouts-{zset[:16]}" / "keepouts.json").read_text())
    assert "theta0_deg" not in doc and doc["source_map"] == f"{NS}/{NAME}"


def _segment(root, name, mark_yaw, resumed_from=""):
    """A saved segment in the store: segment.json listed in its own manifest."""
    body = json.dumps({"action": f"{NS}/{name}", "mark": [0.0, 0.0, mark_yaw],
                       "resumed_from": resumed_from}).encode()
    _write_artifact(root, {"segment.json": body}, name=name)
    return root / NS / name


@pytest.mark.parametrize("segment, want", [
    (b'{"mark": [0.01, -0.02, -73.85], "resumed_from": ""}', -73.85),
    (None, None),
    (b'{"mark": [0.0, 0.0]}', "refused"),
    (b'{"mark": [0.0, 0.0, "north"]}', "refused"),
    (b'not json', "refused"),
])
def test_theta0_is_the_mark_yaw_a_fresh_segment_saved(tmp_path, segment, want):
    d = tmp_path / "copy"
    d.mkdir()
    for name, data in dict(MAP_FILES, **({"segment.json": segment} if segment else {})).items():
        (d / name).write_bytes(data)
    if want == "refused":
        with pytest.raises(w.RefusalError, match="segment.json"):
            w.source_mark_yaw(d, tmp_path / "store")
    else:
        assert w.source_mark_yaw(d, tmp_path / "store") == want


def test_a_resumed_segments_theta0_is_its_fresh_segments_mark_yaw(tmp_path):
    # A resumed map is in its fresh segment's frame. The robot was placed on the mark by hand
    # only for the fresh segment; a resumed one starts wherever the return left it facing.
    store = tmp_path / "store"
    _segment(store, "explore-first", 67.961)
    _segment(store, "explore-resume", -79.832, resumed_from=f"{NS}/explore-first")
    last = _segment(store, "explore-resume-2", 12.0, resumed_from=f"{NS}/explore-resume")
    assert w.source_mark_yaw(last, store) == 67.961


@pytest.mark.parametrize("chain, why", [
    ([("explore-resume", 1.0, f"{NS}/gone")], "gone"),
    ([("a", 1.0, f"{NS}/b"), ("b", 2.0, f"{NS}/a")], "loop"),
    ([("a", 1.0, "../../etc")], "not a segment"),
])
def test_a_resume_chain_that_cannot_reach_a_fresh_segment_is_refused(tmp_path, chain, why):
    store = tmp_path / "store"
    for name, yaw, prev in chain:
        last = _segment(store, name, yaw, resumed_from=prev)
    with pytest.raises(w.RefusalError, match=why):
        w.source_mark_yaw(last, store)


def test_a_chain_segment_changed_after_its_manifest_is_refused(tmp_path):
    store = tmp_path / "store"
    root = _segment(store, "explore-first", 67.961)
    last = _segment(store, "explore-resume", -79.8, resumed_from=f"{NS}/explore-first")
    (root / "segment.json").write_text('{"mark": [0, 0, 1.0], "resumed_from": ""}')
    with pytest.raises(w.RefusalError, match="hashes to"):
        w.source_mark_yaw(last, store)


class _FakeKube:
    def __init__(self, zones):
        self.zones, self.cms = zones, {}

    def fleetzones(self, ns):
        return self.zones

    def configmap(self, ns, name):
        return self.cms.get(name)

    def create_configmap(self, cm):
        self.cms[cm["metadata"]["name"]] = cm

    replace_configmap = create_configmap


def test_the_published_reference_is_the_one_the_adapter_verifies(tmp_path):
    mark = b'{"mark": [0.0, 0.0, 12.5]}'
    files = {"map.yaml": b"image: map.pgm\nresolution: 0.1\norigin: [0.0, 0.0, 0.0]\n",
             "map.pgm": b"P5\n30 30\n255\n" + bytes(900), "segment.json": mark}
    sha = _write_artifact(tmp_path / "store", files)
    zones = _applied()
    for z in zones:
        z["metadata"]["annotations"][w.DIGEST_ANNOTATION] = sha
    kube = _FakeKube(zones)
    zset = w.render_applied(kube, NS, store="floor-maps", mount=tmp_path / "store",
                            work=tmp_path / "work")
    data = kube.cms[w.KEEPOUTS_CONFIGMAP]["data"]
    name, _, digest = data["keepouts_ref"].partition("@sha256:")
    assert name == f"floor-maps/{NS}/keepouts-{zset[:16]}" and digest == data["keepouts_sha256"]
    root = tmp_path / "store" / NS / f"keepouts-{zset[:16]}"
    assert hashlib.sha256((root / "MANIFEST.sha256").read_bytes()).hexdigest() == digest
    import json
    assert json.loads((root / "keepouts.json").read_text())["theta0_deg"] == 12.5
    assert data["theta0_deg"] == "12.5"
