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

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/safety"
)

// The estop hold record (Robot.status.estopHeldBy).
//
// A zone's or the namespace's stop holds a robot from the moment it is issued to it until
// that stop's own clear (RFC-0001 #safety-estop-scopes). The zone- and namespace-estop
// controllers write that fact on the robot when they issue the stop, and only the clear of
// the same carrier removes it. A move out of the zone, a change of the zone tree or of
// estopPolicy, a new position report or a manager restart leaves it in place, so nothing
// but that clear ends the hold. estopscope.Holders and estopscope.HeldOnRecord read it.
//
// Each write is a status patch of the record alone, read from the API server and
// optimistic-locked, retried on a conflict: the record is a list, a JSON merge patch
// replaces a list whole, and two carriers' controllers write the same robot's list.

// holdRef names the carrier whose stop is recorded on a robot.
type holdRef struct {
	kind fleetv1.EstopHoldKind
	name string
	uid  string
}

func zoneHold(z *fleetv1.FleetZone) holdRef {
	return holdRef{kind: fleetv1.EstopHoldFleetZone, name: z.Name, uid: string(z.UID)}
}

func configHold(c *fleetv1.SwarmadaConfig) holdRef {
	return holdRef{kind: fleetv1.EstopHoldSwarmadaConfig, name: c.Name, uid: string(c.UID)}
}

// names reports whether entry is a record of this carrier, by kind and name. A carrier
// recreated under the same name names the same entries: its clear removes them.
func (h holdRef) names(e fleetv1.EstopHold) bool {
	kind := e.Kind
	if kind == "" {
		kind = fleetv1.EstopHoldFleetZone
	}
	return kind == h.kind && e.Name == h.name
}

// hasHold reports whether robot records ref's stop.
func hasHold(robot *fleetv1.Robot, ref holdRef) bool {
	for _, e := range robot.Status.EstopHeldBy {
		if ref.names(e) {
			return true
		}
	}
	return false
}

// inStopState reports whether a stop state is written on robot: Stopping, Stopped or Failed.
func inStopState(robot *fleetv1.Robot) bool {
	switch robot.Status.EstopState {
	case fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed:
		return true
	}
	return false
}

// recordHold writes ref's stop into the robot's status.estopHeldBy. It is a no-op when the
// robot already records it.
func recordHold(ctx context.Context, c client.Client, reader client.Reader, namespace, robot string, ref holdRef) error {
	return patchHolds(ctx, c, reader, namespace, robot, func(in []fleetv1.EstopHold) ([]fleetv1.EstopHold, bool) {
		for _, e := range in {
			if ref.names(e) {
				return in, false
			}
		}
		return append(in, fleetv1.EstopHold{Kind: ref.kind, Name: ref.name, UID: ref.uid,
			Since: metav1.NewTime(time.Now().UTC().Truncate(time.Second))}), true
	})
}

// dropHolds removes from the robot's status.estopHeldBy every entry drop selects.
func dropHolds(ctx context.Context, c client.Client, reader client.Reader, namespace, robot string,
	drop func(fleetv1.EstopHold) bool) error {
	return patchHolds(ctx, c, reader, namespace, robot, func(in []fleetv1.EstopHold) ([]fleetv1.EstopHold, bool) {
		var out []fleetv1.EstopHold
		for _, e := range in {
			if !drop(e) {
				out = append(out, e)
			}
		}
		return out, len(out) != len(in)
	})
}

// patchHolds applies edit to the robot's record, read from the API server, and writes it
// under an optimistic lock, retried on a conflict, with a backoff long enough to outlast the
// other status writes a stop makes (its record, its outcome). A robot that no longer exists has
// no record to keep.
func patchHolds(ctx context.Context, c client.Client, reader client.Reader, namespace, robot string,
	edit func([]fleetv1.EstopHold) ([]fleetv1.EstopHold, bool)) error {
	if reader == nil {
		reader = c
	}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cur := &fleetv1.Robot{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robot}, cur); err != nil {
			return err
		}
		next, changed := edit(append([]fleetv1.EstopHold(nil), cur.Status.EstopHeldBy...))
		if !changed {
			return nil
		}
		base := cur.DeepCopy()
		cur.Status.EstopHeldBy = next
		return c.Status().Patch(ctx, cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("writing the estop hold record of robot %s: %w", robot, err)
	}
	return nil
}

// outcomeUnwritten reports whether a robot's stop must be issued again: its outcome could not
// be written to its estopState, or its hold record could not be written. Neither is recorded
// as done, so the carrier's marker waits and the reconcile is retried.
func outcomeUnwritten(o estopOutcome) bool {
	return errors.Is(o.err, safety.ErrStateUnwritten) || o.recErr != nil
}

