# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The profile registry: profiles.yaml, the catalog documents, and the self-checks.

Three things live here.

1. Loading ``profiles.yaml`` and answering "which profile and level does this
   check belong to".
2. Turning a flat ``ResultSet`` into per-profile Core/Extended results.
3. The report-integrity self-checks every surface carries -- ``*0.1`` (skip
   causes consistent), ``*0.3`` (the run stands alone and names no excluded
   specification) and ``*0.5`` (the registry and the catalogs agree). These are
   the checks that keep the program honest about itself, so they run on every
   report and they are Core everywhere.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass
from pathlib import Path

import yaml

from .model import CheckStatus, ProfileResult, ResultSet, SkipCause, _level

HERE = Path(__file__).resolve().parent
REPO = HERE.parent
REGISTRY_PATH = HERE / "profiles.yaml"

#: Where a specification outside the published contract (a draft for review) keeps the check
#: identifiers it reserves. Each
#: overlay found is merged into the registry (see ``profiles.yaml``, "Normative
#: sources"), so this module names no such specification itself.
OVERLAY_GLOB = "rfcs/*/conformance-registry.yaml"

#: How a catalog document declares a check: ``- **CE1.1 (MUST)** ...``
CHECK_RE = re.compile(r"^-\s+\*\*([A-Z]{1,2}\d+\.\d+)\s*\(([^)]*)\)\*\*\s*(.*)$")


@dataclass
class CheckBinding:
    profile: str
    level: str            # "core" | "extended"
    feature: str | None


