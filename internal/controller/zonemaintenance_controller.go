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
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// zoneMaintenanceFinalizer keeps a ZoneMaintenance around on delete just long
// enough to restore every robot it paused (the "deleting it resumes all robots"
// contract, §9.1.11).
const zoneMaintenanceFinalizer = "swarmada.io/zonemaintenance-resume"

// zmReconcileInterval is the poll cadence while a maintenance is Active — it
// catches a Graceful wind-down robot reaching Idle and the auto-resume deadline.
const zmReconcileInterval = 30 * time.Second

// zmAncestorWalkCap bounds the parentZone ancestor walk (defence against a cyclic
// chain the FleetZone webhook would normally reject).
const zmAncestorWalkCap = 64

// defaultGracefulDrainTimeout is the fail-safe bound on Graceful-mode wind-down
// (ADR-0013): a robot still executing this long after activation is force-paused.
// The live value comes from
// SwarmadaConfig.spec.maintenance.defaultGracefulDrainTimeoutSeconds; this default
// matches that field's CRD default and applies only when no config is readable.
const defaultGracefulDrainTimeout = 300 * time.Second

// ZoneMaintenanceReconciler drives the ZoneMaintenance lifecycle (RFC-0001
// §9.1.11): Scheduled → Active → Completed. While Active it pauses Idle robots in
// scope into the Maintenance phase and lets InProgress robots wind down; on
// resume (auto-resume deadline or deletion) it restores them to Idle.
//
// It is the third and only other writer of Robot.status.phase besides the Robot
// controller (Offline/Discovered) and the FleetAction controller (InProgress). The
// three do not fight: the Robot controller never clobbers a Maintenance phase on a
// live heartbeat, the scheduler only assigns to Idle robots, and this controller
// only pauses an Idle+unassigned robot — so a just-assigned robot is handled as
// wind-down, self-correcting on the next reconcile. Writes are transition-driven,
// never per telemetry tick (RA-1).
type ZoneMaintenanceReconciler struct {
	client.Client

	// Audit records ESTOP_CLEAR_REJECTED (§9.6.5.1) when the resume gate refuses a
	// robot. Nil disables the record; the gate itself is unaffected.
	Audit    audit.Recorder
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// now overrides the clock in tests. Nil means time.Now.
	now func() time.Time
}

