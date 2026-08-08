# ADR-0014: Auto-admit DiscoveredRobots via a class+zone match in the DiscoveredRobot controller

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** API Designer, Kubernetes Expert, Distributed Systems Reviewer, Security Reviewer
- **Related:** RFC-0001 §9.1.2 (two-phase provisioning), §9.1.11 (SwarmadaConfig
  provisioning), ADR-0012/0013 (config-sourced behavior), the RA-1 discipline

## Context

Provisioning is two-phase by default: a robot announces (Discover) → a
`DiscoveredRobot` is staged → an operator runs `swarmctl admit` to build a
schedulable `Robot`. `SwarmadaConfig.spec.provisioning.autoAdmitRobotClass` is
defined to skip the operator step — "robots matching this RobotClass are admitted
automatically without operator review" — but nothing reads it.

Wiring it exposes a gap: **`autoAdmitRobotClass` alone cannot produce a schedulable
Robot.** `RobotSpec.Zone` is mandatory and must reference a leaf FleetZone;
`swarmctl admit` gets it from the operator's required `--zone` flag. Neither the
`RobotClass` nor the `DiscoveredRobot` carries a zone (robots are discovered before
placement), and the config names only a class. Auto-admit therefore needs a second
input: which zone the auto-admitted robots join.

Two more forces:

- The Robot-building logic (`buildRobotFromDiscovered`) lives in `cmd/swarmctl`
  (package main) and is not importable by a controller.
- Admission is a privileged action (it creates a schedulable Robot). Doing it
  automatically must be conservative and auditable.

## Decision

**Add a companion field and perform auto-admit in the DiscoveredRobot controller,
gated on a full class+zone match.**

1. **New config field (api/v1).** Add
   `SwarmadaProvisioningConfig.AutoAdmitZone string`. Auto-admit is enabled only
   when **both** `autoAdmitRobotClass` and `autoAdmitZone` are set; a class without
   a zone is inert (auto-admit cannot build a schedulable Robot) and the controller
   logs a warning rather than guessing a zone. (A namespace Event is a future nicety
   once the controller gains an EventRecorder.)
2. **Trigger.** In the DiscoveredRobot controller (which already reconciles
   DiscoveredRobots for TTL), before the TTL sweep: if the namespace config enables
   auto-admit and `dr.status.suggestedRobotClass == autoAdmitRobotClass` and the
   named RobotClass and leaf zone exist, build the Robot and admit it.
3. **Build + admit.** Build the Robot from the RobotClass (authoritative typed
   collections) plus the discovered manufacturer/model, with `zone = autoAdmitZone`.
   Create the Robot, then delete the DiscoveredRobot — create-before-delete, so a
   failure never loses the staging object (identical ordering to `swarmctl admit`).
4. **Match is conservative.** Only the adapter-suggested class
   (`status.suggestedRobotClass`) is matched — never inferred from manufacturer/model
   heuristics. No suggestion ⇒ no auto-admit ⇒ the operator path stands.

## Alternatives considered

- **Auto-admit in the registrar's Discover path** (create a Robot instead of a
  DiscoveredRobot on first announce). Rejected: Discover is a hot, per-connection
  path that must stay lean and in-band; it would also skip the DiscoveredRobot record
  entirely, losing the staging/audit trail and duplicating class/zone lookups on
  every announce. The controller reconcile loop is the right place for a privileged,
  occasionally-run action.
- **Derive the zone instead of adding a field** — e.g. auto-select the namespace's
  sole leaf zone. Rejected as too magical and fragile (breaks the moment a second
  zone appears); an explicit `autoAdmitZone` is predictable and auditable.
- **Put the zone on the RobotClass.** Rejected: a RobotClass is a reusable hardware
  template shared across zones; binding it to one zone conflates template with
  placement. Placement belongs in the namespace provisioning policy.
- **Extract `buildRobotFromDiscovered` into a shared package now.** Deferred: the
  controller's auto-admit only needs the class path (no operator-override flags), so
  it uses a focused local builder. Consolidating the two builders into
  `internal/admission` is a worthwhile follow-up but is not required here and would
  churn `swarmctl` and its tests.

## Consequences

- **Good.** `autoAdmitRobotClass` finally functions, with the zero-touch onboarding
  it was meant for, behind an explicit, auditable class+zone gate.
- **Obligation — schema regen.** Adds `autoAdmitZone` to `api/v1`; run
  `make generate manifests`. Additive optional field — not breaking.
- **Obligation — RBAC.** The DiscoveredRobot controller must gain `robots: create`,
  `robotclasses: get;list;watch`, `fleetzones: get;list;watch`, and
  `swarmadaconfigs: get;list;watch`. This widens a previously read/delete-only
  controller — noted for the Security Reviewer.
- **Safety.** Create-before-delete preserves the staging object on failure. Matching
  only the adapter-suggested class (never a heuristic) keeps auto-admit conservative;
  the operator two-phase path remains the default whenever the gate is not fully
  configured.
- **Obligation — tests.** Match→admit (Robot created, DR deleted); class set but zone
  unset ⇒ inert + event; suggestion mismatch ⇒ no admit; missing RobotClass/zone ⇒
  no admit, DR retained.
- **Drawback accepted / seam.** A second Robot builder exists briefly (controller vs
  swarmctl); consolidating into `internal/admission` is filed as follow-up. Auto-admit
  also assigns every matching robot to a single `autoAdmitZone`; multi-zone auto-admit
  would need a richer policy and is out of scope.
