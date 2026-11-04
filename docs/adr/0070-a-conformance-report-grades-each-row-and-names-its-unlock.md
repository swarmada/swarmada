# ADR-0070: A conformance report grades each row, names what would unlock it, and does not pass on a skip

- **Status:** accepted
- **Date:** 2026-09-19
- **Deciders:** Maintainers, Test Engineer, API Designer
- **Related:** ADR-0048 (a verdict grades obligations, not observations), which this ADR
  extends and does not reverse; ADR-0007 (conformance is self-certified); ADR-0032
  (version-bound conformance); ADR-0067 (a measurement gate refuses a run rather than
  widening the floor); `conformance/PROFILES.md`, `conformance/PLANT.md`,
  `conformance/unlocks.yaml`, `adapters/CONFORMANCE.md`

## Context

A `ConformanceReport` carried one boolean, `conformant`, computed as *no failed Core
check in a required, applicable profile*. A Core check that was skipped did not fail, so
it did not reach the boolean. The reference adapter's stored report read:

    mode: simulated
    conformant: true
    handshake   core   notVerified   C1.3 deployment-property, C1.4 not-observable-on-wire
    telemetry   core   notVerified   C6.3 not-observable-on-wire
    dispatch    core   notVerified   C3.7 needs-target-restart

and `C3.7`'s own note says a passing report "attests NOTHING about durability across a
restart". A reader of `conformant: true` learned that the adapter spoke the protocol to a
simulator and could not tell that result from a hardware qualification, or tell which
obligations had been established and which had never been tried.

`PROFILES.md` already defined the word differently from the code: *"conformant" means
every Core check of every required, applicable profile passed.* A skipped check has not
passed. The code implemented the weaker reading, and nothing compared the two.

ADR-0048 settled a narrower question: a check that was never an obligation of the
implementation (a binding it did not declare, a legal alternative path it took) is
counted `notApplicable` and does not hold a level at `notVerified`. That rule is kept.
It says what a skip does to a *level*. It does not say what a report should tell a
reader about each row, or what a skip does to the *verdict*.

## Decision

**A report grades every capability row with exactly one of six grades, records the
observation the grade rests on, names for every unmeasured row what would make it
measurable, and reports `pass` only when every required Core row was measured and
passed.**

### 1 · The subject

Every report carries a `subject` block naming what was graded:

```yaml
subject:
  implementation: {project: …, version: …, contractVersion: 1.2.0}
  fidelity: simulated          # simulated | hardware
  plant:                       # {supplied: false} when no plant.yaml was given
    supplied: true
    path: plant.yaml
    digest: sha256:…           # of the plant.yaml the run was graded against
    kind: hardware
    robotModel: {value: …, source: operator}
    firmware:   {value: unknown, reason: not supplied (--firmware)}
```

`fidelity` is `hardware` only when the run is given `--mode physical` **and** a
`plant.yaml` whose `plant.kind` an operator answered `hardware` (`PLANT.md`: no reading
proves a plant is physical). Every other combination is `simulated`, and a contradiction
between the two (`--mode physical` with a simulated or absent plant, or a hardware plant
with `--mode simulated`) is **refused** with exit status 2, not resolved: either
resolution would be a guess about which of the two the operator got wrong (ADR-0067's
rule, applied to fidelity). The model refuses a hardware report without the plant as
well, so no caller can construct one. `robotModel` and `firmware` have no field in
`plant.yaml`; they are taken from the operator and say so, or read `unknown` with the
reason.

`simulated` means *no physical plant stood under the measurement*: a simulator, an
in-tree fixture, or (for `ControlPlane`) a static read of shipped artefacts. `mode`
keeps saying which of those it was.

### 2 · Six grades, one per row

| grade | meaning | unlock |
| :-- | :-- | :-- |
| `measured-hardware` | ran and passed, against an operator-declared hardware plant | — |
| `measured-simulation` | ran and passed, with no physical plant under it | — |
| `failed` | ran and did not pass, at any level | — (the row was measured) |
| `not-supported-by-platform` | observed or declared absent: declined, not declared, another permitted route, or the surface is not implemented | **required** |
| `not-testable-here` | the obligation stands and this setting cannot observe it | **required** |
| `not-tested-suite-gap` | the suite's own limit | **required**, naming the harness change |

Context rows (`C0.2`–`C0.4`) are statements about the run, not capabilities, and carry
no grade.

### 3 · Every SkipCause maps to exactly one grade

The grade of a skipped row is a function of its cause and of nothing else. There is no
per-check override. The table below is `CAUSE_GRADE` in `conformance/model.py`, and a
test fails if the two differ or if any cause is missing or listed twice. The last column
is ADR-0048's `NOT_APPLICABLE_CAUSES`, unchanged.