class Registry:
    def __init__(self, path: Path = REGISTRY_PATH,
                 overlays: list[Path] | None = None) -> None:
        self.path = path
        self.doc = yaml.safe_load(path.read_text())
        self.revision: str = str(self.doc["revision"])
        self._by_surface = {s["name"]: s for s in self.doc["surfaces"]}
        self.overlays = sorted(REPO.glob(OVERLAY_GLOB)) if overlays is None else list(overlays)
        for overlay in self.overlays:
            self._merge(overlay)

    def _merge(self, overlay: Path) -> None:
        """Append an overlay's catalogs, profiles and exclusions to its surfaces.

        Additions only. An overlay that names a surface the registry does not
        declare, or a profile the registry already declares, is an error: it would
        otherwise change what the published registry says rather than add to it.
        """
        doc = yaml.safe_load(overlay.read_text()) or {}
        for add in doc.get("surfaces") or []:
            name = add["name"]
            if name not in self._by_surface:
                raise ValueError(f"{overlay}: surface {name!r} is not declared in {self.path.name}")
            surface = self._by_surface[name]
            have = {p["name"] for p in surface.get("profiles") or []}
            for prof in add.get("profiles") or []:
                if prof["name"] in have:
                    raise ValueError(f"{overlay}: profile {prof['name']!r} is already declared "
                                     f"on {name} in {self.path.name}")
            for key in ("catalogs", "profiles", "excluded"):
                if add.get(key):
                    surface[key] = list(surface.get(key) or []) + list(add[key])

    # ── lookups ──────────────────────────────────────────────────────────────

    def surface(self, name: str) -> dict:
        return self._by_surface[name]

    def bindings(self, surface: str) -> dict[str, CheckBinding]:
        """check id -> (profile, level, feature)."""
        out: dict[str, CheckBinding] = {}
        for prof in self.surface(surface)["profiles"]:
            feature_of: dict[str, str] = {}
            for feat, ids in (prof.get("features") or {}).items():
                for cid in ids:
                    feature_of[cid] = feat
            for level in ("core", "extended"):
                for cid in prof.get(level) or []:
                    out[cid] = CheckBinding(prof["name"], level, feature_of.get(cid))
        return out

    def excluded(self, surface: str) -> dict[str, str]:
        out: dict[str, str] = {}
        for entry in self.surface(surface).get("excluded") or []:
            for cid in entry["checks"]:
                out[cid] = entry["reason"]
        return out

    def excluded_specification_names(self) -> list[str]:
        names: list[str] = []
        for spec in self.doc.get("excludedSpecifications") or []:
            names.append(spec["name"])
            names.extend(spec.get("aliases") or [])
        return names

    def all_check_ids(self) -> set[str]:
        """Every check id the registry knows, across every surface.

        The ``*0.5`` drift check needs this direction to be registry-WIDE: a
        catalog shared by two surfaces (RFC-0003's, which defines both an Event
        Source and a Trigger Consumer profile) defines checks that belong to a
        surface other than the one under test, and reading those as drift would
        make a correct registry fail.
        """
        out: set[str] = set()
        for surface in self._by_surface:
            out |= set(self.bindings(surface)) | set(self.excluded(surface))
        return out

    def catalog_paths(self, surface: str) -> list[Path]:
        return [REPO / c["path"] for c in self.surface(surface)["catalogs"]]

    def catalog_prefixes(self, surface: str) -> list[str]:
        out: list[str] = []
        for c in self.surface(surface)["catalogs"]:
            out.extend(c["prefixes"])
        return out

    # ── aggregation ──────────────────────────────────────────────────────────

    def profile_results(self, surface: str, rs: ResultSet, *, tier: str,
                        applicable: dict[str, bool]) -> list[ProfileResult]:
        """Group a flat ResultSet into per-profile Core/Extended results.

        ``applicable`` carries the driver's verdict for conditional profiles. A
        conditional profile with no entry is treated as NOT applicable, because
        a driver that never considered a profile has not established that it
        applied -- and reporting it as applied-and-unverified would be a
        stronger claim than the run supports.
        """
        by_id = rs.by_id()
        tier_requires = self.surface(surface).get("tierRequires") or {}
        required_names = set(tier_requires.get(tier, [])) if tier_requires else None

        out: list[ProfileResult] = []
        for prof in self.surface(surface)["profiles"]:
            name = prof["name"]
            core_ids = prof.get("core") or []
            ext_ids = prof.get("extended") or []
            conditional = prof.get("conditional")
            never_core = bool(prof.get("neverCore"))

            if required_names is not None:
                required = name in required_names
            else:
                # Untiered surfaces: a profile carrying Core checks is required
                # when it applies; an Extended-only profile never is.
                required = bool(core_ids)
            if never_core:
                required = False
            # The edge profile is required at EVERY tier when it applies: the
            # position tee and the edge-issued estop are safety mechanisms and
            # do not depend on dispatch authority.
            if surface == "FleetAdapter" and name == "edge":
                required = True

            is_applicable = applicable.get(name, not conditional)
            if not is_applicable:
                out.append(ProfileResult(name, required=False, applicable=False,
                                         never_core=never_core))
                continue

            core = _level([by_id[c] for c in core_ids if c in by_id]) if core_ids else None
            if core is not None:
                # Every Core id the registry assigns, recorded or not: the verdict must
                # see a Core check that produced no row (ADR-0070).
                core.check_ids = list(core_ids)
            ext = None
            if ext_ids:
                ext = _level([by_id[c] for c in ext_ids if c in by_id], level="extended")
                supported, unsupported = [], []
                for feat, ids in (prof.get("features") or {}).items():
                    rows = [by_id[c] for c in ids if c in by_id]
                    # A feature is SUPPORTED when at least one of its checks
                    # passed, none failed and none was declined; otherwise
                    # unsupported. Every feature lands in exactly one list -- a
                    # feature in neither is a malformed report (PROFILES.md).
                    # The decline test matters for a check two features share
                    # (C10.2, the terminal outcome of an install of either kind):
                    # its pass on one command must not make the feature whose
                    # own command was declined read as supported.
                    passed = any(r.status is CheckStatus.PASS for r in rows)
                    failed = any(r.status is CheckStatus.FAIL for r in rows)
                    declined = any(r.status is CheckStatus.SKIP
                                   and SkipCause.of(r.detail) is SkipCause.OPTIONAL_DECLINED
                                   for r in rows)
                    (supported if (passed and not failed and not declined)
                     else unsupported).append(feat)
                ext.supported_features = sorted(supported)
                ext.unsupported_features = sorted(unsupported)
            out.append(ProfileResult(name, required=required, applicable=True,
                                     core=core, extended=ext, never_core=never_core))
        return out


