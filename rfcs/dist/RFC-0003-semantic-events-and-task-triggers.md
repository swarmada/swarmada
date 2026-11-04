---
title: "RFC-0003: Semantic Events and Declarative Task Triggers"
author: "Swarmada maintainers <maintainers@swarmada.io>"
status: Draft
created: 2026-07-23
updated: 2026-09-23
swarmada-version: v0.4
published: true
---

# RFC-0003: Semantic Events and Declarative Task Triggers

<a id="abstract"></a>
## 1. Abstract

> **Status: draft for review. Not part of RFC-0001 v0.4, and not to be implemented from this text.**
> This document is published beside RFC-0001 v0.4 for review. Nothing in RFC-0001 depends on it, and an
> implementation that ignores it conforms to RFC-0001. Every resource, field, enum value, annotation,
> RBAC verb, lifecycle transition and `fleet_adapter.v1` message this document adds would need a
> contract change (RFC-0001, Resource Model at a Glance, *The contract-change process*).
>
> **Implementation level.** The document has two halves, at different levels.
>
> - **The wire half** — `TaskEvent` on `ActionStatusUpdate`, sequencing, `terminal`, the response set of
>   [§6.8](#design-details-the-tasktrigger-resource) and the approval gate — is specified and not built.
>   `TaskEvent` and `EventSubjectKind` exist in `fleet_adapter.v1`, but no field carries them, so no adapter
>   can raise a semantic event.
> - **The draft path** ([§7](#resources)) ships. A `SemanticEvent` that matches a `TaskTrigger` produces a
>   `FleetTask` held at phase `Draft`, and nothing on that path dispatches a robot. Where the admission
>   webhooks are served, only a principal that is not a ServiceAccount and holds `activate` can clear a
>   draft; on an install without them the draft flag is not an authorization boundary. No admission
>   webhook validates the three kinds, so their rules are applied after admission.
>
> **A trigger only drafts.** Until a mechanism bounds a trigger's authority by the identity that authored
> it, a trigger's only response is a `FleetTask` held as a draft, which a person activates. The
> `appendAction` and `setDesiredState` responses are specified and MUST NOT be executed
> ([§8](#conformance), E7.4).
>
> **Conformance.** No implementation can claim conformance to this document, and nothing here changes what
> conformance to RFC-0001 means. The Event Source checks (E0–E4, E10) are reserved, and the suite declares no
> Event Source profile. The Trigger Consumer checks (E0.1, E0.2, E5–E9) grade the wire half, which no implementation
> provides, and no report grades them. The draft path's requirements ([§7](#resources)) carry no check
> identifier, and a note after each list there says so. The requirements this document addresses to a
> reader or an operator are listed in [§8](#conformance); no check can grade them.

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHALL NOT**, **SHOULD**, **SHOULD NOT**, **RECOMMENDED**, **MAY** and **OPTIONAL** in this document are to be interpreted as described in BCP 14 (RFC 2119 and RFC 8174) when, and only when, they appear in all capitals.

RFC-0001 defines composition over `FleetAction`s in the composite `FleetTask` (RFC-0001, the `FleetTask` chapter): a dependency graph, a completion/failure/compensation policy, a declarative `desiredState`, and append-only plan growth. Its reactivity is deliberately phase-driven and declarative — a member action advances on a predecessor's *phase*, and an external actor reacts to observed state by writing `desiredState` or appending an action. What RFC-0001 does not provide is a first-class notion of a **domain event** that a robot's own capability raises, or a standardized way to **bind** such an event to a bounded, auditable response.

This RFC adds exactly those two things: a **semantic event** channel on the Fleet Adapter protocol, and a `TaskTrigger` resource that declares `source → condition → response`. The recorded event history is explicitly bounded: a busy execution MUST NOT be able to grow its own object until the control plane can no longer write to it, so the resource carries a recent window and complete scalar counters while the audit log and telemetry sink hold the history. A response is drawn from the same bounded, already-existing RFC-0001 surface — append an action, set a `desiredState` — plus one added capability: a response MAY be marked `requireApproval`, in which case it is computed and staged but never executed until a named human confirms exactly what was staged, under the gate's own single-use, scoped, expiring, record-before-effect properties ([§6.11](#design-details-the-approval-gate)). In this revision a trigger's only response is a `FleetTask` held as a draft, which a person activates ([§6.8](#design-details-the-tasktrigger-resource)). Every addition is strictly additive and capability-gated: an adapter that never emits events is unaffected and continues to conform to RFC-0001 unchanged.

This RFC previously also proposed `FleetAction.spec.executor.completionMode` (a `Continuous`, never-succeeding watch action) and time-synchronized co-completion across a set of actions. Both are removed from this revision. `completionMode` is RFC-0001 core's; co-completion is not specified in this version. What remains here — semantic events and the declarative bindings that consume them — is the coherent thesis, and it stands alone.

> **Maturity (revision v0.2, Swarmada v0.4):** stage: alpha · control-plane: partial · end-to-end: not exercised.
> **A narrower mechanism than this document specifies ships; the wire half does not.** `TaskEvent` and `EventSubjectKind`
> are defined in `fleet_adapter.v1` ahead of this RFC, so that
> `compute/v1`'s `StreamStatus` RPC can reuse one event shape rather than define a second for a
> second execution kind. **Neither is wired onto `ActionStatusUpdate`** — the field this document
> adds is unassigned, deliberately, and no adapter can carry a semantic event.
>
> **Unresolved in this revision (`TaskEvent` in `fleet_adapter.v1`).** `TaskEvent` and
> `EventSubjectKind` are in the `fleet_adapter.v1` proto, and `compute.v1`'s `StreamStatus` returns
> `TaskEvent`. RFC-0001 versions that proto as its contract, yet its Fleet Adapter Protocol chapter
> specifies neither message, and the RFC index says that no message a draft proposes is part of
> RFC-0001 v0.4. RFC-0001 also defines a semantic event as a `TaskEvent`. Draft 5 resolves whether the
> two messages are part of the contract, and so specified by RFC-0001, or leave
> `fleet_adapter.v1`; this revision does not choose.
>
> One further piece exists, on the endpoint side: `ComputeEndpoint.spec.emitsEvents`, in the Go
> type and the generated CRD, is the declaration surface
> [§6.5](#design-details-declaration-is-symmetric) requires of an endpoint and that `E10.1`/`CE6.1`
> state. Both rows are excluded, and no check grades the declaration. No controller reads it, and no endpoint raises an event. Its one reader is the
> `FleetTask` validating webhook (`validateProducerContracts`), which, when a `FleetTask` is
> created or an update changes its `flow`, `inputs` or `actions`, refuses with reason `UndeclaredEvent` an event binding — a `select.subject`, a
> `loop.until.anyOf[].event`, or a `fromMember.event` parameter binding on a member's executor or
> its compensation — that names a type the producing member's `ComputeEndpoint` does not list,
> including when the list is empty. An update that changes none of those three fields is not
> re-checked, and an endpoint that cannot be read is skipped, so the binding is admitted. Nothing checks a raised event against the field:
> the rejection of an undeclared `type` that `E10.1`/`CE6.1` require at ingest is unbuilt.
>
> On the control-plane side a `TaskTrigger` Go type, a generated CRD and a controller that the
> manager runs all exist,
> together with the `SemanticEvent` and `SemanticEventSource` kinds the controller reconciles. What
> ships is deliberately narrower than what this document specifies, and
> [§6.13](#design-details-what-the-reference-implementation-provides) states the difference. **There is
> still no admission webhook for `TaskTrigger`**, so the ownership and `subjectKind` validation this
> document requires is unenforced.
>
> The rest of this document remains absent from the reference implementation, with one partial
> exception. `FleetAction.status` does carry a bounded event window, in a different shape from the
> one specified here: `events[]` (at most 32 entries, the most recent retained, keyed by `seq`),
> `eventSeqHigh` (the highest `seq` ever accepted) and `eventsDropped` (the count retention has
> evicted). There is no `lastEventSeq` and no `observedEventCount`, and the bound is fixed in the
> CRD. The `FleetTask` controller reads the window, for `select` and `loop.until` event arms and
> for `fromMember.event` bindings; its one writer, `ComputeEventIngestor`, which ingests
> `compute.v1` `StreamStatus`, is constructed by no non-test code, so a running manager never
> writes it. `SwarmadaConfig.spec` has no `events` block, so
> `maxRecordedEvents` — which the bounded-history rule below states as a MUST — names a field that
> does not exist. Neither `trigger-response-approve` nor `trigger-response-reject` is registered
> in any role or RBAC manifest, and none of `TriggerResponsePending`, `TriggerResponseApproved`,
> `TriggerResponseRejected` or `TriggerResponseExpired` exists as a constant or has a writer.
>
> **What exercises this document.** Nothing grades it. The Event Source checks (E0–E4, E10) are reserved:
> their identifiers are held so they cannot be reassigned, and the suite declares no Event Source profile. The
> Trigger Consumer checks (E0.1, E0.2, E5–E9) grade the wire half's design, which no implementation
> provides, and no conformance report grades them.

Revision v0.2 fixes two wire details that would break to retrofit once published: it reserves the sequence scope's subject discriminator, and it defines `terminal` for a `completionMode: Continuous` executor run. It also adds the conformance chapter.
<a id="motivation"></a>
## 2. Motivation

Real fleet work is reactive, and some of that reactivity originates on the robot itself. A patrol robot's onboard camera detects an intruder and a responder must be dispatched. An inspection arm finds a defect and a quarantine action must run. A robot working an assembly line loses a capability it needs mid-task, and the rest of the line should not default to a full stop while it waits on that one robot's recovery — another robot, or a pre-declared contingency, should take over the affected step once the first robot is off it, as RFC-0001 states (Safety Architecture, *How the Two Interact*, I1 and I2). RFC-0001 already handles the *external-detector* form of this cleanly: a fixed camera or a separate monitoring system observes, and an external actor writes `desiredState` or appends an action, with no robot-to-robot coupling. What it cannot express is the case where the **robot's own capability** raises the event, because the Fleet Adapter contract carries only lifecycle state (`ActionState`) and `progress_pct` — no domain-event channel.

A second, related need showed up while investigating mid-task recovery policy directly: some computed responses are safe to request immediately (reroute a task to another eligible robot, which RFC-0001's capability-loss reassignment already requests, and carries out under no stop once the first robot is off it, row A20 of RFC-0001's *Action Table* (Safety Architecture)), and some are consequential enough — binding a second robot into an active production step, redirecting work across a zone boundary — that an operator wants the system to compute and stage the response, then hold it for a specific person to confirm before it takes effect, rather than either auto-executing it or requiring a human to notice the failure and author the response from scratch. Neither pure automation nor pure human-authorship serves this case well: the first is unsupervised, the second is exactly the "an external actor watches, decides, and appends" pattern RFC-0001 already leaves to something outside the control plane, and is too slow when the response is already knowable.

None of this belongs in RFC-0001 core: it touches the (alpha) Fleet Adapter proto, it is not uniformly implementable across VDA5050 / MAVLink / ROS 2 ([§9](#drawbacks)), and keeping the core minimal is load-bearing for the CNCF neutral-donation surface. It is gathered here, additive and opt-in.
<a id="goals"></a>
## 3. Goals

1. Define **semantic events** — typed, capability-defined domain events a robot's onboard capability raises — and the additive Fleet Adapter channel that carries them, event-sourced with a sequence monotonic per execution subject so they survive intermittent connectivity idempotently.
2. Define a declarative binding resource, `TaskTrigger` — `source → condition → response` — that turns a matching event into a bounded RFC-0001 action: append an action to a live `FleetTask` (via the existing append-only path and `append` verb), or set a `desiredState`. The mechanism is standardized; specific proprietary integrations stay out of the standard. Until a mechanism bounds a trigger's authority by the identity that authored it, a trigger only drafts a `FleetTask` that a person activates ([§7.3](#resources-tasktrigger)).
3. Scope a trigger's `condition` to the robot, zone, and adapter identity of the event that fired it — so a response can be written once and correctly limited to, for example, one vendor's robots or one physical zone — using a bounded expression over a documented, closed context, never arbitrary code.
4. Define an optional per-response **approval gate**: a response marked `requireApproval` is computed and staged, never executed, until an authenticated human holding a separately-grantable verb confirms the exact staged response. This RFC states the gate's properties itself: single-use, scoped, expiring, record-before-effect.
5. Preserve RFC-0001's single-writer ownership rule: nothing in this RFC writes a task-owned member action's `desiredState`. A response targets only a standalone action or the owning `FleetTask`'s own `desiredState`.
6. Keep every addition **additive, backward-compatible, and capability-gated**: an adapter that does not advertise events is unaffected and still conforms to RFC-0001; a `FleetTask` that needs an unsupported capability does not schedule onto that adapter.
7. Bound what the control plane records. An execution's recorded event history is a **window**, not a log: the custom resource MUST stay writable however many events its execution raises, and the audit log and telemetry sink are the system of record ([§6.7](#design-details-bounded-event-history)).
8. Admit **event sources that are not fleet adapters**. An action executed by an addressable endpoint raises domain events for the same reasons a robot capability does; this RFC specifies who assigns the sequence number for each source class, how events are attributed, and that a producer never names its consumers ([§6.3](#design-details-event-sources-other-than-adapters), [§6.4](#design-details-attribution-a-producer-never-names-its-consumers)).
9. State conformance as testable checks. Two independent profiles — **Event Source** (a fleet adapter, or an executor that is not one) and **Trigger Consumer** (a control plane) — each with MUST/SHOULD/MAY clauses an implementer can be measured against ([§8](#conformance)).
<a id="non-goals"></a>
## 4. Non-Goals

1. **Not motion-level.** Events here are task-level domain signals (seconds), never a real-time control or safety path. Safety-of-life reflexes stay onboard, below the RFC-0001 safety boundary.
2. **Not a replacement for external detectors.** RFC-0001's `desiredState`/append surface already handles external-sensor reactions; this RFC adds the onboard-capability event path alongside it, not instead of it.
3. **Not vendor-specific trigger integrations.** This RFC defines the binding mechanism. An integration with a particular WMS, alarm panel, or vision product is out of scope for the standard and belongs in its own repository, on the ADR-0005 reference-adapter precedent.
4. **Not arbitrary scripting.** A `TaskTrigger`'s `condition` is a bounded expression over a documented, closed context (the firing event, the action, the robot, its zone, and its adapter identity — [§6.10](#design-details-condition-scoping-vendor-zone-and-robot)), so a flow stays statically analyzable. A response is drawn from a fixed, declarative set, consistent with RFC-0001's `FleetTask` chapter.
5. **Not an edge-triggered command bus.** Nothing here commands a robot on an event edge. A response MUST be declarative and level-triggered: it writes intent that persists and re-converges after a disconnect, so a dropped edge is never a lost command (ADR-0047). This is RFC-0001's `desiredState` discipline, not a new rule introduced here.
6. **Not a notification or paging system.** A staged, unapproved response emits an ordinary Kubernetes Event and a status condition. Routing that to a person — chat, pager, email — is operator tooling and stays out of scope, the same boundary RFC-0001 draws around alerting generally: this RFC ships the event, not the alert.
7. **Not `FleetAction.spec.executor.completionMode` and not time-synchronized co-completion.** An earlier draft of this RFC specified both. `completionMode` is RFC-0001 core's, and co-completion is not specified in this version. Neither is redefined here.
8. **Not a role or persona system.** "Who may approve a staged response" is a custom verb (`trigger-response-approve`) that an operator's own RBAC binds to the people it chooses, never to a `ServiceAccount` ([§6.11](#design-details-the-approval-gate)). This RFC does not define a built-in "service engineer" role or any other named persona.
<a id="proposal"></a>
## 5. Proposal

This RFC adds two capabilities on top of RFC-0001, each independently adoptable.

<a id="proposal-semantic-events-and-the-additive-adapter-channel"></a>
### 5.1 Semantic events and the additive adapter channel

A capability may raise a typed **semantic event** (`IntruderDetected`, `DefectFound`) that is opaque to the control plane and matched only by trigger rules. Events reach the control plane on the existing task-status stream and are recorded on `FleetAction.status.events[]`, event-sourced with a monotonic per-action sequence number for idempotent replay under intermittent connectivity. See [§6](#design-details).

<a id="proposal-the-tasktrigger-binding-resource"></a>
### 5.2 The `TaskTrigger` binding resource

A namespace-scoped `swarmada.io/v1` `TaskTrigger` binds `source → condition → response`. On a matching event whose `condition` evaluates true, it enacts a declarative response, never an edge-triggered robot command. In this revision the only response an implementation executes is `draftTask`, which creates a `FleetTask` held at phase `Draft` for a person to activate. Appending an action to a `FleetTask` via the RFC-0001 append-only path, or setting a `desiredState`, is specified and not executed until a mechanism bounds a trigger's authority by the identity that authored it ([§6.8](#design-details-the-tasktrigger-resource)). A response MAY be marked `requireApproval`; if set, the controller computes and stages the response but does not act on it until a scoped human confirmation is redeemed. The authenticated identity behind a `TaskTrigger` is its authority; a matching event can *fire* a trigger within the bounds that identity authored, and can never *create or widen* one.

Neither capability changes the behavior of an `FleetAction` or `FleetTask` that does not use it. An adapter that never emits events is unaffected and continues to conform to RFC-0001 exactly as before this RFC exists.
<a id="design-details"></a>
## 6. Design Details

<a id="design-details-adapter-channel-the-additive-taskevent"></a>
### 6.1 Adapter channel: the additive `TaskEvent`

**A corrected message target.** An earlier draft of this RFC targeted a message, `TaskStatusUpdate{task_id, ...}`, that does not exist in the RFC-0001 Fleet Adapter proto. The real message is `ActionStatusUpdate{action_id=1, state=2, message=3, fencing_token=4, progress_pct=5, end_cause=6, superseding_action_id=7, retained=8}` (RFC-0001's Fleet Adapter Protocol chapter reproduces it). This RFC adds a new, wire-compatible field to that real message:

```proto
// The execution an event was raised by. This RFC defines exactly one kind. The field
// exists so that a later execution kind is an additive enum value rather than a
// reinterpretation of `seq`, which would be a breaking change once published.
enum EventSubjectKind {
  EVENT_SUBJECT_KIND_UNSPECIFIED = 0;       // a receiver of revision v0.2 MUST read this as ACTION
  EVENT_SUBJECT_KIND_ACTION      = 1;       // the FleetAction identified by action_id
}

message TaskEvent {
  string type = 1;                          // "IntruderDetected" — capability-defined, opaque
  uint64 seq  = 2;                          // monotonic per (subject_kind, subject id); idempotency key
  int64  time_ms = 3;
  google.protobuf.Struct payload = 4;
  bool   terminal = 5;                      // marks the event that ends a Continuous execution
  EventSubjectKind subject_kind = 6;        // added in revision v0.2; UNSPECIFIED means ACTION
}
message ActionStatusUpdate {
  // ... existing fields 1–8 unchanged (see the field-number note below) ...
  repeated TaskEvent events = N;            // added by this RFC; would need a contract change, which assigns N
}
```

**The field number is deliberately not assigned here.** Earlier drafts of this chapter wired
`events` onto `ActionStatusUpdate` as field `6`, on the stated premise that field 6 was unused.
That premise no longer holds: in `fleet_adapter.v1`, `ActionEndCause end_cause` takes 6,
`superseding_action_id` takes 7 and `retained` takes 8, and RFC-0001 governs where the two
documents differ. `TaskEvent`
itself is already defined in `fleet_adapter.v1` as a standalone message, so `compute/v1` can
reuse the identical concept; what is **not** wired is this field. Adding it needs a future contract
version, and its number is chosen in that version rather than asserted in advance here. See the note
beside `TaskEvent` in the `fleet_adapter.v1` proto.

**Why `subject_kind` exists in a revision that defines one kind.** `seq` is the idempotency key, and
its scope is the thing it is monotonic *per*. If this RFC publishes with that scope defined as
"per `action_id`" and a later revision needs events raised by an execution that is not a
`FleetAction`, there are two ways out and both are bad: overload `action_id` to carry a
non-action identity, or stand up a second event channel with its own sequence space and its own
replay rule. Reserving the discriminator before publication costs one field and one enum value. Retrofitting it
after publication costs a wire break. An adapter that does not set the field is correct: a receiver
MUST treat `EVENT_SUBJECT_KIND_UNSPECIFIED` as `ACTION`, and MUST reject a `subject_kind` it does not recognise rather than default it to `ACTION`.

**`terminal` is no longer reserved.** RFC-0001 core carries
`FleetAction.spec.executor.completionMode` (`RunToCompletion` | `Continuous`) for watch and
sensor actions that have no natural success. That field exists only on an action that sets `spec.executor`, which RFC-0001 dispatches to a `ComputeEndpoint` and never to a robot: admission rejects `spec.executor` together with `spec.robotSelector` or any robot constraint. `terminal` is therefore an executor's signal. A fleet adapter executes no `Continuous` run, and on a robot-executed action a receiver MUST ignore the flag. RFC-0001 records that field as declared and
validated for its enum but **read by no code at v0.4**: nothing defaults it from the referenced
endpoint's contract, and an implementation MUST NOT read an unset value as `RunToCompletion`.
The semantics below therefore rest on that field acquiring a reader, which is RFC-0001 core's
to deliver and not this RFC's. An
execution running `Continuous` has no other way to say "this is the observation I existed to make."
`terminal: true` is that signal: on a `Continuous` execution it requests the transition to a
terminal phase, and the control plane — never the adapter — performs it. On a `RunToCompletion`
execution the flag has no meaning and a receiver MUST ignore it. An execution MUST emit at most one `terminal: true` event. Defining the bit in this revision prevents a
later revision claiming it for something else; `Continuous` remains RFC-0001 core's to specify, and
this RFC adds no `completionMode` semantics beyond the meaning of this flag.

An adapter that never emits events is unaffected and still conforms to RFC-0001. A capability would advertise the event `type`s it can raise through a proposed addition to the `ScanCapabilities`/registration surface (`CapabilitiesSnapshot` and `SupportedAction` carry no event types in `fleet_adapter.v1`, and adding them would need a contract change), so that admission could validate a `TaskTrigger`'s `source.event` against declared events for the capability it targets.

<a id="design-details-event-sourcing-and-idempotency-ra-1-extended"></a>
### 6.2 Event sourcing and idempotency (RA-1 extended)

**A consistent `seq` scope.** `TaskEvent.seq` is monotonic **per `action_id`**, consistently, everywhere this RFC refers to it — the earlier draft said "per-action" in one place and implied "per-task" in another; there is only one correct answer, because events are raised in the context of one `FleetAction`'s execution, not a `FleetTask`'s. The control plane records events on `FleetAction.status.events[]` and processes each `(action_id, seq)` pair **at most once**. This is RA-1 discipline extended from status to events: the event log is written only on a discrete emitted event, never on a telemetry tick, and trigger evaluation is idempotent under replay.

<a id="design-details-event-sources-other-than-adapters"></a>
### 6.3 Event sources other than adapters

**Added in revision v0.2.** The revision this replaced assumed every semantic event originates in a robot
capability and arrives on `ActionStatusUpdate`. That assumption is wrong in a way that matters:
an action MAY be executed by an addressable endpoint rather than by a robot, and such an executor
raises domain events for exactly the same reasons a capability does. Left unspecified, each
implementation would invent its own sequence scope and its own attribution rule, and `seq` — the
idempotency key — would stop meaning one thing.

There are four source classes. All four produce the same `TaskEvent`, recorded the same way,
evaluated by the same rules.

| Source | How the event arrives | Who assigns `seq` |
|---|---|---|
| **Robot capability** | `ActionStatusUpdate.events[]` on the `ControlStream` | the **adapter**, monotonic per `action_id` |
| **Resident executor** (a long-running execution holding a stream) | its own status message on that stream | the **executor**, monotonic per `action_id` across runs — it is a persistent subject, exactly like an adapter |
| **Request/response executor** (one invocation, one reply) | the events array of its reply | the **control plane**, in reply order, continuing after the highest `seq` the action has accepted |
| **Control plane** (an event it raises itself, such as the discontinuity RFC-0001's `ComputeEndpoint` chapter requires when a pinned endpoint is replaced) | written by the control plane to the action's `status.events[]` | the **control plane**, above the highest `seq` the action has accepted |

The third row is the one that needs stating, because the obvious rule fails. A request/response
executor holds no state between invocations and therefore has nothing to be monotonic *with*;
requiring it to maintain a counter would produce implementations that reset it, and replay would
break silently rather than loudly. So the control plane assigns, and **deduplication rides on the
invocation rather than on the event**: an invocation carries an idempotency key, an executor MUST
return the same reply for a repeat, and the control plane recognises a repeated invocation as already
processed and records its events once. A retried call therefore fires a trigger exactly once.

The fourth row shares the action's sequence with the executor that runs it. When the control plane
replaces a pinned endpoint, it MUST record its own event before the replacement run's status stream
opens, so the resume point that run receives is above that event's `seq`.

> **Not met at v0.4.** The reference control plane assigns no `seq`. It raises no semantic event of
> its own, including the discontinuity RFC-0001's `ComputeEndpoint` chapter requires when a pinned
> endpoint is replaced, and it dispatches no request/response executor. Its one event writer,
> `ComputeEventIngestor`, records the `seq` a
> resident executor assigned and accepts an event only when that `seq` is greater than the action's
> highest accepted `seq`; no non-test code constructs it. No adapter assigns a `seq` either, because
> no field of `ActionStatusUpdate` carries a `TaskEvent`. The rows above are the target for an
> implementation that adds these sources.

> **Not met at v0.4 (a requeue to another adapter).** The highest accepted `seq` belongs to the action, not to the
> adapter that runs it, and nothing resets it or carries it to the next assignment. When an action is requeued to a
> robot served by another adapter, that adapter numbers the action's events from its own counter, and every event at
> or below the action's highest accepted `seq` is discarded as a replay. The first events of the new assignment can
> therefore be dropped without notice, and a trigger that waits on one of them does not fire for it. This contract
> version carries no resume point on the assignment.

<a id="design-details-attribution-a-producer-never-names-its-consumers"></a>
### 6.4 Attribution: a producer never names its consumers

An event is attributed to the **execution that raised it** — the `FleetAction` being executed,
whether by a robot or by an endpoint — and to nothing else. `subject_kind` remains `ACTION` for all
four source classes.

A producer MUST NOT be told where its events are consumed, and MUST NOT address a consumer. This is
not a stylistic preference:

- an executor that named its consumers would have to be reconfigured whenever a task changed, which
  makes a shared executor unshareable;
- it would let the producer choose what fires, which is authority a `TaskTrigger`'s authoring
  identity is supposed to hold;
- and a producer that addressed a specific action could raise an event against an execution it is not
  part of, which is the second-writer problem in a new place.

**Consumers subscribe by naming the producer.** A `TaskTrigger` names its `source`; a task's own
control flow names `{member, event}`. The direction of naming is always consumer → producer. An
executor's only obligation is to report what it observed, against the run it was given.

<a id="design-details-declaration-is-symmetric"></a>
### 6.5 Declaration is symmetric

A capability declares the event `type`s it can raise on the registration surface, and the control
plane rejects an undeclared type so it fires nothing ([§8](#conformance) E1.2). The same rule
applies to an endpoint: the resource describing it declares its event types, and an event whose type
was not declared MUST be rejected and MUST NOT fire a trigger. Without the symmetry, admission can
validate a trigger against a robot source and not against an endpoint source, and the weaker half
becomes the one everybody uses.

<a id="design-details-delivery-timing"></a>
### 6.6 Delivery timing

A resident executor emits events **as it observes them**, so a consumer waiting on one reacts while
the producer is still running. A request/response executor's events arrive **in its reply**, so they
do not exist until that execution completes.

The consequence is easy to get wrong: a loop that waits for an event from a request/response
execution waits for that execution to *finish and report it*, not for the moment the thing happened.
Both behaviours are correct and neither is a defect; the difference MUST be documented where the
sources are described, because a task written against one and moved to the other changes meaning
without changing shape.

<a id="design-details-bounded-event-history"></a>
### 6.7 Bounded event history

**Added in revision v0.2.** The revision this replaced said the control plane "records events on
`FleetAction.status.events[]`" and placed no bound on that list. That is a defect, and it is worth
stating precisely rather than quietly capping the field, because the failure it produces is the
worst class an orchestrator has.

A capability raising an event every few seconds — which is the ordinary case for a proximity or
detection capability, and the ordinary case for a `completionMode: Continuous` executor run — appends to a
single custom resource for as long as its execution runs. Once that object approaches the API
server's per-object size limit, **every write to it fails**, including the status write that would
record its cancellation and the `desiredState` write an operator would use to stop it. The control
plane loses the ability to control the execution *because* it was observing it carefully. Failure is
proportional to how much the deployment uses the feature, so it does not appear in a demonstration.

Therefore:

- The control plane MUST record at most `SwarmadaConfig.spec.events.maxRecordedEvents` entries in
  `status.events[]`, retaining the most recent, and MUST discard older entries rather than the
  write. This revision specifies neither the value's admission ceiling nor its default; the default is meant to be small.
- `status.lastEventSeq` and `status.observedEventCount` are scalars and MUST carry the complete
  history: the count is authoritative even where the list is not. A consumer detecting
  `observedEventCount` greater than the retained entries knows exactly that it is reading a window.
- The **audit log and the telemetry sink are the system of record for events**, not the custom
  resource. `SwarmadaConfig.spec.telemetry.sink` already carries the rule this follows —
  high-cadence data goes to the external store, never to etcd — and events are high-cadence data
  the moment a capability is doing its job.
- Trigger evaluation MUST NOT depend on an event still being present in `status.events[]`. Evaluation
  happens as each `(subject, seq)` is processed, at most once, on arrival; the recorded list is an
  operator-facing window, not the evaluation input. An implementation that evaluates by re-reading
  the list will silently stop firing once the window rolls, and MUST NOT be written that way.

This is RA-1 applied to events with the same reasoning that applies it to status: the control plane
records a discrete transition, and it does not accumulate observations in an object it must keep
writable in order to stay in control.

The rule above binds trigger evaluation. The shipped `FleetTask` flow evaluator does read the
window, for the event arms of `select` and `loop.until` (`flow.ChildState`), and reads
`eventsDropped` beside it to tell an evicted event from one never raised. Nothing writes the window
in a running manager ([§1](#abstract)).

<a id="design-details-the-tasktrigger-resource"></a>
### 6.8 The `TaskTrigger` resource

```yaml
apiVersion: swarmada.io/v1
kind: TaskTrigger
metadata: { name: intruder-response, namespace: yard-west }
spec:
  source:                                # what to watch
    fleetTaskRef: perimeter-sweep        # or a label selector over actions
    subjectKind: Action                  # OPTIONAL, defaults to Action — see below
    event: IntruderDetected              # semantic event type
  condition: >                            # OPTIONAL — see condition-scoping below; omit to match unconditionally
    action.robot.zone == "yard-west" && action.robot.adapterName.startsWith("vda5050")
  response:
    - draftTask:                         # creates a FleetTask held at phase Draft; a person activates it
        taskTemplate:                    # drafted as written: nothing is taken from the event payload
          actions:
            - name: investigate
              action:
                type: Navigate
                zone: yard-west
                payload:
                  # {"x": 3.0, "y": 2.0}, a point the trigger's author chose, not the event's
                  raw: eyJ4IjogMy4wLCAieSI6IDIuMH0=
```

**A trigger only drafts.** An event creates a draft, and a person activates it. Until a mechanism bounds a trigger's authority by the identity that authored it ([§8](#conformance), E7.4), `draftTask` is the only response an implementation MAY execute. It creates a `FleetTask` with `spec.draft: true`, held at phase `Draft` until its flag is cleared, which, where the admission webhooks are served, only a person holding RFC-0001's `activate` verb can do, as [§7.3](#resources-tasktrigger) specifies for the shipped mechanism. An implementation MUST NOT execute an `appendAction` or a `setDesiredState` response, with or without `requireApproval`. `setDesiredState` is specified below, and an `appendAction` response grows a `FleetTask` only through RFC-0001's append-only path and the `append` verb ([§8](#conformance), E7.2), so that the rules of both are fixed before any mechanism enables them.

`source.subjectKind` mirrors `TaskEvent.subject_kind` and defaults to `Action`, the only value this
RFC defines. It is present for the same forward-compatibility reason: a trigger written against this revision
selects an execution kind explicitly, so a later kind widens the enum instead of changing what an
existing trigger silently matches. Admission MUST reject a `subjectKind` it does not recognise.

> **Unresolved in this revision (two `TaskTrigger` shapes).** This section and
> [§7.3](#resources-tasktrigger) give `TaskTrigger.spec` two shapes. Here a trigger watches a running
> execution's wire event (`source.fleetTaskRef`, `source.event`, `source.subjectKind`) and lists its
> responses in `response[]`, `draftTask` among them. The served kind watches a `SemanticEventSource`
> (`source.semanticEventSource`, `source.eventType`), carries one top-level `taskTemplate` and
> `suspend`, and has no `response` and no `subjectKind`, so the admission rule above names a field the
> served kind does not have. Both cannot be the one `swarmada.io/v1` `TaskTrigger`, and the table of
> omissions in [§7](#resources) does not list these differences. Draft 5 resolves which shape the
> kind takes; this revision does not choose.

The controller that reconciles `TaskTrigger` runs entirely control-plane-side; it never contacts a robot. Once responses other than `draftTask` are enabled, it uses the RFC-0001 append-only path (and therefore the `append` RBAC verb) to grow a `FleetTask`, and the RFC-0001 `desiredState` surface to redirect. A trigger's response is bounded to these declarative verbs — never an imperative robot command.

<a id="design-details-ownership-what-setdesiredstate-may-target"></a>
### 6.9 Ownership: what `setDesiredState` may target

**A corrected `desiredState` target.** The withdrawn draft's flagship example set `desiredState` directly on `patrol`, a member action owned by the `perimeter-sweep` `FleetTask`. RFC-0001 is explicit that `desiredState` is operator- and trigger-settable on a *standalone* action; for an action owned by a `FleetTask`, the composite is authoritative and reconciles it to the task's own `desiredState`, and the composite controller creates, adopts and prunes the actions it owns. The `FleetAction` validating webhook admits a change to such an action's `spec` only from the manager's principal, lets a holder of `cancel` on `fleetactions` also write its `spec.desiredState` or its cancel and requeue annotations, and refuses every other principal (RFC-0001, *Authorization — Swarmada RBAC Roles*). A `TaskTrigger` writing a member's `desiredState` directly is therefore a second writer that admission does not tell from the first: its controller runs in the manager, under the principal that rule admits, so the write is admitted, and the composite does not reliably undo it. Its next reconcile puts a member that has not finished back to the task's own `desiredState`, except that a flow task never lifts a member's `Cancelled`; and a task without `spec.flow` stops writing to a member once its cancel has taken effect. A `Cancelled` write can therefore stick, and the member is cancelled for good.

This RFC states the rule explicitly, as a MUST: a `TaskTrigger` response's `setDesiredState.target` MUST name either **(a)** a standalone `FleetAction` not owned by any `FleetTask`, or **(b)** the `fleetTask` itself — its own `desiredState` field, which the composite legitimately reconciles onto its members per RFC-0001's existing ownership model. A response MUST NOT name a task-owned member action directly; an admission webhook that validates `TaskTrigger` on create and update MUST enforce this, as it validates any other cross-reference ([§6.12](#design-details-verification-of-referenced-wire-messages-and-fields) records that none is served). Once a `setDesiredState` response is enabled, a `Returning` response targets the owning `FleetTask`, not the `patrol` member.

<a id="design-details-condition-scoping-vendor-zone-and-robot"></a>
### 6.10 Condition scoping — vendor, zone, and robot

A `TaskTrigger.spec.condition` is a CEL expression, evaluated at admission for syntax and at each firing for the actual match (a `condition` that errors at evaluation MUST be treated as not matching, and the error MUST be surfaced as a status condition and an event) — the same validation technology RFC-0001 already uses for cross-field `SwarmadaConfig` rules, not a new dependency. The expression evaluates against a fixed, documented context, never the full object graph:

| Context field | Type | Source |
|---|---|---|
| `event.type` | string | The firing `TaskEvent.type` |
| `event.payload` | struct | The firing `TaskEvent.payload` |
| `action.robot.name` | string | The `Robot` bound to the firing `FleetAction` |
| `action.robot.zone` | string | That robot's current `FleetZone` |
| `action.robot.adapterName` | string | The `FleetAdapter` the robot's binding resolves to |
| `action.robot.manufacturer` | string | `Robot.spec.manufacturer` |

This is deliberately a closed set, not an open query surface — the same "bounded, declarative, statically analyzable" discipline the response side already holds to ([§4](#non-goals)). A trigger scoped to one vendor's robots in one zone is `condition: action.robot.adapterName == "vda5050-acme" && action.robot.zone == "line-3"`; nothing here requires per-vendor trigger duplication.

<a id="design-details-the-approval-gate"></a>
### 6.11 The approval gate

**Not present in the withdrawn draft.** A response entry MAY set `requireApproval: true`. In this revision only a `draftTask` response is executed ([§6.8](#design-details-the-tasktrigger-resource)), so only a `draftTask` response is staged and redeemed; an `appendAction` or `setDesiredState` response is neither staged nor executed, with or without the flag. The gate is specified for every response so that its rules are fixed before a mechanism enables the others. When a response sets the flag, a matching firing does not execute the response — it computes the response in full (which action, which template, which target) and records it as one entry in `TaskTrigger.status.pendingResponses[]`:

```yaml
status:
  pendingResponses:
    - id: "pr-2026-08-19T14:03:00Z-000"
      firedFrom: { event: IntruderDetected, seq: 41, actionRef: "perimeter-sweep/patrol" }
      computedResponse: { draftTask: { taskTemplate: { actions: [ { name: investigate, action: { type: Navigate, zone: yard-west } } ] } } }
      stagedAt: "2026-08-19T14:03:00Z"
      expiresAt: "2026-08-19T14:18:00Z"     # bounded by spec.approvalTimeoutSeconds
      state: Pending                         # Pending | Approved | Rejected | Expired
```

An event, `TriggerResponsePending`, fires on staging — the notification mechanism, if any, is operator tooling ([§4](#non-goals) item 6). Two added custom verbs, `trigger-response-approve` and `trigger-response-reject`, are separately grantable, MUST NOT appear in any built-in role, and are refused at admission to every ServiceAccount, whatever its roles grant, as RFC-0001 refuses `activate` (RFC-0001, *Authorization — Swarmada RBAC Roles*). As in RFC-0001, automation that authenticates as a user, or impersonates one, is admitted like that user. A principal that is not a ServiceAccount and holds `trigger-response-approve` redeems a specific `pendingResponses[].id`; the controller then executes **exactly the staged `computedResponse`**, never a recomputation — closing the drift between what was approved and what runs. A reject, or an unredeemed entry past `expiresAt`, moves the entry to `Rejected`/`Expired` and it is never executed. `spec.approvalTimeoutSeconds` is REQUIRED whenever any response in the trigger sets `requireApproval: true`, validated at admission with a configured maximum, whose setting this revision does not name, so a pending response cannot wait forever.

Every transition — `TriggerResponsePending`, `TriggerResponseApproved`, `TriggerResponseRejected`, `TriggerResponseExpired` — is an audited event carrying the RFC-0001 Audit Logging required fields plus, for `Approved`, the confirming human identity and the `pendingResponses[].id` redeemed. The audit entry is written **before** the response executes (record-before-effect): if the audit write cannot complete, the response does not run.

This RFC does not define who should hold `trigger-response-approve` — that is an operator RBAC decision. Unlike RFC-0001's custom verbs, which ship in its built-in roles, this RFC keeps `trigger-response-approve` out of every shipped role. A deployment that wants a "service engineer" role binds that verb to whatever principal it names that; the standard defines the mechanism, not the org chart.

<a id="design-details-verification-of-referenced-wire-messages-and-fields"></a>
### 6.12 Verification of referenced wire messages and fields

Every wire message, field, resource, verb and constant this document names was checked against
the `fleet_adapter.v1` proto, the API types, the generated CRDs and the RBAC configuration.
The result is recorded rather than asserted, because most of it is deliberately absent — and because
the revision this replaced was withdrawn for citing a message that did not exist.

| Referenced item | Present? | Where |
| :---- | :---- | :---- |
| `ActionStatusUpdate`, with `action_id=1 … retained=8` | yes | in `fleet_adapter.v1`; the real message the withdrawn draft's `TaskStatusUpdate` did not name |
| `TaskEvent` message | yes | in `fleet_adapter.v1`, defined ahead of this RFC so `compute/v1` can reuse one event shape |
| `EventSubjectKind` enum, with `UNSPECIFIED` and `ACTION` | yes | in `fleet_adapter.v1` (`UNSPECIFIED = 0`, `ACTION = 1`) |
| `events` field on `ActionStatusUpdate` | **no** | not wired; adding it would need a contract change, which assigns the field number; it is not asserted here ([§6.1](#design-details-adapter-channel-the-additive-taskevent)) |
| `ScanCapabilities` / registration declaration of event types | partial | the message exists in `fleet_adapter.v1`, carried in the `Command.command` oneof as `scan`; no controller constructs or sends it at v0.4 |
| `ComputeEndpoint.spec.emitsEvents` (endpoint-side event declaration) | yes | `ComputeEndpointSpec.EmitsEvents`, in the Go type and the generated CRD. Read when a `FleetTask` is created and when an update changes its `flow`, `inputs` or `actions`: the binding webhook (`validateProducerContracts`) refuses, with reason `UndeclaredEvent`, an event binding naming a type the producer's endpoint does not list. Nothing checks a raised event against it, so the rejection of an undeclared `type` at ingest that `E10.1`/`CE6.1` require is unbuilt |
| `TaskTrigger` Go type | yes | in the `swarmada.io/v1` API types |
| `TaskTrigger` generated CRD | yes | generated and served, and reachable as `swarmctl get tasktriggers` |
| `TaskTrigger` controller | partial | a controller the manager runs. It reconciles `SemanticEvent`, not `TaskTrigger`, and its only output is a draft `FleetTask` — narrower than [§5.2](#proposal-the-tasktrigger-binding-resource) specifies ([§6.13](#design-details-what-the-reference-implementation-provides)) |
| `TaskTrigger` admission webhook | **no** | no admission webhook serves the kind, so the ownership and `subjectKind` validation [§6.9](#design-details-ownership-what-setdesiredstate-may-target) requires is unenforced |
| `SemanticEvent`, `SemanticEventSource` kinds | yes | both have Go types and generated CRDs, and are the pair the controller above reconciles against |
| `FleetAction.status.events[]`, `status.lastEventSeq`, `status.observedEventCount` | partial | `status.events[]` exists (`FleetActionStatus.Events`, in the Go type and the generated CRD), bounded at 32 by the CRD's `MaxItems` rather than by `maxRecordedEvents`. Neither scalar exists; `status.eventSeqHigh` (highest `seq` accepted) and `status.eventsDropped` (entries evicted) stand in their place. The `FleetTask` controller reads the list (`flow.ChildState`, `resolveOneMemberBinding`). Its one writer, `ComputeEventIngestor`, is constructed by no non-test code, so nothing populates it at v0.4 |
| `SwarmadaConfig.spec.events.maxRecordedEvents` | **no** | `SwarmadaConfig.spec` carries no `events` block, so the MUST in [§6.7](#design-details-bounded-event-history) names a field that does not exist |
| `trigger-response-approve`, `trigger-response-reject` verbs | **no** | in no role and in no RBAC manifest |
| `TriggerResponsePending` / `Approved` / `Rejected` / `Expired` | **no** | none exists as a constant and none has a writer |
| `FleetAction.spec.executor.completionMode` (`RunToCompletion` \| `Continuous`) | partial | declared and enum-validated; **read by no code outside its API type**, which is RFC-0001 core's to deliver |
| Approval-gate properties (single-use, scoped, expiring, record-before-effect) | **no** | not implemented; this RFC states the properties itself, and they are not a shipped mechanism |
| `E0`–`E4` as a runnable profile | **no** | reserved: the identifiers are held, no `event-source` profile is declared, and no report can present an `event-source` level against them. `E0.1` and `E0.2` are also listed in the `trigger-consumer` profile, which no report grades. `E10.1`–`E10.6` are excluded there too and restated as `CE6.1`–`CE6.6` on the `ComputeEndpoint` surface |

Two rows are worth stating plainly rather than leaving to the table. **The one thing an adapter could
carry, it cannot**: `TaskEvent` and `EventSubjectKind` are on the wire, but nothing references
them from `ActionStatusUpdate`, so no adapter can emit a semantic event at v0.4. And the conformance
identifiers are reserved rather than runnable: the suite declares no `event-source` profile, so no
report carries a row for it, and a reader MUST NOT expect one — there is nothing that could
produce a result against `E0`–`E4`.
<a id="design-details-what-the-reference-implementation-provides"></a>
### 6.13 What the reference implementation provides

The reference implementation ships a **narrower mechanism than this document specifies**, and the
difference is recorded here rather than left for a reader to infer from the table above. The design
in [§5.2](#proposal-the-tasktrigger-binding-resource) and
[§6.8](#design-details-the-tasktrigger-resource) is this RFC's, and is unchanged by what ships at
this revision; `docs/adr/0062-a-semantic-event-produces-a-draft-fleettask.md` records the reasoning for the
divergence.

| This RFC specifies | The reference implementation (RFC-0001 v0.4) |
| :---- | :---- |
| A response appends an action to a live `FleetTask` through the RFC-0001 append-only path, and therefore holds the `append` verb | No append. The controller's RBAC on `fleettasks` is `get;list;watch;create`; its entire output is a **new** `FleetTask` carrying `spec.draft: true`, which the `FleetTask` controller holds at phase `Draft` and generates no children for |
| A response MAY set `desiredState` on a standalone `FleetAction` or on the owning `FleetTask` ([§6.9](#design-details-ownership-what-setdesiredstate-may-target)) | No `desiredState` write of any kind. The ownership rule in [§6.9](#design-details-ownership-what-setdesiredstate-may-target) is therefore not exercised, and remains unenforced rather than satisfied |
| A response MAY be marked `requireApproval`, staged in `TaskTrigger.status.pendingResponses[]` and redeemed by id ([§6.11](#design-details-the-approval-gate)) | No staging. `status.pendingResponses[]` has no writer, and neither approval verb is registered. The draft flag is the gate instead: the `FleetTask` object *is* the staged artefact, and a person clearing `spec.draft` runs what is on the object |
| The controller reconciles `TaskTrigger` | The controller reconciles `SemanticEvent`, evaluating each event against every trigger in its namespace. A trigger is standing configuration; the event is what happens |
| A response is bounded by the authoring identity's own permissions (E7.4) | Not bounded. The controller creates each draft as its own ServiceAccount, the trigger author's permission on `fleettasks` is not consulted, and the draft records the trigger's name, not its author. The draft gate bounds it instead: where the admission webhooks are served, a person holding `activate` reviews the task before it runs |

Two consequences a reader MUST NOT miss. **Nothing on this path dispatches a robot**: every output
is a held `FleetTask`, so no part of the shipped mechanism can put a machine in motion until the
flag is cleared. And **the draft gate is not a substitute for the approval verb this
document specifies.** Clearing `spec.draft` needs the RFC-0001 `activate` verb on `fleettasks`, and
the `FleetTask` admission webhook refuses it to every ServiceAccount whatever its roles grant, so
where the webhooks are served the flag is an authorization boundary that a person holds. It gates
a whole drafted task; `trigger-response-approve` ([§6.11](#design-details-the-approval-gate))
redeems one staged response, and is not built. On an install that serves no admission webhooks the
`activate` check does not run, and there an operator MUST NOT treat the draft flag as an
authorization boundary.
<a id="resources"></a>
## 7. Resource Schemas

The `swarmada.io/v1` API group serves three kinds that belong to this RFC and not to RFC-0001:
`SemanticEventSource`, `SemanticEvent` and `TaskTrigger`. RFC-0001 specifies eighteen CRDs and
names these three as outside its scope; this chapter is where they are specified, and this RFC is
their owner.

**This chapter describes what ships, not what [§5](#proposal) proposes.** The rest of this
document specifies a mechanism in which an adapter raises a wire `TaskEvent` against a running
execution, a `TaskTrigger` narrows it with a CEL `condition` and, once a mechanism bounds a trigger's authority by its author, appends the response to a live `FleetTask` behind an approval verb; until then a trigger only drafts ([§6.8](#design-details-the-tasktrigger-resource)). The three
resources below are a deliberately narrower mechanism that ships, and
[§6.13](#design-details-what-the-reference-implementation-provides) states the difference clause by
clause. Where this chapter and the rest of this document disagree about what an implementation
does, **this chapter governs, because it is written from the shipped types**; where they disagree
about what an implementation SHOULD eventually do, the rest of the document governs.

One consequence a reader MUST NOT miss before reading any schema below: **nothing on this path
dispatches a robot.** Every output is a `FleetTask` held at phase `Draft`. Where the admission
webhooks are served, only a person holding `activate` can clear the flag; on an install that
serves none, any principal with `update` or `patch` on `fleettasks` can, and the flag is not an authorization
boundary ([§6.13](#design-details-what-the-reference-implementation-provides)). The schemas carry no
field that changes that.

> **Maturity (revision v0.2, Swarmada v0.4):** stage: alpha · control-plane: partial · end-to-end: not exercised.
> All three kinds have Go types, generated CRDs and `swarmctl` resource registrations, and the
> manager runs one controller for them.
> **There is no admission webhook for any of the three**, so every rule this chapter states as
> enforced, other than the structural validation of the generated CRD schemas, is enforced at reconcile time, after the object has been admitted — including the
> closed event-type set, whose violation is recorded on the object rather than refused at the API
> server.
> No built-in role grants any verb on the three kinds (RFC-0001, *Role Definitions*). `create` on
> `tasktriggers` lets a principal have the control plane create held `FleetTask`s of any spec in the
> namespace, under the controller's identity; the draft names the trigger, not its author.
> **The `TaskTrigger` controller writes `SemanticEventSource.status.acceptedCount` and `rejectedCount`; nothing writes `conditions`.** After the controller accepts or rejects an event, it recounts the evaluated events in the namespace that name the source and writes both counts. The counts cover the events that exist, so deleting an event lowers them at the next recount. Their only reader is `swarmctl`'s display.
> An operator
> MUST NOT read a zero there as evidence that nothing arrived.

<a id="resources-semanticeventsource"></a>
### 7.1 `SemanticEventSource`

A `SemanticEventSource` declares a source of semantic events that is **not** a fleet adapter, and
the closed set of event types it may raise.

The kind exists because every source class [§6.3](#design-details-event-sources-other-than-adapters)
defines is execution-bound: each raises its events against a `FleetAction` that is already running,
and each needs a vendor adapter or an executor endpoint behind it. None of them can raise an event
when nothing is running, which is the case a trigger is most useful for — a door opened, a shift
started, a work order arrived. A `SemanticEventSource` is the adapter-free ingress for those:
anything that can reach the API server creates a `SemanticEvent` naming the source, and the
controller treats it exactly as it would any other event.

**`spec.source` is a claim, not an identity.** No admission rule binds a source to a principal:
`create` on `semanticevents` lets a principal raise any declared type of any source in the
namespace, and so fire that source's triggers (RFC-0001, *Authorization — Swarmada RBAC Roles*).
RBAC cannot limit `create` by object name. A deployment that gives a source its own identity holds
that identity to that source with an admission policy, for example a ValidatingAdmissionPolicy that
matches the identity's username and requires `spec.source` to name its source. The event records no
producer identity; the API server's audit log is the only record of who created it.

**It is not this document's wire `TaskEvent`, and the difference is not cosmetic.** A `TaskEvent`
([§6.1](#design-details-adapter-channel-the-additive-taskevent)) is a protobuf message defined in
`fleet_adapter.v1`, attributed to an execution and sequenced by its producer, or by the control plane for a
request/response executor ([§8](#conformance), E10.3); no field of `ActionStatusUpdate` carries it, and at v0.4
only `compute.v1`'s `StreamStatus` does. A `SemanticEvent` is
a Kubernetes object with no execution behind it and no wire sequence, so the `seq` idempotency rule
in [§6.2](#design-details-event-sourcing-and-idempotency-ra-1-extended) does not apply to it; its
own once-only rule is stated below.

<a id="resources-schema"></a>
#### 7.1.1 Schema

```yaml
# +kubebuilder:object:root=true
# +kubebuilder:subresource:status
# +kubebuilder:resource:shortName=ses
# +kubebuilder:printcolumn:name="Types",type=string,JSONPath=`.spec.eventTypes`
# +kubebuilder:printcolumn:name="Accepted",type=integer,JSONPath=`.status.acceptedCount`
# +kubebuilder:printcolumn:name="Rejected",type=integer,JSONPath=`.status.rejectedCount`
# +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
apiVersion: swarmada.io/v1
kind: SemanticEventSource
metadata:
  name: dock-door-north
  namespace: yard-west
spec:
  # Human-readable label. Optional; carries no behaviour.
  displayName: "North dock door sensor bridge"
  # The CLOSED set of event types this source may raise. Required, at least one
  # entry, and a set rather than a list.
  # +kubebuilder:validation:Required
  # +kubebuilder:validation:MinItems=1
  # +listType=set
  eventTypes:
    - DoorOpened
    - DoorForced
status:
  # Derived by the reference controller from the evaluated events; see the Maturity note above.
  # +kubebuilder:validation:Minimum=0
  acceptedCount: 0
  # +kubebuilder:validation:Minimum=0
  rejectedCount: 0
  # +listType=map
  # +listMapKey=type
  conditions: []
```

<a id="resources-normative-requirements"></a>
#### 7.1.2 Normative requirements

1. `spec.eventTypes` MUST carry at least one entry, and the set is **closed**: an implementation
   MUST NOT fire a trigger from an event whose `spec.type` the named source does not list. It
   is the rule [§8](#conformance)'s `E10.1` states for an endpoint, applied to a
   `SemanticEventSource`; no check grades it on this path, and the reason it is a declaration rather than an
   inference is that without one a typo in an event type and a type nobody has written a trigger
   for yet are indistinguishable — both do nothing, and a silent no-op is the worst outcome
   for a mechanism whose whole job is to notice that something happened.
2. An implementation MUST record the refusal on the event rather than discarding it
   ([§7.2](#resources-semanticevent), `status.phase: Rejected`).
3. `status.acceptedCount` and `status.rejectedCount` are **counters an implementation MAY write**.
   The reference implementation writes both. A conforming implementation MUST NOT be read as
   non-conforming for leaving them unset, and a reader MUST NOT treat an unset counter as a count
   of zero.

> **No check at v0.4.** No conformance row grades these rules. The CRD schema enforces the
> one-entry minimum of rule 1 at admission. In the reference implementation an event whose source
> does not declare its type, or whose source does not exist, ends `Rejected` and drafts nothing, and
> the controller writes the counters of rule 3.

<a id="resources-semanticevent"></a>
### 7.2 `SemanticEvent`

A `SemanticEvent` is one occurrence: this source says this happened. It is a record of a moment,
not desired state, and that distinction drives its whole lifecycle — it is evaluated once and never
re-converged on. The shipped schema does not fix the record: the CRD carries no immutability rule,
so `spec` stays editable after evaluation, and `payload` is limited to 16 entries but not in the
length of each value.

<a id="resources-schema-2"></a>
#### 7.2.1 Schema

```yaml
# +kubebuilder:object:root=true
# +kubebuilder:subresource:status
# +kubebuilder:resource:shortName=sev
# +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
# +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
# +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
# +kubebuilder:printcolumn:name="Drafted",type=string,JSONPath=`.status.draftedTasks`
# +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
apiVersion: swarmada.io/v1
kind: SemanticEvent
metadata:
  name: door-north-0091
  namespace: yard-west
spec:
  # The SemanticEventSource, in THIS namespace, that raised it.
  # +kubebuilder:validation:Required
  # +kubebuilder:validation:MinLength=1
  # +kubebuilder:validation:MaxLength=253
  source: dock-door-north
  # The semantic event type. MUST be one the source declared.
  # +kubebuilder:validation:Required
  # +kubebuilder:validation:MinLength=1
  type: DoorOpened
  # The event's data, as FLAT STRING key/values and bounded. Scalars only, for the
  # reason ActionResult.values is: a payload is something a trigger reads and an
  # operator scans, not a data channel, and this object lives in etcd.
  # +kubebuilder:validation:MaxProperties=16
  payload:
    door: north
    heldOpenSeconds: "41"
  # When the thing HAPPENED, as distinct from when the object was created. A bridge
  # replaying a backlog sets it. NOTHING DEFAULTS IT, because a defaulted timestamp
  # would assert a time nobody observed.
  occurredAt: "2026-09-23T08:14:02Z"
status:
  # +kubebuilder:validation:Enum=Pending;Accepted;Rejected
  phase: Accepted
  # Why. The whole diagnostic surface for a rejected event.
  message: "accepted; drafted 1 task(s) AWAITING ACTIVATION"
  # The FleetTasks this event caused to be DRAFTED. Written once: a listed task may since have been activated.
  # +listType=set
  draftedTasks:
    - shut-door-crew-door-north-0091
  observedGeneration: 1
```

<a id="resources-lifecycle-and-normative-requirements"></a>
#### 7.2.2 Lifecycle and normative requirements

1. **`status.phase` is evaluated exactly once.** `Accepted` and `Rejected` are terminal: an
   implementation MUST NOT re-evaluate an event already in either phase. An event is a record that
   something happened at a moment, not desired state to be converged on, and re-evaluating one on
   every resync would draft a fresh task each time a controller restarted.
2. **An event naming a source that does not exist, or a type that source does not declare, MUST be
   `Rejected` and MUST fire nothing** (the rule [§8](#conformance)'s `E10.1` states for an endpoint). `status.message` MUST name which of the two it
   was and, for an undeclared type, the set the source does declare. An implementation SHOULD emit
   a Warning Kubernetes Event; the reference implementation emits reason `SemanticEventRejected`.
3. **Both rules are enforced after admission, not at it.** There is no validating webhook, so an
   event naming an undeclared type is accepted by the API server and refused by the controller. An
   implementation MUST NOT present admission success as evidence that an event will fire anything.
4. **`status.draftedTasks` names what the event caused and is the idempotency record.** It is written
   once, when the event is accepted, and not updated afterwards: a listed task may since have been activated,
   dispatched or deleted. Whether it still waits for a person is read from the `FleetTask` (`spec.draft` and
   `status.phase`).
5. `spec.occurredAt` is OPTIONAL and MUST NOT be defaulted. An implementation that needs an
   ordering key in its absence MUST use `metadata.creationTimestamp` and MUST NOT write the
   inferred value back to `spec.occurredAt`.

> **No check at v0.4.** No conformance row grades these rules. In the reference implementation an
> event naming a missing source or an undeclared type ends `Rejected`, with a message, and drafts
> nothing, and a repeated reconcile drafts no second task, which the deterministic draft name of the
> `TaskTrigger` rules also ensures. Rule 5 holds in the reference implementation because no
> controller defaults, reads or writes `spec.occurredAt`; `swarmctl` only displays it. Rule 3 binds
> what an implementation presents.

<a id="resources-tasktrigger"></a>
### 7.3 `TaskTrigger`

A `TaskTrigger` binds one event type from one source to a `FleetTask` template. When it fires it
**drafts** a task and stops there.

**That it does not dispatch is the design, not timidity about automation.** A semantic event is
evidence that something happened, produced by a sensor, a bridge or a model — none of which is
accountable for what a robot then does in a space people are standing in. Auto-dispatch would put
a physical-world decision behind whatever produced the event, and the failure mode is not a wrong
API call but a machine moving because a door sensor glitched. The draft does the tedious part —
composing a task that is valid against the real schema — and stops where judgement begins.

<a id="resources-schema-3"></a>
#### 7.3.1 Schema

```yaml
# +kubebuilder:object:root=true
# +kubebuilder:subresource:status
# +kubebuilder:resource:shortName=tt
# +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.semanticEventSource`
# +kubebuilder:printcolumn:name="Event",type=string,JSONPath=`.spec.source.eventType`
# +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
# +kubebuilder:printcolumn:name="Drafts",type=integer,JSONPath=`.status.draftCount`
# +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
apiVersion: swarmada.io/v1
kind: TaskTrigger
metadata:
  name: shut-door-crew
  namespace: yard-west
spec:
  # +kubebuilder:validation:Required
  source:
    # The SemanticEventSource, in this namespace, to watch.
    # +kubebuilder:validation:Required
    # +kubebuilder:validation:MinLength=1
    # +kubebuilder:validation:MaxLength=253
    semanticEventSource: dock-door-north
    # ONE type per trigger, deliberately: a trigger matching a SET of types would
    # have to decide which template each produced, and the answer is a second
    # trigger.
    # +kubebuilder:validation:Required
    # +kubebuilder:validation:MinLength=1
    eventType: DoorForced
  # The FleetTask spec to draft. It is drafted AS WRITTEN — no substitution from the
  # event payload; see the normative requirements below.
  # +kubebuilder:validation:Required
  taskTemplate:
    # A FleetTask spec, exactly as RFC-0001 specifies it: at least one action.
    actions:
      - name: check-door
        action: { type: Navigate, zone: zone-dock-north }
  # Stops this trigger firing without deleting it. A suspended trigger records nothing:
  # an event it would have matched is accepted as "no TaskTrigger matched", the same as
  # when no trigger exists. `spec.suspend` is what tells the two apart.
  # +kubebuilder:default=false
  suspend: false
status:
  lastFiredAt: "2026-09-23T08:14:03Z"
  lastEventName: door-north-0091
  # +kubebuilder:validation:Minimum=0
  draftCount: 7
  # The drafted FleetTasks still awaiting a human: the review queue for whoever holds activate.
  # A draft leaves it when it is cleared or deleted — see the requirements below.
  # +listType=set
  pendingDrafts:
    - shut-door-crew-door-north-0091
  # +listType=map
  # +listMapKey=type
  conditions: []
  observedGeneration: 3
```

<a id="resources-firing-and-what-a-firing-may-produce"></a>
#### 7.3.2 Firing, and what a firing may produce

1. **The controller reconciles `SemanticEvent`, not `TaskTrigger`.** A trigger is standing configuration; the
   event is what happens. An implementation MUST evaluate an accepted event against every trigger
   in the event's namespace.
2. **A trigger matches when, and only when, `spec.source.semanticEventSource` equals the event's
   `spec.source` and `spec.source.eventType` equals its `spec.type`.** Both comparisons are exact
   string equality; no pattern, prefix or label selector is defined.
3. **A suspended trigger MUST NOT match**, and the event MUST still be evaluated against the
   other triggers and MUST still record its outcome. The outcome does not name a suspended
   trigger it skipped: an event only a suspended trigger would have matched is recorded as
   accepted with no trigger matched, exactly as if that trigger did not exist. An operator tells
   "switched off" from "nothing configured" by the trigger's `spec.suspend`, which the
   `Suspended` printer column shows.
4. **A firing MUST produce a `FleetTask` with `spec.draft: true`, and MUST set that field itself
   rather than trusting the template.** A template that omitted the flag — or set it false — would
   auto-dispatch, which is the one outcome this mechanism exists to prevent.
5. **The draft's name MUST be a deterministic function of the trigger and the event.** The
   reference implementation uses `<trigger>-<event>`. Determinism is what makes a retry after a
   lost status write collide on create rather than draft a second copy; an implementation MUST
   treat an already-exists collision on that name as success when the existing `FleetTask` is
   controlled by that event. A collision with any other `FleetTask` MUST NOT be recorded in the
   event's `status.draftedTasks` or counted as a firing, and the event's `status.message` MUST name
   the conflicting task.
6. **While it is a draft, the draft MUST be owned by the EVENT, not by the trigger.** The draft is
   the consequence of one occurrence: deleting a standing trigger MUST NOT delete drafts a person is
   still reviewing, and deleting the event that caused one is a reasonable way to withdraw it.
   Activation MUST end that ownership: the event's owner reference MUST be removed when `spec.draft`
   is cleared, so deleting an event never deletes or cancels activated work. **Not met by the
   reference implementation:** the owner reference stays after activation. Where the admission
   webhooks are served, the `FleetTask` delete guard refuses the cascade while the task is `Running`
   or `Compensating`; an activated task that is `Pending` or settled is deleted with its event. On an install that
   serves no admission webhooks, nothing refuses the cascade, whatever the task's phase. An
   implementation that does end the ownership on activation meets an edge of rule 5: a retry after
   a lost status write that finds its own draft already activated finds it controlled by no event,
   and records it as a conflict.
7. **A firing SHOULD label the draft with its provenance.** The reference implementation writes
   `swarmada.io/drafted-by`, `swarmada.io/drafted-from` and `swarmada.io/drafted-event`, and an
   annotation `swarmada.io/draft-origin` naming the event, its type and the trigger in one
   sentence. A trigger name, event name or event type that is not a valid label value makes the
   create fail, and nothing is drafted (RFC-0001 FleetTask, *Features outside this chapter*).
8. The drafted task is held by the `FleetTask` controller at phase `Draft` with condition
   `AwaitingActivation` True. Clearing `spec.draft` activates it, and where the admission
   webhooks are served that write is an authorization boundary: it needs the RFC-0001 `activate`
   verb on `fleettasks`, and a ServiceAccount is refused it whatever its roles grant. The
   `trigger-response-approve` verb [§6.11](#design-details-the-approval-gate) specifies is a
   different gate, on a staged response rather than a drafted task, and is not built.

> **No check at v0.4.** No conformance row grades these rules.

<a id="resources-what-these-schemas-deliberately-do-not-carry"></a>
#### 7.3.3 What these schemas deliberately do not carry

Each omission below is a decision with a reason, not an unfinished field. A reader comparing this
chapter against [§5](#proposal) will find the proposed field and MUST NOT expect it to be served.

| Proposed | Not in the shipped schema, because |
| :---- | :---- |
| `TaskTrigger.spec.condition` (a CEL expression scoping the trigger to a vendor, zone or robot — [§6.10](#design-details-condition-scoping-vendor-zone-and-robot)) | The reference implementation vendors no CEL evaluator, and a condition field that nothing evaluates is strictly worse than none: an operator would write a scope, the apply would succeed, and the trigger would fire on every event regardless. The field's whole purpose is to NARROW what fires, which makes an inert one more dangerous than usual |
| Mapping event payload fields into action parameters (no section of this document proposes it) | A half-done version that silently dropped unmapped fields would produce a task that looks parameterised and is not. The template is drafted as written and a person fills values in; that is what the draft is for |
| `TaskTrigger.status.pendingResponses[]` and the approval verbs ([§6.11](#design-details-the-approval-gate)) | Nothing stages a response. The draft flag is the gate instead, guarded by the `activate` verb as stated in rule 8 above |
| A response that appends to a live `FleetTask`, or sets `desiredState` | The controller holds `get;list;watch;create` on `fleettasks` and nothing more; its entire output is a new held task |

Three further gaps are properties of the shipped mechanism rather than missing fields, and an
operator MUST NOT read any of them as working:

- **`TaskTrigger.status.pendingDrafts` is pruned, not kept in step.** A draft is added when the
  trigger fires and removed on a later reconcile in the namespace once it is cleared or deleted —
  including when the event that owns it is deleted. A cache miss is confirmed against the API
  server before a draft is removed, so a draft created a moment ago is not dropped. Removal waits
  for that reconcile; an operator reading the list in the moment after a draft is cleared can
  still see it. `status.draftCount` counts drafts created, once each: an event evaluated a second
  time before its phase is visible collides on the draft's name and is not counted again.
- **`TaskTrigger.status.conditions` carries `Ready` only on the True edge.** It is written when a
  trigger fires (reason `SourceResolved`) and there is no path that writes it False — a trigger
  whose source does not exist is not marked not-ready, because the refusal is recorded on the
  event. A trigger that has never fired therefore has no conditions at all, and an absent `Ready`
  MUST NOT be read as a failure.
- **A draft does not expire.** Nothing removes or fences a draft as its event ages, unlike the
  `expiresAt` of a staged response ([§6.11](#design-details-the-approval-gate)). A person activating a
  draft activates it as composed when the event happened, and MUST check that it still applies.

> **No check at v0.4.** No conformance row grades the behaviour these gaps describe.
<a id="conformance"></a>
## 8. Conformance Checklist

This checklist defines what an implementation MUST, SHOULD, and MAY do to conform to this RFC. It is
derived from [§6](#design-details) and follows the pattern of the Fleet Adapter conformance
checklist (`adapters/CONFORMANCE.md`) and RFC-0004's north checklist. Where this checklist and
RFC-0001 differ in the interpretation of a shared term, **RFC-0001 governs**. This checklist MAY
raise an RFC-0001 `SHOULD` to a `MUST` for conformance to this profile; it MUST NOT weaken an
RFC-0001 requirement.

Everything specified here is **opt-in**. An adapter that emits no semantic events, and a control
plane that reconciles no `TaskTrigger`, are unaffected by this RFC and conform to RFC-0001 exactly as
before it existed. That is asserted as a check rather than assumed: **E0.1**.

There are two independent profiles. An implementation MUST declare which it targets; they share only the
E0 checks and may be implemented by different parties.

| Profile | Implemented by | Checks |
|---|---|---|
| **Event Source** | a fleet adapter, or an executor that is not a fleet adapter | E0, E1–E4; E10 for an executor that is not a fleet adapter |
| **Trigger Consumer** | a control plane | E0, E5–E9 |

> **Not graded at v0.4.** No check grades the declaration. No implementation can claim conformance
> to this document ([§1](#abstract)), so no declaration is made against it.

> **No check at v0.4 (rules addressed to a reader or an operator).** Eleven requirements of this
> document bind a reader, an operator or the person who activates a draft, not an implementation,
> and no conformance check can grade them. In [§6](#design-details): a reader expects no
> `event-source` row in a report; a reader does not miss the two consequences of the draft path; and
> on an install that serves no admission webhooks an operator does not treat the draft flag as an
> authorization boundary. In [§7](#resources): a reader does not miss that nothing on the draft path
> dispatches a robot; an operator does not read a zero count on a `SemanticEventSource` as evidence
> that nothing arrived; an implementation that leaves those counters unset is not read as
> non-conforming, and a reader does not treat an unset counter as a count of zero; a reader does not
> expect a proposed field to be served; an operator does not read the three gaps of the shipped
> mechanism as working, or an absent `Ready` condition as a failure; and a person activating a draft
> checks that it still applies. A deployment meets them in its own procedures.

<a id="conformance-e0-opt-in-and-non-interference"></a>
### 8.1 E0 — Opt-in and non-interference

- **E0.1 (MUST)** An implementation of neither profile MUST pass the full RFC-0001 conformance suite
  unchanged, where "pass" means the RFC-0001
  `ConformanceReport`'s `verdict: pass` (RFC-0001, *Conformance Profiles*, *The `ConformanceReport`*). A test that requires an event to be emitted, or a `TaskTrigger` to exist, MUST NOT
  appear in the RFC-0001 baseline.
- **E0.2 (MUST)** A `FleetTask` requiring an event a target adapter does not declare MUST NOT
  schedule onto that adapter, and the reason MUST be named in the pending `FleetAction`'s `status.message` rather
  than presenting as an unexplained pending action.

<a id="conformance-e1-event-emission"></a>
### 8.2 E1 — Event emission

- **E1.1 (MUST)** Carry semantic events only in `ActionStatusUpdate.events[]`. An implementation MUST
  NOT encode a domain event in `message`, `progress_pct`, or any other field.
- **E1.2 (MUST)** Set `type` to a value the capability declared at registration ([§6.1](#design-details-adapter-channel-the-additive-taskevent)). An undeclared `type` MUST be rejected by the control plane and MUST NOT fire a trigger.
- **E1.3 (MUST)** Set `time_ms` from the adapter's own clock at observation, not at transmission.
- **E1.4 (SHOULD)** Keep `payload` small enough to be carried in a status write; an implementation
  SHOULD reference bulk data rather than embedding it.
- **E1.5 (MUST NOT)** Emit an event as a substitute for a lifecycle transition. `ActionState` remains
  the only lifecycle channel.

<a id="conformance-e2-declaration-and-capability-gating"></a>
### 8.3 E2 — Declaration and capability gating

- **E2.1 (MUST)** Declare every event `type` the adapter can raise, per capability, on the
  `ScanCapabilities`/registration surface, so admission can validate a `TaskTrigger` against it.
- **E2.2 (MUST)** Re-declare on reconnect. An adapter MUST NOT assume the control plane retained a
  declaration across a stream break.
- **E2.3 (SHOULD)** Declare no event `type` the adapter cannot actually raise. An over-broad
  declaration admits triggers that will never fire, which is indistinguishable at the API from a
  correctly-configured trigger waiting for a rare condition.

<a id="conformance-e3-sequencing-and-idempotency"></a>
### 8.4 E3 — Sequencing and idempotency

- **E3.1 (MUST)** Make `seq` strictly monotonic per `(subject_kind, subject id)` — for this RFC's one
  kind, per `action_id`. An implementation MUST NOT reuse a `seq` within an execution and MUST NOT
  reset it on reconnect.
- **E3.2 (MUST)** Tolerate at-least-once delivery: re-transmitting an already-delivered
  `(subject, seq)` after a stream break is correct and MUST be safe.
- **E3.3 (MUST)** Set `subject_kind`, or leave it `EVENT_SUBJECT_KIND_UNSPECIFIED`. A receiver MUST
  treat `UNSPECIFIED` as `ACTION`. A receiver MUST reject a `subject_kind` it does not recognise
  rather than defaulting it to `ACTION`.
- **E3.4 (SHOULD)** Buffer recent events across a short stream break and re-send them on
  reconnect, bounded — an adapter SHOULD drop oldest rather than grow without limit, and SHOULD make
  the drop observable. No message acknowledges an event: `ActionOutcomeAck` answers only a retained
  terminal outcome (RFC-0001, Fleet Adapter Protocol, *The retained outcome*), and E3.2 makes the re-send safe.

<a id="conformance-e4-terminal-and-continuous-executions"></a>
### 8.5 E4 — `terminal` and `Continuous` executions

- **E4.1 (MUST)** Set `terminal: true` only on an executor run whose `completionMode` is `Continuous`. A fleet adapter executes no such run and MUST NOT set the flag.
- **E4.2 (MUST NOT)** Treat `terminal: true` as performing a transition. It requests one; the control
  plane performs it. An adapter MUST NOT stop reporting state on the strength of having set the flag.
- **E4.3 (MUST)** A receiver MUST ignore `terminal` on a `RunToCompletion` execution.
- **E4.4 (MUST)** Emit at most one `terminal: true` event per execution.

<a id="conformance-e10-event-sources-that-are-not-fleet-adapters"></a>
### 8.6 E10 — Event sources that are not fleet adapters

An implementation targeting the **Event Source** profile through an endpoint rather than a fleet
adapter meets E1.3, E1.4, E1.5, E3.2 and E4 as written, and these in place of E1.1, E1.2, E2 and E3.1.
A resident executor also meets E3.3 and E3.4 as written. A request/response executor holds no stream:
E10.3 replaces E3.3 for it, and E3.4 does not apply.

- **E10.1 (MUST)** Declare, on the resource describing the endpoint, every event `type` it can raise.
  The control plane MUST reject an undeclared type, and an undeclared type MUST NOT fire a trigger —
  the same rule E1.2 applies to a capability.
- **E10.2 (MUST)** For a **resident** executor holding a stream: assign `seq` itself, strictly
  monotonic per `action_id` across every run of the action, never reset on reconnect or by a later run, never reused.
- **E10.3 (MUST)** For a **request/response** executor: emit events in its reply with no sequence
  number. The control plane MUST assign `seq` in reply order, continuing after the highest `seq` the invoking `FleetAction` has accepted, and
  MUST attribute the events to the `FleetAction` that invoked it, with `subject_kind: ACTION`.
- **E10.4 (MUST)** A request/response executor MUST return the same reply for a repeated invocation
  key, and the control plane MUST NOT record or evaluate the events of an invocation it has already
  processed. A retried invocation therefore fires a trigger exactly once.
- **E10.5 (MUST NOT)** An executor MUST NOT address a consumer, name a trigger, or raise an event
  against an execution other than the one it was invoked for.
- **E10.6 (MUST)** Document, for each endpoint, whether its events arrive as it observes them or
  in its reply — the two differ in when a waiting consumer resumes
  ([§6.6](#design-details-delivery-timing)).

> **No check at v0.4.** No row grades four rules of [§6](#design-details): a receiver ignores
> `terminal` on a robot-executed action, an unset `completionMode` is not read as
> `RunToCompletion`, a producer is not told where its events are consumed, and a fleet adapter does not address a
> consumer. E4.3 reaches only a `RunToCompletion` execution, and E10.5 binds only an executor that is
> not a fleet adapter. The reference implementation
> performs no transition on `terminal`, reads
> `completionMode` in no code outside its API type, and names no consumer to an endpoint: the
> `compute.v1` `Invocation` carries none. No field of `ActionStatusUpdate` carries a
> `TaskEvent`, so no adapter raises an event.

> **Not graded at v0.4 (control-plane halves).** The control plane's halves of E1.2, E3.3, E4.3,
> E10.1, E10.3 and E10.4 are graded nowhere. The Event Source profile binds the producer, and no
> Trigger Consumer row restates them. The reference control plane checks no raised event's `type`
> against a declaration, reads no `subject_kind`, and dispatches no request/response executor, so it
> assigns no `seq` to a reply and records no invocation key. It performs no transition on `terminal`
> ([§6.13](#design-details-what-the-reference-implementation-provides)).

<a id="conformance-e5-recording-and-bounds"></a>
### 8.7 E5 — Recording and bounds

- **E5.1 (MUST)** Process each `(subject, seq)` at most once, and MUST record events only on arrival
  of a discrete event — never on a telemetry tick (RA-1).
- **E5.2 (MUST)** Bound `status.events[]` at `SwarmadaConfig.spec.events.maxRecordedEvents`,
  retaining the most recent, and MUST discard older entries rather than failing the status write.
- **E5.3 (MUST)** Maintain `status.observedEventCount` and `status.lastEventSeq` as complete,
  monotonic scalars regardless of what the bounded list retains.
- **E5.4 (MUST NOT)** Evaluate a trigger by re-reading `status.events[]`. Evaluation happens on
  arrival; an implementation that reads the list will stop firing when the window rolls.
- **E5.5 (SHOULD)** Deliver every event to the audit log and the configured telemetry sink, which are
  the system of record ([§6.7](#design-details-bounded-event-history)).

<a id="conformance-e6-trigger-evaluation"></a>
### 8.8 E6 — Trigger evaluation

- **E6.1 (MUST)** Evaluate `condition` against the closed context defined in
  [§6.10](#design-details-condition-scoping-vendor-zone-and-robot) and no other data. An implementation MUST NOT expose the
  object graph, arbitrary lookups, or any I/O to the expression.
- **E6.2 (MUST)** Validate `condition` syntax at admission, not only at firing.
- **E6.3 (MUST)** Treat a `condition` that errors at evaluation as **not matching**, and MUST surface
  the error as a status condition and an event. Failing open would let a malformed expression fire an
  unintended response.
- **E6.4 (MUST)** Fire at most one response set per `(subject, seq)` per trigger.

> **No check at v0.4.** No row grades the rule that admission rejects a `source.subjectKind` it does
> not recognise ([§6.8](#design-details-the-tasktrigger-resource)). The shipped `TaskTrigger` carries
> no `subjectKind` field, and no admission webhook serves the kind.

<a id="conformance-e7-ownership-and-response-bounds"></a>
### 8.9 E7 — Ownership and response bounds

- **E7.1 (MUST)** Restrict `setDesiredState.target` to a standalone `FleetAction` or the owning
  `FleetTask`. An implementation MUST reject at admission a target naming a task-owned member action
  ([§6.9](#design-details-ownership-what-setdesiredstate-may-target)).
- **E7.2 (MUST)** Grow a `FleetTask` only through RFC-0001's append-only path and the scoped `append`
  verb.
- **E7.3 (MUST NOT)** Issue any imperative robot command as a trigger response. The response set is
  closed.
- **E7.4 (MUST)** Bound a trigger's authority by the authenticated identity that authored it. A
  firing MUST NOT create or widen a trigger. Until an implementation has a mechanism that bounds
  each response by the author's own permissions, it MUST NOT execute any response other than
  `draftTask`, which creates a `FleetTask` held as a draft for a person to activate. A controller
  that acts on its own ServiceAccount's permissions does not bound a response by the author.
  A `draftTask` response is the exception this check allows: the drafted task runs only once its
  draft flag is cleared, which the RFC-0001 `activate` verb gates where the admission webhooks are
  served ([§7.3](#resources-tasktrigger)).

<a id="conformance-e8-the-approval-gate"></a>
### 8.10 E8 — The approval gate

In this revision the gate stages only a `draftTask` response, the one response E7.4 lets an
implementation execute.

- **E8.1 (MUST)** Require `spec.approvalTimeoutSeconds` at admission whenever any response sets
  `requireApproval: true`, and MUST enforce a configured maximum.
- **E8.2 (MUST)** Execute exactly the staged `computedResponse`, never a recomputation at redemption
  time.
- **E8.3 (MUST)** Make `trigger-response-approve` and `trigger-response-reject` separately grantable,
  and MUST NOT include either in any built-in role. Admission MUST refuse a redemption or a rejection
  made by a ServiceAccount principal before it consults RBAC, whatever that principal's roles grant,
  as RFC-0001 does for `activate`.
- **E8.4 (MUST)** Move an unredeemed entry to `Expired` at `expiresAt` and MUST NOT execute it
  thereafter.
- **E8.5 (MUST)** Redeem a `pendingResponses[].id` at most once.

<a id="conformance-e9-audit"></a>
### 8.11 E9 — Audit

- **E9.1 (MUST)** Write the audit entry **before** the response executes. If the audit write cannot
  complete, the response MUST NOT run.
- **E9.2 (MUST)** Record the confirming human identity and the redeemed `pendingResponses[].id` on an
  `Approved` transition.
- **E9.3 (MUST)** Carry the RFC-0001 Audit Logging required fields on every one of
  `TriggerResponsePending`, `TriggerResponseApproved`, `TriggerResponseRejected`,
  `TriggerResponseExpired`.

<a id="conformance-self-certification"></a>
### 8.12 Self-certification

Once its checks are graded, conformance to this RFC is **self-certified**, on the same terms as every other Swarmada profile
(ADR-0007): the harness produces the result, the implementer publishes it, and there is no
certification authority, no gate, and no fee. Conformance — not repository location — would then make
an implementation conforming to this profile. At v0.4 the harness grades no check here, and no
implementation can claim conformance to this document ([§1](#abstract)).
<a id="drawbacks"></a>
## 9. Drawbacks

1. **Proto surface growth.** Even additive, `TaskEvent` grows the Fleet Adapter contract and the conformance surface. It is gated so non-emitting adapters are unaffected, but the new field is a permanent commitment.
2. **Adapter portability is uneven.** ROS 2 can map any topic or action feedback to an event; VDA5050 (`state.errors`/`information`, order states) and MAVLink (`STATUSTEXT`, custom messages) can express only a subset. Events are therefore capability-gated, and a `FleetTask` needing an unsupported event will not schedule onto such an adapter — visible in the pending `FleetAction`'s `status.message`, but a real constraint operators must plan around, and the same constraint noted for adapter payload portability generally.
3. **Reactive flows are harder to reason about.** Triggers expand the state space beyond a static dependency graph. The bounded, declarative response set and the closed `condition` context mitigate this, but a large trigger graph is still harder to audit than a DAG, and, once responses other than `draftTask` are enabled, the approval gate adds a second path (staged-then-confirmed) alongside the immediate one — a reader has to check `requireApproval` per response to know which applies. In this revision every response is a draft a person activates (drawback 4).
4. **A trigger waits for a person.** In this revision a trigger only drafts a `FleetTask`, and a person activates it ([§6.8](#design-details-the-tasktrigger-resource)). The cost is latency: a recovery that a person must confirm waits for that person. Which responses may run with `requireApproval: false` once a mechanism bounds a trigger's authority by its author is left to a later revision.
5. **A pending response has a real expiry, and getting `approvalTimeoutSeconds` wrong has a real cost.** Too short, and a genuine recovery need silently lapses to `Expired` before a human can act on it — the underlying event is not re-raised, so nothing re-prompts the operator. Too long, and a stale, no-longer-relevant response can sit executable for an unreasonable window. Sizing it to real staffing and response-time reality is an operator decision this RFC does not make for them.
6. **Bounding the recorded event history means the custom resource is a window, not a log.** [§6.7](#design-details-bounded-event-history) caps `status.events[]` so a busy execution cannot grow its own object until it becomes unwritable. The cost is that an operator reading `kubectl get -o yaml` sees recent events only, and must go to the audit log or the telemetry sink for the rest. `status.observedEventCount` makes the truncation visible rather than silent, but a reader who does not look at it will draw a wrong conclusion about how often something fired. The alternative — an unbounded list — trades that confusion for losing write access to the object, which is worse, but the confusion is real and is not fully mitigated.
7. **`terminal` puts a phase-affecting bit on the executor's side of the boundary.** A `Continuous` execution ends when its executor sets `terminal: true`. The control plane performs the transition and remains the only writer of phase, so the ownership rule holds — but an executor defect that sets the flag early ends a watch that should have continued, and the control plane cannot tell that from a correct signal. The mitigation is conformance coverage ([§8](#conformance) E4), not a structural guarantee, and at v0.4 the E4 checks are reserved and grade nothing.
8. **This RFC is smaller than an earlier draft, and a reader of that draft needs to know why.** `FleetAction.spec.executor.completionMode` (a `Continuous`, never-succeeding watch action) and time-synchronized co-completion were both specified here previously. Neither is defined in this revision — see [§1](#abstract) for where `completionMode` lives; co-completion is not specified in this version.
<a id="alternatives"></a>
## 10. Alternatives Considered

**A. Put events in RFC-0001 core.** Rejected. It would touch the Fleet Adapter contract and the mandatory conformance baseline, force every adapter toward event semantics, and enlarge the neutral-donation surface — against RFC-0001's minimalism principle. Layering here keeps the core stable and the feature opt-in.

**B. Edge-triggered imperative commands.** A trigger directly commands a robot (`pause()`/`stop()`) on an event edge. Rejected per ADR-0047: a dropped edge is a lost command, it rebuilds N×M robot-to-robot coupling, and it bypasses the control plane's admission and authority model. The declarative desired-state response persists and re-converges instead.

**C. Onboard-only reactivity, no standard.** Let each robot handle its own reactions with no control-plane involvement. Rejected for the cross-robot case this RFC exists for — a robot that must task a *different* robot — which has to go through the control plane at the plane's latency budget, not the robot's own.

**D. Overload `progress_pct` / `message` for events.** Rejected: untyped, unversioned, no idempotency key, no payload — unusable for reliable trigger evaluation.

**E. A second CRD for staged approvals (a `TriggerApproval` resource with its own lifecycle), rather than a status field on `TaskTrigger`.** Considered and set aside for this revision, not rejected outright. `status.pendingResponses[]` on the resource that produced the response keeps a two-primitive RFC at two primitives, and there is no case yet where approval needs a lifecycle independent of its trigger. Revisit if a real deployment needs approvals owned by a different team than trigger authorship, or approvals that outlive the trigger that staged them.

**F. A built-in "service engineer" role.** Rejected. Unlike RFC-0001's custom verbs, which ship in its built-in roles, this RFC keeps a new consequential verb separately grantable and out of every shipped role. The operator decides who holds `trigger-response-approve`; the standard defines the verb, not the org chart.
<a id="references"></a>
## 11. References

[1] RFC-0001, Swarmada Core Specification — `FleetAction`, the composite `FleetTask` (dependency graph, completion/failure/compensation, `desiredState`, append-only growth), and the Fleet Adapter Protocol.

[2] ADR-0047, "Control intent is level-triggered declarative state, not an edge-triggered command" — the decision this RFC's trigger binding follows: a write persists and re-converges after a disconnect, and re-writing the same value is idempotent, so a dropped edge is never a lost command. The requirement itself is normative in RFC-0001, `FleetAction.spec.desiredState`; ADR-0045 refines it. How a hold entered this way ends is stated by RFC-0001's *Action Table* (Safety Architecture; rows A1 and A6, and the rows out of `Paused` that A1 names), not here.


[3] VDA 5050, *Interface for the communication between automated guided vehicles (AGV) and a master control*, version 2, and the MAVLink common message set — the message sets the portability analysis in [§9](#drawbacks) compares.

[4] `docs/api-principles.md` — the CEL cross-field validation convention `TaskTrigger.spec.condition` follows.

