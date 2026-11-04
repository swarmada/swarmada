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
	"sort"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/zone"
)

// fleetZoneGK is the GroupKind reported in admission error responses.
var fleetZoneGK = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "FleetZone"}

// fleetZoneGR is the GroupResource used for SubjectAccessReview attributes and
// Forbidden responses on the estop custom verbs.
var fleetZoneGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "fleetzones"}

// maxZoneAncestryDepth bounds the parent-chain walk: a zone and this many ancestors, the depth
// a zone's emergency stop reaches (estopscope.MaxAncestors). A chain still climbing past it is
// refused naming the depth; a chain that revisits a zone is refused as a cycle.
const maxZoneAncestryDepth = estopscope.MaxAncestors

const (
	// estopTriggeredAnnotation gates a zone-wide emergency stop. It mirrors the
	// controller's annEstopTriggered (internal/controller/zoneestop_controller.go) —
	// the write of this annotation is the estop "verb site" this webhook guards.
	estopTriggeredAnnotation = "swarmada.io/estop-triggered"
	// estopTriggerVerb / estopClearVerb are the FleetZone custom RBAC verbs
	// (the RBAC roles): swarmada:operator+admin may trigger; only swarmada:admin
	// may clear.
	estopTriggerVerb = "estop-trigger"
	estopClearVerb   = "estop-clear"
)

// VerbAuthorizer decides whether the admission user may exercise a custom RBAC
// verb (estop-trigger / estop-clear / cancel / …) on a resource. It exists so the
// SubjectAccessReview call can be faked in tests; production uses SARAuthorizer.
type VerbAuthorizer interface {
	Authorize(ctx context.Context, user authenticationv1.UserInfo, verb string, resource schema.GroupResource, namespace, name string) (bool, error)
}

// SARAuthorizer authorizes a custom verb by POSTing a SubjectAccessReview for the
// admission user against the target resource+verb, delegating the decision to the
// cluster's RBAC (the roles and verbs defined in RFC-0001 #security-authorization-swarmada-rbac-roles). One
// instance serves every resource — the resource is passed per call.
type SARAuthorizer struct {
	// Client must be able to CREATE authorization.k8s.io SubjectAccessReviews.
	Client client.Client
}

// Authorize returns whether user may perform verb on the named resource.
func (a *SARAuthorizer) Authorize(ctx context.Context, user authenticationv1.UserInfo, verb string, resource schema.GroupResource, namespace, name string) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for k, vals := range user.Extra {
		extra[k] = authorizationv1.ExtraValue(vals)
	}
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user.Username,
			Groups: user.Groups,
			UID:    user.UID,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:     resource.Group,
				Resource:  resource.Resource,
				Verb:      verb,
				Namespace: namespace,
				Name:      name,
			},
		},
	}
	if err := a.Client.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("creating SubjectAccessReview: %w", err)
	}
	return sar.Status.Allowed, nil
}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-fleetzone,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=fleetzones,verbs=create;update;delete,versions=v1,name=vfleetzone.swarmada.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get

