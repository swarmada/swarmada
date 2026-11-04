# `plant.yaml` — what a robot's own stack says about it

> **Draft, non-normative.** This document describes a file format and the rules a tool
> that writes it follows. No conformance check reads `plant.yaml` yet, no profile
> requires one, and nothing here changes a check, a profile, or the
> `ConformanceReport` schema. The mapping to report rows in the last section is the
> intended input to a per-capability report; it is not implemented.

A conformance result is a statement about a pair: an adapter, and the robot it drives.
The suite measures the adapter over the wire. What it cannot measure over the wire is
the robot itself: how fast it may go, how it decelerates, where its sensors sit, which
of its channels measure motion and which echo the command. `plant.yaml` records those
facts, each with where it came from, so that a report can say which of its results
rest on the robot and which on an assumption.

The **plant** is the physical (or simulated) robot and the software stack on it that
the adapter talks to. `plant.yaml` is a **draft** until a person has reviewed it; it is
written by a tool, and a tool can only say what it read.

## The one rule: no field holds a default

Every field is exactly one of:

| `kind` | meaning | required keys |
| :-- | :-- | :-- |
| `read` | taken from the running stack — a topic, a node parameter, a transform, a file | `value`, `source` |
| `measured` | computed from observations the tool itself timed (a rate, a displacement) | `value`, `source` |
| `derived` | computed from other fields; `source` names them and the rule applied | `value`, `source` |
| `operator` | typed by a person; `prompt` records the question they answered | `value`, `source: operator`, `prompt` |
| — | **`value: unknown`** — could not be read | `reason` |

