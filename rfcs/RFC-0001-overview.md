# RFC-0001 at a Glance
### A reader's overview of the Swarmada Core Specification

The full specification is [RFC-0001](dist/RFC-0001-core-spec.md), at
`swarmada-version: v0.4`, status Review. It is long by design — a normative document
covering the eighteen CRDs it specifies, a gRPC protocol, the control plane, the safety and security
models, and the conformance profiles an adapter result is reported against. This
overview is the ~10% that carries the argument, for reviewers, contributors, and
evaluators who want the shape before the detail. It tracks the assembled v0.4
document; where this overview and RFC-0001 differ, **RFC-0001 governs.**

## What Swarmada is

Swarmada is a vendor-neutral, Kubernetes-native control plane for orchestrating
fleets of heterogeneous physical robots. Operators declare the desired state of the
fleet as Kubernetes custom resources; Swarmada continuously reconciles the actual
fleet toward that declaration — coordinating robots from multiple manufacturers
under one standardized API. It is, deliberately, the Kubernetes model applied to
physical robot fleets.

## The gap it fills

The robotics software stack has neutral, open reference points at most layers:
physics simulation (Newton, contributed to the Linux Foundation), perception and
navigation (ROS 2, under the Open Source Robotics Foundation), and the
robot-to-fleet-manager link (VDA 5050, published by the VDA and VDMA). The
**fleet orchestration** layer — the tier that assigns and sequences work across a
mixed-vendor fleet — has open, foundation-hosted software in it: Open-RMF, hosted
by the Open Source Robotics Foundation and managed by the Open Source Robotics
Alliance since 2024. What the layer does not have is a declarative,
Kubernetes-native specification, or a versioned adapter contract with a published
conformance catalog against which an implementation can be measured. Those two
absences are what this RFC addresses.

A facility running automation increasingly operates robots from more than one
manufacturer, each with its own dashboard, fleet API, and update toolchain, and no
single source of truth for fleet state. Below a certain fleet size that is only
inconvenient; above it, it is operationally unsustainable.

## Why now

Two factors converge to make a neutral orchestration standard both viable and
urgent:

1. **Facility fleets increasingly span several vendors.** RFC-0001 states this as a
   premise rather than a sourced figure — see
   [*Why Now*](dist/RFC-0001-core-spec.md#motivation-why-now). Once a facility's
   fleet includes robots from more than one manufacturer, coordination requires a
   dedicated orchestration layer rather than manual, per-vendor operation.
2. **The layer has open software but no specification and no conformance oracle.**
   The robot fleet orchestration layer has open, foundation-hosted software in it,
   but no declarative, Kubernetes-native specification: fleet state is not
   expressible as API objects reconciled by controllers, and no protocol in the
   layer is versioned as a standalone contract an implementer can build against
   independently of any particular codebase. Nor does the layer have a conformance
   oracle — the interoperability standards below it publish schemas but operate no
   conformance scheme and recognise no certification body, and the one open project
   in the layer itself publishes no enumerated check catalog. A specification that
   publishes both a contract and the catalog that measures conformance to it is
   what would occupy this layer as an open standard rather than a proprietary one.

## Why a new project — and not Open RMF

This is the question a CNCF reviewer will ask first, and it deserves a complete,
honest answer.

Open-RMF is the closest existing project and a technically capable one, with real
deployments in logistics and healthcare. It is open source under Apache-2.0, its
intellectual property is held by the Open Source Robotics Foundation — an
independent non-profit — and since 2024 it has been managed by the Open Source
Robotics Alliance under a published project charter. Any argument that it is not
foundation-hosted, or that contributions to it accrue to a commercial vendor, is
incorrect and this RFC does not make one.

Two distinctions remain, and neither is about ownership:

- **Architecture.** Open-RMF predates cloud-native Kubernetes patterns and is
  primarily event-driven; it does not use CRDs, the operator pattern, Helm, or
  Prometheus natively. This specification is built on the reconciliation model —
  the reason the control plane keeps its desired state through connectivity
  loss, mid-task robot failure, and maintenance windows and re-evaluates it on
  every reconcile. Whether recovery then needs an operator is a per-namespace
  policy; under the default (`Never`) an operator cancels a disconnected
  robot's action. Grafting
  reconciliation onto an event-driven core would be a rewrite rather than an
  additive contribution. Open-RMF's own successor roadmap (2026-04-06)
  proposes a modular re-architecture whose module interfaces are, in its words,
  still to be defined; this RFC takes no position on that work.
- **Contract and conformance.** Open-RMF specifies a fleet adapter as a software
  interface within a reference implementation. It does not version that interface
  as a standalone contract, publish an enumerated conformance catalog, or operate a
  conformance program — and neither does any interoperability standard in this
  layer. This specification defines `fleet_adapter.v1` as a versioned contract with
  an RFC 2119 normative check catalog, and requires that every published result
  carry the class of evidence that produced it.

A third property — whether a project's maintainer base is drawn from more than one
employer — is treated in [*A.1 Extend Open-RMF*](dist/RFC-0001-core-spec.md#alternatives-a-1-extend-open-rmf),
under *Alternatives Considered*, together with this project's own
position on it.

**Coexistence, not displacement.** A robot running Open-RMF-based navigation can be
managed through a ROS 2 Fleet Adapter. The two projects address the same layer on
different substrates and with different conformance disciplines; operators can
choose on architecture, cloud infrastructure, and existing stack.

## The architecture in one picture

Swarmada separates fleet-wide coordination (the control plane) from per-robot
execution (the data plane).

```
CONTROL PLANE (cloud or on-prem Kubernetes cluster)
  API Server · Scheduler · Zone Controller · Estop Coordinator
  Traffic Deconfliction Engine · OTA / Model Update Manager
        │  gRPC (bidirectional streaming)
DATA PLANE (edge + on-robot compute)
  Fleet Adapter ──vendor protocol──▶ Robot Agent ──▶ robot
  Zone Controller (edge): local authority when the cloud is unreachable
```

Every component reacts to custom-resource state through the controller-runtime
reconciliation loop rather than calling other components directly. The per-vendor
**Fleet Adapter** — a single standardized gRPC contract — is the only place
vendor-specific code lives, which is what makes the standard genuinely neutral:
adding a robot family means implementing one protocol, not changing the core.

Full treatment: [docs/architecture.md](../docs/architecture.md).

## The API surface

Eighteen namespace-scoped CRDs under `swarmada.io/v1`, by lifecycle:

- **Discovery** — `DiscoveredRobot` (read-only; admitted by the operator)
- **Robots & templates** — `RobotClass`, `Robot`
- **Work & space** — `FleetAction`, `FleetTask`, `FleetZone`
- **Integration** — `FleetAdapter`
- **Health & config** — `RobotProbe`, `SwarmadaConfig`
- **Models & firmware** — `ModelPolicy`, `ModelRollout`, `FirmwareRollout`
- **Maintenance** — `ZoneMaintenance`
- **Non-robot nodes** — `EdgeNode`
- **Compute configuration** — `ComputeEndpoint`, `ComputeWorkloadTemplate`,
  `ResultSink`, `ArtifactStore`

`ModelRollout.spec.targetKind` selects between `Robot` and `EdgeNode` targets; the
`EdgeNode` kind is an edge or zone-controller node a model rollout is specified to
target; the reference controller refuses that target at v0.4. Every CRD in the tree has a chapter in this specification except `SemanticEvent`, `SemanticEventSource` and `TaskTrigger`, which RFC-0003 describes.

Design conventions: [docs/api-principles.md](../docs/api-principles.md).

## What Swarmada is not

The scope boundaries are as important as the scope, and a serious industrial or
healthcare evaluator will look for them:

- **Not motion control.** Swarmada is task-level orchestration on the order of
  seconds. Path planning, obstacle avoidance, and real-time motion stay in the
  robot's own controller (milliseconds).
- **Not a safety system.** The software emergency-stop protocol is not, and must
  never be treated as, a substitute for physical safety hardware — light curtains,
  safety PLCs, functional-safety controls. Those belong to the robot platform.
- **Not the commercial product.** A hosted or managed service is a separate product
  and is explicitly out of scope for this specification.
- **Out of scope.** Multi-site federation is not part of this specification.
  Multi-robot *fan-out* is in this document — a composite `FleetTask` schedules its member
  actions across robots — but multi-robot *coordination* is not: RFC-0001 gives no guarantee
  that members begin together or hold a shared resource as a unit, and the synchronized-start
  barrier is Non-Goal #15.

## Honesty about maturity

RFC-0001 includes a Drawbacks section enumerating eighteen named limitations — the
opaque task payload, the coarse eventually-consistent status, the unbounded silent-
failure window left by an unimplemented capability rescan, and others. Individual chapters carry their
own maturity notes marking what is specified and not implemented, and several of the
v0.4 additions below are in that state.

Implementation status: all eighteen CRDs this specification defines are generated, and
thirteen of them are reconciled by controllers; the remaining five are not, and the reason differs
between them. (The `swarmada.io/v1` API group also serves `SemanticEvent`, `SemanticEventSource`
and `TaskTrigger`, which RFC-0001 v0.4 does not specify and which these counts exclude. They
are specified by RFC-0003, which is their owner and which is a draft published alongside this specification for review and not part of this specification's release or contract.)
The four compute configuration resources (`ComputeEndpoint`, `ComputeWorkloadTemplate`,
`ResultSink`, `ArtifactStore`) are reference-only by design — resolved where they are used
rather than reconciled. `EdgeNode` is not: it is specified ahead of its controller, no
shipped code path reads an `EdgeNode` object, and nothing writes any field of its `status`.
Each chapter's maturity note states what is and is not wired at v0.4. The Traffic Deconfliction Engine and the
`ControlStream`/`SafetyStream` wire are live; and the executable conformance catalog `C0`–`C19` covers the protocol
surface — 75 checks, of which the harness runs 71 (`C8.4` is an obligation on the edge
node, so no adapter result can pass or fail it; `C3.10`, `C3.11` and `C16.2` are defined; this release does not run them) and 67 count toward conformance (`C0.2`–`C0.4` record harness context, and `C6.3` is graded on the control plane as `CP8.1`). `C18` is the sensor-stream duty cycle, added
2026-09-14: it is the first family to close the frames-never-on-the-control-plane rule
and the lease-bound stop, both of which the wire contract had stated as normative and
disclosed as unchecked. The in-tree simulation adapter and the ROS 2,
VDA5050, and MAVLink reference adapters are each recorded `partial` in
[`adapters/REGISTRY.md`](../adapters/REGISTRY.md), with optional
commands declined, and every result earned against a simulated binding. The three external
adapters record contract `1.0.0`, outside the supported range, so their robots are refused
registration and admission. RFC-0001's Not-met notes record where the simulation adapter does
not hold a stop. No adapter is
recorded `passing`, and no physical-hardware validation has been done. Two scenarios
run end-to-end in simulation — discovery → admission → assignment, and camera-fault →
capability-degrade → task-reroute → recovery. (Where this summary and RFC-0001's maturity
and Not-met notes differ, RFC-0001 governs.)

## What v0.4 adds

Five of the additions since v0.3 are listed below, all in the Detailed Specification. Each is normative text;
each carries a maturity note recording how much of it the reference implementation
runs today, which for most of this list is none of it.

- **A typed `FleetAction.status.result`** — an optional, size-bounded outcome
  (`schemaRef`, scalar `values`, `artifacts[]` carried as references, and `producedBy`
  identifying what produced the result). Advisory for consumers by design: detail
  fidelity is a property of the adapter and its platform version, not of Swarmada.
- **Directive supersession** — when a new directive for an already-executing robot
  preempts in place (same action type) and when it requires an explicit, confirmed
  cancel first (type change), with the observable state sequence for each and the
  requirement that a supersession never be recorded as an operator cancellation.
- **Optional zone geometry and composite zones** — a `FleetZone` is valid with
  identity alone, so an adapter that cannot export a map is not thereby second-class;
  composite zones group other zones by name so a task can target a floor once. Zone
  extent is normatively two-dimensional, and the consequence for overlapping air and
  ground footprints is stated rather than left to inference.
- **`ModelRollout` targeting beyond robots** — `spec.targetKind` selects `Robot` or
  `EdgeNode`, so a detection cascade running on a zone-controller box is
  specifiable. Robot targeting is unchanged and the quality gates apply identically
  to both.
- **Conformance profiles, support levels, and capability tiers** — the `C0`–`C16`
  and `C19` checks are partitioned into eleven named profiles, with the `C17`, `C18` and
  reactive-block families carried by four more; each check is Core or Extended;
  Read Only / Traffic Light / Full Control select which profiles an adapter owes; and
  a machine-readable `ConformanceReport` carries the result. Conformance is
  self-certified, with no fee, no gate, and no certificate. The safety profile is
  Core at every tier.

v0.4 also adds five resource kinds (`EdgeNode`, `ComputeEndpoint`, `ComputeWorkloadTemplate`,
`ResultSink` and `ArtifactStore`), the wire additions of contract `1.4.0` (listed once, under
*Current version* in the Fleet Adapter Protocol's versioning section), and two amendments on zones
accepted for v0.4, for zone restrictions and excluded regions. They are normative for v0.4 and published
as separate files, and the next revision folds them into the chapters.

## The contract is frozen at RFC-0001 v1.0

The freeze covers the fleet-adapter wire contract version and the CRD API versions published with
RFC-0001 v1.0. It does not mean wire contract `1.0`. Until then, `0.x` revisions keep refining the
contract, and each keeps changes to existing contracts to a minimum.

Changes to the proto, a CRD's
rules and semantics, or the normative text go through the contract-change process. Each is a new
contract version, with release notes and migration notes. A change that does not touch the wire is a
patch bump by default, and the supported range stays where it is.

A change that binds adapters is a minor bump, and the supported range moves. A change binds adapters if it adds, changes or removes an obligation for adapters. A conformance check that tests an obligation already stated moves the suite revision only. The contract version does not change. A change that
binds adapters for safety also raises the lowest supported contract version to the new one.

CRD versions follow Kubernetes API rules. The CRDs are served as `swarmada.io/v1`, and within `v1`
only backward-compatible changes are made, such as a new optional field. An incompatible schema
change introduces a new API version (`v2`), served alongside `v1` for at least one revision, with
conversion and a migration note. A resource's `stage:` describes its maturity and semantics, not its
schema compatibility.

Until v1.0, `stage:` governs what may change, and each revision states which resources changed stage.
On `alpha`, rules and semantics change only through the contract-change process, and a CRD never takes an
incompatible schema change within `v1`. On `beta`, a resource changes by addition, and any other change
needs a deprecation announced at least one revision ahead.

A `stable` resource does not change before
the next major revision. At RFC-0001 v1.0 the core resources are declared `stable` in `v1`. The core
resources are the thirteen original kinds: `Robot`, `RobotClass`, `DiscoveredRobot`, `FleetAction`, `FleetTask`, `FleetZone`, `FleetAdapter`, `RobotProbe`, `SwarmadaConfig`, `ModelPolicy`, `ModelRollout`, `FirmwareRollout` and `ZoneMaintenance`.

At every stage, a change to a safety rule (stops, the action table, the robot stop table, the
invariants) goes through the contract-change process. The same version updates the conformance suite.
A change that weakens a guarantee says so first, and every unmet rule keeps its Not-met note.

The v1 proto carries the
wire additions of contract `1.4.0` (listed once, under *Current version* in the Fleet Adapter
Protocol's versioning section). Contract `1.4.0` is published with this draft revision (v0.4). The `Robot` CRD carries the one status
addition RFC-0001 specifies with them (`status.stops`), beside the two projections the zone-restrictions amendment states. RFC-0001 also specifies one `Robot`
annotation with them (`swarmada.io/estop-attestation`, an operator's attestation of the check at
the robot), which the reference implementation does not read. The reference control plane does not
act on every `1.4.0` addition, and the deployments in this repository run the simulation adapter at
contract `1.3.0`; RFC-0001's Not-met notes say which additions are not acted on. Normative
text is every sentence with an RFC 2119 keyword wherever it stands, maturity notes
included, every schema block, every state or rule table such a sentence cites, the
RBAC matrix, and the two amendments accepted for v0.4. The rest of maturity notes, examples,
citations and errata are informative, so a later release can still record a gap
as closed. At v0.4 the fleet-adapter contract is `1.4.0`, and its supported range is
`>=1.3.0 <1.5.0`.
RFC-0001's [Resource model](dist/RFC-0001-core-spec.md#resource-model) states the
policy in full.

## Read further

- [RFC-0001 — Core Specification](dist/RFC-0001-core-spec.md) — the normative document
- [Architecture](../docs/architecture.md) — the control plane in depth
- [API design principles](../docs/api-principles.md) — CRD and protocol conventions
- [Fleet Adapter protocol](../proto/fleet_adapter/v1/fleet_adapter.proto) — the vendor-neutral gRPC contract
