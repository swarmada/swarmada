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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// NamespaceEstopReconciler drives NAMESPACE-scope emergency stops (RFC-0001 §9.6.2
// estop scopes): a `swarmada.io/estop-triggered` annotation on the namespace's
// SwarmadaConfig singleton confirmed-estops every robot in the namespace via the
// same primitive the zone/robot scopes use; removing the annotation (the operator-
// authorized estop-clear verb) resets them. It reuses the annEstopTriggered/
// annEstopProcessed idempotency markers and is SAR-gated at admission by the
// SwarmadaConfig webhook (§F-2b).
type NamespaceEstopReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Estopper issues the confirmed per-robot estop. Nil disables namespace estop.
	Estopper ZoneEstopper
	// Audit records the ESTOP_TRIGGERED / ESTOP_CLEARED safety event. Nil skips it.
	Audit audit.Recorder
}

// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch;update;patch

// Reconcile fans a namespace estop out to every robot when the trigger annotation
// is (newly) present on the SwarmadaConfig, and clears it when the annotation is
// removed.
func (r *NamespaceEstopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("swarmadaconfig", req.NamespacedName)

	cfg := &fleetv1.SwarmadaConfig{}
	if err := r.Get(ctx, req.NamespacedName, cfg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Estopper == nil {
		return ctrl.Result{}, nil
	}
	trigger, firing := cfg.Annotations[annEstopTriggered]
	processed, wasProcessed := cfg.Annotations[annEstopProcessed]

	switch {
	case firing && processed == trigger:
		return ctrl.Result{}, nil // this exact estop already fanned out
	case !firing && wasProcessed:
		return r.clearNamespaceEstop(ctx, req.Namespace, cfg)
	case !firing:
		return ctrl.Result{}, nil // no estop active
	}

	reason := trigger
	if reason == "" || reason == "true" {
		reason = "namespace emergency stop"
	}
	issuedBy := "namespace-estop:" + req.Namespace

	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing robots: %w", err)
	}
	stopped, failed := 0, 0
	// See the zone controller: robots_in_scope and the worst ack latency are the
	// fan-out-meaningful half of §9.6.5.1's estop fields; per-robot rpc_sent_at /
	// ack_received_at live on the robot-scope entries.
	inScope := make([]string, 0, len(robots.Items))
	var worstLatency time.Duration
	for i := range robots.Items {
		inScope = append(inScope, robots.Items[i].Name)
		res, terr := r.Estopper.TriggerEstop(ctx, req.Namespace, robots.Items[i].Name, reason, issuedBy)
		if res.Delivered && res.Latency > worstLatency {
			worstLatency = res.Latency
		}
		if terr != nil || res.State != fleetv1.RobotEstopStopped {
			failed++
			logger.Info("namespace estop not confirmed for robot (escalate)", "robot", robots.Items[i].Name,
				"state", res.State, "err", errString(terr))
			continue
		}
		stopped++
	}
	logger.Info("namespace estop fanned out", "namespace", req.Namespace, "reason", reason,
		"robots", len(robots.Items), "stopped", stopped, "unconfirmed", failed)

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
			Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: issuedBy},
			Resource:  audit.Resource{Kind: "SwarmadaConfig", Namespace: req.Namespace, Name: cfg.Name},
			Detail: map[string]string{
				"reason":             reason,
				"robots_in_scope":    strings.Join(inScope, ","),
				"robots":             itoa(len(robots.Items)),
				"stopped":            itoa(stopped),
				"unconfirmed":        itoa(failed),
				"max_ack_latency_ms": itoa(int(worstLatency.Milliseconds())),
			},
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_TRIGGERED audit entry")
		}
	}

	base := cfg.DeepCopy()
	if cfg.Annotations == nil {
		cfg.Annotations = map[string]string{}
	}
	cfg.Annotations[annEstopProcessed] = trigger
	if err := r.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording namespace estop processed: %w", err)
	}
	return ctrl.Result{}, nil
}

// clearNamespaceEstop resets every robot in the namespace to Normal after the
// operator removes the trigger annotation. Idempotent — a robot not under an estop
// is a no-op; a action Paused by the estop stays operator-gated (§9.6.2.4).
func (r *NamespaceEstopReconciler) clearNamespaceEstop(ctx context.Context, namespace string, cfg *fleetv1.SwarmadaConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	clearedBy := "namespace-estop:" + namespace

	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing robots: %w", err)
	}
	cleared := 0
	for i := range robots.Items {
		if _, cerr := r.Estopper.ClearEstop(ctx, namespace, robots.Items[i].Name, clearedBy); cerr != nil {
			logger.Error(cerr, "clearing robot estop", "robot", robots.Items[i].Name)
			continue
		}
		cleared++
	}
	logger.Info("namespace estop cleared", "namespace", namespace, "robots", len(robots.Items))

	if r.Audit != nil {
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: namespace,
			EventType: audit.EventEstopCleared,
			Action:    "estop-clear",
			Outcome:   audit.OutcomeAllowed,
			Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: clearedBy},
			Resource:  audit.Resource{Kind: "SwarmadaConfig", Namespace: namespace, Name: cfg.Name},
			Detail:    map[string]string{"robots": itoa(len(robots.Items)), "cleared": itoa(cleared)},
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_CLEARED audit entry")
		}
	}

	base := cfg.DeepCopy()
	delete(cfg.Annotations, annEstopProcessed)
	if err := r.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing namespace estop processed marker: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the namespace-estop controller under a distinct name so
// it coexists with the other SwarmadaConfig reconcilers.
func (r *NamespaceEstopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.SwarmadaConfig{}).
		Named("namespaceestop").
		Complete(r)
}
