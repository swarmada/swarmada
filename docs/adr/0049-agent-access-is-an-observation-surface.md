# ADR-0049: Agent access to the control plane is an observation surface, not a task source

- **Status:** proposed
- **Date:** 2026-09-04
- **Deciders:** Maintainers
- **Related:** RFC-0001 `ComputeEndpoint` (`spec.binding: Mcp`) and non-goal #10;
  RFC-0004 non-goals #3, #5 and #6; ADR-0006 (the task-source surface is the Kubernetes API);
  ADR-0047 (control intent is level-triggered declarative state); `conformance/CONTROL-PLANE.md` CP1.3

## Context

Two different integrations are both called "MCP", and the specification currently defines only one of
them. The ambiguity is the reason this record exists: the sentence "Swarmada supports MCP" is true,
false, or misleading depending on which direction the reader assumes.

**Outbound — specified.** `ComputeEndpoint` declares `spec.binding: Grpc|Http|Mcp`
(RFC-0001 §9.1.15 *ComputeEndpoint*). A `FleetAction` names an endpoint by name in
`spec.executor.endpointRef` and can say nothing else about it: no URL, no host:port, no image, no
command. Admission enforces three CEL rules, including the rejection of `binding: Mcp` with
`contract: Service`, graded as **CP1.3 (MUST)** in `conformance/CONTROL-PLANE.md`; the conformance
program ships a standard-library-only MCP executor fixture (`conformance/PROFILES.md`). The chapter
records the maturity plainly — *alpha · control-plane: partial · end-to-end: none* — and states there
is no dispatch path at v0.4. `internal/scheduler/scheduler.go:39` defines `ErrExternalAction` and
returns it at `:90` for an action carrying `spec.executor`.

**Inbound — unspecified.** Nothing in the specification, the conformance program or the reference
implementation exposes the control plane *as* a tool server to an external model or agent. A
read-only server of that shape has been proposed; this ADR settles the boundary before it is built,
rather than after.

Four existing constraints already bear on the question, and together they nearly decide it:

- **RFC-0001 non-goal #10 and RFC-0004 non-goal #6** exclude natural-language and LLM task generation
  from the control plane.
- **RFC-0004 non-goal #3** places task authoring, decomposition and planning above the submitted unit
  with the source, not the control plane. Any agent that authors work is therefore, architecturally,
  a task source.
- **ADR-0006** settles that the task-source surface is the Kubernetes API. A second write surface
  would contradict it.
- **ADR-0047** requires that nothing offer a path by which an external actor causes a robot effect
  without passing through admission, scheduling, deconfliction, the assignment lease and the audit
  trail.

One further constraint is stated on the outbound path and applies with equal force inbound:
*"An endpoint's result is data. It is never a command."* The chapter gives the reason — with a
model in the loop, anything written in a robot's field of view can reach it, and prompt injection
becomes a fleet-safety concern rather than a chat-safety one. An inbound surface inverts the flow
but not the exposure: fleet state read by a model is data that a model's operator may act on.

A separate open question — whether a device the control plane schedules may also be reachable by a
direct agent path, with no arbitration between the two writers and no way to detect the second — is
recorded in the decisions register and is not resolved here. This ADR is written so that it does not
depend on that resolution.

## Decision

**A model or agent connects to a Swarmada control plane on exactly one of two paths: as a
`ComputeEndpoint` it calls out to, or as a read-only observation surface it reads from. Neither path
places a model inside the control plane, and neither creates a second task-source surface.**

1. **No model is a component.** The control plane holds no model credential, names no model provider,
   and makes no inference call from a controller, a webhook or the scheduler. A model is reached only
   as a `ComputeEndpoint`, where it is an administrator-installed, namespaced object with its own
   RBAC and audit trail. This restates RFC-0001 non-goal #10 as an operational rule.

2. **An inbound MCP server is an observation surface.** It exposes read operations over control-plane
   state. It MUST NOT offer `apply`, `create`, `patch`, `delete`, or any status write. It adds no API
   type and requires no specification change, because it introduces no new state.

