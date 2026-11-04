# ADR-0048: A conformance verdict grades obligations, not observations

- **Status:** accepted
- **Date:** 2026-09-03
- **Deciders:** Maintainers
- **Related:** ADR-0004, ADR-0007, `conformance/PROFILES.md`

## Context

A `ConformanceReport` carries two things that are easy to conflate: a record of
what the run observed, and a verdict on whether the implementation met its
obligations. The verdict is what a reader acts on. When the two are computed by
one mechanism, an observation that was never an obligation is graded as if it
were, and the verdict stops meaning what it says.

`PROFILES.md` already states the rule for one instance of this and gives the
reason:

> A check that could not apply is a third thing again. … Those are counted
> separately, as `notApplicable` in a profile's statistics, and they do not hold
> a profile at `notVerified`. … Counting the unreachable half as a skip would
> leave the `events` profile permanently `notVerified` for every endpoint that
> ever runs, which is a report that has stopped distinguishing anything.

It was written for `CE6.2`/`CE6.3` — a mutually exclusive pair where no
implementation can satisfy both. It was not applied in two other places that are
the same shape, and both produce a misleading verdict on the reference adapter's
published report.

**Case A — an Extended level reporting `fail`.** `handshake.extended`
reports `fail` because `C13.2` failed. `C13.2` is a `SHOULD`, and
`adapters/CONFORMANCE.md`, *C13 — Contract-version negotiation*, says of it in terms: *"an adapter that accepts
a command it should never have been sent is not non-conforming."* The Extended
tier is optional by construction (`conformance/PROFILES.md`, *Support levels*), so a `fail` there reads the
tier as mandatory. `conformance/PROFILES.md`, *The `ConformanceReport`*, defines the three-value rule for
**`core.result`** and has never defined `extended.result`; `fail` at Extended is
an artefact of reusing one function for both, not a decision anyone took.

**Case B — a Core check unreachable because the implementation took another legal
path.** `C5.6` asks that a `STOPPING` `EstopAck` reach a terminal
state. An adapter that acks terminal directly never sends `STOPPING`, which the
check's own text calls *"equally conformant"*. The check records a skip, and
`_level()` turns any skip into `notVerified`, so `safety` reads `notVerified` for
every conformant adapter that chose the direct-terminal shape. Hardware does not
change this: a real robot acking terminal directly produces the identical skip.

## Decision

**A verdict grades obligations. A check that was not an obligation of the
implementation under test is disclosed and counted, and does not produce a
verdict.**

One rule, applied in two places.

**A · Extended never reports `fail`.** Extended is optional. An implementation MAY
implement an Extended feature and MAY decline it, and `PROFILES.md` requires the
feature lists precisely so a reader can tell which. A failed Extended check
therefore makes its **feature unsupported** — which the report already states in
`unsupportedFeatures` — and does not make the **level** fail. `extended.result`
takes `pass`, `notVerified`, or `notApplicable`, never `fail`. The failed check
keeps `status: fail` in the `checks` list and the failure keeps its count in the
level's `statistics`, so nothing is hidden; only the verdict changes.

`core.result` is untouched. Core is mandatory, `fail` there is exactly right, and
`conformant` continues to read Core alone.

**B · A check made unreachable by a legal alternative path is `notApplicable`.**
A new `SkipCause`, `legal-alternative-path`, means: *the implementation satisfied
the requirement by another route the specification permits, so this check was
never an obligation of it.* It joins `binding-not-declared` and
`contract-not-declared` in `NOT_APPLICABLE_CAUSES`. It is named for the class,
not for `C5.6`, because `CE6.2`/`CE6.3` are the same class and the next such pair
will be too.

**What this rule does NOT cover.** A check the implementation was obliged to
satisfy and did not is a `fail`, and stays one. `legal-alternative-path` is not a
general escape: it requires that the specification actually permits the other
route and that the check is unreachable *because* that route was taken. An
implementation that did not do the thing at all records the cause that says so.

## The objection: claiming less to improve the report

This is the real question, and the answer has two parts — one that holds today and
one that does not.

**What holds.** In this suite an implementer cannot claim less, because an
implementer does not claim at all. `supportedFeatures` and `unsupportedFeatures`
are **derived from check outcomes** (`conformance/registry.py:158-164`), not
asserted by the implementation. There is no declaration input to shade. Declining
a feature is not a lever the report exposes: the only way a feature moves into
`supportedFeatures` is for its checks to pass, and the only way it stays out is
for them not to. Under rule A a failed Extended check still lands its feature in
`unsupportedFeatures` and still shows `status: fail` in the `checks` list, so the
shortfall is published at two of the three places it was published before. Only
the level verdict changes.

