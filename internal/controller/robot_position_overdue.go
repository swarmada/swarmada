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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The ZonePositionOverdue condition (RFC-0001 #fleetzone-zone-controller-current-zone-derivation,
// "Robots that report no position").
//
// A robot whose spec.zone names a zone with a site-aligned polygon is placed only by a
// position frame. FZ-G5's declared membership does not apply to such a zone. Until a
// frame arrives the robot's currentZone is empty, and candidate filter 2 withholds every
// zone-scoped FleetAction from it. That exclusion is specified and is not changed here.
// This condition makes it visible: without it the only signal, ZonePositionUnmatched, is
// emitted on a frame, and a robot that sends none is excluded with nothing said.
//
// Every robot in such a zone is in this state between admission and its first position
// frame, so the condition does not go True at once. The two reasons separate the cases:
//
//   - AwaitingFirstPosition, status False. No position frame has been observed, for less
//     than positionOverdueThreshold. This is the ordinary state of a newly admitted robot.
//     No operator action is needed.
//   - NoPositionReported, status True. No position frame has been observed for at least
//     positionOverdueThreshold. THIS IS THE ONE TO ACT ON: the robot's adapter is not
//     streaming a position, and the robot stays out of zone-scoped work until it does.
//
// PositionReported, status False, is the cleared state: a position frame has been
// observed, and placement follows from it. A position that matches no polygon is the
// between-zones case, which ZonePositionUnmatched reports, not this condition.
//
// The condition is published only for a robot whose spec.zone names a zone that requires
// a position frame to place it: one whose geometry state is Containment, or one that does
// not exist. It is removed when that stops being true. A robot whose declared zone has no
// usable geometry is placed by declaration (FZ-G5) and never carries it.
//
// The condition reports. It does not withhold, admit or revoke anything: filter 2 reads
// status.currentZone alone.
const (
	conditionTypeZonePositionOverdue = "ZonePositionOverdue"

	reasonAwaitingFirstPosition = "AwaitingFirstPosition"
	reasonNoPositionReported    = "NoPositionReported"
	reasonPositionReported      = "PositionReported"

	// positionOverdueThreshold is how long a robot may await its first position frame
	// before the condition goes True. It is fixed. A per-namespace value would need a
	// SwarmadaConfig field, which is an API change, and no existing tunable measures
	// this: connectivityCriticalThresholdSeconds times an Offline robot, and a robot
	// awaiting a position may be Online. It is set to the default of that critical
	// threshold so the two escalations share a horizon.
	positionOverdueThreshold = defaultConnectivityCriticalTimeout
)

// PositionObservations reports whether a frame carrying a position has been observed for
// a robot since the process started. The ZoneController satisfies it.
type PositionObservations interface {
	PositionObserved(namespace, robotID string) bool
}

// overdueThreshold returns the threshold in force: the fixed value, unless a test in this
// package has set a shorter one.
func (r *RobotReconciler) overdueThreshold() time.Duration {
	if r.positionOverdueThreshold > 0 {
		return r.positionOverdueThreshold
	}
	return positionOverdueThreshold
}

// reconcileZonePositionOverdue sets, advances, clears or removes the ZonePositionOverdue
// condition on robot in memory. It returns how long until an AwaitingFirstPosition
// condition becomes due, or 0 when nothing is pending, so the caller can requeue for it.
//
// # RA-1
//
// It runs only inside the Robot reconcile, and it only edits the in-memory robot. The
// write is the reconcile's single status patch, which is issued only when status differs
// from what was read. The condition is edited only when its status, reason or message
// would change, and upsertCondition keeps lastTransitionTime unless the status flips, so
// a reconcile that finds the same state writes nothing. The position frame path does not
// call this: ObservePosition records the frame in memory (markPositioned) and writes
// status only on a currentZone or containment transition, as before. The clear is
// therefore written by the Robot reconcile that the currentZone transition enqueues, or
// by the periodic requeue when the frame moved nothing.
func (r *RobotReconciler) reconcileZonePositionOverdue(ctx context.Context, robot *fleetv1.Robot) (time.Duration, error) {
	existing := findCondition(robot, conditionTypeZonePositionOverdue)

	declared := robot.Spec.Zone
	if declared == "" {
		removeCondition(robot, conditionTypeZonePositionOverdue)
		return 0, nil
	}
	fz := &fleetv1.FleetZone{}
	zoneMissing := false
	if err := r.Get(ctx, client.ObjectKey{Namespace: robot.Namespace, Name: declared}, fz); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("reading declared zone %q: %w", declared, err)
		}
		zoneMissing = true
	}
	if !zoneMissing {
		if state, _ := geometryStateOf(&fz.Spec); state != fleetv1.ZoneGeometryContainment {
			// FZ-G5: placed by declaration. The condition does not apply.
			removeCondition(robot, conditionTypeZonePositionOverdue)
			return 0, nil
		}
	}

	if r.positionReported(robot, existing) {
		if existing != nil && existing.Reason != reasonPositionReported {
			upsertCondition(&robot.Status.Conditions, conditionTypeZonePositionOverdue, metav1.ConditionFalse,
				reasonPositionReported, "a position frame has been observed for this robot")
		}
		return 0, nil
	}

	threshold := r.overdueThreshold()
	switch {
	case existing == nil:
		// Entering the state. lastTransitionTime is set now, and it is the clock the
		// threshold is measured from.
		setOverdueCondition(robot, metav1.ConditionFalse, reasonAwaitingFirstPosition,
			awaitingMessage(declared, zoneMissing))
		return threshold, nil
	case existing.Status == metav1.ConditionTrue:
		setOverdueCondition(robot, metav1.ConditionTrue, reasonNoPositionReported,
			overdueMessage(declared, zoneMissing, robot.Status.CurrentZone, threshold))
		return 0, nil
	default:
		due := existing.LastTransitionTime.Add(threshold)
		if remaining := time.Until(due); remaining > 0 {
			setOverdueCondition(robot, metav1.ConditionFalse, reasonAwaitingFirstPosition,
				awaitingMessage(declared, zoneMissing))
			return remaining, nil
		}
		setOverdueCondition(robot, metav1.ConditionTrue, reasonNoPositionReported,
			overdueMessage(declared, zoneMissing, robot.Status.CurrentZone, threshold))
		return 0, nil
	}
}

