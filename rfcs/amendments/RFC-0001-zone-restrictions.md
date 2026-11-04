---
amendment: zone-restrictions
title: "Zone Restrictions — entry, capability and operating constraints on FleetZone"
target: RFC-0001
status: accepted
created: 2026-09-12
decided-by: ADR-0058
---

# RFC-0001 amendment — Zone Restrictions

**What this file is.** An amendment to RFC-0001 accepted for v0.4, normative for v0.4 and published with it.
The next revision folds it into the chapters. At v0.4 it is kept as a separate file rather than spliced into the chapters: it is not listed in
`rfcs/rfc-0001-core-spec/rfc-0001.meta.yaml`, so `rfcs/dist/RFC-0001-core-spec.md` does not contain it,
and RFC-0001 names it where its rules apply.

**The decisions behind every paragraph below** are recorded in
`docs/adr/0058-zone-restrictions-are-one-model-across-three-axes.md`. Where this text and that ADR
appear to differ, the ADR states the intent and this text states the contract.

**Where each part applies.** Five parts, each naming at its head the RFC-0001 chapter it amends:

| Part | Destination file | Where |
| :-- | :-- | :-- |
| A | *FleetZone* | two new `#####` sections after *Zone Extent Is Two-Dimensional*; new rows in the *Zone Hierarchy Invariants* table; schema additions |
| B | *Scheduler* | Scheduler — filters 11 and 12, maturity rows, CRD-ownership rows |
| C | *FleetAction* | the five refusal reasons and the narrowed-leaf-set report on `status` |
| D | *Fleet Adapter Protocol* | declare / carry / attest / decline on the wire |
| E | *Conformance Profiles and Support Levels* | the `zone-restrictions` profile on two surfaces |

## Maturity, per part

**This replaces a single paragraph that stated the maturity of all five parts at once.** That
paragraph read *"No field named here is in the generated CRD, no filter is in the reference
Scheduler, no field is on the wire, and no conformance check exists"*, and three of its four clauses
were falsified one at a time by three different sessions while the fourth stayed true — which left
the one clause a reader had to believe indistinguishable from the three that had rotted. It was one
sentence shared by five parts, so no session could correct it without rewriting another's statement,
which is how it reached that state.

The table below is per part. **A session landing one part edits one row.**

Re-derived 2026-09-14 by running the commands in the last column against this tree, not by copying
a previous session's summary.

| Part | Surface | State | Re-derived by |
| :-- | :-- | :-- | :-- |
| **A** | `FleetZone.spec.restrictions`, `FleetZone.status.restrictions.effective[]` | **IN THE GENERATED CRD.** Both, with the CEL rules. | `grep -c "restrictions:" config/crd/bases/swarmada.io_fleetzones.yaml` → 2; `grep -n "effective:" config/crd/bases/swarmada.io_fleetzones.yaml` → present |
| **A** | The resolver (FZ-R1 … FZ-R3, FZ-R6) | **IMPLEMENTED.** The Zone Controller folds the ancestor chain into the projection and publishes `RestrictionsResolved` / `RestrictionsConsistent`. | `grep -n "func resolveZoneRestrictions" internal/controller/zone_restrictions.go` |
| **A** | Containment as a SET (FZ-R10 … FZ-R12) | **IMPLEMENTED.** `Robot.status.containingZones[]` is derived and published; restrictions union over it, for filter 12, for the rules FA-R2 carries and for the FA-R7 push. | `grep -c "containingZones" config/crd/bases/swarmada.io_robots.yaml`; `grep -n "func DeriveContainingZones" internal/zone/geometry.go`; `grep -c "operating.forRobot(" internal/controller/dispatch_filters.go internal/controller/fleetaction_controller.go` → 1 each |
| **B** | Candidate filter 11 (zone restriction admissibility) | **IMPLEMENTED**, and called on the dispatch path, except the capability half of the narrowing over the subtree (*Not met at v0.4 (capability narrowing)*, under filter 11). | `grep -n "evaluateZoneRestrictions(action" internal/controller/dispatch_filters.go` |
| **B** | Candidate filter 12 (adapter operating-constraint support) | **IMPLEMENTED**, and called on the dispatch path, over the leaf the robot occupies and every zone of its `status.containingZones[]`. | `grep -n "excludedByOperatingConstraint(robot" internal/controller/dispatch_filters.go` → line 379 |
| **C** | The five refusal reasons | **IN `api/v1`**, and all five are produced. | `grep -c "ActionFailureZone" api/v1/fleetaction_zone_refusals.go` |
| **D** | FA-R1 declare — `CapabilitiesSnapshot.honoured_operating_constraints` | **ON THE WIRE**, and projected to `Robot.status`. | `grep -n "honoured_operating_constraints = " proto/fleet_adapter/v1/fleet_adapter.proto` → field 8 |
| **D** | FA-R2 carry — `AssignAction.operating_constraints` | **ON THE WIRE AND FILLED** on the real dispatch path. | `grep -n "repeated EffectiveOperatingConstraint operating_constraints" proto/…/fleet_adapter.proto` → field 11; `grep -n "a.OperatingConstraints = operating" internal/controller/fleetaction_controller.go` |
| **D** | FA-R3 honour | ***Specified, and not verifiable from any surface.*** Nothing an adapter sends establishes that a camera is off. | — (no check exists or can) |
| **D** | FA-R4 attest | **WITHDRAWN from this contract** (see *Why FA-R4 is withdrawn rather than given a field*). No field, no audit sink, no consumer. | `awk '/^message ActionStatusUpdate/,/^}/' proto/…/fleet_adapter.proto \| grep -c honoured` → 0 |
| **D** | FA-R5 / FA-R6 decline — the rejection value and `unhonourable_subjects` | **ON THE WIRE AND ACTED ON**, by the three in-tree adapters and by the plane, which takes row A24 of RFC-0001's action table and offers the action to the next candidate. | `grep -n "ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET = \|repeated string unhonourable_subjects = " proto/…/fleet_adapter.proto` → values 6 and 5 |
| **D** | FA-R7 tell — `zone_constraints` Command | **ON THE WIRE AND PUSHED** on a containment transition, on a restriction-projection change, and to each contained robot when its adapter registers a `ControlStream`. Optional only for an adapter that declares no subject, which may decline it `unsupported=true`; an adapter that declares any subject implements it (FA-R7). | `grep -n "ZoneConstraints zone_constraints = " proto/…/fleet_adapter.proto` → arm 20 |
| **E** | `zone-restrictions` on the `FleetAdapter` surface | **EXISTS**, always applicable, four rows: `C17.1`, `C17.2`, `C17.3`, `C17.4`. Live — graded on messages a running adapter actually sent. | `grep -oE '"C17\.[0-9]"' adapters/conformance/harness.py \| sort -u` |
| **E** | `zone-restrictions` on the `ControlPlane` surface | **EXISTS**, three rows: `CP7.1`, `CP7.2` (static — read off shipped schemas) and `CP7.3` (recorded unexercised: a live dispatch is not establishable by an artefact profiler). | `grep -oE '"CP7\.[0-9]"' conformance/controlplane.py \| sort -u` |