| cause | grade | never an obligation |
| :-- | :-- | :-- |
| `not-observable-on-wire` | `not-testable-here` | |
| `surface-not-implemented` | `not-supported-by-platform` | |
| `optional-declined` | `not-supported-by-platform` | |
| `optional-not-exhibited` | `not-supported-by-platform` | |
| `binding-not-declared` | `not-supported-by-platform` | yes |
| `contract-not-declared` | `not-supported-by-platform` | yes |
| `needs-resident-target` | `not-tested-suite-gap` | |
| `needs-live-cluster` | `not-tested-suite-gap` | |
| `target-did-not-reach-state` | `not-testable-here` | |
| `legal-alternative-path` | `not-supported-by-platform` | yes |
| `deployment-property` | `not-tested-suite-gap` | |
| `needs-extended-run` | `not-tested-suite-gap` | |
| `needs-target-restart` | `not-tested-suite-gap` | |
| `no-telemetry-in-run` | `not-testable-here` | |
| `requires-successful-reregistration` | `not-tested-suite-gap` | |
| `all-core-subjects-honoured` | `not-testable-here` | |
| `adapter-declares-no-operating-subject` | `not-supported-by-platform` | |
| `adapter-declares-no-sensor-stream` | `not-supported-by-platform` | yes |
| `adapter-declares-no-action` | `not-supported-by-platform` | |
| `adapter-declares-no-required-param` | `not-supported-by-platform` | |
| `adapter-declares-no-enum-param` | `not-supported-by-platform` | yes |
| `adapter-declares-no-custom-verb` | `not-supported-by-platform` | yes |
| `contract-predates-obligation` | `not-supported-by-platform` | yes |
| `no-motion-witness` | `not-testable-here` | |
| `no-independent-motion-channel` | `not-supported-by-platform` | |

The FleetAdapter harness's own spellings `adapter-did-not-reach-state` and
`needs-adapter-restart` resolve through the existing alias table to
`target-did-not-reach-state` and `needs-target-restart`.

**Causes that fit two grades.** Mapping a cause once is a requirement. Where the fit is
not clean, the choice is recorded here and the cause is marked for splitting rather than
settled check by check:

- `target-did-not-reach-state` names two facts: the target did not do something the
  check needed (`not-testable-here`), and the suite's own probe made the state
  unreachable — `C10.1`, where the harness sends an artifact it built to be refused
  (`not-tested-suite-gap`). Mapped to `not-testable-here`; `C10.1`'s unlock names the
  harness change.
- `deployment-property` is defined as "TLS material, process supervision". The TLS half
  is something a harness can reproduce (`not-tested-suite-gap`). Process supervision
  belongs to whoever deploys the target (`not-testable-here`). Mapped to
  `not-tested-suite-gap`, because every row that records it today is on the harness's
  side of that line.
- `needs-live-cluster` is both a setting the run lacks and a mode the ControlPlane
  driver does not have. With no live mode in the suite, no setting can unlock it without
  a harness change, so it is mapped to `not-tested-suite-gap`.
- `legal-alternative-path` fits none of the six exactly. The requirement was *satisfied*,
  by another permitted route. What the row grades — the two-phase `STOPPING` shape in
  `C5.6` — is observably absent, so it is mapped to `not-supported-by-platform`. It stays
  outside the verdict under ADR-0048 rule B, so the grade is a disclosure and not a
  penalty.

Four rows recorded a cause their own note contradicted. Under a verdict boolean the
difference was invisible. Under a published grade it is a false statement, so their
causes were corrected: `CP3.1` and `CP7.3` from `surface-not-implemented` to
`needs-live-cluster` (the surface exists; the static profiler cannot watch it act), `CE7.3`
from `not-observable-on-wire` to `deployment-property` (a hand-off is observable; the
fixture has one replica), and `CE7.4` from `target-did-not-reach-state` to
`needs-extended-run` (the harness never induces the endpoint loss). No verdict changes
because of these corrections.

### 4 · Evidence

Every graded row carries `evidence.observed`: the observation the grade rests on, as the
run recorded it. That is the value, field, count or duration in the check's detail with
the cause spelling removed, plus any number the harness measured and attributed to that
check (`evidence.measurements`). A row whose run recorded an outcome and no observation
says so in `evidence.note` rather than being given one. The FleetAdapter harness
recorded nothing on twenty passing rows, among them the fencing, lease and estop rows.
It now records what it saw on each, and a test fails if a measured FleetAdapter row
carries no observation.

### 5 · Unlock

Every row graded `not-supported-by-platform`, `not-testable-here` or
`not-tested-suite-gap` carries an `unlock`, resolved from `conformance/unlocks.yaml`: per
check first (an exact id, then the longest `PREFIX*`), then per cause. A cause gets a
default only when its unlock is the same whichever check records it. The report builder
**refuses** to write a report containing an unmeasured row it cannot resolve
(`MissingUnlock`), for the reason an unenumerated skip cause is refused. An unlock says
what would make the row testable. It does not promise the row would pass, and on a
`not-supported-by-platform` row it is not a recommendation to add the feature.

### 6 · The verdict

```yaml
verdict: not-established        # pass | fail | not-established
verdictBlockedBy:
  - {checkId: C1.3, profile: handshake, grade: not-tested-suite-gap}
  - …
conformant: false
```

For every Core check of every required, applicable profile, including any Core check
the run recorded no row for:

- any row `failed` → `fail`;
- else any row not measured, whose cause is not in `NOT_APPLICABLE_CAUSES` →
  `not-established`;
