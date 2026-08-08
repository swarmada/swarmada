# ADR-0032: Fleet-adapter contract versioning, compatibility skew, and version-bound conformance gating

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** API Designer, Kubernetes Expert, Distributed Systems Reviewer, Security Reviewer
- **Related:** ADR-0005 (reference adapter policy), ADR-0007 (conformance is self-certified), ADR-0019 (adapter action discovery and validation), ADR-0025 (ControlStream mTLS), RFC-0001 §5.2 (Resource Model at a Glance) / §9.1.13 (FleetAdapter) / §9.2 (Fleet Adapter Protocol); `proto/fleet_adapter/v1/fleet_adapter.proto` (`AdapterHello`, `HelloAck`, `RegistrationRejection`), `adapters/CONFORMANCE.md`, `adapters/REGISTRY.md`

## Context

A Fleet Adapter is an independently installed, independently versioned artifact: a vendor adapter lives in the vendor's own repository and ships on the vendor's cadence (ADR-0005). The `fleet_adapter.v1` contract it speaks is owned by this project. Nothing today constrains the two to agree on a *version* of that contract beyond a coarse identity check, and nothing ties an adapter's conformance result to the specific contract version it will drive at runtime. An adapter that mistranslates an `AssignTask` because it was built against a different revision of the contract is a safety condition, not merely a compatibility defect.

The primitives already in place, as built:

- **Handshake version fields.** `AdapterHello` carries `adapter_version` (the adapter build / semver) and `protocol_version` (a package identity string, e.g. `"fleet_adapter.v1"`). `HelloAck` returns `negotiated_protocol_version`. `RegistrationRejection` defines `REGISTRATION_REJECTION_VERSION_MISMATCH`.
- **Conformance result on status.** `FleetAdapter.status.conformance` holds a digest-verified conformance report (RFC-0001 §9.2.7; the status controller and the harness wiring are implemented). Conformance is **self-certified** against the open harness — the project runs no certification authority (ADR-0007).
- **A factual registry.** `adapters/REGISTRY.md` records, per adapter, the protocol version it was run against and its conformance status (ADR-0005, ADR-0007).
- **Estop is already isolated.** `Estop`/`EstopAck` travel on `SafetyStream`, are not fenced, and are always honored (`CONFORMANCE.md` C5); the deprecated `Command.estop` arms on `ControlStream` exist only for wire compatibility.

What is missing is not a place to put a version — it is *meaning and enforcement* around the version that exists:

1. `protocol_version` is a bare **identity** (`"fleet_adapter.v1"`), not a semver, so it cannot express "compatible within a range." The only outcomes are exact-match or `VERSION_MISMATCH`; there is no defined skew.
2. The conformance result and the assignment path are not defined to be **bound to a specific contract version**. A `status.conformance = Passed` earned against one revision of the contract is treated as valid against any.
3. The version-invariance of the estop path — true today by construction — is **not stated as a contract guarantee**, so a future contract change could erode it unnoticed.

Two constraints bound any solution. Qualification must not couple every vendor to this project's *release* cadence (an O(adapters × releases) matrix no vendor will maintain, and contrary to independent install per ADR-0005). And the enforcement must not become a certification gate operated by the project or its maintainers — ADR-0007 places that function, if it is ever wanted, in a neutral body outside the standard repository.

## Decision

Give the existing contract a semantic version, define a bounded compatibility skew, bind the (self-certified) conformance result to that version, and gate task **assignment** — and only assignment — on the pair. State the estop path's version-invariance as a contract guarantee.

- **Contract version (semver), additive.** Introduce a semver **contract version** for the fleet-adapter contract (proto surface + `CapabilitiesSnapshot`/`SupportedAction` schema per ADR-0019 + conformance-suite revision), distinct from the wire-package identity (`protocol_version`) and the adapter build (`adapter_version`). Carry it additively: `AdapterHello.contract_version` and `HelloAck.negotiated_contract_version` (additive fields; `make proto-lint` breaking-check stays green). The control plane advertises the contract-version range it implements on `SwarmadaConfig.status`.

- **Compatibility gate at the handshake.** The control plane intersects the adapter's reported contract version with its advertised supported range. Out of range fails the handshake with the existing `REGISTRATION_REJECTION_VERSION_MISMATCH`. Supported range is **N and N-1 minor** within the current major; a **major** bump is breaking and requires re-qualification. Patch and minor releases never break an adapter, so they never invalidate its qualification.

- **Version-bound conformance.** `FleetAdapter.status.conformance` records the contract version the result was earned against, and the registry `Protocol` column carries that semver. Conformance remains **self-run and self-attested** (ADR-0007, unchanged): the harness produces the result; the operator (or vendor) attests it via the registry PR flow. The control plane consumes the already-attested result — it does not issue one.