**The clause that used to be buried among the false ones no longer exists**: filters 11 and 12 are
implemented, except the capability half of filter 11's narrowing (its Not-met note in Part B). What is left genuinely unbuilt is **FA-R3** (unverifiable by construction) and
**FA-R4** (withdrawn). Everything else in the table is in the tree, apart from that Part B gap, and the commands beside each row
are how a reader checks that claim rather than trusting it.

**This table is not gate-checked, and that is the remaining exposure.** The specification build, which
runs only in a checkout that holds the chapter sources, does not assemble this amendment — it is deliberately absent from `rfc-0001.meta.yaml` and stays a
separate file (*What this file is*, above) — so `spec-maturity` cannot measure these rows against the Go tree.
The commands above are the substitute, and they are substitute for a gate, not equal to one. A
per-row command that a reader can run is the most this file can carry until it is assembled.

---

## Part A — `crds/fleetzone.md`

### A.1 — new section, after *Zone Extent Is Two-Dimensional*

<!--rfc-num-->
##### Zone Restrictions

A `FleetZone` can carry **restrictions**: standing statements about what may happen in the region it
names. They are how a site policy — *no robots on the patio*, *no mopping on the carpet*, *no cameras
in patient rooms* — is written **once, in the fleet's own vocabulary**, rather than separately inside
each application that happens to be dispatching.

**Restrictions split into three axes, and a `FleetZone` expresses all three in one block.**

1. **Entry** — may a robot be here at all.
2. **Capability** — may it perform *this job* here.
3. **Operating** — what may it have **running** while it is here: camera, microphone, water,
   chemicals, UV, speed, noise.

**Axis 3 is not a narrower axis 2, and an implementation that collapses them does not conform.** "May
clean here" is a statement about the **action**. "Must not have a camera running here" is a statement
about the robot's **state while present**, and it is true or false of a robot that was asked to do
nothing at all. The decisive case is a robot **transiting** a ward with a live microphone: no action
was refused, no capability was required, and the constraint was still broken. Nor is "no water" the
same as "no mop" — water is a consumable a future robot may use for something that is not mopping, and
a restriction written against the capability misses it.

**A restriction is one shape in all three axes.** A rule is the tuple *(declaring zone, subject, mode,
parameters, reason)*. Inheritance, absorbency, precedence, resolution, admission validation, the
status projection and the refusal vocabulary are defined **once over rules** (*Restriction Inheritance and Resolution*, Part A)
and never restated per axis. The three members of `spec.restrictions` exist because each has a
different natural map key — none, a capability name, a subject — and a named list in this API is a
`+listType=map`. The grouping is a schema affordance; the model is one.

<!--rfc-num-->
###### Schema — Zone Restrictions

```yaml
apiVersion: swarmada.io/v1
kind: FleetZone
metadata:
  name: ward-3-bay-2
  namespace: hospital-east
spec:
  displayName: "Ward 3 — Bay 2"
  parentZone: ward-3
  restrictions:
    # Axis 1 — ENTRY. At most one; its presence is the denial.
    entry:
      mode: Denied                     # ZoneRestrictionMode; only `Denied` is valid here (CEL)
      reason: "outdoors behind the fence; no robot access"
      occupantAction: Report           # Report (default) | Graceful | Immediate

    # Axis 2 — CAPABILITY. +listType=map +listMapKey=name
    capabilities:
      - name: clean.mop                # a capability identifier, matched against
        mode: Denied                   # FleetAction.spec.requiredCapabilities
        reason: "carpet that must not get wet"

    # Axis 3 — OPERATING. +listType=map +listMapKey=subject
    operating:
      - subject: camera
        mode: Denied
        reason: "patient privacy"
      - subject: microphone
        mode: Denied
        reason: "patient privacy"
      - subject: speed
        mode: Limited
        max: "0.4"                     # units from SwarmadaConfig.spec.coordinateSystem
        reason: "occupied ward at night"
      - subject: chemical
        mode: Limited
        allowed: ["quat-ammonium-7"]   # an ALLOW-LIST: everything not listed is denied
        reason: "food preparation surfaces"
      - subject: example.com/leaf-blower # a namespaced extension subject
        mode: Denied
status:
  restrictions:
    effective:                          # the RESOLVED set — consumers read this, never the tree
      - axis: Entry
        mode: Denied
        inheritedFrom: ward-3-bay-2
        reason: "outdoors behind the fence; no robot access"
      - axis: Operating
        subject: camera
        mode: Denied
        inheritedFrom: ward-3           # resolved from an ancestor, not declared here
        reason: "patient privacy"
  conditions:
    - type: RestrictionsResolved
      status: "True"
    - type: RestrictionsConsistent
      status: "True"
```

**`spec.restrictions` is additive and optional.** Its absence means a zone states no restriction, which
is what every `FleetZone` written before this section existed meant. No existing object changes
meaning, and no API version bump is required.

<!--rfc-num-->
###### Modes

`ZoneRestrictionMode` is a named string type with exactly three values. **There is no `Allowed`.**

| Mode | Valid on | Meaning |
| :-- | :-- | :-- |
| `Denied` | `entry`, `capabilities`, `operating` | The subject is forbidden in this zone and every descendant |
| `Limited` | `operating` only (CEL) | The subject is permitted only within `max`, or only among `allowed` |
| `Required` | `operating` only (CEL) | The subject MUST be running — the inspection-zone mirror case |

**A rule exists only to narrow, and absence of a rule is permission.** There is deliberately no value
an operator can write that re-permits something an ancestor forbade: the hole is not rejected at
admission, it is **unwritable**. An `Allowed` value would have been written in good faith, been
silently inert, and been believed — in exactly the direction where being believed is worst.

An `operating` rule MUST set `max` when `mode: Limited` and `allowed` is empty, MUST NOT set either
when `mode` is `Denied` or `Required`, and MUST NOT be `Limited` with both `max` and `allowed`
(`max` bounds a scalar, `allowed` enumerates a set; a subject is one or the other). These are CEL
cross-field rules on the schema, so the API server rejects the object before any controller sees it.

<!--rfc-num-->
###### Operating-Constraint Subjects

`subject` is a lowercase identifier. **The normative core is closed; the extension form is
namespaced.**

| Core subject | What it constrains |
| :-- | :-- |
| `camera` | Any image sensor capturing while the robot is present |
| `microphone` | Any audio sensor capturing while the robot is present |
| `water` | Water put on the floor or into the air |
| `chemical` | Any dispensed chemical agent; `allowed` enumerates the permitted agents |
| `uv` | Ultraviolet emission |
| `speed` | Ground speed; `max` is in the namespace's distance unit per second |
| `noise` | Emitted sound; `max` is in dB(A) |

A `subject` containing no `/` MUST be one of the seven above. A `subject` containing a `/` is an
**extension subject**, whose prefix is a DNS domain the extender controls
(`example.com/leaf-blower`). The split is pattern plus CEL rather than an `Enum` marker because the set
is not closed — it is closed *at the core* — and it is the same split this specification already
applies to capability identifiers. **Conformance covers the core seven**, and exercises an extension subject only for FA-R6 (Part E).

**An unrecognised subject is not an absent subject.** A Fleet Adapter that receives an effective
`operating` rule whose `subject` it does not recognise MUST decline the assignment naming that
subject (RFC-0001, *Fleet Adapter Protocol*). Without this rule the extension form would be a bypass: any
site could silence a constraint by namespacing it.

