---
amendment: zone-exclusions
title: "Zone Exclusions — an excluded region is a child zone whose entry restriction is Denied"
target: RFC-0001
status: accepted
created: 2026-09-17
decided-by: ADR-0069
---

# RFC-0001 amendment — Zone Exclusions

**What this file is.** An amendment to RFC-0001 accepted for v0.4, normative for v0.4 and published with it.
The next revision folds it into the chapters. At v0.4 Part A is kept here rather than spliced: this file is not listed in
`rfcs/rfc-0001-core-spec/rfc-0001.meta.yaml`, so `rfcs/dist/RFC-0001-core-spec.md` does not contain it.
Part B is already folded into `crds/fleetzone.md`, *Zone Controller: Current Zone Derivation*.

**The decision behind it** is recorded in
`docs/adr/0069-exclusions-are-child-zones-with-denial-semantics.md`. Where this text and that ADR
appear to differ, the ADR states the intent and this text states the contract.

**Where each part applies.** Two parts.

| Part | Destination file | Where |
| :-- | :-- | :-- |
| A | *FleetZone* | a new `#####` section *Excluded Regions* immediately after *Restriction Inheritance and Resolution*, the last section the zone-restrictions amendment adds after *Zone Extent Is Two-Dimensional* |
| B | *FleetZone* | a paragraph appended to *Zone Controller: Current Zone Derivation*, on nesting |

**Maturity.** Everything Part A requires of the control plane is **implemented and on the dispatch
path**, with one exception: the leaf-set collapse that FZ-X4 guards against is present and unresolved, as the table at the end records. No field is added by this amendment,
no CRD changes, and nothing here is new wire surface. The re-derivation commands are in the table at
the end. FZ-X4 is pinned by `TestExclusionShape_ThreeObjectsDispatchToTheOpenRemainder` and
`TestExclusionShape_TwoObjectsStillRefuseTheFloor` (`internal/controller/zone_exclusion_shape_test.go`).

> **No check grades FZ-X1.** No test covers it, and admission does not check an exclusion's `floor`
> or frame.

---

## Part A — *Excluded Regions*

*Splices into `crds/fleetzone.md` after `##### Restriction Inheritance and Resolution` (zone-restrictions amendment, Part A.2), whose rules and candidate filter 11 it cites.*

<!--rfc-num-->
##### Excluded Regions

**A region that robots must not enter, inside a zone they may, is a child `FleetZone` whose
`spec.restrictions.entry.mode` is `Denied`. It is not a hole in the parent's polygon.**

`spec.physicalBounds.polygon` is one closed ring (FZ-Z1). There is no interior ring, no hole list
and no multi-polygon, and none is planned: a consumer that did not implement an even-odd or winding
rule correctly would read the hole as navigable and route a robot into it, which is a permissive
failure on a surface whose posture is fail-closed (FZ-R6). A denied child zone fails the other way.
A consumer that understands nothing about exclusions still reads a zone that denies entry.

An exclusion is therefore an ordinary zone with an ordinary restriction, and everything already
specified about zones and restrictions applies to it unchanged.

