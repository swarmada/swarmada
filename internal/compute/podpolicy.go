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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// PolicyRule is the sentence every refusal in this file names. It is quoted
// rather than paraphrased because a refusal that does not say WHICH rule it
// enforces leaves an administrator guessing whether they hit a policy or a bug.
const PolicyRule = "script-executing compute must be administrator-installed, never task-supplied: " +
	"a controller-created compute workload MUST NOT be able to escape to the node"

// ── Why an ADMIN-authored template still needs a deny-list ───────────────────
//
// The objection to this file writes itself: a ComputeWorkloadTemplate is
// administrator-authored, and an administrator who can write one could write a
// Deployment instead. So what is being prevented?
//
// The deputy. When provision: workload dispatch is wired, the workload is created
// by the MANAGER's ServiceAccount — not by whoever wrote the template. Holding
// create on computeworkloadtemplates therefore means having the manager create a
// pod on your behalf, under its authority. That silently converts create on one
// swarmada.io CRD into create on pods, and pod-create carries hostPath onto the
// node filesystem, privileged mode, and the host network namespace. A principal
// handed "only a Swarmada CRD" is not bounded by that framing if the CRD reaches
// node root.
//
// Kubernetes will not catch this for us. RBAC's escalation-prevention covers
// `escalate` and `bind` on roles; it has nothing to say about pod-create. Pod
// Security Admission does apply — but it evaluates pod-controller objects (a Job,
// a Deployment) in warn/audit mode only, enforces at Pod creation, and only in a
// namespace an operator remembered to label. A cluster that never labelled the
// namespace has no gate at all. PSA is the defence in depth; this is the control.
//
// ── The policy, and why it is borrowed rather than invented ──────────────────
//
// The deny-list is the Kubernetes Pod Security Standards BASELINE profile's
// host-escape and privilege-acquisition controls. Baseline is the reference on
// purpose: it is what Kubernetes itself defines as "prevents known privilege
// escalations", it is what a labelled namespace's PSA would apply to the resulting
// pod anyway, and a reader can audit this list against a published standard rather
// than trusting a list someone assembled. Anything Baseline permits is permitted
// here — resources, env, volumeMounts, affinity, nodeSelector, tolerations and the
// rest of an ordinary pod spec are untouched.
//
// ── Why this is a Go function and not (all of it) CEL ────────────────────────
//
// The pod-level scalar half IS CEL, on ComputeWorkloadTemplateSpec, and that is
// the better place: a CEL rule holds for every client of the API server, with no
// dependency on a controller or a webhook being alive.
//
// The container-level half cannot be. ComputeWorkloadTemplate embeds a vendored
// corev1.PodTemplateSpec, whose containers/initContainers/ports lists carry no
// maxItems and cannot be given one without forking the type. A CEL rule walking
// them is refused by the API server outright — "estimated rule cost exceeds budget
// by factor of more than 100x" — and the CRD then fails to install at all. That is
// measured against a real API server (TestEnvtest_ComputeWorkloadTemplate*), not
// assumed. So the container half is enforced at admission by a validating webhook
// calling THIS function, and again here at construction.
//
// One function, two call sites — the webhook and BuildWorkload — for the reason
// internal/webhook/fleettask_webhook.go's validateFlow gives: a webhook that is
// down, misconfigured, or bypassed never leaves a template unchecked, and the two
// sites cannot drift apart because they are not two copies.

// allowedAddedCapability is the single capability Pod Security Standards Baseline
// permits a container to add. Everything else — CAP_SYS_ADMIN above all — is a
// route out of the container.
const allowedAddedCapability = corev1.Capability("NET_BIND_SERVICE")

