# ADR-0053: An absent SwarmadaConfig enforces signature verification

- **Status:** accepted
- **Date:** 2026-09-06
- **Deciders:** API Engineer, Controller Engineer
- **Supersedes:** [ADR-0051](0051-a-namespace-is-born-with-signing-on-and-incomplete.md) **decision 6**
  (the no-config fail-open, preserved there on purpose and explicitly deferred to this work)
- **Related:** RFC-0001 [#swarmadaconfig-auto-creation-on-namespace-initialization](../../rfcs/dist/RFC-0001-core-spec.md#swarmadaconfig-auto-creation-on-namespace-initialization)
  (`SwarmadaConfig` auto-creation on namespace initialization), RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist (artifact
  signature verification), ADR-0020 (model artifact signing), ADR-0032 (conformance gating),
  ADR-0052 (the pointer that made *unset* representable)

## Context

Two consumers resolved the same question — *what is this namespace's signing policy?* — and
disagreed about what an **absent** `SwarmadaConfig` meant.

`namespaceSigning` (`internal/controller/fleetadapter_controller.go`) returned an explicit
`&false`: **fail open**. `verifyConformance` then accepted an unsigned conformance report and the
adapter was qualified `Passed`. `signatureRefRequired` (`internal/webhook/rollout_webhook.go`)
returned a denial for the same input: **fail closed**, so a `FirmwareRollout` could not be created
in that namespace at all.

A re-derivation for this work found a **third** consumer, and it agrees with the rollout webhook:
`FirmwareRolloutReconciler.namespaceSigningConfig` returns *"no SwarmadaConfig in namespace %q;
cannot determine signing policy (fail closed)"*. The adapter path was the outlier, one of three.

The disagreement is the defect, independently of which answer is right: two components enforcing
different policies from the same input is a fault an auditor can only find by reading both. And the
one that failed open sat on the path deciding whether unverified software may drive robots.

**Why it was preserved until now.** ADR-0051 decision 6 kept the fail-open deliberately. Making
`RequireSignatureVerification` a `*bool` with a nil-means-true accessor would have flipped this
behaviour as a side effect of a type change, on a path that ADR was not asked to touch, and it would
have taken six controller tests with it. Preserving it kept that change reviewable as what it
claimed to be. ADR-0051 said closing it "belongs with that work, not as a side effect of a type
change." This is that work.

**The zero value is what hid it.** Before the pointer, "no config" and "config with verification
off" produced the identical Go value. Nothing in the code could distinguish a policy that said *off*
from a policy that did not exist. ADR-0052's pointer made the distinction representable, which is
how this surfaced at all.

## Decision

**1. An absent `SwarmadaConfig` resolves to signature verification ON.** `namespaceSigning` returns
the zero-valued policy for the no-config case. Its `RequireSignatureVerification` is nil, and nil
resolves to `true` through `SignatureVerificationRequired()`. All three consumers now give the same
answer for the same input, and it is the answer the schema already gives: the field carries
`+kubebuilder:default=true`, so an object the API server materializes says `true`. A namespace with
no object cannot mean *less* enforcement than a namespace with a defaulted one.

**2. `SignatureVerificationRequired()` stays the only place nil is resolved.** ADR-0051 decision 5
made that accessor the single home of nil semantics. The fix here is therefore a **deletion** — the
explicit `&false` is removed and nothing replaces it. A second site that re-decided nil, even to the
correct value, would be the original defect returning in a new spot.

**3. The refusal names the missing config, not the missing signature.** `namespaceSigning` also
reports whether a config was found. That flag carries no policy; it exists so the operator-facing
message can say *"no SwarmadaConfig in namespace X; signature verification is required by default
(fail closed) — create swarmada-config with a trustRoot, or set
spec.signing.requireSignatureVerification: false to turn verification off deliberately"* rather than
*"the report ConfigMap has no signature"*. Both are true; only one names the fix. An operator who
adds a signature to a report in a config-less namespace has done work that changes nothing.

**4. The bootstrap trigger widens to `FleetAdapter`.** `SwarmadaConfigBootstrapReconciler` created
the config on the first `FleetZone`. That does not deliver the guarantee RFC-0001 #swarmadaconfig-auto-creation-on-namespace-initialization states
in the same paragraph that defines the trigger — *"This ensures every operational namespace always
has a SwarmadaConfig, eliminating the need for controllers to implement fallback logic for the
missing-config case."*

A `FleetAdapter` can exist in a namespace that has no `FleetZone`. `FleetAdapterSpec`
(`api/v1/fleetadapter_types.go`) has no zone reference; no admission webhook is registered for
`fleetadapters` in `config/webhook/manifests.yaml`; the `FleetAdapter` CRD carries no
`x-kubernetes-validations`; and `FleetAdapterReconciler` never reads a `FleetZone`. So the window
was not only transitional — it was **unbounded** for an adapter-only namespace, which is exactly
where the fail-open lived. Watching `FleetAdapter` as well as `FleetZone` is additive to the RFC's
rule and is what makes the RFC's own claim true.

## Options considered

**Fail open in `signatureRefRequired` instead.** Cheaper, and it would also make the paths agree.
Rejected: it removes a fail-closed guard rather than adding one, and it makes *deleting* a
`SwarmadaConfig` an unauditable way to turn signing off for a namespace. Between two components that
disagree, the resolution is not automatically the cheaper one.

**Return an error from both when no config exists, and let each caller decide.** The most honest
about a missing input. Rejected as a larger change that relocates the decision to two call sites
that would each have to make it, without improving the answer either would reach.

**Widen the bootstrap trigger and stop there.** Rejected: a create and the reconcile that follows it
are not one transaction, so the window narrows but never closes. Widening the trigger is not an
alternative to picking the safe answer inside the window — it is what keeps the safe answer from
being reached often.

**Trigger on `Namespace` creation.** Would close the window more completely. Rejected: it needs a
cluster-wide namespace watch and would write a `SwarmadaConfig` into every namespace on the cluster,
including ones that will never hold a robot.

## Consequences

**What an operator must do.** A namespace that holds Swarmada objects but no `SwarmadaConfig` no
longer qualifies adapters: conformance goes to `Failed` with a message naming the missing config, and
because the assignment gate (ADR-0032) requires a passing adapter, no robot in that namespace is
dispatchable until it is fixed. The fix is one object:

```bash
kubectl apply -f - <<'EOF'
apiVersion: swarmada.io/v1
kind: SwarmadaConfig
metadata:
  name: swarmada-config      # the singleton name; the CEL rule requires it
  namespace: <namespace>
spec:
  signing:
    requireSignatureVerification: false   # or leave it out and add a trustRoot
EOF
```

Applying it is enough; the adapter re-qualifies on the next reconcile. Namespaces created from here
on get the object automatically from the widened bootstrap trigger, so this applies to existing
namespaces that predate it — audit with `kubectl get swarmadaconfig -A` and compare against the
namespaces that hold `FleetAdapter` objects.

**This will look like a regression, and the appearance is the point.** Such a namespace was
previously qualifying adapters against unsigned reports because it had stated no policy at all. It
was not verifying anything, and it was not saying so. The new behaviour makes the absence
visible at the moment it matters instead of resolving it silently in the permissive direction.

**The remaining window, and it is reachable in practice.** A `FleetAdapter` create and the bootstrap
reconcile that follows it are separate events, so an adapter reconcile can observe its namespace
before the config lands. On a kind cluster, applying a `FleetAdapter` into an empty namespace lands
in that window on the first try: the config appears, and the adapter's status reads `Failed` with
*"no SwarmadaConfig in namespace ..."*.

The window can only ever produce a **refusal**, never an acceptance, so nothing is admitted inside it
that would be refused outside it. But the refusal does **not** always clear on its own.
`FleetAdapterReconciler` self-requeues only while the adapter's phase is `Connected` or `Degraded`
(`ctrl.Result{RequeueAfter: interval}`); it does not watch `SwarmadaConfig`, so a config appearing is
not an event it sees. An adapter that has not yet registered has no phase, does not requeue, and
keeps the window's message until its next event — which in practice is its own registration, since
becoming `Connected` reconciles it and re-evaluates conformance against the config that now exists.

So: for an adapter that ever connects, the window's message is transient and clears when the adapter
becomes usable. For one that never connects, it persists — and that adapter was not qualifying
anyway. Neither case admits unverified software. The message names the missing config in both, which
is the point: the operator is told what is actually wrong.

**A fresh adapter-only namespace does not start passing, and should not.** The widened trigger gives
the namespace a config; that config is the ADR-0051 bootstrap object, which enforces verification and
carries no trust roots. An unsigned conformance report therefore still fails there, now reporting
*"signature verification required but the report ConfigMap has no signature"*. The change is not that
such a namespace starts qualifying adapters — it is that the namespace now **states** a policy an
operator can see and change, instead of resolving its own silence in the permissive direction.

**Six tests changed, and none of them by weakening an assertion.** Six controller tests created no
`SwarmadaConfig` and asserted `Passed` against an unsigned report. Each now seeds a config that
exists and says verification is not required — the policy they were always assuming, borrowed from a
gap rather than stated. Every assertion is unchanged.

**No API change and no migration.** No type, schema, CRD or stored object is touched. This is a
change to how the *absence* of an object is interpreted by one controller.

**ADR-0051 is otherwise intact.** Decisions 1–5 stand. A namespace is still born with signing on and
incomplete; rule 4 is still a warning; every consumer still fails closed on an empty trust-root set;
the field is still a `*bool` with nil resolved in exactly one accessor. Only decision 6 is superseded.
