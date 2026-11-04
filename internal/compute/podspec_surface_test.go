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

// ─────────────────────────────────────────────────────────────────────────────
// THE COMPLETE POD-SPEC SURFACE OF A ComputeWorkloadTemplate.
//
// podpolicy_test.go proves each escalation vector is refused. This file answers
// the question that comes before that one and cannot be answered by finding
// vectors: WHAT IS THE COMPLETE SET of pod-spec fields a template can influence,
// and what happens to each? A deny-list audited by spot-check is audited against
// the imagination of whoever wrote it. This is audited against the type.
//
// Every field of every type a ComputeWorkloadTemplate's podTemplate can reach is
// enumerated below with one of five dispositions, and three properties are
// enforced mechanically rather than asserted in prose:
//
//   1. Completeness. Reflection over the vendored k8s.io/api types requires a
//      disposition for EVERY field and forbids a disposition for a field that no
//      longer exists. A Kubernetes bump that adds a pod-spec field fails this
//      test until someone triages the new field against the rule. That is the
//      point: "the field is not plumbed through yet" is not a control, and
//      neither is "the field did not exist when we wrote the deny-list".
//
//   2. The wire surface agrees with the Go surface. The installed CRD's
//      podTemplate.spec property set must equal this table's keys, so a dropped
//      `make manifests` cannot leave the schema and the policy describing
//      different objects.
//
//   3. Each disposition is true. Every `rejected` and `constrained` field carries
//      a mutation that MUST be refused naming the rule; every `allowed` field is
//      set, non-zero, in one maximal template that MUST be admitted. A policy
//      that refuses everything is not a policy, it is an outage.
//
// The governing rule is the one quoted in PolicyRule: script-executing compute
// must be administrator-installed, never task-supplied (RFC-0001
// #computeworkloadtemplate). The deputy argument that makes it apply to an
// ADMIN-authored object is in podpolicy.go.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

type disposition string

const (
	// rejected: a template that sets this field is refused, at admission and
	// again at construction, with the refusal naming the rule.
	rejected disposition = "rejected"
	// constrained: the field is permitted, but named values or sub-fields within
	// it are refused. The sub-table for the nested type carries those.
	constrained disposition = "constrained"
	// allowed: passed through to the created workload untouched. Anything Pod
	// Security Standards Baseline permits is permitted here (ADR-0056).
	allowed disposition = "allowed"
	// stamped: whatever the template says, the builder writes its own value.
	stamped disposition = "stamped"
	// pruned: not reachable at all — the generated CRD declares no properties
	// beneath it, so the API server discards a write before anything reads it.
	pruned disposition = "pruned"
)

type surface struct {
	disp disposition
	// why is the one-line reason, and for `allowed` it is the record that the
	// field was REVIEWED and permitted rather than overlooked.
	why string
	// mutate applies the escalating form of this field to an otherwise ordinary
	// pod spec. Required for `rejected` and `constrained`; must be nil otherwise.
	mutate func(*corev1.PodSpec)
	// names is a substring the refusal must contain, so a refusal cannot pass
	// this test by being generic.
	names string
}

// ── corev1.PodTemplateSpec ───────────────────────────────────────────────────
var podTemplateSpecSurface = map[string]surface{
	"metadata": {disp: pruned, why: "controller-gen renders the embedded PodTemplateSpec's metadata as a bare object with no properties, so every key beneath it is discarded on apply; the builder's run label is added after"},
	"spec": {disp: constrained, names: "Host Namespaces",
		why:    "the pod spec proper — every field of it is enumerated in podSpecSurface below",
		mutate: func(p *corev1.PodSpec) { p.HostNetwork = true }},
}