func (r *ZoneMaintenanceReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=swarmada.io,resources=zonemaintenances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=zonemaintenances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=zonemaintenances/finalizers,verbs=update
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch

// Reconcile advances one ZoneMaintenance through its lifecycle.
func (r *ZoneMaintenanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("zonemaintenance", req.NamespacedName)

	zm := &fleetv1.ZoneMaintenance{}
	if err := r.Get(ctx, req.NamespacedName, zm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// ── Deletion: resume every paused robot, then release the finalizer ──────────
	if !zm.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(zm, zoneMaintenanceFinalizer) {
			// Deletion resumes UNGATED (gated=false): the "deleting resumes all
			// robots" contract must always release the finalizer, so a not-yet-Clear
			// estop can never wedge deletion. The hardware estop keeps the robot
			// stopped regardless of its administrative phase.
			if _, err := r.resumeAll(ctx, zm, false); err != nil {
				return ctrl.Result{}, fmt.Errorf("resuming robots on delete: %w", err)
			}
			controllerutil.RemoveFinalizer(zm, zoneMaintenanceFinalizer)
			if err := r.Update(ctx, zm); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
			// Sealed AFTER the finalizer is released, deliberately. Sealing first would
			// duplicate the entry on every retry of a failing Update; sealing after means
			// the record follows a closure that actually completed. The object is gone at
			// this point, so this reconcile is the only one that can write it.
			r.recordMaintenanceDeactivated(ctx, zm, "delete")
			logger.Info("maintenance deleted; robots resumed")
		}
		return ctrl.Result{}, nil
	}

	// ── Ensure the resume finalizer is present before we pause anything ──────────
	if !controllerutil.ContainsFinalizer(zm, zoneMaintenanceFinalizer) {
		controllerutil.AddFinalizer(zm, zoneMaintenanceFinalizer)
		if err := r.Update(ctx, zm); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil // the update re-triggers reconciliation
	}

	// A completed/cancelled maintenance is terminal until the operator deletes it.
	if zm.Status.Phase == fleetv1.ZoneMaintenancePhaseCompleted ||
		zm.Status.Phase == fleetv1.ZoneMaintenancePhaseCancelled {
		return ctrl.Result{}, nil
	}

	now := r.clock()
	original := zm.DeepCopy()

	// ── Scheduled: not yet time to activate ─────────────────────────────────────
	if zm.Spec.ScheduledStart != nil && now.Before(zm.Spec.ScheduledStart.Time) {
		zm.Status.Phase = fleetv1.ZoneMaintenancePhaseScheduled
		if err := r.patchStatusIfChanged(ctx, original, zm); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: zm.Spec.ScheduledStart.Sub(now)}, nil
	}

	// ── Activate on first entry ─────────────────────────────────────────────────
	justActivated := zm.Status.Phase != fleetv1.ZoneMaintenancePhaseActive
	if justActivated {
		zm.Status.Phase = fleetv1.ZoneMaintenancePhaseActive
		zm.Status.ActivatedAt = &metav1.Time{Time: now}
		if zm.Spec.AutoResumeAfterMinutes > 0 {
			zm.Status.AutoResumeAt = &metav1.Time{
				Time: now.Add(time.Duration(zm.Spec.AutoResumeAfterMinutes) * time.Minute),
			}
		}
	}

	// ── Auto-resume deadline reached → restore and complete ─────────────────────
	// The gated resume respects requireEstopClearBeforeResume: a robot whose estop
	// is not Clear is held in Maintenance — an OPERATIONAL hold on the phase flip,
	// never a safety stop (the hardware estop path is separate and authoritative).
	// If any robot is held, the maintenance stays Active and requeues; it completes
	// on its own once the estops clear (or on delete, which resumes ungated).
	if zm.Status.AutoResumeAt != nil && !now.Before(zm.Status.AutoResumeAt.Time) {
		blocked, err := r.resumeAll(ctx, zm, true)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("auto-resume: %w", err)
		}
		if blocked > 0 {
			upsertCondition(&zm.Status.Conditions, zmCondResumeBlockedByEstop, metav1.ConditionTrue,
				"EstopNotClear", "auto-resume is holding: one or more robots have an estop that is not Clear")
			held, err := r.heldPaused(ctx, zm)
			if err != nil {
				return ctrl.Result{}, err
			}
			zm.Status.PausedRobots = held
			zm.Status.WindingDownRobots = nil
			zm.Status.ContinuousCapabilities = nil
			//nolint:gosec // small fleet counts
			zm.Status.PausedRobotsCount = int32(len(held))
			zm.Status.WindingDownRobotsCount = 0
			if err := r.patchStatusIfChanged(ctx, original, zm); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("auto-resume held by estop; staying Active", "heldRobots", len(held))
			return ctrl.Result{RequeueAfter: zmReconcileInterval}, nil
		}
		zm.Status.Phase = fleetv1.ZoneMaintenancePhaseCompleted
		// Stamped on the edge (RA-1). Write-once comes from the terminal guard at the top of
		// Reconcile, which returns before this branch once the phase is Completed — this is
		// the only path that reaches Completed, so the field is written exactly once. That
		// matters because the timestamp is what an outage is measured against; a later
		// reconcile moving it would silently shorten the window it records.
		zm.Status.CompletedAt = &metav1.Time{Time: now}
		zm.Status.PausedRobots = nil
		zm.Status.WindingDownRobots = nil
		zm.Status.ContinuousCapabilities = nil
		zm.Status.PausedRobotsCount = 0
		zm.Status.WindingDownRobotsCount = 0
		upsertCondition(&zm.Status.Conditions, zmCondResumeBlockedByEstop, metav1.ConditionFalse,
			"Resumed", "all robots resumed")
		if err := r.patchStatusIfChanged(ctx, original, zm); err != nil {
			return ctrl.Result{}, err
		}
		// After the patch, not before: the window is Completed only once that write lands,
		// and a seal ahead of it would record a closure that a failed patch then undid.
		r.recordMaintenanceDeactivated(ctx, zm, "auto-resume")
		logger.Info("maintenance auto-resumed")
		return ctrl.Result{}, nil
	}

	// ── Active: pause Idle robots, track wind-down of InProgress robots ──────────
	robots, err := r.robotsInScope(ctx, zm)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Seal the activation here rather than at the phase write above: robots_in_scope is
	// only known once the scope has been resolved, and an ACTIVATED entry without it
	// cannot answer which robots the window actually took out of service. `justActivated`
	// is the Scheduled→Active edge, so this fires once per window.
	if justActivated {
		r.recordMaintenanceActivated(ctx, zm, robots)
	}

	// Graceful wind-down is bounded (ADR-0013): once the maintenance has been Active
	// past the drain timeout, a still-executing robot is force-paused like Immediate.
	// This applies to every non-Immediate mode (Graceful, and the unset default that
	// behaves as Graceful); Immediate already force-requeues, so the timeout is moot.
	drainExpired := zm.Spec.Mode != fleetv1.ZoneMaintenanceModeImmediate &&
		zm.Status.ActivatedAt != nil &&
		now.After(zm.Status.ActivatedAt.Add(r.gracefulDrainTimeoutFor(ctx, zm.Namespace)))

	paused := existingPauseTimes(zm.Status.PausedRobots)
	var pausedRobots []fleetv1.PausedRobotEntry
	var windingDown []fleetv1.WindingDownRobot
	var continuous []fleetv1.ContinuousCapabilityEntry
	for i := range robots {
		robot := &robots[i]
		switch {
		case robot.Status.Phase == fleetv1.RobotPhaseMaintenance:
			// Already paused (by us). Preserve the original pausedAt.
			at := paused[robot.Name]
			if at.IsZero() {
				at = now
			}
			pausedRobots = append(pausedRobots, fleetv1.PausedRobotEntry{Name: robot.Name, PausedAt: metav1.Time{Time: at}})
			// Record the non-pauseable capabilities still running on this paused
			// robot (§9.1.11). Computed only for an already-Maintenance robot, whose
			// status.capabilities the Robot controller has already re-derived — a
			// just-paused robot is picked up on the next reconcile (Robot is watched).
			if caps := continuousCapabilities(robot); len(caps) > 0 {
				continuous = append(continuous, fleetv1.ContinuousCapabilityEntry{RobotName: robot.Name, Capabilities: caps})
			}
			// A non-pauseable capability reported Inactive during maintenance is a
			// CapabilityViolation (§6.10.4): it should keep running but is down.
			if r.Recorder != nil {
				for _, c := range robot.Status.Capabilities {
					if !c.Paused && c.Status == fleetv1.CapabilityStatusInactive {
						r.Recorder.Event(robot, corev1.EventTypeWarning, "CapabilityViolation",
							fmt.Sprintf("non-pauseable capability %q is Inactive during ZoneMaintenance %q", c.Name, zm.Name))
					}
				}
			}
		case robot.Status.Phase == fleetv1.RobotPhaseIdle && robot.Status.AssignedAction == "":
			// Idle and unassigned → pause now.
			if err := r.setRobotPhase(ctx, robot, fleetv1.RobotPhaseMaintenance); err != nil {
				return ctrl.Result{}, err
			}
			pausedRobots = append(pausedRobots, fleetv1.PausedRobotEntry{Name: robot.Name, PausedAt: metav1.Time{Time: now}})
		case robot.Status.Phase == fleetv1.RobotPhaseInProgress || robot.Status.AssignedAction != "":
			// Finishing a action. In Graceful mode it winds down and is paused once it
			// reports Idle — unless the drain timeout has elapsed (ADR-0013), at which
			// point it is force-paused like Immediate. In Immediate mode its action is
			// forcibly requeued: the FleetAction controller confirmed-stops the robot
			// (single-executor safe) and returns the action to Pending, freeing the robot
			// to be paused.
			forceRequeue := zm.Spec.Mode == fleetv1.ZoneMaintenanceModeImmediate || drainExpired
			if forceRequeue && robot.Status.AssignedAction != "" {
				if err := r.requeueAction(ctx, robot.Namespace, robot.Status.AssignedAction, zm.Spec.Reason, zm.Name); err != nil {
					return ctrl.Result{}, err
				}
			}
			windingDown = append(windingDown, fleetv1.WindingDownRobot{Name: robot.Name, AssignedAction: robot.Status.AssignedAction})
		}
		// Offline/Error/Charging/Discovered: not pausable now; a later reconcile
		// converges once the robot returns to Idle.
	}
	sort.Slice(pausedRobots, func(i, j int) bool { return pausedRobots[i].Name < pausedRobots[j].Name })
	sort.Slice(windingDown, func(i, j int) bool { return windingDown[i].Name < windingDown[j].Name })
	sort.Slice(continuous, func(i, j int) bool { return continuous[i].RobotName < continuous[j].RobotName })
	zm.Status.PausedRobots = pausedRobots
	zm.Status.WindingDownRobots = windingDown
	zm.Status.ContinuousCapabilities = continuous
	//nolint:gosec // small fleet counts
	zm.Status.PausedRobotsCount = int32(len(pausedRobots))
	//nolint:gosec // small fleet counts
	zm.Status.WindingDownRobotsCount = int32(len(windingDown))

	if err := r.patchStatusIfChanged(ctx, original, zm); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: zmReconcileInterval}, nil
}

