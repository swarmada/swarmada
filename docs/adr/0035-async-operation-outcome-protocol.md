# ADR-0035: A general async-operation outcome mechanism for the Fleet Adapter protocol

- **Status:** proposed
- **Date:** 2026-08-05
- **Deciders:** _(pending)_
- **Related:** ADR-0033 (install-outcome reporting — the point-solution this generalizes), ADR-0019 (adapter action discovery and validation), RFC-0001 §9.2 (Fleet Adapter Protocol); `proto/fleet_adapter/v1/fleet_adapter.proto`

## Context

The Fleet Adapter protocol has a recurring shape: a command is acknowledged
quickly, but the real **outcome** of a long-running operation arrives later, out of
band. ADR-0033 solved this for firmware and model install — a terminal outcome enum
on `UpdateProgress`, durable snapshot fields, and a projection onto `Robot.status` —
but it solved it **bespoke, per operation**. The acknowledgement-vs-outcome gap it
named is general: the ack and the outcome are two facts arriving at two times, and
each future long-running command (e.g. calibration, bulk configuration)
re-derives its own outcome carrier, terminal enum, snapshot field, and status
projection, along with its own copy of the confirmed-never-inferred and
stream/snapshot rules — which then risk drifting apart.

Forces in tension: additive-only, backward-compatible protocol evolution; the
confirmed-never-inferred audit boundary (ADR-0033) must be preserved; the
stream-for-latency + snapshot-for-recovery split proved useful and should be
reusable, not re-copied; and the optional-command rule (an adapter that does not
implement an operation must not be burdened).

## Decision

_(Pending — this is a proposal stub.)_ Proposed direction: define a single reusable
**async-operation-outcome** primitive in `fleet_adapter.v1` — a correlation id, a
terminal outcome enum (`UNSPECIFIED`/`SUCCEEDED`/`FAILED`), a `failure_reason`, and
the resulting state — delivered on both the streaming path (latency) and a durable
state snapshot (recovery), with a uniform projection onto `Robot.status`. Firmware
install, model install, and future long-running operations become instances of it
rather than one-offs. If adopted, this **supersedes** the bespoke carriers in
ADR-0033 (additive, with an N-1-compatible migration).

## Alternatives considered

- **Keep solving per-operation (the ADR-0033 shape).** Rejected as the very thing
  this ADR exists to replace: each new long-running command re-derives the same
  mechanism, and the audit/latency/recovery invariants risk diverging between copies.
- _(Further options to be enumerated during design.)_

## Consequences

_(To be completed during design.)_ A general primitive shrinks per-operation surface
and keeps the audit, latency, and recovery rules in one place; the cost is a larger
up-front protocol design and a migration path for the firmware/model carriers ADR-0033
already shipped.