// ── corev1.PodSpec ───────────────────────────────────────────────────────────
var podSpecSurface = map[string]surface{
	// ── the three escalation routes pod-create carries ─────────────────────
	"serviceAccountName": {disp: rejected, names: "stamps",
		why:    "vector 1 — a foreign ServiceAccount is the token-reading escalation; refused, then " + StampedServiceAccountName + " is stamped",
		mutate: func(p *corev1.PodSpec) { p.ServiceAccountName = "cluster-admin" }},
	"serviceAccount": {disp: rejected, names: "stamps",
		why:    "vector 1, through the deprecated alias Kubernetes still honours",
		mutate: func(p *corev1.PodSpec) { p.DeprecatedServiceAccount = "cluster-admin" }},
	"volumes": {disp: constrained, names: "HostPath Volumes",
		why: "vector 2 — hostPath, the deprecated gitRepo, flexVolume and inline csi are refused; every other volume source is Baseline-permitted (see volumeSourceSurface)",
		mutate: func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "noderoot",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}},
	"containers": {disp: constrained, names: "Privileged Containers",
		why:    "vector 3 — privileged mode and the rest of the container-level Baseline set are refused (see containerSurface)",
		mutate: func(p *corev1.PodSpec) { p.Containers[0].SecurityContext.Privileged = ptrBool(true) }},
	"initContainers": {disp: constrained, names: "Privileged Containers",
		why: "vector 3 again — an init container reaches the node exactly as an app container does, so checking only containers[] would leave the policy bypassable by moving one line up the file",
		mutate: func(p *corev1.PodSpec) {
			p.InitContainers = []corev1.Container{{Name: "setup", Image: "x@sha256:0",
				SecurityContext: &corev1.SecurityContext{Privileged: ptrBool(true)}}}
		}},

	// ── the rest of the Baseline host-escape set ───────────────────────────
	"hostNetwork": {disp: rejected, names: "Host Namespaces",
		why:    "the node's network namespace — link-local metadata, kubelet's port, every host-bound service",
		mutate: func(p *corev1.PodSpec) { p.HostNetwork = true }},
	"hostPID": {disp: rejected, names: "Host Namespaces",
		why:    "the node's process namespace — every other process on the node, and /proc/1/root",
		mutate: func(p *corev1.PodSpec) { p.HostPID = true }},
	"hostIPC": {disp: rejected, names: "Host Namespaces",
		why:    "the node's IPC namespace — shared memory of every other process on the node",
		mutate: func(p *corev1.PodSpec) { p.HostIPC = true }},
	"ephemeralContainers": {disp: rejected, names: "subresource",
		why: "an ephemeral container shares its target's namespaces and is set through the pod's own subresource, never through a template",
		mutate: func(p *corev1.PodSpec) {
			p.EphemeralContainers = []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "dbg", Image: "busybox"}}}
		}},
	"securityContext": {disp: constrained, names: "Sysctls",
		why: "pod-level sysctls, SELinux overrides, Unconfined seccomp and HostProcess are refused; runAsUser/Group/NonRoot, fsGroup and supplementalGroups are Baseline-permitted (see podSecurityContextSurface)",
		mutate: func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				Sysctls: []corev1.Sysctl{{Name: "kernel.shm_rmid_forced", Value: "1"}}}
		}},

	// ── reviewed and permitted: Baseline permits them, and none of them reaches
	//    the node or another principal's authority ───────────────────────────
	"automountServiceAccountToken": {disp: allowed,
		why: "REVIEWED: mounts the token of the STAMPED account only — the template cannot choose whose token it is, which is what vector 1 was about. Kubernetes' own default is already to mount it, so refusing the field here would not prevent the mount; the bound is that " + StampedServiceAccountName + " holds no RBAC"},
	"nodeName":                      {disp: allowed, why: "REVIEWED: pins the pod to a node, bypassing the scheduler and its taints — a placement choice, not node access; the container boundary is unchanged and Baseline does not restrict it"},
	"nodeSelector":                  {disp: allowed, why: "REVIEWED: placement only"},
	"affinity":                      {disp: allowed, why: "REVIEWED: placement only"},
	"tolerations":                   {disp: allowed, why: "REVIEWED: placement only — tolerating a control-plane taint still lands an unprivileged, non-host-namespaced container"},
	"schedulerName":                 {disp: allowed, why: "REVIEWED: names which scheduler binds the pod; no node access"},
	"schedulingGates":               {disp: allowed, why: "REVIEWED: delays scheduling; no node access"},
	"topologySpreadConstraints":     {disp: allowed, why: "REVIEWED: placement only"},
	"priorityClassName":             {disp: allowed, why: "REVIEWED: preemption is an availability concern, not a privilege boundary; a PriorityClass is a cluster-scoped object an administrator must have created, and Baseline does not restrict it"},
	"priority":                      {disp: allowed, why: "REVIEWED: set by the priority admission controller from priorityClassName; same reasoning"},
	"preemptionPolicy":              {disp: allowed, why: "REVIEWED: same reasoning as priorityClassName"},
	"restartPolicy":                 {disp: allowed, why: "REVIEWED: lifecycle only"},
	"terminationGracePeriodSeconds": {disp: allowed, why: "REVIEWED: lifecycle only"},
	"activeDeadlineSeconds":         {disp: allowed, why: "REVIEWED: lifecycle only"},
	"dnsPolicy":                     {disp: allowed, why: "REVIEWED: name resolution only — ClusterFirstWithHostNet is meaningless without hostNetwork, which is refused"},
	"dnsConfig":                     {disp: allowed, why: "REVIEWED: name resolution only"},
	"hostAliases":                   {disp: allowed, why: "REVIEWED: writes the POD's own /etc/hosts, not the node's"},
	"hostname":                      {disp: allowed, why: "REVIEWED: the pod's own hostname"},
	"subdomain":                     {disp: allowed, why: "REVIEWED: the pod's own DNS subdomain"},
	"setHostnameAsFQDN":             {disp: allowed, why: "REVIEWED: the pod's own hostname"},
	"shareProcessNamespace":         {disp: allowed, why: "REVIEWED: shares a namespace among THIS pod's containers; the node's is hostPID, which is refused"},
	"hostUsers":                     {disp: allowed, why: "REVIEWED: false requests a user namespace, which is strictly more isolation; true is Kubernetes' own default, so refusing it would change nothing"},
	"imagePullSecrets":              {disp: allowed, why: "REVIEWED: reads a Secret in the workload's own namespace, which the template author already administers; the kubelet does the read, so no Swarmada RBAC is involved"},
	"enableServiceLinks":            {disp: allowed, why: "REVIEWED: injects service env vars from the pod's own namespace"},
	"runtimeClassName":              {disp: allowed, why: "REVIEWED: a RuntimeClass is a cluster-scoped object only an administrator can create; naming one selects among sandboxes an administrator already installed, and Baseline does not restrict it"},
	"overhead":                      {disp: allowed, why: "REVIEWED: accounting only, and set by the RuntimeClass admission controller"},
	"resourceClaims":                {disp: allowed, why: "REVIEWED: names a ResourceClaim in the pod's own namespace; the device it resolves to is bounded by a DeviceClass an administrator installed"},
	"readinessGates":                {disp: allowed, why: "REVIEWED: readiness reporting only"},
	"os":                            {disp: allowed, why: "REVIEWED: declares the node OS the pod targets; windows carries hostProcess, which is refused at both securityContext levels"},
}

