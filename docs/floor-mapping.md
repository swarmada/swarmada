# Map a floor, confirm its zones, work in them

This guide walks an operator through the floor-mapping chain: a robot explores a floor in
segments, its map becomes a proposed set of `FleetZone` objects, the operator reviews and
applies the proposal, and work is then addressed to the zones. The applied zones are also the
one source of the keep-out mask the robot's adapter loads. The control plane gains no new
resource for this: the chain uses `FleetTask`, `FleetAction`, `ArtifactStore` and `FleetZone`
as they already are.

`examples/floor-mapping/run.sh` runs the whole chain on a `kind` cluster with the reference
simulation adapter standing in for the robot. The simulation moves nothing while it explores,
runs no SLAM, and draws the map it reports; every message it sends says `sim`.

## The pieces

| Piece | What it is |
| :-- | :-- |
| `explore-floor` | A `Custom` action. One segment explores until a threshold, returns to the start mark and saves the map. The payload names the operation as `operation: explore-floor`. |
| `Charge` | Ends when the battery reads full. It never moves the robot. |
| `ArtifactStore` `floor-maps` | Where maps live: a `VolumeClaim` store over the directory the adapter writes. |
| Zone-proposal worker | A `Deployment` in the demo overlay. It turns each saved map into a proposal, and renders the adapter's keep-out mask from the zones you apply. It can read a `FleetZone` and can't create, change or delete one. |
| Proposal | A `ConfigMap` named `zone-proposal-<action>`, holding `zones.yaml`, `summary.txt` and a whole-floor outline for review. |
| `floor-keepouts` | A `ConfigMap` with `keepouts_ref`: the reference to the keep-out mask rendered from the applied zones, and its digest. |

## 1. Explore

`examples/floor-mapping/tasks/explore-floor.yaml` runs one segment, then a charge and a
segment, up to three times:

```yaml
flow:
  sequence:
    - run: explore-first
    - loop:
        mode: Times
        times: 3
        body:
          sequence:
            - run: charge
            - run: explore-resume
```

The loop repeats a fixed number of times. It can't stop when no frontier is left: a loop's
`until` reads a member's `status.result.values` or `status.events[]`, and an adapter's report
reaches neither. A segment that finds no frontier ends at once and says `frontier: none-left`,
so the remaining iterations are a charge check and an empty segment each.

Each segment's final message carries the map reference and the digest of the map's manifest:

```text
explore-floor (sim): segment done; covered 15.8 of 27.3 m2 (58%); exhausted: none;
frontier: remaining; map-ref=floor-maps/warehouse-a/explore-floor-explore-first
map-sha256=<64 hex digits>
```

The map is saved at `<namespace>/<action-name>/` in the store: `map.yaml`, `map.pgm`,
`frame-delta.json` and `MANIFEST.sha256`, which lists the digest of every other file. A robot
adapter also saves its SLAM pose graph there; the simulation has none to save.

## 2. Review the proposal

The worker writes one proposal per segment. Find them, and read the summary of the segment
that left no frontier:

```sh
kubectl get configmaps -n warehouse-a -l swarmada.io/zone-proposal=true \
  -o custom-columns='PROPOSAL:.metadata.name,VERDICT:.metadata.annotations.swarmada\.io/zone-proposal-verdict'
kubectl get configmap zone-proposal-<action> -n warehouse-a -o jsonpath='{.data.summary\.txt}'
kubectl get configmap zone-proposal-<action> -n warehouse-a -o jsonpath='{.data.zones\.yaml}'
```

Before it writes a proposal, the worker checks that:

- the reference names the action's own path, in an `ArtifactStore` that declares the claim the
  worker mounts;
- `MANIFEST.sha256` hashes to the reported digest, and every file hashes to its manifest line;
- no file in the directory is missing from the manifest.

A map that fails any check still gets a proposal `ConfigMap`, with verdict `refused` and the
reason in `summary.txt`.

The proposal has one composite zone for the floor and one member zone per room. A room is a
region between narrow openings, found in the map's geometry. The map has no notion of a door,
so a wide opening joins two rooms and a narrow gap between furniture splits one. The room names
are the ones you typed in the `floor-mapping` `ConfigMap`. Every zone is marked
`drop-off-unverified`: a 2D lidar reports a descending staircase as open floor, and nothing in
the chain checked for one.

### Keep-outs

A keep-out is a polygon you draw, in the map's frame, in the `floor-mapping` `ConfigMap`:

```yaml
keepouts:
  - name: keepout-1
    reason: operator keep-out, fragile floor edge
    polygon:
      - {x: 1.2, y: 1.2}
      - {x: 1.8, y: 1.2}
      - {x: 1.8, y: 1.8}
      - {x: 1.2, y: 1.8}
```

Each keep-out becomes an exclusion child zone (ADR-0069). The room that holds it becomes a
container with two children: `<room>-open`, where robots are, and `<room>-<keepout>`, whose
entry is denied with your reason. Which room holds a keep-out:

- **Inside one room:** that room.
- **Over the edge of one room**, for example onto cells the map holds as occupied or unknown:
  that room. `summary.txt` says so.
- **In no room of the map:** the nearest room. `summary.txt` gives the distance, for example
  `keep-out 'keepout-1' lies in no room of this map; nearest room floor-room-a, 0.40 m away;
  kept whole, attached to floor-room-a`. Read it before you apply: a keep-out drawn in the wrong
  frame lands in whichever room is nearest.
- **Across more than one room:** the whole proposal is refused. Redraw it as one keep-out in each
  room.

A keep-out is never clipped and never dropped. In the second and third cases the exclusion zone's
`tools.swarmada.io/zone-proposal-keepout-source` annotation also records why it is in that room.

## 3. Confirm

Applying the proposal is the confirm step. Nothing else creates these zones:

```sh
kubectl get configmap zone-proposal-<action> -n warehouse-a \
  -o jsonpath='{.data.zones\.yaml}' | kubectl apply -f -
swarmctl get fleetzones -n warehouse-a
```

`zones.yaml` lists every container before its children; keep that order, because admission
refuses a zone whose parent doesn't exist yet. The zones also show in swarmtop's zone view.

## 4. The keep-out mask

Once the zones are applied, the worker renders the keep-out mask the robot's adapter loads,
from the exclusion zones you applied, in the map's frame. It saves the mask at
`<namespace>/keepouts-<digest>/` in the store, where the digest is that of the applied zone set,
so a changed zone set is a new mask and never an overwrite. It publishes the reference:

```sh
kubectl get configmap floor-keepouts -n warehouse-a -o jsonpath='{.data.keepouts_ref}'
```

The value ends in `@sha256:` and the digest of the set's `MANIFEST.sha256`. An `explore-floor`
payload carries it as `keepouts_ref`, and a robot adapter that checks `Navigate` goals is started
with it. The adapter refuses a set whose files don't match that digest. The zones the control plane
refuses work in and the cells the robot keeps out of then come from the same polygons. The
simulation adapter declares `keepouts_ref` and enforces no geometry.

## 5. Work in the zones

The generated zones are `FrameLocal`: their polygons are in the map's frame, and the control
plane uses them for neither containment nor occupancy. A robot is in a generated zone because
its `spec.zone` says so. Place it where it stands:

```sh
kubectl patch robot sim-robot-001 -n warehouse-a --type merge \
  -p '{"spec":{"zone":"floor-room-a-open"}}'
```

`tasks/patrol-generated-zones.yaml` addresses patrol work to `floor-room-a`. The control plane
resolves the room to its navigable remainder, `floor-room-a-open`, and offers the work only to a
robot whose zone is in that set. That decides which robot does the work, not where it goes: the
goals are the x/y in the payloads, and the control plane checks them against no polygon.

`tasks/goal-into-exclusion.yaml` addresses work to `floor-room-a-keepout-1`. The control plane
refuses it before any robot is chosen: the action goes to `Failed` with reason
`ZoneEntryDenied`, and its message quotes your keep-out's reason. The refusal is by the zone the
action names. The same goal addressed to `floor-room-a` is dispatched; keeping the robot out of
the keep-out is the robot's, with the mask from step 4.

## Payloads are written as `raw`

The task samples write each payload as `raw:`, the base64 of the JSON, with the JSON in a
comment above it. A payload written as plain keys reaches the adapter too, as the same object,
and a `FleetTask` member's plain-key payload reaches its child `FleetAction` intact. The one
exception is a payload whose only key is `raw`, holding a base64 string: written as plain keys it
reads as the `raw` form, so write such a payload in the `raw` form.

## What this chain doesn't do

- It doesn't apply a zone. The worker's `Role` can read a `FleetZone` and write none.
- It doesn't carry the map on the adapter protocol. The map travels by reference: a path and a
  digest in a status message.
- It doesn't check a goal against a zone's polygon, or track which generated zone a robot is in.
  On a robot, the adapter's keep-out mask, rendered from the applied zones, and its costmap keep
  it out of a keep-out. See
  [Known limitations](known-limitations.md#generated-zones-are-not-used-for-containment-and-goals-are-not-checked-against-them).
