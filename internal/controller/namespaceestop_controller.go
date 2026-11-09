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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/metrics"
)

// NamespaceEstopReconciler drives NAMESPACE-scope emergency stops (RFC-0001 #safety-emergency-stop-protocol
// estop scopes): a `swarmada.io/estop-triggered` annotation on the namespace's
// SwarmadaConfig singleton confirmed-estops every robot in the namespace via the
// same primitive the zone/robot scopes use; removing the annotation (the operator-
// authorized estop-clear verb) resets them. It reuses the annEstopTriggered/
// annEstopProcessed idempotency markers and is SAR-gated at admission by the
// SwarmadaConfig webhook (RFC-0001 #security-authorization-swarmada-rbac-roles).
//
// The stop is recorded on each robot it is issued to (Robot.status.estopHeldBy) and the
// record is removed only by the namespace's clear. A Robot created in the namespace while
// its stop is in force was never in the fan-out: the controller watches Robot creates, and
// estopState changes, and stops every robot the stop has not reached through the same
// primitive (holdLateArrivals), and the dispatch choke point withholds every robot in the
// namespace while the stop is in force (dispatchHolds.namespaceStop).
type NamespaceEstopReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Estopper issues the confirmed per-robot estop. Nil disables namespace estop.
	Estopper ZoneEstopper
	// Audit records the ESTOP_TRIGGERED / ESTOP_CLEARED safety event. Nil skips it.
	Audit audit.Recorder
	// Recorder emits the EstopHeldByOwnTrigger Event on a robot a clear leaves stopped.
	// Nil skips it.
	Recorder record.EventRecorder
	// APIReader reads the API server directly, bypassing the manager's cache, for the hold
	// records and the clear's robot list (see ZoneEstopReconciler.APIReader). Nil falls
	// back to Client.
	APIReader client.Reader
	// markers is the processed marker this process last wrote or removed on the namespace's config.
	markers writtenMarkers
}

