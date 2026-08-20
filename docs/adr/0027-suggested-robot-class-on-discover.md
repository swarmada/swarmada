# ADR-0027: Populate DiscoveredRobot.status.suggestedRobotClass at Discover time

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** API Designer, Distributed Systems Reviewer, Security Reviewer
- **Related:** RFC-0001 §9.2.5 / §9.3.1 (two-phase discovery), §9.5.1.2 / §9.2.7 (per-message authorization, mTLS), §9.1.11 (provisioning); ADR-0014 (zero-touch auto-admit); ADR-0025 (mTLS ControlStream — the verified identity source)

## Context

Zero-touch auto-admit (ADR-0014) promotes a `DiscoveredRobot` to a schedulable
`Robot` without an operator when a namespace opts in. Its gate, in
`discoveredrobot_controller.go:maybeAutoAdmit`, is:

```go
if prov.AutoAdmitRobotClass == "" || dr.Status.SuggestedRobotClass != prov.AutoAdmitRobotClass {
    return false, nil
}
```

`prov` comes from `SwarmadaConfig.spec.provisioning` (`autoAdmitRobotClass` +
`autoAdmitZone`). The comparison is against `dr.Status.SuggestedRobotClass` — but
**nothing ever sets that field.** `registrar.go:Discover` builds the whole
`DiscoveredRobotStatus{…}` literal (phase, connectedAt, manufacturer, model,
reported hardware/capabilities/models, TTL) and omits `SuggestedRobotClass`; a
full-tree search finds only the reader above and the type definition. So the field
is always `""`, the equality never holds, and auto-admit is dead code: a discovered
robot with a new name never becomes a `Robot`, even over a fully verified mTLS
ControlStream.

Constraints that shape the fix:

- **Trust boundary.** The adapter's `AdapterHello` (vendor, namespace, …) is
  self-reported — the `AdapterIdentity` doc calls these "untrusted hints". The
  security boundary is the mTLS client-certificate SAN (ADR-0025), surfaced as
  `controlstream.TLSIdentity{AdapterName, Namespace, Verified}`. Auto-admit is a
  **privileged create**, so the suggested class must not be derivable from any
  unverified field.
- Auto-admit is already gated by `SwarmadaConfig.provisioning` and by the presence
  of a matching `RobotClass` + `FleetZone`; this ADR must not widen that gate.
- `Registrar.Discover(ctx, id AdapterIdentity, msg *DiscoverRobot)` does not
  receive the verified `TLSIdentity`, though the caller
  (`server.go:dispatch`) already holds `tlsID`.
- A `FleetAdapter` declares the classes it drives in `spec.servesRobotClasses`
  (`[]string`).

## Decision

Have the registrar derive the suggested class from the **verified adapter's own
`FleetAdapter`**, and populate `SuggestedRobotClass` at Discover time.

1. **Plumb the verified identity into Discover.** Change the `Registrar.Discover`
   signature to accept the verified identity explicitly:
   `Discover(ctx, id AdapterIdentity, tlsID controlstream.TLSIdentity, msg *DiscoverRobot) *DiscoverAck`.
   The caller in `server.go:dispatch` already has `tlsID` in scope and passes it.
   Keeping `TLSIdentity` a distinct typed parameter (rather than folding
   `AdapterName` into `AdapterIdentity`) preserves the verified-vs-untrusted
   separation at the type level. `Register` is unchanged — it re-attaches an
   already-admitted robot and needs no class suggestion.

2. **Resolve the class from `servesRobotClasses`.** Before writing `dr.Status`,
   `Get` the `FleetAdapter{Name: tlsID.AdapterName, Namespace: id.Namespace}`. If it
   is found and `len(spec.servesRobotClasses) == 1`, set
   `SuggestedRobotClass = spec.servesRobotClasses[0]`. If it has zero or multiple
   entries, or is not found, or the lookup errors, leave `SuggestedRobotClass`
   empty — auto-admit does not fire and the two-phase operator path stands. A
   lookup error never blocks discovery (fail safe).

3. **Write it in the existing status literal.** `SuggestedRobotClass` joins the
   `DiscoveredRobotStatus{…}` already written by `Status().Update` at discovery
   time — no new write, no proto change.

A single-class adapter (e.g. `servesRobotClasses: [amr-device]`) therefore
makes its discovered robots eligible for auto-admit; a multi-class or class-less
adapter yields an empty suggestion and stays on operator admission.

## Alternatives considered

- **ALT A — add `suggested_robot_class` to the `DiscoverRobot` proto.** The adapter
  declares the class per robot. More flexible for a heterogeneous adapter, but it
  is a proto/stubs change and, more importantly, it lets the class for a
  **privileged create** come from adapter-supplied wire input. Deriving it instead
  from the adapter's own `FleetAdapter.servesRobotClasses` — an operator-controlled,
  namespaced resource keyed by the *verified* adapter name — keeps the class under
  operator control. Rejected at v0.3; noted as the path if per-robot heterogeneous
  classes are later required.

- **ALT B — infer the class by fuzzy-matching reported hardware/capabilities to a
  `RobotClass`.** No configuration needed, but heuristic matching over
  self-reported inventory is brittle and could mis-admit a robot into the wrong
  class. Rejected.

- **Fold `AdapterName` into `AdapterIdentity` instead of a new parameter.** Smaller
  signature change, but it mixes a verified value into a struct explicitly
  documented as untrusted hints, inviting later code to trust the rest of the
  struct. Rejected in favour of the explicit `TLSIdentity` parameter.

## Consequences

- Auto-admit fires as ADR-0014 intended: with `provisioning.autoAdmitRobotClass`
  + `autoAdmitZone` set and a matching `RobotClass` + `FleetZone` present, a
  single-class adapter's newly-discovered robot is promoted to a `Robot` with no
  operator step. The previously-dead gate now has a real left-hand side.
- The privilege boundary is preserved and, in fact, tightened: the class is a
  function only of the **verified** adapter's own configuration. A forged
  `AdapterHello` cannot influence it — a mismatched name/namespace fails the
  `FleetAdapter` lookup and yields an empty suggestion (no admission). The
  `provisioning` gate and the class/zone existence checks are unchanged.
- New obligations: `Discover` performs one additional `FleetAdapter` `Get`. This is
  on the **discovery** path (a first announce of a new robot), not the telemetry
  hot path, so the cost is negligible. The `Registrar` interface signature changes,
  so the one real implementation and the test fakes are updated.
- **RA-1 unaffected:** the write is the existing discovery-time
  `DiscoveredRobot.status` update, never a telemetry tick.
- Seam left: a single adapter that drives more than one class cannot auto-admit
  under this rule (ambiguous → empty). If that becomes a real need, ALT A (an
  explicit per-robot proto field, still validated against `servesRobotClasses`) is
  the forward path.
