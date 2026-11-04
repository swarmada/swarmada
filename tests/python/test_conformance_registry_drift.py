# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The profile registry covers every check its catalogs define, on every surface.

Why this file exists. `profiles.yaml` calls itself the SPINE of the conformance
program and says the `*0.5` self-check makes drift "a FAILED check and not a
review comment". It does -- but only for someone who RUNS the suite, and nothing
in `make test` ran the profile layer. So the guard existed and never fired.

It was not hypothetical. `adapters/CONFORMANCE.md` grew the `C18` family (sensor
streams and the duty cycle), the FleetAdapter harness implemented all five rows,
and they RAN and PASSED -- while `profiles.yaml` assigned them to no profile. The
profile layer therefore dropped five executed MUSTs on the floor, and `RB0.5`
failed the `attestation` Core profile on every adapter report produced in that
window. The drift was found by generating a report by hand, which is exactly the
review comment the file says this must not be.

These tests run the same comparison `*0.5` runs, for all three surfaces, with no
adapter, no endpoint, no cluster and no network -- because it is a pure
registry-versus-prose comparison and nothing about it needs a subject under test.
A new check family added to a catalog now fails here, at `make test`, rather than
in whichever report a vendor generates first.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from conformance.model import ResultSet
from conformance.registry import Registry, self_checks

ROOT = Path(__file__).resolve().parents[2]

#: Every surface the registry defines, and the check-id prefix its self-checks use.
#: Read from the registry rather than hard-coded, so a NEW surface is covered by
#: these tests the moment it is declared -- a surface nobody added a test for is
#: the same hole this file exists to close.
SURFACES = [
    ("FleetAdapter", "RB"),  # the adapter surface reports its self-checks as RB*
    ("ComputeEndpoint", "CE"),
    ("ControlPlane", "CP"),
]


@pytest.fixture(scope="module")
def registry() -> Registry:
    return Registry(ROOT / "conformance" / "profiles.yaml")


def test_every_declared_surface_is_covered_by_this_file(registry: Registry) -> None:
    """A surface added to the registry and not to SURFACES is untested drift."""
    declared = {s["name"] for s in registry.doc["surfaces"]}
    covered = {name for name, _ in SURFACES}
    assert declared == covered, (
        f"the registry declares surfaces {sorted(declared - covered)} that this file "
        f"does not check, or this file names {sorted(covered - declared)} that the "
        f"registry does not declare. Either way the *0.5 guard is not running "
        f"everywhere it claims to"
    )


@pytest.mark.parametrize("surface,prefix", SURFACES)
def test_registry_and_catalog_agree(registry: Registry, surface: str, prefix: str) -> None:
    """`*0.5`, run as a unit test: no subject, no network, no cluster.

    A check defined in a catalog and assigned to no profile is INVISIBLE to the
    profile layer: it may have run and passed, and the report will neither count
    it nor mention it. A check assigned in the registry and defined in no catalog
    is the mirror -- a profile grading something no prose obliges.
    """
    rs = ResultSet(surface)
    self_checks(registry, surface, rs, prefix)

    drift = [r for r in rs.results if r.check_id.endswith("0.5")]
    assert len(drift) == 1, f"expected exactly one *0.5 result for {surface}, got {drift}"

    result = drift[0]
    assert result.status.value == "pass", (
        f"{surface}: {result.detail}\n\n"
        f"The registry and {os.path.basename(str(registry.catalog_paths(surface)[0]))} "
        f"disagree. If a catalog grew a check family, assign it to a profile in "
        f"conformance/profiles.yaml (and bump `revision`, per that file's own rule that "
        f"a change to what an implementation must do to pass invalidates earlier "
        f"results). If a check was withdrawn from a catalog, remove it from the registry "
        f"or move it to `excluded` with the reason. Do NOT narrow this test."
    )


def test_c18_sensor_streams_is_assigned(registry: Registry) -> None:
    """The specific drift this file was written for, pinned by name.

    Kept as its own case rather than trusted to the sweep above: the sweep would
    also pass if C18 were quietly deleted from the catalog, and that is a
    different and worse resolution of the same failure.
    """
    bindings = registry.bindings("FleetAdapter")
    for cid in ("C18.1", "C18.2", "C18.3", "C18.4", "C18.5"):
        assert cid in bindings, (
            f"{cid} is defined in adapters/CONFORMANCE.md and implemented in the "
            f"FleetAdapter harness, but the registry assigns it to no profile. It will "
            f"run, pass, and be dropped by the profile layer"
        )
        assert bindings[cid].profile == "sensor-streams", (
            f"{cid} is assigned to profile {bindings[cid].profile!r}, expected " f"'sensor-streams'"
        )


def test_the_duty_cycle_and_the_failsafe_are_separate_features(registry: Registry) -> None:
    """C18.5 and C18.3 must not share a feature.

    The catalog draws this line itself -- "the lease is the FAIL-SAFE, not the
    ordinary path" -- and the reason is operational: an adapter can stop cleanly
    on DeactivateStream and still never stop when the control plane goes silent,
    leaving a sensor powered for a whole lease every time. One feature spanning
    both would let a green C18.5 report the fail-safe as held, which is the
    failure the split exists to prevent.
    """
    profiles = {p["name"]: p for p in registry.surface("FleetAdapter")["profiles"]}
    assert "sensor-streams" in profiles, (
        "the FleetAdapter surface declares no 'sensor-streams' profile, so the C18 "
        "family is assigned nowhere — see test_c18_sensor_streams_is_assigned"
    )
    feats = profiles["sensor-streams"].get("features") or {}
    owning = {
        name: checks for name, checks in feats.items() if "C18.3" in checks or "C18.5" in checks
    }
    assert len(owning) == 2, (
        f"C18.3 (stop when abandoned) and C18.5 (stop on command) are reported under "
        f"{owning} — they must be separate features"
    )
    for name, checks in owning.items():
        assert not (
            "C18.3" in checks and "C18.5" in checks
        ), f"feature {name!r} spans both the fail-safe and the ordinary path: {checks}"