// gracefulDrainTimeoutFor resolves the namespace's Graceful-mode drain bound from
// SwarmadaConfig.spec.maintenance.defaultGracefulDrainTimeoutSeconds (ADR-0013). It
// FAILS SAFE to defaultGracefulDrainTimeout on any problem (no config, list error,
// or a non-positive value), so an unreadable policy never shortens the wind-down
// window from its documented default.
func (r *ZoneMaintenanceReconciler) gracefulDrainTimeoutFor(ctx context.Context, namespace string) time.Duration {
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok {
		if s := cfg.Spec.Maintenance.DefaultGracefulDrainTimeoutSeconds; s > 0 {
			return time.Duration(s) * time.Second
		}
	}
	return defaultGracefulDrainTimeout
}

// zmCondResumeBlockedByEstop is set True while a gated (auto-resume) resume is
// holding one or more robots because their estop is not Clear (ADR-0023). It is an
// operational status signal on the administrative transition, not a safety state.
const zmCondResumeBlockedByEstop = "ResumeBlockedByEstop"

// resumeAll restores every robot this maintenance paused (currently in the
// Maintenance phase and in scope) back to Idle — UNLESS another Active
// ZoneMaintenance still covers it, so overlapping windows never un-pause early.
//
// When gated is true (the auto-resume path) it additionally honours the
// operational requireEstopClearBeforeResume gate: a robot whose estop is not Clear
// is left in Maintenance and counted as blocked. This only defers the
// administrative Maintenance→Idle phase flip; it never stops a robot and is
// entirely separate from the hardware estop path. Deletion-driven resume passes
// gated=false so the finalizer always releases. Returns the number of robots held
// by the gate.
func (r *ZoneMaintenanceReconciler) resumeAll(ctx context.Context, zm *fleetv1.ZoneMaintenance, gated bool) (int, error) {
	robots, err := r.robotsInScope(ctx, zm)
	if err != nil {
		return 0, err
	}
	requireClear := gated && r.requireEstopClearBeforeResume(ctx, zm)
	blocked := 0
	for i := range robots {
		robot := &robots[i]
		if robot.Status.Phase != fleetv1.RobotPhaseMaintenance {
			continue
		}
		covered, err := r.coveredByOtherActive(ctx, zm, robot)
		if err != nil {
			return blocked, err
		}
		if covered {
			continue
		}
		if requireClear {
			clear, err := r.estopClear(ctx, zm.Namespace, robot)
			if err != nil {
				return blocked, err
			}
			if !clear {
				blocked++ // operational hold on the phase flip; NOT a safety stop
				r.recordClearRejected(ctx, zm, robot)
				continue
			}
		}
		if err := r.setRobotPhase(ctx, robot, fleetv1.RobotPhaseIdle); err != nil {
			return blocked, err
		}
	}
	return blocked, nil
}

