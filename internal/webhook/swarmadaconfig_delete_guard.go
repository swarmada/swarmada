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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// SwarmadaConfigDeletePath is where the SwarmadaConfig delete guard is served.
const SwarmadaConfigDeletePath = "/validate-swarmada-io-v1-swarmadaconfig-delete"

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-swarmadaconfig-delete,mutating=false,failurePolicy=ignore,sideEffects=None,groups=swarmada.io,resources=swarmadaconfigs,verbs=delete,versions=v1,name=vswarmadaconfigdelete.swarmada.io,admissionReviewVersions=v1

// SwarmadaConfigDeleteGuard refuses to delete a SwarmadaConfig that carries the namespace's
// emergency stop: the swarmada.io/estop-triggered annotation, or the processed marker that
// stands until the stop's clear has been applied. A delete would end the namespace stop
// without the estop-clear verb and without an ESTOP_CLEARED audit entry, and a robot
// created in the namespace afterwards would not be held (RFC-0001 #safety-estop-scopes).
// The refusal holds for every identity and names the clear: removing the trigger, which
// the estop-clear verb authorizes.
//
// One delete is admitted whatever the config carries: one in a namespace that is being
// deleted (namespaceTerminating, the FleetZone delete guard's rule). The robots are going
// with it, so nothing is left for the stop to hold, and a refusal would leave the namespace
// Terminating.
//
// # When the webhook is unavailable
//
// failurePolicy=Ignore, as for the Robot delete guard and for the same reason: DELETE on
// swarmadaconfigs is all this webhook matches, and under Fail a manager outage would leave
// every namespace delete Terminating until a replica came back. The trade: while no replica
// serves, a config carrying the stop can be deleted. The robots the stop already reached
// keep it, because each records it (Robot.status.estopHeldBy); a robot created afterwards
// is not held. The chart renders a literal Ignore, which webhooks.failurePolicy does not
// change.
type SwarmadaConfigDeleteGuard struct {
	// APIReader reads the namespace uncached: a namespace deleted a moment ago must read
	// as terminating. cmd/manager wires the manager's API reader.
	APIReader client.Reader
}

// SetupWebhookWithManager serves the guard at SwarmadaConfigDeletePath. It is registered
// by hand, not through the builder, because the builder's path for SwarmadaConfig is
// vswarmadaconfig's.
func (g *SwarmadaConfigDeleteGuard) SetupWebhookWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register(SwarmadaConfigDeletePath,
		admission.WithCustomValidator(mgr.GetScheme(), &fleetv1.SwarmadaConfig{}, g))
	return nil
}

var _ webhook.CustomValidator = &SwarmadaConfigDeleteGuard{}

// ValidateCreate admits: the guard's configuration matches DELETE only.
func (g *SwarmadaConfigDeleteGuard) ValidateCreate(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate admits: the guard's configuration matches DELETE only.
func (g *SwarmadaConfigDeleteGuard) ValidateUpdate(context.Context, runtime.Object, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete refuses the delete of a config carrying the namespace stop outside a
// terminating namespace.
func (g *SwarmadaConfigDeleteGuard) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cfg, ok := obj.(*fleetv1.SwarmadaConfig)
	if !ok {
		return nil, fmt.Errorf("expected a SwarmadaConfig object but got %T", obj)
	}
	_, triggered := cfg.Annotations[estopTriggeredAnnotation]
	_, marked := cfg.Annotations[estopProcessedAnnotation]
	if !triggered && !marked {
		return nil, nil
	}
	if g.APIReader == nil {
		return nil, fmt.Errorf("SwarmadaConfig delete guard has no reader; refusing to delete %s/%s, which carries "+
			"the namespace emergency stop", cfg.Namespace, cfg.Name)
	}
	terminating, err := namespaceTerminating(ctx, g.APIReader, cfg.Namespace)
	if err != nil {
		return nil, err
	}
	if terminating {
		return nil, nil
	}
	if !triggered {
		// Only the processed marker is left: the clear was issued and is being applied, so there is no
		// trigger to remove, and the next step is to wait for it.
		return nil, apierrors.NewForbidden(swarmadaConfigGR, cfg.Name, fmt.Errorf(
			"SwarmadaConfig %q in namespace %q cannot be deleted while the namespace emergency stop's clear has "+
				"not been applied yet (it carries %s): a delete would end the stop without an authorized clear and "+
				"without an audit entry. Its clear was issued and the control plane is applying it; retry once the "+
				"SwarmadaConfig carries neither %s nor %s. A delete is admitted while the namespace itself is being "+
				"deleted (RFC-0001 #safety-estop-scopes)",
			cfg.Name, cfg.Namespace, estopProcessedAnnotation, estopTriggeredAnnotation, estopProcessedAnnotation))
	}
	why := fmt.Sprintf("it carries the namespace emergency stop (%s)", estopTriggeredAnnotation)
	return nil, apierrors.NewForbidden(swarmadaConfigGR, cfg.Name, fmt.Errorf(
		"SwarmadaConfig %q in namespace %q cannot be deleted while %s: a delete would end the stop without "+
			"an authorized clear and without an audit entry. Clear the stop first, which the estop-clear verb "+
			"authorizes: kubectl annotate swarmadaconfig %s %s- -n %s (swarmctl estop-clear clears a zone's "+
			"stop only); its robots then move to Resuming and wait for swarmctl estop resume. A delete is "+
			"admitted while the namespace itself is being deleted (RFC-0001 #safety-estop-scopes)",
		cfg.Name, cfg.Namespace, why, cfg.Name, estopTriggeredAnnotation, cfg.Namespace))
}
