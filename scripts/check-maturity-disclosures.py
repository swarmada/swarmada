#!/usr/bin/env python3
"""check-maturity-disclosures.py — test a spec's "not implemented" claims against the CODE.

THE GAP THIS FILLS. A maturity disclosure can be checked two ways: against the document's
other statements, or against the code. Checking a document against itself is a real check and
it catches real things. It is structurally unable to catch the failure that actually happened.

WHAT ACTUALLY HAPPENED. RFC-0001 stated in FOUR places that the post-clear
`Resuming` window was specified but never entered by the implementation. The implementation
then landed it. The four disclosures were not retired, so the public safety chapter spent at
least a day telling implementers that a supervised window did not exist — and describing the
LESS SAFE behaviour as the shipped one. Five consecutive audit rounds passed the area. One of them
recorded the caveat as "stated in four separate places without contradiction".

Four consistent statements of a fact that has become false are worse than one, not better,
and no amount of internal-consistency checking can tell the difference. Consistency was
measured against the other statements and never against the code.

WHAT THIS CHECKS
    Every disclosure in an assembled RFC that claims a declared FIELD has no reader —
    "read by no controller", "no reader consumes", "schema-only", "specified, not
    implemented" — is tested against the Go tree:

      1. resolve the field's JSON name to its Go identifier via its `json:"..."` tag in api/
      2. search for that identifier OUTSIDE api/ and outside generated/test files
      3. a hit contradicts the claim

    A claim with a reader is reported. The disclosure is stale, or the reader is a bug; the
    gate does not decide which, it decides that somebody must look.

WHAT IT DOES NOT CATCH, stated so a green run is not over-read. It works on the "propagation
and staleness" failure mode, not on general correctness: semantic contradictions between
prose paragraphs, wrong-direction claims, and claims that are self-consistent and wrong
against the actual code path all survive it. It is a floor, not a replacement for the audit.

Exit 0 clean · 1 a disclosure the code contradicts · 2 the check could not run.
"""

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DIST = ROOT / "rfcs" / "dist"
BASELINE = ROOT / "scripts" / "maturity-disclosure-baseline.txt"

# Phrases that assert "nothing in the implementation reads this".
NO_READER = re.compile(
    r"(read by no controller|no reader consumes|reads? (?:it )?nowhere|"
    r"schema-only|is unread|and unread|consumed by nothing|"
    r"no controller reads (?:it|this)|specified, not consumed)",
    re.I,
)

# A CRD field path as the spec writes it: `spec.scheduling.actionRequeueBackoffSeconds`
FIELD = re.compile(r"`((?:spec|status)(?:\.[A-Za-z][A-Za-z0-9]*)+)`")

SKIP_DIRS = ("/api/", "/zz_generated", "/vendor/", "/_backups/", "/_to_delete/")


def fail(msg):
    print(f"check-maturity-disclosures: {msg}", file=sys.stderr)
    sys.exit(2)


def clauses(line):
    """Split a line into clauses, so a claim is paired only with the field it is about.

    The first draft matched a NO_READER phrase against the whole LINE. RFC-0001's maturity
    rows are single lines carrying a dozen sentences about a dozen fields, so one
    "schema-only" phrase was attributed to every backticked field on the row and produced 45
    findings of which almost all were noise. A gate that cries wolf gets suppressed, which
    is the same outcome as not having one.
    """
    return re.split(r"(?<=[.;])\s+|\s+\|\s+", line)


def go_identifier(json_name):
    """Resolve a JSON field name to its Go identifier via the struct tag in api/.

    Resolved from the tag rather than guessed by capitalising, because the two disagree
    exactly where it matters: `tdeCallTimeoutMs` is `TDECallTimeoutMs`, not `TdeCallTimeoutMs`.
    A guessed identifier finds nothing and the claim then reads as confirmed — a check that
    silently converts "I could not look" into "I looked and it was fine".
    """
    out = subprocess.run(
        [
            "grep",
            "-rhoE",
            rf'[A-Z][A-Za-z0-9]*\s+[^`]*`json:"{re.escape(json_name)}[,"]',
            str(ROOT / "api"),
        ],
        capture_output=True,
        text=True,
    ).stdout
    names = set()
    for line in out.splitlines():
        m = re.match(r"([A-Z][A-Za-z0-9]*)\s", line)
        if m:
            names.add(m.group(1))
    return names


