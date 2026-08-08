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
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/metrics"
)

const (
	// defaultHeartbeatTimeout is the fallback for how long without a Fleet Adapter
	// heartbeat before the robot is considered Offline. The live value is resolved
	// per-namespace from SwarmadaConfig (spec.health.connectivityOfflineThresholdSeconds);
	// this default (matching that field's CRD default) applies only when no config
	// is readable or it carries no positive threshold.
	defaultHeartbeatTimeout = 30 * time.Second

	// defaultConnectivityCriticalTimeout is the fallback for how long a robot must
	// remain Offline before it escalates to the ConnectivityCritical condition
	// (ADR-0011). The live value comes from
	// spec.health.connectivityCriticalThresholdSeconds; this default matches that
	// field's CRD default and applies only when no config is readable.
	defaultConnectivityCriticalTimeout = 120 * time.Second

	// defaultAutoRemoveOfflineGrace is the dwell after a robot goes Offline before opt-in
	// auto-removal (ADR-0030) may reclaim it, applied when the namespace enables removal but
	// leaves the grace unset. Matches the CRD default of autoRemoveOfflineGraceSeconds.
	defaultAutoRemoveOfflineGrace = 5 * time.Minute

	// conditionTypeReady is the standard Ready condition type.
	conditionTypeReady = "Ready"

	// conditionTypeConnectivityCritical marks a robot that has been Offline beyond
	// the critical threshold (ADR-0011). It is a severity signal, not a phase — the
	// robot stays in the Offline phase.
	conditionTypeConnectivityCritical = "ConnectivityCritical"

	// heartbeatConfirmAttempts / heartbeatConfirmInterval are the confirming exchange
	// from §9.6.3.2: three HeartbeatRequests, five seconds apart, before a robot whose
	// telemetry has gone quiet is declared Offline.
	heartbeatConfirmAttempts = 3
	heartbeatConfirmInterval = 5 * time.Second
)

// RBAC for the Robot controller. Declared as a standalone, markers-only comment
// group (blank lines both sides): controller-gen silently drops +kubebuilder:rbac
// markers that share a comment group with prose, so these MUST NOT be folded into
// the type's doc comment below (observed with controller-gen v0.16.5 — the
// robots/finalizers marker and the robots `delete` verb vanished while they lived
// inside the RobotReconciler doc comment).

// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=swarmada.io,resources=robots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots/finalizers,verbs=update
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions,verbs=get

// RobotReconciler reconciles Robot objects.
type RobotReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Audit records ROBOT_ADMITTED (§9.6.5.1) the first time a Robot is reconciled.
	// Nil disables the record; reconciliation is unaffected.
	Audit audit.Recorder

	// Liveness runs the confirming heartbeat exchange before a robot is declared
	// Offline (§9.6.3.2). Nil disables confirmation and the transition falls back to
	// elapsed time alone — the pre-confirmation behaviour, which is the correct
	// fallback: a control plane with no push path must still detect a dead robot.
	Liveness LivenessProber

	// beats counts consecutive unanswered confirmations per robot. Deliberately
	// in-memory: it is scratch state for one Offline decision, and persisting it would
	// add three status writes per event to etcd for a value nobody reads. Leader
	// election means one reconciler owns it; losing it on restart costs at most one
	// extra confirmation round, and the robot's telemetry age is unchanged either way.
	beatsMu sync.Mutex
	beats   map[types.NamespacedName]int
}

// LivenessProber asks a robot's adapter to confirm the robot is alive.
// [github.com/swarmada/swarmada/internal/command.Dispatcher] satisfies it.
type LivenessProber interface {
	// Heartbeat reports whether the adapter answered for this robot. A false with a
	// nil error means the stream was live and nothing answered; a non-nil error means
	// the push could not be made at all. Both are evidence of loss, not proof of life.
	Heartbeat(ctx context.Context, namespace, robotID string) (bool, error)
}

// confirmLiveness runs one attempt of the confirming exchange and reports whether the
// robot answered, how many consecutive attempts have now gone unanswered, and the push
// error if there was one.
//
// With no prober configured it reports the attempt budget as already spent, so the caller
// declares Offline on elapsed time alone. That is the pre-confirmation behaviour and the
// right fallback: a control plane that cannot push must still be able to detect a dead
// robot, and failing "closed" here would mean never declaring Offline at all.
func (r *RobotReconciler) confirmLiveness(ctx context.Context, robot *fleetv1.Robot) (confirmed bool, attempts int, err error) {
	if r.Liveness == nil {
		return false, heartbeatConfirmAttempts, nil
	}
	key := types.NamespacedName{Namespace: robot.Namespace, Name: robot.Name}

	alive, probeErr := r.Liveness.Heartbeat(ctx, robot.Namespace, robot.Name)
	if alive {
		r.clearBeats(robot)
		return true, 0, nil
	}

	r.beatsMu.Lock()
	if r.beats == nil {
		r.beats = map[types.NamespacedName]int{}
	}
	r.beats[key]++
	n := r.beats[key]
	r.beatsMu.Unlock()
	return false, n, probeErr
}

