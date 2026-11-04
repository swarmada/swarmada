# Swarmada Conformance Profiles

The profile registry for the whole standard: three **surfaces**, the **profiles**
each one partitions into, the **Core** and **Extended** support levels, the
**capability tiers** that select what a surface is obliged to satisfy, and the
machine-readable **`ConformanceReport`** that carries a result.

It changes no check and relaxes no obligation. Every check named here is defined
in a catalog document, and where a catalog and this document disagree, **the
catalog governs**; where a catalog and the specification disagree, **the
specification governs** (RFC-0001, *Conformance Profiles and Support Levels*,
"Precedence: the specification overrules the tests"). This document and
[`profiles.yaml`](profiles.yaml) are test artefacts. They can be wrong, and the
`*0.5` self-check is what catches them drifting.

## Surfaces

A **surface** is a party that can be measured. The three are implemented by
different people, ship on different schedules, and produce **separate reports** —
a single report describing more than one of them would attest to work its
implementation metadata does not name.

| Surface | Implemented by | Catalog | Protocol |
| :---- | :---- | :---- | :---- |
| **`FleetAdapter`** | a fleet adapter vendor | [`adapters/CONFORMANCE.md`](../adapters/CONFORMANCE.md) `C0`–`C19`, plus [`REACTIVE-BLOCK.md`](REACTIVE-BLOCK.md) `RB*` | `fleet_adapter.v1` |
| **`ComputeEndpoint`** | whoever writes the executor | [`COMPUTE-ENDPOINT.md`](COMPUTE-ENDPOINT.md) `CE*` | `compute.v1` |
| **`ControlPlane`** | the deployment | [`CONTROL-PLANE.md`](CONTROL-PLANE.md) `CP*` | `swarmada.io/v1` |

The `FleetAdapter` partition of `C0`–`C17` and `C19` is fixed by RFC-0001's
conformance chapter and is restated here in machine-readable form only. What this document
adds is the two Draft 4 surfaces and four new profiles of the adapter surface —
`reactive-grammar`, `reactive`, `zone-restrictions` and `sensor-streams` — and
the report shape that spans all three surfaces. `C18` post-dates that chapter and is
partitioned here rather than there. The chapter partitions `C5.7` (an early `STOPPED`
fails against a motion witness) in `safety`, `C12.2` (the snapshot pushed at registration
carries the `scan` reply's catalog) in `discovery`, and `C19.1`–`C19.4` (an assignment the
catalog does not declare is declined) in `dispatch`, all Core. Where the chapter and this
document disagree about `C0`–`C17` or `C19`, the chapter governs. `C6.3` (no `Robot` status
write per telemetry tick) belongs to no Fleet Adapter profile, and the chapter says so: it
is graded on the `ControlPlane` surface as `CP8.1`, in a `status-discipline` profile,
because the obligation is on the party that writes status.

**The `safety` profile has a platform precondition** (ADR-0071): it is claimable only on a
platform with at least one motion channel independent of the command path that can confirm
rest. A run profiled with a `plant.yaml` whose odometry is all `command-derived` grades
`C5.3` `not-supported-by-platform` (`no-independent-motion-channel`), and that cause keeps
the verdict off `pass`. RFC-0003's `E0`–`E4` are **not** a profile: `profiles.yaml` holds no
RFC-0003 identifier, and the suite offers no profile the identifiers could be
graded under.

**`CE6.1`–`CE6.5` and `CP2.3`–`CP2.5` are registered `excluded` and graded nowhere.** They
are the endpoint's and the control plane's halves of the event rules for a source that is
not a fleet adapter, and RFC-0001 states none of them at v0.4: its `ComputeEndpoint` chapter
calls `spec.emitsEvents` declarative only. A catalog row grades no RFC-0001 obligation the
chapter does not state (*Precedence*), so since suite revision `2026.10.13` each set is
listed under its surface's `excluded:` in `profiles.yaml`. The suite still records the rows
with their grades, and no profile result and no verdict counts them until RFC-0001 states
the rules. The `events` profile keeps `CE6.6` alone, as Extended, and the `endpoint-dispatch`
profile keeps `CP2.1` and `CP2.2` as Core.

