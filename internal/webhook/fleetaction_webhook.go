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

// fleetTaskGR is the GroupResource used for the cancel-verb SubjectAccessReview
// and Forbidden responses.
var fleetTaskGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "fleetactions"}

const (
	// cancelRequestedAnnotation is the operator cancel request the FleetAction
	// controller finalizes on. It mirrors the controller's annCancelRequested; the
	// write of this annotation is the `cancel` verb site this webhook guards.
	cancelRequestedAnnotation = "swarmada.io/cancel-requested"
	// cancelVerb is the FleetAction custom RBAC verb (§9.5.3): swarmada:operator,
	// fleet-manager, and admin hold it.
	cancelVerb = "cancel"
	// indexPendingByZone is the field-index key over FleetActions that emits a
	// zone-scoped action's spec.zone ONLY while it is Pending (ADR-0022). A
	// MatchingFields list on it returns exactly the Pending actions in a zone,
	// served from the informer cache, so the per-zone cap check is
	// O(pending-in-zone) rather than O(all-actions-in-namespace).
	indexPendingByZone = "swarmada.io/pending-zone"
)

// indexActionPendingZone is the index function for indexPendingByZone. It emits
// the action's spec.zone only for a zone-scoped action whose phase is Pending
// (or unset — a freshly created action still occupies the pending queue), and an
// empty slice otherwise, so an action drops out of the index as its cached phase
// leaves Pending.
func indexActionPendingZone(obj client.Object) []string {
	action, ok := obj.(*fleetv1.FleetAction)
	if !ok || action.Spec.Zone == "" {
		return nil
	}
	switch action.Status.Phase {
	case "", fleetv1.ActionPhasePending:
		return []string{action.Spec.Zone}
	default:
		return nil
	}
}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-fleetaction,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=fleetactions,verbs=create;update,versions=v1,name=vfleetaction.swarmada.io,admissionReviewVersions=v1
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// FleetActionValidator enforces the `cancel` custom verb (§9.5.3) at the admission
// site where the cancel is requested — writing the swarmada.io/cancel-requested
// annotation. Enforcing here (not only in the swarmctl verb) means a raw
// `kubectl annotate` cannot cancel a action without the `cancel` grant. It runs
// failurePolicy=Fail and fails CLOSED on any uncertainty.
type FleetActionValidator struct {
	// CancelAuthz authorizes the `cancel` verb when the cancel-requested annotation
	// is added. A nil authorizer makes any cancel-request fail closed — enforcement
	// is never bypassed by misconfiguration.
	CancelAuthz VerbAuthorizer
	// Reader lists FleetAdapters to pre-filter a FleetAction against the connected
	// adapters' advertised action catalog (§9.2). Nil disables the pre-filter (the
	// authoritative check then happens at assignment).
	Reader client.Reader
}

// SetupWebhookWithManager registers the validator with the manager's webhook
// server and installs the Pending-by-zone field index the per-zone cap check
// reads (ADR-0022). The index is registered on the shared cache the webhook's
// Reader consumes, so the count is served without a full namespace scan.
func (v *FleetActionValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&fleetv1.FleetAction{}, indexPendingByZone, indexActionPendingZone); err != nil {
		return err
	}
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.FleetAction{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &FleetActionValidator{}

// ValidateCreate authorizes `cancel` if the action is created already cancel-requested.
func (v *FleetActionValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	action, ok := obj.(*fleetv1.FleetAction)
	if !ok {
		return nil, fmt.Errorf("expected a FleetAction object but got %T", obj)
	}
	if _, requested := action.Annotations[cancelRequestedAnnotation]; requested {
		if err := v.authorizeCancel(ctx, action); err != nil {
			return nil, err
		}
	}
	if err := v.validateSupported(ctx, action); err != nil {
		return nil, err
	}
	return nil, v.enforcePendingCap(ctx, action)
}

// enforcePendingCap rejects a create that would push the action's zone over the
// namespace's maxPendingActionsPerZone cap (ADR-0022). It applies only to a
// zone-scoped action; a zone-less (any-zone) action cannot be attributed to a
// single zone's queue and is exempt. The current Pending count comes from the
// indexPendingByZone cached field index, so the check is O(pending-in-zone).
//
// It fails OPEN on every uncertainty — no Reader, an unreadable/absent
// SwarmadaConfig, a cap of 0 (unbounded), or a list error — because the cap is
// soft backpressure, not a safety invariant, and an unreadable policy must never
// block admission. RA-1: the check is read-only and writes no status.
func (v *FleetActionValidator) enforcePendingCap(ctx context.Context, action *fleetv1.FleetAction) error {
	if v.Reader == nil || action.Spec.Zone == "" {
		return nil
	}
	limit := v.pendingCap(ctx, action.Namespace)
	if limit <= 0 {
		return nil
	}
	var pending fleetv1.FleetActionList
	if err := v.Reader.List(ctx, &pending,
		client.InNamespace(action.Namespace),
		client.MatchingFields{indexPendingByZone: action.Spec.Zone}); err != nil {
		return nil
	}
	// The incoming action is not yet in the cache, so len is the current count;
	// admitting one more must keep the queue at or under the cap.
	if int32(len(pending.Items)) >= limit {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("PendingActionLimitExceeded: zone %q already has %d Pending FleetAction(s), at the namespace cap of %d (SwarmadaConfig.spec.scheduling.maxPendingActionsPerZone)",
				action.Spec.Zone, len(pending.Items), limit))
	}
	return nil
}

