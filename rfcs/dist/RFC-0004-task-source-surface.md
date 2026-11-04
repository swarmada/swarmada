---
title: "RFC-0004: The Task-Source Surface"
author: "Swarmada maintainers <maintainers@swarmada.io>"
status: Exploratory
created: 2026-07-08
updated: 2026-10-15
swarmada-version: v0.4
published: true
---

# RFC-0004: The Task-Source Surface

<a id="abstract"></a>
## 1. Abstract

> **Status: Exploratory draft, published for review. Not part of RFC-0001 v0.4, and not to be implemented
> from this text.** This document is published beside RFC-0001 v0.4 for review. Nothing in RFC-0001 depends
> on it. Every resource, field, enum value, annotation, RBAC verb, lifecycle transition and
> `fleet_adapter.v1` message this document adds would need a contract change (RFC-0001, Resource Model at a
> Glance, *The contract-change process*).

RFC-0001 gives the *south* surface — the Fleet Adapter protocol — first-class treatment: a documented, versioned, conformance-checked contract. It does not give the *north* surface — how a task source (a WMS, a scheduler, an operator tool) submits and observes work — the same treatment, leaving idempotency, backpressure, and the outcome read surface for each integrator to rediscover. ADR-0006 settles the transport question: the north surface **is** the Kubernetes API, and Swarmada defines no bespoke north-side wire protocol. This RFC defines the net-new normative *contract* that ADR-0006 defers to an RFC — a documented **profile over the Kubernetes API** with four parts: idempotent submission by deterministic name of the submitted unit of work — an atomic `FleetAction` or a composite `FleetTask` (both RFC-0001), both accepted through the same create-by-name path — an outcome read surface whose guarantees are stated exactly (coarse, eventually consistent, terminal-authoritative, RA-1), enforced backpressure in upstream-native terms (API Priority & Fairness, `ResourceQuota`, and an admission-enforced in-flight ceiling derived from `SwarmadaConfig`), and a **north boundary invariant**, with the mechanisms that bear on it. It proposes a reference task source and a north conformance checklist as the analogues of `example-noop` and the adapter conformance suite. The load-bearing claim is that the north side is task-granular *because its read surface is coarse and its write surface can be bounded at admission*. The first half holds by RA-1. The second holds only where an operator configures it: the in-flight ceiling is off by default and fails open ([§6.5](#design-details-the-north-boundary-invariant)).

> **Maturity (v0.1):** stage: alpha · control-plane: partial · end-to-end: not exercised.
>
> **What exists.** The Kubernetes API surface the profile is written over, and one of its three backpressure
> layers: the per-zone in-flight ceiling in the `FleetAction` admission webhook. It refuses a `create` with
> `403 Forbidden` whose message contains `PendingActionLimitExceeded:`, after the prefixes
> `admission webhook "vfleetaction.swarmada.io" denied the request:` and
> `fleetactions.swarmada.io "<name>" is forbidden:`; a source matches it as a substring. It is off until an
> operator sets `SwarmadaConfig.spec.scheduling.maxPendingActionsPerZone` (`0` means unbounded), it fails
> open when it cannot read the `SwarmadaConfig` or list the zone's actions, and it exempts a `FleetAction`
> with no zone. No end-to-end run exercises the refusal. API Priority & Fairness and
> `ResourceQuota` are upstream mechanisms an operator configures; the tree ships no manifest for them and no
> task-source `Role`.
>
> **What does not exist.** No reference task source, no Task-Source SDK (the Python SDK holds only a safety
> module for Fleet Adapters), and no checklist file. No check from N1.1 to N6.2 has been run against any
> implementation, no harness implements one, and no conformance registry lists their identifiers.
>
> **Relation to RFC-0001 conformance.** Passing this checklist is not conformance to RFC-0001, and failing
> it changes no RFC-0001 conformance profile or tier. A deployment conformant to RFC-0001 need not run it.
> Where this document and RFC-0001 differ, RFC-0001 governs.
>
> **One route for work from outside the cluster ships:** the external event bridge example.
> Its `Role` grants `create` on `semanticevents` and nothing else. RFC-0003's `TaskTrigger` controller turns
> each accepted event into a `FleetTask` held as a draft, and where the admission webhooks are served the
> `FleetTask` webhook refuses activation to every ServiceAccount.
>
> A reader MUST treat this document as a specification target and MUST NOT read any part of it as a shipped
> capability beyond what *What exists* above names.

This document uses the requirement keywords MUST, MUST NOT, SHOULD, and MAY, and the other key words of BCP 14, as described in BCP 14 (RFC 2119 and RFC 8174) when, and only when, they appear in all capitals.
<a id="motivation"></a>
## 2. Motivation

> **Scope.** Extends RFC-0001 (`FleetAction` and its reserved task-source naming and label conventions; `FleetTask`; the `SwarmadaConfig.spec.scheduling.maxPendingActionsPerZone` backpressure ceiling). Records the *north* integration contract that ADR-0006 leaves to an RFC.

RFC-0001 defines the Fleet Adapter protocol as a first-class, documented, conformance-checked *south* surface, and splits the unit of work into the atomic `FleetAction` — one dispatch, to one robot or, for an `External` action, to a `ComputeEndpoint` — and the composite `FleetTask` — a graph of actions a work order maps to (both RFC-0001). It does not give the *north* surface — how task sources submit and observe work — the same first-class treatment. It states that a WMS or operator tooling creates these resources (RFC-0001 Resource Model at a Glance, reading guidance: a work order maps to a composite `FleetTask`; simple one-step work can be a single `FleetAction`) and defers how work is authored upstream (non-goal #10), but it does not document the obligations, guarantees, and limits of the submission surface itself.

The gap has concrete costs. Without a documented contract: idempotency of submission is left to each integrator to rediscover; there is no stated backpressure behavior when a source outruns the fleet, so an overrunning source can grow the `Pending` set without bound (unlike telemetry, `FleetAction`s and `FleetTask`s are desired state and live in etcd); the outcome read surface and its guarantees are unstated, inviting integrators to build feedback loops the control plane is not designed to serve; and there is no reference artifact proving the surface, as `example-noop` proves the south side. This RFC closes the gap **without** introducing a bespoke north protocol — the transport is the Kubernetes API (ADR-0006). It documents a *profile* over that API and the enforcement needed to keep the profile's invariants true.

RFC-0001 already **reserves** the load-bearing conventions this RFC formalizes: the deterministic `FleetAction` naming pattern (`fa-<stable-hash(work-item-id)>`) and the `swarmada.io/source-id` / `swarmada.io/source-order` labels (RFC-0001 FleetAction), and the admission-time in-flight ceiling `SwarmadaConfig.spec.scheduling.maxPendingActionsPerZone` (rejecting `create` with `PendingActionLimitExceeded`). This RFC therefore documents, hardens, and conformance-checks an already-reserved surface rather than introducing new core fields. Any change to RFC-0001 it implies needs a future contract version.
<a id="goals"></a>
## 3. Goals

1. Document the task-source contract: how a source submits, identifies, and observes work — an atomic `FleetAction` or a composite `FleetTask` — over the Kubernetes API.
2. Specify **idempotent submission** via a deterministic naming convention for both the atomic `FleetAction` and the composite `FleetTask` (and server-side apply semantics).
3. Specify the **outcome read surface** and its guarantees (watch on `FleetAction.status` / `FleetTask.status`; coarse, eventually-consistent, terminal-authoritative), consistent with RA-1.
4. Specify **backpressure** in CRD-native and upstream-native terms: API Priority & Fairness, `ResourceQuota`, and an admission-enforced per-zone in-flight ceiling, set once per namespace, derived from `SwarmadaConfig`.
5. State the **north boundary invariant** and the mechanisms that bear on it, and say which part of it each enforces and which part rests on the source's own conformance.
6. Define a **reference task source** and a north **conformance checklist**, analogous to `example-noop` and the adapter conformance suite.
7. Specify a **Task-Source SDK** surface (ergonomics: typed builders, deterministic naming, watch/outcome helper, backoff) as the north analog of generated stubs.
<a id="non-goals"></a>
## 4. Non-Goals

1. A bespoke north-side wire protocol (settled by ADR-0006 — the surface is the Kubernetes API).
2. A mandatory in-core ingestion gateway translating a WMS message bus into `FleetTask`s; such gateways are out-of-core modules (ADR-0005 precedent).
3. Task authoring, decomposition, or planning logic above the submitted unit — how a work order becomes a composite `FleetTask` or a single `FleetAction` stays with the source (RFC-0001 non-goal #10). The composite `FleetTask` schema (its dependency graph, completion/failure policies) is defined by RFC-0001's FleetTask chapter; this RFC governs only how a source *submits and observes* one, not how it composes one.
4. Cross-namespace or cross-cluster submission (RFC-0001 non-goals #8, #9).
5. Any change to `FleetAction` or `FleetTask` status semantics or the RA-1 status-write discipline.
6. Natural-language / LLM task generation (RFC-0001 non-goal #10).
<a id="proposal"></a>
## 5. Proposal

The task-source surface is a documented **profile over the Kubernetes API** with four parts. The unit of work a source submits is either an atomic `FleetAction` (one dispatch: to one robot, or, for an `External` action, to a `ComputeEndpoint`) or a composite `FleetTask` (a graph of actions a work order maps to) — both RFC-0001; both are accepted through the same create-by-name profile below.

- **Submission.** A source `create`s a `FleetAction` or a `FleetTask` under an authenticated identity, using a deterministic name derived from its upstream work item, making submission idempotent. Server-side apply with a stable field manager is the supported upsert path.
- **Observation.** A source `watch`es its own resources' status (selected by a `swarmada.io/source-id` label) and consumes Kubernetes Events. A submitted `FleetAction` exposes `status.phase`; a submitted `FleetTask` exposes an aggregate `status.phase` plus `status.actionSummary` over its fanned-out child actions. The telemetry plane is not part of the control read surface.
- **Backpressure.** Three layers, cheapest first: API Priority & Fairness (per-identity `FlowSchema`), `ResourceQuota` on `count/fleetactions.swarmada.io` and `count/fleettasks.swarmada.io`, and an admission-enforced in-flight ceiling on pending `FleetAction`s (a `FleetTask` contributes its fanned-out actions to that ceiling), which is off unless an operator sets it. API Priority & Fairness throttling surfaces as `429`, and an in-flight-ceiling refusal as `403 Forbidden`; sources back off on both.
- **The boundary invariant.** The schema has no typed motion field, though the opaque payload can carry a motion plan; the read surface is coarse by RA-1; admission can bound submission where an operator configures it. [§6.5](#design-details-the-north-boundary-invariant) states what each mechanism does by default.

Artifacts this RFC proposes, none of which exists in the tree: a **reference task source** in the core tree (template plus conformance proof), a **Task-Source SDK** under `sdk/python`, and a **north conformance checklist** in a `docs/`- or `adapters/`-parallel location. See the maturity note in [§1](#abstract) for what the tree holds.
<a id="design-details"></a>
## 6. Design Details

The north side is not symmetric with the south side. The Fleet Adapter protocol is bespoke because four forces demand it — adapters sit behind NAT and must dial in, the control plane must push commands including estop, telemetry is high-cadence, and vendor hardware is proprietary. None of those forces act on a task source, which is software that reaches the API server over HTTPS and submits declarative desired state (ADR-0006). The correct north analogue of the *adapter contract* is therefore a documented profile over the Kubernetes API — not a north analogue of the adapter *protocol*. Each subsection below states one obligation of that profile and, where the obligation is an invariant, the mechanism meant to enforce it and the test that would prove it.

<a id="design-details-identity-and-authorization"></a>
### 6.1 Identity and Authorization

A task source's identity is its authenticated Kubernetes principal, and that principal MUST be a ServiceAccount, authenticated with a short-lived token from `TokenRequest`. A task source MUST NOT authenticate as a user, by an mTLS client certificate or an OIDC identity, and MUST NOT use impersonation. RFC-0001's admission refusals of `activate` and of a requeue of held work recognise a ServiceAccount by its username and group only, and admit automation that authenticates as a user, or impersonates one, like that user. No new identity concept is introduced. The namespace is the tenant boundary; cross-namespace submission is out of scope (RFC-0001 non-goal #9).

Authorization is ordinary RBAC. Because the surface accepts both the atomic `FleetAction` and the composite `FleetTask` through one profile, a task source MUST be granted `create`, `get`, `list`, `watch`, and `patch` on `fleetactions` and `fleettasks` — `patch` because server-side apply, the upsert path specified below, is a `PATCH` with `application/apply-patch+yaml` and is unreachable without it (whichever kinds it submits), and MUST NOT be granted any verb on `fleetactions/status` or `fleettasks/status`: status is controller-authored, and the RA-1 discipline is enforced here, at the RBAC layer, not by convention. That `patch` lets a task source set `swarmada.io/requeue-requested` on a `FleetAction` that a stop paused. On an action whose robot a stop holds (RFC-0001 Safety Architecture, *Robot Stop Table*), RFC-0001 refuses a requeue from a ServiceAccount and holds one from a person without `estop-clear` on that stop (*Action Table*, row A2 and *Rules for every A row*), and a task source does not hold that verb. This specification asks more of a task source: it MUST NOT set `swarmada.io/requeue-requested` on any `FleetAction`: the Role grants no read on `robots`, so a source cannot tell whether a stop holds an action's robot (RFC-0001 Safety Architecture chapter). It is an obligation on the task source and does not change RFC-0001's handling of the annotation. The v0.4 reference does not refuse it everywhere RFC-0001 requires: where the robot's `estopState` alone holds it, `Resuming` included, it admits a requeue from any identity that may patch the action (RFC-0001, *Not met at v0.4 (A2)*). On those paths the obligation above is the only barrier. The child `FleetAction`s a composite fans out are owned and status-authored by the control plane. RBAC cannot select objects by owner or label, so the `patch` and `create` this Role grants on `fleetactions` reach every `FleetAction` in the namespace, children and other sources' work included. A source MUST NOT write any `FleetAction` or `FleetTask` it did not create. This Role grants no `cancel`: work a source submitted and its upstream later withdrew is cancelled by a person. The `estop-clear` verb is separate and MUST NOT be granted to an automation identity (RFC-0001 Security Model and Safety Architecture chapters). RBAC alone cannot hold a task source to drafts. The `create` this Role grants on `fleettasks` admits a `FleetTask` with `spec.draft: false`, which runs with no person's approval, and its `create` on `fleetactions` dispatches at once, because a `FleetAction` has no draft state. Its `patch` cannot activate a draft: clearing `spec.draft` needs the RFC-0001 `activate` verb, and where the admission webhooks are served the `FleetTask` webhook refuses activation to every ServiceAccount, whatever its roles grant. A source meant to submit only drafts is therefore bound not to this Role but to the drafts-only `Role` in RFC-0001's security model, which grants no write on `fleetactions`, together with the admission policy set out there ([Authorization — Swarmada RBAC Roles](../dist/RFC-0001-core-spec.md#security-authorization-swarmada-rbac-roles)).

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: warehouse-a
  name: swarmada:task-source
rules:
  - apiGroups: ["swarmada.io"]
    resources: ["fleetactions", "fleettasks"]
    verbs: ["create", "get", "list", "watch", "patch"]
  # patch is required by the server-side apply upsert path (N2.4); without it a source
  # that follows this Role receives 403 on the path this chapter prescribes.
  # No rule grants fleetactions/status or fleettasks/status (RA-1) and none grants estop-clear.
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: warehouse-a
  name: wms-east-to-task-source
subjects:
  - kind: ServiceAccount
    name: wms-east
    namespace: warehouse-a
roleRef:
  kind: Role
  name: swarmada:task-source
  apiGroup: rbac.authorization.k8s.io
```

<a id="design-details-idempotent-submission"></a>
### 6.2 Idempotent Submission

Submission is idempotent by construction through **create-by-name**, and the same mechanism serves both kinds of submitted unit. A source MUST derive a deterministic name from the identifier of its upstream work item, where the hash is a stable, collision-resistant digest (for example the lower-case hex of a truncated SHA-256) computed only from durable upstream identifiers, never from a random nonce and never from the wall-clock time at which the submission happens. A deterministic *occurrence* instant defined by the upstream work item itself — the scheduled fire time of a recurring item, which is identical on every retry of the same occurrence — is a durable identifier and is permitted; the prohibition targets observation time, not scheduled time:

- For an atomic `FleetAction`, the reserved convention is `fa-<stable-hash(work-item-id)>`. The governing rule for any such prefix is that **a name prefix must not be any kind's `shortName`** — a `<shortName>-`-named object reads as a bare kind abbreviation, and does so whether or not the abbreviated kind is the object's own. `fa` is no kind's `shortName`: `FleetAction`'s is `fact` and `FleetAdapter`'s is `fadapt`, so the reserved prefix satisfies the rule.
- For a composite `FleetTask`, use an equivalently deterministic name that **avoids** the `ft-` prefix — `ft` is the composite `FleetTask`'s `shortName`, so an `ft-`-named object breaks the rule above and reads as a bare kind abbreviation — and that also avoids the `fa-` prefix, which is reserved above for atomic actions and whose reuse would collide with the atomic action-child naming space. A `<work-item-id>` or a distinct source-chosen prefix is appropriate; the controller in turn names each fanned-out child `FleetAction` deterministically as `<task>-<action>`, so a source never picks child names itself.

A duplicate `create` then returns `AlreadyExists`, which a source MUST treat as success (the intended unit already exists), making a retry storm after a crash or timeout safe. That holds only while the object exists: once a settled unit is deleted, for example to stay under the `ResourceQuota` in [§6.4](#design-details-backpressure), a late retry creates it again and the work runs a second time. A source therefore MUST record the terminal outcome of each work item before the unit can be deleted, and MUST NOT create a unit again for a work item whose outcome it has recorded. For a composite, a source MUST record the outcome only once every child `FleetAction` is terminal by the atomic rule in [§6.3](#design-details-outcome-read-surface-and-guarantees); a child that disappears before the source saw it terminal has not ended, and its work item goes to a person. An operator deletes settled units only after their source has recorded them. This is stronger and simpler than the south side's fencing tokens: a name plus `resourceVersion` gives optimistic concurrency natively, and the source holds no live authority to fence.

Server-side apply (SSA) with a stable, source-specific field manager is the supported upsert path: a source that must adjust an unassigned unit re-applies the same object under the same field manager, and the API server merges by field ownership. A source MUST NOT use SSA to contend for fields it does not own (it owns `spec`, never `status`). For a composite `FleetTask`, note that `spec.actions` growth is append-only and gated by a scoped admission verb (RFC-0001 FleetTask); a source SSA-adjusting a `FleetTask` owns its `spec` but cannot rewrite already-submitted actions, and cannot change its `spec.inputs` or `spec.flow`, which are immutable after creation.

Every submitted task MUST carry a `swarmada.io/source-id` label identifying the submitting source, and SHOULD carry a `swarmada.io/source-order` label back-referencing the upstream work-item ordering where one exists. These labels are the selector a source uses to re-list its own tasks ([§6.6](#design-details-disconnect-restart)). A composite's child `FleetAction`s are created by the controller and do not carry them; a source finds a composite's children by their controller `ownerReference` to it.

```bash
# Atomic FleetAction: deterministic name + source label; a repeat of this exact call is a safe no-op.
kubectl create -f - <<'EOF'
apiVersion: swarmada.io/v1
kind: FleetAction
metadata:
  name: fa-9f2a1c7b            # fa-<stable-hash(order-line-88213)>
  namespace: warehouse-a
  labels:
    swarmada.io/source-id: wms-east
    swarmada.io/source-order: "88213"
spec:                          # FleetActionSpec — one dispatch, here to one robot
  onFailure: Alert                # a retry re-performs work that may already be done (RFC-0001 FleetAction)
  type: PickUp
  zone: zone-bay-3               # a FleetZone NAME in this namespace, not namespace/name
  requiredCapabilities: ["transport.payload"]
  payload:                       # RFC-0001: the payload object itself, or, as here, one byte field `raw`
    # Decoded: {"orderLineId":"88213","skuId":"A-1042","quantity":2}
    raw: eyJvcmRlckxpbmVJZCI6Ijg4MjEzIiwic2t1SWQiOiJBLTEwNDIiLCJxdWFudGl0eSI6Mn0=
EOF
# Re-running returns: Error ... "fa-9f2a1c7b" already exists  → treat as success.
```

A multi-step work order is submitted the same way as a composite `FleetTask`, whose per-action entries each embed a `FleetActionSpec` (RFC-0001 FleetTask); the same idempotent create-by-name and `swarmada.io/source-id` label discipline apply, only the deterministic name avoids the `ft-` prefix as noted above.

<a id="design-details-outcome-read-surface-and-guarantees"></a>
### 6.3 Outcome Read Surface and Guarantees

The entire control-relevant north read surface is the submitted unit's `status` — `phase`, terminal outcome, `message`, `conditions`, and a `FleetTask`'s `observedGeneration` — and, for a composite, the `status` of the child `FleetAction`s it owns, each observed via a **watch/informer**, plus Kubernetes Events. The shape of that status depends on which kind was submitted:

- A submitted **`FleetAction`** exposes `status.phase`, whose terminal values are `Succeeded`, `Failed`, or `Cancelled`. That phase is the disposition. Whether the work was done is a separate field, `status.outcomeCertainty` (below).
- A submitted **`FleetTask`** fans out to child `FleetActions` and exposes an **aggregate** `status.phase` (a pure function of the child phases and the task's policies, monotonic toward terminal — RFC-0001 FleetTask) plus `status.actions[]`, one entry per member, and `status.actionSummary` (for example `"3/5 Succeeded"`), a per-composite roll-up a source reads without fanning out to the children itself. A `FleetTask` with `spec.flow` carries no `actionSummary` at v0.4: the flow path does not write it (RFC-0001 FleetTask). A source watches the composite for the aggregate outcome, and MAY watch the owned child `FleetAction`s for per-action detail. A composite's terminal phase states the outcome of its policy, not that its members have stopped: this profile does not rely on a composite's phase waiting for its members' cancels to be confirmed, nor, under `completionPolicy: Any` or `Quorum`, for its other members to end. Before a source submits work that repeats any member of a terminal composite, it MUST observe every child `FleetAction` of that composite in a phase that is terminal by the rule for an atomic `FleetAction` below.

Before a terminal phase, a unit can wait in a phase this list does not name. A `FleetAction` can be `Paused` (by an emergency stop or by `spec.desiredState`, and waiting for an operator's decision), `Revoking` (its robot lost connectivity) or `Preempted` (displaced by a higher-priority action). A `FleetTask` created as a draft stays in `Draft` until a person activates it.

Two of the read surface's guarantees hold identically for both kinds; the third holds for a `FleetTask` only:

- **Terminal phases are durable and monotonic.** Once a submitted unit reaches a terminal outcome — `Succeeded`, `Failed`, or `Cancelled`, or, for a composite `FleetTask` settling under `failurePolicy: Compensate`, `Compensated` — that outcome does not change and does not regress. An atomic `FleetAction`'s `Failed` is terminal only where the RFC-0001 *Action Table* makes it so: not under row A23's retry, and not while it is `Failed` (`StopUnconfirmed`) before row A3 releases its robot, whether or not row A6 holds it. Under `onFailure: Requeue` with retries left the action returns to `Pending`, and the source keeps watching it. A source that recognises only the first three blocks indefinitely on a task that compensates.
- **`Failed` does not say the work was not done.** A `FleetAction`'s `status.outcomeCertainty` is `Established` only where the robot or executor reported what it achieved. `Unestablished` and an empty value both leave open whether the work was done, and RFC-0001 requires a person to resolve such an outcome (RFC-0001 FleetAction, *Unestablished outcomes*). A source MUST NOT submit work that repeats a `Failed` action, or a member of a `Failed` composite, whose `status.outcomeCertainty` is not `Established`. It hands that work item to a person. The reference control plane writes no `status.outcomeCertainty`, so at v0.4 this applies to every `Failed` action.
- **Delivery is at-least-once with resume.** A watcher that disconnects resumes from its last observed `resourceVersion` without missing a terminal transition; duplicate deliveries are possible and MUST be tolerated. If the server answers `410 Gone`, the watcher re-lists by label and resumes from the list's `resourceVersion`.
- **Staleness is detectable on a `FleetTask`.** Its `status.observedGeneration` lets a reader tell whether the status reflects the latest `spec`. A `FleetAction`'s status declares the same field, but no controller writes it, so it stays unset and shows nothing.

The explicit **non-guarantee** is latency: `status` is coarse and eventually consistent by RA-1, with no upper bound on how quickly a transition becomes visible. RA-1 makes status a throttled projection, written only on a material transition, and sets no further rate limit: the control plane writes a `FleetAction`'s phase on each transition it ingests. A source's control loop MUST NOT depend on the telemetry plane (the time-series backend) as an input — that plane MAY be exposed for dashboards, but reading it as a control signal is forbidden, mirroring the rule that the Zone Controller derives state from the ingest stream and never reads the sink (RFC-0001 Telemetry Pipeline and the Two-Data-Plane Split). A source is coupled only to the `FleetAction` / `FleetTask` CRD schema it can see, never to scheduler state, TDE reservations, or any control-plane internal.

```python
# Pseudo-code.
# Reference outcome loop (atomic FleetAction): watch to terminal, resume by resourceVersion, idempotent.
w = watch.Watch()
for event in w.stream(api.list_namespaced_fleetaction, namespace="warehouse-a",
                      label_selector="swarmada.io/source-id=wms-east"):
    fa = event["object"]
    phase = fa.status.phase
    # A Failed action is final only where RFC-0001's Action Table makes it so: not under row A23's retry, and not while
    # it is Failed (StopUnconfirmed) before row A3 releases its robot. still_open() is true when
    # failureReason == "StopUnconfirmed" and status.assignedRobot is set, or when spec.onFailure == Requeue,
    # status.retryCount < spec.retryPolicy.maxRetries and status.scheduledAt is set.
    # A final Failed whose outcomeCertainty is not Established goes to a person, never to a re-submit.
    if phase in ("Succeeded", "Cancelled") or (phase == "Failed" and not still_open(fa)):
        record_outcome(fa.metadata.name, phase, fa.status.message)
    # No blocking on latency; re-plan at work-order granularity, not per tick.

# A composite FleetTask is watched the same way (list_namespaced_fleettask); the terminal signal
# is its aggregate status.phase, and status.actionSummary ("N/M Succeeded") rolls up the children.
```

<a id="design-details-backpressure"></a>
### 6.4 Backpressure

`FleetAction`s and `FleetTask`s are desired state and MUST live in etcd, so an unbounded `Pending` set is an unbounded etcd object count — and a composite `FleetTask` is doubly costly, since it also spawns child `FleetAction` objects as it fans out. The buffer is bounded at the door, in three layers ordered cheapest first.

1. **Transport — API Priority & Fairness (free).** A `FlowSchema` plus `PriorityLevelConfiguration` per task-source identity throttles a runaway source at the API server before admission, without affecting well-behaved sources.

   ```yaml
   apiVersion: flowcontrol.apiserver.k8s.io/v1
   kind: PriorityLevelConfiguration
   metadata: { name: task-sources }
   spec:
     type: Limited
     limited:
       nominalConcurrencyShares: 10
       limitResponse: { type: Queue }
   ---
   apiVersion: flowcontrol.apiserver.k8s.io/v1
   kind: FlowSchema
   metadata: { name: task-sources }
   spec:
     priorityLevelConfiguration: { name: task-sources }
     distinguisherMethod: { type: ByUser }
     rules:
       - subjects:
           - kind: ServiceAccount
             serviceAccount: { name: wms-east, namespace: warehouse-a }
         resourceRules:
           - verbs: ["create"]
             apiGroups: ["swarmada.io"]
             resources: ["fleetactions", "fleettasks"]
             namespaces: ["warehouse-a"]
   ```

2. **Count — `ResourceQuota` (native).** `count/fleetactions.swarmada.io` and `count/fleettasks.swarmada.io` per namespace cap the total objects of each kind a tenant may hold.

   ```yaml
   apiVersion: v1
   kind: ResourceQuota
   metadata: { name: task-count, namespace: warehouse-a }
   spec:
     hard:
       "count/fleetactions.swarmada.io": "5000"
       "count/fleettasks.swarmada.io": "500"
   ```

3. **In-flight ceiling — admission (CRD-native config).** A validating webhook refuses a `create` with `403 Forbidden`, its message carrying `PendingActionLimitExceeded:` after the prefixes `admission webhook "vfleetaction.swarmada.io" denied the request:` and `fleetactions.swarmada.io "<name>" is forbidden:`; a source matches it as a substring, when the target zone already holds as many `Pending` `FleetAction`s as `SwarmadaConfig.spec.scheduling.maxPendingActionsPerZone` allows. The ceiling counts atomic actions because actions are what a zone's robots ultimately execute; a composite `FleetTask` therefore contributes the actions it fans out (as they become eligible and children are created) to that bound, rather than counting as one. RFC-0001 already enforces this ceiling at admission with that string in the message (RFC-0001 SwarmadaConfig); this RFC documents it as a task-source obligation and conformance-checks the source's handling of it (N4.3) rather than introducing it. The ceiling lives in a CRD, so it is declarative and operator-tunable. It is also off by default: the field is unset, and `0` means unbounded. The webhook fails open when it cannot read the `SwarmadaConfig` or list the zone's actions, and a `FleetAction` with no zone is exempt.

Admission-time **rejection** is preferred over **shedding**: deleting already-accepted desired state is surprising and unsafe for physical work, so the profile bounds the queue rather than draining it. A source meets API Priority & Fairness throttling as `429`, which Kubernetes clients retry with backoff. It meets the in-flight ceiling as `403 Forbidden`, the status an RBAC refusal and the webhook's other refusals also return; the message, which carries `PendingActionLimitExceeded:` anywhere in its text, tells the two apart.

The ceiling is enforced in the `FleetAction` validating webhook, which is a synchronous dependency in the `create` path: if it is unavailable, new work cannot be admitted. The same webhook, and the `FleetTask` webhook, enforce RFC-0001's cancel, requeue, activation and stop-finalizer checks, so a deployment MUST NOT omit either webhook, which RFC-0001 already requires an install to provision (Safety Architecture), or set its `failurePolicy` to `Ignore`, which this profile adds, to remove that dependency. The ceiling keeps no counter: it counts the zone's `Pending` `FleetAction`s at each `create`, and a correct count across concurrent creates is easy to get wrong. The profile therefore defines two conformance profiles ([§7](#conformance)): an **APF + `ResourceQuota` only** profile, in which `maxPendingActionsPerZone` is unset, and a **full** profile, in which it is set. Both run the webhooks. An implementation MUST document which profile it runs.

<a id="design-details-the-north-boundary-invariant"></a>
### 6.5 The North Boundary Invariant

> **The north boundary invariant.** Swarmada's north side — the surface through which task sources submit work — is the Kubernetes API and nothing else. A task source declares desired work by creating `FleetAction` or `FleetTask` resources and observes outcomes by watching their status; it never holds a live control channel, never writes status, and never reads the telemetry plane as a control input. The granularity of the north side is a whole unit of work (seconds to minutes), never a setpoint, waypoint, or per-tick correction. This profile states the boundary, and the mechanism enforces only part of it. The read surface is coarse and eventually consistent by RA-1. An operator can bound the work a source injects with API Priority & Fairness, which limits concurrent requests rather than create rate, and with the in-flight ceiling, which is off by default. Any interaction that would require sub-second feedback, a persistent command channel to the source, or status authored by the source belongs below the safety boundary, on the robot — not on the north side.

The misuse this prevents is a source treating a submitted unit's create→outcome — whether a `FleetAction` or a `FleetTask` — as a servo step — an inner control loop (waypoint-by-waypoint correction) or an online learning loop (each submission an RL environment step). Both smear the safety boundary and hammer etcd. Four independent mechanisms bear on the invariant. The table states what each does by default and the test that would prove it; of those tests, the tree holds only a webhook unit test of the in-flight ceiling's refusal (`internal/webhook/fleetaction_pending_cap_test.go`):

| Mechanism | What it does by default | Test it would need |
| :--- | :--- | :--- |
| **Schema** | Neither `FleetActionSpec` nor the composite `FleetTask` schema (which embeds `FleetActionSpec` per action) has a waypoint-stream or a continuous-setpoint field; `payload` is opaque and passed verbatim to the adapter, never interpreted as control; an `External` action carries no `payload` and reaches no robot (RFC-0001 FleetAction, *External Actions*). Always in force, as part of the CRD. The payload can still carry a whole motion plan, such as a waypoint route, in one submission. | Schema review + a test asserting no motion/setpoint field exists and that `payload` is never parsed by the control plane. |
| **Read surface** | The only outcome signal is the submitted unit's `status` (`FleetAction.status`, or the aggregate `FleetTask.status` and the status of the child `FleetAction`s it owns), coarse by RA-1. No limit applies beyond RA-1's material-transition rule: a `FleetAction`'s phase is written on each transition. | A timing test measuring whether status latency can sustain a sub-second loop. |
| **Admission** | Nothing, until an operator configures it; the tree ships no API Priority & Fairness objects. API Priority & Fairness limits a source's concurrent requests, not its create rate. The per-zone in-flight ceiling, once set, refuses a `create` with `403` when the zone holds that many `Pending` actions; it fails open, and a zone-less action is exempt. | Load test: a source exceeding the configured limits receives `429` from API Priority & Fairness, or `403` with `PendingActionLimitExceeded` from the ceiling. |
| **RBAC** | The Role in [§6.1](#design-details-identity-and-authorization) grants no verb on `fleetactions/status` or `fleettasks/status`. That holds where an operator binds a source to that Role or one like it; the tree ships no task-source Role. | AuthZ test: `create` on `fleetactions/status` or `fleettasks/status` as a task-source identity is `Forbidden`. |

A source *does* legitimately close a loop — at **work-order** granularity: it re-plans, re-decomposes, and re-prioritizes based on terminal outcomes. That is supported, because RA-1 keeps the feedback coarse and the source coupled only to a CRD schema. A re-plan is safe only once the work it replaces has ended by the rules of N3.3 and N3.6: a re-plan that repeats unfinished work gives it to a second robot.

<a id="design-details-disconnect-restart"></a>
### 6.6 Disconnect / Restart

There is no stream to drop — that is the point. A task source is stateless with respect to the control plane. If it crashes, the `FleetAction`s and `FleetTask`s it created persist and keep reconciling. On restart it re-lists its own resources by the `swarmada.io/source-id` label selector and reconciles intent against observed state — a resync, exactly like a controller — and idempotent create-by-name makes a replay safe while the unit it names still exists ([§6.2](#design-details-idempotent-submission)). The north side holds no live robot authority, so it needs none of the south side's lease, fencing, or `Revoking` machinery, which exist precisely because the adapter *does* hold live authority over a moving robot. This asymmetry is the headline: the north side is safe under disconnect because it is declarative and holds no authority.

<a id="design-details-reference-task-source"></a>
### 6.7 Reference Task Source

The north analogue of `example-noop` is a minimal reference task source that proves the contract in the smallest correct form: it authenticates with a ServiceAccount token; creates a handful of `FleetAction`s (and at least one composite `FleetTask`) with deterministic names from a static work order (proving idempotency across both kinds); watches their status to terminal outcome, reading a `FleetAction`'s `status.phase` and a `FleetTask`'s aggregate `status.phase` / `status.actionSummary`, which a `FleetTask` with `spec.flow` does not carry at v0.4 (proving the read surface); backs off on `429` and on the in-flight ceiling's `403` (proving backpressure handling); and, on restart, re-lists and resyncs (proving stateless recovery). Per the ADR-0005 precedent, the reference source and the SDK belong in the core tree as templates and conformance infrastructure, and neither is there ([§1](#abstract)); concrete WMS integrations live in their owners' repositories and never in the neutral core. Its proof obligations are exactly the checklist in [§7](#conformance).

<a id="design-details-sdk-surface"></a>
### 6.8 SDK Surface

The Task-Source SDK this document specifies is the north ergonomics layer — the analogue of a generated gRPC stub, not a new protocol. It would live in `sdk/python`, which holds only a `safety` module and none of the surface below ([§1](#abstract)). It would provide typed `FleetAction` and composite `FleetTask` builders, a deterministic-naming helper that implements the `fa-<stable-hash(work-item-id)>` convention for actions (and an `ft-`-free naming helper for composites), a watch/outcome iterator that yields terminal transitions — surfacing a `FleetAction`'s phase and a `FleetTask`'s aggregate phase plus `actionSummary` — and resumes by `resourceVersion`, and a backoff-aware client that treats `AlreadyExists` as success and a `429`, or a `403` whose message contains `PendingActionLimitExceeded:`, as retry-with-backoff. The SDK is a convenience over the API, never a required path: a source that speaks the Kubernetes API directly is fully conformant without it.
<a id="conformance"></a>
## 7. North Conformance Checklist

This checklist defines what a task source MUST, SHOULD, and MAY do to be a conforming Swarmada task source. It is the north analogue of the Fleet Adapter conformance checklist (`adapters/CONFORMANCE.md`) and is derived from the profile in [§6](#design-details). Where this checklist and RFC-0001 differ in the *interpretation* of a shared term or mechanism, RFC-0001 governs. This checklist MAY, however, raise an RFC-0001 `SHOULD` to a `MUST` for conformance to this profile — narrowing a permission is what a profile is for, and N2.1 is exactly such a strengthening of the deterministic-naming `SHOULD` in RFC-0001's `FleetAction` chapter. A profile MUST NOT weaken an RFC-0001 requirement. Conformance — not repository location — would make a task source conforming to this draft profile; no task source can claim it at v0.4 ([§1](#abstract)). The reference task source ([§6.7](#design-details-reference-task-source)) exists to pass every check below.

A deployment that runs task sources MUST declare which backpressure profile it targets: **APF+Quota** (checks N4.1–N4.2) or **Full** (N4.1–N4.3). Neither profile omits the admission webhooks.

> **No check.** Five rules of [§6](#design-details) have no check in this list: that `FleetAction`s and `FleetTask`s live in etcd ([§6.4](#design-details-backpressure)); documenting which backpressure profile an implementation runs ([§6.4](#design-details-backpressure)), which the declaration this checklist requires above restates; the verbs granted to a task source ([§6.1](#design-details-identity-and-authorization)); recording each work item's terminal outcome before its unit can be deleted, and not creating that unit again ([§6.2](#design-details-idempotent-submission)); and serving the `FleetAction` and `FleetTask` validating webhooks with a `failurePolicy` other than `Ignore` ([§6.4](#design-details-backpressure)). The first is a property of every custom resource the Kubernetes API stores, which no source can change. The second is a declaration, not a behaviour a check observes. The third and the fifth bind the deployment that runs a source, not the source. The tree ships no task-source `Role`. The shipped webhook configuration sets `failurePolicy: Fail` on both webhooks; the separate `FleetAction` delete check is `Ignore` and fails open, as RFC-0001 specifies.

<a id="conformance-n1-identity-and-authorization"></a>
### 7.1 N1 — Identity and Authorization

- **N1.1 (MUST)** Authenticate as a Kubernetes ServiceAccount, with a short-lived token; never as a user, by mTLS client certificate or OIDC identity. Hold no ambient or shared credential.
- **N1.2 (MUST)** Operate within a single namespace per identity; make no cross-namespace submission.
- **N1.3 (MUST NOT)** Request or use any verb on `fleetactions/status` or `fleettasks/status`.
- **N1.4 (MUST NOT)** Hold or use the `estop-clear` verb.
- **N1.5 (MUST NOT)** Set `swarmada.io/requeue-requested` on any `FleetAction`. A requeue is an operator's decision (RFC-0001 Safety Architecture, row A2), and a task source cannot observe whether a stop holds the action's robot.
- **N1.6 (MUST NOT)** Create, patch or apply any `FleetAction` or `FleetTask` it did not itself submit, including the child `FleetAction`s of a composite.
- **N1.7 (MUST NOT)** Use impersonation, or hold the `impersonate` verb on users, groups or service accounts.

<a id="conformance-n2-idempotent-submission"></a>
### 7.2 N2 — Idempotent Submission

- **N2.1 (MUST)** Name each submitted unit deterministically from durable upstream identifiers only — never from a random nonce and never from the wall-clock time of submission; a scheduled occurrence instant defined by the upstream item is durable and is permitted: an atomic `FleetAction` as `fa-<stable-hash(work-item-id)>`, and a composite `FleetTask` with an equivalently deterministic name that avoids the `ft-` prefix (`ft` is the composite's `shortName`, and a name prefix must not be any kind's `shortName`) and the `fa-` prefix reserved for atomic actions.
- **N2.2 (MUST)** Treat an `AlreadyExists` response to `create` as success.
- **N2.3 (MUST)** Label each submitted `FleetAction` or `FleetTask` with `swarmada.io/source-id`; **(SHOULD)** set `swarmada.io/source-order` where an upstream ordering exists.
- **N2.4 (SHOULD)** Use server-side apply with a stable field manager for any post-create adjustment, owning only `spec`.
- **N2.5 (MUST NOT)** Use server-side apply to contend for a field it does not own (it owns `spec`, never `status`).

<a id="conformance-n3-outcome-observation"></a>
### 7.3 N3 — Outcome Observation

- **N3.1 (MUST)** Observe outcomes only via watch/informer on the submitted unit's status (`FleetAction.status`, or the aggregate `FleetTask.status` — its `phase`, `status.actions[]` and `actionSummary`, which a `FleetTask` with `spec.flow` does not carry at v0.4), on the status of the child `FleetAction`s a submitted composite owns, and Kubernetes Events.
- **N3.2 (MUST)** Resume a broken watch from the last observed `resourceVersion`, re-list on `410 Gone`, and tolerate at-least-once (duplicate) delivery.
- **N3.3 (MUST)** Treat `Succeeded`/`Failed`/`Cancelled` as durable, monotonic terminal outcomes, and additionally `Compensated` for a composite `FleetTask`; an atomic `FleetAction`'s `Failed` is terminal only where the RFC-0001 *Action Table* makes it so: not under row A23's retry, and not while it is `Failed` (`StopUnconfirmed`) before row A3 releases its robot, whether or not row A6 holds it. RFC-0001's `FleetTask` phase enum is `Draft;Pending;Running;Succeeded;Failed;Compensating;Compensated;Cancelled`, in which `Compensating` is transient and `Compensated` is the terminal settling state under `failurePolicy: Compensate`. A source that omits `Compensated` from its terminal set never observes an outcome for a compensated task. A composite `FleetTask`'s terminal phase is not evidence that its members have stopped: before re-submitting work that repeats a member, or recording the composite's outcome, a source MUST observe every child `FleetAction` of the composite terminal by the atomic rule in this check. A child that disappears before it was observed terminal has not ended; the work item goes to a person.
- **N3.4 (MUST NOT)** Read the telemetry backend as a control input, or couple to scheduler state, TDE reservations, or any control-plane internal.
- **N3.5 (MUST NOT)** Assume any latency bound on status; re-plan at work-order granularity, not per tick.
- **N3.6 (MUST NOT)** Re-submit work that repeats a `Failed` `FleetAction`, or a member of a `Failed` composite, whose `status.outcomeCertainty` is not `Established`, empty included. Hand that work item to a person.

<a id="conformance-n4-backpressure-handling"></a>
### 7.4 N4 — Backpressure Handling

- **N4.1 (MUST)** Back off and retry on API Priority & Fairness throttling (`429`), as an ordinary Kubernetes client.
- **N4.2 (MUST)** Handle `ResourceQuota` rejection on `create` without dropping the underlying work-item intent.
- **N4.3 (MUST, Full profile)** Handle `PendingActionLimitExceeded` (the admission-enforced in-flight ceiling on pending `FleetAction`s in a zone, `SwarmadaConfig.spec.scheduling.maxPendingActionsPerZone`, to which a `FleetTask`'s fanned-out actions also count) with backoff; **(MUST NOT)** attempt to bypass it by writing to a different zone the work does not belong to, or by omitting `spec.zone` from work that belongs to a zone.

<a id="conformance-n5-statelessness-and-recovery"></a>
### 7.5 N5 — Statelessness and Recovery

- **N5.1 (MUST)** Hold no live authority over any robot and no persistent control channel to the control plane.
- **N5.2 (MUST)** On restart, re-list owned tasks by `swarmada.io/source-id` and reconcile intent against observed state rather than blindly re-submitting.
- **N5.3 (SHOULD)** Be safely restartable at any point, relying on idempotent create-by-name for replay safety.

<a id="conformance-n6-boundary-invariant"></a>
### 7.6 N6 — Boundary Invariant

- **N6.1 (MUST NOT)** Use a submitted unit's create→outcome (`FleetAction` or `FleetTask`) as an inner control or online-learning loop (setpoint, waypoint, or per-tick correction).
- **N6.2 (MUST)** Express work at whole-unit-of-work granularity (seconds to minutes); keep any sub-second loop on the robot, below the safety boundary.

A `skip` on any check records what was and was not verified for a given source and is not a pass. A conforming task source has no failed MUST/MUST NOT among the checks that apply to its declared profile, and a claim of conformance names every check that was skipped and its cause. "Conforming" is this checklist's term: it is not RFC-0001's `verdict: pass` (RFC-0001, *Conformance Profiles*), which a Core check left unmeasured withholds.
<a id="drawbacks"></a>
## 8. Drawbacks

This section is deliberately the most critical in the RFC. The profile buys neutrality and reuse at a real, named cost.

1. **Kubernetes-client lift.** Task sources must be Kubernetes API clients, or use the SDK. Enterprise WMS teams native to AMQP, MQTT, or REST webhooks may never have written an informer or reasoned about `resourceVersion`. This is a genuine integration cost, and it is the reason this document proposes the SDK and the reference source, neither of which exists in the tree. For a system that sends REST webhooks, one route does ship: the external event bridge example maps an authenticated request to an RFC-0003 `SemanticEvent`, and the work it causes waits as a draft `FleetTask` until a person activates it. The standard does not resolve it by building a gateway, because a WMS-format shim is data-format glue that would bake one task-source worldview into the neutral core (ADR-0006); such a gateway is legitimate only out-of-core, and the decision to build one should be triggered by an integrator reporting the lift as a hard blocker, not by symmetry with the south side.

2. **The enforcement webhook is a synchronous create-path dependency.** An unavailable `FleetAction` webhook blocks all new work in either profile, because the same webhook holds RFC-0001's stop and cancel checks. The Full profile adds the in-flight ceiling, whose count of a zone's `Pending` actions is easy to get wrong under concurrent creates, and a mis-set ceiling. The APF+Quota profile leaves the ceiling unset and avoids those two costs, not the webhook.

3. **etcd holds all pending work.** Unlike telemetry, which RA-1 keeps off etcd, `FleetAction`s and `FleetTask`s are etcd objects — and a composite `FleetTask` amplifies the count by spawning child `FleetAction`s as it fans out; the in-flight ceiling on pending actions is load-bearing. Mis-tuning it either starves throughput (too low) or risks the datastore (too high), and getting it right requires understanding both the fleet's drain rate and etcd's object budget.

4. **The coarse read surface constrains legitimate fast feedback.** A source that genuinely wants low-latency outcome signals cannot get them from the API and must use the (non-control) observability plane. This is the same mechanism that makes the boundary invariant structural, so it cannot be relaxed without weakening the invariant — but it is a real limit for sources that want tight, honest, non-motion feedback.

5. **Determinism is the source's responsibility.** Idempotency rests entirely on the source deriving a stable name from a durable upstream identifier. A source that hashes in a timestamp, a retry counter, or any non-durable field silently breaks idempotency and will double-submit under retry — and the failure is invisible until it manifests as duplicate physical work.

6. **The ergonomic surface is left to whoever builds it first.** Declining to ship a north ingestion gateway in the core leaves that surface to third-party implementations. This is a deliberate neutrality trade with a real cost: if a third-party gateway becomes the de-facto north interface while no SDK or reference source shows that the API-native path is tractable, integrators will learn that gateway's conventions rather than this profile's, and the profile will be documenting a path few take.
<a id="alternatives"></a>
## 9. Alternatives Considered

- **Bespoke north protocol (north analog of `fleet_adapter.v1`).** Rejected — reinvents the API server's authn/authz/admission/quota/versioning as a second normative surface; south-side forces (NAT, pushed estop, telemetry cadence, proprietary hardware) do not act here. (ADR-0006.)
- **Mandatory in-core ingestion gateway.** Rejected as core — data-format glue, not a protocol; harms neutrality and adds a maintenance tax (ADR-0005 lesson). Legitimate out-of-core, registry-tracked; revisit on a design-partner blocker.
- **Documentation-only (no enforcement).** Rejected — leaves the boundary invariant and backpressure as norms an overrunning or loop-driving source can ignore. This profile pairs each with the mechanisms that bear on it, although some act only where an operator configures them ([§6.5](#design-details-the-north-boundary-invariant)).
- **Status-only feedback with no watch contract.** Rejected — without stated delivery and staleness guarantees, integrators build fragile polling loops.
<a id="references"></a>
## 10. References

References follow the RFC-0001 grouping. URLs were verified as of the `updated` date in this chapter's frontmatter.

<a id="references-normative"></a>
### Normative

[1] Swarmada, "RFC-0001: Swarmada Core Specification." `rfcs/dist/RFC-0001-core-spec.md` *The base specification this RFC extends: the FleetAction chapter (the atomic unit) and its reserved task-source naming and label conventions, the FleetTask chapter (the composite unit), Resource Model at a Glance (reading guidance: a work order maps to a composite FleetTask, simple one-step work to a single FleetAction), the `SwarmadaConfig.spec.scheduling.maxPendingActionsPerZone` ceiling, the Telemetry Pipeline and the Two-Data-Plane Split (Zone Controller derives state from the ingest stream, never the sink), the Security Model and the Safety Architecture on the estop-clear prohibition, and the RA-1 status-write discipline.*

[2] Swarmada, "ADR-0006: The task-source surface is the Kubernetes API." `docs/adr/0006-north-side-is-the-kubernetes-api.md` *The transport decision this RFC builds its contract on. ADR-0006 records that the north surface is the Kubernetes API and that no bespoke north protocol will be introduced; this RFC adds the read-surface guarantees, the enforced ceiling, the boundary invariant, and the reference artifacts it defers.*

[3] S. Bradner, "Key words for use in RFCs to Indicate Requirement Levels," IETF RFC 2119, March 1997. [https://www.rfc-editor.org/rfc/rfc2119](https://www.rfc-editor.org/rfc/rfc2119) *Defines MUST, MUST NOT, SHOULD, and MAY as used throughout this document and the conformance checklist.*

[4] Kubernetes Community, "Kubernetes API Conventions." [https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md) *The profile reuses these conventions rather than reinventing them; `docs/api-principles.md` binds Swarmada to them.*

<a id="references-informative"></a>
### Informative

[5] Kubernetes, "API Priority and Fairness." [https://kubernetes.io/docs/concepts/cluster-administration/flow-control/](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/) *The `FlowSchema` / `PriorityLevelConfiguration` mechanism used as the first, free layer of north backpressure.*

[6] Kubernetes, "Resource Quotas." [https://kubernetes.io/docs/concepts/policy/resource-quotas/](https://kubernetes.io/docs/concepts/policy/resource-quotas/) *`count/fleetactions.swarmada.io` and `count/fleettasks.swarmada.io` per-namespace object caps — the second backpressure layer.*

[7] Kubernetes, "Server-Side Apply." [https://kubernetes.io/docs/reference/using-api/server-side-apply/](https://kubernetes.io/docs/reference/using-api/server-side-apply/) *The field-manager semantics behind the idempotent-upsert submission path.*

[8] Swarmada, "API Design Principles." `docs/api-principles.md` *CRD conventions. The profile relies on the served API version instead of a connect-time protocol negotiation. Any addition the profile implies would need a contract change.*
