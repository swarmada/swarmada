# Fleet Adapter Conformance Harness

An executable driver for [`../CONFORMANCE.md`](../CONFORMANCE.md). It is the
control-plane side of `fleet_adapter.v1`: it stands up the gRPC server an adapter
dials into, drives the adapter through scenarios that map to the CONFORMANCE.md
checks, and prints a `pass` / `fail` / `skip` result per check.

This is a **conformance test suite, not a certification authority**. It produces
a factual result an adapter's authors self-attest to; the project runs no
certification program and issues no mark (see
[ADR-0005](../../docs/adr/0005-reference-adapter-policy.md) and
[ADR-0007](../../docs/adr/0007-conformance-self-certification.md)).

## Scope

The harness drives the full `C0`–`C19` catalog in [`../CONFORMANCE.md`](../CONFORMANCE.md) and
records a result for every check id it defines. A check the run could not exercise is recorded
`skip` with an enumerated cause (`SkipCause` in `report.py`), so the report states exactly what
was and was not verified.

| Area | What the harness does |
| :--- | :-------------------- |
| C0 report integrity | Self-checks on the run and its context; `C0.1` fails the suite if a skip contradicts its recorded cause |
| C1 handshake | Both streams opened together; `AdapterHello` first; protocol version. `C1.3` (client certificate) is recorded `skip`: the harness listens on a plaintext port (see mTLS below) |
| C2 lifecycle | Registration and the redial, when the adapter registers a robot (else `skip`) |
| C3 fencing tokens | MISSING / STALE rejection, accept + echo, idempotent re-delivery, adoption of the control plane's token, a cancel naming a preempted action leaving the preemptor running |
| C4 leases | Self-stop when a lease is not renewed (`C4.2`), renewal, stale `lease_generation` |
| C5 estop | Confirmed `EstopAck` on `SafetyStream`; estop not on `ControlStream` |
| C6 telemetry | Telemetry and status discipline, when the adapter streams telemetry (else `skip`) |
| C7 optional commands | Decline via `unsupported=true`, and artefact verification |
| C8 edge stream | Serves `EdgeService`, advertises an edge endpoint on `RegisterAck`, checks the adapter dials it and confirms an edge-issued estop |
| C9–C10 | `verify_*` results and update progress, owed only by an adapter that accepts the command |
| C11–C12 | Pause / resume and capability scan |
| C13–C15 | Contract-version negotiation, estop version-invariance, version-bound report |
| C16 | Task status reporting, terminal states included |
| C17 | Zone operating constraints: declaration well formed; an unhonourable rule is declined naming the subject |
| C18 | Sensor streams and the duty cycle |
| C19 | An assignment outside the adapter's declared action catalog is declined `INVALID_ACTION` |

## Running

The harness needs the generated Python stubs on a path (`make proto` writes them
under `proto/`). It starts the server, launches the adapter-under-test pointed at
it, runs the checks, and exits non-zero if any **MUST / MUST NOT** check failed.

```bash
# From the repository root, after `make proto`:
python -m adapters.conformance \
    --stub-path proto \
    --adapter-name example-noop \
    --adapter-cmd 'python adapters/example-noop/noop_adapter.py --endpoint localhost:{port}'
```

`{port}` in `--adapter-cmd` is substituted with `--port` (default `9090`). Add
`--json report.json` to also write a machine-readable report.

Or, equivalently, via Make:

```bash
make conformance ADAPTER='python adapters/example-noop/noop_adapter.py --endpoint localhost:{port}'
```

## Interpreting the report

- **pass** — the adapter satisfied the check.
- **fail** — the adapter violated it. A failed MUST / MUST NOT makes the adapter
  non-conforming and sets a non-zero exit code.
- **skip** — the check was not exercised (the adapter did not initiate or
  implement the behavior). A skip is not a pass; it records what went unverified.

A conforming adapter is one with no failed MUST / MUST NOT among the checks that
ran. Record the result and the protocol version in [`../REGISTRY.md`](../REGISTRY.md).

## mTLS

Production connections use mutual TLS, and the adapter's client certificate is its
identity (`C1.3`). The harness listens on a plaintext port (`add_insecure_port`), so it
does not grade `C1.3`: the report records it `skip` with the cause `deployment-property`,
and certificate identity is asserted in the control plane's own `ControlStream` tests.

## Layout

| File | Purpose |
| :--- | :------ |
| `report.py` | The result model (`CheckResult`, `Report`) and JSON/text rendering. |
| `harness.py` | The control-plane test server and the C0–C19 scenario driver. |
| `__main__.py` | CLI: start server, launch the adapter, run, report, exit. |