// requireEstopClearBeforeResume resolves the effective operational resume gate:
// spec.requireEstopClearBeforeResume wins; else the namespace default
// SwarmadaConfig.spec.maintenance.requireEstopClearBeforeResume; else true (the
// field's CRD default). It gates only the administrative resume transition and is
// never a safety mechanism.
func (r *ZoneMaintenanceReconciler) requireEstopClearBeforeResume(ctx context.Context, zm *fleetv1.ZoneMaintenance) bool {
	if zm.Spec.RequireEstopClearBeforeResume != nil {
		return *zm.Spec.RequireEstopClearBeforeResume
	}
	if cfg, ok := namespaceConfig(ctx, r.Client, zm.Namespace); ok && cfg.Spec.Maintenance.RequireEstopClearBeforeResume != nil {
		return *cfg.Spec.Maintenance.RequireEstopClearBeforeResume
	}
	return true
}

// estopClear reports whether a robot may be administratively resumed under the
// gate: its own estop is Normal (or unset) AND its zone's estop is Clear (or
// unset). It reads the OBSERVED estop projections only to decide a phase flip — it
// is not part of the hardware estop path (§9.6.2) and never stops or holds a robot.
// A missing zone is treated as nothing to gate on (Clear).
func (r *ZoneMaintenanceReconciler) estopClear(ctx context.Context, namespace string, robot *fleetv1.Robot) (bool, error) {
	if robot.Status.EstopState != "" && robot.Status.EstopState != fleetv1.RobotEstopNormal {
		return false, nil
	}
	zoneName := robot.Status.CurrentZone
	if zoneName == "" {
		zoneName = robot.Spec.Zone
	}
	if zoneName == "" {
		return true, nil
	}
	fz := &fleetv1.FleetZone{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: zoneName}, fz); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("resolving zone %q for estop check: %w", zoneName, err)
	}
	switch fz.Status.EstopStatus {
	case "", fleetv1.ZoneEstopClear:
		return true, nil
	default:
		return false, nil
	}
}

