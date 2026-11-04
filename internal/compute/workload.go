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

package compute

import (
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// StampedServiceAccountName is the fixed, low-privilege ServiceAccount every
// controller-created workload runs under, regardless of what its template
// requested (the second containment rule: "the controller stamps a
// fixed, low-privilege ServiceAccount and MUST refuse one named in the
// template"). It MUST already exist in the run's namespace; this package does
// not create it.
const StampedServiceAccountName = "swarmada-compute-runtime"

// runLabelKey labels a controller-created workload's pod template with the
// run it belongs to, and doubles as the Deployment mode's pod selector — each
// run gets its own label value, so two concurrent runs' Deployments never
// select each other's pods.
const runLabelKey = "swarmada.io/compute-run"

// ErrTemplateNamesServiceAccount is returned when a ComputeWorkloadTemplate's
// PodTemplate names its own ServiceAccount. The CEL rule on
// ComputeWorkloadTemplateSpec already rejects this at admission;
// this is defense in depth against a template that predates the rule or was
// admitted under a relaxed schema, so the controller NEVER trusts admission as
// the only gate for a privilege-escalation-relevant check.
var ErrTemplateNamesServiceAccount = errors.New("template must not name a ServiceAccount — the controller stamps one")

// BuildWorkload constructs the Kubernetes object provision: workload creates
// for one run (mode 3): a Job for WorkloadTemplateModeJob, a
// Deployment for WorkloadTemplateModeDeployment. It does not create the
// object — the caller does, via the client, so a caller can inspect or
// dry-run it first.
//
// Containment, all enforced here regardless of what admission
// already checked:
//   - the returned object's Namespace is ALWAYS owner.GetNamespace() — never
//     settable by a caller — so workload creation is namespace-scoped, never
//     cluster-wide;
//   - the pod template's ServiceAccountName is ALWAYS overwritten to
//     StampedServiceAccountName; a template naming its own is refused
//     (ErrTemplateNamesServiceAccount), not silently overridden;
//   - the pod spec is refused outright if it requests anything on the
//     host-escape / privilege-acquisition deny-list — hostPath, a host
//     namespace, privileged mode, added capabilities, an unmasked /proc, a host
//     port (ValidatePodTemplate, podpolicy.go). The manager creates this object
//     under its OWN ServiceAccount, so without this, create on
//     computeworkloadtemplates would be create on privileged pods;
//   - the object carries an owner reference to owner (controller=true,
//     blockOwnerDeletion=true) via controllerutil.SetControllerReference, so
//     Kubernetes' OWN garbage collector reclaims it on the owner's deletion —
//     this does not depend on the Swarmada controller-manager being alive at
//     that moment (the "garbage collection is the duty cycle" property).
func BuildWorkload(template *fleetv1.ComputeWorkloadTemplate, owner client.Object, scheme *runtime.Scheme, name string) (client.Object, error) {
	if template.Spec.PodTemplate.Spec.ServiceAccountName != "" || template.Spec.PodTemplate.Spec.DeprecatedServiceAccount != "" {
		return nil, fmt.Errorf("%w: template %q", ErrTemplateNamesServiceAccount, template.Name)
	}
	// The rest of the containment policy (podpolicy.go), re-run at construction for
	// the same reason the ServiceAccount check above is: this must never trust
	// admission as its only gate. A template admitted under an older schema, edited
	// through a bypassed webhook, or written while the CRD's CEL rules were missing
	// reaches here — and is refused, not silently sanitised. Refusal and override
	// differ in what an administrator learns.
	if err := ValidatePodTemplate(&template.Spec.PodTemplate.Spec); err != nil {
		return nil, fmt.Errorf("computeworkloadtemplate %q: %w", template.Name, err)
	}

	podTemplate := *template.Spec.PodTemplate.DeepCopy()
	podTemplate.Spec.ServiceAccountName = StampedServiceAccountName
	podTemplate.Spec.DeprecatedServiceAccount = "" // clear the deprecated alias too.
	if podTemplate.Labels == nil {
		podTemplate.Labels = map[string]string{}
	}
	podTemplate.Labels[runLabelKey] = name

	meta := metav1.ObjectMeta{
		Name:      name,
		Namespace: owner.GetNamespace(), // ALWAYS the owner's namespace — never cluster-wide.
	}

	var obj client.Object
	switch template.Spec.Mode {
	case fleetv1.WorkloadTemplateModeJob:
		obj = &batchv1.Job{
			ObjectMeta: meta,
			Spec:       batchv1.JobSpec{Template: podTemplate},
		}
	case fleetv1.WorkloadTemplateModeDeployment:
		replicas := int32(1)
		obj = &appsv1.Deployment{
			ObjectMeta: meta,
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{runLabelKey: name}},
				Template: podTemplate,
			},
		}
	default:
		return nil, fmt.Errorf("computeworkloadtemplate %q: unknown mode %q", template.Name, template.Spec.Mode)
	}

	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference on %s/%s: %w", owner.GetNamespace(), name, err)
	}
	return obj, nil
}
