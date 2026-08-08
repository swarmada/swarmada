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

// RolloutPhase is common to both FirmwareRollout and ModelRollout.
// +kubebuilder:validation:Enum=Pending;InProgress;Paused;Succeeded;Failed
type RolloutPhase string

// RolloutPhase values shared by FirmwareRollout and ModelRollout (§9.1.7, §9.1.8).
const (
	RolloutPhasePending    RolloutPhase = "Pending"
	RolloutPhaseInProgress RolloutPhase = "InProgress"
	RolloutPhasePaused     RolloutPhase = "Paused"
	RolloutPhaseSucceeded  RolloutPhase = "Succeeded"
	RolloutPhaseFailed     RolloutPhase = "Failed"
)

// RolloutStrategyType selects the rollout strategy. Only RollingUpdate at v0.1.
// +kubebuilder:validation:Enum=RollingUpdate
type RolloutStrategyType string

// RolloutStrategyType values.
const (
	// StrategyRollingUpdate is the batched rolling-update strategy.
	StrategyRollingUpdate RolloutStrategyType = "RollingUpdate"
)

// RolloutStrategy selects a rollout strategy and carries its parameters, mirroring
// the Kubernetes Deployment shape (spec.strategy.type + a strategy-named sub-object).
// Only RollingUpdate is defined at v0.1.
type RolloutStrategy struct {
	// Type selects the rollout strategy. Only RollingUpdate is supported at v0.1.
	// +kubebuilder:validation:Enum=RollingUpdate
	// +kubebuilder:default=RollingUpdate
	Type RolloutStrategyType `json:"type,omitempty"`

	// RollingUpdate holds the RollingUpdate strategy's parameters (applies when
	// type is RollingUpdate). When omitted, the documented defaults apply
	// (see RollingUpdateOrDefault).
	// +optional
	RollingUpdate *RollingUpdateStrategy `json:"rollingUpdate,omitempty"`
}

// RollingUpdateOrDefault returns the RollingUpdate parameters, substituting the
// documented defaults ("10%", pauseOnError=true) when the block is omitted.
func (s RolloutStrategy) RollingUpdateOrDefault() RollingUpdateStrategy {
	if s.RollingUpdate != nil {
		return *s.RollingUpdate
	}
	return RollingUpdateStrategy{MaxUnavailable: "10%", PauseOnError: true}
}

// RollingUpdateStrategy mirrors the K8s RollingUpdate pattern adapted for
// physical robot constraints. It is nested under RolloutStrategy.rollingUpdate.
type RollingUpdateStrategy struct {
	// MaxUnavailable is the maximum number or percentage of robots that may
	// simultaneously be in the updating state.
	// Examples: "10%" or "5".
	// +kubebuilder:default="10%"
	MaxUnavailable string `json:"maxUnavailable,omitempty"`

	// PauseOnError halts the rollout if any robot fails to update.
	// The operator must resume with swarmctl resume rollout.
	// +kubebuilder:default=true
	PauseOnError bool `json:"pauseOnError,omitempty"`
}

// RolloutSafetyConstraints are physical safety guards applied during updates.
type RolloutSafetyConstraints struct {
	// MinBatteryPct: do not update robots below this battery level.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=30
	MinBatteryPct int32 `json:"minBatteryPct,omitempty"`

	// RequireIdleState: only update robots in Idle phase.
	// +kubebuilder:default=true
	RequireIdleState bool `json:"requireIdleState,omitempty"`

	// MaintenanceWindowOnly: only update robots during their configured
	// MaintenanceWindow. Slowest but least disruptive.
	// +kubebuilder:default=false
	MaintenanceWindowOnly bool `json:"maintenanceWindowOnly,omitempty"`
}

