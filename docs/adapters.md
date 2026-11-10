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

- Python 3.10 or newer for the SDK and an adapter (`sdk/python/pyproject.toml`). The
  repository's own `swarmada` package — simulation, ML and tooling, the root
  `pyproject.toml` — needs Python 3.12 or newer.
- The generated protocol stubs, committed under `proto/`. Regenerate them with
  `make proto-py` only after changing a `.proto` file; that needs uv, or
  `pip install '.[codegen]'`.
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

  The Makefile has no target that clones them: run the three commands above.

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

**`make deploy-minimal` serves no `ControlStream`.** It applies `config/default`, whose
manager carries none of the three `--fleet-adapter-tls-*` flags and not
`--fleet-adapter-insecure-authz` (the manager refuses that flag together with the TLS
flags). With neither, the manager logs
`NOT serving ControlStream (fail closed)` at startup and nothing listens for adapters
(`controlStreamShouldServe` in `cmd/manager/main.go`). An adapter pointed at a
port-forward of that deployment has nothing to connect to.

`make deploy` applies `config/controlstream-tls`, the first overlay below. Deploy one
of the overlays that turns the `ControlStream` on. Both need
[cert-manager](https://cert-manager.io) installed first, and both add a
`swarmada-controlstream` Service in the `system` namespace on port `9445`:

| Overlay | What it serves | Use |
| :--- | :--- | :--- |
| `config/controlstream-tls` | mTLS with per-robot authorization against the adapter's certificate identity; the admission webhooks enabled, with a cert-manager serving certificate | a cluster that should enforce adapter identity |
| `config/overlays/quickstart-mtls` | mTLS with per-robot authorization, and the admission webhooks disabled; it also issues a client certificate for an adapter named `sim-fleet-adapter` in `warehouse-a` | dev/demo only; the path `examples/warehouse-quickstart/run.sh` uses |

```bash
kubectl apply -k config/controlstream-tls --server-side --force-conflicts
kubectl port-forward -n system service/swarmada-controlstream 9445:9445
```

The port-forward runs in the foreground; run it in a separate terminal or
background it (`&`) before starting the adapter. The adapter then dials
`localhost:9445` and must present a client certificate issued by the overlay's
`controlstream-ca-issuer`: both overlays require and verify a client certificate
(TLS 1.3), so a connection without one fails the handshake. The control plane takes
the adapter's `FleetAdapter` name from that certificate's SAN
(`<adapter>.<namespace>.svc.cluster.local`). Through a port-forward the dialled
address is `127.0.0.1`, which the server certificate does not name, so the adapter
has to verify against `swarmada-controlstream.system.svc`. The in-tree simulation
adapter takes all of this as flags:

```bash
PYTHONPATH=proto python3 adapters/simulation/sim_adapter.py --endpoint localhost:9445 \
    --namespace warehouse-a --robot-id sim-robot-001 --zone warehouse-a \
    --tls-ca ca.crt --tls-cert tls.crt --tls-key tls.key \
    --tls-server-name swarmada-controlstream.system.svc
```

Under `quickstart-mtls` the three files are the `ca.crt`, `tls.crt` and `tls.key` keys
of the Secret `sim-fleet-adapter-client-tls` in `system`
(`kubectl -n system get secret sim-fleet-adapter-client-tls -o jsonpath='{.data.tls\.crt}' | base64 -d > tls.crt`,
and likewise for the other two). `config/controlstream-tls` issues no client
certificate: create a cert-manager `Certificate` in `system` (the issuer is namespaced
there) with the `client auth` usage against `controlstream-ca-issuer` for each adapter. How a reference adapter in
`adapters/external/` takes its certificate is defined in that adapter's own
repository.

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
it, drives it through the C0–C19 scenarios in
[`CONFORMANCE.md`](../adapters/CONFORMANCE.md), and reports pass/fail/skip
per check. It does not require a real Kubernetes control plane — it *is* the
control-plane side, for test purposes.

```bash
# In-tree simulation adapter (baseline; also usable as a smoke test of the
# harness itself):
make conformance-sim

# Any other adapter, in its default (simulated-binding) mode:
make conformance ADAPTER='python3 adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py --endpoint localhost:{port}'
make conformance ADAPTER='python3 adapters/external/fleet-adapter-vda5050/fleet_adapter_vda5050/adapter.py --endpoint localhost:{port} --comms simulated --insecure'
make conformance ADAPTER='python3 adapters/external/fleet-adapter-mavlink/fleet_adapter_mavlink/adapter.py --endpoint localhost:{port} --link simulated'
```

`{port}` is substituted with the harness's `--port` (`CONFORMANCE_PORT`, default
`9090`). The harness serves plaintext, and the VDA5050 adapter refuses to start
without TLS unless it is given `--insecure`, which is for local tests such as this
one; a checkout of the adapter from before that flag rejects it as an unrecognized
argument. Add `CONFORMANCE_ARGS='--json report.json'` to also write a machine-readable
report; `--json` given to `make` directly is a `make` option and fails. A conforming
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
**Contract version** column. The conformance-suite revision is recorded beside it
and is not part of it: a suite change re-attests adapters without moving the
version they negotiate. The version a control plane implements, and the
range it accepts, are stated in RFC-0001
[Package and versioning](../rfcs/dist/RFC-0001-core-spec.md#fleet-adapter-protocol-package-and-versioning).

The control plane gates on the report, not on the registry row. A robot is admitted,
and its adapter is dispatched to, only when the adapter's `FleetAdapter` is
`Connected` and its `spec.conformanceReport` names a ConfigMap whose report matches
`digest`, records `conformant: true`, carries a `contract_version` inside the
supported range (`contract.Supports`, called from the Robot admission webhook and from
`adapterDispatchReadiness`), and carries a signature that verifies against a trust root
in the namespace's `SwarmadaConfig`. Signature verification is on by default: a
namespace with no `SwarmadaConfig` fails every report closed, and only
`spec.signing.requireSignatureVerification: false` turns it off. The admission half runs only where the admission webhooks
are enabled. Four installs disable them: `make deploy-minimal` (`config/default`), the quickstart
overlays (`config/overlays/quickstart-mtls` among them), the Helm chart with
`webhooks.enabled=false`, and `make run`. On those, a `Robot` is admitted whatever its adapter's
conformance, and the estop, `robots/status` and delete checks do not run either (see
[Known limitations](known-limitations.md#an-install-without-admission-webhooks-checks-nothing-at-admission)). The
`adapters/REGISTRY.md` row is the public attestation; no controller reads it.

| Contract-version bump | What an adapter must do |
| :--- | :--- |
| **Major** (`1.x.y` → `2.0.0`) | Re-run `make conformance` against an adapter that declares the new major, put the new report in the ConfigMap the adapter's `FleetAdapter.spec.conformanceReport` names and update its `digest`, and update the **Contract version** column in [`adapters/REGISTRY.md`](../adapters/REGISTRY.md). A major bump is breaking, so a report earned against the previous major is outside the supported range: until the referenced report is replaced, robots bound to that adapter are refused admission and the adapter is excluded from dispatch. |
| **Minor** (`1.N.y` → `1.(N+1).0`) | A wire addition, or a change that binds adapters. The window moves. A change that binds adapters for safety also raises the lowest supported version to the new one, so an older adapter is refused at once. At contract `1.4.0` the supported range is `>=1.3.0 <1.5.0`, N and N-1, and it is not widened. An adapter that reports `1.2.0` or below is outside it: its `RegisterRobot` and `DiscoverRobot` are refused `VERSION_MISMATCH`, and a report stamped with that version satisfies neither admission nor dispatch. The harness stamps the version the adapter reports, so moving into the range means raising the declared `CONTRACT_VERSION` to `1.3.0` or `1.4.0`, a version the adapter implements, re-running `make conformance`, and replacing the referenced report. |
| **Patch** (`1.0.0` → `1.0.1`) | Nothing. The patch component is not considered in compatibility. |

Separately from the contract version, a `harness`-class conformance-suite revision
re-earns a result: `adapters/REGISTRY.md` marks a row earned before one **stale**
(unverified at the current revision, not failed). The control plane does not act on
the suite revision (`spec.conformanceReport.suiteVersion` appears only in the
`FleetAdapter` status message), so a stale row does not by itself stop admission or
dispatch.

An adapter that reports no `contract_version` at all is treated as incompatible,
not as compatible-by-default: its registrations are refused `VERSION_MISMATCH` and
its robots are not dispatchable. Telemetry, heartbeat, and emergency stop keep
working throughout — the gate is on work, never on observation or stopping.

**What eviction looks like.** An adapter whose declared version is outside the
supported range still connects. `HelloAck.accepted` is true,
`negotiated_contract_version` is empty, and the ack message reads
`connected, but robot registration is refused: <reason>`. Its `FleetAdapter` moves to
`Rejected`, and a heartbeat does not clear that; only a handshake that agrees a
supported version does. Every `RegisterRobot` and `DiscoverRobot` on the session is
answered `REGISTRATION_REJECTION_VERSION_MISMATCH`, and no `Robot` or
`DiscoveredRobot` is written. The session is kept: telemetry and heartbeats flow, and
emergency stop is delivered on the `SafetyStream`, which carries no version handshake.

**Which version the in-tree adapters declare.** The adapter template declares a
`CONTRACT_VERSION` equal to the control plane's `contract.Version`, so a generated
adapter starts with the full two-minor window. `make spec-contract` fails when the
template, `contract.Version` and the conformance harness's `CONTRACT_VERSION` disagree.
The simulation adapter declares `1.4.0` when run with `--latch`, which keeps the stop latch, and
`1.3.0` without it; the example-noop adapter declares `1.3.0`. So the N-1 half of the window is
exercised in-tree; a test asserts that both stay inside the supported range. The deployments in
this repository run the simulation adapter without `--latch`, because the reference control plane
ends no local set at v0.4, and a latching adapter that took an edge stop would take no work until
it restarts. RFC-0001 gives work only to a robot whose adapter's connection negotiated
`1.4.0`, the version that carries the stop latch (candidate filter 9). The control plane does not
check this at v0.4, so the example-noop adapter's robots are still given work.

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

Scaffold from the cookiecutter template, which generates an adapter wired to the
shared `swarmada-sdk` safety primitives (fencing, confirmed estop, lease
self-stop) and redialling a dropped stream with bounded backoff. Run unmodified
with its simulated binding, it fails no conformance check; its profiled verdict is
`not-established`, because four required rows are not measured for it (see the template's
[README](../adapters/template/README.md#what-the-harness-reports-for-a-generated-adapter)):

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