**What does not hold, and should be said plainly.** That defence is a property of
the current implementation, not of the specification. `conformance/PROFILES.md`, *Support levels*, says an
implementation *"MUST declare its Extended features explicitly"* — describing a
declaration the suite never reads. The moment a declaration input exists, the
incentive appears: declare nothing, and every Extended check becomes
`notApplicable` rather than `fail`. The derivation is what prevents
that, and derivation is an implementation detail.

**So the defence is insufficient on its own, and this ADR does not pretend
otherwise.** Two things would make it sufficient, and neither is in scope here:

1. **Publish the declaration alongside the derivation and check them against each
   other.** A feature declared supported whose checks did not pass is a
   contradiction the report can state — the same shape as `C0.1`, which already
   asserts that recorded skip causes are consistent with what the run observed. A
   declaration that cannot be contradicted by the run is exactly the free text
   `conformance/PROFILES.md`, *Support levels*, refuses elsewhere.
2. **Make the count visible where the claim is read.** A report whose Extended
   levels are uniformly `notApplicable` is a report that demonstrated nothing at
   that tier, and a reader comparing two implementations should not have to open
   both to learn it. A tier line stating *n of m Extended features supported*
   makes claiming less legible as claiming less.

Filed as a separate work item rather than decided here, because both change the report
schema and this ADR deliberately does not. That item also records the narrower
fault underneath: `conformance/PROFILES.md`, *Support levels*, already says an implementation "MUST
declare its Extended features explicitly", describing a declaration input the
suite has never had.

## Consequences

**What improves.** `safety` on the reference adapter reports `pass` rather than
`notVerified`, and the reason it previously did not is gone rather than
explained. `handshake.extended` loses a `fail` that never meant what it appeared
to mean. A profile no longer sits permanently at `notVerified` because an
implementation chose one of two legal shapes.

**What a reader loses, and it is real.** Before this change, `extended.result:
fail` was the one place a failed Extended check was visible at a glance. After
it, the verdict for an Extended level with a failure and an Extended level with a
skip are both `notVerified`, and telling them apart requires reading
`statistics.fail` or `unsupportedFeatures`. That is a genuine loss of legibility
at the summary line, accepted because the alternative — a verdict that says
`fail` for something no implementation was obliged to do — is worse. The
statistics block is unchanged precisely so the information survives somewhere
falsifiable.

**What is now enforced rather than assumed.** A skip whose detail string is not
an enumerated `SkipCause` is an error, not a silent fallthrough to
`"unenumerated"`. `C5.6`'s misspelling survived because the fallthrough was
quiet: the harness wrote `adapter-did-not-reach-state`, the profiler's enum says
`target-did-not-reach-state`, and the mismatch produced a valid-looking report
instead of a failure. `conformance/PROFILES.md`, *Support levels*, already forbade the outcome; nothing
enforced it.

**What is deliberately not changed.** `OPTIONAL_DECLINED` is **not** added to
`NOT_APPLICABLE_CAUSES`, though rule A's logic would admit it: a declined
optional feature genuinely was never an obligation. It is held back because the
cause is not trustworthy as things stand. `C10.1` records `optional-declined` with the
detail *"adapter refused the unverifiable probe artifact … so the optional
progress path was not reachable"* — which is a path made unreachable by an
earlier correct refusal, not a decline. Promoting the cause today would convert
that mislabelling into `provisioning.extended: pass` for a feature the adapter
never demonstrated, and would move five Compute Endpoint levels from
`notVerified` to `notApplicable` on the same unreliable signal. The cause must be
made honest before it can be trusted with a verdict. Filed as a separate work item.

**Scope of the verdict change.** Two verdicts change, both on
`conformance-reports/fleet-adapter.yaml`, both inside this repository. No
`ConformanceReport` artefact exists outside it; the only externally published
verdicts are the prose rows in the public `adapters/REGISTRY.md`, which say
*"safety-complete"* and *"C13.2 defence-in-depth absent — non-blocking"*. Both
become more consistent with the report, not less, so no published claim changes
meaning and ADR-0004's amendment path is not engaged.