// inForceTargets splits the robots a carrier's stop in force must still reach. A robot it
// has reached carries the stop in status.stops, uncleared, and a stop state; it needs at most
// its estopHeldBy record. Every other needs the stop, sent to it as its own stop under its own
// stop ID (RFC-0001 #safety-robot-stop-table, row S1): one that came into the stop's scope
// since (created there, moved there, or put under the zone by a change of the tree); one that
// records this stop but reads Normal or Resuming, because its stop state was never written;
// and one already in a stop state (another stop's) that this carrier's stop never reached.
// The key is the stop record, not the stop state, so a pass that runs again re-sends nothing
// to a robot whose record has landed. A stop this process has sent and not yet recorded counts
// as its record (sent, by robot name): it is in the control plane's own state until the record
// lands (RFC-0001 #safety-estop-scopes), so a pass that runs inside that window re-sends nothing
// either. scoped is the robots the tree puts in the stop's scope now; all is every robot in the
// namespace, whose records are read so a robot that has since left the scope is not lost.
func inForceTargets(ref holdRef, scoped, all []fleetv1.Robot, sent map[string]bool) (stop, recordOnly []fleetv1.Robot) {
	in := make(map[string]bool, len(scoped))
	for i := range scoped {
		in[scoped[i].Name] = true
	}
	for i := range all {
		r := all[i]
		// A robot being deleted is a robot: until it is gone it may still be moving, and it
		// holds its last finalizer until it is stopped (the stop guard, estopscope).
		has := hasHold(&r, ref)
		if !has && !in[r.Name] {
			continue
		}
		switch {
		case (recordsStop(&r, ref) || sent[r.Name]) && inStopState(&r):
			if !has {
				recordOnly = append(recordOnly, r)
			}
		default:
			stop = append(stop, r)
		}
	}
	return stop, recordOnly
}

// sentStops is an Estopper that keeps the stops it has sent and not yet recorded
// (safety.Dispatcher, the Estopper the manager wires).
type sentStops interface {
	SentUnrecorded(namespace string, carrier safety.Carrier) map[string]bool
}

var _ sentStops = (*safety.Dispatcher)(nil)

// sentUnrecorded is the robots est has sent ref's stop and not yet recorded, by name; nil when est
// keeps no such state. A pass takes it before it reads the robots: a stop whose record lands after
// it is then in what the pass reads.
func sentUnrecorded(est ZoneEstopper, namespace string, ref holdRef) map[string]bool {
	if s, ok := est.(sentStops); ok {
		return s.SentUnrecorded(namespace, carrierOf(ref))
	}
	return nil
}

// recordsStop reports whether robot's status.stops holds an uncleared stop of ref's carrier.
func recordsStop(robot *fleetv1.Robot, ref holdRef) bool {
	for _, st := range robot.Status.Stops {
		if st.ClearedAt == nil && st.Carrier != nil && string(st.Carrier.Kind) == string(ref.kind) &&
			st.Carrier.Name == ref.name && (ref.uid == "" || st.Carrier.UID == "" || st.Carrier.UID == ref.uid) {
			return true
		}
	}
	return false
}

// recordHoldAndWithdraw records ref's stop on robot and withdraws any pending-firmware
// instruction left on it: an over-the-air pull to a robot a stop covers is dispatch, and a
// hand-written instruction must not stand on a stopped robot for its agent to act on at its
// next idle transition. It is the record callback every fan-out and late-arrival pass uses.
func recordHoldAndWithdraw(ctx context.Context, c client.Client, reader client.Reader, namespace, robot string, ref holdRef) error {
	if err := recordHold(ctx, c, reader, namespace, robot, ref); err != nil {
		return err
	}
	return withdrawPendingFirmwareOnStop(ctx, c, reader, namespace, robot)
}

// withdrawPendingFirmwareOnStop removes the swarmada.io/pending-firmware-* keys from a robot a
// stop reaches, whoever wrote them. A failed write is returned so the pass is retried; a robot
// that is gone is a no-op.
func withdrawPendingFirmwareOnStop(ctx context.Context, c client.Client, reader client.Reader, namespace, robot string) error {
	// Read again and retried on a conflict: the stop's own record (status.stops) lands on the
	// robot moments after the stop is sent, and changes its resourceVersion.
	withdrawn := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		rb := &fleetv1.Robot{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: robot}, rb); err != nil {
			return err
		}
		if !carriesPendingFirmware(rb.Annotations) {
			return nil
		}
		base := rb.DeepCopy()
		delete(rb.Annotations, annPendingFirmwareVersion)
		delete(rb.Annotations, annPendingFirmwareURI)
		delete(rb.Annotations, annPendingFirmwareChecksum)
		withdrawn = true
		return c.Patch(ctx, rb, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
	if apierrors.IsNotFound(err) || err == nil && !withdrawn {
		return nil
	}
	if err != nil {
		return fmt.Errorf("withdrawing the pending firmware instruction from robot %s under a stop: %w", robot, err)
	}
	log.FromContext(ctx).Info("pending firmware instruction withdrawn from a robot a stop covers", "robot", robot)
	return nil
}