// FleetZoneValidator enforces the cross-resource and geometric invariants of the
// FleetZone hierarchy that per-field CEL cannot express (RFC-0001 #control-plane-swarmada-api-server):
//
//   - a named parentZone must exist in the same namespace, and cannot be the zone
//     itself;
//   - the parent chain must be acyclic (A→B→A is rejected);
//   - spec.physicalBounds.polygon must be a simple (non-self-intersecting) polygon,
//     so containment is well defined;
//   - a zone that anything still references cannot be deleted: a child zone, a robot
//     that declares it or is placed in it, or a FleetAction that can still run in it.
//
// It runs failurePolicy=Fail: a FleetZone that cannot be validated is not admitted.
type FleetZoneValidator struct {
	// Client is not read by any check here: every read goes through APIReader.
	Client client.Client
	// APIReader reads the API server directly, bypassing the manager's cache. The delete
	// check makes every read through it, and so do the create and update checks on the
	// parentZone chain (the parent's existence and the cycle walk). A cached read can miss
	// an object admitted milliseconds earlier: on delete a miss admits a delete that
	// leaves a dangling reference, and on the chain a stale parent admits a cycle
	// (A→B→A) while the check honestly saw none. A nil reader makes every delete, and
	// every create or update that names a parentZone, fail closed.
	APIReader client.Reader
	// EstopAuthz authorizes the estop-trigger / estop-clear custom verbs when the
	// swarmada.io/estop-triggered annotation is written (RFC-0001 #security-authorization-swarmada-rbac-roles). A nil authorizer
	// makes any estop-annotation change fail closed — enforcement is never bypassed
	// by misconfiguration; structural (non-estop) validation is unaffected.
	EstopAuthz VerbAuthorizer
}

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *FleetZoneValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.FleetZone{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &FleetZoneValidator{}

// ValidateCreate runs the parent-chain and polygon checks on creation, refuses a
// create that already carries the estop idempotence marker, and authorizes
// estop-trigger when the zone is created already carrying the estop-triggered
// annotation.
func (v *FleetZoneValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	zoneObj, ok := obj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", obj)
	}
	// On CREATE as well as UPDATE. A zone born with the marker already stamped has
	// its first estop read as already fanned out, which is the same suppression a
	// forged update buys — and it is open to anyone who can create the zone, or who
	// can delete an existing one and make it again. A create has no old annotations;
	// the guard is written for that. It runs FIRST so that a forged marker is named
	// as the reason even when the zone is also structurally invalid.
	if err := GuardEstopProcessedMarker(ctx, fleetZoneGR, zoneObj.Namespace, zoneObj.Name,
		nil, zoneObj.Annotations); err != nil {
		return nil, err
	}
	// The actor a zone is born with is the stamper's, at its trigger, or none.
	if err := guardEstopActor(ctx, fleetZoneGR, zoneObj.Namespace, zoneObj.Name,
		nil, zoneObj.Annotations, estopActorStampKeys); err != nil {
		return nil, err
	}
	// A carrier created with the trigger already set is an estop: its reconciler has no
	// create predicate, finds a trigger and no processed marker, and fans out. So the
	// verb is authorized exactly as on the update that adds the annotation. Without
	// this, `create` alone was enough to deliver an estop at any scope, including the
	// namespace-wide one on the SwarmadaConfig. There is nothing to clear at create, so
	// the verb is always estop-trigger.
	if verb, isEstopChange := estopVerbForAnnotations(nil, zoneObj.Annotations); isEstopChange {
		if err := v.authorizeEstop(ctx, zoneObj, verb); err != nil {
			return nil, err
		}
		if err := refuseCompositeEstop(zoneObj); err != nil {
			return nil, err
		}
	}
	return v.validate(ctx, zoneObj)
}

// ValidateUpdate re-runs the structural checks whenever the parent or polygon may
// have changed, and — when the update writes the estop-triggered annotation —
// authorizes the corresponding estop custom verb (RFC-0001 #security-authorization-swarmada-rbac-roles).
//
// One write is exempt from the structural checks: an update whose only change is to the
// estop annotations (onlyEstopAnnotationsChangedOn). The parent chain and FZ-R7 resolve
// against the zone's ancestors, which can change after this zone was admitted: FZ-R8
// admits an ancestor tightening onto an existing descendant by construction, and the
// descendant then contradicts it. None of that is a reason to refuse a stop, or the
// controller's record that it fanned the stop out. The control plane's writes of metadata
// alone (the stop finalizer it holds, the marker it records) are exempt for the same reason.
//
// On either path, the stop finalizer of a zone that carries a stop is removed only by the
// control plane or an estop-clear holder (refuseStopFinalizerRemoval).
func (v *FleetZoneValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newZone, ok := newObj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", newObj)
	}
	oldZone, ok := oldObj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", oldObj)
	}
	if err := refuseStopFinalizerRemoval(ctx, v.EstopAuthz, v.APIReader, fleetZoneGR, oldZone, newZone); err != nil {
		return nil, err
	}
	// Ahead of the estop-only exemption: a change of the actor alone is an estop-only write.
	if err := guardEstopActor(ctx, fleetZoneGR, newZone.Namespace, newZone.Name,
		oldZone.Annotations, newZone.Annotations, estopActorStampKeys); err != nil {
		return nil, err
	}

	// An emergency stop is never refused for something unrelated to the stop. The three
	// estop checks still run on this path, the marker guard first so that a forged marker
	// is named as the reason (as on create). RFC-0001 #control-plane-swarmada-api-server (the
	// estop-only update exemption); the estop checks, RFC-0001 #safety-estop-scopes.
	if onlyEstopAnnotationsChangedOn(oldZone, newZone, nil,
		func(z *fleetv1.FleetZone) { z.Status = fleetv1.FleetZoneStatus{} }) || controllerMetadataOnly(ctx, oldZone, newZone) {
		if err := GuardEstopProcessedMarker(ctx, fleetZoneGR, newZone.Namespace, newZone.Name,
			oldZone.Annotations, newZone.Annotations); err != nil {
			return nil, err
		}
		return nil, v.authorizeEstopChange(ctx, oldZone, newZone)
	}

	warns, err := v.validate(ctx, newZone)
	if err != nil {
		return warns, err
	}
	if err := v.authorizeEstopChange(ctx, oldZone, newZone); err != nil {
		return warns, err
	}
	if err := refuseCompositeUnderEstop(oldZone, newZone); err != nil {
		return warns, err
	}
	if err := refusePropagateChangeUnderPendingStop(ctx, v.APIReader, oldZone, newZone); err != nil {
		return warns, err
	}
	if err := v.refuseEditThatUncoversAZone(ctx, oldZone, newZone); err != nil {
		return warns, err
	}
	if err := v.refuseCompositeOverDeclaredRobots(ctx, oldZone, newZone); err != nil {
		return warns, err
	}
	// Unconditional, NOT inside authorizeEstopChange: a forged marker arrives on an
	// update that leaves the estop-triggered annotation alone, so the verb gate is never
	// consulted, and the marker then suppresses the fan-out of the trigger that follows.
	if err := GuardEstopProcessedMarker(ctx, fleetZoneGR, newZone.Namespace, newZone.Name,
		oldZone.Annotations, newZone.Annotations); err != nil {
		return warns, err
	}
	return warns, nil
}

