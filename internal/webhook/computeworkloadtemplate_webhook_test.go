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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func boolp(b bool) *bool { return &b }

func cwt(mutate func(*corev1.PodSpec)) *fleetv1.ComputeWorkloadTemplate {
	pod := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name: "runtime", Image: "ghcr.io/example/plate-reader@sha256:0000",
			SecurityContext: &corev1.SecurityContext{RunAsNonRoot: boolp(true)},
		}},
	}
	if mutate != nil {
		mutate(&pod)
	}
	return &fleetv1.ComputeWorkloadTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
		Spec: fleetv1.ComputeWorkloadTemplateSpec{
			Mode:        fleetv1.WorkloadTemplateModeJob,
			PodTemplate: corev1.PodTemplateSpec{Spec: pod},
		},
	}
}

// The CONTAINER half of the containment policy at admission. These are exactly the
// checks CEL could not carry: a rule walking containers[]/ports[] on a vendored,
// unbounded PodTemplateSpec is refused by the API server for cost, and the CRD then
// does not install at all. So this validator is the admission gate for them, and
// this test is what proves the gate exists — the CEL envtest cannot cover these.
func TestComputeWorkloadTemplateValidator_RefusesPrivilegeAcquisition(t *testing.T) {
	unmasked := corev1.UnmaskedProcMount
	v := &ComputeWorkloadTemplateValidator{}

	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodSpec)
		names  string
		vector string
	}{
		{"privileged container", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Privileged = boolp(true)
		}, "Privileged Containers", "vector 3 — privileged mode"},
		{"privileged init container", func(p *corev1.PodSpec) {
			p.InitContainers = []corev1.Container{{Name: "setup", Image: "x@sha256:0",
				SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}}}
		}, "Privileged Containers", "vector 3, moved one list up the file"},
		{"SYS_ADMIN capability", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Capabilities = &corev1.Capabilities{
				Add: []corev1.Capability{"SYS_ADMIN"}}
		}, "Capabilities", "capability acquisition — privileged mode by another name"},
		{"unmasked /proc", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.ProcMount = &unmasked
		}, "/proc Mount Type", "an unmasked /proc"},
		{"Unconfined seccomp", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeUnconfined}
		}, "Seccomp", "the syscall filter removed"},
		{"host port", func(p *corev1.PodSpec) {
			p.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 80, HostPort: 80}}
		}, "Host Ports", "the node network interface"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := cwt(tc.mutate)

			// CREATE is refused …
			if _, err := v.ValidateCreate(context.Background(), obj); err == nil {
				t.Fatalf("ValidateCreate ADMITTED %s", tc.vector)
			} else if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("refused, but not by the expected rule (want %q): %v", tc.names, err)
			}

			// … and so is UPDATE. Admitting a benign template and then patching the
			// escalation in is the obvious way past a create-only gate, so the update
			// path runs the identical check rather than trusting the create.
			if _, err := v.ValidateUpdate(context.Background(), cwt(nil), obj); err == nil {
				t.Fatalf("ValidateUpdate ADMITTED %s — a create-only gate is bypassed by "+
					"creating a benign template and patching it", tc.vector)
			}
		})
	}
}

// A foreign ServiceAccount is refused here too, so all three of the item's vectors
// are closed at this one gate even for a template that reached it with the CRD's
// CEL rule missing.
func TestComputeWorkloadTemplateValidator_RefusesForeignServiceAccount(t *testing.T) {
	v := &ComputeWorkloadTemplateValidator{}
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodSpec)
	}{
		{"serviceAccountName", func(p *corev1.PodSpec) { p.ServiceAccountName = "cluster-admin" }},
		{"deprecated serviceAccount", func(p *corev1.PodSpec) { p.DeprecatedServiceAccount = "cluster-admin" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), cwt(tc.mutate))
			if err == nil {
				t.Fatal("ADMITTED a template naming its own ServiceAccount — vector 1")
			}
			if !strings.Contains(err.Error(), "stamps") {
				t.Errorf("refusal does not name the stamping rule: %v", err)
			}
		})
	}
}

// An ordinary template is ADMITTED. A gate that refuses everything is an outage,
// not a policy.
func TestComputeWorkloadTemplateValidator_AdmitsAnOrdinaryTemplate(t *testing.T) {
	v := &ComputeWorkloadTemplateValidator{}
	if _, err := v.ValidateCreate(context.Background(), cwt(nil)); err != nil {
		t.Fatalf("an ordinary template was refused: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), cwt(nil), cwt(nil)); err != nil {
		t.Fatalf("an ordinary update was refused: %v", err)
	}
}

// The validator fails CLOSED on an object it cannot inspect. A type it does not
// recognise is not one it may vouch for.
func TestComputeWorkloadTemplateValidator_FailsClosedOnAnUnexpectedType(t *testing.T) {
	v := &ComputeWorkloadTemplateValidator{}
	if _, err := v.ValidateCreate(context.Background(), &fleetv1.Robot{}); err == nil {
		t.Fatal("admitted an object it could not inspect")
	}
}