// clearBeats drops the attempt counter for a robot. Called when the robot answers and
// when it is finally declared Offline, so a later stall starts a fresh exchange rather
// than inheriting a spent budget and skipping confirmation entirely.
func (r *RobotReconciler) clearBeats(robot *fleetv1.Robot) {
	r.beatsMu.Lock()
	delete(r.beats, types.NamespacedName{Namespace: robot.Namespace, Name: robot.Name})
	r.beatsMu.Unlock()
}

// Reconcile is the core reconciliation loop for Robot objects.
// It is called whenever a Robot is created, updated, or deleted, and also on
// a periodic requeue to detect heartbeat timeouts.
func (r *RobotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("robot", req.NamespacedName)

	// ── 1. Fetch the Robot ────────────────────────────────────────────────────
	robot := &fleetv1.Robot{}
	if err := r.Get(ctx, req.NamespacedName, robot); err != nil {
		if errors.IsNotFound(err) {
			// Robot was deleted; clean up any assigned actions.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching robot: %w", err)
	}

	original := robot.DeepCopy()

	// Offline and Critical thresholds are namespace tunables (spec.health.*); resolve
	// both from a single SwarmadaConfig read, each failing safe to its default.
	heartbeatTimeout, criticalTimeout := r.connectivityTimeouts(ctx, robot.Namespace)

	// ── 2. Check heartbeat timeout ────────────────────────────────────────────
	// LastHeartbeat moved under status.connectivity.lastSeenAt (RFC-0001
	// §Robot Schema) — there is no longer a separate top-level field.
	if robot.Status.Connectivity != nil && robot.Status.Connectivity.LastSeenAt != nil {
		age := time.Since(robot.Status.Connectivity.LastSeenAt.Time)
		switch {
		case age > heartbeatTimeout && robot.Status.Phase != fleetv1.RobotPhaseOffline:
			// Confirm before declaring (§9.6.3.2). Stale telemetry is not the same fact as
			// a dead robot: a robot whose telemetry stalls while its stream is still live
			// was, until now, declared Offline without ever being asked. That matters
			// because Offline is the single trigger for in-flight action revocation
			// (§9.6.3.5) — a false Offline revokes work from a robot that is still doing it.
			//
			// The exchange is spread across reconciles rather than slept through inline:
			// three attempts five seconds apart is fifteen seconds, and holding a workqueue
			// slot that long per robot would let one flaky adapter stall reconciliation for
			// the whole fleet.
			confirmed, attempts, err := r.confirmLiveness(ctx, robot)
			if confirmed {
				logger.Info("telemetry stale but robot answered a heartbeat; not marking Offline",
					"age", age, "attempts", attempts)
				// Answered: leave the phase alone and come back when the next attempt
				// would be due. lastSeenAt is untouched on purpose — the robot answered a
				// liveness probe, it did not send telemetry, and conflating the two would
				// hide a genuinely stalled telemetry stream behind a healthy-looking age.
				return ctrl.Result{RequeueAfter: heartbeatConfirmInterval}, nil
			}
			if attempts < heartbeatConfirmAttempts {
				logger.V(1).Info("heartbeat unanswered; retrying before declaring Offline",
					"age", age, "attempt", attempts, "of", heartbeatConfirmAttempts, "err", err)
				return ctrl.Result{RequeueAfter: heartbeatConfirmInterval}, nil
			}
			logger.Info("robot heartbeat timed out and did not answer confirmation, marking Offline",
				"age", age, "attempts", attempts)
			r.clearBeats(robot)
			robot.Status.Phase = fleetv1.RobotPhaseOffline
			r.setCondition(robot, conditionTypeReady, metav1.ConditionFalse,
				"HeartbeatTimeout", fmt.Sprintf("No heartbeat for %s; %d confirmation attempt(s) unanswered",
					age.Round(time.Second), attempts))
			// Seal on the transition, not on the condition: this branch runs once, on the
			// edge into Offline (the switch requires phase != Offline to enter).
			r.recordRobotOffline(ctx, robot, robot.Status.Connectivity.LastSeenAt.Time, heartbeatTimeout)
		case age <= heartbeatTimeout:
			// Live: the Fleet Adapter is projecting fresh liveness (ADR-0026). Mark
			// Ready and advance a robot sitting on a liveness-owned phase — Discovered
			// (freshly admitted) or Offline (recovering) — to its steady summary:
			// InProgress when it holds an assigned action, else Idle (ADR-0029).
			// Scheduler-owned (Idle/Assigned/InProgress), Maintenance, Charging and
			// Error are left to their owners. The advance is derived from
			// conditions/liveness/assignedAction, rides the material-change patch below,
			// and fires on the edge only (once off Discovered the phase is no longer
			// liveness-owned) — never a telemetry-tick write (RA-1). Ready reflects
			// liveness only; schedulability (Idle + Connected + Conformance) is a
			// separate gate and is not touched here.
			if isLivenessOwnedPhase(robot.Status.Phase) {
				robot.Status.Phase = steadyPhaseForLive(robot)
			}
			if cur := findCondition(robot, conditionTypeReady); cur == nil || cur.Status != metav1.ConditionTrue {
				r.setCondition(robot, conditionTypeReady, metav1.ConditionTrue,
					"AdapterLive", "robot reachable via its Fleet Adapter")
			}
		}
	} else if robot.Status.Phase == "" {
		robot.Status.Phase = fleetv1.RobotPhaseDiscovered
		r.setCondition(robot, conditionTypeReady, metav1.ConditionUnknown,
			"Initialising", "Waiting for first Fleet Adapter heartbeat")
		// First time the control plane has ever observed this Robot, so this is the
		// admission (§9.6.5.1 ROBOT_ADMITTED). An unset phase is the idempotence
		// marker: it is written exactly once and never returns to "", so the entry
		// cannot be duplicated on requeue and no extra annotation is needed. This
		// covers BOTH admission paths — `swarmctl admit` and auto-admit (ADR-0014) —
		// because both end with a Robot the controller has not seen before.
		r.recordRobotAdmitted(ctx, robot)
	}

	// ── 3. Derive capabilities via the §6.10 truth table ──────────────────────
	priorCaps := robot.Status.Capabilities
	robot.Status.Capabilities = deriveCapabilities(robot)
	r.recordCapabilityDegradations(ctx, robot, priorCaps, robot.Status.Capabilities)

	// ── 3b. Offline-duration accounting (§9.3.8) ──────────────────────────────
	// Anchor OfflineSince when the robot enters Offline; at reconnect (phase left
	// Offline) observe the completed span and clear the anchor. Purely
	// observational — the OfflineSince write rides the existing material-change
	// patch below (RA-1: no extra write, and never on a telemetry tick). The
	// observe/clear fires exactly once per reconnect (OfflineSince is then nil).
	switch {
	case robot.Status.Phase == fleetv1.RobotPhaseOffline && robot.Status.OfflineSince == nil:
		now := metav1.Now()
		robot.Status.OfflineSince = &now
	case robot.Status.Phase != fleetv1.RobotPhaseOffline && robot.Status.OfflineSince != nil:
		offlineFor := time.Since(robot.Status.OfflineSince.Time)
		metrics.ObserveRobotOfflineDuration(robot.Namespace, offlineFor)
		// Same edge the metric is observed on, and it fires exactly once: OfflineSince is
		// cleared immediately below, so a later reconcile cannot re-enter this branch.
		r.recordRobotReconnected(ctx, robot, offlineFor)
		robot.Status.OfflineSince = nil
	}

	// ── 3c. Prolonged-offline "Critical" escalation (ADR-0011, §9.1.11 health) ──
	// A robot Offline for at least connectivityCriticalThresholdSeconds is surfaced
	// as a ConnectivityCritical condition — NOT a phase, since it is still Offline.
	// The condition is written only on its True↔False edges, so it rides the
	// material-change guard below and never churns LastTransitionTime (RA-1).
	critical := robot.Status.Phase == fleetv1.RobotPhaseOffline &&
		robot.Status.OfflineSince != nil &&
		!time.Now().Before(robot.Status.OfflineSince.Add(criticalTimeout))
	if r.reconcileConnectivityCritical(robot, critical) {
		logger.Info("robot connectivity critical: offline beyond threshold", "threshold", criticalTimeout)
		metrics.IncRobotConnectivityCritical(robot.Namespace)
		// reconcileConnectivityCritical returns true only on the False→True edge, so this
		// seals once per escalation rather than on every reconcile while Critical holds.
		r.recordRobotCritical(ctx, robot, time.Since(robot.Status.OfflineSince.Time))
	}

	// ── 3d. Opt-in offline auto-removal (ADR-0030) ────────────────────────────
	// Reclaim an auto-admitted robot whose adapter presence is gone and whose lease is
	// provably dead — e.g. a robot killed without a clean disconnect. Gated so a warehouse
	// (operator-created) robot is never removed: the Robot must be auto-admitted, the namespace
	// must opt in, the offline dwell must have elapsed, and any assigned action's lease must be
	// provably dead (§9.6.3.5; a nil horizon or a transient lookup error is NOT death — fail
	// closed). This is a lifecycle action driven by presence + lease death, never a telemetry
	// tick (RA-1), and it short-circuits the status patch since the object is being deleted.
	if robot.Status.Phase == fleetv1.RobotPhaseOffline && robot.Status.OfflineSince != nil && isAutoAdmitted(robot) {
		if enabled, grace := r.autoRemovePolicy(ctx, robot.Namespace); enabled &&
			!time.Now().Before(robot.Status.OfflineSince.Add(grace)) &&
			r.assignedLeaseProvablyDead(ctx, robot) {
			logger.Info("auto-removing offline auto-admitted robot: adapter presence gone and lease provably dead (ADR-0030)",
				"offlineSince", robot.Status.OfflineSince.Time, "grace", grace)
			if err := r.Delete(ctx, robot); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("auto-removing offline robot: %w", err)
			}
			return ctrl.Result{}, nil
		}
	}

	// ── 4. Persist status only on a material change ───────────────────────────
	// Skip no-op patches so unchanged robots do not generate a status write to
	// etcd on every periodic reconcile (RA-1: Robot.status is a throttled
	// projection of material state, not a per-tick heartbeat sink).
	if !reflect.DeepEqual(original.Status, robot.Status) {
		if err := r.Status().Patch(ctx, robot, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching robot status: %w", err)
		}
	}

	// Requeue after half the heartbeat timeout so we catch timeouts promptly. When
	// Offline but not yet Critical, also aim at the critical horizon if it is sooner,
	// so the escalation fires promptly even under a long offline threshold.
	requeue := heartbeatTimeout / 2
	if robot.Status.Phase == fleetv1.RobotPhaseOffline && !critical && robot.Status.OfflineSince != nil {
		if untilCritical := time.Until(robot.Status.OfflineSince.Add(criticalTimeout)); untilCritical > 0 && untilCritical < requeue {
			requeue = untilCritical
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// connectivityTimeouts resolves the namespace's offline and Critical thresholds
// (spec.health.connectivity{Offline,Critical}ThresholdSeconds) from a single
// SwarmadaConfig read. Each FAILS SAFE to its default — defaultHeartbeatTimeout /
// defaultConnectivityCriticalTimeout — on any problem (no config, list error, or a
// non-positive value), so an unreadable policy never changes the detection windows
// from their documented defaults (ADR-0011).
func (r *RobotReconciler) connectivityTimeouts(ctx context.Context, namespace string) (offline, critical time.Duration) {
	offline, critical = defaultHeartbeatTimeout, defaultConnectivityCriticalTimeout
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok {
		if s := cfg.Spec.Health.ConnectivityOfflineThresholdSeconds; s > 0 {
			offline = time.Duration(s) * time.Second
		}
		if s := cfg.Spec.Health.ConnectivityCriticalThresholdSeconds; s > 0 {
			critical = time.Duration(s) * time.Second
		}
	}
	return offline, critical
}

// reconcileConnectivityCritical sets or clears the ConnectivityCritical condition
// on its edges only (never on an unchanged robot), so the write rides the material-
// change guard and does not churn LastTransitionTime (RA-1). It returns true iff it
// just escalated (False→True), so the caller emits the escalation metric exactly
// once per episode.
func (r *RobotReconciler) reconcileConnectivityCritical(robot *fleetv1.Robot, critical bool) bool {
	existing := findCondition(robot, conditionTypeConnectivityCritical)
	switch {
	case critical && (existing == nil || existing.Status != metav1.ConditionTrue):
		r.setCondition(robot, conditionTypeConnectivityCritical, metav1.ConditionTrue,
			"OfflineThresholdExceeded", "robot offline beyond the connectivity-critical threshold")
		return true
	case !critical && existing != nil && existing.Status == metav1.ConditionTrue:
		r.setCondition(robot, conditionTypeConnectivityCritical, metav1.ConditionFalse,
			"Reconnected", "robot connectivity recovered below the critical threshold")
	}
	return false
}

// isAutoAdmitted reports whether the Robot was created by auto-admit (ADR-0014) and is thus
// eligible for opt-in offline auto-removal. Operator-created robots lack the marker and are
// never removed (ADR-0030).
func isAutoAdmitted(robot *fleetv1.Robot) bool {
	return robot.Annotations[fleetv1.AutoAdmittedAnnotation] == "true"
}

// autoRemovePolicy resolves the namespace's offline auto-removal opt-in and grace dwell from a
// single SwarmadaConfig read (ADR-0030). It FAILS SAFE to disabled on any problem (no config or
// the opt-in unset), so an unreadable policy never removes a robot. The grace falls back to
// defaultAutoRemoveOfflineGrace when unset or non-positive.
func (r *RobotReconciler) autoRemovePolicy(ctx context.Context, namespace string) (enabled bool, grace time.Duration) {
	grace = defaultAutoRemoveOfflineGrace
	cfg, ok := namespaceConfig(ctx, r.Client, namespace)
	if !ok || !cfg.Spec.Provisioning.AutoRemoveOfflineRobots {
		return false, grace
	}
	if s := cfg.Spec.Provisioning.AutoRemoveOfflineGraceSeconds; s > 0 {
		grace = time.Duration(s) * time.Second
	}
	return true, grace
}

// assignedLeaseProvablyDead reports whether the robot holds no live lease: it has no assigned
// action, the named FleetAction is gone, or that action's lease horizon is provably past
// (§9.6.3.5, reusing leaseProvablyDead/leaseClockSkew from the action controller). A nil horizon
// is NOT proof of death, and a transient lookup error FAILS CLOSED (returns false) so removal
// never races a robot that might still be executing (ADR-0030).
func (r *RobotReconciler) assignedLeaseProvablyDead(ctx context.Context, robot *fleetv1.Robot) bool {
	name := robot.Status.AssignedAction
	if name == "" {
		return true
	}
	action := &fleetv1.FleetAction{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: robot.Namespace, Name: name}, action); err != nil {
		if errors.IsNotFound(err) {
			return true
		}
		return false // fail closed on a transient error
	}
	return leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), time.Now(), leaseClockSkew)
}