**New in Draft 4, by surface**

| Surface | Profiles added | Level |
| :---- | :---- | :---- |
| `FleetAdapter` | `reactive-grammar` | Core, always applicable |
| `FleetAdapter` | `reactive` | Extended, when a grammar version is declared |
| `FleetAdapter` | `zone-restrictions` | Extended, always applicable |
| `FleetAdapter` | `sensor-streams` | Extended, always applicable |
| `ComputeEndpoint` | `attestation`, `invocation`, `envelope`, `provenance` | Core |
| `ComputeEndpoint` | `events` | Extended (`CE6.6`), when `spec.emitsEvents` is non-empty; `CE6.1`–`CE6.5` are registered `excluded` |
| `ComputeEndpoint` | `binding-http`, `binding-grpc`, `binding-mcp` | Core, for the declared binding only |
| `ComputeEndpoint` | `service-contract` | Core, when `contract: Service` |
| `ControlPlane` | `attestation`, `containment`, `endpoint-dispatch`, `result-is-data`, `image-provenance`, `zone-restrictions`, `status-discipline` | Core (`endpoint-dispatch` is `CP2.1`–`CP2.2`; `CP2.3`–`CP2.5` are registered `excluded`) |
| `ControlPlane` | `cluster-compute` | **Extended, never Core** |
| `ControlPlane` | `reactive-dispatch` | Extended |
| `ControlPlane` | `trigger-consumer` | Reserved by RFC-0003, a draft for review that is not part of the draft 4 contract: absent from the published registry, never graded at v0.4 |

## Support levels

**Core** is mandatory for every implementation to which the profile applies.
Core MUST NOT be disabled, negotiated, or declined. There is no partial Core:
"conformant" means every Core check of every required, applicable profile passed —
was measured, and passed. A skipped Core check has not passed (see *The capability
sheet* below).

**Extended** is optional. An implementation MAY implement an Extended feature and
MAY decline it — but an implemented feature is held to the **full** obligation
stated for it. "Optional" governs whether the operation exists, never how
correctly it behaves once it does.

The `supportedFeatures` and `unsupportedFeatures` lists are **derived by the suite
from check outcomes**, not declared by the implementation. A feature is supported
when at least one of its checks passed, none failed and none was declined
(`optional-declined`), and unsupported otherwise (`conformance/registry.py`); the two lists together enumerate every Extended
feature the profile defines, so a feature in neither is a malformed report. There
is no declaration input anywhere in the suite, and an implementation cannot claim
a feature it did not demonstrate.

That is a property worth stating rather than an accident to be corrected. This
document previously placed the declaration on the implementation as a normative
requirement — describing an input nothing reads and no implementer could supply.
Bringing the code into line with that sentence, which is the natural next change
for anyone reading it as normative, would introduce the first place in this
program where **saying less produces a better-looking result**:
under ADR-0048 an Extended level never reports `fail`, so an implementation that
declared nothing would have every Extended check counted `notApplicable` rather
than disclosed as a shortfall. Conformance is self-certified (ADR-0007) — the
implementer runs the suite and publishes the report, with no gate and no reviewer
— and the whole design rests on a report being hard to flatter.

A declaration input MAY be added, and if it is, it MUST be falsifiable: the suite
MUST keep computing the derived lists and MUST record a disagreement between the
declared and the derived list as a finding, in the shape `*0.1` already uses for
skip causes. A declared feature whose checks did not pass is a contradiction the
report can state. Without that cross-check, a declaration is exactly the
unfalsifiable prose this document refuses everywhere else, and MUST NOT be
introduced.