# ── The shared report-integrity self-checks ──────────────────────────────────

def _catalog_check_ids(paths: list[Path], prefixes: list[str]) -> dict[str, str]:
    """Parse ``- **ID (LEVEL)** title`` rows out of the catalog documents.

    A listed catalog that does not exist raises: skipping it would drop every id it
    defines and report the registry's rows for them as drift, naming the wrong cause.
    """
    found: dict[str, str] = {}
    for p in paths:
        if not p.exists():
            raise FileNotFoundError(f"the registry lists a catalog that does not exist: "
                                    f"{os.path.relpath(p, REPO)}")
        for line in p.read_text().splitlines():
            m = CHECK_RE.match(line.strip())
            if not m:
                continue
            cid = m.group(1)
            if any(cid.startswith(pre) and cid[len(pre)].isdigit() for pre in prefixes):
                found[cid] = m.group(3).strip()
    return found


def _cause_is_checkable(rs: ResultSet, cid: str, cause: str) -> bool:
    """Whether ``*0.1`` has a test that can contradict ``cause`` recorded on row ``cid``.

    Kept beside the tests in ``self_checks`` and in step with them: a cause they cannot
    contradict in this run must not be counted as checked.
    """
    if cause == SkipCause.SURFACE_NOT_IMPLEMENTED.value:
        return "surface_driven" in rs.context
    if cause == SkipCause.BINDING_NOT_DECLARED.value:
        return bool(rs.context.get("binding")) and cid.startswith(("CE3", "CE4", "CE5"))
    if cause == SkipCause.CONTRACT_NOT_DECLARED.value:
        return "contract" in rs.context and cid.startswith("CE7")
    return False


