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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// EdgeNodeClass classifies a non-robot node the control plane can push to. It is
// specified as the CLASS FIELD a ModelRollout's targetSelector is resolved against
// when spec.targetKind is EdgeNode: the selector matches labels, and nodeClass says
// what kind of box wears them. At v0.4 nothing resolves against it: the ModelRollout
// controller skips a rollout whose spec.targetKind is EdgeNode.
//
// The set is closed and follows ADR-0010, which defines the site's edge roles:
//
//	ZoneController — holds a FleetZone's local authority, including headless
//	                 emergency-stop authority during a control-plane partition,
//	                 and serves EdgeService.EdgeStream to the adapters on its LAN.
//	                 This is the box a zone's detection cascade runs on.
//	SiteUplink     — owns the outbound-initiated session to the control plane for
//	                 the facility. ADR-0010 permits co-location with zone control
//	                 in one runtime; a co-located node declares ZoneController and
//	                 REPORTS both roles in status.roles.
//	ComputeNode    — edge compute with neither zone authority nor the site uplink:
//	                 a box that exists to run models.
//
// +kubebuilder:validation:Enum=ZoneController;SiteUplink;ComputeNode
type EdgeNodeClass string

// EdgeNodeClass values.
const (
	// EdgeNodeClassZoneController is a Zone Controller edge node (ADR-0010).
	EdgeNodeClassZoneController EdgeNodeClass = "ZoneController"
	// EdgeNodeClassSiteUplink is a node whose only role is the site's outbound uplink.
	EdgeNodeClassSiteUplink EdgeNodeClass = "SiteUplink"
	// EdgeNodeClassComputeNode is edge compute with no zone or uplink authority.
	EdgeNodeClassComputeNode EdgeNodeClass = "ComputeNode"
)

// EdgeNodeRole is a role the registered runtime reports it is actually serving.
// Roles are OBSERVED, not declared: spec.nodeClass is the operator's intent and
// status.roles is what the node came back with, so a ZoneController that is also
// carrying the site uplink (the common small-site shape in ADR-0010) reports both
// without the operator having to describe the co-location twice.
// +kubebuilder:validation:Enum=ZoneControl;SiteUplink;Inference
type EdgeNodeRole string

// EdgeNodeRole values.
const (
	// EdgeNodeRoleZoneControl — the node holds local authority for its zones.
	EdgeNodeRoleZoneControl EdgeNodeRole = "ZoneControl"
	// EdgeNodeRoleSiteUplink — the node owns the facility's outbound session.
	EdgeNodeRoleSiteUplink EdgeNodeRole = "SiteUplink"
	// EdgeNodeRoleInference — the node runs inference models.
	EdgeNodeRoleInference EdgeNodeRole = "Inference"
)

// EdgeNodePhase is an EdgeNode's lifecycle phase. It is deliberately NOT
// RobotPhase: an edge node has no Idle/Assigned/InProgress task lifecycle and no
// Charging state, and reusing a robot's enum would invite a reader to assume the
// scheduler treats the two alike.
// +kubebuilder:validation:Enum=Pending;Ready;Updating;Degraded;Offline
type EdgeNodePhase string

// EdgeNodePhase values.
const (
	// EdgeNodePhasePending — declared by an operator, no runtime has registered yet.
	EdgeNodePhasePending EdgeNodePhase = "Pending"
	// EdgeNodePhaseReady — registered, session live, serving its roles.
	EdgeNodePhaseReady EdgeNodePhase = "Ready"
	// EdgeNodePhaseUpdating — a rollout is installing on this node.
	EdgeNodePhaseUpdating EdgeNodePhase = "Updating"
	// EdgeNodePhaseDegraded — registered and serving, but at least one declared model
	// or capability is not Active.
	EdgeNodePhaseDegraded EdgeNodePhase = "Degraded"
	// EdgeNodePhaseOffline — no session within the liveness window.
	EdgeNodePhaseOffline EdgeNodePhase = "Offline"
)

