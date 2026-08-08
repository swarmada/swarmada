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
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// fleetTaskCompositeGR is the GroupResource for the composite FleetTask, used for the `append`
// SubjectAccessReview and Forbidden responses.
var fleetTaskCompositeGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "fleettasks"}

// appendVerb is the scoped custom RBAC verb required to grow a live FleetTask's action list
// (RFC-0001 §9.1.5). It is distinct from `update`: general write access does NOT grant append.
const appendVerb = "append"

// fleetTaskDeletablePhases is the set of phases in which a FleetTask is settled and `delete` is
// permitted (`docs/operations.md`: "delete a settled task (Pending or terminal only)"). Anything else —
// Running, or a saga still Compensating — is live: deleting it would orphan the child FleetActions and
// their assignment leases without the confirmed-stop drain, so the operator cancels first.
//
// The set is expressed positively rather than by listing "active" phases, so a phase added to the API
// later is refused by default (fail closed) instead of silently becoming deletable.
var fleetTaskDeletablePhases = map[fleetv1.FleetTaskPhase]bool{
	"":                                true, // never reconciled: nothing was ever dispatched
	fleetv1.FleetTaskPhasePending:     true, // admitted, not yet started
	fleetv1.FleetTaskPhaseSucceeded:   true,
	fleetv1.FleetTaskPhaseFailed:      true,
	fleetv1.FleetTaskPhaseCancelled:   true,
	fleetv1.FleetTaskPhaseCompensated: true, // saga compensation finished — terminal
}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-fleettask,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=fleettasks,verbs=create;update;delete,versions=v1,name=vfleettask.swarmada.io,admissionReviewVersions=v1

// FleetTaskValidator enforces the composite FleetTask update discipline (RFC-0001 §9.1.5):
//   - spec.actions is APPEND-ONLY: existing entries are immutable and may not be removed; the list
//     may only grow. Growing it requires the scoped `append` verb (SubjectAccessReview).
//   - completionPolicy, failurePolicy, and quorum are immutable after creation (a graph's
//     semantics must not shift mid-run). desiredState remains mutable.
//   - `delete` is refused while the task is live (see fleetTaskDeletablePhases): a running composite
//     is cancelled through the confirmed-cancel path first, so no robot is freed while its lease is
//     alive.
//
// It runs failurePolicy=Fail and fails CLOSED on any uncertainty.
type FleetTaskValidator struct {
	// AppendAuthz authorizes the `append` verb when a new action is appended. A nil authorizer
	// makes any append fail closed — enforcement is never bypassed by misconfiguration.
	AppendAuthz VerbAuthorizer
}

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *FleetTaskValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.FleetTask{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &FleetTaskValidator{}

// ValidateCreate is a no-op: the initial structure is validated by the CRD's CEL rules, and
// append-only + policy immutability only constrain updates.
func (v *FleetTaskValidator) ValidateCreate(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete refuses to delete a LIVE composite. A FleetTask owns its child FleetActions via
// ownerReferences, so deleting one mid-flight garbage-collects the children out from under their
// assignment leases — freeing a robot the control plane cannot prove has stopped, which is exactly
// what the confirmed-cancel discipline (§9.6.3.5) exists to prevent. The operator cancels first
// (`swarmctl cancel task`, i.e. the swarmada.io/cancel-requested annotation), and deletes the settled
// record afterwards.
//
// There is deliberately no drain-on-delete here: a deletion finalizer running the same
// confirmed-cancel drain is the eventual convergence, and until it exists this guard is the safety
// mechanism (FleetTask has no DeletionTimestamp handling today).
func (v *FleetTaskValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	t, ok := obj.(*fleetv1.FleetTask)
	if !ok {
		// Fail closed: an object we cannot inspect is not provably settled.
		return nil, fmt.Errorf("expected a FleetTask object but got %T", obj)
	}
	if fleetTaskDeletablePhases[t.Status.Phase] {
		return nil, nil
	}
	return nil, apierrors.NewForbidden(fleetTaskCompositeGR, t.Name, fmt.Errorf(
		"FleetTask is %s: a live composite cannot be deleted (it owns its FleetActions and their "+
			"assignment leases). Cancel it first (swarmctl cancel task %s -n %s), then delete the "+
			"settled record", phaseOrUnset(string(t.Status.Phase)), t.Name, t.Namespace))
}

// phaseOrUnset renders an empty phase readably in an admission message.
func phaseOrUnset(p string) string {
	if p == "" {
		return "in an unset phase"
	}
	return "in phase " + p
}

// ValidateUpdate enforces policy immutability and the append-only action list.
func (v *FleetTaskValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newT, ok := newObj.(*fleetv1.FleetTask)
	if !ok {
		return nil, fmt.Errorf("expected a FleetTask object but got %T", newObj)
	}
	oldT, ok := oldObj.(*fleetv1.FleetTask)
	if !ok {
		return nil, fmt.Errorf("expected a FleetTask object but got %T", oldObj)
	}

	// 1. Immutable policies.
	if newT.Spec.CompletionPolicy != oldT.Spec.CompletionPolicy ||
		newT.Spec.FailurePolicy != oldT.Spec.FailurePolicy ||
		!reflect.DeepEqual(oldT.Spec.Quorum, newT.Spec.Quorum) {
		return nil, apierrors.NewForbidden(fleetTaskCompositeGR, newT.Name,
			fmt.Errorf("completionPolicy, quorum, and failurePolicy are immutable after creation (RFC-0001 §9.1.5)"))
	}

	// 2. Append-only: every existing action must be present unchanged; the list may only grow.
	oldByName := make(map[string]fleetv1.FleetTaskAction, len(oldT.Spec.Actions))
	for _, a := range oldT.Spec.Actions {
		oldByName[a.Name] = a
	}
	appended := false
	for i := range newT.Spec.Actions {
		na := newT.Spec.Actions[i]
		if oa, existed := oldByName[na.Name]; existed {
			if !reflect.DeepEqual(oa, na) {
				return nil, apierrors.NewForbidden(fleetTaskCompositeGR, newT.Name,
					fmt.Errorf("existing action %q is immutable; the action list is append-only (RFC-0001 §9.1.5)", na.Name))
			}
			delete(oldByName, na.Name)
		} else {
			appended = true
		}
	}
	if len(oldByName) > 0 {
		return nil, apierrors.NewForbidden(fleetTaskCompositeGR, newT.Name,
			fmt.Errorf("actions may not be removed; the action list is append-only (RFC-0001 §9.1.5)"))
	}

	// 3. Appending a new action requires the scoped `append` verb.
	if appended {
		return nil, v.authorizeAppend(ctx, newT)
	}
	return nil, nil
}

// authorizeAppend fails closed unless the admission user holds the `append` verb.
func (v *FleetTaskValidator) authorizeAppend(ctx context.Context, task *fleetv1.FleetTask) error {
	if v.AppendAuthz == nil {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("append authorization is not configured; refusing to append to FleetTask %q", task.Name))
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("cannot authorize append: no admission request in context"))
	}
	allowed, err := v.AppendAuthz.Authorize(ctx, req.UserInfo, appendVerb, fleetTaskCompositeGR, task.Namespace, task.Name)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("authorizing append on FleetTask %q: %w", task.Name, err))
	}
	if !allowed {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("user %q is not permitted to append actions to FleetTask %q (RFC-0001 §9.1.5)",
				req.UserInfo.Username, task.Name))
	}
	return nil
}