| ID | Rule |
| :---- | :---- |
| **FZ-X1** | An excluded region MUST be expressed as a `FleetZone` whose `spec.parentZone` names the zone the region is cut out of, whose `spec.physicalBounds` covers the excluded region on the same `floor` and in the same frame as its parent, and whose `spec.restrictions.entry.mode` is `Denied`. The containment relation is `spec.parentZone`; `spec.memberZones` MUST NOT be used, because composition asserts nothing physical (FZ-C4) and is not traversed by the restriction fold (FZ-R1). |
| **FZ-X2** | Containment is a **set**, so a robot inside an excluded region is contained by the parent **and** the exclusion simultaneously, and both appear in `Robot.status.containingZones[]` (FZ-R10). Restrictions aggregate over that set and `Denied` absorbs (FZ-R2), so the robot is bound by the denial whatever else contains it. |
| **FZ-X3** | An action targeting the exclusion is refused **terminally**, with `status.failureReason: ZoneEntryDenied`, by candidate filter 11. An action targeting an ancestor has the exclusion **removed from its resolved leaf set and named** in the narrowed-out report — the whole floor is not refused for one excluded pillar, and the pillar is not dropped silently. Both behaviours are candidate filter 11's, unchanged: *absorbing over the target relation, narrowing over the subtree*, with the narrowed leaf set reported and never silent. An exclusion is a denied descendant like any other. |
| **FZ-X4** | **The zone that carries exclusion children MUST NOT be the zone robots occupy.** The navigable remainder MUST be its own sibling child of the same parent. A leaf set is resolved by walking the containment tree **downward to zones with no children**, so a zone that gains an exclusion child is no longer in its own leaf set: its open area then belongs to no leaf, candidate filter 2 excludes every robot reporting the parent as `status.currentZone`, and — once every remaining leaf is denied — filter 11 refuses the action terminally with *"the subtree is exhausted"*. The refusal is fail-closed and is still a floor-wide outage. The conforming shape is three objects: a container, a navigable remainder, and one exclusion per region. |
| **FZ-X5** | **An exclusion does not make a region protected.** A control-plane refusal means the action was **not dispatched**; it is not a claim that no robot enters the region. Nothing in this specification reaches a robot driven by its own vendor application, its own buttons or its own schedule, and entry restrictions are **not** carried on the adapter wire — only the operating axis is (FA-R7). An operator surface that reports an exclusion as *protected* rather than as *not dispatched to* claims a property this API does not provide. |
| **FZ-X6** | A robot already inside a region when its denial takes effect is governed by `spec.restrictions.entry.occupantAction`, unchanged: `Report` is the default and the only value implemented, **no robot is moved**, and one `ZoneEntryDeniedWithOccupants` Warning names who was inside. Withholding new work is credited to the denial, not to this field. |

> **No check grades FZ-X4.** It binds the deployment that writes the zone tree. The tests named
> under *Maturity* pin what the control plane does with each shape.

**Worked shape.** A floor with one excluded plinth:

```yaml
# the container — carries the children, holds no robots
apiVersion: swarmada.io/v1
kind: FleetZone
metadata: { name: floor1, namespace: site-a }
spec:
  displayName: "Floor 1"
  physicalBounds:            # the floor and frame FZ-X1 compares the exclusion against
    floor: 1
    frame: { id: site, alignment: SiteAligned }
    polygon: [ { "x": 0.0, "y": 0.0 }, { "x": 10.0, "y": 0.0 }, { "x": 10.0, "y": 10.0 }, { "x": 0.0, "y": 10.0 } ]
---
# the navigable remainder — robots are HERE; Robot.spec.zone names this
apiVersion: swarmada.io/v1
kind: FleetZone
metadata: { name: floor1-open, namespace: site-a }
spec:
  parentZone: floor1
  physicalBounds:
    floor: 1
    frame: { id: site, alignment: SiteAligned }
    polygon: [ { "x": 0.0, "y": 0.0 }, { "x": 10.0, "y": 0.0 }, { "x": 10.0, "y": 10.0 }, { "x": 0.0, "y": 10.0 } ]
---
# the exclusion
apiVersion: swarmada.io/v1
kind: FleetZone
metadata: { name: floor1-exclusion-plinth, namespace: site-a }
spec:
  parentZone: floor1
  physicalBounds:
    floor: 1
    frame: { id: site, alignment: SiteAligned }
    polygon: [ { "x": 4.0, "y": 4.0 }, { "x": 6.0, "y": 4.0 }, { "x": 6.0, "y": 6.0 }, { "x": 4.0, "y": 6.0 } ]
  restrictions:
    entry:
      mode: Denied
      reason: "structural column and its base plinth"
      occupantAction: Report
```

An action targeting `floor1` resolves to the leaf set `{floor1-open}`, reports
`floor1-exclusion-plinth` as narrowed out, and dispatches. An action targeting
`floor1-exclusion-plinth` is refused terminally. A robot at (5, 5) carries
`containingZones: [floor1, floor1-exclusion-plinth, floor1-open]`: the container's polygon contains the point too, and the set is sorted (FZ-R10).

