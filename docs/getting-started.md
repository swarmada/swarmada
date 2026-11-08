# Getting started

Swarmada connects robots to a Kubernetes control plane through Fleet Adapters. This
page orders the ways to start by what each one needs and what it shows. Before
building on what a tier shows, read [Known limitations](known-limitations.md).

| Tier | You run | You need | You finish with |
| :--- | :------ | :------- | :-------------- |
| 1. Try it | `make quickstart` | Docker, `kind`, `kubectl`, `make`, Go 1.26 or later, `python3` with `grpcio`, `protobuf` and `pyyaml` | A control plane on a local cluster, and a simulated robot that completed an action |
| 2. Connect an adapter | `make first-run` | Docker, `kind`, `kubectl`, `make`, Go 1.26 or later, `helm` | A robot admitted through the admission webhooks, and a `FleetTask` that reached `Succeeded` over mTLS |
| 3. Your own robot | An adapter for your robot | A cluster, an adapter inside the contract window, images you build | The tier 2 sequence, with your robot |

No tier needs hardware. Tiers 1 and 2 each create their own `kind` cluster.

## Tier 1: try it

When you finish, a `kind` cluster named `swarmada-quickstart` runs the control plane
and a sample fleet of three robots in namespace `warehouse-a`. One robot is driven by
a simulation adapter process, and the scheduler assigned it an action that it
completed.

