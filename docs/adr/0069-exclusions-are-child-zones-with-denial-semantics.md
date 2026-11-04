# ADR-0069: An excluded region is a child zone whose entry restriction is `Denied`, not an interior ring in `PhysicalBounds.Polygon`

- **Status:** accepted
- **Date:** 2026-09-17
- **Deciders:** Maintainer (decided 2026-09-17); API Designer, Controller Engineer
- **Related:** ADR-0058 (zone restrictions are one model across three axes), ADR-0059 (a robot is
  bound by every zone that contains it), ADR-0064 (composite-zone validity splits across CEL and the
  webhook), ADR-0063 (a field this tree cannot evaluate is absent, not inert), ADR-0067 (a
  measurement gate refuses a run rather than widening the floor);
  `rfcs/amendments/RFC-0001-zone-restrictions.md` (FZ-R1 … FZ-R12),
  `rfcs/amendments/RFC-0001-zone-exclusions.md` (the spec text this decision requires);
  `api/v1/fleetzone_types.go`, `internal/controller/dispatch_zone_restrictions.go`,
  `internal/zone/geometry.go`

## Context

A floor plan has holes in it. A structural column, a plinth, a stairwell, a furniture island, a
region a survey left unresolved: mapped space that is inside the outline of the floor and that a
robot must not enter. `FleetZone` has had nowhere to put them.

`PhysicalBounds.Polygon` is `[]Point` (`api/v1/fleetzone_types.go:227`) — one closed ring, with at
least three vertices, the last joining back to the first. There is no `Holes`, `InteriorRing` or
`MultiPolygon` field in `api/` or in `internal/`. So a zone traced from a real occupancy grid
encloses its own holes, and the control plane reads every point inside the ring as inside the zone.

That gap was measured on 2026-09-15 against the first multi-room capture of a real floor, and stated
then as *excluded regions have nowhere to go*. The geometry half of the same measurement — convex
fits enclosing walls and neighbouring rooms — was closed the same day by a contour tracer; this half
was left, because it is a modelling decision rather than a geometry change.

The machinery that would have to enforce an exclusion, whatever its representation, already exists:

- **Containment is a set.** `DeriveContainingZones` (`internal/zone/geometry.go:198`) returns the
  name of **every** zone whose polygon contains a position, not one arbitrated winner. ADR-0059
  states why — *"a robot is bound by every zone that contains it"* — and FZ-R10 … FZ-R12 carry it.
- **Restrictions aggregate over that set and denial absorbs.** `resolveZoneRestrictions`
  (`internal/controller/zone_restrictions.go:101`) folds the ancestor chain root-downward;
  `foldRule` (`:203`) implements the mode lattice in which `Denied` wins outright (FZ-R2);
  `unionEffectiveRestrictionsWithUnresolved` (`:419`) unions the projections across the containment
  set for the wire push.
- **The model has an entry axis, and its only mode is denial.** ADR-0058 defines one restriction
  model across entry, capability and operating. `ZoneEntryRestriction` carries a CEL rule pinning
  the mode (`api/v1/fleetzone_restrictions_types.go:297`): *"spec.restrictions.entry.mode must be
  Denied: a rule exists only to restrict, and the presence of an entry rule IS the denial."* There
  is no `Allowed` value at any axis, which is what makes a hole in an ancestor's boundary
  unwritable.
- **Candidate filter 11 is live and fail-closed.** `evaluateZoneRestrictions`
  (`internal/controller/dispatch_zone_restrictions.go:110`) is action-scoped and terminal, unlike
  filters 2 and 8 which degrade on a failed read. It refuses an action whose target carries an
  effective entry denial (`:186-191`) and holds — rather than trims — an action whose subtree it
  cannot resolve (`:238-248`).
- **Composite zones already exist**, with `MemberZones` (`api/v1/fleetzone_types.go:90`), FZ-C1…C6,
  and the CEL rules at `fleetzone_types.go:25` and `:53` forbidding `sharedResources` and requiring
  `maxConcurrentRobots == 0` on a composite (ADR-0064).

So the question this ADR settles is not whether the plane can refuse entry to a region. It is which
of two representations of "this region is excluded" the plane is told about.

