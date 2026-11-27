# Connect a ROS 2 hobby robot

This page connects one robot that runs ROS 2 and Nav2 to a Swarmada control plane on
your computer. The robot is admitted the way page 1's simulated robot was, then it
answers an emergency stop at rest, drives one short leg, and runs the four-leg
`hello-world` task on your own floor.

Swarmada does not move the robot. Nav2 on the robot plans and drives; the Fleet Adapter
on the robot turns each `FleetAction` into a Nav2 `NavigateToPose` goal and reports the
robot's state back. A software stop does not replace a physical stop switch: keep one
within reach for every step that moves the robot.

**How this page was tested.** Every command ran on one computer, with a ROS 2 Jazzy
container standing in for the robot: a simulated base that publishes odometry and
answers `NavigateToPose` goals, without real Nav2, sensors or motors. The install steps
also ran in a ROS 2 Humble container. Steps marked **Not tested on hardware** were not
run on a physical robot.

## What the robot needs

- **ROS 2 Humble or Jazzy, with Nav2 running and a map.** The adapter sends goals in the
  `map` frame, so the robot needs localization that publishes the `map` → `odom`
  transform, such as AMCL or SLAM. Without it, the robot reports no position, and the
  control plane gives it no work.
- **Odometry on `odom`.** The adapter confirms an emergency stop from odometry received
  after the stop. Odometry that is measured, from wheel encoders for example, can
  confirm it. Odometry that only repeats the velocity command cannot: on such a base,
  every stop is answered `STOPPING`, then `FAILED`.
- **Python 3.10 or later, `grpcio` 1.64.1 or later and `protobuf` 3.20.3 or later**, for
  the adapter. ROS 2 Humble as shipped on Ubuntu 22.04 meets this: its Python is 3.10,
  and the adapter installs its own `grpcio` and `protobuf` into a virtual environment.
  Python 3.12 or later is needed only on the computer where the conformance harness
  runs.
- **A network path to your computer**, on TCP port 9445.
- **A physical stop switch within reach.**

## Step 1: run the control plane

On your computer, in the `swarmada` checkout from page 1, create a cluster and install
cert-manager, which issues the certificates the robot connects with:

```bash
kind create cluster --name swarmada-home
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
kubectl wait deployment --all -n cert-manager --for=condition=Available --timeout=180s
```

A deployment can read `Available` a little before cert-manager's own webhook answers,
and the chart's certificates are refused until it does. Wait for the webhook to answer a
server-side dry run, which creates nothing:

```bash
until kubectl create --dry-run=server -o name -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: webhook-probe, namespace: default}
spec: {selfSigned: {}}
EOF
do sleep 5; done
```

It prints `issuer.cert-manager.io/webhook-probe (server dry run)` once the webhook answers.

Install the Helm chart. Its default image is the published, signed
`ghcr.io/swarmada/swarmada` release, so nothing is built. The two `--set` lines ask the
chart for a client certificate for the robot's adapter, `home-robot-adapter`, in
namespace `home`:

```bash
helm install swarmada deploy/swarmada -n swarmada-system --create-namespace \
  --set 'controlStream.tls.certManager.adapterClients[0].name=home-robot-adapter' \
  --set 'controlStream.tls.certManager.adapterClients[0].namespace=home' \
  --wait --timeout 6m
```

The chart runs with the admission webhooks on and the `ControlStream` open only to
clients with a certificate from its own certificate authority, as page 1's step 3 did.

`--wait` returns once the chart's pods are ready, which can be before Swarmada's
admission webhook accepts requests. Until it does, it refuses the objects in step 2.
Create the robot's namespace, and wait for the webhook to answer a server-side dry run
of the namespace's configuration, which creates nothing:

```bash
kubectl create namespace home
until kubectl apply --dry-run=server -f - <<'EOF'
apiVersion: swarmada.io/v1
kind: SwarmadaConfig
metadata: {name: swarmada-config, namespace: home}
spec:
  signing: {requireSignatureVerification: false}
  telemetry: {sink: {type: Drop}}
EOF
do sleep 5; done
```

Until the webhook answers, each try prints an error that ends in
`connect: connection refused`. Once it answers, the loop ends with:

```text
swarmadaconfig.swarmada.io/swarmada-config created (server dry run)
```

## Step 2: describe the robot

Create the namespace's configuration, a zone, and a `RobotClass` for the robot. The
class lists one capability, `navigation.2d`, which is what the adapter's one action,
`Navigate`, requires. A class lists at least one hardware component; change the lidar
below to what your robot carries:

```bash
kubectl apply -f - <<'EOF'
apiVersion: swarmada.io/v1
kind: SwarmadaConfig
metadata:
  name: swarmada-config
  namespace: home
spec:
  signing:
    requireSignatureVerification: false
  telemetry:
    sink:
      type: Drop
---
apiVersion: swarmada.io/v1
kind: FleetZone
metadata:
  name: home
  namespace: home
spec:
  displayName: "Home"
---
apiVersion: swarmada.io/v1
kind: RobotClass
metadata:
  name: home-robot-v1
  namespace: home
spec:
  manufacturer: Homebuilt
  model: ros2-nav2
  baseAdapter:
    name: home-robot-adapter
    version: "1.3.0"
  hardware:
    - name: lidar
      type: Lidar
      model: 2d-lidar
  baseCapabilities:
    - name: navigation.2d
      type: hardware-native
      pauseable: false
EOF
```

`requireSignatureVerification: false` lets the control plane accept a conformance
report that is not signed; it still checks the report against its digest.

## Step 3: qualify the adapter

The control plane admits a robot only through an adapter with a conformance report. The
conformance harness drives the adapter through the checks in
[the conformance suite](../../adapters/CONFORMANCE.md) and writes that report. It runs on
your computer, against the adapter's simulated binding, so no robot is needed.

The harness needs Python 3.12 or later. In the `swarmada` checkout, use page 1's virtual
environment, and install the adapter and the SDK it is built on:

```bash
git clone --branch v1.3.0 https://github.com/swarmada/fleet-adapter-ros2.git ../fleet-adapter-ros2
source .venv/bin/activate
pip install ./sdk/python ../fleet-adapter-ros2
make conformance \
  ADAPTER='fleet-adapter-ros2 --endpoint localhost:{port} --edge-latch-file harness-edge-latch' \
  ADAPTER_NAME=home-robot-adapter CONFORMANCE_ARGS='--json home-robot-report.json'
rm -f harness-edge-latch
```

One check sends the adapter an emergency stop from an edge node. The adapter records
such a stop in a latch file and refuses to start again while the file is there, the way
a robot stays stopped until a person clears it. `--edge-latch-file` keeps the harness's
latch in this folder, and `rm` clears it. Without the flag, the latch goes to
`~/.local/state/swarmada/` on your computer, and a second harness run fails at its
first check.

On the test computer, the harness took 70 seconds and ended with:

```text
29 passed, 0 failed, 1 advisory, 49 skipped   => CONFORMANT (for the checks that ran)
wrote home-robot-report.json
```

The harness gives an adapter at contract `1.3.0`, as this one is, no work. So the 28
checks that need an assignment are among the 49 skipped, and the report says nothing
about how the adapter runs work. Step 8 is where you see it do that.

Store the report, and describe the adapter with the report's digest:

```bash
kubectl create configmap home-robot-adapter-conformance -n home \
  --from-file=report.json=home-robot-report.json
DIGEST="sha256:$(shasum -a 256 home-robot-report.json | cut -d ' ' -f 1)"
kubectl apply -f - <<EOF
apiVersion: swarmada.io/v1
kind: FleetAdapter
metadata:
  name: home-robot-adapter
  namespace: home
spec:
  vendor: ros2-nav2
  simulated: false
  servesRobotClasses:
    - home-robot-v1
  heartbeatIntervalSeconds: 10
  conformanceReport:
    suiteVersion: "C0-C20"
    configMapRef: home-robot-adapter-conformance
    digest: "${DIGEST}"
EOF
```

A passing report shows that the adapter speaks the protocol correctly. It is not
evidence that your robot works: the harness never talked to it.

## Step 4: give the robot its certificate and a way in

The chart issued the adapter's client certificate into a Secret. Write it to files:

```bash
mkdir -p swarmada-certs
kubectl get secret home-robot-adapter-home-client-tls -n swarmada-system \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > swarmada-certs/ca.crt
kubectl get secret home-robot-adapter-home-client-tls -n swarmada-system \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > swarmada-certs/tls.crt
kubectl get secret home-robot-adapter-home-client-tls -n swarmada-system \
  -o jsonpath='{.data.tls\.key}' | base64 -d > swarmada-certs/tls.key
chmod 600 swarmada-certs/tls.key
```

`tls.key` is the adapter's private key: anyone who holds it can connect as your robot's
adapter. Copy the folder to the robot's home directory, for example with
`scp -r swarmada-certs <user>@<robot>:` (**Not tested on hardware**), and keep no other
copy.

Open the control plane's `ControlStream` to your network, in a terminal you leave open:

```bash
kubectl -n swarmada-system port-forward --address 0.0.0.0 \
  service/swarmada-controlstream 9445:9445
```

