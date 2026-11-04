# Control Plane Conformance — the external-action surface

This document defines what a **control plane** must, should, and may do to
conform on the Draft 4 external-action surface: dispatching to Compute
Endpoints, keeping an endpoint's result from becoming a command, pinning what
runs, provisioning cluster compute, and gating the reactive tier. One RFC-0003
check is still part of this surface in the published registry: `CP3.3`, as an Extended feature
(`result-is-data.approval-default`), which no level and no verdict counts. Where this
document and RFC-0001 differ, **RFC-0001 governs**.

The party under test is a deployment, so a report against this catalog names the
deployment as its implementation and records `capabilityTier: NotApplicable`:
the tier grades how much authority an adapter *offers* over a fleet, and the
control plane is the party holding authority, not offering it.

> **Status:** the checks in `CP0`, `CP1`, `CP4` and `CP5` are driven today by
> `conformance/controlplane.py` against the shipped CRD schemas and the shipped
> manager RBAC — a static, cluster-free run a stranger can reproduce from a
> clone. `CP2`, `CP3` and `CP6` describe runtime behaviour of a controller that
> does not yet dispatch to endpoints; they record `skip` with an enumerated cause
> until it does. A skip is not a pass.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP0 — Report integrity (self-checks on this suite)

- **CP0.1 (MUST)** A skip reason must be consistent with what the run observed,
  and must name an enumerated cause rather than free text.
- **CP0.2 (MUST)** The report names the deployment under test and the CRD and
  RBAC artefacts the run actually read. A static run that cannot say which files
  it read is asserting the harness.
- **CP0.3 (MUST)** The run stands alone: every artefact it read resolves inside
  the public repository, and no profile, check id, check title or fixture path
  names a specification this suite cannot read.
- **CP0.5 (MUST)** Every check the registry names is defined in this document,
  and every check defined here appears in the registry.

### CP0.x — harness context

- **CP0.4 (context)** The artefact paths and digests the run read. Harness
  context; it does not count toward conformance.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP1 — Containment: an endpoint is referenced, never described

- **CP1.1 (MUST NOT)** Offer any field, anywhere in a `FleetAction`, a
  `FleetTask` or a `ComputeEndpoint`, through which a task author could supply an
  image, a script, a command, a URL or a `host:port`. The privilege boundary
  between *may ask the fleet to do work* and *may run arbitrary code* must not be
  crossed by a field in a task spec; once it is, no RBAC design downstream can
  recover it. Verified against the shipped CRD schemas, so it is a
  property of what ships rather than of what was intended.
- **CP1.2 (MUST)** Resolve an executor only by **name**, to an administrator-
  created object in a permitted namespace.
- **CP1.3 (MUST)** Reject `binding: Mcp` with `contract: Service` at admission.
  MCP is request/response and has no binary frame stream; `Grpc` is the only
  binding that carries one.
- **CP1.4 (MUST)** Require `streamPolicy.pinEndpoint: true` for
  `contract: Service`, and resolve **one** address at activation rather than
  handing out a Service VIP. Load-balancing per connection silently splits a
  stream across replicas, degrading tracking, motion estimates and pre-roll
  instead of failing.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP2 — Endpoint dispatch

- **CP2.1 (MUST)** Derive the invocation key from
  `(namespace, action, runGeneration, attempt)` and send it on every dispatch.
  Swarmada retry plus transport retry otherwise double-writes against an external
  system.
- **CP2.2 (MUST NOT)** Block a reconcile on an endpoint call. An endpoint's
  latency is unbounded, and a controller that waits on one has made an external
  system's outage into a control-plane outage.