**A skip is neither.** A check not exercised records `skip` with a cause from the
enumerated `SkipCause` set — never free text, because a prose justification is
not falsifiable and drifts exactly like any other unverified claim. **This is
enforced, not only required:** a detail string that resolves to no enumerated
cause raises `UnenumeratedSkipCause` and the run fails. It previously fell
through to the literal string `"unenumerated"` in the report, and that silence is
how two checks (`C5.6`, `C3.7`) sat misspelled across the harness and profiler
vocabularies without anyone noticing. A Core check
recorded `skip` does not establish the obligation it covers, the profile
result is `notVerified`, never `pass`, and the report's verdict is
`not-established`, never `pass`.

**A check that could not apply is a third thing again.** Three of the enumerated
causes — `binding-not-declared`, `contract-not-declared` and
`legal-alternative-path` — mean the check was never an obligation of this
implementation, not that it went unestablished. A `Grpc`-only check against an
`Http` endpoint is in that class, and so is the resident-executor sequencing rule
against a request/response one. So is a check made unreachable because the
implementation satisfied the requirement by another route the specification
permits: `C5.6` grades the two-phase `STOPPING`→terminal estop shape, and an
adapter that acks terminal directly — *"which is equally conformant"* — was never
obliged to satisfy it (ADR-0048 rule B). Those are counted separately, as
`notApplicable` in a profile's statistics, and they do not hold a profile at
`notVerified`.

The distinction is not cosmetic. Several of these checks come in **mutually
exclusive pairs** — `CE6.2` binds a resident executor and `CE6.3` a
request/response one, and no endpoint is both. The `envelope` profile's Core holds
`CE2.2` and `CE2.7`, which a resident endpoint cannot reach. Counting the unreachable
half as a skip would leave a required profile permanently `notVerified` for every
endpoint of the other kind, which is a report that has stopped distinguishing
anything. (`CE6.2` and `CE6.3` are registered `excluded` since suite revision
`2026.10.13`, above; the rule holds for the rows the suite still grades.)

### Three profiles that are Extended for stated reasons

**`cluster-compute` is Extended and can never be Core.** `provision: workload`
has the controller create a `Job` or `Deployment` per run, which requires RBAC
the manager does not hold — and pod-create is an escalation path: whoever creates
a pod may set `serviceAccountName` and read that account's tokens. Making it Core
would make every conforming deployment hold that authority. **A deployment that
declines workload-create RBAC passes Core in full**, and
`CP5.5` verifies the decline rather than inferring it from an absence.

**`reactive` is Extended at every capability tier, and holding it never raises
the tier.** None of the four effects commands motion, so a Read Only adapter may
host reactive blocks and remain Read Only. Its sibling `reactive-grammar` is
Core and always applies, because its subject is the grammar rather than the
adapter — see [`REACTIVE-BLOCK.md`](REACTIVE-BLOCK.md) for why the split is
load-bearing rather than tidy.

**`sensor-streams` is Extended, and the catalog says so in as many words.** An
adapter declining `activate_stream` declines the duty cycle for its robots, which
is conformant under `C7.1` and means nothing activates — so nothing can be left
on. It is **always applicable** for the reason `zone-restrictions` was corrected
to be: `C18.1` grades the shape of whatever was declared and *declaring nothing
passes*, so a conditional would report `notApplicable` over a row that had
actually run. `C18.2`–`C18.5` carry their own decline arm (`activate_stream`)
instead.

**It gates no capability tier**, and a Read Only adapter may serve sensor streams
and stay Read Only. Switching a sensor on commands no **motion** — and observation
is what a Read Only adapter is for. RFC-0001's tier chapter is the complete
statement of what a tier requires and does not name this profile.

**The ordinary path and the fail-safe are separate features, deliberately.**
`sensor-streams.duty-cycle` (`C18.5`, stop on `DeactivateStream`) and
`sensor-streams.lease-failsafe` (`C18.3`, stop when the control plane goes silent)
grade different things, and the catalog draws the line itself: *"the lease is the
fail-safe, not the ordinary path."* An adapter can stop cleanly on command and
still never stop when abandoned — leaving a camera running for a whole lease every
time the control plane goes quiet, which is the hazard, the cost and the privacy
exposure the family exists for. One feature spanning both would let a green
`C18.5` report the fail-safe as held.

