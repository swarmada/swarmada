# ADR-0034 — `spec.preferredRobot`: a soft "this robot if it can" scheduling preference

- **Status:** accepted
- **Date:** 2026-08-01
- **Affects:** `api/v1` (`FleetActionSpec`, `SwarmadaConfigSpec.Scheduling`),
  `internal/scheduler`, `internal/controller/fleetaction_controller.go`,
  RFC-0001 §control-plane rank phase.

## Context

A client that dispatches work *on behalf of a specific robot* — an operator UI, or an adapter acting
for that robot — could previously say only one of two things:

- `spec.robotSelector: <name>` — a **hard pin**: that robot or nothing.
- nothing at all — full scheduling across the fleet.

Neither expresses the common intent, "run this on the device I'm holding if it can, otherwise let
the fleet take it." Clients were forced to choose up front and, lacking a way to ask, to put the
choice to the operator as a dialog on every dispatch.

The hard pin is also a poor stand-in for the soft intent, because the reference implementation
short-circuits on it:

```go
// internal/scheduler/scheduler.go — robotMatchesAction
// Explicit robot selector overrides zone and capability checks.
if action.Spec.RobotSelector != "" {
    return robot.Name == action.Spec.RobotSelector
}
```

A pinned robot therefore skips the zone and capability predicates, so "pin the requester" can hand a
camera task to a device whose camera is off. (Note this is itself a divergence from RFC-0001, where
the selector is filter 6 of ten and all ten apply — tracked separately; it is not resolved by this
ADR, only avoided.)

## Decision

Add **`FleetAction.spec.preferredRobot`**, an optional robot name, applied as the **top soft rank
key**, above manufacturer match and battery level. Gate it on
**`SwarmadaConfig.spec.scheduling.honorPreferredRobot`**, defaulting to `true`.

Shape and rationale follow ADR-0022's manufacturer preference exactly — a per-action hint plus a
namespace flag, resolved by the caller and passed into the scheduler, which stays a pure decision
function.

**It ranks; it never filters.** The preferred robot is only in the eligible set if it passed every
hard filter, so the preference can never hand work to a robot that cannot do it. When the named
robot is busy, out of zone, low on battery, mid-model-update, under an estop, or missing a required
capability, it is simply absent and the fleet takes the action with no special case.

Three consequences worth stating because they are the questions people ask:

1. **No zone relaxation.** A preferred robot outside the action's zone stays ineligible. Relaxing
   the zone filter for a preferred robot would reintroduce the `robotSelector` problem
   deliberately.
2. **The flag defaults on.** The hint is opt-in per action, so a namespace that never sets
   `preferredRobot` is unaffected either way; a client that does set it means it. Turning the flag
   off makes the hint inert — it never makes the named robot *ineligible*.
3. **The preference is not spent.** It is a property of the action, so it still applies on
   reassignment (e.g. after capability-loss reassignment) if the robot becomes eligible again.

Naming a robot that does not exist, or one that is never eligible, is **not an error**. The hint
has no effect and the action schedules normally — a client should not be able to strand an action by
expressing a preference.

## Alternatives considered

- **Resolve it client-side into `robotSelector`.** Possible today with no spec change, and offered
  as an interim. Rejected as the answer: it is the hard pin, so it skips capability/zone checks and
  has no fallback, and it leaves the standard with no notion of the intent, so every client
  reinvents the decision.
- **A `self` sentinel resolved at admission.** Rejected for now: there is no robot identity to
  resolve from. The action's author is the client's ServiceAccount, and an adapter's mTLS SAN
  encodes adapter and tenant, not robot. Worth revisiting if actions ever become robot-authored —
  it is the only variant that is tamper-resistant, since a client could not then claim to be a robot
  it is not.
- **Generalise the selector to a `metav1.LabelSelector`** (as `RobotProbe.spec.robotSelector`
  already is). Attractive independently — it would enable affinity classes — but a selector is a
  hard filter, so it does not express "prefer", and it does not answer "which robot am I" without
  the client resolving that anyway.
- **A mode on the existing field (`robotSelectorPolicy: Require|Prefer`).** Rejected: two fields
  that must be read together, and the `Require` path keeps the capability/zone bypass, so the sharp
  edge survives with a better label. Keeping hard and soft in separate fields is easier to reason
  about and to validate.

## Consequences

- `Scheduler.SelectRobot` takes one more namespace-resolved flag. `DefaultScheduler` is the only
  implementation; a third-party scheduler implementing the interface must add the parameter, and may
  ignore it (the RFC's signature is explicitly illustrative, and the rank phase is
  implementation-defined).
- Behaviour is **unchanged** for every action that does not set `preferredRobot`, whatever the flag
  is set to. Covered by a regression test.
- Clients gain a way to drop "which robot?" prompts: express the preference and let the scheduler
  decide.
