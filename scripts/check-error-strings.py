#!/usr/bin/env python3
"""Check user-facing Go message strings against the documentation voice rules.

Rationale and the full rule set: .vale/styles/Swarmada/README.md

Capitalisation and trailing punctuation are already enforced by staticcheck
ST1005 (see .golangci.yml). This checks the vocabulary ST1005 does not see:
marketing register, first person, apology, and emotional performance in a
message an operator reads while something is broken.

Only string literals passed to a message constructor are examined. Comments,
identifiers, and struct tags are not.

Usage: python3 scripts/check-error-strings.py [path ...]   (default: .)
Exit:  0 clean, 1 violations found.
"""

from __future__ import annotations

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


def iter_go_files(roots: list[str]):
    for root in roots:
        for path in Path(root).rglob("*.go"):
            if SKIP_DIRS & set(path.parts):
                continue
            if SKIP_FILES.search(path.name):
                continue
            yield path


def main() -> int:
    roots = sys.argv[1:] or ["."]
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

    if findings:
        print("Message strings violating the documentation voice rules:\n")
        for finding in findings:
            print(finding)
        print(
            "\nA message names the object and the cause, adds at most one next action,\n"
            "and apologises for nothing. See .vale/styles/Swarmada/README.md"
        )
        return 1

    print("check-error-strings: no violations")
    return 0


if __name__ == "__main__":
    sys.exit(main())
