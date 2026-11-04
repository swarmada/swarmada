#!/usr/bin/env python3
"""gen-ungraded-obligations.py — generate RFC-0001's list of control-plane rules no check grades.

WHY THIS EXISTS. A conformance check grades some of the rules RFC-0001 states with a BCP-14
keyword. Others the reference control plane meets and nothing grades, and a reader could not tell
the two apart, or tell either from a rule the reference does not meet. The chapter
*Control-Plane Obligations Not Graded at v0.4* lists the second kind, each with the Go test (or,
where no test exists, the code) that shows it. It is generated so that it cannot drift from the
rules it summarises.

WHAT IT READS. One ledger, rfcs/rfc-0001-core-spec/obligations.yaml, classifies every keyword
occurrence (MUST, MUST NOT, REQUIRED, SHALL, SHALL NOT, in prose, tables and code-block comments)
in the chapters in SCOPE. One entry per rule:

    - id: CPO-031                 # stable; never reused, never renumbered
      chapter: control-plane.md   # relative to rfcs/rfc-0001-core-spec/
      quote: "..."                # exact text; occurs exactly once in the chapter
      keyword: MUST               # the quote holds it, and so does the statement
      disposition: ungraded       # see DISPOSITIONS
      statement: "..."            # ungraded: a one-line summary that keeps the keyword
      evidence:                   # ungraded: at least one; a Go or Python function that exists
        - {file: internal/controller/x_test.go, func: TestX}
      except: "..."               # ungraded, optional: a Not-met note at the rule covers part of it
      check: CP1.1                # graded: a check id a conformance catalog defines
      note: "..."                 # not-met: words of the Not-met note, found in the same chapter
      note_in: crds/robot.md      # not-met, optional: the chapter that holds the note instead
      of: CPO-012                 # dup: the entry that states the same rule
      reason: "..."               # not-binding, unverified: why

Only `ungraded` entries render, and a `dup` of one renders as a pointer to it. A rule the
reference does not meet has no way into the list: its entry is `not-met`, and its note must exist
where the rule is stated.

THE GUARDS (exit 2, naming the entry): a quote that is absent or occurs twice; a quote or a
statement that lacks the entry's keyword, so a summary cannot soften MUST or drop NOT; an
evidence function that does not exist, so a renamed test breaks the build instead of leaving a
dead citation; a `note` or `except` that the chapter no longer holds; a `check` no catalog
defines; a `dup` of an unknown entry.

THE CHECK (`--check`) fails, exit 1, on either of:

  1. STALE — the committed chapter differs from a fresh generation. Printed as a unified diff.
  2. UNCLASSIFIED — a keyword occurrence in a chapter in scope that no entry's quote covers. A
     new MUST cannot land without a disposition.

A checkout that carries the assembled specification but not the chapter sources and the ledger
(the published tree) checks every row the assembled chapter lists: each one's evidence must still
exist. Ledger completeness is checked where the sources are, and it says so.

Usage: python3 scripts/gen-ungraded-obligations.py [--check] [--root DIR]
Exit:  0 clean / written · 1 findings · 2 the check could not run.
       A check that exits without having run is not a pass.
"""

from __future__ import annotations

import argparse
import difflib
import glob
import importlib.util
import re
import sys
from pathlib import Path

import yaml

SPEC = "rfcs/rfc-0001-core-spec"
LEDGER = SPEC + "/obligations.yaml"
OUT = SPEC + "/ungraded-obligations.md"
MANIFEST = SPEC + "/rfc-0001.meta.yaml"
ASSEMBLER = "rfcs/assemble.py"
DIST = "rfcs/dist/RFC-0001-core-spec.md"
CHAPTER_ID = "ungraded-control-plane-obligations"
TITLE = "Control-Plane Obligations Not Graded at v0.4"
# The chapters whose keyword occurrences the ledger must classify. The amendments keep their own
# no-check notes and join when they are folded into RFC-0001.
SCOPE = ["control-plane.md", "crds/*.md", "tde.md", "security.md", "safety.md",
         "fleet-adapter-protocol.md", "sensor-streams.md", "conformance-profiles.md"]
