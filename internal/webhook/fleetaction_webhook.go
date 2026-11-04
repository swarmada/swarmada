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
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/executorguard"
)

// fleetTaskGR is the GroupResource used for the cancel-verb SubjectAccessReview
// and Forbidden responses.
var fleetTaskGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "fleetactions"}

const (
	// cancelRequestedAnnotation is the operator cancel request the FleetAction
	// controller finalizes on. It mirrors the controller's annCancelRequested; the
	// write of this annotation is the `cancel` verb site this webhook guards.
	cancelRequestedAnnotation = "swarmada.io/cancel-requested"
	// cancelVerb is the FleetAction custom RBAC verb (RFC-0001 #security-permission-matrix): swarmada:operator,
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

// G1 resolves an action's executor references at admission. These are
// the ONLY objects this webhook reads of the four compute configuration resources,
// and it only ever reads them: get/list/watch, never a write. `watch` is present
// because the manager serves these Gets from its shared informer cache — a cache
// that cannot establish a watch never becomes ready, so get without watch would be
// a permission that silently never works.
// +kubebuilder:rbac:groups=swarmada.io,resources=computeendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=resultsinks,verbs=get;list;watch

// FleetActionValidator enforces the `cancel` custom verb (RFC-0001 #security-authorization-swarmada-rbac-roles) at both
// admission sites where a cancel is requested — writing the swarmada.io/cancel-requested
// annotation, and changing spec.desiredState to Cancelled. Enforcing here (not only in
// the swarmctl verb) means a raw `kubectl annotate` or `kubectl patch` cannot cancel an
// action without the `cancel` grant. It runs failurePolicy=Fail and fails CLOSED on any
// uncertainty.
type FleetActionValidator struct {
	// CancelAuthz authorizes the `cancel` verb when the cancel-requested annotation
	// is added. A nil authorizer makes any cancel-request fail closed — enforcement
	// is never bypassed by misconfiguration.
	CancelAuthz VerbAuthorizer
	// Reader lists FleetAdapters to pre-filter a FleetAction against the connected
	// adapters' advertised action catalog (§9.2). Nil disables the pre-filter (the
	// authoritative check then happens at assignment).
	Reader client.Reader
	// APIReader reads the API server directly, bypassing the manager's cache. The requeue
	// check reads the action's robot and the stops that cover it through it (the stop
	// guard); a cached read can miss a trigger written a moment earlier. Nil falls back to
	// Reader, and with neither the check fails closed.
	APIReader client.Reader
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
		WithValidator(ownerRefGuarded{v}).
		Complete()
}

var _ webhook.CustomValidator = &FleetActionValidator{}

// ValidateCreate authorizes `cancel` if the action is created already cancel-requested, and
// holds an action created already requeue-requested to the same rule as the update that adds
// the annotation: created under the name of work a stop paused, it would move that work.
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
	if _, requested := action.Annotations[requeueRequestedAnnotation]; executorguard.RequeueChecked(true, false, requested) {
		if err := v.authorizeRequeueOfHeldWork(ctx, action); err != nil {
			return nil, err
		}
	}
	if err := v.validateSupported(ctx, action); err != nil {
		return nil, err
	}
	if err := v.validateExternalRefs(ctx, action); err != nil {
		return nil, err
	}
	if err := v.enforcePendingCap(ctx, action); err != nil {
		return nil, err
	}
	return executorUnreadFieldWarnings(action.Spec.Executor, "spec.executor"), nil
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

// validateSupported pre-filters a FleetAction against the action catalogs recorded on
// the namespace's FleetAdapters (FleetAdapter.status.supportedActions, §9.2), whether
// or not each adapter is connected: if at least one adapter in the namespace carries a
// catalog and none list the action's type, admission is refused. When no adapter
// carries a catalog (or no Reader is configured), it fails open — the authoritative
// per-instance check happens later, at assignment. It never blocks admission on a
// transient list error.
//
// A Custom action that names a verb (spec.customType) is matched on the pair: an entry
// of type Custom serves it when the entry is keyed by that verb or is unkeyed (an unkeyed
// Custom entry declares every verb its adapter serves that no keyed entry names). So the
// verb is refused only when every Custom entry in the namespace is keyed and none by it.
// A Custom action naming no verb matches any Custom entry, as before custom_type existed.
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
			if catalogEntryServes(sa, action) {
				return nil
			}
		}
	}
	if anyCatalog && action.Spec.Type == fleetv1.ActionTypeCustom && action.Spec.CustomType != "" {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("custom verb %q (type Custom) is in no FleetAdapter's status.supportedActions in namespace %q: no entry is keyed customType %q and no Custom entry is unkeyed (every adapter's recorded catalog is checked, connected or not)", action.Spec.CustomType, action.Namespace, action.Spec.CustomType))
	}
	if anyCatalog {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("action type %q is in no FleetAdapter's status.supportedActions in namespace %q (every adapter's recorded catalog is checked, connected or not)", action.Spec.Type, action.Namespace))
	}
	return nil
}

