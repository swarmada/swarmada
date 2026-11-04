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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func ptrBool(b bool) *bool { return &b }

// basePod is an ordinary, admissible compute pod spec: an image, a resource
// request, an emptyDir, a config mount. Every negative case below is this plus
// ONE escalating field, so a refusal can only be attributed to that field — and
// the positive case proves the policy is a deny-list, not a wall.
func basePod() corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Volumes: []corev1.Volume{
			{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "cfg", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"}}}},
		},
		Containers: []corev1.Container{{
			Name:         "runtime",
			Image:        "ghcr.io/example/plate-reader@sha256:0000",
			VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:           ptrBool(true),
				ReadOnlyRootFilesystem: ptrBool(true),
			},
		}},
	}
}

// An ordinary pod spec is ADMITTED. Without this the whole suite could pass by
// refusing everything, which is not a policy but an outage.
func TestValidatePodTemplate_AdmitsAnOrdinaryComputePod(t *testing.T) {
	if err := ValidatePodTemplate(ptrPod(basePod())); err != nil {
		t.Fatalf("an ordinary compute pod spec was refused: %v", err)
	}
}

func ptrPod(p corev1.PodSpec) *corev1.PodSpec { return &p }

// THE THREE VECTORS, plus every other member of the deny-list. Each case is
// basePod() plus one escalating field, and each asserts BOTH that it is refused
// and that the refusal NAMES THE RULE — a refusal that does not say which rule it
// enforces leaves an administrator unable to tell policy from bug.
func TestValidatePodTemplate_RefusesEveryEscalationVector(t *testing.T) {
	unmasked := corev1.UnmaskedProcMount
	cases := []struct {
		name   string
		mutate func(*corev1.PodSpec)
		names  string // substring the refusal must contain
		vector string
	}{
		// ── vector 1: the ServiceAccount (already closed; asserted here too so
		// all three vectors are covered by one suite) ──────────────────────────
		{"serviceAccountName", func(p *corev1.PodSpec) { p.ServiceAccountName = "cluster-admin" },
			"stamps", "1 — foreign ServiceAccount"},
		{"deprecated serviceAccount alias", func(p *corev1.PodSpec) { p.DeprecatedServiceAccount = "cluster-admin" },
			"stamps", "1 — foreign ServiceAccount"},

		// ── vector 2: hostPath ─────────────────────────────────────────────────
		{"hostPath volume", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "noderoot",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}, "HostPath Volumes", "2 — hostPath"},

		// ── vector 3: privileged mode ──────────────────────────────────────────
		{"privileged container", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Privileged = ptrBool(true)
		}, "Privileged Containers", "3 — privileged"},
		{"privileged INIT container", func(p *corev1.PodSpec) {
			p.InitContainers = []corev1.Container{{Name: "setup", Image: "x@sha256:0",
				SecurityContext: &corev1.SecurityContext{Privileged: ptrBool(true)}}}
		}, "Privileged Containers", "3 — privileged, via initContainers"},

		// ── the rest of the Baseline host-escape set ───────────────────────────
		{"hostNetwork", func(p *corev1.PodSpec) { p.HostNetwork = true }, "Host Namespaces", "host namespace"},
		{"hostPID", func(p *corev1.PodSpec) { p.HostPID = true }, "Host Namespaces", "host namespace"},
		{"hostIPC", func(p *corev1.PodSpec) { p.HostIPC = true }, "Host Namespaces", "host namespace"},
		{"gitRepo volume", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "src",
				VolumeSource: corev1.VolumeSource{GitRepo: &corev1.GitRepoVolumeSource{Repository: "https://x/y"}}})
		}, "path-traversal", "deprecated gitRepo"},
		{"added capability", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Capabilities = &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}
		}, "Capabilities", "capability acquisition"},
		{"unmasked procMount", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.ProcMount = &unmasked
		}, "/proc Mount Type", "unmasked /proc"},
		{"Unconfined seccomp", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeUnconfined}
		}, "Seccomp", "syscall filter removed"},
		{"hostPort", func(p *corev1.PodSpec) {
			p.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		}, "Host Ports", "node network interface"},
		{"pod sysctls", func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				Sysctls: []corev1.Sysctl{{Name: "kernel.shm_rmid_forced", Value: "1"}}}
		}, "Sysctls", "node kernel"},
		{"ephemeralContainers", func(p *corev1.PodSpec) {
			p.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "dbg", Image: "busybox"}}}
		}, "subresource", "debug surface"},
		{"SELinux type override", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.SELinuxOptions = &corev1.SELinuxOptions{Type: "spc_t"}
		}, "SELinux", "SELinux type escape"},
		{"HostProcess container", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.WindowsOptions = &corev1.WindowsSecurityContextOptions{
				HostProcess: ptrBool(true)}
		}, "HostProcess", "runs on the node itself"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := basePod()
			tc.mutate(&pod)

			err := ValidatePodTemplate(&pod)
			if err == nil {
				t.Fatalf("ACCEPTED an escalating pod spec (vector: %s). A template like this, "+
					"created by the manager's own ServiceAccount, makes create on "+
					"computeworkloadtemplates equivalent to create on privileged pods", tc.vector)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("refused, but the message does not name the rule (want substring %q): %v",
					tc.names, err)
			}
			// EVERY refusal quotes the governing sentence, so an operator reading a
			// rejected write learns the principle and not only the field.
			if !strings.Contains(err.Error(), PolicyRule) {
				t.Errorf("refusal does not quote the rule it enforces: %v", err)
			}
		})
	}
}

// The builder refuses the same specs, so the policy holds even when admission did
// not run — a template admitted under an older schema, or written while the
// webhook was down or bypassed.
func TestBuildWorkload_RefusesAnEscalatingTemplateEvenWhenAdmissionDidNot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodSpec)
		names  string
	}{
		{"hostPath", func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "n", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}, "HostPath Volumes"},
		{"privileged", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Privileged = ptrBool(true)
		}, "Privileged Containers"},
		{"hostNetwork", func(p *corev1.PodSpec) { p.HostNetwork = true }, "Host Namespaces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := basePod()
			tc.mutate(&pod)
			tmpl := &fleetv1.ComputeWorkloadTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
				Spec: fleetv1.ComputeWorkloadTemplateSpec{
					Mode:        fleetv1.WorkloadTemplateModeJob,
					PodTemplate: corev1.PodTemplateSpec{Spec: pod},
				},
			}
			sch := runtime.NewScheme()
			if err := fleetv1.AddToScheme(sch); err != nil {
				t.Fatal(err)
			}
			owner := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "u"}}

			obj, err := BuildWorkload(tmpl, owner, sch, "run-1")
			if err == nil {
				t.Fatalf("BuildWorkload BUILT %T from an escalating template; the builder must never "+
					"trust admission as its only gate", obj)
			}
			if !strings.Contains(err.Error(), tc.names) || !strings.Contains(err.Error(), PolicyRule) {
				t.Errorf("refusal does not name the rule: %v", err)
			}
		})
	}
}