// authorizeEstopChange authorizes the estop custom verb an old→new transition of the
// estop-triggered annotation requires, and refuses a trigger on a composite (FZ-C5). A
// clear is never refused on the composite ground: an estop annotation that reached a
// composite some other way must still be removable. A write that is not an estop action
// passes.
func (v *FleetZoneValidator) authorizeEstopChange(ctx context.Context, oldZone, newZone *fleetv1.FleetZone) error {
	verb, isEstopChange := estopVerbFor(oldZone, newZone)
	if !isEstopChange {
		return nil
	}
	if err := v.authorizeEstop(ctx, newZone, verb); err != nil {
		return err
	}
	if verb == estopTriggerVerb {
		return refuseCompositeEstop(newZone)
	}
	return nil
}

// estopVerbFor reports which estop custom verb an old→new transition of the
// estop-triggered annotation requires, and whether the transition is an estop
// action at all. Adding or re-valuing the annotation is an estop-trigger; removing
// it is an estop-clear; an unchanged annotation is not an estop action.
func estopVerbFor(oldZone, newZone *fleetv1.FleetZone) (verb string, isEstopChange bool) {
	return estopVerbForAnnotations(oldZone.Annotations, newZone.Annotations)
}

// estopVerbForAnnotations reports which estop custom verb an old→new transition of
// the estop-triggered annotation requires, and whether it is an estop action at
// all. Shared by every estop-annotation carrier (FleetZone, Robot, SwarmadaConfig)
// so the trigger/clear authorization rule is identical across estop scopes.
func estopVerbForAnnotations(oldAnn, newAnn map[string]string) (verb string, isEstopChange bool) {
	oldVal, had := oldAnn[estopTriggeredAnnotation]
	newVal, has := newAnn[estopTriggeredAnnotation]
	switch {
	case !had && has:
		return estopTriggerVerb, true // added → trigger
	case had && has && oldVal != newVal:
		return estopTriggerVerb, true // re-valued → re-trigger
	case had && !has:
		return estopClearVerb, true // removed → clear
	default:
		return "", false
	}
}

// authorizeEstopVerb fails closed unless the admission user is permitted the estop
// custom verb on the named resource. Shared by every estop carrier's
// validator: a nil authorizer, a missing admission request, an SAR error, or a
// denied SAR all refuse the write.
func authorizeEstopVerb(ctx context.Context, authz VerbAuthorizer, gr schema.GroupResource, namespace, name, verb string) error {
	if authz == nil {
		return apierrors.NewForbidden(gr, name,
			fmt.Errorf("estop authorization is not configured; refusing %q on %s %q", verb, gr.Resource, name))
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(gr, name,
			fmt.Errorf("cannot authorize %q: no admission request in context", verb))
	}
	allowed, err := authz.Authorize(ctx, req.UserInfo, verb, gr, namespace, name)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("authorizing %q on %s %q: %w", verb, gr.Resource, name, err))
	}
	if !allowed {
		return apierrors.NewForbidden(gr, name,
			fmt.Errorf("user %q is not permitted to %q %s %q (see the Swarmada RBAC roles)",
				req.UserInfo.Username, verb, gr.Resource, name))
	}
	return nil
}