// heldPaused returns the paused-robot entries for the maintenance's still
// Maintenance-phase robots, preserving each robot's original pausedAt. Used when an
// estop-blocked auto-resume keeps the maintenance Active, so the status lists and
// counts reflect only the robots still held.
func (r *ZoneMaintenanceReconciler) heldPaused(ctx context.Context, zm *fleetv1.ZoneMaintenance) ([]fleetv1.PausedRobotEntry, error) {
	robots, err := r.robotsInScope(ctx, zm)
	if err != nil {
		return nil, err
	}
	paused := existingPauseTimes(zm.Status.PausedRobots)
	var out []fleetv1.PausedRobotEntry
	for i := range robots {
		robot := &robots[i]
		if robot.Status.Phase != fleetv1.RobotPhaseMaintenance {
			continue
		}
		at := paused[robot.Name]
		if at.IsZero() {
			at = r.clock()
		}
		out = append(out, fleetv1.PausedRobotEntry{Name: robot.Name, PausedAt: metav1.Time{Time: at}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// coveredByOtherActive reports whether any OTHER Active, non-deleting
// ZoneMaintenance in the namespace also scopes this robot.
func (r *ZoneMaintenanceReconciler) coveredByOtherActive(ctx context.Context, self *fleetv1.ZoneMaintenance, robot *fleetv1.Robot) (bool, error) {
	var list fleetv1.ZoneMaintenanceList
	if err := r.List(ctx, &list, client.InNamespace(self.Namespace)); err != nil {
		return false, fmt.Errorf("listing ZoneMaintenances: %w", err)
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == self.Name {
			continue
		}
		if !other.DeletionTimestamp.IsZero() || other.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
			continue
		}
		in, err := r.robotInScope(ctx, other, robot)
		if err != nil {
			return false, err
		}
		if in {
			return true, nil
		}
	}
	return false, nil
}

// robotsInScope lists the robots the maintenance targets.
func (r *ZoneMaintenanceReconciler) robotsInScope(ctx context.Context, zm *fleetv1.ZoneMaintenance) ([]fleetv1.Robot, error) {
	var all fleetv1.RobotList
	if err := r.List(ctx, &all, client.InNamespace(zm.Namespace)); err != nil {
		return nil, fmt.Errorf("listing robots: %w", err)
	}
	out := make([]fleetv1.Robot, 0, len(all.Items))
	for i := range all.Items {
		in, err := r.robotInScope(ctx, zm, &all.Items[i])
		if err != nil {
			return nil, err
		}
		if in {
			out = append(out, all.Items[i])
		}
	}
	return out, nil
}

// robotInScope reports whether a robot falls under a maintenance's scope. A
// namespace scope covers every robot in the namespace; a zone scope covers robots
// whose zone is the target or any descendant of it (bounded parentZone walk).
func (r *ZoneMaintenanceReconciler) robotInScope(ctx context.Context, zm *fleetv1.ZoneMaintenance, robot *fleetv1.Robot) (bool, error) {
	if zm.Spec.Scope.Type == fleetv1.MaintenanceScopeNamespace {
		return true, nil
	}
	target := zm.Spec.Scope.ZoneName
	if target == "" {
		return false, nil
	}
	robotZone := robot.Status.CurrentZone
	if robotZone == "" {
		robotZone = robot.Spec.Zone
	}
	if robotZone == "" {
		return false, nil
	}
	// Walk up from the robot's zone; a match against the target means the target is
	// that zone or one of its ancestors (so a parent-zone maintenance covers the
	// subtree).
	zone := robotZone
	for depth := 0; zone != "" && depth < zmAncestorWalkCap; depth++ {
		if zone == target {
			return true, nil
		}
		fz := &fleetv1.FleetZone{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: zm.Namespace, Name: zone}, fz); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // chain end / missing ancestor
			}
			return false, fmt.Errorf("resolving zone %q: %w", zone, err)
		}
		zone = fz.Spec.ParentZone
	}
	return false, nil
}

