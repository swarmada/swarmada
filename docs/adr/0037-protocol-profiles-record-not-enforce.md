# ADR-0037: Protocol profiles — declare which external standard an adapter speaks; record it, do not enforce it

- **Status:** accepted
- **Date:** 2026-08-12 (accepted 2026-08-13)
- **Deciders:** API Designer, Kubernetes Expert, Distributed Systems Reviewer
- **Related:** ADR-0032 (contract versioning and conformance gating), ADR-0019 (`status.supportedActions` as discovery), ADR-0007 (conformance is self-certified), ADR-0005 (reference adapter policy), RFC-0001 §9.6 (Fleet Adapter protocol), RFC-0001 {{ref:fleetadapter}}

> **Implementation note (2026-08-21).** The rename this ADR mandates has landed: *capability
> profile* no longer occurs anywhere in RFC-0001, and *capability set* is the term in use. The
> Context below therefore describes the collision **as it stood on 2026-08-12**, not the current
> text — it is retained as the record of why the decision was taken, and its citations name
> chapters rather than line numbers because the passages it describes have since been rewritten
> by this very decision.

## Context

A Fleet Adapter bridges the Swarmada contract to some external protocol. Nothing in the
API records **which** external protocol that is.

`api/v1/fleetadapter_types.go` carries a full version vocabulary — `ProtocolVersion:92`,
`SuiteVersion:123`, `ConformanceContractVersion:198`, `NegotiatedProtocolVersion:204`,
`NegotiatedContractVersion:206` — and every one of those describes the *Swarmada*
contract or its conformance suite. The external standard appears in exactly one place:
`adapters/REGISTRY.md`, as prose in an `Interface / class` column, adjacent to a
`Protocol` column that means the Swarmada contract. Two columns, both reading
"protocol", two different axes. `docs/adapters.md` uses the words "comms
profile" for the same idea, in documentation only.

The consequence is that a sentence of the form "adapter X supports standard Y" cannot
be stated as a fact with a shape. It has no version, no role, no place to record what
verified it, and no vocabulary in which a third party writing their own adapter could
state what they implemented. Conformance in this project means conformance to the
Swarmada contract, measured by the C0–C16 catalog (ADR-0007); the catalog asserts on the
northbound gRPC surface only. A registry row that pairs the word CONFORMANT with the
name of an external standard therefore reads, to a column-scanning reader, as a claim
the harness does not make.

Two further facts constrain the shape of any fix.

**The word "profile" was already overloaded.** The Fleet Adapter protocol chapter reserved a
future seam for *capability profiles* — an adapter declaring conformance to individual
parts of the Swarmada contract at independent versions. That is a northbound axis.
RFC-0004 (named in the references chapter) used "profile" in a third sense, for the task-submission
surface as a profile over the Kubernetes API. The safety chapter used the word generically,
and `docs/adapter-use-cases.md` used it for a CLI flag. Adding a fourth sense without
resolving the collision would compound a defect that exists today.

**Nothing on the robot side of the boundary declares which protocol is spoken.**
`api/v1/robot_types.go` has no protocol, standard or dialect field; a robot binds to an
adapter by name (`AdapterRef:116-121`, reached via `Robot.spec.adapter:207`).
`api/v1/robotclass_types.go` declares `manufacturer:79`, `model:84`, `baseAdapter:89`,
`hardware:98`, `defaultModels:105` and `baseCapabilities:112` — the machine's identity,
parts, abilities and integration binding — and no protocol.

> **Citation note (2026-08-13).** The evidence in Context was read on 2026-08-12 and is kept as
> the record this decision was made against. Two citations have since moved: `adapters/REGISTRY.md`
> renamed its `Interface / class` column to **Robot interface** and its `Protocol` column to
> **Contract version**, and the line numbers shifted. The observation the citations support — that
> the external standard was recorded as prose beside a column named for the Swarmada contract — was
> accurate when written and is what the decision rests on.

## Decision

Introduce a **protocol profile**: a declared, versioned statement of which external
standard a Fleet Adapter speaks. It is recorded, not enforced.

