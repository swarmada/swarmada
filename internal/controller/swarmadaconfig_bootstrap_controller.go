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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// SwarmadaConfigName is the fixed, required name of the per-namespace singleton
// SwarmadaConfig (RFC-0001 #swarmadaconfig; the CEL rule on the type enforces it).
const SwarmadaConfigName = "swarmada-config"

// SwarmadaConfigBootstrapReconciler ensures that every namespace holding swarmada.io
// workload objects also has a swarmada-config (RFC-0001 #swarmadaconfig-auto-creation-on-namespace-initialization): "When the
// first FleetZone resource is created in a namespace, the Zone Controller checks
// for the presence of a SwarmadaConfig named swarmada-config. If absent, it
// creates one with all default values."
//
// It triggers on FleetAdapter as well as FleetZone. RFC-0001 #swarmadaconfig-auto-creation-on-namespace-initialization names the first
// FleetZone as the trigger and, in the same paragraph, states the guarantee that trigger
// is there to deliver: "This ensures every operational namespace always has a
// SwarmadaConfig, eliminating the need for controllers to implement fallback logic for
// the missing-config case." The FleetZone trigger alone does NOT deliver it. A
// FleetAdapter has no zone reference (FleetAdapterSpec, api/v1/fleetadapter_types.go),
// no admission webhook, and no CEL rule tying it to one, and FleetAdapterReconciler never
// reads a FleetZone — so a namespace can hold adapters and never get a config. That
// namespace is exactly where the missing-config fallback used to fail open (ADR-0053).
// Widening the trigger is additive to the RFC's rule and is what makes its stated
// guarantee true.
//
// RFC-0001 assigns this responsibility to the Zone Controller (RFC-0001 #control-plane-zone-controller).
// It runs as its own reconciler rather than inside ZoneController (zone_controller.go), so
// its trigger set, which includes FleetAdapter, is independent of the zone-topology
// reconciler's.
//
// It writes only a new SwarmadaConfig object (with an empty spec, so the API
// server applies the schema defaults on create); it never writes Robot status,
// so it is orthogonal to the RA-1 telemetry-tick discipline.
type SwarmadaConfigBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetadapters,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch;create

// Reconcile is triggered by FleetZone and FleetAdapter events; it needs only the
// triggering object's namespace. It creates swarmada-config if that namespace has none.
// It is idempotent: a present config is a no-op, and a create that races another writer
// is treated as success.
func (r *SwarmadaConfigBootstrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace)

	cfg := &fleetv1.SwarmadaConfig{}
	err := r.Get(ctx, types.NamespacedName{Name: SwarmadaConfigName, Namespace: req.Namespace}, cfg)
	if err == nil {
		return ctrl.Result{}, nil // already present; nothing to do
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("checking for swarmada-config: %w", err)
	}

	// Absent: create a defaulted config. An empty spec is intentional — the API
	// server fills every field from the kubebuilder defaults on create (RFC-0001 #swarmadaconfig-auto-creation-on-namespace-initialization).
	newCfg := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SwarmadaConfigName,
			Namespace: req.Namespace,
		},
	}
	if err := r.Create(ctx, newCfg); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Raced with another writer (or a prior reconcile); the invariant holds.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("auto-creating swarmada-config: %w", err)
	}
	logger.Info("auto-created swarmada-config for namespace on first FleetZone or FleetAdapter")
	return ctrl.Result{}, nil
}

// SetupWithManager registers the bootstrap reconciler against FleetZone AND FleetAdapter
// events. Both map to a bare namespace request: Reconcile reads only req.Namespace, and
// the object that triggered it is not otherwise interesting.
//
// This NARROWS the no-config window; it does not close it, and the window is reachable:
// a FleetAdapter create and the reconcile that follows it are not one transaction, so an
// adapter reconcile routinely observes the namespace before the config lands. What happens
// inside the window is now the safe answer rather than the permissive one — conformance
// fails closed, naming the missing config — so the window can only ever produce a refusal.
// FleetAdapterReconciler does not watch SwarmadaConfig and self-requeues only while
// Connected or Degraded, so that refusal clears on the adapter's next event rather than
// when the config appears. See ADR-0053.
func (r *SwarmadaConfigBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetZone{}).
		Watches(&fleetv1.FleetAdapter{}, handler.EnqueueRequestsFromMapFunc(bootstrapNamespaceRequest)).
		Complete(r)
}

// bootstrapNamespaceRequest maps any triggering object to the swarmada-config in its own
// namespace. The name is fixed rather than the object's, so every adapter in a namespace
// collapses onto one queue key instead of one per adapter.
func bootstrapNamespaceRequest(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: SwarmadaConfigName},
	}}
}
