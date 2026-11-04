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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// fleetTaskCompositeGR is the GroupResource for the composite FleetTask, used for the `append`
// SubjectAccessReview and Forbidden responses.
var fleetTaskCompositeGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "fleettasks"}

// appendVerb is the scoped custom RBAC verb required to grow a live FleetTask's action list
// (RFC-0001 §9.1.5). It is distinct from `update`: general write access does NOT grant append.
const appendVerb = "append"

// activateVerb is the custom RBAC verb required to activate a draft FleetTask: an update that
// moves spec.draft from true to false (RFC-0001 #security-permission-matrix). A draft is held
// so that a person stands between an observation and a robot moving; `update` alone would let
// any automation identity holding it stand in for that person.
const activateVerb = "activate"

// serviceAccountsGroup is the group the API server puts every ServiceAccount principal in.
const serviceAccountsGroup = "system:serviceaccounts"

// fleetTaskDeletablePhases is the set of phases in which a FleetTask is settled and `delete` is
// permitted (`docs/operations.md`: "delete a settled task (Pending or terminal only)"). Anything else —
// Running, or a saga still Compensating — is live: deleting it would orphan the child FleetActions and
// their assignment leases without the confirmed-stop drain, so the operator cancels first.
//
// The set is expressed positively rather than by listing "active" phases, so a phase added to the API
// later is refused by default (fail closed) instead of silently becoming deletable.
var fleetTaskDeletablePhases = map[fleetv1.FleetTaskPhase]bool{
	"":                                true, // never reconciled: nothing was ever dispatched
	fleetv1.FleetTaskPhaseDraft:       true, // a held proposal: no child exists; declining it is deleting it
	fleetv1.FleetTaskPhasePending:     true, // admitted, not yet started
	fleetv1.FleetTaskPhaseSucceeded:   true,
	fleetv1.FleetTaskPhaseFailed:      true,
	fleetv1.FleetTaskPhaseCancelled:   true,
	fleetv1.FleetTaskPhaseCompensated: true, // saga compensation finished — terminal
}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-fleettask,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=fleettasks,verbs=create;update;delete,versions=v1,name=vfleettask.swarmada.io,admissionReviewVersions=v1

