# Fleet Adapter use cases — commands and scripts

This is a task-oriented companion to [`adapters.md`](adapters.md) (install/run/
test/conformance mechanics) and [`adapters/REGISTRY.md`](../adapters/REGISTRY.md)
(what each adapter covers and its conformance status). Where those two documents
answer "how do I install and run an adapter," this one answers "what do I run to
accomplish a specific thing," end to end, with copy-pasteable commands.

Every command below is checked against what the repository actually contains
today. Where a use case depends on something that is not built (such as
parts of the scenario-preset system), that's called out explicitly rather than
documented as if it already works.

## Prerequisites (once, for every use case below)

The generated Python stubs are committed under `proto/`.

```bash
pip install -e sdk/python           # Swarmada Python SDK (add --break-system-packages on Homebrew macOS Python)
```

The `adapters/external/…` paths in the use cases below assume you have cloned the
relevant reference adapter from its own repo ([ADR-0005](adr/0005-reference-adapter-policy.md)).
The in-tree `simulation` adapter (`make conformance-sim`) needs no clone.

## 1. Smoke-test a single adapter with no cluster at all

Fastest possible check that an adapter builds, dials in, and completes the
handshake — no minikube, no other adapters, nothing else running. Useful right
after cloning, or after changing an adapter's own code.

```bash
# Terminal 1 — a bare gRPC endpoint standing in for the control plane:
python -m adapters.conformance --stub-path proto --adapter-name smoke-test \
    --adapter-cmd 'python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py --endpoint localhost:{port}'
```

This is `make conformance` under the hood — it starts the test server, launches
the adapter, and exits non-zero on any failed MUST/MUST NOT. Swap the
`--adapter-cmd` for VDA5050 or MAVLink to smoke-test those instead:

```bash
make conformance ADAPTER='python3 adapters/external/fleet-adapter-vda5050/fleet_adapter_vda5050/adapter.py --endpoint localhost:{port} --comms simulated'
make conformance ADAPTER='python3 adapters/external/fleet-adapter-mavlink/fleet_adapter_mavlink/adapter.py --endpoint localhost:{port} --link simulated'
make conformance-sim   # in-tree simulation adapter (KinematicSim)
```

## 2. Full local fleet demo — discover, admit, assign (Demo A)

The end-to-end loop against a real (local) control plane: bring up minikube,
deploy the controller-manager, apply the sample resources, and watch a robot
get discovered, admitted, and assigned a task.

```bash
# Bring up the cluster and controller-manager (see deploy-minikube.md for detail):
minikube start --profile swarmada-dev
kubectl config use-context swarmada-dev
make docker-build IMG=swarmada-controller:dev
minikube image load swarmada-controller:dev --profile swarmada-dev
make install
# cert-manager issues the ControlStream server and adapter client certificates:
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s
# config/overlays/quickstart-mtls, not `make deploy`: it turns the admission webhooks on,
# and a Robot applied from a file is then refused until its adapter is Connected.
kubectl apply -k config/overlays/quickstart-mtls --server-side --force-conflicts

# Create the namespace and apply the sample FleetZone/Robot/FleetTask:
kubectl create namespace warehouse-a
kubectl apply -f config/samples/ -n warehouse-a -l 'swarmada.io/sample-posture!=webhooks-on'

# Watch the fleet come up:
kubectl get robots,fleettasks -n warehouse-a -w
```

In a second terminal, point the in-tree simulation adapter at the control
plane. `make deploy-minimal` (plain `config/default`) would not work here: its manager has no
TLS material and no `--fleet-adapter-insecure-authz`, so it serves no `ControlStream`
at all. The `quickstart-mtls` overlay serves one with mTLS and issues the adapter's
client certificate into the Secret `sim-fleet-adapter-client-tls`:

```bash
kubectl -n system get secret sim-fleet-adapter-client-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt
kubectl -n system get secret sim-fleet-adapter-client-tls -o jsonpath='{.data.tls\.crt}' | base64 -d > tls.crt
kubectl -n system get secret sim-fleet-adapter-client-tls -o jsonpath='{.data.tls\.key}' | base64 -d > tls.key
kubectl port-forward -n system service/swarmada-controlstream 9445:9445 &
PYTHONPATH=proto python3 adapters/simulation/sim_adapter.py --endpoint localhost:9445 \
    --namespace warehouse-a --robot-id sim-robot-001 --zone warehouse-a \
    --tls-ca ca.crt --tls-cert tls.crt --tls-key tls.key \
    --tls-server-name swarmada-controlstream.system.svc
```

`quickstart-mtls` is dev/demo only: it leaves the admission webhooks disabled. Per-robot
authorization is on, so the robot the adapter drives must be bound to it
(`spec.adapter.name`) and carry a class the `FleetAdapter` lists in
`spec.servesRobotClasses`; the sample fleet's robots carry `simbot-v1`. [Adapters § Against a real, running control
plane](adapters.md#against-a-real-running-control-plane) describes
`config/controlstream-tls`, which keeps both on. `examples/warehouse-quickstart/run.sh`
performs the cert-manager install, the overlay deploy, the certificate extraction and
the adapter launch the same way.

With only `config/samples/` applied, no `FleetAction` is assigned. Both sample robots
(`config/samples/robot_sample.yaml`) bind to a `FleetAdapter` named `sim-fleet-adapter`
(`spec.adapter.name`), and `config/samples/` contains no `FleetAdapter`. Dispatch
withholds a robot whose bound `FleetAdapter` is absent, not `Connected`, not
conformance-`Passed`, or bound to an unsupported contract version
(`adapterDispatchReadiness`). Assignment also needs, in `warehouse-a`, that
`FleetAdapter` with a `spec.conformanceReport` referencing a digest-matching report
that records `conformant: true` and a `contract_version` in the supported range, and a
`SwarmadaConfig` that either supplies a trust root for a signed report or sets
`spec.signing.requireSignatureVerification: false` (see [Adapters § Contract versions
and re-qualification](adapters.md#contract-versions-and-re-qualification)). Those are
necessary, not sufficient: the scheduler's zone, capability and battery filters still
apply to each action. The sample `FleetTask` (`receiving-round`) is never assigned to a
robot itself: the control plane generates one `FleetAction` per member and
schedules those, releasing each member as its `dependsOn` predecessors succeed.

## 3. Fault-injection / recovery demo (the Demo B story)

The capability-degrade → task-reroute → recovery scenario used in Swarmada's
public demo material. There are two ways to drive it; pick the one that fits how
you're running the fleet.

> **Note.** The *task-reroute* and *recovery* steps are implemented as
> *Capability-loss reassignment* (RFC-0001 control-plane). When a reachable robot holding
> an `Assigned` or `InProgress` action no longer satisfies that action's required
> capabilities, the FleetAction controller marks the action for requeue with reason
> `CapabilityLost` (`reasonCapabilityLost` in
> `internal/controller/fleetaction_controller.go`) and sends `cancel_action`. The
> outcome follows the adapter's answer: a confirmed safe stop returns the action to
> `Pending` for another robot; a robot that recovered mid-commitment (returned its load
> or went to base) fails the action `CapabilityLostDuringExecution`, after which the
> action's `onFailure` policy decides the next step; a robot that had already completed
> the action lets it succeed normally. A robot that cannot confirm a stop keeps the
> action until its lease is provably dead.

### Way 1 — adapter-driven (on the wire, recommended)

The `hardware-fault` scenario makes the simulated Fleet Adapter emit a real
`DEGRADED` hardware status over `TelemetryPayload.hardware` (and the initial
`CapabilitiesSnapshot`); the control plane derives the capability degrade and
reroutes the in-flight task, then recovers. One flag, deterministic timing. Run it
against the control plane, port-forward and client certificate from §2:

```bash
PYTHONPATH=proto python3 adapters/simulation/sim_adapter.py --endpoint localhost:9445 \
    --namespace warehouse-a --robot-id sim-robot-001 --zone warehouse-a \
    --tls-ca ca.crt --tls-cert tls.crt --tls-key tls.key \
    --tls-server-name swarmada-controlstream.system.svc \
    --scenario hardware-fault --fault-component camera_front --fault-at 30 --recover-at 90
```

Only the in-tree simulation adapter consumes the scenario presets; the ROS 2 / VDA5050 /
MAVLink simulated backends do not ([`adapters/scenarios`](../adapters/scenarios/README.md)).
That page lists all five presets.

### Way 2 — manual / status projection

When no live adapter is running (the projection-based quickstart), degrade the
`Robot` status by hand. Note `status.hardware` is an **array** of

> **This is a demo shortcut, not an operator workflow.** `Robot.status` is controller-owned
> and operators must not write it (RFC-0001 §9.1.3; RA-1, RFC-0001 Terminology) — `status.hardware[]` is a control-plane-owned field, written from
> adapter telemetry by the Robot status sink (RFC-0001 #control-plane-robot-health-and-connectivity). Use this only against a simulated fleet with no
> live adapter, and never against a real one.
`{name, status}` components:

```bash
kubectl patch robot sim-robot-002 -n warehouse-a --subresource=status --type=merge \
  -p '{"status":{"hardware":[{"name":"camera_front","status":"Degraded"}]}}'
```

The projection-based quickstart installs no admission webhooks. On an install that does
(`make deploy`, the Helm chart), this patch is refused: it changes a component's state, which
only the control plane writes, because the operator's resume derives the robot's capabilities from
it. Moving a robot out of `Error` or into or out of `Maintenance` or `Offline`, changing a
model's status, `status.modelGrantedCapabilities` or
`status.capabilities` by hand is refused there too
(RFC-0001 #control-plane-swarmada-api-server, the admission-rule table).

The `simulation/fault_injection/camera_fault.py` helper writes a
`/tmp/swarmada_fault_{robot}_{component}` sentinel to *mark* an injected fault
for this manual workflow:

```bash
python -m simulation.fault_injection.camera_fault --robot sim-robot-002 --component camera_front
python -m simulation.fault_injection.camera_fault --robot sim-robot-002 --component camera_front --list
python -m simulation.fault_injection.camera_fault --robot sim-robot-002 --component camera_front --clear
```

**Note:** no adapter reads that sentinel today — the degrade you observe comes from
the status patch above, not the file. To make the sentinel adapter-driven, repoint
it into a live-override channel the adapter watches; no adapter has one, so
Way 1 is the adapter-driven path.

## 4. Running the ROS 2 adapter against a real Nav2 workspace

> **Before running §4–§7 against a cluster.** The control plane from §2 serves `ControlStream`
> over mTLS only and requires a client certificate issued by the overlay's CA; a connection
> without one fails the handshake ([Adapters § Against a real, running control
> plane](adapters.md#against-a-real-running-control-plane)). `quickstart-mtls` issues that
> certificate for one adapter, `sim-fleet-adapter`; each adapter below needs its own
> `FleetAdapter` and certificate. Each external adapter's repository documents how it takes TLS
> material, so the commands below show binding selection only. Each external reference
> adapter also reports contract `1.0.0`, outside the `>=1.3.0 <1.5.0` range the control plane
> accepts ([REGISTRY.md](../adapters/REGISTRY.md)): its `FleetAdapter` is set `Rejected` and
> its robots are refused registration and admission until the adapter declares a contract
> version inside that range. The range is not widened.

The default `--binding simulated` needs no ROS 2 install at all. Once you have
a sourced ROS 2 + Nav2 workspace (real hardware or a Gazebo-simulated robot),
switch bindings:

```bash
source /opt/ros/jazzy/setup.bash        # or your workspace's local setup.bash
python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py \
    --binding ros2 --endpoint localhost:9445 --robot-id nav2-robot-01
```

The adapter maps a `FleetAction` assignment (`assign_action`) to a `NavigateToPose` action goal,
`odom`/`battery`/`diagnostics` to telemetry, and a confirmed estop to a Nav2
cancel + hold (never inferred — `is_stopped` comes from the odom twist).

## 5. Running the VDA5050 adapter against a real MQTT broker

```bash
pip install -e 'adapters/external/fleet-adapter-vda5050[vda5050]'   # adds paho-mqtt

python -m fleet_adapter_vda5050.adapter --endpoint localhost:9445 \
    --comms vda5050 --mqtt-host broker.local --mqtt-port 1883
```

A `FleetAction` assignment (`assign_action`) becomes a VDA5050 `order` (monotonic `headerId`/
`orderUpdateId`); task status comes from `state`; a confirmed estop is a
`cancelOrder` `instantAction` safe-hold, confirmed from `state`, never a timer.
`--comms simulated` (no broker) remains the default for anyone without one.

## 6. Running the MAVLink adapter against PX4 SITL + Gazebo

```bash
pip install -e 'adapters/external/fleet-adapter-mavlink[mavlink]'   # adds pymavlink

# In a separate terminal, from a PX4-Autopilot checkout (a PX4 make target, not one of
# this repository's): start PX4 SITL + Gazebo (see the PX4 docs for your platform)
make px4_sitl gazebo

python -m fleet_adapter_mavlink.adapter --endpoint localhost:9445 \
    --link mavlink --mavlink-url udp:127.0.0.1:14540
```

Remember the scope boundary: this adapter does task/mission handoff only
(`NAV_WAYPOINT` + `MISSION_START`), never flight control or airspace
deconfliction — the certified flight stack executes the mission, and a
confirmed estop here is an RTL safe-hold hand-off, not a substitute for
certified flight-safety systems.

## 7. Multi-vendor fleet — several protocols against one control plane

The target shape: several adapters speaking different protocols against the same
running control plane, each surfacing its robots as `Robot` objects the scheduler
treats uniformly. At v0.4 none of the three reference adapters can register a robot;
see the note at the start of §4.

```bash
# Terminal 1 — a simulated ROS 2 robot:
python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py \
    --endpoint localhost:9445 --robot-id ros2-sim-01

# Terminal 2 — a simulated VDA5050 robot:
python -m fleet_adapter_vda5050.adapter --endpoint localhost:9445 \
    --comms simulated --robot-id vda5050-sim-01

# Terminal 3 — a simulated MAVLink vehicle:
python -m fleet_adapter_mavlink.adapter --endpoint localhost:9445 \
    --link simulated --robot-id mavlink-sim-01

# Terminal 4 — watch for Robots (none from these three adapters registers at v0.4; see §4):
kubectl get robots -n warehouse-a -w
```

Once each adapter declares a contract version inside the supported range, apply a
`FleetAction` whose `requiredCapabilities` only one of the three declares and confirm
the scheduler picks the matching robot rather than the first one that registered.

## 8. Validating a candidate adapter before opening a Registry PR

Before adding or updating a row in [`adapters/REGISTRY.md`](../adapters/REGISTRY.md):

```bash
make conformance ADAPTER='<your adapter's run command, with {port} for the endpoint>' \
    CONFORMANCE_ARGS='--json report.json'
```

A conforming adapter has no failed MUST/MUST NOT among the checks that ran. A
`skip` is not a pass — record what it means (e.g. a timer-driven, on-robot
behavior that a unit test verifies separately, not the harness). Attach
`report.json` and the unit-test results to the PR that updates the Registry.

## 9. Scaffolding a brand-new adapter

```bash
pip install cookiecutter
cookiecutter adapters/template/
```

The generated adapter is wired to the shared `swarmada-sdk` safety
primitives (fencing, confirmed estop, lease self-stop) and redials a dropped
stream with bounded backoff. Run unmodified with its simulated binding, it fails no
conformance check, and its profiled verdict is `not-established` (see the template's
[README](../adapters/template/README.md#what-the-harness-reports-for-a-generated-adapter)).
Implement the `RobotBinding` seam for the
target robot interface, then run `make conformance` against it until every
non-skipped check passes before proposing a Registry entry.
