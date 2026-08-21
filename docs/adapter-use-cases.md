# Fleet Adapter use cases — commands and scripts

This is a task-oriented companion to [`adapters.md`](adapters.md) (install/run/
test/conformance mechanics) and [`adapters/REGISTRY.md`](../adapters/REGISTRY.md)
(what each adapter covers and its conformance status). Where those two documents
answer "how do I install and run an adapter," this one answers "what do I run to
accomplish a specific thing," end to end, with copy-pasteable commands.

Every command below is checked against what the repository actually contains
today. Where a use case depends on work that is planned but not yet built (such as
parts of the scenario-preset system), that's called out explicitly rather than
documented as if it already works.

## Prerequisites (once, for every use case below)

```bash
make proto-py                       # generated Python stubs, written to proto/
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
make deploy IMG=swarmada-controller:dev

# Create the namespace and apply the sample FleetZone/Robot/FleetTask:
kubectl create namespace warehouse-a
kubectl apply -f config/samples/ -n warehouse-a

# Watch the fleet come up:
kubectl get robots,fleettasks -n warehouse-a -w
```

In a second terminal, point the in-tree simulation adapter at the control
plane (forward the port first if the adapter runs outside the cluster):

```bash
kubectl port-forward -n system deployment/swarmada-controller-manager 9443:9443 &
python3 adapters/simulation/sim_adapter.py --endpoint localhost:9443
```

You should see the sample `Robot` (`sim-robot-001`, `config/samples/robot_sample.yaml`)
move from `Discovered` to registered, and the sample `FleetAction`s get assigned to
it once telemetry confirms it meets each action's declared zone and capability
requirements. The sample `FleetTask` (`receiving-round`) is never assigned to a
robot itself: the control plane generates one `FleetAction` per member and
schedules those, releasing each member as its `dependsOn` predecessors succeed.

## 3. Fault-injection / recovery demo (the Demo B story)

The capability-degrade → task-reroute → recovery scenario used in Swarmada's
public demo material. There are two ways to drive it; pick the one that fits how
you're running the fleet.

> **Note.** The *task-reroute* and *recovery* steps are governed by
> *Capability-loss reassignment* (RFC-0001 control-plane; **impl: planned**): an
> in-flight task is reassigned via an adapter-confirmed safe stop, or — when the
> robot is mid-commitment and cannot safely hand off — the adapter recovers
> (returns load / to base) and the task fails `CapabilityLostDuringExecution`.
> Until that lands, the on-the-wire path below demonstrates the capability
> degrade/recover arc; the reroute is the specified target behaviour.

### Way 1 — adapter-driven (on the wire, recommended)

The `hardware-fault` scenario makes the simulated Fleet Adapter emit a real
`DEGRADED` hardware status over `TelemetryPayload.hardware` (and the initial
`CapabilitiesSnapshot`); the control plane derives the capability degrade and
reroutes the in-flight task, then recovers. One flag, deterministic timing:

```bash
python3 adapters/simulation/sim_adapter.py --endpoint localhost:9443 \
    --scenario hardware-fault --fault-component camera_front --fault-at 30 --recover-at 90
```

The same `--scenario hardware-fault` works on the ROS 2 / VDA5050 / MAVLink
simulated backends (`--binding` / `--comms` / `--link simulated`). See
[`adapters/scenarios`](../adapters/scenarios/README.md) for all five presets.

### Way 2 — manual / status projection

When no live adapter is running (the projection-based quickstart), degrade the
`Robot` status by hand. Note `status.hardware` is an **array** of

> **This is a demo shortcut, not an operator workflow.** `Robot.status` is controller-owned
> and operators must not write it (RFC-0001 §9.1.3; RA-1, RFC-0001 Terminology) — `status.hardware[]` is a control-plane-owned field, written from
> adapter telemetry by the Robot status sink (RFC-0001 §9.3.3). Use this only against a simulated fleet with no
> live adapter, and never against a real one.
`{name, status}` components:

```bash
kubectl patch robot sim-robot-002 -n warehouse-a --subresource=status --type=merge \
  -p '{"status":{"hardware":[{"name":"camera_front","status":"Degraded"}]}}'
```

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
it into a live-override channel the adapter watches (a deliberate future feature);
until then, Way 1 is the adapter-driven path.

## 4. Running the ROS 2 adapter against a real Nav2 workspace

The default `--binding simulated` needs no ROS 2 install at all. Once you have
a sourced ROS 2 + Nav2 workspace (real hardware or a Gazebo-simulated robot),
switch bindings:

```bash
source /opt/ros/jazzy/setup.bash        # or your workspace's local setup.bash
python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py \
    --binding ros2 --endpoint localhost:9443 --robot-id nav2-robot-01
```

The adapter maps `FleetTask` assignment to a `NavigateToPose` action goal,
`odom`/`battery`/`diagnostics` to telemetry, and a confirmed estop to a Nav2
cancel + hold (never inferred — `is_stopped` comes from the odom twist).

## 5. Running the VDA5050 adapter against a real MQTT broker

```bash
pip install -e 'adapters/external/fleet-adapter-vda5050[vda5050]'   # adds paho-mqtt

python -m fleet_adapter_vda5050.adapter --endpoint localhost:9443 \
    --comms vda5050 --mqtt-host broker.local --mqtt-port 1883
```

`FleetTask` assignment becomes a VDA5050 `order` (monotonic `headerId`/
`orderUpdateId`); task status comes from `state`; a confirmed estop is a
`cancelOrder` `instantAction` safe-hold, confirmed from `state`, never a timer.
`--comms simulated` (no broker) remains the default for anyone without one.

## 6. Running the MAVLink adapter against PX4 SITL + Gazebo

```bash
pip install -e 'adapters/external/fleet-adapter-mavlink[mavlink]'   # adds pymavlink

# In a separate terminal: start PX4 SITL + Gazebo (see the PX4 docs for your platform)
make px4_sitl gazebo

python -m fleet_adapter_mavlink.adapter --endpoint localhost:9443 \
    --link mavlink --mavlink-url udp:127.0.0.1:14540
```

Remember the scope boundary: this adapter does task/mission handoff only
(`NAV_WAYPOINT` + `MISSION_START`), never flight control or airspace
deconfliction — the certified flight stack executes the mission, and a
confirmed estop here is an RTL safe-hold hand-off, not a substitute for
certified flight-safety systems.

## 7. Multi-vendor fleet — several protocols against one control plane

The core value proposition, demoed directly: bring up several adapters
speaking different protocols against the same running control plane and
watch them all show up as `Robot` objects the scheduler treats uniformly.

```bash
# Terminal 1 — a simulated ROS 2 robot:
python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py \
    --endpoint localhost:9443 --robot-id ros2-sim-01

# Terminal 2 — a simulated VDA5050 robot:
python -m fleet_adapter_vda5050.adapter --endpoint localhost:9443 \
    --comms simulated --robot-id vda5050-sim-01

# Terminal 3 — a simulated MAVLink vehicle:
python -m fleet_adapter_mavlink.adapter --endpoint localhost:9443 \
    --link simulated --robot-id mavlink-sim-01

# Terminal 4 — watch all three register as Robots regardless of wire protocol:
kubectl get robots -n warehouse-a -w
```

Apply a `FleetTask` whose `requiredCapabilities` only one of the three
declares and confirm the scheduler picks the matching robot rather than the
first one that registered.

## 8. Validating a candidate adapter before opening a Registry PR

Before adding or updating a row in [`adapters/REGISTRY.md`](../adapters/REGISTRY.md):

```bash
make conformance ADAPTER='<your adapter's run command, with {port} for the endpoint>' \
    --json report.json
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

The generated adapter is wired to the audited `swarmada-sdk` safety
primitives (fencing, confirmed estop, lease self-stop) and passes the
conformance handshake immediately. Implement the `RobotBinding` seam for the
target robot interface, then run `make conformance` against it until every
non-skipped check passes before proposing a Registry entry.
