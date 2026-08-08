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

This cut covers the safety-critical subset **C1–C6** — the checks
[`REGISTRY.md`](../REGISTRY.md) expects a submission to demonstrate. Checks that
the harness does not yet drive (C7 optional-command decline, C8 edge stream, and
the timer-driven parts of C4) are reported as `skip` with a reason, so the report
always states exactly what was and was not verified.

| Area | Coverage in this cut |
| :--- | :------------------- |
| C1 handshake | Both streams opened together; `AdapterHello` first; protocol version |
| C2 lifecycle | Observed only if the adapter self-registers (else `skip`) |
| C3 fencing tokens | MISSING / STALE rejection, accept + echo, idempotent re-delivery |
| C4 leases | Reported `skip` — self-stop is a timer-driven on-robot behavior |
| C5 estop | Confirmed `EstopAck` on `SafetyStream`; estop not on `ControlStream` |
| C6 telemetry | Observed only if the adapter streams telemetry (else `skip`) |

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

Production connections use mutual TLS and the adapter's client certificate is its
identity (C1.3, RFC-0001 §5.5). This cut runs over an insecure local channel to
keep the check logic the focus; certificate-identity and per-robot authorization
checks are a planned addition and are tracked as such in the report.

## Layout

| File | Purpose |
| :--- | :------ |
| `report.py` | The result model (`CheckResult`, `Report`) and JSON/text rendering. |
| `harness.py` | The control-plane test server and the C1–C6 scenario driver. |
| `__main__.py` | CLI: start server, launch the adapter, run, report, exit. |