- **Work gate (registration and assignment; nothing more).** `AssignTask` eligibility requires both a compatible negotiated contract version and a `status.conformance` result earned against a compatible version. An incompatible adapter is additionally refused **registration**: the handshake itself is accepted, so the adapter stays visible and its session stands, but `RegisterRobot` and `DiscoverRobot` on that connection are rejected `RegistrationRejection.VERSION_MISMATCH`. Registration is where the refusal has to land — the enum exists on `RegisterAck` / `DiscoverAck` and nowhere else, so it is the only point at which the wire can express this — and bringing a robot under management through an adapter that can never be dispatched to would only create a robot that cannot work. An adapter that fails either condition keeps streaming telemetry and heartbeats and — always — receives estop, subject to mTLS identity (ADR-0025). The gate is on *work* only; it never blocks observation or stopping. Missing or unparseable version data is treated as incompatible (fail-closed on work), never as an implicit pass.

- **Estop is version-invariant.** `Estop`/`EstopAck` are frozen across all contract versions within a major line: no field of either message may acquire a dependency on the contract version, and estop MUST be honored against any connected adapter irrespective of its compatibility or conformance state. This records as a contract guarantee what `CONFORMANCE.md` C5 and the `SafetyStream` split already provide, and the conformance harness asserts it.

## Alternatives considered

- **Keep `protocol_version` a bare identity; reject only on exact mismatch.** No graceful skew: any additive proto change would either break every adapter or mean nothing. Rejected — it cannot express compatibility within a range, which is the whole requirement.

- **Qualify adapters against the Swarmada release number.** Every patch release would nominally invalidate every adapter (O(adapters × releases)), coupling independently-shipped vendor adapters to this project's release train — contrary to ADR-0005. Rejected. The release notes instead publish a release→contract-version map; qualification keys on the contract version.

- **A certification gate the control plane (or maintainers) operate before assignment.** Makes the project a gatekeeper and contradicts ADR-0007. Rejected. The runtime gate consumes a *self-run, self-attested* conformance result inside the operator's own cluster; it is an operational admission control, not a certification authority, and it leaves ADR-0007's seam for a future neutral mark intact.

- **Overload `adapter_version` (the build) for compatibility.** The build is not the contract: two vendor builds at different `adapter_version`s can implement the same contract version, and one vendor can hold a build fixed across contract revisions. Rejected — conflates two independent axes.

## Consequences

- **Graceful skew, legible compatibility.** N/N-1 minor support means an adapter and a control plane need not upgrade in lockstep; the registry `Protocol` column and `status.conformance` make "which adapter is qualified for which contract version" answerable without reading code. Patch/minor releases stop invalidating adapters.

- **The contract becomes a governed semver artifact.** It must be bumped deliberately when the proto surface, the `SupportedAction` schema, or the conformance-suite revision changes, and each release must publish its supported contract range and the release→contract map. This is new maintenance the project takes on.

- **A defined re-qualification trigger for vendors.** On a **major** contract bump an adapter re-runs `make conformance` and updates its registry row (the ADR-0007 PR flow). Minor/patch bumps require nothing. This is the "confirmed for a given version" property, expressed through the existing self-certification mechanism rather than a new authority.

- **Estop safety is pinned.** The version-invariance guarantee prevents a future contract change from silently coupling the stop path to negotiation state; the harness gains a case that asserts it.

- **Fail-closed on work, fail-open on stopping.** An unqualified or version-incompatible adapter is visible (telemetry) and controllable (estop) but cannot be dispatched work — the safe posture for a physical fleet.

- **Implementation baseline (audited 2026-07-25).** The enforcement seam already exists: the RobotAdmissionGate ANDs `status.phase == Connected` with `Conformance == Passed`, and the FleetAdapter controller sets `phase = Rejected` on a failed conformance report or an un-negotiable protocol version — so dispatch is already gated *indirectly* (the Scheduler correctly ignores the adapter and needs no change). Two additive gaps remain: (1) `status.conformance` is not bound to the contract version it was earned against — `Conformance` and `negotiatedProtocolVersion` are separate fields and `spec.protocolVersion` is the bare identifier `"fleet_adapter.v1"`, not a semver; bind them and give the version semver meaning. (2) Version negotiation is exact-identifier, not a semver range with N/N-1 skew; upgrade it and fold the range check into the existing admission gate. Both are additive and preserve the RA-1 discipline (version/conformance written on transition, never on a telemetry tick). No new Scheduler check and no certification authority are required.

- **Seams left open.** Capability-profile-granularity versioning (finer than the whole-contract gate) is deferred; a neutral certification body can still layer a mark on top of the self-certified, now version-bound result without any change here (the ADR-0007 seam is preserved).