## Decision

**An excluded region is modelled as a child `FleetZone` whose `spec.restrictions.entry.mode` is
`Denied`, covering the excluded polygon. It is not modelled as an interior ring in
`PhysicalBounds.Polygon`.** No geometry type changes; `PhysicalBounds` keeps its single ring.

The specifics needed to act on it:

- The exclusion's `spec.parentZone` names the zone it is cut out of, so the containment tree carries
  the relation and FZ-R1's fold applies. Composition (`spec.memberZones`) is **not** used for this:
  it asserts nothing physical (`fleetzone_types.go:62-71`), and an exclusion is a claim about space.
- The exclusion's `spec.physicalBounds.polygon` is the excluded region, in the same frame and on the
  same floor as its parent. It is a zone in every other respect: it has a name, a status and a
  reconcile loop.
- `spec.restrictions.entry.reason` carries the operator's own words. It is quoted verbatim into the
  refusal an operator reads (`dispatch_zone_restrictions.go:276` `entryDeniedMessage`), and a
  refusal without one cannot say why.
- **The zone that carries exclusion children is never the zone robots occupy.** The navigable
  remainder is its own sibling child. See *The shape is three objects, not two* below; this is a
  requirement, not a style note, and the naive two-object form takes the floor out of service.

### What was established by reading the code, and how

A child zone carrying `entry: Denied` produces correct refusal today **on three of the four paths
that matter**, with no new enforcement. Measured 2026-09-17 against this tree, with throwaway tests
calling the real filter and geometry functions:

| Path | Result | Where |
| :-- | :-- | :-- |
| A robot inside the excluded polygon is contained by both zones | `containingZones = [floor1 floor1-exclusion-plinth]` | `internal/zone/geometry.go:198` |
| It reports the exclusion as `currentZone` (small exclusion) | `currentZone = "floor1-exclusion-plinth"` | `geometry.go:96`, leaf preference at `:94` |
| An action targeting the exclusion is refused, terminally | `refuse=true terminal=true reason=ZoneEntryDenied` | `dispatch_zone_restrictions.go:186-191` |
| An action targeting the parent has the exclusion removed from its leaf set and **named** | `narrowedOut = [floor1-exclusion-plinth]` | `dispatch_zone_restrictions.go:211-233` |

The fourth path does not hold, and the ADR states it rather than assuming it.

### The shape is three objects, not two

`resolveZoneLeaves` (`internal/controller/dispatch_filters.go:237`) resolves an action's zone to the
zones at or below it that have **no children**. A zone with children is not in its own leaf set —
correct for a floor partitioned into rooms, where the rooms cover the floor, and wrong for an
exclusion child, which covers a hole rather than a share.

Measured, on `floor1` with two denied children and nothing else:

```
resolveZoneLeaves(floor1) = [floor1-exclusion-plinth floor1-exclusion-stairwell]   floor1 absent
                                                     — true with NO restriction declared anywhere
filter 2  : robot currentZone=floor1 -> excluded, "not among the leaf zones of the action's zone"
filter 11 : refuse=true terminal=true, "zone \"floor1\" is not itself denied, but EVERY zone within
            it is … The subtree is exhausted, so there is nowhere in \"floor1\" to send a robot"
```

Adding one excluded pillar refuses the whole floor. The direction is fail-closed, which is why this
is a shape requirement and not a safety defect, and it is still a floor-wide outage.

The shape that works today, verified in the same run:

```
floor1                      container; carries the exclusion children
├── floor1-open             the navigable remainder — robots are HERE
└── floor1-exclusion-*      the excluded polygons, entry: Denied
```

```
TARGET=floor1 : refuse=false  leaves=[floor1-open]  narrowedOut=[floor1-exclusion-plinth]
filter 2      : robot currentZone=floor1-open -> admitted
```

This ADR requires that shape. It does **not** decide whether requiring it is the right answer, or
whether `resolveZoneLeaves` should instead admit a zone that carries its own polygon. That question
is open, and it is deliberately not answered here: it is a change to candidate filter 2's resolution
for every deployment, which is a larger decision than the one this ADR holds.

