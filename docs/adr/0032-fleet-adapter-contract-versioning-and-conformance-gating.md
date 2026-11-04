# ADR-0032: Fleet-adapter contract versioning, compatibility skew, and version-bound conformance gating

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** API Designer, Kubernetes Expert, Distributed Systems Reviewer, Security Reviewer
- **Related:** ADR-0005 (reference adapter policy), ADR-0007 (conformance is self-certified), ADR-0019 (adapter action discovery and validation), ADR-0025 (ControlStream mTLS), RFC-0001 "Resource Model at a Glance" / "FleetAdapter" / "Fleet Adapter Protocol"; `proto/fleet_adapter/v1/fleet_adapter.proto` (`AdapterHello`, `HelloAck`, `RegistrationRejection`), `adapters/CONFORMANCE.md`, `adapters/REGISTRY.md`

## Context

A Fleet Adapter is an independently installed, independently versioned artifact: a vendor adapter lives in the vendor's own repository and ships on the vendor's cadence (ADR-0005). The `fleet_adapter.v1` contract it speaks is owned by this project. Nothing today constrains the two to agree on a *version* of that contract beyond a coarse identity check, and nothing ties an adapter's conformance result to the specific contract version it will drive at runtime. An adapter that mistranslates an `AssignTask` because it was built against a different revision of the contract is a safety condition, not only a compatibility defect.

The primitives already in place, as built:

- **Handshake version fields.** `AdapterHello` carries `adapter_version` (the adapter build / semver) and `protocol_version` (a package identity string, e.g. `"fleet_adapter.v1"`). `HelloAck` returns `negotiated_protocol_version`. `RegistrationRejection` defines `REGISTRATION_REJECTION_VERSION_MISMATCH`.
- **Conformance result on status.** `FleetAdapter.status.conformance` holds a digest-verified conformance report (RFC-0001, "FleetAdapter"; the status controller and the harness wiring are implemented). Conformance is **self-certified** against the open harness — the project runs no certification authority (ADR-0007).
- **A factual registry.** `adapters/REGISTRY.md` records, per adapter, the protocol version it was run against and its conformance status (ADR-0005, ADR-0007).
- **Estop is already isolated.** `Estop`/`EstopAck` travel on `SafetyStream`, are not fenced, and are always honored (`CONFORMANCE.md` C5); the deprecated `Command.estop` arms on `ControlStream` exist only for wire compatibility.

What is missing is not a place to put a version — it is *meaning and enforcement* around the version that exists:

1. `protocol_version` is a bare **identity** (`"fleet_adapter.v1"`), not a semver, so it cannot express "compatible within a range." The only outcomes are exact-match or `VERSION_MISMATCH`; there is no defined skew.
2. The conformance result and the assignment path are not defined to be **bound to a specific contract version**. A `status.conformance = Passed` earned against one revision of the contract is treated as valid against any.
3. The version-invariance of the estop path — true today by construction — is **not stated as a contract guarantee**, so a future contract change could erode it unnoticed.

Two constraints bound any solution. Qualification must not couple every vendor to this project's *release* cadence (an O(adapters × releases) matrix no vendor will maintain, and contrary to independent install per ADR-0005). And the enforcement must not become a certification gate operated by the project or its maintainers — ADR-0007 places that function, if it is ever wanted, in a neutral body outside the standard repository.

## Decision

Give the existing contract a semantic version, define a bounded compatibility skew, bind the (self-certified) conformance result to that version, and gate task **assignment** — and only assignment — on the pair. State the estop path's version-invariance as a contract guarantee.

- **Contract version (semver), additive.** Introduce a semver **contract version** for the fleet-adapter contract (proto surface + `CapabilitiesSnapshot`/`SupportedAction` schema per ADR-0019). The **conformance-suite revision is deliberately NOT part of it** — it is provenance recorded beside the result, per `adapters/REGISTRY.md`: *"a suite change re-attests adapters, it does not move the contract they negotiate."* Folding it in would make every test improvement a protocol change, and would let a result carry forward across a pass bar it never cleared, distinct from the wire-package identity (`protocol_version`) and the adapter build (`adapter_version`). Carry it additively: `AdapterHello.contract_version` and `HelloAck.negotiated_contract_version` (additive fields; `make proto-lint` breaking-check stays green). The control plane advertises the contract-version range it implements on `SwarmadaConfig.status`.

