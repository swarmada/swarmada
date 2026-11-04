#!/usr/bin/env bash
# test-check-gate-integrity.sh — prove that scripts/check-gate-integrity.py REFUSES.
#
# A GATE THAT HAS ONLY EVER SEEN A CLEAN TREE HAS NOT BEEN TESTED, which is precisely the
# defect that gate exists to catch. This plants one instance of each of the four shapes it
# claims to catch, asserts it goes red on each, removes the plant, and asserts it goes green
# again. Both halves matter: a checker that refuses everything is as useless as one that
# refuses nothing.
#
# The plants land in a SCRATCH TREE built here, never in the checkout. A self-test that edits
# the repository to prove a point leaves the repository edited when it is interrupted.
#
# Exit 0 every case held · 1 a case failed · 2 the test could not run.
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GATE="$ROOT/scripts/check-gate-integrity.py"
[ -f "$GATE" ] || { echo "no checker at $GATE" >&2; exit 2; }
PY=${PYTHON:-python3}
command -v "$PY" >/dev/null 2>&1 || { echo "no python interpreter" >&2; exit 2; }

TMP=$(mktemp -d) || exit 2
trap 'rm -rf "$TMP"' EXIT

pass=0 fail=0

# ── a minimal, CLEAN tree: one wired gate, one register that accounts for it ──────────────
seed() {
  rm -rf "$TMP/t"; mkdir -p "$TMP/t/gates" "$TMP/t/conformance-reports"
  printf 'check:\n\tbash gates/check-wired.sh\n' > "$TMP/t/Makefile"
  cat > "$TMP/t/gates/check-wired.sh" <<'G'
#!/usr/bin/env bash
[ -f wanted ] || exit 1
exit 0
G
  : > "$TMP/t/gate-integrity-register.txt"
}

run() { "$PY" "$GATE" --root "$TMP/t" >"$TMP/out" 2>&1; echo $?; }

expect() { # expect <exit> <substring> <label>
  local want="$1" needle="$2" label="$3" got
  got=$(run)
  if [ "$got" = "$want" ] && grep -qF "$needle" "$TMP/out"; then
    echo "  pass  $label"; pass=$((pass+1))
  else
    echo "  FAIL  $label — exit $got (want $want), looking for: $needle"
    sed 's/^/        /' "$TMP/out"; fail=$((fail+1))
  fi
}

echo "── clean tree ───────────────────────────────────────────"
seed
expect 0 "0 finding(s)." "a clean tree passes"

echo "── shape 1: a skip that is structurally invisible ───────"
seed
printf '[ -f optional ] || { echo "optional input absent — skipping"; exit 0; }\n' \
  >> "$TMP/t/gates/check-wired.sh"
expect 1 "NEW skip" "an unregistered skip is refused"
printf 'skip :: gates/check-wired.sh::- :: 1 :: the optional input\n' \
  >> "$TMP/t/gate-integrity-register.txt"
expect 0 "0 finding(s)." "the same skip, named and counted, passes"
printf '[ -f other ] || { echo "skipping the other one too"; exit 0; }\n' \
  >> "$TMP/t/gates/check-wired.sh"
expect 1 "WORSE skip" "a SECOND skip at a registered key is refused"

echo "── shape 2: a tautological exit ─────────────────────────"
seed
cat > "$TMP/t/gates/check-taut.py" <<'G'
import sys
ok = False
sys.exit(0 if ok else 0)
G
printf '\tpython3 gates/check-taut.py\n' >> "$TMP/t/Makefile"
expect 1 "R2 TAUTOLOGY" "a checker whose exit conditional is 0 either way is refused"
seed
printf 'echo hello\nexit 0\n' > "$TMP/t/gates/check-nofail.sh"
printf '\tbash gates/check-nofail.sh\n' >> "$TMP/t/Makefile"
expect 1 "R2 TAUTOLOGY" "a checker with no reachable non-zero exit is refused"

echo "── shape 3: an unreferenced gate ────────────────────────"
seed
printf '#!/usr/bin/env bash\n[ -f x ] || exit 1\n' > "$TMP/t/gates/check-orphan.sh"
expect 1 "R3 UNREFERENCED" "a gate no recipe and no invoked script calls is refused"
printf 'unwired :: gates/check-orphan.sh :: 0 :: owed a target\n' \
  >> "$TMP/t/gate-integrity-register.txt"
expect 0 "UNWIRED DEBT" "registered as debt it passes, and SAYS it did not run"
printf '\tbash gates/check-orphan.sh\n' >> "$TMP/t/Makefile"
expect 1 "REPAIRED unwired" "once wired, the stale debt entry is refused"

echo "── shape 3b: a MENTION is not an invocation ─────────────"
seed
printf '#!/usr/bin/env bash\n[ -f x ] || exit 1\n' > "$TMP/t/gates/check-orphan.sh"
printf '\t@echo "see gates/check-orphan.sh for what this does not do"\n' >> "$TMP/t/Makefile"
expect 1 "R3 UNREFERENCED" "a recipe that PRINTS the path does not wire it"

echo "── shape 4: a notApplicable that swallows an obligation ─"
seed
cat > "$TMP/t/conformance-reports/surface.yaml" <<'G'
profiles:
- name: trigger-consumer
  core:
    result: notApplicable
    statistics:
      pass: 0
      fail: 0
      skip: 0
      notApplicable: 3
G
expect 1 "NEW inert" "an unregistered notApplicable verdict is refused"