// positionReported reports whether there is evidence that the robot has sent a position.
// Three sources, any one sufficient: a non-empty status.containingZones, which only a
// position frame writes; the Zone Controller's in-memory record, which also covers a
// position that matched no polygon; and a condition already cleared, which carries the
// evidence across a restart that empties the in-memory record.
func (r *RobotReconciler) positionReported(robot *fleetv1.Robot, existing *metav1.Condition) bool {
	if len(robot.Status.ContainingZones) > 0 {
		return true
	}
	if existing != nil && existing.Reason == reasonPositionReported {
		return true
	}
	id := robot.Annotations[RobotIDAnnotation]
	return r.Positions != nil && id != "" && r.Positions.PositionObserved(robot.Namespace, id)
}

// setOverdueCondition edits the condition only when status, reason or message differ, so
// an unchanged state leaves the object byte-identical and the reconcile writes nothing.
func setOverdueCondition(robot *fleetv1.Robot, status metav1.ConditionStatus, reason, message string) {
	if c := findCondition(robot, conditionTypeZonePositionOverdue); c != nil &&
		c.Status == status && c.Reason == reason && c.Message == message {
		return
	}
	upsertCondition(&robot.Status.Conditions, conditionTypeZonePositionOverdue, status, reason, message)
}

// removeCondition drops condType from the robot's conditions, if present.
func removeCondition(robot *fleetv1.Robot, condType string) {
	out := robot.Status.Conditions[:0]
	for _, c := range robot.Status.Conditions {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	robot.Status.Conditions = out
}

func awaitingMessage(declared string, zoneMissing bool) string {
	if zoneMissing {
		return fmt.Sprintf("no position frame observed yet; spec.zone %q names no FleetZone, so only a position frame can place this robot", declared)
	}
	return fmt.Sprintf("no position frame observed yet; spec.zone %q has a site-aligned polygon, so only a position frame can place this robot", declared)
}

func overdueMessage(declared string, zoneMissing bool, currentZone string, threshold time.Duration) string {
	why := fmt.Sprintf("spec.zone %q has a site-aligned polygon", declared)
	if zoneMissing {
		why = fmt.Sprintf("spec.zone %q names no FleetZone", declared)
	}
	if currentZone == "" {
		return fmt.Sprintf("no position frame observed for %s; %s, so status.currentZone is empty and zone-scoped actions are withheld from this robot",
			threshold, why)
	}
	return fmt.Sprintf("no position frame observed for %s; %s, and status.currentZone still reads %q, which no position frame has confirmed",
		threshold, why, currentZone)
}
