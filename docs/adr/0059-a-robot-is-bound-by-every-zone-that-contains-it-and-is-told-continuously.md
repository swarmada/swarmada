# ADR-0059: A robot is bound by every zone that contains it, and is told what may run there continuously

- **Status:** accepted
- **Date:** 2026-09-14
- **Deciders:** Maintainers (API, control plane, distributed systems)
- **Related:** ADR-0058 (zone restrictions are one model across three axes), and its Amendment 1
  (what filter 12 withholds) and Amendment 2 (filter 12's scope is the resolved leaf set);
  ADR-0032 (contract versioning); ADR-0040 (zone bounds on `RegisterAck`); ADR-0013 (graceful
  drain); RFC-0001 *Control Plane Component Specifications* (Zone Controller, Scheduler),
  RFC-0001 *Fleet Adapter Protocol*; `rfcs/amendments/RFC-0001-zone-restrictions.md`

## Context

### The requirement

> A robot, as it moves, must reflect the current zones where it is located, so limitations are
> aggregated across them, and the robot is commanded what capabilities may be active there.

Three parts, and the design turns on all three being read literally: containment is a **set**,
restrictions **aggregate over the set**, and the adapter is **told, continuously**, rather than once
at assignment.

### This is the missing half of an obligation the contract already states

`fleet_adapter.v1`, on `AssignAction.operating_constraints`:

> The RESOLVED EFFECTIVE operating rules in force … honour every one of them **for the whole time
> the robot is in the zone, including while only passing through**.

That is FA-R3, and it has been normative since the Part D surface landed. FA-R2 only ever sent an
adapter the rules for the **assignment target's** chain, **once**, at assignment. So an adapter that
entered any other zone — a corridor it transits, an overlapping bay, a zone whose rule was written
after dispatch — **had no rule set for where it now was, and could not comply with an obligation
this specification already placed on it.**

Stated plainly, because the framing decides what this ADR is: **nothing here adds an obligation. It
makes an existing one satisfiable.** An adapter author reading FA-R3 today is told to honour rules
for the whole time the robot is in a zone, and is given the rules for one zone at one moment. That
is not a demanding contract; it is an impossible one.

### Two holes, which turn out to be one hole seen from two sides

**Geometrically.** Zone polygons overlap by design — a charging bay inside a warehouse floor, a
sterile corridor crossing two wards — and nothing validates that siblings are disjoint, correctly.
`internal/zone/geometry.go` resolves several matches to one winner: *prefer leaf zones; among leaves
pick the nearest centroid*. `Robot.status.currentZone` is a single string, and restriction
resolution folds the ancestor chain of **that one name**.

So a robot standing physically inside two overlapping zones was subject to the restrictions of
exactly one of them — whichever won a tie-break **written to answer where a robot should report it
is**. That tie-break was doing safety work it was never designed for, and it resolved
**permissively**: it picked one chain and discarded the other, where FZ-R2 (denial is absorbing) and
FZ-R6 (fail closed) both point at a union.

Measured, against the resolver as it stood, over the amendment's own fixtures:

| Case | Two containing zones | Before |
| :-- | :-- | :-- |
| A denial in the loser | `ward-a` denies `camera`; `bay-1` declares nothing | **nothing in force** |
| A denial losing to a bound | `ward` denies `camera`; `bay` bounds it at 30 | **`Limited max=30`** — weaker than a rule that was read |
| A requirement in the loser | `terminal-clean` requires `uv` | **nothing in force** |
| `Denied` and `Required` in two zones | `ward` denies `uv`; `clean` requires it | **`Required`** |

The second and fourth rows are worse than the item that filed this knew. The second emits an
effective rule **more permissive than one the resolver read**, which is the one thing FZ-R2 forbids
outright. The fourth switches a UV lamp **on** in an occupied ward by operation of a merge rule —
the exact outcome FZ-R3's own text says it exists to prevent, arriving through the containment
question rather than the chain question.

**Temporally.** Enforcement was admission-only. Candidate filters 11 and 12 gate **dispatch**;
nothing re-evaluated a robot once it was moving. A `Continuous` action admitted at T0 passed
admission once and was never judged again, so a camera denial written at T1 reached it through no
path whatsoever.

The two are one hole: **a robot's obligations were computed once, from one zone, at one moment.**
Fixing either alone leaves the other.

## Decision

### 1. Containment is a set. `currentZone` is unchanged

`Robot.status.containingZones[]` is added: **every zone whose polygon contains the robot**, sorted
by name. `Robot.status.currentZone` keeps its cardinality, its derivation and its tie-break exactly
as they are.

The two answer different questions and were being conflated. `currentZone` answers *"where should
this robot report it is"* — candidate filter 2, the reservation lifecycle, TDE admission,
`SpecZoneMatchesCurrent` and `ZonePositionUnmatched` all read it and all want one answer.
`containingZones` answers *"what binds it"*. Separating them is additive, disturbs nothing that
reads `currentZone`, and makes each tie-break answer only the question it was designed for.

### 2. Restrictions aggregate as a union of already-resolved projections

The set in force for a robot is the **union of `status.restrictions.effective[]` across every zone
containing it, folded most-restrictive** — denial absorbs, two bounds intersect, two allow-lists
intersect, `Denied` and `Required` resolve to `Denied`.

**It is a union of ANSWERS, never a second walk of the tree.** Each input has already folded its own
ancestor chain (FZ-R1/FZ-R4). Re-resolving here would be a second implementation of the fold, which
FZ-R4 forbids, and FZ-R9 names the specific way it goes wrong: it drifts to nearest-ancestor-wins.

The union reuses `foldRule` — the same function, unchanged — rather than a union-specific merge.
That is structural rather than economical: its single governing sentence, *the fold never drops a
constraint it read*, is exactly what the union needs, and a second merge would have been a second
place for those four rules to live. The one that drifted would have been the one deciding what a
robot may run.

`inheritedFrom` survives the union naming the **declaring** zone, not the containing one. It is the
field that tells an operator which object to edit, and a union that rewrote it would send them to a
zone with nothing in it.

### 3. The push is edge-triggered, on two events and no others

A robot is told what may run where it is by a new `zone_constraints` Command, sent when **its
containment changes** and when **a containing zone's restriction projection changes** — a spec or
topology edit, which FZ-R5 already guarantees is not a telemetry tick.

**RA-1 is the whole difficulty and the pattern was already in the file.** Containment is derived
from position telemetry, which is precisely where a per-tick status write is prohibited.
`ObservePosition` already solved this shape for `currentZone` — a compare-and-set against both the
last write and the persisted status — and the containment set and the push ride that same gate
rather than a new one.

**Two gates, not one.** The status write fires when the containment SET changes, because
`containingZones` must report what is true. The push fires when the **resolved rule set** changes,
which is strictly smaller: a robot crossing from one unrestricted zone into another changes its
containment and changes nothing an adapter has to do, and re-sending an identical set would be a
command per boundary crossing on a fleet where a robot may cross several in a minute.

Both comparisons are on **sorted** values. That is load-bearing rather than tidy: a map-iteration
reordering in either would make an unchanged fact look like a transition and emit on every tick.

### 4. THE ARCHITECTURE: the plane evaluates containment and pushes. The adapter does not

This was the open question, and it is settled here.

**Chosen — (a) the plane evaluates and pushes.** The control plane derives the containment set from
position telemetry, unions the projections, and sends the effective set to the adapter.

Why:

- **The plane already owns geometry.** `FleetZone.spec.physicalBounds`, the frame model, the
  `GeometryAvailable` condition and `DeriveCurrentZone` are all here. Containment is one more
  question asked of data the plane already holds and already walks once per position frame.
- **One evaluator of the rules, which is FZ-R4's entire point.** *"What is forbidden here"* is
  decided in one place. The adapter *"is never asked to walk a zone tree"* — and under (b) it would
  be asked to do point-in-polygon as well.
- **It does not enlarge the wire.** The rules travel in the type `AssignAction` already carries, so
  an adapter implements one representation of an operating rule rather than two that could diverge.
- **The cost is a lag, and this specification has never claimed to be the thing that physically
  stops a robot.** ADR-0058 §12 and the refusal register both say so already. Starting to claim it
  here, on the strength of a mechanism that still cannot switch a camera off, would be the one
  over-claim the whole design has been careful to avoid.

**Rejected — (b) the plane ships geometry and rules; the adapter evaluates locally.** The plane
would send polygons, frames and floors; each adapter would run point-in-polygon against its own
pose and apply the rules itself.

It is the better answer on the one axis that matters most for a real boundary: **it has no crossing
lag.** A robot evaluating locally knows it has entered a zone at the moment it enters, not a
round trip later. That is how geofencing is actually built, and this ADR does not pretend otherwise.

It is rejected for three costs, in order of weight:

1. **A second evaluator of the same rules in every vendor's build** — which is the divergence FZ-R4
   exists to prevent, arriving through the wire instead of through a controller. Two adapters
   disagreeing about whether a robot is inside a ward is a disagreement no conformance check can
   see, because both would be answering from their own pose.
2. **Every adapter author must implement point-in-polygon correctly**, including the boundary case,
   the self-intersecting polygon, and the floor comparison. A site's safety boundary would then be
   as correct as the least careful ray-casting implementation in the fleet.
3. **A far larger wire surface** — polygons, coordinate frames, floors, and their updates — and a
   conformance obligation to match, on a contract whose Part D surface had landed shortly before this decision.

**What (b) would be for, stated so the choice can be revisited on evidence rather than re-argued.**
(b) is the honest answer for a **genuine safety boundary** — a region where crossing it with a
subject running is itself the harm, rather than a policy violation to be corrected. This design does
not provide that and does not claim to. The seam is left in §6.

### 5. The push rides an existing shape

`ControlPlaneMessage.Command` already carries `ZoneAdmission`: a control-plane-initiated,
zone-scoped push on `ControlStream`, emitted on a containment transition and on no other event. That
is this message's precedent in the trigger, the direction, the stream and the best-effort delivery
discipline, so `zone_constraints` is a **new `Command` variant beside it** — not a new stream, not a
new RPC. A second transport would have doubled the handshake, the fencing story and the conformance
surface to carry one more message shape.

`Denied`, `Limited` **and `Required`** all travel. The requirement's phrasing was *"what capabilities
can be active"*, and `Required` is the inspection-zone mirror case: entering a zone can oblige a
subject to be **on**, not only off. A push that only ever forbade would implement half the enum.

The command is **optional**, on the same footing as `zone_admission`: an adapter built before it
exists answers `unsupported=true` and is not in breach. Additive proto surface, so **MINOR** per
ADR-0032 A1.1: `1.2.0` → `1.3.0`.

### 6. What this does not do, and the seam that would close each

This section is normative in the same sense §A1.4 of ADR-0058's Amendment 1 is: a limit stated
without its remedy is a disclaimer, and a limit stated with one is a remedy a later
maintainer can act on without re-deriving the problem.

**Every field added here is designed for the absent case.** An optional field added later must not
change what its absence meant before it existed. That is the rule that makes the four seams below
real rather than decorative, and it is the same discipline ADR-0058 §2 already applies to
`ZoneRestrictionMode` having no `Allowed` value: absence is permission, and it stays permission.

> **THE RULE ARRIVES AFTER THE ROBOT CROSSES THE BOUNDARY.** The loop is telemetry-up then
> command-down. A robot that enters a restricted zone is inside it, with whatever it was running
> still running, for at least one telemetry interval plus one command round trip. **This push does
> not make a zone safe. It makes FA-R3 satisfiable** for an adapter that wants to comply, which is a
> different and a smaller claim, and the difference must not be blurred in any operator surface.
>
> *Seam.* Closing it needs architecture (b) — adapter-local evaluation — for which the plane would
> have to carry geometry. The field that would carry it is an additive `zone_geometry` arm on
> `ZoneConstraints` bearing each containing zone's polygon, frame and floor. Its absence today means
> exactly what it would mean then: *no geometry was sent, so evaluate nothing locally*. An adapter
> that receives geometry may evaluate ahead of the push; one that does not is in precisely the
> position it is in now.

> **A ZONE WITH NO SITE-ALIGNED POLYGON HAS NO CONTAINMENT ANSWER.** `ZoneGeometryAbsent` — a zone
> that declares no polygon at all — and `FrameLocal` — a polygon the plane cannot place in the site
> frame — never appear in `containingZones`, because containment cannot be derived for a region
> whose shape in the site frame the plane does not know. **Robots inside such a zone's declared
> region are governed by the assignment path alone**: filters 11 and 12 still evaluate its
> restrictions when an action targets it, and nothing is pushed to a robot that only walks in.
>
> This is not a silent gap. `GeometryAvailable: False` already reports it per zone, with
> `IdentityOnly` or `FrameLocalGeometry` as the reason, and the same condition already governs
> boundary-breach detection.
>
> *Seam.* A later frame-alignment answer — a transform from a named local frame into the site frame,
> published where the plane can read it — would let a `FrameLocal` zone participate with no change
> to anything here: the zone would start appearing in the candidate set. What would have to
> be true is that the transform is authoritative and versioned, because a containment answer derived
> from a stale transform is worse than none. `ZoneGeometryAbsent` cannot be closed by any transform:
> a zone that declares no shape has no shape, and its membership stays declared rather than derived.

> **THERE IS NO TIME DIMENSION.** A restriction is on or off, and changes only when a principal
> edits `spec`. *"No camera recording in this ward between 22:00 and 06:00"* is still inexpressible.
> The model is edge-triggered on **containment** and on **`spec` edits**, and on nothing else — in
> particular there is no clock anywhere in it, and no controller wakes up at 22:00.
>
> *Seam.* `ZoneOperatingRestriction` and `EffectiveZoneRestriction` are shaped so a window can be
> added as an **optional** field whose absence keeps today's meaning exactly: a rule with no window
> is in force always, which is what every rule written before the field existed meant. A windowed
> rule would additionally need a wake-up — the projection would become a function of time, which
> FZ-R5 guarantees, in draft 4, that it is not — and that is the part a later decision must take
> deliberately rather than inherit.

> **THE ADAPTER'S COMPLIANCE IS A CLAIM, NOT A MEASUREMENT.** `ZoneConstraintsAck.acknowledged` says
> an adapter applied the set. Nothing in a control plane can verify that a camera is off (ADR-0058
> §12). The plane records what was claimed and cannot check it, and an operator surface that renders
> an acknowledgement as *enforced* is claiming a property this contract does not provide.
>
> *Seam.* Whether an attestation should be carried at all, and on what message, is an open question
> this ADR deliberately does not pre-empt. `ZoneConstraintsAck` carries `unhonourable_subjects` —
> what the adapter says it **cannot** honour where the robot now is — which is reportable and, unlike
> the same list on `AssignActionResult`, **not preventable**: the robot is already there and there is
> no assignment to decline. That asymmetry is the crossing lag arriving as data rather than as a
> disclaimer.

### 7. What this deliberately does not do: cancel work

A robot is **told what it may run**. Its work is not taken away.

Re-evaluating in-flight actions on a restriction change and cancelling those that no longer pass was
the obvious alternative, and it is refused here on ADR-0058 §10's own reasoning: a control plane that
starts **moving or stopping robots** in response to an edit of a spec field is a new autonomous
behaviour, and this surface is explicitly not a safety system. §10 already made `Report` the default
for the entry axis for exactly this reason, and inventing a harsher default for the operating axis
would be incoherent with it.

The substitution is better, not only cheaper: an adapter that honours the pushed set has **stopped
the camera without anyone stopping the robot**. Cancellation would have stopped the work and left the
camera's state to the same unverifiable adapter behaviour.

If a case is found that genuinely needs cancellation — an operating rule whose violation cannot be
corrected by ceasing the subject — it needs an `occupantAction` analogue for axis 3 so an operator
**chooses** it rather than receives it. That is a separate decision and is not taken here.

## Alternatives considered

**Forbid overlap where either zone declares a restriction, by admission.** Narrow and cheap; the
webhook already walks the chain. Rejected: it makes a legitimate geometry illegal the moment a rule
is written, which is the permanent-lockout shape ADR-0058's Amendment 1 §A1.3 had to reason about
already — and it would be a wall an operator hits while drawing a map, for a reason about a rule they
may not have written.

**Decide that `currentZone` is definitional — a robot is in exactly one zone by fiat.** Cheapest, and
the only option requiring no code. Rejected because it publishes a standard in which a denial is
escaped by drawing a second polygon, and the escape would be invisible: no filter fires, no condition
goes false, no event is emitted, and every component behaves exactly as specified.

**Resolve restrictions per robot into `FleetZone.status`.** Rejected: it would make the zone's
projection robot-relative, which FZ-R4's shape does not admit, and the obvious field — *which robots
are inside this zone at any given moment* — is written from position telemetry on every frame, the one write
RA-1 prohibits outright. The containment set lives on the **Robot**, where the per-robot fact belongs
and where it is written on a transition.

## Consequences

- A denial, a bound or a requirement in **any** zone containing a robot binds it. The four escapes
  tabulated above are closed, and each is covered by a test that fails against the previous resolver.
- `Robot.status.containingZones[]` is a new, additive, optional field. Absent means what it meant
  before the field existed: nothing was derived.
- An adapter can now comply with FA-R3. Whether it does is unverifiable, as it always was.
- A restriction written while a robot is standing in the zone reaches its adapter. Enforcement is no
  longer admission-only.
- One more optional `Command` arm, one contract minor, and an eviction window that moves with it:
  `>=1.2.0 <1.4.0`. The three in-tree adapters were moved to `1.2.0` in the same change, which is the
  version they implement; the three external reference adapters declare `1.0.0` and were already
  outside the previous window.
- **The crossing lag is now a stated property of the system rather than an unexamined one.** That is
  a consequence worth naming on its own: the previous design had the same lag and never said so,
  because it never sent anything at all.

## Amendment 1 — the time dimension stays absent in draft 4

- **Date:** 2026-09-26
- **Basis:** ADR-0004's second rule — `FleetZone` is `alpha`, RFC-0001 is in `Draft`, and this
  records a refusal of a feature §6 left open. Per `docs/adr/README.md` the text above is left
  intact and this is appended.
- **What is unchanged:** every decision above, and §6's seam, which remains the shape a window
  would take.

§6 closes its time-dimension limit with *"that is the part a later decision must take deliberately
rather than inherit."* That decision is now taken, and it is a refusal for draft 4, recorded in
ADR-0058 Amendment 4 §A4.2:

> **The push stays edge-triggered on containment and on `spec` edits, and on nothing else.** No
> clock is added to the Zone Controller, to filters 11 and 12 or to the push, and no window field is
> added to the schema. A restriction written for part of a day is inexpressible in draft 4.

The limit in §6 is therefore not an open question awaiting an answer. It is the answer for this
draft, and a reader should treat it as the behaviour draft 4 ships. Resolving a window at read
time, through one shared evaluator, is the lead candidate carried to draft 5.