// ValidatePodTemplate reports whether a template's pod spec is admissible, or an
// error naming the offending field, the value, and the rule. A nil return means
// the spec requests nothing on the deny-list.
//
// It is deliberately whole-spec rather than incremental: it re-checks the
// pod-level fields the CRD's CEL rules already cover, so a dropped regeneration,
// an edited CRD, or a template admitted under an older schema is still refused
// here. Nothing in this file trusts that admission ran.
func ValidatePodTemplate(pod *corev1.PodSpec) error {
	if pod == nil {
		return nil
	}
	// ── the ServiceAccount vector (first of the three) ───────────────────────
	if pod.ServiceAccountName != "" || pod.DeprecatedServiceAccount != "" {
		// Wrapped so errors.Is(err, ErrTemplateNamesServiceAccount) still holds for
		// callers that discriminate on it, while the message quotes the governing rule
		// like every other refusal here.
		return fmt.Errorf("%w. %s", fmt.Errorf("%w: the controller stamps %q",
			ErrTemplateNamesServiceAccount, StampedServiceAccountName), PolicyRule)
	}

	// ── pod-level host escape ────────────────────────────────────────────────
	for _, h := range []struct {
		field string
		set   bool
	}{
		{"hostNetwork", pod.HostNetwork},
		{"hostPID", pod.HostPID},
		{"hostIPC", pod.HostIPC},
	} {
		if h.set {
			return policyErr("spec.%s: true — a workload MUST NOT join a host namespace "+
				"(Pod Security Standards Baseline, Host Namespaces)", h.field)
		}
	}
	for i := range pod.Volumes {
		v := &pod.Volumes[i]
		switch {
		case v.HostPath != nil:
			return policyErr("spec.volumes[%d] (%q): hostPath %q — a workload MUST NOT mount the "+
				"node filesystem (Pod Security Standards Baseline, HostPath Volumes)",
				i, v.Name, v.HostPath.Path)
		case v.GitRepo != nil:
			return policyErr("spec.volumes[%d] (%q): gitRepo — deprecated by Kubernetes and a known "+
				"path-traversal surface; stage the repository into an image instead", i, v.Name)
		// Beyond Baseline, deliberately: each hands the pod whatever a node-installed
		// driver exposes, node paths included, so either walks around the hostPath
		// refusal without it firing. Restricted refuses flexVolume and permits inline
		// CSI, so neither profile closes this.
		case v.FlexVolume != nil:
			return policyErr("spec.volumes[%d] (%q): flexVolume driver %q — it runs a binary installed on "+
				"the node and exposes whatever that binary mounts; use a persistentVolumeClaim or an "+
				"ephemeral volume", i, v.Name, v.FlexVolume.Driver)
		case v.CSI != nil:
			return policyErr("spec.volumes[%d] (%q): inline CSI driver %q — it exposes whatever that "+
				"driver chooses, node paths included; use a persistentVolumeClaim or an ephemeral "+
				"volume, whose StorageClass an administrator controls", i, v.Name, v.CSI.Driver)
		}
	}
	if sc := pod.SecurityContext; sc != nil {
		if len(sc.Sysctls) > 0 {
			return policyErr("spec.securityContext.sysctls[%d entries]: a sysctl is set on the node "+
				"kernel (Pod Security Standards Baseline, Sysctls)", len(sc.Sysctls))
		}
		if err := checkSELinux(sc.SELinuxOptions, "spec.securityContext"); err != nil {
			return err
		}
		if err := checkSeccomp(sc.SeccompProfile, "spec.securityContext"); err != nil {
			return err
		}
		if sc.WindowsOptions != nil && sc.WindowsOptions.HostProcess != nil && *sc.WindowsOptions.HostProcess {
			return policyErr("spec.securityContext.windowsOptions.hostProcess: true — a HostProcess " +
				"container runs on the node itself (Pod Security Standards Baseline, HostProcess)")
		}
	}
	if len(pod.EphemeralContainers) > 0 {
		return policyErr("spec.ephemeralContainers[%d entries]: an ephemeral container shares the "+
			"target's namespaces and is set through the pod's own subresource, never through a template",
			len(pod.EphemeralContainers))
	}

	// ── container-level privilege acquisition ────────────────────────────────
	// initContainers are checked identically and not as an afterthought: an init
	// container runs with the same access to the node as an app container, and
	// checking only containers[] would leave the whole policy bypassable by moving
	// one line up the file.
	for i := range pod.Containers {
		if err := checkContainer(&pod.Containers[i], "spec.containers", i); err != nil {
			return err
		}
	}
	for i := range pod.InitContainers {
		if err := checkContainer(&pod.InitContainers[i], "spec.initContainers", i); err != nil {
			return err
		}
	}
	return nil
}

