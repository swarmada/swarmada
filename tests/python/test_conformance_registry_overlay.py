# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The published registry stands alone, and an overlay only adds to it.

``conformance/profiles.yaml`` is published. A specification that is not published
keeps the check identifiers it reserves in an overlay beside its own sources, which
the registry merges when the file is present. Three properties follow, and each is
checked here without a subject, a network or a cluster:

1. With no overlay present -- which is what a public checkout has -- every surface's
   ``*0.5`` drift check passes and no catalog is listed that the checkout lacks.
2. A catalog that is listed and missing is a FAILED ``*0.5`` that says so, never a
   silent skip that reports the catalog's ids as drift under the wrong cause.
3. An overlay adds; it cannot redeclare a profile the published registry declares.
"""
from __future__ import annotations

from pathlib import Path

import pytest

from conformance.model import ResultSet
from conformance.registry import REGISTRY_PATH, Registry, self_checks

ROOT = Path(__file__).resolve().parents[2]

SURFACES = [("FleetAdapter", "RB"), ("ComputeEndpoint", "CE"), ("ControlPlane", "CP")]


def _drift(reg: Registry, surface: str, prefix: str):
    rs = ResultSet(surface)
    self_checks(reg, surface, rs, prefix)
    rows = [r for r in rs.results if r.check_id.endswith("0.5")]
    assert len(rows) == 1, rows
    return rows[0]


@pytest.mark.parametrize("surface,prefix", SURFACES)
def test_the_published_registry_alone_agrees_with_its_catalogs(surface: str, prefix: str) -> None:
    reg = Registry(REGISTRY_PATH, overlays=[])
    for path in reg.catalog_paths(surface):
        assert path.exists(), f"{surface} lists {path.relative_to(ROOT)}, which this checkout lacks"
    row = _drift(reg, surface, prefix)
    assert row.status.value == "pass", f"{surface}: {row.detail}"


def test_a_listed_catalog_that_is_missing_is_a_failed_drift_check(tmp_path: Path) -> None:
    overlay = tmp_path / "conformance-registry.yaml"
    overlay.write_text(
        "surfaces:\n"
        "  - name: ControlPlane\n"
        "    catalogs:\n"
        "      - path: rfcs/no-such-specification/conformance.md\n"
        "        prefixes: [Z]\n"
    )
    reg = Registry(REGISTRY_PATH, overlays=[overlay])
    row = _drift(reg, "ControlPlane", "CP")
    assert row.status.value == "fail"
    assert "does not exist" in row.detail and "no-such-specification" in row.detail, row.detail


def test_an_overlay_cannot_redeclare_a_published_profile(tmp_path: Path) -> None:
    overlay = tmp_path / "conformance-registry.yaml"
    overlay.write_text(
        "surfaces:\n"
        "  - name: ControlPlane\n"
        "    profiles:\n"
        "      - name: attestation\n"
        "        core: [CP0.1]\n"
    )
    with pytest.raises(ValueError, match="already declared"):
        Registry(REGISTRY_PATH, overlays=[overlay])