echo "── evidence: a capture is read, never judged or run ─────"
seed
# The capture directory is held in a variable: a literal path under it, quoted, reads as an
# invocation to the gate this tests -- which is the EVIDENCE INVOKED rule working.
CAP="$TMP/t/captures/2026-01-01-session/tools"
mkdir -p "$CAP"
printf 'echo hello\nexit 0\n' > "$CAP/check-arc.sh"
printf '[ -f x ] || { echo "input absent — skipping"; exit 0; }\n' > "$CAP/proof.sh"
expect 0 "0 finding(s)." "an unwired tautology and a skip inside a capture are not findings"
printf '\tbash captures/2026-01-01-session/tools/check-arc.sh\n' >> "$TMP/t/Makefile"
expect 1 "EVIDENCE INVOKED" "a recipe that RUNS a capture is refused, so the prune hides no gate"
seed
mkdir -p "$TMP/t/captures/2026-01-01-session"
printf 'echo hello\nexit 0\n' > "$TMP/t/gates/check-nofail.sh"
printf '\tbash gates/check-nofail.sh\n' >> "$TMP/t/Makefile"
expect 1 "R2 TAUTOLOGY" "a live gate beside a captures directory is still judged"

echo "── evidence: a session's track-reports/<id>-evidence directory ──"
seed
# Two sessions snapshot the same script. Walked as live, the pair shares one register key and
# the run is exit 2 before any rule is evaluated; pruned as evidence, the tree stays clean.
for s in A B; do
  EV="$TMP/t/track-reports/$s-evidence/pre-edit/examples/demo"
  mkdir -p "$EV"
  printf 'echo hello\nexit 0\n' > "$EV/run.sh"
done
expect 0 "0 finding(s)." "two sessions' snapshots of one script are evidence, not a key collision"
printf '\tbash track-reports/A-evidence/pre-edit/examples/demo/run.sh\n' >> "$TMP/t/Makefile"
expect 1 "EVIDENCE INVOKED" "a recipe that RUNS a session's evidence copy is refused"
seed
mkdir -p "$TMP/t/live-evidence/gates"
printf 'echo hello\nexit 0\n' > "$TMP/t/live-evidence/gates/check-nofail.sh"
printf '\tbash live-evidence/gates/check-nofail.sh\n' >> "$TMP/t/Makefile"
expect 1 "R2 TAUTOLOGY" "a -evidence directory outside track-reports is still walked"

echo "── a documented target whose recipe runs nothing ───────"
seed
printf 'lint-planted: ##! a gate whose recipe was commented out\n#\tbash gates/check-wired.sh\n' >> "$TMP/t/Makefile"
expect 1 "EMPTY RECIPE" "a documented target with every recipe line commented out is refused"
seed
printf 'lint-planted: ##! nothing at all\n\nother:\n\tbash gates/check-wired.sh\n' >> "$TMP/t/Makefile"
expect 1 "EMPTY RECIPE" "a documented target with no recipe at all is refused"
seed
printf 'all-planted: check ##! an aggregate runs its prerequisites\n' >> "$TMP/t/Makefile"
expect 0 "0 finding(s)." "a documented aggregate with prerequisites passes"
seed
printf 'lint-planted: ##! a recipe after comments\n\t# why\n\n\tbash gates/check-wired.sh\n' >> "$TMP/t/Makefile"
expect 0 "0 finding(s)." "a recipe with comment and blank lines before its command passes"

echo "── the register may not rot ─────────────────────────────"
seed
printf 'skip :: gates/check-wired.sh::- :: 1 :: a skip that is not there\n' \
  >> "$TMP/t/gate-integrity-register.txt"
expect 1 "REPAIRED skip" "a register entry whose site is gone is refused"
seed
printf 'skip :: gates/check-wired.sh::- :: 1\n' >> "$TMP/t/gate-integrity-register.txt"
expect 2 "malformed" "an entry with no reason is refused, and the run is could-not-run"
seed
rm -f "$TMP/t/gate-integrity-register.txt"
expect 2 "has not passed" "no register at all is exit 2, never a silent 0"

echo "── a register beside a subtree ──────────────────────────"
# A subtree not in every checkout carries its own entries. The planted skip's message is built
# from a variable: a literal here would be one more skip site in THIS file, which the main
# register counts.
seed
W=skipping
mkdir -p "$TMP/t/sub/tools"
printf '#!/usr/bin/env bash\n[ -f optional ] || { echo "optional input absent — %s"; exit 0; }\n' "$W" \
  > "$TMP/t/sub/tools/run.sh"
expect 1 "NEW skip" "a subtree's skip with no entry in any register is refused"
printf 'skip :: tools/run.sh::- :: 1 :: the optional input\n' > "$TMP/t/sub/gate-integrity-register.txt"
expect 0 "0 finding(s)." "an entry in a register beside the subtree is loaded: its site is not NEW"
printf 'skip :: tools/run.sh::- :: 1 :: the same site, named again\n' \
  >> "$TMP/t/gate-integrity-register.txt"
expect 2 "ambiguous" "an entry named in the main register AND in one beside a subtree is exit 2"
seed
printf 'skip :: gates/check-wired.sh::- :: 1 :: once\nskip :: gates/check-wired.sh::- :: 1 :: twice\n' \
  >> "$TMP/t/gate-integrity-register.txt"
expect 2 "ambiguous" "an entry named twice in one register is exit 2, never last-one-wins"
seed
mkdir -p "$TMP/t/captures"
printf 'skip :: tools/run.sh::- :: 1 :: a site this checkout does not carry\n' \
  > "$TMP/t/captures/gate-integrity-register.txt"
expect 0 "0 finding(s)." "a register inside an evidence directory is evidence, not an overlay"

echo "─────────────────────────────────────────────────────────"
echo "$pass passed · $fail failed"
[ "$fail" -eq 0 ] || exit 1
