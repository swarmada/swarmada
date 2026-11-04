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

> **Status:** Pre-release and under active development. Every conformance result to
> date was earned in simulation; none has been earned on physical hardware. See
> [Status](#status).

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
  implementation follows it rather than defining it; the C0–C19 conformance suite is
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
Before assigning an action that names a zone, the Traffic Deconfliction Engine gates
on that zone's capacity; the scheduler does not transition such an action to
`Assigned` without a grant. A shared resource declared on a zone is reserved only when
a Fleet Adapter requests it; the scheduler does not request one.

Robots are not trusted on announcement. A Fleet Adapter's first announcement creates
a `DiscoveredRobot`; an operator promotes it with `swarmctl admit` before it can
receive work.

The full design is in [docs/architecture.md](docs/architecture.md); the normative
specification is [RFC-0001](rfcs/dist/RFC-0001-core-spec.md), which governs wherever
it and this document differ.

## Non-Goals

Swarmada does not do motion control, does not replace physical safety hardware, is not
a hosted product, and does not define a scheduling algorithm — the full list (20 items)
is in [RFC-0001 §4](rfcs/dist/RFC-0001-core-spec.md#non-goals).

## Quick start

**Prerequisites:** Docker (running). See [docs/quickstart.md](docs/quickstart.md) for
the full per-OS list and `make setup-macos`.

No hardware required, no existing cluster needed. `make quickstart` creates its own
`kind` cluster and brings the warehouse scenario up end-to-end; pass `SCENARIO=` to
pick one — `healthy-fleet`, `battery-edge`,
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

The quickstart runs with the admission webhooks off, so that it can apply its robots
from a file. The posture a real install runs in has admission webhooks on and an
mTLS `ControlStream` that authorizes each robot's messages. To run that end to end:

```bash
make first-run
```

It creates a `kind` cluster, installs the Helm chart from source, starts a simulated
adapter in the cluster, admits the robot the adapter announces, and runs a
`FleetTask` to `Succeeded`. See [examples/first-run](examples/first-run/README.md).

### Using an existing cluster

Install the control plane with Helm. The chart's defaults serve the admission webhooks
and the `ControlStream` with cert-manager-issued certificates, so install
[cert-manager](https://cert-manager.io) first, or see
[deploy/swarmada/README.md](deploy/swarmada/README.md) for the alternatives. No manager
image is published; that README shows how to build and load one, and how to give an
adapter its client certificate:

```bash
helm install swarmada deploy/swarmada -n swarmada-system --create-namespace \
  --set image.repository=swarmada-controller --set image.tag=dev \
  --set 'controlStream.tls.certManager.adapterClients[0].name=sim-fleet-adapter' \
  --set 'controlStream.tls.certManager.adapterClients[0].namespace=warehouse-a'
```

With the webhooks serving, a `Robot` applied from a file is refused unless its
`FleetAdapter` is already `Connected` with conformance `Passed`, so a robot arrives
through discovery: its Fleet Adapter connects, a `DiscoveredRobot` appears, and an
operator admits it. [config/samples/first-run](config/samples/first-run/) declares the
configuration, the robot class and the `FleetAdapter` for that path, and
[examples/first-run/sim-adapter](examples/first-run/sim-adapter/kustomization.yaml)
runs the simulation adapter that connects as that `FleetAdapter`. Nothing connects
until an adapter runs, and no adapter image is published, so the block builds one and
loads it into the cluster (`kind load` shown; on another cluster, push it to a
registry the cluster pulls from). `swarmctl` is built from source into `bin/`. `helm
install` returns before the manager is ready, so the block waits for it first; for a few
seconds after that the webhook Service may still not route, and an `apply` refused with
`connection refused` in that window succeeds when run again:

```bash
kubectl -n swarmada-system rollout status deploy/swarmada-controller-manager --timeout=300s
kubectl create namespace warehouse-a
kubectl apply -k config/samples/first-run

docker build -f Dockerfile.sim-adapter -t swarmada-sim-adapter:dev .
kind load docker-image swarmada-sim-adapter:dev --name <your-kind-cluster>
kubectl apply -k examples/first-run/sim-adapter
kubectl -n swarmada-system rollout status deploy/sim-fleet-adapter --timeout=180s
kubectl -n warehouse-a wait fleetadapter/sim-fleet-adapter --timeout=180s \
  --for=jsonpath='{.status.conformance}'=Passed

make swarmctl
kubectl get discoveredrobots -n warehouse-a
bin/swarmctl admit robot <discovered-name> -n warehouse-a --zone warehouse-a \
  --adapter sim-fleet-adapter --class simbot-v1 --yes
kubectl apply -f config/samples/first-run/fleettask.yaml
kubectl -n warehouse-a wait fleettask/first-run --timeout=300s \
  --for=jsonpath='{.status.phase}'=Succeeded
```

[config/samples/README.md](config/samples/README.md) sets out both admission paths,
with and without webhooks, and the exact commands.

### Run from source

To run the control plane from source instead — prerequisites are Go 1.25+ (the
`go` directive in `go.mod`; CI and the `Dockerfile` build with 1.26),
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
kubectl apply -f config/samples/ -l 'swarmada.io/sample-posture!=webhooks-on'   # make run serves no webhooks
kubectl get robots,fleettasks -n warehouse-a
```

## Documentation

- [Getting started](docs/getting-started.md) — three tiers, from a simulated robot on a
  local cluster to your own robot
- [Architecture](docs/architecture.md) — the two-plane control/data model, the
  control-plane components, and the CRDs
- [Quickstart](docs/quickstart.md) — run the demos on a local `kind` cluster and
  the C0–C19 adapter conformance suite
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
> the simulation-based demo scenarios, and the edge Zone Controller node
> (`cmd/edge`, `Dockerfile.edge`) with its no-hardware demo (`examples/edge-node`).
> The three reference Fleet Adapters are maintained in their own public
> repositories; `scripts/fetch-reference-adapters.sh` clones them into
> `adapters/external/`, which is not checked in.

Swarmada is pre-release; public claims track the code:

- **API** — 21 CRDs are defined and generate under `swarmada.io/v1`:
  the eighteen RFC-0001 specifies, and `TaskTrigger`, `SemanticEvent` and
  `SemanticEventSource` from the draft RFC-0003.
- **Controllers** — reconciliation is implemented, wired into the manager, and
  tested for these CRDs: `Robot` (full capability derivation), `RobotClass`
  (referencing-robot count), `FleetAction` (the atomic unit of dispatch —
  assignment lease, estop-pause, preemption, and capability-loss reassignment of
  an in-flight action on a required-capability degradation), `FleetTask` (the
  composite objective — dependency graph, completion and failure policies, saga
  compensation), `DiscoveredRobot` (two-phase discovery with TTL/Stale sweep),
  `FleetZone` (zone topology + live position derivation), `SwarmadaConfig`
  (+ auto-bootstrap), `ModelRollout`, `FirmwareRollout` (fail-closed signature
  verification), `ModelPolicy`, `RobotProbe`, `FleetAdapter` (connectivity +
  conformance status), `ZoneMaintenance` (maintenance-window lifecycle,
  graceful in-progress-task wind-down), and `TaskTrigger` (its reconciler runs on
  each `SemanticEvent` and, on a match, creates a `FleetTask` with
  `spec.draft: true`, held at phase `Draft` until a person clears the flag).
  `EdgeNode`, `ComputeEndpoint`, `ComputeWorkloadTemplate`, `ResultSink`,
  `ArtifactStore` and `SemanticEventSource` have no reconciler.
- **Traffic Deconfliction Engine** — implemented as a blocking gate before every
  assignment of an action that names a zone: zone-capacity gating, and preemption by a
  `Critical` or `High` preemptor. Shared resources are granted or queued when a Fleet
  Adapter requests them, never denied; the scheduler does not request them.
- **Control plane wire** — the `ControlStream` gRPC server is live, with per-message
  `robot_id` authorization, the six built-in RBAC roles, a tamper-evident
  hash-chained audit log, and confirmed-only emergency-stop delivery over
  `SafetyStream`. The Zone Controller edge node is implemented as a runnable,
  tested binary — mTLS-served `EdgeStream`, fail-safe config sync from
  `FleetZone`/`Robot`, a local safety-input hook, and a no-hardware demo
  (`cmd/edge`, `examples/edge-node`).
- **Adapters** — the Fleet Adapter contract is defined by `proto/` and RFC-0001,
  and the in-tree simulation adapter drives the demo. An executable C0–C19
  conformance harness (`adapters/conformance/`) is in-tree, and three reference
  adapters (per [ADR-0005](docs/adr/0005-reference-adapter-policy.md)) are maintained
  in their own repositories — ROS 2, VDA5050, and MAVLink — and are cloned into
  `adapters/external/`, which is not checked in here (see
  [docs/adapters.md](docs/adapters.md)). ROS 2 is event-driven and
  VDA5050 is request/response, so the set spans two structurally different
  integration paradigms. All three declare contract `1.0.0`, outside the
  supported range `>=1.3.0 <1.5.0`, so this release refuses their robots (see
  [adapters/REGISTRY.md](adapters/REGISTRY.md)). No conformance grade has been earned on physical
  hardware; development measurements taken on a physical robot are recorded in
  [ADR-0063](docs/adr/0063-a-field-this-tree-cannot-evaluate-is-absent-not-inert.md)
  and [ADR-0069](docs/adr/0069-exclusions-are-child-zones-with-denial-semantics.md).

The [architecture](docs/architecture.md) is documented as designed; this list is the
authoritative statement of what runs today. Before building on it, read
[docs/known-limitations.md](docs/known-limitations.md): what this release does not do, what
you will see, and what to do instead.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, the DCO
sign-off requirement, and the RFC process. All participation is governed by the
[Code of Conduct](CODE_OF_CONDUCT.md); project governance is described in
[GOVERNANCE.md](GOVERNANCE.md); report security issues per [SECURITY.md](SECURITY.md).

## License

Apache 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