// refuseCompositeEstop rejects an estop trigger on a composite zone (RFC-0001 FZ-C5).
// Composition asserts nothing physical, so estop propagation follows containment edges
// only (FZ-C4) and a composite would propagate the stop to none of its members. A
// composite holds no robot directly because the Robot admission gate refuses a
// spec.zone that names one (FZ-C2); a robot whose zone gained members after it was
// admitted is reached per robot or at namespace scope. Accepting the trigger would be
// a stop that stops nothing, the most dangerous kind of false assurance, so it is
// refused and the operator is told what to stop instead. The message is RFC-0001's,
// verbatim.
func refuseCompositeEstop(z *fleetv1.FleetZone) error {
	if len(z.Spec.MemberZones) == 0 {
		return nil
	}
	//nolint:staticcheck // ST1005: RFC-0001 FZ-C5 specifies this message verbatim, final period included.
	return apierrors.NewForbidden(fleetZoneGR, z.Name, fmt.Errorf(
		"FleetZone '%s' is a composite zone; estop does not propagate across composition edges. "+
			"Trigger estop on a containment zone, on each member ([%s]), or at namespace scope.",
		z.Name, strings.Join(z.Spec.MemberZones, ", ")))
}

// refuseCompositeUnderEstop refuses the other way an estop could come to name a
// composite: members added to a zone that already carries the estop trigger. The trigger
// was admitted on a terminal zone, but the controller fans it out when it reconciles,
// and by then the zone would be a composite whose estop propagates to none of its
// members (FZ-C4), so the stop an operator issued would cover only the robots that still
// declare the zone itself, possibly none. It refuses the membership change, never the
// estop: the zone stays as it was, stopped, and the members can be added once the estop
// is cleared. The trigger is on the object being written, so this needs no read of any
// other object and cannot race one. RFC-0001 FZ-C5.
func refuseCompositeUnderEstop(oldZone, newZone *fleetv1.FleetZone) error {
	if len(oldZone.Spec.MemberZones) > 0 || len(newZone.Spec.MemberZones) == 0 {
		return nil
	}
	if _, stopped := newZone.Annotations[estopTriggeredAnnotation]; !stopped {
		return nil
	}
	return apierrors.NewForbidden(fleetZoneGR, newZone.Name, fmt.Errorf(
		"FleetZone %q is under an active estop (%s is set), so it cannot become a composite: estop does not "+
			"propagate across composition edges, and the stop would then cover none of the members "+
			"[%s]. Clear the estop first, or stop each member",
		newZone.Name, estopTriggeredAnnotation, strings.Join(newZone.Spec.MemberZones, ", ")))
}

// refuseCompositeOverDeclaredRobots refuses members added to a zone that a Robot declares
// in spec.zone. A robot is never in a composite (RFC-0001 FZ-C2): the Robot admission gate
// refuses a composite spec.zone when the robot is written, and this is the other side of
// the same rule, the zone becoming a composite under robots that already declare it.
// Those robots would keep the zone, FZ-G5 would place them in it when it has no usable
// geometry, and a zone estop on it is refused (FZ-C5), so no zone estop would reach them.
//
// Only the change that makes the zone a composite is checked; a composite's member list
// may be edited freely. The Robot list is read from the API server, as the delete check
// reads it. A Robot admitted concurrently into this zone can still see the zone as it
// was: admission has no cross-object transaction.
func (v *FleetZoneValidator) refuseCompositeOverDeclaredRobots(ctx context.Context, oldZone, newZone *fleetv1.FleetZone) error {
	if len(oldZone.Spec.MemberZones) > 0 || len(newZone.Spec.MemberZones) == 0 {
		return nil
	}
	if v.APIReader == nil {
		return fmt.Errorf("FleetZone validator has no uncached reader; refusing to add members to %s/%s "+
			"without checking which robots declare it", newZone.Namespace, newZone.Name)
	}
	var robots fleetv1.RobotList
	if err := v.APIReader.List(ctx, &robots, client.InNamespace(newZone.Namespace)); err != nil {
		return fmt.Errorf("listing Robots in %q: %w", newZone.Namespace, err)
	}
	var declared []string
	for i := range robots.Items {
		if robots.Items[i].Spec.Zone == newZone.Name {
			declared = append(declared, robots.Items[i].Name)
		}
	}
	if len(declared) == 0 {
		return nil
	}
	return apierrors.NewInvalid(fleetZoneGK, newZone.Name, field.ErrorList{field.Forbidden(
		field.NewPath("spec").Child("memberZones"),
		fmt.Sprintf("FleetZone %q is declared by robots %s (spec.zone), and a robot is never in a composite "+
			"zone: re-home them to a terminal zone first, then add the members", newZone.Name,
			namedReferences(declared)))})
}

// authorizeEstop fails closed unless the admission user is permitted the estop verb.
func (v *FleetZoneValidator) authorizeEstop(ctx context.Context, z *fleetv1.FleetZone, verb string) error {
	return authorizeEstopVerb(ctx, v.EstopAuthz, fleetZoneGR, z.Namespace, z.Name, verb)
}