def self_checks(reg: Registry, surface: str, rs: ResultSet, prefix: str,
                *, include_skip_check: bool = True) -> None:
    """Record ``*0.1``, ``*0.3`` and ``*0.5`` on ``rs``.

    ``prefix`` is the surface's check prefix ("CE", "RB", "CP"). The three ids
    differ per surface only in that prefix; their meaning does not.

    ``include_skip_check`` is off for the FleetAdapter surface: the adapter
    harness records the skip-consistency verdict itself, as ``C0.1``, and a
    second row asserting the same property under a different id would be two
    checks where the catalog defines one.
    """
    ids = {"skips": f"{prefix}0.1", "standalone": f"{prefix}0.3", "drift": f"{prefix}0.5"}
    # RB leads with coverage, so its integrity rows shift by one. Stated here
    # rather than left for a reader to infer from the catalog.
    if prefix == "RB":
        ids = {"skips": "RB0.2", "standalone": "RB0.3", "drift": "RB0.5"}

    # ── *0.1 — skip causes consistent with the run ───────────────────────────
    contradictions: list[str] = []
    causes = rs.skip_causes()
    for cid, cause in causes:
        if cause == SkipCause.SURFACE_NOT_IMPLEMENTED.value and rs.context.get("surface_driven"):
            contradictions.append(f"{cid} claims {cause} in a run that drove the surface")
        if cause == SkipCause.BINDING_NOT_DECLARED.value:
            declared = (rs.context.get("binding") or "").lower()
            family = {"http": "CE3", "grpc": "CE4", "mcp": "CE5"}.get(declared)
            if family and cid.startswith(family):
                contradictions.append(
                    f"{cid} claims {cause} for the binding the endpoint DID declare "
                    f"({declared})")
        if cause == SkipCause.CONTRACT_NOT_DECLARED.value:
            if (rs.context.get("contract") == "Service") and cid.startswith("CE7"):
                contradictions.append(
                    f"{cid} claims {cause} for the contract the endpoint DID declare")
    if not include_skip_check:
        pass
    elif contradictions:
        rs.bad(ids["skips"], "Skip reasons are consistent with the run",
               "; ".join(contradictions))
    elif include_skip_check:
        # Say how many of the recorded causes the tests above can contradict, not only how
        # many were recorded: the rest are recorded, not checked, and a count that folded
        # them in would claim a cross-check this run did not make.
        checkable = [(cid, cause) for cid, cause in causes if _cause_is_checkable(rs, cid, cause)]
        unchecked = sorted({cause for cid, cause in causes
                            if not _cause_is_checkable(rs, cid, cause)})
        rs.ok(ids["skips"], "Skip reasons are consistent with the run",
              f"{len(checkable)} of {len(causes)} recorded skip cause(s) are of a kind this run "
              "can contradict, and none was contradicted"
              + (f". The other {len(causes) - len(checkable)} "
                 f"{'is' if len(causes) - len(checkable) == 1 else 'are'} recorded, not checked: "
                 f"{unchecked}" if unchecked else ""))

    # ── *0.3 — the run stands alone, and names no excluded specification ─────
    problems: list[str] = []
    for path in rs.artefacts:
        resolved = Path(path).resolve()
        try:
            resolved.relative_to(REPO)
        except ValueError:
            problems.append(f"read an artefact outside the public tree: {resolved}")
    banned = [n.lower() for n in reg.excluded_specification_names()]
    haystack: list[tuple[str, str]] = []
    for prof in reg.surface(surface)["profiles"]:
        haystack.append(("profile name", prof["name"]))
        for feat in (prof.get("features") or {}):
            haystack.append(("feature name", feat))
    for r in rs.results:
        haystack.append(("check id", r.check_id))
        haystack.append(("check title", r.title))
    for path in rs.artefacts:
        haystack.append(("artefact path", path))
    for where, text in haystack:
        low = text.lower()
        for name in banned:
            if re.search(rf"(?<![a-z0-9]){re.escape(name)}(?![a-z0-9])", low):
                problems.append(f"{where} names an excluded specification: {text!r}")
    if problems:
        rs.bad(ids["standalone"], "The run stands alone and names no unreadable specification",
               "; ".join(sorted(set(problems))))
    else:
        subject = rs.context.get("subject")
        rs.ok(ids["standalone"], "The run stands alone and names no unreadable specification",
              f"{len(rs.artefacts)} suite artefact(s) read, all inside the repository; "
              f"{len(banned)} excluded name(s) absent from every profile, check and path"
              + (f"; subject under test: {subject}" if subject else "") +
              ". LIMIT: this establishes that the SUITE needs nothing outside the public "
              "tree. It cannot see inside a subject an operator supplies with --target or "
              "--adapter-report, and makes no claim about that subject's own dependencies")

    # ── *0.5 — the registry and the catalogs agree ───────────────────────────
    prefixes = reg.catalog_prefixes(surface)
    try:
        catalog = _catalog_check_ids(reg.catalog_paths(surface), prefixes)
    except FileNotFoundError as exc:
        rs.bad(ids["drift"], "The registry and the catalog documents agree", str(exc))
        return
    registry_ids = set(reg.bindings(surface)) | set(reg.excluded(surface))
    # Only compare ids this surface's own catalogs own. A surface that imports a
    # catalog by reference (ControlPlane importing RFC-0003's E-checks) is
    # compared against that catalog too, since it is listed in `catalogs`.
    registry_ids = {c for c in registry_ids
                    if any(c.startswith(p) and c[len(p)].isdigit() for p in prefixes)}
    missing_from_catalog = sorted(registry_ids - set(catalog))
    missing_from_registry = sorted(set(catalog) - reg.all_check_ids())
    if missing_from_catalog or missing_from_registry:
        detail = []
        if missing_from_catalog:
            detail.append(f"in the registry, undefined in the catalog: {missing_from_catalog}")
        if missing_from_registry:
            detail.append(f"defined in the catalog, absent from the registry: "
                          f"{missing_from_registry}")
        rs.bad(ids["drift"], "The registry and the catalog documents agree", "; ".join(detail))
    else:
        rs.ok(ids["drift"], "The registry and the catalog documents agree",
              f"{len(registry_ids)} check(s) matched across "
              f"{', '.join(os.path.relpath(p, REPO) for p in reg.catalog_paths(surface))}")