// ── corev1.Container (and, identically, initContainers) ──────────────────────
var containerSurface = map[string]surface{
	"securityContext": {disp: constrained, names: "Privileged Containers",
		why:    "vector 3 lives here — see containerSecurityContextSurface",
		mutate: func(p *corev1.PodSpec) { p.Containers[0].SecurityContext.Privileged = ptrBool(true) }},
	"ports": {disp: constrained, names: "Host Ports",
		why: "a hostPort binds the node's own network interface; containerPort does not",
		mutate: func(p *corev1.PodSpec) {
			p.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		}},

	// The code-bearing fields. They are ALLOWED HERE and nowhere else, and that
	// is the whole containment position rather than an exception to it: this
	// object is administrator-authored, and conformance check CP1.1
	// (conformance/controlplane.py) fails if any of these names appears in a
	// task-authored CRD's spec.
	"image":   {disp: allowed, why: "ADMIN-AUTHORED ONLY: the image is the compute. No FleetTask, FleetAction or ComputeEndpoint field can supply one (CP1.1)"},
	"command": {disp: allowed, why: "ADMIN-AUTHORED ONLY: as image (CP1.1)"},
	"args":    {disp: allowed, why: "ADMIN-AUTHORED ONLY: as image (CP1.1). Nothing substitutes a task-supplied value into this list — TestBuildWorkload_CarriesNothingButTheTemplate"},

	"name":                     {disp: allowed, why: "REVIEWED: identifier only"},
	"workingDir":               {disp: allowed, why: "REVIEWED: a path INSIDE the container"},
	"env":                      {disp: allowed, why: "REVIEWED: may read a Secret or ConfigMap in the workload's own namespace, which the template author already administers; the kubelet performs the read"},
	"envFrom":                  {disp: allowed, why: "REVIEWED: as env"},
	"resources":                {disp: allowed, why: "REVIEWED: accounting only"},
	"resizePolicy":             {disp: allowed, why: "REVIEWED: in-place resize behaviour; accounting only"},
	"restartPolicy":            {disp: allowed, why: "REVIEWED: per-container restart (sidecar init containers); lifecycle only"},
	"volumeMounts":             {disp: allowed, why: "REVIEWED: mounts a volume from spec.volumes, which is itself constrained. Bidirectional mountPropagation is rejected by the kubelet unless the container is privileged, which is refused"},
	"volumeDevices":            {disp: allowed, why: "REVIEWED: a raw block device from a PersistentVolumeClaim an administrator provisioned"},
	"livenessProbe":            {disp: allowed, why: "REVIEWED: runs inside the container"},
	"readinessProbe":           {disp: allowed, why: "REVIEWED: runs inside the container"},
	"startupProbe":             {disp: allowed, why: "REVIEWED: runs inside the container"},
	"lifecycle":                {disp: allowed, why: "REVIEWED: hooks run inside the container"},
	"terminationMessagePath":   {disp: allowed, why: "REVIEWED: a path INSIDE the container"},
	"terminationMessagePolicy": {disp: allowed, why: "REVIEWED: reporting only"},
	"imagePullPolicy":          {disp: allowed, why: "REVIEWED: pull behaviour only"},
	"stdin":                    {disp: allowed, why: "REVIEWED: stream plumbing; attaching still requires pods/attach, which the manager does not hold"},
	"stdinOnce":                {disp: allowed, why: "REVIEWED: as stdin"},
	"tty":                      {disp: allowed, why: "REVIEWED: as stdin"},
}

// ── corev1.SecurityContext (container level) ─────────────────────────────────
var containerSecurityContextSurface = map[string]surface{
	"privileged": {disp: rejected, names: "Privileged Containers",
		why:    "vector 3 — every capability, every device, effectively node root",
		mutate: func(p *corev1.PodSpec) { p.Containers[0].SecurityContext.Privileged = ptrBool(true) }},
	"capabilities": {disp: constrained, names: "Capabilities",
		why: "only NET_BIND_SERVICE may be ADDED (CAP_SYS_ADMIN above all is a route out); drops are unrestricted",
		mutate: func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Capabilities = &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}
		}},
	"procMount": {disp: constrained, names: "/proc Mount Type",
		why: "only Default; an unmasked /proc exposes the node",
		mutate: func(p *corev1.PodSpec) {
			unmasked := corev1.UnmaskedProcMount
			p.Containers[0].SecurityContext.ProcMount = &unmasked
		}},
	"seccompProfile": {disp: constrained, names: "Seccomp",
		why: "Unconfined removes the syscall filter; RuntimeDefault and Localhost are permitted",
		mutate: func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		}},
	"seLinuxOptions": {disp: constrained, names: "SELinux",
		why: "only container_t, container_init_t and container_kvm_t; user and role must be unset",
		mutate: func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.SELinuxOptions = &corev1.SELinuxOptions{Type: "spc_t"}
		}},
	"windowsOptions": {disp: constrained, names: "HostProcess",
		why: "hostProcess runs on the node itself; runAsUserName and the GMSA fields are permitted",
		mutate: func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.WindowsOptions = &corev1.WindowsSecurityContextOptions{HostProcess: ptrBool(true)}
		}},

	"runAsUser":                {disp: allowed, why: "REVIEWED: uid 0 INSIDE the container is not node root, and Baseline does not require runAsNonRoot — Restricted does, and this object's bar is Baseline (ADR-0056)"},
	"runAsGroup":               {disp: allowed, why: "REVIEWED: as runAsUser"},
	"runAsNonRoot":             {disp: allowed, why: "REVIEWED: hardening the workload author may choose"},
	"readOnlyRootFilesystem":   {disp: allowed, why: "REVIEWED: hardening the workload author may choose"},
	"allowPrivilegeEscalation": {disp: allowed, why: "REVIEWED: true permits setuid binaries to gain privilege WITHIN the container's capability set, which is already bounded by the capabilities rule above; Restricted forbids it, Baseline does not"},
}

