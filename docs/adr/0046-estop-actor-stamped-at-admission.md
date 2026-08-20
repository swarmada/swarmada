# ADR-0046: Stamp the authenticated operator at admission; never let attribution block a stop

- **Status:** accepted
- **Date:** 2026-08-20
- **Deciders:** Al — Option A (mutating webhooks stamp `swarmada.io/estop-actor` from the admission
  request) approved; Option B (caller self-asserts the actor, validator refuses a mismatch) rejected
  because it puts an identity round-trip on the estop path. Fallback reuses `service-account` with an
  `unattributed:` prefix rather than adding a fourth `ActorType`; the chart's webhook drift is closed
  and gated in the same change
- **Related:** RFC-0001 §9.6.5 (estop state machine and its audit record), §9.5.4 (audit envelope,
  `actor.type`), §9.6.2 (estop scopes and timing), §F-2b (estop verb authorization);
  ADR-0041 (rollout resume — the second operator intent this attributes), ADR-0025 (the cert-manager
  webhook serving path this reuses unchanged), ADR-0033 (`emitted` means a writer exists)

## Context

The control plane authorizes a real human and then throws the name away.

`authorizeEstopVerb` (`internal/webhook/fleetzone_webhook.go`) runs a `SubjectAccessReview` against
`req.UserInfo` for every estop trigger and clear. It fails closed and it works. But the admission request
ends there, and the estop controllers reconcile asynchronously — by the time `ZoneEstopReconciler` seals
`ESTOP_TRIGGERED`, the request that carried the identity is long gone. So the controllers stamped an
actor derived from the scope instead:

    zone-estop:<zone>   robot-estop:<robot>   namespace-estop:<namespace>   firmwarerollout-controller

with `actor.type: service-account`. Two consequences, and the second is worse than the first.

**The safety audit log could not attribute an emergency stop to a person.** For a log whose stated
purpose is evidence for a safety case, the actor on the most safety-relevant event in the system was a
string derived from the object being acted on.

**An unattributable entry was indistinguishable from an attributed one.** `zone-estop:floor-1` reads
exactly like a service account someone configured. An auditor had no way to tell an entry whose actor
was verified from one whose actor was never captured — so every estop entry had to be treated as the
weaker of the two.

