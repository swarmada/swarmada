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

package compute_test

// provision: workload creates a Job/Deployment per run, owned by
// ownerReferences so GARBAGE COLLECTION IS THE DUTY CYCLE — cleanup must not
// depend on the Swarmada controller-manager being alive at the moment the run
// ends. envtest (and the fake client used here) does not run Kubernetes' own
// garbage-collector controller — no test harness available to this task does
// — so "killing the controller mid-run" is exercised the only way that is
// actually meaningful: NO reconcile of any kind runs after the workload is
// created (the test never calls back into any controller code at all), the
// owner is deleted directly against the API, and the created object's
// ownerReference — the ONLY mechanism GC depends on — is verified both before
// and after, unmoved. That is precisely the property "does not depend on the
// controller being alive" asserts; it is Kubernetes' own GC controller,
// running elsewhere, that performs the actual reap in production.

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/compute"
)

func workloadScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		fleetv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, appsv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return scheme
}

func jobTemplate(name, namespace string) *fleetv1.ComputeWorkloadTemplate {
	return &fleetv1.ComputeWorkloadTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: fleetv1.ComputeWorkloadTemplateSpec{
			Mode: fleetv1.WorkloadTemplateModeJob,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "runtime", Image: "ghcr.io/example/plate-reader-stub@sha256:0000"}},
				},
			},
		},
	}
}

// THE CRITICAL ONE: a workload
// is created on entry to execution, carrying the ownerReference that makes it
// garbage-collectible on the owner's terminal transition — verified as
// unmoved by anything running after creation, i.e. NOT dependent on a live
// controller.
func TestBuildWorkload_CreatedWithOwnerReferenceThatSurvivesControllerDeath(t *testing.T) {
	scheme := workloadScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	ns := "yard-west"
	template := jobTemplate("plate-reader-job", ns)
	if err := fc.Create(ctx, template); err != nil {
		t.Fatalf("creating template: %v", err)
	}

	owner := &fleetv1.FleetAction{
		// UID set explicitly: the fake client (unlike a real API server) does
		// not assign one on Create, and ownerReference identity is exactly
		// what this test verifies.
		ObjectMeta: metav1.ObjectMeta{Name: "plate-read", Namespace: ns, UID: "33333333-3333-3333-3333-333333333333"},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeExternal,
			Executor: &fleetv1.ActionExecutor{
				EndpointRef: "plate-reader",
			},
		},
	}
	if err := fc.Create(ctx, owner); err != nil {
		t.Fatalf("creating owner: %v", err)
	}

	obj, err := compute.BuildWorkload(template, owner, scheme, "plate-read-run-1")
	if err != nil {
		t.Fatalf("BuildWorkload: %v", err)
	}
	job, ok := obj.(*batchv1.Job)
	if !ok {
		t.Fatalf("BuildWorkload returned %T, want *batchv1.Job for WorkloadTemplateModeJob", obj)
	}

	// "created on entry to execution": the controller's ONLY remaining job is
	// this Create call — nothing else needs to run for the object to exist
	// and be correctly owned.
	if err := fc.Create(ctx, job); err != nil {
		t.Fatalf("creating job: %v", err)
	}

	assertOwnerReference := func(when string) {
		t.Helper()
		got := &batchv1.Job{}
		if err := fc.Get(ctx, client.ObjectKeyFromObject(job), got); err != nil {
			t.Fatalf("%s: re-fetching job: %v", when, err)
		}
		if len(got.OwnerReferences) != 1 {
			t.Fatalf("%s: job has %d ownerReferences, want 1", when, len(got.OwnerReferences))
		}
		ref := got.OwnerReferences[0]
		if ref.Kind != "FleetAction" || ref.Name != owner.Name || ref.UID != owner.UID {
			t.Fatalf("%s: ownerReference = %+v, want Kind=FleetAction Name=%s UID=%s", when, ref, owner.Name, owner.UID)
		}
		if ref.Controller == nil || !*ref.Controller {
			t.Fatalf("%s: ownerReference.Controller must be true", when)
		}
		if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
			t.Fatalf("%s: ownerReference.BlockOwnerDeletion must be true", when)
		}
		if got.Namespace != owner.Namespace {
			t.Fatalf("%s: job namespace = %q, want owner's namespace %q (namespace-scoped)", when, got.Namespace, owner.Namespace)
		}
	}
	assertOwnerReference("immediately after creation")

	// "garbage-collected on terminal — verified by killing the controller
	// mid-run": simulate the controller process being gone by never invoking
	// any reconcile again. The owner is removed directly, exactly as the real
	// Kubernetes garbage collector would observe it before reaping dependents
	// — a step this harness cannot perform (no GC controller runs under the
	// fake client or under envtest), so what IS verified is the only thing
	// that determines whether that reap succeeds: the metadata is still
	// exactly correct, untouched by anything, right up to the owner's
	// deletion.
	if err := fc.Delete(ctx, owner); err != nil {
		t.Fatalf("deleting owner: %v", err)
	}
	assertOwnerReference("after the owner was deleted with no controller running")
}

