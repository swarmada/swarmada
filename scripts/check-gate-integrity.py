#!/usr/bin/env python3
"""check-gate-integrity.py — the gate that fails when a CHECK DID NOT RUN.

THE DEFECT CLASS. This repository builds correct checkers reliably and invokes them
unreliably. Every instance below was a check that was right, was shipped, and silently did
nothing:

  * an assembly target commented out, so the target existed and exited 0 without assembling
  * ``sys.exit(0 if ok else 0)`` — a checker that cannot return non-zero
  * a smoke harness printing ALL ASSERTIONS PASSED for fault cases that were never injected
  * a publication boundary guard run against the wrong checkout
  * a demo assertion checker reachable only from comments inside the files it validates
  * a composite target taking its gates as prerequisites, so the first failure hid six verdicts
  * a breaking-change check that prints "no 'main' branch — skipping" and exits 0
  * a test skipping when it cannot read the other half of the pair it exists to compare
  * per-adapter subtests that skip when the adapter is not in this checkout — which is normal
  * a conformance level reporting notApplicable for exactly the checks a rule demands be red

Each was caught by an attentive reader and fixed one at a time. That is the failure: THE CATCH
RATE DEPENDS ON WHO IS READING. This file is the mechanism that does not.

THE FIVE SHAPES, and the rule that catches each:

  R1 SILENT SKIP        A test or check that declines to run. Legitimate — optional control
                        plane binaries, adapters released separately — and therefore the
                        dangerous kind: a skip whose trigger is the NORMAL state is invisible
                        in a green run. Every skip site must be named in the register with a
                        reason and an expected count. A new one, or a higher count, fails.

  R2 TAUTOLOGY          A gate with no reachable non-zero termination. It cannot report a
                        finding, so its green is content-free. Not registerable: a gate that
                        cannot fail is wrong in every case, and the fix is the gate, not a
                        declaration.

  R3 UNREFERENCED       A gate no invoked thing invokes. Reachability is computed from CALL
                        SITES — recipe lines and workflow run blocks, then transitively
                        through the scripts those reach — never from a filename appearing in
                        prose, because the two most important checkers in this repository are
                        each NAMED by a file that does not run them.

  R4 INERT VERDICT      A graded obligation reported `notApplicable`. It leaves the pass/fail
                        counts entirely, so a level can report it while owing exactly the
                        check it reports away. Register it, with the reason it does not apply.

  EMPTY RECIPE          A make target documented as one (`##`) whose recipe runs nothing and
                        which has no prerequisite: every line commented out, or none at all.
                        It exits 0 having done nothing, which is how an assembly target was
                        disabled and still "passed". Not registerable, for R2's reason.

THE REGISTER IS A RATCHET, NOT AN AMNESTY. `gate-integrity-register.txt` fails in all four
directions, on the terms the documentation-voice register already uses: an unlisted site is
NEW, a higher count is WORSE, a lower count is SHRUNK (update the register — an accurate
register is the whole product), and a listed site that no longer exists is REPAIRED. The
register can only be correct; it cannot rot into a list nobody maintains.

THIS FILE NAMES NO TREE AND NO DIRECTORY. It resolves the repository root from its own
location and walks it. Keys are the last two path segments, so the register is portable
between checkouts that do not contain the same directories — and so that a register shipped
with a publishable file does not write down a path that only one checkout has.

A REGISTER MAY LIVE BESIDE ITS SUBTREE. Every file named `gate-integrity-register.txt` that the
walk reaches is loaded with the main one, on the same terms. A subtree that is not in every
checkout carries the entries for its own files, so a checkout without the subtree has neither
the files nor entries that would report them REPAIRED, and the main register names nothing that
only one checkout has. An entry named in two registers is exit 2: two reasons and two counts for
one site would let either register silently override the other.

WHAT THIS DOES NOT CATCH, stated so a green run is not over-read:

  * a gate that is invoked and asserts the wrong thing. Reachability is not correctness.
  * a gate whose target exists and which nobody types. In a checkout with no continuous
    integration, reachable is the whole of what is available — which is why this gate prints
    what would invoke these gates automatically in THIS checkout, and prints "nothing" when
    that is the answer.
  * a skip implemented by returning early rather than by calling a skip API.
  * an inert verdict expressed as a passing check over an empty input set.
  * anything inside an EVIDENCE directory (see below) that nothing live invokes. By rule it
    is a capture, not a gate; the rule refuses only the invocation.

Exit 0 every rule held · 1 a finding · 2 the check could not run.
"""

