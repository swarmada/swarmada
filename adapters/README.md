# Fleet Adapters

A **Fleet Adapter** bridges one manufacturer's robots to the Swarmada control
plane by implementing the `fleet_adapter.v1` gRPC contract defined in
[`proto/fleet_adapter/v1/fleet_adapter.proto`](../proto/fleet_adapter/v1/fleet_adapter.proto).
Adapters are the project's primary extension point: adding a robot family is an
additive act that implements a stable contract, not a change to the control
plane. RFC-0001 #fleet-adapter-protocol is the normative description of the protocol.

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
| Skeleton, template, simulation | `adapters/example-noop/`, `adapters/template/`, `adapters/simulation/` in this repo | The only adapter code this tree holds: a reference skeleton, the cookiecutter template, and a simulation adapter kept as test and demonstration infrastructure ([ADR-0005](../docs/adr/0005-reference-adapter-policy.md)). |
| Reference adapters for open interfaces (ROS 2 / Nav2, VDA5050, MAVLink) | Their own repositories under the `swarmada` organisation, tracked in [`REGISTRY.md`](REGISTRY.md); clone one into `adapters/external/` to run it locally | Keeps the core focused on the contract, the harness, the template and the registry ([ADR-0005](../docs/adr/0005-reference-adapter-policy.md)). |
| Vendor-specific adapters | The vendor's own repository | A vendor owns the code, its license, and its release cadence, and scaffolds it from the template; vendor-specific adapters are never built in the core tree ([ADR-0005](../docs/adr/0005-reference-adapter-policy.md), [ADR-0002](../docs/adr/0002-repository-topology.md)). |

Either way, an adapter is only a valid Swarmada adapter if it **passes the
conformance suite** in [`CONFORMANCE.md`](CONFORMANCE.md). "Vendor-neutral
standard" means exactly that: conformance, not core-tree membership, is what
makes an adapter part of the standard.

## Writing an adapter

1. Read [`CONFORMANCE.md`](CONFORMANCE.md) first — it is the specification your
   adapter is measured against.
2. Generate client stubs from the proto (`make proto`) or use the checked-in
   generated stubs under `proto/fleet_adapter/v1/`.
3. Scaffold the adapter from the [`template/`](template/) cookiecutter. It
   generates an adapter wired to the shared `swarmada-sdk` safety primitives
   (fencing, lease self-stop, confirmed estop) with a simulated robot binding.
   [`example-noop/`](example-noop/) shows the stream topology, the handshake and
   command dispatch in the smallest form, but it declares no action type and
   refuses every assignment, so it exercises neither fencing nor the lease
   self-stop; read it, do not start from it.
4. Replace the simulated binding at each `TODO(vendor)` marker with calls into the
   manufacturer's fleet API.
5. Run the conformance suite and fix every `MUST` failure before submitting.

## In this directory

| Path | Purpose |
| :--- | :------ |
| [`CONFORMANCE.md`](CONFORMANCE.md) | The conformance specification every adapter must satisfy. |
| [`REGISTRY.md`](REGISTRY.md) | The registry of known adapters: repository, maintainer, protocol version, and conformance status. |
| [`example-noop/`](example-noop/) | A reference skeleton with no hardware behind it: it answers every command, declares no action type and refuses every assignment. |
| [`template/`](template/) | The cookiecutter template a new adapter is generated from. |
| [`simulation/`](simulation/) | The in-tree simulation adapter (`make conformance-sim`). |
| [`conformance/`](conformance/) | The executable conformance harness (`make conformance`). |
| [`scenarios/`](scenarios/) | Scenario presets shared by the simulation adapter. |
