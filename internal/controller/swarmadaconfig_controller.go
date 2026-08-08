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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
)

// swarmadaConfigResyncInterval is how often an unconfigured-sink SwarmadaConfig
// is re-reconciled so the TelemetrySinkUnconfigured condition and its Warning
// event keep surfacing until an operator resolves them (RFC-0001 §9.3.7
// Invariant 1: the signal repeats "every reconciliation cycle until resolved").
const swarmadaConfigResyncInterval = 5 * time.Minute

// SwarmadaConfigReconciler surfaces telemetry-pipeline health on the namespace
// SwarmadaConfig. Today it owns the TelemetrySinkUnconfigured condition
// (RFC-0001 §9.1.11.1 conditions table, §9.3.7 Invariant 1): when
// spec.telemetry.sink.type is unset the control plane is in observed-degraded
// mode and MUST say so loudly rather than dropping telemetry silently.
//
// This reconciler writes only SwarmadaConfig.status; it never touches Robot
// status, so it is orthogonal to the RA-1 telemetry-tick discipline.
type SwarmadaConfigReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs/status,verbs=get;update;patch

// Reconcile evaluates spec.telemetry.sink.type and sets the
// TelemetrySinkUnconfigured condition accordingly.
func (r *SwarmadaConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("swarmadaconfig", req.NamespacedName)

	cfg := &fleetv1.SwarmadaConfig{}
	if err := r.Get(ctx, req.NamespacedName, cfg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	original := cfg.DeepCopy()

	// The empty sink type is the "unconfigured" signal; Drop and every real store
	// are "configured" (Drop is the operator's informed opt-in to discard).
	unconfigured := cfg.Spec.Telemetry.Sink.Type == fleetv1.TelemetrySinkUnset

	cond := metav1.Condition{
		Type:               fleetv1.ConditionTelemetrySinkUnconfigured,
		ObservedGeneration: cfg.Generation,
	}
	if unconfigured {
		cond.Status = metav1.ConditionTrue
		cond.Reason = fleetv1.ReasonSinkNotConfigured
		cond.Message = "spec.telemetry.sink.type is unset; high-cadence telemetry is not being forwarded " +
			"(observed-degraded). Set a real store or explicitly opt in with type: Drop."
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = fleetv1.ReasonSinkConfigured
		cond.Message = fmt.Sprintf("telemetry sink configured (type: %s)", cfg.Spec.Telemetry.Sink.Type)
	}
	meta.SetStatusCondition(&cfg.Status.Conditions, cond)
	cfg.Status.ObservedGeneration = cfg.Generation

	// Advertise the contract-version range this build implements (ADR-0032). Set on every
	// reconcile — which includes the startup pass over every existing SwarmadaConfig — so it is
	// correct from boot, self-heals if an operator edits it, and lands on a config created later.
	// It rides the change-gated patch below, so an unchanged range writes nothing (RA-1).
	cfg.Status.SupportedContractRange = contract.SupportedRange()

	if !equality.Semantic.DeepEqual(original.Status, cfg.Status) {
		if err := r.Status().Patch(ctx, cfg, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching SwarmadaConfig status: %w", err)
		}
	}

	if unconfigured {
		// Emit the Warning event and requeue so operators keep seeing it via
		// `kubectl describe swarmadaconfig` without metrics access (§9.3.7).
		if r.Recorder != nil {
			r.Recorder.Event(cfg, corev1.EventTypeWarning, fleetv1.ConditionTelemetrySinkUnconfigured, cond.Message)
		}
		logger.Info("telemetry sink unconfigured; namespace is in observed-degraded mode")
		return ctrl.Result{RequeueAfter: swarmadaConfigResyncInterval}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the SwarmadaConfig controller.
func (r *SwarmadaConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.SwarmadaConfig{}).
		Complete(r)
}
