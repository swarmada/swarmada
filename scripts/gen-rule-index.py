#!/usr/bin/env python3
"""gen-rule-index.py — generate the rules index, and check it against the text that cites it.

WHY THIS EXISTS. Code comments, CRD descriptions, the adapter protocol, conformance output and
documentation cite namespaced rule identifiers (a FleetZone rule, an adapter obligation) and
the bare FleetAction admission rules. Some of those rules are in the assembled RFC-0001; some
are staged amendments to RFC-0001 that are not yet folded into it and are not published as
files. A reader holding only the published documents had nothing to look the second kind up
in. docs/rule-index.md is that place: every rule identifier, its one-line normative statement,
and where the specification states it.

WHAT IT READS. The definitions are read, never kept here:

  * rfcs/dist/RFC-0001-core-spec.md — the assembled specification. A rule table
    (`| ID | Rule |` or `| ID | Obligation |`) row `| **<id>[ — name]** | <statement> |`,
    and a bare-rule paragraph `**<Rn> — <name>.** <statement>`, each with the nearest
    preceding heading anchor as the place it is stated.
  * rfcs/amendments/RFC-0001-*.md — staged amendments, same two shapes, grouped under the
    amendment's own title. The status shown is the deciding ADR's (`decided-by:`), read from
    docs/adr/, because that is the decision in force; an amendment decided by no ADR shows
    its own. A `decided-by` ADR that cannot be found is exit 2.

`{{ref:<anchor>}}` in an amendment and `[§n](#<anchor>)` in the assembly are both rendered as
an `RFC-0001 #<anchor>` link into the assembled specification; an anchor it does not define is
exit 2, so the index never links a section that does not exist. An identifier defined twice
with different statements is exit 2 as well: the index cannot choose between two contracts.

THE CHECK (`--check`) fails, exit 1, on either of:

  1. STALE — the committed index differs from a fresh generation. Printed as a unified diff.
  2. UNINDEXED — a public file cites a rule identifier the index does not list. "Public file"
     is exactly scripts/check-error-strings.py's citation surface (its file set and its scope
     exclusions, imported rather than copied, so the two cannot drift), and a citation is a
     namespaced identifier of any family the index defines or any `XX-Rn`, or a bare `Rn`.
     A bare `Rn` also resolves against the runtime-assertion documents that gate already reads.

A checkout that carries the assembled specification but not the amendment sources (the
published tree) regenerates every row from the assembly and carries the staged rows over from
the committed index, saying how many. Those rows are re-derived where the sources are.

Usage: python3 scripts/gen-rule-index.py [--check] [--root DIR]
Exit:  0 clean / written · 1 findings · 2 the check could not run.
       A check that exits without having run is not a pass.
"""

from __future__ import annotations

import argparse
import difflib
import importlib.util
import os
import re
import sys
from dataclasses import dataclass
from pathlib import Path

INDEX = "docs/rule-index.md"
DIST = "rfcs/dist/RFC-0001-core-spec.md"
AMENDMENTS = "rfcs/amendments/RFC-0001-*.md"
CHECKER = "scripts/check-error-strings.py"
# The index is read from docs/, so a link into the assembled specification is relative to it.
DIST_LINK = "../rfcs/dist/RFC-0001-core-spec.md"