// defaultActionMaxRetries mirrors the RetryPolicy.maxRetries CRD default, as the FleetAction
// controller's own constant does. A FleetAction with no spec.retryPolicy is requeued after a
// failure until its retryCount reaches this.
const defaultActionMaxRetries int32 = 3

// maxNamedReferences caps how many referencing objects one refusal names.
const maxNamedReferences = 10

// ValidateDelete refuses a delete that would leave something referring to a zone that no
// longer exists. It is one rule, applied to four kinds of reference (RFC-0001
// #fleetzone-deleting-a-zone):
//
//   - child: a FleetZone whose spec.parentZone names the zone;
//   - declared: a Robot whose spec.zone names it;
//   - present: a Robot whose status.currentZone names it. The declared check does not
//     cover this: a robot declared in zone A can be inside this zone's polygon;
//   - targeted: a FleetAction whose spec.zone names it and which can still run.
//
// Each refusal names the kind of reference and the objects that hold it, so the operator
// knows what to move. Every read goes through APIReader. Namespace teardown is exempt (see
// namespaceTerminating).
//
// Two limits are stated in the spec rather than closed here. A robot that has entered the
// zone but whose position has not yet been derived is not present to this check. Two
// deletes in flight together, a zone and its parent, can each see the other still there.
func (v *FleetZoneValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	zoneObj, ok := obj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", obj)
	}
	if v.APIReader == nil {
		return nil, apierrors.NewInternalError(fmt.Errorf(
			"the FleetZone delete check has no API reader configured; refusing to delete %q unchecked",
			zoneObj.Name))
	}
	if err := v.refuseDeleteOfAStoppedZone(ctx, zoneObj); err != nil {
		return nil, err
	}
	refs, err := v.zoneReferences(ctx, zoneObj.Namespace, zoneObj.Name)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	terminating, err := namespaceTerminating(ctx, v.APIReader, zoneObj.Namespace)
	if err != nil {
		return nil, err
	}
	if terminating {
		return nil, nil
	}
	return nil, apierrors.NewInvalid(fleetZoneGK, zoneObj.Name, refs)
}

// refuseDeleteOfAStoppedZone refuses, to every identity, the delete of a zone that carries an
// emergency stop, the swarmada.io/estop-triggered annotation or the processed marker that stands
// until the stop's clear has been applied, as the SwarmadaConfig delete guard does for the
// namespace's stop, and the delete of a zone below one that does. A delete ends the stop with no
// clear and no ESTOP_CLEARED audit entry: a stop still being applied never reaches its robots, and
// a zone created again under the same name, or elsewhere, holds none of the robots that enter it.
// A stop is released only through its clear. A delete is admitted while the namespace itself is
// being deleted (namespaceTerminating).
func (v *FleetZoneValidator) refuseDeleteOfAStoppedZone(ctx context.Context, z *fleetv1.FleetZone) error {
	_, triggered := z.Annotations[estopTriggeredAnnotation]
	_, marked := z.Annotations[estopProcessedAnnotation]
	above, err := estopscope.StopsOver(ctx, v.APIReader, z.Namespace, z.Spec.ParentZone, false, estopscope.Carries)
	if err != nil {
		return err
	}
	if !triggered && !marked && len(above) == 0 {
		return nil
	}
	terminating, err := namespaceTerminating(ctx, v.APIReader, z.Namespace)
	if err != nil || terminating {
		return err
	}
	// A zone below a stopped zone, deleted and created again elsewhere, leaves that stop's reach
	// as a re-parent does (refuseNarrowingAStop).
	if err := refuseNarrowing(z.Namespace, z.Name, fmt.Sprintf("deleting zone %q", z.Name), above); err != nil {
		return err
	}
	if !triggered && !marked {
		return nil
	}
	why := fmt.Sprintf("it carries an emergency stop (%s)", estopTriggeredAnnotation)
	next := fmt.Sprintf("clear it first, which the estop-clear verb authorizes: swarmctl estop-clear %s "+
		"--reason <text> -n %s; its robots then move to Resuming and wait for swarmctl estop resume", z.Name, z.Namespace)
	if !triggered {
		why = fmt.Sprintf("its emergency stop's clear has not been applied yet (it carries %s)", estopProcessedAnnotation)
		next = fmt.Sprintf("its clear was issued and the control plane is applying it; retry once the zone "+
			"carries neither %s nor %s", estopTriggeredAnnotation, estopProcessedAnnotation)
	}
	return apierrors.NewForbidden(fleetZoneGR, z.Name, fmt.Errorf(
		"FleetZone %q in namespace %q cannot be deleted while %s: a delete would end the stop without a clear "+
			"and without an audit entry, and a zone created again under the same name would not stop the robots "+
			"that enter it. A stop is released only through its clear: %s. A delete is admitted while the "+
			"namespace itself is being deleted (RFC-0001 #safety-estop-scopes)",
		z.Name, z.Namespace, why, next))
}