**These are not capability identifiers, and the two vocabularies never meet.**
`Robot.status.capabilities` is NOT consulted to resolve a subject, and a subject that collides with a
capability name means nothing. A capability answers *can this robot be asked to do X*; an operating
subject answers *is X happening right now*. A capability's status (`Active`/`Degraded`/`Inactive`)
grades health, not whether the thing is switched on. The shape of the identifier is borrowed; the
vocabulary is not.

<!--rfc-num-->
###### Capability Restrictions Carry No Limits

A `capabilities[]` rule is a denial and nothing else. There is no `Limited` mode and no `limits` map
on axis 2. "May clean here, but at 0.4 m/s" is **two rules, one per axis** — a capability is not
denied, and a speed is. Putting a bound under a capability name would duplicate the subject vocabulary
in a second place with a second set of units, a second inheritance question and a second conformance
target, which is the collapse of axis 3 into axis 2 running in the other direction.

*(Seam, non-normative: a genuinely capability-scoped operating limit would be an optional
`whilePerforming: [capability]` selector on an **operating** rule. That is additive and keeps the
subject vocabulary in one place; axis 2 does not grow parameters.)*

<!--rfc-num-->
###### What Restrictions Are Not

**A zone restriction is not a safety control, and an implementation MUST NOT present it as one.** A
control-plane refusal governs what is dispatched **through this fleet**. It cannot stop a robot driven
by its own vendor application, its own buttons, its own schedule, or a person carrying it. The
enforcement asymmetry is stated rather than papered over: nothing in a control plane can physically
switch off a camera, and an operator surface that reports a denied zone as *protected* rather than as
*not dispatched to* is claiming a property this specification does not provide.

What this specification does provide is narrower and worth stating: **an operator can declare a
boundary once, in the fleet's vocabulary, and every path through the fleet respects it** — the
Scheduler, `swarmctl`, a second adapter, and any client using the Kubernetes API.

### A.2 — new section, immediately after the above

<!--rfc-num-->
##### Restriction Inheritance and Resolution

<!--rfc-num-->
###### Rules inherit down containment edges only

A rule declared on a zone applies to that zone **and every descendant**, following `spec.parentZone`
and nothing else. It MUST NOT propagate across `spec.memberZones`: composition is a targeting relation
that asserts nothing physical, and a restriction crossing it would let a group assert authority over
zones it does not contain. This matches estop propagation, which
follows containment alone (RFC-0001 FleetZone, *Composite Zones and Zone Expansion*). A `sharedResources` name follows
neither relation at v0.4: the reference looks it up across every `FleetZone` in the namespace, and the specified narrowing to the requester's `spec.parentZone` ancestors is not implemented (RFC-0001 FleetZone, *Resource name lookup*).

<!--rfc-num-->
###### The effective rule is the most restrictive in the chain, not the nearest

| ID | Rule |
| :---- | :---- |
| **FZ-R1** | For each *(axis, subject)*, the **effective** rule for a zone is the most restrictive rule declared anywhere in its ancestor chain, root-downward, including the zone itself. `Denied` is more restrictive than `Limited`, which is more restrictive than no rule. Between two `Limited` rules on one subject the **tighter** `max` wins and the **intersection** of the `allowed` lists wins. Between `Required` and `Limited` on one subject the effective rule is `Required` and it keeps the bound: the tighter `max`, or the intersection of the `allowed` lists. The pair is not a contradiction (only `Denied` with `Required` is, FZ-R3), and the order in the chain does not change the answer. A nearest-ancestor-wins resolution does not conform. |
| **FZ-R2** | Denial is **absorbing**. No resolution, merge, or default may produce an effective rule more permissive than a rule declared by an ancestor. An implementation whose resolver can emit a weaker effective rule than some rule it read does not conform, whatever its admission checks do. |
| **FZ-R3** | `Denied` and `Required` on the same subject in one chain are a **contradiction**, not a widening. It resolves to `Denied`, and the zone publishes `RestrictionsConsistent: False` naming both declaring zones plus a `RestrictionContradiction` Warning event. `Denied` wins because a zone in which a camera must be off and must be on is a zone no robot may work in; resolving the other way would switch a camera on in a patient room by operation of a merge rule. |
| **FZ-R4** | The Zone Controller writes the resolved set to `status.restrictions.effective[]`, each entry naming the zone it was `inheritedFrom`, and publishes `RestrictionsResolved`. **Consumers read `status`, never the tree**, for the same reason they read `status.geometry` rather than inspecting `spec.physicalBounds`: "what is forbidden here" is decided in one place. |
| **FZ-R5** | `status.restrictions` is a projection of `spec` and the zone tree. It MUST be recomputed on a spec or topology change and MUST NOT be recomputed on a telemetry tick; it carries no per-robot and no positional data. Restriction handling adds **no new per-tick status write anywhere** in this specification. |
| **FZ-R6** | Resolution MUST fail **closed**. A zone whose ancestor chain cannot be read, or that names a `parentZone` that does not resolve, publishes `RestrictionsResolved: False` and is treated as **not dispatchable** until it resolves. An unresolvable restriction is not an absent restriction. |

This specification has no time-windowed restrictions. A restriction is in force from the moment its projection
is published until `spec` is edited; *"between 22:00 and 06:00"* is not expressible, and no
controller acts at a clock boundary (ADR-0058 Amendment 4 §A4.2).

<!--rfc-num-->
###### Admission refuses a new hole. It refuses a new wall only as one side of a contradiction

The two directions of change get deliberately different answers, and the asymmetry is the safety
property.

| ID | Rule |
| :---- | :---- |
| **FZ-R7** | A write that makes a **descendant** more permissive than an ancestor — a raised `max`, a widened `allowed` list, a `Required` under an ancestor's `Denied` or the reverse — is **rejected at admission**, naming the ancestor zone and the conflicting rule. It is never silently ignored: silently ignoring produces an object whose `spec` says one thing and whose behaviour is another, and the purpose of this surface is that the API is where the rule is stated. |
| **FZ-R8** | A write that makes an **ancestor** more restrictive is **always admitted**, even when an existing descendant now contradicts it. The contradiction resolves per FZ-R1/FZ-R3 and is reported on the descendant (`RestrictionsConsistent: False` plus a Warning event). Refusing a parent's tightening on account of a child would make a boundary un-addable, would let a leaf veto a site policy, and would require the webhook to list a whole subtree on every zone write. |
| **FZ-R9** | **Admission is the diagnostic; resolution is the enforcement.** FZ-R7 tells an operator now; FZ-R1–FZ-R2 hold on an object admission never saw — restored from a backup, applied before the webhook was installed, written under `failurePolicy: Ignore`, or raced past the check. An implementation that enforces FZ-R7 and resolves nearest-ancestor-wins has built the diagnostic and skipped the invariant. |
| **FZ-R10** | **Containment is a SET, and restrictions aggregate over it.** A robot is subject to the effective restrictions of **every zone whose polygon contains it**, not only the one arbitrated onto `Robot.status.currentZone`. Zone polygons may overlap, and `currentZone` resolves an overlap to one winner to answer *where the robot reports it is*; using that winner to decide *what binds it* discards the loser's rules, which is the permissive direction and does not conform. The set is published on `Robot.status.containingZones[]`, sorted, and `currentZone` keeps its cardinality and its derivation unchanged. |
| **FZ-R11** | **The aggregate is a UNION OF ALREADY-RESOLVED PROJECTIONS, then the ordinary most-restrictive fold.** Each containing zone's `status.restrictions.effective[]` has already folded that zone's own ancestor chain (FZ-R1/FZ-R4); the union folds those answers by the same rules — `Denied` absorbs, two `max` values take the tighter, two `allowed` lists intersect, `Denied` plus `Required` resolves to `Denied`. An implementation that re-walks the tree to compute this has built a second resolver, which FZ-R4 forbids; a second resolver can drift from the first, and a nearest-ancestor-wins one does not conform (FZ-R1). `inheritedFrom` survives the union naming the **declaring** zone, never the containing one. |
| **FZ-R12** | **A zone with no site-aligned polygon has no containment answer, and is not a member.** A zone whose geometry state is `Absent` or `FrameLocal` never appears in `containingZones[]`: containment cannot be derived for a region whose shape in the site frame the plane does not know. Robots inside such a zone's declared region are governed by the **assignment path alone** — filters 11 and 12 still evaluate its restrictions for an action that targets it. The condition `GeometryAvailable: False` already reports this per zone and is the place an operator learns it. |

