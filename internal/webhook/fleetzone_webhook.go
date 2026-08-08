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

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/zone"
)

// fleetZoneGK is the GroupKind reported in admission error responses.
var fleetZoneGK = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "FleetZone"}

// fleetZoneGR is the GroupResource used for SubjectAccessReview attributes and
// Forbidden responses on the estop custom verbs.
var fleetZoneGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "fleetzones"}

// maxZoneAncestryDepth bounds the parent-chain walk. A chain longer than this is
// treated as a cycle (or an unusably deep hierarchy) and rejected fail-closed.
const maxZoneAncestryDepth = 64

const (
	// estopTriggeredAnnotation gates a zone-wide emergency stop. It mirrors the
	// controller's annEstopTriggered (internal/controller/zoneestop_controller.go) —
	// the write of this annotation is the estop "verb site" this webhook guards.
	estopTriggeredAnnotation = "swarmada.io/estop-triggered"
	// estopTriggerVerb / estopClearVerb are the FleetZone custom RBAC verbs
	// (security.md §F-2b): swarmada:operator+admin may trigger; only swarmada:admin
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
// cluster's RBAC (the roles/verbs defined in security.md §F-2b / §9.5.3). One
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

// FleetZoneValidator enforces the cross-resource and geometric invariants of the
// FleetZone hierarchy that per-field CEL cannot express (RFC-0001 §9.3.1):
//
//   - a named parentZone must exist in the same namespace, and cannot be the zone
//     itself;
//   - the parent chain must be acyclic (A→B→A is rejected);
//   - spec.physicalBounds.polygon must be a simple (non-self-intersecting) polygon,
//     so containment is well defined;
//   - a zone with child zones cannot be deleted (children would be orphaned).
//
// It runs failurePolicy=Fail: a FleetZone that cannot be validated is not admitted.
type FleetZoneValidator struct {
	Client client.Client
	// EstopAuthz authorizes the estop-trigger / estop-clear custom verbs when the
	// swarmada.io/estop-triggered annotation is written (§F-2b). A nil authorizer
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

// ValidateCreate runs the parent-chain and polygon checks on creation.
func (v *FleetZoneValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	zoneObj, ok := obj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", obj)
	}
	return v.validate(ctx, zoneObj)
}

// ValidateUpdate re-runs the structural checks whenever the parent or polygon may
// have changed, and — when the update writes the estop-triggered annotation —
// authorizes the corresponding estop custom verb (§F-2b).
func (v *FleetZoneValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newZone, ok := newObj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", newObj)
	}
	oldZone, ok := oldObj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", oldObj)
	}
	warns, err := v.validate(ctx, newZone)
	if err != nil {
		return warns, err
	}
	if verb, isEstopChange := estopVerbFor(oldZone, newZone); isEstopChange {
		if err := v.authorizeEstop(ctx, newZone, verb); err != nil {
			return warns, err
		}
	}
	return warns, nil
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
// custom verb on the named resource (§F-2b). Shared by every estop carrier's
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
			fmt.Errorf("user %q is not permitted to %q %s %q (RFC-0001 §F-2b)",
				req.UserInfo.Username, verb, gr.Resource, name))
	}
	return nil
}

// authorizeEstop fails closed unless the admission user is permitted the estop verb.
func (v *FleetZoneValidator) authorizeEstop(ctx context.Context, z *fleetv1.FleetZone, verb string) error {
	return authorizeEstopVerb(ctx, v.EstopAuthz, fleetZoneGR, z.Namespace, z.Name, verb)
}

// ValidateDelete rejects deleting a zone that still has child zones — the children's
// parentZone would dangle. Delete the children (or re-parent them) first.
func (v *FleetZoneValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	zoneObj, ok := obj.(*fleetv1.FleetZone)
	if !ok {
		return nil, fmt.Errorf("expected a FleetZone object but got %T", obj)
	}
	children, err := v.childZones(ctx, zoneObj.Namespace, zoneObj.Name)
	if err != nil {
		return nil, err
	}
	if len(children) > 0 {
		return nil, apierrors.NewInvalid(fleetZoneGK, zoneObj.Name, field.ErrorList{
			field.Forbidden(field.NewPath("metadata").Child("name"),
				fmt.Sprintf("FleetZone %q has child zones %v (spec.parentZone); delete or re-parent them first",
					zoneObj.Name, children)),
		})
	}
	return nil, nil
}