import re
import sys
from collections import defaultdict
from pathlib import Path

#: The tree under test. Resolved from this file's own location, never written down: the same
#: discipline every other gate in this family uses, and the reason this one can be invoked
#: from a recipe that does not name a directory only one checkout has.
#:
#: `--root DIR` points it at a different tree. That exists so the self-test beside this file
#: can build a small tree, plant one defect of each shape in it and watch each go red --
#: because a gate that has only ever seen a clean tree has not been tested, which is precisely
#: the defect it exists to catch.
#: The register's file name. The main register is the one beside this file (at the root under
#: `--root`); any other file of this name the walk reaches is an OVERLAY, loaded with it.
REGISTER_NAME = "gate-integrity-register.txt"
ROOT = Path(__file__).resolve().parent.parent
REGISTER = Path(__file__).resolve().parent / REGISTER_NAME
if "--root" in sys.argv:
    ROOT = Path(sys.argv[sys.argv.index("--root") + 1]).resolve()
    REGISTER = ROOT / REGISTER_NAME

#: Directories that cannot hold a gate or a graded obligation. Pruned so the walk is cheap and
#: so a vendored copy of somebody else's test suite is not held to this repository's register.
PRUNE = {
    ".git",
    ".github",
    "venv",
    ".venv",
    "node_modules",
    "__pycache__",
    "vendor",
    "_backups",
    "_to_delete",
    "bin",
    "dist",
    "site-packages",
    ".mypy_cache",
    ".pytest_cache",
    ".ruff_cache",
    "coverage",
    "testdata",
    "archive",
    "_archive",
}
PRUNE_PREFIX = ("watch_", "Claude ")

#: Directories that hold EVIDENCE: the frozen artefacts of one session -- the scripts it ran,
#: the output it captured, a scratch Makefile it typed into. A capture is not a gate and is
#: never invoked again; it is read. Judging it by the rules for live gates made this gate red
#: on an untouched tree, and the only ways to turn it green without this rule were to exempt
#: each capture by name (a register that grows with every session) or to edit the evidence to
#: dodge the naming convention, which is worse than a red gate.
#:
#: Pruned like PRUNE, with one difference that keeps the prune from HIDING a gate: a live
#: invocation -- a recipe, a workflow run block, or a script those reach -- that names a path
#: under one of these directories is a finding (EVIDENCE INVOKED). A gate put here and wired
#: is refused; a file put here and never invoked is evidence, which is what the directory says.
EVIDENCE = {"captures"}
#: A session's evidence directory, `track-reports/<session>-evidence/`, is the same thing under
#: another name: point-in-time copies of the scripts that session ran or edited. Two sessions
#: snapshotting the same script collide on one register key, so without this every new
#: snapshot of a shared script stops the gate. Scoped to that parent so a live directory that
#: happens to end in `-evidence` elsewhere is still walked.
EVIDENCE_SUFFIX, EVIDENCE_PARENT = "-evidence", "track-reports"


def is_evidence_dir(parent: str, name: str) -> bool:
    """True for a directory whose contents are evidence: read, never walked as live."""
    return name in EVIDENCE or (parent == EVIDENCE_PARENT and name.endswith(EVIDENCE_SUFFIX))


def names_evidence(tok: str) -> bool:
    """True when an invoked path runs something under an evidence directory. A token may be
    relative, so the parent test is dropped here: this side refuses, so broader is safer."""
    return any(seg in EVIDENCE or seg.endswith(EVIDENCE_SUFFIX) for seg in tok.split("/")[:-1])

# ── classification ────────────────────────────────────────────────────────────────────────

#: A GATE, by the naming convention this repository actually uses. Name-based on purpose: it
#: is mechanical, a reader can predict it, and the register carries the exceptions. `test_*.py`
#: (underscore) is a collected unit test and is invoked by its runner; `test-*.sh` (hyphen) is
#: a standalone script somebody has to invoke.
GATE_BASENAME = re.compile(
    r"^(check|verify|guard|audit|assert|lint|test)[-.].*\.(py|sh)$"
    r"|^.*[-_](gate|guard|check|lint|audit)\.(py|sh)$"
)