// EdgeNodeIdentity is the cryptographic identity the runtime MUST present on its
// outbound session for the control plane to bind that session to this object.
// Registration is specified as a BINDING, not a discovery: an edge node is the
// facility's trust anchor (ADR-0010), so it is admitted by an operator authoring
// this resource and is never auto-admitted the way a robot may be (ADR-0014 is
// scoped to robots). At v0.4 there is no registration path: no session is bound
// to an EdgeNode, and neither field below is checked.
type EdgeNodeIdentity struct {
	// CommonName is the subject CN on the node's client certificate, issued by
	// the ControlStream CA. It is specified as the key that binds a session to
	// this object, with a session whose CN matches no EdgeNode refused. At v0.4 no
	// session is checked against it, and nothing is refused on it.
	// +kubebuilder:validation:MinLength=1
	CommonName string `json:"commonName"`

	// SPIFFEID optionally pins the node's SPIFFE identity. It is specified as
	// checked in addition to CommonName, never instead of it. At v0.4 no code reads
	// it: no session is checked against it, and setting it constrains nothing.
	// +optional
	SPIFFEID string `json:"spiffeID,omitempty"`
}

// EdgeNodeSpec is the operator-authored declaration of a non-robot node.
//
// +kubebuilder:validation:XValidation:rule="self.nodeClass != 'ZoneController' || (has(self.zones) && size(self.zones) > 0)",message="a ZoneController must list at least one zone in spec.zones; a ComputeNode or SiteUplink may list none"
type EdgeNodeSpec struct {
	// NodeClass is the class field that identifies what this node is. Required:
	// there is no defensible default, because the safety consequences of a rollout
	// differ by class (a ZoneController carries estop authority; a ComputeNode
	// does not).
	// +kubebuilder:validation:Required
	NodeClass EdgeNodeClass `json:"nodeClass"`

	// Identity is the mTLS identity the node's session must present.
	// +kubebuilder:validation:Required
	Identity EdgeNodeIdentity `json:"identity"`

	// Zones are the FleetZones this node serves. A ZoneController MUST list at
	// least one, and the API server refuses one that lists none (the CEL rule on
	// EdgeNodeSpec); a ComputeNode or SiteUplink may list none. They are specified
	// as what a rollout's blast radius is reported against
	// (status.zoneControlAvailable). No code reads them at v0.4.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	Zones []string `json:"zones,omitempty"`

	// InstalledModels declares the inference model packages deployed to this
	// node's compute. It is specified as the list a ModelRollout targeting this
	// node locates its entry in by spec.modelName, as on a Robot; runtime state
	// lives only in status.installedModels[]. At v0.4 no code reads it: the
	// ModelRollout controller skips a rollout whose spec.targetKind is EdgeNode,
	// with a TargetKindNotImplemented Warning event, and updates no node.
	// MaxItems bounds CEL cost (see Robot.spec.hardware's comment).
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	InstalledModels []ClassModel `json:"installedModels,omitempty"`

	// MaintenanceWindow is specified as this node's update window for a rollout
	// whose safetyConstraints.maintenanceWindowOnly is true. No code reads it at
	// v0.4: a ModelRollout that targets an EdgeNode is refused.
	// +optional
	MaintenanceWindow *MaintenanceWindow `json:"maintenanceWindow,omitempty"`
}

