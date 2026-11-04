# map-to-fleetzone

Turns a ROS occupancy-grid map (`<name>.yaml` + `<name>.pgm`, what a map saver writes) into
`FleetZone` manifests. It emits and does not apply: nothing here contacts a cluster.

| File | What it does |
| :-- | :-- |
| `map_to_fleetzone.py` | One `FleetZone` per connected free component. `--mode traced` (the default) follows the free space's own boundary; `bbox` and `hull` are convex fits kept for hand checking. |
| `floor_to_member_zones.py` | One composite `FleetZone` for the floor and one member zone per room, split at narrow necks. |
| `traversable.py` | The driven tier: free space the robot's own trajectory covered, when a trajectory is supplied. |
| `render_zones.py` | A labelled PNG of the split, with the caveats printed on the image. |

```
python3 floor_to_member_zones.py map.yaml --name floor --namespace warehouse-a \
    --room-names room-a,room-b -o zones.yaml
python3 map_to_fleetzone.py map.yaml --mode traced --name floor > floor.yaml
```

Only PyYAML is needed.

## What the output does not claim

- **Unknown is not free.** A cell no ray reached is never inside a zone.
- **Every boundary is geometry.** A neck is not a door: a wide opening reads as one room and a
  narrow gap between furniture reads as two. Room names are typed by the operator.
- **Drop-off hazards were never checked.** A 2D lidar reports a descending staircase as open
  floor. Every emitted zone carries `tools.swarmada.io/map-to-fleetzone-drop-off-unverified: "true"`.
- **The frame is the map's.** `spec.physicalBounds.frame.alignment` is `FrameLocal`: the control
  plane carries the polygon and does not use it for containment until someone verifies the map
  frame against the site frame and changes it to `SiteAligned`.
- **Interior holes are not represented.** A zone is one ring. An excluded region inside a room
  becomes a child zone whose entry is denied (ADR-0069); the floor-mapping demo's worker does
  that for an operator's keep-outs.

The floor-mapping demo (`examples/floor-mapping/`) runs these tools in a cluster, on a map a
robot saved by reference, and writes the result as a proposal the operator reviews and applies.
