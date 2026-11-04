# Conformance profiles, catalogs, and the harness

The layer above `adapters/conformance`. That harness drives the Fleet Adapter
catalog `C0`–`C19` and emits one flat result set; this package groups results
into **named profiles** at **Core** and **Extended** levels, emits the
machine-readable **`ConformanceReport`**, and drives the two catalogs the Draft 4
external-action surface adds.

| File | What it is |
| :---- | :---- |
| [`PROFILES.md`](PROFILES.md) | The registry document: surfaces, profiles, levels, capability tiers, the `ConformanceReport` schema, self-certification |
| [`profiles.yaml`](profiles.yaml) | The same partition, machine-readable. The harness reads this; the `*0.5` check fails when it drifts from the catalogs |
| [`COMPUTE-ENDPOINT.md`](COMPUTE-ENDPOINT.md) | `CE0`–`CE8` — the executor side: bindings, idempotency, the reply envelope, events |
| [`REACTIVE-BLOCK.md`](REACTIVE-BLOCK.md) | `RB0`–`RB6` — exhaustive coverage of the reactive grammar |
| [`CONTROL-PLANE.md`](CONTROL-PLANE.md) | `CP0`–`CP6` — the deployment: containment, dispatch, provenance, cluster compute |
| [`reactive-grammar.v1.yaml`](reactive-grammar.v1.yaml) | The grammar, machine-readable. `RB0.1` fails when a construct here has no check |
| [`fixtures/`](fixtures) | A conforming executor per binding, plus named defects so the checks can be seen to fail |

## Running it

```bash
make conformance-profiles          # every surface this repository can drive
```

Or a surface at a time. **Prefer the `make` targets** (`conformance-profiles`,
`conformance-endpoint`, `conformance-controlplane`): every fixture speaking
`compute.v1` over gRPC or MCP imports the generated stubs and so needs `proto/`
on `PYTHONPATH`, which those targets set. Run bare, such a fixture dies with
`ModuleNotFoundError: No module named 'fleet_adapter'` — the suite is fine, the
path is not. By hand, prefix with `PYTHONPATH=proto`. The HTTP fixture and the
control-plane surface import no stubs and run bare as written.

```bash
# A Compute Endpoint, against an in-tree fixture the harness starts itself.
python3 -m conformance compute-endpoint --endpoint plate-reader-http
python3 -m conformance compute-endpoint --endpoint plate-reader-grpc
python3 -m conformance compute-endpoint --endpoint plate-reader-mcp
python3 -m conformance compute-endpoint --endpoint plate-watcher-grpc   # contract: Service

# A Compute Endpoint you deployed.
python3 -m conformance compute-endpoint --endpoint plate-reader-grpc --target 10.0.0.4:9443

# The deployment. Static: no cluster, no API server, no network.
python3 -m conformance control-plane

# A Fleet Adapter, from a run of the C0-C19 harness.
make conformance CONFORMANCE_ARGS='--json /tmp/adapter.json'
python3 -m conformance fleet-adapter --adapter-report /tmp/adapter.json --tier FullControl
```

`--out DIR` writes each report as YAML. The exit code is non-zero when any
report is non-conformant.

## Prerequisites

A clone and a Python interpreter. `PyYAML` and `grpcio` (already this project's
dependencies) are needed; the HTTP and MCP fixtures are standard library only,
and the gRPC one uses the stubs `make proto` generates. Nothing here needs a
cluster, a network, or a repository other than this one — `*0.3` fails the run
if that stops being true.

**Python 3.10 is the floor for this package, and it is lower than the project's
own.** The repository declares `requires-python = ">=3.12"` (numpy needs it), but
`conformance/` has to import on **3.10**: the robot's vendor container is Python
3.10.12, it is where the adapter harness runs — DDS discovery does not cross a
virtual-machine boundary to a LAN host — and grading a run against a `plant.yaml`
is the one stage that has a reason to run where the plant is. The floor is not a
convention: `tests/python/test_conformance_runs_on_the_harness_floor.py` fails on
any 3.11-or-later name this package reaches for, and `make test-py` runs it. Adding
one is fine — say so there, and say what the robot is then expected to run.

## Reading a report

- **pass** — the check was exercised and satisfied.
- **fail** — the check was exercised and violated. A failed Core check in a
  required, applicable profile makes the report non-conformant.
- **skip** — the check was not exercised, with an enumerated cause. A skip is
  not a pass; a Core profile carrying one reads `notVerified`, never `pass`.
- **notApplicable** — a conditional profile that did not apply. Different from
  a profile that applied and went unverified, and reported differently on
  purpose.

## Proving the checks are checks

A check that has never been seen to fail is not known to be a check. Each
fixture implements named defects:

```bash
python3 -m conformance compute-endpoint --endpoint plate-reader-http --misbehave command
# [FAIL] CE2.5  Nothing in the reply is interpretable as an instruction
#               — command-shaped keys present: values=['desiredState']
```

`--misbehave` accepts `prefix-dedup` (`CE1.6`), `command` (`CE2.5`), `oversize`
(`CE2.3`), `empty-is-fail` (`CE2.2`) and `no-replay` (`CE1.2`, `CE1.3`).

## Self-certification

Self-certified, published, no gate and no fee (ADR-0007). Running the suite
needs no permission, no registration, and no payment, and this project issues no
certificate and operates no certification authority. See
[`PROFILES.md`](PROFILES.md#self-certification-and-publication).