<!--rfc-num-->
###### Robots already inside when a restriction tightens

`entry.occupantAction` selects what happens to robots present in a zone at the moment an entry denial
takes effect. It reuses `ZoneMaintenance`'s wind-down vocabulary rather than inventing a second
spelling of the same choice.

| Value | Behaviour |
| :---- | :---- |
| `Report` (default) | No robot is moved, and the Zone Controller emits **one** `ZoneEntryDeniedWithOccupants` Warning event naming the robots that **were inside** when the denial took effect. New work is withheld from the zone by the entry denial itself, not by this field. |
| `Graceful` | *Reserved.* Robots finish their current action and are then wound down, per the Graceful discipline of RFC-0001 ZoneMaintenance, except that no action is marked for a forcible requeue: an entry restriction is not one of the setters row A20 of RFC-0001's action table names. **Specified, not implemented at v0.4.** |
| `Immediate` | *Reserved.* No behaviour beyond `Report`'s is specified for it in this contract version. An entry restriction is not one of the setters row A20 of RFC-0001's action table names, so it marks no action for a forcible requeue. |

`Report` is the default because a control plane that starts **moving robots in response to an edit of
a `spec` field** is a new autonomous motion path, and this surface is explicitly not a safety system.

> **AT v0.4 ALL THREE VALUES DO THE SAME THING: they NAME the occupants, once, and move none.**
> The event is emitted (`internal/controller/zone_occupant_report.go`), latched once per transition
> into the denied-with-occupants state exactly as the `RestrictionContradiction` event beside it is.
> `Graceful` and `Immediate` remain **reserved** — their MOTION is not implemented — and the
> admission webhook warns on both, saying that the zone behaves as `Report` and what `Report` is.
>
> **The occupant list is read from published projections, never from a position frame.**
> `Robot.status.containingZones[]` and `status.currentZone` are each written only on a containment
> transition, so consulting them costs a List on a reconcile — which FZ-R5 guarantees is a
> spec-or-topology event — and never scales with telemetry volume. Both are consulted, and neither
> subsumes the other: `containingZones` is polygon containment, so a zone with no site-aligned
> polygon never appears in it; `currentZone` is ONE name arbitrated from overlapping polygons, so
> reading it alone misses the robot ADR-0059's containment ruling exists for.
>
> **What withholding new work does and does not owe to this field.** An effective `entry: Denied`
> withholds work from the zone through candidate filter 11 regardless of `occupantAction` — so that
> half is real, and it is not `Report`'s contribution. `occupantAction` governs only what happens to
> robots ALREADY PRESENT.
>
> **HISTORY, kept because it is why this field is shaped as it is.** Until 2026-09-14 this paragraph
> said `Report` emitted that event and **no producer emitted it.** The session that found the gap
> STRUCK the promise from all three normative texts rather than leaving it standing — the right
> call, because *a disclosure that over-claims is worse than no disclosure*: it is read as the
> authoritative statement of the limit, and an operator who closed a ward and waited for an event
> naming who was inside waited for nothing, having been told twice in normative text that it would
> arrive. For that interval the enum genuinely had three values and one behaviour, which made
> `Report` indistinguishable from the two documented as inert. The producer closes that, and **none
> of the struck over-claims returns with it:** no robot is moved, withholding work is still credited
> to the denial rather than to this field, and the event is still not a statement that anyone left.

**No occupant report may become a status stream.** No occupancy list is added to
`FleetZone.status`: the standing view is `status.robotCount`, which is already a throttled,
material-transition projection. A status field naming the robots inside a denied zone at any given moment
would be written from position telemetry, which this specification prohibits. That constraint is
what shapes the event above as a one-shot latched Warning rather than a list, and it holds whether
or not the event exists yet.

<!--rfc-num-->
###### Who may change a restriction

A restriction lives in `spec`, so **write access to the declaring `FleetZone` is write access to the
restriction.** So is `patch` on `fleetzones/status`, which the edge node's `swarmada:edge` role holds:
candidate filters 11 and 12 read `status.restrictions.effective[]`, and nothing guards a write to it
(RFC-0001 Security, *Role Definitions*). Kubernetes custom resources admit only `status` and `scale` subresources, so
`spec.restrictions` cannot be given RBAC of its own without becoming a separate object, and a separate
object costs the property that makes a `FleetZone` worth reading: one `get` answers *what is this
zone*.

Stated plainly, because an operator planning a deployment needs it stated: **FZ-R7 stops a principal
from opening a hole in someone else's boundary; nothing here stops a principal who may write the
declaring zone from deleting the wall at its source.** The Zone Controller emits one
`ZoneRestrictionsChanged` event (type `Normal`) on a zone each time the restrictions in force at it
change — its `status.restrictions.effective` set before and after are named in the message —
including when the change comes from an ancestor. An edit that leaves the effective set unchanged, a
resync, and a controller restart emit nothing (RA-1). The event records what changed, not who changed
it; the actor is in the API server's audit log. A deployment where the zone author and the policy author
are different people MUST enforce that separation with RBAC on `FleetZone` itself.

> **No check grades this rule.** It binds the deployment, not the control plane or an adapter, and
> no conformance surface observes a deployment's RBAC.

### A.3 — new rows for the *Zone Hierarchy Invariants (Controller-Enforced)* table

**IMPLEMENTED.** `spec.restrictions` is a property of the generated CRD, the CEL rules below are on
the schema, and the FZ-R7 ancestor check is in the admission webhook
(`internal/webhook/fleetzone_restrictions_fzr7.go`). This paragraph previously said the opposite —
that nothing reaches the webhook and nothing is rejected — which was true when it was written and
was falsified by the session that landed Part A. See *Maturity, per part* above for the per-row
state and the command that re-derives each.

