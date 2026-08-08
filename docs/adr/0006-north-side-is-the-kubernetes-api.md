# ADR-0006: The task-source surface is the Kubernetes API

- **Status:** accepted
- **Date:** 2026-07-07
- **Deciders:** Maintainers
- **Related:** RFC-0001 (§5.2.4 FleetTask, non-goals #9, #10), ADR-0002, ADR-0005, `docs/architecture.md`

## Context

Swarmada has two integration surfaces. The *south* surface — the Fleet Adapter
protocol (`proto/fleet_adapter/v1/fleet_adapter.proto`) — is a bespoke gRPC
contract. It has to be: adapters run behind NAT with no routable address and must
dial the control plane; the control plane must push commands, including emergency
stop, to a client it did not dial; telemetry arrives at high cadence; and vendor
hardware is heterogeneous and proprietary. The adapter-initiated bidirectional
stream topology exists to satisfy those forces.

The *north* surface — how a task source (a WMS, a scheduler, an operator tool)
submits work — has no such forces. A task source can reach the cluster's API
server over HTTPS, submits declarative desired state (`FleetTask` resources), and
requires neither pushed commands nor a high-cadence channel. RFC-0001 already has
task sources create `FleetTask` resources through the API (§5.2.4) and defers the
question of how tasks are *authored* upstream (non-goal #10). What has not been
recorded is the decision that follows from this: that the north surface **is** the
Kubernetes API, and that Swarmada will not introduce a second, bespoke north-side
wire protocol. Because "build a symmetric north protocol" is a tempting and
recurring suggestion, the decision is worth recording so it is not re-litigated.

## Decision

The task-source integration surface is the Kubernetes API. A task source submits
work by creating `FleetTask` resources and observes outcomes by watching
`FleetTask` status. Swarmada does not define a bespoke north-side wire protocol.

Concretely, the north surface reuses the API server's existing mechanisms rather
than reinventing them:

- **Authentication and authorization** — ServiceAccount tokens, mTLS, or OIDC for
  identity; RBAC for who may submit. `FleetTask` status remains controller-authored
  (RA-1): no task-source role is granted `fleettasks/status` write.
- **Tenancy** — the namespace is the tenant boundary; cross-namespace submission
  stays out of scope (RFC-0001 non-goal #9).
- **Idempotency** — deterministic `FleetTask` names (and, where used, server-side
  apply with a stable field manager) make a repeated submission idempotent by
  construction.
- **Flow control** — API Priority & Fairness and `ResourceQuota` bound submission
  rate and object count using upstream primitives.
- **Versioning** — the CRD versioning rule in `docs/api-principles.md` governs;
  breaking changes move to a new API version with conversion, not a negotiated
  protocol handshake.

A task source is therefore stateless with respect to the control plane: it holds
no live authority, and if it restarts, the resources it created persist and
continue to reconcile.

This ADR records the transport decision only. The first-class *contract* built on
top of it — the read-surface guarantees, the enforced backpressure ceiling, the
boundary invariant, and a reference task source with a conformance checklist — is
proposed separately as an RFC.

## Alternatives considered

- **A bespoke north-side protocol (a north analog of `fleet_adapter.v1`).**
  Rejected. It would discard authentication, RBAC, admission control, namespaces,
  optimistic concurrency, idempotent create-by-name, watch, `ResourceQuota`,
  audit, and API Priority & Fairness — all of which the API server already
  provides — and rebuild each, less completely, as a second normative surface the
  standard must version and a standards body must review. The forces that justify
  a bespoke protocol on the south side (NAT traversal, pushed estop, high-cadence
  telemetry, proprietary hardware) do not act on the north side. Symmetry with the
  south side is not itself a force.

- **A mandatory ingestion gateway in the core (message bus / webhook → FleetTask).**
  Rejected *as a core, standardized surface*. Translating a specific WMS's message
  format into `FleetTask`s is data-format glue, not a protocol; baking one task-
  source worldview into the neutral core would compromise neutrality and impose a
  standing maintenance tax — the same in-tree-integration failure mode ADR-0005
  avoids on the south side. Such a gateway is legitimate as an out-of-core module
  in its owner's repository, tracked through the registry, and may be revisited if
  a design partner reports the Kubernetes-client integration as a hard blocker.

## Consequences

- The north surface inherits the API server's authn, authz, admission, tenancy,
  quota, and audit for free, and gains no second wire protocol to specify,
  version, or secure.
- Task sources are ordinary Kubernetes API clients. Teams that do not operate
  Kubernetes clients face an integration lift; the project addresses ergonomics
  with a Task-Source SDK and a reference task source (proposed in the companion
  RFC), not with a bespoke protocol.
- The RA-1 status-write discipline extends to the north side by RBAC: task sources
  read status and never write it.
- The decision leaves a clean seam: an out-of-core ingestion gateway can be added
  later without changing the core, because the core surface is unchanged — it is
  the Kubernetes API.
