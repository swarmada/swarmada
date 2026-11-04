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

// RolloutPhase values shared by FirmwareRollout and ModelRollout (RFC-0001 #firmwarerollout, #modelrollout).
const (
	RolloutPhasePending    RolloutPhase = "Pending"
	RolloutPhaseInProgress RolloutPhase = "InProgress"
	RolloutPhasePaused     RolloutPhase = "Paused"
	RolloutPhaseSucceeded  RolloutPhase = "Succeeded"
	RolloutPhaseFailed     RolloutPhase = "Failed"
)

// RolloutDesiredState is the operator's declarative intent for an in-flight
// rollout, common to FirmwareRollout and ModelRollout. It is the operator's half
// of a lifecycle whose other half the controller owns.
//
// LEVEL-TRIGGERED, like FleetAction/FleetTask spec.desiredState (ADR-0047): a
// write persists and re-converges after a disconnect, and re-writing the same
// value is idempotent, so a dropped edge is never a lost command.
//
// IT CAN ONLY ADD A HOLD, NEVER RELEASE ONE IT DID NOT PLACE. `Paused` withholds
// dispatch; `Running` withdraws that hold and nothing else. A rollout halted by
// pauseOnError stays halted under `Running`, because the exit from that halt is
// the rollout-resume verb, which decides what happens to the robots that failed
// (ADR-0041). The two holds compose: a rollout resumes when neither is in force.
// This is what lets the operator's intent be symmetric — writing Running lifts an
// operator hold — without giving an operator a spec field that could undo a safety
// halt, which is the asymmetry ADR-0045 had to impose on FleetAction, where the two
// sources of Paused were indistinguishable.
// +kubebuilder:validation:Enum=Running;Paused
type RolloutDesiredState string

// RolloutDesiredState values.
const (
	// RolloutDesiredStateRunning — dispatch as the strategy allows (default).
	RolloutDesiredStateRunning RolloutDesiredState = "Running"
	// RolloutDesiredStatePaused — an operator hold: no FURTHER robot enters a
	// batch. In-flight installs are NOT interrupted; a robot mid-install is
	// physically committed and cancelling it is what would leave a robot on an
	// indeterminate version.
	RolloutDesiredStatePaused RolloutDesiredState = "Paused"
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
	pause := true
	return RollingUpdateStrategy{MaxUnavailable: "10%", PauseOnError: &pause}
}

// RollingUpdateStrategy mirrors the K8s RollingUpdate pattern adapted for
// physical robot constraints. It is nested under RolloutStrategy.rollingUpdate.
type RollingUpdateStrategy struct {
	// MaxUnavailable is the maximum number or percentage of robots that may
	// simultaneously be in the updating state.
	// Examples: "10%" or "5".
	// +kubebuilder:default="10%"
	MaxUnavailable string `json:"maxUnavailable,omitempty"`

	// PauseOnError halts the rollout if any robot fails to update: no further robot
	// enters the batch while a failure stands, though in-flight updaters continue.
	//
	// The operator resumes with `swarmctl rollout resume`, which is gated on the
	// rollout-resume custom verb (RFC-0001 #security-permission-matrix) and writes the swarmada.io/rollout-resume
	// annotation the controller consumes. Resume EXCLUDES the robots that failed from
	// further attempts by this rollout rather than retrying them (ADR-0041); a fixed
	// artifact needs a fresh rollout. Excluded robots are named in
	// status.excludedRobots and count as settled, so the rollout can reach a terminal
	// phase and its record becomes deletable.
	// +kubebuilder:default=true
	// +optional
	PauseOnError *bool `json:"pauseOnError,omitempty"`
}

// RolloutSafetyConstraints are physical safety guards applied during updates.
type RolloutSafetyConstraints struct {
	// MinBatteryPct: do not update robots below this battery level.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=30
	MinBatteryPct int32 `json:"minBatteryPct,omitempty"`

	// RequireIdleState: only update robots in Idle phase. Set false to allow updating a
	// robot that is executing an action; unset means true.
	// +kubebuilder:default=true
	// +optional
	RequireIdleState *bool `json:"requireIdleState,omitempty"`

	// MaintenanceWindowOnly: only update robots during their configured
	// MaintenanceWindow. Slowest but least disruptive.
	// +kubebuilder:default=false
	MaintenanceWindowOnly bool `json:"maintenanceWindowOnly,omitempty"`
}

