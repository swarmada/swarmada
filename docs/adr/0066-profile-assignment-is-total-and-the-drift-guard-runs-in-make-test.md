# ADR-0066: Every catalog check is assigned to a profile, and the drift guard runs in `make test`

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, API Designer, Test Engineer
- **Related:** ADR-0007 (conformance is self-certified), ADR-0032 (contract versioning; the suite revision is not the contract version), ADR-0037 (protocol profiles record, not enforce), ADR-0048 (a verdict grades obligations, not observations); `conformance/profiles.yaml`, `conformance/PROFILES.md`, `tests/python/test_conformance_registry_drift.py`

## Context

The conformance suite is partitioned into named **profiles** with **Core** (mandatory)
and **Extended** (declared) support levels, and each surface — `FleetAdapter`,
`ComputeEndpoint`, `ControlPlane` — produces its own machine-readable
`ConformanceReport`. `profiles.yaml` calls itself the spine that keeps the catalog
(`adapters/CONFORMANCE.md`), the harness and the reports in agreement, and it carries a
self-check, `RB0.5`, that fails when the registry and the catalog documents disagree.

The measurement, taken 2026-09-15, found the framework built and one hole in it.
Generating an adapter report produced a **failing Core check**:

```
[FAIL] RB0.5  The registry and the catalog documents agree
              — defined in the catalog, absent from the registry:
                ['C18.1', 'C18.2', 'C18.3', 'C18.4', 'C18.5']
...
attestation          core      yes   fail         5/1/0
tier: FullControl   standalone: True   => NON-CONFORMANT
```

`adapters/CONFORMANCE.md` had grown the `C18` family (sensor streams and the duty
cycle). The FleetAdapter harness implemented all five rows. **They ran and they
passed.** And `profiles.yaml` assigned them to no profile, so the profile layer dropped
five executed MUSTs on the floor and `RB0.5` failed the `attestation` Core profile on
**every** adapter report produced in that window.

Two distinct facts are in that measurement. First, an unassigned check is more than
unreported — it turns a conformant adapter's report NON-CONFORMANT, so the failure is
loud in the wrong place. Second, and worse: **the guard existed and nothing ran it.**
`profiles.yaml` says drift is *"a FAILED check and not a review comment"* — which it is,
for someone who runs the suite, and nothing in `make test` ran the profile layer. The
drift was found by generating a report by hand, which is exactly the review comment the
file says this must not be.

## Decision

**Profile assignment is total: every check in a surface's catalog belongs to exactly
one profile feature, and the totality is machine-checked on every `make test`.**

- **The `sensor-streams` profile** was added to the FleetAdapter surface, assigning
  `C18.1`–`C18.5`. It is **Extended**, because the catalog says the whole family is
  optional — *"an adapter declining `activate_stream` declines the duty cycle for its
  robots, which is conformant under `C7.1`"*.
- **Always applicable, never conditional.** `C18.1` grades the shape of whatever was
  declared and *declaring nothing passes*; the harness agrees structurally (`_DECLINE_ARMS`
  arms `C18.2`–`C18.5` behind `activate_stream` and leaves `C18.1` unarmed). A
  conditional profile would report `notApplicable` over a row that had actually run —
  the exact contradiction `zone-restrictions` was corrected for on 2026-09-14, and the
  observation/obligation confusion ADR-0048 rules out.
- **It gates no capability tier** and is absent from `tierRequires` on purpose.
  Switching a sensor on commands no **motion**, so a Read Only adapter may serve sensor
  streams and remain Read Only — observation is what Read Only is *for*. The `reactive`
  profile already uses this reasoning, and RFC-0001's tier chapter is the complete
  statement of what a tier requires.
- **The duty cycle and the fail-safe are separate features.**
  `sensor-streams.duty-cycle` (`C18.5`, stop on `DeactivateStream`) is the ordinary
  path; `sensor-streams.lease-failsafe` (`C18.3`, stop when abandoned) is the fail-safe.
  The catalog draws the line itself — *"the lease is the fail-safe, not the ordinary
  path"* — and an adapter can stop cleanly on command while never stopping when
  abandoned, leaving a camera powered for a whole lease every time the control plane
  goes quiet. One feature spanning both would let a green `C18.5` report the fail-safe
  as held.
- **This family's safe state is the inverse of the e-stop lease's.** A stop must always
  be honoured, so nothing may gate it; a stream's failure mode is *left on*, so its
  lease **stops** it. `C18.3` is not `C4.1` with a different noun.