// setRobotPhase writes a material Robot.status.phase transition (RA-1: only on a
// real change, never a per-tick write).
func (r *ZoneMaintenanceReconciler) setRobotPhase(ctx context.Context, robot *fleetv1.Robot, phase fleetv1.RobotPhase) error {
	if robot.Status.Phase == phase {
		return nil
	}
	base := robot.DeepCopy()
	robot.Status.Phase = phase
	if err := r.Status().Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("setting robot %q phase to %s: %w", robot.Name, phase, err)
	}
	return nil
}

// patchStatusIfChanged persists the ZoneMaintenance status only when it changed.
func (r *ZoneMaintenanceReconciler) patchStatusIfChanged(ctx context.Context, original, zm *fleetv1.ZoneMaintenance) error {
	if equality.Semantic.DeepEqual(original.Status, zm.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, zm, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patching ZoneMaintenance status: %w", err)
	}
	return nil
}

// requeueAction marks a robot's in-progress action for a forcible requeue (Immediate
// mode). It sets the `swarmada.io/requeue-requested` annotation the FleetAction
// controller consumes — which confirmed-stops the robot and returns the action to
// Pending (single-executor safe). Idempotent: a no-op when the annotation (or the
// terminal-action cases) already apply; a missing/terminal action is skipped.
func (r *ZoneMaintenanceReconciler) requeueAction(ctx context.Context, namespace, actionName, reason, maintenanceName string) error {
	action := &fleetv1.FleetAction{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: actionName}, action); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // action already gone; nothing to requeue
		}
		return fmt.Errorf("fetching task to requeue: %w", err)
	}
	switch action.Status.Phase {
	case fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseFailed, fleetv1.ActionPhaseCancelled, fleetv1.ActionPhasePending:
		return nil // terminal or already unassigned — no requeue needed
	}
	if _, set := action.Annotations[annRequeueRequested]; set {
		return nil // already requested
	}
	base := action.DeepCopy()
	if action.Annotations == nil {
		action.Annotations = map[string]string{}
	}
	if reason == "" {
		reason = "zone maintenance"
	}
	action.Annotations[annRequeueRequested] = reason
	if err := r.Patch(ctx, action, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("requesting task requeue: %w", err)
	}
	// Sealed only when the annotation is newly set: every path above returns early when it
	// is already present or the action is terminal, so this is the transition edge and the
	// entry cannot repeat while the window stays Active.
	r.recordActionRequeued(ctx, namespace, actionName, maintenanceName)
	return nil
}