- **Compatibility gate at the handshake.** The control plane intersects the adapter's reported contract version with its advertised supported range. Out of range fails the handshake with the existing `REGISTRATION_REJECTION_VERSION_MISMATCH`. Supported range is **N and N-1 minor** within the current major; a **major** bump is breaking and requires re-qualification. ~~Patch and minor releases never break an adapter, so they never invalidate its qualification.~~ **STRUCK — see Amendment 1 §A1.2.** A minor release never invalidates a *qualification*, and it does evict an adapter: the window is two minors wide, so an adapter falls out of range on the **second** minor after the version it reports and is from then on refused registration. The corrected statement is §A1.2's.

- **Version-bound conformance.** `FleetAdapter.status.conformance` records the contract version the result was earned against, and the registry `Protocol` column carries that semver. Conformance remains **self-run and self-attested** (ADR-0007, unchanged): the harness produces the result; the operator (or vendor) attests it via the registry PR flow. The control plane consumes the already-attested result — it does not issue one.

- **Work gate (registration and assignment; nothing more).** `AssignTask` eligibility requires both a compatible negotiated contract version and a `status.conformance` result earned against a compatible version. An incompatible adapter is additionally refused **registration**: the handshake itself is accepted, so the adapter stays visible and its session stands, but `RegisterRobot` and `DiscoverRobot` on that connection are rejected `RegistrationRejection.VERSION_MISMATCH`. Registration is where the refusal has to land — the enum exists on `RegisterAck` / `DiscoverAck` and nowhere else, so it is the only point at which the wire can express this — and bringing a robot under management through an adapter that can never be dispatched to would only create a robot that cannot work. An adapter that fails either condition keeps streaming telemetry and heartbeats and — always — receives estop, subject to mTLS identity (ADR-0025). The gate is on *work* only; it never blocks observation or stopping. Missing or unparseable version data is treated as incompatible (fail-closed on work), never as an implicit pass.

- **Estop is version-invariant.** `Estop`/`EstopAck` are frozen across all contract versions within a major line: no field of either message may acquire a dependency on the contract version, and estop MUST be honored against any connected adapter irrespective of its compatibility or conformance state. This records as a contract guarantee what `CONFORMANCE.md` C5 and the `SafetyStream` split already provide, and the conformance harness asserts it.

## Alternatives considered

- **Keep `protocol_version` a bare identity; reject only on exact mismatch.** No graceful skew: any additive proto change would either break every adapter or mean nothing. Rejected — it cannot express compatibility within a range, which is the whole requirement.

- **Qualify adapters against the Swarmada release number.** Every patch release would nominally invalidate every adapter (O(adapters × releases)), coupling independently-shipped vendor adapters to this project's release train — contrary to ADR-0005. Rejected. The release notes instead publish a release→contract-version map; qualification keys on the contract version.

- **A certification gate the control plane (or maintainers) operate before assignment.** Makes the project a gatekeeper and contradicts ADR-0007. Rejected. The runtime gate consumes a *self-run, self-attested* conformance result inside the operator's own cluster; it is an operational admission control, not a certification authority, and it leaves ADR-0007's seam for a future neutral mark intact.

- **Overload `adapter_version` (the build) for compatibility.** The build is not the contract: two vendor builds at different `adapter_version`s can implement the same contract version, and one vendor can hold a build fixed across contract revisions. Rejected — conflates two independent axes.

## Consequences

- **Graceful skew, legible compatibility.** N/N-1 minor support means an adapter and a control plane need not upgrade in lockstep; the registry `Protocol` column and `status.conformance` make "which adapter is qualified for which contract version" answerable without reading code. ~~Patch/minor releases stop invalidating adapters.~~ **STRUCK — see Amendment 1 §A1.2.** They stop invalidating QUALIFICATIONS. The skew is graceful for one minor and expires at the second, which is a migration window rather than a standing compatibility promise.

- **The contract becomes a governed semver artifact.** ~~It must be bumped deliberately when the proto surface, the `SupportedAction` schema, or the conformance-suite revision changes~~ **STRUCK — see Amendment 1 §A1.1.** It must be bumped deliberately when the proto surface or the `SupportedAction` schema changes; a conformance-suite revision re-attests adapters and does not move the contract version. Each release must publish its supported contract range and the release→contract map. This is new maintenance the project takes on.