// reader is the uncached reader when one is wired, else the client.
func (r *NamespaceEstopReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch

// Reconcile fans a namespace estop out to every robot when the trigger annotation
// is (newly) present on the SwarmadaConfig, and clears it when the annotation is
// removed.
func (r *NamespaceEstopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cfg := &fleetv1.SwarmadaConfig{}
	if err := r.Get(ctx, req.NamespacedName, cfg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return r.reconcileNamespace(ctx, req, cfg)
}

// reconcileNamespace is the pass on cfg as it was read: from the cache, or from the API server
// when a clear finds a trigger written while it ran.
func (r *NamespaceEstopReconciler) reconcileNamespace(ctx context.Context, req ctrl.Request, cfg *fleetv1.SwarmadaConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("swarmadaconfig", req.NamespacedName)
	if r.Estopper == nil {
		return ctrl.Result{}, nil
	}
	trigger, firing := cfg.Annotations[annEstopTriggered]
	processed, wasProcessed := cfg.Annotations[annEstopProcessed]
	if r.markers.behind(req.NamespacedName, trigger, firing, processed, wasProcessed) {
		// As in the zone controller: the cache is behind a marker this process wrote or removed.
		fresh := &fleetv1.SwarmadaConfig{}
		if err := r.reader().Get(ctx, req.NamespacedName, fresh); err == nil {
			cfg = fresh
			trigger, firing = cfg.Annotations[annEstopTriggered]
			processed, wasProcessed = cfg.Annotations[annEstopProcessed]
		}
	}

	// The stop finalizer: held from the trigger until the clear has been applied, so a config
	// deleted while no replica serves the delete check stays until the stop is applied and
	// cleared (estopscope.StopFinalizer). A namespace being torn down releases it.
	cfg, again, done, err := holdStopFinalizer(ctx, r.Client, r.reader(), cfg, carrierHoldsStop[*fleetv1.SwarmadaConfig])
	if done || err != nil {
		return ctrl.Result{}, err
	}
	if again {
		return r.reconcileNamespace(ctx, req, cfg)
	}

	switch {
	case firing && wasProcessed && processed == trigger:
		// This exact estop already fanned out (the marker's presence is compared as well as
		// its value: an absent marker reads as "", and an empty trigger is a stop like any
		// other). A Robot created in the namespace since is stopped now, and the robots it
		// covers are re-evaluated on a timer as well as on events.
		return ctrl.Result{RequeueAfter: stopInForceResync}, r.holdLateArrivals(ctx, req.Namespace, cfg, trigger)
	case !firing && wasProcessed:
		r.markers.forget(req.NamespacedName)
		return r.clearNamespaceEstop(ctx, req, cfg)
	case !firing:
		return ctrl.Result{}, nil // no estop active
	}

	reason := estopReason(trigger, "namespace emergency stop")
	actor := estopActor(cfg.Annotations, "namespace-estop:"+req.Namespace)
	issuedBy := actor.Identity

	var robots fleetv1.RobotList
	if err := r.reader().List(ctx, &robots, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing robots: %w", err)
	}
	stopped, failed := 0, 0
	// See the zone controller: robots_in_scope and the worst ack latency are the
	// fan-out-meaningful half of RFC-0001 #safety-required-events's estop fields; per-robot rpc_sent_at /
	// ack_received_at live on the robot-scope entries.
	inScope := make([]string, 0, len(robots.Items))
	var worstLatency time.Duration
	ref := configHold(cfg)
	// Issued in PARALLEL (RFC-0001 #safety-estop-scopes); see estopFanout. A namespace estop is the widest
	// fan-out there is, so it is where sequential dispatch cost the most.
	outcomes, fanout := estopFanoutHeld(ctx, r.Estopper, req.Namespace, robots.Items, reason, issuedBy,
		metrics.ScopeNamespace, carrierOf(ref), func(ctx context.Context, robot string) error {
			return recordHoldAndWithdraw(ctx, r.Client, r.reader(), req.Namespace, robot, ref)
		})
	unwritten := 0
	for _, o := range outcomes {
		inScope = append(inScope, o.robot)
		if o.result.Delivered && o.result.Latency > worstLatency {
			worstLatency = o.result.Latency
		}
		if outcomeUnwritten(o) {
			unwritten++
			logger.Info("namespace estop outcome or hold record not written for robot (retrying)", "robot", o.robot,
				"state", o.result.State, "err", errString(o.err), "recordErr", errString(o.recErr))
		}
		if o.err != nil || o.result.State != fleetv1.RobotEstopStopped {
			failed++
			logger.Info("namespace estop not confirmed for robot (escalate)", "robot", o.robot,
				"state", o.result.State, "err", errString(o.err))
			continue
		}
		stopped++
	}
	metrics.ObserveEstopFanout(req.Namespace, metrics.ScopeNamespace, fanout)
	logger.Info("namespace estop fanned out", "namespace", req.Namespace, "reason", reason,
		"robots", len(robots.Items), "stopped", stopped, "unconfirmed", failed, "fanout", fanout)

	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if failed > 0 {
			outcome = audit.OutcomeError
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: req.Namespace,
			EventType: audit.EventEstopTriggered,
			Action:    "estop-trigger",
			Outcome:   outcome,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "SwarmadaConfig", Namespace: req.Namespace, Name: cfg.Name},
			Detail: map[string]string{
				"reason":             reason,
				"robots_in_scope":    strings.Join(inScope, ","),
				"robots":             itoa(len(robots.Items)),
				"stopped":            itoa(stopped),
				"unconfirmed":        itoa(failed),
				"unwritten":          itoa(unwritten),
				"max_ack_latency_ms": itoa(int(worstLatency.Milliseconds())),
			},
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_TRIGGERED audit entry")
		}
	}

	// A robot whose outcome or hold record was not written is not recorded as reached: the
	// marker waits and the requeue fans out again (RFC-0001 #safety-estop-state-machine).
	if unwritten > 0 {
		return ctrl.Result{}, fmt.Errorf("namespace %s estop: the outcome or hold record of %d robot(s) was not written; "+
			"fanning out again", req.Namespace, unwritten)
	}

	base := cfg.DeepCopy()
	if cfg.Annotations == nil {
		cfg.Annotations = map[string]string{}
	}
	cfg.Annotations[annEstopProcessed] = trigger
	consumeEstopActor(cfg.Annotations)
	if err := r.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording namespace estop processed: %w", err)
	}
	r.markers.wrote(req.NamespacedName, trigger)
	return ctrl.Result{}, nil
}