- **The guard runs unattended.** `tests/python/test_conformance_registry_drift.py` runs
  the same comparison `RB*0.5` runs, parameterised over all three surfaces, with **no
  adapter, no endpoint, no cluster and no network** — it is a pure registry-versus-prose
  comparison and needs no subject under test. It also fails if a surface is added to the
  registry and not to the test, pins `C18` by name (the sweep alone would also pass if
  `C18` were quietly *deleted* from the catalog, a different and worse resolution of the
  same failure), and pins the duty-cycle/fail-safe split.
- **The guard was proven falsifiable, not assumed.** Removing the `sensor-streams` block
  from `profiles.yaml` turned 3 of the 6 tests red with legible messages; restoring it
  returned 6/6 green. A guard that cannot fail is not a guard.

## Alternatives considered

- **Leave `C18` unassigned and downgrade `RB0.5` to a warning.** Rejected: it silences
  the one mechanism that caught the drift, and it converts a machine check back into the
  review comment `profiles.yaml` says it must never be.
- **Make `RB0.5` tolerate checks that exist in the catalog but no profile.** Rejected
  for the same reason. Totality is the property that makes a profile report a complete
  statement about a surface; a tolerated remainder is an unbounded blind spot.
- **Assign `C18` to an existing profile** (`reactive`, or `attestation`). Rejected: the
  tier and support-level semantics differ — `sensor-streams` is Extended and gates no
  tier — and folding it in would have changed the meaning of an existing profile's
  green result.
- **One `sensor-streams.lifecycle` feature covering `C18.3` and `C18.5`.** Rejected;
  see the fail-safe split above.
- **Run the drift guard in CI only, not in `make test`.** Rejected: the failure this
  ADR exists to prevent is precisely a guard that exists somewhere nobody runs. The test
  needs no adapter, no cluster and no network, so there is no cost argument for keeping
  it out of the default target.
- **Delete `C18` from the catalog until profiles were ready for it.** Rejected, and the
  new test pins against it: the checks were implemented, ran, and passed — removing them
  would discard demonstrated conformance to make a bookkeeping error go away.

## Consequences

- The suite revision was bumped to `2026.09.15`, following the file's own stated rule
  and the `zone-restrictions` precedent: **adding a profile is a suite change, not a
  contract change** — the contract version is unmoved and no adapter renegotiates
  anything (ADR-0032) — but it changes what a report says, so results earned against
  `2026.09.13` are invalidated and every row in `adapters/REGISTRY.md` must be
  re-attested against it.
- All six in-tree reports regenerate CONFORMANT via `make conformance-profiles`;
  `conformance-reports/fleet-adapter.yaml` carries `suiteRevision: 2026.09.15`,
  `capabilityTier: FullControl`, and `RB0.5` passing with 119 checks matched across
  three catalogs.
- `make test` grew 6 tests (121 → 127 Python tests) and now fails on registry drift
  without anyone generating a report by hand.
- **`adapters/REGISTRY.md` has not been re-attested**, and three sites still cite the
  superseded `2026.09.13` / `C0`–`C17`. The `simulation` row's replacement evidence
  already exists in the tree; `example-noop` has not been re-run. Striking another
  party's attestation row is a register decision, not a harness change. The error runs
  in the safe direction — a reader concludes "unverified" about a row that passes — so
  it is a stale citation rather than an unverified claim.
- `PROFILES.md` was updated in step (catalog range `C0`–`C18`, the Draft 4 table, a
  substantive section, and the heading corrected from "Two profiles" to "Three"),
  because the human-readable half drifting from the registry is the same defect in the
  other direction.
- RFC-0001's conformance chapter was **not** extended. `C18` post-dates it and is now
  partitioned in `PROFILES.md`; the precedence is recorded there (where the chapter and
  that document disagree about `C0`–`C17`, the chapter governs) rather than silently
  extending a normative chapter from a test artefact. Whether `C18` should be lifted
  into the chapter is a specification decision, left open.
- The `E0`–`E4` reservation (RFC-0003 Event Source identifiers) stays `excluded`. The
  `TaskTrigger`/`SemanticEvent` mechanism exists (ADR-0062), but the exclusion also
  turns on `FleetAction.status.events[]` being filled — the field exists and no shipped
  component writes it at v0.4 — and on `SwarmadaConfig.spec.events`, which does not
  exist; lifting it would create profile rows nothing can exercise, the
  precise failure the exclusion was written about.
