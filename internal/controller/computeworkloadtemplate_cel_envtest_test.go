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

package controller

// the second containment rule — "the controller stamps a fixed,
// low-privilege ServiceAccount and MUST refuse one named in the template" —
// and this task's required test: "a template naming its own ServiceAccount is
// rejected". internal/compute/workload_test.go proves BuildWorkload refuses
// one in Go; this proves the SAME rule is enforced by the API server itself,
// at admission, before any controller code runs at all — the
// fleettask_cel_envtest_test.go pattern applied to a new CRD, because a CEL
// rule that silently stopped firing (a typo, a dropped regeneration, a wrong
// path) would look identical to a correct one under a fake client that
// performs no admission whatsoever.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func mkComputeWorkloadTemplate(name, ns, serviceAccountName string) *fleetv1.ComputeWorkloadTemplate {
	return &fleetv1.ComputeWorkloadTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.ComputeWorkloadTemplateSpec{
			Mode: fleetv1.WorkloadTemplateModeJob,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: serviceAccountName,
					Containers:         []corev1.Container{{Name: "runtime", Image: "ghcr.io/example/plate-reader-stub@sha256:0000"}},
				},
			},
		},
	}
}

// THE CRITICAL ONE: a template naming its own ServiceAccount must be
// REJECTED by the API server, not merely disregarded by a well-behaved
// controller.
func TestEnvtest_ComputeWorkloadTemplateRefusesOwnServiceAccount(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	err := envK8s.Create(ctx, mkComputeWorkloadTemplate("bad-template", ns, "cluster-admin"))
	if err == nil {
		t.Fatal("the API server accepted a ComputeWorkloadTemplate naming its own ServiceAccount — " +
			"the CEL XValidation rule containment requires is not in force. A template like this would " +
			"let a task author (indirectly, via whoever installs the template) choose the ServiceAccount " +
			"a controller-created workload runs under, defeating the stamped-account containment.")
	}
	if !strings.Contains(err.Error(), "must be unset") {
		t.Errorf("rejected, but not by the expected rule — message was: %v", err)
	}

	// A template that leaves ServiceAccountName unset must still be ACCEPTED —
	// proving the rule targets the specific field, not the object generally.
	if err := envK8s.Create(ctx, mkComputeWorkloadTemplate("good-template", ns, "")); err != nil {
		t.Errorf("a template with no serviceAccountName was rejected: %v", err)
	}
}

// The pod-level half of the containment deny-list, enforced by the API SERVER
// itself. These are the fields that fit inside CEL's cost budget over a vendored,
// unbounded PodTemplateSpec; the container-level half could not (the API server
// refuses such a rule outright — "estimated rule cost exceeds budget by factor of
// more than 100x" — and the CRD then fails to install at all), so it lives in the
// ComputeWorkloadTemplate validating webhook instead. The split is measured, not
// chosen: see internal/compute/podpolicy.go.
//
// Enforced HERE rather than only in Go because a CEL rule holds for every client
// of the API server, including ones this specification does not know about, and
// with no dependency on the manager or its webhook being alive.
func TestEnvtest_ComputeWorkloadTemplateRefusesHostEscapeAtAdmission(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	cases := []struct {
		name   string
		mutate func(*corev1.PodSpec)
		names  string // the refusal must name this
		vector string
	}{
		{"hostPath volume", func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "noderoot", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}, "HostPath Volumes", "vector 2 — the node filesystem"},
		{"gitRepo volume", func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "src", VolumeSource: corev1.VolumeSource{
				GitRepo: &corev1.GitRepoVolumeSource{Repository: "https://example/x"}}}}
		}, "gitRepo", "a deprecated path-traversal surface"},
		{"hostNetwork", func(p *corev1.PodSpec) { p.HostNetwork = true },
			"Host Namespaces", "the node network namespace"},
		{"hostPID", func(p *corev1.PodSpec) { p.HostPID = true },
			"Host Namespaces", "the node process namespace"},
		{"hostIPC", func(p *corev1.PodSpec) { p.HostIPC = true },
			"Host Namespaces", "the node IPC namespace"},
		{"pod sysctls", func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				Sysctls: []corev1.Sysctl{{Name: "kernel.shm_rmid_forced", Value: "1"}}}
		}, "Sysctls", "the node kernel"},
		{"ephemeralContainers", func(p *corev1.PodSpec) {
			p.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "dbg", Image: "busybox"}}}
		}, "ephemeralContainers", "a debug surface sharing the target's namespaces"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mkComputeWorkloadTemplate(fmt.Sprintf("escalate-%d", i), ns, "")
			tc.mutate(&tmpl.Spec.PodTemplate.Spec)

			err := envK8s.Create(ctx, tmpl)
			if err == nil {
				t.Fatalf("the API server ACCEPTED a template reaching %s. When provision: workload "+
					"dispatch is wired, this object is created by the MANAGER's ServiceAccount — so "+
					"create on computeworkloadtemplates would become create on a pod that escapes "+
					"to the node", tc.vector)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("rejected, but not by the expected rule (want %q): %v", tc.names, err)
			}
		})
	}

	// The control: an ordinary template with none of the above is still ACCEPTED,
	// proving the rules target specific fields rather than the object generally.
	if err := envK8s.Create(ctx, mkComputeWorkloadTemplate("ordinary", ns, "")); err != nil {
		t.Errorf("an ordinary template was rejected: %v", err)
	}
}
