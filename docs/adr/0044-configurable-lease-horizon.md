# ADR-0044: Make the assignment-lease horizon namespace-configurable, bounded, and resolved once

- **Status:** accepted
- **Date:** 2026-08-20
- **Deciders:** Al — `spec.scheduling.{leaseDurationSeconds,clockSkewMarginSeconds}` with the bounds
  below approved; the renewal interval stays derived (`duration / 3`) rather than becoming a third
  field, and no per-`FleetAction` override is added
- **Related:** RFC-0001 §9.6.3.5 (assignment lease and the single-executor guarantee),
  §9.6.3.2 (control-plane response timeline), §9.6.3.3 item 1 (adapter local behaviour),
  §9.2.8 (Fleet Adapter compliance checklist — "Task-lease self-stop"), §9.3.8 (metrics and the
  stuck-`Revoking` alert); ADR-0011 (the per-namespace connectivity thresholds this mirrors),
  ADR-0012 (when a resolved field must be a nil-able pointer instead), ADR-0030 (fail-closed lease
  reads in the Robot controller)

## Context

The task-lease horizon is the load-bearing number in the single-executor guarantee. Two independent
timers are derived from it, on opposite sides of the wire:

1. The control plane writes `now + duration` to `FleetAction.status.leaseExpiresAt` and refuses to
   reassign an action until `now ≥ leaseExpiresAt + skew` (§9.6.3.5 condition 3).
2. The robot's Fleet Adapter receives the same number as `Command.lease_duration_ms` and arms a
   self-stop timer from it. When the lease is not renewed in time the adapter MUST bring the action
   to a safe stop on its own (§9.2.8 "Task-lease self-stop", §9.6.3.3 item 1).

The guarantee holds only because those two timers are the same quantity. The robot has provably
halted before the control plane hands the action to anyone else.

Until this decision both numbers were compile-time constants in
`internal/controller/fleetaction_controller.go` — `leaseDuration = 30s`, `leaseClockSkew = 5s` — with
a standing TODO to source them from `SwarmadaConfig.spec.scheduling`. That left three defects:

- **No operator control.** 30s is right for a warehouse AMR on good Wi-Fi. It is wrong for a robot
  on a link with multi-second round trips, or for a long-running action whose safe-stop procedure
  itself takes tens of seconds. Operators had no way to say so.
- **Phantom configuration in the spec.** §9.3.8 carried a normative requirement that a `Revoking`
  count persisting longer than `leaseDurationMs + clockSkewMarginMs` MUST trigger an alert, and the
  shipped `PrometheusRule` repeated the phrase to operators. Neither symbol existed as a field
  anywhere in the tree, and `clockSkewMarginMs` had no defined value at all, so an operator could not
  compute the threshold the MUST told them to alert on.
- **Two spellings, one quantity.** §9.6.3.2's timeline said `leaseDuration + clockSkewMargin`, §9.3.8
  said `leaseDurationMs + clockSkewMarginMs`, and the wire says `lease_duration_ms`. Nothing stated
  which was authoritative or how they converted.

Separately, `crds/swarmadaconfig.md`'s worked connectivity sequence was built on
`leaseDurationMs = 90s` — three times the shipped value — so every timestamp an operator might have
reasoned from was wrong.

## Decision

**Add `spec.scheduling.leaseDurationSeconds` (default 30, min 10, max 300) and
`spec.scheduling.clockSkewMarginSeconds` (default 5, min 1, max 60) to `SwarmadaConfig`, resolve both
through a single fail-safe helper, and thread that one resolved value to every consumer including the
wire.**

1. **Schema defaults, not controller defaults.** Both are plain `int32` with `+kubebuilder:default`
   and explicit `Minimum`/`Maximum`, matching `spec.health.connectivityOfflineThresholdSeconds`.
   ADR-0012's nil-able-pointer exception does not apply: this is a two-level resolution (namespace
   config over built-in constant), not the three-level per-object case, so there is no "unset vs
   explicit" distinction for a schema default to erase.

2. **One resolver, failing safe.** `leaseTimings(ctx, client, namespace) (duration, renew, skew)` in
   `internal/controller/fleetaction_controller.go` reads the namespace `SwarmadaConfig` once through
   the shared `namespaceConfig` helper and falls back to `defaultLeaseDuration` /
   `defaultLeaseClockSkew` on *any* problem — no config, list error, or non-positive value — exactly
   as `connectivityTimeouts` does for the health thresholds (ADR-0011). An unreadable policy can
   never lengthen the window a disconnected robot keeps moving.

   It is a free function taking a `client.Client` rather than a reconciler method because the Robot
   controller resolves the same skew in `assignedLeaseProvablyDead`. Both controllers must judge
   lease death by the same number; a second copy of the resolution logic is exactly the drift this
   ADR exists to prevent.

3. **The renewal interval stays derived.** `renew` is always `duration / 3` (§9.3.2) and is not a
   field. A separately configurable interval would let an operator widen the horizon without renewing
   any sooner, which is the one combination that makes a long horizon dangerous rather than only
   slow.

4. **The wire takes the resolved value as a parameter, not a lookup.** `pushAssignAction` and
   `pushRenewLease` now accept `leaseDur time.Duration` from the caller that wrote
   `status.leaseExpiresAt`, rather than reading a package constant or re-resolving. The compiler now
   requires the two halves of the guarantee to come from the same expression.

