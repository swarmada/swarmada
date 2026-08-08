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
physics simulation (Newton, donated to the Linux Foundation), perception and
navigation (ROS 2), application programming (Intrinsic Flowstate), hardware and
model runtimes (NVIDIA Isaac / GR00T). The **fleet orchestration** layer — the tier
that assigns and sequences work across a mixed-vendor fleet — has no vendor-neutral,
cloud-native open standard. Swarmada occupies that layer.

A facility running automation today typically operates robots from three to five
manufacturers at once, each with its own dashboard, fleet API, and update toolchain,
and no single source of truth for fleet state. Below a certain fleet size that is
merely inconvenient; above it, it is operationally unsustainable.

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
3. **The neutral layer is unoccupied.** There is no CNCF-hosted, declarative,
   cloud-native fleet orchestration standard. If a vendor-neutral project does not
   occupy this space, a proprietary standard will.

## Why a new project — and not Open RMF

This is the question a CNCF reviewer will ask first, and it deserves a complete,
honest answer.

Open RMF is the closest existing project and a technically capable one, with real
deployments in logistics and healthcare. It is **not a neutral standard.** The
codebase originated in the entity (OSRC-SG) that Intrinsic — an Alphabet subsidiary
— acquired in December 2022, and Intrinsic moved fully under Google in
February 2026. The Open Source Robotics Alliance has provided genuine community governance
since 2024, but the codebase remains associated with a single large technology
vendor, which is the concern operators and manufacturers raise when evaluating
long-term neutrality.

Two distinctions matter:

- **Governance.** CNCF neutrality requires that no single vendor control a project's
  direction. A contribution to Open RMF becomes an asset under Google's stewardship
  regardless of the quality or volume of the contribution — governance structure is
  not something third-party contributors can change. Enterprise operators,
  particularly those on Azure or AWS, have raised explicit concerns about adopting a
  robot-fleet standard controlled by a competing cloud provider; an AWS-originated
  standard would face the same skepticism from the other direction. Neutrality is
  the property that makes a standard adoptable across the industry.
- **Architecture.** Open RMF predates cloud-native Kubernetes patterns and is
  primarily event-driven; it does not use CRDs, the operator pattern, Helm, or
  Prometheus natively. Swarmada is built on the reconciliation model — the reason it
  recovers from connectivity loss, mid-task robot failure, and maintenance windows
  without operator intervention. Grafting reconciliation onto an event-driven core
  would be a rewrite, not an additive contribution.

**Coexistence, not displacement.** A robot running Open RMF-based navigation can be
managed by Swarmada through a ROS 2 Fleet Adapter. The two projects address the same
layer under different governance models; operators can choose based on their
neutrality requirements, cloud preferences, and existing stack. Swarmada's goal is
to provide the neutral alternative that Open RMF structurally cannot be.

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