# An identifier: a two-letter namespace, a one-letter family, a number (`XX-Yn`), or a bare
# `Rn`. A letter suffix (`n` + `a`) is part of the identifier.
_NS_ID = r"[A-Z]{2}-[A-Z]\d+[a-z]?"
_BARE_ID = r"R\d+[a-z]?"
TABLE_HEAD = re.compile(r"^\|\s*ID\s*\|\s*(Rule|Obligation)\s*\|\s*$")
TABLE_SEP = re.compile(r"^\|\s*:?-+:?\s*\|\s*:?-+:?\s*\|\s*$")
TABLE_ROW = re.compile(
    rf"^\|\s*\*\*(?P<id>{_NS_ID}|{_BARE_ID})(?:\s*[—-]\s*(?P<name>[^*|]+?))?\*\*\s*\|"
    r"\s*(?P<text>.*?)\s*\|\s*$"
)
PARA_HEAD = re.compile(rf"^\*\*(?P<id>{_BARE_ID})\s+[—-]\s+(?P<name>[^*]+?)\.\*\*\s+(?P<text>.*)$")
ANCHOR = re.compile(r'^<a id="([^"]+)"></a>\s*$')
FENCE = re.compile(r"^\s*(```|~~~)")
FRONT = re.compile(r"^---\n(.*?)\n---\n", re.S)
REF_TOKEN = re.compile(r"\{\{ref:([a-z0-9][a-z0-9-]*)\}\}")
DIST_LINK_IN = re.compile(r"\[([^\]]*)\]\(#([a-z0-9][a-z0-9-]*)\)")
OTHER_LINK = re.compile(r"\]\((?!https?://|#|" + re.escape(DIST_LINK) + r"#)([^)]*)\)")

STAGED_BEGIN = "<!-- staged-rules: begin -->"
STAGED_END = "<!-- staged-rules: end -->"


class CannotRunError(Exception):
    """The index cannot be generated or checked from this checkout: exit 2."""


@dataclass(frozen=True)
class Rule:
    rid: str
    name: str
    text: str
    where: str  # a dist anchor, or "" for a staged rule (the group says where)
    source: str  # repo-relative path, for error messages only; never rendered


def sort_key(rid: str) -> tuple:
    m = re.match(r"^(?:([A-Z]{2})-)?([A-Z])(\d+)([a-z]?)$", rid)
    if not m:
        return (rid,)
    ns, fam, num, suf = m.groups()
    return (ns or "", fam, int(num), suf)


def ref_link(anchor: str) -> str:
    return f"[RFC-0001 #{anchor}]({DIST_LINK}#{anchor})"


def render_text(text: str, anchors: set[str], source: str, rid: str) -> str:
    def dist_link(m: re.Match) -> str:
        label, anchor = m.group(1), m.group(2)
        if anchor not in anchors:
            raise CannotRunError(
                f"{source}: {rid} links #{anchor}, which the assembly does not define"
            )
        return ref_link(anchor) if label.startswith("§") else f"[{label}]({DIST_LINK}#{anchor})"

    def ref_token(m: re.Match) -> str:
        anchor = m.group(1)
        if anchor not in anchors:
            raise CannotRunError(
                f"{source}: {rid} cites {{{{ref:{anchor}}}}}, which the assembly does not define"
            )
        return ref_link(anchor)

    out = REF_TOKEN.sub(ref_token, DIST_LINK_IN.sub(dist_link, text))
    other = OTHER_LINK.search(out)
    if other:
        raise CannotRunError(
            f"{source}: {rid} carries a relative link ({other.group(1)}) the index cannot follow"
        )
    return " ".join(out.split())


def parse_rules(path: Path, rel: str, anchors: set[str], track_anchor: bool) -> list[Rule]:
    """Every rule-table row and bare-rule paragraph in one document, in document order."""
    lines = path.read_text(encoding="utf-8").splitlines()
    rules: list[Rule] = []
    where = ""
    in_fence = False
    in_table = False
    i = 0
    while i < len(lines):
        line = lines[i]
        if FENCE.match(line):
            in_fence = not in_fence
            in_table = False
            i += 1
            continue
        if in_fence:
            i += 1
            continue
        a = ANCHOR.match(line)
        if a and track_anchor:
            where = a.group(1)
        if TABLE_HEAD.match(line) and i + 1 < len(lines) and TABLE_SEP.match(lines[i + 1]):
            in_table = True
            i += 2
            continue
        if in_table:
            if not line.startswith("|"):
                in_table = False
            else:
                m = TABLE_ROW.match(line)
                if m:
                    rid = m.group("id")
                    rules.append(
                        Rule(
                            rid,
                            (m.group("name") or "").strip(),
                            render_text(m.group("text"), anchors, rel, rid),
                            where,
                            rel,
                        )
                    )
                i += 1
                continue
        p = PARA_HEAD.match(line)
        if p:
            body = [p.group("text")]
            j = i + 1
            while j < len(lines) and lines[j].strip():
                body.append(lines[j].strip())
                j += 1
            rid = p.group("id")
            rules.append(
                Rule(
                    rid,
                    p.group("name").strip(),
                    render_text(" ".join(body), anchors, rel, rid),
                    where,
                    rel,
                )
            )
            i = j
            continue
        i += 1
    return rules