> **`CP2.3`–`CP2.5` are registered `excluded` and graded nowhere.** They are the
> control plane's half of `CE6.3`, `CE6.4` and `CE6.1`, and RFC-0001 states none of them
> at v0.4: its `ComputeEndpoint` chapter calls `spec.emitsEvents` declarative only. Under
> RFC-0001's precedence rule (conformance profiles, *Precedence*) a row here grades no
> RFC-0001 obligation the chapter does not state, so since suite revision `2026.10.13`
> [`profiles.yaml`](profiles.yaml) lists them under the surface's `excluded:`, in no
> profile. The suite still records each row with its grade, and no profile result and no
> verdict counts them until RFC-0001 states them. The `endpoint-dispatch` profile holds
> `CP2.1` and `CP2.2` as Core. The endpoint's half, `CE6.1`–`CE6.5`, is excluded with
> them ([`COMPUTE-ENDPOINT.md`](COMPUTE-ENDPOINT.md)).

- **CP2.3 (MUST)** Assign `seq` in reply order for a request/response executor,
  continuing after the highest `seq` the invoking `FleetAction` has accepted, and
  attribute the events to the invoking `FleetAction` with `subject_kind: ACTION`
  (the control plane's half of `CE6.3`). The contract keeps one `seq` space per
  action, across every run and invocation of it (the `StreamStatus` comment in
  `proto/compute/v1/compute.proto`); a numbering that restarts at 1 for each
  invocation loses the second invocation's events.
- **CP2.4 (MUST NOT)** Record or evaluate the events of an invocation already
  processed, so a retried invocation's events are recorded once (the control
  plane's half of `CE6.4`).
- **CP2.5 (MUST)** Reject an event type the endpoint's
  `spec.emitsEvents` does not declare, and act on nothing from one
  (the control plane's half of `CE6.1`). Declaration is symmetric with the adapter path; without the
  symmetry, the unvalidated half becomes the one everybody uses. **RFC-0001 does not state this
  rejection at v0.4:** its `ComputeEndpoint` chapter calls `spec.emitsEvents` declarative only,
  with nothing validating a raised event against it. This row is registered `excluded` and
  graded nowhere until the chapter states the rejection (see the note above `CP2.3`).

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP3 — A result is data; it is never a command

- **CP3.1 (MUST NOT)** Contain any path by which an endpoint's response reaches
  an actuator without passing through a **declarative construct the task author
  wrote in advance** — a `select` over an enumerated case set, or a `loop.until` — or through a person's activation of a `FleetTask` drafted with `spec.draft: true` (RFC-0001 ComputeEndpoint).
  RFC-0003's `TaskTrigger` is outside RFC-0001's contract, and so is any path
  through one. With a model-backed executor in
  the loop, anything written in the robot's field of view is an input to that
  model; prompt injection stops being a chat-safety concern and becomes a
  fleet-safety one.
- **CP3.2 (MUST)** Take the `default` branch for a value the author did not
  enumerate. A model returning `"drive to the loading dock"` where the author
  wrote `chair | door | other` selects `other`. That property is the structural
  form of `CP3.1` and every future extension to the result path should be tested
  against it.
- **CP3.3 (SHOULD)** Treat `requireApproval: true` as the safer default for a
  trigger response that commits new physical work. `requireApproval` is a field
  of RFC-0003's `TaskTrigger`, which RFC-0001 does not specify, so this Extended
  row has no subject in a deployment of RFC-0001 alone.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP4 — Image provenance

- **CP4.1 (MUST)** Record, on the run, the resolved digest of the bytes that
  served it. Every run must be attributable to specific bytes; an endpoint's half
  of this is `CE8.1`.
- **CP4.2 (MUST NOT)** Admit a mutable image reference. **A run that resolved
  through a mutable tag fails Core**, and no configuration rescues it: a
  deployment that enables `spec.compute.allowMutableImageRefs` is not Core-
  conformant, whatever else it passes, and its report says so rather than
  omitting it. The lab-iteration need that motivated a mutable reference is met
  by recording the resolved digest on every run — which relaxes the staleness
  guarantee only, since the code still arrives as a signed image.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP7 — Zone restrictions (ADR-0058)

An operator states a site rule once — *no robots on the patio*, *no mopping on the
carpet* — in the fleet's own vocabulary, and every path through the fleet respects
it. Candidate filters 11 and 12 are the control-plane half; the adapter half is
`zone-restrictions` on the `FleetAdapter` surface.

- **CP7.1 (MUST)** Carry the resolved restriction projection the filters read:
  `FleetZone.status.restrictions.effective[]`, each entry naming its `axis`, its
  `mode`, and the zone it was `inheritedFrom`. Consumers read the PROJECTION, never
  `spec.restrictions` and never the raw ancestor tree (FZ-R4) — a second
  implementation of the fold is a second answer, and FZ-R9 names the way it drifts:
  to nearest-ancestor-wins, which does not conform. `inheritedFrom` is the
  load-bearing field: a refusal must name the zone that DECLARED the rule, which is
  very often not the target.
- **CP7.2 (MUST)** Carry `FleetZone.spec.restrictions`, so a site policy is
  expressible in the API at all rather than only inside whichever application
  happens to be dispatching.
- **CP7.3 (MUST)** Fill `AssignAction.operating_constraints` on a real dispatch with
  the resolved effective operating rules in force **at the zone the robot is being
  sent to** (FA-R2), so the adapter can honour or decline them. **Recorded as
  unexercised by this profiler, never green.** It is a property of a live dispatch:
  whether the field is *filled*, from the resolved projection at the robot's leaf,
  cannot be read off a schema, and confirming that the field *exists* would be a green
  line meaning only that the schema has a slot. The behavioural half is verified in
  the reference implementation
  (`internal/controller/zone_operating_constraints_dispatch_envtest_test.go`, which
  drives the real reconciler, and `internal/command/operating_constraints_wire_test.go`,
  which asserts the message); the adapter half is `C17.1`/`C17.2`/`C17.4` on the
  `FleetAdapter` surface, which are live.

**What CP7.1 and CP7.2 do NOT establish.** They are read off the shipped schemas, not off a
running cluster: they prove the surface exists, not that the filters behave. The
behavioural half lives in the reference implementation's own tests
(`internal/controller/dispatch_zone_restrictions_test.go` and its envtest sibling),
and a deployment that carries the surface and implements no filter passes these two
and is still not conformant. Saying so here is cheaper than letting a reader infer
more from a green line than it means. **CP7.3 does not even claim that much** — no
pairing of a running control plane with a running adapter exists in any harness in this
tree, so the row records what cannot be established here rather than substituting what
can.

**The adapter half is not static, and the asymmetry is worth knowing.** The
`zone-restrictions` profile on the `FleetAdapter` surface (`C17.1`, `C17.2`, `C17.3`
in [`../adapters/CONFORMANCE.md`](../adapters/CONFORMANCE.md)) drives a running
adapter: it dispatches an assignment carrying an operating constraint the adapter
cannot honour and grades the refusal it actually sent. A green line there means the
adapter **declined**; a green line here means the deployment **can express** the rule.
Two profiles of the same name, from the same ADR, carrying different strengths of
evidence.

`C17.4` on that surface goes one step further and is worth knowing about from here: it
requires the adapter to **accept** an assignment carrying only subjects it declared, so
the family grades an answer that depends on what arrived rather than an adapter that
refuses reflexively. That is the closest any harness in this tree comes to checking
FA-R2's arrival — and it checks it at the adapter, against a set the harness itself
fabricated, never against one a real control plane filled. That is why `CP7.3` above is
a recorded non-result rather than a green line.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP5 — Cluster compute (`provision: workload`) — **Extended, never Core**

This profile is **Extended at every level and can never be Core**. A deployment
that grants the manager no create authority on workload kinds declines the
profile and **MUST pass Core in full**. That is not a
concession; it is the shipping order. `provision: existing` creates nothing,
needs no new RBAC, works in every topology, and is what S1 ships.

The reason the profile can never be Core is that pod-create is an escalation
path: a principal who can create a pod can set `serviceAccountName` and read that
account's tokens, mount `hostPath`, or request privileged mode. Making that a
condition of conformance would make every conforming deployment hold it.

- **CP5.1 (MUST, when implemented)** Stamp a fixed, low-privilege
  `ServiceAccount` on every workload created, and **refuse** a template that
  names its own.
- **CP5.2 (MUST, when implemented)** Scope workload creation to a namespace,
  never cluster-wide. Once granted cluster-wide it cannot be narrowed by anything
  downstream.
- **CP5.3 (MUST, when implemented)** Create workloads only from an
  administrator-created template referenced **by name**. There is no inline pod
  spec in a task, and `CP1.1` is the check that says so.
- **CP5.4 (MUST, when implemented)** Set an `ownerReference` from the workload to
  the action, so garbage collection *is* the duty cycle and "activated only for
  the time it needs to function" stops depending on a lease implementation being
  correct.
- **CP5.5 (MUST)** A deployment granting no workload-create authority declines
  this profile and passes Core in full. This check exists so the decline is a
  **verified outcome** rather than an absence: the harness reads the shipped
  manager RBAC, confirms no workload verbs are granted, and records the profile
  not-applicable — which is a different fact from a profile that applied and went
  unverified.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP6 — Reactive dispatch gating

- **CP6.1 (MUST)** Refuse to dispatch a `ReactiveBlock` using a construct the
  target adapter's declared `reactive_grammar_version` does not cover. This is
  the gate that makes a closed grammar enforceable across a version skew.
- **CP6.2 (MUST)** Reject at admission a predicate naming a control-plane source.
  The adapter closes the grammar (`RB2.7`) and the control plane
  closes admission; a hole on either side is the whole hole.
- **CP6.3 (MUST)** Reject at admission an effect outside the closed set of four.
- **CP6.4 (MUST)** Refuse to dispatch `playCue` when
  `SwarmadaConfig.spec.reactive.allowLocalCues` is false.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CP8 — Status-write discipline (RA-1)

- **CP8.1 (MUST NOT)** Write `Robot` status on a telemetry tick. Telemetry is
  projected, not written to status per tick; a status write happens on a state
  transition and never on the arrival of a telemetry frame. This is the control
  plane's half of the Fleet Adapter catalog's `C6.3`, which states the same
  discipline and cannot be graded on the adapter surface: nothing an adapter puts
  on the wire can show whether the plane that receives it writes status per
  frame. It is graded here, on the party that holds the write, and the Fleet
  Adapter profiles exclude `C6.3` so that an adapter's result does not rest on a
  property of the plane. Establishing it needs a running control plane fed
  telemetry and observed for status writes; this suite's control-plane driver
  reads shipped artefacts only, so the row records that it was not exercised.

## Control-plane obligations with no CP-row

RFC-0001 states these control-plane obligations, and this catalog has no check for any of
them. Each is an obligation all the same: passing this suite does not show a control plane
meets them. The RFC-0001 section that states each is named in parentheses.

- **The safety invariants.** Keep I1 to I7: work assigned to a robot a stop holds, `Resuming`
  included, never moves to another robot automatically: it leaves only on the robot's own terminal
  report or an operator's decision (the `estop-clear` holder's requeue, or a cancel). Work a stop
  held stays held after the operator release, and moves then only as row A6 lists; a lease horizon
  that passed under the hold is no proof. Under no stop other work moves on the proof the row names;
  work a robot may have started is never given to a second robot without such a decision or proof; only the operator release lifts a stop hold; one lease horizon, and none computed
  when `leaseExpiresAt` cannot be read; the report token and `assignmentGeneration` are never
  compared with each other; no `AssignAction` to a robot a stop holds; and nothing the control
  plane cannot confirm releases a robot, resumes work or moves work, the cases I7 names (safety,
  *How the Two Interact*, SAFETY-INVARIANTS).
- **Guard timing.** Check each row's guard again when its transition is written; a row whose
  check fails did not happen (safety, *Robot Stop Table*, *Rules for every S row*, and *Action
  Table*, *Rules for every A row*, which state how the check is made).
- **Rows a condition may hold back.** Let a controller-owned condition hold back only the rows
  the table names as suppressible (A15, A17 and A23), never a row that guards a stop, and
  record the start of a `RUNNING` report held back from A17 (safety, *Action
  Table*, *Rules for every A row*, *The start* and the term *started*).
- **A stop on two paths.** Send each stop at once on both of the robot's streams: as an `Estop`
  on its `SafetyStream`, and as the stop's fence, the same `Estop` in order with the
  `AssignAction`s, on its `ControlStream`. Mint the stop ID when the stop is issued, and carry
  it on every copy and every re-send. Put no write, webhook, store or lock on either send, let a
  failed send on one stream delay nothing on the other, and record the stop in
  `Robot.status.stops` only after sending (safety, *Robot Stop Table*, the term
  *stop ID* and row S1; fleet adapter protocol, *The Stop Latch*).
- **Stop windows and a change of leader.** Run S4's delivery and confirmation windows for each
  stop, from the times row S4 names, and send again from a new leader every stop in force with no
  confirmed outcome: one still `Stopping`, one never acknowledged, and one with no record, which
  it sends as row S1 does (safety, *Robot Stop Table*, row S4 and *Sending a stop again*).
- **`EstopClear`.** On the clear, send on the robot's `SafetyStream` the `EstopClear` of each stop
  row S5 names, and remove a cleared stop's record only as *Removing a recorded stop* states; answer
  a report as *Reported stop IDs and local-latch reports* states (safety, *Robot
  Stop Table*, the term *recorded stop*, row S5, *Removing a recorded stop* and *Reported stop IDs
  and local-latch reports*; fleet adapter protocol, *The Stop Latch*). **Not met at v0.4:** the
  reference control plane answers a report only of an ID whose entry it removed, and records no
  other reported stop (`internal/safety/dispatcher.go`, `RouteAck`).
- **Stops an adapter reports.** Treat a stop ID an adapter reports as *Reported stop IDs and
  local-latch reports* and the term *in force* state: a reported stop is a stop the control plane sent, looked up for that robot
  only (safety, *Robot Stop Table*; fleet adapter protocol, *The Stop Latch*).
  **Not met at v0.4:** the reference control plane drops an `EstopAck` that names no stop it sent
  (`internal/safety/dispatcher.go`, `RouteAck`).
- **Edge stops.** Handle an edge stop as safety, *Edge /
  Headless Emergency Stop*, states; that section is the one statement of the rule (fleet adapter
  protocol, *The Stop Latch*, for the report). **Not met at v0.4:** the reference control plane
  reads no report of a local latch (`internal/controlstream/server.go`).
- **Dispatch needs contract `1.4.0`.** Give a robot no work unless its adapter's connection
  negotiated contract `1.4.0`; a connection at `1.3.x` still registers, reports and is stopped
  (control plane, *Scheduler*, candidate filter 9). **Not met at v0.4:** by default the reference
  control plane dispatches to any connection inside the supported range. The check exists behind
  the manager flag `--require-latch-contract`, which is off by default (`adapterLatchContract`,
  `internal/controller/adapter_readiness.go`).
- **`CommandResult` binding.** Mint each `command_id` once across streams, adapters and leader
  terms, and read a result only as the binding of a `CommandResult` states (safety,
  *Assignment Lease and the Single-Executor Guarantee*, *Binding of a `CommandResult`*).
- **The last finalizer of a held robot.** While any stop holds a robot, refuse the removal of
  its last finalizer to every identity but the control plane's, except while its namespace is
  being deleted (safety, *Assignment Lease and the Single-Executor Guarantee*, the robot's
  record of the action).
- **A terminal report off `InProgress`, and a requeue on a lost robot's work.** Write a
  terminal report for an action that is not `InProgress` by row A25, and carry out a requeue
  request on a `Revoking` action no stop holds by row A26 (safety, *Action Table*).
- **Stops and clears sent again on new streams.** When a `SafetyStream` opens for a robot on
  which a stop is in force, one not yet acknowledged included, or whose `estopState` is
  `Stopping`, `Stopped` or `Failed`, send its stop again under the same stop ID, and write the
  answer by rows S2 to S4. When a `ControlStream` opens for a robot, send the fence of each stop
  in force, recorded or not, before any `AssignAction`, and send no `AssignAction` for a robot on a
  `ControlStream` until its register on that stream has been processed and those fences sent there.
  Send each of these, and count each report,
  only on the streams of the adapter the robot's `spec.adapter` names (safety, *Robot Stop
  Table*, row S1, *Sending a stop again* and *The robot's adapter*; *Action Table*, row A13).
- **The end state.** On each abnormal event record a fresh end-state stop for the robot (an
  `EndState` entry of `Robot.status.stops`, with a fresh stop ID), hold the robot under it whatever
  its `estopState` until an operator's attestation of the check at the robot ends it, refuse an
  attestation from a ServiceAccount, and refuse a change of `spec.adapter.name`, as *Abnormal events
  and the end-state stop*, *The attestation* and *The robot's adapter* state; those rule blocks
  are the one statement of all of it (safety, *Robot Stop Table*).
  **Not met at v0.4:** the `Robot` webhook admits a change of `spec.adapter.name`
  (`internal/webhook/robot_webhook.go`), the reference records no end-state stop and reads no
  attestation, and a robot that registers again is not stopped.
- **`accepted = false` as a proof.** Release an assignment on `accepted = false` only when the
  answer is bound to the assignment's one `AssignAction`. Row A24 states when a refusal
  releases an assignment: under no stop only; a refusal under a stop moves nothing (safety,
  invariant I1; *Assignment Lease and the Single-Executor Guarantee*; row A24).
- **Refusing the release.** Refuse the operator release (row S6) while the robot's own telemetry
  since the clear shows it is not ready, and name the cause. Hold the release for no capability
  that was not Active before the stop, and for none that a `ZoneMaintenance` window holds
  `Paused`. Carry out no refused release later, when its cause resolves: the operator releases the
  robot again (safety, *Estop State Machine*, *When the release is refused*, *What does not hold
  the release* and *A refused release is closed, not queued*).
- **Dispatch during the window.** Withhold a `Resuming` robot from dispatch for the whole window,
  by an exclusion separate from candidate filter 10 (safety, *Estop State Machine*, *Dispatch
  during the window*).
- **A leader that finds a robot in `RESUMING`.** When it did not accept the robot's clear,
  re-arm the release check rather than release the robot, take the first projection it observes
  as the post-clear baseline, and refuse the release until telemetry strictly later than that
  baseline (safety, *Estop State Machine*, the second requirement across a change of leader).
- **Leadership.** Depend on no exclusive leadership for safety: two leaders may briefly overlap.
  Send on an adapter's streams only while holding leadership, and close them on losing it (safety,
  *Robot Stop Table*). **Not met at v0.4:** the reference is not shown to stay safe while two
  leaders overlap; it runs one manager replica.