// isLivenessOwnedPhase reports whether the Robot controller may advance this phase when the
// robot goes live: Discovered (freshly admitted, awaiting first liveness), Offline (recovering),
// or empty (uninitialised). The scheduler owns Idle/Assigned/InProgress, the ZoneMaintenance
// controller owns Maintenance, and Charging/Error are robot-reported — none are overridden
// here (ADR-0029).
func isLivenessOwnedPhase(p fleetv1.RobotPhase) bool {
	return p == fleetv1.RobotPhaseDiscovered || p == fleetv1.RobotPhaseOffline || p == ""
}

// steadyPhaseForLive is the phase a live robot settles into, derived purely from whether it
// currently holds an assigned action: InProgress ("Working") when one is assigned, else Idle
// ("Ready"). This mirrors the scheduler's own assign/release transitions
// (fleetaction_controller), so the two agree on a robot that is both live and assigned (ADR-0029).
func steadyPhaseForLive(robot *fleetv1.Robot) fleetv1.RobotPhase {
	if robot.Status.AssignedAction != "" {
		return fleetv1.RobotPhaseInProgress
	}
	return fleetv1.RobotPhaseIdle
}

// findCondition returns a pointer to the named condition on the robot, or nil.
func findCondition(robot *fleetv1.Robot, condType string) *metav1.Condition {
	for i := range robot.Status.Conditions {
		if robot.Status.Conditions[i].Type == condType {
			return &robot.Status.Conditions[i]
		}
	}
	return nil
}