// clearNamespaceEstop moves every estopped robot in the namespace to Resuming after the
// operator removes the trigger annotation. Idempotent — a robot not under an estop
// is a no-op; a action Paused by the estop stays operator-gated (RFC-0001 #safety-fleetaction-behaviour-during-estop).
// A robot another stop still holds is not cleared: its own robot-scope trigger, or a
// zone's stop that holds it. A clear of one scope is not the clear of another (RFC-0001
// #safety-estop-scopes); the clear of that stop releases it. Each robot's record of the
// namespace's stop is removed, and the marker is dropped only once every write has landed;
// a failed write leaves it, and the clear is retried. A trigger written while the clear ran is
// kept, and the pass runs again on it as a new stop (dropClearedMarker).
func (r *NamespaceEstopReconciler) clearNamespaceEstop(ctx context.Context, req ctrl.Request, cfg *fleetv1.SwarmadaConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespace := req.Namespace
	actor := estopActor(cfg.Annotations, "namespace-estop:"+namespace)
	clearedBy := actor.Identity

	var robots fleetv1.RobotList
	if err := r.reader().List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing robots: %w", err)
	}
	zones, configs, err := readStandingStops(ctx, r.reader(), namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	cleared, held, cerr := clearCarrier(ctx, r.Client, r.reader(), r.Estopper, r.Recorder, namespace, configHold(cfg),
		robots.Items, zones, configs, clearedBy, "namespace "+namespace)
	logger.Info("namespace estop cleared", "namespace", namespace, "robots", len(robots.Items),
		"heldByOwnEstop", held.own, "heldByWiderEstop", held.wider, "err", errString(cerr))

	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if cerr != nil {
			outcome = audit.OutcomeError
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: namespace,
			EventType: audit.EventEstopCleared,
			Action:    "estop-clear",
			Outcome:   outcome,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "SwarmadaConfig", Namespace: namespace, Name: cfg.Name},
			Detail:    held.detail(map[string]string{"robots": itoa(len(robots.Items)), "cleared": itoa(cleared)}),
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_CLEARED audit entry")
		}
	}
	if cerr != nil {
		return ctrl.Result{}, fmt.Errorf("namespace %s estop clear: %w", namespace, cerr)
	}

	removedMarker := cfg.Annotations[annEstopProcessed]
	cfg, again, err := dropClearedMarker(ctx, r.Client, r.reader(), cfg,
		func(*fleetv1.SwarmadaConfig) (bool, error) { return true, nil })
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing namespace estop processed marker: %w", err)
	}
	r.markers.removed(client.ObjectKeyFromObject(cfg), removedMarker)
	if again {
		// A trigger written while this clear ran: its stop is issued now.
		return r.reconcileNamespace(ctx, req, cfg)
	}
	return ctrl.Result{}, nil
}

// namespaceLateArrivalActor is who the audit entry for a late arrival's stop names, as
// lateArrivalActor does for a zone.
const namespaceLateArrivalActor = "namespaceestop-controller"