`--address 0.0.0.0` listens on every network interface of your computer; to listen on
one, give its address on your home network instead. A firewall on your computer may
block the robot until you allow incoming TCP on port 9445. The stream accepts only
clients that present a certificate from the chart's certificate authority, and the
connection is encrypted both ways.

## Step 5: install the adapter on the robot

On the robot, get the adapter and the SDK it is built on. The SDK is not published as a
Python package, so it comes from a checkout of Swarmada:

```bash
git clone --branch v0.4.1 https://github.com/swarmada/swarmada.git ~/swarmada
git clone --branch v1.3.0 https://github.com/swarmada/fleet-adapter-ros2.git ~/fleet-adapter-ros2
```

Install them into a virtual environment that also sees the ROS 2 Python packages. On
Humble:

```bash
sudo apt install python3-venv
source /opt/ros/humble/setup.bash
python3 -m venv --system-site-packages ~/fleet-adapter-venv
~/fleet-adapter-venv/bin/python -m pip install --upgrade 'pip>=23.1'
~/fleet-adapter-venv/bin/python -m pip install ~/swarmada/sdk/python
~/fleet-adapter-venv/bin/python -m pip install ~/fleet-adapter-ros2
```

On Jazzy, source `/opt/ros/jazzy/setup.bash` instead. `--system-site-packages` lets the
adapter import `rclpy` and the Nav2 messages from the ROS 2 installation. pip 23.1 or
later is needed because Ubuntu 22.04's pip installs these packages under the name
`UNKNOWN`; the upgrade stays inside the virtual environment. The
[adapter's README](https://github.com/swarmada/fleet-adapter-ros2#install) explains both.

On the test computer, these steps passed on Humble's Python 3.10 with `grpcio` 1.64.1 and
`protobuf` 3.20.3, the lowest versions the adapter accepts, and with the latest
versions, `grpcio` 1.85.4 and `protobuf` 7.36.2. The adapter's own tests passed on both.

## Step 6: start the adapter

On the robot, with Nav2 and localization running, and `<computer>` set to your
computer's address on your home network:

```bash
source /opt/ros/humble/setup.bash
~/fleet-adapter-venv/bin/fleet-adapter-ros2 --binding ros2 \
  --endpoint <computer>:9445 --namespace home --robot-id home-robot-1 \
  --tls-ca ~/swarmada-certs/ca.crt --tls-cert ~/swarmada-certs/tls.crt \
  --tls-key ~/swarmada-certs/tls.key \
  --tls-server-name swarmada-controlstream.swarmada-system.svc \
  --stall-window 5.0 --stall-min-progress 0.10 --goal-response-bound 1.0 \
  --odometry-source measured --provisioning-interval 5
```

- `--tls-server-name` is the name in the control plane's certificate. The robot dials
  your computer's address, so the name has to be given.
- `--stall-window`, `--stall-min-progress`, `--goal-response-bound` and
  `--odometry-source` have no defaults, because they are properties of your robot. The
  values above are the test stand-in's. For your robot:
  - `--stall-window` must outlast its longest turn in place or recovery, in seconds;
  - `--stall-min-progress` must exceed the noise in its position estimate, in metres;
  - `--goal-response-bound` must exceed the longest time Nav2 takes to accept a goal,
    in seconds;
  - `--odometry-source` is `measured` only if the odometry comes from the wheels or
    another sensor, and `command-derived` if it repeats the velocity command.
- `--provisioning-interval 5` makes the adapter announce the robot every 5 seconds until
  it is admitted, instead of every 60.

**Not tested on hardware:** this step, on a physical robot.

## Step 7: admit the robot

On your computer:

```bash
kubectl get fleetadapter,discoveredrobots -n home
```

```text
NAME                                          VENDOR      PHASE       CONFORMANCE   ROBOTS   AGE
fleetadapter.swarmada.io/home-robot-adapter   ros2-nav2   Connected   Passed                 11s

NAME                                       MANUFACTURER   MODEL   FIRMWARE   PHASE        CONNECTED   TTL
discoveredrobot.swarmada.io/home-robot-1   ROS 2          Nav2               Discovered   1s          2026-11-27T21:34:16Z
```

The `FleetAdapter` reads `Connected` with conformance `Passed`, and the robot appears as a
`DiscoveredRobot`. Admit it into the zone, with its class. The control plane creates the
`Robot` shortly after the admission, and the `Robot` then reads `Idle`. Wait for both:

```bash
swarmctl admit robot home-robot-1 -n home --zone home --class home-robot-v1 --yes
kubectl wait robot/home-robot-1 -n home --for=create --timeout=120s
kubectl wait robot/home-robot-1 -n home --for=jsonpath='{.status.phase}'=Idle --timeout=180s
kubectl get robot home-robot-1 -n home
```

```text
discoveredrobot.swarmada.io/home-robot-1 admitted into zone home; the control plane will create robot.swarmada.io/home-robot-1
robot.swarmada.io/home-robot-1 condition met
robot.swarmada.io/home-robot-1 condition met
NAME           PHASE   ZONE   BATTERY   ACTION   AGE
home-robot-1   Idle    home   100                3s
```

## Step 8: first motion, in order

Do these three in order, and go on only when each one ends as shown. Keep the physical
stop switch within reach.

**A stop, at rest.** Stop every robot in the zone, and wait for the robot to confirm the
stop:

```bash
swarmctl estop trigger home -n home --reason "stop drill" --yes
kubectl wait robot/home-robot-1 -n home --for=jsonpath='{.status.estopState}'=Stopped --timeout=60s
```

```text
fleetzone.swarmada.io/home emergency stop triggered.
robot.swarmada.io/home-robot-1 condition met
```

`Stopped` means the adapter confirmed the robot at rest. If the wait times out instead,
read the state with
`kubectl get robot home-robot-1 -n home -o jsonpath='{.status.estopState}'`. `Failed`
means the control plane got no confirmed stop: treat the robot as not stopped, and use
the stop switch.

Clear the stop, then release the robot. A cleared robot reads `Resuming` and gets no
work until you release it. Both are recorded in the safety audit log with their reason:

```bash
swarmctl estop-clear home -n home --reason "stop drill over" --yes
kubectl wait robot/home-robot-1 -n home --for=jsonpath='{.status.estopState}'=Resuming --timeout=60s
swarmctl estop resume home-robot-1 -n home --reason "stop drill over" --yes
kubectl get robot home-robot-1 -n home -o jsonpath='{.status.estopState}{"\n"}'
```

```text
fleetzone.swarmada.io/home emergency stop cleared (reason: stop drill over); its robots are Resuming until released with "swarmctl estop resume".
robot.swarmada.io/home-robot-1 condition met
robot.swarmada.io/home-robot-1 released: estopState Normal.
Normal
```

If the control plane refuses the release, `swarmctl` prints why, for example that the
robot has sent no telemetry since the clear. Run the release again once the cause is
resolved.

**One short leg.** Half a metre ahead of where the map's origin is. Pick a point on your
own map that is free floor, clear of walls and of anything the robot must not touch:

```bash
kubectl apply -f - <<'EOF'
apiVersion: swarmada.io/v1
kind: FleetTask
metadata:
  name: first-leg
  namespace: home
spec:
  actions:
  - name: short-leg
    action:
      type: Navigate
      robotSelector: home-robot-1
      onFailure: Alert
      payload: {x: 0.5, "y": 0.0}
EOF
kubectl wait fleettask/first-leg -n home \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s
```

**The four legs.** Copy page 1's task for your robot and namespace, then edit the four
points in `my-hello-world.yaml` to free floor on your own map:

```bash
sed -e 's/sim-robot-001/home-robot-1/' -e 's/namespace: warehouse-a/namespace: home/' \
  examples/hello-world/hello-world-task.yaml > my-hello-world.yaml
kubectl apply -f my-hello-world.yaml
kubectl wait fleettask/hello-world -n home \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
kubectl get fleettask hello-world -n home
```

```text
fleettask.swarmada.io/hello-world created
fleettask.swarmada.io/hello-world condition met
NAME          PHASE       ACTIONS         DESIRED   AGE
hello-world   Succeeded   4/4 Succeeded   Running   28s
```

On the stand-in, the short leg took about a second and the four legs 28 seconds, at
0.5 m/s.

To stop the robot partway through, cancel the task: `swarmctl cancel task hello-world -n
home`. For anything that must stop at once, use the stop switch or `swarmctl estop
trigger`.

**Not tested on hardware:** this step, on a physical robot.

## Clean up

On the robot, stop the adapter with Ctrl-C. On your computer, stop the port-forward with
Ctrl-C, then:

```bash
kind delete cluster --name swarmada-home
```

## What this page shows, and what it does not

**Shown, on the stand-in:** a ROS 2 adapter installed on Humble and Jazzy, qualified,
connected over mTLS from outside the cluster, admitted, stopped at rest, and given
`Navigate` work.

**Not shown:**

- **A physical robot.** The stand-in has no motors, no sensors and no real Nav2. Every
  conformance result recorded for this adapter was earned in simulation.
- **Navigation quality.** Whether a goal is reached, and how, is Nav2's work on your
  robot.
- **Report signing,** turned off in step 2.
- **Work in the conformance report.** The harness gives this adapter no work, as step 3
  says. Step 8 is the only place on this page where it runs work.

[Known limitations](../known-limitations.md) lists the rest. Page 3 writes a task for a
home routine.