| Invariant | Enforcement point | Error message |
| :---- | :---- | :---- |
| A descendant may not weaken an ancestor's restriction (FZ-R7) | Admission webhook on FleetZone create/update | `<rule> is more permissive than the rule declared by ancestor zone "<ancestor>" (<ancestor rule>); a descendant may not re-permit what an ancestor restricts` |
| `spec.restrictions.entry.mode` must be `Denied` | Generated CRD schema (CEL) | `"spec.restrictions.entry.mode must be Denied: a rule exists only to restrict, and the presence of an entry rule IS the denial"` |
| `Limited` and `Required` are valid only on `operating` rules | Generated CRD schema (CEL) | `"spec.restrictions.capabilities[].mode must be Denied: mode Limited and mode Required are valid only on spec.restrictions.operating[], because a capability restriction carries no limits — a bound on what runs is an operating rule on its own subject"` |
| `max` and `allowed` only on a `Limited` operating rule | Generated CRD schema (CEL) | `"spec.restrictions.operating[]: max and allowed are meaningful only when mode is Limited; a Denied or Required rule sets neither"` |
| A `Limited` operating rule sets exactly one of `max` or `allowed` | Generated CRD schema (CEL) | `"spec.restrictions.operating[]: set exactly one of max or allowed on a Limited rule — max bounds a scalar, allowed enumerates a set, and a subject is one or the other"` |
| A bare `subject` must be one of the normative core seven | Generated CRD schema (CEL) | `"spec.restrictions.operating[].subject must be one of the normative core seven (camera, microphone, water, chemical, uv, speed, noise), or a namespaced extension subject such as 'example.com/lidar'"` |
| An extension `subject` prefix must be a DNS domain | Generated CRD schema (pattern) | The API server's generic pattern-mismatch error: the schema carries a `pattern`, not a message |

---

## Part B — `control-plane.md`, Scheduler

### B.1 — two new hard-constraint filters

The roster grows from ten filters to twelve. **Both new filters are IMPLEMENTED**, apart from the
half the Part B Not-met note names, and are called on
the dispatch path (`internal/controller/dispatch_filters.go`). This paragraph previously said they
were specified-not-implemented and therefore failed **open** — *"the reference control plane
dispatches into a restricted zone"* — which was true when it was written and is now the most
dangerous sentence in the file to leave standing: a reader who believes it will build a private
side-channel to enforce what the plane already enforces. See *Maturity, per part* above.

> **11. Zone restriction admissibility — *action-scoped*.** The effective restriction set of
> `FleetAction.spec.zone` and its ancestor chain (*Restriction Inheritance and Resolution*, Part A) MUST
> admit the action. The action is refused when the target zone, **or any ancestor of it**, carries an
> effective `entry: Denied`, or when `spec.requiredCapabilities` intersects the effective denied
> capability set.
>
> **This filter does not eliminate a candidate; it eliminates the action, and the transition is
> terminal.** Its answer does not vary by robot, so a `Pending` requeue would report a standing,
> permanent refusal identically to a momentarily busy fleet — the one confusion an operator must not
> be left in. The action takes row A16 of RFC-0001's action table, with the `failureReason` named in
> *Zone-Restriction Refusals* (Part C), carrying the declaring zone and the operator's `reason` (nothing about retrying
> changes a standing rule).
>
> **Absorbing over the target relation, narrowing over the subtree.** A denial on the target or an
> ancestor refuses the whole action and is never trimmed away. A denial on a **descendant** of the
> target, an effective `entry: Denied` or a denied capability that `spec.requiredCapabilities` names,
> does not refuse the action: the denied zone is removed from the resolved leaf set of filter
> 2, and **every removed zone MUST be named on the action's status**. Trimming silently is how an
> operator comes to believe a region was serviced that was never entered.
>
> If the narrowing empties the leaf set, the action is refused with the subtree-exhausted detail.

> **Not met at v0.4 (capability narrowing).** The reference control plane removes a descendant from the leaf set only for an effective `entry: Denied` (`evaluateZoneRestrictions`, `internal/controller/dispatch_zone_restrictions.go`). A descendant that denies a capability the action requires stays in the leaf set and is not named on the action's status, and filter 11 does not withhold a robot standing there.

