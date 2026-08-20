# Installing and running Fleet Adapters

A **Fleet Adapter** is the process that bridges one robot class's native
protocol (ROS 2, VDA5050, MAVLink, a vendor SDK, ...) to the standardized
[`fleet_adapter.v1`](../proto/fleet_adapter/v1/fleet_adapter.proto) gRPC
contract the control plane speaks. This document covers installing and
running the adapters that exist today — the in-tree simulation adapter and
the ROS 2, VDA5050, and MAVLink reference adapters — plus how to verify one
against the [conformance suite](../adapters/CONFORMANCE.md) and how to point
one at a real control plane.

See [`adapters/REGISTRY.md`](../adapters/REGISTRY.md) for what each adapter
covers and its current conformance result, and
[ADR-0005](adr/0005-reference-adapter-policy.md) for why reference adapters
for open standards live in the project org while vendor adapters live in
their own repository.

## Prerequisites

- Python 3.12+ (the floor used across the repository; see the root `pyproject.toml`).
- The generated protocol stubs: `make proto-py` from the repository root
  (writes to `proto/`).
- The Swarmada Python SDK, installed editable from the repository root:

  ```bash
  pip install -e sdk/python
  ```

  On a Homebrew-managed macOS Python, add `--break-system-packages`
  (PEP 668 blocks unmanaged installs into the system interpreter otherwise).

