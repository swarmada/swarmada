# ADR-0040: Carry the zone boundary on `RegisterAck`, and refuse an off-zone assignment

- **Status:** proposed
- **Date:** 2026-08-16
- **Deciders:** _(pending)_
- **Related:** ADR-0032 (contract versioning — bump rules applied below), ADR-0039 (the other additive `fleet_adapter.v1` field awaiting a decision), RFC-0001 §9.3.1 / §9.3.4 (FleetZone, point-in-polygon containment), `adapters/CONFORMANCE.md` C2.3 (honour `RegisterAck` configuration) and C3 (anti-double-execution posture)

## Context

A `FleetAction` whose destination lies outside the world the robot operates in is dispatched,
accepted by the adapter, and then fails somewhere downstream — in the ROS 2 case at the Nav2
planner, which answers *"Goal Coordinates of(50.0, 50.0) was outside bounds"* about two seconds
later. Nothing refuses it at the point where refusing is cheap and unambiguous.

**The platform already models the boundary.** `FleetZone.spec.physicalBounds` carries it:

```go
type PhysicalBounds struct {
    Floor       int32     // 0 = ground, relative to SwarmadaConfig.coordinateSystem
    Polygon     []Point   // closed boundary, >=3 vertices; self-intersection rejected
    MinAltitude *float64  // geodetic/aerial namespaces only
    MaxAltitude *float64
}
```

and the Zone Controller already runs point-in-polygon against it to derive a robot's current
zone from position telemetry (§9.3.4). The model is sound and, importantly, **coordinate-system
neutral** — a polygon plus a floor, or an altitude band, describes a warehouse aisle and a drone
corridor equally well. That matters because the three reference adapters (`-ros2`, `-vda5050`,
`-mavlink`) have entirely different geometry, and a shape that only suited one of them would be
the wrong abstraction.

**What is missing is plumbing, not modelling.** `RegisterAck.assigned_zone` is a bare `string`:
the zone's *name*. The polygon stays in the cluster. An adapter therefore cannot answer the one
question it is best placed to answer — *"is this target inside the world I was given?"* — even
though the control plane knows.

The ROS 2 adapter has no usable local substitute. Nav2's global costmap is a **rolling 40 m
window centred on the robot**, so "inside the costmap" describes where the robot is standing,
not where the world ends; a bounds check against it would reject valid distant targets and
accept invalid ones as the robot drives.

## Decision

**Carry the operating zone's boundary on `RegisterAck`, and give the adapter an enumerated way
to refuse a target outside it.**

```protobuf
message RegisterAck {
  // ... accepted = 1, rejection = 2, message = 3, authoritative_action_state = 4,
  //     telemetry_interval_seconds = 10, assigned_zone = 11,
  //     active_capabilities = 12, edge_endpoints = 13 ...

  // The operating boundary for `assigned_zone`, mirroring FleetZone.spec.physicalBounds.
  // Absent means the control plane states no boundary: the adapter MUST NOT invent one
  // and MUST NOT refuse on bounds grounds. (Explicit presence, same discipline as C6.2.)
  ZoneBounds zone_bounds = 14;
}

message ZoneBounds {
  string  zone           = 1;  // FleetZone name; matches assigned_zone
  string  zone_version   = 2;  // the FleetZone resourceVersion this geometry came from
  int32   floor          = 3;
  repeated Point polygon = 4;  // closed boundary, >= 3 vertices
  optional double min_altitude = 5;  // geodetic/aerial namespaces only
  optional double max_altitude = 6;
}

enum AssignActionRejection {
  // ... UNSPECIFIED = 0 ... INVALID_ACTION = 5 ...
  ASSIGN_ACTION_REJECTION_TARGET_OUTSIDE_ZONE = 6;  // resolved target is outside zone_bounds
}
```

`RegisterAck` fields 1–4 and 10–13 are in use, so **14** is free; `AssignActionRejection` uses
0–5, so **6** is free.

**A distinct rejection reason, not `INVALID_ACTION`.** `INVALID_ACTION = 5` means "unparseable /
unsupported". The control plane must be able to tell *"I cannot understand this command"* from
*"I understood it and it points outside my world"* — only the second is a routing or planning
error, and only the second implicates the zone assignment.

**`zone_version` is not decoration.** It is what makes "which map both ends are talking about"
answerable, and it is what a map-change gate keys on. The `FleetZone` already has a
`resourceVersion`; no new identity needs inventing.

### Enforcement sits in two places, deliberately

