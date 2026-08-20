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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

const (
	// annEstopTriggered fires a zone-wide estop (set by the `estop-trigger` verb,
	// §9.5.3). Its value is the estop reason/id — a NEW value re-fires; the same
	// value is idempotent.
	annEstopTriggered = "swarmada.io/estop-triggered"
	// annEstopProcessed records the last estop-trigger value the controller acted
	// on, so a re-reconcile does not re-fan-out the same estop.
	annEstopProcessed = "swarmada.io/estop-processed"
	// zoneEstopWalkCap bounds the parentZone ancestor walk.
	zoneEstopWalkCap = 64
)

// ZoneEstopper is the confirmed per-robot estop primitive the zone fan-out reuses.
// [github.com/swarmada/swarmada/internal/safety.Dispatcher] satisfies it: it pushes
// the estop over the robot's SafetyStream and marks Stopped only on a CONFIRMED
// EstopAck — a robot it cannot confirm resolves to Failed (escalate), never Stopped.
type ZoneEstopper interface {
	// scope names the OPERATOR ACTION that produced this stop (robot / zone / namespace),
	// not the object being written. A zone or namespace estop fans out per robot through
	// this same primitive, so nothing below this call can distinguish the three — the
	// §9.3.8 `scope` label has to be carried in from the reconciler that fanned out.
	TriggerEstop(ctx context.Context, namespace, robotID, reason, issuedBy string,
		scope metrics.EstopScope) (safety.Result, error)
	// ClearEstop resets a robot's estop to Normal on an operator-authorized clear
	// (§9.6.2.3). No-op when the robot is not estopped; the action stays operator-gated.
	ClearEstop(ctx context.Context, namespace, robotID, clearedBy string) (fleetv1.RobotEstopState, error)
}

// ZoneEstopReconciler drives zone-level emergency stops with hierarchical
// propagation (RFC-0001 §9.6.2.5): on an estop-trigger it confirmed-estops every
// robot in the zone — and, when estopPolicy.propagateToChildren, every robot in a
// descendant zone — and notifies the parent (propagateToParent) with a
// ChildEstopTriggered event, WITHOUT auto-estopping the parent.
type ZoneEstopReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Estopper issues the confirmed per-robot estop. Nil disables zone estop.
	Estopper ZoneEstopper
	// Recorder emits the ChildEstopTriggered event on the parent. Nil skips it.
	Recorder record.EventRecorder
	// Audit records the ESTOP_TRIGGERED safety event. Nil skips it.
	Audit audit.Recorder
}

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch

// Reconcile fans out a zone estop when the trigger annotation is (newly) present.
func (r *ZoneEstopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("fleetzone", req.NamespacedName)

	zone := &fleetv1.FleetZone{}
	if err := r.Get(ctx, req.NamespacedName, zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.Estopper == nil {
		return ctrl.Result{}, nil
	}
	trigger, firing := zone.Annotations[annEstopTriggered]
	processed, wasProcessed := zone.Annotations[annEstopProcessed]

	switch {
	case firing && processed == trigger:
		return ctrl.Result{}, nil // this exact estop already fanned out
	case !firing && wasProcessed:
		// The estop-trigger annotation was removed (the operator-authorized
		// estop-clear verb): resume the robots and drop the processed marker.
		return r.clearZoneEstop(ctx, req.Namespace, zone)
	case !firing:
		return ctrl.Result{}, nil // no estop active
	}

	toChildren, toParent := true, false // §9.6.2.5 documented defaults
	if p := zone.Spec.EstopPolicy; p != nil {
		toChildren, toParent = p.PropagateToChildren, p.PropagateToParent
	}
	reason := trigger
	if reason == "" || reason == "true" {
		reason = "zone emergency stop"
	}
	// The operator the mutating webhook stamped at admission (ADR-0046); falls back to
	// an explicitly unattributed actor when none was resolvable — never to a synthetic
	// name that reads like a real one.
	actor := estopActor(zone.Annotations, "zone-estop:"+zone.Name)
	issuedBy := actor.Identity

	// ── Fan out to every robot in scope (confirmed estop each) ─────────────────
	robots, err := r.robotsInEstopScope(ctx, req.Namespace, zone.Name, toChildren)
	if err != nil {
		return ctrl.Result{}, err
	}
	stopped, failed := 0, 0
	// Collected for the audit entry: WHICH robots were in scope (§9.6.5.1
	// robots_in_scope) and the worst ack latency across the fan-out. A per-robot
	// rpc_sent_at/ack_received_at pair cannot collapse into one entry for a fan-out,
	// so the robot-scope entries carry those; here the worst latency is the number
	// that matters, because the §9.6.2.2 SLA is breached if ANY robot exceeds it.
	inScope := make([]string, 0, len(robots))
	var worstLatency time.Duration
	// Issued in PARALLEL (§9.6.2.1): one goroutine per robot, so a slow acknowledgement
	// from one robot never delays the estop signal to the others. estopFanout also returns
	// the episode's wall-clock duration — trigger to last robot resolved — which is the
	// interval the per-robot latency histogram structurally cannot see (ADR-0042).
	outcomes, fanout := estopFanout(ctx, r.Estopper, req.Namespace, robots, reason, issuedBy,
		metrics.ScopeZone)
	// Aggregated after the join, in robot order: the counts, the worst latency and the audit
	// entry must not depend on which goroutine happened to finish first.
	for _, o := range outcomes {
		inScope = append(inScope, o.robot)
		if o.result.Delivered && o.result.Latency > worstLatency {
			worstLatency = o.result.Latency
		}
		if o.err != nil || o.result.State != fleetv1.RobotEstopStopped {
			failed++
			logger.Info("zone estop not confirmed for robot (escalate)", "robot", o.robot,
				"state", o.result.State, "err", errString(o.err))
			continue
		}
		stopped++
	}
	// Observed for every episode that fans out, including one that stopped nothing: a zone
	// whose robots all failed to confirm is exactly the episode an operator needs timed.
	metrics.ObserveEstopFanout(req.Namespace, metrics.ScopeZone, fanout)
	logger.Info("zone estop fanned out", "zone", zone.Name, "reason", reason,
		"robots", len(robots), "stopped", stopped, "unconfirmed", failed, "fanout", fanout)

	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if failed > 0 {
			outcome = audit.OutcomeError // some robots could not be confirmed stopped
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: req.Namespace,
			EventType: audit.EventEstopTriggered,
			Action:    "estop-trigger",
			Outcome:   outcome,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "FleetZone", Namespace: req.Namespace, Name: zone.Name},
			Detail: map[string]string{
				"reason": reason,
				// Comma-separated with no spaces — the list encoding a flat
				// map[string]string forces (§9.6.5.2). Readers split on ",".
				"robots_in_scope":    strings.Join(inScope, ","),
				"robots":             itoa(len(robots)),
				"stopped":            itoa(stopped),
				"unconfirmed":        itoa(failed),
				"max_ack_latency_ms": itoa(int(worstLatency.Milliseconds())),
			},
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_TRIGGERED audit entry")
		}
	}

	// ── Notify the parent (no auto-estop) ──────────────────────────────────────
	if toParent && zone.Spec.ParentZone != "" && r.Recorder != nil {
		parent := &fleetv1.FleetZone{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: zone.Spec.ParentZone}, parent); err == nil {
			r.Recorder.Event(parent, corev1.EventTypeWarning, "ChildEstopTriggered",
				fmt.Sprintf("child zone %s triggered an emergency stop (%s); this zone is NOT auto-estopped", zone.Name, reason))
			// Reflect the descendant estop on the parent's status (does not
			// override a parent that is itself directly Triggered).
			if parent.Status.EstopStatus != fleetv1.ZoneEstopTriggered {
				pbase := parent.DeepCopy()
				pnow := metav1.Now()
				parent.Status.EstopStatus = fleetv1.ZoneEstopChildTriggered
				parent.Status.LastEstopAt = &pnow
				if serr := r.Status().Patch(ctx, parent, client.MergeFrom(pbase)); serr != nil {
					logger.Error(serr, "writing parent ChildTriggered status")
				}
			}
		}
	}

	// Mark this trigger processed so it is not re-fanned-out.
	base := zone.DeepCopy()
	if zone.Annotations == nil {
		zone.Annotations = map[string]string{}
	}
	zone.Annotations[annEstopProcessed] = trigger
	if err := r.Patch(ctx, zone, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording estop processed: %w", err)
	}

	// Reflect the active estop in the zone's status (§9.1.5).
	sbase := zone.DeepCopy()
	now := metav1.Now()
	zone.Status.EstopStatus = fleetv1.ZoneEstopTriggered
	zone.Status.LastEstopAt = &now
	if err := r.Status().Patch(ctx, zone, client.MergeFrom(sbase)); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing zone estop status: %w", err)
	}
	return ctrl.Result{}, nil
}