// carriesPendingFirmware reports whether any swarmada.io/pending-firmware-* key is present.
func carriesPendingFirmware(annotations map[string]string) bool {
	for _, k := range []string{annPendingFirmwareVersion, annPendingFirmwareURI, annPendingFirmwareChecksum} {
		if _, ok := annotations[k]; ok {
			return true
		}
	}
	return false
}

// clearTargets is every robot a carrier's clear must reach: those the tree puts in its scope
// now, and every robot that records its stop, wherever it is now.
func clearTargets(ref holdRef, scoped, all []fleetv1.Robot) []fleetv1.Robot {
	in := make(map[string]bool, len(scoped))
	out := append([]fleetv1.Robot(nil), scoped...)
	for i := range scoped {
		in[scoped[i].Name] = true
	}
	for i := range all {
		if !in[all[i].Name] && hasHold(&all[i], ref) {
			out = append(out, all[i])
		}
	}
	return out
}

// withoutHold is a copy of robot that no longer records ref's stop: what the clear of ref
// asks estopscope about, so the stop being cleared is never the answer.
func withoutHold(robot *fleetv1.Robot, ref holdRef) *fleetv1.Robot {
	out := robot.DeepCopy()
	out.Status.EstopHeldBy = nil
	for _, e := range robot.Status.EstopHeldBy {
		if !ref.names(e) {
			out.Status.EstopHeldBy = append(out.Status.EstopHeldBy, e)
		}
	}
	return out
}

// clearEnds is what a clear ends on a robot (safety.Ends): the estopHeldBy records drop selects,
// and each zone or namespace stop recorded in status.stops that has ended. A recorded stop has ended
// when its carrier is cleared, the carrier this clear is for, or exists under the same UID and
// carries neither the trigger nor the processed marker (estopscope.InForce). A carrier that no
// longer exists, or exists under another UID, ends only on the robot's own clear (cleared nil),
// which releases a record whose carrier is gone, as it drops that carrier's estopHeldBy record.
func clearEnds(cleared *holdRef, drop func(fleetv1.EstopHold) bool, zones []fleetv1.FleetZone,
	configs []fleetv1.SwarmadaConfig) safety.Ends {
	return safety.Ends{Hold: drop, Stop: func(st fleetv1.RobotStop) bool {
		c := st.Carrier
		if c == nil {
			return false
		}
		if cleared != nil && string(cleared.kind) == string(c.Kind) && cleared.name == c.Name {
			return true
		}
		var meta *metav1.ObjectMeta
		switch c.Kind {
		case fleetv1.StopCarrierFleetZone:
			for i := range zones {
				if zones[i].Name == c.Name {
					meta = &zones[i].ObjectMeta
				}
			}
		case fleetv1.StopCarrierSwarmadaConfig:
			for i := range configs {
				if configs[i].Name == c.Name {
					meta = &configs[i].ObjectMeta
				}
			}
		default:
			return false
		}
		if meta == nil || (c.UID != "" && string(meta.UID) != c.UID) {
			return cleared == nil
		}
		return !estopscope.InForce(meta.Annotations)
	}}
}

// clearCarrier applies the clear of ref's stop to robots. A robot another stop still holds
// keeps its state and loses only ref's record. Every other robot is moved to Resuming
// (Estopper.ClearEstop), and then loses ref's record, so a crash between the two is retried
// and the resume is refused until the record is gone. Every robot is tried; any failed write
// is returned, and the caller keeps its marker so the clear is retried.
func clearCarrier(ctx context.Context, c client.Client, reader client.Reader, est ZoneEstopper,
	rec record.EventRecorder, namespace string, ref holdRef, robots []fleetv1.Robot,
	zones []fleetv1.FleetZone, configs []fleetv1.SwarmadaConfig, clearedBy, clearedScope string,
) (int, heldRobots, error) {
	cleared := 0
	var held heldRobots
	var errs []error
	for i := range robots {
		r := &robots[i]
		if by, ok := estopscope.StandingStop(withoutHold(r, ref), zones, configs); ok {
			held.add(r.Name, by)
			heldByStandingStop(rec, r, clearedScope, by)
		} else {
			if _, err := est.ClearEstop(ctx, namespace, r.Name, clearedBy, clearEnds(&ref, ref.names, zones, configs)); err != nil {
				errs = append(errs, fmt.Errorf("clearing robot %s: %w", r.Name, err))
				continue
			}
			cleared++
		}
		if hasHold(r, ref) {
			if err := dropHolds(ctx, c, reader, namespace, r.Name, ref.names); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return cleared, held, errors.Join(errs...)
}