// FleetTaskValidator enforces the composite FleetTask update discipline (RFC-0001 §9.1.5):
// - spec.actions is APPEND-ONLY: existing entries are immutable and may not be removed; the list
// may only grow. Growing it requires the scoped `append` verb (SubjectAccessReview).
// - completionPolicy, failurePolicy, and quorum are immutable after creation (a graph's
// semantics must not shift mid-run). desiredState remains mutable.
// - `delete` is refused while the task is live (see fleetTaskDeletablePhases): a running composite
// is cancelled through the confirmed-cancel path first, so no robot is freed while its lease is
// alive.
//
// - a change of desiredState to Cancelled requires the `cancel` verb on fleettasks
// (SubjectAccessReview), the same verb the FleetAction validator requires for the
// cancel-requested annotation.
// - clearing spec.draft (activating a draft) requires the `activate` verb on fleettasks
// (SubjectAccessReview), and is refused to every ServiceAccount whatever it holds; a
// ServiceAccount cannot add members to a draft either.
//
// It runs failurePolicy=Fail and fails CLOSED on any uncertainty.
type FleetTaskValidator struct {
	// AppendAuthz authorizes the `append` verb when a new action is appended. A nil authorizer
	// makes any append fail closed — enforcement is never bypassed by misconfiguration.
	AppendAuthz VerbAuthorizer
	// CancelAuthz authorizes the `cancel` verb when spec.desiredState changes to Cancelled. A
	// nil authorizer makes any such change fail closed.
	CancelAuthz VerbAuthorizer
	// ActivateAuthz authorizes the `activate` verb when spec.draft changes from true to false.
	// A nil authorizer makes any activation fail closed.
	ActivateAuthz VerbAuthorizer
	// Reader resolves the ComputeEndpoint a producer binding's member references — a
	// select subject, a loop.until condition, a fromMember parameter (producerRefs) — so
	// admission can decide the result-versus-event binding from the producer's declared
	// contract rather than leaving it to the author (check S6), and can refuse an event
	// type the producer does not declare (S6b).
	//
	// Nil disables ONLY that cross-object check, and does so on purpose: unlike the
	// append and delete guards above, S6 is a correctness check over a second object,
	// not a safety invariant over this one, and a misconfigured Reader must not make
	// every FleetTask unwritable. The structural checks — flow shape, declared inputs,
	// member references — need no reader and always run.
	Reader client.Reader
}

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *FleetTaskValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.FleetTask{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &FleetTaskValidator{}

// ValidateCreate checks spec.flow's structure. Everything ELSE
// about a fresh FleetTask is validated by the CRD's CEL rules — append-only
// and policy immutability only constrain updates — but flow's tree shape is
// NOT one of those things: `loop` is a CEL reserved word and structural CRD
// schemas cannot express true recursion past depth 1 (see
// api/v1/fleettask_flow_types.go's comments, verified against a real API
// server, not assumed), so this is the ONLY admission-time check for it.
func (v *FleetTaskValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	t, ok := obj.(*fleetv1.FleetTask)
	if !ok {
		return nil, fmt.Errorf("expected a FleetTask object but got %T", obj)
	}
	if err := validateFlow(t); err != nil {
		return nil, err
	}
	if err := validateBindings(t); err != nil {
		return nil, err
	}
	if err := v.validateProducerContracts(ctx, t); err != nil {
		return nil, err
	}
	return fleetTaskWarnings(t), nil
}

// fleetTaskWarnings is what create returns, and what an update that re-runs the
// binding checks returns: one list, so the two paths cannot drift apart.
func fleetTaskWarnings(t *fleetv1.FleetTask) admission.Warnings {
	return append(unevaluableEventBindingWarnings(t), fleetTaskUnreadFieldWarnings(t)...)
}

// validateFlow rejects an invalid spec.flow tree at the boundary. It is the
// SAME check (internal/flow.ValidateStructure) the FleetTask controller runs
// again at reconcile — the same belt-and-suspenders relationship v0.3's
// dependsOn already has between validateGraph (reconcile) and the CRD's CEL
// rules (admission) — so a webhook that is down, misconfigured, or bypassed
// (a raw subresource write, a downgrade) never leaves an invalid flow
// unchecked, and the two call sites can never disagree: they are one
// function, not two independently-maintained copies of the same rules.
func validateFlow(t *fleetv1.FleetTask) error {
	if t.Spec.Flow == nil {
		return nil
	}
	msg, ok := flow.ValidateStructure(t.Spec.Flow, flow.MemberNames(t.Spec.Actions))
	if ok {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: fleetTaskCompositeGR.Group, Kind: "FleetTask"},
		t.Name,
		field.ErrorList{field.Invalid(field.NewPath("spec", "flow"), "flow", msg)},
	)
}

// ValidateDelete refuses to delete a LIVE composite. A FleetTask owns its child FleetActions via
// ownerReferences, so deleting one mid-flight garbage-collects the children out from under their
// assignment leases — freeing a robot the control plane cannot prove has stopped, which is exactly
// what the confirmed-cancel discipline (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee) exists to prevent. The operator cancels first
// (`swarmctl cancel task`, i.e. spec.desiredState: Cancelled fanned out to the members), and deletes the settled
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