// deriveCapabilities computes status.capabilities[] from the declared
// spec.capabilities[] using the §6.10 Capability Type System truth table, unioned
// with the model-granted capabilities a completed ModelRollout recorded in
// status.modelGrantedCapabilities[]. It is a pure projection of already-material
// inputs (status.hardware[], status.installedModels[], phase) — the Reconcile
// DeepEqual guard means it triggers a status write only when the set changes, so
// it never becomes a per-tick sink (RA-1).
func deriveCapabilities(robot *fleetv1.Robot) []fleetv1.CapabilityStatusEntry {
	hwByName := make(map[string]fleetv1.HardwareStatus, len(robot.Status.Hardware))
	for _, hw := range robot.Status.Hardware {
		hwByName[hw.Name] = hw.Status
	}
	// spec.hardware[] carries the numeric attribute fields a SourceField parameter
	// sources from (e.g. hardware[load-platform].spec.maxPayloadKg).
	specHwByName := make(map[string]fleetv1.HardwareComponent, len(robot.Spec.Hardware))
	for i := range robot.Spec.Hardware {
		specHwByName[robot.Spec.Hardware[i].Name] = robot.Spec.Hardware[i]
	}
	modelByName := make(map[string]fleetv1.ModelStatus, len(robot.Status.InstalledModels))
	for _, m := range robot.Status.InstalledModels {
		modelByName[m.Name] = m.Status
	}
	// A robot suspended by ZoneMaintenance is in the Maintenance phase (driven by
	// the Zone Controller from the ZoneMaintenance lifecycle); pauseable
	// capabilities are then Paused (§6.10.4/.5).
	underMaintenance := robot.Status.Phase == fleetv1.RobotPhaseMaintenance

	// Prior entries let DegradedSince persist across reconciles instead of churning.
	prev := make(map[string]fleetv1.CapabilityStatusEntry, len(robot.Status.Capabilities))
	for _, c := range robot.Status.Capabilities {
		prev[c.Name] = c
	}

	byName := make(map[string]fleetv1.CapabilityStatusEntry, len(robot.Spec.Capabilities))
	for i := range robot.Spec.Capabilities {
		capDef := &robot.Spec.Capabilities[i]
		status, paused, reason := evaluateCapability(capDef, hwByName, modelByName, underMaintenance)
		entry := fleetv1.CapabilityStatusEntry{
			Name:               capDef.Name,
			Status:             status,
			Paused:             paused,
			Reason:             reason,
			ResolvedParameters: resolveParameters(capDef, specHwByName),
			// Surface the class's degradedPolicy so the Scheduler (which reads only
			// Robot.status) can serve lower-constraint actions on a Degraded capability
			// without fetching the RobotClass.
			DegradedSchedulable: capDef.DegradedPolicy != nil && capDef.DegradedPolicy.Schedulable,
		}
		entry.DegradedSince = degradedSince(status, prev[capDef.Name])
		byName[capDef.Name] = entry
	}

	// Union in model-granted capabilities (written by the OTA/Model Update Manager
	// only after a rollout completes, so the model is Active). A spec-declared
	// entry of the same name wins.
	for _, mg := range robot.Status.ModelGrantedCapabilities {
		for _, name := range mg.Capabilities {
			if _, declared := byName[name]; declared {
				continue
			}
			byName[name] = fleetv1.CapabilityStatusEntry{Name: name, Status: fleetv1.CapabilityStatusActive}
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names) // stable order so an unchanged set compares equal

	result := make([]fleetv1.CapabilityStatusEntry, len(names))
	for i, name := range names {
		result[i] = byName[name]
	}
	return result
}

// evaluateCapability applies the §6.10 truth table to one declared capability.
// Returns its computed status, whether it is maintenance-paused, and a reason for
// any non-Active status.
func evaluateCapability(
	capDef *fleetv1.ClassCapability,
	hwByName map[string]fleetv1.HardwareStatus,
	modelByName map[string]fleetv1.ModelStatus,
	underMaintenance bool,
) (fleetv1.CapabilityStatus, bool, string) {
	// Maintenance pause applies only to pauseable capabilities (§6.10.4). Safety/
	// monitoring capabilities (pauseable:false) keep operating and are evaluated.
	if underMaintenance && capDef.Pauseable {
		return fleetv1.CapabilityStatusPaused, true, "suspended by zone maintenance"
	}

	// manual capabilities are operator-asserted; no hardware/model activation gate.
	if capDef.Type == fleetv1.CapabilityKindManual {
		return fleetv1.CapabilityStatusActive, false, ""
	}

	// Hardware gate (all non-manual types): a required component absent or Failed
	// deactivates the capability; a Degraded component degrades it.
	degraded := false
	for _, name := range capDef.RequiredHardware {
		st, ok := hwByName[name]
		if !ok {
			return fleetv1.CapabilityStatusInactive, false, "required hardware not reported: " + name
		}
		switch st {
		case fleetv1.HardwareDisabled:
			// Intentionally off (operator/edge toggle) — Inactive but benign, distinct
			// from a fault; not critical, not a "failed" reason (ADR-0031).
			return fleetv1.CapabilityStatusInactive, false, "disabled: " + name
		case fleetv1.HardwareFailed:
			return fleetv1.CapabilityStatusInactive, false, "required hardware failed: " + name
		case fleetv1.HardwareDegraded:
			degraded = true
		}
	}

	// Model gate (model-driven only, §6.10.2): the providing model must be Active.
	if capDef.Type == fleetv1.CapabilityKindModelDriven {
		st, ok := modelByName[capDef.ProvidingModel]
		if !ok || st != fleetv1.ModelStatusActive {
			return fleetv1.CapabilityStatusInactive, false, modelReason(st, ok)
		}
	}

	if degraded {
		return fleetv1.CapabilityStatusDegraded, false, "required hardware degraded"
	}
	return fleetv1.CapabilityStatusActive, false, ""
}

// modelReason explains why a model-driven capability is Inactive, distinguishing a
// transient rollout from a permanent failure (§6.10.2).
func modelReason(st fleetv1.ModelStatus, present bool) string {
	if !present {
		return "providing model not installed"
	}
	switch st {
	case fleetv1.ModelStatusUpdating:
		return "providing model rolling out"
	case fleetv1.ModelStatusFailed:
		return "providing model failed health check"
	default:
		return "providing model not active"
	}
}

// degradedSince carries forward the prior DegradedSince timestamp so it is set
// once on the Active→non-Active transition and cleared on return to Active — it
// must not churn on every reconcile.
func degradedSince(status fleetv1.CapabilityStatus, prev fleetv1.CapabilityStatusEntry) *metav1.Time {
	if status == fleetv1.CapabilityStatusActive {
		return nil
	}
	if prev.DegradedSince != nil {
		return prev.DegradedSince
	}
	now := metav1.Now()
	return &now
}

// sourceFieldRe matches a SourceField path of the form
// "hardware[<component-name>].spec.<field>" (§6.10.3).
var sourceFieldRe = regexp.MustCompile(`^hardware\[([^\]]+)\]\.spec\.([A-Za-z][A-Za-z0-9]*)$`)

// resolveParameters resolves a parametric capability's parameters into numeric
// ResolvedParameters (§6.10.3). A static Value is parsed as a float; a dynamic
// SourceField is resolved from the robot's spec.hardware[] at THIS evaluation, so a
// hardware attribute that changes after admission is reflected without re-admission.
// A parameter that cannot be resolved (bad literal, unparseable path, unknown
// component/field, or an unset attribute) is omitted — the same skip semantics the
// Value path already used — and the Scheduler's parametric filter treats an absent
// parameter as unsatisfied.
func resolveParameters(capDef *fleetv1.ClassCapability, hardware map[string]fleetv1.HardwareComponent) map[string]float64 {
	if len(capDef.Parameters) == 0 {
		return nil
	}
	out := make(map[string]float64)
	for name, p := range capDef.Parameters {
		switch {
		case p.Value != "":
			if v, err := strconv.ParseFloat(p.Value, 64); err == nil {
				out[name] = v
			}
		case p.SourceField != "":
			if v, ok := resolveSourceField(p.SourceField, hardware); ok {
				out[name] = v
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveSourceField resolves "hardware[<name>].spec.<field>" to the numeric value of
// that hardware component's attribute, or (0, false) if the path is malformed, the
// component is absent, the field is unknown, or the attribute is unset.
func resolveSourceField(path string, hardware map[string]fleetv1.HardwareComponent) (float64, bool) {
	m := sourceFieldRe.FindStringSubmatch(path)
	if m == nil {
		return 0, false
	}
	c, ok := hardware[m[1]]
	if !ok {
		return 0, false
	}
	return hardwareSpecValue(c, m[2])
}

// hardwareSpecValue returns the numeric value of a HardwareComponent's named spec
// attribute (by JSON field name). An unset (nil) attribute or an unknown field name
// returns (0, false).
func hardwareSpecValue(c fleetv1.HardwareComponent, field string) (float64, bool) {
	f64 := func(p *float64) (float64, bool) {
		if p == nil {
			return 0, false
		}
		return *p, true
	}
	i32 := func(p *int32) (float64, bool) {
		if p == nil {
			return 0, false
		}
		return float64(*p), true
	}
	switch field {
	case "rangeM":
		return f64(c.RangeM)
	case "horizontalFovDeg":
		return f64(c.HorizontalFovDeg)
	case "resolutionMp":
		return f64(c.ResolutionMp)
	case "maxPayloadKg":
		return f64(c.MaxPayloadKg)
	case "maxGripForceN":
		return f64(c.MaxGripForceN)
	case "frameRateFps":
		return i32(c.FrameRateFps)
	case "platformLengthMm":
		return i32(c.PlatformLengthMm)
	case "platformWidthMm":
		return i32(c.PlatformWidthMm)
	case "strokeMm":
		return i32(c.StrokeMm)
	default:
		return 0, false
	}
}

// setCondition upserts a metav1.Condition on the Robot.
func (r *RobotReconciler) setCondition(robot *fleetv1.Robot, condType string,
	status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range robot.Status.Conditions {
		if c.Type == condType {
			robot.Status.Conditions[i].Status = status
			robot.Status.Conditions[i].Reason = reason
			robot.Status.Conditions[i].Message = message
			robot.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	robot.Status.Conditions = append(robot.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: robot.Generation,
	})
}

// SetupWithManager registers the controller with the manager.
func (r *RobotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.Robot{}).
		Complete(r)
}

// sealRobotEvent appends one §9.6.5.1 entry about a Robot to the tamper-evident chain.
//
// Best-effort and nil-safe by construction: every caller sits on a transition that has
// already been decided, so a sink failure must log and return — never block the
// transition. An audit log that can stop a robot being declared Offline would be a
// liveness dependency on an observability component, which is the wrong trade in exactly
// the situation the record exists to describe.
func (r *RobotReconciler) sealRobotEvent(ctx context.Context, robot *fleetv1.Robot, eventType, action string, detail map[string]string) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: eventType,
		Namespace: robot.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "robot-controller"},
		Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
		Action:    action,
		Outcome:   audit.OutcomeAllowed,
		Detail:    detail,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording audit entry", "event", eventType, "robot", robot.Name)
	}
}

// recordRobotOffline seals ROBOT_OFFLINE on the T1 transition. The threshold is recorded
// alongside the last-seen time because the same elapsed gap is or is not an outage
// depending on the namespace's configured threshold, and that config can change between
// the incident and the review.
func (r *RobotReconciler) recordRobotOffline(ctx context.Context, robot *fleetv1.Robot, lastSeen time.Time, threshold time.Duration) {
	r.sealRobotEvent(ctx, robot, audit.EventRobotOffline, "offline", map[string]string{
		"last_seen_at":              lastSeen.UTC().Format(time.RFC3339Nano),
		"offline_threshold_seconds": strconv.Itoa(int(threshold.Seconds())),
	})
}

// recordRobotCritical seals ROBOT_CRITICAL on the T2 escalation edge, listing the actions
// left in Revoking. Those action names are the point of the record: at T2 nothing is
// requeued — reassignment waits for lease expiry (§9.6.3.5) — so an operator reading the
// chain needs to see precisely which work is stranded and on which robot.
func (r *RobotReconciler) recordRobotCritical(ctx context.Context, robot *fleetv1.Robot, offlineFor time.Duration) {
	detail := map[string]string{"offline_duration_seconds": strconv.Itoa(int(offlineFor.Seconds()))}
	detail["revoking_actions"] = strings.Join(r.revokingActionsFor(ctx, robot), ",")
	r.sealRobotEvent(ctx, robot, audit.EventRobotCritical, "critical", detail)
}

// recordRobotReconnected seals ROBOT_RECONNECTED when the robot leaves Offline. The
// firmware version is recorded because a robot that reconnects on a different firmware
// than it dropped on is a materially different event from a clean reconnect.
func (r *RobotReconciler) recordRobotReconnected(ctx context.Context, robot *fleetv1.Robot, offlineFor time.Duration) {
	r.sealRobotEvent(ctx, robot, audit.EventRobotReconnected, "reconnect", map[string]string{
		"offline_duration_seconds": strconv.Itoa(int(offlineFor.Seconds())),
		"firmware_version":         robot.Status.FirmwareVersion,
	})
}

// revokingActionsFor lists this robot's FleetActions currently in Revoking. A lookup
// failure yields no names rather than failing the record: an incomplete detail field is
// worth more to an incident review than a missing entry.
func (r *RobotReconciler) revokingActionsFor(ctx context.Context, robot *fleetv1.Robot) []string {
	var actions fleetv1.FleetActionList
	if err := r.List(ctx, &actions, client.InNamespace(robot.Namespace)); err != nil {
		return nil
	}
	var names []string
	for i := range actions.Items {
		a := &actions.Items[i]
		if a.Status.Phase == fleetv1.ActionPhaseRevoking && a.Status.AssignedRobot == robot.Name {
			names = append(names, a.Name)
		}
	}
	sort.Strings(names)
	return names
}

// recordCapabilityDegradations seals CAPABILITY_DEGRADED for each capability that has just
// left Active. Derived by diffing the freshly-computed list against the one already on the
// object, so it fires on the TRANSITION only — a capability that stays Degraded across
// reconciles is recorded once, not on every pass (RA-1).
func (r *RobotReconciler) recordCapabilityDegradations(ctx context.Context, robot *fleetv1.Robot,
	prior, next []fleetv1.CapabilityStatusEntry) {
	if r.Audit == nil {
		return
	}
	was := make(map[string]fleetv1.CapabilityStatus, len(prior))
	for i := range prior {
		was[prior[i].Name] = prior[i].Status
	}
	for i := range next {
		e := &next[i]
		// Only Active → non-Active. A capability that was already non-Active has been
		// recorded; one that has no prior entry is new, not degraded.
		before, seen := was[e.Name]
		if !seen || before != fleetv1.CapabilityStatusActive || e.Status == fleetv1.CapabilityStatusActive {
			continue
		}
		r.sealRobotEvent(ctx, robot, audit.EventCapabilityDegraded, "capability-degrade", map[string]string{
			"capability_name": e.Name,
			"prior_status":    string(before),
			"new_status":      string(e.Status),
			"reason":          e.Reason,
		})
	}
}

// recordRobotAdmitted writes ROBOT_ADMITTED (§9.6.5.1) on a Robot's first
// reconcile. The two-phase admission gate (§6.6) is a security control, so its
// decisions belong in the tamper-evident chain and not only in the event stream.
// Best-effort: the Robot exists either way, and a sink failure must not block it.
func (r *RobotReconciler) recordRobotAdmitted(ctx context.Context, robot *fleetv1.Robot) {
	if r.Audit == nil {
		return
	}
	detail := map[string]string{"zone": robot.Spec.Zone}
	if robot.Spec.RobotClass != "" {
		detail["robot_class"] = robot.Spec.RobotClass
	}
	// Distinguishes the zero-touch path from an operator-reviewed one; an auditor
	// reading the chain needs to know which gate the robot actually passed.
	if isAutoAdmitted(robot) {
		detail["admission_path"] = "auto-admit"
	} else {
		detail["admission_path"] = "operator"
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventRobotAdmitted,
		Namespace: robot.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "robot-controller"},
		Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
		Action:    "admit",
		Outcome:   audit.OutcomeAllowed,
		Detail:    detail,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording ROBOT_ADMITTED audit entry", "robot", robot.Name)
	}
}
