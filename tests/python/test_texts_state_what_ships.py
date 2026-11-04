# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Published sentences that had drifted from the code they describe, each pinned to that code.

The RFC is read from the assembled dist, which is what ships; the chapter sources do not.

1. A config comment cites the FleetAdapter admission gate by its anchor. It cited a section
   number whose heading is the two-phase discovery rationale.
2. The DiscoveredRobot chapter says the registrar finds a Robot through its
   `swarmada.io/robot-id` annotation (`robotid.Lookup`). It said the lookup was by object name
   and that the annotation was read nowhere in the registrar.
3. The operator texts on the audit chain say what the RFC says: `verify audit` does not detect
   a log whose newest entries were removed.
4. The SwarmadaConfig conditions table names `SigningIncomplete` and the reasons the controller
   writes.
5. The fleet-adapter protocol's rejection table names every `AssignActionRejection` value the
   proto defines.
6. The three external adapter rows in the registry say plainly that they are not qualified at
   the current contract.
7. No text tells an operator that a robot holding a stop can always be released and then
   deleted: one that never reports again stays held. Nor does one promise a command of a later
   draft that would remove it: no command decommissions one robot.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DIST = ROOT / "rfcs" / "dist" / "RFC-0001-core-spec.md"


def _read(rel: str) -> str:
    return (ROOT / rel).read_text(encoding="utf-8")


def _flat(text: str) -> str:
    return " ".join(text.split())


def _says(text: str, phrase: str) -> bool:
    # A plain bool, so a failing assertion does not have pytest diff a whole document.
    return phrase in text


def _dist_section(anchor: str) -> str:
    """The dist text under `anchor`'s heading, up to the next heading of the same or a higher
    level. A `#` line inside a fenced block (a YAML comment) is not a heading."""
    lines = DIST.read_text(encoding="utf-8").splitlines()
    at = lines.index(f'<a id="{anchor}"></a>')
    level = len(lines[at + 1]) - len(lines[at + 1].lstrip("#"))
    assert level, f"no heading after anchor {anchor}"
    out, fenced = [], False
    for line in lines[at + 2:]:
        if line.startswith("```"):
            fenced = not fenced
        elif not fenced and re.match(rf"#{{1,{level}}} ", line):
            break
        out.append(line)
    return "\n".join(out)


def test_the_webhook_cert_overlay_cites_the_admission_gate_by_its_anchor():
    text = _read("config/controlstream-tls/webhook-cert.yaml")
    assert "§6.6" not in text, "webhook-cert.yaml cites §6.6, the two-phase discovery rationale"
    assert "#fleetadapter-admission-gating" in text
    assert '<a id="fleetadapter-admission-gating"></a>' in DIST.read_text(encoding="utf-8")


def test_the_registrar_text_names_the_annotation_lookup_the_code_uses():
    registrar = _read("internal/registrar/registrar.go")
    assert registrar.count("robotid.Lookup(") >= 2, (
        "the registrar no longer resolves Discover and Register through robotid.Lookup; "
        "re-read the DiscoveredRobot chapter")
    dist = _flat(DIST.read_text(encoding="utf-8"))
    for stale in ("referenced nowhere", "resolves by object *name*",
                  "The match is on the Robot object's **name**", "Which key is authoritative"):
        assert not _says(dist, stale), f"the dist still says {stale!r}"
    conflicts = _flat(_dist_section("discoveredrobot-conflict-handling-duplicate-robot-id"))
    assert _says(conflicts, "robotid.Lookup") and _says(conflicts, "swarmada.io/robot-id"), (
        "the DiscoveredRobot conflict-handling section does not name the annotation lookup")


def test_the_audit_texts_say_a_removed_tail_is_not_detected():
    rfc = _flat(_dist_section("safety-log-entry-format"))
    assert _says(rfc, "It cannot detect truncation of the tail, or a rewrite re-sealed from the "
                      "changed entry on"), (
        "the RFC no longer states the tail-truncation limit; re-read the operator texts")
    ops = _flat(_read("docs/operations.md"))
    for stale in ("cannot be edited or truncated", "to prove nothing was tampered with"):
        assert not _says(ops, stale), f"operations.md still says {stale!r}"
    assert ops.count("newest entries") >= 2, (
        "operations.md does not say, at both audit sites, that removed newest entries go "
        "undetected")
    kl = _flat(_read("docs/known-limitations.md"))
    assert _says(kl, "### A verified audit chain can still be missing its newest entries"), (
        "known-limitations has no entry on the audit chain's tail")
    assert _says(kl, "highest sequence number"), "known-limitations gives no mitigation"