3. **A write path, if ever wanted, is not a new surface.** An agent that submits work is a task
   source and submits through the surface RFC-0004 governs, under ADR-0006. An inbound tool server
   MUST NOT become a second task-source surface, and MUST NOT offer a verb that reaches a robot
   without traversing the path ADR-0047 requires.

4. **Validation and planning tools are deferred, not permitted by default.** A tool that validates a
   submitted manifest, or computes an assignment without committing one, is compatible with the
   non-goals above — the client authors, the control plane only answers. It is nonetheless deferred
   until specified, because a planning path that deliberately produces no status write touches the
   RA-1 discipline that RFC-0004 non-goal #5 places out of scope for casual change.

5. **The two directions are named separately in every document.** "Executor binding" for the outbound
   `ComputeEndpoint` path; "observation surface" for the inbound one. Neither is referred to as
   "MCP support" without a direction.

## Alternatives considered

**Call a model API directly from a controller or webhook.** One hop, no extra process, full control
of the request. Rejected on four counts: it places non-determinism inside the reconciliation loop,
which is the property the architecture exists to provide; it puts a provider name and a credential
inside the cluster, which a neutral standard should not require of an operator; the call has none of
the RBAC or audit properties a `ComputeEndpoint` carries; and it violates RFC-0001 non-goal #10
directly.

**Reach a model over the existing `Http` binding rather than `Mcp`.** Not rejected — this is the
correct shape for a fixed inference service with a stable request envelope, and for a streaming
perception service only `Grpc` will do, since `contract: Service` requires it. It is a different
case from the one this ADR addresses: it does not give an operator a way to interrogate fleet state,
and the endpoint's parameter schema must be restated rather than discovered.

**Place an agent framework behind `binding: Mcp` as an executor.** Permitted by the specification as
written, and no change is proposed. Recorded here with its costs: latency is unbounded behind an
endpoint the `App` contract treats as producing one terminal result, failure modes are opaque, and
the prompt-injection surface is the largest of any option. The "a result is data, never a command"
rule is what contains it, and it should be cited wherever this configuration is documented.

**Give the inbound server a write path.** Rejected. It would constitute a second task-source surface
in contradiction of ADR-0006, and would offer a verb reaching a robot outside the path ADR-0047
requires. The two-writer question in the decisions register is a further reason to decline, not the
principal one.

**Ship an interactive model-backed interface in this repository.** Rejected. It would embed a model
provider choice in a vendor-neutral standard, require an operator to supply a model credential to get
value from the control plane, and place task authoring inside the control plane in contradiction of
RFC-0004 non-goal #3.

**Expose no inbound surface at all.** The honest alternative, and the cost of rejecting it should be
stated: thirteen resource types is a real operator-onboarding cost, and answering a question that
spans several of them currently requires a reader who already knows the model. An observation surface
reduces that cost without adding state, which is why it is preferred over doing nothing.

## Consequences

**What this preserves.** The control plane holds no model credential and names no provider, so
adopting it requires no relationship with a model vendor. Non-determinism stays outside the
reconciliation loop or behind a named endpoint. A read-only surface has no verb by which an injected
instruction could reach a robot, which is a stronger claim than a policy of restraint.

**New obligations.** The read-only property must be enforced by construction and tested, not
asserted — an observation surface that grows a convenience mutation is the failure mode to guard
against, and it is the same scope-creep risk the non-goals already name. The serving identity's RBAC
becomes the security boundary: it is least-privilege read, and it is the deployer's obligation to
scope it. Authentication of the tool server itself is a deployment concern and must be documented as
one rather than assumed. The two names in decision 5 must be used consistently, including in
conformance and release material.

**Drawbacks accepted.** An operator cannot author work through the observation surface; natural-
language authoring stays outside the standard, with the task source, which is where the non-goals
put it. A second protocol surface is one more thing to version and maintain. MCP's authorization
story is young and moving, and this decision takes a dependency on it for a developer-facing surface
even though not for a normative one.

**Seams left for future change.** A write path, if it is ever wanted, attaches at the RFC-0004 north
surface and nowhere else. Resolution of the two-writer question in the decisions register may tighten
deployment guidance further; it cannot loosen decisions 2 and 3, which stand on ADR-0006 and
ADR-0047 independently.