// RolloutTargetKind selects the kind of object a ModelRollout's targetSelector is
// resolved against. It is a discriminator, not a filter: a rollout targets exactly
// one kind, and the status family for the other kind is left at zero.
//
// Robot is the default, so every ModelRollout authored before this field existed
// keeps its meaning unchanged.
// +kubebuilder:validation:Enum=Robot;EdgeNode
type RolloutTargetKind string

// RolloutTargetKind values.
const (
	// RolloutTargetRobot resolves targetSelector against Robot labels.
	RolloutTargetRobot RolloutTargetKind = "Robot"
	// RolloutTargetEdgeNode resolves targetSelector against EdgeNode labels — an
	// edge or zone-controller box the control plane can push to, not an endpoint
	// executor (see EdgeNode's type comment).
	RolloutTargetEdgeNode RolloutTargetKind = "EdgeNode"
)

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
	// PreviousVersion is the robot's firmware version before this update. Filled
	// on a FirmwareRollout only; a ModelRollout leaves it empty at v0.4 and records
	// the previous model version in status.rollbackVersions instead.
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

// RolloutBatchNode is a per-node entry in a ModelRollout's active update batch when
// spec.targetKind is EdgeNode. It mirrors RolloutBatchRobot field for field, keyed
// by nodeName rather than robotName.
//
// It is a separate type rather than an optional nodeName on RolloutBatchRobot
// because the existing field is named for robots and is required; widening it would
// leave every reader guessing which of two names carries the subject. Two lists,
// each with one meaning, is the additive change (see ModelRolloutStatus).
type RolloutBatchNode struct {
	// NodeName is the EdgeNode currently in the active update batch.
	NodeName string `json:"nodeName"`
	// Namespace is the node's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// UpdateStartedAt is when the node entered the active update batch.
	// +optional
	UpdateStartedAt *metav1.Time `json:"updateStartedAt,omitempty"`
	// PreviousVersion is the node's model version before this update.
	// +optional
	PreviousVersion string `json:"previousVersion,omitempty"`
	// UpdatePhase is the node's current intra-update phase:
	// Downloading / Verifying / Installing / HealthChecking. Advisory.
	// +optional
	UpdatePhase string `json:"updatePhase,omitempty"`
	// CapabilitiesSuspendedAt is when this node's affected capabilities were
	// suspended for the update, stamped per attempt at batch entry. Only the
	// capabilities in spec.grantsCapabilities are suspended; a node's zone-control
	// and emergency-stop authority is never suspended by a rollout
	// (EdgeNode.status.zoneControlAvailable).
	// +optional
	CapabilitiesSuspendedAt *metav1.Time `json:"capabilitiesSuspendedAt,omitempty"`
}