* **Control plane, before dispatch.** The Zone Controller already computes point-in-polygon.
  Refusing to dispatch a `FleetAction` whose resolved destination falls outside the robot's zone
  chain is the stronger guard: the command never exists. **This needs no contract change and
  should land first.**
* **Adapter, on assignment.** Defence in depth for a control plane that is wrong, stale, or
  bypassed — the same posture the contract already takes with fencing tokens, where the adapter
  re-checks an ordering the control plane has already decided. This is the half that needs the
  wire change.

### The map-change requirement is NOT part of this ADR

"A map may only change while no in-flight task is affected" is enforced in the cluster, not on
the wire: a guard in `FleetZoneValidator.ValidateUpdate` (`internal/webhook/fleetzone_webhook.go`),
which already re-runs structural checks on update and has no such guard today. It needs no proto
change and is tracked separately. It is mentioned here only because `zone_version` is what lets
an adapter notice that the geometry it was given is no longer current.

### The version question, from ADR-0032's rules

ADR-0032 requires a deliberate bump when the proto surface changes, and states that a **major**
bump is breaking and forces re-qualification, while patch/minor never invalidate an adapter.

* Adding an `optional`/absent-able message field and a new enum value is **additive and
  wire-compatible**: an adapter built against 1.0.0 ignores `zone_bounds` under the proto3
  unknown-field rule, and never emits the new rejection value.
* It changes the proto surface, so it **does** require a bump.
* It does not break an existing adapter, therefore **minor**: contract **1.0.0 → 1.1.0**.

**A new enum value is not free, and this is the trap.** A control plane that receives
`TARGET_OUTSIDE_ZONE = 6` from a newer adapter while running older generated code sees an
unknown enum number. Proto3 preserves it, but any exhaustive `switch` on the rejection reason
must have a default arm that treats an unrecognised rejection as *a rejection* — never as an
accept. That is a fail-closed requirement on the consumer and belongs in the ADR, not discovered
later.

If ADR-0039 also lands, the two additive changes should share the single 1.1.0 bump rather than
taking one each.

**Conformance lands as SHOULD initially**, for the same reason as ADR-0039: an adapter that does
not implement the check is not *wrong* under 1.0.0, and a new MUST would fail every existing
adapter — breaking behaviour reached through a non-breaking wire change, which is exactly what
ADR-0032's bump rules exist to prevent. It tightens to MUST at the next major.

## Consequences

- **The adapter can refuse what it is best placed to refuse**, at the moment of assignment, with
  a reason the control plane can act on — instead of accepting and failing seconds later in a
  vendor-specific planner with a vendor-specific message.
- **A conformance check becomes writable**: advertise bounds on `RegisterAck`, assign a target
  outside them, require `accepted=false` with `TARGET_OUTSIDE_ZONE`. Today the requirement is
  unverifiable, which is how it would join C3.1's company as a MUST with no test.
- **Adapters that ignore `zone_bounds` keep working**, and are not silently non-conformant while
  the check is SHOULD.
- **The polygon is repeated on every `RegisterAck`.** A large zone boundary is sent per
  registration, not per telemetry tick, so the cost is bounded; `zone_version` lets a future
  optimisation skip resending unchanged geometry.
- **Altitude is carried but unused by ground adapters.** Deliberate — the alternative is a
  ground-shaped field that `-mavlink` cannot use.
- **This ADR changes no code.** The proto, generated stubs, `CONFORMANCE.md` and RFC-0001 are
  untouched.

## Alternatives considered

- **A bounding box instead of a polygon.** Smaller and simpler, but it cannot express an
  L-shaped aisle or any non-convex zone, and the platform already stores a polygon — a box would
  be a lossy re-encoding of data that exists, and "inside the box but outside the zone" would be
  accepted.
- **Send the map instead of the boundary** (an occupancy grid, or a reference to one). Far more
  data, vendor-specific encoding, and it answers a different question: obstacles inside the world
  versus the edge of the world. Obstacle avoidance is the planner's job and stays there.
- **Reuse `INVALID_ACTION` for the rejection.** Rejected under *Decision*: it conflates "cannot
  parse" with "outside my world".
- **Adapter-side only, with bounds from local configuration.** Puts the boundary in two places
  that must agree and provides no way to detect divergence — the failure mode ITEM-0113 records
  for harness copies, reproduced in the safety path.
- **Control-plane only, no adapter check.** Sufficient when the control plane is correct, which
  is the assumption fencing tokens already refuse to make.