The live robot's adapter runs with the first `python3` on your `PATH`, and that
interpreter needs the `grpcio`, `protobuf` and `pyyaml` packages. Without `pyyaml` the
adapter runs no scenario, and `hardware-fault` fails. A virtual environment in the
checkout provides them:

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install grpcio protobuf pyyaml
make quickstart
```

On macOS, `make setup-macos` installs Homebrew if it is missing, then Go, `kubectl`,
`kind`, `minikube`, Docker Desktop and `protoc`. It does not start Docker, and it
installs neither `helm` nor the Python packages. If it installed Go, open a new
terminal before `make quickstart`. On Linux, install the prerequisites with the
distribution's package manager; on Windows, run under WSL2. The
[quickstart](quickstart.md#prerequisites) lists them per operating system.

The run builds the controller image, so the first one takes longer than the ones after
it. On a terminal, it first asks for a scenario (Enter picks `healthy-fleet`), and it
pauses once before the live robot starts (Enter continues). It ends with a line that
starts `✅ Quickstart end state reached` and leaves the cluster running. To look at it:

```bash
kubectl get robots,fleetactions,fleettasks -n warehouse-a
```

Three robots read `Idle`, and the action `deliver-pallet-001` reads `Succeeded` on
`sim-robot-001`, the robot with an adapter. The other two actions stay `Pending`, and
the task that holds one of them stays `Running`; the [scenario
table](quickstart.md#the-demos--scenarios) says why.

To run a scenario by name, set `SCENARIO`. Each run reuses the cluster.

```bash
make quickstart SCENARIO=estop-drill
make quickstart SCENARIO=hardware-fault
```

`estop-drill` stops the live robot while it holds work and confirms the stop from the
robot's reported state. `hardware-fault` fails a camera on the robot mid-route; the
work moves to another robot and completes there. The [quickstart](quickstart.md#the-demos--scenarios)
lists every scenario, and [what is real and what is
simulated](quickstart.md#honest-notes--whats-real-vs-simulated).

Tier 1 applies its robots from a file, with the admission webhooks off. It does not
show how a robot arrives in a real installation; tier 2 does. To delete the cluster
and stop the adapter processes the run started:

```bash
bash examples/warehouse-quickstart/run.sh --clean
```

## Tier 2: connect an adapter

When you finish, a `kind` cluster named `swarmada-first-run` runs the Helm chart in
the posture a real installation runs in. A simulation adapter runs in the cluster, the
robot it announced was admitted with `swarmctl admit`, and a two-step `FleetTask`
reached `Succeeded` on it.

```bash
make first-run
```

It needs Docker, `kind`, `kubectl`, `helm`, Go and `make`, and no Python packages: the
adapter runs in the cluster from a manifest. The chart runs with:

- **Admission webhooks on.** A `Robot` is admitted only while its `FleetAdapter` is
  `Connected` and its conformance result is `Passed`.
- **`ControlStream` authorization on, over mTLS.** An adapter is trusted only for the
  `FleetAdapter` its client certificate names, and only for the robot classes that
  adapter serves.

A robot reaches the control plane in this order, and the script runs each step:

1. The adapter connects. Its `FleetAdapter` reads `Connected`, and its conformance
   result reads `Passed`.
2. The adapter announces the robot it serves, which appears as a `DiscoveredRobot`.
3. An operator admits it with `swarmctl admit`, naming its zone, adapter and robot
   class. The `Robot` is created and reads `Idle`.
4. The scheduler assigns the task's actions to it, and the task reaches `Succeeded`.

The run ends with a line that starts `PASS:`. [First run](../examples/first-run/README.md)
lists each step, what it checks and the variables that change it. The script uses a
kubeconfig file of its own, so add the cluster to yours to look at it:

```bash
kind export kubeconfig --name swarmada-first-run
kubectl get fleetadapter,discoveredrobots,robot,fleettask,fleetaction -n warehouse-a
```

The `FleetAdapter` `sim-fleet-adapter` reads `Connected` with conformance `Passed`,
`sim-robot-001` reads `Idle`, and the task `first-run` reads `Succeeded` with both of
its actions `Succeeded`. No `DiscoveredRobot` is listed: admission deletes it once the
`Robot` exists.

Tier 2 does not show report signing, which the sample configuration turns off, or more
than one manager replica, which the chart refuses while `ControlStream` is enabled. To
admit a robot on a cluster you already run, see [Using an existing
cluster](../README.md#using-an-existing-cluster). To delete this one:

```bash
kind delete cluster --name swarmada-first-run
```

## Tier 3: your own robot

This tier is a list of facts from the rest of the documentation, not a procedure. The
sequence is the one in tier 2, with your adapter in place of the simulation adapter.

- **The adapter.** A robot connects through a Fleet Adapter that implements the
  [`fleet_adapter.v1`](../proto/fleet_adapter/v1/fleet_adapter.proto) gRPC contract.
  Swarmada does not do motion control: the robot's own stack moves it, and the
  adapter turns each `FleetAction` into a command that stack runs.
- **The contract window.** The control plane implements contract `1.4.0` and accepts
  `>=1.3.0 <1.5.0`. An adapter that reports a version outside that range is refused
  `RegisterRobot` and `DiscoverRobot`, and a conformance report stamped with such a
  version satisfies neither admission nor dispatch. See [Contract versions and
  re-qualification](adapters.md#contract-versions-and-re-qualification).
- **The ROS 2 reference adapter.** It lives in its own repository;
  `scripts/fetch-reference-adapters.sh` clones it into `adapters/external/`. Its
  registry row records contract `1.3.0`, inside the window
  ([adapter registry](../adapters/REGISTRY.md)). Its real binding
  needs a sourced ROS 2 and Nav2 workspace; without one it runs a simulated binding.
- **Writing your own.** [Writing a Fleet Adapter](adapter-authoring.md) starts from
  the template, which wires the confirmed emergency stop, fencing-token ordering and
  assignment-lease self-stop from `swarmada-sdk`. A Python adapter needs Python 3.10
  or later, `grpcio` 1.64.1 or later and `protobuf` 3.20.3 or later. The conformance
  harness checks an adapter without a cluster; [Adapters](adapters.md#running-the-conformance-suite)
  gives the commands.
- **mTLS.** With `ControlStream` authorization on, each adapter presents a client
  certificate whose DNS SAN, `<adapter>.<namespace>.svc.cluster.local`, names its
  `FleetAdapter`. The Helm chart issues one per entry in
  `controlStream.tls.certManager.adapterClients`
  ([chart README](../deploy/swarmada/README.md#a-client-certificate-for-one-fleetadapter)).
- **The `Robot` resource.** With the admission webhooks on, a `Robot` is admitted only
  while its adapter's `FleetAdapter` is `Connected` and references a conformance report
  that matches its digest, records `conformant: true`, carries a contract version
  inside the window and, unless the namespace's `SwarmadaConfig` turns verification
  off, carries a signature that verifies. A robot arrives as a `DiscoveredRobot`, and
  `swarmctl admit` creates the `Robot`.
- **Images.** No manager image and no adapter image is published. Build them and load
  them into the cluster, as the [README](../README.md#using-an-existing-cluster) shows for
  the simulation adapter.
- **Hardware.** Every conformance result to date was earned in simulation; none on
  physical hardware. Swarmada does not replace physical safety hardware. A
  passing conformance report shows that an adapter speaks the protocol correctly; it
  is not evidence that the robot behind it works. The [conformance
  suite](../adapters/CONFORMANCE.md) lists what each check covers.