> **12. Adapter operating-constraint support — *robot-scoped*.** For every effective `operating` rule
> in force **at the leaf zone the candidate robot occupies, or in any zone of its `status.containingZones[]`** (folded per FZ-R11) — `status.currentZone`, which filter 2
> has already proven is within the resolved leaf set of `FleetAction.spec.zone` — the robot's
> `status.honouredOperatingConstraints[]`, projected from its serving adapter's
> `CapabilitiesSnapshot` (FA-R1, Part D), MUST contain the rule's `subject`. A robot
> whose adapter has not declared a subject in force is **withheld**, and the action is not
> assigned to it (row A15 of RFC-0001's action table): this is a fact about today's fleet, not a standing refusal, and a conforming adapter
> deployed tomorrow makes the same action dispatchable.
>
> **The scope is the resolved leaf set, not the target zone chain.** Filter 2 admits robots from the
> whole **subtree** of the action's zone, so a filter that consulted only the target's own chain
> would never see an `operating` rule declared on a **descendant** — a rule with a bypass, which is
> the thing two-place enforcement exists to prevent. A leaf's effective set is already the fold of
> itself and its ancestors (*Restriction Inheritance and Resolution*, Part A), so evaluating
> at the leaf **already includes the target chain**: there is no union with the target chain to
> compute, and an implementation that merges the target chain's rules in again has reintroduced
> resolution inside the filter. The rules of the robot's other containing zones are folded in per
> FZ-R11, as the rule above states. The `fleet_adapter.v1` comments on
> `AssignAction.operating_constraints` and `EffectiveOperatingConstraint` describe the rules in
> force at the robot's leaf alone. FA-R2 governs over them; at contract `1.4.0` they
> stay as they are.
>
> **Evaluation is per (candidate, leaf), and this is NOT filter 11's narrowing.** Filter 11's
> narrowing is zone-scoped — an entry-denied leaf is removed for everyone, because its answer does
> not vary by robot. Filter 12's does. A leaf leaves the reachable set only when **no candidate can
> honour it**; the same leaf may drop out for one candidate and be kept for another, and the action
> dispatches to a robot in a leaf it can actually serve. Filter 12 MUST NOT remove a zone from
> filter 2's leaf set.
>
> A robot the action **pins** is not exempt (filter 6 lets a pin skip filters 2, 3 and 5, not this
> one): whether a vendor's adapter honours a constraint is not a judgement an operator can assert
> into being. A pinned robot standing outside the subtree is judged against the **target's own
> chain** as the floor.
>
> **When the filter withholds every candidate**, the refusal is `ZoneOperatingConstraintUnmet`,
> row A15 of RFC-0001's action table is not taken, and the refusal MUST name **the subject and every zone it is in force
> in** — the way filter 11's subtree-exhausted refusal names every narrowed zone. The declaring zone
> is very often not the target.
>
> Evaluation is **fail-closed**, on the same footing as filter 9: an unset `spec.adapter.name`, an
> absent or unreadable adapter, a robot carrying no declaration, or an unresolvable zone chain all
> withhold the robot. A robot that cannot prove it honours a constraint has not honoured it.
>
> **What this filter cannot see.** The control plane does **no path planning**, so a zone the robot
> only **crosses** on its way to the leaf is not a zone this filter can evaluate — it is absent
> input, not an implementation gap. A transiting robot is covered by the adapter obligation (Part D,
> FA-R1/FA-R2), not here.

### B.2 — the read-failure asymmetry, stated where an implementer will look

> **Filters 11 and 12 fail closed on a `FleetZone` read error, and filter 8 fails closed on a
> `ZoneMaintenance` read error. Filter 2 alone is skipped, and the action is still not dispatched.**
>
> A failed `FleetZone` list proves nothing about any robot, so filter 2 is skipped and the degradation
> is reported. The same failed read leaves filter 11 unresolved, so the action is held `Pending` and
> requeues. A failed `ZoneMaintenance` list leaves unknown whether a window holds a robot, so every
> candidate is withheld as filter 8 and the action requeues (RFC-0001 Control Plane, *Scheduler*).
> A failed list means something different to filter 11: the restriction set is **unresolvable**, and
> an unresolvable restriction is not an absent one. The action reports `ZoneRestrictionsUnresolved`
> and row A15 is not taken: it is not dispatched.
>
> An implementation that skips filter 8, 11 or 12 on a failed list read converts one transient
> list error into a dispatch into a drained or denied zone.

> **In a namespace where any `FleetZone` declares `spec.restrictions`, an action whose `spec.zone` names no existing `FleetZone` is refused, not dispatched.** Resolving
> an unknown zone name to itself is the correct compatibility choice for containment — it keeps an
> action targeting a zone the operator has not created behaving as it did before filter 2 existed —
> and it is the wrong one once a zone name carries a restriction set, because a name that resolves to
> nothing is a name whose restrictions cannot be evaluated. Unknown is not permitted. The refusal
> enumerates the zones that do exist (*Zone-Restriction Refusals*, Part C). In a namespace where no `FleetZone` declares
> `spec.restrictions`, filter 11 does not apply, and an unknown name resolves to itself as it does for
> containment.

### B.3 — Scheduler CRD-ownership rows

| CRD | Access |
| :---- | :---- |
| `FleetZone` | Reads `status.restrictions.effective[]` and the `RestrictionsResolved` condition for filters 11 and 12 (the resolved projection, never `spec.restrictions` and never the raw tree — FZ-R4). **Implemented**: `newDispatchFilterState` lists `FleetZone` once per dispatch call and filters 2, 11 and 12 all consume it |
| `Robot` | Additionally reads `status.honouredOperatingConstraints[]`, `status.currentZone` and `status.containingZones[]` (filter 12, which judges a candidate against the rules in force at the leaf it occupies and in every zone containing it). **Implemented** |
| `FleetAction` | Additionally writes the restriction `failureReason`, the narrowed-leaf-set report, and the declining robot and the subjects it named on `status` |

---

## Part C — `crds/fleetaction.md`

<!--rfc-num-->
##### Zone-Restriction Refusals

Five refusal reasons, and the distinctions between them are not cosmetic. Each is the
sentence an operator reads when a robot is not moving, and the five reasons are genuinely different:
*there is no such zone*, *you told me not to go there*, *the zone is open but this job is not*, *no
robot here can honour the rule*, and *I could not work out what the rule is*. Collapsing any two of
them costs the operator the difference.

| `failureReason` | Phase | Set when | The refusal MUST carry |
| :---- | :---- | :---- | :---- |
| `ZoneEntryDenied` | `Failed` | The target zone or an ancestor carries effective `entry: Denied`, or the subtree narrowing emptied the leaf set | The **declaring zone** (which may not be the target), the operator's `reason`, and that the refusal holds whether the zone is targeted directly **or reached through an ancestor** |
| `ZoneCapabilityDenied` | `Failed` | `spec.requiredCapabilities` intersects the effective denied capability set | The **capability**, the declaring zone, the `reason` — and that the zone itself is **not** closed: one capability is |
| `ZoneOperatingConstraintUnmet` | `Pending` (with `failureReason` set while it waits), then `Failed` on deadline | No candidate robot's adapter declares every effective `operating` subject in force **at the leaf that robot occupies** | The **subject**, the zone that declares it, **every zone of the resolved leaf set it is in force in**, and which adapters were asked — distinguishing *cannot* honour from *did not* |
| `ZoneRestrictionsUnresolved` (carried in `status.message`; `failureReason` stays empty) | `Pending` | The zone chain could not be read, or a `parentZone` does not resolve. A contradictory chain is not this case: FZ-R3 resolves it to `Denied` | Which resolution failed, and that **no decision was made** — the action is held, not judged |
| `ZoneUnknown` | `Failed` | `spec.zone` names no `FleetZone` in the namespace, and some `FleetZone` in the namespace declares `spec.restrictions` | The name as written, and an enumeration of the zones that exist. Ambiguity asks; it never guesses |

**`ZoneEntryDenied` and `ZoneCapabilityDenied` are terminal; `ZoneOperatingConstraintUnmet` and
`ZoneRestrictionsUnresolved` are not.** The first two are standing decisions that no retry and no
fleet change will satisfy. The second two are facts about today — a conforming adapter deployed
tomorrow, or a `FleetZone` that becomes readable, makes the same action dispatchable — so the action
waits and fails on its deadline like any other unsatisfied `Pending`.

**A refusal MUST NOT claim more than it does.** `ZoneEntryDenied` means the action was not dispatched.
It does not mean no robot will enter the zone: an operator surface that renders it as *protected*
rather than as *not dispatched to* is asserting a property this specification does not provide
(*Zone Restrictions*, Part A, *What Restrictions Are Not*).

<!--rfc-num-->
###### The narrowed leaf set is reported, never silent

When filter 11 removes a denied descendant from the resolved leaf set of filter 2, the action's
`status` MUST name every removed zone and the rule that removed it, and the action proceeds against
the remainder. This is the one place a restriction narrows rather than refuses, and the distinction is
between a **standing decision** and **what the target happens to contain**:

- The target, or an ancestor of it, denies entry or a required capability → **refuse the whole action**. It is never trimmed.
- The target's subtree contains a descendant that denies entry or a required capability → **narrow, and name every exclusion**.
- Narrowing empties the set → **refuse**, with the subtree-exhausted detail.

Refusing a whole floor because one closet inside it is denied would make the floor undispatchable for
a reason no operator can act on. Trimming the closet **silently** would be worse: it is how an
operator comes to believe a region was serviced that was never entered, on a fleet where a truncated
run and a complete one can be indistinguishable in the outcome report.

---

## Part D — `fleet-adapter-protocol.md`

<!--rfc-num-->
##### Zone Operating Constraints: Declare, Honour, Attest, or Decline

**Nothing in a control plane can physically switch off a camera.** A specification that promised
enforcement here would be lying. What it can require is an adapter behaviour a conformance suite can
go red on, and this is it.

**Maturity, per rule rather than in one blanket sentence**, because the seven no longer share one
state:

| Rule | State |
| :---- | :---- |
| **FA-R1 — declare** | **Implemented.** `CapabilitiesSnapshot.honoured_operating_constraints` is on the wire and is projected to `Robot.status.honouredOperatingConstraints[]`. |
| **FA-R2 — carry** | **Implemented.** `AssignAction.operating_constraints` is filled on the dispatch path from the resolved projection at the robot's leaf, folded per FZ-R11 with every zone of its `status.containingZones[]`: the set filter 12 judged it against. |
| **FA-R3 — honour** | ***Specified, not verifiable.*** Nothing an adapter sends can establish that a camera is off, so no conformance check exists or can. |
| **FA-R4 — attest** | **WITHDRAWN from this contract.** Not only unimplemented: there is no field on the wire, no audit event to record it into, and no consumer. Recorded below as a non-normative intent, with the prerequisites an attestation obligation would need. |
| **FA-R5 — decline** | **Implemented**, on both sides: `AssignActionRejection` carries the value, `AssignActionResult.unhonourable_subjects` carries the names, the three in-tree adapters decline, and the control plane takes row A24 of RFC-0001's action table and offers the action to the next candidate. |
| **FA-R6 — unknown means decline** | **Implemented**, by the same adapter predicate as FA-R5: a subject an adapter does not recognise is not a subject it declared, so it lands in the same refusal. |
| **FA-R7 — tell** | **Implemented.** The `zone_constraints` Command is on the wire and is pushed on a containment transition, on a restriction-projection change, and to each contained robot when its adapter registers a `ControlStream`. An adapter that declares no subject may decline it `unsupported=true`. |

| ID | Obligation |
| :---- | :---- |
| **FA-R1 — declare** | An adapter MUST declare, in `CapabilitiesSnapshot.honoured_operating_constraints` (additive; a new field number in `fleet_adapter.v1`), the operating subjects it can honour for each robot. The control plane projects it read-only to `Robot.status.honouredOperatingConstraints[]`, the same path `sensor_streams` already takes. An adapter that declares none is not in breach; its robots are withheld from zones that restrict a subject (RFC-0001 Control Plane, *Scheduler*, filter 12). |
| **FA-R2 — carry** | The control plane MUST send, on `AssignAction` (additive; a new field number), the **resolved effective** operating rules in force **at the zone the robot is being dispatched to, folded per FZ-R11 with every zone of its `status.containingZones[]`** — its `status.currentZone`, which candidate filter 2 has already proven is within the resolved leaf set of `spec.zone`, or, for a pinned robot standing outside that set, the **target's own chain** — in each case the same set filter 12 judged that robot against. The adapter is never asked to walk a zone tree, and never receives a rule that resolution has already superseded. **The scope is the leaf, not the target's own chain** (RFC-0001 Control Plane, *Scheduler*, filter 12): filter 2 admits robots from the whole subtree, so the target's own projection is empty for an action on a zone whose rule is declared one level down, and an adapter sent that set has nothing to decline. The leaf's projection already contains every ancestor's rule, the target's included, so this is narrower in scope and never narrower in content. |
| **FA-R3 — honour** | An adapter that accepts an assignment carrying an operating rule MUST honour it for the whole time the robot is in the zone, including while only passing through: the rule constrains the robot's **state**, not the action. |
| **FA-R4 — attest** | **WITHDRAWN from this contract — see *Why FA-R4 is withdrawn rather than given a field* below.** The withdrawn text required the adapter to report, on the action's terminal outcome, which subjects it honoured. There is nowhere on the wire for the report to go, nowhere in the control plane for it to be recorded, and nothing that would read it; an obligation in that position has already stopped being one. This contract carries no attestation obligation; the shape one would take is stated below so it is not re-derived. **An attestation is a claim, not a measurement** — that half of the original text is unaffected and is why the obligation was never verifiable in the first place. |
| **FA-R5 — decline** | An adapter that **cannot** honour a rule MUST decline the assignment — `AssignActionRejection` gains a value for it, and `AssignActionResult` carries the subjects it could not honour — rather than accepting it and ignoring the rule. Declining costs the adapter nothing: row A24 of RFC-0001's action table applies and the Scheduler tries the next candidate. |
| **FA-R6 — unknown means decline** | An adapter that receives a rule whose `subject` it does not recognise MUST decline under FA-R5 naming that subject. An unrecognised constraint is **not** an absent constraint; treating it as absent would make the namespaced extension form a universal bypass. |
| **FA-R7 — tell, continuously** | The control plane MUST send the resolved operating rules in force **across every zone containing the robot** on a `zone_constraints` Command, when that answer changes, and again to each robot that stands in a zone when its adapter registers a `ControlStream`, because a newly connected adapter process holds no rule set; it sends them at no other time. It is **what makes FA-R3 satisfiable** and adds no honour obligation of its own (its adapter rules govern whether an adapter must implement the push and how it reads one): FA-R3 already binds an adapter for the whole time the robot is in the zone, including while transiting, and `AssignAction` only ever carried the rules in force where the robot stood at dispatch, once — so an adapter entering any other zone had no rule set for where it was. `Denied`, `Limited` **and `Required`** all travel: entering a zone can oblige a subject to be **on**. An **empty** set with no `unresolved_zones` means no rule is in force in any zone whose site-aligned polygon contains the robot, which is permission for those zones, and is how the plane says a robot has LEFT a restricted region; an adapter MUST NOT read it as a lockout. A push never revokes the rules carried on the robot's current `AssignAction`: FA-R3 still binds them, including in a zone FZ-R12 keeps out of `containingZones[]`. The command is **optional** for an adapter that declares no subject under FA-R1: such an adapter answers `unsupported=true` and is not in breach. An adapter that declares any subject MUST implement it, since without it the transit half of FA-R3 cannot be met. **THE RULE ARRIVES AFTER THE ROBOT CROSSES THE BOUNDARY** — the loop is telemetry-up then command-down — so this does not make a zone safe, and no implementation or operator surface may present it as doing so (ADR-0059 §6). |

**Why FA-R5 is the testable half.** Declare/honour/attest are claims. *Decline* is an observable
message on a wire: dispatch into a zone carrying a constraint the adapter does not implement, and
require a refusal naming the subject. That single check converts an unverifiable promise into an
adapter behaviour a suite can fail.

**Why FA-R4 is withdrawn rather than given a field.** Decided 2026-09-14.

FA-R4 was a MUST with no wire surface: `ActionStatusUpdate` carries `action_id`, `state`, `message`,
`fencing_token`, `progress_pct`, `end_cause`, `superseding_action_id` and `retained`, and no honoured-subject
list; no other terminal message carries one either. The obvious repair is an additive repeated
`honoured_subjects` field on that message, required on a terminal state when the assignment carried
operating constraints. **It was not taken, and the reason is not the field's cost.**

It is what the field would land in. **There is no action-terminal-outcome audit event in this
tree at all.** The audit chain records `FLEETACTION_CANCELLED`, `ACTION_REQUEUED_BY_MAINTENANCE`
and `ACTION_PAUSED_BY_ESTOP`; an action succeeding or failing records nothing. `FleetAction.status`
carries no honoured-subject field either. So an attestation arriving on the wire today would be
written by every conforming adapter, on every terminal transition, and **read by nothing** —
recorded in no audit entry, projected onto no status, and gradeable only for arrival and shape.

That is the same defect class as a normative text promising an event that is emitted nowhere: it
creates the *appearance* of an audit trail rather than an audit trail. **A field whose only
consumer is a conformance check asserting that the field arrived is a conformance obligation that
grades itself.** ADR-0058 §12's *"a false attestation is both a conformance failure and a security
event, and is recorded as both"* describes a mechanism with neither half built, and adding the wire
field would build the cheaper half while leaving §12 still describing something that does not
happen.

**What each rejected option costs.**

- **(a) `honoured_subjects` on `ActionStatusUpdate`.** Cheap, additive, one minor bump. Costs: the
  field is meaningful on exactly one of the many transitions the message carries, every adapter is
  obliged to populate it, and nothing consumes it. The conformance row it enables can only assert
  arrival — so a green row would mean *an adapter sent a list*, and would be read as *the plane has
  an attestation record*, which it would not.
- **(b) A new terminal-outcome message.** Semantically cleaner and a larger surface, and it
  competes with `ActionStatusUpdate`'s existing terminal role. Rejected for (a)'s reason plus its
  own: designing a terminal-report message around a payload nothing reads fixes the shape of that
  message before its actual consumers exist.

**The seam, recorded so it does not have to be re-derived.** This contract carries no attestation
obligation, and adding one is a contract change (RFC-0001, *Resource Model at a Glance*). An attestation obligation
is sound only when **all three** of the following hold, and the order matters — the sink comes first:

1. a terminal-outcome audit event exists, so a false attestation can be *recorded* as §12 requires;
2. `honoured_subjects` is added as an additive repeated field on `ActionStatusUpdate`, whose
   **absence keeps the meaning it has today**: no attestation was made, which is what every message
   sent before the field existed meant — never "nothing was honoured";
3. `adapters/CONFORMANCE.md` gains a row grading its **arrival and well-formedness only**, saying
   in the row itself that it grades neither truth nor compliance.

The intent is recorded here and is **not** a MUST, because a MUST that no implementation
can satisfy observably and no suite can notice being missed has already stopped being one — and
leaving it in place teaches a reader that a MUST in this document may be decorative.

**Why the adapter obligation does not replace the control-plane filters.** A prior integration
measured, against a real device, that no application-side no-go marking reached the device at all — so
for that device, a refusal not enforced in the adapter is not enforced. That establishes the adapter
as the **last** line. It does not establish it as the only place the rule belongs: a rule only the
adapter knows is a rule that `kubectl`, `swarmctl`, a second adapter and any scheduler walk straight
through, that an operator cannot state, and that conformance cannot test — because there would be no
rule in the API to test. **A rule enforced in one place is a rule with a bypass**; this specification
requires both.

---

## Part E — `conformance-profiles.md`

A new profile, `zone-restrictions`, on **two** surfaces, because both parties have obligations.

| Surface | Profile | Level | Obligations |
| :---- | :---- | :---- | :---- |
| `FleetAdapter` | `zone-restrictions` | Extended — **always applicable** | FA-R1, FA-R3, FA-R5 and FA-R6. The load-bearing case is FA-R5/FA-R6: dispatch into a zone restricting a subject the adapter does not implement, and require a decline **naming the subject**, demonstrated by a run whose harness report reads `conformant: false` against an adapter that accepts and ignores it. As an Extended profile it does not enter the `ConformanceReport` verdict |
| `ControlPlane` | `zone-restrictions` | Core | Filters 11 and 12; FA-R2 and FA-R7; the terminal-versus-`Pending` split across the five refusal reasons; the narrowed-leaf-set report; FZ-R1/FZ-R2 resolution (most-restrictive-in-chain, never nearest-ancestor); FZ-R6 and the read-failure fail-closed rule; FZ-R7's admission rejection |

> **What the `ControlPlane` rows grade.** `CP7.1` grades that the shipped `FleetZone` schema carries
> `status.restrictions.effective[]` with `axis`, `mode` and `inheritedFrom`, the surface of FZ-R4.
> `CP7.2` grades that it carries `spec.restrictions`. Neither reads a running cluster. `CP7.3` covers
> FA-R2 and is recorded unexercised, never green.
>
> **Not graded at v0.4 (no conformance check).** No check grades filters 11 and 12, what each of the
> five refusal reasons carries and whether it is terminal or `Pending`, the narrowed-leaf-set report,
> the refusal of an unknown zone, the read-failure rule of B.2, FZ-R1, FZ-R2, FZ-R3, FZ-R5, FZ-R6,
> FZ-R7, FZ-R8, FZ-R10, FZ-R11, FZ-R12, the schema rules of A.3, the `ZoneEntryDeniedWithOccupants`
> and `ZoneRestrictionsChanged` events, the control plane's half of FA-R5, FA-R7 (the push, and an adapter's reading of an empty push as
> permission rather than a lockout), or the rule that a
> restriction does not propagate across `spec.memberZones`. The reference implementation's tests
> exercise filters 11 and 12 (`internal/controller/dispatch_zone_restrictions_test.go`,
> `internal/controller/dispatch_operating_constraints_test.go` and their envtest siblings), FZ-R5,
> FZ-R10, FZ-R11 and FA-R7. No test covers the rule that a restriction does not propagate across
> `spec.memberZones`.
>
> **No check grades the presentation rules** in *What Restrictions Are Not* (Part A) and
> *Zone-Restriction Refusals* (Part C). They bind operator surfaces, and no conformance surface
> observes one.

