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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeyRef selects a key within a namespace-local Secret. The credential
// it names is specified to be resolved by the CONTROL PLANE and passed to a
// runtime as a short-lived, scope-bounded grant; at v0.4 nothing resolves it.
// It never appears in a FleetTask or FleetAction, because no field exists there
// to carry one.
type SecretKeyRef struct {
	// Name is the Secret name in the same namespace.
	Name string `json:"name"`
	// Key is the data key holding the credential.
	Key string `json:"key"`
}

// RetentionPolicy declares how long an external destination should keep what
// was written to it. It is a DECLARATION, not a
// guarantee: whether anything actually enforces it depends on the backing —
// see ArtifactStoreSpec's and ResultSinkSpec's own docs for which kinds can
// and cannot.
type RetentionPolicy struct {
	// MaxAgeDays is the retention period.
	// +kubebuilder:validation:Minimum=1
	MaxAgeDays int32 `json:"maxAgeDays"`
}

// ArtifactStoreKind selects an ArtifactStore's backing.
// +kubebuilder:validation:Enum=ObjectStore;VolumeClaim;EdgeLocal
type ArtifactStoreKind string

// ArtifactStoreKind values.
const (
	ArtifactStoreKindObjectStore ArtifactStoreKind = "ObjectStore"
	ArtifactStoreKindVolumeClaim ArtifactStoreKind = "VolumeClaim"
	ArtifactStoreKindEdgeLocal   ArtifactStoreKind = "EdgeLocal"
)

// VolumeAccessMode mirrors the Kubernetes PersistentVolume access modes this
// design cares about — declared here (not imported from corev1) because it is
// what a VolumeClaim-backed ArtifactStore ITSELF asserts about the claim, not
// a live read of the PVC's actual status.
// +kubebuilder:validation:Enum=ReadWriteOnce;ReadOnlyMany;ReadWriteMany
type VolumeAccessMode string

// VolumeAccessMode values.
const (
	VolumeAccessModeReadWriteOnce VolumeAccessMode = "ReadWriteOnce"
	VolumeAccessModeReadOnlyMany  VolumeAccessMode = "ReadOnlyMany"
	VolumeAccessModeReadWriteMany VolumeAccessMode = "ReadWriteMany"
)

// ReachableFromEverywhere reports whether this access mode lets more than one
// node read the claim at once — the property that decides whether a
// VolumeClaim-backed ArtifactStore is placement-constrained
// (ArtifactStoreNotReachable).
func (m VolumeAccessMode) ReachableFromEverywhere() bool {
	return m == VolumeAccessModeReadWriteMany || m == VolumeAccessModeReadOnlyMany
}

// ObjectStoreBacking is an ArtifactStore backed by object storage — reachable
// from anywhere, at the cost of bandwidth and an edge-held credential.
type ObjectStoreBacking struct {
	// Endpoint is the object-store endpoint (e.g. "s3://swarmada-evidence").
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// CredentialRef names the Secret that holds write credentials for this store.
	// At v0.4 the control plane does not read it and gives these credentials to
	// no executor; a writer must obtain its own. It is never carried in a task
	// spec.
	// +optional
	CredentialRef *SecretKeyRef `json:"credentialRef,omitempty"`
}

// VolumeClaimBacking is an ArtifactStore backed by a Kubernetes
// PersistentVolumeClaim — reachable from cluster-side steps, at the cost of
// the edge reaching cluster storage. AccessMode
// is DECLARED here, not read from the live PVC: it is what decides whether
// this store is placement-constrained (ReachableFromEverywhere), and a task
// author's scheduling outcome must not depend on a live cluster read
// succeeding just to know that.
type VolumeClaimBacking struct {
	// ClaimName is the PersistentVolumeClaim name, in this namespace.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// SubPath is a path within the claim's volume.
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// AccessMode declares what the claim supports. A non-ReadWriteMany (and
	// non-ReadOnlyMany) claim can be read from ONE place only, making a
	// consuming step placement-constrained by this input — the scheduling
	// gate ArtifactStoreNotReachable exists for exactly this.
	AccessMode VolumeAccessMode `json:"accessMode"`
}

// EdgeLocalBacking is an ArtifactStore backed by a directory the facility
// edge runtime already owns — zero-cost capture (the edge runtime already
// receives the stream), reachable ONLY from that zone.
type EdgeLocalBacking struct {
	// Zone is the FleetZone this edge location serves. A consuming step must
	// be co-located with it (ArtifactStoreNotReachable otherwise).
	// +kubebuilder:validation:MinLength=1
	Zone string `json:"zone"`

	// Path is a directory the edge runtime already owns.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`
}

// ArtifactStoreSpec is an administrator-installed reference to where artifact
// bytes actually live. An artifact reference
// (ActionResultArtifact) names a store BY NAME; there is no field anywhere a
// task could write a hostPath, a node name, or a bare filesystem path — the
// same containment shape as ComputeEndpoint and ResultSink.
//
// +kubebuilder:validation:XValidation:rule="(self.kind == 'ObjectStore') == has(self.objectStore)",message="objectStore must be set if and only if kind is ObjectStore"
// +kubebuilder:validation:XValidation:rule="(self.kind == 'VolumeClaim') == has(self.volumeClaim)",message="volumeClaim must be set if and only if kind is VolumeClaim"
// +kubebuilder:validation:XValidation:rule="(self.kind == 'EdgeLocal') == has(self.edgeLocal)",message="edgeLocal must be set if and only if kind is EdgeLocal"
type ArtifactStoreSpec struct {
	// Kind selects the backing.
	Kind ArtifactStoreKind `json:"kind"`

	// ObjectStore configures an ObjectStore-kind backing. Required iff Kind is ObjectStore.
	// +optional
	ObjectStore *ObjectStoreBacking `json:"objectStore,omitempty"`

	// VolumeClaim configures a VolumeClaim-kind backing. Required iff Kind is VolumeClaim.
	// +optional
	VolumeClaim *VolumeClaimBacking `json:"volumeClaim,omitempty"`

	// EdgeLocal configures an EdgeLocal-kind backing. Required iff Kind is EdgeLocal.
	// +optional
	EdgeLocal *EdgeLocalBacking `json:"edgeLocal,omitempty"`

	// Retention declares how long artifacts should be kept.
	//
	// ENFORCEABLE on ObjectStore, via the bucket's own lifecycle policy.
	// ADVISORY ONLY on VolumeClaim and EdgeLocal — NOTHING SWEEPS THEM. A
	// deployment keeping identifying material (a plate, a face) on an
	// EdgeLocal or VolumeClaim store is relying entirely on an operator's own
	// cron job or equivalent process; declaring Retention here does not make
	// that happen. That is a legitimate operational choice, and this field
	// exists so it is a VISIBLE one rather than an implicit one.
	// +optional
	Retention *RetentionPolicy `json:"retention,omitempty"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// ArtifactStore is the Schema for the artifactstores API.
//
// NO STATUS SUBRESOURCE, DELIBERATELY. ArtifactStore is reference-only
// configuration, in the same shape as StorageClass or IngressClass. No controller
// reconciles it, so there is nothing it could truthfully report.
//
// No controller or webhook reads it at this version: nothing stores an artifact in
// it or checks that it is reachable, and the ArtifactStoreNotReachable scheduling
// reason is written by nothing.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=as
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=".spec.kind"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ArtifactStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ArtifactStoreSpec `json:"spec,omitempty"`
}

// ArtifactStoreList contains a list of ArtifactStore.
// +kubebuilder:object:root=true
type ArtifactStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArtifactStore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArtifactStore{}, &ArtifactStoreList{})
}
