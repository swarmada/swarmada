# Hello World: a four-leg task on the simulated robot

Two `FleetTask` files to run once [`make first-run`](../first-run/) has admitted its
simulated robot, `sim-robot-001`:

- [`hello-world-task.yaml`](hello-world-task.yaml): four legs, each a `Navigate` to an
  x/y point, from the base to A, B, C and back to the base. Each leg starts once the one
  before it has `Succeeded`. Its `failurePolicy` is `Compensate`, and leg 1 carries an
  undo that drives to the base.
- [`go-to-base-task.yaml`](go-to-base-task.yaml): one leg, a `Navigate` to the base.

The comments in each file say what every field does, and what to change to run them on
your own robot.

## Before you start

Run the first run, then add its cluster to your kubeconfig, since the script uses a
kubeconfig file of its own:

```sh
make first-run
kind export kubeconfig --name swarmada-first-run
```

`make first-run` also builds `bin/swarmctl`. Run the commands below from the repository
root.

## Run the task

```sh
kubectl apply -f examples/hello-world/hello-world-task.yaml
kubectl get fleettask hello-world -n warehouse-a
```

Once the four legs have run, the task shows:

```
NAME          PHASE       ACTIONS         DESIRED   AGE
hello-world   Succeeded   4/4 Succeeded   Running   17s
```

The age differs from run to run. Each leg is a `FleetAction` of its own:
`kubectl get fleetaction -n warehouse-a` lists them.

## Finish early

To run the task again, delete it first, then apply it:

```sh
kubectl delete fleettask hello-world -n warehouse-a
kubectl apply -f examples/hello-world/hello-world-task.yaml
```

To finish while it runs, cancel it:

```sh
bin/swarmctl cancel task hello-world -n warehouse-a
```

`swarmctl` asks before it cancels; `--yes` skips the question. The leg that is running
stops, and the legs after it do not start.

- **Cancelled after leg 1 has succeeded:** leg 1's undo drives the robot to the base, and
  the task ends `Compensated`, for example with `1/4 Succeeded`.
- **Cancelled during leg 1:** there is nothing to undo yet. The task ends `Compensated`
  with `0/4 Succeeded`, and the robot stays where it stopped. To send it to the base,
  apply the one-leg task once no other task holds the robot (`kubectl get fleetaction -n
  warehouse-a`):

  ```sh
  kubectl apply -f examples/hello-world/go-to-base-task.yaml
  kubectl get fleettask go-to-base -n warehouse-a
  ```

  It ends `Succeeded`, `1/1 Succeeded`.

On the simulated robot a leg lasts about two seconds, so a cancel has to follow the
apply closely to land during a given leg.

## Clean up

```sh
kind delete cluster --name swarmada-first-run
```
