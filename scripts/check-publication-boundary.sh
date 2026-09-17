#!/usr/bin/env bash
# check-publication-boundary.sh — run the publication boundary guard against THIS checkout.
#
#   scripts/check-publication-boundary.sh            # deny-severity findings fail
#   STRICT=1 scripts/check-publication-boundary.sh   # warnings fail too
#   make check-boundary / make check-boundary-strict
#
# WHY THIS WRAPPER EXISTS
#
# The guard's ruleset enumerates precisely what must never be published, so publishing
# it would publish the map. Guard and ruleset therefore live in the authoring tree, not
# here. The practical consequence for most of this repository's life was that the guard
# only ever ran there, against that tree — where it passes.
#
# A clean run there was never evidence about what is published. The two trees differ in
# hundreds of files. Two deny-severity findings reached GitHub in 6e74560 while the
# guard was reporting clean, because it was reading the other tree.
#
# This wrapper points the guard at the published checkout, which is the tree that is
# actually pushed.
#
# WHY THIS FILE IS NOT NAMED AFTER THE GUARD
#
# The ruleset denies the guard's own filename by path, anywhere in a publishable set,
# on the reasoning that if that rule fires something has copied the guard into a public
# checkout. A wrapper sharing that name fires it as a false positive — and a rule that
# cries wolf gets ignored, which is the one outcome that rule cannot afford. It also
# matches a not-scanned rule, so a wrapper named that way is never content-checked at
# all: it could carry anything and the guard would skip it.
#
# THE FILE LIST
#
#   git ls-files --cached --others --exclude-standard
#
# NOT bare `git ls-files`. Bare ls-files lists TRACKED files only, so it is blind to
# exactly the new files a promote is about to add — the moment when a file most often
# slips across. `--others --exclude-standard` adds untracked-not-ignored and honours
# .gitignore. The guard's own header makes the same point.
#
# LOCATING THE GUARD
#
# No default path is baked in: a literal path to the authoring tree is itself a
# deny-severity string in a published file, and a home-directory path is a warning.
# Supply it once and it is remembered:
#
#   git config swarmada.boundary-guard /path/to/the/guard/script
#
# or per-invocation, BOUNDARY_GUARD=/path/to/the/guard/script.
#
# WHY STRICT EXISTS
#
# The guard exits 0 for warn-severity findings and 1 only for deny. One warn-severity
# rule covers commercial vocabulary, and it is warn by design: its pattern cannot tell
# prose describing the market from prose describing this project, so it asks a human
# rather than blocking. That is right for the authoring tree, where drafts discuss the
# market.
#
# It is wrong for the published standard, where that vocabulary should not appear at
# all. Warn-severity is why a gate wired up here would have gone green with commercial
# material live in the assembled specification — which is what happened, for weeks.
# STRICT=1 makes warnings fatal, and is what the pre-push hook uses.
#
# Changing that rule's severity for real belongs in the ruleset, in the authoring tree.
# This wrapper is the published half and cannot reach it.
#
# EXIT CODES
#   0  clean
#   1  a finding this wrapper treats as fatal
#   2  the guard could not run

set -euo pipefail

STRICT="${STRICT:-0}"
SKIP_IF_UNAVAILABLE="${SKIP_IF_UNAVAILABLE:-0}"

cd "$(git rev-parse --show-toplevel)"

GUARD="${BOUNDARY_GUARD:-$(git config --get swarmada.boundary-guard || true)}"

if [ -z "$GUARD" ] || [ ! -x "$GUARD" ]; then
  # Most contributors do not have the authoring tree. Failing hard for them turns this
  # into something people uninstall, and the ruleset's own header warns about guards
  # that get switched off within a week. Say so loudly, then decide by policy — the one
  # thing never to do is pass silently.
  echo "boundary: guard not found." >&2
  echo "boundary: set it once with" >&2
  echo "boundary:   git config swarmada.boundary-guard /path/to/the/guard/script" >&2
  echo "boundary: or pass BOUNDARY_GUARD=/path/to/the/guard/script." >&2
  if [ "$SKIP_IF_UNAVAILABLE" = "1" ]; then
    echo "boundary: SKIPPED — this checkout was NOT verified against the ruleset." >&2
    exit 0
  fi
  exit 2
fi

rc=0
out=$(
  set -o pipefail
  git ls-files --cached --others --exclude-standard \
    | MODE=guard "$GUARD"
) || rc=$?

printf '%s\n' "$out"

if [ "$rc" -eq 2 ]; then
  echo "boundary: the guard could not run (exit 2)." >&2
  exit 2
fi

if [ "$rc" -ne 0 ]; then
  echo "boundary: deny-severity findings above. Not publishable." >&2
  exit 1
fi

if [ "$STRICT" = "1" ]; then
  # The guard prints an "N deny · M warn" tally; read the warn count off it rather than
  # re-implementing severity here.
  warns=$(printf '%s\n' "$out" | sed -n 's/.*·[[:space:]]*\([0-9][0-9]*\)[[:space:]]*warn.*/\1/p' | tail -1)
  warns="${warns:-0}"
  if [ "$warns" -gt 0 ]; then
    echo "boundary: STRICT — $warns warning(s) above are fatal on the published tree." >&2
    exit 1
  fi
fi

exit 0