// holdLateArrivals brings every Robot in the namespace that the stop has not reached
// (inForceTargets): a robot created in the namespace (by a fleet-manager, or admitted from
// discovery) after the fan-out, and a robot that records the stop but whose stop state was
// never written (the write failed). A robot already in a stop state is only recorded, which
// is also how a robot stopped before the record existed gets one on the first reconcile of a
// stop in force.
//
// Each goes through the confirmed per-robot primitive, as the fan-out does: Stopped on a
// confirmed acknowledgement, Failed when it cannot be confirmed, never Normal. Both hold the
// robot, and it leaves as the robots the fan-out reached do: through the namespace's clear
// and an operator's confirmed resume (RFC-0001 #safety-estop-state-machine). Nothing here
// writes a stop state itself, and nothing is remembered between passes but the robot's own
// record: a robot is tried until its stop state and its record are both written, and any
// failed write is returned so the pass is retried.
func (r *NamespaceEstopReconciler) holdLateArrivals(ctx context.Context, namespace string,
	cfg *fleetv1.SwarmadaConfig, trigger string) error {
	ref := configHold(cfg)
	sent := sentUnrecorded(r.Estopper, namespace, ref)
	var robots fleetv1.RobotList
	if err := r.reader().List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing robots: %w", err)
	}
	late, recordOnly := inForceTargets(ref, robots.Items, robots.Items, sent)
	var errs []error
	for i := range recordOnly {
		if err := recordHoldAndWithdraw(ctx, r.Client, r.reader(), namespace, recordOnly[i].Name, ref); err != nil {
			errs = append(errs, err)
		}
	}
	if len(late) == 0 {
		return errors.Join(errs...)
	}
	reason := fmt.Sprintf("%s (namespace %s; the robot arrived after the stop)",
		estopReason(trigger, "namespace emergency stop"), namespace)
	outcomes, fanout := estopFanoutHeld(ctx, r.Estopper, namespace, late, reason, namespaceLateArrivalActor,
		metrics.ScopeNamespace, carrierOf(ref), func(ctx context.Context, robot string) error {
			return recordHoldAndWithdraw(ctx, r.Client, r.reader(), namespace, robot, ref)
		})
	names := make([]string, 0, len(outcomes))
	stopped, failed, unwritten := 0, 0, 0
	for _, o := range outcomes {
		names = append(names, o.robot)
		if outcomeUnwritten(o) {
			unwritten++
		}
		if o.err != nil || o.result.State != fleetv1.RobotEstopStopped {
			failed++
			continue
		}
		stopped++
	}
	log.FromContext(ctx).Info("namespace estop in force: stopped robots that arrived after it",
		"namespace", namespace, "robots", names, "stopped", stopped, "unconfirmed", failed, "unwritten", unwritten,
		"fanout", fanout)
	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if failed > 0 {
			outcome = audit.OutcomeError
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: namespace,
			EventType: audit.EventEstopTriggered,
			Action:    "estop-trigger",
			Outcome:   outcome,
			Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: namespaceLateArrivalActor},
			Resource:  audit.Resource{Kind: "SwarmadaConfig", Namespace: namespace, Name: cfg.Name},
			Detail: map[string]string{
				"reason":          reason,
				"robots_in_scope": strings.Join(names, ","),
				"robots":          itoa(len(late)),
				"stopped":         itoa(stopped),
				"unconfirmed":     itoa(failed),
				"unwritten":       itoa(unwritten),
				"late_arrival":    "true",
			},
		}); aerr != nil {
			log.FromContext(ctx).Error(aerr, "recording ESTOP_TRIGGERED audit entry for late arrivals")
		}
	}
	if unwritten > 0 {
		errs = append(errs, fmt.Errorf("namespace %s estop: the outcome or hold record of %d late robot(s) was not "+
			"written; retrying", namespace, unwritten))
	}
	return errors.Join(errs...)
}

// configsOfRobot maps a Robot's create to its namespace's SwarmadaConfig, whose estop, when
// in force, then stops it (holdLateArrivals). A list that fails falls back to the singleton's
// fixed name, so a create is never dropped for a read error.
func (r *NamespaceEstopReconciler) configsOfRobot(ctx context.Context, obj client.Object) []reconcile.Request {
	var list fleetv1.SwarmadaConfigList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(), Name: SwarmadaConfigName}}}
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace, Name: list.Items[i].Name}})
	}
	return out
}

// robotCreated admits a Robot's create, the only event that brings a robot into a
// namespace, and an update that changes its estopState, which can show that a stop it
// records was never written.
var robotCreated = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*fleetv1.Robot)
		n, ok2 := e.ObjectNew.(*fleetv1.Robot)
		return ok1 && ok2 && o.Status.EstopState != n.Status.EstopState
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// SetupWithManager registers the namespace-estop controller under a distinct name so
// it coexists with the other SwarmadaConfig reconcilers. A Robot created in the namespace
// reconciles its SwarmadaConfig, so a stop in force reaches it (holdLateArrivals).
func (r *NamespaceEstopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.SwarmadaConfig{}).
		Watches(&fleetv1.Robot{}, handler.EnqueueRequestsFromMapFunc(r.configsOfRobot),
			builder.WithPredicates(robotCreated)).
		Named("namespaceestop").
		Complete(r)
}
