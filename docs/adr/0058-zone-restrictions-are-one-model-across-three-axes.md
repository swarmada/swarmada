# ADR-0058: Zone restrictions are one model across three axes, enforced in the plane and obliged in the adapter

- **Status:** accepted
- **Date:** 2026-09-12
- **Deciders:** Maintainers (API, control plane, conformance)
- **Related:** RFC-0001 `FleetZone`, RFC-0001 *Control Plane Component Specifications* (Scheduler),
  RFC-0001 *Fleet Adapter Protocol*; ADR-0004 (in-place amendment of an `alpha` surface),
  ADR-0008 (spec document structure), ADR-0040 (zone bounds on `RegisterAck`, off-zone assignment
  refusal), ADR-0019 (adapter action discovery and validation), ADR-0032 (contract versioning and
  conformance gating), ADR-0013 (graceful drain), `docs/api-principles.md`,
  `rfcs/amendments/RFC-0001-zone-restrictions.md` (the chapter text this ADR decides)

## Context

### What a zone can and cannot say

`FleetZone.spec` carries `displayName`, `parentZone`, `waypoints`, `physicalBounds`, `edgeNode`,
`maxConcurrentRobots`, `sharedResources`, `estopPolicy` and `provisioningPolicy`
(`api/v1/fleetzone_types.go`). Every one of them is about **capacity, geometry, topology or an
emergency**. None of them means any of the following, and site policy everywhere is written in
exactly these three sentences:

1. **Entry** — may a robot be here at all.
2. **Capability** — may it perform *this job* here.
3. **Operating** — what may it have **running** while it is here: camera, microphone, water,
   chemicals, UV, speed, noise.

So an operator can state **capacity** in the fleet's vocabulary and have it enforced for everyone,
and can state **access** only inside whichever application happens to be dispatching. That
asymmetry is the defect.

The near-miss fields are worth naming, because each looks like the answer and is not.
`maxConcurrentRobots: 0` means the **opposite** of "no robots here" — the field's own documentation
says `0 = no limit`. `estopPolicy` models an emergency with a clear-and-resume lifecycle; a patio is
not an emergency, it is a standing fact about the building. `ZoneMaintenance` is a *window*, with a
scheduled start and a `Completed` phase; a permanent boundary modelled as a window that never
completes is indistinguishable from one that was forgotten. Labels and annotations are not schema,
are not validated, are enforced by nothing, and are invisible to anything reading the API rather
than the object.

### The provenance: a specification defect found by implementing the specification

This did not come from review. It came from a device.

A downstream integration — a private Matter adapter driving a consumer vacuum that exposes the
Matter `ServiceArea` cluster — was given an owner requirement during a calibration walk: one area
(outdoor space behind a patio fence) must never be entered. Two measurements followed, and both
matter here.

**Measured 2026-09-07.** The area was marked NO-GO in the vendor's own phone application. Immediately
afterwards, `ServiceArea.SelectAreas` for that same area returned `status: 0` and read back the
selection. The device accepted a selection of an area its owner had explicitly forbidden. **No
app-side map state — names, splits, virtual walls, no-mop, no-go — crosses to the Matter surface at
all.** The consequence that project drew is correct and is the load-bearing fact for this ADR: *if
the refusal is not enforced in the adapter, it is not enforced.* It refuses before a byte reaches
the device, and an unreadable or absent policy file refuses everything rather than defaulting
permissive.

**Measured 2026-09-12.** With the same deployment's zones restored into a live cluster and the patio
marked off-limits in the adapter's own record, the corresponding `FleetZone` read:

```yaml
spec:
  displayName: Patio
status:
  estopStatus: Clear
```

An accurate report of a zone carrying no restrictions. The restriction existed, the household had set
it deliberately, it was enforced at translate time — and **nothing in the cluster knew it.** The
integration had to stamp vendor annotations on the `FleetZone` purely so that a person reading the
cluster was not misled, and one of those annotation values says, in its own text, that the control
plane does not read them.

Both prior conclusions were right, and both were **adapter-local**. The half neither covers is that
**the fleet's own vocabulary cannot state the rule** — so every integrator re-invents it privately,
in a private JSON file, with private wording, exactly as this project did.

That is a specification defect discovered by implementing the specification, and it is the strongest
provenance a standard can have. It is not a feature request from a design document; it is the record
of a conformant implementation reaching a real deployment and finding that the standard had no
sentence for what the deployment needed to say. A reviewer should read the annotation values as the
measure of what this standard cannot yet express: while they exist, they are the gap.

### The forces in tension

- **The plane cannot switch a camera off.** Nothing in a control plane can physically stop an
  emission. A standard that promised enforcement of axis 3 would be lying.
- **The adapter cannot be the only enforcement point either.** The 2026-09-07 measurement shows the
  adapter is the *last* point at which a rule can be enforced for that device. It does not show it is
  the *only* one it belongs at. A rule that only the adapter knows is a rule `swarmctl`, `kubectl`, a
  second adapter, and any scheduler can walk straight through — all reachable today with no misuse of
  any API.
- **`parentZone` already exists**, so any rule placed on a zone has an undefined effect on its
  children until this decision defines one. Deny-inherits-down is the obvious rule and the dangerous
  one.
- **Admission is bypassable.** A webhook can be uninstalled, set to `failurePolicy: Ignore`, raced, or
  skipped entirely by an etcd restore. A safety-shaped invariant that exists only in a webhook is not
  an invariant.
- **The vocabulary of axis 3 is a standards problem, not a schema problem.** A closed enum is testable
  and will be incomplete within a year; an open string is extensible and enforces nothing.

## Decision

Adopt **one restriction model on `FleetZone`, covering all three axes**, enforced at **two** points,
with a vocabulary that is closed at the core and namespaced at the edge. The normative text is
`rfcs/amendments/RFC-0001-zone-restrictions.md`, staged for in-place folding into RFC-0001 per
ADR-0004 (`FleetZone` is `stage: alpha`). This ADR records the decisions that text encodes.

### 1. One model, three axes — not two features

`FleetZone.spec.restrictions` is a single block. A **restriction rule** is, in every axis, the same
tuple: *(declaring zone, subject, mode, parameters, reason)*. Inheritance, absorbency, precedence,
resolution, status projection, refusal vocabulary, admission validation and conformance are defined
**once over rules**, never per axis.

```yaml
spec:
  restrictions:
    entry:                                # axis 1 — at most one; presence is the denial
      mode: Denied
      reason: "outdoors behind the fence; no robot access"
    capabilities:                         # axis 2 — +listType=map +listMapKey=name
      - name: clean.mop
        mode: Denied
        reason: "carpet that must not get wet"
    operating:                            # axis 3 — +listType=map +listMapKey=subject
      - subject: camera
        mode: Denied
        reason: "patient privacy"
      - subject: speed
        mode: Limited
        max: "0.4"
        reason: "occupied ward at night"
      - subject: chemical
        mode: Limited
        allowed: ["quat-ammonium-7"]      # the allow-list case
        reason: "food preparation"
```