#: A non-zero termination is REACHABLE. `set -e` counts: an unchecked failing command under it
#: terminates the script non-zero, which is a real finding path.
SH_NONZERO = re.compile(
    r"(^|[;&|(\s])exit\s+(?![0]\b)(\$|\"?\$|[1-9])"
    r"|(^|[;&|(\s])return\s+[1-9]"
    r"|^\s*set\s+-[a-z]*e"
)
PY_NONZERO = re.compile(r"(sys\.exit|SystemExit)\(\s*(?![0]\s*\))")
#: The literal shape that started this: both branches of the conditional are zero, so the
#: condition is decorative. Flagged on sight, whatever else the file contains.
PY_TAUTOLOGY = re.compile(
    r"^\s*(?:sys\.exit|raise\s+SystemExit)\(\s*0\s+if\b.*\belse\s+0\s*\)", re.M
)

# ── skip sites ────────────────────────────────────────────────────────────────────────────

GO_SKIP = re.compile(r"\bt\.Skip(f|Now)?\s*\(|\bSkipf?\s*\(\s*[\"`]")
PY_SKIP = re.compile(r"pytest\.skip\s*\(|@pytest\.mark\.skip|unittest\.skip|raise\s+SkipTest")
#: A shell or recipe line that announces it is not doing the thing. The word must be in a
#: message the run prints, which is how these hide: the run says "skipping" and exits 0.
SH_SKIP = re.compile(r"(echo|printf|info|note|warn)\b[^|;&]*\bskipp?(ing|ed)\b", re.I)
#: A gate invocation whose failure is discarded. Same family: the check ran and cannot report.
SH_SWALLOW = re.compile(r"\|\|\s*(true\b|:\s*$|echo\b)")

GO_FUNC = re.compile(r"^func\s+(\w+)")
PY_FUNC = re.compile(r"^\s*def\s+(\w+)")
SH_FUNC = re.compile(r"^\s*([A-Za-z_][A-Za-z0-9_-]*)\s*\(\s*\)\s*\{")
MK_TARGET = re.compile(r"^([A-Za-z0-9][A-Za-z0-9_.\-]*)\s*:(?!=)")
PROFILE_NAME = re.compile(r"^\s*-?\s*name:\s*(\S+)")


class CollisionError(Exception):
    """Two files share a register key. Refused rather than merged -- see claim()."""


def key_for(path: Path) -> str:
    """The register key: the last two path segments.

    Two segments rather than the full relative path so the register survives a checkout that
    holds a different subset of this tree, and so a publishable register does not write down a
    directory that only one checkout has. A collision between two files with the same last two
    segments is exit 2, never a silent merge of their counts.
    """
    parts = path.relative_to(ROOT).parts
    return "/".join(parts[-2:]) if len(parts) >= 2 else parts[-1]


def walk():
    """Every file under ROOT that is not pruned."""
    stack = [ROOT]
    while stack:
        d = stack.pop()
        try:
            entries = sorted(d.iterdir())
        except OSError:
            continue
        for e in entries:
            if e.is_symlink():
                continue
            if e.is_dir():
                if (
                    e.name in PRUNE
                    or is_evidence_dir(d.name, e.name)
                    or e.name.startswith(PRUNE_PREFIX)
                ):
                    continue
                stack.append(e)
            elif e.is_file():
                yield e


def read(p: Path) -> str:
    try:
        return p.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return ""


def strip_hash_comments(line: str) -> str:
    """Drop a trailing `#` comment. Crude on purpose: quoting a `#` in a call site is rare,
    and the cost of the crudeness is a MISSED call site, which fails closed."""
    i = line.find("#")
    return line if i < 0 else line[:i]


def enclosing(lines, idx, pattern, default="-"):
    """The nearest declaration at or above `idx` matching `pattern`."""
    for j in range(idx, -1, -1):
        m = pattern.match(lines[j])
        if m:
            return m.group(1)
    return default


# ── R3: reachability, from call sites rather than from names ──────────────────────────────

#: An INVOCATION, as opposed to a mention. Read only where code runs -- recipe lines, workflow
#: run blocks, and the bodies of scripts -- never in a comment, a target line or help text. The
#: two checkers whose wiring this repository most cares about were each read as reachable by a
#: name search: one matched a make TARGET spelled like the file, the other was named twice in
#: PROSE inside a script that only discusses it.
INVOKE_FORMS = [
    re.compile(r"(?:^|[\s;&|(=`$@])(?:bash|sh|zsh|source|\.|exec)\s+\"?([^\s\"';|&)]+)"),
    re.compile(
        r"(?:^|[\s;&|(=`$@])(?:python3?|\$\(PYTHON\)|\$\{PYTHON\}|uv\s+run)\s+\"?([^\s\"';|&)]+)"
    ),
    re.compile(r"[\"']([^\s\"']+\.(?:sh|py))[\"']"),
    # A bare path token. Requires a `/`: a bare basename with no directory is how a name in
    # prose looks, and prose is what this must not count.
    re.compile(r"(?:^|[\s;&|(=`$@])([^\s\"';|&)]*/[^\s\"';|&)]*\.(?:sh|py))"),
]

