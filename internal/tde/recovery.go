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
)

// RecoveryMode selects how the TDE rebuilds reservation state on start (§9.4.7).
type RecoveryMode string

// RecoveryMode values.
const (
	// RecoverValidate is the "Zone Controller ready" path: keep an Occupied entry
	// only if the robot is still physically in the zone (Robot.status.currentZone,
	// a durable etcd read — D-TDE-9), and release every Reserved entry (transit
	// state cannot be validated — D-TDE-8).
	RecoverValidate RecoveryMode = "Validate"
	// RecoverReleaseAll is the conservative fallback: drop all reservations and
	// reset queues. Safe and disruptive; use when Zone Controller health is
	// uncertain (D-TDE-10).
	RecoverReleaseAll RecoveryMode = "ReleaseAll"
	// RecoverReleaseReservedOnly drops Reserved entries but keeps Occupied ones
	// (Zone-Controller state known-good, only the TDE process restarted).
	RecoverReleaseReservedOnly RecoveryMode = "ReleaseReservedOnly"
)

// Recover rebuilds the in-process reservation state from FleetZone.status on TDE
// start (§9.4.7). It MUST run before the engine serves RequestReservation. The
// supplied client is used for the reads/writes (at startup this is a direct
// client, since the manager cache is not yet running). It is idempotent.
func (e *Engine) Recover(ctx context.Context, c client.Client, mode RecoveryMode) error {
	var zones fleetv1.FleetZoneList
	if err := c.List(ctx, &zones); err != nil {
		return fmt.Errorf("listing zones for TDE recovery: %w", err)
	}
	for i := range zones.Items {
		if err := e.recoverZone(ctx, c, &zones.Items[i], mode); err != nil {
			return err
		}
	}
	// State rebuilt: open the grant gate (the documented "engine now serves" point).
	e.recovered.Store(true)
	return nil
}

func (e *Engine) recoverZone(ctx context.Context, c client.Client, z *fleetv1.FleetZone, mode RecoveryMode) error {
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
	if mode == RecoverReleaseAll {
		zs.resources = map[string]*fleetv1.SharedResourceQueue{}
	} else {
		zs.resources = queuesFromStatus(z.Status.SharedResourceQueues)
	}

	// Mirror the recovered set back to status via the supplied client.
	original := z.DeepCopy()
	z.Status.Reservations = kept
	z.Status.SharedResourceQueues = queuesSlice(zs.resources)
	occ, _ := zs.counts(e.now())
	//nolint:gosec // occ is a small, non-negative count of Occupied reservations.
	z.Status.CurrentConcurrentRobots = int32(occ)
	if err := c.Status().Patch(ctx, z, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patching recovered zone %s/%s: %w", z.Namespace, z.Name, err)
	}
	return nil
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
			return false // §9.4.7: Reserved is always released
		}
		robot := &fleetv1.Robot{}
		if err := c.Get(ctx, types.NamespacedName{Name: r.RobotID, Namespace: z.Namespace}, robot); err != nil {
			return false // cannot validate presence → release
		}
		return robot.Status.CurrentZone == z.Name // keep only if physically present
	}
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