// pendingCap resolves the namespace's per-zone Pending cap from its
// SwarmadaConfig singleton, failing safe to 0 (unbounded) on any read problem.
func (v *FleetActionValidator) pendingCap(ctx context.Context, namespace string) int32 {
	var configs fleetv1.SwarmadaConfigList
	if err := v.Reader.List(ctx, &configs, client.InNamespace(namespace)); err != nil || len(configs.Items) == 0 {
		return 0
	}
	return configs.Items[0].Spec.Scheduling.MaxPendingActionsPerZone
}

// validateSupported pre-filters a FleetAction against the action catalogs connected
// Fleet Adapters advertise (FleetAdapter.status.supportedActions, §9.2): if at least
// one adapter in the namespace advertises a catalog and none list the action's type,
// admission is refused. When no adapter has advertised a catalog yet (or no Reader is
// configured), it fails open — the authoritative per-instance check happens later, at
// assignment. It never blocks admission on a transient list error.
func (v *FleetActionValidator) validateSupported(ctx context.Context, action *fleetv1.FleetAction) error {
	if v.Reader == nil {
		return nil
	}
	var adapters fleetv1.FleetAdapterList
	if err := v.Reader.List(ctx, &adapters, client.InNamespace(action.Namespace)); err != nil {
		return nil
	}
	anyCatalog := false
	for i := range adapters.Items {
		cat := adapters.Items[i].Status.SupportedActions
		if len(cat) == 0 {
			continue
		}
		anyCatalog = true
		for _, sa := range cat {
			if sa.ActionType == string(action.Spec.Type) {
				return nil
			}
		}
	}
	if anyCatalog {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("action type %q is not advertised by any connected Fleet Adapter in namespace %q", action.Spec.Type, action.Namespace))
	}
	return nil
}

// ValidateUpdate authorizes `cancel` when the cancel-requested annotation is newly
// added (absent → present). Re-valuing an existing request or any other update is
// not a new cancel and is not gated.
func (v *FleetActionValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newAction, ok := newObj.(*fleetv1.FleetAction)
	if !ok {
		return nil, fmt.Errorf("expected a FleetAction object but got %T", newObj)
	}
	oldAction, ok := oldObj.(*fleetv1.FleetAction)
	if !ok {
		return nil, fmt.Errorf("expected a FleetAction object but got %T", oldObj)
	}
	// spec.zone is immutable after creation (ADR-0022): the controller keys TDE
	// reservation acquire AND release on it, so a mid-flight re-target releases the
	// slot against the wrong zone — leaking the original zone's reservation — and
	// also slips past the create-time per-zone Pending cap. Re-targeting is a
	// cancel-and-recreate, never an in-place edit.
	if oldAction.Spec.Zone != newAction.Spec.Zone {
		return nil, apierrors.NewInvalid(
			fleetv1.GroupVersion.WithKind("FleetAction").GroupKind(),
			newAction.Name,
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "zone"),
				fmt.Sprintf("spec.zone is immutable (was %q, requested %q); cancel and recreate to re-target an action to a different zone",
					oldAction.Spec.Zone, newAction.Spec.Zone),
			)},
		)
	}
	_, hadCancel := oldAction.Annotations[cancelRequestedAnnotation]
	_, hasCancel := newAction.Annotations[cancelRequestedAnnotation]
	if !hadCancel && hasCancel {
		return nil, v.authorizeCancel(ctx, newAction)
	}
	return nil, nil
}

// ValidateDelete is a no-op — deletion is governed by standard RBAC on fleetactions.
func (v *FleetActionValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// authorizeCancel fails closed unless the admission user holds the `cancel` verb.
func (v *FleetActionValidator) authorizeCancel(ctx context.Context, action *fleetv1.FleetAction) error {
	if v.CancelAuthz == nil {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("cancel authorization is not configured; refusing to cancel FleetAction %q", action.Name))
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("cannot authorize cancel: no admission request in context"))
	}
	allowed, err := v.CancelAuthz.Authorize(ctx, req.UserInfo, cancelVerb, fleetTaskGR, action.Namespace, action.Name)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("authorizing cancel on FleetAction %q: %w", action.Name, err))
	}
	if !allowed {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("user %q is not permitted to cancel FleetAction %q (RFC-0001 §9.5.3)",
				req.UserInfo.Username, action.Name))
	}
	return nil
}
