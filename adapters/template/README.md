# Vendor Fleet Adapter template

A [cookiecutter](https://cookiecutter.readthedocs.io/) template that scaffolds a
**safety-complete** Swarmada Fleet Adapter you own from the first commit. Per
[ADR-0005](../../docs/adr/0005-reference-adapter-policy.md), vendor adapters live
in the vendor's own repository — the project ships this template so you start
conformant, not a core-authored stub you inherit.

## What you get

A generated adapter that is **feature-basic but safety-complete** out of the box:

- The CONFORMANCE.md safety MUSTs are wired from the audited `swarmada-sdk`
  primitives — you do **not** re-implement (or fake) them:
  - **C3** fencing-token ordering (`swarmada_sdk.FenceGuard`),
  - **C4** assignment-lease self-stop (`swarmada_sdk.LeaseMonitor`),
  - **C5** confirmed emergency stop (`swarmada_sdk.confirm_estop`).
- Optional commands are declined with `unsupported = true` (C7.1).
- A `SimulatedRobot` binding so the generated adapter is conformant immediately;
  every `TODO(vendor)` marks where you bind it to your real fleet API — preserving
  the confirmed-stop contract (`is_stopped()` reflects a *real* halt, never a
  guess).
- A CI workflow that runs the published conformance harness on every push.

## Use it

```bash
pip install cookiecutter
cookiecutter path/to/swarmada/adapters/template
# answer the prompts (adapter name, vendor, robot class, …)

cd fleet-adapter-<you>
pip install -e . swarmada-sdk
make test          # the safety wiring tests
make conformance   # drive your adapter against the harness (C0–C16)
```

Then replace `SimulatedRobot` in `<package>/robot.py` with a binding to your fleet
API, keeping every safety contract. Register your adapter in the Swarmada
[`adapters/REGISTRY.md`](../REGISTRY.md) once it passes conformance.
