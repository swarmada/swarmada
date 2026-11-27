# Run Swarmada with simulated robots

This page runs Swarmada on one computer, with simulated robots and no hardware. When
you finish, you have:

- seen a control plane assign work to a simulated robot, and stop that robot with an
  emergency stop;
- admitted a simulated robot the way a real robot is admitted: through its adapter,
  over mTLS, with the admission webhooks on;
- run a four-leg `FleetTask` on that robot, and cancelled it partway through.

Steps 1 and 3 each create a local Kubernetes cluster with `kind`, and the page deletes
each one when you are done with it. Nothing is installed outside the clusters, your Go build cache and the Python
environment in step 1.

The page was tested on macOS on arm64, with Docker 29.9.2 (8 CPUs and 21 GiB of
memory for Docker), `kind` 0.33.0, `kubectl` 1.36.1, `helm` 4.3.0, Go 1.26.4 and
Python 3.14.8. That computer had built these images before, so Docker's and Go's build caches were
warm, and the whole page took 7 minutes. A first run on a computer with empty caches
takes longer; it was not measured, and neither was a smaller machine or another
operating system.

## Before you start

You need:

- Docker, running;
- `kind`, `kubectl`, `make`, `helm`, and Go 1.26 or later;
- `python3`, with the `venv` module;
- network access: the runs pull cert-manager from its release page and Python packages
  from the Python Package Index.

On macOS, `make setup-macos` installs Go, `kubectl`, `kind` and Docker Desktop, among
others. It does not start Docker, and it installs neither `helm` nor the Python packages:

```bash
make setup-macos
brew install helm
```

If it installed Go, open another terminal before you continue. On Linux, install the
same tools with the distribution's package manager. On Windows, run every step inside
WSL2, with Docker Desktop's WSL2 backend. The [quickstart](../quickstart.md#prerequisites)
lists the prerequisites per operating system.

Get the source at the release tag and run every command on this page from the
repository root:

```bash
git clone --branch v0.4.1 https://github.com/swarmada/swarmada.git
cd swarmada
```

## Step 1: watch the control plane assign work

The simulated robot's adapter runs with the `python3` that comes first on your `PATH`.
It needs three packages, so create a virtual environment for them:

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install grpcio protobuf pyyaml
make quickstart
```

The run creates a `kind` cluster named `swarmada-quickstart`, builds the controller
image, installs the control plane and a sample fleet of three robots, and starts one
simulation adapter process. On a terminal it asks for a scenario first: press Enter for
`healthy-fleet`. It pauses once before the simulated robot starts: press Enter again.

The run took 3 minutes on the test computer. It ends with these lines and leaves the
cluster running:

```text
✅ Quickstart end state reached: all robots Idle, the real scheduler
   assigned a FleetAction in warehouse-a, and the live robot completed one.
```

When the run ends, it also stops the simulation adapter process it started, so the
robot reports nothing more after that. Look at what it made:

```bash
kubectl get robots,fleetactions,fleettasks -n warehouse-a
```

```text
NAME                              PHASE   ZONE          BATTERY   ACTION   AGE
robot.swarmada.io/sim-robot-001   Idle    warehouse-a   99                 2m
robot.swarmada.io/sim-robot-002   Idle    warehouse-a   85                 2m
robot.swarmada.io/sim-robot-003   Idle    warehouse-a   85                 2m

NAME                                                       TYPE       VERB   PHASE       OUTCOME   PRIORITY   ROBOT           ZONE          AGE
fleetaction.swarmada.io/deliver-pallet-001                 Navigate          Succeeded             High       sim-robot-001   warehouse-a   2m
fleetaction.swarmada.io/inspect-receiving-dock             Navigate          Succeeded             Normal     sim-robot-001   warehouse-a   2m
fleetaction.swarmada.io/receiving-round-001-inspect-dock   Navigate          Succeeded             Normal     sim-robot-001   warehouse-a   2m

NAME                                        PHASE       ACTIONS         DESIRED   AGE
fleettask.swarmada.io/receiving-round-001   Succeeded   1/1 Succeeded   Running   2m
```

Three `Robot`s read `Idle`. All three `FleetAction`s, and the one-step `FleetTask`
`receiving-round-001`, read `Succeeded` on `sim-robot-001`, the one robot with an
adapter. The other two robots have no adapter and report no position, so the scheduler
places neither of them in the zone and gives them no work. The ages and battery values
differ from run to run. The [scenario table](../quickstart.md#the-demos--scenarios)
lists what each scenario shows.

## Step 2: stop a robot

An emergency stop in Swarmada is confirmed from the robot's reported state, never
assumed. Run the drill on the same cluster:

```bash
make quickstart SCENARIO=estop-drill
```

It gives `sim-robot-001` work, stops it while it holds that work, and waits for the
adapter to report the robot at rest. It took 2 minutes and ends with:

```text
✅ Quickstart end state reached: sim-robot-001 was emergency-stopped while holding
   work, CONFIRMED the stop from simulator ground truth, and its action was
   paused; the rest of the fleet stayed healthy and scheduling continued.
