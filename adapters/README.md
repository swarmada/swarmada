# Fleet Adapters

A **Fleet Adapter** bridges one manufacturer's robots to the Swarmada control
plane by implementing the `fleet_adapter.v1` gRPC contract defined in
[`proto/fleet_adapter/v1/fleet_adapter.proto`](../proto/fleet_adapter/v1/fleet_adapter.proto).
Adapters are the project's primary extension point: adding a robot family is an
additive act that implements a stable contract, not a change to the control
plane. RFC-0001 §5.3 is the normative description of the protocol.

## The contract in one paragraph

The control plane is the gRPC **server**; each adapter is a **client** that
dials in and opens two long-lived bidirectional streams. `ControlStream`
carries all operational traffic (registration, telemetry, task commands and
their results, heartbeats). `SafetyStream` carries emergency-stop traffic only,
on a physically separate stream so it can never queue behind bulk data. An
adapter multiplexes every robot it manages over these streams; each message
carries a `robot_id`, and per-robot authorization is enforced server-side
against the adapter's mTLS client-certificate identity. Where a zone declares an
edge node, the adapter also holds an `EdgeService` stream to it.

## Where an adapter lives

| Case | Location | Why |
| :--- | :------- | :-- |
| Reference / community adapters | `adapters/<name>/` in this repo | Maintained by the core project; kept in lockstep with the contract. |
| Vendor-owned adapters | A separate repository under the org | A third party owns the code, its license, its CLA scope, and its release cadence (see [`docs/adr/0002`](../docs/adr/0002-repository-topology.md)). |

Either way, an adapter is only a valid Swarmada adapter if it **passes the
conformance suite** in [`CONFORMANCE.md`](CONFORMANCE.md). "Vendor-neutral
standard" means exactly that: conformance, not core-tree membership, is what
makes an adapter part of the standard.

## Writing an adapter

1. Read [`CONFORMANCE.md`](CONFORMANCE.md) first — it is the specification your
   adapter is measured against.
2. Generate client stubs from the proto (`make proto`) or use the checked-in
   generated stubs under `proto/fleet_adapter/v1/`.
3. Start from [`example-noop/`](example-noop/), which shows the required stream
   topology, the handshake, command dispatch, and the safety-critical behaviors
   without any real robot integration.
4. Replace the no-op handlers with calls into the manufacturer's fleet API.
5. Run the conformance suite and fix every `MUST` failure before submitting.

## In this directory

| Path | Purpose |
| :--- | :------ |
| [`CONFORMANCE.md`](CONFORMANCE.md) | The conformance specification every adapter must satisfy. |
| [`REGISTRY.md`](REGISTRY.md) | The registry of known adapters: repository, maintainer, protocol version, and conformance status. |
| [`example-noop/`](example-noop/) | A reference adapter that speaks the full protocol with no real hardware behind it. |
