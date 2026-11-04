# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Ingests a Fleet Adapter harness run and re-expresses it as a ConformanceReport.

``adapters/conformance`` drives ``C0``-``C16`` and writes a flat JSON result set.
This module reads that file rather than importing the harness, so the profile
layer and the adapter harness stay independently runnable -- and so a vendor can
profile a report produced on another machine, at another time, by a harness
revision they did not run.

It adds no adapter check and changes no verdict. It groups what already ran.
"""

from __future__ import annotations

import json
from pathlib import Path

from .model import CheckResult, CheckStatus, ResultSet, SkipCause

#: The adapter harness records level strings this suite already understands.
LEVELS = {"MUST", "MUST NOT", "SHOULD", "MAY"}


def ingest(path: Path, rs: ResultSet) -> dict:
    # Recorded as the SUBJECT of the run, not as an artefact of the suite. The
    # standalone check asks whether the suite depends on anything outside the
    # public tree; the report under test is the thing being profiled, exactly
    # like an endpoint named with --target, and an operator keeping it in /tmp
    # is not a dependency on a private repository.
    rs.context["subject"] = str(path)
    doc = json.loads(Path(path).read_text())
    for row in doc["results"]:
        level = row.get("level", "MUST")
        # C0.2-C0.4 are harness context and do not count toward conformance; the
        # registry excludes them, and marking the level here keeps them out of
        # the text rendering too.
        if row["check_id"] in ("C0.2", "C0.3", "C0.4"):
            level = "context"
        rs.results.append(CheckResult(
            check_id=row["check_id"], title=row["title"],
            status=CheckStatus(row["status"]), level=level if level in LEVELS or
            level == "context" else "MUST", detail=row.get("detail", "")))
    rs.context.update({
        "adapter": doc.get("adapter"),
        "surface_driven": True,
        "honoured_operating_constraints": doc.get("honoured_operating_constraints"),
        "measurements_by_check": measurements_by_check(doc),
    })
    return doc


def measurements_by_check(doc: dict) -> dict[str, dict]:
    """The harness's measured numbers, filed under the check that measured them.

    A number the harness did not attribute to a check is left out of every row
    rather than attached by guesswork; it stays in the harness JSON it came from.
    """
    owners = doc.get("measurement_checks") or {}
    out: dict[str, dict] = {}
    for key, value in (doc.get("measurements") or {}).items():
        cid = owners.get(key)
        if cid:
            out.setdefault(cid, {})[key] = value
    return out


#: Every Core row that cannot be established on a platform with no motion channel
#: independent of the command path. ADR-0071's precondition says such a platform cannot
#: satisfy C5.2 and C5.3 together and that the honest terminal answer to every estop on it
#: is FAILED; these are the rows that answer then lands on.
#:
#:   C5.2   the confirmed EstopAck (STOPPED) on SafetyStream
#:   C5.3   STOPPED is adapter-confirmed, not inferred -- the row the precondition
#:          was first written for
#:   C8.3   the same estop, confirmed on the edge stream
#:   C14.1  the same estop again, against an adapter that failed the version gate
#:   C3.6   a cancel's CancelDisposition. The enum has no value meaning "stop commanded,
#:          rest unwitnessable", so an adapter that will not claim STOPPED_SAFELY on this
#:          platform can send none; the missing value is queued as a contract change and
#:          the row is graded from the platform until it lands
#:
#: C16.1 IS DELIBERATELY NOT HERE. An unacknowledged cancel that emits no terminal
#: ActionStatusUpdate leaves the control plane without an outcome whatever the drivetrain
#: can witness, so it was an adapter gap, it was fixed, and it stays graded from the wire.
#: A platform precondition that swallowed it would hide the one row of the five that a
#: robot with no encoder does not explain.
PLATFORM_PRECONDITION_CHECKS = ("C3.6", "C5.2", "C5.3", "C8.3", "C14.1")


def apply_platform_precondition(rs: ResultSet, plant: dict | None) -> str | None:
    """Grade the ADR-0071 platform precondition from plant.yaml, over the rows it reaches.

    The `safety` profile is claimable only on a platform with at least one motion channel
    independent of the command path that can confirm rest. `plant.yaml` records, per
    odometry topic, whether the guided motion test found it `measured` or `command-derived`
    (conformance/PLANT.md, `motion_channel`). When every odometry topic is `command-derived`
    and none is `measured`, the platform has nothing that can confirm rest, a STOPPED on it
    is the inference C5.3 forbids however the wire looks, and every row in
    ``PLATFORM_PRECONDITION_CHECKS`` is re-recorded as a skip with
    `no-independent-motion-channel` -- graded `not-supported-by-platform` and kept OUT of the
    never-an-obligation set, so the verdict cannot reach `pass`.

    Widened from C5.3 alone to the five rows above. With one row re-graded and the rest left
    as the wire recorded them, an adapter giving ADR-0071's honest answer on such a platform
    read as four failed Core rows -- which tells a reader the adapter has four defects when
    what it has is a robot with no encoder, and makes "claim STOPPED anyway" the cheapest way
    to a green run. The verdict is unchanged in kind: still short of `pass`, and for the
    reason the platform gives rather than one the adapter is blamed for.

    A plant WITH a measured channel is not excused anything: each row keeps the status the
    run recorded, and gains a note saying the precondition was met, so a failure there reads
    as what it is.

    Returns the finding it applied -- "met", "unmet", "unknown" -- or None when there is no
    plant declaration or the run recorded none of these rows. One answer for the plant, not
    one per row: the precondition is a property of the platform.
    """
    if not plant:
        return None
    odometry = ((plant.get("motion_channel") or {}).get("odometry") or {})
    values = {topic: (field or {}).get("value") if isinstance(field, dict) else field
              for topic, field in odometry.items()}
    measured = [t for t, v in values.items() if v == "measured"]
    derived = [t for t, v in values.items() if v == "command-derived"]
    unmet = bool(derived) and len(derived) == len(values)
    if measured:
        finding = "met"
        note = (f"platform precondition met: plant.yaml motion_channel shows {measured} "
                f"measured")
    elif unmet:
        finding, note = "unmet", None
    else:
        finding = "unknown"
        note = ("platform precondition not established: plant.yaml records no measured or "
                "command-derived odometry topic")

    seen = False
    for i, r in enumerate(rs.results):
        if r.check_id not in PLATFORM_PRECONDITION_CHECKS:
            continue
        seen = True
        if unmet:
            rs.results[i] = CheckResult(
                check_id=r.check_id, title=r.title, status=CheckStatus.SKIP, level=r.level,
                detail=(f"{SkipCause.NO_INDEPENDENT_MOTION_CHANNEL.value}: plant.yaml "
                        f"motion_channel shows every odometry topic command-derived "
                        f"({derived}) and none measured, so nothing on this platform can "
                        f"confirm rest. What the run observed: {r.detail}"))
        else:
            rs.results[i] = CheckResult(check_id=r.check_id, title=r.title, status=r.status,
                                        level=r.level, detail=f"{r.detail}; {note}")
    return finding if seen else None


def applicability(rs: ResultSet) -> dict[str, bool]:
    """Which conditional FleetAdapter profiles applied in this run."""
    by_id = rs.by_id()
    # `edge` applies when the adapter was advertised an edge endpoint and the
    # run actually exercised it. A C8 row that never ran means the harness could
    # not establish that the profile applied -- which is a different fact from
    # the profile applying and failing.
    edge = any(c in by_id and by_id[c].status is not CheckStatus.SKIP
               for c in ("C8.1", "C8.2", "C8.3"))
    # `zone-restrictions` IS NO LONGER CONDITIONAL, corrected 2026-09-14. It applied when the
    # adapter declared at least one honoured operating subject, which the specification's
    # conformance-profiles chapter stated and which contradicted its own next sentence: the
    # load-bearing case is a run going RED against an adapter that ACCEPTS AND IGNORES a
    # rule, and that is an adapter declaring NOTHING. The profile reported notApplicable for
    # exactly the adapters whose red run it demanded.
    #
    # FA-R5's antecedent has no declaring clause, and no row in the profile needed the
    # conditional: C17.1/C17.2 have no declaring antecedent, C17.3 grades the shape of
    # whatever was declared and declaring none passes, and C17.4 carries its own enumerated
    # skip cause for an absent declaration. So the profile is always applicable and this
    # function no longer returns a verdict for it -- an unconditional profile has no
    # applicability question to answer.
    # RFC-0003's Event Source checks E0-E4 are no longer a profile: they are
    # reserved-not-implemented as exclusions in that specification's registry
    # overlay, because nothing in the tree implements the mechanism they grade. There is no
    # applicability verdict to report for a profile the registry does not
    # declare, so none is returned.
    return {"edge": edge}