def test_the_swarmadaconfig_conditions_table_names_signing_incomplete():
    types = _read("api/v1/swarmadaconfig_types.go")
    consts = dict(re.findall(r'(ConditionSigningIncomplete'
                             r'|Reason(?:NoTrustRoots|TrustRootsConfigured|VerificationDisabled))'
                             r'\s*=\s*"([^"]+)"', types))
    assert len(consts) == 4, f"read {consts} from swarmadaconfig_types.go"
    table = [ln for ln in _dist_section("swarmadaconfig").splitlines()
             if ln.startswith(f"| `{consts['ConditionSigningIncomplete']}`")]
    assert table, "the SwarmadaConfig conditions table has no SigningIncomplete row"
    rows = " ".join(table)
    for name, value in consts.items():
        if name.startswith("Reason"):
            assert f"`{value}`" in rows, f"the SigningIncomplete rows do not name reason {value!r}"


def test_the_rejection_table_names_every_assign_action_rejection():
    proto = _read("proto/fleet_adapter/v1/fleet_adapter.proto")
    enum = re.search(r"enum AssignActionRejection \{(.*?)\n\}", proto, re.S)
    assert enum, "no AssignActionRejection enum in fleet_adapter.proto"
    values = {v for v in re.findall(r"ASSIGN_ACTION_REJECTION_([A-Z_]+)\s*=", enum.group(1))
              if v != "UNSPECIFIED"}
    assert len(values) >= 6, f"read {sorted(values)}"
    row = next(ln for ln in DIST.read_text(encoding="utf-8").splitlines()
               if ln.startswith("| Task assignment | `AssignActionRejection`"))
    named = set(re.findall(r"`([A-Z_]+)`", row.split("|")[3]))
    assert named == values, (
        f"the rejection table names {sorted(named)}; the proto defines {sorted(values)}")


def test_the_external_registry_rows_say_they_are_not_qualified():
    rows = [ln for ln in _read("adapters/REGISTRY.md").splitlines()
            if re.match(r"\| `fleet-adapter-(?:ros2|vda5050|mavlink)` \|", ln)]
    assert len(rows) == 3, f"found {len(rows)} external adapter rows"
    for row in rows:
        cell = row.strip().strip("|").split("|")[5].strip()
        assert cell.startswith("**Not qualified at the current contract — pending "
                               "re-qualification.**"), (
            f"{row.split('|')[1].strip()}: the Conformance cell does not open with the plain "
            f"statement: {cell[:120]!r}")


def test_no_text_promises_that_a_held_robot_can_be_released_and_deleted():
    kl = _read("docs/known-limitations.md")
    hold = _flat(kl.split("### A shared-resource hold outlives a robot that never returns", 1)[1]
                 .split("\n### ", 1)[0])
    stop = _flat(kl.split("### A stop is kept on the Robot object only", 1)[1]
                 .split("\n### ", 1)[0])
    ops_heading = "**A robot that holds a stop cannot be deleted.**"
    ops = _flat(_read("docs/operations.md").split(ops_heading, 1)[1].split("\n\n", 1)[0])
    tde = _flat(_dist_section("tde-normative-invariants"))
    for where, text in (("known-limitations, shared-resource hold", hold),
                        ("known-limitations, stop kept on the Robot", stop),
                        ("operations.md", ops), ("the RFC's TDE-7", tde)):
        assert _says(text, "reports again"), (
            f"{where} does not say that a held robot that never reports stays held until it "
            f"reports again")
    assert not _says(hold, "Delete that robot's `Robot`. The hold is released at once"), (
        "known-limitations tells the operator to delete a robot that may be held")
    assert not _says(stop, "Release the robot, then delete it."), (
        "known-limitations tells the operator to release a robot that may never report")
    for where, text in (("known-limitations, stop kept on the Robot", stop),
                        ("operations.md", ops)):
        assert _says(text, "no command decommissions one robot"), (
            f"{where} does not say that no command decommissions one robot")
        assert not _says(text, "next draft"), (
            f"{where} promises the next draft a change")
