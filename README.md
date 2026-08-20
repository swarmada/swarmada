# Swarmada

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-pre--release-orange.svg)](#status)
[![CI](https://github.com/swarmada/swarmada/actions/workflows/ci.yml/badge.svg)](https://github.com/swarmada/swarmada/actions/workflows/ci.yml)
[![DCO](https://github.com/swarmada/swarmada/actions/workflows/dco.yml/badge.svg)](https://github.com/swarmada/swarmada/actions/workflows/dco.yml)

**Kubernetes-native, vendor-neutral orchestration for fleets of physical robots.**

Swarmada coordinates fleets of heterogeneous robots the way Kubernetes coordinates
containers: you declare the desired state of the fleet as Kubernetes custom
resources, and Swarmada continuously reconciles the fleet toward it — across robots
from different manufacturers, under one standardized API.

> **Status:** Pre-release and under active development. All validation to date is in
> simulation; no physical hardware validation has been done. See [Status](#status).

## Why Swarmada

- **A facility running automation today typically operates robots from three to five
  vendors at once**, each with its own dashboard, fleet API, and update toolchain —
  and no single source of truth for fleet state.

- **The robotics software stack has neutral, open reference points for simulation,
  perception, and navigation.** The fleet orchestration layer has open,
  foundation-hosted software — principally Open-RMF, hosted by the Open Source
  Robotics Foundation and managed by the Open Source Robotics Alliance since 2024 —
  but no Kubernetes-native, declarative standard, and no versioned adapter contract
  with a published conformance catalog that a manufacturer can implement against and
  be measured on.

- **Swarmada specifies that contract and publishes that catalog.**
  [RFC-0001](rfcs/dist/RFC-0001-core-spec.md) is normative and the reference
  implementation follows it rather than defining it; the C0–C16 conformance suite is
  executable, and adapter conformance is reported on the `FleetAdapter` resource.

## How it works

Operators declare intent as custom resources; controllers reconcile the fleet
toward it. A per-manufacturer **Fleet Adapter** translates one standardized gRPC
contract into each vendor's native API, so adding a robot family is an additive,
community-maintainable act rather than a change to the core.

| Kubernetes | Swarmada |
| :--- | :--- |
| Node | `Robot` / `RobotClass` |
| Pod / Deployment | `FleetAction` / `FleetTask` |
| Namespace | `FleetZone` |
| Rolling update | `FirmwareRollout` / `ModelRollout` |
| `kubectl` | `swarmctl` |

A `FleetZone` is the unit of physical space and policy, a `Robot` is the unit of
capacity, and a `FleetTask` is a composite objective that decomposes into
`FleetAction`s — the atomic unit of dispatch. The task itself never crosses the
Fleet Adapter boundary; its actions do.

```yaml
apiVersion: swarmada.io/v1
kind: Robot
metadata:
  name: sim-robot-001
  namespace: warehouse-a
spec:
  manufacturer: SimBot
  model: SimBot-250
  adapter:
    name: sim-fleet-adapter
    version: "0.1.0"
  zone: warehouse-a
---
apiVersion: swarmada.io/v1
kind: FleetTask
metadata:
  name: receiving-round
  namespace: warehouse-a
spec:
  completionPolicy: All
  failurePolicy: FailFast
  desiredState: Running
  actions:
    - name: approach-dock
      action:
        type: Navigate
        zone: warehouse-a
        priority: High
        requiredCapabilities: [navigation]
    - name: inspect-dock
      dependsOn: [approach-dock]
      action:
        type: Navigate
        zone: warehouse-a
        priority: Normal
        requiredCapabilities: [navigation, camera_front]
```

An action names the capabilities it requires, never a robot. The `Robot` controller
derives capabilities from component health — hardware-native ones from healthy
physical components, model-driven ones from healthy components *and* an active
inference model — so a camera fault degrades the capability, and an in-flight
`FleetAction` whose required capability is lost is reassigned rather than stranded.
Before any assignment, the Traffic Deconfliction Engine gates on zone capacity and
shared-resource reservation; the scheduler does not transition an action to
`Assigned` without a grant.

Robots are not trusted on announcement. A Fleet Adapter's first announcement creates
a `DiscoveredRobot`; an operator promotes it with `swarmctl admit` before it can
receive work.

The full design is in [docs/architecture.md](docs/architecture.md); the normative
specification is [RFC-0001](rfcs/dist/RFC-0001-core-spec.md), which governs wherever
it and this document differ.

## Quick start

Install the control plane into an existing cluster with Helm:

```bash
helm install swarmada deploy/swarmada -n swarmada-system --create-namespace
```

Then declare a zone, admit a robot, and dispatch a task:

```bash
kubectl create namespace warehouse-a
kubectl apply -f config/samples/
swarmctl admit robot sim-robot-001 --zone warehouse-a
kubectl get robots,fleettasks -n warehouse-a
```

No hardware required. `make quickstart` brings the warehouse scenario up on `kind`
end-to-end; pass `SCENARIO=` to pick one — `healthy-fleet`, `battery-edge`,
`battery-handoff`, `hardware-fault`, `comms-flaky`, `estop-drill`, or the coverage
run `full-surface`:

```bash
make quickstart SCENARIO=hardware-fault
```

Two scenarios exercise the core loops end-to-end: discovery → admission →
assignment, and camera-fault → capability-degrade → task-reroute → recovery. The
[warehouse-quickstart](examples/warehouse-quickstart/README.md) packages these and
more. [`tools/swarmtop`](tools/swarmtop/README.md) is a terminal fleet inspector that
renders the status fields `kubectl get` can't column-ize.

To run the control plane from source instead — prerequisites are Go 1.22+,
`kubectl`, and a local cluster (`kind` or `minikube`); see
[CONTRIBUTING.md](CONTRIBUTING.md) for the full development environment, including
the simulation stack (ROS 2 Jazzy, NVIDIA Isaac Sim):

```bash
git clone https://github.com/swarmada/swarmada
cd swarmada

kind create cluster --name swarmada-dev    # or: minikube start --profile swarmada-dev
make install        # generate and apply the CRDs
make run            # run the control plane against your current kubecontext

# in another shell:
kubectl create namespace warehouse-a
kubectl apply -f config/samples/
kubectl get robots,fleettasks -n warehouse-a
```

## Documentation

- [Architecture](docs/architecture.md) — the two-plane control/data model, the
  control-plane components, and the thirteen CRDs
- [Quickstart](docs/quickstart.md) — run the demos on a local `kind` cluster and
  the C0–C16 adapter conformance suite
- [Adapters](docs/adapters.md) — writing a Fleet Adapter, and the reference set
- [API design principles](docs/api-principles.md) — CRD and protocol conventions
- [RFC-0001 overview](rfcs/RFC-0001-overview.md) — a 2–3 page reader's guide to the specification
- [RFC-0001](rfcs/dist/RFC-0001-core-spec.md) — the normative core specification
- [Fleet Adapter protocol](proto/fleet_adapter/v1/fleet_adapter.proto) — the
  vendor-neutral gRPC contract every adapter implements

## Status

> **Scope of this repository.** This repository contains the specification, the
> control-plane reference implementation, the `swarmctl` CLI, the Helm deployment
> chart, the Python SDK, the adapter conformance harness, the simulation adapter, and
> the simulation-based demo scenarios. Two things are maintained outside this
> repository: the edge Zone Controller node and its no-hardware demo, and the three
> reference Fleet Adapters, which `make adapters` clones from their own public
> repositories.

Swarmada is pre-release; public claims track the code:

- **API** — all thirteen CRDs are defined and generate under `swarmada.io/v1`.
- **Controllers** — reconciliation is implemented, wired into the manager, and
  tested for all thirteen CRDs: `Robot` (full capability derivation), `RobotClass`
  (referencing-robot count), `FleetAction` (the atomic unit of dispatch —
  assignment lease, estop-pause, preemption, and capability-loss reassignment of
  an in-flight action on a required-capability degradation), `FleetTask` (the
  composite objective — dependency graph, completion and failure policies, saga
  compensation), `DiscoveredRobot` (two-phase discovery with TTL/Stale sweep),
  `FleetZone` (zone topology + live position derivation), `SwarmadaConfig`
  (+ auto-bootstrap), `ModelRollout`, `FirmwareRollout` (fail-closed signature
  verification), `ModelPolicy`, `RobotProbe`, `FleetAdapter` (connectivity +
  conformance status), and `ZoneMaintenance` (maintenance-window lifecycle,
  graceful in-progress-task wind-down).
- **Traffic Deconfliction Engine** — implemented: zone-capacity gating, shared-resource
  reservation, and Critical-band preemption run as a blocking gate before every task
  assignment.
- **Control plane wire** — the `ControlStream` gRPC server is live, with per-message
  `robot_id` authorization, the six built-in RBAC roles, a tamper-evident
  hash-chained audit log, and confirmed-only emergency-stop delivery over
  `SafetyStream`. The Zone Controller edge node is implemented as a runnable,
  tested binary — mTLS-served `EdgeStream`, fail-safe config sync from
  `FleetZone`/`Robot`, a local safety-input hook, and a no-hardware demo — with
  its code maintained outside this repository (see Scope above). No physical
  hardware validation has been done.
- **Adapters** — the Fleet Adapter contract is defined by `proto/` and RFC-0001,
  and the in-tree simulation adapter drives the demo. An executable C0–C16
  conformance harness (`adapters/conformance/`) is in-tree, and three reference
  adapters (per [ADR-0005](docs/adr/0005-reference-adapter-policy.md)) are maintained
  in their own repositories — ROS 2, VDA5050, and MAVLink — and are cloned into
  `adapters/external/`, which is not checked in here (see
  [docs/adapters.md](docs/adapters.md)). ROS 2 is event-driven and
  VDA5050 is request/response, so the set spans two structurally different
  integration paradigms. All validation is in simulation; no physical hardware
  validation has been done.

The [architecture](docs/architecture.md) is documented as designed; this list is the
authoritative statement of what runs today.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, the DCO
sign-off requirement, and the RFC process. All participation is governed by the
[Code of Conduct](CODE_OF_CONDUCT.md); project governance is described in
[GOVERNANCE.md](GOVERNANCE.md); report security issues per [SECURITY.md](SECURITY.md).

## License

Apache 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
