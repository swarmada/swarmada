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
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// SwarmadaConfigName is the fixed, required name of the per-namespace singleton
// SwarmadaConfig (RFC-0001 §9.1.11; the CEL rule on the type enforces it).
const SwarmadaConfigName = "swarmada-config"

// SwarmadaConfigBootstrapReconciler ensures that every namespace containing at
// least one FleetZone also has a swarmada-config (RFC-0001 §9.1.11.3): "When the
// first FleetZone resource is created in a namespace, the Zone Controller checks
// for the presence of a SwarmadaConfig named swarmada-config. If absent, it
// creates one with all default values."
//
// This is the config-bootstrap slice of the Zone Controller (§9.3.4), which is
// not yet built (backlog §D); when the Zone Controller lands this responsibility
// folds into it. Splitting it out now lets the guarantee exist without waiting
// for the full zone-topology controller.
//
// It writes only a new SwarmadaConfig object (with an empty spec, so the API
// server applies the schema defaults on create); it never writes Robot status,
// so it is orthogonal to the RA-1 telemetry-tick discipline.
type SwarmadaConfigBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch;create

// Reconcile is triggered by FleetZone events; it needs only the zone's namespace.
// It creates swarmada-config if that namespace has none. It is idempotent: a
// present config is a no-op, and a create that races another writer is treated as
// success.
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
	// server fills every field from the kubebuilder defaults on create (§9.1.11.3).
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
	logger.Info("auto-created swarmada-config for namespace on first FleetZone")
	return ctrl.Result{}, nil
}

// SetupWithManager registers the bootstrap reconciler against FleetZone events.
func (r *SwarmadaConfigBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetZone{}).
		Complete(r)
}