// zoneReferences returns one error per kind of reference that still holds the zone.
func (v *FleetZoneValidator) zoneReferences(ctx context.Context, namespace, name string) (field.ErrorList, error) {
	var errs field.ErrorList
	namePath := field.NewPath("metadata").Child("name")

	children, err := childZonesOf(ctx, v.APIReader, namespace, name)
	if err != nil {
		return nil, err
	}
	if len(children) > 0 {
		errs = append(errs, field.Forbidden(namePath,
			fmt.Sprintf("FleetZone %q has child zones %v (spec.parentZone); delete or re-parent them first",
				name, children)))
	}

	var robots fleetv1.RobotList
	if err := v.APIReader.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Robots in %q: %w", namespace, err)
	}
	var declared, present []string
	for i := range robots.Items {
		r := &robots.Items[i]
		if r.Spec.Zone == name {
			declared = append(declared, r.Name)
		}
		if r.Status.CurrentZone == name {
			present = append(present, r.Name)
		}
	}
	if len(declared) > 0 {
		errs = append(errs, field.Forbidden(namePath,
			fmt.Sprintf("FleetZone %q is declared by robots %s (spec.zone); re-home or delete them first",
				name, namedReferences(declared))))
	}
	if len(present) > 0 {
		errs = append(errs, field.Forbidden(namePath,
			fmt.Sprintf("FleetZone %q has robots present in it %s (status.currentZone); "+
				"move them out of the zone or delete them first",
				name, namedReferences(present))))
	}

	var actions fleetv1.FleetActionList
	if err := v.APIReader.List(ctx, &actions, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing FleetActions in %q: %w", namespace, err)
	}
	var targeted []string
	for i := range actions.Items {
		a := &actions.Items[i]
		if a.Spec.Zone == name && actionCanStillRun(a) {
			targeted = append(targeted, a.Name)
		}
	}
	if len(targeted) > 0 {
		errs = append(errs, field.Forbidden(namePath,
			fmt.Sprintf("FleetZone %q is targeted by FleetActions that can still run %s (spec.zone); "+
				"cancel them or let them finish first",
				name, namedReferences(targeted))))
	}
	return errs, nil
}

// namedReferences renders a sorted list of object names, capped at maxNamedReferences.
func namedReferences(names []string) string {
	sort.Strings(names)
	if len(names) <= maxNamedReferences {
		return fmt.Sprintf("%v", names)
	}
	return fmt.Sprintf("%v and %d more", names[:maxNamedReferences], len(names)-maxNamedReferences)
}

// actionCanStillRun reports whether a FleetAction can still dispatch work into its zone. The
// terminal set is the FleetAction state machine's (RFC-0001 #fleetaction-state-machine):
// Succeeded and Cancelled have no outgoing edge. Failed has one, back to Pending, when
// onFailure is Requeue, the action was actually assigned, and retries remain; the condition
// is the FleetAction controller's. Every other phase can still run, including the empty
// phase of an action no controller has reconciled yet.
func actionCanStillRun(a *fleetv1.FleetAction) bool {
	switch a.Status.Phase {
	case fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseCancelled:
		return false
	case fleetv1.ActionPhaseFailed:
		return failedActionMayRequeue(a)
	default:
		return true
	}
}

// failedActionMayRequeue reports whether a Failed action still has its Failed -> Pending edge.
func failedActionMayRequeue(a *fleetv1.FleetAction) bool {
	policy := a.Spec.OnFailure
	if policy == "" {
		policy = fleetv1.ActionFailureRequeue // CRD default; guards un-defaulted objects.
	}
	if policy != fleetv1.ActionFailureRequeue || a.Status.ScheduledAt == nil {
		return false
	}
	maxRetries := defaultActionMaxRetries
	if p := a.Spec.RetryPolicy; p != nil {
		maxRetries = p.MaxRetries
	}
	return a.Status.RetryCount < maxRetries
}