// ── corev1.PodSecurityContext (pod level) ────────────────────────────────────
var podSecurityContextSurface = map[string]surface{
	"sysctls": {disp: rejected, names: "Sysctls",
		why: "a sysctl is set on the NODE kernel",
		mutate: func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{{Name: "kernel.shm_rmid_forced", Value: "1"}}}
		}},
	"seLinuxOptions": {disp: constrained, names: "SELinux",
		why: "as the container-level field",
		mutate: func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t"}}
		}},
	"seccompProfile": {disp: constrained, names: "Seccomp",
		why: "as the container-level field",
		mutate: func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
		}},
	"windowsOptions": {disp: constrained, names: "HostProcess",
		why: "as the container-level field",
		mutate: func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: ptrBool(true)}}
		}},

	"runAsUser":           {disp: allowed, why: "REVIEWED: as the container-level field"},
	"runAsGroup":          {disp: allowed, why: "REVIEWED: as the container-level field"},
	"runAsNonRoot":        {disp: allowed, why: "REVIEWED: as the container-level field"},
	"supplementalGroups":  {disp: allowed, why: "REVIEWED: supplementary GIDs inside the container; volume access is bounded by which volumes may be mounted at all"},
	"fsGroup":             {disp: allowed, why: "REVIEWED: ownership of the pod's own volumes"},
	"fsGroupChangePolicy": {disp: allowed, why: "REVIEWED: whether that chown is recursive"},
}

// ── corev1.VolumeSource ──────────────────────────────────────────────────────
//
// Baseline restricts exactly one source — hostPath. Three more are refused in
// addition, which Baseline does not cover: gitRepo, deprecated by Kubernetes and a
// known path-traversal surface; flexVolume and inline csi, which hand the pod
// whatever a node-installed driver exposes, node paths included. Restricted refuses
// flexVolume and PERMITS inline csi, so neither profile closes the CSI path; this
// list does. Everything else names storage an administrator provisioned, or the
// pod's own scratch space.
var volumeSourceSurface = map[string]surface{
	"hostPath": {disp: rejected, names: "HostPath Volumes",
		why: "vector 2 — the node filesystem, which is read/write on every other pod's data and on the kubelet's own credentials",
		mutate: func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "noderoot",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}},
	"gitRepo": {disp: rejected, names: "path-traversal",
		why: "beyond Baseline, deliberately: deprecated by Kubernetes and a known path-traversal surface",
		mutate: func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "src",
				VolumeSource: corev1.VolumeSource{GitRepo: &corev1.GitRepoVolumeSource{Repository: "https://x/y"}}})
		}},

	"emptyDir":              {disp: allowed, why: "REVIEWED: the pod's own scratch space"},
	"configMap":             {disp: allowed, why: "REVIEWED: an object in the workload's own namespace, read by the kubelet"},
	"secret":                {disp: allowed, why: "REVIEWED: as configMap — the template author administers that namespace, and the workload's namespace is the owner's, never one the referencing task chooses"},
	"downwardAPI":           {disp: allowed, why: "REVIEWED: the pod's own metadata"},
	"projected":             {disp: allowed, why: "REVIEWED: a projection of the three above plus the stamped account's token"},
	"persistentVolumeClaim": {disp: allowed, why: "REVIEWED: storage an administrator provisioned, bound by the namespace's StorageClass"},
	"ephemeral":             {disp: allowed, why: "REVIEWED: an inline PVC, same bound"},
	"csi": {disp: rejected, names: "inline CSI",
		why: "beyond Baseline and Restricted, deliberately: both permit an inline CSI volume, and what it reaches is whatever its driver exposes, node paths included — the hostPath refusal walked around without firing",
		mutate: func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "driver",
				VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "example.csi.k8s.io"}}})
		}},
	"flexVolume": {disp: rejected, names: "flexVolume",
		why: "beyond Baseline, deliberately (Restricted refuses it too): deprecated, and it runs a driver binary installed on the node, exposing whatever that binary mounts",
		mutate: func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "flex",
				VolumeSource: corev1.VolumeSource{FlexVolume: &corev1.FlexVolumeSource{Driver: "example/flex"}}})
		}},
	"nfs":                  {disp: allowed, why: "REVIEWED: network storage; reaches no node-local path"},
	"iscsi":                {disp: allowed, why: "REVIEWED: network storage"},
	"rbd":                  {disp: allowed, why: "REVIEWED: network storage"},
	"cephfs":               {disp: allowed, why: "REVIEWED: network storage"},
	"glusterfs":            {disp: allowed, why: "REVIEWED: network storage"},
	"cinder":               {disp: allowed, why: "REVIEWED: cloud provider volume"},
	"gcePersistentDisk":    {disp: allowed, why: "REVIEWED: cloud provider volume"},
	"awsElasticBlockStore": {disp: allowed, why: "REVIEWED: cloud provider volume"},
	"azureDisk":            {disp: allowed, why: "REVIEWED: cloud provider volume"},
	"azureFile":            {disp: allowed, why: "REVIEWED: cloud provider volume"},
	"vsphereVolume":        {disp: allowed, why: "REVIEWED: cloud provider volume"},
	"photonPersistentDisk": {disp: allowed, why: "REVIEWED: cloud provider volume"},
	"portworxVolume":       {disp: allowed, why: "REVIEWED: vendor volume"},
	"scaleIO":              {disp: allowed, why: "REVIEWED: vendor volume"},
	"storageos":            {disp: allowed, why: "REVIEWED: vendor volume"},
	"quobyte":              {disp: allowed, why: "REVIEWED: vendor volume"},
	"flocker":              {disp: allowed, why: "REVIEWED: vendor volume"},
	"fc":                   {disp: allowed, why: "REVIEWED: fibre channel storage"},
}