def ambiguous(json_name, idents):
    """True when the JSON name maps to a field on several API types.

    `phase`, `startTime` and `timeoutSeconds` each exist on many CRDs. A bare-identifier
    grep cannot attribute a reader to the type the disclosure is about, so a hit proves
    nothing — and reporting it as a contradiction is worse than not checking, because it
    buries the real ones. These are counted as UNRESOLVABLE and printed, never silently
    dropped: "I could not look" must not read as "I looked and it was fine".
    """
    if len(idents) > 1:
        return True
    out = subprocess.run(
        ["grep", "-rc", rf'json:"{re.escape(json_name)}[,"]', "-r", str(ROOT / "api")],
        capture_output=True,
        text=True,
    ).stdout
    counts = (line.rsplit(":", 1)[-1] for line in out.splitlines())
    total = sum(int(c) for c in counts if c.isdigit())
    return total > 1


def readers(ident):
    """Non-api, non-generated, non-test references to a Go identifier."""
    out = subprocess.run(
        ["grep", "-rn", "--include=*.go", rf"\b{ident}\b", str(ROOT)],
        capture_output=True,
        text=True,
    ).stdout
    hits = []
    for line in out.splitlines():
        path = line.split(":", 1)[0]
        rel = path.replace(str(ROOT), "")
        if any(d in rel for d in SKIP_DIRS):
            continue
        if rel.endswith("_test.go"):
            continue
        hits.append(rel.lstrip("/") + ":" + line.split(":")[1])
    return hits


def main():
    docs = sorted(DIST.glob("*.md"))
    if not docs:
        fail(
            f"no assembled specs in {DIST}. "
            f"Nothing to check is not the same as clean."
        )

    known = set()
    if BASELINE.is_file():
        for ln in BASELINE.read_text().splitlines():
            ln = ln.split("#", 1)[0].strip()
            if ln:
                known.add(ln)

    claims = 0
    unresolved = 0
    findings = []
    unresolvable = []
    seen = set()

    for doc in docs:
        for lineno, line in enumerate(doc.read_text().splitlines(), 1):
            if not NO_READER.search(line):
                continue
            for clause in clauses(line):
                if not NO_READER.search(clause):
                    continue
                for field in FIELD.findall(clause):
                    json_name = field.split(".")[-1]
                    key = f"{doc.name}:{field}"
                    if key in seen:
                        continue
                    seen.add(key)
                    idents = go_identifier(json_name)
                    if not idents or ambiguous(json_name, idents):
                        unresolved += 1
                        unresolvable.append(
                            (
                                doc.name,
                                lineno,
                                field,
                                (
                                    "no Go identifier"
                                    if not idents
                                    else "the JSON name is a field on several API types"
                                ),
                            )
                        )
                        continue
                    claims += 1
                    for ident in sorted(idents):
                        hits = readers(ident)
                        if hits:
                            findings.append((doc.name, lineno, field, ident, hits))

    print(
        f"maturity disclosures — {len(docs)} document(s) · {claims} 'no reader' claim(s) "
        f"tested against the Go tree · {unresolved} not resolvable to one API field"
    )

    new = [f for f in findings if f"{f[2]}" not in known]
    print(
        f"                       {len(new)} contradicted by code · "
        f"{len(findings) - len(new)} known (baseline)"
    )

    if unresolvable:
        print(f"\n  {len(unresolvable)} claim(s) could not be tested. Listed, not counted as")
        print("  passing — a check that cannot look must not report that it looked:")
        for name, lineno, field, why in unresolvable[:8]:
            print(f"      {name}:{lineno}  `{field}` — {why}")
        if len(unresolvable) > 8:
            print(f"      ... and {len(unresolvable) - 8} more")

    if new:
        print()
        for name, lineno, field, ident, hits in new:
            print(f"  STALE  {name}:{lineno}")
            print(f"         claims `{field}` has no reader; Go identifier {ident} is read at:")
            for h in hits[:4]:
                print(f"             {h}")
            if len(hits) > 4:
                print(f"             ... and {len(hits) - 4} more")
        print(f"\n  {len(new)} disclosure(s) the code contradicts.")
        print("  Either the disclosure is stale or the reader is a bug. Consistency with the")
        print("  document's other statements is not evidence about either.")
        return 1

    print("\n  every testable 'no reader' claim still holds against the code")
    return 0


if __name__ == "__main__":
    sys.exit(main())