def front_matter(path: Path) -> dict[str, str]:
    m = FRONT.match(path.read_text(encoding="utf-8"))
    out: dict[str, str] = {}
    if m:
        for line in m.group(1).splitlines():
            k, _, v = line.partition(":")
            out[k.strip()] = v.strip().strip('"')
    return out


def staged_status(root: Path, rel: str, fm: dict[str, str]) -> str:
    """The status a staged amendment is shown with.

    An amendment's own `status:` records where its text stood when it was drafted. Once an
    ADR decides it, the ADR's status is the one in force, and showing the amendment's would
    tell a reader that rules the decision put in force are only proposed.
    """
    adr = fm.get("decided-by", "")
    if not adr:
        return fm["status"]
    m = re.fullmatch(r"ADR-(\d{4})", adr)
    if not m:
        raise CannotRunError(f"{rel}: decided-by {adr!r} is not an ADR-NNNN identifier")
    found = sorted(root.glob(f"docs/adr/{m.group(1)}-*.md"))
    if len(found) != 1:
        raise CannotRunError(f"{rel}: decided-by {adr} names no single file in docs/adr/")
    st = re.search(r"^- \*\*Status:\*\*\s*([A-Za-z]+)", found[0].read_text(encoding="utf-8"), re.M)
    if not st:
        raise CannotRunError(f"{found[0].relative_to(root).as_posix()} has no Status line")
    return f"{st.group(1).lower()} ({adr})"


def one(rules: list[Rule], seen: dict[str, Rule]) -> None:
    for r in rules:
        prev = seen.get(r.rid)
        if prev is None:
            seen[r.rid] = r
        elif (prev.name, prev.text) != (r.name, r.text):
            raise CannotRunError(
                f"{r.rid} is defined twice with different statements: {prev.source} and {r.source}"
            )


def cell(s: str) -> str:
    return s.replace("\n", " ")


def id_cell(r: Rule) -> str:
    # The source's own shape (`**<id> — <name>**`), which is also the shape
    # check-error-strings reads as a definition.
    return f"**{r.rid} — {r.name}**" if r.name else f"**{r.rid}**"


def render(assembled: list[Rule], staged_block: str) -> str:
    out = [
        "# Rule index",
        "",
        "<!-- Generated by scripts/gen-rule-index.py from the RFC-0001 sources.",
        "     Do not edit by hand; regenerate with `make rule-index`.",
        "     `make test` fails when this file is stale. -->",
        "",
        "Every rule identifier cited in this repository — in code comments, CRD descriptions,",
        "the adapter protocol, conformance output and documentation — is listed here with its",
        "normative statement and the place the specification states it. A public file that",
        "cites an identifier not listed here fails `make test`.",
        "",
        "The statements are copied from the specification, not restated. Where a statement",
        "here and the [assembled RFC-0001](" + DIST_LINK + ") differ, the specification",
        "governs.",
        "",
    ]
    bare = sorted((r for r in assembled if "-" not in r.rid), key=lambda r: sort_key(r.rid))
    if bare:
        # Computed, not spelled: this file is on the citation surface it checks.
        span = f"`{bare[0].rid}`" if len(bare) == 1 else f"`{bare[0].rid}` … `{bare[-1].rid}`"
        out += [
            f"Bare {span} are admission rules of the resource whose section states them.",
            "They are a different vocabulary from the runtime assertions `RA-n` defined in",
            "[architecture.md](architecture.md).",
            "",
        ]
    out += [
        "## Rules stated in RFC-0001",
        "",
        "| ID | Rule | Stated in |",
        "| :---- | :---- | :---- |",
    ]
    for r in sorted(assembled, key=lambda r: sort_key(r.rid)):
        out.append(f"| {id_cell(r)} | {cell(r.text)} | {ref_link(r.where)} |")
    out += ["", STAGED_BEGIN, staged_block.strip("\n"), STAGED_END, ""]
    return "\n".join(out)