An `unknown` carries a `reason` and never a `source`. A value never appears without a
`source`. A writer refuses to emit a document that breaks either rule, and a bare number
anywhere outside a field (outside the tool's own run metadata) is refused as well:
`max_velocity: 0.5` with no source is indistinguishable from a value that was never
read, and that is the failure this format exists to prevent.

Two consequences follow, and both are deliberate:

- **A failed read is `unknown`, not zero.** A ROS 2 parameter that is not set on a node
  comes back typed `NOT_SET` with every numeric slot zero-filled. A tool that reads
  the numeric slot without checking the type produces `0.0`, a number nobody measured.
  The reference tool refuses it and records `unknown — parameter is not set on the node`.
- **A file is not the running configuration.** A parameter file found on disk says what a
  launch *could* load. Values from files are listed as `file_candidates`, each with its
  path and md5, and are never copied into a live field. Only an answer from the running
  node (`GetParameters`) fills a live field.

An `unknown` may also carry `status` and `unlock` (below) when the reason is that the
thing is not testable in this setting.

## `source` strings

A `source` names the exact place a value came from, in a form someone else can re-read:

| prefix | example |
| :-- | :-- |
| `param:` | `param: /velocity_smoother max_velocity` |
| `topic:` | `topic: /battery_state voltage, median of 10` |
| `tf:` | `tf: base_link -> lidar_frame` |
| `urdf joints:` | `urdf joints: base_footprint -> base_link -> lidar_frame` |
| `file:` | `file: /opt/.../nav2_params.yaml (md5 1d6f…)` |
| `graph:` | `graph: node /fake_robot is the testbed's simulated plant` |
| `env:` | `env: ROS_DISTRO` |
| `measured:` | `measured: arrival times at the probe over a 10.0 s window` |
| `operator` | with `prompt:` holding the question as it was shown |

## Sections

Field names below are paths into the document. `<topic>` and `<frame>` stand for the
names the stack reported.

### `plant`

| field | kinds | notes |
| :-- | :-- | :-- |
| `plant.kind` | `read` · `operator` · unknown | `simulated` when a simulator marker is read (a known simulated-plant node, a `/diagnostics` status declaring `simulated=true`, a `/clock` publisher). `hardware` only ever as `operator`: no reading proves a plant is physical. An operator's `hardware` over a simulator marker is `unknown` with the conflict as the reason. |

A document whose `plant.kind` is `simulated` can never support a hardware claim, however
many of its fields are filled.

### `ros`

`distro`, `rmw`, `rmw_env`, `domain_id`, `nodes`, `topics` (per topic: `types`,
`publishers`, `subscribers`), and `node_discovery` — `derived: inconsistent` when the node
list is empty while topics have publishers, which happens across mismatched ROS 2
distributions. When it is inconsistent, a node's absence from the list is not evidence
that the node is absent.

### `rates`

One field per odometry, laser scan, IMU, battery and joint-state topic that has a
publisher, plus fixed rows for `/odom`, `/scan`, `/battery_state` and `/joint_states`
whether present or not, so that an absence is stated. A `measured` rate carries:

- `hz`, `n`, and `period_ms` (`median`, `p99`, `max`, `min`, `sd`) from **arrival** times;
- `by_header_stamp` — the same statistics from the messages' own stamps;
- `receive_minus_stamp_s` — how far the stamps sit from the receiving clock.

Fewer than three messages in the window is `unknown` (no rate), with the publisher list
in the reason: a topic that has a publisher and delivers nothing is a finding, not 0 Hz.

### `tf` and `urdf`

`tf.frames`, `tf.base_frames` (which of `base_link`, `base_footprint` exist),
`tf.lidar_frames` (read from each scan's `header.frame_id`), and
`tf.lidar_pose.<frame>_in_<base>` — `x y z` (m) and `roll pitch yaw` (rad) composed along
the transform chain, with the chain as the source. `urdf.description` and
`urdf.lidar_pose` are the same poses composed from the URDF's joints. Both are kept: when
they disagree, the disagreement is itself the finding.

### `nav2`

Live parameters only, each `param:`-sourced or `unknown`:

| field | parameter |
| :-- | :-- |
| `nav2.velocity_smoother.{max_velocity, min_velocity, max_accel, max_decel, feedback, odom_topic}` | `/velocity_smoother` |
| `nav2.controller_server.{controller_plugins, controller_frequency}` and per plugin id `<id>.{plugin, family, footprint_model.type, footprint_model.radius, min_obstacle_dist, max_vel_x}` | `/controller_server` |
| `nav2.{local,global}_costmap.{robot_radius, footprint, footprint_padding, robot_base_frame, inflation_layer.inflation_radius}` | `/{local,global}_costmap/{local,global}_costmap` |
| `nav2.{local,global}_costmap.geometry_in_force` | `derived`: `footprint` when it is a non-empty polygon, else `robot_radius` |
| `nav2.file_candidates` | list of `{file, md5, values}` — files, never the running configuration |

`<id>.family` names the local planner (`TEB`, `DWB`, `MPPI`, `RegulatedPurePursuit`, …)
from the plugin string.

`max_velocity` is the **smoother's** limit. A driver below the smoother may clamp lower,
and nothing in the ROS graph reports that. `plant.yaml` has no field for the platform's
effective speed ceiling, because it cannot be read without commanding motion.

### `battery`

`battery.voltage` and `battery.percentage` from `sensor_msgs/BatteryState` (NaN, the
message's own "not measured", stays `unknown`), or `unknown — /battery_state has 0
publishers`. `battery.other_battery_topics` lists topics with `battery` in the name and a
bare numeric type, with the raw value and `units: not declared by the message type; not
converted`: a tool does not guess whether a `UInt16` is millivolts.

### `clock`

`ros_time_utc`, `container_wall_utc`, `ros_minus_wall_s`, and, when the operator passes a
reference time taken on their own machine immediately before launch,
`container_minus_reference_s`. `clock.host_ntp` is always `unknown` from inside a
container, which shares the host's clock but cannot see its synchronisation state. The
operator records the host's `timedatectl` output beside the file.

### `motion_channel` — the guided test

A static reading cannot tell odometry that measures motion from odometry that echoes the
velocity command. With the robot powered and **no command published**, a person rests,
lays a hand on the robot without pushing (a control for their own body appearing in the
scan), pushes it about 10 cm, and lets go. Each window is timed and each prints one line
of instruction.

| field | values | rule |
| :-- | :-- | :-- |
| `motion_channel.odometry.<topic>` | `measured` · `command-derived` · unknown | `measured` if the pose moved ≥ 3 cm across the push. `command-derived` only if it moved < 5 mm **and** an independent witness shows the push happened (an IMU deviation in the push window that did not occur in the hand-on window, a scan change exceeding both rest and hand-on, or an operator-reported move ≥ 3 cm). Still odometry with no witness is `unknown`: the test cannot tell command-derived from not-pushed. Any non-zero velocity command during the test voids it. |
| `motion_channel.imu_refutes_rest.<topic>` | `refutes rest during the push` · `did not refute …` | An IMU can **refute** rest. It is never reported as confirming rest. |
| `motion_channel.scan_difference.<topic>` | the per-window distributions of consecutive-frame range difference, and whether the push separates from rest and from hand-on | A difference statistic, not a scan-matched velocity, and not claimed as a rest-confirming channel. |
| `motion_channel.operator_push_distance_cm` | `operator` | |

The thresholds (3 cm, 5 mm, the IMU deviation rule) belong to the test, are printed
beside every result, and are not properties of the plant.

### `geometry`

`geometry.physical_width_m` — always `unknown` from the stack (no ROS 2 interface reports
physical size; a costmap radius is configuration), answered by one tape measurement as
`operator`. `geometry.lidar_height_m` — asked only when neither TF nor URDF gave a lidar
pose.

## Not testable here

On a simulated plant the guided steps record nothing and ask nothing. Each guided field
is written as:

```yaml
value: unknown
reason: "not-testable-here: the plant is simulated; a hand push has no physical effect on it, so nothing was recorded"
status: not-testable-here
unlock: run plant_guided.py on the physical robot
```

## Intended use in a per-capability report (not implemented)

A per-capability report gives every capability one status — `measured-hardware`,
`measured-simulation`, `failed`, `not-supported-by-platform`, `not-testable-here`,
`not-tested-suite-gap` — and every status other than a measurement carries an `unlock:`,
the concrete thing that would make it testable. `plant.yaml` is the evidence those
statuses rest on:

| capability (catalog) | `plant.yaml` fields | how the status would follow |
| :-- | :-- | :-- |
| confirmed stop — `safety` (C5.2, C5.3) | `plant.kind`, `motion_channel.*` | `simulated` ⇒ at most `measured-simulation`. Every odometry topic `command-derived` and no other channel that measures motion independently of the command ⇒ `not-supported-by-platform`, `unlock: odometry derived from wheel encoders or another motion sensor, or an external motion witness`. A channel that refutes rest (IMU) forbids `STOPPED` while it refutes, and does not confirm it. |
| stopping interval and cancel-to-rest latency (hardware validation) | `nav2.velocity_smoother.max_velocity`, `max_decel`, `motion_channel.odometry.*` | The harness's cruise floor and deceleration are the plant's, not a simulator's. A declared constant that differs from the value read ⇒ the run is refused. `max_velocity` is the smoother's limit only (see `nav2`). Without an independent motion channel ⇒ `not-supported-by-platform`. |
| route feasibility (clearance) | `nav2.*_costmap.geometry_in_force`, `nav2.controller_server.<id>.footprint_model.*`, `min_obstacle_dist`, `geometry.physical_width_m` | The planner's radius, the controller's clearance and the physical width are reported side by side. Any of them `unknown` ⇒ `not-testable-here`, `unlock:` naming that field. |
| telemetry — battery (C6) | `battery.*` | `unknown — /battery_state has 0 publishers` ⇒ `not-supported-by-platform`, `unlock: a sensor_msgs/BatteryState publisher` (a raw numeric topic without declared units does not qualify). |
| telemetry — liveness (C6) | `rates.*` (`hz`, `period_ms.p99`, `max`) | The period distribution sets what "stale" can mean on this plant. A rate that is `unknown` ⇒ `not-testable-here`. |
| timestamps as evidence | `clock.*` | An unknown or large `container_minus_reference_s` ⇒ robot-side stamps are not ordered against the operator's records. Evidence rows use the operator's clock. |
| subject of the report | `plant.kind`, `ros.distro`, the whole document's md5 | The report's subject is adapter × plant. The report quotes the `plant.yaml` it ran against. |

## Reference tool

A reference probe for ROS 2 + Nav2 ships with the ROS 2 fleet adapter under
`testbed/probe/`:

- `plant_probe.py` — observe-only. It creates no publisher, sends no goal, sets no
  parameter, and lists its own node's publishers in the output as a live check.
- `plant_guided.py` — the timed hand-push windows and the typed measurements.

Both run with `rclpy` and the Python standard library on Python ≥ 3.10.