// recordActionRequeued seals ACTION_REQUEUED_BY_MAINTENANCE when a maintenance window
// interrupts in-flight work.
//
// The event is named for what happens, not for what §9.6.4 once said: the action is
// returned to Pending and is re-schedulable on another robot, which is a different fact
// from an estop Pause — that keeps the action bound to the robot it was taken from and
// waits for an operator. An entry claiming the action was "paused" would send a reviewer
// looking for a robot still holding it.
func (r *ZoneMaintenanceReconciler) recordActionRequeued(ctx context.Context, namespace, actionName, maintenanceName string) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventActionRequeuedByMaintenance,
		Namespace: namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "zonemaintenance-controller"},
		Resource:  audit.Resource{Kind: "FleetAction", Namespace: namespace, Name: actionName},
		Action:    "requeue",
		Outcome:   audit.OutcomeAllowed,
		Detail: map[string]string{
			"action_name":      actionName,
			"maintenance_name": maintenanceName,
		},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording ACTION_REQUEUED_BY_MAINTENANCE", "action", actionName)
	}
}

// continuousCapabilities returns the names of a paused robot's non-pauseable
// capabilities that are still running. On a Maintenance-phase robot the Robot
// controller has already paused the pauseable capabilities (Paused=true), so a
// capability with Paused=false that is not Inactive is a non-pauseable one still
// operating (Active or Degraded). Sorted for a stable status write (§9.1.11).
func continuousCapabilities(robot *fleetv1.Robot) []string {
	var out []string
	for _, c := range robot.Status.Capabilities {
		if !c.Paused && c.Status != fleetv1.CapabilityStatusInactive {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// existingPauseTimes indexes the recorded pausedAt per robot so a re-reconcile
// preserves the original pause time instead of resetting it each pass.
func existingPauseTimes(entries []fleetv1.PausedRobotEntry) map[string]time.Time {
	out := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		out[e.Name] = e.PausedAt.Time
	}
	return out
}

// SetupWithManager registers the ZoneMaintenance controller. It watches Robots so
// a wind-down robot reaching Idle promptly re-evaluates the maintenances in its
// namespace.
func (r *ZoneMaintenanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	robotToMaintenances := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		robot, ok := obj.(*fleetv1.Robot)
		if !ok {
			return nil
		}
		var list fleetv1.ZoneMaintenanceList
		if err := r.List(ctx, &list, client.InNamespace(robot.Namespace)); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
			}})
		}
		return reqs
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.ZoneMaintenance{}).
		Watches(&fleetv1.Robot{}, robotToMaintenances).
		Complete(r)
}