// ── corev1.ContainerPort ─────────────────────────────────────────────────────
var containerPortSurface = map[string]surface{
	"hostPort": {disp: rejected, names: "Host Ports",
		why: "binds the NODE's network interface directly",
		mutate: func(p *corev1.PodSpec) {
			p.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		}},
	"hostIP":        {disp: allowed, why: "REVIEWED: meaningless without hostPort, which is refused"},
	"containerPort": {disp: allowed, why: "REVIEWED: the pod's own port"},
	"name":          {disp: allowed, why: "REVIEWED: identifier only"},
	"protocol":      {disp: allowed, why: "REVIEWED: the pod's own port"},
}

// ─────────────────────────────────────────────────────────────────────────────

var surfaces = []struct {
	name  string
	typ   reflect.Type
	table map[string]surface
}{
	{"corev1.PodTemplateSpec", reflect.TypeOf(corev1.PodTemplateSpec{}), podTemplateSpecSurface},
	{"corev1.PodSpec", reflect.TypeOf(corev1.PodSpec{}), podSpecSurface},
	{"corev1.Container", reflect.TypeOf(corev1.Container{}), containerSurface},
	{"corev1.SecurityContext", reflect.TypeOf(corev1.SecurityContext{}), containerSecurityContextSurface},
	{"corev1.PodSecurityContext", reflect.TypeOf(corev1.PodSecurityContext{}), podSecurityContextSurface},
	{"corev1.VolumeSource", reflect.TypeOf(corev1.VolumeSource{}), volumeSourceSurface},
	{"corev1.ContainerPort", reflect.TypeOf(corev1.ContainerPort{}), containerPortSurface},
}

// jsonName is the wire name of a struct field, which is what an administrator
// writes and what the CRD schema carries — never the Go name.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

// PROPERTY 1 — COMPLETENESS.
//
// Every field of every type a template's podTemplate can reach carries a
// disposition, and no disposition names a field that does not exist. This is the
// test that makes the enumeration an enumeration rather than a list someone once
// wrote: a Kubernetes bump that adds a pod-spec field fails here until it is
// triaged against the rule.
func TestPodSpecSurface_EveryFieldHasADisposition(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			seen := map[string]bool{}
			for i := 0; i < s.typ.NumField(); i++ {
				name := jsonName(s.typ.Field(i))
				if name == "" {
					continue
				}
				seen[name] = true
				got, ok := s.table[name]
				if !ok {
					t.Errorf("%s.%s has NO disposition. A pod-spec field that nobody classified is a "+
						"field nobody bounded: %s. Decide whether a template may set it, and record "+
						"which — do not leave it to whether anyone has plumbed it through yet",
						s.name, name, PolicyRule)
					continue
				}
				if got.why == "" {
					t.Errorf("%s.%s is classified %q with no reason", s.name, name, got.disp)
				}
				needsMutation := got.disp == rejected || got.disp == constrained
				if needsMutation && got.mutate == nil {
					t.Errorf("%s.%s is classified %q but carries no mutation to prove it; a claimed "+
						"refusal that is never exercised is a comment", s.name, name, got.disp)
				}
				if !needsMutation && got.mutate != nil {
					t.Errorf("%s.%s is classified %q yet carries a refusal mutation", s.name, name, got.disp)
				}
			}
			for name := range s.table {
				if !seen[name] {
					t.Errorf("%s has a disposition for %q, which the type no longer declares — the "+
						"enumeration has drifted from the API it claims to cover", s.name, name)
				}
			}
		})
	}
}

// PROPERTY 2 — THE WIRE SURFACE AND THE GO SURFACE ARE THE SAME SURFACE.
//
// The installed CRD is what an administrator's `kubectl apply` is checked
// against. If `make manifests` were skipped, or the embedded type changed
// without regeneration, the schema and this policy would describe different
// objects and the enumeration above would be an audit of the wrong thing.
func TestPodSpecSurface_MatchesTheInstalledCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases",
		"swarmada.io_computeworkloadtemplates.yaml"))
	if err != nil {
		t.Fatalf("reading the generated CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema apiSchema `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing the generated CRD: %v", err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("the generated CRD declares no versions")
	}
	podTemplate := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].Properties["podTemplate"]

	// The `pruned` disposition on podTemplate.metadata, verified rather than
	// assumed: controller-gen renders it as a bare object, so the API server
	// discards every key beneath it.
	if md, ok := podTemplate.Properties["metadata"]; !ok {
		t.Error("podTemplate.metadata is absent from the schema entirely")
	} else if len(md.Properties) != 0 || md.XPreserveUnknownFields {
		t.Errorf("podTemplate.metadata is classified %q but the schema now declares %d properties "+
			"(preserveUnknown=%v) — reclassify it, because a write beneath it is no longer discarded",
			pruned, len(md.Properties), md.XPreserveUnknownFields)
	}

	schemaFields := podTemplate.Properties["spec"].Properties
	if len(schemaFields) == 0 {
		t.Fatal("the generated CRD carries no properties under podTemplate.spec")
	}
	for name := range schemaFields {
		if _, ok := podSpecSurface[name]; !ok {
			t.Errorf("the installed CRD accepts podTemplate.spec.%s and the enumeration does not "+
				"classify it", name)
		}
	}
	for name := range podSpecSurface {
		if _, ok := schemaFields[name]; !ok {
			t.Errorf("the enumeration classifies podTemplate.spec.%s and the installed CRD does not "+
				"carry it — run `make manifests`", name)
		}
	}
}

