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
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/compute"
)

// computeWorkloadTemplateGK is the GroupKind used in Invalid responses.
var computeWorkloadTemplateGK = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "ComputeWorkloadTemplate"}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-computeworkloadtemplate,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=computeworkloadtemplates,verbs=create;update,versions=v1,name=vcomputeworkloadtemplate.swarmada.io,admissionReviewVersions=v1

// ComputeWorkloadTemplateValidator refuses a template whose pod spec could escape
// to the node — the container half of the containment policy the CRD's CEL rules
// cannot express.
//
// WHY A WEBHOOK RATHER THAN CEL, since CEL is otherwise the better instrument
// (it holds for every client of the API server with no dependency on this process
// being alive): ComputeWorkloadTemplate embeds a vendored corev1.PodTemplateSpec,
// whose containers/initContainers/ports lists declare no maxItems and cannot be
// given one without forking the type. A CEL rule walking them is refused by the
// API server — "estimated rule cost exceeds budget by factor of more than 100x" —
// and the CRD then does not install AT ALL. That is measured against a real API
// server, not assumed. The pod-level scalars that DO fit stayed in CEL
// (api/v1/computeworkloadtemplate_types.go); only the list-walking half is here.
//
// It runs failurePolicy=Fail, so an unreachable webhook refuses the write rather
// than admitting an unchecked template. That is the correct direction for a
// privilege-escalation gate and the opposite of the estop-actor defaulters, which
// are deliberately failurePolicy=Ignore because attribution must never block a
// safe stop. Nothing here is on a safety path: refusing a compute template delays
// a batch job.
//
// The same check runs again in compute.BuildWorkload at construction, and both
// call one function — a webhook that is down, misconfigured, or bypassed never
// leaves a template unchecked, and the two sites cannot drift because they are
// not two copies of the rules.
type ComputeWorkloadTemplateValidator struct{}

// SetupWebhookWithManager registers the validator with the manager's webhook server.
func (v *ComputeWorkloadTemplateValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.ComputeWorkloadTemplate{}).
		WithValidator(v).
		Complete()
}

var _ webhook.CustomValidator = &ComputeWorkloadTemplateValidator{}

// ValidateCreate refuses an escalating pod spec at admission.
func (v *ComputeWorkloadTemplateValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, validateComputeWorkloadTemplate(obj)
}

// ValidateUpdate applies the identical check. An update is the obvious way past a
// create-only gate: admit a benign template, then patch hostPath into it.
func (v *ComputeWorkloadTemplateValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return nil, validateComputeWorkloadTemplate(newObj)
}

// ValidateDelete permits deletion. A template is reference-only configuration and
// removing one can start nothing.
func (v *ComputeWorkloadTemplateValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateComputeWorkloadTemplate runs the shared policy and shapes the refusal as
// a field error, so an administrator is told which field and which rule.
func validateComputeWorkloadTemplate(obj runtime.Object) error {
	t, ok := obj.(*fleetv1.ComputeWorkloadTemplate)
	if !ok {
		// Fail closed on an object this validator cannot inspect. A type it does not
		// recognise is not an object it may vouch for.
		return fmt.Errorf("expected a ComputeWorkloadTemplate object but got %T", obj)
	}
	if err := compute.ValidatePodTemplate(&t.Spec.PodTemplate.Spec); err != nil {
		return apierrors.NewInvalid(computeWorkloadTemplateGK, t.Name, field.ErrorList{
			field.Invalid(field.NewPath("spec", "podTemplate"), "<pod spec>", err.Error()),
		})
	}
	return nil
}