The three members exist because each has a **different natural map key** — none, a capability name, a
subject — and `docs/api-principles.md` requires a named list to be a `+listType=map`. The grouping is
a schema affordance. It is not three schemas, and every rule below applies to all three members
without restatement.

**Axis 3 is not a smaller axis 2, and the schema is what keeps them apart.** "May clean here" is
about the action; "must not have a camera running here" is about the robot's **state while present**,
true or false of a robot that was asked to do nothing at all. The case that decides it: *a robot
transiting a ward with a live microphone.* No action was refused, no capability was required, and the
constraint was still broken. Neither is "no water" the same as "no mop" — water is a consumable a
future robot may use for something that is not mopping, and a constraint written against the
capability misses it.

### 2. Rules only restrict. There is no `Allowed`, and absence is permission

`ZoneRestrictionMode` is `Denied | Limited | Required`. There is no `Allowed` value, at any axis.

A rule exists only to narrow. Absence of a rule is permission, so a zone that takes no position
writes nothing. This is not economy: it makes **a hole unwritable**. An operator cannot express
"re-permit here" at all, so the entire class of "a child opens a hole in a parent's boundary by
declaring `Allowed`" is removed at the schema, not caught at admission. An `Allowed` value would have
been a footgun in exactly the direction that matters — written in good faith, silently inert, and
believed.

`Required` (an inspection zone where a camera **must** be running) is admissible on `operating` only,
enforced by CEL. Including it costs a contradiction rule (decision 4); that cost is paid explicitly
rather than deferred, because refusing it later would be an API change and the mirror case is real.

### 3. Inheritance: down containment edges, and the effective rule is the most restrictive in the chain

A rule declared on a zone applies to that zone and **every descendant**, following `spec.parentZone`
only — never `spec.memberZones`. Composition asserts nothing physical and is a targeting relation;
estop propagation and shared-resource inheritance already follow containment alone, and a restriction
that crossed composition edges would let a group grant itself authority over zones it does not
contain.

For a given subject, the effective rule is **the most restrictive rule anywhere in the ancestor
chain, root-downward — not the nearest.** `Denied` beats `Limited` beats absence; between two
`Limited` rules on the same subject the tighter bound wins, and between two `allowed` lists the
intersection wins.

"Most restrictive in the chain" rather than "nearest ancestor wins" is the whole decision, and the
reason is that **resolution, not admission, is the enforcement.** A resolver that always errs
restrictive is correct on an object admission never saw: a zone restored from a backup, applied
before the webhook was installed, written while `failurePolicy: Ignore` was in effect, or raced past
the check. Admission is the diagnostic that tells an operator *now*; the resolver is what holds when
the diagnostic did not run.

### 4. Admission refuses a new hole. It never refuses a new wall

The two directions get deliberately different answers.

- **A descendant weakening an ancestor's rule** — a lower `Limited` bound raised, an `allowed` list
  widened, a `Required` written under a `Denied` or the reverse — is **rejected at admission**, with
  the ancestor zone and the conflicting rule named in the message. Not silently ignored. Silently
  ignoring produces an object whose `spec` says one thing and whose behaviour is another, and the
  entire point of this work is that the API is the place the rule is stated. The check costs one more
  walk of a chain the `FleetZone` webhook already walks for cycle detection.
- **An ancestor tightening onto an existing descendant** — a parent adds a denial a child already
  contradicts — is **admitted**. The contradiction resolves in favour of the restriction (decision 3),
  and the descendant publishes `RestrictionsConsistent: False` naming the ancestor, plus a Warning
  event. Refusing a parent's *tightening* on account of a child would make a boundary un-addable, let
  a leaf someone else owns veto a site policy, and require the webhook to list a whole subtree on
  every zone write. The direction of the asymmetry is what makes it safe: the unresolvable case is
  never the permissive one.

A `Denied`/`Required` contradiction on one subject resolves to `Denied`. A zone where a camera must
be off and must be on is a zone no robot may work in; resolving to `Denied` leaves the camera off and
the action declined, rather than switching a camera on in a patient room by operation of a merge rule.

### 5. Enforcement is in two places, because a rule enforced in one place is a rule with a bypass

**In the control plane — two candidate filters, in `internal/controller/dispatch_filters.go`.** They
belong in that file and not in `internal/scheduler` for the reason the file's own header gives: they
need a read of an object that is neither the robot nor the action — the `FleetZone` tree, which
`newDispatchFilterState` already lists once per call.

- **Candidate filter 11 — zone restriction admissibility.** *Action-scoped.* Resolves the target
  zone's ancestor chain and refuses the action outright when entry is denied for the target (or any
  ancestor of it), or when `spec.requiredCapabilities` intersects the effective denied-capability set.
  Because its answer does not vary by robot, a failure is **terminal for the action** — `Failed` with
  a `failureReason` naming the rule and the declaring zone — and not a `Pending` requeue. This is the
  one place the filter framing is escaped deliberately: leaving a standing, permanent refusal
  `Pending` would make "you told me never to go there" indistinguishable from "the fleet is busy",
  which is precisely the confusion the downstream adapter's refusal wording exists to prevent.
- **Candidate filter 12 — adapter operating-constraint support.** *Robot-scoped.* A robot whose
  serving adapter has not declared that it honours every effective `operating` subject in force in the
  target zone chain is **withheld**, and the action stays `Pending` — it is a fact about today's
  fleet, not a standing refusal. Evaluation is **fail-closed**, on the same footing as filter 9.

**In the adapter — declare, honour, attest, or decline.** The plane cannot verify that a camera is
off. So the obligation is shaped as something a suite can go red on:

- the **zone declares** the constraint;
- the **adapter declares** which subjects it can honour, in `CapabilitiesSnapshot`, projected
  read-only to `Robot.status`;
- the control plane **carries the resolved constraint set** on `AssignAction`;
- the adapter **honours** it and **attests** on the action's outcome which subjects it honoured;
- an adapter that **cannot** honour a constraint **declines the assignment naming the subject**,
  rather than accepting it and ignoring the constraint.

**A rule enforced in one place only is a rule with a bypass**, and the two bypasses are not
hypothetical. Without the plane, `kubectl`, `swarmctl`, a second adapter and any scheduler dispatch
into the denied zone unimpeded. Without the adapter, the plane has authorised an action it cannot
police and has no idea whether the microphone was on.

### 6. Both filters fail **closed** on a read error, explicitly unlike filters 2 and 8 beside them

`newDispatchFilterState` treats a failed `FleetZone` list as non-fatal and skips filter 2, on a stated
and correct rationale: a failed list proves nothing about any robot, and failing every candidate
closed would let one transient error idle a namespace.