// checkContainer applies the container half of Baseline to one container.
func checkContainer(c *corev1.Container, list string, i int) error {
	at := fmt.Sprintf("%s[%d] (%q)", list, i, c.Name)
	for _, p := range c.Ports {
		if p.HostPort != 0 {
			return policyErr("%s: ports hostPort %d — a host port binds the node's network "+
				"interface directly (Pod Security Standards Baseline, Host Ports)", at, p.HostPort)
		}
	}
	sc := c.SecurityContext
	if sc == nil {
		return nil
	}
	if sc.Privileged != nil && *sc.Privileged {
		return policyErr("%s: securityContext.privileged: true — a privileged container holds the "+
			"node's capabilities and devices (Pod Security Standards Baseline, Privileged Containers)", at)
	}
	if sc.ProcMount != nil && *sc.ProcMount != corev1.DefaultProcMount {
		return policyErr("%s: securityContext.procMount: %q — an unmasked /proc exposes the node "+
			"(Pod Security Standards Baseline, /proc Mount Type)", at, *sc.ProcMount)
	}
	if sc.Capabilities != nil {
		for _, add := range sc.Capabilities.Add {
			if add != allowedAddedCapability {
				return policyErr("%s: securityContext.capabilities.add %q — only %q may be added "+
					"(Pod Security Standards Baseline, Capabilities)", at, add, allowedAddedCapability)
			}
		}
	}
	if err := checkSELinux(sc.SELinuxOptions, at+": securityContext"); err != nil {
		return err
	}
	if err := checkSeccomp(sc.SeccompProfile, at+": securityContext"); err != nil {
		return err
	}
	if sc.WindowsOptions != nil && sc.WindowsOptions.HostProcess != nil && *sc.WindowsOptions.HostProcess {
		return policyErr("%s: securityContext.windowsOptions.hostProcess: true — a HostProcess "+
			"container runs on the node itself (Pod Security Standards Baseline, HostProcess)", at)
	}
	return nil
}

// allowedSELinuxTypes are the container types Pod Security Standards Baseline
// permits. `user` and `role` must be unset entirely.
var allowedSELinuxTypes = map[string]bool{
	"": true, "container_t": true, "container_init_t": true, "container_kvm_t": true,
}

func checkSELinux(o *corev1.SELinuxOptions, at string) error {
	if o == nil {
		return nil
	}
	if !allowedSELinuxTypes[o.Type] {
		return policyErr("%s.seLinuxOptions.type: %q — only %s may be set "+
			"(Pod Security Standards Baseline, SELinux)", at, o.Type, "container_t, container_init_t, container_kvm_t")
	}
	if o.User != "" || o.Role != "" {
		return policyErr("%s.seLinuxOptions: user/role must be unset "+
			"(Pod Security Standards Baseline, SELinux)", at)
	}
	return nil
}

func checkSeccomp(p *corev1.SeccompProfile, at string) error {
	if p != nil && p.Type == corev1.SeccompProfileTypeUnconfined {
		return policyErr("%s.seccompProfile.type: Unconfined — the syscall filter is removed "+
			"(Pod Security Standards Baseline, Seccomp)", at)
	}
	return nil
}

// policyErr formats a refusal that always names the rule it enforces.
func policyErr(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return fmt.Errorf("%s %s", msg, PolicyRule)
}