> **Direction note.** This family's safe state is the **inverse** of the
> emergency-stop lease's. A stop must always be honoured, so nothing may gate it;
> a stream's failure mode is *left on*, so its lease **stops** it. Each fails
> toward its own safe state and those states point opposite ways — which is why
> `C18.3` is not a restatement of `C4.1` with a different noun.

### The profile that stopped needing this section

**`zone-restrictions` on the `FleetAdapter` surface is ALWAYS applicable.** It used to
apply only when the adapter declared at least one honoured operating subject, and this
section existed to explain why that was compatible with its decline checks being
recorded in every report — the only place in this registry needing such an
explanation, which was the signal that the applicability rule was wrong rather than
subtle.

It was wrong in a specific and measurable way. The specification's own load-bearing
case for the profile is *"a run that goes RED against an adapter that accepts and
ignores it"*, and an adapter that accepts and ignores an operating rule is one that
declares **nothing**. So the profile reported `notApplicable` for exactly the adapters
whose red run it demanded:

    zone-restrictions    —         —     notApplicable      <- before
    zone-restrictions    extended  no    notVerified  3/0/1 <- after

`C17.1`'s obligation — decline an assignment carrying an operating constraint you
cannot honour, naming the subject — has no antecedent about *declaring*: an adapter
that honours nothing cannot honour anything it is sent, so every operating rule that
reaches it is one it must refuse. The applicability rule now says the same thing the
obligation does, and no check in the profile lost anything: `C17.1` and `C17.2` have no
declaring antecedent, `C17.3` grades the **shape** of whatever was declared and
declaring none **passes**, and `C17.4` needs a declaration to exist and carries its own
enumerated skip cause (`adapter-declares-no-operating-subject`) for the absence.

**A check whose red run lands in a profile reported `notApplicable` is a check that
cannot fail** — or rather, could not be *read* as failing, since the row did fail the
report. That is the reason this was worth correcting rather than documenting. Corrected
2026-09-14. See `C17` in
[`adapters/CONFORMANCE.md`](../adapters/CONFORMANCE.md).

**Its two same-named halves establish different things.** `zone-restrictions` on
the `FleetAdapter` surface is **live**: the adapter under test is a running
process the harness dials into, and a green `C17.1` means it declined a real
assignment on the wire. `zone-restrictions` on the `ControlPlane` surface
(`CP7.1`, `CP7.2`) is **static**: it is read off the shipped CRD schemas, and a
deployment that carries the surface and implements no filter passes it. Same name,
same ADR, different strength of evidence — and a reader comparing two green lines
should know which is which.

### One thing that is Core and fails loudly

**A mutable image reference fails Core.** `CP4.2` is a Core check, and a run that
resolved its executor through a mutable tag fails it. No configuration rescues
it: a deployment enabling `spec.compute.allowMutableImageRefs` is not
Core-conformant, whatever else it passes, and its report records that rather than
omitting it. The lab-iteration need is met by recording the resolved digest on
every run — the code still arrives as a signed image, and every run stays
attributable to specific bytes.

## Capability tiers

A **capability tier** states how much authority an implementation offers over the
fleet, and thereby which profiles it is obliged to satisfy.

| Tier | Authority | Surfaces it applies to |
| :---- | :---- | :---- |
| **Read Only** | Observation without command. | `FleetAdapter`; **fixed** for `ComputeEndpoint` |
| **Traffic Light** | Observation, plus pause and resume of named capabilities. | `FleetAdapter` |
| **Full Control** | Observation, pause/resume, plus assignment and execution under lease. | `FleetAdapter` |
| **Not Applicable** | The surface offers no fleet authority to grade. | `ControlPlane` |

The tier axis is defined on the `FleetAdapter` surface, where RFC-0001's
conformance chapter states which profiles each tier requires and why the safety
floor does not move with the tier. The other two surfaces carry a **fixed** value,
stated rather than omitted — a missing field cannot be distinguished from an
incomplete report:

- **`ComputeEndpoint` is Read Only, always.** An endpoint holds no authority over
  the fleet **by construction**: its result is data and is never a command
  and `CE2.5` is the Core check that holds the
  line. A `ComputeEndpoint` report declaring any other tier is malformed. This is
  the honest use of the vocabulary rather than a forced one — the tier is not
  understated, there is genuinely no authority to grade.
- **`ControlPlane` is Not Applicable.** The tier grades authority *offered*; the
  control plane is the party *holding* it.

> **Prior art (non-normative).** Full Control, Traffic Light and Read Only are
> Open-RMF's integration levels for fleet adapters, adopted as vocabulary. An
> integrator who knows Open-RMF should read the table above without learning a
> second set of words for the same three ideas. RFC-0001's chapter records the
> one deliberate divergence: Swarmada's Read Only removes the obligation to
> accept work and does **not** remove the obligation to stop.

## The public suite stands alone

A stranger cloning `github.com/swarmada/swarmada` runs every check in this
program with nothing else present. **A dependency on a private repository is a
defect, not a convenience**, and it is a defect the suite detects rather than one
review is asked to notice: `*0.3` fails the run when any artefact it read
resolves outside the public tree.

The fixtures are in-tree and dependency-light for the same reason — the HTTP and
MCP executors are Python standard library only, so a clone plus a Python
interpreter is the whole prerequisite.

### Specifications this program asserts nothing about

**MHS appears in no profile.** A profile asserting support for a
specification nobody outside can read is untestable by a stranger, which fails
the standing rule above; and depending on a single-vendor, not-yet-open preview
reproduces the exact condition this project cites as disqualifying elsewhere. It
remains reachable over the `Mcp` binding as deployment configuration, which is
true today and commits nothing.

Enforced, not only stated: `*0.3` fails the run if an excluded name appears in
a profile name, a check id, a check title, or a fixture path. Re-assess when the
specification is published under a neutral foundation and can be read.

## The `ConformanceReport`

A run produces one `ConformanceReport`: a machine-readable document recording who
ran the suite, against what, and with which result per profile and per row. It is
the artefact submitted for publication. The control plane does not read it:
`FleetAdapter.spec.conformanceReport` references the Fleet Adapter harness's own
JSON report, whose `conformant` has a different meaning (see
[`adapters/CONFORMANCE.md`](../adapters/CONFORMANCE.md), *Passing*).

```yaml
apiVersion: conformance.swarmada.io/v1alpha1
kind: ConformanceReport
surface: ComputeEndpoint                 # required; FleetAdapter | ComputeEndpoint | ControlPlane
implementation:
  organization: acme-perception          # required; the entity attesting
  project: plate-reader                  # required; the endpoint, not the model
  version: v1.4.2                        # required; the BUILD version
  url: https://example.invalid/plate-reader
date: "2026-09-02T10:14:07Z"             # required; RFC 3339, UTC
protocolVersion: compute.v1              # required; wire package identity, not a semver
contractVersion: 1.0.0                   # required; the semver the result is earned against
suiteRevision: "2026.09.02"              # required; the registry revision that ran
mode: simulated                          # required; simulated | physical (physical needs a plant.yaml)
capabilityTier: ReadOnly                 # required; fixed for this surface
target:                                  # required for ComputeEndpoint
  binding: Http                          # Grpc | Http | Mcp
  contract: App                          # App | Service
standalone: true                         # required; every artefact read resolved in-tree
subject:                                 # required; what was graded
  implementation: {project: plate-reader, version: v1.4.2, contractVersion: 1.0.0}
  fidelity: simulated                    # simulated | hardware
  plant: {supplied: false}               # the plant.yaml, its digest, robot model, firmware
verdict: pass                            # required; pass | fail | not-established
verdictBlockedBy: []                     # required; every required Core row preventing pass
conformant: true                         # required; true only when verdict is pass
grades: {measured-simulation: 34, failed: 0, not-supported-by-platform: 20,
         not-testable-here: 0, not-tested-suite-gap: 0}
profiles:
  - name: invocation
    required: true
    core:
      result: pass                       # pass | fail | notVerified
      statistics: {pass: 6, fail: 0, skip: 0}
    extended:
      result: pass
      statistics: {pass: 1, fail: 0, skip: 0}
      supportedFeatures: [invocation.replay-marker]
      unsupportedFeatures: []
  - name: binding-grpc
    required: false
    applicable: false                    # the endpoint declares binding: Http
  - name: service-contract
    required: false
    applicable: false                    # the endpoint declares contract: App
checks:
  - checkId: CE1.2
    title: The same reply for a repeated invocation key
    status: pass
    level: MUST
    detail: status, values, artifacts and events identical across two calls with …
    grade: measured-simulation           # exactly one of the six grades
    evidence:                            # what the run saw, not only the verdict
      observed: status, values, artifacts and events identical across two calls with …
  - checkId: CE4.1
    title: Implement ComputeRuntimeService as the server
    status: skip
    level: MUST
    detail: 'binding-not-declared: the endpoint declares binding: Http'
    grade: not-supported-by-platform
    evidence: {cause: binding-not-declared, observed: 'the endpoint declares binding: Http'}
    unlock: a target that declares the binding this row belongs to
```