#: A line whose command is a printer is TALKING about a path, not running it.
PRINTER = re.compile(r"^\s*[@-]*\s*(echo|printf|info|note|warn|cat|ls|grep|sed)\b")


def call_sites(text: str, kind: str) -> set:
    """Script paths this text INVOKES, as written. Comments are stripped first; a filename in
    prose is not a call site, and this repository has two checkers whose only 'reference' is
    exactly that. Reachability keys on the basename; the full token is kept so an invocation
    of a path under an EVIDENCE directory can be refused."""
    out = set()
    lines = text.splitlines()
    if kind == "make":
        # Recipe lines only. A target line `lint-py:` is not an invocation of `lint.py`, and
        # reading target lines is how a naive census first read the demo linter as wired.
        lines = [ln[1:] for ln in lines if ln.startswith("\t")]
    elif kind == "yaml":
        lines = _run_block_lines(lines)
    elif kind == "py":
        text2 = re.sub(r'("""|\'\'\')(?:.|\n)*?\1', "", text)
        lines = text2.splitlines()
    for ln in lines:
        ln = strip_hash_comments(ln)
        if not ln.strip() or PRINTER.match(ln):
            continue
        for rx in INVOKE_FORMS:
            for m in rx.finditer(ln):
                tok = m.group(1).strip("\"'")
                if tok.rsplit("/", 1)[-1].endswith((".sh", ".py")):
                    out.add(tok)
    return out


def _run_block_lines(lines):
    """The shell inside a workflow's `run:` steps, and nothing else."""
    out, depth = [], None
    for ln in lines:
        stripped = ln.strip()
        if depth is None:
            m = re.match(r"^(\s*)-?\s*run:\s*(.*)$", ln)
            if m:
                depth = len(m.group(1))
                if m.group(2) not in ("|", ">", "|-", ">-", ""):
                    out.append(m.group(2))
            continue
        if stripped and (len(ln) - len(ln.lstrip())) <= depth:
            depth = None
            m = re.match(r"^(\s*)-?\s*run:\s*(.*)$", ln)
            if m:
                depth = len(m.group(1))
                if m.group(2) not in ("|", ">", "|-", ">-", ""):
                    out.append(m.group(2))
            continue
        out.append(ln)
    return out


# ── the register ──────────────────────────────────────────────────────────────────────────

#: The register's four DEBT kinds and its two CLASSIFICATION kinds.
#:
#:   skip     a test or check that declined to run
#:   swallow  a gate invocation whose failure is discarded (`|| true`, `|| echo`)
#:   inert    a graded obligation reported notApplicable
#:   unwired  a real gate with no call site — DEBT, and the count may only shrink
#:   exempt   a file the naming convention read as a gate and which is not one
KINDS = ("skip", "swallow", "inert", "unwired", "exempt")


def load_register(path=None, reg=None, origin=None):
    """Parse one register into `reg`, keyed (kind, key) -> (count, reason).

    Called once for the main register and once per overlay, into the same dict. `origin`
    records where each entry was read, so an entry named twice -- in one register or across
    two -- is refused with both locations, never resolved by whichever was read last."""
    path = REGISTER if path is None else path
    reg = {} if reg is None else reg
    origin = {} if origin is None else origin
    if not path.exists():
        return None
    where = path.relative_to(ROOT) if path.is_relative_to(ROOT) else path
    for n, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        # The KEY itself contains `::` (file::symbol), so this cannot split on the
        # separator. Anchored on the count instead: the first `:: <digits> ::` closes the key.
        m = re.match(r"^(\w+)\s*::\s*(.+?)\s*::\s*(\d+)\s*::\s*(.+)$", line)
        if not m:
            print(
                f"check-gate-integrity: register {where} line {n} is malformed "
                f"(want `kind :: key :: count :: reason`): {raw}",
                file=sys.stderr,
            )
            return "malformed"
        kind, key, count, reason = m.group(1), m.group(2), m.group(3), m.group(4).strip()
        if kind not in KINDS:
            print(
                f"check-gate-integrity: register {where} line {n} has unknown kind {kind!r}; "
                f"one of {', '.join(KINDS)}",
                file=sys.stderr,
            )
            return "malformed"
        if not reason:
            print(
                f"check-gate-integrity: register {where} line {n} has no reason. A register entry "
                f"without one is an amnesty, which is what this file exists not to be.",
                file=sys.stderr,
            )
            return "malformed"
        if (kind, key) in reg:
            print(
                f"check-gate-integrity: register entry `{kind} :: {key}` is ambiguous — "
                f"{origin[(kind, key)]} and {where} line {n} both name it. Two counts for one "
                f"site would let either register silently override the other.",
                file=sys.stderr,
            )
            return "malformed"
        reg[(kind, key)] = (int(count), reason)
        origin[(kind, key)] = f"{where} line {n}"
    return reg


