# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The harness's check set and levels are pinned to the current suite revision.

Why this file exists. A suite revision names the obligations a result was earned
against, and a registry row names the revision. Twice the harness's check set changed
under an open revision -- `C17.4` was added inside `2026.09.13`, and `C18.1`-`C18.5`
were added between `2026.09.13` and `2026.09.15` with no revision of their own -- so a
row's "earned at suite `<rev>`" did not say which harness it faced, and the class of
`2026.09.15` had to be reconstructed from run records.

`conformance/profiles.yaml` now pins, on the current revision's entry, every check id
the harness records and the level it records it at (`harnessChecks:`). This test reads
the same two things out of `adapters/conformance/harness.py` and fails, naming the
difference, when they disagree. Adding, removing or re-levelling a check therefore
cannot land without a new revision entry that pins the new set -- which is the bump.

HOW THE HARNESS IS READ. Statically, with the `ast` module: every `self.r.add(...)`
call, its check id and its `level=`. A call whose id or level is a variable is resolved
from the loop that binds it (a literal tuple, a module table, or a class table) or from
the local conditional that sets it. A call the reader cannot resolve FAILS the test
rather than being skipped: a guard that silently ignores the calls it cannot read is a
guard that can be walked around by writing one more loop.

A level the harness computes at run time from two literals (C3.8: MUST at the attested
contract version, SHOULD at N-1) is pinned as both, joined with `|`.
"""

from __future__ import annotations

import ast
import re
from collections import defaultdict
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
HARNESS = ROOT / "adapters" / "conformance" / "harness.py"
PROFILES = ROOT / "conformance" / "profiles.yaml"
_ID = re.compile(r"C\d+\.\d+")


def _key(cid: str) -> tuple[int, ...]:
    return tuple(int(x) for x in cid[1:].split("."))


def _literal(node: ast.AST) -> object:
    try:
        return ast.literal_eval(node)
    except (ValueError, SyntaxError, TypeError):
        return None


def harness_checks(source: str) -> tuple[dict[str, str], list[int]]:
    """check id -> levels ('|'-joined, sorted), and the line numbers it could not resolve."""
    tree = ast.parse(source)
    module: dict[str, object] = {}
    classes: dict[str, ast.AST] = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Module):
            for st in node.body:
                if isinstance(st, ast.Assign) and len(st.targets) == 1 and isinstance(
                        st.targets[0], ast.Name):
                    module[st.targets[0].id] = _literal(st.value)
                elif isinstance(st, ast.AnnAssign) and isinstance(st.target, ast.Name) and st.value:
                    module[st.target.id] = _literal(st.value)
        if isinstance(node, ast.ClassDef):
            for st in node.body:
                if isinstance(st, ast.Assign) and len(st.targets) == 1 and isinstance(
                        st.targets[0], ast.Name):
                    classes[st.targets[0].id] = st.value

    parents: dict[ast.AST, ast.AST] = {}
    for n in ast.walk(tree):
        for c in ast.iter_child_nodes(n):
            parents[c] = n

    def ancestors(n: ast.AST):
        while n in parents:
            n = parents[n]
            yield n

    def rows_of(it: ast.AST) -> list | None:
        """The rows a `for` iterates: literal, module table (sliced), or class table."""
        if isinstance(it, ast.Subscript) and isinstance(it.value, ast.Name):
            rows = module.get(it.value.id)
            if isinstance(rows, list) and isinstance(it.slice, ast.Slice):
                lo = _literal(it.slice.lower) if it.slice.lower else None
                return rows[lo:]
            if isinstance(rows, dict):
                # A table keyed at run time (the rows a check records when it cannot run):
                # every value the key could select.
                return [row for value in rows.values() for row in value]
            return None
        if isinstance(it, ast.Name) and isinstance(module.get(it.id), (list, tuple)):
            return list(module[it.id])
        if isinstance(it, ast.Attribute) and it.attr in classes:
            it = classes[it.attr]
        if isinstance(it, (ast.List, ast.Tuple)):
            # Element-wise, so a row holding a non-literal (an enum member) still yields
            # its literal fields.
            return [tuple(_literal(e) for e in r.elts) if isinstance(r, ast.Tuple)
                    else _literal(r) for r in it.elts]
        return None

    def levels(lv: ast.AST | None, bind: dict, call: ast.AST) -> set[str] | None:
        if lv is None:
            return {"MUST"}
        if isinstance(lv, ast.Constant) and isinstance(lv.value, str):
            return {lv.value}
        if isinstance(lv, ast.Name) and lv.id in bind:
            return {bind[lv.id]}
        if isinstance(lv, ast.IfExp):
            test = lv.test
            if (isinstance(test, ast.Compare) and isinstance(test.left, ast.Name)
                    and test.left.id in bind and len(test.ops) == 1
                    and isinstance(test.ops[0], ast.Eq)):
                pick = lv.body if bind[test.left.id] == _literal(test.comparators[0]) else lv.orelse
                return levels(pick, bind, call)
            a, b = levels(lv.body, bind, call), levels(lv.orelse, bind, call)
            return (a | b) if a is not None and b is not None else None
        if (isinstance(lv, ast.Call) and isinstance(lv.func, ast.Attribute)
                and lv.func.attr == "get" and isinstance(lv.func.value, ast.Name)
                and isinstance(module.get(lv.func.value.id), dict)
                and isinstance(lv.args[0], ast.Name) and lv.args[0].id in bind):
            return {module[lv.func.value.id].get(bind[lv.args[0].id], _literal(lv.args[1]))}
        if isinstance(lv, ast.Name):
            # A local set from a conditional of two literals, in the enclosing function.
            fn = next((a for a in ancestors(call) if isinstance(a, ast.FunctionDef)), None)
            for st in ast.walk(fn) if fn else ():
                if (isinstance(st, ast.Assign) and len(st.targets) == 1
                        and isinstance(st.targets[0], ast.Name) and st.targets[0].id == lv.id):
                    return levels(st.value, bind, call)
        return None

    out: dict[str, set[str]] = defaultdict(set)
    unresolved: list[int] = []
    for call in ast.walk(tree):
        if not (isinstance(call, ast.Call) and isinstance(call.func, ast.Attribute)
                and call.func.attr == "add" and isinstance(call.func.value, ast.Attribute)
                and call.func.value.attr == "r" and call.args):
            continue
        a0 = call.args[0]
        lv = next((k.value for k in call.keywords if k.arg == "level"), None)
        if isinstance(a0, ast.Constant) and isinstance(a0.value, str):
            if not _ID.fullmatch(a0.value):
                continue  # the harness's own "run" row, not a catalog check
            got = levels(lv, {}, call)
            if got is None:
                unresolved.append(call.lineno)
            else:
                out[a0.value] |= got
            continue
        resolved = False
        if isinstance(a0, ast.Name):
            for loop in (a for a in ancestors(call) if isinstance(a, ast.For)):
                t = loop.target
                names = ([e.id for e in t.elts if isinstance(e, ast.Name)]
                         if isinstance(t, ast.Tuple) else [getattr(t, "id", None)])
                if a0.id not in names:
                    continue
                rows = rows_of(loop.iter)
                if rows is None:
                    break
                for row in rows:
                    bind = dict(zip(names, row if isinstance(row, tuple) else (row,)))
                    got = levels(lv, bind, call)
                    if got is None or not isinstance(bind.get(a0.id), str):
                        break
                    out[bind[a0.id]] |= got
                else:
                    resolved = True
                break
        if not resolved and isinstance(a0, ast.Name):
            # An id passed in as a parameter: resolved from the literal ids every call site
            # of the enclosing method passes for it.
            fn = next((a for a in ancestors(call) if isinstance(a, ast.FunctionDef)), None)
            params = [a.arg for a in fn.args.args] if fn else []
            if a0.id in params:
                pos = params.index(a0.id) - 1  # `self` is not passed at a call site
                ids = []
                for site in ast.walk(tree):
                    if (isinstance(site, ast.Call) and isinstance(site.func, ast.Attribute)
                            and site.func.attr == fn.name):
                        arg = (site.args[pos] if 0 <= pos < len(site.args) else
                               next((k.value for k in site.keywords if k.arg == a0.id), None))
                        ids.append(_literal(arg) if arg is not None else None)
                got = levels(lv, {}, call)
                if ids and all(isinstance(i, str) for i in ids) and got is not None:
                    for i in ids:
                        out[i] |= got
                    resolved = True
        if not resolved:
            unresolved.append(call.lineno)
    return ({k: "|".join(sorted(v)) for k, v in sorted(out.items(), key=lambda kv: _key(kv[0]))},
            unresolved)


def _pinned() -> tuple[str, dict[str, str]]:
    doc = yaml.safe_load(PROFILES.read_text(encoding="utf-8"))
    current = str(doc["revision"])
    entry = next((e for e in doc.get("revisions") or [] if str(e.get("revision")) == current),
                 None)
    assert entry is not None, f"profiles.yaml has no `revisions:` entry for {current}"
    pinned = entry.get("harnessChecks")
    assert pinned, (
        f"the current revision {current} pins no `harnessChecks:`, so a change to the harness's "
        f"check set could land under it unnoticed; this guard cannot run")
    return current, {str(k): str(v) for k, v in pinned.items()}


def test_every_harness_call_is_readable() -> None:
    _, unresolved = harness_checks(HARNESS.read_text(encoding="utf-8"))
    assert not unresolved, (
        f"adapters/conformance/harness.py records a check at line(s) {unresolved} whose id or "
        f"level this guard cannot resolve statically. Record it with a literal id and level, "
        f"or extend the reader in this file; do not exempt the call")


def test_the_harness_check_set_is_the_one_pinned_to_the_current_revision() -> None:
    current, pinned = _pinned()
    found, _ = harness_checks(HARNESS.read_text(encoding="utf-8"))
    added = sorted(set(found) - set(pinned), key=_key)
    removed = sorted(set(pinned) - set(found), key=_key)
    relevelled = sorted((c for c in set(found) & set(pinned) if found[c] != pinned[c]), key=_key)
    problems = []
    if added:
        problems.append("recorded by the harness, not pinned: "
                        + ", ".join(f"{c} ({found[c]})" for c in added))
    if removed:
        problems.append("pinned, no longer recorded: " + ", ".join(removed))
    if relevelled:
        problems.append("level changed: " + ", ".join(
            f"{c} pinned {pinned[c]}, recorded {found[c]}" for c in relevelled))
    assert not problems, (
        f"the harness no longer matches the check set pinned to suite revision {current}: "
        + "; ".join(problems) + ". A change to what an implementation must do to pass is a "
        "`harness`-class revision: add a new entry to `revisions:` in conformance/profiles.yaml "
        "with class, reason and this `harnessChecks:` set, and move `revision:` to it")


def test_the_reader_sees_a_dummy_check_added_without_a_bump() -> None:
    """The guard's own falsifiability, kept in the suite: a new literal check and a changed
    level are both reported against the pinned set."""
    _, pinned = _pinned()
    source = HARNESS.read_text(encoding="utf-8") + (
        "\n\ndef _dummy(self):\n"
        "    self.r.add(\"C99.1\", \"dummy\", CheckStatus.PASS, level=\"MUST\")\n")
    found, unresolved = harness_checks(source)
    assert not unresolved
    assert "C99.1" in found and "C99.1" not in pinned
