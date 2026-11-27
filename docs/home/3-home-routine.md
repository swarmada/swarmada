# Write a task for a home routine

This page writes an evening round for the robot from page 2: it visits the front door,
the back door and the kitchen, then returns to its base. You run the round when you ask
for it, and then every evening at a set time.

**How this page was tested.** Every command ran against page 2's setup on one computer,
with the same ROS 2 stand-in for the robot. Steps marked **Not tested on hardware** were
not run on a physical robot.

## How a task is built

A `FleetTask` is a list of actions. Each action is one piece of work for one robot, and
the control plane runs it as a `FleetAction` of its own. The fields this page uses:

| Field | What it does |
| :---- | :----------- |
| `actions[].name` | Names the action inside the task. |
| `action.type` | The work. The ROS 2 adapter serves one type, `Navigate`. |
| `action.robotSelector` | The robot that does it, by name. |
| `action.payload` | For `Navigate`: `x` and `y` in metres, and optionally `yaw` in radians, in the robot's `map` frame. |
| `action.onFailure` | `Alert` leaves a failed action failed and raises an Event for a person to read. |
| `dependsOn` | The actions that come first. An action with none starts at once. |
| `failurePolicy` | What the task does when an action fails or the task is cancelled. `Compensate` runs the `compensation` of each action that had succeeded. |
| `compensation` | An action's undo. |

Write `y` as `"y"`: YAML reads a bare `y` as `true`.

## The evening round

Pick the four points on your robot's map. Each one is free floor, clear of walls and of
anything the robot must not touch. The numbers below are the test stand-in's, which has
no walls.

```bash
cat > evening-round.yaml <<'EOF'
apiVersion: swarmada.io/v1
kind: FleetTask
metadata:
  generateName: evening-round-
  namespace: home
  labels:
    routine: evening-round
spec:
  failurePolicy: Compensate
  actions:
  - name: front-door
    action:
      type: Navigate
      robotSelector: home-robot-1
      onFailure: Alert
      payload: {x: 3.0, "y": 0.0}
    compensation:
      type: Navigate
      robotSelector: home-robot-1
      onFailure: Alert
      payload: {x: 0.0, "y": 0.0}
  - name: back-door
    dependsOn: [front-door]
    action:
      type: Navigate
      robotSelector: home-robot-1
      onFailure: Alert
      payload: {x: 3.0, "y": 2.5}
  - name: kitchen
    dependsOn: [back-door]
    action:
      type: Navigate
      robotSelector: home-robot-1
      onFailure: Alert
      payload: {x: 1.0, "y": 2.5}
  - name: base
    dependsOn: [kitchen]
    action:
      type: Navigate
      robotSelector: home-robot-1
      onFailure: Alert
      payload: {x: 0.0, "y": 0.0}
EOF
```

`generateName` gives each run its own name, such as `evening-round-x7k2p`, so the same
file can run every evening. The `routine` label finds every run later. If any leg fails
or the round is cancelled after the front door, the front door's compensation drives
the robot back to its base.

Run it once, by hand. `kubectl create`, not `kubectl apply`, because only `create`
fills in a `generateName`. The first line keeps the round's name, and the second waits
for the round to end:

```bash
ROUND=$(kubectl create -f evening-round.yaml -o name)
kubectl wait "$ROUND" -n home --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
kubectl get fleettask -n home -l routine=evening-round
```

```text
fleettask.swarmada.io/evening-round-zm6bv condition met
NAME                  PHASE       ACTIONS         DESIRED   AGE
evening-round-zm6bv   Succeeded   4/4 Succeeded   Running   32s
```

On the stand-in the round took 32 seconds and ended `Succeeded`, `4/4 Succeeded`.

## Run it every evening

Swarmada itself has no clock trigger: nothing in it starts a task at a time of day. A
Kubernetes `CronJob` can: at the time you set, it starts a short job that creates the
task from the same file.