## Alternatives considered

### Interior rings in `PhysicalBounds.Polygon` — rejected, and the reason is the failure direction

The obvious representation: let `PhysicalBounds` carry a list of rings, outer first, interiors after,
and have the point-in-polygon test apply an even-odd or winding rule.

It is rejected for two reasons, and the second is the deciding one.

**It is an API change to a shipped v0.4 surface.** `Polygon []Point` is in the generated CRD and in
every `FleetZone` written so far. Widening it means a new field or a new shape for an existing one,
`docs/api-principles.md` review, and a contract version step under ADR-0032 with N/N-1 skew — for a
type whose consumers include the edge node (`internal/edge/node.go`), the admission webhook's
simplicity predicate (`internal/zone/simple_polygon.go`), every adapter, and any client using the
Kubernetes API.

**And every consumer would have to implement even-odd or winding rules correctly. One that does not
treats the hole as navigable and drives into the excluded region.**

That is the same failure family as the three defects this zone geometry has already produced —
unknown cells counted as free, never-observed space presented as floor, and a convex fit enclosing
walls: a geometry subtlety that fails **silently and permissively**. A child zone fails the other way — a consumer that understands
nothing about exclusions still sees a zone that denies entry and refuses. **On a specification whose
safety posture is FZ-R6 "fail closed", a representation that degrades permissively contradicts the
document.**

The asymmetry is worth stating exactly. A consumer that ignores interior rings is *wrong in the
direction of entering the excluded region*, and nothing in the system can detect that it did. A
consumer that ignores the exclusion zone reads a `FleetZone` with a restriction it does not
understand — and the paths that enforce the restriction are in the control plane, not in that
consumer, so the refusal still happens.

### Model the exclusion with `spec.memberZones` — rejected

Composition is top-down, forms a DAG, asserts nothing physical, and is traversed for targeting only
(`api/v1/fleetzone_types.go:62-71`, ADR-0064). An exclusion is a claim about space. Using the
composition edge would put a spatial claim on the relation that is documented to make none, and
would not reach FZ-R1's fold, which follows `spec.parentZone` only.

### `maxConcurrentRobots: 0`, an estop, or a `ZoneMaintenance` window — rejected

Each is already refused by `api/v1/fleetzone_types.go:137-147`, and for reasons that apply here
unchanged: `maxConcurrentRobots: 0` means the **opposite** (0 = no limit), an estop models an
emergency with a clear-and-resume lifecycle, and a maintenance window has a `Completed` phase. A
permanent boundary written as any of them is indistinguishable from one that was forgotten.

### Leave the holes inside the ring and disclose them in annotations — rejected as an end state

This is what `map-to-fleetzone` does today, and it was the right interim step: the tracer returns the
interior rings and every traced manifest records `enclosed-holes` and `enclosed-hole-area-m2`. But a
number in an annotation is read by nothing. ADR-0067 makes the same distinction for the measurement
gate: *a number printed that no code reads is documentation, not a control.* A robot can still be
routed into a disclosed hole.

## Consequences

### The cost is object count, and it is measured rather than estimated

Emitting `floor1` — the MentorPi capture, 342×218 at 0.05 m — through the map tool and counting the
enclosed regions. The map tool is not packaged as a command, so this runs from the tool's own
directory, `tools/map-to-fleetzone/`:

```
$ python3 map_to_fleetzone.py testdata/floor1.yaml --free-thresh 0.196 --name floor1
```

The `--free-thresh` override is not optional: at the map's own `free_thresh: 0.25` the tool exits 2
under ADR-0067's measurement gate, because grey level 205 would place 124.91 m² of never-observed
space in the free band.

