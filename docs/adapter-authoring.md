# Writing a Fleet Adapter

A Fleet Adapter translates between the Swarmada control plane and a robot's own
interface, so the orchestrator can command and observe robots it did not write the
software for. This guide is the copyable path from empty repo to a registered,
conformant adapter. It names no vendor and endorses no product — it describes the
process, and the [conformance suite](../adapters/CONFORMANCE.md) is the only judge of
whether an adapter is correct.

## Where an adapter lives

Per [ADR-0005](adr/0005-reference-adapter-policy.md):

- **In-tree** (`adapters/<name>/`) is reserved for project test infrastructure — the
  reference skeleton/template and the simulation adapter.
- **Every functional adapter lives in its own repository** — reference adapters for
  open, vendor-neutral interfaces and vendor-specific adapters alike — and is listed
  in the [adapter registry](../adapters/REGISTRY.md) by a single row.

You own your repository from the first commit. The project does not host, gate, or
certify it; it publishes a template and a test suite, and you self-attest.

## The path

1. **Scaffold from the template.** Generate your adapter from
   [`adapters/template/`](../adapters/template/) (cookiecutter). It emits a
   **safety-complete** adapter already wired to the audited `swarmada-sdk` safety
   primitives, so the safety-critical MUSTs — confirmed e-stop, fencing-token
   ordering, assignment-lease self-stop — pass out of the box. Start here rather than
   from scratch; the safety wiring is the part you least want to hand-roll.
2. **Implement the contract for your interface.** Fill in telemetry (map your robot's
   position / battery / health onto the protocol's numeric fields), task execution
   (turn a FleetTask into your robot's native command and echo the fencing token in
   status), and the confirmed-stop path (report `is_stopped` only from real robot
   state, never inferred). Decline anything optional you don't support with
   `unsupported = true`.
3. **Prove it with the conformance suite.** Run the neutral
   [conformance harness](../adapters/CONFORMANCE.md) against your adapter (a simulated
   binding is enough — no hardware required) until it passes the current suite. The
   harness stamps the **contract version** your result was earned against into its
   report.
4. **Register it.** Open a pull request adding one row to the
   [adapter registry](../adapters/REGISTRY.md) with your repository, maintainer, the
   contract version, and the conformance result. The pull request *is* the
   attestation ([ADR-0007](adr/0007-conformance-self-certification.md)) — no authority
   signs off on it.

## What the project maintains, and what you do

The project maintains the contract, the template, and the conformance suite. You
maintain your adapter and re-run the suite on a **major** contract-version bump. That
split is deliberate: it keeps the standard neutral and lets the adapter ecosystem grow
without any single party in the loop.
