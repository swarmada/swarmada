# ADR-0056: A workload template's pod spec is bounded by Pod Security Standards Baseline, enforced across CEL and a webhook

- **Status:** accepted
- **Date:** 2026-09-06
- **Deciders:** Security Reviewer, Kubernetes Expert
- **Related:** RFC-0001 [ComputeWorkloadTemplate](../../rfcs/dist/RFC-0001-core-spec.md#computeworkloadtemplate)
  (containment rules, RBAC surface, the Baseline deny-list and the ordering requirement),
  ADR-0032 (conformance gating)

## Context

`provision: workload` has the controller create a `Job` or `Deployment` per compute run from an
administrator-authored `ComputeWorkloadTemplate`. A prior review required the narrowing to land
before any workload-create grant: a stamped ServiceAccount, namespace scoping, admin-created
templates referenced by name, and an Extended conformance profile. All four were built. A
re-derivation on 2026-09-06 found the first escalation vector closed and two open — `hostPath` and
privileged mode were neither refused nor stripped anywhere.

Two things the item did not settle had to be settled to close them.

**Why an ADMIN-authored object needs a deny-list at all.** The natural objection is that an
administrator who can write a template could write a `Deployment` instead, so nothing is being
prevented. That misreads who creates the object. When mode 3 is wired the workload is created by the
**manager's** ServiceAccount, not by the template's author — so holding `create` on
`computeworkloadtemplates` means having the manager create a pod on your behalf, under its
authority. A `swarmada.io` CRD silently becomes pod-create, and pod-create carries `hostPath` onto
the node filesystem, privileged mode, and the host network namespace. This is a confused deputy, and it is the
containment rule — script-executing compute is administrator-installed, never task-supplied —
reached one object further out: a principal handed "only a Swarmada CRD" is not bounded by that
framing if the CRD reaches node root.

Kubernetes does not close it. RBAC's escalation-prevention covers `escalate` and `bind` on
roles and says nothing about pod-create. Pod Security Admission does apply — but it evaluates
pod-controller objects in warn/audit mode only, enforces at `Pod` creation, and only in a namespace
an operator remembered to label. A cluster that never labelled the namespace has no gate at all.

**What CEL can carry, measured rather than assumed.** RFC-0001's chapter is explicit that this class
of invariant belongs at the API-server boundary: *"a privilege-escalation-relevant invariant that is
enforced only by a controller is an invariant that holds only while that controller is the sole
writer, whereas a CEL rule holds for every client of the API server."* The obvious implementation is
therefore all-CEL. It does not work. `ComputeWorkloadTemplate` embeds a vendored
`corev1.PodTemplateSpec` whose `containers`/`initContainers`/`ports` lists declare no `maxItems` and
cannot be given one without forking the type. A CEL rule walking them is refused outright:

    spec.validation.openAPIV3Schema.properties[spec].x-kubernetes-validations[5].rule: Forbidden:
    estimated rule cost exceeds budget by factor of more than 100x

and the CRD then **fails to install at all** — the failure is not a weaker gate, it is no CRD. This
was measured against a real API server, not predicted.

## Decision

**1. The bound on a template's pod spec is the Pod Security Standards *Baseline* profile's
host-escape and privilege-acquisition controls.** Baseline is borrowed rather than invented: it is
what Kubernetes itself defines as "prevents known privilege escalations", it is what a labelled
namespace's PSA would apply to the resulting pod anyway, and a reader can audit the list against a
published standard instead of trusting one somebody assembled. **Anything Baseline permits is
permitted here** — `resources`, `env`, `volumeMounts`, `affinity`, `nodeSelector`, `tolerations` and
the rest of an ordinary pod spec are untouched. One addition beyond Baseline: `gitRepo` volumes,
deprecated by Kubernetes and a known path-traversal surface.

**2. Enforcement is split, and the split is drawn by measurement.** The pod-level scalars that fit
inside the cost budget stay in CEL on `ComputeWorkloadTemplateSpec` — `serviceAccountName`, host
namespaces, `hostPath`/`gitRepo` volumes, `sysctls`, `ephemeralContainers`. The container-level half
that cannot — privileged mode, added capabilities, `procMount`, seccomp, host ports — is enforced by
a `failurePolicy: Fail` validating webhook. Both call one function (`compute.ValidatePodTemplate`),
which `BuildWorkload` calls again at construction, so a webhook that is down, misconfigured or
bypassed never leaves a template unchecked and the sites cannot drift apart.

**3. The narrowing's presence is asserted by a test conditioned on the grant, not on today's
state.** `TestManagerRBAC_WorkloadCreateAuthorityImpliesContainment` passes while no workload-create
authority exists and, the moment one appears, requires every containment artefact to be present
already.

## Consequences

- An administrator cannot express a privileged or node-mounting compute workload **through this
  object**. They retain every other route their own RBAC allows; what they lose is the ability to
  route one through the manager's ServiceAccount. That is the point.
- The policy is enforced in two places with different availability characteristics. The CEL half
  survives the manager being down; the webhook half does not, but fails closed.
- Baseline, not Restricted, is the bar. `runAsNonRoot`, `readOnlyRootFilesystem` and dropping all
  capabilities are **not** required — Restricted would break ordinary compute images for hardening
  that is the workload author's call, and this object's job is the privilege boundary.
- When this ADR was taken, the RFC-0001 chapter's *Normative Requirements* and *Failure Modes* did not
  carry these rules. That specification change was its own decision, reported rather than made here;
  the chapter's *Normative Requirements* and *Failure Modes* carry the Baseline rules.

## Alternatives considered

**Replace the embedded `corev1.PodTemplateSpec` with a narrowed Swarmada-owned struct** (an
allow-list type). Strongest containment — an escalating field could not be *expressed*, not only
refused — and it would shrink a 523 KB CRD that already exceeds the 256 KiB client-side-apply limit.
Rejected here as out of proportion to the finding: it is a breaking change to a `v1` CRD, it
contradicts the chapter's stated design (*"Standard Kubernetes PodTemplateSpec, reused rather than
reinvented — an administrator who can already write a Deployment can write this"*), and it needs an
RFC rather than an ADR. It remains the better long-term answer and should be reconsidered before
mode 3 ships.

**Rely on Pod Security Admission.** Rejected as a control, kept as defence in depth: it is opt-in
per namespace by label, and a gate that a missing label silently disables is not a boundary.

**Grant nothing and drop `provision: workload` entirely**, supporting only `provision: existing`.
Still available and still cheap — the builder has no callers. This ADR does not decide that mode 3 ships; it decides what must be true
first.