# Where a `check` must be defined.
CATALOGS = ["conformance/*.md", "adapters/CONFORMANCE.md"]
DISPOSITIONS = {
    "ungraded": "met by the reference, graded by no check: listed",
    "graded": "a conformance check grades it",
    "not-met": "the reference does not meet it; a Not-met note says so at the rule",
    "adapter": "binds a Fleet Adapter; graded or listed through the adapter checklist",
    "other-party": "binds the edge node, a client, a tool or the robot",
    "deployment": "binds an operator, a deployment or a reader",
    "dup": "restates another entry's rule",
    "not-binding": "the keyword binds no one (a quotation, an example, a definition)",
    "unverified": "binds the control plane, not yet checked against the reference: not listed",
}
KEYWORD = re.compile(r"\b(MUST NOT|MUST|REQUIRED|SHALL NOT|SHALL)\b")
ENTRY_ID = re.compile(r"^CPO-\d{3,4}$")
CHECK_ID = re.compile(r"^[A-Z]{1,3}\d+(\.\d+)*[a-z]?$")

HEADER = """---
chapter: {cid}
title: "{title}"
status: review
owner: ""
updated: {updated}
---

<!-- Generated by scripts/gen-ungraded-obligations.py from obligations.yaml, the ledger beside this
file. Edit the ledger, then run that script; do not edit this file. -->

Each row below is a rule that binds the control plane, that this specification states with a
BCP-14 keyword, and that no conformance check grades at v0.4. The reference control plane meets
every row. The last column names the test, or where no test exists the code, that shows it. The
list is a disclosure. Each rule binds a conforming control plane exactly as its own chapter states
it, and the statement in this table is a summary that does not replace it.

A rule the reference does not meet is not listed here. It carries a *Not met at v0.4* note where
it is stated. A rule that binds a Fleet Adapter is graded or listed through
{{{{ref:fleet-adapter-protocol-fleet-adapter-compliance-checklist}}}}. A rule that binds an
operator, a deployment or a reader is a deployment obligation, and no conformance check grades it.
A row marked *in part* is met except for the part a *Not met at v0.4* note at the rule discloses.

| Rule | Stated in | Statement | Reference evidence |
| :-- | :-- | :-- | :-- |
"""

ROW = re.compile(r"^\| (CPO-\d{3,4}) \| [^|]* \| [^|]* \| (?P<ev>.*) \|$")
EV_ITEM = re.compile(r"`([A-Za-z_][A-Za-z0-9_]*)` (?:in )?\(?`([^`]+)`\)?")


class LedgerError(Exception):
    """The ledger or its inputs are wrong; the check could not run."""


def tokens(text: str) -> list[str]:
    return KEYWORD.findall(text)