func (v *FleetZoneValidator) validate(ctx context.Context, z *fleetv1.FleetZone) (admission.Warnings, error) {
	var errs field.ErrorList
	parentPath := field.NewPath("spec").Child("parentZone")

	if z.Spec.ParentZone != "" {
		switch {
		case z.Spec.ParentZone == z.Name:
			errs = append(errs, field.Invalid(parentPath, z.Spec.ParentZone,
				"a FleetZone cannot be its own parentZone"))
		default:
			var parent fleetv1.FleetZone
			key := client.ObjectKey{Namespace: z.Namespace, Name: z.Spec.ParentZone}
			if err := v.Client.Get(ctx, key, &parent); err != nil {
				if apierrors.IsNotFound(err) {
					errs = append(errs, field.Invalid(parentPath, z.Spec.ParentZone,
						fmt.Sprintf("parentZone %q not found in namespace %q", z.Spec.ParentZone, z.Namespace)))
				} else {
					return nil, fmt.Errorf("resolving parentZone %q: %w", z.Spec.ParentZone, err)
				}
			} else if cyclePath, err := v.detectCycle(ctx, z); err != nil {
				return nil, err
			} else if cyclePath != "" {
				errs = append(errs, field.Invalid(parentPath, z.Spec.ParentZone,
					fmt.Sprintf("parentZone chain forms a cycle: %s", cyclePath)))
			}
		}
	}

	if pb := z.Spec.PhysicalBounds; pb != nil && zone.SelfIntersects(toZonePoints(pb.Polygon)) {
		errs = append(errs, field.Invalid(
			field.NewPath("spec").Child("physicalBounds").Child("polygon"), "<polygon>",
			"polygon is self-intersecting; a zone boundary must be a simple polygon"))
	}

	if len(errs) == 0 {
		return nil, nil
	}
	return nil, apierrors.NewInvalid(fleetZoneGK, z.Name, errs)
}

// detectCycle walks the parent chain upward from z. It returns a non-empty path
// string (for the error message) if the chain revisits a zone — either back to z
// itself, a pre-existing loop, or an over-deep hierarchy. A missing ancestor simply
// ends the chain (no cycle); the direct-parent existence check is separate.
func (v *FleetZoneValidator) detectCycle(ctx context.Context, z *fleetv1.FleetZone) (string, error) {
	visited := map[string]bool{z.Name: true}
	pathNames := []string{z.Name}
	current := z.Spec.ParentZone
	for depth := 0; current != "" && depth < maxZoneAncestryDepth; depth++ {
		pathNames = append(pathNames, current)
		if visited[current] {
			return joinPath(pathNames), nil
		}
		visited[current] = true

		var parent fleetv1.FleetZone
		if err := v.Client.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: current}, &parent); err != nil {
			if apierrors.IsNotFound(err) {
				return "", nil // chain ends at a missing ancestor — no cycle
			}
			return "", fmt.Errorf("walking parentZone chain at %q: %w", current, err)
		}
		current = parent.Spec.ParentZone
	}
	if current != "" {
		// Still climbing after the depth cap → treat as a cycle / unusable depth.
		return joinPath(append(pathNames, "…")), nil
	}
	return "", nil
}

// childZones returns the names of FleetZones in the namespace whose parentZone is name.
func (v *FleetZoneValidator) childZones(ctx context.Context, namespace, name string) ([]string, error) {
	return childZonesOf(ctx, v.Client, namespace, name)
}

// childZonesOf lists the zones naming `name` as their parent. Shared with the Robot
// admission gate, which needs the same answer to decide whether a zone is a leaf.
//
// Leafness is computed from the tree, not read from FleetZone.status.isLeaf, on purpose.
// The status field is written by the Zone Controller, so a robot created moments after its
// zone would be judged against a status that has not been written yet — admission would
// reject a correct object because a controller had not caught up. Deriving it here also
// produces the child list the rejection message is specified to carry.
func childZonesOf(ctx context.Context, c client.Client, namespace, name string) ([]string, error) {
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
