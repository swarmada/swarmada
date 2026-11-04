# First run: the secure path, end to end

```sh
make first-run
```

This brings up a new kind cluster and installs Swarmada in the posture the Helm chart
installs by default:

- **admission webhooks on**, so a `Robot` is admitted only while its adapter is
  connected and conformance-qualified; and
- **ControlStream authorization on**, over mTLS, so an adapter is trusted only for the
  `FleetAdapter` its client certificate names, and only for the robot classes that
  adapter serves.

It then connects a simulated adapter, admits the robot the adapter announces, and runs
a two-step `FleetTask` through it to `Succeeded`. Every step is run by the script. The
adapter is a Deployment the cluster starts from a manifest; nobody starts a process by
hand.

## What it does

| Step | What runs | What it checks |
| --- | --- | --- |
| 1 | `kind create cluster` (a new cluster; an existing one of the same name is refused) | |
| 2 | cert-manager v1.16.2 | its webhook answers a server-side dry run |
| 3 | `make docker-build`, `docker build -f Dockerfile.sim-adapter`, `kind load` | |
| 4 | `helm install swarmada deploy/swarmada -n swarmada-system` with one adapter client certificate | the rendered manager args carry the three ControlStream TLS flags and not `--fleet-adapter-insecure-authz`; the manager logs that it serves ControlStream with mTLS |
| 5 | `kubectl create namespace warehouse-a`, then the rendered `config/samples/first-run` in two phases: ConfigMap, SwarmadaConfig, RobotClass; then FleetZone, FleetAdapter | the CRDs are Established, the webhook Service has a ready endpoint, both webhook configurations carry the CA bundle, admission answers a server-side dry run; `swarmada-config` exists before phase 2 |
| 6 | `kubectl apply -k examples/first-run/sim-adapter` | the adapter's client certificate was issued |
| 7 | | `FleetAdapter` `Connected`/`Passed` with a contract version recorded; a `DiscoveredRobot` appears |
| 8 | `swarmctl admit robot sim-robot-001 --class simbot-v1` | the `Robot` exists, is `Idle`, and carries the class; the `FleetAdapter` lists its six action types (a `Custom` verb has an entry of its own, so it lists seven entries), and the robot holds every capability they require `Active` |
| 9 | `kubectl apply -f config/samples/first-run/fleettask.yaml` | the `FleetTask` reaches `Succeeded` |

The adapter runs with `--provisioning-interval=5`, a demo setting. Until a robot is
admitted, an adapter re-sends its registration on an interval, and admission takes effect
on the next attempt. RFC-0001 sets that interval to 60 seconds when the control plane
names no announce lifetime, so without the flag step 8 can wait up to a minute.

Needs `kind`, `kubectl`, `docker`, `helm`, `go` and `make`. The script uses a kubeconfig
file of its own, so it does not change your current context.

## Options

| Variable | Default | Meaning |
| --- | --- | --- |
| `FIRST_RUN_CLUSTER` | `swarmada-first-run` | kind cluster name |
| `FIRST_RUN_USE_EXISTING` | unset | `1` runs in an existing, empty kind cluster of that name instead of creating one |
| `FIRST_RUN_DELETE` | unset | `1` deletes the cluster at the end, if the script created it |
| `CERT_MANAGER_VERSION` | `v1.16.2` | cert-manager release |

On success the cluster is left running so you can look at it. Add its context to your
kubeconfig first, since the script did not:

```sh
kind export kubeconfig --name swarmada-first-run
kubectl get fleetadapter,robot,fleettask,fleetaction -n warehouse-a
kind delete cluster --name swarmada-first-run
```

## What it does not show

- **Signing.** The sample `SwarmadaConfig` turns report signature verification off, and
  says why in a comment. The report is still digest-verified.
- **More than one replica.** The chart refuses `replicaCount` above 1 while ControlStream
  is enabled; see `deploy/swarmada/README.md`.