**Field semantics.** Fields shared with RFC-0001's report shape keep their
meaning there: `implementation`, `date`, `protocolVersion`, `contractVersion`,
`suiteRevision`, `mode`, `profiles[]`, `core.result`, `extended`, `skipped[]`.
`conformant` is narrowed by ADR-0070 (below). Four more are added or narrowed by
this document, and the sheet fields are defined in *The capability sheet*.

- `surface` — REQUIRED. Which party the report attests to. One report names one
  surface; a reader MUST NOT read a `ComputeEndpoint` result as saying anything
  about the adapter or the deployment that talks to it.
- `capabilityTier` — as RFC-0001 defines, extended with `NotApplicable`, and
  **fixed** for two of the three surfaces (above). A value contradicting the
  surface's fixed value makes the report malformed.
- `target` — REQUIRED on a `ComputeEndpoint` report: the declared `binding` and
  `contract`, which select the conditional profiles. `CE0.2` asserts these are
  the ones the harness actually drove.
- `standalone` — REQUIRED. True when every artefact the run read resolved inside
  the public repository. It attests what `*0.3` verified and nothing more: it
  cannot see inside an out-of-tree target an operator points the harness at with
  `--target`, and it is not a claim about that target's own dependencies.

**Three-value `core.result` is load-bearing.** `pass` when every Core check
passed; `fail` when any failed; `notVerified` when none failed but at least one
was skipped. Collapsing `notVerified` into `pass` reports an unestablished
obligation as an established one.

**`extended.result` never takes `fail`** (ADR-0048 rule A). Extended is optional,
so a failed Extended check makes its **feature** unsupported — which
`unsupportedFeatures` already states — and does not make the **level** fail. A
`fail` there would read a declared tier as a mandatory one. It takes `pass`,
`notVerified`, or `notApplicable`. The failure is not hidden: the check keeps
`status: fail` in the `checks` list and its count in the level's `statistics`.
The cost is that an Extended level with a failure and one with a skip both read
`notVerified`, so telling them apart means reading `statistics.fail` or
`unsupportedFeatures` — accepted, because a verdict of `fail` for something no
implementation was obliged to do is the worse error.

**`applicable: false` is not omission.** A conditional profile that did not apply
carries an entry with no `core` or `extended` block. Omitting the entry is NOT
equivalent, because a missing entry cannot be distinguished from an incomplete
report — and the distinction between *never applied* and *applied and unverified*
is exactly what an operator needs.

## The capability sheet

Every row of a report, other than the harness-context rows (`C0.2`–`C0.4`), carries
exactly one **grade**, the **evidence** it rests on, and — when it was not measured —
an **unlock**. The rule and its reasons are ADR-0070, which extends ADR-0048.

