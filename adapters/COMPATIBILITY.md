# Adapter compatibility

One row per release of a reference Fleet Adapter. A row states the fleet-adapter contract version
the release implements, the RFC-0001 revision it was built against, and the control-plane release a
conformance run tested it with.

An adapter's version is its own, apart from the RFC's version and the control plane's. Its first
two numbers, X.Y, are the contract version its hello declares: an adapter at `v1.3.0` declares
contract `1.3.0`. The third number counts the adapter's own releases, which it makes without an RFC
or control-plane release.

A control plane accepts a range of contract versions. Swarmada 0.4.1 implements contract `1.4.0`
and accepts `>=1.3.0 <1.5.0`
([RFC-0001, Package and versioning](../rfcs/dist/RFC-0001-core-spec.md#fleet-adapter-protocol-package-and-versioning)).
An adapter outside that range keeps its session and receives every stop, and its robots are refused
registration and admission.

A profile verdict of `not-established` means a required check was not measured, not that one
failed.

| Adapter release | Contract version | RFC-0001 | Control plane | Conformance |
| :-------------- | :--------------- | :------- | :------------ | :---------- |
| [`fleet-adapter-ros2`](https://github.com/swarmada/fleet-adapter-ros2) `v1.3.0` | `1.3.0` | v0.4 | 0.4.1 | 29 passed, 0 failed, 1 advisory, 49 skipped, at suite `2026.11.14`, on a connection that agreed `1.3.0`; profile verdict `not-established`; no MUST-level failure. Required Core rows not measured: `C1.3`, `C3.7`, `C1.4`, `C5.7`. The advisory is `C13.2` (SHOULD), graded failed: the adapter accepts an `AssignAction` sent after a `VERSION_MISMATCH` refusal. Dispatch is largely unexercised at `1.3.0`: a `1.3.0` connection gets no assignments, so 15 dispatch Core rows and the lease Core rows don't apply. Run on `4c64441`, the tagged commit. |
| [`fleet-adapter-vda5050`](https://github.com/swarmada/fleet-adapter-vda5050) `v1.4.0` | `1.4.0` | v0.4 | 0.4.1 | 53 passed, 0 failed, 26 skipped, at suite `2026.11.14`; profile verdict `not-established`; no MUST-level failure. Required Core rows not measured: `C1.3`, `C3.7`, `C1.4`, `C5.7`, `C19.2`. Run on `af54d01`, the tagged commit. |
| [`fleet-adapter-vda5050`](https://github.com/swarmada/fleet-adapter-vda5050) `v1.4.1` | `1.4.0` | v0.4 | 0.4.1 | 53 passed, 0 failed, 26 skipped, at suite `2026.11.14`; profile verdict `not-established`; no MUST-level failure. Required Core rows not measured: `C1.3`, `C3.7`, `C1.4`, `C5.7`, `C19.2`. The run names one robot, so the rows for an adapter that serves several (`C20.1`, `C20.2`, `C20.5`–`C20.7`) aren't measured. Run on `53c2935`, the tagged commit. |

**Control plane** names the swarmada release whose conformance harness ran against the adapter
release. **Conformance** gives that run's suite revision and verdict, and the adapter's row in
[`REGISTRY.md`](REGISTRY.md) carries the full result. A row that reads *pending conformance* has no
run yet: its control-plane release is the one it is to be tested with, and the row claims no result.

An adapter with no release has no row here. Its contract version is in its row in
[`REGISTRY.md`](REGISTRY.md).

A release adds its row when it is tagged, reading *pending conformance*. The pull request that
records its conformance result in `REGISTRY.md` fills in its **Control plane** and **Conformance**
cells.
