# ADR-0062: A semantic event produces a draft FleetTask, and the draft gate is forced by the controller

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, Kubernetes Expert
- **Related:** ADR-0006 (the task-source surface is the Kubernetes API), ADR-0045 (a desiredState hold is operator-lifted), ADR-0047 (declarative intent is level-triggered), ADR-0063 (a field this tree cannot evaluate is absent, not inert), RFC-0003 §6.10–§6.11; `api/v1/tasktrigger_types.go`, `api/v1/semanticevent_types.go`, `internal/controller/tasktrigger_controller.go`

## Context

Draft 4 asks for a `TaskTrigger`: a semantic event produces a `FleetTask`, a human
activates it, and only then is it dispatched — **no auto-dispatch** — with a *generic
public event source* so the public repository can demonstrate the mechanism with no
vendor adapter behind it.

The starting state, measured on 2026-09-15: **no `TaskTrigger` kind, controller or
webhook existed** — 18 CRDs, none of them this. `FleetTask` had no draft or
activation concept, and no event ingress existed that did not require a vendor adapter
behind it.

That last point is the constraint that shaped the design. RFC-0003's three event
source classes are all **execution-bound**: each raises events against a `FleetAction`
that is already running, behind a vendor adapter or an executor endpoint. None can be
demonstrated from a public repository, and — more importantly — **none can raise an
event when nothing is running**, which is the case a trigger is most useful for ("the
door sensor fired and no task exists yet").

Two forces then pull against each other. A trigger exists to close a loop
automatically; a fleet of physical robots must not start physical work because an
event arrived. The gate between them has to be somewhere that operator-authored data
cannot switch off.

## Decision

**Three new kinds, and a draft phase on `FleetTask`.** `SemanticEventSource` declares
the closed set of event types a non-adapter source may raise; `SemanticEvent` is one
occurrence; `TaskTrigger` binds a (source, eventType) pair to a `FleetTask` template.

**`SemanticEventSource` is the adapter-free, Kubernetes-native ingress.** Anything
that can reach the API server creates a `SemanticEvent` — consistent with ADR-0006,
which already makes the API server the task-source surface. It is deliberately **not**
named `TaskEvent`: that is RFC-0003's protobuf message, attributed to an execution and
sequenced by the adapter, and giving two different things one name would make the
idempotency rules of one appear to apply to the other.

**A firing produces a draft, and the draft gate is forced by the controller, not read
from the template.** A firing sets `FleetTask.spec.draft: true` and phase `Draft`
(`api/v1/fleettask_types.go:84,199`); the FleetTask controller returns immediately,
generating no children and scheduling nothing. A human clears the flag to activate.
The template is ordinary operator-authored data and **must not be able to switch the
guarantee off** — a test sets `draft: false` in a template and asserts the draft is
still held.

**The gate sits at the very top of `Reconcile`, before the `spec.flow` branch.** The
guarantee is only as strong as the narrowest path that bypasses it, and there are two
scheduling paths below that point which would each have needed their own check.

**An event whose type its source never declared is `Rejected`, fires nothing, and says
why** (RFC-0003 E10.1). Without the declaration, a typo and a type nobody has written
a trigger for are indistinguishable — both silently do nothing — and a silent no-op is
the worst outcome for a mechanism whose entire job is to notice that something
happened.

## Alternatives considered

- **Hold the new task with `desiredState: Paused`** instead of a new `draft` field.
  Rejected. `Paused` already means a safety hold on work that is *under way*, reached
  through an e-stop or a maintenance window and governed by rules about who may lift
  it (ADR-0045). A draft has never run, holds no robot and has no lease. Overloading
  `Paused` would put an approval gate and an emergency stop on the same control — the
  precise conflation ADR-0060 exists to prevent on the neighbouring mechanism.
- **Read the draft flag from the trigger's template.** Rejected: a template that
  omitted `draft`, or set it false, would auto-dispatch, which makes the no-auto-dispatch
  guarantee a property of every template an operator ever writes.
- **Auto-dispatch with a post-hoc cancel window.** Rejected. The robot has already
  moved; a cancel window converts a gate into a race against physical motion.
- **Reuse RFC-0003's execution-bound source classes and ship no generic source.**
  Rejected: nothing in the public repository could then demonstrate a trigger, and no
  trigger could fire when nothing was running.
- **RFC-0003 §6.11's `status.pendingResponses[]` machinery** — stage a computed
  response in status and redeem it by id, so that what executes is exactly what was
  approved. The draft gate reaches the same end differently: the `FleetTask` object
  *is* the staged artefact, and activating it runs what is on the object. The RFC's
  id/expiry/reject machinery is not implemented, and `approvalTimeoutSeconds` has no
  analogue — a draft waits indefinitely, which is the conservative direction.

## Consequences

- The public repository can demonstrate event-driven task creation end to end with no
  vendor adapter, via `config/samples/tasktrigger_demo.yaml`.
- `FleetTask` gains a phase and a spec field, and `swarmctl` gained the three kinds
  with print columns and describe output — `make test` caught them as unaddressable
  (`kubectl get tasktriggers` would work, `swarmctl get tasktriggers` would not) and
  failed until they were registered.
- The draft gate writes status once and is idempotent thereafter. A draft can wait a
  long time for a person, and a controller that rewrote status on every resync would
  turn a waiting task into a write loop against etcd (RA-1 holds: nothing here writes
  status on a telemetry tick).
- **Activation is not gated on a custom verb, and this is the known weakness.**
  Clearing `spec.draft` needs only `update` on `fleettasks`, so a ServiceAccount can
  authorise physical work. The gate is meant to put a *person* in the loop and the
  RBAC model cannot express "person". Two in-tree precedents answer this with a
  separately grantable verb that MUST NOT be held by a ServiceAccount (RFC-0003
  §6.11's `trigger-response-approve`, and `EstopClearGrant`); the fix is a validating
  webhook plus an RBAC surface, neither of which is a controller. It is a containment
  gap rather than an open door because no built-in role grants a workload
  blanket `fleettasks` update today — a missing containment, not an open door.
- `TaskTrigger` has no admission webhook yet. RFC-0003 requires ownership validation on
  `setDesiredState` targets and rejection of an unrecognised `subjectKind`; this
  controller implements neither verb — its only response is "draft a task" — so those
  rules have nothing to validate. They land with the response verbs.
- A trigger cannot yet be narrowed to one vendor or one zone. `spec.condition` and
  `paramsFrom` are **absent rather than inert**, for the reasons recorded in ADR-0063.

## Amendment 1 — activation needs the `activate` verb, and never a ServiceAccount

- **Date:** 2026-10-04
- **Basis:** `docs/adr/README.md` — the decision is unchanged (a semantic event produces a held
  draft, and the controller forces the flag), so no superseding ADR. The known weakness recorded
  under Consequences is closed.
- **What changes.** Clearing `spec.draft` (an update that moves it from `true` to `false`) needs the
  `activate` verb on `fleettasks`, checked by the `FleetTask` validating webhook with a
  SubjectAccessReview, and the webhook refuses every ServiceAccount principal before it consults
  RBAC. By the same test, a ServiceAccount cannot add members to a stored draft. `operator`, `fleet-manager` and `admin` hold the verb (RFC-0001
  #security-permission-matrix).
- **What it does not change.** An install that serves no admission webhooks runs no such check. An
  activation is not written to the safety audit log; the API server's audit record of the patch is
  the record of who activated a draft.
