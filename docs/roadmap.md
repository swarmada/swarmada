# Swarmada Roadmap

This roadmap describes the functional scope of the Swarmada standard and its reference
implementation. It is indicative and changes as the specification and implementation
change. The normative API surface is defined by RFC-0001, the `swarmada.io` Custom
Resource Definitions under `api/v1`, and the `fleet_adapter.v1` gRPC contract.
Per-resource stability and implementation status are graded in
[RFC-0001, Resource model](../rfcs/dist/RFC-0001-core-spec.md#resource-model).

## How to read this

- **Shipped (alpha)**: runs in the reference implementation. Each entry carries its
  RFC-0001 maturity grade as `control-plane / end-to-end`. `partial` means some of the
  specified behaviour is missing; the RFC names what. Nothing here is stable: every
  resource is `stage: alpha`.
- **Not implemented**: no running implementation, whether or not RFC-0001 specifies it.
- What a first deployment should not assume is listed in
  [Known limitations](known-limitations.md).

## Adapters

A robot family is integrated through a Fleet Adapter that implements the
`fleet_adapter.v1` gRPC contract. Adapter coverage is verified by the in-tree
conformance suite; not every adapter implements every function, and the suite reports
per-adapter coverage. Reference and community adapter families:

| Adapter | Robot domain |
| :-- | :-- |
| `simulation` | reference / conformance backbone |
| `vda5050` | VDA5050-compliant AMRs |
| `ros2` | ROS 2 / Nav2 robots |
| `mavlink` | MAVLink (aerial) |

The `simulation` adapter is in this repository. The other three are maintained in their
own repositories; see [Installing and running Fleet Adapters](adapters.md).

## Shipped (alpha)

### Discovery, registration and health

- **Discovery and admission** (partial / partial). Two-phase provisioning: a Fleet
  Adapter announces a robot (`DiscoveredRobot`); an operator admits it to a schedulable
  `Robot` with `swarmctl admit`, or rejects it. `RobotClass` templates (partial /
  partial) supply shared hardware, model, and capability defaults.
- **Registration and reconnect** (Fleet Adapter protocol: implemented / partial). A known
  robot reconnects and resynchronises its authoritative task state.
- **Health monitoring and telemetry** (`Robot`: partial / partial). Heartbeat and
  telemetry flow over the adapter streams; derived state is projected onto
  `Robot.status`; capability health is computed from hardware and model status.
  Position reaches zone derivation and the telemetry sink, not `Robot.status.position`.
- **Inspection.** `swarmctl get` and `describe`, the `swarmtop` terminal view, and a
  Prometheus metrics contract.
- **Transport security and RBAC** (implemented / partial). Mutual TLS for adapter
  connections, per-message `robot_id` authorization, and six built-in RBAC roles.

### Tasks, zones and safety

- **Task orchestration** (`FleetTask`, `FleetAction`: partial / partial). Declarative
  `FleetTask` creation, scheduling and assignment, status tracking, confirmed
  cancellation, and preemption.
- **Zones and traffic deconfliction** (`FleetZone`, TDE: partial / partial).
  Hierarchical `FleetZone` tree, zone-membership derivation from position, and a
  blocking gate on zone capacity before every zoned assignment. Shared resources are
  reserved only on an adapter's request; the scheduler does not request them.
- **Emergency stop** (partial / partial). Confirmed zone-wide stop and clear with
  hierarchical propagation (`swarmctl estop trigger`, `swarmctl estop-clear`); robot and
  namespace scope, triggered by the `swarmada.io/estop-triggered` annotation on a
  `Robot` or on the namespace's `SwarmadaConfig` (no `swarmctl` verb for either scope);
  and edge/headless stop during control-plane connectivity loss, through the Zone
  Controller edge node (`cmd/edge`). RFC-0001 has an edge stop latch at the robot, cleared
  only by a reset there. At v0.4 no adapter latches or reports it, and the control
  plane does not withhold the robot from dispatch ([known limitations](known-limitations.md#an-edge-estop-is-invisible-to-the-control-plane)).
- **Planned maintenance** (`ZoneMaintenance`: partial / partial). Graceful and immediate
  windows that take robots out of scheduling without safety escalation. The robots are
  not sent a pause command.
- **Active verification** (`RobotProbe`: partial / partial). Hardware, capability, and
  model probes.
- **Safety audit log.** A tamper-evident, hash-chained log, read, verified and exported
  with `swarmctl get audit`, `swarmctl verify audit` and `swarmctl export audit` from an
  exported chain.

### Updates, models and configuration

- **Firmware / OTA** (`FirmwareRollout`: partial / partial). Staged batches, safety
  constraints, and signature and checksum verification by the control plane, failing
  closed.
- **AI model lifecycle** (`ModelRollout`, `ModelPolicy`: partial / partial).
  Quality-gated deployment with capability gating on model status. The model's
  signature is verified by the adapter, not the control plane.
- **Configuration surface** (`SwarmadaConfig`: implemented / partial). Namespace
  defaults, auto-created on a namespace's first `FleetZone`. Some fields are validated
  and read by no controller; the RFC lists them.
- **Coordinate system.** The `SwarmadaConfig` coordinate schema, including WGS84 and
  AGL/MSL altitude reference, is validated at admission and stamped onto each `Robot` as
  annotations. The control plane does not transform coordinates.

### Experimental

- **TaskTrigger.** On a matching `SemanticEvent`, creates a `FleetTask` held at phase
  `Draft` until a person clears `spec.draft`. This surface is experimental and has no
  RFC-0001 maturity grade.

## Not implemented

- **Read-only web dashboard.** No fleet, robot, and adapter web view is in this
  repository; see [dashboard tools](dashboard-tools.md) for general-purpose options.
- **Multi-replica command delivery.** The chart refuses more than one manager replica
  while `ControlStream` is enabled.
- **Server-side audit read API and audit retention.** The audit commands read an
  exported chain; archival and age-based deletion are specified, not implemented.
- **EdgeNode reconciliation** (none / none), including `ModelRollout` with
  `targetKind: EdgeNode`.
- **Compute surface.** `ComputeEndpoint` (partial / none), `ComputeWorkloadTemplate`
  (none / none), `ResultSink` (partial / none), `ArtifactStore` (none / none).
- **Conformance profiles acted on by the control plane** (not implemented / not
  implemented). The suite reports profile results; nothing consumes them.
- **Sensor streams** (partial / none). Declaration and projection only.
- **End-to-end aerial operation.** Geodetic positions are not consumed by any
  control-plane path.
- **Keyless signing.** Cosign keyless (Fulcio certificate identity) verification.

## Status and normativity

This roadmap is not normative. The authoritative definitions are RFC-0001, the CRD
schemas under `api/v1`, and the `fleet_adapter.v1` proto. Where this roadmap and the
specification differ, the specification governs.