| grade | meaning |
| :-- | :-- |
| `measured-hardware` | ran and passed against a plant an operator declared physical in `plant.yaml` |
| `measured-simulation` | ran and passed with no physical plant under it: a simulator, an in-tree fixture, or a static read; `mode` says which |
| `failed` | ran and did not pass, at any level |
| `not-supported-by-platform` | observed or declared absent: declined, not declared, another permitted route, or the surface is not implemented |
| `not-testable-here` | the obligation stands and this setting cannot observe it |
| `not-tested-suite-gap` | the suite's own limit |

A skipped row's grade is a function of its `SkipCause` alone; ADR-0070 publishes the
table, and `CAUSE_GRADE` in `model.py` is the same table.

- `evidence` — `observed` is what the run recorded for the row, with the cause
  spelling removed; `measurements` holds any number the harness measured and
  attributed to that check. A row whose run recorded an outcome and no observation
  says so in `note`; the report never supplies an observation the run did not make.
- `unlock` — REQUIRED on every `not-*` row, and absent on measured and failed rows:
  the concrete thing (a declaration, a sensor, a harness change, a witness outside
  the suite) that would make the row testable. It is resolved from
  [`unlocks.yaml`](unlocks.yaml), and a report containing a `not-*` row with no unlock
  is refused rather than written. An unlock says what would make a row testable, not
  that the row would pass.
- `subject` — what was graded: the implementation, the `fidelity`, and the
  `plant.yaml` the run was graded against (its digest, and the robot model and
  firmware as the operator stated them). `fidelity` is `hardware` only with
  `--mode physical` **and** a `plant.yaml` whose `plant.kind` an operator answered
  `hardware` ([`PLANT.md`](PLANT.md)); a contradiction between the two refuses the
  run. A simulated report never contains the grade `measured-hardware`.
- `verdict` — over every Core check of every required, applicable profile: `fail`
  if any row failed; otherwise `not-established` if any row was not measured and its
  cause is not one of the never-an-obligation causes (ADR-0048 rule B); otherwise
  `pass`. A Core check the run recorded no row for is not measured.
  `verdictBlockedBy` lists every row that prevents `pass`.
- `conformant` — `true` only when `verdict` is `pass`. It remains the Core statement,
  printed beside the sheet and not instead of it.

The human summary prints the sheet first and the verdict under it.
`python -m conformance` exits `0` on `pass`, `1` on any `fail`, `3` when nothing
failed and something was not established, and `2` when the run was refused.

## Self-certification and publication

Conformance is **self-certified** (ADR-0007). An implementer runs the published
suite against their own implementation, and the resulting `ConformanceReport` is
the attestation. No party issues a certificate; this project operates no
certification authority, no trust list beyond the factual registry, and no mark.

- An implementer MAY run the suite against anything, at any time, for any
  purpose. Running it requires no permission, no registration, and no fee.
- A conformance claim MUST be accompanied by the report that supports it. "X is
  conformant", published without the report, is unsupported.
- A published claim MUST state the **surface**, the **contract version**, and —
  where the surface has one — the **capability tier**, the **binding and
  contract**, or the **grammar version** the result was earned at. Without them
  the word has no fixed meaning, because the profiles that applied differ.
- An implementer MUST NOT describe a self-certified result as certification,
  endorsement, or approval by this project. Publication records a factual
  measurement.
- A result MUST be re-earned across a **major** contract-version bump, and when a
  suite revision changes what an implementation must do to pass. A revision that
  reorganises or clarifies without moving the bar does not invalidate a result.

**Why the program is not gated.** A suite that costs money or requires approval
to run measures whoever can afford it rather than whoever implemented the
contract, and makes the party operating the gate a gatekeeper over an interface
this specification exists to keep vendor-neutral. The cost is that a published
result is only as trustworthy as the implementer who ran it and the harness they
ran — which is why the `*0` self-checks, the honesty of skip reporting, and
`RB0.1`'s coverage assertion are load-bearing rather than ceremony.

## Running it

```bash
make conformance-profiles          # every surface this repository can drive
```

See [`README.md`](README.md) for the individual targets and what each one needs.
