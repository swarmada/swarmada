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
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/safety"
)

// RobotEstopReconciler drives ROBOT-scope emergency stops (RFC-0001 §9.6.2 estop
// scopes): a `swarmada.io/estop-triggered` annotation on a single Robot confirmed-
// estops that one robot via the same primitive the zone fan-out uses, and removing
// the annotation (the operator-authorized estop-clear verb) resets it to Normal. It
// reuses the annEstopTriggered/annEstopProcessed idempotency markers, so a
// re-reconcile of the same trigger value is a no-op and a new value re-fires. Both
// annotation writes are SAR-gated at admission by the Robot validating webhook
// (§F-2b), so an unauthorized user cannot trigger or clear a robot estop.
type RobotEstopReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// now overrides the clock in tests. Nil means time.Now.
	now func() time.Time
	// Estopper issues the confirmed per-robot estop. Nil disables robot estop.
	Estopper ZoneEstopper
	// Audit records the ESTOP_TRIGGERED / ESTOP_CLEARED safety event. Nil skips it.
	Audit audit.Recorder
}

// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch;update;patch

// Reconcile issues a robot estop when the trigger annotation is (newly) present and
// clears it when the annotation is removed.
func (r *RobotEstopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("robot", req.NamespacedName)

	robot := &fleetv1.Robot{}
	if err := r.Get(ctx, req.NamespacedName, robot); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Estopper == nil {
		return ctrl.Result{}, nil
	}
	trigger, firing := robot.Annotations[annEstopTriggered]
	processed, wasProcessed := robot.Annotations[annEstopProcessed]

	switch {
	case firing && processed == trigger:
		return ctrl.Result{}, nil // this exact estop already issued
	case !firing && wasProcessed:
		return r.clearRobotEstop(ctx, robot)
	case !firing:
		return ctrl.Result{}, nil // no estop active
	}

	reason := trigger
	if reason == "" || reason == "true" {
		reason = "robot emergency stop"
	}
	issuedBy := "robot-estop:" + robot.Name

	sentAt := r.clock()
	res, terr := r.Estopper.TriggerEstop(ctx, robot.Namespace, robot.Name, reason, issuedBy)
	confirmed := terr == nil && res.State == fleetv1.RobotEstopStopped
	if !confirmed {
		logger.Info("robot estop not confirmed (escalate)", "robot", robot.Name,
			"state", res.State, "err", errString(terr))
	}

	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if !confirmed {
			outcome = audit.OutcomeError
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: robot.Namespace,
			EventType: audit.EventEstopTriggered,
			Action:    "estop-trigger",
			Outcome:   outcome,
			Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: issuedBy},
			Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
			// The §9.6.2.2 SLA is measured on this round trip, so the audit chain — not
			// only Prometheus — carries the evidence a post-incident review needs. A
			// latency of 0 means no ack came back, which is why delivered is recorded
			// separately rather than inferred from the number.
			Detail: estopTimingDetail(map[string]string{
				"reason": reason, "state": string(res.State),
			}, sentAt, res),
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_TRIGGERED audit entry")
		}
	}

	base := robot.DeepCopy()
	if robot.Annotations == nil {
		robot.Annotations = map[string]string{}
	}
	robot.Annotations[annEstopProcessed] = trigger
	if err := r.Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording robot estop processed: %w", err)
	}
	return ctrl.Result{}, nil
}

// clearRobotEstop resets the robot to Normal after the operator removes the trigger
// annotation (estop-clear). Idempotent — a robot not under an estop is a no-op; a
// action Paused by the estop stays operator-gated (§9.6.2.4).
func (r *RobotEstopReconciler) clearRobotEstop(ctx context.Context, robot *fleetv1.Robot) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	clearedBy := "robot-estop:" + robot.Name
	if _, cerr := r.Estopper.ClearEstop(ctx, robot.Namespace, robot.Name, clearedBy); cerr != nil {
		logger.Error(cerr, "clearing robot estop", "robot", robot.Name)
	}

	if r.Audit != nil {
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: robot.Namespace,
			EventType: audit.EventEstopCleared,
			Action:    "estop-clear",
			Outcome:   audit.OutcomeAllowed,
			Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: clearedBy},
			Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_CLEARED audit entry")
		}
	}

	base := robot.DeepCopy()
	delete(robot.Annotations, annEstopProcessed)
	if err := r.Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing robot estop processed marker: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the robot-estop controller under a distinct name so it
// coexists with the capability-deriving Robot reconciler on the same resource.
func (r *RobotEstopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.Robot{}).
		Named("robotestop").
		Complete(r)
}

// estopTimingDetail adds the §9.6.5.1 estop timing fields to an audit detail map.
//
// Values are strings because audit.Entry.Detail is map[string]string — a flat string
// map keeps the hash chain free of number-formatting ambiguity (§9.6.5.2). Timestamps
// use RFC3339 with nanoseconds so a sub-millisecond round trip is not rounded away.
func estopTimingDetail(detail map[string]string, sentAt time.Time, res safety.Result) map[string]string {
	detail["rpc_sent_at"] = sentAt.UTC().Format(time.RFC3339Nano)
	detail["delivered"] = strconv.FormatBool(res.Delivered)
	if res.Delivered {
		detail["ack_received_at"] = sentAt.Add(res.Latency).UTC().Format(time.RFC3339Nano)
		detail["ack_latency_ms"] = strconv.FormatInt(res.Latency.Milliseconds(), 10)
		detail["latency_violation"] = strconv.FormatBool(res.LatencyViolation)
	}
	return detail
}

// clock is the reconciler's time source; tests substitute it so an audit entry's
// timestamps are deterministic.
func (r *RobotEstopReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
