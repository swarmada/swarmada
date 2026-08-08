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

package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-zonemaintenance,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=zonemaintenances,verbs=create;update,versions=v1,name=vzonemaintenance.swarmada.io,admissionReviewVersions=v1

var zoneMaintenanceGK = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "ZoneMaintenance"}

// ZoneMaintenanceValidator enforces the cross-resource invariant that per-field CEL
// cannot express (RFC-0001 §9.3.1): when spec.scope.type is Zone, spec.scope.zoneName
// must name a FleetZone that exists in the same namespace.
//
// Why this is worth rejecting at admission rather than reporting later: a ZoneMaintenance
// naming a zone that does not exist resolves to an EMPTY robot set, and an empty set is
// indistinguishable at a glance from a window that is working and has nothing to pause.
// The resource reaches phase Active, `status.pausedRobots` stays empty, and the operator
// reads that as "the zone is quiet" — while every robot they meant to stop keeps working.
// A technician entering that zone is the hazard this catches. §9.6.1 is explicit that
// ZoneMaintenance does not create a physically safe zone, but an operator relying on it as
// a precondition should at least be told the window targets nothing.
//
// It runs failurePolicy=Fail: a ZoneMaintenance that cannot be validated is not admitted.
type ZoneMaintenanceValidator struct {
	Client client.Client
}

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *ZoneMaintenanceValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.ZoneMaintenance{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &ZoneMaintenanceValidator{}

// ValidateCreate resolves the scope on creation.
func (v *ZoneMaintenanceValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	zm, ok := obj.(*fleetv1.ZoneMaintenance)
	if !ok {
		return nil, fmt.Errorf("expected a ZoneMaintenance object but got %T", obj)
	}
	return v.validate(ctx, zm)
}

// ValidateUpdate re-resolves the scope: an update may re-point zoneName, and a window
// that is already Active is exactly the one where a dangling target matters most.
func (v *ZoneMaintenanceValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	zm, ok := newObj.(*fleetv1.ZoneMaintenance)
	if !ok {
		return nil, fmt.Errorf("expected a ZoneMaintenance object but got %T", newObj)
	}
	return v.validate(ctx, zm)
}

// ValidateDelete allows deletion. Deleting a ZoneMaintenance resumes its paused robots
// through the resume finalizer (§9.1.10); refusing the delete would strand them paused.
func (v *ZoneMaintenanceValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ZoneMaintenanceValidator) validate(ctx context.Context, zm *fleetv1.ZoneMaintenance) (admission.Warnings, error) {
	// Namespace-scoped windows target every robot in the namespace and carry no zoneName
	// to resolve. Only the Zone form has a cross-object reference.
	if zm.Spec.Scope.Type != fleetv1.MaintenanceScopeZone {
		return nil, nil
	}
	zonePath := field.NewPath("spec").Child("scope").Child("zoneName")
	name := zm.Spec.Scope.ZoneName
	if name == "" {
		return nil, apierrors.NewInvalid(zoneMaintenanceGK, zm.Name, field.ErrorList{
			field.Required(zonePath, "zoneName is required when scope.type is Zone"),
		})
	}

	var z fleetv1.FleetZone
	if err := v.Client.Get(ctx, client.ObjectKey{Namespace: zm.Namespace, Name: name}, &z); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierrors.NewInvalid(zoneMaintenanceGK, zm.Name, field.ErrorList{
				field.Invalid(zonePath, name,
					fmt.Sprintf("FleetZone %q does not exist in namespace %q", name, zm.Namespace)),
			})
		}
		// Fail closed. An unreadable zone is not a missing one, and admitting on a read
		// error is how a maintenance window that targets nothing gets created during the
		// exact API-server trouble an operator is most likely to be reacting to.
		return nil, fmt.Errorf("resolving spec.scope.zoneName %q: %w", name, err)
	}

	// A parent zone is deliberately allowed: §9.1.10 resolves a Zone scope to the target
	// zone AND its descendants, so maintaining a whole branch is a supported intent — unlike
	// Robot.spec.zone, which places one robot and must therefore be a leaf.
	return nil, nil
}