def render_staged(groups: list[tuple[str, str, list[Rule]]]) -> str:
    out: list[str] = [
        "## Rules staged for RFC-0001",
        "",
        "These rules are amendments to RFC-0001 that its assembled text does not contain.",
        "This index is where they are published.",
    ]
    for title, status, rules in groups:
        out += [
            "",
            f"### {title}",
            "",
            f"Status: {status}.",
            "",
            "| ID | Rule |",
            "| :---- | :---- |",
        ]
        for r in sorted(rules, key=lambda r: sort_key(r.rid)):
            out.append(f"| {id_cell(r)} | {cell(r.text)} |")
    return "\n".join(out)


def committed_staged(committed: str) -> tuple[str, int]:
    """The staged block of the committed index, and how many rows it holds."""
    a, b = committed.find(STAGED_BEGIN), committed.find(STAGED_END)
    if a < 0 or b < a:
        raise CannotRunError(f"{INDEX} has no staged-rules block to carry over")
    block = committed[a + len(STAGED_BEGIN) : b].strip("\n")
    return block, sum(1 for ln in block.splitlines() if ln.startswith("| **"))


def generate(root: Path, committed: str | None) -> tuple[str, list[str]]:
    """Return (index text, notes)."""
    notes: list[str] = []
    dist = root / DIST
    if not dist.is_file():
        raise CannotRunError(f"{DIST} is not in this checkout; nothing to generate the index from")
    dist_text = dist.read_text(encoding="utf-8")
    anchors = set(re.findall(r'^<a id="([^"]+)"></a>', dist_text, re.M))

    seen: dict[str, Rule] = {}
    assembled = parse_rules(dist, DIST, anchors, track_anchor=True)
    for r in assembled:
        if not r.where:
            raise CannotRunError(f"{DIST}: {r.rid} precedes every heading anchor")
    one(assembled, seen)

    sources = sorted(root.glob(AMENDMENTS))
    if sources:
        groups = []
        for src in sources:
            rel = src.relative_to(root).as_posix()
            fm = front_matter(src)
            rules = parse_rules(src, rel, anchors, track_anchor=False)
            if not rules:
                continue
            if "title" not in fm or "status" not in fm:
                raise CannotRunError(f"{rel} defines rules but has no title/status front matter")
            one(rules, seen)
            groups.append((fm["title"], staged_status(root, rel, fm), rules))
        staged = render_staged(groups)
    else:
        if committed is None:
            raise CannotRunError(
                "the amendment sources are not in this checkout and there is no committed index "
                "to carry the staged rules from"
            )
        staged, n = committed_staged(committed)
        folded = sorted(indexed_ids(staged) & set(seen), key=sort_key)
        if folded:
            # A staged rule the assembly now states: identical or not, two rows for one id is
            # not an index. Where the sources are present this is caught by `one` above.
            raise CannotRunError(
                f"{', '.join(folded)} are stated in the assembly and still carried as staged; "
                "regenerate the index where the amendment sources are"
            )
        notes.append(
            f"rule-index: the amendment sources are not in this checkout; {n} staged row(s) "
            "carried over from the committed index, every assembled row regenerated"
        )
    return render(assembled, staged), notes


