# Swarmada Architecture

Swarmada is a vendor-neutral, Kubernetes-native control plane for orchestrating
fleets of heterogeneous physical robots. Operators declare desired fleet state as
Kubernetes custom resources; Swarmada continuously reconciles the actual state of
the fleet toward that declaration — coordinating robots from multiple
manufacturers under one standardized API.

This document describes the system architecture and the reasoning behind it. The
normative specification is [RFC-0001](../rfcs/dist/RFC-0001-core-spec.md); where
the two differ, RFC-0001 governs.

## The problem

A facility running automation today typically deploys robots from three to five
manufacturers at once — picking AMRs from one vendor, autonomous forklifts from
another, inspection robots from a third. Each ships its own dashboard, its own
fleet API, and its own update toolchain. There is no single source of truth for
fleet state, no way to assign a task to whichever robot is best-suited regardless
of vendor, and no unified health or update path.

The robotics software stack has neutral, open reference points at most layers —
simulation, perception, navigation, application programming. The **fleet
orchestration** layer does not. Swarmada occupies that layer as a cloud-native
open standard: the coordination tier between a facility's task sources (a WMS, an
operator, a scheduler) and the robots that carry the work out.

## Design principles

1. **Declarative and reconciliation-based.** Operators declare *what* should be
   true; controllers work continuously to make reality match. This is the
   Kubernetes model, and it is the reason Swarmada converges after connectivity
   loss and mid-task failure with minimal operator intervention. Minimal, not
   none: admission, the `Manual` rollback default, and clearing an estop are
   deliberately operator-gated.
2. **Vendor-neutral by construction.** No vendor assumptions live in the core. A
   single gRPC contract — the Fleet Adapter protocol — is the only integration
   surface, and any manufacturer can implement it without a licensing
   relationship.
