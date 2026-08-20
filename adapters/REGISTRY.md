# Adapter Registry

The registry of Fleet Adapters known to the project: where each one lives, who
maintains it, which contract version it targets, and its conformance status. It
exists so a bug can be routed to the right repository and so fleet-wide adapter
health is legible at a glance.

Listing is **factual**: an entry records a conformance result against a contract
version. It is not a certification or an endorsement — the project publishes the
neutral [conformance suite](CONFORMANCE.md) and this matrix, and runs no
certification program (see [ADR-0005](../docs/adr/0005-reference-adapter-policy.md)).

## How placement works

Per [ADR-0005](../docs/adr/0005-reference-adapter-policy.md):

- **In-tree** (`adapters/<name>/`): the reference skeleton/template, and a
  simulation adapter used as test infrastructure.
- **Own repository**: every functional adapter lives in its own repository and is
  tracked here by a Registry row — for both open, vendor-neutral interfaces and
  vendor-specific adapters.

Scaffold your adapter from the [`adapters/template/`](template/) cookiecutter — it
generates a **safety-complete** adapter wired to the audited `swarmada-sdk` safety
primitives (C3/C4/C5) that passes the conformance harness out of the box, so you
start conformant and own your repo from commit one.

## Conformance status legend

| Value | `FleetAdapter.status.conformance` | Meaning |
| :---- | :---- | :------ |
| `passing` | `Passed` | Passes the conformance suite for the listed contract version. |
| `partial` | not `Passed` | Implements the required (safety-critical) subset; some optional areas unverified. |
| `wip` | not `Passed` | Under development; not yet conformant. |
| `template` | not `Passed` | A skeleton/example, not a usable adapter. |

Only `passing` maps to `Passed`. Robots are admitted only against an adapter whose
`status.conformance` is `Passed`, so a row in any other state is not admission-eligible.

Every conforming adapter — whatever its status — must satisfy the safety-critical
MUSTs (confirmed estop, fencing-token ordering, assignment-lease self-stop);
"partial" refers only to *optional* commands declined via `unsupported = true`.

## Registry

The **Contract version** column carries the fleet-adapter contract version (semver, e.g.
`1.0.0`) that the row's conformance result was earned against, with the wire-package
identity in parentheses (`fleet_adapter.v1`). The semver is what compatibility is
judged on. A **major** bump requires re-running the suite and updating the row;
minor/patch bumps do not.

> **Suite revision.** A row records two versions, never one: the **contract version** the result was
> earned against (the column below) and the **suite revision** that produced it — at v0.3,
> **C0–C16, 44 automated checks** — named in the **Conformance** cell. The suite revision is recorded
> alongside the contract version and is never folded into it: a suite change re-attests adapters, it
> does not move the contract they negotiate
> ([ADR-0032](../docs/adr/0032-fleet-adapter-contract-versioning-and-conformance-gating.md) binds a
> result to a contract version; this is the provenance record beside it).
>
> The catalogue in [`CONFORMANCE.md`](CONFORMANCE.md) lists one further check, **C8.4**, which the
> harness does not run: it is an obligation on the *edge node*, not on an adapter, so no adapter
> result can pass or fail it. 44 is the number a `make conformance` run reports.

| Adapter | Robot interface | Repository | Maintained by | Contract version | Conformance |
| :------ | :-------------- | :--------- | :------------ | :--------------- | :---------- |
| `example-noop` | — (reference skeleton) | in-tree `adapters/example-noop/` | Core project | `1.0.0` (`fleet_adapter.v1`) | `template` (skeleton/example; registers no robot) |
| `simulation` | Simulator (test infrastructure) | in-tree `adapters/simulation/` | Core project | `1.0.0` (`fleet_adapter.v1`) | `partial` (safety-complete C3/C4/C5; CONFORMANT vs the current C0–C16 harness via `make conformance-sim`; optional commands declined) |
| `fleet-adapter-ros2` | ROS 2 / Nav2 | [`swarmada/fleet-adapter-ros2`](https://github.com/swarmada/fleet-adapter-ros2) | [@AlexBahel](https://github.com/AlexBahel) | `1.0.0` (`fleet_adapter.v1`) | `partial` (safety-complete; CONFORMANT vs the C0–C16 harness against a simulated binding, optional commands declined; C13.2 defence-in-depth absent — non-blocking; no live ROS 2/Nav2 runtime yet) |
| `fleet-adapter-vda5050` | VDA5050 (MQTT) — *target interface; not yet exercised* | [`swarmada/fleet-adapter-vda5050`](https://github.com/swarmada/fleet-adapter-vda5050) | [@AlexBahel](https://github.com/AlexBahel) | `1.0.0` (`fleet_adapter.v1`) | `partial` (safety-complete; CONFORMANT vs the C0–C16 harness against a simulated binding, optional commands declined; C13.2 defence-in-depth absent — non-blocking; no live MQTT/VDA5050 runtime yet) |
| `fleet-adapter-mavlink` | MAVLink / PX4 | [`swarmada/fleet-adapter-mavlink`](https://github.com/swarmada/fleet-adapter-mavlink) | [@AlexBahel](https://github.com/AlexBahel) | `1.0.0` (`fleet_adapter.v1`) | `partial` (safety-complete; CONFORMANT vs the C0–C16 harness against a simulated binding, optional commands declined; C13.2 defence-in-depth absent — non-blocking; no live MAVLink/PX4 runtime yet) |

New reference or vendor adapters are welcome — open a pull request adding a Registry
row once the adapter passes the [conformance suite](CONFORMANCE.md).

## Adding or updating an entry

Open a pull request that edits this file:

1. Run the [conformance suite](CONFORMANCE.md) against your adapter and record the
   result together with the **contract version** it was earned against — the
   `contract_version` the harness stamps into the report (semver, e.g. `1.0.0`).
   Put that semver in the **Contract version** column, with the wire-package identity in
   parentheses: `` `1.0.0` (`fleet_adapter.v1`) ``. The suite self-certifies
   ([ADR-0007](../docs/adr/0007-conformance-self-certification.md)): this pull
   request is the attestation — no authority signs off on it.
   Re-qualification is required only on a **major** contract-version bump.
2. Add (or update) a row in **Registry** with the repository and maintainer.
3. For a vendor-specific adapter, the repository is the vendor's own; for a
   neutral reference adapter, it is a repository under the project org.
