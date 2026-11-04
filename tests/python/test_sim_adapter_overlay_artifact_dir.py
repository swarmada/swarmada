# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The in-cluster simulation adapter has a writable directory for the maps it saves.

Why this file exists. The adapter lists ``Custom explore-floor`` in its catalog, and that verb
saves a map. With no ``--artifact-dir`` it falls back to a scratch directory under the system
temporary directory. The overlay runs it with a read-only root filesystem and no ``/tmp``, so
the fallback raised ``FileNotFoundError`` on the first segment and the action was left held.

These tests require that:

- the overlay's Deployment passes ``--artifact-dir``, and mounts a writable ``emptyDir`` at
  exactly that path;
- the first-run kustomization, which restates the container's args whole, passes the same
  ``--artifact-dir``, so the overlay's flag is not dropped there;
- with ``--artifact-dir`` set, a map is saved with no usable system temporary directory.
"""

from __future__ import annotations

import tempfile
from pathlib import Path

import pytest
import yaml

from adapters.simulation import floor_mapping as fm

ROOT = Path(__file__).resolve().parents[2]
OVERLAY = ROOT / "config" / "overlays" / "demo-webhooks" / "sim-adapter" / "adapter.yaml"
FIRST_RUN = ROOT / "examples" / "first-run" / "sim-adapter" / "kustomization.yaml"
FLAG = "--artifact-dir="


def _artifact_dir(args: list[str]) -> str:
    dirs = [a[len(FLAG):] for a in args if a.startswith(FLAG)]
    assert len(dirs) == 1, f"want one {FLAG}<path> argument, got {dirs} in {args}"
    return dirs[0]


def _adapter(spec: dict) -> dict:
    return next(c for c in spec["containers"] if c["name"] == "adapter")


def _deployment() -> dict:
    docs = [d for d in yaml.safe_load_all(OVERLAY.read_text(encoding="utf-8")) if d]
    return next(d for d in docs if d["kind"] == "Deployment")["spec"]["template"]["spec"]


def test_the_overlay_mounts_a_writable_emptydir_at_the_artifact_dir():
    pod = _deployment()
    container = _adapter(pod)
    assert container["securityContext"]["readOnlyRootFilesystem"] is True, (
        "this test exists because the root filesystem is read-only")
    path = _artifact_dir(container["args"])
    mount = next((m for m in container.get("volumeMounts", []) if m["mountPath"] == path), None)
    assert mount is not None, f"nothing is mounted at the artifact dir {path}"
    assert not mount.get("readOnly", False), f"the mount at {path} is read-only"
    volume = next(v for v in pod["volumes"] if v["name"] == mount["name"])
    assert "emptyDir" in volume, f"volume {mount['name']} at {path} is not an emptyDir"


def test_first_run_keeps_the_overlay_artifact_dir():
    kust = yaml.safe_load(FIRST_RUN.read_text(encoding="utf-8"))
    patched = [yaml.safe_load(p["patch"]) for p in kust["patches"]]
    deployment = next(p for p in patched if p.get("kind") == "Deployment")
    container = _adapter(deployment["spec"]["template"]["spec"])
    assert _artifact_dir(container["args"]) == _artifact_dir(_adapter(_deployment())["args"])


def test_a_map_is_saved_with_no_usable_temporary_directory(tmp_path, monkeypatch):
    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path / "no-such-tmp"))
    with pytest.raises(FileNotFoundError):
        fm.ArtifactDir(None, "sim-scratch")
    root = tmp_path / "artifacts"
    root.mkdir()
    ref, digest = fm.ArtifactDir(str(root), "sim-scratch").save("warehouse-a", "seg-1",
                                                                {"map.pgm": b"P5\n1 1\n255\n\0"})
    assert (root / "warehouse-a" / "seg-1" / "map.pgm").is_file()
    assert ref and digest