def load_overlays(files, reg, origin):
    """Load every register the walk reached other than the main one, into `reg`.

    Discovered, never listed: a subtree's register travels with the subtree, so this file names
    no directory, and a checkout that lacks the subtree lacks its register too. Returns the
    overlays read, or "malformed" (exit 2) on a bad line or an entry named twice."""
    main = REGISTER.resolve()
    overlays = sorted(p for p in files if p.name == REGISTER_NAME and p.resolve() != main)
    for p in overlays:
        if load_register(p, reg, origin) == "malformed":
            return "malformed"
    return overlays


# ── the scan ──────────────────────────────────────────────────────────────────────────────


def scan():
    try:
        return _scan()
    except CollisionError as c:
        k, a, b = c.args
        print(
            f"check-gate-integrity: register key {k!r} is ambiguous — "
            f"{a.relative_to(ROOT)} and {b.relative_to(ROOT)}. A key naming two files "
            f"would merge their counts and hide a change in either.",
            file=sys.stderr,
        )
        return None


def _scan():
    observed = defaultdict(int)  # (kind, key) -> count
    where = defaultdict(list)  # (kind, key) -> ["path:line  detail", ...]
    gates = {}  # basename -> Path
    gate_collisions = defaultdict(list)
    text_of = {}
    keys_seen = {}  # key -> Path, to catch a two-segment collision

    files = list(walk())

    for p in files:
        name = p.name
        rel = p.relative_to(ROOT)
        txt = read(p)
        text_of[p] = txt
        lines = txt.splitlines()
        k = key_for(p)

        def claim(kind, sym, detail):
            """Record one observation, refusing a key that would name two files.

            A collision would MERGE two files' counts, so a new skip in one could be masked by
            a removed skip in the other -- this gate's own defect, in this gate."""
            prev = keys_seen.setdefault(k, p)
            if prev != p:
                raise CollisionError(k, prev, p)
            observed[(kind, f"{k}::{sym}")] += 1
            where[(kind, f"{k}::{sym}")].append(detail)

        # R2/R3 census: candidate gates.
        is_gate = (
            name.endswith((".py", ".sh"))
            and GATE_BASENAME.match(name)
            and not name.startswith("test_")
        )
        if is_gate:
            prev = keys_seen.setdefault(k, p)
            if prev != p:
                raise CollisionError(k, prev, p)
            if name in gates:
                gate_collisions[name] += [gates[name], p]
            gates[name] = p

        # R1: skip sites.
        if name.endswith("_test.go"):
            for i, ln in enumerate(lines):
                if ln.lstrip().startswith("//"):
                    continue
                if GO_SKIP.search(ln):
                    claim("skip", enclosing(lines, i, GO_FUNC), f"{rel}:{i+1}")
        elif name.endswith(".py"):
            for i, ln in enumerate(lines):
                if ln.lstrip().startswith("#"):
                    continue
                if PY_SKIP.search(ln):
                    claim("skip", enclosing(lines, i, PY_FUNC), f"{rel}:{i+1}")
        elif name.endswith((".sh", ".bash")) or name == "Makefile" or name.endswith(".mk"):
            is_make = name == "Makefile" or name.endswith(".mk")
            for i, ln in enumerate(lines):
                body = ln[1:] if (is_make and ln.startswith("\t")) else ln
                if body.lstrip().startswith("#"):
                    continue
                if SH_SKIP.search(body):
                    claim(
                        "skip",
                        enclosing(lines, i, MK_TARGET if is_make else SH_FUNC),
                        f"{rel}:{i+1}",
                    )
                if SH_SWALLOW.search(body) and GATE_BASENAME.match(name):
                    claim("swallow", enclosing(lines, i, SH_FUNC), f"{rel}:{i+1}")

        # R4: inert verdicts in the graded reports.
        if name.endswith((".yaml", ".yml")) and "conformance-report" in str(rel):
            # The report's OWN arithmetic, not a recount of it. `skipped:` entries also appear
            # in the full checks list, so counting `- checkId:` lines double-counts every skip
            # and produces a register nobody can reconcile against the document.
            for i, ln in enumerate(lines):
                if re.match(r"^\s*result:\s*notApplicable\s*$", ln):
                    claim("inert", "verdict", f"{rel}:{i+1}")
                m = re.match(r"^\s*skip:\s*([0-9]+)\s*$", ln)
                if m:
                    for _ in range(int(m.group(1))):
                        claim("skip", "statistics", f"{rel}:{i+1}")
                m = re.match(r"^\s*notApplicable:\s*([0-9]+)\s*$", ln)
                if m:
                    for _ in range(int(m.group(1))):
                        claim("inert", "statistics", f"{rel}:{i+1}")

    if gate_collisions:
        for n, ps in gate_collisions.items():
            print(
                f"check-gate-integrity: more than one gate named {n!r}: "
                + ", ".join(str(x.relative_to(ROOT)) for x in dict.fromkeys(ps)),
                file=sys.stderr,
            )
        return None

    return files, observed, where, gates, text_of