// ValidateUpdate enforces policy immutability and the append-only action list, re-runs
// the create-time binding checks when the spec they read changes, and authorizes a
// cancel.
//
// THE BINDING CHECKS RUN ON A CHANGE, NOT ON EVERY WRITE. validateBindings and
// validateProducerContracts read spec.flow, spec.inputs and spec.actions, and the
// second also reads each producer's ComputeEndpoint. An update that leaves those three
// alone cannot make this task's bindings invalid; only an edit to another object can,
// and refusing this task's next write for it would refuse a cancel or a label change
// on a running task because someone edited an endpoint. So an unchanged binding spec
// is not re-checked, and an object that was valid when written stays writable.
func (v *FleetTaskValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newT, ok := newObj.(*fleetv1.FleetTask)
	if !ok {
		return nil, fmt.Errorf("expected a FleetTask object but got %T", newObj)
	}
	oldT, ok := oldObj.(*fleetv1.FleetTask)
	if !ok {
		return nil, fmt.Errorf("expected a FleetTask object but got %T", oldObj)
	}

	// 0. spec.flow's tree shape — nothing else here constrains spec.flow, so
	// an update that introduces or edits one is checked the same as a create.
	if err := validateFlow(newT); err != nil {
		return nil, err
	}

	// 1. Immutable policies.
	if newT.Spec.CompletionPolicy != oldT.Spec.CompletionPolicy ||
		newT.Spec.FailurePolicy != oldT.Spec.FailurePolicy ||
		!reflect.DeepEqual(oldT.Spec.Quorum, newT.Spec.Quorum) {
		return nil, apierrors.NewForbidden(fleetTaskCompositeGR, newT.Name,
			fmt.Errorf("completionPolicy, quorum, and failurePolicy are immutable after creation (RFC-0001 #fleettask-completion-failure-and-compensation)"))
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

	// 3. The create-time binding checks, whenever the spec they read changes: without
	// them an update could carry a task create would refuse (an input a binding still
	// names removed, a flow naming an event its producer does not declare).
	var warnings admission.Warnings
	if bindingSpecChanged(oldT, newT) {
		if err := validateBindings(newT); err != nil {
			return nil, err
		}
		if err := v.validateProducerContracts(ctx, newT); err != nil {
			return nil, err
		}
		warnings = fleetTaskWarnings(newT)
	}

	// 4. Cancelling the task requires the `cancel` verb (RFC-0001
	// #security-permission-matrix), as the cancel-requested annotation does on a
	// FleetAction. Only the transition is checked: re-writing Cancelled, or any other
	// update to a task already cancelling, is not a new cancel.
	if oldT.Spec.DesiredState != fleetv1.DesiredStateCancelled &&
		newT.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		if err := v.authorizeCancel(ctx, newT); err != nil {
			return nil, err
		}
	}

	// 5. A draft is a person's to approve. Activating it requires a person holding
	// `activate`, and a ServiceAccount cannot add members to it: with inputs and flow
	// immutable, growing the draft is the one way left for automation to change what the
	// person approves. Only those two writes are checked: a label, a finalizer, a cancel or
	// a status write to a draft is neither, and a person may still append. A task created
	// with spec.draft false needs only create. A JSON-patch remove of spec.draft arrives
	// here as false, because the field defaults to false.
	if oldT.Spec.Draft {
		if appended {
			if err := refuseServiceAccountGrowingDraft(ctx, newT); err != nil {
				return nil, err
			}
		}
		if !newT.Spec.Draft {
			if err := v.authorizeActivate(ctx, newT); err != nil {
				return nil, err
			}
		}
	}

	// 6. Appending a new action requires the scoped `append` verb.
	if appended {
		if err := v.authorizeAppend(ctx, newT); err != nil {
			return nil, err
		}
	}
	return warnings, nil
}

// refuseServiceAccountGrowingDraft refuses an update that adds members to a stored draft when
// a ServiceAccount makes it, by the same test authorizeActivate applies. It fails closed when
// the requester cannot be read.
func refuseServiceAccountGrowingDraft(ctx context.Context, task *fleetv1.FleetTask) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("cannot tell who is adding members to draft FleetTask %q: no admission request in context", task.Name))
	}
	if isServiceAccountPrincipal(req.UserInfo.Username, req.UserInfo.Groups) {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("%q is a ServiceAccount, and a ServiceAccount cannot add members to draft FleetTask %q: "+
				"a person approves a draft as it stands, so only a person may grow it before activating it "+
				"(RFC-0001 #security-permission-matrix)", req.UserInfo.Username, task.Name))
	}
	return nil
}

