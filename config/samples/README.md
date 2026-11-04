# Samples, and the one thing they assume

These files are fixtures for the demos and runbooks. Most of them are inert declarations
that work on any control plane. **The ones that declare a `Robot` are not**, and that is
worth reading before the first one fails. So the samples come in four groups:

| Files | For | Apply with |
| --- | --- | --- |
| [`first-run/`](first-run/) | a control plane with admission webhooks **on** and `ControlStream` authorization on: the Helm chart's defaults, `make deploy`, `make first-run` | `kubectl apply -k config/samples/first-run`, then discovery and `swarmctl admit` (below) |
| `demo_a.yaml`, `robot_sample.yaml`, `fleetzone_edge.yaml` | a control plane with admission webhooks **off**: the quickstart overlays, `make deploy-minimal`, `make run`, the Helm chart with `webhooks.enabled=false`. Each declares `Robot` objects from a file, which only works with webhooks off. Those installs check nothing at admission: not the estop verbs, not the `robots/status` guard, not the refusal to delete a `Robot` that holds a stop ([known limitations](../../docs/known-limitations.md#an-install-without-admission-webhooks-checks-nothing-at-admission)) | `kubectl apply -f <file>` |
| `robotclass_defaulting_demo.yaml` | webhooks **on**; applied twice, around the discovery path (below and in its header) | `kubectl apply -f <file>` |
| every other file | either posture; none declares a `Robot` | `kubectl apply -f <file>` |

Every file in this directory uses the namespace `warehouse-a`; create it first. To apply
the whole directory to a webhooks-off control plane, leave out the one document that needs
webhooks on (the bare `Robot` in `robotclass_defaulting_demo.yaml`, which carries the
label `swarmada.io/sample-posture: webhooks-on`):

```
kubectl apply -f config/samples/ -l 'swarmada.io/sample-posture!=webhooks-on'
```

A plain `kubectl apply -f config/samples/` mixes the postures and fails on that `Robot`.
There is no whole-directory apply for a webhooks-on control plane: there, use
`first-run/`.

## The secure first run: `first-run/`

`first-run/` declares what a namespace needs before a robot can be admitted with the
webhooks on, and no `Robot`:

- a `SwarmadaConfig`, with signature verification off and the reason in a comment;
- the `RobotClass` `simbot-v1`, which the Robot admission gate resolves;
- a `FleetZone`, the terminal zone the robot is placed in;
- the `FleetAdapter` `sim-fleet-adapter`, serving `simbot-v1`, with a conformance report
  the conformance harness wrote, pinned by digest;
- `fleettask.yaml`, applied separately once a robot has been admitted.

The robot arrives by discovery, as described below. The commands run from the repository root
against the control plane `make first-run` installs: the Helm chart with webhooks and
ControlStream mTLS, and the simulation adapter running in `swarmada-system`. On a cluster where
`make first-run` has already run, delete its namespace first and restart the adapter. The chart
and the adapter stay, but a connected adapter does not announce its robot again once the robot's
`Robot` is gone:

```
kubectl delete namespace warehouse-a --wait
kubectl -n swarmada-system rollout restart deployment/sim-fleet-adapter
kubectl -n swarmada-system rollout status deployment/sim-fleet-adapter --timeout=120s
```

`make swarmctl` builds the CLI into `bin/`, and the `export` puts it on `PATH` for this shell and
for the `swarmctl` commands further down:

```
make swarmctl
export PATH="$PWD/bin:$PATH"
kubectl create namespace warehouse-a
kubectl apply -k config/samples/first-run
kubectl wait --for=create discoveredrobot/sim-robot-001 -n warehouse-a --timeout=120s
kubectl get discoveredrobots -n warehouse-a
swarmctl admit robot sim-robot-001 -n warehouse-a --zone warehouse-a \
  --adapter sim-fleet-adapter --class simbot-v1 --yes
kubectl apply -f config/samples/first-run/fleettask.yaml
```

`make first-run` does all of it on a new kind cluster, with the simulation adapter
running in the cluster, and waits for the `FleetTask` to reach `Succeeded`.

## Two ways a Robot comes under management, and only one works from a file

A `Robot` object may be created directly, or it may be **admitted** from a robot the fleet
discovered. Which one is available depends on whether the admission webhooks are serving.

### Webhooks OFF — a `Robot` may be applied from a file

This is what the quickstart overlays and the minimal deploy ship: `config/default`
(`make deploy-minimal`), `config/overlays/quickstart-mtls`, `quickstart-projected`,
`quickstart-dev`, and `config/manager/manager.yaml` itself all set `ENABLE_WEBHOOKS=false`,
because the admission webhooks need serving certificates those overlays do not provision
(see `docs/deploy-minikube.md`). `make run` runs the manager as a local process with
`ENABLE_WEBHOOKS=false`, and the Helm chart installs none with `webhooks.enabled=false`.
`config/controlstream-tls` (`make deploy`) and the Helm chart's default turn the webhooks on.
With no `vrobot.swarmada.io` in the request path, a `Robot` document is stored as written and
its bound `FleetAdapter` may connect at any time afterwards. The same absence opens more than
that: on all four installs, `patch` is enough to trigger or clear an estop, anyone who can
write `robots/status` can set a stopped robot `Normal`, and a `Robot` that holds a stop can be
deleted and created again `Normal`. Use them on a single-user development cluster only
([known limitations](../../docs/known-limitations.md#an-install-without-admission-webhooks-checks-nothing-at-admission)).

**`demo_a.yaml`, `robot_sample.yaml` and `fleetzone_edge.yaml` pre-declare `Robot` objects
and assume this mode.** It is
not the *intended* production posture, which is the next section.

### Webhooks ON — a `Robot` may not be applied from a file

`RobotAdmissionGate` (`internal/webhook/robot_webhook.go`) refuses a `Robot` whose named
`FleetAdapter` is not simultaneously present, serving the robot's class, `phase=Connected`
and `conformance=Passed`. Its rationale is written down and is a physical-safety argument:
*"a Robot is not 'real' until a conformant adapter can actually drive it."* An adapter
reaches `Connected` only by a live process registering over the ControlStream, so on a
freshly-created namespace no `Robot` in any file can be created:

```
The Robot "bare-robot-001" is invalid: spec.adapter.name: Invalid value: "sim-fleet-adapter":
  FleetAdapter "sim-fleet-adapter" is not ready: phase=Pending conformance=Passed
  (want Connected/Passed; RFC-0001 #fleetadapter-admission-gating)
```

With no `FleetAdapter` object at all, the reason reads `FleetAdapter "sim-fleet-adapter" not found
in namespace "warehouse-a"` instead. *(Both re-measured against `config/overlays/demo-webhooks` on
2026-09-23.)*

**The path that works is discovery**, and it is the one the design intends:

1. Deploy with webhooks serving — the Helm chart, `make deploy`, or
   `config/overlays/demo-webhooks`.
2. Start the adapter. It opens its ControlStream and `Register`s the robot.
3. The control plane answers `NOT_ADMITTED`, the adapter announces with `DiscoverRobot`,
   and a `DiscoveredRobot` appears:

   ```
   kubectl get discoveredrobots -n warehouse-a
   ```

4. An operator admits it, which is when the `Robot` is created — at a moment when its
   adapter demonstrably *is* Connected:

   ```
   swarmctl admit robot <discovered-name> -n warehouse-a \
     --zone warehouse-a --adapter sim-fleet-adapter --class simbot-v1 --yes
   ```

   `--class` is not optional here. The `sim-fleet-adapter` in `config/samples/first-run`
   serves only `simbot-v1`, and the class pins the adapter version the Robot records: the
   simulation adapter reports `0.1.0-sim`, which the Robot CRD's `X.Y.Z` pattern refuses.
   Without it `admit` refuses the command and names the flag, rather than recording an
   admission that cannot complete. With `config/overlays/demo-webhooks` the adapter serves
   `amr-standard` instead (the class `robotclass_defaulting_demo.yaml` creates, below); for
   any setup, `kubectl get fleetadapter sim-fleet-adapter -n warehouse-a -o
   jsonpath='{.spec.servesRobotClasses}'` names the class to pass.

   `admit` is **level-triggered**: it writes a `swarmada.io/admit` annotation and the
   DiscoveredRobot controller retries until the adapter is ready. An admit issued while the
   adapter is still `Disconnected` completes on its own once the adapter returns, so there
   is no timing window to hit. While it waits, each refused attempt is recorded as an
   `AdmissionFailed` event on the `DiscoveredRobot`, with the reason
   (`kubectl describe discoveredrobot <discovered-name> -n warehouse-a`).

## Why the webhooks-off samples do not use the discovery path

Because a YAML file cannot. Steps 2 and 4 are a running process and an operator command;
there is nothing to `kubectl apply`. A `Robot` document in a sample **is** the webhooks-off
shortcut, and the honest thing is to say so rather than to ship a file that works only on a
deployment mode its reader did not choose.

So each file whose `Robot` documents shortcut discovery carries a header naming this
dependency, and the demos that apply them
(`examples/warehouse-quickstart`, `examples/demo-ab-live`, `examples/full-surface-demo`)
run against webhooks-off overlays for the same reason. `first-run/` is the set for the
discovery path.

## Which samples declare a Robot

Re-derived from the tree, 2026-09-25:

```
$ grep -rln '^kind: Robot$' config/samples/
config/samples/robotclass_defaulting_demo.yaml
config/samples/demo_a.yaml
config/samples/fleetzone_edge.yaml
config/samples/robot_sample.yaml
```

The last three assume **webhooks off**. `robotclass_defaulting_demo.yaml` is the
exception and inverts the assumption: it exists to demonstrate the *mutating* defaulter, so
it requires webhooks ON and carries its own header saying so.

Because that file needs webhooks on, its `Robot` is subject to the gate above like any other.
So it is applied **twice**, with the discovery path in between:

1. `kubectl apply -f config/samples/robotclass_defaulting_demo.yaml` creates the
   `SwarmadaConfig`, `FleetZone` and `RobotClass`. The `Robot` is refused with the
   `not found` reason above. That is expected.
2. Bring `sim-fleet-adapter` under management by the discovery path: start the adapter, wait
   for the `DiscoveredRobot`, then `swarmctl admit robot <name> -n warehouse-a --zone
   warehouse-a --class amr-standard --yes`.
3. Apply the same file again. The three objects are `unchanged`, and the `Robot` is created,
   carrying the class's hardware, capabilities, adapter, manufacturer, model and telemetry
   interval, none of which is in the file.

That is the only order that works with webhooks on, and it was run in that order on
2026-09-23.