def empty_recipes(text: str) -> list:
    """(line, target) for each documented target that runs nothing and depends on nothing.

    Make treats blank lines and comment lines among a recipe as part of it, so the recipe ends
    at the first line that is neither, a tab-led line, nor blank; a recipe whose every tab-led
    line is a comment is empty. A prerequisite, or an inline `target: ; cmd`, is work.
    """
    lines, out = text.split("\n"), []
    for i, ln in enumerate(lines):
        m = MK_TARGET.match(ln)
        if not m or "##" not in ln:
            continue
        head = ln.split("##", 1)[0].split(":", 1)[1]
        prereqs, _, inline = head.partition(";")
        if prereqs.strip() or inline.strip():
            continue
        runs = False
        for nxt in lines[i + 1 :]:
            if nxt.startswith("\t"):
                body = nxt[1:].strip()
                if body and not body.startswith("#"):
                    runs = True
                    break
            elif nxt.strip() and not nxt.lstrip().startswith("#"):
                break
        if not runs:
            out.append((i + 1, m.group(1)))
    return out


def reachability(files, gates, text_of):
    """Basenames invoked, transitively, from the roots a person or a runner actually enters:
    every Makefile's recipes and every workflow's run blocks.

    Nodes are PATHS, not basenames. A tree with three Makefiles collapsed to one node once, and
    the collapse silently emptied the main one's call sites -- this gate's own defect, in this
    gate, found by running it.
    """
    edges, by_base, roots = {}, defaultdict(list), []
    mk_text = ""
    for p in files:
        base = p.name
        by_base[base].append(p)
        is_mk = base == "Makefile" or base.endswith(".mk")
        is_wf = "/workflows/" in str(p) and p.suffix in (".yml", ".yaml")
        kind = (
            "make"
            if is_mk
            else (
                "yaml"
                if is_wf
                else "py" if p.suffix == ".py" else "sh" if p.suffix in (".sh", ".bash") else None
            )
        )
        if kind is None:
            continue
        edges[p] = call_sites(text_of[p], kind)
        if is_mk:
            roots.append(p)
            if p.parent == ROOT:
                mk_text += text_of[p]
        elif is_wf:
            # A second root set. These fire nowhere in a checkout with no remote, which is why
            # this gate prints its invocation summary rather than letting green stand in for
            # "something runs these".
            roots.append(p)

    reached, queue, seen = set(), list(roots), set(roots)
    evidence_calls = []  # (live invoker, token) for each invocation of a path under EVIDENCE
    while queue:
        node = queue.pop()
        for tok in sorted(edges.get(node, ())):
            base = tok.rsplit("/", 1)[-1]
            reached.add(base)
            if names_evidence(tok):
                evidence_calls.append((node, tok))
            for tgt in by_base.get(base, ()):
                if tgt not in seen:
                    seen.add(tgt)
                    queue.append(tgt)
    return reached, mk_text, evidence_calls


def nonzero_reachable(p: Path, txt: str) -> bool:
    if p.suffix == ".py":
        return bool(PY_NONZERO.search(txt))
    body = "\n".join(strip_hash_comments(ln) for ln in txt.splitlines())
    return bool(SH_NONZERO.search(body))


