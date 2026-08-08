# example-noop adapter

A reference Fleet Adapter that speaks the full `fleet_adapter.v1` protocol with
**no real hardware behind it**. It exists to demonstrate the required stream
topology and the safety-critical behaviors from
[`../CONFORMANCE.md`](../CONFORMANCE.md) in the smallest possible correct form,
so a real adapter can start from a working shape rather than a blank file.

What it demonstrates:

- opening `ControlStream` and `SafetyStream` and sending `AdapterHello` first;
- dispatching pushed `Command`s and returning a matching `CommandResult`;
- **confirmed** emergency stop on `SafetyStream` (never inferred);
- replying to heartbeats and declining optional commands via `unsupported`.

What it does **not** do: talk to any robot. Every handler returns a safe,
canned result. The `TODO(vendor)` markers show exactly where a real adapter
substitutes calls into a manufacturer's fleet API.

## Running

The adapter needs the generated gRPC stubs on the Python path. Generate them
with `make proto` from the repository root, then:

```bash
# from the repository root, with generated stubs importable as fleet_adapter.v1
python adapters/example-noop/noop_adapter.py --endpoint localhost:9090 --vendor noop
```

This is illustrative: without a control plane listening and without mTLS
material it will not complete a handshake. It is meant to be read and copied,
not deployed.
