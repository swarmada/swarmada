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
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/contract"
)

// swarmadaConfigResyncInterval is how often an unconfigured-sink SwarmadaConfig
// is re-reconciled so the TelemetrySinkUnconfigured condition and its Warning
// event keep surfacing until an operator resolves them (RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split
// Invariant 1: the signal repeats "every reconciliation cycle until resolved").
const swarmadaConfigResyncInterval = 5 * time.Minute

// SwarmadaConfigReconciler surfaces telemetry-pipeline health on the namespace
// SwarmadaConfig. Today it owns the TelemetrySinkUnconfigured condition
// (RFC-0001 #swarmadaconfig-schema conditions table, RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split Invariant 1): when
// spec.telemetry.sink.type is unset the control plane is in observed-degraded
// mode and MUST say so loudly rather than dropping telemetry silently.
//
// It also owns the SigningIncomplete condition: signature verification required with
// no trust root to verify against. That state is legal (ADR-0051) and every consumer
// fails closed on it, so in a fresh namespace no adapter qualifies. The admission
// warning for it goes to whoever wrote the config, which for the auto-created config
// is the bootstrap reconciler; the condition puts it where an operator looks.
//
// It records SWARMADA_CONFIG_MODIFIED once a change to spec has been stored (see
// recordConfigModified).
//
// This reconciler writes only SwarmadaConfig.status; it never touches Robot
// status, so it is orthogonal to the RA-1 telemetry-tick discipline.
type SwarmadaConfigReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Audit records SWARMADA_CONFIG_MODIFIED into the tamper-evident chain (RFC-0001
	// #safety-required-events). Nil skips the record; the status below is unaffected.
	Audit audit.Recorder
}

// swarmadaConfigControllerActor is who SWARMADA_CONFIG_MODIFIED names: the controller that
// observed the stored change. The Kubernetes API server's audit log records who wrote it.
const swarmadaConfigControllerActor = "swarmadaconfig-controller"

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

	// SigningIncomplete rides the change-gated patch below, so an unchanged signing
	// policy writes nothing (RA-1); its message carries nothing that varies between
	// reconciles for the same spec.
	wasIncomplete := meta.IsStatusConditionTrue(original.Status.Conditions, fleetv1.ConditionSigningIncomplete)
	signing := signingIncompleteCondition(cfg)
	meta.SetStatusCondition(&cfg.Status.Conditions, signing)
	cfg.Status.ObservedGeneration = cfg.Generation

	// Advertise the contract-version range this build implements (ADR-0032). Set on every
	// reconcile — which includes the startup pass over every existing SwarmadaConfig — so it is
	// correct from boot, self-heals if an operator edits it, and lands on a config created later.
	// It rides the change-gated patch below, so an unchanged range writes nothing (RA-1).
	cfg.Status.SupportedContractRange = contract.SupportedRange()

	// A spec change not yet observed is recorded before the status write that observes it,
	// so a write that is lost records it again on the next pass.
	if from := max(original.Status.ObservedGeneration, 1); cfg.Generation > from {
		r.recordConfigModified(ctx, cfg)
	}

	if !equality.Semantic.DeepEqual(original.Status, cfg.Status) {
		if err := r.Status().Patch(ctx, cfg, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching SwarmadaConfig status: %w", err)
		}
	}

	// One Event on the transition into SigningIncomplete, not one per reconcile: the
	// condition holds the state, and this reconciler requeues while the sink is unset.
	if signing.Status == metav1.ConditionTrue && !wasIncomplete && r.Recorder != nil {
		r.Recorder.Event(cfg, corev1.EventTypeWarning, fleetv1.ConditionSigningIncomplete, signing.Message)
	}

	if unconfigured {
		// Emit the Warning event and requeue so operators keep seeing it via
		// `kubectl describe swarmadaconfig` without metrics access (RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split).
		if r.Recorder != nil {
			r.Recorder.Event(cfg, corev1.EventTypeWarning, fleetv1.ConditionTelemetrySinkUnconfigured, cond.Message)
		}
		logger.Info("telemetry sink unconfigured; namespace is in observed-degraded mode")
		return ctrl.Result{RequeueAfter: swarmadaConfigResyncInterval}, nil
	}
	return ctrl.Result{}, nil
}

// recordConfigModified writes SWARMADA_CONFIG_MODIFIED for the stored spec changes this
// reconciler has not yet observed: an entry records a change the API server stored, so a
// server-side dry run, or an update the API server refused, records nothing. Generation 1 is
// the create, which is not a modification. Edits stored between two passes, or before this
// reconciler's first pass, are one entry, and a status write that is lost records them again
// (at least once).
//
// Best-effort: a sink failure is logged and the status write goes on, so an unreachable
// audit sink does not hold the conditions below.
func (r *SwarmadaConfigReconciler) recordConfigModified(ctx context.Context, cfg *fleetv1.SwarmadaConfig) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventSwarmadaConfigMod,
		Namespace: cfg.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: swarmadaConfigControllerActor},
		Resource:  audit.Resource{Kind: "SwarmadaConfig", Namespace: cfg.Namespace, Name: cfg.Name},
		Action:    "update",
		Outcome:   audit.OutcomeAllowed,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording SWARMADA_CONFIG_MODIFIED audit entry",
			"namespace", cfg.Namespace, "name", cfg.Name)
	}
}

// signingIncompleteCondition computes SigningIncomplete from cfg's signing policy.
func signingIncompleteCondition(cfg *fleetv1.SwarmadaConfig) metav1.Condition {
	cond := metav1.Condition{
		Type:               fleetv1.ConditionSigningIncomplete,
		ObservedGeneration: cfg.Generation,
	}
	sig := cfg.Spec.Signing
	switch {
	case !sig.SignatureVerificationRequired():
		cond.Status = metav1.ConditionFalse
		cond.Reason = fleetv1.ReasonVerificationDisabled
		cond.Message = "spec.signing.requireSignatureVerification is false: artifacts and conformance " +
			"reports are not signature-verified in this namespace"
	case len(sig.TrustRoots) == 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = fleetv1.ReasonNoTrustRoots
		cond.Message = "signature verification is required and spec.signing.trustRoots is empty, so " +
			"every signature check fails closed: no FleetAdapter conformance report, firmware or model " +
			"artifact can verify in this namespace. Add a trustRoot, or set " +
			"spec.signing.requireSignatureVerification: false to turn verification off deliberately"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = fleetv1.ReasonTrustRootsConfigured
		cond.Message = "signature verification is required and at least one trustRoot is configured"
	}
	return cond
}

// SetupWithManager registers the SwarmadaConfig controller.
func (r *SwarmadaConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.SwarmadaConfig{}).
		Complete(r)
}