Conformance covers the **core seven** subjects only. An extension subject is exercised exactly once,
for FA-R6: an adapter that silently accepts an unknown subject fails.

> **WHY THE `FleetAdapter` PROFILE IS ALWAYS APPLICABLE, corrected 2026-09-14.** It was written as
> *"applicable when the adapter declares any honoured subject"*, which could not be satisfied
> alongside this table's own next sentence. The load-bearing case is *"a run that goes RED against an
> adapter that accepts and ignores it"* — and an adapter that accepts and ignores an operating rule
> is, in every instance, one that declares **no** honoured subject. Under the old rule the profile
> was reported `notApplicable` for exactly the adapters whose red run it demanded, and a reader saw
> `notApplicable` beside a failing MUST from the same profile.
>
> **FA-R5's own antecedent has no declaring clause** — *"an adapter that CANNOT honour a rule MUST
> decline"* — and an adapter that honours nothing cannot honour anything, so every operating rule
> that reaches it is one it must decline. The profile's applicability now says the same thing its
> obligations do.
>
> **The cost the alternative readings carried, and why neither was taken.** Splitting into two
> profiles — one conditional for declare/honour, one always-applicable for decline — gives each rule
> its own antecedent, and names a second profile the specification does not have; a vendor reads a
> separately-named profile as separately optional, which is the one thing the decline obligation is
> not. Leaving the rule as written and documenting the split in the catalogs is what shipped, and it
> leaves a future implementer free to gate the rows on applicability and silently lose the only
> check in this model that can go red.
>
> **What "always applicable" does NOT cost, re-derived against the rows that actually shipped rather
> than against the rules in the abstract.** The concern was that FA-R1's and FA-R3's declare-and-
> honour rows would sit in an always-applicable profile most adapters hold nothing of. They do not:
> FA-R3 has **no check and can have none** (nothing an adapter sends establishes that a camera is
> off), and FA-R1's check grades the **shape** of whatever was declared — **declaring none passes**,
> which FA-R1 states is not a breach. Where a row genuinely does need a declaration to exist, it
> carries its own **enumerated skip cause** rather than relying on profile-level inapplicability.
> So no row in this profile needs the profile to be conditional, and the conditional was buying
> nothing while costing the contradiction above.

**Precedence is unchanged**: where a catalog and this specification disagree, the specification
governs. Adding a profile adds MUST checks to the harness result that admission reads, so the registry
revision that introduces it invalidates harness results earned before it. The `ConformanceReport`
verdict reads Core rows only, and an Extended profile does not change it.