- **A defined re-qualification trigger for vendors.** On a **major** contract bump an adapter re-runs `make conformance` and updates its registry row (the ADR-0007 PR flow). ~~Minor/patch bumps require nothing.~~ **STRUCK — see Amendment 1 §A1.2.** A minor bump requires no RE-QUALIFICATION — and it does require a vendor to raise the `CONTRACT_VERSION` its adapter declares, in its own repository, before the second minor after that version evicts it. That is a one-line declaration change and not a suite re-run, which is the distinction the struck sentence collapsed. This is the "confirmed for a given version" property, expressed through the existing self-certification mechanism rather than a new authority.

- **Estop safety is pinned.** The version-invariance guarantee prevents a future contract change from silently coupling the stop path to negotiation state; the harness gains a case that asserts it.

- **Fail-closed on work, fail-open on stopping.** An unqualified or version-incompatible adapter is visible (telemetry) and controllable (estop) but cannot be dispatched work — the safe posture for a physical fleet.

- **Implementation baseline (updated 2026-08-19).** The enforcement seam exists and both additive gaps originally noted here (audited 2026-07-25) have since been closed: the RobotAdmissionGate ANDs `status.phase == Connected` with `Conformance == Passed`, and the FleetAdapter controller sets `phase = Rejected` on a failed conformance report or an un-negotiable protocol version, so dispatch is gated *indirectly* (the Scheduler correctly ignores the adapter and needs no change). `status.conformanceContractVersion` now binds `status.conformance` to the semver contract version it was earned against (`api/v1/fleetadapter_types.go`), and the Scheduler's candidate filter 9 re-checks it at scheduling time. Version negotiation is a semver range with N/N-1 skew within the current major, implemented in `internal/contract/version.go` (`SupportedRange`/`Supports`) and wired into the admission webhook, the ControlStream server, and the FleetAdapter/SwarmadaConfig controllers. No new Scheduler check and no certification authority were required, consistent with the original design.

- **Seams left open.** Capability-profile-granularity versioning (finer than the whole-contract gate) is deferred; a neutral certification body can still layer a mark on top of the self-certified, now version-bound result without any change here (the ADR-0007 seam is preserved).

## Amendment 1 — what the zone-restriction adapter surface bumps, and what a minor bump actually costs an adapter

- **Date:** 2026-09-13
- **Basis:** `docs/adr/README.md` — the decision itself is not changed, so no superseding ADR. The
  original text above is left intact and this is appended. Two things are recorded here that the
  original could not: the application of the versioning rule to a change that had not been written
  when it was made, and a contradiction inside the original text that is corrected rather than
  re-read.
- **Sources:** the pre-implementation ruling requested before
  `rfcs/amendments/RFC-0001-zone-restrictions.md` Parts D and E are written into
  `fleet_adapter.v1`.
- **What is unchanged:** the decision. The contract stays a semver. The skew stays **N and N-1
  minor** within the current major. The work gate stays on registration and assignment and nothing
  more. Estop stays version-invariant. Conformance stays self-run and self-attested (ADR-0007).

### A1.1 — The Part D and Part E additions are a MINOR bump: `1.1.0` → `1.2.0`

The governing definition is the one in **Decision**, bullet 1: the contract version is a semver
over **two** components — the proto surface, and the `CapabilitiesSnapshot`/`SupportedAction`
schema per ADR-0019. The conformance-suite revision is **deliberately not** one of them; it is
provenance recorded beside the result, because *"a suite change re-attests adapters, it does not
move the contract they negotiate."* Each component, applied:

**Component 1 — the proto surface.** Part D adds four surfaces to `fleet_adapter.v1`:
`CapabilitiesSnapshot.honoured_operating_constraints` (FA-R1), the resolved operating-rule set on
`AssignAction` (FA-R2), a new `AssignActionRejection` value and the unhonoured-subject list on
`AssignActionResult` (FA-R5). All four take new field numbers. None renumbers, removes, retypes or
changes the meaning of an existing field; `make proto-lint`'s breaking check stays green. A build
that has never seen them parses a message carrying them and ignores them. **Additive — MINOR.**

**Component 2 — the `CapabilitiesSnapshot`/`SupportedAction` schema.** One new optional repeated
field on `CapabilitiesSnapshot`. `SupportedAction` is untouched. A snapshot from an adapter that
does not set it is byte-identical to one produced before the field existed, and absence carries
exactly the meaning the older adapter already had. No existing field changes meaning.
**Additive — MINOR.**

**Component 3 does not exist.** Part E adds a `zone-restrictions` conformance profile on two
surfaces and raises the pass bar — its own text says *"the registry revision that introduces it
invalidates results earned before it."* Under Decision bullet 1 that is a **suite** change: it
triggers re-attestation through the ADR-0007 registry PR flow, and it does not move the number an
adapter negotiates. The reasoning recorded in bullet 1 applies here unchanged — folding the suite
revision in would make this test improvement a protocol change, and would evict every adapter that
had not re-run a suite it was never required to run.