// catalogEntryServes reports whether one catalog entry declares the action: the type
// must match, and for a Custom action naming a verb, the entry must be keyed by that verb
// or unkeyed.
func catalogEntryServes(sa fleetv1.SupportedAction, action *fleetv1.FleetAction) bool {
	if sa.ActionType != string(action.Spec.Type) {
		return false
	}
	if action.Spec.Type != fleetv1.ActionTypeCustom || action.Spec.CustomType == "" {
		return true
	}
	return sa.CustomType == "" || sa.CustomType == action.Spec.CustomType
}

// ── G1: external-reference admission gate ──────────────────────

const (
	// ReasonExternalEndpointNotFound is the closed-vocabulary reason
	// for: a referenced ComputeEndpoint or ResultSink does not exist. G1 rejects
	// the write, because "a reference to a nonexistent object cannot become valid
	// by waiting" — unlike a G2 reason, which leaves the action Pending.
	ReasonExternalEndpointNotFound = "ExternalEndpointNotFound"
	// ReasonExternalContractMismatch is the closed-vocabulary reason
	// for: the endpoint and what consumes it are contractually incompatible. Used
	// here when a named ResultSink declares a column set that does not accept every
	// result field the referenced ComputeEndpoint declares.
	ReasonExternalContractMismatch = "ExternalContractMismatch"
)

// validateExternalRefs implements G1 for a FleetAction carrying
// spec.executor: every referenced ComputeEndpoint and ResultSink MUST exist in the
// action's namespace, and each sink MUST accept the endpoint's declared results.
// This is the check `FleetActionSpec.Executor.Sinks`' doc comment has promised since
// the field was added, and which did not exist.
//
// FAILURE DISCIPLINE. A definitive NotFound is a rejection — that is the entire
// point of the gate, and waiting cannot fix it. Any OTHER read error (API server
// unavailable, RBAC not yet granted, cache not synced) fails OPEN, matching
// validateSupported above: an unreadable dependency must not block an otherwise
// valid write, and  places G2 at scheduling and G3 at activation precisely so
// that a miss here is caught before anything runs. G1 is the early, cheap gate, not
// the only one.
//
// RA-1: read-only. It writes no status and emits no event.
func (v *FleetActionValidator) validateExternalRefs(ctx context.Context, action *fleetv1.FleetAction) error {
	if v.Reader == nil || action.Spec.Executor == nil {
		return nil
	}
	ns := action.Namespace

	var endpoint fleetv1.ComputeEndpoint
	switch err := v.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: action.Spec.Executor.EndpointRef}, &endpoint); {
	case apierrors.IsNotFound(err):
		return apierrors.NewInvalid(
			fleetv1.GroupVersion.WithKind("FleetAction").GroupKind(), action.Name,
			field.ErrorList{field.Invalid(
				field.NewPath("spec", "executor", "endpointRef"), action.Spec.Executor.EndpointRef,
				fmt.Sprintf("%s: no ComputeEndpoint %q exists in namespace %q",
					ReasonExternalEndpointNotFound, action.Spec.Executor.EndpointRef, ns),
			)},
		)
	case err != nil:
		return nil // fail open — see the failure discipline above.
	}

	for i, sinkName := range action.Spec.Executor.Sinks {
		var sink fleetv1.ResultSink
		switch err := v.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: sinkName}, &sink); {
		case apierrors.IsNotFound(err):
			return apierrors.NewInvalid(
				fleetv1.GroupVersion.WithKind("FleetAction").GroupKind(), action.Name,
				field.ErrorList{field.Invalid(
					field.NewPath("spec", "executor", "sinks").Index(i), sinkName,
					fmt.Sprintf("%s: no ResultSink %q exists in namespace %q",
						ReasonExternalEndpointNotFound, sinkName, ns),
				)},
			)
		case err != nil:
			return nil // fail open — see the failure discipline above.
		}
		if missing := resultsNotAccepted(&endpoint, &sink); len(missing) > 0 {
			return apierrors.NewInvalid(
				fleetv1.GroupVersion.WithKind("FleetAction").GroupKind(), action.Name,
				field.ErrorList{field.Invalid(
					field.NewPath("spec", "executor", "sinks").Index(i), sinkName,
					fmt.Sprintf("%s: ResultSink %q declares a column set that does not accept result field(s) %v declared by ComputeEndpoint %q",
						ReasonExternalContractMismatch, sinkName, missing, action.Spec.Executor.EndpointRef),
				)},
			)
		}
	}
	return nil
}

