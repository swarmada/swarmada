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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ComputeWorkloadTemplateSpec is an administrator-authored pod template for
// provision: workload. It is the ONLY place a pod spec, an
// image, or a command may be written — a ComputeEndpoint's provision.workload
// names this object BY NAME; there is no field anywhere in a FleetAction or a
// ComputeEndpoint through which a task could supply one directly — the first
// containment rule.
//
// ServiceAccountName is deliberately absent from what a template controls: the
// controller stamps a fixed, low-privilege ServiceAccount on every workload it
// creates from this template and MUST REFUSE a template that names its own
// (the second containment rule; CEL below rejects it at admission, before a
// controller ever reads it — the same "reject at the boundary, not deep in
// code" discipline api-principles.md prescribes for cross-field invariants).
//
// # The confused-deputy problem this object creates, and the deny-list that bounds it
//
// A template is administrator-authored, which is often mistaken for "therefore
// unconstrained". It is not, and the reason is the deputy: when provision:
// workload dispatch is wired, the object is created by the MANAGER's
// ServiceAccount, not by whoever wrote the template. Whoever holds create on
// computeworkloadtemplates therefore has the manager create a pod on their
// behalf, under the manager's authority — so create on this CRD silently becomes
// create on pods, with everything pod-create carries: hostPath onto the node
// filesystem, privileged mode, the host network namespace.
//
// That is the escalation the containment position forbids, reached one object
// further out: script-executing compute must be administrator-installed, and a
// principal who can reach node root through a Swarmada CRD is not bounded by
// having been handed "only" a Swarmada CRD (RFC-0001 #computeworkloadtemplate).
//
// The deny-list below is the Kubernetes Pod Security Standards BASELINE profile's
// host-escape and privilege-acquisition controls, restated as CEL so they hold at
// this object's own boundary. Baseline is the reference on purpose rather than a
// bespoke list: it is the policy Kubernetes itself defines as "prevents known
// privilege escalations", it is what a cluster's Pod Security Admission would
// apply to the resulting pod anyway, and a reader can check this list against a
// published standard instead of taking it on trust. Anything Baseline permits is
// permitted here.
//
// Pod Security Admission is the defence in depth, not the control. It evaluates
// pod-controller objects (a Job, a Deployment) in warn/audit mode only and
// enforces at Pod creation, and only in a namespace an operator has labelled — so
// a cluster that never labelled the namespace has no gate at all. This object
// must carry its own.
//
// +kubebuilder:validation:XValidation:rule="(!has(self.podTemplate.spec.serviceAccountName) || size(self.podTemplate.spec.serviceAccountName) == 0) && (!has(self.podTemplate.spec.serviceAccount) || size(self.podTemplate.spec.serviceAccount) == 0)",message="podTemplate.spec.serviceAccountName (and the deprecated serviceAccount alias) must be unset — the controller stamps a fixed, low-privilege ServiceAccount on every workload it creates and refuses a template naming its own"
// +kubebuilder:validation:XValidation:rule="(!has(self.podTemplate.spec.hostNetwork) || !self.podTemplate.spec.hostNetwork) && (!has(self.podTemplate.spec.hostPID) || !self.podTemplate.spec.hostPID) && (!has(self.podTemplate.spec.hostIPC) || !self.podTemplate.spec.hostIPC)",message="podTemplate.spec.hostNetwork, hostPID and hostIPC must be unset or false — a controller-created compute workload MUST NOT join a host namespace (Pod Security Standards Baseline, Host Namespaces). The manager creates this workload under its own ServiceAccount, so a template that reaches the node's namespaces would make create on computeworkloadtemplates equivalent to node-level code execution"
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.volumes) || self.podTemplate.spec.volumes.all(v, !has(v.hostPath) && !has(v.gitRepo) && !has(v.flexVolume) && !has(v.csi))",message="podTemplate.spec.volumes must not use hostPath (Pod Security Standards Baseline, HostPath Volumes), the deprecated gitRepo, flexVolume or an inline csi volume — a controller-created compute workload MUST NOT mount the node filesystem, and a flexVolume or inline csi driver exposes whatever its node-installed driver chooses, node paths included. The manager creates this workload under its own ServiceAccount, so such a mount would make create on computeworkloadtemplates equivalent to read/write on the node; use a persistentVolumeClaim or an ephemeral volume instead"
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.securityContext) || !has(self.podTemplate.spec.securityContext.sysctls) || size(self.podTemplate.spec.securityContext.sysctls) == 0",message="podTemplate.spec.securityContext.sysctls must be empty — an unsafe sysctl is set on the node kernel (Pod Security Standards Baseline, Sysctls)"
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.ephemeralContainers) || size(self.podTemplate.spec.ephemeralContainers) == 0",message="podTemplate.spec.ephemeralContainers must be empty — an ephemeral container is a debug surface that shares the target's namespaces and is set through the pod's own subresource, never through a template"
type ComputeWorkloadTemplateSpec struct {
	// Mode selects whether a run creates a Job (RunToCompletion — App contract)
	// or a Deployment+Service (Continuous — Service contract).
	Mode WorkloadTemplateMode `json:"mode"`

	// PodTemplate is the admin-authored pod template. Standard Kubernetes
	// PodTemplateSpec, reused rather than reinvented — an
	// administrator who can already write a Deployment can write this.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// ComputeWorkloadTemplate is the Schema for the computeworkloadtemplates API.
//
// NO STATUS SUBRESOURCE, DELIBERATELY. ComputeWorkloadTemplate is reference-only
// configuration, in the same shape as StorageClass or IngressClass: an administrator
// installs it and a ComputeEndpoint's provision.workload names it. No controller
// reconciles it, so there is nothing it could truthfully report.
//
// There is no dispatch path at this version: no workload is ever created from a
// template. Its CEL rules and admission webhook validate the pod template when it is
// written; nothing else reads it.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=cwt
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ComputeWorkloadTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ComputeWorkloadTemplateSpec `json:"spec,omitempty"`
}

// ComputeWorkloadTemplateList contains a list of ComputeWorkloadTemplate.
// +kubebuilder:object:root=true
type ComputeWorkloadTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ComputeWorkloadTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ComputeWorkloadTemplate{}, &ComputeWorkloadTemplateList{})
}