The job needs permission to create `FleetTask`s in namespace `home`, and nothing else.
Store the file in a `ConfigMap`, then create a `ServiceAccount` with that one permission
and the `CronJob`:

```bash
kubectl create configmap evening-round -n home --from-file=evening-round.yaml
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: routine-runner
  namespace: home
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: create-fleettasks
  namespace: home
rules:
- apiGroups: ["swarmada.io"]
  resources: ["fleettasks"]
  verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: routine-runner-create-fleettasks
  namespace: home
subjects:
- kind: ServiceAccount
  name: routine-runner
  namespace: home
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: create-fleettasks
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: evening-round
  namespace: home
spec:
  schedule: "0 21 * * *"
  timeZone: "Etc/UTC"
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 600
  jobTemplate:
    spec:
      backoffLimit: 0
      template:
        spec:
          serviceAccountName: routine-runner
          restartPolicy: Never
          containers:
          - name: create-task
            image: registry.k8s.io/kubectl:v1.37.0
            args: ["create", "-f", "/routine/evening-round.yaml"]
            volumeMounts:
            - name: routine
              mountPath: /routine
          volumes:
          - name: routine
            configMap:
              name: evening-round
EOF
```

- `schedule` is a cron expression: `0 21 * * *` is 21:00 every day. `timeZone` is the
  zone it is read in; set it to yours, for example `Europe/Paris`.
- `startingDeadlineSeconds: 600` lets a run that was missed by up to 10 minutes still
  start. A run missed by more, because the computer was asleep or the cluster was not
  running, is skipped.
- The job only creates the task; the round runs afterwards. If the robot is still busy
  with an earlier task, the round's first leg waits for it.
- `registry.k8s.io/kubectl` is the Kubernetes project's `kubectl` image. Keep its
  version within one minor version of your cluster's.

To try the schedule without waiting for 21:00, start one job from it by hand:

```bash
kubectl create job evening-round-now --from=cronjob/evening-round -n home
kubectl wait job/evening-round-now -n home --for=condition=Complete --timeout=120s
kubectl get fleettask -n home -l routine=evening-round
```

```text
job.batch/evening-round-now created
job.batch/evening-round-now condition met
NAME                  PHASE       ACTIONS         DESIRED   AGE
evening-round-ww24t   Running     0/4 Succeeded   Running   2s
evening-round-zm6bv   Succeeded   4/4 Succeeded   Running   37s
```

The wait for the schedule itself was not tested: the test started the job by hand, as
above.

## Check, stop and tidy up

To see each run, newest last:

```bash
kubectl get fleettask -n home -l routine=evening-round --sort-by=.metadata.creationTimestamp
```

To stop a round while it runs, cancel it by name. The robot stops its current leg. If the
front door leg had already succeeded, its compensation drives the robot back to its base;
if not, the robot stays where it stopped:

```bash
swarmctl cancel task <name> -n home --yes
```

For anything that must stop at once, use the stop switch or `swarmctl estop trigger`.

Each run stays in the cluster until you delete it. To delete every finished run:

```bash
kubectl delete fleettask -n home -l routine=evening-round
```

To stop the routine, delete the `CronJob`: `kubectl delete cronjob evening-round -n home`.

## Keeping the robot out of a room

`fleet-adapter-ros2` v1.3.0 declares no zone restriction that it honours, so Swarmada
cannot keep this robot out of a room. Do it on the robot, in Nav2, for example with a
keepout mask on its costmap. **Not tested on hardware**, and not tested on the stand-in,
which runs no Nav2.

## What this page shows, and what it does not

**Shown, on the stand-in:** a four-leg round run on demand, the same round created by a
`CronJob` job that holds only the permission to create tasks, and runs found and deleted
by label.

**Not shown:** the round on a physical robot, a schedule reached by the clock, rooms kept
out by Nav2, and what happens when the robot's battery runs low during a round.