- The reference adapters (ROS 2, VDA5050, MAVLink) live in their **own repos**
  ([ADR-0005](adr/0005-reference-adapter-policy.md)), not in this tree. Clone the one
  you want into `adapters/external/` — the paths below assume it is checked out there:
  - [`github.com/swarmada/fleet-adapter-ros2`](https://github.com/swarmada/fleet-adapter-ros2)
  - [`github.com/swarmada/fleet-adapter-vda5050`](https://github.com/swarmada/fleet-adapter-vda5050)
  - [`github.com/swarmada/fleet-adapter-mavlink`](https://github.com/swarmada/fleet-adapter-mavlink)

  The in-tree `simulation` adapter needs no clone.

  The three reference adapters live in their own repositories. Clone them into
  `adapters/external/` (or `git -C <dir> pull --ff-only` to update):

  ```sh
  mkdir -p adapters/external && cd adapters/external
  git clone https://github.com/swarmada/fleet-adapter-ros2.git
  git clone https://github.com/swarmada/fleet-adapter-vda5050.git
  git clone https://github.com/swarmada/fleet-adapter-mavlink.git
  ```

  A `make adapters` convenience target that does this in one step is planned; it is
  not present in the Makefile today.

### If your machine has more than one Python interpreter

Check which `python3` a shell resolves to before installing anything:

```bash
which python3
```

The `sdk/python` install and each adapter's `pip install -e` must land in
**the same interpreter that will run the adapter and the conformance
harness** — a package installed into a different Python (for example, installing while your project `.venv` is not activated, so
`python3` resolves to a system or Homebrew install) is invisible to `python3 -m adapters.conformance` or to
`python3 <adapter>/adapter.py`, and fails with
`ModuleNotFoundError: No module named 'swarmada_sdk'` even though the import
succeeds from an interactive shell using the other interpreter. Confirm with:

```bash
python3 -c "import swarmada_sdk; print(swarmada_sdk.__file__)"
```

## Installing an adapter

Each reference adapter lives under `adapters/external/<name>/` and installs
the same way:

```bash
pip install -e adapters/external/fleet-adapter-ros2
pip install -e adapters/external/fleet-adapter-vda5050
pip install -e adapters/external/fleet-adapter-mavlink
```

Each adapter's default install has no dependency on the underlying runtime
(ROS 2, an MQTT broker, PX4) — it runs standalone against a **simulated
binding**, sufficient to pass the handshake and drive the whole conformance
suite. The real-runtime dependency is an optional extra:

```bash
# ROS 2 binding needs a sourced ROS 2 + Nav2 workspace, not a pip extra.
pip install -e 'adapters/external/fleet-adapter-vda5050[vda5050]'   # paho-mqtt
pip install -e 'adapters/external/fleet-adapter-mavlink[mavlink]'   # pymavlink
```

## Running an adapter

### Against a local test endpoint (no control plane needed)

Each adapter can be pointed at any `fleet_adapter.v1` gRPC endpoint, including
the conformance harness's own test server (see below) or a bare
`localhost:<port>` you are otherwise driving by hand:

```bash
# ROS 2 (simulated binding, no ROS 2 install required):
python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py \
    --endpoint localhost:9090

# VDA5050 (simulated protocol profile):
python3 adapters/external/fleet-adapter-vda5050/fleet_adapter_vda5050/adapter.py \
    --endpoint localhost:9090 --comms simulated

# MAVLink (simulated link):
python3 adapters/external/fleet-adapter-mavlink/fleet_adapter_mavlink/adapter.py \
    --endpoint localhost:9090 --link simulated
```

### Against a real, running control plane

Once a `swarmada-controller-manager` is deployed (see
[Deploying on minikube](deploy-minikube.md)), its `ControlStream`/
`SafetyStream` gRPC server listens in-cluster on `:9443`. From outside the
cluster, forward that port first:

```bash
kubectl port-forward -n system deployment/swarmada-controller-manager 9443:9443
```

Then point any adapter at it:

```bash
python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py \
    --endpoint localhost:9443
```

The port-forward runs in the foreground; run it in a separate terminal or
background it (`&`) before starting the adapter. Production connections use
mutual TLS with the adapter's client certificate as its identity
(RFC-0001 §9.5, Security Model); the in-cluster manager runs with
`ENABLE_WEBHOOKS=false` and no mTLS wiring yet, so this path is
for development only.

## Running an adapter's own tests

Each adapter ships unit tests for its safety wiring (fencing, confirmed
estop, lease self-stop) and, where applicable, its protocol-profile logic:

```bash
pytest adapters/external/fleet-adapter-ros2/tests/ -v
pytest adapters/external/fleet-adapter-vda5050/tests/ -v
pytest adapters/external/fleet-adapter-mavlink/tests/ -v
```

Or via each adapter's own `Makefile`: `make test` from inside the adapter's
directory (`adapters/external/<name>/`).

## Running the conformance suite

The [conformance harness](../adapters/conformance/README.md) starts its own
`fleet_adapter.v1` gRPC test server, launches the adapter under test against
it, drives it through the C0–C16 scenarios in
[`CONFORMANCE.md`](../adapters/CONFORMANCE.md), and reports pass/fail/skip
per check. It does not require a real Kubernetes control plane — it *is* the
control-plane side, for test purposes.

```bash
# In-tree simulation adapter (baseline; also usable as a smoke test of the
# harness itself):
make conformance-sim

# Any other adapter, in its default (simulated-binding) mode:
make conformance ADAPTER='python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py --endpoint localhost:{port}'
make conformance ADAPTER='python3 adapters/external/fleet-adapter-vda5050/fleet_adapter_vda5050/adapter.py --endpoint localhost:{port} --comms simulated'
make conformance ADAPTER='python3 adapters/external/fleet-adapter-mavlink/fleet_adapter_mavlink/adapter.py --endpoint localhost:{port} --link simulated'
```

`{port}` is substituted with the harness's `--port` (default `9090`). Add
`--json report.json` to also write a machine-readable report. A conforming
adapter has no failed MUST/MUST NOT among the checks that ran; a `skip` is
not a pass — it means the harness could not observe that behavior in this
run (e.g. a timer-driven, on-robot behavior like lease self-stop) and it must
be verified some other way (typically a unit test), not left unverified.

Record a new or updated result in [`adapters/REGISTRY.md`](../adapters/REGISTRY.md).

### Contract versions and re-qualification

A conformance result is bound to the **contract version** it was earned against —
the semver the harness stamps into the report as `contract_version`. That is a
different thing from the `fleet_adapter.v1` wire-package identity (an identity
string, which cannot express compatibility) and from the adapter's own build
version (which versions an implementation, not the contract). `make conformance`
prints it in the report header, and `adapters/REGISTRY.md` records it in the
| **Major** (`1.x.y` → `2.0.0`) | Re-run `make conformance` and update the **Contract version** column in [`adapters/REGISTRY.md`](../adapters/REGISTRY.md). A major bump is breaking, so a result earned against the previous major stops being binding: a control plane on the new major will not admit or dispatch to robots bound to that adapter until the row is updated. |

| Contract-version bump | What an adapter must do |
| :--- | :--- |
| **Major** (`1.x.y` → `2.0.0`) | Re-run `make conformance` and update the **Contract version** column in [`adapters/REGISTRY.md`](../adapters/REGISTRY.md). A major bump is breaking, so a result earned against the previous major stops being binding: a control plane on the new major will not admit or dispatch to robots bound to that adapter until the row is updated. |
| **Minor** (`1.0.y` → `1.1.0`) | Nothing. A control plane supports minor N and N-1, so an adapter one minor behind is still in range and its existing qualification stays valid. |
| **Patch** (`1.0.0` → `1.0.1`) | Nothing. The patch component is not considered in compatibility. |

An adapter that reports no `contract_version` at all is treated as incompatible,
not as compatible-by-default: its registrations are refused `VERSION_MISMATCH` and
its robots are not dispatchable. Telemetry, heartbeat, and emergency stop keep
working throughout — the gate is on work, never on observation or stopping.

## Shutting down

An adapter process holds no state the control plane depends on being
persisted — stopping it is always safe and requires no prior notice to the
control plane.

- **A single adapter run in the foreground:** `Ctrl-C`. The process exits and
  closes its `ControlStream`/`SafetyStream` connections; the control plane
  observes this as the robot(s) it registered going silent and marks them
  stale once their heartbeat/telemetry interval lapses (RFC-0001's staleness
  detection, not an explicit "goodbye" message from the adapter).
- **The conformance harness (`make conformance` / `make conformance-sim`):**
  it exits on its own once the scenario finishes; `Ctrl-C` stops it early if
  needed and the harness's own test server (not a real control plane) is
  torn down with it.
- **A `kubectl port-forward` used to reach an in-cluster control plane:**
  `Ctrl-C` in its terminal, or `kill %1` if it was backgrounded with `&`.
  Stopping the port-forward does not affect the control plane itself — only
  this adapter's path to it.
- **The control plane the adapter was connected to:** see
  [Deploying on minikube § Shut down](deploy-minikube.md#7-shut-down) for
  stopping or tearing down the cluster side.

If several adapters are running at once (e.g. driving multiple simulated
robot classes for a demo), stop each with `Ctrl-C` in its own terminal — there
is no single command that stops all adapters together, since each is an
independent process by design (no shared adapter-manager process to signal).

## Building a new adapter

Scaffold from the cookiecutter template, which generates a **safety-complete**
adapter wired to the audited `swarmada-sdk` safety primitives (fencing,
confirmed estop, lease self-stop) and passes the conformance handshake out of
the box:

```bash
pip install cookiecutter
cookiecutter adapters/template/
```

Then implement the `RobotBinding` seam for the target robot interface and run
the conformance suite until every non-skipped check passes.

> **Rename pending.** This seam is named `RobotBinding` (CLI flag `--binding`) in the
> current code. It identifies a robot-side *transport* — not the `Robot.spec.adapter`
> binding, and not a Kubernetes `RoleBinding` — and is filed for rename to
> `RobotTransport` (`--transport`). The current names remain correct until that lands.
