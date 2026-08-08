# Swarmada Roadmap

This roadmap describes the functional scope of the Swarmada standard and its reference
implementation, grouped by delivery phase. It is indicative and will change as the
specification and implementation evolve. The normative API surface is defined by
RFC-0001, the `swarmada.io` Custom Resource Definitions under `api/v1`, and the
`fleet_adapter.v1` gRPC contract; per-resource stability (`stage:`) and implementation
(`impl:`) status is tracked in RFC-0001 §5.2.

## How to read this

- **Now** — the current supported scope: what a first deployment can rely on.
- **Next** — the following band of functionality, in active development.
- **Later** — planned functionality that is specified or intended but not yet scheduled.

Phase labels describe *supported scope*, not a calendar. A resource may be defined in
the specification ahead of the phase in which its full behaviour becomes supported.

## Adapters

A robot family is integrated through a Fleet Adapter that implements the
`fleet_adapter.v1` gRPC contract. Adapter coverage is verified by the in-tree
conformance suite; not every adapter implements every function, and the suite reports
current per-adapter coverage. Reference and community adapter families:

| Adapter | Robot domain |
| :-- | :-- |
| `simulation` | reference / conformance backbone |
| `vda5050` | VDA5050-compliant AMRs |
| `ros2` | ROS 2 / Nav2 robots |
| `mavlink` | MAVLink (aerial) |

Adapter coverage broadens by phase: discovery and health first, then task and safety
functions, then update and model-lifecycle functions.

## Now — discovery and health

- **Discovery and admission.** Two-phase provisioning: a Fleet Adapter announces a robot
  (`DiscoveredRobot`); an operator admits it to a schedulable `Robot`, or rejects it.
  `RobotClass` templates supply shared hardware, model, and capability defaults.
- **Registration and reconnect.** A known robot reconnects and resynchronises its
  authoritative task state.
- **Health monitoring and telemetry.** Heartbeat and telemetry (position, battery,
  hardware) flow over the adapter streams; derived state is projected onto
  `Robot.status`; capability health is computed from hardware and model status.
- **Inspection.** Read access through `swarmctl` (`get` / `describe`), the `swarmtop`
  terminal view, and a Prometheus metrics contract.
- **Transport security.** Mutual TLS for adapter connections; Kubernetes RBAC roles for
  operator access.

## Next — tasks, zones, and safety

- **Task orchestration.** Declarative `FleetTask` creation, scheduling and assignment,
  status tracking, confirmed cancellation, and preemption.
- **Zones and traffic deconfliction.** Hierarchical `FleetZone` tree, zone-membership
  derivation from position, and a blocking reservation gate for zone capacity and shared
  resources.
- **Emergency stop (zone scope).** Confirmed zone-wide stop and clear, with hierarchical
  propagation.
- **Planned maintenance.** `ZoneMaintenance` windows (graceful and immediate) that pause
  and resume robots without triggering safety escalation.
- **Active verification.** `RobotProbe` checks that actively verify hardware, capability,
  and model health.
- **Audit.** Reading and integrity-verifying the tamper-evident safety audit log.
- **Read-only dashboard.** A fleet, robot, and adapter health view.

## Later — updates, models, and extended scope

- **Firmware / OTA.** `FirmwareRollout` with staged batches, safety constraints, and
  signature and checksum verification.
- **AI model lifecycle.** `ModelRollout` and `ModelPolicy` quality-gated deployment, with
  capability gating on model status.
- **Extended emergency stop.** Robot- and namespace-scoped stop, and edge / headless stop
  for operation during control-plane connectivity loss.
- **Configuration surface.** Full `SwarmadaConfig` namespace defaults.
- **Audit export.** Administrative export of the audit log.
- **Geodetic coordinates.** The `SwarmadaConfig` coordinate schema (WGS84 / AGL–MSL
  altitude reference) is defined for aerial fleets; end-to-end aerial support lands with
  the MAVLink task and zone path.

## Status and normativity

This roadmap is not normative. The authoritative definitions are RFC-0001, the CRD
schemas under `api/v1`, and the `fleet_adapter.v1` proto. Where this roadmap and the
specification differ, the specification governs.
