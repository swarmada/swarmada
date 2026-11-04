# ADR-0051: A namespace is born with signature verification ON and incomplete, and incompleteness is a warning rather than a rejection

- **Status:** accepted; **decision 6 superseded 2026-09-06 by [ADR-0053](0053-an-absent-swarmadaconfig-enforces-signature-verification.md)**
- **Date:** 2026-09-06
- **Deciders:** API Engineer, Kubernetes Expert
- **Related:** RFC-0001 [#fleet-adapter-protocol-fleet-adapter-compliance-checklist](../../rfcs/dist/RFC-0001-core-spec.md#fleet-adapter-protocol-fleet-adapter-compliance-checklist) (artifact signature
  verification), RFC-0001 `SwarmadaConfig` precedence rules (namespace auto-creation),
  ADR-0020 (model artifact signing), ADR-0032 (conformance gating — the consumer that fails
  closed on an unsatisfiable signing policy), [api-principles.md](../api-principles.md)
  ("Optional scalars are pointers", "Defaults belong in the schema", "Versioning and
  compatibility")

## Context

Two coupled defects, found by the UC-1.7 verification run on 2026-09-06 and filed together.

**The auto-created config cannot pass its own webhook.** `SwarmadaConfigBootstrapReconciler`
creates an **empty-spec** `SwarmadaConfig` on the first `FleetZone` in a namespace, relying on
kubebuilder defaults to fill it. Defaulting produces `signing.requireSignatureVerification: true`
with `signing.trustRoots` empty, and `vswarmadaconfig.swarmada.io` rule 4 rejects exactly that
pair. With `ENABLE_WEBHOOKS=true` the reconciler hot-loops. Because the validating webhook is
`failurePolicy: fail`, a manager that is down blocks every `swarmada.io` write — including the
hand-written config that would break the loop. That state was reached on 2026-09-05 and cost a
cluster rebuild.

**The field cannot be set to `false`.** `RequireSignatureVerification` is a non-pointer `bool`
carrying `omitempty` and `+kubebuilder:default=true`. A Go round-trip drops `false` on the way out,
and structural defaulting restores `true` on the way in. Three shipped scripts set it —
`examples/demo-ab-live/run.sh`, `examples/warehouse-quickstart/run.sh`,
`examples/full-surface-demo/run.sh` — and on a webhook-enabled cluster all three settings are inert.
It appears to work only where the mutating webhook is absent, because structural defaulting alone
fills only fields the request omitted; an explicitly-sent `false` survives when nothing re-marshals
the object.

**The fact that reframed the decision.** The obvious repair for the first defect — do not create a
config the controller knows is invalid — is the *worst* available option, because an absent config
is not a neutral state:

```go
// internal/controller/fleetadapter_controller.go, namespaceSigning
if len(configs.Items) == 0 {
    return fleetv1.SwarmadaSigningConfig{}, nil   // zero value: enforcement OFF
}
```

With no `SwarmadaConfig`, `verifyConformance` reads a zero-valued signing policy and enforcement is
**off**. The auto-created object is therefore not bookkeeping; it is **the control that makes
default-on real**. Delaying it, or creating it with verification disabled, is a silent downgrade of
the namespace's security posture — the outcome this decision exists to avoid.

That inverts the problem. The invariant "enforcement ⇒ trust root" is, as written, *unsatisfiable by
the very object that establishes the secure default*, because the control plane cannot invent a
trust root: a trust root is a public key in an operator-held Secret.

## Decision

**1. A namespace is born with `requireSignatureVerification: true` and no trust roots, and that
state is legal.** The bootstrap object keeps the secure default. It is *incomplete*, not *invalid*.

**2. Rule 4 of `vswarmadaconfig.swarmada.io` becomes an admission WARNING, not a rejection.** An
operator applying a config with enforcement on and no trust root gets immediate feedback on
`kubectl apply` and the write succeeds.

**3. Every consumer continues to fail closed, and that is where the safety property lives.**
`loadTrustRoots` errors on an empty set; `verifyConformance` fails the adapter with "signature
verification required but the report ConfigMap has no signature"; `firmwarerollout_controller`
demands a signature ref. An incomplete namespace verifies nothing successfully — it *refuses* work
rather than *permitting* unverified work.

**4. `RequireSignatureVerification` becomes `*bool`.** `api-principles.md`, *Field conventions*:

> **Optional scalars are pointers.** An optional numeric or boolean field is a pointer (`*float64`,
> `*int32`, `*bool`) so that *unset* is distinguishable from a meaningful zero. […] Do not use a
> bare value type for a field where zero is a real, different-from-absent value.

For a signing gate, `false` is emphatically a real, different-from-absent value. The field also
gains the `+optional` marker it was missing, per *"`+optional` and `omitempty` travel together"*.

**5. `nil` means `true`, defined once.** A single accessor,
`SwarmadaSigningConfig.SignatureVerificationRequired()`, resolves nil to `true`. This is forced, not
chosen: the schema default is `true`, so a Go accessor that read nil as `false` would disagree with
the API server about the same object.

**6. The no-config fail-open is preserved, made explicit, and filed separately.**
*(SUPERSEDED 2026-09-06 by [ADR-0053](0053-an-absent-swarmadaconfig-enforces-signature-verification.md).
An absent `SwarmadaConfig` now resolves to verification ON, matching the rollout webhook and the
firmware rollout reconciler. `namespaceSigning` no longer returns an explicit `&false`; it returns
the zero value and lets `SignatureVerificationRequired()` resolve nil, as this decision's own
paragraph anticipated. The deferral below is discharged, not withdrawn: it was right to keep the
posture change out of a type change, and ADR-0053 is the work it was deferred to. The rest of
ADR-0051 stands.)* Resolving nil to
true would silently convert `namespaceSigning`'s zero-value return — the "no `SwarmadaConfig` in the
namespace" case — from enforcement *off* to enforcement *on*. That is a posture change on a path
this decision was not asked to touch, and it is a different defect from either of the two here. The
function now returns an explicit `&false` for that case, with a comment naming the inconsistency
(the rollout admission webhook answers the same question fail-*closed*), so the behaviour is
unchanged and no longer an accident of a zero value. It is tracked separately; closing it belongs
with that work, not as a side effect of a type change.

**This decision does not weaken the signing default, in as many words:** a fresh namespace enforces
signature verification, and an operator must now take an explicit, durable, auditable action —
writing `requireSignatureVerification: false` — to turn it off. Today that action is accepted and
silently ignored, which is worse than either honest answer.

## Options considered

**A — Bootstrap with a trust root.** Rejected as impossible rather than undesirable. A trust root is
a public key the operator holds; the control plane has nothing to put there. The reachable variant —
bootstrap a config that *references* a Secret that does not exist — produces a namespace that looks
configured and verifies nothing, which is the silent downgrade under a different name.

**B — Default `requireSignatureVerification` to false and let operators opt in.** Rejected on
security posture. It converts a fail-closed default into a fail-open one for every existing
namespace on upgrade, and it is precisely the trade this decision was warned against: swapping a
crash for a silent downgrade, where the downgrade is the worse of the two. It is also unnecessary —
the reconcile loop is caused by the *admission rule*, not by the default.

**C — Make the webhook recognise and exempt the object the controller creates.** Rejected as
brittle, and adopted in spirit. Recognising "the controller's object" means matching on name plus an
empty spec, which is guessable, spoofable by anyone who can create a `SwarmadaConfig`, and silently
stops applying the moment the bootstrap shape changes. Worse, it leaves the rule rejecting a state
that is *legal for the controller and illegal for an operator* — the same object, two verdicts,
depending on who wrote it. Decision 2 keeps C's insight (the incomplete config must be creatable)
without the special case: it is legal for everyone, and warned about for everyone.

## Consequences

**A validation is loosened, deliberately.** `api-principles.md` *Versioning and compatibility* makes
*tightening* validation breaking; loosening is additive and needs no version bump. The cost is real:
an operator typo (`trustRoots` misspelled, so it prunes away) is no longer caught at write time. It
is caught at first use instead, with a fail-closed error naming the namespace. Accepted, because the
alternative is an unrecoverable cluster.

**Namespaces with no `SwarmadaConfig` at all still do NOT enforce verification.** Decision 6.
*(No longer true as of 2026-09-06 — see [ADR-0053](0053-an-absent-swarmadaconfig-enforces-signature-verification.md).
They now do. The six tests named below were given an explicit verification-off config rather than
being left to lean on the absence.)* Six
controller tests exercise that path and were what surfaced it: they create no config, and under a
naive nil-means-true they began failing conformance. Preserving the behaviour keeps this change
reviewable as what it claims to be, and the argument for changing it is tracked separately.

**Stored-object compatibility — no migration, one behaviour change.** The OpenAPI schema for the
field is unchanged: `type: boolean` with `default: true` before and after, because `*bool` and `bool`
generate the same structural schema. Therefore:

- A stored object that **omits** the key is defaulted to `true` on read, exactly as before. No
  migration, no version bump, no conversion webhook.
- A stored object carrying `true` reads as `&true`. Unchanged.
- A stored object carrying **`false`** — reachable today only on a webhooks-off cluster, since a
  mutating round-trip erased it — previously had its `false` restored to `true` behind the
  operator's back. It will now be **honoured**. A namespace whose operator wrote `false` and
  believed it was ignored will, after this change, genuinely stop verifying. This is the fix working
  as designed, and it is the one upgrade note that matters: audit for
  `requireSignatureVerification: false` before rolling out.

The Go type change is a compile-time break for in-tree consumers only; there are four, all updated
through the accessor. It is not a Kubernetes API break.

**What this does not do.** It does not move any maturity grade, does not disable the validating
webhook, and does not change the default. The four other fields in `api/v1` carrying the same
`bool` + `omitempty` + `default=true` shape are filed separately; this ADR fixes only the signing
field and its bootstrap interaction.
