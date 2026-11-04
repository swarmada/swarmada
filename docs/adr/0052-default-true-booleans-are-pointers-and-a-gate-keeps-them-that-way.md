# ADR-0052: A default-true CRD boolean is a pointer, and a mechanical gate keeps it that way

- **Status:** accepted
- **Date:** 2026-09-06
- **Deciders:** API Engineer, Kubernetes Expert
- **Related:** ADR-0051 (the signing field, where this shape was first found and fixed, and the
  warning-instead-of-rejection precedent this reuses), RFC-0001
  [§9.1.6](../../rfcs/dist/RFC-0001-core-spec.md) (`FleetZone.spec.estopPolicy`),
  RFC-0001 #firmwarerollout-schema and #modelrollout-schema (rollout `safetyConstraints`, `strategy.rollingUpdate`),
  [api-principles.md](../api-principles.md) ("Optional scalars are pointers", "Versioning and
  compatibility")

## Context

ADR-0051 fixed `SwarmadaConfig.spec.signing.requireSignatureVerification`, a published CRD boolean
that could not be set to `false`: a non-pointer `bool` with `omitempty` and
`+kubebuilder:default=true` serializes its zero value away, and structural defaulting restores the
default. The operator's `false` is discarded with no error.

A sweep of `api/v1` for that exact shape found **five** fields, not one. The other four:

| Field | JSON path | What it gates |
| :-- | :-- | :-- |
| `RollingUpdateStrategy.PauseOnError` | `spec.strategy.rollingUpdate.pauseOnError` | whether a failed robot halts a rollout |
| `RolloutSafetyConstraints.RequireIdleState` | `spec.safetyConstraints.requireIdleState` | whether a rollout may touch a robot that is not `Idle` |
| `ZoneEstopPolicy.PropagateToChildren` | `spec.estopPolicy.propagateToChildren` | whether an estop reaches descendant zones |
| `SwarmadaEstopConfig.RequireExplicitClearAfterEstop` | `spec.estop.requireExplicitClearAfterEstop` | whether an estopped robot needs an explicit operator clear |

**Three of the four are normatively specified as settable, so this is not a design question — the
control plane is failing to implement RFC-0001.**

- *"per `estopPolicy.propagateToChildren` (**default true**)"* and *"Downward propagation stops
  robots: the triggered zone and, **when `propagateToChildren` is set**, every descendant"*
  (RFC-0001 §9.1.6). A default, described by its set/unset behaviour.
- *"`pauseOnError: false`, because `pauseOnError: true` claims every failure as `Paused` first"*
  (RFC-0001 #firmwarerollout) — the spec reasons explicitly about a value operators cannot currently write.
- *"`requireIdleState: true` for maximum safety"* (RFC-0001 #firmwarerollout-schema) — advice, which presumes a choice.

**The fourth is different, and RFC-0001 already says so:** *"the prohibition it names is nonetheless
unconditionally enforced, because no auto-clear path exists — **the field is inert, the rule is
not**"* (RFC-0001 #resource-model). It has zero read sites in the tree. It is documentation wearing the
costume of a setting.

None of the four is currently a safety hole: every one is stuck in its *conservative* position,
which is exactly why five instances accumulated without anyone noticing.

## Decision

**1. All four become `*bool` with an accessor that resolves nil to the documented default.** This is
`api-principles.md`, *Field conventions*: *"Optional scalars are pointers … Do not use a bare value
type for a field where zero is a real, different-from-absent value."* Each accessor is the single
place the nil semantics live, so no call site re-decides them.

**2. The three settable fields become genuinely settable.** `pauseOnError: false`,
`requireIdleState: false` and `propagateToChildren: false` now do what they say. Defaults are
unchanged — all three still default `true`.

**This enables three knobs that reduce a safety margin, and that is said here in as many words.** A
rollout can be told to continue past a failure; to update a robot that is not idle; an estop can be
scoped to one zone instead of cascading. Each is a documented, specified operator choice with a safe
default, and each is now an explicit, durable, auditable line in a manifest. The alternative is the
status quo: accepting the operator's instruction and silently doing the opposite. Between an
operator who knowingly narrows an estop and a control plane that quietly overrides them, the second
is worse — it means no operator can trust that any boolean in this API does what they wrote.

**3. `RequireExplicitClearAfterEstop` stays pinned true, and the API stops pretending otherwise.**
The accessor always returns true. Setting it to `false` produces an admission **warning** naming the
field as unconditionally enforced. The pointer is what makes that warning possible: without it,
"operator wrote false" and "unset" are the same value and the webhook has nothing to warn about.

Warning rather than rejection, following ADR-0051's precedent and for the same reason: rejecting
would be *tightening validation*, which `api-principles.md` classes as breaking, and would fail
manifests that apply cleanly today. A warning is additive, tells the operator the truth, and leaves
their manifest working.

**4. A mechanical gate prevents recurrence.** `TestNoDefaultTrueBoolWithOmitempty` parses `api/v1`'s
own source and fails on any `bool` field carrying `omitempty` and `+kubebuilder:default=true`. This
shape recurred five times in one API group; five one-off fixes without a gate is an invitation to a
sixth. It runs under `make test`, needs no new Makefile wiring, and lives beside the types it
governs.

## Options considered

**Drop `omitempty` instead of using a pointer.** One character per field and it makes `false`
serialize. Rejected: it leaves a value type where the principles require a pointer, still cannot
distinguish unset from false, and does nothing to stop the next default-true `bool`. It also changes
the serialized form of every object, where the pointer change does not.

**Reject `requireExplicitClearAfterEstop: false` with a CEL rule.** More forceful and arguably more
honest than a warning. Rejected as a breaking change: a manifest that applies today would start
failing, for a field whose runtime behaviour does not change either way. The warning conveys the
same fact at the same moment without breaking anyone.

**Remove `requireExplicitClearAfterEstop` entirely.** It is inert and read by nothing. Rejected:
removing a published field is breaking under *Versioning and compatibility*, and the field has
documentary value — RFC-0001 references it by name as the statement of a prohibition.

**Fix only the three settable fields and leave the fourth.** Rejected because it leaves the gate in
decision 4 with a permanent exception, and an exception is how the shape returns.

## Consequences

**Stored-object compatibility: no migration, no version bump.** `bool` → `*bool` generates an
identical OpenAPI schema — `type: boolean` with the same `default` — so the CRD structural schema is
unchanged for all four fields. An object that omits the key is defaulted exactly as before; an
object storing `true` is unaffected.

**One behaviour change per settable field, and it is the fix working.** An object that already
stores an explicit `false` — reachable only where no Go round-trip re-marshalled it — previously had
that `false` silently restored to `true`. It will now be honoured. Before rolling this out, audit
for `pauseOnError: false`, `requireIdleState: false` and `propagateToChildren: false` in existing
manifests and stored objects: those namespaces will begin behaving as their authors originally
wrote, which may not be what their operators have come to expect.

`requireExplicitClearAfterEstop` has no such consequence: its accessor is pinned true, so no stored
value changes any behaviour.

**The Go type change is a compile-time break for in-tree consumers only.** Six call sites, all
routed through the new accessors. It is not a Kubernetes API break.
