# ADR-0030: Opt-in auto-removal of Offline auto-admitted Robots

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** Control Plane / CRD-controller capability
- **Related:** ADR-0014 (auto-admit DiscoveredRobots), ADR-0026 (per-robot liveness projection), ADR-0029 (Robot phase state machine), RFC-0001 §9.6.3.5 (lease death / single-executor safety)

## Context

Auto-admit (ADR-0014) creates a schedulable Robot for a discovered robot whose
suggested class + zone match the namespace policy. This is ideal for **ephemeral**
robots that come and go.

When such a robot disconnects **cleanly**, the client removes its Robot (e.g. the
adapter calls delete on the robot's behalf). But an **unclean** death — the
adapter process is killed, the network drops, the robot is force-powered-off —
sends no disconnect. The control plane marks the Robot `Offline`
(heartbeat lapse, ADR-0026) and then keeps it forever: swarmtop shows a growing
list of dead robots. The control plane has no policy of its own to reclaim an
auto-admitted robot whose adapter is provably gone.

Two forces constrain a fix:

1. **Warehouse robots must persist.** An operator-provisioned AMR that goes offline
   for a shift change or a charge must **not** be deleted — its identity, config,
   and history are durable. Blanket "remove all Offline robots" is wrong.
2. **Removal must be safe under the single-executor model (§9.6.3.5).** Deleting a
   robot that might still be executing an assigned action risks the exact
   double-execution the lease machinery exists to prevent. Removal must wait until
   any lease is **provably** dead — and a nil/absent lease horizon is *not* proof of
   death.

## Decision

Add an **opt-in** `SwarmadaConfig.provisioning` policy that lets the Robot
controller remove an **auto-admitted** Robot once its adapter presence is gone and
any lease is provably dead. Two fields:

- `autoRemoveOfflineRobots` (bool, default **false**) — the master opt-in.
- `autoRemoveOfflineGraceSeconds` (int32, default **300**) — dwell after the robot
  enters `Offline` before removal, so a brief adapter blip doesn't evict a robot
  that reconnects.

The Robot controller removes a Robot **iff all** of the following hold:

1. **Policy enabled** in the robot's namespace (`autoRemoveOfflineRobots: true`).
2. **The Robot was auto-admitted** — it carries `swarmada.io/auto-admitted: "true"`,
   stamped by `buildAutoAdmitRobot`. Operator-created robots lack the marker and are
   **never** removed, regardless of the policy. *This is the warehouse gate.*
3. **Adapter presence is gone** — phase `Offline` (heartbeat older than the
   namespace offline threshold, ADR-0026).
4. **Dwell elapsed** — `now ≥ status.offlineSince + grace`. `offlineSince` is already
   tracked by the controller (§9.3.8 offline accounting).
5. **Any lease is provably dead** — either `status.assignedAction` is empty (no
   lease), or the named FleetAction is gone (`NotFound`), or its
   `status.leaseExpiresAt` is provably past per **the existing** `leaseProvablyDead`
   (§9.6.3.5 condition 3: `now ≥ lease + skew`). A nil horizon is **not** death, and
   a transient lookup error **fails closed** (no removal).

Removal reuses the control plane's own lease-death primitive
(`leaseProvablyDead` / `leaseClockSkew` in `fleetaction_controller.go`) rather than
reinventing it, so it honors exactly the same safety the scheduler uses to decide a
robot has stopped.

## Alternatives considered

- **TTL / ownerReference garbage collection.** Rejected: a time/owner GC can't see
  the lease and would evict a robot mid-action, violating §9.6.3.5.
- **Blanket removal of every Offline robot.** Rejected: breaks warehouse
  persistence. The auto-admitted marker + opt-in scope removal to exactly the
  ephemeral fleet.
- **Reap on an "unclean disconnect" signal from the adapter.** Rejected: the control
  plane cannot reliably distinguish clean from unclean at the adapter layer.
  "Presence gone (Offline) + lease provably dead" is the observable, safe equivalent
  and needs no new wire signal.
- **A finalizer-based drain before delete.** Rejected as unnecessary: the
  lease-dead gate already proves nothing is executing, so there is nothing to drain.
- **Remove on the first Offline reconcile (no dwell).** Rejected: a robot that drops
  its connection for a few seconds would be evicted and forced to re-discover. The grace
  dwell makes removal deliberate.

## Consequences

- **Ephemeral fleets self-clean** without relying on a downstream disconnect call;
  an unclean robot death is reclaimed after the dwell once its lease is dead.
- **Warehouse fleets are unaffected**: default off, and even when on, only
  auto-admitted robots are eligible.
- **New obligations:** `buildAutoAdmitRobot` must stamp the marker; the reaper must
  keep the lease check fail-closed (nil ≠ dead, transient error ⇒ no removal); the
  controller requeues toward the reap horizon so removal fires promptly.
- **RA-1 preserved:** removal is a lifecycle action triggered by presence + lease
  death, not a telemetry-tick write, and adds no status writes.
- **Destructive, accepted with guards:** deletion is irreversible, gated behind
  opt-in + the marker + the dwell + a lease-death proof. The controller logs the
  removal with its reason; emitting a Kubernetes audit Event (a `Recorder` on the
  RobotReconciler) is a documented follow-on.
- **Re-admission is the lifecycle:** the DiscoveredRobot was deleted at admission, so
  removing the Robot fully reclaims the identity. If the same device reconnects it
  re-discovers and re-admits as a fresh Robot — the intended flow for ephemeral
  devices.
- **API change:** new `SwarmadaConfig` fields and a new annotation constant require
  `make generate manifests`; the Robot controller gains `get` on `fleetactions`.
