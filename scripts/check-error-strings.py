#!/usr/bin/env python3
# Copyright 2026 The Swarmada Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Two gates on what a public file says: operator-facing voice, and internal citations.

Rationale and the full rule set: .vale/styles/Swarmada/README.md

CHECK 1 — VOICE (Go message strings).
Capitalisation and trailing punctuation are already enforced by staticcheck
ST1005 (see .golangci.yml). This checks the vocabulary ST1005 does not see:
marketing register, first person, apology, and emotional performance in a
message an operator reads while something is broken.

Only string literals passed to a message constructor are examined. Comments,
identifiers, and struct tags are not.

CHECK 2 — INTERNAL CITATIONS (the whole public surface).
A public file citing a tracker item or `design §5.5` points a reader at a document
they cannot read. Draft 4 spent three separate passes removing these and each
pass found sites the previous one missed, because both existing gates were blind
to the class in a different way:

  * PUBLICATION.yaml's C25 covers tracker identifiers (ITEM-, RESEARCH-, TRACKER-,
    Master Plan) but supplied no `design §` / `decision D` arm, so a clean
    `check-boundary.sh` run was never evidence about those. That arm is now C26.
  * Every check in the series was LINE-BASED. The 2026-09-03 sweep deleted `§N.N`
    from four comments where the citation wrapped a line break, leaving
    `(design\n// )` — a dangling parenthetical that reached operators through the
    CRD descriptions and `kubectl explain`. A line-based grep could not see the
    citation before the cut and could not see the scar after it.

So this check reads each file WHOLE and matches across comment line breaks, and it
flags the repair scar as well as the citation: an empty or malformed parenthetical
is itself evidence a previous sweep cut a citation out badly, and is itself a
defect on the surface an operator reads.

Severity follows the triage rule PUBLICATION.yaml states for C25 — a deny that
fails every run from the first day is one an operator learns to bypass, and is
worse than a warning they read:

  * DENY (exit 1): citations naming a document that is not public, a public file naming a
    private tree, and repair scars.
    The public surface carries none of these; a new one is a regression.
  * WARN (exit 0): a bare `§N.N` with no document named before it. ~400 of these
    are a standing repo convention meaning "RFC-0001", already resolved by
    the specification's own citation check. Listing them is a sweep, not a gate. `--strict`
    promotes them to findings and is how that sweep is run.

Usage: python3 scripts/check-error-strings.py [--strict] [path ...]   (default: .)
Exit:  0 clean · 1 findings · 2 the check could not run.
       A check that exits without having run is not a pass.
