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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The estop actor guard.
//
// swarmada.io/estop-actor is the only surviving record of who triggered or cleared a stop
// (ADR-0046). Two writers own it: the mutating stamper writes the authenticated requester
// when a stop's trigger changes (estop_actor_defaulter.go), and the controller removes it
// once it has sealed the edge it attributes (internal/controller: consumeEstopActor). Nothing
// refused any other write, so a client could rewrite the actor of a stop already in force, or
// leave a value between edges for the next edge whose stamp was skipped. The controller seals
// that value as a user.
//
// guardEstopActor admits exactly the two owners' writes, for every identity:
//
//   - at a change of a key the stamper stamps on (stampKeys): no actor, or the requester's
//     own authenticated username. That is all the stamper ever writes, so a request it served
//     always passes, and one it skipped passes whenever the client left the actor alone: a
//     stop is never refused because the stamp was skipped;
//   - anywhere else: the removal of the actor by the controller's own ServiceAccount.
//
// Every other change is refused, the controller's included: it removes the stamp and never
// writes one.
//
// Like the marker guard, it lives in the validating webhooks (failurePolicy=Fail), not in
// the stampers (failurePolicy=Ignore): a guard that an unreachable webhook server silently
// disables is not a guard.

// estopActorStampKeys are the keys whose change the stamper stamps on, on a FleetZone and a
// SwarmadaConfig (stampEstopActor). They must stay the keys the stampers use, or the guard
// refuses the stampers' own writes.
var estopActorStampKeys = []string{estopTriggeredAnnotation}

// robotEstopActorStampKeys adds, on a Robot, the operator's release request
// (RobotDefaulter.Default).
var robotEstopActorStampKeys = []string{estopTriggeredAnnotation, fleetv1.EstopResumeAnnotation}

// guardEstopActor refuses a change to swarmada.io/estop-actor that is neither the stamper's
// stamp at a change of one of stampKeys nor the controller's removal. oldAnn is nil on a
// create. A write that leaves the actor unchanged is admitted.
func guardEstopActor(ctx context.Context, gr schema.GroupResource, namespace, name string, oldAnn, newAnn map[string]string, stampKeys []string) error {
	oldVal, had := oldAnn[AnnEstopActor]
	newVal, has := newAnn[AnnEstopActor]
	if had == has && oldVal == newVal {
		return nil // the actor is untouched by this request
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(gr, name,
			fmt.Errorf("cannot attribute a change of %q: no admission request in context", AnnEstopActor))
	}
	user := req.UserInfo.Username
	if anyAnnotationChanged(oldAnn, newAnn, stampKeys) {
		if !has || (user != "" && newVal == user) {
			return nil
		}
	} else if !has && isControllerServiceAccount(user) {
		return nil
	}
	return apierrors.NewForbidden(gr, name,
		fmt.Errorf("user %q may not change %q on %s %q in namespace %q: it is stamped by the control plane with "+
			"the authenticated requester when the stop changes, and removed by the control plane once that "+
			"change is recorded (RFC-0001 #security-audit-logging)",
			user, AnnEstopActor, gr.Resource, name, namespace))
}

// anyAnnotationChanged reports whether any of keys was added, removed or re-valued between
// oldAnn and newAnn, as the stamper's transitioned does.
func anyAnnotationChanged(oldAnn, newAnn map[string]string, keys []string) bool {
	for _, k := range keys {
		ov, oh := oldAnn[k]
		nv, nh := newAnn[k]
		if oh != nh || ov != nv {
			return true
		}
	}
	return false
}