type apiSchema struct {
	Properties             map[string]apiSchema `json:"properties"`
	XPreserveUnknownFields bool                 `json:"x-kubernetes-preserve-unknown-fields"`
}

// PROPERTY 3a — EVERY CLAIMED REFUSAL IS A REFUSAL.
//
// Each `rejected` and `constrained` field's escalating form is applied to an
// otherwise ordinary pod and must be refused, naming both the specific control
// and the governing rule. podpolicy_test.go asserts the same for the vectors it
// enumerates by hand; this asserts it for everything the TYPE carries, so the
// two cannot diverge as the API grows.
func TestPodSpecSurface_EveryRefusalIsRefusedAndNamesTheRule(t *testing.T) {
	for _, s := range surfaces {
		for name, sf := range s.table {
			if sf.mutate == nil {
				continue
			}
			t.Run(s.name+"."+name, func(t *testing.T) {
				pod := basePod()
				sf.mutate(&pod)
				err := ValidatePodTemplate(&pod)
				if err == nil {
					t.Fatalf("ACCEPTED %s.%s in its escalating form. This pod would be created by the "+
						"MANAGER's ServiceAccount, so admitting it makes create on "+
						"computeworkloadtemplates equivalent to create on a pod that %s", s.name, name, sf.why)
				}
				if sf.names != "" && !strings.Contains(err.Error(), sf.names) {
					t.Errorf("refused, but the message names no control (want %q): %v", sf.names, err)
				}
				if !strings.Contains(err.Error(), PolicyRule) {
					t.Errorf("refused without quoting the rule it enforces: %v", err)
				}
			})
		}
	}
}

// maximalAdmissibleTemplate sets EVERY field the enumeration marks `allowed`, at
// once, in one template. It is the positive half of the audit and it is also the
// documentation: this is the pod spec an administrator may write.
func maximalAdmissibleTemplate() corev1.PodSpec {
	one := int64(1)
	oneI32 := int32(1)
	yes := true
	policy := corev1.FSGroupChangeAlways
	sidecar := corev1.ContainerRestartPolicyAlways
	preempt := corev1.PreemptLowerPriority
	return corev1.PodSpec{
		// lifecycle, placement, naming
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: &one,
		ActiveDeadlineSeconds:         &one,
		DNSPolicy:                     corev1.DNSClusterFirst,
		DNSConfig:                     &corev1.PodDNSConfig{Nameservers: []string{"10.0.0.10"}},
		HostAliases:                   []corev1.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"peer"}}},
		Hostname:                      "runtime",
		Subdomain:                     "compute",
		SetHostnameAsFQDN:             &yes,
		NodeName:                      "node-1",
		NodeSelector:                  map[string]string{"kubernetes.io/os": "linux"},
		Affinity:                      &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
		Tolerations:                   []corev1.Toleration{{Key: "compute", Operator: corev1.TolerationOpExists}},
		SchedulerName:                 "default-scheduler",
		SchedulingGates:               []corev1.PodSchedulingGate{{Name: "swarmada.io/wait"}},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway}},
		PriorityClassName: "compute-batch",
		Priority:          &oneI32,
		PreemptionPolicy:  &preempt,
		RuntimeClassName:  strPtr("gvisor"),
		Overhead:          corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
		OS:                &corev1.PodOS{Name: corev1.Linux},

		// identity and isolation, all Baseline-permitted
		AutomountServiceAccountToken: &yes,
		ShareProcessNamespace:        &yes,
		HostUsers:                    &yes,
		EnableServiceLinks:           &yes,
		ImagePullSecrets:             []corev1.LocalObjectReference{{Name: "registry"}},
		ReadinessGates:               []corev1.PodReadinessGate{{ConditionType: "swarmada.io/warm"}},
		ResourceClaims:               []corev1.PodResourceClaim{{Name: "gpu"}},
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser: &one, RunAsGroup: &one, RunAsNonRoot: &yes,
			SupplementalGroups: []int64{1}, FSGroup: &one, FSGroupChangePolicy: &policy,
			SELinuxOptions: &corev1.SELinuxOptions{Type: "container_t"},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			WindowsOptions: &corev1.WindowsSecurityContextOptions{RunAsUserName: strPtr("ContainerUser")},
		},

		// every non-hostPath, non-gitRepo volume source
		Volumes: allAdmissibleVolumes(),

		InitContainers: []corev1.Container{maximalAdmissibleContainer("setup", &sidecar)},
		Containers:     []corev1.Container{maximalAdmissibleContainer("runtime", nil)},
	}
}