// RolloutNodeResult records the failure outcome for a single EdgeNode.
type RolloutNodeResult struct {
	NodeName string `json:"nodeName"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	PreviousVersion string `json:"previousVersion,omitempty"`
	// +optional
	FailedAt *metav1.Time `json:"failedAt,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
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
	// TargetSelector selects which robots receive this firmware update. It is
	// re-resolved on every reconcile, so a robot relabelled mid-rollout joins or
	// leaves the rollout.
	// +kubebuilder:validation:Required
	TargetSelector metav1.LabelSelector `json:"targetSelector"`

	// NewVersion is the target firmware version string.
	//
	// Swarmada treats it as OPAQUE: it is never parsed, ordered or compared. Batch
	// classification is by EQUALITY only — a robot is "done" when
	// Status.FirmwareVersion == NewVersion and "updating" when its pending-firmware
	// annotation == NewVersion; the eligible set is sorted by NAME, not by version.
	// Vendor firmware strings are frequently not semver ("2.5.0-rc1", "v2.5.0",
	// "2026.06"), so constraining the form here would reject valid targets while
	// protecting nothing.
	//
	// ModelRollout.NewVersion IS pattern-constrained to bare major.minor.patch,
	// because that controller refuses downgrades via versionIsNewer and therefore
	// needs a parseable order. The asymmetry is deliberate.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	NewVersion string `json:"newVersion"`

	// FirmwareURI is the download URL for the firmware artifact.
	// +kubebuilder:validation:Required
	FirmwareURI string `json:"firmwareUri"`

	// FirmwareChecksum is the SHA-256 digest of the firmware artifact, as
	// "sha256:<64 hex>". The detached signature is verified over this value; the
	// robot re-verifies the downloaded bytes against it before install (RFC-0001 #firmwarerollout-schema).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	FirmwareChecksum string `json:"firmwareChecksum"`

	// FirmwareSignatureRef references the detached signature for the artifact,
	// verified against SwarmadaConfig.spec.signing.trustRoots before any dispatch.
	// When requireSignatureVerification is true this MUST be set; omitting it fails
	// the rollout closed (RFC-0001 #firmwarerollout-schema, #fleet-adapter-protocol-fleet-adapter-compliance-checklist). Supported forms: an
	// oci:// ref (digest- or tag-addressed, including a cosign signature tag), an
	// https:// URL, or an inline "bundle:<base64>" raw signature. Any other scheme,
	// s3:// included, fails the rollout closed while signing is enforced.
	// +optional
	FirmwareSignatureRef string `json:"firmwareSignatureRef,omitempty"`

	// Strategy controls how the rollout progresses.
	// +optional
	Strategy RolloutStrategy `json:"strategy,omitempty"`

	// DesiredState is the operator's level-triggered intent for this rollout.
	// Writing `Paused` withholds dispatch of any further robot; writing `Running`
	// withdraws that hold. It does NOT release a pauseOnError halt — see
	// RolloutDesiredState and ADR-0041 — so an operator cannot use it to push a
	// rollout past a failure the halt exists to surface.
	// +optional
	// +kubebuilder:validation:Enum=Running;Paused
	// +kubebuilder:default=Running
	DesiredState RolloutDesiredState `json:"desiredState,omitempty"`

	// SafetyConstraints restricts when updates may be applied.
	// +optional
	SafetyConstraints RolloutSafetyConstraints `json:"safetyConstraints,omitempty"`

	// Delivery is specified to control whether the robot pulls or is pushed the
	// update. No controller reads it at this version: every FirmwareRollout is
	// delivered as PullOnIdle, and PushImmediate pushes nothing.
	// +kubebuilder:default=PullOnIdle
	Delivery FirmwareDeliveryMechanism `json:"deliveryMechanism,omitempty"`

	// RollbackPolicy is specified to control what happens on failure. No controller
	// reads it on a FirmwareRollout at this version: neither value changes what
	// happens on failure, and Auto rolls nothing back.
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
	// StartedAt/CompletedAt/PausedAt are top-level rollout timing stamps. At v0.4
	// only PausedAt is written (on the transition into Paused, dropped on leaving
	// it); StartedAt and CompletedAt are written by no controller and stay empty.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`
	// CurrentBatch is the per-robot active update batch (RFC-0001 #firmwarerollout-schema). Each entry carries
	// the robot's live updatePhase, sourced from adapter UpdateProgress.
	// +optional
	// +listType=atomic
	CurrentBatch []RolloutBatchRobot  `json:"currentBatch,omitempty"`
	FailedRobots []RolloutRobotResult `json:"failedRobots,omitempty"`

	// ExcludedRobots names the robots an operator resume removed from this rollout
	// (ADR-0041). They failed, the operator resumed past them, and this rollout will
	// not attempt them again — retrying a failed artifact is what a FRESH rollout is
	// for. They are excluded from every progress bucket, so they neither re-pause the
	// rollout nor hold it short of a terminal phase; a non-empty list surfaces that the
	// fleet did not fully converge on this version.
	// +optional
	ExcludedRobots []string `json:"excludedRobots,omitempty"`
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
//
// The two eligibility axes are kind-exclusive. requiredHardware enumerates ROBOT
// hardware types (Lidar, Gripper, LoadPlatform…), none of which describes an edge
// box, so it is rejected on an EdgeNode-targeted rollout rather than silently
// ignored; requiredNodeCapabilities is the edge axis and is rejected on a
// robot-targeted one. Stating each as a rule keeps a mis-set field an admission
// error instead of a rollout that quietly selects the wrong set.
// +kubebuilder:validation:XValidation:rule="!has(self.targetKind) || self.targetKind != 'EdgeNode' || !has(self.requiredHardware)",message="requiredHardware enumerates robot hardware types and is not meaningful for targetKind: EdgeNode — use requiredNodeCapabilities"
// +kubebuilder:validation:XValidation:rule="!has(self.requiredNodeCapabilities) || (has(self.targetKind) && self.targetKind == 'EdgeNode')",message="requiredNodeCapabilities applies only to targetKind: EdgeNode — use requiredHardware for robots"
type ModelRolloutSpec struct {
	// TargetKind selects the kind of object TargetSelector is resolved against.
	// Defaults to Robot, so an existing ModelRollout that omits it keeps exactly
	// the behaviour it had before this field existed. EdgeNode targeting is specified but not implemented at this version:
	// the ModelRollout controller refuses a rollout with targetKind EdgeNode (a
	// TargetKindNotImplemented Warning event) and updates no node.
	// +optional
	// +kubebuilder:default=Robot
	TargetKind RolloutTargetKind `json:"targetKind,omitempty"`

	// TargetSelector selects which objects of TargetKind receive this model
	// update: Robot labels by default, EdgeNode labels when TargetKind is
	// EdgeNode. The selector itself is kind-neutral — only what it is matched
	// against changes — so robot targeting is unchanged and no second selector
	// field exists to keep in sync. It is re-resolved on every reconcile, so a
	// robot relabelled mid-rollout joins or leaves the rollout.
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
	// the robot re-verifies the downloaded bytes before install (RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist), and the
	// adapter verifies the detached signature over this value. Unlike
	// FirmwareRollout, the ModelRollout control plane verifies no signature before
	// dispatch at v0.4 (ADR-0020).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ModelChecksum string `json:"modelChecksum"`

	// ModelSignatureRef references the detached signature or sigstore bundle for the
	// model artifact. It is carried to the adapter in the model_update Command, and
	// the Fleet Adapter verifies it against the trust roots before install; the
	// ModelRollout control plane does not fetch or verify it at v0.4. When
	// requireSignatureVerification is true (or the namespace has no SwarmadaConfig)
	// the ModelRollout admission webhook refuses a create or update that leaves it
	// empty (ADR-0020, RFC-0001 #modelrollout-schema).
	// +optional
	ModelSignatureRef string `json:"modelSignatureRef,omitempty"`

	// AttestationRef optionally references a signed {modelDigest, metrics}
	// attestation produced by the evaluation job, specified to bind the
	// quality-gate metrics to the artifact being shipped (ADR-0020). Specified, not
	// implemented at v0.4: no controller resolves or verifies it.
	// +optional
	AttestationRef string `json:"attestationRef,omitempty"`

	// RequiredHardware lists hardware component TYPES (not names) that must be
	// present and Healthy for a robot to receive this model.
	// Robots in targetSelector without the required hardware are never dispatched;
	// at v0.4 they are counted in RobotsTotal and remain in RobotsPending, not in
	// RobotsIneligible.
	// Valid only when TargetKind is Robot (see the type-level CEL rules).
	// +optional
	RequiredHardware []HardwareComponentType `json:"requiredHardware,omitempty"`

	// RequiredNodeCapabilities is the EdgeNode eligibility axis: each name listed
	// here MUST appear in EdgeNode.status.capabilities[] with status Active for the
	// node to enter a batch. It replaces RequiredHardware for edge targets because
	// an edge box's fitness to run a model is a capability question (does it offer
	// inference on an accelerator this model needs) and not a robot-hardware-type
	// question. A node matched by the selector that lacks one is counted in
	// NodesIneligible, never in NodesFailed.
	//
	// MaxItems bounds CEL cost (see GrantsCapabilities below).
	// Valid only when TargetKind is EdgeNode (see the type-level CEL rules).
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	RequiredNodeCapabilities []string `json:"requiredNodeCapabilities,omitempty"`

	// GrantsCapabilities lists capabilities that become Active when this model
	// is Active. During the update nothing writes an Unavailable status: while a
	// robot's copy of the model is Updating, the capability derivation reports each
	// model-driven capability that model provides as Inactive (reason "providing
	// model rolling out").
	//
	// MaxItems bounds CEL cost (see Robot.spec.hardware's comment): the disjointness rule on
	// this type cross-products the two lists, and the API server refuses to install a CRD
	// whose estimated rule cost is unbounded. Matches RobotClass's MaxItems=32 on the same
	// concept, and 32 capabilities granted by a single model is already far past plausible.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	GrantsCapabilities []string `json:"grantsCapabilities,omitempty"`

	// RevokesCapabilities lists capabilities removed when this model replaces
	// a previous model with a different capability set. ("Capability set" is the
	// northbound seam of ADR-0037; the southbound axis is a "protocol profile".
	// This comment said "capability profile", which is neither.)
	// MaxItems bounds CEL cost (see GrantsCapabilities above).
	// +optional
	// +kubebuilder:validation:MaxItems=32
	RevokesCapabilities []string `json:"revokesCapabilities,omitempty"`

	// Strategy controls how the rollout progresses.
	// +optional
	Strategy RolloutStrategy `json:"strategy,omitempty"`

	// DesiredState is the operator's level-triggered intent for this rollout.
	// Writing `Paused` withholds dispatch of any further robot; writing `Running`
	// withdraws that hold. It does NOT release a pauseOnError halt — see
	// RolloutDesiredState and ADR-0041 — so an operator cannot use it to push a
	// rollout past a failure the halt exists to surface.
	// +optional
	// +kubebuilder:validation:Enum=Running;Paused
	// +kubebuilder:default=Running
	DesiredState RolloutDesiredState `json:"desiredState,omitempty"`

	// SafetyConstraints restricts when updates may be applied.
	// +optional
	SafetyConstraints RolloutSafetyConstraints `json:"safetyConstraints,omitempty"`

	// MaxDownloadTimeMinutes: specified so that if a model download exceeds this
	// duration on a given robot, that robot's update is marked Failed.
	// 0 means no limit. Specified, not implemented at v0.4: no controller reads
	// this field or times a download against it.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=480
	// +kubebuilder:default=30
	MaxDownloadTimeMinutes int32 `json:"maxDownloadTimeMinutes,omitempty"`

	// RollbackPolicy controls what happens on failure.
	// +kubebuilder:default=Manual
	RollbackPolicy ModelRollbackPolicy `json:"rollbackPolicy,omitempty"`
}

// ModelRolloutStatus defines the observed state of a ModelRollout.
//
// The status carries two parallel progress families, one per RolloutTargetKind:
// the robots* fields and the nodes* fields. A rollout targets exactly one kind, so
// one family is populated and the other left at zero. At this version only the
// robots* family is ever populated: EdgeNode targeting is not implemented. They
// are parallel rather than renamed-and-shared because renaming robotsTotal would
// change the meaning of a field readers already consume, which is a breaking change
// (docs/api-principles.md, "Versioning and compatibility"); adding fields is not.
type ModelRolloutStatus struct {
	Phase                   RolloutPhase `json:"phase,omitempty"`
	RobotsTotal             int32        `json:"robotsTotal,omitempty"`
	RobotsUpdated           int32        `json:"robotsUpdated,omitempty"`
	RobotsPending           int32        `json:"robotsPending,omitempty"`
	RobotsFailed            int32        `json:"robotsFailed,omitempty"`
	CapabilitiesSuspendedOn int32        `json:"capabilitiesSuspendedOn,omitempty"`
	// RobotsIneligible counts selector-matched robots excluded from the rollout.
	// At v0.4 it counts only robots refused as a downgrade (already running a newer
	// version of modelName); a robot lacking a required hardware type is not
	// counted here.
	// +optional
	RobotsIneligible int32 `json:"robotsIneligible,omitempty"`
	// StartedAt/CompletedAt/PausedAt are top-level rollout timing stamps. At v0.4
	// only PausedAt is written (on the transition into Paused, dropped on leaving
	// it); StartedAt and CompletedAt are written by no controller and stay empty.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`
	// CurrentBatch is the per-robot active update batch (RFC-0001 #modelrollout-schema); each entry carries
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

	// ExcludedRobots names the robots an operator resume removed from this rollout
	// (ADR-0041). They failed, the operator resumed past them, and this rollout will
	// not attempt them again — retrying a failed artifact is what a FRESH rollout is
	// for. They are excluded from every progress bucket, so they neither re-pause the
	// rollout nor hold it short of a terminal phase; a non-empty list surfaces that the
	// fleet did not fully converge on this version.
	// +optional
	ExcludedRobots []string `json:"excludedRobots,omitempty"`

	// RollbackVersions records, per robot, the model version it was running when it
	// entered this rollout's batch — the revert target for an Auto rollback. It is
	// captured at batch entry (before the robot leaves that version) and carried
	// forward across reconciles; it is the only place the previous version survives
	// once the robot reports newVersion.
	//
	// It is keyed by TARGET OBJECT NAME, so it serves both target kinds: a rollout
	// resolves one kind, and no robot and EdgeNode contend for the same key within
	// one rollout.
	// +optional
	RollbackVersions map[string]string `json:"rollbackVersions,omitempty"`

	// ── EdgeNode target family (spec.targetKind: EdgeNode) ────────────────────
	// Specified to be populated only when the rollout targets EdgeNodes. EdgeNode
	// targeting is not implemented at this version, so every field below stays zero.
	// Each field mirrors its robots* sibling above against a different kind.

	// NodesTotal is specified as the number of EdgeNodes matched by the selector AND
	// eligible (every name in spec.requiredNodeCapabilities Active). Always 0 at this
	// version: edgeNode targeting is specified but not implemented at this version.
	// +optional
	NodesTotal int32 `json:"nodesTotal,omitempty"`
	// NodesUpdated is the number running newVersion with capabilities restored.
	// +optional
	NodesUpdated int32 `json:"nodesUpdated,omitempty"`
	// NodesPending is the number matched and eligible but not yet batched.
	// +optional
	NodesPending int32 `json:"nodesPending,omitempty"`
	// NodesFailed is the number whose update failed.
	// +optional
	NodesFailed int32 `json:"nodesFailed,omitempty"`
	// NodesIneligible counts selector-matched nodes excluded from the rollout for
	// lacking a required node capability.
	// +optional
	NodesIneligible int32 `json:"nodesIneligible,omitempty"`

	// CapabilitiesSuspendedOnNodes is specified as the number of EdgeNodes whose
	// granted capabilities are suspended for this rollout (on a ZoneController, the
	// zone-detection blast radius of the current batch). Always 0 at this version:
	// edgeNode targeting is specified but not implemented at this version.
	// +optional
	CapabilitiesSuspendedOnNodes int32 `json:"capabilitiesSuspendedOnNodes,omitempty"`

	// CurrentNodeBatch is the per-node active update batch.
	// +optional
	// +listType=atomic
	CurrentNodeBatch []RolloutBatchNode `json:"currentNodeBatch,omitempty"`

	// FailedNodes records the failure outcome per EdgeNode.
	// +optional
	// +listType=atomic
	FailedNodes []RolloutNodeResult `json:"failedNodes,omitempty"`

	// NodesRolledBack counts EdgeNodes this rollout auto-reverted to their previous
	// model version (rollbackPolicy=Auto).
	// +optional
	NodesRolledBack int32 `json:"nodesRolledBack,omitempty"`

	// RolledBackNodes names the EdgeNodes this rollout has auto-reverted. They are
	// excluded from further attempts by this rollout.
	// +optional
	// +listType=set
	RolledBackNodes []string `json:"rolledBackNodes,omitempty"`

	// ExcludedNodes names the EdgeNodes an operator resume removed from this
	// rollout (ADR-0041), on the same terms as ExcludedRobots.
	// +optional
	// +listType=set
	ExcludedNodes []string `json:"excludedNodes,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelRollout is the Schema for the modelrollouts API.
//
// Key difference from FirmwareRollout: while a robot's copy of the model is
// Updating, the model-driven capabilities it provides are derived Inactive (reason
// "providing model rolling out") until the robot reports the model Active again.
//
// A rollout targets Robots. spec.targetKind EdgeNode (an edge or zone-controller
// box) is specified but not implemented at this version: the controller refuses such
// a rollout and updates no node. A rollout never targets an
// endpoint executor: a ComputeEndpoint has no ControlStream, so a ModelUpdate has
// no way to reach it, and endpoint model policy is a version requirement plus a
// scheduling gate and result provenance instead (ADR-0006).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mr
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".spec.modelName"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetKind"
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

// PausesOnError reports whether a failed robot halts the rollout.
//
// nil resolves to TRUE because the field carries +kubebuilder:default=true: the API server
// materializes an omitted key as true, and an accessor that disagreed with the schema
// default would answer differently from the API server about the same object.
func (r RollingUpdateStrategy) PausesOnError() bool {
	return r.PauseOnError == nil || *r.PauseOnError
}

// RequiresIdleState reports whether a rollout may only touch robots in the Idle phase.
// nil resolves to TRUE, matching the schema default.
func (c RolloutSafetyConstraints) RequiresIdleState() bool {
	return c.RequireIdleState == nil || *c.RequireIdleState
}
