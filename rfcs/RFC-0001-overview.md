# RFC-0001 at a Glance
### A reader's overview of the Swarmada Core Specification

The full specification is [RFC-0001](dist/RFC-0001-core-spec.md). It is long by
design — a normative document covering thirteen CRDs, a gRPC protocol, the control
plane, and the safety and security models. This overview is the ~10% that carries
the argument, for reviewers, contributors, and evaluators who want the shape before
the detail. Where this overview and RFC-0001 differ, **RFC-0001 governs.**

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

A facility running automation today typically operates robots from three to five
manufacturers at once, each with its own dashboard, fleet API, and update toolchain,
and no single source of truth for fleet state. Below a certain fleet size that is
only inconvenient; above it, it is operationally unsustainable.

## Why now

Three factors converge to make a neutral orchestration standard both viable and
urgent:

1. **Fleet sizes crossed the coordination threshold.** The median new AMR deployment
   grew from about 15 robots in 2024 to roughly 35 per facility in 2026. At 35-plus
   robots across three to five vendors, manual coordination stops working and a
   software orchestration layer becomes a requirement, not an optimization.
2. **Per-robot budgets are legible.** Robots-as-a-Service already establishes a
   recurring per-robot line item, so a neutral orchestration layer at a small
   fraction of that spend is arithmetically justifiable against an existing budget
   line.
3. **The cloud-native layer is unoccupied, and no test oracle exists anywhere in
   it.** There is no CNCF-hosted, declarative, cloud-native fleet orchestration
   standard. Separately, no interoperability standard in this field operates a
   conformance scheme: VDA 5050 and the MassRobotics AMR Interoperability Standard
   publish schemas but no test suite and no certification body, ISO 21423 is not
   yet published, and Open-RMF is a reference implementation rather than a
   standard. A specification that publishes both a contract and the catalog that
   measures conformance to it is what would occupy this layer as an open standard
   rather than a proprietary one.

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
  the reason the control plane recovers from connectivity loss, mid-task robot
  failure, and maintenance windows without operator intervention. Grafting
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
employer — is treated in {{ref:alternatives}} §A.1, together with this project's own
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
  API Server · Scheduler · Health Monitor · Zone Controller
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

Thirteen namespace-scoped CRDs under `swarmada.io/v1`, by lifecycle:

- **Discovery** — `DiscoveredRobot` (read-only; admitted by the operator)
- **Robots & templates** — `RobotClass`, `Robot`
- **Work & space** — `FleetAction`, `FleetTask`, `FleetZone`
- **Integration** — `FleetAdapter`
- **Health & config** — `RobotProbe`, `SwarmadaConfig`
- **Models & firmware** — `ModelPolicy`, `ModelRollout`, `FirmwareRollout`
- **Maintenance** — `ZoneMaintenance`

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
- **Deferred, not omitted.** Multi-site federation is scoped to a future RFC (RFC-0002); multi-robot
  coordination is specified in this document as the composite `FleetTask`.

## Honesty about maturity

RFC-0001 includes a candid Drawbacks section enumerating twelve named limitations —
the opaque task payload, the coarse eventually-consistent status, the bounded silent-
failure window of interval-based probing, and others. This overview does not hide
them; the willingness to state them is part of the case that the design is real.

Implementation status: all thirteen CRDs are defined, generate, and are reconciled by
controllers; the Traffic Deconfliction Engine and the `ControlStream`/`SafetyStream`
wire are live; an executable conformance suite (C1–C8) covers the full protocol
surface, the in-tree simulation adapter is conformant against it, and the ROS 2,
VDA5050, and MAVLink reference adapters pass the same suite. Two scenarios run
end-to-end in simulation — discovery → admission → assignment, and camera-fault →
capability-degrade → task-reroute → recovery. (The authoritative, current status is
the Status section of the top-level `README.md`.)

## Read further

- [RFC-0001 — Core Specification](dist/RFC-0001-core-spec.md) — the normative document
- [Architecture](../docs/architecture.md) — the control plane in depth
- [API design principles](../docs/api-principles.md) — CRD and protocol conventions
- [Fleet Adapter protocol](../proto/fleet_adapter/v1/fleet_adapter.proto) — the vendor-neutral gRPC contract
