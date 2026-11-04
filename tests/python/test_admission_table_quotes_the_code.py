# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""RFC-0001's admission-rule table quotes the rejection messages the code prints.

Why this file exists. RFC-0001's control-plane chapter lists the admission
rules in a table whose last column, "Rejection message", quotes each message in
backticks. An operator who is refused searches for that text. Four SwarmadaConfig
rows quoted messages the webhook never printed -- one of them, "to prevent
reservation release before auto-cancel fires", described a different hazard from the
one the webhook names ("a dual-execution window").

Each quoted message must match ONE string literal of non-test Go source under
`internal/` or `api/v1/` (webhook messages and CEL `message=` markers), read with its
`"..." +` continuations joined. The quote is matched whole and in order, not fragment
by fragment across the source: a quote stitched from two messages matches neither.
A placeholder written `<name>`, `'<name>'` or `\\"value\\"` must stand where the literal
has a format verb (`%q`, `%s`, `%d`, ...), and nowhere else. An ellipsis (`…`) stands for
text the table leaves out, format verbs included.

KNOWN MISMATCHES
    `_KNOWN` lists rows that still quote text no source prints. It is a debt list, not
    an allow-list: a listed row that starts matching fails `test_known_list_is_not_stale`
    until it is removed, so the list can only shrink.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
# The published RFC: the chapter sources are not promoted, the assembled dist is, and
# `spec-stale` holds it fresh against them.
TABLE = ROOT / "rfcs" / "dist" / "RFC-0001-core-spec.md"
_TABLE_INTRO = "*Admission webhook rules"

# (resource, the start of the quoted message) for rows that do not yet match.
_KNOWN: set[tuple[str, str]] = set()

_VERB = "\x00"
# A placeholder, with the quotes %q prints around it, or a quoted value (\"swarmada-config\").
_PLACEHOLDER = re.compile(r"""'<[^>]+>'|\\"<[^>]+>\\"|<[^>]+>|\\"[^"\\]*\\\"""")
_ELLIPSIS = re.compile(r"…|\.\.\.")


def _go_literals() -> set[str]:
    files = [p for p in (ROOT / "internal").rglob("*.go") if not p.name.endswith("_test.go")]
    files += [p for p in (ROOT / "api" / "v1").glob("*.go")
              if not p.name.endswith("_test.go") and not p.name.startswith("zz_generated")]
    lits = set()
    for p in files:
        # "first part " +
        #     "second part" is one message.
        text = re.sub(r'"\s*\+\s*\n\s*"', "", p.read_text())
        for s in re.findall(r'"((?:[^"\\\n]|\\.)*)"', text):
            # A format verb in the source stands for any value.
            lits.add(re.sub(r"%[-+# 0-9.]*[a-zA-Z]", _VERB, s.replace('\\"', '"')))
    return lits


def _quoted_rows() -> list[tuple[int, str, str]]:
    rows = []
    lines = TABLE.read_text().splitlines()
    start = next(i for i, line in enumerate(lines) if line.startswith(_TABLE_INTRO))
    in_table = False
    for n, line in enumerate(lines[start + 1:], start + 2):
        if line.startswith("|"):
            in_table = True
        elif in_table:
            break  # the admission table ends at its first non-table line
        if not line.startswith("| `"):
            continue
        cells = [c.strip() for c in line.split("|")]
        if len(cells) < 6:
            continue
        resource = cells[1].strip("`")
        # A quote may itself hold backticks (`swarmctl estop resume`): it ends at `"` + backtick.
        for quoted in re.findall(r'`"(.+?)"`', cells[4]):
            rows.append((n, resource, quoted))
    return rows


def _pattern(quoted: str) -> re.Pattern[str]:
    # The text an ellipsis leaves out includes the spaces around it ("own reports …" is
    # "own reports, and ...").
    quoted = re.sub(rf"\s*({_ELLIPSIS.pattern})\s*", r"\1", quoted)
    out, pos = [], 0
    for m in re.finditer(f"{_PLACEHOLDER.pattern}|{_ELLIPSIS.pattern}", quoted):
        out.append(_text(quoted[pos:m.start()]))
        out.append(".*?" if _ELLIPSIS.fullmatch(m.group()) else f"['\"]?{_VERB}['\"]?")
        pos = m.end()
    out.append(_text(quoted[pos:]))
    return re.compile("".join(out), re.S)