- else `pass`.

`conformant` stays as the Core statement and is `true` only when `verdict` is `pass`.
`verdictBlockedBy` lists every row that prevents `pass`. Extended rows and profiles that
are not required at the tier do not enter the verdict, as before. The human summary
prints the sheet first and the verdict under it. `python -m conformance` exits `0`
(pass), `1` (any fail), `3` (nothing failed, something not established) or `2` (refused
or could not start).

## Alternatives considered

- **Keep the boolean alone.** Rejected. It is the defect: one bit cannot distinguish
  *established*, *failed* and *never tried*, and it said `true` over four unestablished
  Core obligations on the reference report.
- **Count a skipped Core row as `notApplicable`.** Rejected, and ADR-0048 already
  rejected it for the causes it did not name: an unestablished obligation is not an
  absent one. Counting skips away would make `pass` cheaper to earn by testing less,
  which is the direction a self-certified report (ADR-0007) must never lean.
- **Let a check override the grade its cause implies.** Rejected. It would let the grade
  of any row be argued case by case, and the ADR table would stop being the rule. A
  cause that does not fit is split instead.
- **A seventh grade for "satisfied by another permitted route".** Considered for
  `legal-alternative-path` and not taken: the six grades were set by the ruling this
  ADR implements, and the row stays outside the verdict either way. The seam is noted
  above.
- **Grade hardware from `--mode physical` alone.** Rejected. A flag is a claim, not
  evidence. The plant declaration is the evidence, and its digest is what a reader can
  check.

## Consequences

**What a reader gains.** Each row states what was established, at which fidelity, from
which observation, and, when it was not established, what would establish it. A
simulated result can no longer be read as a hardware one: the grade, the subject block
and the verdict all say `simulated`, and the builder refuses the alternative without a
plant.

**Verdicts that change.** Regenerated under this rule, four of the six stored reports
change from `conformant: true` to `verdict: not-established`:

| report | blocked by |
| :-- | :-- |
| `fleet-adapter` | `C1.3` (suite gap: the harness serves plaintext), `C1.4` (not testable here: no health state on the wire), `C6.3` (not testable here: the obligation is on the control plane's write path), `C3.7` (suite gap: no restart hook) |
| `control-plane` | `CP2.1`–`CP2.5`, `CP4.1` (not supported: surfaces not in the tree), `CP3.1`, `CP7.3` (suite gap: no live mode) |
| `compute-endpoint-plate-reader-grpc` | `CE8.1`, `CE8.3` (not testable here: no build identity on the reply) |
| `compute-endpoint-plate-watcher-grpc` | `CE2.4`, `CE7.3`, `CE7.4`, `CE7.6`, `CE8.1`, `CE8.3` |

The two HTTP and MCP endpoint reports stay `pass`.

**No FleetAdapter report can reach `pass` yet.** `C1.3` and `C3.7` need harness work (TLS
from a test CA, a restart hook). `C1.4` has no wire observable. `C6.3` grades a
property of the control plane's write path and not of the adapter. Until those four are
resolved — by harness change, by an external witness, or by moving `C6.3` to the surface
that owns it — every adapter's report reads `not-established`, whatever the adapter
does. That is the honest reading of the suite as it stands, and it is the list of work
that would change it. It is not a defect in any adapter.

**The admission boolean is a different one, and is unchanged here.** The FleetAdapter
controller reads `conformant` from the harness's own JSON report
(`adapters/conformance/report.py`, `python -m adapters.conformance --json`), whose
meaning is "no failed MUST or MUST NOT among the checks that ran". That report has no
Core partition, so it cannot compute this verdict, and this ADR does not change it. The
verdict above is the profiled report's. Given the previous paragraph, feeding it to
admission today would refuse every adapter. Whether admission should read it, and when,
is a separate decision for the owners of the control plane and the API.

**The specification text lags this decision on one field.** RFC-0001's conformance
chapter defines the report field as "`conformant` — true when no Core check fails in any
required, applicable profile", which is the reading this ADR retires. The same chapter
already requires that a skipped Core check "does not establish the obligation it covers",
and that no report record the profile as passing, so the change is to one field
definition, not to the chapter's intent. The chapter is `stage: alpha`, so under ADR-0004
the correction is made in place in the chapter, with a re-review of that section. Until
it is made, the chapter and the suite disagree about this one field.

**The suite revision.** The verdict change moves what an implementation must show to
read `conformant: true`, which under `PROFILES.md` is a revision that invalidates earlier
results. The revision in `profiles.yaml` moves together with the registry rows that cite
it, when those rows are re-graded under this sheet. It is not moved alone.

**New obligations.** `unlocks.yaml` must cover every unmeasured row the suite can
produce. The builder enforces that for every row a run actually produces, and does not
enforce it for rows no run has produced. The ADR table and `CAUSE_GRADE` must move
together. A new `SkipCause` needs a grade before it can be recorded.

**What a reader loses.** The single word. A dashboard that read `conformant: true` for
the reference adapter now reads `false`, and has to read `verdict` and `verdictBlockedBy`
to learn that nothing failed. That is the point.