// RolloutRobotResult records the failure outcome for a single robot.
type RolloutRobotResult struct {
	RobotName string `json:"robotName"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	PreviousVersion string `json:"previousVersion,omitempty"`
	// +optional
	FailedAt *metav1.Time `json:"failedAt,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// RolloutBatchRobot is a per-robot entry in a rollout's active update batch. Its
// updatePhase reflects the robot's most recently reported intra-update progress
// (the adapter's advisory UpdateProgress, §9.2); when the adapter reports none it
// stays at the initial phase the controller set when the robot entered the batch.
// The phase enum differs by rollout kind (see UpdatePhase), so the field is a
// string validated by the control plane rather than a single CRD enum.
type RolloutBatchRobot struct {
	// RobotName is the robot currently in the active update batch.
	RobotName string `json:"robotName"`
	// Namespace is the robot's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// UpdateStartedAt is when the robot entered the active update batch. Preserved
	// across reconciles for the current attempt.
	// +optional
	UpdateStartedAt *metav1.Time `json:"updateStartedAt,omitempty"`
	// PreviousVersion is the robot's firmware/model version before this update.
	// +optional
	PreviousVersion string `json:"previousVersion,omitempty"`
	// UpdatePhase is the robot's current intra-update phase. Firmware:
	// Pulling / Installing / Verifying / Rebooting. Model:
	// Downloading / Verifying / Installing / HealthChecking. Advisory — sourced from
	// the adapter's UpdateProgress, else the controller's initial phase.
	// +optional
	UpdatePhase string `json:"updatePhase,omitempty"`
	// CapabilitiesSuspendedAt is when this robot's affected capabilities were
	// suspended for the update (ModelRollout only), stamped per attempt at batch
	// entry.
	// +optional
	CapabilitiesSuspendedAt *metav1.Time `json:"capabilitiesSuspendedAt,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// FirmwareRollout
// ─────────────────────────────────────────────────────────────────────────────

// FirmwareDeliveryMechanism controls how firmware reaches the robot.
// +kubebuilder:validation:Enum=PullOnIdle;PushImmediate
type FirmwareDeliveryMechanism string

const (
	// FirmwareDeliveryPullOnIdle — robot pulls firmware when it next becomes Idle.
	FirmwareDeliveryPullOnIdle FirmwareDeliveryMechanism = "PullOnIdle"
	// FirmwareDeliveryPushImmediate — control plane pushes firmware now (respects safety constraints).
	FirmwareDeliveryPushImmediate FirmwareDeliveryMechanism = "PushImmediate"
)

// FirmwareRollbackPolicy defines rollback behaviour on failure.
// +kubebuilder:validation:Enum=Manual;Auto
type FirmwareRollbackPolicy string

const (
	// FirmwareRollbackManual — operator must explicitly run swarmctl undo rollout.
	FirmwareRollbackManual FirmwareRollbackPolicy = "Manual"
	// FirmwareRollbackAuto — revert to previous version automatically on failure.
	FirmwareRollbackAuto FirmwareRollbackPolicy = "Auto"
)

// FirmwareRolloutSpec defines the desired state of a FirmwareRollout.
type FirmwareRolloutSpec struct {
	// TargetSelector selects which robots receive this firmware update.
	// +kubebuilder:validation:Required
	TargetSelector metav1.LabelSelector `json:"targetSelector"`

	// NewVersion is the target firmware version string (semver).
	//
	// The pattern matches ModelRollout.spec.newVersion: both are compared for ordering at
	// rollout start, and an unparseable version cannot be ordered. MinLength=1 alone admitted
	// values like "latest" or "v2.1" that read as versions and sort as nothing, so the failure
	// arrived at batch selection — after the rollout was created and an operator believed it
	// scheduled — rather than at admission.
	//
	// Deliberately bare `major.minor.patch`: prerelease and build metadata are excluded here
	// for the same reason the contract-version parser excludes them (RFC-0001 §9.2.2) —
	// admitting them would require this API to define their precedence, and it does not.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^\d+\.\d+\.\d+$`
	NewVersion string `json:"newVersion"`

	// FirmwareURI is the download URL for the firmware artifact.
	// +kubebuilder:validation:Required
	FirmwareURI string `json:"firmwareUri"`

	// FirmwareChecksum is the SHA-256 digest of the firmware artifact, as
	// "sha256:<64 hex>". The detached signature is verified over this value; the
	// robot re-verifies the downloaded bytes against it before install (§9.1.7.1).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	FirmwareChecksum string `json:"firmwareChecksum"`

	// FirmwareSignatureRef references the detached signature or sigstore bundle for
	// the artifact, verified against SwarmadaConfig.spec.signing.trustRoots before
	// any dispatch. When requireSignatureVerification is true this MUST be set;
	// omitting it fails the rollout closed (§9.1.7.1, §9.2.8). Supported forms: an
	// OCI/https/s3 ref, or an inline "bundle:<base64>" signature.
	// +optional
	FirmwareSignatureRef string `json:"firmwareSignatureRef,omitempty"`

	// Strategy controls how the rollout progresses.
	// +optional
	Strategy RolloutStrategy `json:"strategy,omitempty"`

	// SafetyConstraints restricts when updates may be applied.
	// +optional
	SafetyConstraints RolloutSafetyConstraints `json:"safetyConstraints,omitempty"`

	// Delivery controls whether the robot pulls or is pushed the update.
	// +kubebuilder:default=PullOnIdle
	Delivery FirmwareDeliveryMechanism `json:"deliveryMechanism,omitempty"`

	// RollbackPolicy controls what happens on failure.
	// Manual is strongly recommended (Auto can create fleet version inconsistency).
	// +kubebuilder:default=Manual
	RollbackPolicy FirmwareRollbackPolicy `json:"rollbackPolicy,omitempty"`
}

// FirmwareRolloutStatus defines the observed state of a FirmwareRollout.
type FirmwareRolloutStatus struct {
	Phase         RolloutPhase `json:"phase,omitempty"`
	RobotsTotal   int32        `json:"robotsTotal,omitempty"`
	RobotsUpdated int32        `json:"robotsUpdated,omitempty"`
	RobotsPending int32        `json:"robotsPending,omitempty"`
	RobotsFailed  int32        `json:"robotsFailed,omitempty"`
	// RobotsSkipped is an informational sub-count of RobotsPending: robots failing a
	// safety constraint this cycle, re-evaluated next cycle. Not a disjoint total term.
	// +optional
	RobotsSkipped int32 `json:"robotsSkipped,omitempty"`
	// StartedAt/CompletedAt/PausedAt are top-level rollout timing stamps.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`
	// CurrentBatch is the per-robot active update batch (§6.6). Each entry carries
	// the robot's live updatePhase, sourced from adapter UpdateProgress.
	// +optional
	// +listType=atomic
	CurrentBatch []RolloutBatchRobot  `json:"currentBatch,omitempty"`
	FailedRobots []RolloutRobotResult `json:"failedRobots,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FirmwareRollout is the Schema for the firmwarerollouts API.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fwr
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.newVersion"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Updated",type="integer",JSONPath=".status.robotsUpdated"
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.robotsTotal"
type FirmwareRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              FirmwareRolloutSpec   `json:"spec,omitempty"`
	Status            FirmwareRolloutStatus `json:"status,omitempty"`
}

// FirmwareRolloutList contains a list of FirmwareRollout resources.
// +kubebuilder:object:root=true
type FirmwareRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FirmwareRollout `json:"items"`
}

// ─────────────────────────────────────────────────────────────────────────────
// ModelRollout
// ─────────────────────────────────────────────────────────────────────────────

// ModelRollbackPolicy is identical semantics to FirmwareRollbackPolicy.
// Manual is strongly recommended — silent rollbacks create fleet version
// inconsistency and may restore a silently-incorrect model.
// +kubebuilder:validation:Enum=Manual;Auto
type ModelRollbackPolicy string

// ModelRollbackPolicy values (§6.7): Manual leaves a failed model rollout for an operator to roll
// back, Auto reactivates the robot-retained previous version without re-downloading it.
const (
	ModelRollbackManual ModelRollbackPolicy = "Manual"
	ModelRollbackAuto   ModelRollbackPolicy = "Auto"
)

// ModelRolloutSpec defines the desired state of a ModelRollout.
//
// A capability may not be both granted and revoked by one rollout. The two lists drive
// opposite edges of the §6.10 capability derivation, so an overlap has no coherent meaning:
// whichever list the controller happened to apply second would decide, making the outcome an
// artefact of iteration order rather than of the declaration. Rejecting at admission is the
// only place the operator still has the context to say which they meant.
// +kubebuilder:validation:XValidation:rule="!has(self.grantsCapabilities) || !has(self.revokesCapabilities) || !self.grantsCapabilities.exists(c, c in self.revokesCapabilities)",message="a capability must not appear in both grantsCapabilities and revokesCapabilities"
type ModelRolloutSpec struct {
	// TargetSelector selects which robots receive this model update.
	// +kubebuilder:validation:Required
	TargetSelector metav1.LabelSelector `json:"targetSelector"`

	// ModelName is the logical model name (not version-specific).
	// Must match InstalledModel.name on target robots.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`

	// NewVersion is the target model version string (semver).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^\d+\.\d+\.\d+$`
	NewVersion string `json:"newVersion"`

	// ModelURI is the OCI registry URI or HTTPS URL of the model artifact.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(oci|s3|https)://`
	ModelURI string `json:"modelUri"`

	// ModelChecksum is the SHA-256 digest of the model artifact, as
	// "sha256:<64 hex>". It is carried to the adapter in the model_update Command so
	// the robot re-verifies the downloaded bytes before install (§9.2.8), and — when
	// signing is enforced — the detached signature is verified over this value
	// against the SwarmadaConfig trust roots before any dispatch (parity with
	// FirmwareRollout, ADR-0020).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ModelChecksum string `json:"modelChecksum"`

	// ModelSignatureRef references the detached signature or sigstore bundle for the
	// model artifact, verified against SwarmadaConfig.spec.signing.trustRoots before
	// any dispatch. When requireSignatureVerification is true this MUST be set;
	// omitting it fails the rollout closed (ADR-0020, §9.2.8). Supported forms: an
	// https/oci ref, or an inline "bundle:<base64>" signature.
	// +optional
	ModelSignatureRef string `json:"modelSignatureRef,omitempty"`

	// AttestationRef optionally references a signed {modelDigest, metrics}
	// attestation produced by the evaluation job. When present, the control plane
	// verifies the signature and that the digest matches modelChecksum, binding the
	// quality-gate metrics to the artifact being shipped (ADR-0020). Enforcement is
	// configurable and defaults off.
	// +optional
	AttestationRef string `json:"attestationRef,omitempty"`

	// RequiredHardware lists hardware component TYPES (not names) that must be
	// present and Healthy for a robot to receive this model.
	// Robots in targetSelector without the required hardware are silently skipped.
	// +optional
	RequiredHardware []HardwareComponentType `json:"requiredHardware,omitempty"`

	// GrantsCapabilities lists capabilities that become Active when this model
	// is Active. These capabilities are set to Unavailable during the update.
	//
	// MaxItems bounds CEL cost (see Robot.spec.hardware's comment): the disjointness rule on
	// this type cross-products the two lists, and the API server refuses to install a CRD
	// whose estimated rule cost is unbounded. Matches RobotClass's MaxItems=32 on the same
	// concept, and 32 capabilities granted by a single model is already far past plausible.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	GrantsCapabilities []string `json:"grantsCapabilities,omitempty"`

	// RevokesCapabilities lists capabilities removed when this model replaces
	// a previous model with a different capability profile.
	// MaxItems bounds CEL cost (see GrantsCapabilities above).
	// +optional
	// +kubebuilder:validation:MaxItems=32
	RevokesCapabilities []string `json:"revokesCapabilities,omitempty"`

	// Strategy controls how the rollout progresses.
	// +optional
	Strategy RolloutStrategy `json:"strategy,omitempty"`

	// SafetyConstraints restricts when updates may be applied.
	// +optional
	SafetyConstraints RolloutSafetyConstraints `json:"safetyConstraints,omitempty"`

	// MaxDownloadTimeMinutes: if a model download exceeds this duration on a
	// given robot, that robot's update is marked Failed.
	// 0 means no limit.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=480
	// +kubebuilder:default=30
	MaxDownloadTimeMinutes int32 `json:"maxDownloadTimeMinutes,omitempty"`

	// RollbackPolicy controls what happens on failure.
	// +kubebuilder:default=Manual
	RollbackPolicy ModelRollbackPolicy `json:"rollbackPolicy,omitempty"`
}

// ModelRolloutStatus defines the observed state of a ModelRollout.
type ModelRolloutStatus struct {
	Phase                   RolloutPhase `json:"phase,omitempty"`
	RobotsTotal             int32        `json:"robotsTotal,omitempty"`
	RobotsUpdated           int32        `json:"robotsUpdated,omitempty"`
	RobotsPending           int32        `json:"robotsPending,omitempty"`
	RobotsFailed            int32        `json:"robotsFailed,omitempty"`
	CapabilitiesSuspendedOn int32        `json:"capabilitiesSuspendedOn,omitempty"`
	// RobotsIneligible counts selector-matched robots excluded from the rollout
	// (e.g. lacking a required hardware type).
	// +optional
	RobotsIneligible int32 `json:"robotsIneligible,omitempty"`
	// StartedAt/CompletedAt/PausedAt are top-level rollout timing stamps.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`
	// CurrentBatch is the per-robot active update batch (§6.7); each entry carries
	// the robot's live updatePhase, sourced from adapter UpdateProgress.
	// +optional
	// +listType=atomic
	CurrentBatch []RolloutBatchRobot  `json:"currentBatch,omitempty"`
	FailedRobots []RolloutRobotResult `json:"failedRobots,omitempty"`

	// RobotsRolledBack counts robots this rollout auto-reverted to their previous
	// model version (rollbackPolicy=Auto, §6.7). A non-zero value surfaces fleet
	// version fragmentation: these robots are back in service but NOT on newVersion.
	// +optional
	RobotsRolledBack int32 `json:"robotsRolledBack,omitempty"`

	// RolledBackRobots names the robots this rollout has auto-reverted. They are
	// excluded from further update attempts by this rollout (a fixed model needs a
	// fresh rollout), so a reverted robot is never pushed back into an update loop.
	// +optional
	RolledBackRobots []string `json:"rolledBackRobots,omitempty"`

	// RollbackVersions records, per robot, the model version it was running when it
	// entered this rollout's batch — the revert target for an Auto rollback. It is
	// captured at batch entry (before the robot leaves that version) and carried
	// forward across reconciles; it is the only place the previous version survives
	// once the robot reports newVersion.
	// +optional
	RollbackVersions map[string]string `json:"rollbackVersions,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelRollout is the Schema for the modelrollouts API.
//
// Key difference from FirmwareRollout: when a ModelRollout begins for a robot,
// all capabilities listed in spec.grantsCapabilities are set to Unavailable for
// that robot until the update completes and the model returns to Active status.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mr
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".spec.modelName"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.newVersion"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Updated",type="integer",JSONPath=".status.robotsUpdated"
// +kubebuilder:printcolumn:name="Suspended",type="integer",JSONPath=".status.capabilitiesSuspendedOn"
type ModelRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelRolloutSpec   `json:"spec,omitempty"`
	Status            ModelRolloutStatus `json:"status,omitempty"`
}

// ModelRolloutList contains a list of ModelRollout resources.
// +kubebuilder:object:root=true
type ModelRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelRollout `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FirmwareRollout{}, &FirmwareRolloutList{})
	SchemeBuilder.Register(&ModelRollout{}, &ModelRolloutList{})
}
