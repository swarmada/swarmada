# Vendor Fleet Adapter template

A [cookiecutter](https://cookiecutter.readthedocs.io/) template that scaffolds a
Swarmada Fleet Adapter you own from the first commit. Per
[ADR-0005](../../docs/adr/0005-reference-adapter-policy.md), vendor adapters live
in the vendor's own repository — the project ships this template so you start
from working safety wiring, not a core-authored stub you inherit.

## What you get

A generated adapter that is **feature-basic**, with the safety paths wired:

- The CONFORMANCE.md safety MUSTs are wired from the shared `swarmada-sdk`
  primitives — you do **not** re-implement (or fake) them:
  - **C3** fencing-token ordering (`swarmada_sdk.FenceGuard`), including adopting the
    control plane's own token on re-registration (C3.8),
  - **C4** assignment-lease self-stop (`swarmada_sdk.LeaseMonitor`), reported to
    the control plane as a `STOPPED` `ActionStatusUpdate` (the commanded stop, not
    terminal) and then, once rest is confirmed or cannot be, a terminal `FAILED` with
    `end_cause` `ACTION_END_CAUSE_LEASE_EXPIRED` (C4.2),
  - **C5** confirmed emergency stop (`swarmada_sdk.confirm_estop`), with
    `stop_initiated_at` stamped when the stop is commanded (C5.3).
- Reconnect: a dropped `ControlStream` or `SafetyStream` is redialled with bounded,
  jittered exponential backoff (C1.5). On re-registration the adapter adopts the
  control plane's authoritative action state (C2.2), sends a full hardware snapshot
  in its first telemetry (C6.1), and takes up the advertised telemetry interval
  (C2.3). Work arriving on a session whose registration was refused is declined
  (C13.2).
- An `ActionStatusUpdate` on acceptance (`RUNNING`), on arrival at the target
  (`SUCCEEDED`) and on cancel (`CANCELLED`) (C16.1), echoing the executing fencing
  token (C6.4). A cancel that names an action the robot is no longer running, which
  the control plane sends after one action preempts another, is acknowledged and
  stops nothing (C3.9).
- An `EdgeStream` to every edge endpoint the control plane advertises on
  `RegisterAck`, with a `PositionFrame` per telemetry tick and the same confirmed
  estop on it (C8.1–C8.3).
- Optional commands, `zone_constraints` among them, are declined with
  `unsupported = true` (C7.1).
- The stop latch of contract `1.4.0` (RFC-0001 `#fleet-adapter-protocol-the-stop-latch`,
  `C5.8`): a stop, on `SafetyStream` or as its fence on `ControlStream`, latches its ID; every
  `AssignAction` is refused `ROBOT_UNAVAILABLE` until the stop's `EstopClear` lifts it, which is
  answered with an `EstopClearAck`; work a stop interrupts ends `FAILED` and is never resumed.
  Only the `EstopClear` that names a local set ends the refusal from an edge stop, and the
  reference control plane ends no local set at v0.4: against it, a robot this adapter took an
  edge stop for takes no work until the adapter restarts. Never report `1.4.0` without the latch.
- `CONTRACT_VERSION` at the contract version this repository implements, so a
  generated adapter starts with the full two-minor support window. `make
  spec-contract` fails if the template falls behind it.
- A `--namespace` option (default from the `namespace` prompt) for the namespace
  declared in `AdapterHello`. With a client certificate, the certificate binds the
  namespace: pass `--namespace ""` or its exact name, since any other value is
  refused at the handshake. Without a certificate the value is used as declared, so
  it must name the namespace your Robots are admitted in.
- A `SimulatedRobot` binding, so the generated adapter runs before you have written
  any fleet code; every `TODO(vendor)` marks where you bind it to your real fleet
  API — preserving the confirmed-stop contract (`is_stopped()` reflects a *real*
  halt, never a guess).
- `make conformance` and `make profile`, which run the conformance harness from a
  Swarmada checkout against the adapter and group the result into profiles, and a
  CI workflow that installs the adapter on Python 3.10, runs its tests and runs the
  harness.

## What the harness reports for a generated adapter

Rendered with the defaults in `cookiecutter.json` and run, unmodified, with its
`SimulatedRobot` binding through the conformance harness at suite revision
`2026.09.29` on Python 3.10, a generated adapter fails no check: **51 passed, 0
failed, 18 skipped**, which the harness prints as `CONFORMANT (for the checks that
ran)`. That is not a full pass. Grouped into profiles (`make profile`), the verdict
is **`not-established`**, because four required Core rows are not measured:

- `C1.3`, `C1.4`, `C3.7` — the harness measures these for no adapter: it serves
  plaintext, no wire carries an adapter's own health, and it never restarts the
  adapter.
- `C5.7` — grading an early `STOPPED` needs a motion witness written by something
  other than the adapter; the simulated binding writes none.

The other skipped rows are: the declined optional commands (`C7.2`, `C7.3`,
`C9.1`, `C10.1`, `C10.2`); the empty operating-subject and sensor-stream
declarations (`C17.4`, `C18.2`–`C18.5`); a catalog with no enum parameter
(`C19.4`); the one-phase estop acknowledgement, which is equally conformant
(`C5.6`); `C1.5`, whose backoff curve needs a longer run than the harness makes;
and `C6.3`, which is graded on the control plane, not the adapter.

This result describes the template as shipped, against a simulated binding. It
says nothing about your adapter once `SimulatedRobot` is replaced: re-run the
harness against your own binding and record that result. `make
check-adapter-template` renders the template and runs its unit tests; it does not
run the harness, so nothing re-measures the figures above when the template or the
suite changes.

## Use it

You need a checkout of the Swarmada repository: `swarmada-sdk` and the
conformance harness are installed from it, not from any package index. A
generated adapter does not list `swarmada-sdk` as a dependency, so pip never
looks the name up on an index. Install it from the checkout in the same command
as the adapter, as below, and never by name.

```bash
pip install cookiecutter
cookiecutter path/to/swarmada/adapters/template
# answer the prompts (adapter name, vendor, robot class, namespace, …)

cd fleet-adapter-<you>
pip install path/to/swarmada/sdk/python -e '.[dev]'
make test                                    # the safety-wiring tests
make conformance SWARMADA=path/to/swarmada   # the harness, C0–C19
make profile SWARMADA=path/to/swarmada       # the profiled verdict
```

Python 3.10 or newer. The adapter imports the `fleet_adapter.v1` stubs from
`path/to/swarmada/proto`, which import on protobuf 3.20.3 / grpcio 1.64.1 and
newer; [`docs/adapter-authoring.md`](../../docs/adapter-authoring.md#python-and-runtime-versions)
says how to run the adapter on a robot image that already carries an older runtime.

Then replace `SimulatedRobot` in `<package>/robot.py` with a binding to your fleet
API, keeping every safety contract. Register your adapter in the Swarmada
[`adapters/REGISTRY.md`](../REGISTRY.md) with the result your adapter earns.