1. **Name.** The southbound axis is a *protocol profile*. The northbound seam reserved
   in the Fleet Adapter protocol chapter is renamed from *capability profile* to
   **capability set**. The *rename* is free — the seam is reserved and unimplemented, and the
   phrase "capability profile" occurred once in normative text. The *name* is not free of
   collision, which this ADR originally implied: "capability set" already appears in ordinary
   English in six places, describing the set of capabilities a robot advertises. That is
   resolved in `terminology.md`, whose entry states that the term divides the contract surface
   and that the phrase "a robot's capability set" elsewhere is ordinary English and not the
   defined term — not by avoiding the name. _(Editorial correction, 2026-08-13: the decision is
   unchanged; the claim that the name was collision-free was wrong when written.)_ The bare word
   "profile" is not used in normative text; it is always qualified.

2. **Terminology.** `terminology.md` gains an entry defining *protocol profile*
   (southbound, an external standard), *capability set* (northbound, a subset of the
   Swarmada contract, reserved), and noting RFC-0004's distinct usage. The same entry
   disambiguates *protocol*, which already means the Swarmada contract in
   `protocolVersion` and `negotiatedProtocolVersion`; a protocol profile never means the
   Swarmada contract, and the qualifier is part of the term.

3. **Where it is declared — on both sides of the adapter boundary, because they are two
   different statements.**
   - On **`FleetAdapter`**, optional and repeated, in both `spec` and `status`,
     mirroring the existing `spec.protocolVersion` / `status.negotiatedProtocolVersion`
     shape. The `spec` value is what an operator expects the process to speak; the
     `status` value is what the adapter declared at handshake. Each entry carries an
     identifier and a version — for example `vda5050` at `3.0.0`.
   - On **`RobotClass`**, in `spec`, beside `baseAdapter:89`: the protocol robots of
     this class speak. A protocol is a property of a robot model, not of an individual
     machine, which is the same argument that puts `hardware:98` and
     `baseCapabilities:112` on the class; robots inherit it through the existing
     admission-time merge, so no per-`Robot` field is introduced.
   - `FleetAdapter.spec.servesRobotClasses` is already the relation between the two, so
     the comparison has a home. Nothing in the control plane performs it.

   A protocol profile is **not** a capability and is not declared in
   `spec.baseCapabilities`. Every field on a class capability is inapplicable to a wire
   protocol: `type:310` admits only `hardware-native`, `model-driven` or `manual`;
   `pauseable:316`, `requiredHardware:324` and `providingModel:329` have no meaning for
   a protocol; and a capability's defining property is that it is active or inactive as
   a function of hardware health (RFC-0001 §6.10.1), which a protocol never is.
   Capabilities state what a robot can do. A protocol profile states what language it is
   addressed in.

4. **Status is written from the report, not from the wire.** A profile declaration is
   self-asserted by the adapter, which is a trust boundary. `status` follows the
   discipline already applied to `status.supportedActions` and
   `status.conformanceContractVersion`: written transition-only, never on a telemetry
   tick (RA-1).

5. **Recording and enforcement stay separate concerns**, as ADR-0032 already states for
   `conformanceContractVersion` (RFC-0001 §9.1.13). Specifically:
   - **Dispatch is never gated on a protocol profile.** Following ADR-0019
     (RFC-0001 §9.1.13), a declaration may inform or pre-filter and must never
     cause a wrong dispatch or a silent non-dispatch.
   - **Admission is not gated on it.** The admission gate
     (RFC-0001 §9.1.13) is scalar and fail-closed; a scalar condition cannot
     represent an adapter declaring two profiles of which one passes and one fails.
   - **No condition, no event, no status message is raised on a `spec`/`status`
     mismatch.** The two values are recorded and may be compared by a reader or an
     external tool. Nothing in the control plane acts on the comparison.

6. **Enforcement is out of scope, not only deferred.** No enforcement behaviour of any
   kind is specified by this decision, and none is planned for a named release. Both
   sides of the comparison exist under this decision, so the barrier is no longer
   structural — the comparison is possible and deliberately not built. The case that
   would motivate reopening the question is version drift: VDA 5050 moved 2.1.0 → 3.0.0
   in March 2026 with breaking changes across a topic tree keyed on the major version,
   so an adapter implementing 3.0.0 against robots declared as speaking 2.1.0 fails
   silently. Reopening it should also wait until at least two profiles exist to disagree
   with each other.

## Alternatives considered

- **Record only, permanently — no condition, no future enforcement.** Rejected: it
  forgoes the version-drift case, which is the one place a declaration would earn its
  keep, and a field nothing ever reads invites the question of why it exists.