5. **One spelling in the spec.** `spec.scheduling.leaseDurationSeconds` and `clockSkewMarginSeconds`
   are the normative names, in seconds; `lease_duration_ms` is the wire encoding of the first, in
   milliseconds, converted at the wire boundary. §9.6.3.5 states the conversion; `leaseDurationMs`
   and `clockSkewMarginMs` are retired from normative prose.

## Why these bounds

The bounds are the safety argument, not schema hygiene. `lease_duration_ms` reaches the adapter and
arms the self-stop timer, so **`leaseDurationSeconds` is a direct upper bound on how long a robot
that has lost its link keeps moving before halting itself.** It is a safety-relevant knob, not a
tuning knob, and it is the only field in `spec.scheduling` of which that is true.

- **`leaseDurationSeconds` max 300.** Five minutes of unsupervised motion after a link loss is the
  most that is defensible for a floor robot at all, and it is already generous. The ceiling exists so
  that a typo or a copied-in "make the flapping stop" value cannot silently buy a disconnected robot
  an unbounded run. An operator who genuinely needs longer needs a different safety case, not a
  bigger number.
- **`leaseDurationSeconds` min 10.** The renewal interval is `duration / 3`, so a 10s floor keeps
  renewals at ~3.3s — clear of ordinary reconcile latency and wire round trips. Below that, normal
  scheduling jitter starts to look like lease expiry and healthy robots get revoked.
- **`clockSkewMarginSeconds` min 1.** The margin is what guarantees the robot has already self-halted
  when the control plane reassigns. Zero would permit reassignment at the same instant the robot's
  own timer fires, so any clock disagreement at all opens a double-execution window. It may never
  be zero.
- **`clockSkewMarginSeconds` max 60.** The margin buys safety by delaying recovery; past a minute it
  is paying real downtime for skew that NTP-disciplined clocks do not have. A deployment needing more
  has a clock problem to fix, not a margin to raise.

## Alternatives considered

- **Leave both as constants and only document the values.** Fixes the phantom-configuration defect in
  the spec at zero risk, and was tempting for exactly that reason. Rejected: it leaves operators with
  a safety-relevant timer they cannot match to their link quality or their robots' stopping distance,
  and the §9.3.8 MUST-alert threshold stays uncomputable in any deployment that is not at defaults.
- **Make the renewal interval a third field.** Rejected under item 3 above — it exists only to enable
  the dangerous combination.
- **Per-`FleetAction` override (`spec.leaseDurationSeconds`).** Genuinely attractive: a slow docking
  action plausibly wants a longer horizon than a navigate. Rejected at v0.3 because it makes the
  horizon settable by whoever submits work — typically an integration, not an operator — which is the
  wrong side of the trust boundary for a field that governs unsupervised motion. It would also force
  the field to a nil-able pointer with no schema default per ADR-0012. Revisit only with an
  admission-side ceiling.
- **Re-resolve the config inside `pushAssignAction`/`pushRenewLease` instead of passing it in.**
  Less plumbing, and correct almost always. Rejected: a config edit landing between the status write
  and the wire push would send the robot a different horizon than the one the control plane is
  holding itself to. Passing the value makes that unrepresentable.
- **Unbounded fields, or bounds only in documentation.** Rejected: the CRD schema is the only place a
  bound is actually enforced, and the whole point of the ceiling is to stop a value nobody reviewed.

## Consequences

- **Good.** The §9.3.8 stuck-`Revoking` alert threshold is now computable from two real fields, so the
  normative MUST is satisfiable. The TODO in `fleetaction_controller.go` is discharged.
- **Good.** `leaseTimings` is the single resolution point for both controllers and for the wire, so
  "the robot and the control plane disagree about the lease" is now one function's responsibility
  rather than a property that had to hold across nineteen call sites.
- **Obligation — the constants must track the CRD defaults.** `defaultLeaseDuration` (30s) and
  `defaultLeaseClockSkew` (5s) are fail-safe fallbacks and MUST equal the `+kubebuilder:default`
  values. `make spec-check` enforces the doc↔CRD half of that; the Go↔CRD half is by hand, as it is
  for ADR-0011's thresholds.
- **Obligation — the shipped `PrometheusRule` cannot read namespace config.** Its `for: 1m` clears
  the 35s default horizon, but a namespace configuring a horizon above 55s must raise `for` past its
  own horizon or the alert fires on revocations still legitimately waiting out the lease. Stated in
  the rule's comment and its description, because a static rule cannot compute it.
- **RA-1 / safety.** Bounds are enforced at admission, and every read path fails safe *down* to the
  built-in defaults, never up. The failure mode the bounds exist to prevent — a robot self-stopping
  at 30s while the control plane waits out a 90s horizon, leaving the action assignable to a second
  robot while the first still moves — is now covered by a regression test that asserts the pushed
  `lease_duration_ms` equals the configured horizon rather than any literal.
- **Drawback accepted.** `confirmedStopWithDisposition` and `holdStop` re-resolve the config rather
  than receiving it, because they are reached from paths where no resolved value is in scope. These
  are read-only decisions (is the lease dead; when to requeue) and never touch the wire, so a
  mid-reconcile config edit changes only timing, not what the robot was told. Threading a value
  through every caller was not worth the churn for that.
- **Drawback accepted.** Raising `leaseDurationSeconds` slows recovery from a genuine robot loss by
  the same amount it extends the safety horizon. That trade is the operator's to make, which is the
  point; the bounds keep it inside a defensible range.