def _text(chunk: str) -> str:
    return r"\s+".join(re.escape(w) for w in chunk.split(" ")) if chunk else ""


def _matching_literals(quoted: str, literals: set[str]) -> set[str]:
    pattern = _pattern(quoted)
    return {lit for lit in literals if pattern.search(lit)}


def _matches(quoted: str, literals: set[str]) -> bool:
    return len(_matching_literals(quoted, literals)) == 1


def _mismatches() -> set[tuple[str, str]]:
    literals = _go_literals()
    out = set()
    for _, resource, quoted in _quoted_rows():
        if not _matches(quoted, literals):
            known = next((k for k in _KNOWN if k[0] == resource and quoted.startswith(k[1])), None)
            out.add(known or (resource, quoted))
    return out


def test_the_table_parses():
    rows = _quoted_rows()
    # Two CRD-pattern rows quote the API server's message instead; the next test reads them.
    assert len(rows) >= 20, f"only {len(rows)} quoted messages parsed from {TABLE.name}"
    assert any(r == "SwarmadaConfig" for _, r, _ in rows)
    # A quote holding backticks is read, not skipped.
    assert any("(`swarmctl estop resume`)" in q for _, _, q in rows), (
        "no quote holding `swarmctl estop resume` was parsed; the quote reader stops at a backtick")


def test_every_quoted_message_is_one_the_code_prints():
    literals = _go_literals()
    bad = []
    for n, resource, quoted in _quoted_rows():
        if any(k[0] == resource and quoted.startswith(k[1]) for k in _KNOWN):
            continue
        found = _matching_literals(quoted, literals)
        if len(found) != 1:
            bad.append(f"line {n}, {resource}: {quoted!r} matches {len(found)} Go literals"
                       + (f" {sorted(found)}" if found else ""))
    assert not bad, (
        "control-plane.md quotes rejection messages that are not one message the code prints: "
        + "; ".join(bad))


def test_known_list_is_not_stale():
    repaired = _KNOWN - _mismatches()
    assert not repaired, (
        f"these rows now match the code; remove them from _KNOWN: {sorted(repaired)}")


_API_PATTERN = re.compile(r"in body should match '([^']+)'")


def test_a_quoted_api_server_pattern_message_names_the_crd_pattern():
    """A row enforced by a CRD `pattern` quotes the API server's own message, not a Go literal.

    The Go-literal check above cannot read those, so this one does: the pattern the row
    quotes must be a `pattern:` some generated CRD under config/crd/bases declares.
    """
    crds = "\n".join(p.read_text() for p in (ROOT / "config" / "crd" / "bases").glob("*.yaml"))
    quoted = []
    for line in TABLE.read_text().splitlines():
        if "API server's own pattern message:" in line:
            quoted += _API_PATTERN.findall(line)
    assert quoted, "no row quotes an API server pattern message; this test read nothing"
    missing = [p for p in quoted if f"pattern: {p}\n" not in crds]
    assert not missing, f"quoted patterns no CRD declares: {missing}"


def test_the_matcher_can_fail():
    """A matcher that accepts anything would pass every row above."""
    literals = _go_literals()
    # A paraphrase of a real message.
    assert not _matches("disconnectedReservationTTLSeconds must exceed disconnectTimeoutSeconds "
                        "to prevent reservation release before auto-cancel fires", literals)
    # Stitched from two messages: FleetZone's field name and ZoneMaintenance's wording.
    assert not _matches("spec.parentZone '<name>' does not exist in namespace '<ns>'", literals)
    # A placeholder where the message has fixed text.
    assert not _matches("<field> is required when onDisconnect is AfterTimeout", literals)
    # Fixed text where the message has a format verb.
    assert not _matches("parentZone 'warehouse' not found in namespace '<ns>'", literals)
    assert _matches("disconnectTimeoutSeconds is required when onDisconnect is AfterTimeout",
                    literals)
    assert _matches("parentZone '<name>' not found in namespace '<ns>'", literals)