"""

from __future__ import annotations

import bisect
import re
import sys
from pathlib import Path

SKIP_DIRS = {".git", "vendor", "proto", "bin", "venv", ".venv", "node_modules"}
SKIP_FILES = re.compile(r"(_test\.go|^zz_generated.*\.go)$")

# Constructors whose string argument reaches an operator.
CONSTRUCTOR = re.compile(
    r"\b("
    r"errors\.New|fmt\.Errorf|fmt\.Sprintf|"
    r"log\.[A-Za-z]+|klog\.[A-Za-z]+|"
    r"\.Error|\.Errorf|\.Info|\.Infof|\.Warn|\.Warnf|"
    r"SetCondition|Message"
    r")\b"
)

# Interpreted and raw string literals.
LITERAL = re.compile(r'"(?:[^"\\\n]|\\.)*"' + r"|`[^`]*`")

BANNED = [
    (
        re.compile(r"\b(oops|whoops|uh-oh)\b", re.I),
        "performs an emotion at an operator who is debugging",
    ),
    (
        re.compile(r"\b(sorry|apolog\w+|unfortunately)\b", re.I),
        "apologises; a message apologises for nothing",
    ),
    (re.compile(r"\bplease\b", re.I), "is politeness in a machine message"),
    (
        re.compile(r"\b(we|we're|we've|our|us)\b", re.I),
        "gives a machine message a corporate voice",
    ),
    (
        re.compile(r"\b(simply|just|easily|obviously|trivially)\b", re.I),
        "asserts that something is easy",
    ),
    (
        re.compile(r"\b(magic|magical|awesome|great news|something went wrong)\b", re.I),
        "is marketing register or names nothing",
    ),
    (re.compile(r"!"), "uses an exclamation point"),
]

# ── Check 2: internal citations on the public surface ────────────────────────────────────

# What the citation check reads: the file types that carry prose a reader follows. It began as
# the specification citation check's set (.go .py .proto .md .yaml .yml .sh), which left the build
# and packaging files -- pyproject.toml, the Makefile, the Dockerfiles -- to the line-based
# pre-promote backstop, the one kind of check this file exists because it misses things. The
# Makefile is the most comment-dense file in the tree and has carried tracker citations to
# the public surface before. Config and text formats are read for the same reason.
CITE_SUFFIXES = {
    ".go", ".py", ".proto", ".md", ".yaml", ".yml", ".sh",
    ".toml", ".txt", ".json", ".ini", ".cfg",
}
# Files whose NAME, not suffix, says what they are. `Dockerfile.edge` has a suffix that says
# nothing, so the Dockerfile family is matched by name.
CITE_NAMES = re.compile(
    r"^(Makefile|GNUmakefile|Dockerfile(\..+)?|\.dockerignore|\.gitignore|CODEOWNERS|\.editorconfig)$"
)

# What it does not read, and why each one is out.
#   the private trees        — they may cite internal documents; that is what they are for
#   scratch and vendored     — unreviewed or not ours
#   generated files          — the citation lives in the source, and fixing the artefact
#                              is overwritten by the next `make generate`
#   deploy/swarmada          — the chart is generated from api/v1 by hack/sync-helm-crds.sh
#   this file                — it names every pattern below, so it matches itself
#
# THE LIST IS THE BOUNDARY FILE'S, and the two must not drift. This check asserts that a
# PUBLIC file cites nothing private, so a file the project has declared private is not its
# subject -- and it was reading 25 of them: `.claude/` (P04), `ml/` (P06), `scripts/media/`
# (P10) and `media/` (P11) are all `private_paths` entries in
# the boundary declaration, `PUBLICATION.yaml`. Private prose citing a private tracker was
# reported as a public file depending on a private one, and three such files were reworded to
# satisfy a gate that should never have read them.
#
# The patterns are copied here rather than read from that file, because P09 declares
# PUBLICATION.yaml itself private and this script is public: making it depend on that file at
# run time is the exact violation this check exists to catch, and it would fail outright in
# the published tree, where the boundary file is not present. The copy is held honest from
# outside instead -- `tests/python/test_citation_gate_scope.py` reads the boundary file and
# fails if any declared-private path is still scanned. Add a private_paths entry and that
# test names it; it does not have to be remembered here.
# The private planning tree's directory name, built from its parts so that this published file
# does not spell it (tests/python/test_citation_gate_scope.py does the same).
_PLANNING_DIR = "-".join(("internal", "planning"))
CITE_SKIP = re.compile(
    r"(^|/)("
    r"\.git|node_modules|venv|\.venv|__pycache__|\.pytest_cache|\.ruff_cache|\.mypy_cache|"
    r"vendor|bin|" + _PLANNING_DIR + r"|internal-projects|_backups|_to_delete|\.audit4-edits|"
    r"swarmada\.egg-info|conformance-reports|deploy|"
    # private_paths: P11 `^media/` and P10 `^scripts/media/` (one token covers both, and
    # there is no other `media/` in the tree), P04 and P05 the editor and agent config.
    r"media|\.claude|\.idea|\.vscode"
    r")/"
    # P06, anchored: the string this is matched against always starts with "/", and `edge/`
    # and `demo/` are PUBLIC everywhere but the top level -- `cmd/edge/`, `internal/edge/`
    # and `examples/edge-node/demo/` must stay in scope.
    r"|^/(ml|edge|demo|simulation/private)/"
    # P12/P13, anchored: the chapter trees and assembled documents of the four RFCs this release
    # does not publish, each declared `published: false` in its own manifest.
    r"|^/rfcs/(rfc-000[2567]-|dist/RFC-000[2567]-)"
    # P14 patch backups and rejects, anywhere; P15, anchored: the private review guide.
    r"|\.(orig|rej)$|^/REVIEW-GUIDE\.md$"
    # P17, anchored: the specification, demo-fixture and repository-gate tooling, which runs
    # only where the chapter sources and demo fixtures are.
    r"|^/scripts/(check-demo-expectations\.py|check-demo-manifests\.py|check-repo-gates\.sh|"
    r"check-spec-citations\.py|check-spec-headings\.py|check-spec-refs\.py|specdiff\.py|"
    r"check-action-lifecycle-single-source\.py|"
    r"spec-citation-baseline\.txt|crd-chapter-owners\.txt)$"
    # P16, anywhere: a folder an AI assistant writes its working drafts into.
    r"|(^|/)([Cc]laude|[Cc]odex|[Cc]ursor|[Cc]opilot|[Aa]gent|AI|ai|LLM|llm)[ _-]?[Oo]utputs?/"
    r"|\.pb\.go$|_pb2(?:_grpc)?\.py$|(^|/)zz_generated[^/]*$"
    r"|(^|/)check-error-strings\.py$"
)

# Lead-in words that introduce a citation. A scar is one of these left standing with the
# citation cut out from behind it.
_LEADIN = r"(?:design|context|see|cf\.?|per|section)"

# DENY: a citation naming a document the reader cannot open.
CITATIONS = [
    (
        re.compile(r"\bITEM-\d{4}\b"),
        "cites a tracker item; the tracker is not published",
    ),
    (
        re.compile(r"\bdesign\s+§", re.I),
        "cites the internal design document by section number",
    ),
    (
        re.compile(r"\bdecision\s+D\d+\b", re.I),
        "cites an internal decision record; public decisions are ADRs in docs/adr/",
    ),
    (
        re.compile(r"\bcontext\s+§", re.I),
        "cites the internal context document by section number",
    ),
    # A maintainer ruling id: `R-P<n>-<n>` (a pass ruling) or `R-<MMDD>-<n>` (a dated ruling).
    # The rulings are not published, so a public comment that cites one gives the reader an id
    # and nothing to look it up in. State the reason instead, with an RFC-0001 anchor. This file
    # is exempt from the scan; tests/python/test_error_string_ruling_ids.py checks that it
    # carries no ruling id of its own.
    (
        re.compile(r"\bR-(?:P\d+|\d{4})-\d+\b"),
        "cites a maintainer ruling; the rulings are not published",
    ),
    # Internal session and review labels: a remediation session, a review round, a gate ruling,
    # an audit finding. None is published, so a public file that names one gives the reader a
    # label and nothing to look it up in. State the reason, with an RFC-0001 anchor.
    # tests/python/test_error_string_session_ids.py plants each form and checks this file.
    (
        re.compile(r"\bREM\d+[a-z]?\b"),
        "names an internal session; internal sessions are not published",
    ),
    (
        re.compile(r"\bRR-\d+[a-z]?\b"),
        "names an internal session (a review round); internal sessions are not published",
    ),
    (
        re.compile(r"\bGate \d+ Ruling\b"),
        "names an internal session's ruling (a release gate); internal sessions are not published",
    ),
    (
        re.compile(r"\bAudit \d+ (?:finding|found)\b"),
        "names an internal session's finding (an audit); internal sessions are not published",
    ),
    (
        re.compile(r"\bRESEARCH-[A-Za-z]"),
        "cites an internal research note",
    ),
    (
        re.compile(r"\bTRACKER-[A-Za-z]"),
        "cites an internal tracker",
    ),
    (
        re.compile(r"\bMaster.?Plan\b", re.I),
        "cites the internal master plan",
    ),
    # A public file that NAMES a private tree as content: a `cd` into it in a runbook line, a
    # path in a table, a label domain built from its name. The boundary file's C04 denies these
    # line by line at promote time; this reads them whole, across line breaks. It ran as a
    # warn until the four sites standing when it was written were repaired, then was promoted.
    (
        re.compile(r"swarmada[-_]origin|internal[-_]planning|internal[-_]projects", re.I),
        "names a private tree; a reader outside this repository cannot follow it",
    ),
]

# DENY: the scar a bad excision leaves. Each alternative requires evidence of a CUT —
# a dangling `§`, trailing whitespace before the close, or an open parenthesis a sentence
# then ends without — so an ordinary parenthetical such as `()`, `(context)` or `(a, b)`
# is not a finding.
#
# THE UNCLOSED ARM, and why the first four were not enough. Every arm below it requires a
# closing `)`, because they were written for an excision that emptied a parenthetical and
# left it balanced. A different excision cut the section number AND the closing paren,
# leaving `(` followed by the sentence's own full stop:
#
#     // ActionPhasePaused: interrupted by estop; awaits an operator decision (.
#
# Five of those stood in api/v1/fleetaction_types.go, three of them baked into the
# generated CRD descriptions, so `kubectl explain fleetaction.spec.priority` rendered a
# description ending "at a zone or shared resource (." — on the public schema surface. This
# gate's own failure text already told authors "Cut the WHOLE parenthetical — a leftover
# `(design )` is itself a finding"; it simply could not see this shape of leftover. Three
# separate passes re-derived the five sites by hand before anything checked for them.
#
# The punctuation must be followed by whitespace or end of line, which is what makes it a
# sentence terminator rather than the first character of something ordinary. Without that
# lookahead the arm matched 116 innocent sites on this tree: markdown relative links
# `](../path)`, regex sources `(.*)` and `(.{0,1200}?)`, and ellipsis calls `f(...)`. The
# preceding-backslash guard keeps it off regex literals that escape their own paren.
SCAR = re.compile(
    rf"\((?:{_LEADIN}[ \t]+)*{_LEADIN}[ \t]*§[ \t]*\)"  # (design §)
    rf"|\((?:{_LEADIN}[ \t]+)*{_LEADIN}[ \t]+\)"  # (design )
    r"|\([ \t]*§[ \t]*\)"  # (§)
    r"|\([ \t]+\)"  # ( )
    r"|(?<!\\)\([ \t]*[.,;](?=[ \t]|$)",  # (.  -- cut took the close paren too
    # check-boundary.sh greps -i for every content rule, so C26 cannot tell `Context §2`
    # from `context §3`. These match case-insensitively for the same reason: the leak does
    # not become safe because someone capitalised it at the start of a sentence.
    re.I,
)
# `( )` is the one scar shape with an innocent twin: folding line breaks turns an ordinary
#     fmt.Sprintf(
#     )
# into `( )`. The other shapes carry a lead-in word or a dangling `§` and cannot be
# confused with code, so only this one is required not to span a line break. A real
# whitespace scar — `(§5.5)` cut to `( )` — sits on one line, which is exactly the
# discrimination this makes. The wrapped case is still caught: `(design\n// )` folds to
# `(design )` and matches the lead-in arm above.
SCAR_WHITESPACE_ONLY = re.compile(r"^\([ \t]+\)$")
SCAR_REASON = "is a parenthetical with its citation cut out — repair it, do not leave the scar"

# DENY, in comments and prose only: the scars a cut leaves when it took a citation out of the
# MIDDLE of a parenthetical, or took the parenthetical's close with it, instead of emptying it.
# Every SCAR arm above needs the parenthetical emptied down to a lead-in word, a `§` or
# whitespace, so none of these was seen:
#
#     // The design ( rule 3, ) puts the deduplication responsibility ...
#     // envelope's vocabulary ( so both bindings report outcomes ...
#     // ... is why this particular rule earns a test: aggregate( treats a nil
#     (`conformance/PROFILES.md`, *Support levels*,), so a `fail` there ...
#     // ... more recent successes exist
#     // . Owner-referenced children, so ...
#
# Each shape is ordinary in CODE -- a trailing comma before `)` closes every multi-line Go call
# and every one-element Python tuple, and `foo(` then a lowercase argument on the next line is
# how long calls wrap -- and the text is folded across ALL line breaks before matching, so code
# would match on almost every file. These arms therefore fire only when every source line the
# match touches is a comment line (or prose, in .md/.txt outside a fence), and not inside a
# `code span` on its line: `ifeq ($(GO),)` quoted in a Makefile comment is code.
PROSE_SCAR = re.compile(
    r"[,;][ \t]*\)"  # `(C1.3, )`, `*Support levels*,)`: the cut took what followed the comma
    r"|\([ \t]+[a-z]"  # `design ( rule 3`, `aggregate( treats`: the cut took what `(` opened
)
# `exist\n// . Owner`: a continuation line that opens on the sentence's own punctuation, which
# the cut stranded when it removed the parenthetical ending the line above. Only across a line
# break: on one line, `vale .` in a usage comment is a command, not a scar.
PROSE_SCAR_LEAD = re.compile(r"\w[ \t]+([.,;])(?=[ \t]|$)")
PROSE_SCAR_REASON = (
    "is what a cut through a parenthetical left behind — restore the sentence, "
    "do not leave the fragment"
)
_COMMENT_LINE = re.compile(r"^[ \t]*(?://|#(?!!)|/\*|\*(?:[ \t]|$)|--)")
_PROSE_SUFFIXES = {".md", ".txt"}
_FENCE = ("```", "~~~")


def prose_lines(path: Path, text: str) -> set[int]:
    """Line numbers (1-based) that hold comment or prose text rather than code."""
    lines = text.split("\n")
    if path.suffix not in _PROSE_SUFFIXES:
        return {n for n, line in enumerate(lines, 1) if _COMMENT_LINE.match(line)}
    out: set[int] = set()
    fenced = False
    for n, line in enumerate(lines, 1):
        if line.lstrip().startswith(_FENCE):
            fenced = not fenced
            continue
        if not fenced:
            out.add(n)
    return out


def prose_scars(path: Path, text: str, normal: str, linemap: list[int]) -> list[tuple[int, int]]:
    """(start, end) offsets into `normal` of each PROSE_SCAR / PROSE_SCAR_LEAD finding."""
    prose = prose_lines(path, text)

    def line_of(offset: int) -> int:
        return linemap[offset] if offset < len(linemap) else 0

    def in_code_span(offset: int) -> bool:
        line_start = bisect.bisect_left(linemap, line_of(offset))
        return normal.count("`", line_start, offset) % 2 == 1

    found: list[tuple[int, int]] = []
    for match in PROSE_SCAR.finditer(normal):
        first, last = line_of(match.start()), line_of(match.end() - 1)
        if all(n in prose for n in range(first, last + 1)) and not in_code_span(match.start()):
            found.append((match.start(), match.end()))
    for match in PROSE_SCAR_LEAD.finditer(normal):
        word, mark = line_of(match.start()), line_of(match.start(1))
        if word != mark and word in prose and mark in prose:
            found.append((match.start(1), match.end()))
    return found


# WARN: a section number with no document named in front of it. Bare `§9.1.2` conventionally
# means RFC-0001 here, but the convention is the reason `§9.6.2` survived the draft-4
# renumbering pointing at the wrong chapter.
BARE_SECTION = re.compile(r"§\s*\d+(?:\.\d+)+")
NAMES_DOC = re.compile(r"RFC-\d{4}|ADR-\d{4}|[A-Z][A-Za-z-]*\.md\b")
DOC_WINDOW = 120
# Inside rfcs/ the assembler owns numbering and emits `[§9.1.12](#anchor)` links by the
# thousand, so a bare number there is the house style rather than an under-qualified
# citation — the same carve-out the specification's citation check makes. The DENY classes above
# still apply to rfcs/: `design §` reaching the published spec is a real leak.
BARE_SECTION_SKIP = re.compile(r"(^|/)rfcs/")

# Every newline is folded to a single space, with any comment marker that opens the
# continuation line, so a citation split as `design\n// §5.5` is one string to match
# against. Offsets are mapped back to source line numbers for reporting.
CONTINUATION = re.compile(r"\n[ \t]*(?://+|\#+|\*(?!/)|--)?[ \t]*")


# ── Runtime assertion labels (RA-N) and rule ids (RN) ────────────────────────────────────
# These are NOT a fixed pattern, because some of them are published vocabulary and some are
# internal review-action labels, and the two are indistinguishable by shape. A label that a
# published document defines is a real concept a reader can look up; others, internal labels
# from an earlier review series, reached the wire contract, where an adapter author reading
# `(anti double-execution, R2/RA-<n>)` has nothing to look them up in. Worse, that `R2` collided
# with a DIFFERENT published `R2` — the forbidden-nine CEL rule on FleetAction.
#
# So the check resolves each reference against what is actually defined, and the defined set is
# read from the documents rather than hardcoded here: add `**RA-<n> — ...**` to architecture.md
# and that label becomes citable the same day, with no edit to this file.
_RA_DEF = re.compile(r"^\s*[-*]?\s*\*\*(RA-\d+[a-z]?)\s*[—-]", re.M)
_RULE_DEF = re.compile(r"^\s*(?:\|\s*)?[-*]?\s*\*\*(R\d+[a-z]?)\s*[—-]", re.M)

# A SECOND vocabulary, and the reason this file used to fail on a correct document.
# A chapter's own rule ids are namespaced -- `FZ-Rn` for FleetZone resolution and admission,
# `FA-Rn` for the adapter obligations -- and they are not runtime assertions at all. `-` is
# not a word character, so `\bR7\b` matched INSIDE `FZ-R7` and reported nineteen references
# in one chapter as undefined, while `FZ-R1`..`FZ-R5` passed by colliding with the bare
# runtime assertions that happen to exist -- a pass and a fail neither of which was about
# anything a reader would call correctness.
#
# The namespaced form is now matched WHOLE and resolved against namespaced definitions, so
# `FZ-R7` is checked rather than ignored: cite `FZ-R12` and it fails until something defines
# it. Definitions are read from the documents, like every other definition here -- a chapter
# introduces these in a table, either as `| **FZ-R1** |` or as `| **FA-R1 -- declare** |`, so
# both forms count. The BARE `Rn` pattern is deliberately left exactly as it was: a file
# citing an undefined bare `Rn` must still fail, which is the half of this gate that has
# caught real defects.
_NS_RULE_DEF = re.compile(r"^\s*(?:\|\s*)?[-*]?\s*\*\*([A-Z]{2}-R\d+[a-z]?)(?:\*\*|\s*[—-])", re.M)
_LABEL_REF = re.compile(r"\b([A-Z]{2}-R\d+[a-z]?|RA-\d+[a-z]?(?:-[A-Za-z0-9]+)?|R\d+[a-z]?)\b")
# Where the labels are DEFINED. These files may name any label; everything else must resolve.
_LABEL_SOURCES = ("docs/architecture.md",)
_DEFINED: set[str] = set()
# Read for definitions and still CHECKED themselves -- unlike _LABEL_SOURCES, which are
# exempt. The rule ids are read from the RULES INDEX, and only from it: docs/rule-index.md is
# generated by scripts/gen-rule-index.py from the assembled RFC-0001 and its staged
# amendments, and it is the one definition of them a published checkout carries. This used to
# read the chapter sources and the amendments directly; neither is published, so the gate was
# green where they are present and red where they are not -- a definition a reader cannot open
# is not one. That the index is current, and lists every id a public file cites, is
# `gen-rule-index.py --check`'s job.
_LABEL_SOURCE_GLOBS = ("docs/rule-index.md",)


def defined_labels(repo: Path) -> set[str]:
    """Every RA-N / RN a public reader can actually look up."""
    found: set[str] = set()
    paths = [repo / f for f in _LABEL_SOURCES]
    for g in _LABEL_SOURCE_GLOBS:
        paths.extend(repo.glob(g))
    for f in paths:
        try:
            t = f.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        found.update(_RA_DEF.findall(t))
        found.update(_RULE_DEF.findall(t))
        found.update(_NS_RULE_DEF.findall(t))
    return found


def normalise(text: str) -> tuple[str, list[int]]:
    """Return the text with comment line breaks folded, and a line number per character."""
    parts: list[str] = []
    lines: list[int] = []
    lineno = 1
    pos = 0
    for match in CONTINUATION.finditer(text):
        segment = text[pos : match.start()]
        parts.append(segment)
        lines.extend([lineno] * len(segment))
        lineno += text.count("\n", match.start(), match.end())
        parts.append(" ")
        lines.append(lineno)
        pos = match.end()
    segment = text[pos:]
    parts.append(segment)
    lines.extend([lineno] * len(segment))
    return "".join(parts), lines


def iter_go_files(roots: list[str]):
    for root in roots:
        for path in Path(root).rglob("*.go"):
            if SKIP_DIRS & set(path.parts):
                continue
            if SKIP_FILES.search(path.name):
                continue
            yield path


def iter_surface_files(roots: list[str]):
    for root in roots:
        for path in sorted(Path(root).rglob("*")):
            if not (path.suffix in CITE_SUFFIXES or CITE_NAMES.match(path.name)):
                continue
            if not path.is_file():
                continue
            if CITE_SKIP.search("/" + str(path)):
                continue
            yield path


def check_voice(roots: list[str]) -> list[str]:
    findings: list[str] = []
    for path in iter_go_files(roots):
        try:
            lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
        except OSError:
            continue
        for lineno, line in enumerate(lines, 1):
            if not CONSTRUCTOR.search(line):
                continue
            for literal in LITERAL.findall(line):
                for pattern, reason in BANNED:
                    hit = pattern.search(literal)
                    if hit:
                        findings.append(
                            f"{path}:{lineno}: {literal.strip()}\n" f"    {hit.group(0)!r} {reason}"
                        )
                        break
    return findings


def check_citations(roots: list[str]) -> tuple[list[str], list[str], int]:
    """Return (deny findings, warn findings, files scanned)."""
    global _DEFINED
    _DEFINED = defined_labels(Path(__file__).resolve().parent.parent)
    deny: list[str] = []
    warn: list[str] = []
    scanned = 0

    for path in iter_surface_files(roots):
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        scanned += 1
        normal, linemap = normalise(text)

        def where(offset: int) -> int:
            return linemap[offset] if offset < len(linemap) else 0

        def excerpt(start: int, end: int) -> str:
            return " ".join(normal[max(0, start - 60) : end + 30].split())

        for pattern, reason in CITATIONS:
            for match in pattern.finditer(normal):
                deny.append(
                    f"{path}:{where(match.start())}: {match.group(0)!r} {reason}\n"
                    f"    ...{excerpt(match.start(), match.end())}..."
                )
        if not str(path).endswith(_LABEL_SOURCES):
            for match in _LABEL_REF.finditer(normal):
                label = match.group(0)
                # `<label>-safe`, `<label>-consistent` — prose built off a DEFINED label, not a
                # citation of a new one. A lowercase-word suffix is an adjective; an uppercase
                # or numeric one (`<label>-D1`) is a sub-label and must resolve on its own.
                root = re.sub(r"-[a-z]+$", "", label)
                if label in _DEFINED or root in _DEFINED:
                    continue
                # Name the vocabulary the label is actually from. Reporting `FZ-R42` as a
                # "runtime-assertion label" is how the two got confused in the first place.
                kind = (
                    "chapter rule id"
                    if re.match(r"[A-Z]{2}-R", label)
                    else "runtime-assertion label"
                )
                deny.append(
                    f"{path}:{where(match.start())}: {label!r} is a {kind} "
                    f"that no public document defines — a reader has nothing to look it up in\n"
                    f"    ...{excerpt(match.start(), match.end())}..."
                )
        for match in SCAR.finditer(normal):
            if SCAR_WHITESPACE_ONLY.match(match.group(0)) and where(match.start()) != where(
                match.end() - 1
            ):
                continue
            deny.append(
                f"{path}:{where(match.start())}: {match.group(0)!r} {SCAR_REASON}\n"
                f"    ...{excerpt(match.start(), match.end())}..."
            )
        for start, end in prose_scars(path, text, normal, linemap):
            deny.append(
                f"{path}:{where(start)}: {normal[start:end]!r} {PROSE_SCAR_REASON}\n"
                f"    ...{excerpt(start, end)}..."
            )
        if BARE_SECTION_SKIP.search("/" + str(path)):
            continue
        for match in BARE_SECTION.finditer(normal):
            before = normal[max(0, match.start() - DOC_WINDOW) : match.start()]
            if not NAMES_DOC.search(before):
                warn.append(
                    f"{path}:{where(match.start())}: {match.group(0)!r} names no document\n"
                    f"    ...{excerpt(match.start(), match.end())}..."
                )

    return deny, warn, scanned


def main() -> int:
    argv = sys.argv[1:]
    strict = "--strict" in argv
    roots = [a for a in argv if not a.startswith("-")] or ["."]

    for root in roots:
        if not Path(root).exists():
            print(f"check-error-strings: {root} does not exist", file=sys.stderr)
            return 2

    voice = check_voice(roots)
    deny, warn, scanned = check_citations(roots)

    if scanned == 0:
        print(
            "check-error-strings: scanned 0 files — the scope patterns exclude everything.\n"
            "A check that exits without having run is not a pass.",
            file=sys.stderr,
        )
        return 2

    if voice:
        print("Message strings violating the documentation voice rules:\n")
        for finding in voice:
            print(finding)
        print(
            "\nA message names the object and the cause, adds at most one next action,\n"
            "and apologises for nothing. See .vale/styles/Swarmada/README.md"
        )

    if deny:
        print(f"\nInternal citations on the public surface ({scanned} files scanned):\n")
        for finding in deny:
            print(finding)
        print(
            "\nA public file must not depend on a private one. Replace the citation with\n"
            "the reason it encoded, or with an RFC-0001 anchor (RFC-0001 #some-anchor).\n"
            "Cut the WHOLE parenthetical — a leftover `(design )` is itself a finding."
        )

    if warn and strict:
        print(f"\nSection numbers naming no document ({len(warn)}):\n")
        for finding in warn:
            print(finding)

    if voice or deny or (warn and strict):
        return 1

    print(f"check-error-strings: no violations ({scanned} files scanned for citations)")
    if warn:
        print(
            f"check-error-strings: {len(warn)} bare section number(s) name no document "
            f"(warn; run with --strict to list)"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())
