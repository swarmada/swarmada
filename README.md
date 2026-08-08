# Swarmada

**Kubernetes-native, vendor-neutral orchestration for fleets of physical robots.**

Swarmada coordinates fleets of heterogeneous robots the way Kubernetes coordinates
containers: you declare the desired state of the fleet as Kubernetes custom
resources, and Swarmada continuously reconciles the fleet toward it — across robots
from different manufacturers, under one standardized API.

> **Status:** Pre-release and under active development. See [Status](#status).

## Why Swarmada

A facility running automation today typically operates robots from three to five
vendors at once, each with its own dashboard, fleet API, and update toolchain — and
no single source of truth for fleet state. The robotics software stack has neutral,
open reference points for simulation, perception, and navigation. The **fleet
orchestration** layer — the tier that assigns and sequences work across a mixed
fleet — has no vendor-neutral, cloud-native open standard. Swarmada fills that gap.

## How it works

Operators declare intent as custom resources; controllers reconcile the fleet
toward it. A per-manufacturer **Fleet Adapter** translates one standardized gRPC
contract into each vendor's native API, so adding a robot family is an additive,
community-maintainable act rather than a change to the core.

| Kubernetes | Swarmada |
| :--- | :--- |
| Node | `Robot` / `RobotClass` |
| Pod / Deployment | `FleetTask` |
| Namespace | `FleetZone` |
| Rolling update | `FirmwareRollout` / `ModelRollout` |
| `kubectl` | `swarmctl` |

The full design is in [docs/architecture.md](docs/architecture.md); the normative
specification is [RFC-0001](rfcs/dist/RFC-0001-core-spec.md).

## Quick start

Prerequisites: Go 1.22+, `kubectl`, and a local cluster (`kind` or `minikube`).
See [CONTRIBUTING.md](CONTRIBUTING.md) for the full development environment,
including the simulation stack (ROS 2 Jazzy, NVIDIA Isaac Sim).

```bash
git clone https://github.com/swarmada/swarmada
cd swarmada

kind create cluster --name swarmada-dev
make install        # generate and apply the CRDs
make run            # run the control plane against your current kubecontext

# in another shell:
kubectl create namespace warehouse-a
kubectl apply -f config/samples/
kubectl get robots,fleettasks -n warehouse-a
```

Prefer `minikube`? The same steps work — just swap the cluster-creation line:

```bash
git clone https://github.com/swarmada/swarmada
cd swarmada

minikube start --profile swarmada-dev
make install        # generate and apply the CRDs
make run            # run the control plane against your current kubecontext

# in another shell:
kubectl create namespace warehouse-a
kubectl apply -f config/samples/
kubectl get robots,fleettasks -n warehouse-a
```

Two scenarios run end-to-end in simulation: discovery → admission → assignment, and
camera-fault → capability-degrade → task-reroute → recovery. The
[warehouse-quickstart](examples/warehouse-quickstart/README.md) packages these and
more behind one command — `make quickstart`, then pick a scenario (or pass
`--scenario NAME`): `healthy-fleet`, `battery-edge`, `battery-handoff`,
`hardware-fault`, `comms-flaky`, `estop-drill`, plus the coverage run
`full-surface`. [`tools/swarmtop`](tools/swarmtop/README.md) is a terminal fleet
inspector that renders the status fields `kubectl get` can't column-ize.

## Documentation

- [Architecture](docs/architecture.md) — the two-plane control/data model, the
  control-plane components, and the thirteen CRDs
- [Quickstart](docs/quickstart.md) — run the demos on a local `kind` cluster and
  the C0–C16 adapter conformance suite
- [API design principles](docs/api-principles.md) — CRD and protocol conventions
- [RFC-0001 overview](rfcs/RFC-0001-overview.md) — a 2–3 page reader's guide to the specification
- [RFC-0001](rfcs/dist/RFC-0001-core-spec.md) — the normative core specification
- [Fleet Adapter protocol](proto/fleet_adapter/v1/fleet_adapter.proto) — the
  vendor-neutral gRPC contract every adapter implements

## Status

> **Scope of this repository.** This repository contains the specification, the
> control-plane reference implementation, the `swarmctl` CLI, the Helm deployment
> chart, the Python SDK, the adapter conformance harness, the reference adapters, and
> the simulation-based demo scenarios. One component is maintained outside this
> repository: the edge Zone Controller node and its no-hardware demo.

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
  conformance harness (`adapters/conformance/`) and three reference adapters
  (per [ADR-0005](docs/adr/0005-reference-adapter-policy.md)) are in-tree under
  `adapters/external/`: ROS 2, VDA5050, and MAVLink. ROS 2 is event-driven and
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