The zone: **189 vertices, 55.16 m², ratio 1.05×** over 52.65 m² of covered free floor, one connected
component. Its annotations (the tool's annotation prefix elided):

```
…/enclosed-holes: '61'
…/enclosed-hole-area-m2: '2.6150'
```

**Modelled faithfully, one 55 m² floor becomes 62 `FleetZone` objects** — one floor plus 61
exclusions — each with a name, a spec, a status, a reconcile loop, a restriction projection and a
row in `kubectl get fleetzones`. Under the three-object shape above it is 63.

The hole sizes are not uniform, and the distribution is the whole of the honest answer. Recomputed
from the tool's own tracer over the same component:

| threshold | holes at or above it | objects for `floor1` |
| :-- | --: | --: |
| any | 61 | 63 |
| ≥ 0.01 m² (a 10 cm square) | 23 | 25 |
| ≥ 0.0625 m² (a 25 cm square) | 3 | 5 |
| ≥ 0.25 m² | 2 | 4 |
| ≥ 1.0 m² | 1 | 3 |

The three largest are 1.22 m², 0.83 m² and 0.12 m²; the remaining 58 total 0.55 m².

The other two captures in `testdata/`, same command with `--free-thresh 0.196`: `map_01` 98 holes /
2.065 m² over 45.38 m², and `map_02` 138 holes / 4.285 m² over 70.21 m².

**Stated as a limit rather than decided away.** At one object per traced hole this representation
does not scale to a raster-derived floor: 62 objects for 55 m² extrapolates to four figures for a
warehouse. It is usable at a threshold, and **no threshold is decided here** — nothing in this tree
measures which holes matter, and choosing one is a claim about robot footprint, sensor range and
what an operator is willing to leave un-modelled. A hole below the threshold stays inside the ring
and a robot can still be routed into it, exactly as today. That is the trade this ADR makes: the
excluded regions an operator names are enforced; the ones nobody names are not, and the count of
what was not named is in the annotations.

### Obligations this creates

- The exclusion pattern must be described **where `FleetZone` is specified**, so a reader does not
  have to infer it from three ADRs. `rfcs/amendments/RFC-0001-zone-exclusions.md` is the staged
  chapter text, with its splice point named at its head.
- The leaf-set collapse is open, and the three-object shape is a requirement until it is answered.
  Nothing enforces the shape: an operator who hangs an exclusion directly off the zone robots occupy
  gets a floor-wide terminal refusal and a message that names the exclusions.
- The arbitration is open: `DeriveCurrentZone` arbitrates between two contained leaves by centroid
  distance, which is not a nesting test. A robot standing inside a **large** exclusion reports the
  enclosing zone as `currentZone` and remains a dispatch candidate. Measured: correct for an
  exclusion at (1,1)-(3,3) in a 10×10 zone, wrong at (7,7) inside an (0,0)-(8,8) exclusion in the
  same zone. `containingZones` is right in both.

### What this decision does NOT decide

- **It does not change `PhysicalBounds`.** No interior rings, no multi-ring polygons, no geometry
  field. That remains available as a future decision, and this ADR is not a supersession of it.
- **It does not change any enforcement path.** Filter 11, filter 2, the fold and the containment set
  are as they were. The two candidate remedies for the leaf-set collapse — admitting a zone with its
  own polygon into its own leaf set, or marking the parent edge as an exclusion edge — are named in
  *The shape is three objects, not two* and decided nowhere.
- **It does not decide a hole-size threshold**, nor that the map tool should emit exclusion zones at
  all. Whether `map-to-fleetzone` grows an `--emit-exclusions` mode, and what it would filter on, is
  a separate decision with its own evidence.
- **It does not make an excluded region protected.** ADR-0058's limit applies unchanged: a control
  plane refusal means the action was **not dispatched**. It is not a claim that no robot enters the
  region — not by its own vendor application, its own buttons, its own schedule, or a person
  carrying it. An operator surface that reports an exclusion as *protected* rather than as *not
  dispatched to* is claiming a property this API does not provide.
- **It does not decide what happens to a robot already inside an exclusion when the denial takes
  effect.** ADR-0058 §10 already decided that: `occupantAction: Report` is the default and the only
  value implemented, no robot is moved, and the Zone Controller emits one
  `ZoneEntryDeniedWithOccupants` warning naming who was inside
  (`internal/controller/zone_occupant_report.go`).
- **It does not put entry denials on the adapter wire.** Only the operating axis travels
  (`internal/controller/zone_containment.go:209-218`); entry and capability are decided before a
  robot is chosen. An adapter is not told that the region its robot is standing in is denied.

### RA-1

Unaffected. An exclusion zone is a `FleetZone` like any other: its projection is written by the Zone
Controller when the zone graph or a restriction changes, never on a telemetry tick. The containment
set a robot publishes is gated on the set **moving**, and the constraint push on the resolved rule
set moving (`internal/controller/zone_containment.go:44-63`), both of which an exclusion zone
participates in exactly as its parent does.

---

## Amendment 1 — the hole threshold, from a measured footprint: maximum inscribed radius, at the smallest footprint in the fleet

- **Date:** 2026-09-17
- **Basis:** the ADR above declines a threshold because *"nothing in this tree measures which holes
  matter"*. Something now does. the MentorPi was taped on 2026-09-17: **100 mm**
  across the metal chassis at the front axle, **170 mm** across the wheels. The wheels stick out, so
  they govern — **half-width 0.085 m**. Per `docs/adr/README.md` the text above is left intact and
  this is appended.
- **What is unchanged:** the decision, FZ-X1 … FZ-X6, and `PhysicalBounds`. No geometry type
  changes, no ring gains a hole, no enforcement path moves. This amendment decides **which holes are
  worth an object**, which the ADR above names as open, and nothing else. In particular it does
  **not** decide that the map tool should emit exclusion zones; that remains open.

### A1.1 — The rule

**A hole a robot cannot physically be inside does not need to be a zone.** An exclusion over a
region no robot can occupy enforces nothing: it is an object with a name, a spec, a status, a
reconcile loop and a restriction projection, whose containment test can never fire.

Concretely, and this is the whole of the rule:

> An enclosed hole is modelled as an exclusion **iff the largest circle that fits inside it has
> radius at least the smallest footprint half-width in the fleet.**

### A1.2 — Area is the wrong test, and the table above is on area

The *Consequences* table thresholds `floor1`'s 61 holes on area. Area does not answer the question.
A **0.05 × 1.80 m slot** and a **0.30 × 0.30 m blob** both cover 0.09 m²; the slot admits nothing
and the blob admits a MentorPi with 65 mm to spare. An area threshold either keeps both or drops
both, and either way it is wrong about one of them.

What decides entry is the **maximum inscribed radius** — a distance transform over the hole's
cells, taking the maximum — compared against the half-width.

**Where the two rules disagree on the three captures, measured rather than asserted.** Re-derived
2026-09-17 with `map_to_fleetzone.py --free-thresh 0.196 --fleet-min-half-width 0.085`:

| capture | holes | area ≥ 0.0625 m² (the table's "25 cm square" row) | radius ≥ 0.085 m | disagreement |
| :-- | --: | --: | --: | :-- |
| `floor1` | 61 | 3 | 3 | none — the same three holes |
| `map_01` | 98 | 4 | 4 | none — the same four holes |
| `map_02` | 138 | 4 | 3 | **one hole** |

**On two of the three captures the two rules give the same answer, and that is worth stating.** It
is also not a reason to threshold on area: the agreement is a property of those two maps, and
`map_02` shows what it costs when it fails. `map_02` carries a 0.09 m² hole — a diagonal ribbon
0.60 × 0.40 m across, lower-left corner at (−1.05, 0.28) m in the map frame — whose largest
inscribed circle is **0.080 m**. It is 1.44× the "25 cm square" area threshold and admits no
MentorPi. Under the area rule it becomes a `FleetZone` with a reconcile loop and a denial that can
never bind. Under the radius rule it stays a disclosed hole, which is what it is.

At the looser area row the two rules part company everywhere: area ≥ 0.01 m² keeps 23 / 27 / 17
holes across the three captures, of which **20 / 23 / 14** admit no robot at 0.085 m.

**The search is bounded, and it is bounded in the safe direction.** The maximum is taken over
candidate centres on a ¼-cell lattice, so the value found is a **lower** bound on the true radius —
and under-stating a radius *drops* a hole a robot fits into, which is the unsafe direction. The
lattice's worst-case error (`√2 / 2·samples` cells, 8.8 mm at 0.05 m/px) is proven and added back;
the comparison is against that **upper** bound. A hole kept in error costs one object. A hole
dropped in error is a region a robot can drive into that nothing models.

### A1.3 — The threshold is a property of the FLEET, not of one robot

A **smaller** robot enters holes a larger one cannot. So the rule fails closed *downward*: filter by
the robot you happen to have measured, and the moment a smaller one joins the fleet, holes that were
correctly dropped become enterable — and **nothing re-emits them**. The manifests are already
applied; the regions they omit are not in the API at all; no controller can discover the omission,
because the omission is an absent object.

**The threshold is therefore the smallest footprint in the fleet, not the current robot's.** This is
FZ-R6 applied to the emission step rather than the evaluation step: when in doubt, model the
exclusion.

**The re-emission obligation, stated as an obligation.** Adding a robot whose half-width is below
the value a zone set was generated against **requires re-emitting that zone set against the new
minimum**. Nothing in the control plane enforces this and nothing can: the input is a fleet
composition the zone objects do not reference.

What makes it *detectable* is that the artefact records the footprint it was generated against.
`map_to_fleetzone` writes `fleet-min-half-width-m-applied` into every emitted manifest, beside the
existing `enclosed-holes` / `enclosed-hole-area-m2` keys and beside the counts and areas of what was
dropped. An operator onboarding a smaller robot can read that annotation off every applied
`FleetZone` and see which zone sets are stale. An annotation is read by nothing — ADR-0067's
distinction holds, and the ADR above applies it to this very tool — so this is disclosure, not a
control. It is the difference between an obligation someone can check and one nobody can.

### A1.4 — There is NO default footprint, and there must not be one

The measurement above was taken because a sibling instrument, `corridor_probe.py`, carried an
unverified `--half-width 0.22` as a **default**, documented as *"footprint radius plus margin"*, 2.6× the measured robot, for weeks,
unchecked — because a default is silent.

**The filter flag therefore has no default.** With no footprint supplied the tool counts every hole,
exactly as before, and says in both the manifest and the stderr summary that no footprint was
supplied and nothing was filtered. A silent default here would repeat the defect in the instrument
that revealed it, one step further downstream — and the artefact it corrupted would be applied
`FleetZone` objects rather than a corridor figure.

### A1.5 — The cost, re-derived

`ADR-0069`'s counts were **reproduced, not inherited**, at the same command and the same
`--free-thresh 0.196` override: 61 / 98 / 138 holes over 2.6150 / 2.0650 / 4.2850 m², and the
`floor1` area table (61 / 23 / 3 / 2 / 1) reproduces row for row.

Objects under the three-object shape — one container and one navigable remainder per traced
component, plus one exclusion per modelled hole:

| fleet minimum half-width | `floor1` | `map_01` | `map_02` |
| :-- | --: | --: | --: |
| none supplied (the ADR above) | 61 holes → **63** | 98 holes → 102 | 138 holes → 140 |
| 0.050 m | 12 → 14 | 13 → 17 | 9 → 11 |
| 0.060 m | 4 → 6 | 7 → 11 | 5 → 7 |
| 0.075 m | 3 → 5 | 6 → 10 | 4 → 6 |
| **0.085 m — the measured MentorPi** | **3 → 5** | **4 → 8** | **3 → 5** |
| 0.100 m | 3 → 5 | 4 → 8 | 3 → 5 |
| 0.110 m | 2 → 4 | 4 → 8 | 3 → 5 |
| 0.220 m — the unverified value the tape measurement replaced | 2 → 4 | 2 → 6 | 1 → 3 |

`map_01` traces two components, which is why its object count sits four above its hole count.

**`floor1` goes from 63 objects to 5.** The ADR above states the limit as *"at one object per traced
hole this representation does not scale to a raster-derived floor"*; at the measured footprint it
scales, and the reason is that 58 of `floor1`'s 61 holes are cracks rather than places — they total
0.4450 m² between them.

The threshold was **not tuned until the count looked nice.** 0.085 m is a tape measurement; the
plateau between 0.075 m and 0.100 m is a fact about these three maps, not a chosen operating point,
and the sweep is published above so that a different fleet reads its own row.

### A1.6 — What this amendment does NOT decide, and what the rule does not claim

- **It does not claim an excluded region is protected.** FZ-X5 is unchanged.
- **It does not decide that `map-to-fleetzone` should emit exclusion zones.** The tool counts and
  discloses; it still emits one ring per component and no exclusion children. Whether it grows an
  `--emit-exclusions` mode remains open, as the ADR above says.
- **It does not change what happens to the holes it drops.** A dropped hole stays inside the ring
  and a robot can still be routed into it, exactly as today. Its count and its area are in the
  annotations.
- **It does not cover a robot straddling a hole's boundary.** The test asks whether the robot fits
  *inside* the hole, not whether its centre can be over the hole while its body rests on mapped
  floor. A robot half on the floor and half over a 5 cm sliver of never-observed cells reports a
  position inside that sliver, and `DeriveContainingZones` would put it in an exclusion there if one
  existed. **No threshold above zero excludes that case**, so no threshold — including this one —
  should be read as claiming it. What this rule claims is narrower and is the claim the object count
  turns on: a hole no robot can occupy is not worth an object.
- **It does not settle the planner's radius.** The deeper finding of that same measurement stands: 0.085 m answers
  *"will the robot physically fit"*; *"will Nav2 plan a path through it"* is answered by the vendor's
  own `robot_radius`, which has still never been read off the robot. A hole this rule drops is one
  no robot fits in; it is not a claim about what the stack will plan through.

## Amendment 2 — the three-object shape is a standing requirement, and a nested entry denial wins `currentZone`

- **Date:** 2026-09-26
- **Basis:** ADR-0004's second rule — `FleetZone` is `alpha` and RFC-0001 is in `Draft`. Per
  `docs/adr/README.md` the text above is left intact and this is appended.
- **What is unchanged:** the decision itself, Amendment 1, and every enforcement path: filter 11,
  filter 2, the fold and the containment set.

*Obligations this creates* named two questions as open. Both are now answered.

**The leaf-set collapse: the three-object shape is required, and nothing in code changes.** A
container carries the exclusion children, a navigable-remainder child is where robots are, and each
exclusion carries `entry: Denied`. This is a standing requirement of draft 4, not a stopgap until
another remedy lands. Neither remedy named in *The shape is three objects, not two* is taken:
admitting a zone into its own leaf set would make a robot reporting a partitioned floor dispatchable
for its rooms' work, and an exclusion edge would be a second `parentZone` edge type. Two tests pin
both halves of the requirement: `TestExclusionShape_ThreeObjectsDispatchToTheOpenRemainder` asserts
that the three-object shape dispatches to the open remainder and names the exclusion it narrowed out,
and `TestExclusionShape_TwoObjectsStillRefuseTheFloor` asserts that the two-object shape still
refuses the floor. If the second ever passes, the leaf-set walk has changed and this requirement is
to be revisited, not the test relaxed.

**The arbitration: a leaf that denies entry and lies inside another contained leaf is preferred.**
`DeriveCurrentZone` now narrows the contained leaves to those that carry their own entry denial and
whose polygon lies inside another contained leaf, when any do, and arbitrates by centroid only among
what remains. The measured case above now answers the exclusion at (7,7) inside the (0,0)-(8,8)
exclusion. The preference is narrow and fails closed: overlaps that do not nest, or where neither
leaf denies entry, arbitrate by centroid as before, so no deployment without a nested entry denial
sees a different `currentZone`. The preference is pinned by
`TestDeriveCurrentZone_NestedEntryDenialWinsInsideALargeExclusion`,
`TestDeriveCurrentZone_DenialPreferenceIsNarrow` and, through the Zone Controller's own candidate
builder, `TestExclusionShape_CurrentZoneInsideTheExclusionIsTheExclusion`.

Nesting is tested by the enclosed polygon's vertices, which is exact for a convex enclosing zone and
for the shared-edge shape this pattern produces. A truly nested exclusion is never missed. With a
concave enclosing zone, an exclusion whose edge crosses a notch while its vertices stay inside is
read as nested when it is not, and the error is in the fail-closed direction: the denying zone is
reported.
