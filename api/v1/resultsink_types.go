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

// ResultSinkKind selects a ResultSink's protocol.
// +kubebuilder:validation:Enum=Database;ObjectStore;Webhook
type ResultSinkKind string

// ResultSinkKind values.
const (
	ResultSinkKindDatabase    ResultSinkKind = "Database"
	ResultSinkKindObjectStore ResultSinkKind = "ObjectStore"
	ResultSinkKindWebhook     ResultSinkKind = "Webhook"
)

// ResultSinkSchema declares what a runtime is PERMITTED to write — the
// containment boundary on the data-path write itself, not just on who may
// reference the sink.
type ResultSinkSchema struct {
	// Table names the destination table (Database kind) or a logical
	// equivalent for other kinds.
	// +optional
	Table string `json:"table,omitempty"`

	// Columns lists the permitted column/field names.
	// +optional
	// +listType=set
	Columns []string `json:"columns,omitempty"`
}

// ResultSinkSpec is an administrator-installed, operator-configured egress
// destination for a run's result — the same
// TelemetrySink precedent (SwarmadaConfig.spec.telemetry.sink: "High-cadence
// telemetry goes to the TSDB sink, never to etcd"), scoped per-namespace and
// per-task rather than cluster-wide.
//
// THE CONTROL PLANE NEVER PROXIES THE ROW. It is specified to resolve
// CredentialRef and pass the runtime a short-lived, sink-scoped grant that
// expires with the run's lease; the runtime writes the record itself, on the
// data path. At v0.4 there is no egress path: nothing writes a result to a sink,
// nothing reads CredentialRef, and no grant is issued. No connection string,
// and no credential, can appear in a FleetTask or a FleetAction, because no
// field exists there to carry one — Endpoint and CredentialRef live ONLY here,
// on an administrator-installed object.
type ResultSinkSpec struct {
	// Kind selects the sink's protocol.
	Kind ResultSinkKind `json:"kind"`

	// Endpoint is the sink's address (e.g. "postgres://evidence.internal:5432/detections").
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// CredentialRef names the Secret that holds write credentials for this sink.
	// At v0.4 the control plane does not read it and gives these credentials to
	// no executor; a writer must obtain its own. It is never carried in a task
	// spec.
	// +optional
	CredentialRef *SecretKeyRef `json:"credentialRef,omitempty"`

	// Schema declares what the runtime may write.
	// +optional
	Schema *ResultSinkSchema `json:"schema,omitempty"`

	// Retention declares how long written records should be kept. This is a
	// DECLARATION at the destination, not an enforcement
	// Swarmada performs — whether it is actually enforced depends entirely on
	// the destination the operator chose (e.g. a Postgres retention job), and
	// this field does not claim otherwise.
	// +optional
	Retention *RetentionPolicy `json:"retention,omitempty"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// ResultSink is the Schema for the resultsinks API.
//
// NO STATUS SUBRESOURCE, DELIBERATELY. ResultSink is reference-only configuration,
// in the same shape as StorageClass or IngressClass: an administrator installs it and
// a FleetAction names it in spec.executor.sinks. No controller reconciles it, so
// there is nothing it could truthfully report.
//
// There is no dispatch path at this version: nothing writes a result to a sink, and
// the readiness of the store behind it is reported nowhere, on this object or on a
// FleetAction. The control plane reads it at admission only: the FleetAction
// admission webhook refuses a FleetAction naming a ResultSink that does not exist in
// its namespace, or whose declared columns do not accept the result fields of the
// action's ComputeEndpoint.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=rs
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=".spec.kind"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ResultSink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ResultSinkSpec `json:"spec,omitempty"`
}

// ResultSinkList contains a list of ResultSink.
// +kubebuilder:object:root=true
type ResultSinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResultSink `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResultSink{}, &ResultSinkList{})
}
