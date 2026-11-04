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

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// FleetActionDeletePath is where the FleetAction delete guard is served.
const FleetActionDeletePath = "/validate-swarmada-io-v1-fleetaction-delete"

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-fleetaction-delete,mutating=false,failurePolicy=ignore,sideEffects=None,groups=swarmada.io,resources=fleetactions,verbs=delete,versions=v1,name=vfleetactiondelete.swarmada.io,admissionReviewVersions=v1

// FleetActionDeleteGuard refuses the delete of a FleetAction that a robot under a stop records,
// unless the requester holds estop-clear on every stop that covers that robot: deleted, the work
// could be created again under its name and given to another robot while the stop stands (RFC-0001
// #safety-fleetaction-behaviour-during-estop). A ServiceAccount never qualifies, whatever its roles
// grant. An admitted delete leaves the action Terminating with the stop finalizer until the stop no
// longer holds the robot.
//
// failurePolicy=Ignore, like the Robot delete guard: a manager outage must not block a namespace
// delete. A delete admitted unchecked in an outage is kept by the same finalizer. The manager's
// principal, the garbage collector deleting a FleetTask's member, and a namespace being deleted are
// admitted. The garbage collector's delete follows the task's own delete, which is checked; the
// task's held members are then kept by the stop finalizer, so its delete is admitted. Any other
// delete by the garbage collector is not that cascade, and is checked as a ServiceAccount's.
type FleetActionDeleteGuard struct {
	// APIReader reads the robots, zones and namespace uncached.
	APIReader client.Reader
	// EstopAuthz answers whether the requester holds estop-clear on a stop. Nil refuses.
	EstopAuthz VerbAuthorizer
}

// SetupWebhookWithManager serves the guard at FleetActionDeletePath.
func (g *FleetActionDeleteGuard) SetupWebhookWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register(FleetActionDeletePath,
		admission.WithCustomValidator(mgr.GetScheme(), &fleetv1.FleetAction{}, g))
	return nil
}

var _ webhook.CustomValidator = &FleetActionDeleteGuard{}

// ValidateCreate is not served on this path.
func (g *FleetActionDeleteGuard) ValidateCreate(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate is not served on this path.
func (g *FleetActionDeleteGuard) ValidateUpdate(context.Context, runtime.Object, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete refuses the delete of held work to anyone without estop-clear on its stop.
func (g *FleetActionDeleteGuard) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	action, ok := obj.(*fleetv1.FleetAction)
	if !ok {
		return nil, fmt.Errorf("expected a FleetAction object but got %T", obj)
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewForbidden(fleetTaskGR, action.Name, fmt.Errorf("cannot authorize the delete: no admission request in context"))
	}
	if isControllerServiceAccount(req.UserInfo.Username) ||
		isGarbageCollector(req.UserInfo.Username) && isFleetTaskControllerRef(metav1.GetControllerOfNoCopy(action)) {
		return nil, nil
	}
	return nil, refuseMovingHeldWork(ctx, g.APIReader, g.EstopAuthz, req.UserInfo, action, "delete")
}

// refuseMovingHeldWork refuses what (a delete of action, or the removal of its stop finalizer) to a
// requester without estop-clear on every stop that covers a robot recording the action, and to a
// ServiceAccount or the garbage collector whatever its roles grant. A namespace being deleted is
// exempt.
func refuseMovingHeldWork(ctx context.Context, rd client.Reader, authz VerbAuthorizer, user authenticationv1.UserInfo,
	action *fleetv1.FleetAction, what string) error {
	if rd == nil {
		return apierrors.NewForbidden(fleetTaskGR, action.Name, fmt.Errorf("the held-work check has no reader; refusing to %s FleetAction %q unchecked", what, action.Name))
	}
	var list fleetv1.RobotList
	if err := rd.List(ctx, &list, client.InNamespace(action.Namespace)); err != nil {
		return apierrors.NewInternalError(fmt.Errorf("listing robots for the held-work check: %w", err))
	}
	for i := range list.Items {
		robot := &list.Items[i]
		if robot.Status.AssignedAction != action.Name {
			continue
		}
		cov, err := estopscope.Covering(ctx, rd, robot)
		if err != nil {
			return apierrors.NewInternalError(fmt.Errorf("deciding which stops cover robot %q: %w", robot.Name, err))
		}
		if !cov.Covered() {
			continue
		}
		if terminating, err := namespaceTerminating(ctx, rd, action.Namespace); err != nil || terminating {
			return err
		}
		if isServiceAccountPrincipal(user.Username, user.Groups) || isGarbageCollector(user.Username) {
			s := cov.Stops[0]
			return apierrors.NewForbidden(fleetTaskGR, action.Name, fmt.Errorf(
				"user %q may not %s FleetAction %q: robot %q, under the emergency stop of %s %q, records it and may "+
					"still run it, and a ServiceAccount never decides where held work goes, whatever its roles grant. %s "+
					"(RFC-0001 #safety-fleetaction-behaviour-during-estop)",
				user.Username, what, action.Name, robot.Name, s.Kind, s.Name, whoDecides(action, s)))
		}
		for _, s := range cov.Stops {
			if allowed, err := holdsEstopClear(ctx, authz, user, action.Namespace, s); err != nil {
				return err
			} else if !allowed {
				return apierrors.NewForbidden(fleetTaskGR, action.Name, fmt.Errorf(
					"user %q may not %s FleetAction %q: robot %q, under the emergency stop of %s %q, records it and may "+
						"still run it, and only the authority that clears that stop decides where held work goes. %s "+
						"(RFC-0001 #safety-fleetaction-behaviour-during-estop)",
					user.Username, what, action.Name, robot.Name, s.Kind, s.Name, whoDecides(action, s)))
			}
		}
	}
	return nil
}

// holdsEstopClear asks whether user holds estop-clear on the carrier of stop s. A nil authorizer
// refuses.
func holdsEstopClear(ctx context.Context, authz VerbAuthorizer, user authenticationv1.UserInfo, namespace string,
	s estopscope.Holder) (bool, error) {
	if authz == nil {
		return false, nil
	}
	gr := robotGR
	switch s.Kind {
	case estopscope.KindZone:
		gr = fleetZoneGR
	case estopscope.KindNamespace:
		gr = swarmadaConfigGR
	}
	allowed, err := authz.Authorize(ctx, user, estopClearVerb, gr, namespace, s.Name)
	if err != nil {
		return false, apierrors.NewInternalError(fmt.Errorf("authorizing %q on %s %q: %w", estopClearVerb, gr.Resource, s.Name, err))
	}
	return allowed, nil
}

// whoDecides names, in a refusal, who decides where work a stop holds goes and the exact commands.
func whoDecides(action *fleetv1.FleetAction, s estopscope.Holder) string {
	return fmt.Sprintf("A holder of estop-clear on %s %q (swarmada:admin by default) decides: requeue it to another robot "+
		"(kubectl annotate fleetaction %s swarmada.io/requeue-requested=\"<reason>\" -n %s), cancel it (swarmctl cancel "+
		"action %s --reason \"<text>\" -n %s), or clear the stop and resume the robot", s.Kind, s.Name, action.Name,
		action.Namespace, action.Name, action.Namespace)
}