// clearZoneEstop resumes the zone's robots after an operator clears the estop
// (the trigger annotation was removed). It resolves the same scope as the trigger
// and resets each estopped robot to Normal (idempotent — a robot not under an
// estop is a no-op); a action Paused by the estop stays operator-gated (§9.6.2.4).
// It then drops the processed marker so a fresh trigger fires cleanly.
func (r *ZoneEstopReconciler) clearZoneEstop(ctx context.Context, namespace string, zone *fleetv1.FleetZone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	toChildren := true // scope must match the trigger's default
	if p := zone.Spec.EstopPolicy; p != nil {
		toChildren = p.PropagateToChildren
	}
	actor := estopActor(zone.Annotations, "zone-estop:"+zone.Name)
	clearedBy := actor.Identity

	robots, err := r.robotsInEstopScope(ctx, namespace, zone.Name, toChildren)
	if err != nil {
		return ctrl.Result{}, err
	}
	cleared := 0
	for i := range robots {
		if _, cerr := r.Estopper.ClearEstop(ctx, namespace, robots[i].Name, clearedBy); cerr != nil {
			logger.Error(cerr, "clearing robot estop", "robot", robots[i].Name)
			continue
		}
		cleared++
	}
	logger.Info("zone estop cleared", "zone", zone.Name, "robots", len(robots))

	if r.Audit != nil {
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: namespace,
			EventType: audit.EventEstopCleared,
			Action:    "estop-clear",
			Outcome:   audit.OutcomeAllowed,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "FleetZone", Namespace: namespace, Name: zone.Name},
			Detail:    map[string]string{"robots": itoa(len(robots)), "cleared": itoa(cleared)},
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_CLEARED audit entry")
		}
	}

	base := zone.DeepCopy()
	delete(zone.Annotations, annEstopProcessed)
	if err := r.Patch(ctx, zone, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing estop processed marker: %w", err)
	}

	// Reset the zone's estop status.
	sbase := zone.DeepCopy()
	zone.Status.EstopStatus = fleetv1.ZoneEstopClear
	if err := r.Status().Patch(ctx, zone, client.MergeFrom(sbase)); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing zone estop status: %w", err)
	}
	return ctrl.Result{}, nil
}

// robotsInEstopScope lists the robots a zone estop targets: every robot in the
// zone, plus — when toChildren — every robot in a descendant zone.
func (r *ZoneEstopReconciler) robotsInEstopScope(ctx context.Context, namespace, zoneName string, toChildren bool) ([]fleetv1.Robot, error) {
	var all fleetv1.RobotList
	if err := r.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing robots: %w", err)
	}
	out := make([]fleetv1.Robot, 0, len(all.Items))
	for i := range all.Items {
		robotZone := all.Items[i].Status.CurrentZone
		if robotZone == "" {
			robotZone = all.Items[i].Spec.Zone
		}
		in, err := r.zoneCovers(ctx, namespace, zoneName, robotZone, toChildren)
		if err != nil {
			return nil, err
		}
		if in {
			out = append(out, all.Items[i])
		}
	}
	return out, nil
}

// zoneCovers reports whether an estop on zoneName reaches a robot whose zone is
// robotZone: always when robotZone IS the zone; and — when toChildren — when
// zoneName is an ancestor of robotZone (bounded parentZone walk).
func (r *ZoneEstopReconciler) zoneCovers(ctx context.Context, namespace, zoneName, robotZone string, toChildren bool) (bool, error) {
	if robotZone == "" {
		return false, nil
	}
	if robotZone == zoneName {
		return true, nil
	}
	if !toChildren {
		return false, nil
	}
	zone := robotZone
	for depth := 0; zone != "" && depth < zoneEstopWalkCap; depth++ {
		fz := &fleetv1.FleetZone{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: zone}, fz); err != nil {
			return false, nil // missing ancestor → chain end
		}
		if fz.Spec.ParentZone == zoneName {
			return true, nil
		}
		zone = fz.Spec.ParentZone
	}
	return false, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// SetupWithManager registers the zone-estop controller.
func (r *ZoneEstopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetZone{}).
		Named("zoneestop").
		Complete(r)
}