// resultsNotAccepted returns the endpoint result field names a sink's declared
// schema does not accept, in declaration order.
//
// A sink that declares NO schema, or a schema with no columns, accepts anything:
// the check is against what the operator actually declared, and an absent
// declaration is not an empty one. This is the same explicit-presence discipline
// the rest of the API applies to safety-relevant scalars — a missing column list
// means "not stated", never "accepts nothing", which would reject every sink that
// simply had not been described yet.
func resultsNotAccepted(endpoint *fleetv1.ComputeEndpoint, sink *fleetv1.ResultSink) []string {
	if sink.Spec.Schema == nil || len(sink.Spec.Schema.Columns) == 0 {
		return nil
	}
	accepted := make(map[string]bool, len(sink.Spec.Schema.Columns))
	for _, c := range sink.Spec.Schema.Columns {
		accepted[c] = true
	}
	var missing []string
	for _, r := range endpoint.Spec.Results {
		if !accepted[r.Name] {
			missing = append(missing, r.Name)
		}
	}
	return missing
}

// ValidateUpdate authorizes `cancel` when the cancel-requested annotation is newly
// added (absent → present), or when spec.desiredState changes to Cancelled. Re-valuing
// an existing request, re-writing Cancelled, or any other update is not a new cancel
// and is not gated.
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
	if err := v.validateExternalRefs(ctx, newAction); err != nil {
		return nil, err
	}
	_, hadCancel := oldAction.Annotations[cancelRequestedAnnotation]
	_, hasCancel := newAction.Annotations[cancelRequestedAnnotation]
	if !hadCancel && hasCancel {
		if err := v.authorizeCancel(ctx, newAction); err != nil {
			return nil, err
		}
	}
	// A requeue of work an emergency stop paused is the one edit that moves held work, and it
	// takes the authority that clears the stop (RFC-0001 #safety-fleetaction-behaviour-during-estop).
	// The stop finalizer keeps work a robot under a stop records through any delete; removing it by
	// hand is the same decision as the delete, and takes the same authority. The control plane is
	// not exempt: it removes the finalizer once the stop no longer holds the robot, which this check
	// admits, and a removal it decided on a stale read is refused and retried.
	if controllerutil.ContainsFinalizer(oldAction, estopscope.StopFinalizer) && !controllerutil.ContainsFinalizer(newAction, estopscope.StopFinalizer) {
		req, err := admission.RequestFromContext(ctx)
		if err != nil {
			return nil, apierrors.NewForbidden(fleetTaskGR, newAction.Name, fmt.Errorf("cannot authorize the finalizer removal: no admission request in context"))
		}
		rd := v.APIReader
		if rd == nil {
			rd = v.Reader
		}
		if err := refuseMovingHeldWork(ctx, rd, v.CancelAuthz, req.UserInfo, oldAction, "remove the stop finalizer of"); err != nil {
			return nil, err
		}
	}
	_, hadRequeue := oldAction.Annotations[requeueRequestedAnnotation]
	_, hasRequeue := newAction.Annotations[requeueRequestedAnnotation]
	if executorguard.RequeueChecked(false, hadRequeue, hasRequeue) {
		if err := v.authorizeRequeueOfHeldWork(ctx, newAction); err != nil {
			return nil, err
		}
	}
	// The same verb for the desiredState route, as the FleetTask validator requires it
	// for a task. The manager is exempt: cancelling a FleetTask fans Cancelled out to its
	// members under the manager's own ServiceAccount, which holds no `cancel`, and the
	// user who cancelled the task was already authorized for it there.
	if oldAction.Spec.DesiredState != fleetv1.DesiredStateCancelled &&
		newAction.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		if err := v.authorizeDesiredStateCancel(ctx, newAction); err != nil {
			return nil, err
		}
	}
	// Warned when the value changes, not on every write: the controllers write this
	// object's metadata, and they are not the author the warning is for.
	if newAction.Spec.Executor != nil &&
		(oldAction.Spec.Executor == nil ||
			!reflect.DeepEqual(oldAction.Spec.Executor.OnUnresolvedParameter, newAction.Spec.Executor.OnUnresolvedParameter)) {
		return executorUnreadFieldWarnings(newAction.Spec.Executor, "spec.executor"), nil
	}
	return nil, nil
}