- **The clear and the release, by their triggers.** Clear a robot's last stop (row S5) only on its
  authorized clear: `swarmada.io/estop-triggered` removed from its carrier, or the robot's
  end-state stops ended by an operator's attestation. Release a robot (row S6) only on a new value
  of `swarmada.io/estop-resume` written by an identity that holds `estop-clear`, and answer each
  value once (safety, *Robot Stop Table*, rows S5 and S6).
- **A destination under a stop.** Dispatch no action whose destination is in a zone under a stop:
  hold it `Pending` with the condition `ZoneStopped` and a `Warning` Event that names the zone, and
  remove the condition once the destination is no longer in a zone under a stop (safety, *Action
  Table*, row A15).
- **Unreachability.** Never reassign work on unreachability alone before the lease horizon
  (safety, *Assignment Lease and the Single-Executor Guarantee*).
- **An install with no outcome.** Where no install outcome arrives, record no install failure: an
  install abandoned on the rollout's own deadline is recorded as unconfirmed (fleet adapter
  protocol, *Install-outcome reporting*).

<!-- ─────────────────────────────────────────────────────────────────────── -->
## Passing

A deployment conforms when it satisfies every Core check of every applicable
profile, and declares the Extended profiles it holds. Declining
`cluster-compute` and `reactive-dispatch` is fully conformant — both are
Extended, and a deployment that declines both still passes Core in full.

## Self-certification

Self-certified on the same terms as every other Swarmada profile (ADR-0007):
the harness produces the result, the implementer publishes it, and there is no
certification authority, no gate, and no fee.
