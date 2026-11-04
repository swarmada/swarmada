# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Drives REACTIVE-BLOCK.md against the grammar manifest and, when one exists,
an adapter that declares ``reactive_grammar_version``.

Every check here has two halves. The GRAMMAR half asks whether the construct is
correctly specified and covered; the EXECUTION half asks whether an
implementation honours it. The grammar half runs today, from
``reactive-grammar.v1.yaml``. The execution half needs an adapter declaring a
grammar version, and there is none in the tree -- S5 is gated on physical
hardware and has not run -- so those checks record
``surface-not-implemented`` and carry the grammar finding in the detail.

Four checks are discharged ENTIRELY by the grammar's shape, and they are the
MUST NOTs: RB2.7, RB3.5, RB3.6 and RB3.7 ask what the grammar OFFERS, and a
grammar that offers no motion effect cannot have one executed. Those pass on
grammar evidence alone, which is exactly what "enforced by the shape of the
grammar, not by a rule someone remembers" means -- and what S5's gate asks to
be checked by grammar coverage rather than by inspection.
"""

from __future__ import annotations

import re
from pathlib import Path

import yaml

from .model import ResultSet, SkipCause

HERE = Path(__file__).resolve().parent
MANIFEST = HERE / "reactive-grammar.v1.yaml"
CATALOG = HERE / "REACTIVE-BLOCK.md"

COVERAGE_ROW = re.compile(r"^\|\s*`([^`]+)`\s*\|\s*([^|]+?)\s*\|$")


def load_manifest(rs: ResultSet) -> dict:
    rs.artefacts.append(str(MANIFEST))
    return yaml.safe_load(MANIFEST.read_text())


def construct_ids(manifest: dict) -> list[str]:
    out: list[str] = []
    for key in ("structure", "predicates", "aggregates", "comparisons", "effects",
                "prohibitions"):
        for entry in manifest.get(key) or []:
            out.append(entry["id"])
    return out


def coverage_table(rs: ResultSet) -> dict[str, list[str]]:
    rs.artefacts.append(str(CATALOG))
    text = CATALOG.read_text()
    block = text.split("<!--rb-coverage-->", 1)[1].split("<!--/rb-coverage-->", 1)[0]
    table: dict[str, list[str]] = {}
    for line in block.splitlines():
        m = COVERAGE_ROW.match(line.strip())
        if not m or m.group(1) == "Construct":
            continue
        table[m.group(1)] = [c.strip() for c in m.group(2).split(",") if c.strip()]
    return table


def run(rs: ResultSet, known_check_ids: set[str], *,
        adapter_grammar_version: str | None = None) -> dict[str, bool]:
    manifest = load_manifest(rs)
    table = coverage_table(rs)
    constructs = construct_ids(manifest)
    version = str(manifest["grammarVersion"])

    rs.ctx("RB0.4", "Harness context: the grammar version and manifest revision",
           f"manifest grammarVersion={version} revision={manifest['revision']}; "
           f"adapter declared {adapter_grammar_version or '(none)'}")

    # ── RB0.1 — the check this profile exists for ────────────────────────────
    uncovered = [c for c in constructs if not table.get(c)]
    orphaned = [c for c in table if c not in constructs]
    unknown = sorted({cid for ids in table.values() for cid in ids} - known_check_ids)
    if uncovered or orphaned or unknown:
        detail = []
        if uncovered:
            detail.append(f"constructs with no check: {uncovered}")
        if orphaned:
            detail.append(f"table rows naming a construct that does not exist: {orphaned}")
        if unknown:
            detail.append(f"checks named in the table but absent from the registry: {unknown}")
        rs.bad("RB0.1", "Every grammar construct is covered by a check", "; ".join(detail))
    else:
        rs.ok("RB0.1", "Every grammar construct is covered by a check",
              f"{len(constructs)} construct(s) in grammar v{version}, each bound to at "
              f"least one check; {len(table)} table row(s), every one naming a construct "
              "that exists and checks the registry knows")

    driven = adapter_grammar_version is not None
    rs.context["surface_driven"] = driven

    def grammar_only(cid: str, title: str, ok: bool, evidence: str,
                     level: str = "MUST NOT") -> None:
        """A check the grammar's shape discharges on its own."""
        (rs.ok if ok else rs.bad)(cid, title, evidence, level=level)

    def needs_adapter(cid: str, title: str, grammar_finding: str,
                      level: str = "MUST") -> None:
        if driven:
            rs.skip(cid, title, SkipCause.TARGET_DID_NOT_REACH_STATE,
                    "an adapter declared a grammar version but this harness cannot yet "
                    "dispatch a block to it", level=level)
        else:
            rs.skip(cid, title, SkipCause.SURFACE_NOT_IMPLEMENTED,
                    f"no adapter in the tree declares reactive_grammar_version (S5 is "
                    f"hardware-gated and has not run). Grammar half verified: "
                    f"{grammar_finding}", level=level)

    struct = {e["id"]: e for e in manifest["structure"]}
    predicates = {e["id"]: e for e in manifest["predicates"]}
    effects = {e["id"]: e for e in manifest["effects"]}
    prohibitions = {e["id"]: e for e in manifest["prohibitions"]}
    aggregates = [e["id"].split(".", 1)[1] for e in manifest["aggregates"]]
    comparisons = [e["id"].split(".", 1)[1] for e in manifest["comparisons"]]

    # ── RB1 — structure ──────────────────────────────────────────────────────
    needs_adapter("RB1.1", "Accept a block as spec.reactive.rules[], dispatched with the action",
                  f"`block.rules` is declared required={struct['block.rules']['required']}")
    needs_adapter("RB1.2", "Require a unique rule name",
                  f"`rule.name` is declared required={struct['rule.name']['required']}")
    needs_adapter("RB1.3", "Accept exactly one predicate under `when`",
                  "the manifest declares one predicate per rule and offers no and/or/not "
                  "combinator")
    needs_adapter("RB1.4", "Apply effects in the order written",
                  f"`rule.then` is declared required={struct['rule.then']['required']}")
    needs_adapter("RB1.5", "Require cooldownMs and never default it",
                  f"`rule.cooldownMs` required={struct['rule.cooldownMs']['required']}; "
                  f"`prohibition.unboundedRule` present")

    # ── RB2 — predicates ─────────────────────────────────────────────────────
    stream_fields = [f["name"] for f in predicates["predicate.stream"]["fields"]]
    needs_adapter("RB2.1", "Support a stream predicate over a declared stream",
                  f"`predicate.stream` declares {stream_fields}")
    needs_adapter("RB2.2", "Require forMs on a stream predicate and never default it",
                  "`forMs` is declared required on `predicate.stream`")
    needs_adapter("RB2.3", "Support a computeResult predicate over a local executor",
                  "`predicate.computeResult` source="
                  f"{predicates['predicate.computeResult']['source']}")
    needs_adapter("RB2.4", "Support a computeEvent predicate over a local executor",
                  "`predicate.computeEvent` source="
                  f"{predicates['predicate.computeEvent']['source']}")
    needs_adapter("RB2.5", "Support exactly the enumerated aggregates and reject any other",
                  f"the manifest closes the set at {aggregates}")
    needs_adapter("RB2.6", "Support exactly the enumerated comparisons and reject any other",
                  f"the manifest closes the set at {comparisons}")

    non_local = [p for p in predicates.values() if p.get("source") != "robot-local"]
    banned = set(prohibitions["prohibition.controlPlaneSource"]["reject"])
    named = {f["name"] for p in predicates.values() for f in p["fields"]}
    leaks = sorted(named & banned)
    grammar_only("RB2.7", "No predicate matches a control-plane source",
                 not non_local and not leaks,
                 f"all {len(predicates)} predicate(s) declare source: robot-local, and no "
                 f"predicate field is one of the {len(banned)} control-plane names the "
                 f"manifest rejects ({sorted(banned)}). With a downward event "
                 "path a TaskTrigger could reach a robot-local effect that ran with no "
                 "admission, no scheduling, no reservation and no lease check")

    # ── RB3 — effects ────────────────────────────────────────────────────────
    needs_adapter("RB3.1", "Support activateStream with a bounded rate and hold",
                  f"`effect.activateStream` fields "
                  f"{[f['name'] for f in effects['effect.activateStream']['fields']]}")
    needs_adapter("RB3.2", "Support invokeExecutor naming a class the adapter hosts",
                  "`effect.invokeExecutor` names a robot-local executor class")
    needs_adapter("RB3.3", "Support raiseEvent",
                  "`effect.raiseEvent` is the grammar's only upward path")
    needs_adapter("RB3.4", "Support playCue with a declared cue id",
                  f"`effect.playCue` is gated by {effects['effect.playCue']['gatedBy']}")

    grammar_only("RB3.5", "Reject an effect key outside the closed set of four",
                 len(effects) == 4 and "prohibition.openEffectSet" in prohibitions,
                 f"the grammar declares exactly {len(effects)} effects "
                 f"({sorted(e.split('.', 1)[1] for e in effects)}) and an explicit "
                 "prohibition on any other. There is no extension point, no `custom`, "
                 "and no passthrough")

    motion_named = sorted({e.split(".", 1)[1] for e in effects}
                          & set(prohibitions["prohibition.motionEffect"]["reject"]))
    motion_flagged = [e for e, v in effects.items() if v.get("commandsMotion")]
    grammar_only("RB3.6", "No effect commands motion",
                 not motion_named and not motion_flagged,
                 f"none of the {len(effects)} effects is one of the "
                 f"{len(prohibitions['prohibition.motionEffect']['reject'])} motion verbs "
                 f"the manifest rejects "
                 f"({prohibitions['prohibition.motionEffect']['reject']}), and every effect "
                 "declares commandsMotion: false. Principle 2 is enforced by the SHAPE of "
                 "the grammar; this is that shape asserted, not inspected")

    cue_fields = {f["name"] for f in effects["effect.playCue"]["fields"]}
    free_form = sorted(cue_fields & set(prohibitions["prohibition.freeFormCue"]["reject"]))
    grammar_only("RB3.7", "playCue carries no free-form content",
                 not free_form and cue_fields == {"cueId"},
                 f"`effect.playCue` declares exactly {sorted(cue_fields)}; none of "
                 f"{prohibitions['prohibition.freeFormCue']['reject']} is present, so a "
                 "model's output cannot become audible in a facility with nothing in between")

    # ── RB4 / RB5 / RB6 — execution ──────────────────────────────────────────
    needs_adapter("RB4.1", "A block lives exactly as long as its owning action",
                  "the manifest states the block is dispatched with the action")
    needs_adapter("RB4.2", "Stop evaluating on lease expiry",
                  "the safe direction for a reactive block is off")
    needs_adapter("RB4.3", "Declare reactive_grammar_version and execute only what it covers",
                  f"the manifest's grammarVersion is {version}")
    needs_adapter("RB5.1", "raiseEvent travels upward as a TaskEvent obeying E1 and E3",
                  "`effect.raiseEvent` is the grammar's only upward path")
    needs_adapter("RB5.2", "A fired rule is observable",
                  "`rule.name` exists so a fired rule can be attributed")
    needs_adapter("RB5.3", "No downward control-plane event path is consumed",
                  "the grammar closes the predicate side (RB2.7); this closes the transport",
                  level="MUST NOT")
    needs_adapter("RB6.1", "Honour allowLocalCues: false",
                  f"`effect.playCue` declares gatedBy: {effects['effect.playCue']['gatedBy']}")
    needs_adapter("RB6.2", "Resolve a cue id to a declared capability and emit nothing otherwise",
                  "`cueId` is the only field on the effect")

    # `reactive-grammar` always applies; `reactive` applies only to an adapter
    # that declares a grammar version. The split is why RB0.1 is visible in
    # every report rather than only in the few produced by adapters that
    # implement the tier.
    return {"reactive-grammar": True, "reactive": driven}
