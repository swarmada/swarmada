# Swarmada at home

These three pages run Swarmada on one computer, connect one ROS 2 robot to it, and then
give that robot a routine. You need no cluster of your own: each page creates a local
Kubernetes cluster with `kind`, and deletes it when you are done.

| Page | You run | You need | You finish with |
| :--- | :------ | :------- | :-------------- |
| 1. [Run Swarmada with simulated robots](1-simulated-robots.md) | `make quickstart`, `make first-run` and a four-leg `FleetTask` | Docker, `kind`, `kubectl`, `make`, `helm`, Go 1.26 or later, `python3` | A simulated robot admitted over mTLS, and a task run and cancelled on it |
| 2. [Connect a ROS 2 hobby robot](2-ros2-robot.md) | The Helm chart on your computer, the ROS 2 Fleet Adapter on the robot | Page 1's tools, with Python 3.12 or later; a robot that runs ROS 2 Humble or Jazzy, Nav2 and a map; a physical stop switch | Your robot admitted, stopped at rest, and driven through four legs |
| 3. [Write a task for a home routine](3-home-routine.md) | A `FleetTask` and a Kubernetes `CronJob` | Page 2's setup | A round of your home that runs when you ask, and every evening at a set time |

On the test computer, page 1 took 7 minutes with warm build caches, and pages 2 and 3
took 3 and 2 minutes, with a ROS 2 container standing in for the robot. Each page says
what it was tested on.

## Limits, before you start

- **No hardware result.** Pages 2 and 3 were tested with a ROS 2 container standing in
  for the robot. Steps that only happen on a physical robot are marked **Not tested on
  hardware**. Every conformance result recorded for the ROS 2 adapter was earned in
  simulation.
- **Swarmada does not move the robot.** Nav2 on the robot plans and drives. A software
  stop does not replace a physical stop switch.
- **No clock trigger.** Nothing in Swarmada starts a task at a time of day. Page 3 uses
  a Kubernetes `CronJob` for that.

[Known limitations](../known-limitations.md) lists the rest. [Getting started](../getting-started.md)
orders the same steps as tiers, for any robot and any cluster.
