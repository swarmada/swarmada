# example-noop adapter

A reference Fleet Adapter skeleton that answers every `fleet_adapter.v1` command
with **no real hardware behind it**. It exists to show the required stream
topology and the handshake from [`../CONFORMANCE.md`](../CONFORMANCE.md) in the
smallest possible form. It declares no action type, so it refuses every
assignment `INVALID_ACTION` and registers no robot; the checks that need an
accepted assignment — fencing (C3), the lease self-stop (C4), action status (C16)
and the action catalog (C19) — are not supported for it
([registry](../REGISTRY.md)). A new adapter is scaffolded from
[`../template/`](../template/), which uses the shared `swarmada-sdk` safety
primitives; read this skeleton alongside it.

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