def invocation_summary(mk_text: str) -> list:
    """What, in THIS checkout, would run a gate without a person typing it.

    Computed, never asserted: the answer differs between checkouts of the same repository, and
    a sentence hard-coding either answer is false in the other one."""
    out = []
    has_vcs = (ROOT / ".git").exists()
    wf = ROOT / ".github" / "workflows"
    n_wf = len(list(wf.glob("*.yml"))) + len(list(wf.glob("*.yaml"))) if wf.is_dir() else 0
    if has_vcs and n_wf:
        out.append(
            f"This checkout is under version control and carries {n_wf} workflow file(s); "
            f"whether they fire depends on the remote they are pushed to."
        )
    elif n_wf:
        out.append(
            f"THIS CHECKOUT IS NOT UNDER VERSION CONTROL and carries {n_wf} workflow "
            f"file(s). No workflow here has ever fired and none can: the files are a "
            f"SOURCE DOCUMENT, not an invocation. Every gate below runs when a person "
            f"types it, and at no other time."
        )
    else:
        out.append(
            "This checkout carries no workflow files. Every gate below runs when a "
            "person types it, and at no other time."
        )
    return out


def main() -> int:
    origin = {}  # (kind, key) -> "register line N", across every register loaded
    reg = load_register(REGISTER, None, origin)
    if reg == "malformed":
        return 2
    if reg is None:
        print(
            f"check-gate-integrity: no register at {REGISTER.relative_to(ROOT)}. "
            f"A gate that cannot read the register it ratchets against has not passed — it "
            f"has not run. Seed it with --print-register.",
            file=sys.stderr,
        )
        return 2

    res = scan()
    if res is None:
        return 2
    files, observed, where, gates, text_of = res
    overlays = load_overlays(files, reg, origin)
    if overlays == "malformed":
        return 2
    reached, mk_text, evidence_calls = reachability(files, gates, text_of)

    findings = []

    # ── evidence is read, never run ───────────────────────────────────────────────────────
    # The walk does not enter EVIDENCE directories, so a gate living in one would escape R1
    # and R2. It can only matter if something live runs it; that invocation is the finding.
    for node, tok in evidence_calls:
        findings.append(
            (
                "EVIDENCE INVOKED",
                str(node.relative_to(ROOT)),
                f"invokes {tok}, which is under an evidence directory "
                f"({', '.join(sorted(EVIDENCE))}, {EVIDENCE_PARENT}/*{EVIDENCE_SUFFIX}). "
                f"Evidence is not scanned as a gate, so a "
                f"gate run from there is unchecked. Move the gate out of the capture, or stop "
                f"invoking it.",
            )
        )

    # ── R2 tautology ──────────────────────────────────────────────────────────────────────
    exempt_keys = {k for (kind, k) in reg if kind == "exempt"}
    for base, p in sorted(gates.items()):
        if key_for(p) in exempt_keys:
            continue
        txt = text_of[p]
        rel = p.relative_to(ROOT)
        if p.suffix == ".py" and PY_TAUTOLOGY.search(txt):
            findings.append(
                (
                    "R2 TAUTOLOGY",
                    str(rel),
                    "both branches of the exit conditional are 0; this gate cannot "
                    "report a finding",
                )
            )
        elif not nonzero_reachable(p, txt):
            findings.append(
                (
                    "R2 TAUTOLOGY",
                    str(rel),
                    "no reachable non-zero termination; this gate cannot report a "
                    "finding, so its green means nothing",
                )
            )

    # ── R3 unreferenced ───────────────────────────────────────────────────────────────────
    unwired = []
    for base, p in sorted(gates.items()):
        k = key_for(p)
        if k in exempt_keys:
            continue
        if base in reached:
            if ("unwired", k) in reg:
                findings.append(
                    (
                        "REPAIRED unwired",
                        k,
                        "registered as unwired, but something now invokes it — " "delete the entry",
                    )
                )
            continue
        if ("exempt", k) in reg:
            continue
        if ("unwired", k) in reg:
            unwired.append(k)
            continue
        findings.append(
            (
                "R3 UNREFERENCED",
                str(p.relative_to(ROOT)),
                "no recipe line, no workflow run block and no invoked script calls "
                "this. Give it a target; or record it `unwired` with the reason it "
                "is still owed one; or `exempt` if the naming convention read it as "
                "a gate and it is not one.",
            )
        )

    # ── empty recipe ──────────────────────────────────────────────────────────────────────
    for p in files:
        if p.name != "Makefile" and p.suffix != ".mk":
            continue
        for line, target in empty_recipes(read(p)):
            findings.append(
                (
                    "EMPTY RECIPE",
                    f"{p.relative_to(ROOT)}:{line}",
                    f"target `{target}` is documented but runs nothing and depends on nothing: "
                    f"it exits 0 having done nothing. Restore the recipe, or delete the target.",
                )
            )

    # A file the register classifies as NOT a gate contributes no gate observations. Keeping
    # its `|| true`s in the register would fill the debt list with shell idiom from scripts
    # this gate has already agreed are not gates.
    for key in [k for k in observed if k[1].split("::")[0] in exempt_keys and k[0] == "swallow"]:
        del observed[key]

    # ── R1 / R4 register ratchet ──────────────────────────────────────────────────────────
    for (kind, key), n in sorted(observed.items()):
        if (kind, key) not in reg:
            findings.append(
                (f"NEW {kind}", key, f"{n} site(s), unregistered — {where[(kind, key)][0]}")
            )
        else:
            want, _ = reg[(kind, key)]
            if n > want:
                findings.append((f"WORSE {kind}", key, f"{want} registered, {n} found"))
            elif n < want:
                findings.append(
                    (
                        f"SHRUNK {kind}",
                        key,
                        f"{want} registered, {n} found — lower the register; an "
                        f"inaccurate register is the product failing, not improving",
                    )
                )
    for (kind, key), (want, _) in sorted(reg.items()):
        if kind in ("exempt", "unwired"):
            # Both classify a FILE rather than count observations. `unwired` has its own
            # REPAIRED rule in R3, which fires when something starts invoking it.
            if key not in {key_for(p) for p in gates.values()}:
                findings.append(
                    (
                        f"REPAIRED {kind}",
                        key,
                        f"registered as {kind}, but no gate of that name exists",
                    )
                )
            continue
        if (kind, key) not in observed:
            findings.append(
                (f"REPAIRED {kind}", key, f"{want} registered, 0 found — delete the entry")
            )

    # ── report ────────────────────────────────────────────────────────────────────────────
    n_skip = sum(n for (k, _), n in observed.items() if k == "skip")
    n_swal = sum(n for (k, _), n in observed.items() if k == "swallow")
    n_inert = sum(n for (k, _), n in observed.items() if k == "inert")
    n_exempt = len(exempt_keys)
    # An UNREGISTERED unreferenced gate is not wired either. Counting it as wired because it is
    # not on the debt list would print a reassuring number beside the finding that contradicts
    # it -- the shape this gate exists to refuse, in this gate's own summary line.
    n_orphan = sum(1 for f in findings if f[0] == "R3 UNREFERENCED")
    print(
        f"gate-integrity: {len(gates)} gate(s) — "
        f"{len(gates) - len(unwired) - n_exempt - n_orphan} wired, "
        f"{len(unwired)} unwired (registered debt), {n_orphan} unreferenced and unregistered, "
        f"{n_exempt} not gates"
    )
    print(
        f"gate-integrity: skips {n_skip} · swallowed failures {n_swal} · "
        f"inert verdicts {n_inert} — every one named in the register"
    )
    if overlays:
        # Printed so a green run in a checkout WITHOUT a subtree's register does not read as
        # having checked that subtree's entries, and so one with it shows they were loaded.
        print(
            f"gate-integrity: registers read — {REGISTER.relative_to(ROOT)}, and beside a "
            f"subtree: {', '.join(str(p.relative_to(ROOT)) for p in overlays)}"
        )
    if unwired:
        print(
            f"gate-integrity: UNWIRED DEBT — {', '.join(unwired)}. Registered, so this run is "
            f"not a claim that they ran."
        )
    for line in invocation_summary(mk_text):
        print(f"gate-integrity: {line}")

    if findings:
        print()
        for rule, subject, detail in findings:
            print(f"  {rule:<18} {subject}")
            print(f"  {'':<18}   {detail}")
        print()
        print(
            f"gate-integrity: {len(findings)} finding(s). A check that exits 0 without "
            f"running is not a pass."
        )
        return 1
    print("gate-integrity: 0 finding(s).")
    return 0


def print_register():
    res = scan()
    if res is None:
        return 2
    files, observed, where, gates, text_of = res
    reached, _ = reachability(files, gates, text_of)
    for (kind, key), n in sorted(observed.items()):
        print(f"{kind} :: {key} :: {n} :: REASON REQUIRED   # {where[(kind, key)][0]}")
    for base, p in sorted(gates.items()):
        if base not in reached:
            print(f"unwired :: {key_for(p)} :: 0 :: REASON REQUIRED")
    return 0


if __name__ == "__main__":
    if "--print-register" in sys.argv:
        sys.exit(print_register())
    sys.exit(main())