func maximalAdmissibleContainer(name string, restart *corev1.ContainerRestartPolicy) corev1.Container {
	one := int64(1)
	yes := true
	def := corev1.DefaultProcMount
	return corev1.Container{
		Name:                     name,
		Image:                    "ghcr.io/example/plate-reader@sha256:0000",
		Command:                  []string{"/runtime"},
		Args:                     []string{"--region", "yard-west"},
		WorkingDir:               "/work",
		Ports:                    []corev1.ContainerPort{{Name: "grpc", ContainerPort: 8080, Protocol: corev1.ProtocolTCP, HostIP: "10.0.0.1"}},
		Env:                      []corev1.EnvVar{{Name: "REGION", Value: "yard-west"}},
		EnvFrom:                  []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"}}}},
		Resources:                corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
		ResizePolicy:             []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired}},
		RestartPolicy:            restart,
		VolumeMounts:             []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
		VolumeDevices:            []corev1.VolumeDevice{{Name: "block", DevicePath: "/dev/xvda"}},
		LivenessProbe:            &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}},
		ReadinessProbe:           &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}},
		StartupProbe:             &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}},
		Lifecycle:                &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/drain"}}}},
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		ImagePullPolicy:          corev1.PullIfNotPresent,
		Stdin:                    true,
		StdinOnce:                true,
		TTY:                      true,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: &one, RunAsGroup: &one, RunAsNonRoot: &yes,
			ReadOnlyRootFilesystem: &yes, AllowPrivilegeEscalation: &yes,
			Capabilities:   &corev1.Capabilities{Add: []corev1.Capability{allowedAddedCapability}, Drop: []corev1.Capability{"ALL"}},
			ProcMount:      &def,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			SELinuxOptions: &corev1.SELinuxOptions{Type: "container_t"},
			WindowsOptions: &corev1.WindowsSecurityContextOptions{RunAsUserName: strPtr("ContainerUser")},
		},
	}
}

func strPtr(s string) *string { return &s }

// allAdmissibleVolumes sets one volume per permitted VolumeSource, so the
// positive case covers the whole of that type too.
func allAdmissibleVolumes() []corev1.Volume {
	v := func(name string, src corev1.VolumeSource) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: src}
	}
	return []corev1.Volume{
		v("scratch", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}),
		v("cfg", corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"}}}),
		v("sec", corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "creds"}}),
		v("down", corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}}),
		v("proj", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}),
		v("pvc", corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}),
		v("eph", corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}),
		v("nfs", corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.2", Path: "/export"}}),
		v("iscsi", corev1.VolumeSource{ISCSI: &corev1.ISCSIVolumeSource{TargetPortal: "10.0.0.3", IQN: "iqn.2026-09.example:1", Lun: 0}}),
		v("rbd", corev1.VolumeSource{RBD: &corev1.RBDVolumeSource{CephMonitors: []string{"10.0.0.4"}, RBDImage: "img"}}),
		v("cephfs", corev1.VolumeSource{CephFS: &corev1.CephFSVolumeSource{Monitors: []string{"10.0.0.4"}}}),
		v("gluster", corev1.VolumeSource{Glusterfs: &corev1.GlusterfsVolumeSource{EndpointsName: "gl", Path: "/v"}}),
		v("cinder", corev1.VolumeSource{Cinder: &corev1.CinderVolumeSource{VolumeID: "v"}}),
		v("gce", corev1.VolumeSource{GCEPersistentDisk: &corev1.GCEPersistentDiskVolumeSource{PDName: "d"}}),
		v("ebs", corev1.VolumeSource{AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{VolumeID: "v"}}),
		v("azdisk", corev1.VolumeSource{AzureDisk: &corev1.AzureDiskVolumeSource{DiskName: "d", DataDiskURI: "u"}}),
		v("azfile", corev1.VolumeSource{AzureFile: &corev1.AzureFileVolumeSource{SecretName: "s", ShareName: "sh"}}),
		v("vsphere", corev1.VolumeSource{VsphereVolume: &corev1.VsphereVirtualDiskVolumeSource{VolumePath: "[ds] v.vmdk"}}),
		v("photon", corev1.VolumeSource{PhotonPersistentDisk: &corev1.PhotonPersistentDiskVolumeSource{PdID: "p"}}),
		v("portworx", corev1.VolumeSource{PortworxVolume: &corev1.PortworxVolumeSource{VolumeID: "v"}}),
		v("scaleio", corev1.VolumeSource{ScaleIO: &corev1.ScaleIOVolumeSource{Gateway: "g", System: "s", SecretRef: &corev1.LocalObjectReference{Name: "s"}}}),
		v("storageos", corev1.VolumeSource{StorageOS: &corev1.StorageOSVolumeSource{VolumeName: "v"}}),
		v("quobyte", corev1.VolumeSource{Quobyte: &corev1.QuobyteVolumeSource{Registry: "r", Volume: "v"}}),
		v("flocker", corev1.VolumeSource{Flocker: &corev1.FlockerVolumeSource{DatasetName: "d"}}),
		v("fc", corev1.VolumeSource{FC: &corev1.FCVolumeSource{TargetWWNs: []string{"w"}}}),
	}
}

