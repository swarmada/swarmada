# Deploying Swarmada on minikube (developer / prototype)

This walks through running Swarmada as an actual **in-cluster container** on
`minikube` — building the controller-manager image, loading it into the
cluster, and deploying it with `kubectl`/`kustomize` — rather than the
`make run` local-process path in the main [README Quick Start](../README.md#quick-start).

This is the **Developer / Prototype** deployment topology described in
[RFC-0001 §5.1.4](../rfcs/dist/RFC-0001-core-spec.md#architecture-deployment-topology):
a single `minikube` cluster running every control-plane component, with
simulated robots (or a Fleet Adapter connecting over `localhost`/in-cluster).
It is not the single-site or multi-site production topology — no Zone
Controller edge node or Jetson-hosted Fleet Adapter is involved here.

> **OS scope:** macOS only for now (Apple Silicon and Intel). Linux and
> Windows setup scripts are planned closer to release. The `make`
> targets themselves (`docker-build`, `install`, `deploy`, ...) are
> OS-agnostic; only `scripts/setup-macos.sh` and `scripts/deploy-minikube.sh`
> are macOS-specific (mainly the Homebrew-based installs).

## The fast path: two scripts

If you're on macOS and just want it running:

```bash
git clone https://github.com/swarmada/swarmada
cd swarmada

scripts/setup-macos.sh       # one-time: Homebrew, Go, kubectl, minikube, Docker, protoc
# — start Docker Desktop if the script tells you to, then —
scripts/deploy-minikube.sh   # start minikube, build the image, load it, install CRDs, deploy
```

Equivalent `make` targets: `make setup-macos` and `make deploy-minikube`
(the latter accepts `MINIKUBE_PROFILE=` and `IMG=` overrides, e.g.
`make deploy-minikube IMG=my-tag:dev`).

Both scripts are idempotent (safe to re-run) and each step prints what it's
doing. The rest of this document explains what those scripts do and why, one
step at a time — read on if something fails, if you're not on macOS, or if
you'd rather run each step by hand.

## Prerequisites, installed by `scripts/setup-macos.sh`

The script installs, in order, checking whether each is already present
before acting:

1. **Homebrew** (if missing) — via the official install script.
2. **Xcode Command Line Tools** — provides `make`, `git`. If this is the
   first time, macOS pops up a GUI installer dialog; accept it, then re-run
   `scripts/setup-macos.sh` (the script exits after triggering this since the
   GUI install runs asynchronously).
3. **Go** (`brew install go`) — 1.22+.
4. **kubectl** (`brew install kubectl`).
5. **minikube** (`brew install minikube`).
6. **Docker Desktop** (`brew install --cask docker`) — installed but not
   started; you must open it once yourself (`open -a Docker`) and wait for
   the whale icon to show "running" before deploying, since minikube's
   Docker driver needs the daemon up.
7. **protoc**, the Protocol Buffers compiler (`brew install protobuf`) — the
   controller-manager imports generated proto stubs
   (`internal/controlstream` → `proto/fleet_adapter/v1`), which are
   `.gitignored` and must be regenerated locally before every container
   build. `make docker-build` regenerates them for you (via the `proto-go`
   target), but `protoc` itself must already be on your machine.
8. **protoc-gen-go** / **protoc-gen-go-grpc** — the Go codegen plugins,
   version-pinned to match the gRPC runtime in `go.mod` (see the Makefile's
   comment on `PROTOC_GEN_GO_VERSION`/`PROTOC_GEN_GO_GRPC_VERSION` — a
   mismatch here is what previously broke the bidi-streaming build).

**Deliberately not installed:** `grpcio-tools` (Python gRPC stubs, needed only
for adapter development, not for the Go controller-manager/Docker path).
Homebrew's `python3` is "externally managed" (PEP 668) and refuses a plain
`pip install`; the script won't silently choose `--break-system-packages` or
a virtualenv on your behalf. If you need it later: `pip install grpcio-tools
--break-system-packages`, or `make proto-py` after making that same call
yourself.

## Manual walkthrough (what the scripts automate)

## 1. Start the cluster

```bash
minikube start --profile swarmada-dev
kubectl config use-context swarmada-dev
```

## 2. Build the controller-manager image

The `Dockerfile` at the repo root is a multi-stage build (Go 1.26 builder →
distroless static, nonroot runtime) that compiles `cmd/manager`:

```bash
make docker-build IMG=swarmada-controller:dev
```

`IMG` defaults to `ghcr.io/swarmada/swarmada:latest` if you omit it — for a
local dev loop, tagging it something like `swarmada-controller:dev` avoids
any confusion with a real registry image.

`docker-build` depends on the `proto-go` target (Go stubs only — no Python
toolchain needed), so it regenerates `proto/fleet_adapter/v1/*.pb.go` before
invoking `docker build` — the Dockerfile's builder stage `COPY proto/
proto/`s that in and needs the generated files to exist (they're
`.gitignored`, not checked in; `.dockerignore` deliberately does **not**
exclude `proto/`, unlike most non-Go directories, for the same reason). If
this step fails with `protoc: command not found`, install it first
(`brew install protobuf` on macOS) and install the Go plugins
(`go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11` and
`google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1` — versions must match
the Makefile's pinned `PROTOC_GEN_GO_VERSION`/`PROTOC_GEN_GO_GRPC_VERSION`).
`scripts/setup-macos.sh` does all of this for you.

## 3. Load the image into minikube

`minikube`'s Kubernetes runtime does not see images built by your host
Docker daemon by default. Load it in explicitly:

```bash
minikube image load swarmada-controller:dev --profile swarmada-dev
```

(Alternative: `eval $(minikube -p swarmada-dev docker-env)` before step 2, so
`docker build` targets minikube's own Docker daemon directly — then step 3 is
unnecessary. Either works; `minikube image load` is more explicit about what's
happening and is what these instructions assume.)

## 4. Install the CRDs

```bash
make install
```

This applies the thirteen CRD manifests in `config/crd/bases/` directly
(no kustomize overlay needed for CRDs alone).

## 5. Deploy the controller-manager

```bash
make deploy IMG=swarmada-controller:dev
```

This runs `kubectl apply -k config/default/`, which:

- creates the `system` namespace and a `swarmada-manager` ServiceAccount
  (`config/manager/manager.yaml`),
- binds that ServiceAccount to the manager's `ClusterRole`
  (`config/rbac/manager_role_binding.yaml` — this and the Deployment itself
  did not exist before this guide was written; see the note below),
- deploys the `swarmada-controller-manager` Deployment, and
- swaps the placeholder `controller:latest` image for the `IMG` you built
  (kustomize's `images:` transformer in `config/default/kustomization.yaml`).

Verify it came up:

```bash
kubectl get pods -n system
kubectl logs -n system deployment/swarmada-controller-manager
```

## 6. Create resources

```bash
kubectl create namespace warehouse-a
kubectl apply -f config/samples/
kubectl get robots,fleettasks -n warehouse-a
```

## 7. Shut down

Stop things in the reverse order they were started, so nothing is left
reconciling against a cluster that's disappearing out from under it.

If any Fleet Adapter is connected (see
[Installing and running Fleet Adapters](adapters.md#running-an-adapter)),
stop it first — `Ctrl-C` in its terminal. It disconnects cleanly; the control
plane simply marks the robot's `FleetAdapter`/`Robot` status stale once its
heartbeat lapses, it does not need to be told in advance. If a
`kubectl port-forward` is running in another terminal, `Ctrl-C` that too (or
`kill %1` if it was backgrounded with `&`).

**To just stop working for now, keeping everything in place** (fastest to
resume — no rebuild, no re-`apply` needed next time):

```bash
minikube stop --profile swarmada-dev
```

This pauses the cluster's VM/container; `minikube start --profile swarmada-dev`
brings it back with the manager Deployment, CRDs, and any sample resources
you created all still there.

**To remove the sample resources but keep the manager and cluster running:**

```bash
kubectl delete namespace warehouse-a
```

**To fully tear down** the manager, CRDs, and RBAC from the cluster (cluster
itself stays up):

```bash
make undeploy
make uninstall
```

**To remove everything**, including the `minikube` cluster itself:

```bash
make undeploy
make uninstall
minikube delete --profile swarmada-dev
```

`minikube delete` also frees the disk image `minikube start` created: check
`minikube profile list` afterward to confirm no `swarmada-dev` profile
remains. There is no separate cleanup step for the Docker image loaded via
`minikube image load` — it goes away with the profile.

## Known limitation: admission webhooks are disabled in this overlay

`config/default/kustomization.yaml` deliberately **excludes**
`config/webhook/` and runs the manager with `ENABLE_WEBHOOKS=false`. The two
admission webhooks (`RobotDefaulter`, `RobotAdmissionGate` and friends) need
real TLS serving certs — normally provisioned by `cert-manager` issuing a
`Certificate` and injecting the CA bundle into the
`MutatingWebhookConfiguration`/`ValidatingWebhookConfiguration` objects in
`config/webhook/manifests.yaml`. That wiring doesn't exist yet in this repo
(no `config/certmanager/` overlay, no CA-injection annotations on the webhook
manifests). This quickstart keeps `make deploy` usable in the meantime by
running without webhooks — meaning `RobotClass`-to-`Robot` field merging and
the adapter-conformance admission gate do not run in this deployment mode.
`make run` (local process) has the same `ENABLE_WEBHOOKS=false` escape hatch
for the same reason, so this isn't a new limitation — it's the same one,
made honest for the in-cluster path too. Wiring `cert-manager` for a real
webhook-enabled deployment is tracked as follow-up work, not covered here.

## What was missing before this guide

`make deploy`/`make undeploy` referenced `config/default/` before this
change, but that overlay — and `config/manager/` (the Deployment manifest
itself) and a `ClusterRoleBinding` tying the manager's ServiceAccount to its
`ClusterRole` — did not exist. Only `make run` (local process) worked. This
guide was written alongside adding those three pieces, so it documents the
now-real flow, not an aspirational one. Two further bugs were found and fixed
validating this path: the `Dockerfile` didn't `COPY proto/` despite the
manager needing it, and `.dockerignore` excluded `proto/` from the build
context even after that fix — both are corrected now, and `docker-build`'s
proto dependency was narrowed to `proto-go` so it no longer requires the
Python toolchain.

## Other operating systems

This guide and `scripts/setup-macos.sh`/`scripts/deploy-minikube.sh` are
macOS-only right now. The underlying `make` targets
(`docker-build`, `install`, `deploy`, `proto-go`, ...) have no macOS-specific
logic — a Linux or Windows (WSL2) developer can follow the "Manual
walkthrough" section above directly, substituting their platform's package
manager for the `brew install` calls (e.g. `apt install`/`choco install`).
Dedicated `scripts/setup-linux.sh` and Windows/WSL2 instructions are planned
before public release, not yet written.