def load_assembler(root: Path):
    spec = importlib.util.spec_from_file_location("assemble", root / ASSEMBLER)
    if spec is None or spec.loader is None:
        raise LedgerError(f"cannot load {ASSEMBLER}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def chapters_in_order(root: Path) -> list[tuple[str, str, int]]:
    """(file, chapter id, base depth) for every manifest chapter, in assembly order."""
    meta = yaml.safe_load((root / MANIFEST).read_text(encoding="utf-8"))
    out: list[tuple[str, str, int]] = []

    def walk(nodes, depth):
        for nd in nodes:
            out.append((nd["file"], nd["id"], depth + 2))
            walk(nd.get("children") or [], depth + 1)

    walk(meta["sections"], 0)
    return out


def scope_files(root: Path) -> list[str]:
    spec = root / SPEC
    found: list[str] = []
    for pat in SCOPE:
        hits = sorted(str(Path(p).relative_to(spec)) for p in glob.glob(str(spec / pat)))
        if not hits:
            raise LedgerError(f"no chapter matches scope pattern {pat}")
        found += hits
    return found


def anchors(asm, body: str, base_depth: int, chapter_id: str) -> list[tuple[int, str]]:
    """(offset in body, slug) of every heading anchor, exactly as the assembler assigns them."""
    out = asm.renumber_body(body, base_depth, "0", chapter_id)
    res: list[tuple[int, str]] = []
    pos = 0
    pending = None
    for line in out.splitlines(keepends=True):
        m = re.match(r'^<a id="([^"]+)"></a>$', line.rstrip("\n"))
        if m:
            pending = m.group(1)
            continue
        if pending is not None:
            # the heading line follows; find its title in the source body
            title = re.sub(r"^#+\s+(?:\d+(?:\.\d+)*\s+)?", "", line.rstrip("\n"))
            hit = re.search(r"^#{2,8}\s+" + re.escape(title) + r"\s*$", body[pos:], re.M)
            if hit is None:
                raise LedgerError(f"{chapter_id}: cannot place heading {title!r}")
            pos += hit.start()
            res.append((pos, pending))
            pos += len(hit.group(0))
            pending = None
    return res


def func_exists(root: Path, file: str, func: str) -> bool:
    p = root / file
    if not p.is_file():
        return False
    text = p.read_text(encoding="utf-8", errors="replace")
    if file.endswith(".go"):
        return re.search(r"^func (?:\([^)]*\) )?" + re.escape(func) + r"\b", text, re.M) is not None
    if file.endswith(".py"):
        return re.search(r"^\s*def " + re.escape(func) + r"\b", text, re.M) is not None
    return False


def catalog_ids(root: Path) -> str:
    text = ""
    for pat in CATALOGS:
        for p in sorted(glob.glob(str(root / pat))):
            text += Path(p).read_text(encoding="utf-8", errors="replace")
    if not text:
        raise LedgerError("no conformance catalog found to resolve `check` ids")
    return text


def cell(text: str) -> str:
    if "|" in text or "\n" in text:
        raise LedgerError(f"a statement may hold no '|' or newline: {text!r}")
    return text.strip()


def evidence_cell(ev: list[dict]) -> str:
    def is_test(e: dict) -> bool:
        return Path(e["file"]).name.startswith("test_") or e["file"].endswith("_test.go")

    parts = [f"`{e['func']}` (`{e['file']}`" + (")" if is_test(e) else ", code)") for e in ev]
    tail = "" if any(is_test(e) for e in ev) else "; no test pins it"
    return "; ".join(parts) + tail


def build(root: Path) -> tuple[str, list[str]]:
    """Return (the generated chapter, UNCLASSIFIED findings). Raises LedgerError on a bad entry."""
    ledger = yaml.safe_load((root / LEDGER).read_text(encoding="utf-8"))
    if not isinstance(ledger, dict) or "entries" not in ledger or "updated" not in ledger:
        raise LedgerError(f"{LEDGER}: needs `updated` and `entries`")
    entries = ledger["entries"] or []
    asm = load_assembler(root)
    order = chapters_in_order(root)
    in_scope = scope_files(root)
    meta = {f: (cid, depth) for f, cid, depth in order}
    for f in in_scope:
        if f not in meta:
            raise LedgerError(f"{f} is in scope but not in the manifest")
    bodies = {f: asm._FM.sub("", (root / SPEC / f).read_text(encoding="utf-8"), count=1)
              for f in in_scope}
    heads = {f: anchors(asm, bodies[f], meta[f][1], meta[f][0]) for f in in_scope}
    catalogs = None
    by_id: dict[str, dict] = {}
    spans: dict[str, list[tuple[int, int]]] = {f: [] for f in in_scope}
    errors: list[str] = []
    for e in entries:
        eid = e.get("id", "?")
        try:
            if not ENTRY_ID.match(str(eid)):
                raise LedgerError("bad id")
            if eid in by_id:
                raise LedgerError("id used twice")
            f = e.get("chapter")
            if f not in bodies:
                raise LedgerError(f"chapter {f!r} is not in scope")
            q = e.get("quote") or ""
            n = bodies[f].count(q)
            if not q or n != 1:
                raise LedgerError(f"quote occurs {n} times in {f}: {q[:60]!r}")
            kw = e.get("keyword")
            if kw not in ("MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT"):
                raise LedgerError(f"keyword {kw!r}")
            if kw not in tokens(q):
                raise LedgerError(f"the quote does not hold {kw}")
            d = e.get("disposition")
            if d not in DISPOSITIONS:
                raise LedgerError(f"disposition {d!r}")
            if d == "ungraded":
                st = e.get("statement") or ""
                if kw not in tokens(st):
                    raise LedgerError(f"the statement does not hold {kw}")
                ev = e.get("evidence") or []
                if not ev:
                    raise LedgerError("no evidence")
                for x in ev:
                    if not func_exists(root, x.get("file", ""), x.get("func", "")):
                        raise LedgerError(f"evidence {x.get('func')} not found in {x.get('file')}")
                if e.get("except") and e["except"] not in bodies[f]:
                    raise LedgerError(f"except note not found in {f}: {e['except'][:60]!r}")
            elif d == "graded":
                c = str(e.get("check") or "")
                if catalogs is None:
                    catalogs = catalog_ids(root)
                if not CHECK_ID.match(c) or not re.search(r"\b" + re.escape(c) + r"\b", catalogs):
                    raise LedgerError(f"check {c!r} is defined by no catalog")
            elif d == "not-met":
                where = e.get("note_in") or f
                held = bodies.get(where)
                if held is None:
                    p_in = root / SPEC / where
                    held = p_in.read_text(encoding="utf-8") if p_in.is_file() else ""
                if not e.get("note") or e["note"] not in held:
                    raise LedgerError(f"note not found in {where}: {str(e.get('note'))[:60]!r}")
            elif d in ("not-binding", "unverified"):
                if not e.get("reason"):
                    raise LedgerError(f"{d} needs a reason")
            p = bodies[f].find(q)
            spans[f].append((p, p + len(q)))
            by_id[eid] = e
        except LedgerError as err:
            errors.append(f"{eid}: {err}")
    for e in entries:
        if e.get("disposition") == "dup" and e.get("of") not in by_id:
            errors.append(f"{e.get('id')}: dup of unknown entry {e.get('of')!r}")
    if errors:
        raise LedgerError("ledger errors:\n  " + "\n  ".join(errors))

    unclassified: list[str] = []
    for f in in_scope:
        body = bodies[f]
        for m in KEYWORD.finditer(body):
            if not any(a <= m.start() and m.end() <= b for a, b in spans[f]):
                line_no = body.count("\n", 0, m.start()) + 1
                line = body.splitlines()[line_no - 1].strip()
                unclassified.append(f"{f}:{line_no} (body line) {m.group(1)}: {line[:110]}")

    def slug_at(f: str, pos: int) -> str:
        s = meta[f][0]
        for off, slug in heads[f]:
            if off > pos:
                break
            s = slug
        return s

    rank = {f: i for i, (f, _c, _d) in enumerate(order)}
    rows = []
    for e in entries:
        d = e["disposition"]
        if d not in ("ungraded", "dup"):
            continue
        f = e["chapter"]
        pos = bodies[f].find(e["quote"])
        if d == "dup":
            primary = by_id[e["of"]]
            if primary["disposition"] != "ungraded":
                continue
            stmt = f"Restates {e['of']}."
            ev = "—"
        else:
            stmt = cell(e["statement"])
            if e.get("except"):
                stmt += " *In part.*"
            ev = evidence_cell(e["evidence"])
        ref = "{{ref:" + slug_at(f, pos) + "}}"
        rows.append((rank[f], pos, f"| {e['id']} | {ref} | {stmt} | {ev} |"))
    rows.sort()
    text = HEADER.format(cid=CHAPTER_ID, title=TITLE, updated=ledger["updated"])
    text += "".join(r[2] + "\n" for r in rows)
    counts: dict[str, int] = {}
    for e in entries:
        counts[e["disposition"]] = counts.get(e["disposition"], 0) + 1
    text += (f"\nThe list holds {sum(1 for r in rows)} rows. The ledger classifies "
             f"{len(entries)} rules in {len(in_scope)} chapters.\n")
    return text, unclassified


def published_check(root: Path) -> int:
    """The published tree: no chapter sources or ledger. Check the evidence the dist lists."""
    dist = root / DIST
    if not dist.is_file():
        print(f"gen-ungraded-obligations: neither {LEDGER} with its chapters nor {DIST} is here; "
              "the check could not run", file=sys.stderr)
        return 2
    text = dist.read_text(encoding="utf-8")
    start = text.find(f'<a id="{CHAPTER_ID}"></a>')
    if start < 0:
        print(f"gen-ungraded-obligations: {DIST} has no chapter {CHAPTER_ID}", file=sys.stderr)
        return 1
    rows = 0
    bad = []
    for line in text[start:].splitlines()[2:]:
        if line.startswith("<a id="):
            break
        m = ROW.match(line)
        if not m:
            continue
        rows += 1
        for func, file in EV_ITEM.findall(m.group("ev")):
            if not func_exists(root, file, func):
                bad.append(f"{m.group(1)}: evidence {func} not found in {file}")
    if rows == 0:
        print(f"gen-ungraded-obligations: {DIST} lists no rows", file=sys.stderr)
        return 1
    for b in bad:
        print("EVIDENCE " + b)
    print(f"gen-ungraded-obligations: the chapter sources and {LEDGER} are not in this checkout; "
          f"checked the evidence of the {rows} rows {DIST} lists ({len(bad)} missing). Ledger "
          "completeness is checked where the sources are.")
    return 1 if bad else 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--check", action="store_true",
                    help="fail if stale or unclassified; write nothing")
    ap.add_argument("--root", default=str(Path(__file__).resolve().parent.parent))
    args = ap.parse_args()
    root = Path(args.root).resolve()
    if not (root / LEDGER).is_file() or not (root / ASSEMBLER).is_file():
        if args.check:
            return published_check(root)
        print(f"gen-ungraded-obligations: {LEDGER} or {ASSEMBLER} is missing; nothing to generate "
              "from", file=sys.stderr)
        return 2
    try:
        text, unclassified = build(root)
    except (LedgerError, OSError, yaml.YAMLError, KeyError) as err:
        print(f"gen-ungraded-obligations: {err}", file=sys.stderr)
        return 2
    out = root / OUT
    if not args.check:
        out.write_text(text, encoding="utf-8")
        print(f"gen-ungraded-obligations: wrote {OUT}")
        for u in unclassified:
            print("UNCLASSIFIED " + u)
        return 1 if unclassified else 0
    rc = 0
    old = out.read_text(encoding="utf-8") if out.is_file() else ""
    if old != text:
        rc = 1
        print(f"STALE {OUT}: regenerate with `python3 scripts/gen-ungraded-obligations.py`")
        sys.stdout.writelines(difflib.unified_diff(old.splitlines(keepends=True),
                                                   text.splitlines(keepends=True),
                                                   OUT + " (committed)", OUT + " (generated)"))
    for u in unclassified:
        print("UNCLASSIFIED " + u)
        rc = 1
    if rc == 0:
        print(f"gen-ungraded-obligations: {OUT} is current; every keyword occurrence in scope "
              "is classified")
    return rc


if __name__ == "__main__":
    sys.exit(main())