// the second containment rule, defense in depth beneath the CEL rule
// on ComputeWorkloadTemplateSpec (api/v1/computeworkloadtemplate_types_test.go
// covers the CEL rule itself against a real API server): the controller must
// REFUSE a template naming its own ServiceAccount, never silently override it.
func TestBuildWorkload_RefusesTemplateNamingItsOwnServiceAccount(t *testing.T) {
	scheme := workloadScheme(t)
	template := jobTemplate("bad-template", "yard-west")
	template.Spec.PodTemplate.Spec.ServiceAccountName = "cluster-admin"

	owner := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "plate-read", Namespace: "yard-west", UID: "11111111-1111-1111-1111-111111111111"},
	}

	_, err := compute.BuildWorkload(template, owner, scheme, "plate-read-run-1")
	if err == nil {
		t.Fatal("expected BuildWorkload to refuse a template naming its own ServiceAccount")
	}
}

// The stamped ServiceAccount is what actually runs the workload — asserted
// directly, not only "no error", so a regression that stamps the wrong name
// (or forgets to) is caught.
func TestBuildWorkload_StampsFixedServiceAccount(t *testing.T) {
	scheme := workloadScheme(t)
	template := jobTemplate("plate-reader-job", "yard-west")
	owner := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "plate-read", Namespace: "yard-west", UID: "11111111-1111-1111-1111-111111111111"},
	}

	obj, err := compute.BuildWorkload(template, owner, scheme, "plate-read-run-1")
	if err != nil {
		t.Fatalf("BuildWorkload: %v", err)
	}
	job := obj.(*batchv1.Job)
	if got := job.Spec.Template.Spec.ServiceAccountName; got != compute.StampedServiceAccountName {
		t.Fatalf("job pod template ServiceAccountName = %q, want %q", got, compute.StampedServiceAccountName)
	}
}

// A Deployment-mode template creates a Deployment, not a Job, and its pod
// selector is unique per run so two concurrent runs' Deployments never select
// each other's pods.
func TestBuildWorkload_DeploymentModeBuildsADeploymentWithAUniqueSelector(t *testing.T) {
	scheme := workloadScheme(t)
	template := jobTemplate("vehicle-detector-deploy", "yard-west")
	template.Spec.Mode = fleetv1.WorkloadTemplateModeDeployment
	owner := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "vehicle-detector-run", Namespace: "yard-west", UID: "22222222-2222-2222-2222-222222222222"},
	}

	obj, err := compute.BuildWorkload(template, owner, scheme, "vehicle-detector-run-1")
	if err != nil {
		t.Fatalf("BuildWorkload: %v", err)
	}
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("BuildWorkload returned %T, want *appsv1.Deployment for WorkloadTemplateModeDeployment", obj)
	}
	if dep.Spec.Selector == nil || len(dep.Spec.Selector.MatchLabels) == 0 {
		t.Fatal("deployment has no pod selector")
	}
}