- **Gate dispatch on the profile.** Rejected: there is nothing to match against, because
  no `Robot` declares a protocol. Gating on an unpopulated field is also a known failure
  shape in this codebase — a control plane that stops dispatching without saying why.

- **Gate admission on the profile in v1.** Rejected: the gate is scalar and fail-closed
  and cannot express a partially-passing multi-profile adapter. Record first; gate when
  there is something to gate on.

- **An operator policy knob (`Off` | `Warn` | `Enforce`).** Rejected as premature: a
  policy switch for behaviour that has never run once triples the states the
  specification must describe.

- **A profile-registry custom resource.** Rejected: the discovery surface is a `status`
  projection under the ADR-0019 discipline, not a new cluster-scoped resource type.

- **Declare it on `Robot` rather than `RobotClass`.** Rejected: the protocol a machine
  speaks is a property of the model, and per-instance declaration invites drift between
  robots that are by construction identical. The class→robot merge already distributes
  class facts to instances.

- **Declare it as an entry in `spec.baseCapabilities`.** Rejected: a capability carries
  activation semantics — kind, `pauseable`, `requiredHardware`, `providingModel`, and an
  active/inactive state derived from hardware health — none of which a wire protocol
  has. It would be a permanent special case inside a shipped mechanism.

- **Nest it under `spec.baseAdapter`.** Rejected on cardinality: a class speaks its
  protocol whether or not that particular adapter build is the one deployed, and one
  adapter may speak several protocols.

- **Name it `comms profile`.** Rejected: it is informal for normative text, and it
  appears only as a CLI flag in `docs/adapters.md`, on a binary not present in
  this repository.

- **Name it `protocol binding`.** Rejected: `binding` is load-bearing in two other
  senses — `AdapterRef:116` binds an adapter to a robot, and conformance results
  describe a *simulated binding*. It would trade one collision for a worse one.

- **Name it `dialect`.** Rejected: a dialect is a variant of one language, and these are
  different languages. The MAVLink precedent for the word would mislead.

- **Name it `interop profile`.** Considered, on the ground that `protocol profile` sits
  beside `protocolVersion` where "protocol" means the Swarmada contract. Rejected in
  favour of the more conventional term; the residual collision is handled by the
  terminology entry above and by the rule that the qualifier is never dropped.

## Consequences

**What becomes possible.** A claim about an external standard acquires a shape: an
identifier, a version, and a place to record what verified it. A third party can state
what their own adapter implements in a vocabulary this project understands, which is a
precondition for conformance claims that do not route through this repository's own
adapters. Version drift becomes expressible, and later detectable.

**New obligations.**

- `terminology.md` must be kept accurate as the three senses evolve; this ADR is the
  reason that entry exists. It must also separate the declarations that now sit side by
  side on `RobotClass` — *hardware component* (present or absent), *capability* (active
  or inactive, as a function of hardware health), *model* (deployed or not), *adapter
  binding* (connected or not) and *protocol profile* (declared or not) — since a reader
  who conflates any two of them will place a field in the wrong one.
- The `Interface / class` column in `adapters/REGISTRY.md` becomes a profile identifier
  rather than free text, and the registry must not pair the word CONFORMANT with the
  name of an external standard, since the C0–C16 catalog asserts on the northbound
  surface only.
- CRD regeneration, and the API review that `docs/api-principles.md` requires for any
  new field.
- The rename in the Fleet Adapter protocol chapter must land before anything is built
  against that seam; after that it costs a migration. **Landed — see the implementation note.**

**Drawbacks accepted.** One more noun in the specification. A declared field that
nothing enforces in v1, whose value is documentary until the robot-side expectation
exists. And a name that shares a word with `protocolVersion`, mitigated by terminology
rather than avoided.

**Left open, deliberately, and not decided here.** Whether a `RobotClass` may declare
more than one profile; whether `DiscoveredRobot` should carry an *observed* profile,
which would be better evidence than an operator's declaration; the semantics of a
per-profile capability ceiling and how a claim is checked against it; a per-robot
sourcing declaration, which is a different cardinality again from the per-class and
per-adapter declarations; and per-profile conformance selection and reporting in the
harness. None is a precondition for the declaration, and the last three want at least
two profiles in existence to be designed against.