**Filters 11 and 12 must not inherit that.** A failed list means the restriction set is
*unresolvable*, and an unresolvable restriction is not an absent one. The action reports
`ZoneRestrictionsUnresolved` and stays `Pending`; it is not dispatched. This is the same rule the
downstream adapter arrived at independently — an unreadable or missing policy file refuses everything
rather than defaulting permissive — and it is called out here because the surrounding code in that
file documents the opposite discipline, and a later implementer pattern-matching the file would get
it backwards.

For the same reason, an action whose `spec.zone` names **no existing `FleetZone`** is refused rather
than dispatched. In draft 4 `resolveZoneLeaves` resolves an unknown zone to itself, which was the right
compatibility choice when the only question was containment; once a zone name carries a restriction
set, a name that resolves to nothing is a name whose restrictions cannot be evaluated. Unknown is not
permitted.

### 7. Axis 3's vocabulary: a closed normative core plus a namespaced extension — and it is **not** the capability vocabulary

`subject` is a lowercase string. Without a `/` it MUST be one of the **normative core seven**:
`camera`, `microphone`, `water`, `chemical`, `uv`, `speed`, `noise`. With a `/` it is a namespaced
extension (`example.com/leaf-blower`), whose namespace is a DNS domain. Pattern plus CEL, not an
`Enum` marker, because the set is not closed — it is closed *at the core*, which is the same split the
standard already uses for capability identifiers. Conformance covers the core only.

**An unrecognised subject is not an absent subject.** An adapter that receives an `operating` subject
it does not recognise MUST decline the assignment. Without that rule the extension form is a bypass:
any site could silence a constraint by namespacing it.

**The argument that was rejected.** `FleetAction.spec.requiredCapabilities` already names capabilities
(`clean_area`), capability identifiers are already dotted and lowercase, adapters already declare
their capabilities through `CapabilitiesSnapshot`, and `FleetAdapter.spec.servesRobotClasses` sets a
precedent for matching on names. Reusing that vocabulary would have cost nothing to build and would
have reused the matcher, the registry and the operator's existing mental model. It is a category
error, on three counts:

- **A capability is an ability; an operating constraint is an actuality.** A capability answers *can
  this robot be asked to do X*. An operating constraint answers *is X happening right now*. The
  transiting robot with a live microphone has no required capability naming a microphone — there is no
  action, or the action is `navigate` — so denying capability `sensor.microphone` in the ward would
  eliminate robots whose *job* needs a microphone and would say nothing whatever about the robot
  carrying a hot one through.
- **Capability names are adapter-declared and vendor-extensible.** The set is whatever adapters
  report. A site-policy vocabulary must be authored by the **site**, before any adapter exists, and
  must mean the same thing across every vendor. A vocabulary whose membership is decided by the
  parties it constrains cannot constrain them.
- **Capability *status* is about health, not about running.** `Active`/`Degraded`/`Inactive` grades
  whether a capability works. It has no value meaning "switched on", and adding one would overload a
  field the Scheduler already reads for a different decision.

The shape is borrowed from capability identifiers; the vocabulary is not. `Robot.status.capabilities`
is never consulted to resolve a subject, and a subject that happens to collide with a capability name
means nothing.

### 8. Axis 2 carries `Denied` only. Limits live on axis 3

A capability rule is a denial, full stop — no `Limited`, no `limits` map. "May clean here, but at
0.4 m/s" is two rules, one per axis, and never one rule with a parameter block. Putting a speed bound
under a capability name would duplicate the axis-3 subject vocabulary in a second place, with a second
set of units, a second inheritance question, and a second conformance target — and it is exactly the
collapse of axis 3 into axis 2 that this model refuses in the other direction.

The seam is named rather than closed: if a genuinely capability-scoped operating limit is needed, the
**operating** rule grows an optional `whilePerforming: [capability]` selector. That is additive, it
keeps the subject vocabulary in one place, and it does not require axis 2 to grow parameters.

### 9. Five refusal shapes, measured against the four the downstream adapter already ships

Refusal wording was not invented here. A private household deployment already had four refusal kinds
in production against a real device and a real family, distinguished by a machine-readable `kind` so a
surface can tell them apart without parsing prose. They were designed against the case where a person
is standing in a room asking why the robot is not moving. The mapping is deliberate:

| Adapter refusal (shipped) | RFC-0001 `failureReason` | Carried over |
| :-- | :-- | :-- |
| `off-limits` — *"…is marked off limits — {reason}, so I will not send the robot there, on its own or as part of anything else."* plus a divergence notice | `ZoneEntryDenied` | The declaring zone and the operator's `reason`; **"on its own or as part of anything else"** is the absorbing property, restated as: the refusal holds whether the zone is the target or is reached through an ancestor; and the honest limit — a control-plane refusal is not a guarantee about the floor |
| `no-mop` — names the room, and **offers the way through**: *"Ask me to clean it with Quick and I'll do that."* | `ZoneCapabilityDenied` | Names the capability and says the zone is **not** closed — one capability is. A capability denial that read like an entry denial would cost the operator the room |
| `unconfirmed` — *"…is on the robot's map but has not been confirmed… it is listed, not dispatchable."* | `ZoneRestrictionsUnresolved` | The fail-closed case. Unknown is not permitted, and the message must say which resolution failed rather than implying a decision was made |
| `unknown-zone` — *"I don't know a zone called 'X'. I can clean: …"*, offering the list rather than guessing | `ZoneUnknown` | A target that names no `FleetZone` is refused, and the refusal enumerates the zones that exist. Ambiguity asks; it never guesses |
| (no counterpart — the adapter's axis 2 is `no-mop`) | `ZoneOperatingConstraintUnmet` | New. Names the subject **and** the adapter, and distinguishes *cannot* honour from *did not* |

The fifth shipped behaviour is not a refusal at all, and it is the one most worth importing. A
**partly-undeliverable bundle** — a room that is several areas, some reachable — is *dispatched* with
the exclusion **named before the human confirms**, because refusing a whole room over a strip under a
bed would make it permanently uncleanable for a reason the household can fix by moving furniture. But
an **off-limits** member refuses the whole request and is never trimmed, because dropping an area
silently is how a household comes to believe a room was cleaned that was never selected — on a device
that reports a refused run byte-identically to a completed one.

That distinction — *a standing decision refuses the whole request; a fact about today narrows it and
says so* — is carried up verbatim:

- The target zone, or any ancestor of it, denies entry → **refuse the action**. Absorbing.
- The target zone's subtree contains a denied descendant → **narrow the resolved leaf set and name
  every excluded zone on the action's status**. Never silently.
- Narrowing empties the leaf set → **refuse**, with the subtree-exhausted detail.

### 10. Robots already inside a zone whose restrictions tighten: `Report`, and nothing else at v0.4

`entry.occupantAction` is `Report | Graceful | Immediate`, defaulting to `Report`, and **only
`Report` is specified as implemented.** `Graceful` and `Immediate` reuse `ZoneMaintenance`'s existing
wind-down vocabulary and machinery rather than inventing a second spelling of the same choice, and are
reserved.

`Report` is the v0.4 answer because a control plane that starts moving robots in response to an edit
of a `spec` field is a new autonomous motion path, and the central honest claim of this whole design
is that it is **not a safety system**. A policy edit that commands motion is a larger claim than the
requirement makes.

**And `Report` must not become an RA-1 violation.** "Name the robots inside a zone at
the moment it is denied" is a status write whose input is position telemetry, which is exactly the write
`docs/api-principles.md` and AGENTS.md prohibit per tick. So `Report` emits a **one-shot Warning event
naming the occupants at the moment the rule takes effect**, and adds **no occupancy list to
`FleetZone.status`**. The standing view is already `status.robotCount`, which is a throttled,
material-transition projection. There is no new per-tick write anywhere in this design.

### 11. The restrictions stay in `spec`, and the boundary is exactly as strong as write access to the zone

A denial any writer can flip is not much of a boundary, and this decision does not pretend otherwise.
Kubernetes CRDs admit only `status` and `scale` subresources, so there is no way to give
`spec.restrictions` its own RBAC without making it a separate object — and a separate object costs the
property that makes `FleetZone` worth reading: one `get` answers "what is this zone".

So: it stays in `spec`; the control is the admission rule that no write may open a hole (decision 4),
an event on every restriction change for audit, and RBAC on `FleetZone` itself. **Stated plainly: a
principal who may write the declaring `FleetZone` may delete the wall at its source, and nothing in
this design prevents that.** A separate `ZoneRestriction` object is the named deferred option, and the
thing that would force it is a namespace where the zone author and the policy author are different
people.

### 12. An attestation is a claim, not a measurement, and the spec says so

An adapter asserting "camera off" has asserted something. It has not measured anything, and the
control plane cannot. A false attestation is **both** a conformance failure (the suite can construct
the case) and a security event (the audit trail is what makes it actionable later) — it is recorded as
both rather than being filed as only the cheaper one.

This is the sentence that keeps the standard honest: **declare, honour, attest, or decline is
checkable; "enforced" would be a lie.** What conformance can go red on is the *decline* — dispatch
into a zone carrying a constraint the adapter does not implement, and require a refusal naming the
subject.

## Alternatives considered

**Two features, two schemas — `spec.access` and `spec.operatingConstraints`.** The shape the two
source requirements were filed in. Rejected: inheritance, absorbency, precedence, the resolver, the
status projection, the admission rules and the refusal vocabulary are identical across the three axes,
and two blocks would have specified each of them twice and then drifted. The thing that is genuinely
different between the axes is the *enforcement point*, and that is a property of the rule, recorded on
the rule.

**One flat `rules[]` list with a subject-kind discriminator.** Maximally uniform, and it makes the
"one model" claim structurally obvious. Rejected on the house convention: a flat list has no natural
map key, so it needs either an operator-invented rule name on every rule — including the singleton
entry rule, where it is pure ceremony — or `+listType=atomic`, which loses server-side-apply merge and
makes two controllers or two operators editing different rules clobber each other. The three members
each have a natural key. The uniformity is preserved in the *semantics*, which are defined over the
resolved rule set and never per member.

**Express axis 3 in the capability vocabulary.** Argued at length and rejected in decision 7:
ability is not actuality, an adapter-declared vocabulary cannot bind the parties that declare it, and
capability *status* grades health rather than running.

**Per-capability denial only, using `maxConcurrentRobots: 0` for whole-zone denial.** Cheaper, and
known to be wrong: `0` is documented as *no limit*, so setting it to forbid entry declares the zone
unlimited. There would be no way to state whole-zone denial at all.

**A separate `ZoneAccessPolicy` object.** Its own RBAC is the real argument for it, and it remains the
right answer for a multi-tenant namespace. Rejected for draft 4: it costs a second object to look at when
asking what a zone is, and it splits the ancestor walk across two kinds. Named as the deferred option
in decision 11 rather than dismissed.

**A free-form label set the adapter interprets.** Cheap, and it standardises nothing. Two vendors
will read `no-camera` differently and neither will be wrong.

**Model it as a never-completing `ZoneMaintenance` window.** Rejected: a permanent boundary modelled
as a window that never completes is indistinguishable from a window someone forgot to close, and
`autoResumeAfterMinutes` makes the failure mode "the boundary reopens by itself".

**Leave it entirely in the adapter.** The 2026-09-07 measurement proves the adapter is the *last*
line; it does not prove it is the *only* place the rule belongs. Rejected: it leaves the rule
unenforced against `kubectl`, `swarmctl`, a second adapter and any scheduler, unstateable by an
operator, and untestable by conformance — because there would be no rule in the API to test.

**Defer to a future policy engine.** Correct eventually, and it leaves a care setting, a
restricted-media site and a household unable to state the rule at all in the meantime.

## Consequences

**What this buys.** An operator states a site rule once — "no cameras in patient rooms" — in the
fleet's own vocabulary, and it holds for every robot from every vendor, including one integrated next
year by someone who never read the site's policy document. That is what a vendor-neutral fleet layer
is for. No stack owns this primitive today: application tooling expresses it per application, hardware
stacks express it per robot, and neither can state it **per place**, which is how every site policy in
the world is actually written.

**New obligations created.**

- Two new candidate filters, and the RFC's filter roster grows from ten to twelve. The maturity table
  in the Scheduler chapter must gain two rows, and both start as *specified, not implemented*.
- Three CRD surfaces: `FleetZone.spec.restrictions`, `FleetZone.status.restrictions` (resolved
  effective set plus `inheritedFrom`), and two conditions (`RestrictionsResolved`,
  `RestrictionsConsistent`). All additive and optional, so no existing object changes meaning and no
  API version bump is required.
- Three `fleet_adapter.v1` additions, all additive with new field numbers:
  `CapabilitiesSnapshot.honoured_operating_constraints`, a resolved constraint set on `AssignAction`,
  and a new `AssignActionRejection` value plus the subject list on `AssignActionResult`. Per
  `docs/api-principles.md` these are additive within `v1`; they are **not** a breaking change and do
  not move to `fleet_adapter.v2`.
- A new adapter conformance section and a matching control-plane one, with the registry bumped in
  lockstep — the `*0.5` self-checks fail on drift between catalog and registry, by design.
- The `FleetZone` admission webhook grows an ancestor-chain restriction walk beside the cycle walk it
  already performs.

**Drawbacks accepted.**

- **This is not a safety system, and saying so is part of the design.** A control-plane refusal cannot
  stop a robot driven by its own vendor app, its own buttons, a schedule, or a person carrying it. The
  2026-09-07 measurement is exactly that: seven app-side operations, none of which reached the wire.
  The claim made here is narrower and still worth making — an operator should be able to state a
  boundary **once**, in the fleet's vocabulary, and have every path *through the fleet* respect it.
- **Attestation is unverified by construction.** Decision 12 accepts this and converts it into
  something testable rather than papering over it.
- **The boundary is only as strong as `FleetZone` write access** (decision 11), until and unless a
  separate object earns its keep.
- **A terminal refusal from filter 11 is a behaviour change for an action already in flight** when a
  restriction is added mid-`Pending`. That is the correct outcome and it will surprise someone: an
  action queued before the wall went up fails against the wall, rather than waiting for it to come
  down.
- **Two `Limited` bounds are intersected, not overridden**, so a deep zone tree can become
  arbitrarily restrictive in a way no single object shows. `status.restrictions` with `inheritedFrom`
  exists precisely so an operator can see the resolved answer without walking the tree by hand; that
  projection is now load-bearing rather than a convenience.

**Seams left for future change.** `whilePerforming` on an operating rule (decision 8); time-scoping
for quiet hours — which should select **between** zone policies rather than putting a schedule inside
a rule, because the schedule machinery already exists; `Graceful`/`Immediate` occupant handling
(decision 10); and a separate `ZoneRestriction` object with its own RBAC (decision 11).

## Amendment 1 — what candidate filter 12 withholds, and what withholding does not protect against

- **Date:** 2026-09-13
- **Basis:** ADR-0004's second rule — `FleetZone` is `alpha`, RFC-0001 is in `Draft`, filter 12 is
  *specified, not implemented at v0.4*, and this is additive normative text on an unimplemented
  surface. Per `docs/adr/README.md` the text above is left intact and this is appended.
- **Sources:** the pre-implementation ruling requested before the Part D wire surface is written,
  so that the Scheduler filter and the adapter obligation are built against a settled answer.
- **What is unchanged:** decision 5's filter 12 and decision 6's fail-closed discipline. **This
  amendment does not narrow what filter 12 withholds.** It settles one reading decision 5 left
  open, adds one reporting obligation, and states out loud what the chosen reading does not cover.

### A1.1 — The question

FA-R1 says an adapter that declares no honoured operating subjects *"is not in breach; its robots
are withheld from zones that restrict a subject."* An adapter built against contract
`1.1.0` **cannot** declare anything: `CapabilitiesSnapshot.honoured_operating_constraints` does not
exist in its build. It is inside the ADR-0032 compatibility skew, fully conformant for the contract
version it reports, and under a literal FA-R1 it is withheld from every zone carrying any operating
rule — permanently, with no operator action that lifts it short of a vendor rebuild. Two readings
of filter 12 were on the table:

- **(a)** withhold any robot whose adapter has not declared the subject, **including** one that
  declared nothing because it predates the field;
- **(b)** withhold only a robot whose adapter declares *some* subjects and not this one, treating
  an adapter that declares nothing as not participating in the feature.

### A1.2 — Ruling: (a). (b) is refused

Normative. An implementation of candidate filter 12 conforms only if it behaves as follows:

> **Candidate filter 12 withholds a candidate robot when, for any effective `operating` rule in
> force in the target zone chain, the robot's `status.honouredOperatingConstraints[]` does not
> contain that rule's `subject`. An empty or absent `honouredOperatingConstraints[]` satisfies no
> subject.**
>
> An adapter that declares nothing is **not** in breach of FA-R1 and is **not** marked
> non-conformant; its robots are withheld on exactly the same footing as a robot whose adapter
> declares other subjects and not this one. There is no reading under which the absence of a
> declaration is treated as participation, as an implicit claim, or as a reason to dispatch.
>
> **No configuration, annotation, feature gate, `SwarmadaConfig` field or per-zone override may
> relax this.** A restriction with an off switch is not a restriction.

Why (b) is refused, in this ADR's own terms:

1. **(b) is the permissive resolution of an unresolvable case.** Decision 4 states the asymmetry
   the whole design turns on — *"the unresolvable case is never the permissive one"* — and decision
   6 requires filters 11 and 12 to fail closed precisely where the surrounding code in
   `dispatch_filters.go` fails open. An adapter that declares nothing has told the plane nothing
   about whether it can honour the rule. That is the unresolvable case, and (b) resolves it
   permissive.
2. **(b)'s hole is aimed at the founding case, not at an edge.** Decision 1's deciding example is
   *a robot transiting a ward with a live microphone.* An adapter that models no operating subjects
   at all is precisely the adapter most likely to have a microphone it does not model and cannot
   switch off. (b) dispatches that robot into that ward. It turns the case that justifies axis 3's
   existence into the axis's blind spot.
3. **The filter's own sentence already answers it.** *"A robot that cannot prove it honours a
   constraint has not honoured it."* An empty declaration is not a proof of anything.

### A1.3 — (a)'s cost is accepted and made legible, not mitigated

(a)'s cost is real and is stated here rather than discovered later: on the day an operator writes an
operating rule, every robot whose serving adapter predates
`CapabilitiesSnapshot.honoured_operating_constraints` is withheld from that zone, and only a
rebuilt or replaced adapter lifts it. Across a mixed fleet, the zone is undispatchable from the
moment the rule is written.

That cost is **not** reduced by weakening the filter. It is reduced by making it diagnosable, and
one obligation is added for that. Normative:

> **Filter 12 MUST record, for each withheld robot, the `subject` that was not declared and which
> of two causes applies:**
>
> - **`AdapterDeclaresNoOperatingSubjects`** — the robot's `status.honouredOperatingConstraints[]`
>   is absent or empty. The adapter has not opted into the feature at all; the usual reason is that
>   it was built against a contract version in which the field does not exist.
> - **`OperatingSubjectNotDeclared`** — the list is non-empty and does not contain this subject.
>   The adapter has opted in and excluded this subject.
>
> The two causes withhold **identically**. They are distinguished because the operator action that
> resolves them is different: the first needs a rebuilt or replaced adapter and no amount of
> waiting helps, the second needs the subject added to an adapter that already models subjects.
> Reporting both as an undifferentiated `Pending` leaves an operator unable to tell *"no robot in
> this fleet can ever take this action"* from *"the fleet is busy"* — the one confusion decision 5
> refuses to leave filter 11 in, and it is no more acceptable at filter 12 only because the
> outcome there is `Pending` rather than terminal.
>
> Both causes are written on the `FleetAction`'s status alongside the withheld-candidate reporting
> the filter already performs. **Neither adds a per-tick status write** (RA-1, decision 10): the
> cause is a property of the candidate set at the moment the filter runs, recorded on the same
> material-transition path the existing reporting uses.

A second clause, on a surface filter 12 does not own, is stated here because this is where the
reasoning lives. Normative, binding the `FleetZone` admission path:

> **When a `FleetZone` write adds or tightens an `operating` rule, the admission path SHOULD emit a
> one-shot Warning event naming the subject and the number of robots then in the zone's scope whose
> adapters do not declare it.** Same shape and the same machinery as decision 10's `Report` and
> decision 4's contradiction warning: one event at the moment the rule takes effect, no occupancy
> list on `status`, no new per-tick write anywhere. This is the cheapest place to surface the
> mixed-fleet cost, because it is the last moment at which the operator can still decide
> differently. It is `SHOULD` and not `MUST` because a plane that cannot resolve the candidate set
> at admission time must still admit the write — decision 4 never refuses a new wall.

### A1.4 — What this reading does **not** protect against

Stated as its own section so it can never be the silent half of the ruling:

> **Filter 12 withholds a robot from being assigned work in a restricted zone. It does not stop the
> robot.** An adapter that declares nothing is refused dispatch *through the fleet* into a zone that
> forbids a live microphone, and is not prevented from entering that zone under its own vendor
> application, its own buttons, a schedule, a stored map, or a person carrying it — with the
> microphone running. That is the exact case decision 1 says axis 3 exists for, and filter 12 does
> not cover it. Decision 5 already records why: a rule enforced in one place is a rule with a
> bypass, and the plane is one of the two places, not the boundary.
>
> **Nor does it protect against a false declaration.** A robot that passes filter 12 has an adapter
> that *claims* the subject. Decision 12 governs: an attestation is a claim, not a measurement, and
> the plane cannot verify it. Passing filter 12 means the plane did not knowingly dispatch into a
> constraint that no adapter claims to honour. It means nothing more than that.
>
> **And the refused reading's hole is named here too,** because it was considered and rejected
> rather than missed: had (b) been taken, an adapter that opted out of the feature entirely would be
> dispatched into a zone that forbids a live microphone. Refusing (b) is what buys protection
> against that, and it is bought at the price in A1.3 — a mixed fleet in which pre-Part-D adapters
> are withheld from every operating-restricted zone until they are rebuilt.

### A1.5 — What an implementation of filter 12 must therefore do

1. Read `status.honouredOperatingConstraints[]` and treat absent and empty identically: satisfying
   no subject.
2. Withhold the robot, leave the action `Pending`, and never make the action terminal — filter 12's
   answer is a fact about today's fleet, unlike filter 11's.
3. Fail closed on every read failure, on the same footing as filter 9 and per decision 6.
4. Record the subject and one of the two causes in A1.3 for every withheld robot.
5. Expose no switch of any kind that changes any of the above.

## Amendment 2 — filter 12's scope is the resolved leaf set, not the target zone chain

- **Date:** 2026-09-13
- **Basis:** ADR-0004's second rule — `FleetZone` is `alpha`, RFC-0001 is in `Draft`, and this is a
  scope correction to normative text on an `alpha` surface. Per `docs/adr/README.md` the text above
  is left intact and this is appended.
- **Sources:** a defect report filed against the landed filter 12 by the session that implemented
  it. The implementation was correct against decision 5 and Amendment 1 as written; the specified
  scope was wrong.
- **What is unchanged:** Amendment 1 §A1.2's ruling on what a non-declaring adapter means, decision
  6's fail-closed discipline, and filter 12's `Pending` (never `Failed`) outcome. **This amendment
  does not narrow what filter 12 withholds.** It widens where it looks.

### A2.1 — The hole decision 5 left

Decision 5 and Amendment 1 §A1.2 both scope filter 12 to *"every effective `operating` rule in force
in the **target zone chain**"*. A zone's `status.restrictions.effective[]` is the fold of **itself and
its ancestors** (FZ-R1, FZ-R4) — it does not contain rules declared by its **descendants**. So filter
12, implemented exactly as specified, never saw an operating rule declared *below* the target.

Candidate filter 2 meanwhile admits robots from the whole **subtree**: it matches
`Robot.status.currentZone` against the resolved leaf set of the action's zone, which is every leaf at
or below the target. The two together leave the following:

| Object | State |
| :-- | :-- |
| `FleetAction.spec.zone` | `ward` |
| `ward` | declares no operating rule; effective set empty |
| `ward/icu` (leaf, child of `ward`) | declares `operating: [{subject: camera, mode: Denied}]` |
| `amr-1` | `status.currentZone = ward/icu`, adapter declares **no** operating subjects |

Filter 11 admits the action — `ward` denies no entry, and the descendant is not entry-denied, so no
narrowing occurs. Filter 2 admits `amr-1`, because `ward/icu` is in the leaf set. Filter 12 reads
`ward`'s empty projection and withholds nobody. **A robot is dispatched into the ICU with a camera.**
That is decision 1's own deciding case, arriving through the filter that exists to prevent it.

**A rule that one filter admits past and another cannot see is a rule with a bypass**, which is the
precise thing decision 5 exists to prevent. Decision 5's own sentence — *"a rule enforced in one
place is a rule with a bypass"* — is about the plane and the adapter; this is the same defect one
level down, inside the plane, between filter 2 and filter 12.

It also inverts the model's incentive. An operator who writes the rule on the **most specific** zone —
the obvious place, and the one `inheritedFrom` and FZ-R7 train them to use — got the **weakest**
enforcement.

### A2.2 — Ruling: the scope is the resolved leaf set, evaluated per (candidate, leaf)

Normative. An implementation of candidate filter 12 conforms only if it behaves as follows:

> **Filter 12's rule set is scoped to the action's RESOLVED LEAF SET** — the same set candidate
> filter 2 admits robots from, after filter 11's narrowing — **and not to the target zone's own
> chain.** A candidate robot is judged against **the effective `operating` rules in force at the
> zone it occupies**: `Robot.status.currentZone`, which filter 2 has already proven is in that set.
>
> **The evaluation is per (candidate, leaf), not per target and not per zone.** A robot is eligible
> for a leaf when its `status.honouredOperatingConstraints[]` contains every `operating` subject in
> force **at that leaf**. A leaf therefore leaves the reachable set **only when no candidate can
> honour it**; the same leaf may drop out for one candidate and be kept for another. This is
> deliberately **not** filter 11's shape: filter 11's narrowing is zone-scoped, so an entry-denied
> leaf is out for everyone, because its answer does not vary by robot. Filter 12's does.
>
> **No rule sets are merged.** A leaf's `status.restrictions.effective[]` is already the fold of
> itself and its ancestors (FZ-R1, FZ-R4), so evaluating against the leaf **already includes the
> target chain**. An implementation that unions two rule sets has reintroduced resolution inside the
> filter, which FZ-R4 forbids and FZ-R9 says drifts to nearest-ancestor-wins.
>
> **When every leaf drops out**, the refusal is `ZoneOperatingConstraintUnmet` — still `Pending`,
> never `Failed`, per §9 and Amendment 1 §A1.5 — and it **MUST name which subject and which zones
> it is in force in**, the way filter 11's subtree-exhausted refusal names every narrowed zone. The
> declaring zone is very often not the target, and an operator who cannot tell which zone caused the
> hold will go hunting for an adapter setting.
>
> **No configuration, annotation, feature gate, `SwarmadaConfig` field or per-zone override may
> relax or restore the narrower scope.** Amendment 1 §A1.2's prohibition applies unchanged.

A robot the action **pins** (`spec.robotSelector`) skips filter 2 per RFC-0001 filter 6 and may
therefore stand outside the subtree. It is **not** exempt from filter 12 — whether a vendor's
adapter honours a camera constraint is not a judgement an operator can assert into being — and it is
judged against the **target's own chain** as the floor, which is what every candidate was judged
against before this amendment.

### A2.3 — The three refused alternatives, on the record

1. **Union the target's projection with every leaf's.** Fail-closed and cheap: the leaf set is
   already computed for filter 2 in the same call. Refused because it makes a large target zone
   undispatchable for a rule on one closet, and because it destroys the property filter 11 is
   careful to preserve and states out loud in its own refusal — *"the zone is NOT CLOSED"*. A rule
   on one leaf must not close its siblings.
2. **Evaluate against the robot's own `currentZone` only, with no leaf-set framing.** Sharper, and
   the difference from the ruling is small in code — but it is defeated by the very case the axis
   exists for. It says nothing about a zone the robot **transits**, and decision 1's deciding
   example is a robot *transiting* a ward. The ruling does not close that either (§A2.4), but it
   does not pretend the robot's current zone is the whole question.
3. **Leave it and state the limit.** Refused as a disposition, but the *stating* half is kept and is
   §A2.4 below: §A1.4 already commits to naming what the filter does not protect against, and an
   unnamed limit is the one an operator discovers in production.

The ruling is also the only one consistent with this ADR's own thesis. Axis 1 already handles a
restricted descendant by narrowing the leaf set and naming every removed zone. **One model across
three axes means axis 3's descendant story is the same shape as axis 1's**, not a new one — differing
only where the axes genuinely differ, which is that filter 12's answer varies by robot and filter
11's does not.

### A2.4 — What the widened scope still does **not** protect against

§A1.4 is extended, not replaced. Everything it names still holds: filter 12 withholds a robot from
being **assigned work** and does not stop the robot, and a robot that passes has an adapter that
only **claims** the subject. Added:

> **The widened scope does not cover a zone the robot CROSSES on its way to the leaf.** The control
> plane does **no path planning**. It knows where a robot is (`status.currentZone`) and which leaves
> the action may reach; it does not know, and has no surface on which to learn, which zones the
> robot will pass through between the two. **A transit corridor is not a zone this filter can see.**
>
> So a robot standing in a leaf that restricts nothing, dispatched to work in that leaf, may cross a
> sibling leaf that denies a camera — with the camera running — and filter 12 will have said
> nothing about it. This is not an implementation gap that a later release closes by looking harder
> at the same data: it is absent input. Closing it needs a route, which the plane does not have, or
> enforcement at the boundary, which is decision 5's other half — **the adapter**. FA-R1 and FA-R2
> are where a transiting robot is actually covered, and they are covered there by the adapter
> honouring the constraint for the duration of the assignment rather than by the plane predicting a
> path.
>
> **Nor does it cover a robot whose `currentZone` is stale or absent.** An absent `currentZone` is
> excluded by filter 2 (RFC-0001 #fleetzone-zone-controller-current-zone-derivation, "Between-zone positions"), so it does not reach filter 12
> at all — but a robot whose projected zone lags its physical position is judged against the rules
> of the zone the plane believes it is in. That is the same eventual-consistency limit filter 2
> carries, and this amendment neither worsens nor repairs it.

### A2.5 — What an implementation must therefore do

1. Resolve the rule set from **every zone in the leaf set filter 2 uses**, after filter 11's
   narrowing, plus the target itself as the floor for a pinned robot outside the subtree.
2. Judge each candidate against **the rules in force at `status.currentZone`**, a lookup and not a
   merge.
3. Drop a leaf only when **no candidate** can honour it; never remove a leaf from filter 2's set on
   filter 12's account.
4. Name **the subject and every zone it is in force in** on the `ZoneOperatingConstraintUnmet`
   refusal, and keep the action `Pending`.
5. Expose no switch that restores the narrower scope.

Everything in Amendment 1 §A1.5 continues to apply unchanged.

## Amendment 3 — §12's "recorded as both" describes a mechanism with neither half built; FA-R4 is withdrawn as a MUST

- **Date:** 2026-09-14
- **Basis:** ADR-0004's second rule — `FleetZone` is `alpha`, RFC-0001 is in `Draft`, and this
  narrows a claim about a mechanism rather than reversing a decision. Per `docs/adr/README.md` the
  text above is left intact and this is appended.
- **Sources:** the tracker item filed against FA-R4 having no wire surface, and the ruling session
  that took it rather than deferring again.
- **What is unchanged:** §12's first and governing claim — **an attestation is a claim, not a
  measurement** — and everything decisions 1–11 say. Amendments 1 and 2 are unaffected.

### A3.1 — The sentence, and what is actually behind it

§12 states:

> A false attestation is **both** a conformance failure (the suite can construct the case) and a
> security event (the audit trail is what makes it actionable later) — it is recorded as both rather
> than being filed as only the cheaper one.

**Neither half is built, and the second cannot be built from where the first would land.**

- **The conformance half.** The suite cannot construct the case, because there is no attestation to
  falsify. `adapters/CONFORMANCE.md` C17 says so in its own *"What is not checked here:
  attestation"* section, and the reason it gives is narrower than unverifiability:
  `fleet_adapter.v1` carries no field for one. `ActionStatusUpdate` has `action_id`, `state`,
  `message`, `fencing_token`, `progress_pct`, `end_cause` and `superseding_action_id`, and no
  honoured-subject list; no other terminal message has one either.
- **The audit half.** There is **no action-terminal-outcome audit event in this tree at all.** The
  chain records `FLEETACTION_CANCELLED`, `ACTION_REQUEUED_BY_MAINTENANCE` and
  `ACTION_PAUSED_BY_ESTOP`. An action succeeding or failing records nothing, so there is no entry an
  attestation could ride and nothing for a later investigation to read. `FleetAction.status` carries
  no honoured-subject field either.

So the sentence names a *disposition* — file it as both, not as the cheaper one — for an input that
does not exist, into a sink that does not exist.

### A3.2 — Ruling: FA-R4 is withdrawn as a MUST, and §12's recording claim is narrowed to an intent

Normative:

> **FA-R4 is not a MUST at v0.4.** The intent — that an adapter reports which subjects it honoured
> on an action's terminal outcome — is recorded in the amendment's Part D, together with the three
> conditions under which it becomes a MUST again. It is not an obligation until then.
>
> **§12's "recorded as both" is an INTENT, not a description of shipped behaviour.** What §12 states
> unconditionally, and what this amendment does not touch, is that an attestation is **a claim, not
> a measurement**, and that the plane cannot verify it. What it may not be read as asserting is that
> a false attestation is recorded anywhere *at v0.4*, because v0.4 has no attestation and no
> record.

### A3.3 — Why a wire field was not added to rescue the sentence

The cheap repair was an additive repeated `honoured_subjects` on `ActionStatusUpdate` — one minor
bump, no behaviour change for an adapter that ignores it. It was rejected, and not on cost.

**A field written by every conforming adapter and read by nothing is not an audit trail; it is the
appearance of one.** The only check it would enable grades *arrival and shape*, so a green row would
mean *an adapter sent a list* and would be read as *the plane holds an attestation record*. That is
the same defect class as a normative text promising an event that is emitted nowhere — which this
ruling session struck from three places on the same day, for the same reason. Building the cheaper
half of §12 while leaving the expensive half absent would have made the over-claim harder to see,
not smaller.

**The order matters and is recorded so the next attempt does not start at the wrong end: the SINK
comes first.** A terminal-outcome audit event is what makes an attestation actionable; the wire
field is what fills it. Adding the field first produces a message nothing consumes, and a
specification that then has to explain why its own MUST is unobservable — which is the position this
amendment is correcting.

### A3.4 — What is given up, stated rather than implied

The audit trail §12 relies on. That is a real loss and it is the reason this was a decision rather
than a tidy-up: at v0.4 there is **no mechanism at all** by which an adapter's compliance with an
operating rule is recorded, and therefore none by which a false claim of compliance could be
investigated afterwards. The plane does not knowingly dispatch into a constraint no adapter claims
to honour (filter 12), the adapter must decline one it cannot honour (FA-R5), and after that the
record ends.

Writing that down is the point. The alternative on offer was a field that would have let the same
sentence stand while changing nothing about what is actually recorded.

## Amendment 4 — `Required` with `Limited` carries both; time windows are refused for draft 4

- **Date:** 2026-09-26
- **Basis:** ADR-0004's second rule — `FleetZone` is `alpha`, RFC-0001 is in `Draft`, and both
  rulings record what the resolver already does or what the schema already lacks. Neither changes
  behaviour. Per `docs/adr/README.md` the text above is left intact and this is appended.
- **Sources:** two questions this ADR left open and a ruling session answered on the same day: the
  fourth mode pair decision 3 never names, and the time dimension ADR-0059 §6 names as a seam.
- **What is unchanged:** decisions 1–12 and Amendments 1–3. FZ-R5 is not amended.

### A4.1 — `Required` and `Limited` on one subject: carry both

Decision 3 and FZ-R1 order `Denied` against `Limited` and one `Limited` against another. Decision 4
and FZ-R3 settle `Denied` against `Required`. **The fourth pair was named nowhere**, and it is
reachable with no misuse of the API: an inspection bay declares `uv: Required`, and a zone above or
below it declares `uv: Limited, max: "5"`. Both are admissible, and FZ-R7 does not settle the pair,
because "more permissive" is the undefined question.

Normative:

> **`Required` and `Limited` on one (axis, subject) in one chain resolve to `Required`, and the
> resolved rule keeps the bound**: the tighter `max`, or the intersection of the `allowed` lists,
> exactly as between two `Limited` rules. The subject must be running, and within those limits. The
> pair is **not** a contradiction and emits no `RestrictionContradiction`. The order of the two in
> the chain does not change the answer.

`Required` is not a point on the `Denied`/`Limited` scale. `Limited` narrows what may happen;
`Required` mandates it. They are two statements about one subject, so neither is dropped. Carrying
both is the only reading that cannot emit a rule weaker than one it read, which is FZ-R2.

This is what the resolver already does, and a test now pins it in both chain orders and for an
allow-list. The three alternatives are refused on the record:

- **A contradiction, like `Denied` with `Required`.** It would make an ordinary site policy ("UV
  must run here, and never above 5") unwritable, and a contradiction resolves to `Denied`, which
  would forbid the very subject the zone mandates.
- **`Required` as orthogonal to the bound.** It would let a `Required` written below a `Limited`
  discard the ancestor's bound, which is a hole FZ-R7 exists to refuse.
- **Forbid the pair at admission.** Every zone author below a bound would then be unable to mandate
  the bounded subject, which is the inspection-zone case §2 admits `Required` for.

### A4.2 — Time windows: refused for draft 4, recorded rather than implied

ADR-0059 §6 states *"THERE IS NO TIME DIMENSION"* and names the seam: an optional window field
whose absence keeps today's meaning, plus a wake-up that a later decision must take deliberately.
The schema half is nearly free. The wake-up is the decision, and it was taken:

> **Draft 4 does not support time-windowed restrictions.** A restriction is in force from the
> moment its projection is published until a principal edits `spec`, and at no other boundary. No
> window field is added. *"No camera recording in this ward between 22:00 and 06:00"* is
> inexpressible, and a deployment that needs it writes the always-on rule, which is stricter than
> the policy, or edits `spec` at the boundaries itself.

**Why refused rather than built.** Each of the three wake-ups costs a property this ADR or its
neighbours state as normative:

- **A timer in the Zone Controller** makes `status.restrictions` a function of time. FZ-R5 says it
  is recomputed on a spec or topology change and never on a telemetry tick, and a clock is neither.
  FZ-R5 would have to admit a third trigger.
- **Resolving the window at read time**, in filters 11 and 12 and in the push, keeps FZ-R5 intact but
  puts a second evaluator of one rule in several places, which is the drift FZ-R4 exists to prevent.
- **Carrying the window to the adapter** removes the plane's clock and makes the adapter the thing
  that knows what time it is, across a fleet whose clocks nothing in this specification
  synchronises.

None of these is wrong. Each needs a normative change that draft 4 has no evidence to make, and
the refusal keeps every rule a deployment writes today meaning exactly what it says.

**Carried to draft 5.** Resolving the window at read time is the lead candidate. It is the only
option that leaves the projection timeless, so FZ-R5 is not amended. Its cost is a single shared
window evaluator that filters 11 and 12 and the push all call, so FZ-R4's one-evaluator discipline
holds by construction rather than by care. The schema seam in ADR-0059 §6 is unchanged and remains
the shape a window field would take.