**Ruling: MINOR. `1.1.0` → `1.2.0`.** The bump itself is performed by the change that lands Part D;
this amendment settles only what it means.

**A contrary reading exists in the tree and is not the governing one.** Three places still describe
the contract version as a semver over *three* things, the third being the conformance-suite
revision: this ADR's own **Consequences** ("It must be bumped deliberately when the proto surface,
the `SupportedAction` schema, or the conformance-suite revision changes"), the doc comment on
`contract.Version` in `internal/contract/version.go`, and the comment on `CONTRACT_VERSION` in
`adapters/conformance/report.py`. All three predate the narrowing recorded in Decision bullet 1 and
were not moved with it. The difference is not editorial: under the three-part reading Part E's new
profile is itself a component change, and because a new profile invalidates results earned before
it, the same change would have to be read as **MAJOR** — the opposite answer, with re-qualification
demanded of every vendor. **Decision governs; the three stale statements are a defect against this
ADR, filed in the tracker, and belong to the owners of those files.**

### A1.2 — A minor bump **does** break an adapter. The claim in Decision bullet 3 is false as written

Decision bullet 3 states: *"Patch and minor releases never break an adapter, so they never
invalidate its qualification."* **Consequences** repeats it (*"Patch/minor releases stop
invalidating adapters"*) and again (*"Minor/patch bumps require nothing"*).

That cannot be true alongside the sentence immediately before it in the same bullet: *"Supported
range is N and N-1 minor within the current major."* A window two minors wide is a window that
expires. Measured against `internal/contract/version.go`, where `SupportedRange()` is derived from
`Version` rather than maintained beside it:

    const Version = "1.1.0"    ->    SupportedRange() == ">=1.0.0 <1.2.0"
    const Version = "1.2.0"    ->    SupportedRange() == ">=1.1.0 <1.3.0"

An adapter reporting `1.0.0` is inside the first range and outside the second. `Supports("1.0.0")`
against `1.2.0` returns false, with *"contract_version \"1.0.0\" is outside the supported range
>=1.1.0 <1.3.0"*. The consequence is the one Decision bullet 4 specifies, and it is not advisory:
the handshake is still accepted, the session stands, telemetry and heartbeats continue and estop is
still honoured — and `RegisterRobot` and `DiscoverRobot` on that connection are refused
`REGISTRATION_REJECTION_VERSION_MISMATCH`. No robot can be brought under management through that
adapter. No operator action lifts it. Only a vendor rebuild does.

**Ruling: it is a break.** The sentence has two halves and only one survives:

- **True, and kept.** A minor bump never *invalidates a qualification*. The conformance report is
  untouched, `status.conformanceContractVersion` still records the version the result was earned
  against, and nothing in the ADR-0007 attestation flow has to be redone.
- **False, and struck.** *"Patch and minor releases never break an adapter."* A **patch** never
  does. A **minor** does not break an adapter built against the current or the immediately
  preceding minor. The **second** minor release after an adapter's reported version evicts it from
  the supported range, and an evicted adapter cannot register a robot.

The corrected statement, which replaces the struck one wherever the original text asserts it:

> A **patch** release never affects an adapter. A **minor** release never affects an adapter
> reporting the current or the immediately preceding minor. An adapter falls out of the supported
> range on the **second** minor release after the contract version it reports, and is from then on
> refused registration — **without** its conformance result being invalidated. A qualification that
> survives is not the same thing as an adapter that still works, and the original text collapsed
> the two.

This is not hypothetical for the bump A1.1 authorises. Every adapter in this repository declares
`CONTRACT_VERSION = "1.0.0"` — the simulation adapter, the `example-noop` reference skeleton and
the cookiecutter template — so all three are evicted the moment `contract.Version` reaches
`1.2.0`. `TestInTreeAdaptersReportASupportedContractVersion` in
`internal/contract/adapter_versions_test.go` asserts this through the real `Supports()` and goes
red for all three on the bump. **That test going red is correct behaviour and a true finding, not a
test to relax.** The repair is to move each declaration to the lowest version inside the new range
that is a true claim about what that adapter implements — which is a question about each adapter,
not a substitution — and the release notes must carry the eviction as a compatibility note beside
the release-to-contract map.