// namespaceTerminating reports whether the namespace is being deleted. Namespace teardown is
// exempt from the delete check. The operator has already decided to delete everything in the
// namespace, and a refusal would leave it in Terminating until something else removed the
// references.
//
// The signal is the Namespace's own deletionTimestamp, read uncached. The API server sets it
// before the namespace controller deletes anything in the namespace, and nothing clears it,
// so it cannot exempt a delete in a namespace that is staying. The identity making the
// request is deliberately not the signal: which user the namespace controller deletes as
// depends on how the cluster runs kube-controller-manager. Any delete in a terminating
// namespace is exempt, including an operator's own, because every object in it is going
// anyway.
//
// A namespace that cannot be read fails closed: the delete is refused.
func namespaceTerminating(ctx context.Context, r client.Reader, namespace string) (bool, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading namespace %q to check for teardown: %w", namespace, err)
	}
	return ns.DeletionTimestamp != nil, nil
}

func (v *FleetZoneValidator) validate(ctx context.Context, z *fleetv1.FleetZone) (admission.Warnings, error) {
	var errs field.ErrorList
	parentPath := field.NewPath("spec").Child("parentZone")

	if z.Spec.ParentZone != "" {
		switch {
		case z.Spec.ParentZone == z.Name:
			errs = append(errs, field.Invalid(parentPath, z.Spec.ParentZone,
				"a FleetZone cannot be its own parentZone"))
		case v.APIReader == nil:
			return nil, fmt.Errorf("FleetZone validator has no uncached reader; refusing to admit "+
				"%s/%s with parentZone %q unvalidated", z.Namespace, z.Name, z.Spec.ParentZone)
		default:
			var parent fleetv1.FleetZone
			key := client.ObjectKey{Namespace: z.Namespace, Name: z.Spec.ParentZone}
			if err := v.APIReader.Get(ctx, key, &parent); err != nil {
				if apierrors.IsNotFound(err) {
					errs = append(errs, field.Invalid(parentPath, z.Spec.ParentZone,
						fmt.Sprintf("parentZone %q not found in namespace %q", z.Spec.ParentZone, z.Namespace)))
				} else {
					return nil, fmt.Errorf("resolving parentZone %q: %w", z.Spec.ParentZone, err)
				}
			} else if ancestors, problem, err := v.walkAncestors(ctx, z); err != nil {
				return nil, err
			} else if problem != "" {
				errs = append(errs, field.Invalid(parentPath, z.Spec.ParentZone, problem))
			} else {
				// FZ-R7, over the chain the cycle walk just produced.
				errs = append(errs, restrictionWeakeningErrors(z, ancestors)...)
			}
		}
	}

	if pb := z.Spec.PhysicalBounds; pb != nil && zone.SelfIntersects(toZonePoints(pb.Polygon)) {
		errs = append(errs, field.Invalid(
			field.NewPath("spec").Child("physicalBounds").Child("polygon"), "<polygon>",
			"polygon is self-intersecting; a zone boundary must be a simple polygon"))
	}

	if len(errs) == 0 {
		return occupantActionWarnings(z), nil
	}
	return occupantActionWarnings(z), apierrors.NewInvalid(fleetZoneGK, z.Name, errs)
}

// occupantActionWarnings discloses that entry.occupantAction values other than
// Report are INERT at v0.4 (ADR-0058 §10).
//
// Graceful and Immediate are in the schema because the amendment's table specifies
// all three and marks two RESERVED — a reader of the CRD must be able to see the
// shape of the decision. Neither is implemented: only Report is, and Report moves no
// robot at all.
//
// # Why a warning and not a rejection
//
// Rejecting a value the published enum documents would make the CRD contradict
// itself, and would mean a manifest written against the specification is refused by
// the implementation of that specification. Accepting it in silence is worse, and is
// the thing the prompt for this work named: an operator sets
// occupantAction: Immediate, believes robots will be pulled out of a zone they just
// closed, and nothing happens — a safety expectation created by a field that does
// nothing. A warning is the one option that neither lies nor refuses: the write
// succeeds, kubectl prints the disclosure, and the operator learns at the moment they
// set it.
//
// It is emitted on create AND update, because the value can be introduced by either.
func occupantActionWarnings(z *fleetv1.FleetZone) admission.Warnings {
	if z.Spec.Restrictions == nil || z.Spec.Restrictions.Entry == nil {
		return nil
	}
	switch z.Spec.Restrictions.Entry.OccupantAction {
	case fleetv1.ZoneOccupantGraceful, fleetv1.ZoneOccupantImmediate:
		// THE DISCLOSURE MUST NOT ITSELF OVER-CLAIM, and it has been wrong in both
		// directions, which is why the wording is spelled out here rather than trusted.
		//
		// It once told the operator they WOULD be told which robots were inside, by a
		// ZoneEntryDeniedWithOccupants event that was emitted nowhere — the same defect
		// this warning exists to prevent, one size smaller, and worse than silence
		// because a warning is read as the authoritative statement of the limit. That
		// clause was struck. It then said no event names them, which became false when
		// the producer landed (internal/controller/zone_occupant_report.go).
		//
		// What is true of ALL THREE values is: no robot is moved, and the occupants are
		// named once. So the warning's job is narrower than it was — it discloses only
		// that the MOTION these two values promise does not happen, and says the
		// reporting does. It still does not credit this field with withholding new work:
		// an effective entry denial withholds it through candidate filter 11 whatever
		// occupantAction says.
		return admission.Warnings{fmt.Sprintf(
			"spec.restrictions.entry.occupantAction: %q is SPECIFIED BUT NOT IMPLEMENTED at v0.4 "+
				"and its MOTION is inert: no robot will be moved, wound down or requeued by it. "+
				"The zone behaves as occupantAction: Report, which at v0.4 means exactly one "+
				"thing — a single ZoneEntryDeniedWithOccupants warning event NAMING the robots "+
				"that were inside when the denial took effect. That is a report, not a "+
				"clearance. (New work IS withheld from a denied zone, by the entry denial "+
				"itself, whatever this field says.) Do not rely on this field to clear a zone.",
			z.Spec.Restrictions.Entry.OccupantAction)}
	}
	return nil
}