3. **Task-level orchestration, not motion control.** Swarmada assigns and
   sequences work at the granularity of seconds. Path planning, obstacle
   avoidance, and real-time motion stay in the robot's own controller. This
   boundary is deliberate and load-bearing (see [Safety boundary](#the-safety-boundary)).
4. **Cloud-native.** CRDs, the controller-runtime operator pattern, Prometheus
   metrics, and Helm packaging — so Swarmada composes with the infrastructure
   tooling operators already run.
5. **Fails safe under partition.** Edge components retain local authority when the
   control plane is unreachable, and reconciliation makes transient failures
   recoverable rather than incident-triggering.

## The Kubernetes analogy

Swarmada maps robot-fleet concepts onto Kubernetes primitives so that operators
familiar with Kubernetes can reason about a fleet with the same mental model.

| Kubernetes | Swarmada | Meaning |
| :--- | :--- | :--- |
| Node | `Robot` / `RobotClass` | A physical robot; a template for a robot type |
| Pod / Deployment | `FleetAction` / `FleetTask` | The atomic unit of dispatch; the composite objective that composes them |
| Namespace | `FleetZone` | A physical or administrative area of the facility |
| Health probe | `RobotProbe` | Active verification of a robot subsystem |
| Rolling update | `FirmwareRollout` / `ModelRollout` | Fleet-wide firmware or model rollout |
| Node admission | `DiscoveredRobot` → `Robot` | Two-phase discovery then operator admission |
| ConfigMap | `SwarmadaConfig` | Namespace-level tunables |
| `kubectl` | `swarmctl` | The command-line interface |

The mapping is a guide, not an equivalence — robots are stateful, physical, and
long-lived in ways containers are not, which is why the data plane below diverges
from the Kubernetes kubelet model.

## Two-plane architecture

Swarmada separates fleet-wide coordination (the **control plane**) from per-robot
execution (the **data plane**).

```
┌──────────────────────────── CONTROL PLANE  ────────────────────────────┐
│                  (cloud or on-premise Kubernetes cluster)              │
│                                                                        │
│   operator tooling ──REST──▶ API Server ──controller-runtime──▶        │
│   (swarmctl, WMS)                 │        Scheduler · Robot Ctrl      │
│                                   │        Zone Controller             │
│                                   │        Traffic Deconfliction       │
│                                   │        OTA / Model Update Manager  │
└───────────────────────────────────┼────────────────────────────────────┘
                                    │  gRPC (bidirectional streaming)
┌───────────────────────────────────┼──── DATA PLANE ────────────────────┐
│                                   ▼    (edge + on-robot compute)       │
│   Fleet Adapter  ──vendor protocol──▶  Robot Agent  ──▶ robot hardware │
│   Zone Controller (edge node): local enforcement, offline authority    │
└────────────────────────────────────────────────────────────────────────┘
```

### Control plane components

Each control-plane component has a single responsibility and reacts to custom
resource state through the controller-runtime reconciliation loop rather than
calling other components directly.

- **API Server** — exposes the REST and gRPC surface; validates and persists all
  custom resource mutations.
- **Scheduler** — selects the robot for each pending `FleetTask` by capability
  match, proximity, and load. The selection algorithm is pluggable; the interface
  is standardized, the policy is not.
- **Robot reconciler** — drives the robot lifecycle: capability derivation, the
  aggregate `status.health` summary, heartbeat-timeout offline detection and the
  prolonged-offline escalation. Together with the Probe Controller, the FleetAdapter
  controller and the telemetry pipeline it realises what RFC-0001 §9.3.3 groups as
  robot health and connectivity; there is no single "Health Monitor" component.
- **Zone Controller** — reconciles `FleetZone` resources and enforces zone-level
  capacity and access policy.
- **Traffic Deconfliction Engine** — a synchronous gate the Scheduler must clear
  before any assignment; enforces zone capacity and shared-resource reservations
  (corridors, charging docks, elevators).
- **OTA / Model Update Manager** — drives `FirmwareRollout` and `ModelRollout`
  resources, distributing artifacts under rolling-update and safety constraints.

### Data plane components

- **Robot Agent** — runs alongside a single robot; executes assigned tasks and
  reports telemetry.
- **Fleet Adapter** — the per-manufacturer translation layer. It implements the
  Fleet Adapter gRPC service and maps the standardized protocol onto one vendor's
  native API. This is the only place vendor-specific code lives.
- **Zone Controller (edge node)** — runs at the facility edge for low-latency
  decisions and retains local authority (including emergency-stop authority for
  its zone) when the control plane is unreachable.

### The reconciliation model

Every component follows the desired-state → observed-state pattern. An operator
declares a `FleetTask`; the relevant controller works continuously to make the
fleet match it. If a robot loses connectivity mid-task, its `FleetTask` stays in a
non-terminal state; when the controller's watch next fires, the task is requeued
and reassigned automatically. Transient hardware and network failures become
recoverable events rather than incidents requiring manual triage — the single
largest operational advantage of the model for physical fleets.

### Communication patterns

- **REST/HTTPS** — operator tooling (`swarmctl`, dashboards, WMS integrations)
  to the API Server. Optimized for human-driven and low-frequency access.
- **gRPC bidirectional streaming** — Fleet Adapters maintain persistent streams
  to the control plane for low-latency task dispatch and continuous telemetry
  without polling.
- **controller-runtime reconciliation** — all internal state transitions are
  driven by watch events, never by direct calls between control-plane components.

## The API surface: thirteen CRDs

Swarmada defines thirteen namespace-scoped custom resource definitions, grouped by
lifecycle.

**Discovery and admission**
- `DiscoveredRobot` — created automatically when a Fleet Adapter reports a robot
  with no existing `Robot` resource; read-only, admitted by the operator.

**Robots and templates**
- `RobotClass` — a template capturing the hardware, default models, capabilities,
  and operational defaults shared by every robot of one type.
- `Robot` — an individual admitted robot; inherits from a `RobotClass` or declares
  its hardware and capabilities inline.

**Work and space**
- `FleetAction` — the **atomic** unit of dispatch: one action, one robot. Carries the
  required capabilities, a target zone, a priority, and an opaque payload. This is what
  the Scheduler assigns, what the assignment lease and the single-executor guarantee are
  defined on, and what an adapter actually receives over the wire.
- `FleetTask` — the **composite** objective an upstream work order maps to, composing one
  or more `FleetAction`s via `ownerReferences`: a dependency graph, completion and failure
  policies, and saga compensation. A `FleetTask` is never dispatched directly; its actions
  are.
- `FleetZone` — a physical or administrative area, with hierarchical parent/child
  structure and shared-resource declarations.

**Integration**
- `FleetAdapter` — declares a vendor adapter, its endpoint, the classes it serves,
  and its conformance state. A robot is admissible only when a connected,
  conformant adapter serves its class.

**Health and configuration**
- `RobotProbe` — an active health check that verifies hardware, capability, or
  model status via the Fleet Adapter, distinct from passive telemetry.
- `SwarmadaConfig` — namespace-level tunables for health, scheduling,
  provisioning, and maintenance behaviour.

**Models and firmware**
- `ModelPolicy` — a quality gate between an AI training pipeline and deployment;
  evaluates reported metrics and creates a `ModelRollout` when the gate passes.
- `ModelRollout` — a staged rollout of an AI model, gating affected capabilities
  during the update.
- `FirmwareRollout` — a staged firmware rollout under rolling-update and safety
  constraints.

**Maintenance**
- `ZoneMaintenance` — a planned, reversible pause of a zone or namespace,
  semantically distinct from emergency stop.

## The Fleet Adapter protocol

The Fleet Adapter protocol is the boundary between the neutral core and
vendor-specific hardware. It is a gRPC service (`fleet_adapter.v1`) covering the
full robot lifecycle: discovery, registration, telemetry streaming, task
assignment and status, emergency stop, capability scanning, and active hardware
verification. All connections use mutual TLS and short-lived bearer tokens.

The control plane connects to each adapter; the adapter translates the
standardized calls into the manufacturer's native interface. Because every vendor
integration reduces to implementing this one contract, adding a robot family is an
additive, community-maintainable act rather than a change to the core. A
conformance suite gates whether an adapter may drive robots, so an unverified
adapter cannot silently control physical hardware.

## Telemetry and state: the two-plane data split

High-frequency robot telemetry — position, battery, raw sensor metrics — flows
through the telemetry pipeline into a time-series backend and is **never** written to
etcd at telemetry cadence. Only *material transitions* (a zone change, a
connectivity phase change, a capability health change, a battery-bucket crossing)
are patched onto a resource's status.

This split is deliberate. Writing every telemetry tick to etcd would make
control-plane write load scale with fleet size times telemetry rate, capping the
fleet the store can support. Keeping the fast path off etcd bounds write load to
transitions, so fleet size is limited by the physical facility, not by the
datastore. The accepted trade-off is that a resource's status is coarse and
eventually-consistent: `Robot` status reports a bucketed battery level and a
throttled last-known pose, and any tooling that needs live values queries the
time-series backend rather than the Kubernetes API.

## Reconciliation invariants (RA-N)

A few cross-cutting invariants recur throughout the design. Code and spec reference
them by a short label (`RA-N`) wherever a component must uphold one. They are the
non-negotiable rules the control plane is built around:

- **RA-1 — status-write discipline.** Never write a resource's `status` on a
  telemetry tick. Status is a throttled projection, written only on a *material
  transition* (see [Telemetry and state](#telemetry-and-state-the-two-plane-data-split)
  above); per-tick status writes are prohibited. This is what bounds control-plane
  write load to transitions rather than fleet-size × telemetry-rate.
- **RA-4 — single-executor guarantee.** A robot's assignment is never freed or
  reassigned on an ambiguous signal — a lost acknowledgement or an unreachable
  push. A task is released only when the robot is *provably* not executing it: the
  adapter confirms, or the assignment lease is provably dead. Freeing a robot on
  unreachability alone risks two executors for one physical task — the
  dual-execution hazard this invariant exists to exclude.
- **RA-6 — dedicated safety channel.** Emergency-stop is never multiplexed behind
  bulk telemetry on a shared stream, where estop latency would couple to telemetry
  congestion. Safety-critical signals travel on their own channel (the SafetyStream
  RPC), independent of the telemetry path.

## The safety boundary

Swarmada is task-level orchestration operating on the order of seconds. It does
**not** issue motion commands, plan paths, or perform real-time obstacle
avoidance — those remain the responsibility of the robot's onboard controller and
navigation stack, on the order of milliseconds.

Swarmada's emergency-stop protocol operates at the software layer: it propagates a
stop intent to adapters with acknowledgement requirements and timing bounds. It is
not, and must never be treated as, a substitute for physical safety hardware —
light curtains, safety PLCs, and the controls that meet functional-safety
standards. The physical stopping distance and any hard safety guarantee belong to
the robot platform.

## Deployment topologies

- **Developer / prototype** — all control-plane components on a single local
  Kubernetes cluster (e.g. minikube), with robots simulated (e.g. in Isaac Sim)
  and Fleet Adapters connecting over localhost.
- **Single-site production** — the control plane on an on-premise cluster, one
  Zone Controller edge node per physical zone, and Fleet Adapters on edge compute
  co-located with the robots.
- **Multi-site** — each site is a dedicated namespace with its own control-plane
  deployment and zone set. Cross-site task routing and federation are out of scope
  for the current specification and deferred to a future RFC.

## Implementation status

Swarmada is pre-release and evolving. Public claims track the code:

- **CRDs:** all thirteen are defined and generate under `swarmada.io/v1`.
- **Controllers:** every CRD has a reconciler registered with the manager
  (`cmd/manager/main.go`), each covered by `envtest` suites in
  `internal/controller/`. `Robot` and `FleetTask` are the most exercised paths
  (assignment, leasing, preemption, traffic-deconfliction gating, estop); the
  robot-class, discovery, adapter, probe, zone, zone-estop, maintenance,
  model-policy, model-rollout, firmware-rollout, and config controllers are
  implemented and under active hardening.
- **Adapters:** three reference adapters — ROS 2/Nav2, VDA5050, and MAVLink/PX4 —
  plus the in-tree simulation adapter run against the executable conformance
  harness (`adapters/conformance/`, run via `make conformance`). The harness
  covers checks C0–C16. The reference adapters are exercised against it through
  simulated bindings only; none has been run against a live runtime, and
  `adapters/REGISTRY.md` — the authoritative per-adapter record — grades all
  three `partial` as of this revision. Only a `passing` grade maps to
  `FleetAdapter.status.conformance: Passed`, which robot admission requires. The lease self-stop (C4.2) and the
  telemetry-path checks (C6.x) are asserted in the suite itself, not separately:
  an adapter reports the stop it performs, which is what makes the primary
  dual-execution safeguard observable rather than assumed. Bindings
  against a live runtime (a sourced ROS 2/Nav2 workspace, an MQTT broker, PX4
  SITL) are the remaining pieces that need real hardware or a full runtime.
- **Demos:** discover → admit → assign, and camera-fault → capability-degrade →
  task-reroute → recovery, both run end-to-end in simulation.

This document describes the architecture as designed; the normative behavior is
defined by [RFC-0001](../rfcs/dist/RFC-0001-core-spec.md), and the status list above
is the authoritative statement of what runs.

## Further reading

- [RFC-0001 — Core Specification](../rfcs/dist/RFC-0001-core-spec.md) (normative)
- [Fleet Adapter protocol](../proto/fleet_adapter/v1/fleet_adapter.proto) — the vendor-neutral gRPC contract
- [API design principles](api-principles.md) — CRD and API design conventions