The same gap covered `ROLLOUT_RESUMED` on both rollout kinds (ADR-0041's operator resume), which is an
operator intent sealed by a controller for exactly the same reason.

## Decision

**Persist the identity at the one moment it exists. A mutating admission webhook stamps
`swarmada.io/estop-actor` from `req.UserInfo.Username` when an estop or resume annotation transitions;
the controllers read it off the object they already fetch.**

1. **Five carriers, four new mutators.** `FleetZone`, `SwarmadaConfig`, `FirmwareRollout` and
   `ModelRollout` each get a stamping defaulter; `Robot` already had a mutating webhook, so its stamp
   rides `RobotDefaulter`. `ModelRollout` is included even though only `FirmwareRollout` was named in the
   original scope: both seal `ROLLOUT_RESUMED`, and attribution that differs between two halves of one
   feature is worse than uniform absence.

2. **Stamp on the TRANSITION, not on presence.** Added, re-valued and removed are all estop edges —
   removal is the clear, which is the act most worth attributing. An update that leaves the annotation
   untouched must not restamp, or an unrelated label edit silently reattributes a standing estop to
   whoever made it.

3. **The stamp is the webhook's assertion, not a field the caller fills in.** A client-supplied value is
   overwritten on the same admission, so writing the annotation directly cannot name someone else.

4. **`failurePolicy: Ignore` on all four.** This is the invariant expressed as a marker: attribution is
   best-effort, authorization is not. If the webhook server is unreachable the stamp is skipped and the
   estop still lands. Authorization is untouched — the *validating* webhooks keep `failurePolicy: Fail`
   and keep running the SAR.

5. **Unresolved identity is recorded as unresolved.** `estopActor` returns
   `service-account` + `unattributed:<scope>` when no stamp is present. It never returns `ActorUser`
   for an identity nobody authenticated, and the `unattributed:` prefix is deliberately not a plausible
   principal name, so the "attributed vs unattributable" distinction the old actors destroyed is legible
   again. The scope is retained after the prefix: which estop it was, was never in doubt.

6. **The identity rides the envelope only.** `audit.Entry.Actor`, never the `Detail` map —
   `scripts/specdiff.py`'s `_ENVELOPE_FIELDS` check fails on duplicated envelope identity, and a test
   asserts the absence directly so the failure surfaces before the spec gate.

7. **Controller-authored events keep their service-account actor.** `sealFirmwareEventAs` /
   `sealModelEventAs` take an explicit actor and only `ROLLOUT_RESUMED` uses it. Install outcomes and the
   pause edge are genuinely the controller's own acts; naming a user there would be a false attribution,
   which is the defect this ADR exists to remove, pointed the other way.

## Alternatives considered

- **Option B — the caller self-asserts the actor and the validator refuses a mismatch.** No new
  webhooks, no mutation. Rejected, and not narrowly: **a client cannot reliably compute the username the
  API server will derive for it.** Kubeconfig auth may be a client certificate, a bearer token, an exec
  plugin or OIDC; the honest implementation needs a `SelfSubjectReview` round-trip *on the estop path*,
  and any failure of that round-trip — RBAC denying `selfsubjectreviews`, an older cluster, a network
  blip — **refuses the emergency stop**. The bare-`kubectl` mode is worse: an operator would have to type
  their identity exactly as the API server sees it (`system:serviceaccount:ops:oncall`, or an OIDC
  subject with an issuer prefix), and a typo means the stop is rejected. A safety control that declines
  to engage because attribution metadata was malformed has inverted its own purpose. Option A cannot fail
  that way: the worst case is a less informative log entry.
- **Record the estop entry from the validating webhook**, as `recordConfigModified` already does for
  `SWARMADA_CONFIG_MODIFIED`. Rejected: the estop entry carries fan-out results the webhook does not have
  — `robots_in_scope`, worst ack latency, per-robot outcome — because they do not exist until the
  fan-out runs. Splitting into an admission-time "requested" entry plus a controller-time "outcome" entry
  would work, but it changes the §9.6.5.1 event table and doubles the entries an auditor must correlate.
- **A fourth `ActorType` (`unattributed`).** Cleaner to consume and machine-checkable. Rejected by Al:
  `actor.type` is a normative §9.5.4 enum, and widening it to describe a *provenance* property rather
  than an actor *kind* pushes the cost onto every existing consumer. `service-account` +
  `unattributed:` prefix carries the same information without a wire change.
- **Leave the disclosure in place.** It was honest and it was cheap. Rejected because the disclosure's
  own remediation ("recover attribution from the API server audit log, correlated by timestamp") asks
  every operator to build a correlation pipeline for something the control plane already knew and
  discarded.

## Consequences

- **Good.** `ESTOP_TRIGGERED`, `ESTOP_CLEARED` and `ROLLOUT_RESUMED` name a person, on all three estop
  scopes and both rollout kinds. A safety case can cite this log for operator attribution rather than
  the API server's.
- **Good.** An auditor can now distinguish an attributed entry from an unattributable one, which no
  amount of care could recover from the previous scheme.
- **Obligation — the two halves must keep the same annotation name.** `internal/webhook`'s
  `AnnEstopActor` and `internal/controller`'s `annEstopActor` are separate constants (the dependency
  runs one way only). A rename that touches one and not the other silently degrades every entry to
  unattributed, and nothing else would fail. A test pins the spelling.
- **Obligation — `failurePolicy: Ignore` must survive regeneration.** It is carried by the kubebuilder
  markers and rendered literally by the chart generator (the chart's `webhooks.failurePolicy` override
  applies only to `Fail` webhooks, so an operator cannot tighten a stamper into the estop path).
- **RA-1 / safety.** No new blocking surface: the validating webhooks on these resources already had
  `failurePolicy: Fail` and already gated the same writes, so the availability posture of an estop write
  is unchanged. The stampers add no status writes and no reconcile work.
- **Drawback accepted.** The robot-scope stamp rides `RobotDefaulter`, which is `failurePolicy: Fail`
  because the RobotClass merge must not be skipped. A robot-scoped estop therefore inherits Robot
  admission's existing availability posture rather than the `Ignore` of the other four. This is not a
  regression — the same write already went through that webhook and through the fail-closed
  `RobotAdmissionGate` — but it is an asymmetry, and it is documented at the call site.
- **Drawback accepted.** An operator who edits the annotation twice in one `kubectl apply` (removing and
  re-adding within a single admission) is attributed once, for the net transition. There is one request
  and one identity; the intermediate state never existed on the server.
- **Scope discovered, and closed here.** `deploy/swarmada/templates/webhook.yaml` was a HAND-written
  mirror of `config/webhook/manifests.yaml` with no sync tool and no gate, and it had drifted to 4 of 9
  webhooks — a Helm-installed cluster was not enforcing five validators, including the FleetAction
  cancel-verb SAR and the rollout delete guards. `hack/sync-helm-webhooks.py` now generates it and
  `make helm-verify-sync` gates it, the webhook twin of the CRD sync that already existed. Syncing also
  surfaced pending CRD drift from the previous round's validation markers.