def load_checker(root: Path):
    path = root / CHECKER
    if not path.is_file():
        raise CannotRunError(
            f"{CHECKER} is not in this checkout; the citation surface is defined there"
        )
    spec = importlib.util.spec_from_file_location("check_error_strings_surface", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def indexed_ids(text: str) -> set[str]:
    return set(re.findall(rf"^\|\s*\*\*({_NS_ID}|{_BARE_ID})(?:\*\*|\s+—)", text, re.M))


def unindexed_citations(root: Path, index_text: str) -> tuple[list[str], int]:
    """Every public citation of a rule identifier the index does not list."""
    chk = load_checker(root)
    listed = indexed_ids(index_text)
    families = {re.sub(r"\d+[a-z]?$", "", i) for i in listed if "-" in i}
    ns_cite = re.compile(rf"\b({_NS_ID})\b")
    # Not after `-`: `\bRn` alone matches INSIDE a namespaced id, the confusion
    # check-error-strings.py documents at its label patterns.
    bare_cite = re.compile(rf"(?<![\w-])({_BARE_ID})\b")
    # A bare `Rn` may also be defined by the runtime-assertion documents check-error-strings
    # exempts and reads (`**Rn — ...**`); those are public and are that gate's vocabulary.
    other_defs: set[str] = set()
    for f in chk._LABEL_SOURCES:
        try:
            other_defs.update(chk._RULE_DEF.findall((root / f).read_text(encoding="utf-8")))
        except OSError:
            pass
    findings: list[str] = []
    scanned = 0
    for path in chk.iter_surface_files(["."]):
        if str(path).endswith(chk._LABEL_SOURCES):
            continue
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        scanned += 1
        normal, linemap = chk.normalise(text)
        for pat in (ns_cite, bare_cite):
            for m in pat.finditer(normal):
                rid = m.group(1)
                if pat is ns_cite:
                    fam = re.sub(r"\d+[a-z]?$", "", rid)
                    if fam not in families and not fam.endswith("-R"):
                        continue
                    if rid in listed:
                        continue
                elif rid in listed or rid in other_defs:
                    continue
                line = linemap[m.start()] if m.start() < len(linemap) else 0
                findings.append(f"{path}:{line}: {rid!r} is cited but {INDEX} does not list it")
    return findings, scanned


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--check", action="store_true", help="fail if the index is stale or incomplete")
    ap.add_argument("--root", default=str(Path(__file__).resolve().parent.parent))
    args = ap.parse_args()
    root = Path(args.root).resolve()
    index = root / INDEX
    committed = index.read_text(encoding="utf-8") if index.is_file() else None

    try:
        fresh, notes = generate(root, committed)
    except CannotRunError as e:
        print(f"gen-rule-index: {e}", file=sys.stderr)
        return 2

    if not args.check:
        index.parent.mkdir(parents=True, exist_ok=True)
        index.write_text(fresh, encoding="utf-8")
        for n in notes:
            print(n)
        print(f"gen-rule-index: wrote {INDEX} ({len(indexed_ids(fresh))} rules)")
        return 0

    for n in notes:
        print(n)
    if committed is None:
        print(
            f"gen-rule-index: {INDEX} does not exist; run `make rule-index`",
            file=sys.stderr,
        )
        return 1

    # The checker's scope patterns are written against repo-relative paths.
    os.chdir(root)
    try:
        findings, scanned = unindexed_citations(root, committed)
    except CannotRunError as e:
        print(f"gen-rule-index: {e}", file=sys.stderr)
        return 2
    if scanned == 0:
        print(
            "gen-rule-index: scanned 0 public files — the scope excludes everything",
            file=sys.stderr,
        )
        return 2

    failed = False
    if fresh != committed:
        failed = True
        print(f"{INDEX} is stale — it differs from a fresh generation. Run `make rule-index`.\n")
        sys.stdout.writelines(
            difflib.unified_diff(
                committed.splitlines(keepends=True),
                fresh.splitlines(keepends=True),
                f"{INDEX} (committed)",
                f"{INDEX} (generated)",
            )
        )
    if findings:
        failed = True
        print(f"\nRule identifiers cited on the public surface that {INDEX} does not list:\n")
        for f in findings:
            print(f)
        print(
            "\nDefine the rule in RFC-0001 (or its staged amendment) and run `make rule-index`,\n"
            "or cite a rule that exists."
        )
    if failed:
        return 1
    print(
        f"gen-rule-index: {INDEX} is current and lists every rule identifier cited "
        f"({len(indexed_ids(committed))} rules; {scanned} public files scanned)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