// EdgeNodeStatus is specified to be written only by the control plane, as the
// surface a ModelRollout against this node is read through — see the MUST list on
// ZoneControlAvailable and Capabilities below. At v0.4 nothing writes any field of
// it.
type EdgeNodeStatus struct {
	// Phase is the node's lifecycle phase.
	// +optional
	Phase EdgeNodePhase `json:"phase,omitempty"`

	// Roles are the roles the registered runtime reports serving (observed, not
	// declared — see EdgeNodeRole).
	// +optional
	// +listType=set
	Roles []EdgeNodeRole `json:"roles,omitempty"`

	// RegisteredAt is when the current session bound to this object.
	// +optional
	RegisteredAt *metav1.Time `json:"registeredAt,omitempty"`

	// LastHeartbeatAt is the last liveness signal on the node's session. It is
	// specified to be written on a material transition only, never per telemetry
	// tick (RA-1).
	// +optional
	LastHeartbeatAt *metav1.Time `json:"lastHeartbeatAt,omitempty"`

	// AgentVersion is the edge runtime build the node registered with.
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`

	// InstalledModels is the node's per-model runtime report, parallel to
	// spec.installedModels[] — the same shape a Robot reports, so one reader
	// serves both target kinds.
	// +optional
	// +listType=map
	// +listMapKey=name
	InstalledModels []InstalledModelStatusEntry `json:"installedModels,omitempty"`

	// ModelGrantedCapabilities records what a completed ModelRollout granted, per
	// model. It is specified to be written only by the rollout controller, which
	// refuses EdgeNode targets at v0.4, so nothing writes it.
	// +optional
	// +listType=map
	// +listMapKey=modelName
	ModelGrantedCapabilities []ModelGrantedCapabilityEntry `json:"modelGrantedCapabilities,omitempty"`

	// Capabilities is the computed capability set this node offers. During a
	// ModelRollout against this node, every name in the rollout's
	// grantsCapabilities MUST appear here with status Unavailable and a reason
	// naming the rollout; after a successful rollout each MUST read Active, and a
	// revoked name MUST be absent. It is specified as the observable that says
	// whether the zone's detection is currently degraded. Nothing writes it at v0.4.
	// +optional
	// +listType=map
	// +listMapKey=name
	Capabilities []CapabilityStatusEntry `json:"capabilities,omitempty"`

	// ZoneControlAvailable reports whether the node is still holding local
	// authority — zone admission and headless emergency stop — for spec.zones.
	// A ModelRollout MUST NOT suspend it: a model update may take a node's
	// inference capabilities Unavailable, never its estop authority, so this MUST
	// stay true across every phase of a rollout on a ZoneController. A pointer
	// because false ("authority lost") and unset ("never reported") are different
	// facts and only one of them is an incident. Nothing writes it at v0.4, so it
	// is always unset.
	// +optional
	ZoneControlAvailable *bool `json:"zoneControlAvailable,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions is specified to carry Registered, Ready and Updating. Nothing
	// writes it at v0.4.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EdgeNode is the Schema for the edgenodes API.
//
// An EdgeNode declares a non-robot node the control plane is specified to push
// to: an edge or zone-controller box serving a zone, not a machine that moves. It
// exists so a deployment whose detection cascade runs beside the zone rather than
// onboard a robot is expressible — see ModelRolloutSpec.TargetKind.
//
// At v0.4 no controller reconciles an EdgeNode. No session is bound to one,
// nothing writes its status, and a ModelRollout with targetKind EdgeNode is
// refused. The shipped zone-controller runtime does not read this kind. The field
// descriptions state the specified contract.
//
// It is NOT an executor. An endpoint executor (ComputeEndpoint) has no
// ControlStream and no session the control plane can push a ModelUpdate down, so
// model policy for an endpoint is a version REQUIREMENT plus a scheduling gate
// and provenance on the result, never a ModelRollout target (ADR-0006).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=en
// +kubebuilder:printcolumn:name="Class",type="string",JSONPath=".spec.nodeClass"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="ZoneControl",type="boolean",JSONPath=".status.zoneControlAvailable"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type EdgeNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EdgeNodeSpec   `json:"spec,omitempty"`
	Status            EdgeNodeStatus `json:"status,omitempty"`
}

// EdgeNodeList contains a list of EdgeNode resources.
// +kubebuilder:object:root=true
type EdgeNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EdgeNode{}, &EdgeNodeList{})
}
