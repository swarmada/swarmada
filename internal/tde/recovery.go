/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tde

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/robotid"
)

// RecoveryMode selects how the TDE rebuilds reservation state on start (RFC-0001 #tde-restart-recovery).
type RecoveryMode string

// RecoveryMode values.
const (
	// RecoverValidate is the "Zone Controller ready" path: keep an Occupied entry
	// only if the robot is still physically in the zone (Robot.status.currentZone,
	// a durable etcd read — D-TDE-9), and release every Reserved entry (transit
	// state cannot be validated — D-TDE-8).
	RecoverValidate RecoveryMode = "Validate"
	// RecoverReleaseAll is the conservative fallback: drop all reservations and
	// every action's queue entries. Safe and disruptive; use when Zone Controller
	// health is uncertain (D-TDE-10). Adapter-initiated holds survive it as they
	// survive every mode (keepAdapterEntry).
	RecoverReleaseAll RecoveryMode = "ReleaseAll"
	// RecoverReleaseReservedOnly drops Reserved entries but keeps Occupied ones
	// (Zone-Controller state known-good, only the TDE process restarted).
	RecoverReleaseReservedOnly RecoveryMode = "ReleaseReservedOnly"
)

// Recover rebuilds the in-process reservation state from FleetZone.status on TDE
// start (RFC-0001 #tde-restart-recovery). It MUST run before the engine serves RequestReservation. The
// supplied client is used for the reads/writes (at startup this is a direct
// client, since the manager cache is not yet running). It is idempotent.
func (e *Engine) Recover(ctx context.Context, c client.Client, mode RecoveryMode) error {
	var zones fleetv1.FleetZoneList
	if err := c.List(ctx, &zones); err != nil {
		return fmt.Errorf("listing zones for TDE recovery: %w", err)
	}
	var promoted []promotion
	for i := range zones.Items {
		ps, err := e.recoverZone(ctx, c, &zones.Items[i], mode)
		if err != nil {
			return err
		}
		promoted = append(promoted, ps...)
	}
	// State rebuilt: open the grant gate (the documented "engine now serves" point).
	e.recovered.Store(true)
	// A waiter recovery promoted is told, as a release promoting it would tell it. An
	// adapter that has not reconnected yet misses the push; its next reserve reports Granted.
	e.tell(ctx, promoted)
	return nil
}

func (e *Engine) recoverZone(ctx context.Context, c client.Client, z *fleetv1.FleetZone, mode RecoveryMode) ([]promotion, error) {
	states, unlock := e.lockZones(z.Namespace, z.Name)
	defer unlock()
	zs := states[zoneKey(z.Namespace, z.Name)]

	var kept []fleetv1.ZoneReservation
	for _, r := range z.Status.Reservations {
		if e.keepOnRecovery(ctx, c, z, r, mode) {
			kept = append(kept, r)
		}
	}
	zs.reservations = kept
	zs.resources = queuesFromStatus(z.Status.SharedResourceQueues)

	// Releasing a reservation has to release the holds that went with it. The queues
	// are rebuilt verbatim from status while reservations are dropped, so an action
	// recovery had just released would otherwise stay the recorded holder of a corridor
	// or lift with nothing left to release it -- a shared resource leaked permanently
	// across a restart, and its waiters never promoted (D-TDE-7).
	//
	// An adapter-initiated entry is not a reservation's hold and is not released with
	// one. Its robot was told Granted or Queued and is told nothing by a restart, so
	// dropping it would free a lift or door the robot still believes it holds and grant
	// it to the next asker (TDE-5). It is kept while its Robot still exists.
	keptIDs := make(map[string]bool, len(kept))
	for i := range kept {
		keptIDs[kept[i].ActionID] = true
	}
	var promoted []promotion
	for _, q := range zs.resources {
		for _, id := range queueActionIDs(q) {
			if keptIDs[id] {
				continue
			}
			keep, err := keepAdapterEntry(ctx, c, z.Namespace, q, id)
			if err != nil {
				return nil, fmt.Errorf("recovering %s in zone %s/%s: %w", q.ResourceName, z.Namespace, z.Name, err)
			}
			if keep {
				continue
			}
			capacity := e.resourceCapacity(ctx, z.Namespace, z.Name, q.ResourceName)
			before := adapterHolders(q)
			releaseHolder(q, id, e.now(), capacity)
			promoted = append(promoted, promotedSince(z.Namespace, q, before)...)
		}
	}

	// Mirror the recovered set back to status via the supplied client.
	original := z.DeepCopy()
	z.Status.Reservations = kept
	z.Status.SharedResourceQueues = queuesSlice(zs.resources)
	occ, _ := zs.counts(e.now())
	//nolint:gosec // occ is a small, non-negative count of Occupied reservations.
	z.Status.CurrentConcurrentRobots = int32(occ)
	if err := c.Status().Patch(ctx, z, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("patching recovered zone %s/%s: %w", z.Namespace, z.Name, err)
	}
	return promoted, nil
}

// keepOnRecovery decides whether a persisted reservation survives recovery.
func (e *Engine) keepOnRecovery(
	ctx context.Context, c client.Client, z *fleetv1.FleetZone, r fleetv1.ZoneReservation, mode RecoveryMode,
) bool {
	switch mode {
	case RecoverReleaseAll:
		return false
	case RecoverReleaseReservedOnly:
		return r.State == fleetv1.ReservationOccupied
	default: // RecoverValidate
		if r.State != fleetv1.ReservationOccupied {
			return false // RFC-0001 #tde-restart-recovery: Reserved is always released
		}
		robot := &fleetv1.Robot{}
		if err := c.Get(ctx, types.NamespacedName{Name: r.RobotID, Namespace: z.Namespace}, robot); err != nil {
			return false // cannot validate presence → release
		}
		return robot.Status.CurrentZone == z.Name // keep only if physically present
	}
}

// keepAdapterEntry reports whether the queue entry keyed id is an adapter-initiated
// hold or wait (ReserveResource keys those by the robot: ActionID == RobotID) whose
// Robot still exists in namespace. The Robot object is the durable record that the
// robot the grant was made to is still in the fleet; one that has been deleted cannot
// hold anything. A lookup that fails is returned rather than read as absence, so
// recovery retries with the grant gate closed instead of freeing a held resource.
func keepAdapterEntry(
	ctx context.Context, c client.Reader, namespace string, q *fleetv1.SharedResourceQueue, id string,
) (bool, error) {
	robotID, ok := adapterEntryRobot(q, id)
	if !ok {
		return false, nil
	}
	robot, err := robotid.Lookup(ctx, c, namespace, robotID)
	if err != nil {
		return false, err
	}
	return robot != nil, nil
}

// adapterEntryRobot returns the robot of the holder or waiter keyed id, and whether
// that entry is adapter-initiated rather than an action's.
func adapterEntryRobot(q *fleetv1.SharedResourceQueue, id string) (string, bool) {
	for i := range q.CurrentHolders {
		if h := q.CurrentHolders[i]; h.ActionID == id {
			return h.RobotID, h.RobotID == id
		}
	}
	for i := range q.WaitQueue {
		if w := q.WaitQueue[i]; w.ActionID == id {
			return w.RobotID, w.RobotID == id
		}
	}
	return "", false
}

// queuesFromStatus rebuilds the in-process shared-resource queue map from status.
func queuesFromStatus(qs []fleetv1.SharedResourceQueue) map[string]*fleetv1.SharedResourceQueue {
	out := make(map[string]*fleetv1.SharedResourceQueue, len(qs))
	for i := range qs {
		q := qs[i]
		out[q.ResourceName] = &q
	}
	return out
}
