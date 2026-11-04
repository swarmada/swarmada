# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The command table in `docs/operations.md` names roles that hold the verbs it lists.

Why this file exists. `docs/operations.md` §2 is a table of operations: for each one it
names the API verb, the least-privileged built-in role that may perform it, and the
command. The §1 role table is compared with the RBAC files by a Go test; this table
was compared with nothing, and it named `fleet-manager` for two operations that role
cannot perform -- ending a ZoneMaintenance (`update`) and deleting a FleetAdapter
(`delete`). A fleet manager following the table got Forbidden on both.

For every row whose role is one of the four operator roles, these tests require that:

- the named role's ClusterRole in `config/rbac/` grants each listed verb on the row's
  resource (the role files are flat: each lists everything it grants); and
- the next role down the ladder (viewer < operator < fleet-manager < admin) does not
  grant all of them, so the named role is the least-privileged one.

THE RESOURCE OF A ROW is the first kind the command cell names (`kubectl delete
fleetadapter …` → `fleetadapters`), or the resource a verb cell names explicitly
(`patch` on `swarmadaconfigs`). Rows whose command names no kind are resolved by
`_BY_OPERATION`; a row that resolves to nothing fails the test, so a new row cannot
be skipped silently.

WHAT THEY DO NOT CHECK
    The Helm chart's copy of the roles (`make helm-verify-sync` compares it with
    `config/rbac`), and whether the command in the row performs the listed verb.
"""

from __future__ import annotations

import html
import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
DOC = ROOT / "docs" / "operations.md"
LADDER = ["viewer", "operator", "fleet-manager", "admin"]

_KINDS = {
    "robot": "robots", "discoveredrobot": "discoveredrobots", "robotclass": "robotclasses",
    "fleetaction": "fleetactions", "fleettask": "fleettasks", "fleetzone": "fleetzones",
    "zonemaintenance": "zonemaintenances", "robotprobe": "robotprobes",
    "firmwarerollout": "firmwarerollouts", "modelrollout": "modelrollouts",
    "modelpolicy": "modelpolicies", "swarmadaconfig": "swarmadaconfigs",
    "fleetadapter": "fleetadapters",
}

# Rows whose command cell names no kind, or names one the verb is not checked on.
_BY_OPERATION = {
    "admit a discovered robot": "discoveredrobots",
    "re-admit an admitted robot": "discoveredrobots",
    "reject a discovered robot": "discoveredrobots",
    "activate a scheduled one now": "zonemaintenances",
    "estop a zone": "fleetzones",
    "clear an estop on a zone": "fleetzones",
    "create a rollout": "firmwarerollouts",
    "release a pauseOnError halt": "firmwarerollouts",
    "read the safety audit log": "safetyauditlogs",
    "verify the audit hash chain": "safetyauditlogs",
    "export the audit log": "safetyauditlogs",
}


def _grants(role: str) -> set[tuple[str, str]]:
    doc = yaml.safe_load((ROOT / "config" / "rbac" / f"swarmada_{role.replace('-', '_')}_role.yaml")
                         .read_text())
    return {(res, verb) for rule in doc["rules"] for res in rule.get("resources", [])
            for verb in rule["verbs"]}


def _text(cell: str) -> str:
    return html.unescape(re.sub(r"<[^>]+>", "", cell)).strip()


def _rows() -> list[tuple[str, list[str], str, str]]:
    """(operation, verbs, role, resource) for every row naming an operator role."""
    out = []
    for m in re.finditer(r"<tr><td>(.*?)</td><td>(.*?)</td><td>(.*?)</td><td>(.*?)</td></tr>",
                         DOC.read_text()):
        op_cell, verb_cell, role_cell, cmd_cell = m.groups()
        op, role = _text(op_cell), _text(role_cell)
        if role not in LADDER:
            continue
        on = re.search(r"on <code>([\w-]+)</code>", verb_cell)
        verbs = [v for v in re.findall(r"<code>([\w-]+)</code>", verb_cell)
                 if not on or v != on.group(1)]
        key = next((k for k in _BY_OPERATION if op.startswith(k)), None)
        if on:
            resource = on.group(1)
        elif key:
            resource = _BY_OPERATION[key]
        else:
            nouns = [_KINDS[w] for w in re.findall(r"[a-z]+", _text(cmd_cell)) if w in _KINDS]
            assert nouns, f"operations.md row {op!r} names no resource; add it to _BY_OPERATION"
            resource = nouns[0]
        out.append((op, verbs, role, resource))
    return out


def test_the_table_parses():
    rows = _rows()
    assert len(rows) > 40, f"only {len(rows)} role-bearing rows parsed from {DOC.name}"
    assert {r for _, _, r, _ in rows} >= {"viewer", "operator", "fleet-manager", "admin"}


def test_the_named_role_holds_every_listed_verb():
    grants = {r: _grants(r) for r in LADDER}
    wrong = [f"{op!r}: {role} lacks {v} on {res}"
             for op, verbs, role, res in _rows() for v in verbs if (res, v) not in grants[role]]
    assert not wrong, "operations.md names a role that cannot do the operation: " + "; ".join(wrong)


def test_the_named_role_is_the_least_privileged():
    grants = {r: _grants(r) for r in LADDER}
    wrong = []
    for op, verbs, role, res in _rows():
        below = LADDER.index(role) - 1
        if below >= 0 and verbs and all((res, v) in grants[LADDER[below]] for v in verbs):
            wrong.append(f"{op!r}: {LADDER[below]} already holds {verbs} on {res}")
    assert not wrong, (
        "operations.md names a role above the least-privileged one: " + "; ".join(wrong))