// PROPERTY 3b — EVERY CLAIMED PERMISSION IS A PERMISSION, AND IS EXERCISED.
//
// Two assertions, and the second is the one that keeps the first honest: the
// maximal template is admitted, AND reflection confirms it actually SETS every
// field the enumeration marks `allowed`. Without that second half this test
// would keep passing as fields were added to the table and never exercised —
// which is how an allow-list quietly becomes a list of guesses.
func TestPodSpecSurface_TheMaximalAdmissibleTemplateIsAdmitted(t *testing.T) {
	pod := maximalAdmissibleTemplate()
	if err := ValidatePodTemplate(&pod); err != nil {
		t.Fatalf("an administrator's ordinary pod spec was REFUSED: %v\n\nThe bar is Pod Security "+
			"Standards Baseline, not Restricted (ADR-0056). A policy that refuses ordinary compute "+
			"images is an outage, not containment", err)
	}

	app, init := pod.Containers[0], pod.InitContainers[0]
	assertAllowedFieldsAreSet(t, "corev1.PodSpec", podSpecSurface, reflect.ValueOf(pod))
	assertAllowedFieldsAreSet(t, "corev1.PodSecurityContext", podSecurityContextSurface, reflect.ValueOf(*pod.SecurityContext))
	// Across BOTH containers rather than each: container-level restartPolicy is
	// only valid on an init container (the sidecar form), so requiring every
	// allowed field on every container would demand a pod spec the API server
	// itself rejects — and this literal is documentation as well as a fixture.
	assertAllowedFieldsAreSet(t, "corev1.Container", containerSurface,
		reflect.ValueOf(app), reflect.ValueOf(init))
	assertAllowedFieldsAreSet(t, "corev1.SecurityContext", containerSecurityContextSurface,
		reflect.ValueOf(*app.SecurityContext), reflect.ValueOf(*init.SecurityContext))
	assertAllowedFieldsAreSet(t, "corev1.ContainerPort", containerPortSurface,
		reflect.ValueOf(app.Ports[0]), reflect.ValueOf(init.Ports[0]))

	// Every permitted volume source appears exactly once.
	set := map[string]bool{}
	for i := range pod.Volumes {
		src := reflect.ValueOf(pod.Volumes[i].VolumeSource)
		for j := 0; j < src.NumField(); j++ {
			if !src.Field(j).IsNil() {
				set[jsonName(src.Type().Field(j))] = true
			}
		}
	}
	for name, sf := range volumeSourceSurface {
		if sf.disp == allowed && !set[name] {
			t.Errorf("volume source %q is classified allowed and the maximal template never uses it, "+
				"so nothing proves it is in fact admitted", name)
		}
	}
}

// assertAllowedFieldsAreSet requires every `allowed` field of a type to be set,
// non-zero, in at least one of the given instances.
func assertAllowedFieldsAreSet(t *testing.T, what string, table map[string]surface, vs ...reflect.Value) {
	t.Helper()
	for i := 0; i < vs[0].NumField(); i++ {
		name := jsonName(vs[0].Type().Field(i))
		if name == "" || table[name].disp != allowed {
			continue
		}
		set := false
		for _, v := range vs {
			if !v.Field(i).IsZero() {
				set = true
			}
		}
		if !set {
			t.Errorf("%s.%s is classified allowed but the maximal admissible template leaves it "+
				"unset, so nothing proves it is in fact admitted", what, name)
		}
	}
}

// PROPERTY 4 — NOTHING BUT THE TEMPLATE REACHES THE POD SPEC.
//
// The enumeration above bounds what an ADMINISTRATOR may write. This bounds what
// a TASK AUTHOR may write, and it is the half that cannot be closed by a
// deny-list: a FleetAction carries an endpointRef and parameter values, and the
// obvious future implementation of parameter passing is to substitute them into
// env or args — at which point task authorship becomes influence over a pod spec
// and the rule is broken from the other end.
//
// Today nothing is substituted. "Today nothing is substituted" is not a control,
// so this asserts it: the built pod spec must equal the template's byte for byte
// apart from the two stamped fields and the run label, and the caller-supplied
// run name must appear nowhere inside the pod spec. Whoever wires parameter
// passing gets a failing test naming the rule rather than a silent widening.
func TestBuildWorkload_CarriesNothingButTheTemplate(t *testing.T) {
	sch := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	tmpl := &fleetv1.ComputeWorkloadTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
		Spec: fleetv1.ComputeWorkloadTemplateSpec{
			Mode: fleetv1.WorkloadTemplateModeJob,
			PodTemplate: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "plate-reader"}},
				Spec:       maximalAdmissibleTemplate(),
			},
		},
	}
	// The run name is the ONLY caller-supplied value BuildWorkload takes, and a
	// dispatcher would derive it from the task-authored FleetAction's name. It
	// must reach metadata and the run label, and nothing else.
	const runName = "task-authored-name"

	obj, err := BuildWorkload(tmpl, &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "u"}}, sch, runName)
	if err != nil {
		t.Fatalf("BuildWorkload refused an admissible template: %v", err)
	}
	job, ok := obj.(*batchv1.Job)
	if !ok {
		t.Fatalf("Job mode built %T", obj)
	}

	got := job.Spec.Template
	want := *tmpl.Spec.PodTemplate.DeepCopy()
	// The two documented mutations, applied to the expectation — so anything
	// ELSE the builder touches shows up as a difference.
	want.Spec.ServiceAccountName = StampedServiceAccountName
	want.Labels[runLabelKey] = runName

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the built pod template differs from the administrator's template by more than the "+
			"stamped ServiceAccount and the run label.\n got: %#v\nwant: %#v\n\n%s — a value that "+
			"reaches the pod spec from anywhere but the template is a value a task author may one "+
			"day control", got, want, PolicyRule)
	}

	// Belt and braces, and the assertion that survives a future refactor of the
	// comparison above: the run name appears in metadata and the selector, never
	// in the pod's own spec.
	spec := got.DeepCopy()
	spec.ObjectMeta = metav1.ObjectMeta{}
	if strings.Contains(strings.Join(rendered(t, spec), "\n"), runName) {
		t.Errorf("the caller-supplied run name %q reached the POD SPEC. %s", runName, PolicyRule)
	}
}

// rendered serialises an object to YAML lines, for a substring check that does
// not depend on knowing which field a value landed in.
func rendered(t *testing.T, v any) []string {
	t.Helper()
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	lines := strings.Split(string(out), "\n")
	sort.Strings(lines)
	return lines
}