**The `y:` key is quoted deliberately.** Under YAML 1.1 scalar resolution a bare `y` is the boolean
`true`, and `sigs.k8s.io/yaml` still implements that rule, so an unquoted key collapses the ring onto
`y = 0`. `config/samples/fleetzone_edge.yaml` quotes its polygon keys for the same reason.

**The cost is object count, and it is not small.** One `FleetZone` per excluded region, each with a
name, a status, a reconcile loop and a restriction projection. Measured on a traced 55.16 m² floor
capture, the tracer returns **61** enclosed regions totalling 2.615 m²; modelled one-for-one that
floor is 63 objects. Three of the 61 exceed a 25 cm square. This specification sets **no size
threshold** and states none: a region an operator does not name is not excluded, and a region below
whatever threshold an operator chooses stays inside the parent's ring and remains navigable.

---

## Part B — nesting is decided before centroid distance

*Splices into `crds/fleetzone.md`, appended to `##### Zone Controller: Current Zone Derivation`.*

**Where an entry-denying leaf lies inside another matching leaf, `currentZone` is the denying
leaf.** The derivation prefers leaf zones; among leaves, it prefers one that carries its own entry
denial and whose polygon lies inside another matching leaf, and only then the nearest centroid. An
exclusion and the navigable remainder that encloses it are both leaves at the same depth, and the
remainder's ring cannot have the exclusion cut out of it (FZ-Z1), so both contain the same points;
without the preference, a robot inside a **large** exclusion would report the enclosing zone. The
preference is narrow: overlaps that do not nest, or where no leaf denies entry, arbitrate by
centroid exactly as before, so no deployment without a nested entry denial sees a different
`currentZone`. `status.containingZones[]` is unaffected and remains what the restriction union
reads (FZ-R10 … FZ-R12).

---

## Re-derivation

Run against this tree, not copied from a previous session's summary.

| Claim | State | Re-derived by |
| :-- | :-- | :-- |
| Containment is a set | **IMPLEMENTED** | `grep -n "func DeriveContainingZones" internal/zone/geometry.go` → 249 |
| Restrictions union over that set | **IMPLEMENTED** | `grep -n "func unionEffectiveRestrictionsWithUnresolved" internal/controller/zone_restrictions.go` → 419 |
| `Denied` absorbs in the fold (FZ-R2) | **IMPLEMENTED** | `grep -n "func foldRule" internal/controller/zone_restrictions.go` → 203 |
| Entry mode is pinned to `Denied` | **IN THE GENERATED CRD** | `grep -n "presence of an entry rule IS the denial" api/v1/fleetzone_restrictions_types.go` |
| FZ-X3 terminal refusal on the target | **IMPLEMENTED**, on the dispatch path | `sed -n '186,191p' internal/controller/dispatch_zone_restrictions.go` |
| FZ-X3 narrowing and naming on an ancestor | **IMPLEMENTED** | `sed -n '211,233p' internal/controller/dispatch_zone_restrictions.go` |
| FZ-X4 the leaf-set collapse | **PRESENT, UNRESOLVED** — the reason FZ-X4 is a MUST | `grep -n "func resolveZoneLeaves" internal/controller/dispatch_filters.go` |
| FZ-X5 entry is not on the wire | **BY DESIGN** | `sed -n '209,218p' internal/controller/zone_containment.go` |
| FZ-X6 occupant report | **IMPLEMENTED** | `internal/controller/zone_occupant_report.go` |
| Part B nesting arbitration | **IMPLEMENTED** | `grep -n "func preferNestedDenials" internal/zone/geometry.go` → 164 |
| The 61-hole measurement | **REPRODUCIBLE** with the map tool, which is not yet published | from the tool's directory: `python3 map_to_fleetzone.py testdata/floor1.yaml --free-thresh 0.196 --name floor1 \| grep enclosed-hole` |
