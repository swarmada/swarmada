# ADR-0010: The edge Zone Controller is the site connectivity boundary

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** Maintainers
- **Related:** ADR-0006 (task-source surface is the Kubernetes API), ADR-0005 (reference adapter policy), `docs/architecture.md`

## Context

The architecture defines a **Zone Controller** in two places: as a control-plane
component that reconciles `FleetZone` resources, and as an **edge node** that
retains local authority (including emergency-stop authority for its zone) when the
control plane is unreachable. The data-plane description also states that "the
control plane connects to each adapter" over gRPC.

That connection direction holds only when the control plane and the Fleet Adapters
share a routable network. It does not hold for a common — and, for the neutral
standard, unavoidable — topology: the robots and their adapters sit on a facility's
private LAN behind NAT and a corporate firewall, while the control plane runs
elsewhere (another site, a regional cluster, or a remote/hosted cluster). In that
topology the control plane cannot initiate a connection inward; facility IT will
not open inbound ports so an external control plane can drive robots. Yet the
standard already promises reconciliation that survives partition (design
principle 5) and an edge component with offline authority.

Two forces are in tension:

- The standard must describe how orchestration reaches a physical zone across a
  WAN without requiring inbound firewall exceptions, or its multi-site and
  remote-control-plane topologies are unspecified.
- The design should not multiply edge components. There is already an edge Zone
  Controller at the facility; adding a separate "gateway" box beside it would
  duplicate the trust anchor, the offline-authority logic, and the deployment unit.

This is an architectural decision about the neutral standard. It does not describe
any hosted product; a hosted "site gateway" appliance would be one *implementation*
of the role defined here, and is out of scope for this standard.

## Decision

The **edge Zone Controller is the site connectivity boundary.** The single edge
component that owns a zone's offline authority also owns the connection between
the facility and the control plane, and it initiates that connection **outbound**.

- **Outbound-initiated, mutually-authenticated session.** The edge Zone Controller
  dials the control plane and maintains a persistent, mutually-authenticated
  session; the control plane never requires an inbound path to the facility. Task
  dispatch and telemetry multiplex over that session. This inverts the "control
  plane connects to each adapter" description for the cross-WAN case: adapters
  connect to their local edge Zone Controller, which is the one endpoint that
  reaches the control plane.
- **One deployable, two separable roles.** Zone control (local coordination and
  offline authority) and site uplink (the outbound connectivity boundary) are
  distinct roles that ship as one edge runtime when co-located — the common
  small-site case: one facility, one edge node, both roles. A larger site may run
  one uplink and several zone controllers; the roles are defined separately so
  they can scale independently, but nothing forces that split on a small site.
- **The edge node is the fleet's trust anchor.** The mutual-TLS identity presented
  to the control plane is the facility's identity. A robot reaches only its own
  zone's edge node, and that edge node reaches only its own control plane —
  isolation is anchored at the edge, not in a shared endpoint.
- **Offline authority is unchanged.** When the outbound session is down, the edge
  Zone Controller retains local authority for its zone exactly as today; the
  connectivity role does not weaken the partition-tolerance guarantee, it is the
  mechanism that carries it across a WAN.

## Alternatives considered

- **Separate gateway component beside the Zone Controller.** Rejected: it
  duplicates the trust anchor, the deployment unit, and the offline-authority
  surface for no benefit at a single-facility site. The connectivity boundary and
  the offline-authority boundary are the same boundary; splitting them invites
  drift between them.
- **Control plane keeps initiating inbound to adapters.** Rejected for cross-WAN
  topologies: it requires inbound firewall exceptions no facility IT will grant,
  and leaves the remote/multi-site control-plane case unspecified.
- **Push all connectivity into each Fleet Adapter.** Rejected: it spreads the WAN
  trust boundary across every vendor adapter, multiplies credential management,
  and puts survivability logic in vendor-specific code (contra ADR-0005). The edge
  Zone Controller is the natural single place for it.

## Consequences

- **Easier:** remote and multi-site control planes become expressible — a facility
  behind NAT can be orchestrated by a control plane anywhere with no inbound
  exposure. The trust and survivability story has one owner per site.
- **Harder:** the edge Zone Controller's responsibilities grow (session
  management, reconnection, backpressure over one multiplexed link). Its
  specification must state the connection-direction inversion and the offline
  contract precisely, and the conformance harness's edge checks (C8, currently
  follow-on) must cover disconnect/reconnect and offline-authority behaviour.
- **To revisit:** `docs/architecture.md` describes "the control plane connects to
  each adapter" and lists deployment topologies that predate this inversion; both
  need an editorial pass to reflect the outbound-initiated edge boundary. The
  normative wording belongs in an RFC amendment to RFC-0001, not in this ADR.
- **Boundary held:** this ADR defines a role, not a product. Any hosted site
  gateway, tunnel endpoint, or multi-cluster management built on this role is a
  separate product concern, out of scope for this standard.