// bindingSpecChanged reports whether an update changes any field validateBindings or
// validateProducerContracts reads from the task itself.
func bindingSpecChanged(oldT, newT *fleetv1.FleetTask) bool {
	return !reflect.DeepEqual(oldT.Spec.Flow, newT.Spec.Flow) ||
		!reflect.DeepEqual(oldT.Spec.Inputs, newT.Spec.Inputs) ||
		!reflect.DeepEqual(oldT.Spec.Actions, newT.Spec.Actions)
}

// authorizeCancel fails closed unless the admission user holds `cancel` on fleettasks.
func (v *FleetTaskValidator) authorizeCancel(ctx context.Context, task *fleetv1.FleetTask) error {
	if v.CancelAuthz == nil {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("cancel authorization is not configured; refusing to cancel FleetTask %q", task.Name))
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("cannot authorize cancel: no admission request in context"))
	}
	allowed, err := v.CancelAuthz.Authorize(ctx, req.UserInfo, cancelVerb, fleetTaskCompositeGR, task.Namespace, task.Name)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("authorizing cancel on FleetTask %q: %w", task.Name, err))
	}
	if !allowed {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("user %q is not permitted to cancel FleetTask %q: setting spec.desiredState to Cancelled "+
				"requires the cancel verb on fleettasks (RFC-0001 #security-authorization-swarmada-rbac-roles)",
				req.UserInfo.Username, task.Name))
	}
	return nil
}

// authorizeActivate fails closed unless the admission user is not a ServiceAccount and holds
// `activate` on fleettasks. The ServiceAccount refusal comes first and does not consult RBAC:
// RBAC can grant a verb to any subject, so "a ServiceAccount cannot hold activate" is only
// true if admission refuses one that does.
func (v *FleetTaskValidator) authorizeActivate(ctx context.Context, task *fleetv1.FleetTask) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("cannot authorize activate: no admission request in context"))
	}
	if isServiceAccountPrincipal(req.UserInfo.Username, req.UserInfo.Groups) {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("%q is a ServiceAccount, and a ServiceAccount cannot activate draft FleetTask %q: "+
				"clearing spec.draft is the person's checkpoint before a robot moves, and it needs a "+
				"person holding the activate verb on fleettasks (RFC-0001 #security-permission-matrix)",
				req.UserInfo.Username, task.Name))
	}
	if v.ActivateAuthz == nil {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("activate authorization is not configured; refusing to activate FleetTask %q", task.Name))
	}
	allowed, err := v.ActivateAuthz.Authorize(ctx, req.UserInfo, activateVerb, fleetTaskCompositeGR, task.Namespace, task.Name)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("authorizing activate on FleetTask %q: %w", task.Name, err))
	}
	if !allowed {
		return apierrors.NewForbidden(fleetTaskCompositeGR, task.Name,
			fmt.Errorf("user %q is not permitted to activate draft FleetTask %q: clearing spec.draft "+
				"requires the activate verb on fleettasks (RFC-0001 #security-permission-matrix)",
				req.UserInfo.Username, task.Name))
	}
	return nil
}

// isServiceAccountPrincipal reports whether a request comes from a ServiceAccount: by the
// username form the API server gives one, or by the group it puts every one in. Either is
// enough, so a malformed username in that group is still refused.
func isServiceAccountPrincipal(username string, groups []string) bool {
	if strings.HasPrefix(username, serviceAccountUserPrefix) {
		return true
	}
	for _, g := range groups {
		if g == serviceAccountsGroup {
			return true
		}
	}
	return false
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