```

The simulated robot reports its rest from the simulator's own state. A real robot
reports it from its odometry, and page 2 covers what that needs. A software stop does
not replace a physical stop switch on a real robot.

Step 1 applied the robots from a file, with the admission webhooks off. That is not how
a robot arrives in a real installation; step 3 is. Delete this cluster, and any
adapter process a run left behind, then leave the virtual environment:

```bash
bash examples/warehouse-quickstart/run.sh --clean
deactivate
```

## Step 3: admit a robot the way a real one is admitted

```bash
make first-run
```

This creates a `kind` cluster named `swarmada-first-run` and installs the Helm chart
with the admission webhooks and `ControlStream` mTLS on, as a real installation runs.
A simulation adapter runs inside the cluster, so no Python packages are needed. The
script then takes the robot through the same steps your own robot takes on page 2:

1. The adapter connects. Its `FleetAdapter` reads `Connected`, and its conformance
   result reads `Passed`.
2. The adapter announces the robot it serves, which appears as a `DiscoveredRobot`.
3. `swarmctl admit` admits the robot with its class, and the `Robot` reads `Idle`.
4. A two-step `FleetTask` runs on it to `Succeeded`.

It took 1.5 minutes and ends with a line that starts `PASS:`. It also builds the
`swarmctl` command line tool into `bin/`, which step 4 uses. The script keeps a
kubeconfig file of its own, so add the cluster to yours, and put `bin/` on your `PATH`:

```bash
kind export kubeconfig --name swarmada-first-run
export PATH="$PWD/bin:$PATH"
kubectl get fleetadapter,robot,fleettask -n warehouse-a
```

```text
NAME                                         VENDOR       PHASE       CONFORMANCE   ROBOTS   AGE
fleetadapter.swarmada.io/sim-fleet-adapter   simulation   Connected   Passed        1        22s

NAME                              PHASE   ZONE          BATTERY   ACTION   AGE
robot.swarmada.io/sim-robot-001   Idle    warehouse-a   99                 18s

NAME                              PHASE       ACTIONS         DESIRED   AGE
fleettask.swarmada.io/first-run   Succeeded   2/2 Succeeded   Running   15s
```

[First run](../../examples/first-run/README.md) lists what each step checks.

## Step 4: run a task, and cancel one

[`examples/hello-world`](../../examples/hello-world/README.md) holds a `FleetTask` of
four legs: from the base to points A, B and C, then back to the base. Each leg starts
once the one before it has `Succeeded`. Apply it and wait for it to finish:

```bash
kubectl apply -f examples/hello-world/hello-world-task.yaml
kubectl wait fleettask/hello-world -n warehouse-a \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s
kubectl get fleettask hello-world -n warehouse-a
```

```text
NAME          PHASE       ACTIONS         DESIRED   AGE
hello-world   Succeeded   4/4 Succeeded   Running   15s
```

On the test computer the four legs took 15 seconds in all. Each leg is a `FleetAction`
of its own: `kubectl get fleetaction -n warehouse-a` lists them.

To see a cancel, delete the task, apply it again and cancel it at once. `--yes` skips
the confirmation question:

```bash
kubectl delete fleettask hello-world -n warehouse-a
kubectl apply -f examples/hello-world/hello-world-task.yaml
swarmctl cancel task hello-world -n warehouse-a --yes
kubectl get fleettask hello-world -n warehouse-a
```

```text
NAME          PHASE     ACTIONS         DESIRED     AGE
hello-world   Running   0/4 Succeeded   Cancelled   0s
```

`DESIRED` reads `Cancelled` at once; the phase follows once the robot has stopped. The
leg that is running stops, and the legs after it do not start. Wait for the end:

```bash
kubectl wait fleettask/hello-world -n warehouse-a \
  --for=jsonpath='{.status.phase}'=Compensated --timeout=60s
```

The task's `failurePolicy` is `Compensate`, so it ends `Compensated`, and what the robot
does depends on when the cancel arrived:

- **During leg 1:** there is nothing to undo, and the robot stays where it stopped.
  The task ends with `0/4 Succeeded`. This is what happened on the test computer.
- **After leg 1 has succeeded:** leg 1's undo drives the robot to the base, and the
  task ends with, for example, `1/4 Succeeded`. This case was not reproduced on the test
  computer.

To send the robot to the base after a cancel during leg 1, wait until no other task
holds it (`kubectl get fleetaction -n warehouse-a`), then apply the one-leg task:

```bash
kubectl apply -f examples/hello-world/go-to-base-task.yaml
kubectl wait fleettask/go-to-base -n warehouse-a \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=60s
```

## Clean up

```bash
kind delete cluster --name swarmada-first-run
```

## What this page shows, and what it does not

**Shown:** the control plane, the scheduler, admission with webhooks and mTLS, a
confirmed emergency stop, and a task with dependent legs, a cancel and an undo. None of
it is mocked.

**Not shown:**

- **Motion.** The simulated robot is a two-dimensional model with no obstacles, map or
  sensors. It tests the fleet layer, not navigation.
- **A real robot.** Every conformance result recorded for Swarmada was earned in
  simulation; none on hardware. A passing conformance report shows that an adapter
  speaks the protocol correctly, not that the robot behind it works.
- **Report signing.** The first-run sample turns signature verification off; the report
  is still checked against its digest.
- **More than one replica** of the control plane.

[Known limitations](../known-limitations.md) lists the rest. Page 2 connects a ROS 2
robot to a cluster like the one in step 3.