// sealMaintenanceEvent appends one §9.6.5.1 entry about a maintenance window. Best-effort
// and nil-safe: the window opens or closes either way, and an audit sink must never be able
// to hold robots in maintenance because it could not record that they were.
func (r *ZoneMaintenanceReconciler) sealMaintenanceEvent(ctx context.Context, zm *fleetv1.ZoneMaintenance, eventType, action string, detail map[string]string) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: eventType,
		Namespace: zm.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "zonemaintenance-controller"},
		Resource:  audit.Resource{Kind: "ZoneMaintenance", Namespace: zm.Namespace, Name: zm.Name},
		Action:    action,
		Outcome:   audit.OutcomeAllowed,
		Detail:    detail,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording audit entry", "event", eventType, "zonemaintenance", zm.Name)
	}
}

// recordMaintenanceActivated seals ZONE_MAINTENANCE_ACTIVATED on the Scheduled→Active edge.
//
// robots_in_scope is the field that makes the entry useful: it is the set the window
// actually resolved at activation, which is not derivable later — the zone tree, robot
// assignments and the window's own scope can all change before anyone reads the chain.
func (r *ZoneMaintenanceReconciler) recordMaintenanceActivated(ctx context.Context, zm *fleetv1.ZoneMaintenance, robots []fleetv1.Robot) {
	names := make([]string, 0, len(robots))
	for i := range robots {
		names = append(names, robots[i].Name)
	}
	sort.Strings(names)
	r.sealMaintenanceEvent(ctx, zm, audit.EventZoneMaintenanceActivated, "activate", map[string]string{
		"zone":            zm.Spec.Scope.ZoneName,
		"mode":            string(zm.Spec.Mode),
		"reason":          zm.Spec.Reason,
		"robots_in_scope": strings.Join(names, ","),
	})
}

// recordMaintenanceDeactivated seals ZONE_MAINTENANCE_DEACTIVATED when the window closes.
//
// closed_by distinguishes the two exits, and they are genuinely different facts: an
// auto-resume means the window ran its planned course, while a delete means an operator
// ended it — possibly early, and ungated, since deletion resumes robots even when an estop
// is not Clear. An entry that did not say which happened would leave a reviewer unable to
// tell a completed maintenance from an aborted one.
func (r *ZoneMaintenanceReconciler) recordMaintenanceDeactivated(ctx context.Context, zm *fleetv1.ZoneMaintenance, closedBy string) {
	detail := map[string]string{
		"zone":      zm.Spec.Scope.ZoneName,
		"closed_by": closedBy,
	}
	// Duration is measured from the activation stamp. A window deleted before it ever
	// activated has none; recording 0 would assert a zero-length maintenance rather than
	// "it never opened", so the field is omitted instead.
	if zm.Status.ActivatedAt != nil {
		detail["duration_seconds"] = strconv.Itoa(int(r.clock().Sub(zm.Status.ActivatedAt.Time).Seconds()))
	}
	r.sealMaintenanceEvent(ctx, zm, audit.EventZoneMaintenanceDeactivated, "deactivate", detail)
}

// request was refused rather than merely deferred.
//
// This is an OPERATIONAL hold, not a safety stop: the estop itself is untouched and
// nothing here clears or re-triggers it. Best-effort, like every other audit write.
// recordClearRejected writes ESTOP_CLEAR_REJECTED (§9.6.5.1): a resume was attempted
// but the requireEstopClearBeforeResume precondition was not met, so the robot stays
// paused. Recorded per blocked robot because that is the granularity an operator
// needs — "which robot held the window open" — and the outcome is Denied, since the
func (r *ZoneMaintenanceReconciler) recordClearRejected(ctx context.Context, zm *fleetv1.ZoneMaintenance, robot *fleetv1.Robot) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventEstopClearRejected,
		Namespace: zm.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "zonemaintenance-controller"},
		Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
		Action:    "resume",
		Outcome:   audit.OutcomeDenied,
		Detail: map[string]string{
			"reason":           "requireEstopClearBeforeResume: robot estop is not Normal",
			"zonemaintenance":  zm.Name,
			"robot_estopstate": string(robot.Status.EstopState),
		},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording ESTOP_CLEAR_REJECTED audit entry", "robot", robot.Name)
	}
}