The contradiction is recorded as a defect against this ADR rather than resolved by re-reading.
"Never breaks an adapter" sitting beside a rule that expires cannot be rescued by a narrower
reading of *break*: it was written as an unconditional promise, it was read as one, and a reading
that made it technically survivable would leave the next reader to make the same mistake.

**Option named and not taken:** widening the skew (N-2, or a deprecation window) would postpone the
eviction, not remove it, and it trades the O(adapters × releases) matrix Decision rejected back in
by degrees. It is left as a seam, to be decided against a measured vendor cadence rather than
against this bump.

~~**Recorded 2026-10-04: the window is final.** Draft 4 is the last draft that changes the contract,
so no later minor release comes. The contract stays at `1.4.0` and the supported range stays
`>=1.3.0 <1.5.0`. The expiry this section describes does not fire again, and the seam above is
closed.~~ **STRUCK 2026-11-01.**

**Recorded 2026-11-01: the contract is frozen at RFC-0001 v1.0.** Until then a later contract version
can move the window, and the expiry this section describes fires at each minor. A minor is a wire addition
or a change that binds adapters: one that adds, changes or removes an obligation for
adapters. A change that binds adapters for safety also raises the lowest supported version to the new one. Any
other change off the wire is a patch bump, which does not move the window. A conformance check that tests
an obligation already stated moves the suite revision only. The contract version does not change, so §A1.1
stands as written. At contract `1.4.0` the
supported range is `>=1.3.0 <1.5.0`. The window is not widened: an adapter outside it, such as one still
declaring `1.0.0`, moves to a version inside it in its own repository. The policy is stated in RFC-0001's
Resource model and in [API Design Principles](../api-principles.md#versioning-and-compatibility).

### A1.3 — The three sites are struck IN PLACE, not rewritten

Recorded because the next reader will ask why an immutable ADR has strikethrough in its Decision.

`docs/adr/README.md` permits an editorial correction in place and reserves a superseding ADR for a
change to the decision. **§A1.2 is where the decision changed**; the three sites below it were left
asserting the struck sentence, which is worse than either state — a reader arriving at Decision
bullet 3 had no signal that an amendment forty lines down had already contradicted it.

So each site now carries the sentence struck through, a pointer to §A1.2, and the corrected claim
beside it. That is deliberately not a silent rewrite: **the false sentence stays visible because it
was quoted**, in release notes, in vendor conversations and in this tracker, and a reader who
arrives holding the quotation must be able to find it and see that it is struck. Deleting it would
leave them concluding they had misremembered.

The three sites, and what each now says:

| Site | Struck | Now |
| :-- | :-- | :-- |
| Decision, compatibility-gate bullet | *"Patch and minor releases never break an adapter, so they never invalidate its qualification."* | A minor never invalidates a qualification; the second minor after an adapter's reported version evicts it and it is then refused registration. |
| Consequences, graceful-skew bullet | *"Patch/minor releases stop invalidating adapters."* | They stop invalidating **qualifications**. The skew is graceful for one minor and expires at the second — a migration window, not a standing promise. |
| Consequences, re-qualification bullet | *"Minor/patch bumps require nothing."* | A minor requires no **re-qualification**, and does require the vendor to raise the declared `CONTRACT_VERSION` in their own repository. A one-line declaration change, not a suite re-run. |
| Consequences, governed-semver bullet *(struck 2026-09-24; points to §A1.1, not §A1.2)* | *"It must be bumped deliberately when the proto surface, the `SupportedAction` schema, or the conformance-suite revision changes"* | Bumped when the proto surface or the `SupportedAction` schema changes. A suite revision re-attests adapters and does not move the contract version — the reading §A1.1 already set out, which this bullet had kept contradicting. |

**The third row is the one that changes what a vendor does**, and it is the half the original text
hid most completely: "requires nothing" was read as "no action at all", when the action required is
small, mandatory, and in a repository this project does not control.

### A1.4 — Consequences of this amendment

- `contract.Version` and the harness's `CONTRACT_VERSION` are now checked to move together by
  `make spec-contract` (`scripts/check-contract-version-pair.py`).
  The rule was stated in `version.go`'s own comment from the day the constant landed and had no
  invoker until now.
- The N/N-1 window is documented as an **expiry**, not a promise of indefinite compatibility.
  Anything that quotes "minor releases never break an adapter" is quoting struck text.
- A contract bump now has a defined pre-flight: check which declared adapter versions fall out of
  the new range before the bump lands, not after.