// ValidateDelete is a no-op — deletion is governed by standard RBAC on fleetactions.
func (v *FleetActionValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// authorizeDesiredStateCancel admits the manager's own principal and otherwise defers to
// authorizeCancel. A request it cannot attribute is refused.
func (v *FleetActionValidator) authorizeDesiredStateCancel(ctx context.Context, action *fleetv1.FleetAction) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("cannot authorize cancel: no admission request in context"))
	}
	if isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	return v.authorizeCancel(ctx, action)
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
			fmt.Errorf("user %q is not permitted to cancel FleetAction %q (RFC-0001 #security-authorization-swarmada-rbac-roles)",
				req.UserInfo.Username, action.Name))
	}
	return nil
}

// requeueRequestedAnnotation marks a FleetAction for a forcible requeue; the controller reads it.
const requeueRequestedAnnotation = "swarmada.io/requeue-requested"

// authorizeRequeueOfHeldWork admits a requeue of an action whose robot an emergency stop covers
// only to a requester who holds estop-clear on each covering carrier (the same SubjectAccessReview
// the estop verbs use), or to the control plane's own principal. The robot is the action's bound
// robot, or, when a hold has released the binding, every robot that still records the action
// (the executor guard keeps that record until the robot is provably off the action); on a create,
// which carries no binding, every robot that records the name. An action no robot is bound to or
// records is not held work, and its requeue is admitted as before.
func (v *FleetActionValidator) authorizeRequeueOfHeldWork(ctx context.Context, action *fleetv1.FleetAction) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("cannot authorize the requeue: no admission request in context"))
	}
	if isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	rd := v.APIReader
	if rd == nil {
		rd = v.Reader
	}
	if rd == nil {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("the requeue check has no reader configured; refusing to requeue FleetAction %q unchecked", action.Name))
	}
	var robots []fleetv1.Robot
	if action.Status.AssignedRobot != "" {
		var rb fleetv1.Robot
		if err := rd.Get(ctx, client.ObjectKey{Namespace: action.Namespace, Name: action.Status.AssignedRobot}, &rb); err != nil {
			if !apierrors.IsNotFound(err) {
				return apierrors.NewInternalError(fmt.Errorf("reading robot %q for the requeue check: %w", action.Status.AssignedRobot, err))
			}
		} else {
			robots = append(robots, rb)
		}
	} else {
		var list fleetv1.RobotList
		if err := rd.List(ctx, &list, client.InNamespace(action.Namespace)); err != nil {
			return apierrors.NewInternalError(fmt.Errorf("listing robots for the requeue check: %w", err))
		}
		for i := range list.Items {
			if list.Items[i].Status.AssignedAction == action.Name {
				robots = append(robots, list.Items[i])
			}
		}
	}
	for i := range robots {
		cov, err := estopscope.Covering(ctx, rd, &robots[i])
		if err != nil {
			return apierrors.NewInternalError(fmt.Errorf("deciding which stops cover robot %q for the requeue check: %w", robots[i].Name, err))
		}
		for _, s := range cov.Stops {
			gr := robotGR
			switch s.Kind {
			case estopscope.KindZone:
				gr = fleetZoneGR
			case estopscope.KindNamespace:
				gr = swarmadaConfigGR
			}
			if v.CancelAuthz == nil {
				return apierrors.NewForbidden(fleetTaskGR, action.Name,
					fmt.Errorf("estop authorization is not configured; refusing to requeue FleetAction %q, whose robot %q is "+
						"under the emergency stop of %s %q", action.Name, robots[i].Name, s.Kind, s.Name))
			}
			allowed, err := v.CancelAuthz.Authorize(ctx, req.UserInfo, estopClearVerb, gr, action.Namespace, s.Name)
			if err != nil {
				return apierrors.NewInternalError(fmt.Errorf("authorizing %q on %s %q: %w", estopClearVerb, gr.Resource, s.Name, err))
			}
			if !allowed {
				return apierrors.NewForbidden(fleetTaskGR, action.Name, fmt.Errorf(
					"user %q may not requeue FleetAction %q: robot %q is under the emergency stop of %s %q, and moving work "+
						"a stop paused needs the estop-clear verb on that stop, the authority that clears it. %s "+
						"(RFC-0001 #safety-fleetaction-behaviour-during-estop)",
					req.UserInfo.Username, action.Name, robots[i].Name, s.Kind, s.Name, whoDecides(action, s)))
			}
		}
	}
	return nil
}