// walkAncestors climbs the parentZone chain upward from z ONCE, through the uncached
// APIReader (validate has checked it is set), and returns both
// answers that walk can give: the ancestors it visited (nearest first), and a
// non-empty cycle path if the chain revisits a zone — back to z itself, a
// pre-existing loop, or an over-deep hierarchy. A missing ancestor simply ends the
// chain (no cycle); the direct-parent existence check is separate.
//
// It returns the ancestors rather than discarding them because FZ-R7 needs exactly
// the chain this walk already produces. Adding a second walk for the restriction
// check would double the Gets on every FleetZone write and would let the two
// disagree about what an ancestor is — about a depth cap, say, or a missing link.
//
// Ancestors are returned even alongside a cycle: the caller reports the cycle and
// skips the restriction comparison, but a partial chain is still what was read.
func (v *FleetZoneValidator) walkAncestors(ctx context.Context, z *fleetv1.FleetZone) ([]fleetv1.FleetZone, string, error) {
	var ancestors []fleetv1.FleetZone
	visited := map[string]bool{z.Name: true}
	pathNames := []string{z.Name}
	current := z.Spec.ParentZone
	for depth := 0; current != "" && depth < maxZoneAncestryDepth; depth++ {
		pathNames = append(pathNames, current)
		if visited[current] {
			return ancestors, fmt.Sprintf("parentZone chain forms a cycle: %s", joinPath(pathNames)), nil
		}
		visited[current] = true

		var parent fleetv1.FleetZone
		if err := v.APIReader.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: current}, &parent); err != nil {
			if apierrors.IsNotFound(err) {
				return ancestors, "", nil // chain ends at a missing ancestor — no cycle
			}
			return nil, "", fmt.Errorf("walking parentZone chain at %q: %w", current, err)
		}
		ancestors = append(ancestors, parent)
		current = parent.Spec.ParentZone
	}
	if current != "" {
		// Still climbing after the bound: too deep for a stop on the root to reach the zone.
		return ancestors, tooDeepMessage(z.Name, maxZoneAncestryDepth+1), nil
	}
	return ancestors, "", nil
}

// childZonesOf lists the zones naming `name` as their parent. Shared with the Robot
// admission gate, which needs the same answer to decide whether a zone is a leaf. Both
// callers pass their uncached APIReader.
//
// Leafness is computed from the tree, not read from FleetZone.status.isLeaf, on purpose.
// The status field is written by the Zone Controller, so a robot created moments after its
// zone would be judged against a status that has not been written yet — admission would
// reject a correct object because a controller had not caught up. Deriving it here also
// produces the child list the rejection message is specified to carry.
func childZonesOf(ctx context.Context, c client.Reader, namespace, name string) ([]string, error) {
	var zones fleetv1.FleetZoneList
	if err := c.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing FleetZones in %q: %w", namespace, err)
	}
	var children []string
	for i := range zones.Items {
		if zones.Items[i].Spec.ParentZone == name {
			children = append(children, zones.Items[i].Name)
		}
	}
	sort.Strings(children)
	return children, nil
}

func toZonePoints(pts []fleetv1.Point) []zone.Point {
	out := make([]zone.Point, len(pts))
	for i, p := range pts {
		out[i] = zone.Point{X: p.X, Y: p.Y}
	}
	return out
}

func joinPath(names []string) string {
	s := ""
	for i, n := range names {
		if i > 0 {
			s += " → "
		}
		s += n
	}
	return s
}
