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

// SwarmadaHealthConfig holds health-tracking tunables for the namespace.
type SwarmadaHealthConfig struct {
	// TelemetryIntervalSeconds is specified as the namespace default passive
	// telemetry rate for robots. No controller reads it at this version: the interval
	// an adapter is acked with comes from Robot.spec.telemetryIntervalSeconds or
	// RobotClass.spec.defaultTelemetry only.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	TelemetryIntervalSeconds int32 `json:"telemetryIntervalSeconds,omitempty"`
	// CapabilityRescanIntervalSeconds is specified as the period of a full
	// capability snapshot request (the ScanCapabilities command); 0 disables it. No
	// controller reads it at this version and no ScanCapabilities command is ever
	// sent, so no periodic rescan happens at any value.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	CapabilityRescanIntervalSeconds int32 `json:"capabilityRescanIntervalSeconds,omitempty"`
	// DefaultHardwareProbeIntervalSeconds is the RobotProbe controller's fallback for
	// a RobotProbe whose spec.intervalSeconds is not positive. That field carries a
	// CRD default (60) the API server always applies, so this value is not used in
	// practice.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=3600
	DefaultHardwareProbeIntervalSeconds int32 `json:"defaultHardwareProbeIntervalSeconds,omitempty"`
	// DefaultProbeTimeoutSeconds is the RobotProbe controller's fallback for a
	// RobotProbe whose spec.timeoutSeconds is not positive. That field carries a CRD
	// default (10) the API server always applies, so this value is not used in
	// practice.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	DefaultProbeTimeoutSeconds int32 `json:"defaultProbeTimeoutSeconds,omitempty"`
	// DefaultProbeFailureThreshold: consecutive failures before Degraded.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	DefaultProbeFailureThreshold int32 `json:"defaultProbeFailureThreshold,omitempty"`
	// DefaultProbeRecoveryThreshold: consecutive successes before Healthy.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	DefaultProbeRecoveryThreshold int32 `json:"defaultProbeRecoveryThreshold,omitempty"`
	// ConnectivityOfflineThresholdSeconds is the age of a Robot's
	// status.connectivity.lastSeenAt past which the Robot reconciler runs a
	// confirming heartbeat exchange (three attempts, five seconds apart) and, if
	// none is answered, sets the phase to Offline. lastSeenAt is stamped from the
	// bound Fleet Adapter's ControlStream liveness, not from the robot's telemetry.
	// Offline does not change status.health.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=3600
	ConnectivityOfflineThresholdSeconds int32 `json:"connectivityOfflineThresholdSeconds,omitempty"`
	// ConnectivityCriticalThresholdSeconds is how long a Robot must have been
	// Offline, measured from status.offlineSince, before the ConnectivityCritical
	// condition is set True. It raises the condition, a metric and an audit entry;
	// it does not change status.health or reassign work. Must exceed
	// ConnectivityOfflineThresholdSeconds (enforced by the admission webhook).
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=86400
	ConnectivityCriticalThresholdSeconds int32 `json:"connectivityCriticalThresholdSeconds,omitempty"`
	// DisableAllProbes, when true, suspends all RobotProbe verification in the
	// namespace: the probe loop issues no Verify RPCs and reports probes as
	// Unknown/paused. Passive telemetry is unaffected.
	// +optional
	// +kubebuilder:default=false
	DisableAllProbes *bool `json:"disableAllProbes,omitempty"`
}

// SwarmadaSchedulingConfig holds scheduler tunables.
type SwarmadaSchedulingConfig struct {
	// DefaultAcceptDegradedCapabilities: namespace-level default.
	// +kubebuilder:default=false
	DefaultAcceptDegradedCapabilities bool `json:"defaultAcceptDegradedCapabilities,omitempty"`
	// ActionRequeueBackoffSeconds is the backoff before a failed FleetAction is
	// requeued to Pending, applied only to a FleetAction that sets no
	// spec.retryPolicy. It does not set the re-check interval for a Pending
	// action with no eligible robot.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	ActionRequeueBackoffSeconds int32 `json:"actionRequeueBackoffSeconds,omitempty"`
	// MaxPendingActionsPerZone caps the number of Pending FleetActions the scheduler
	// admits per zone before applying backpressure. 0 means unbounded.
	// Enforced by the FleetAction admission webhook (pendingCap).
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxPendingActionsPerZone int32 `json:"maxPendingActionsPerZone,omitempty"`
	// PreferSameManufacturer, when true, ranks robots whose manufacturer matches an
	// action's PreferredManufacturer first (soft preference).
	// Applied by the FleetAction controller's scheduler tiebreak (preferSameManufacturer).
	// +optional
	// +kubebuilder:default=true
	PreferSameManufacturer *bool `json:"preferSameManufacturer,omitempty"`

	// HonorPreferredRobot, when true, ranks an action's spec.preferredRobot first among
	// otherwise-eligible robots (soft preference). Defaults to true: the hint is opt-in
	// per action, so a namespace that never sets preferredRobot is unaffected either way,
	// and a client that does set it means it.
	//
	// Turning this OFF does not make a preferred robot ineligible — it makes the hint
	// inert, and ranking falls back to manufacturer then battery. Use it to centrally
	// disable client-expressed robot affinity without editing the actions themselves.
	// +optional
	// +kubebuilder:default=true
	HonorPreferredRobot *bool `json:"honorPreferredRobot,omitempty"`

	// LeaseDurationSeconds is the task-lease duration (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee): how long a robot may
	// execute an assigned action without a renewal before it MUST self-stop.
	//
	// This is a SAFETY bound, not a tuning knob. The resolved value is both written to
	// FleetAction.status.leaseExpiresAt and sent to the robot as lease_duration_ms on
	// AssignAction and RenewActionLease,
	// which arms the adapter's self-stop timer (RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist "Task-lease self-stop", RFC-0001 #safety-fleet-adapter-local-behaviour-during-connectivity-loss
	// item 1). Raising it directly extends how long a robot that has lost its link keeps
	// moving before halting itself — hence the 300s ceiling. The 10s floor keeps the
	// lease above the derived renewal interval by a workable margin.
	//
	// The renewal interval is always leaseDurationSeconds/3 (RFC-0001 #control-plane-scheduler) and is NOT separately
	// configurable: deriving it makes it impossible to lengthen the lease without also
	// renewing proportionally sooner (ADR-0044).
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=300
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds,omitempty"`

	// ClockSkewMarginSeconds is the clock-skew margin of the lease horizon, which RFC-0001
	// #safety-action-table defines once (assignment-lease condition 3
	// holds once that horizon has passed). It absorbs the delivery delay of an assignment
	// or renewal and the drift between the control plane's clock and the robot's timer.
	//
	// Also a safety bound: too small and the control plane may reassign an action while
	// the robot still believes its lease is live — a double-execution window; too large
	// and recovery after a disconnect is needlessly delayed. Bounded to 1..60s (ADR-0044).
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	ClockSkewMarginSeconds int32 `json:"clockSkewMarginSeconds,omitempty"`
}

// ProvisioningMode controls how new robots are onboarded.
// +kubebuilder:validation:Enum=TwoPhase;DirectRegister
type ProvisioningMode string

// ProvisioningMode values.
const (
	// ProvisioningModeTwoPhase: Discover -> admit (K8s node model). Default.
	ProvisioningModeTwoPhase ProvisioningMode = "TwoPhase"
	// ProvisioningModeDirectRegister: robots go directly to Robot CRD on first connect.
	ProvisioningModeDirectRegister ProvisioningMode = "DirectRegister"
)

// SwarmadaProvisioningConfig holds robot provisioning settings.
type SwarmadaProvisioningConfig struct {
	// Mode is specified to control the discovery-to-admission flow. No controller
	// reads it at this version, and neither value changes behaviour: a robot that
	// connects is recorded as a DiscoveredRobot and becomes a Robot only when it is
	// admitted, and TwoPhase does not suppress auto-admit (autoAdmitRobotClass and
	// autoAdmitZone govern that). Admission warns when it is set to DirectRegister.
	// +kubebuilder:default=TwoPhase
	Mode ProvisioningMode `json:"mode,omitempty"`
	// DiscoveredRobotTTLMinutes: auto-delete un-admitted DiscoveredRobots after this time.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	DiscoveredRobotTTLMinutes int32 `json:"discoveredRobotTTLMinutes,omitempty"`
	// AutoAdmitRobotClass: if set, robots whose adapter-suggested RobotClass equals
	// this value are admitted automatically without operator review. Auto-admit is
	// enabled only when AutoAdmitZone is also set (ADR-0014).
	// +optional
	AutoAdmitRobotClass string `json:"autoAdmitRobotClass,omitempty"`
	// AutoAdmitZone is the leaf FleetZone auto-admitted robots are placed in. Auto-
	// admit requires both this and AutoAdmitRobotClass; a class without a zone is
	// inert, since a schedulable Robot requires a zone (ADR-0014).
	// +optional
	AutoAdmitZone string `json:"autoAdmitZone,omitempty"`
	// AutoRemoveOfflineRobots opts an auto-admitted Robot into automatic removal once its
	// adapter presence is gone (phase Offline) and any assigned action's lease is provably
	// dead (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee). Off by default. Only Robots that were auto-admitted (ADR-0014) are
	// eligible — operator-created robots are never removed, regardless of this setting.
	// This reclaims ephemeral robots (e.g. a robot) killed without a clean disconnect (ADR-0030).
	// +optional
	AutoRemoveOfflineRobots bool `json:"autoRemoveOfflineRobots,omitempty"`
	// AutoRemoveOfflineGraceSeconds is the dwell after a robot enters Offline before it is
	// removed, so a brief adapter blip does not evict a robot that reconnects. Applies only
	// when AutoRemoveOfflineRobots is true (ADR-0030).
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	AutoRemoveOfflineGraceSeconds int32 `json:"autoRemoveOfflineGraceSeconds,omitempty"`
}

// SwarmadaMaintenanceConfig holds zone maintenance defaults.
type SwarmadaMaintenanceConfig struct {
	// DefaultGracefulDrainTimeoutSeconds: after this duration in Graceful mode,
	// robots are force-paused even if actions are not yet complete.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	DefaultGracefulDrainTimeoutSeconds int32 `json:"defaultGracefulDrainTimeoutSeconds,omitempty"`
	// DefaultAutoResumeMinutes is specified as the namespace default for
	// ZoneMaintenance.spec.autoResumeAfterMinutes (0 = no auto-resume). No controller
	// reads it at this version: the ZoneMaintenance controller reads only the
	// window's own spec.autoResumeAfterMinutes, which defaults to 0.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=480
	DefaultAutoResumeMinutes int32 `json:"defaultAutoResumeMinutes,omitempty"`
	// RequireEstopClearBeforeResume is specified as the namespace default for the
	// ZoneMaintenance field of the same name: block resume of a paused robot until
	// any active estop on it is cleared. It is unreachable at v0.4: the API server
	// defaults ZoneMaintenance.spec.requireEstopClearBeforeResume to true, so a
	// window that omits it is gated whatever this value says.
	// +optional
	// +kubebuilder:default=true
	RequireEstopClearBeforeResume *bool `json:"requireEstopClearBeforeResume,omitempty"`
}

// ── Telemetry pipeline (RFC-0001 #swarmadaconfig-telemetry-pipeline-and-status-projection, RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split) ───────────────────────────

// TelemetrySinkType selects the high-cadence TSDB sink. The empty default is a
// deliberate "unconfigured" signal, NOT a silent drop: the control plane enters
// observed-degraded mode and raises the TelemetrySinkUnconfigured condition until
// a real store or an explicit Drop is set (RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split Invariant 1).
// +kubebuilder:validation:Enum="";Drop;PrometheusRemoteWrite;VictoriaMetrics;Mimir
type TelemetrySinkType string

// TelemetrySinkType values.
const (
	// TelemetrySinkUnset is the default: no sink configured (observed-degraded).
	TelemetrySinkUnset TelemetrySinkType = ""
	// TelemetrySinkDrop is the informed opt-in to discard high-cadence telemetry.
	TelemetrySinkDrop TelemetrySinkType = "Drop"
	// TelemetrySinkPrometheusRemoteWrite writes via Prometheus remote-write.
	TelemetrySinkPrometheusRemoteWrite TelemetrySinkType = "PrometheusRemoteWrite"
	// TelemetrySinkVictoriaMetrics writes via the Prometheus remote-write protocol
	// to a VictoriaMetrics remote-write endpoint.
	TelemetrySinkVictoriaMetrics TelemetrySinkType = "VictoriaMetrics"
	// TelemetrySinkMimir writes via a Grafana Mimir remote-write endpoint.
	TelemetrySinkMimir TelemetrySinkType = "Mimir"
)

// TelemetrySink is the high-cadence TSDB sink target (RFC-0001 #swarmadaconfig-schema).
type TelemetrySink struct {
	// Type selects the sink. MUST be set explicitly by the operator; the empty
	// default raises TelemetrySinkUnconfigured rather than silently dropping.
	// +kubebuilder:default=""
	Type TelemetrySinkType `json:"type,omitempty"`
	// Endpoint is the remote-write / ingest endpoint. Required when Type is a real
	// store; ignored when Type is Drop or empty.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// SwarmadaTelemetryConfig configures the two-data-plane split (RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split): where
// high-cadence telemetry goes and how the material Robot.status projection is
// throttled. High-cadence telemetry goes to the TSDB sink, never to etcd (RA-1).
type SwarmadaTelemetryConfig struct {
	// Sink is the high-cadence TSDB target.
	// +optional
	Sink TelemetrySink `json:"sink,omitempty"`
	// StatusWriteMinIntervalSeconds caps how often one robot's status may be
	// written; a non-critical material change arriving sooner is coalesced.
	// Safety-critical transitions bypass the cap. 0 disables it.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=0
	StatusWriteMinIntervalSeconds int32 `json:"statusWriteMinIntervalSeconds,omitempty"`
	// MaxStatusWritesPerMinutePerRobot is a hard ceiling on non-critical status
	// writes per robot per minute; the more restrictive of this and
	// StatusWriteMinIntervalSeconds applies. 0 = no ceiling.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxStatusWritesPerMinutePerRobot int32 `json:"maxStatusWritesPerMinutePerRobot,omitempty"`
	// MaterialBatteryThresholds are ascending battery percentages that count as
	// material; a status write occurs only when a reading crosses one. Entering
	// the lowest bucket is safety-critical and bypasses the rate cap.
	// +kubebuilder:default={15,30}
	MaterialBatteryThresholds []int32 `json:"materialBatteryThresholds,omitempty"`
}

// ── Artifact signing (RFC-0001 #swarmadaconfig-schema, #security-model-and-firmware-registry-credentials) ─────────────────────────────

// SigningSecretKeyRef selects a key within a namespace-local Secret holding a
// PEM-encoded cosign public key or x509 signing certificate.
type SigningSecretKeyRef struct {
	// Name is the Secret name in the same namespace.
	Name string `json:"name"`
	// Key is the data key holding the PEM material.
	Key string `json:"key"`
}

// SigningTrustRoot is a public-key anchor against which artifact signatures are
// verified (RFC-0001 #swarmadaconfig-schema).
type SigningTrustRoot struct {
	// Name is a logical identifier, written to the audit-log detail.
	Name string `json:"name"`
	// SecretRef references the Secret and key holding the trust anchor.
	SecretRef SigningSecretKeyRef `json:"secretRef"`
}

// SwarmadaSigningConfig configures artifact signature verification for firmware
// and model updates (RFC-0001 #swarmadaconfig-schema). The control plane reads these trust
// roots itself and sends them to no adapter: the FirmwareRollout controller verifies a firmware
// signature against them before dispatch, and FleetAdapter conformance verification checks a
// report's signature against them.
type SwarmadaSigningConfig struct {
	// RequireSignatureVerification, when true (default), makes the control plane verify a
	// FirmwareRollout artifact's signature against a trust root before dispatch, requires a
	// ModelRollout to name a signature the adapter verifies, and requires a signed conformance
	// report. Set it to false to turn verification off for this namespace; unset means true.
	// +kubebuilder:default=true
	// +optional
	RequireSignatureVerification *bool `json:"requireSignatureVerification,omitempty"`
	// A pointer because false is a real, different-from-absent value for a signing gate,
	// which api-principles.md ("Optional scalars are pointers") requires a pointer for. As
	// a non-pointer bool with omitempty it could not be set to false AT ALL: the zero value
	// serialized away and structural defaulting put the default back, so an operator who
	// turned verification off got it on with no error. Read it through
	// SignatureVerificationRequired(), never directly. This note is deliberately kept out
	// of the field's doc comment so it stays out of the published CRD description: the
	// blank line below it is what keeps it out, because controller-gen reads only the
	// comment block directly above a field.

	// TrustRoots are the public-key anchors. At least one is needed for any artifact to
	// verify when RequireSignatureVerification is true; a config with neither is
	// INCOMPLETE rather than invalid, so admission warns instead of rejecting and every
	// consumer fails closed until a root is added (ADR-0051).
	// +optional
	TrustRoots []SigningTrustRoot `json:"trustRoots,omitempty"`
	// RekorURL, when set, requires a FirmwareRollout artifact's entry to appear in
	// the named transparency log before the control plane dispatches it. Checked
	// only by the FirmwareRollout controller, and only when signature verification
	// is required; ModelRollout artifacts are not checked against the log.
	// +optional
	RekorURL string `json:"rekorUrl,omitempty"`

	// RekorPublicKey pins the transparency log's own public key (PEM in a Secret).
	//
	// It is what makes the log check MEAN anything. Without a pinned key the control plane can only
	// ask the log whether it indexes a hash, and a hostile or impersonated endpoint answers yes to
	// everything; fetching the key from the same endpoint being verified is circular, so the key
	// MUST come from the operator. With it pinned, the entry's inclusion proof and
	// signed-entry-timestamp are verified cryptographically and a forged response fails closed.
	//
	// Optional for compatibility: when unset the check degrades to index presence only. The
	// controller records that only in a debug-level log line; no condition, Event or status
	// field reports it.
	// +optional
	RekorPublicKey *SigningSecretKeyRef `json:"rekorPublicKey,omitempty"`
}

// SignatureVerificationRequired reports whether this namespace enforces artifact
// signature verification.
//
// nil resolves to TRUE, and that is forced rather than chosen: the field carries
// +kubebuilder:default=true, so the API server materializes an omitted key as true. An
// accessor that read nil as false would disagree with the API server about the same
// object. It also means the zero-valued SwarmadaSigningConfig returned for a namespace
// with NO SwarmadaConfig reads as enforcement ON, which is the fail-closed answer for a
// signing gate (ADR-0051).
func (s SwarmadaSigningConfig) SignatureVerificationRequired() bool {
	return s.RequireSignatureVerification == nil || *s.RequireSignatureVerification
}

// ── Emergency-stop delivery (RFC-0001 #swarmadaconfig-estop-delivery-configuration) ──────────────────────────────

// EstopPartialDeliveryBehavior selects the response when some adapters in a zone
// have not ACKd an estop while others have.
// +kubebuilder:validation:Enum=BlockNewActions;Alert
type EstopPartialDeliveryBehavior string

// EstopPartialDeliveryBehavior values.
const (
	// EstopBlockNewActions blocks new assignments to the zone until full delivery.
	EstopBlockNewActions EstopPartialDeliveryBehavior = "BlockNewActions"
	// EstopAlert emits a Warning only and adds no zone-wide block; Scheduler
	// filter 10 still withholds every robot under an active stop, whatever this value.
	EstopAlert EstopPartialDeliveryBehavior = "Alert"
)

// EstopRetryPolicy bounds estop redelivery on non-ACK (RFC-0001 #swarmadaconfig-estop-delivery-configuration).
type EstopRetryPolicy struct {
	// MaxAttempts is specified as the redelivery ceiling (0 = retry until ACK or
	// clear). No code reads it at this version: the estop dispatcher makes a single
	// delivery attempt per robot and never retries.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
	// RetryIntervalMs is specified as the delay between redelivery attempts. No code
	// reads it at this version: the estop dispatcher never retries a delivery.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:default=250
	RetryIntervalMs int32 `json:"retryIntervalMs,omitempty"`
}

// EstopDeliveryConfig governs estop delivery over the SafetyStream (RFC-0001 #swarmadaconfig-estop-delivery-configuration).
type EstopDeliveryConfig struct {
	// PerAdapterTimeoutMs is the maximum wait for an adapter's EstopAck for a robot.
	// ACK means stop INITIATED, not complete. When it passes with no ACK the robot's
	// estopState becomes Failed (a dropped estop); the delivery is not retried.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:default=500
	PerAdapterTimeoutMs int32 `json:"perAdapterTimeoutMs,omitempty"`
	// RetryPolicy is specified to bound redelivery. No code reads it at this
	// version: the estop dispatcher makes a single delivery attempt per robot.
	// +optional
	RetryPolicy EstopRetryPolicy `json:"retryPolicy,omitempty"`
	// PartialDeliveryBehavior is specified to select the response when some adapters
	// have not ACKd an estop. No code reads it at this version: there is no
	// partial-delivery aggregation, and each robot's unconfirmed stop is reported on
	// its own estopState (Failed).
	// +kubebuilder:default=BlockNewActions
	PartialDeliveryBehavior EstopPartialDeliveryBehavior `json:"partialDeliveryBehavior,omitempty"`
}

// SwarmadaEstopConfig configures emergency-stop delivery (RFC-0001 #swarmadaconfig-estop-delivery-configuration).
type SwarmadaEstopConfig struct {
	// Delivery governs SafetyStream estop delivery and retry.
	// +optional
	Delivery EstopDeliveryConfig `json:"delivery,omitempty"`
	// RequireExplicitClearAfterEstop records, for traceability, that a robot which ACKd an
	// estop MUST NOT resume until an operator explicitly clears it. The prohibition is
	// UNCONDITIONAL: there is no auto-clear path in the control plane, so this field
	// describes the rule rather than controlling it, and setting it false changes nothing.
	// Admission warns when it is set false rather than silently accepting the value.
	// +kubebuilder:default=true
	// +optional
	RequireExplicitClearAfterEstop *bool `json:"requireExplicitClearAfterEstop,omitempty"`
}

// ── Action cancellation on disconnect (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect) ──────────────────────

// ActionCancellationPolicy governs FleetAction fate when its assigned robot
// disconnects. The lease model (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee) remains the primary dual-execution
// safeguard; this is an additional wall-clock policy layer.
// +kubebuilder:validation:Enum=Never;AfterTimeout;WhenActionExpired
type ActionCancellationPolicy string

// ActionCancellationPolicy values.
const (
	// ActionCancellationNever gives a lost robot's action no automatic outcome: an
	// operator cancels it (RFC-0001 #safety-action-table, rows
	// A9 and A10). Safest for non-idempotent actions.
	ActionCancellationNever ActionCancellationPolicy = "Never"
	// ActionCancellationAfterTimeout lets row A9 reassign a Revoking action once
	// DisconnectTimeoutSeconds have passed since status.disconnectedAt, its lease is
	// provably dead and no stop holds its robot. It moves no held work (RFC-0001 #safety-action-table, Terms).
	ActionCancellationAfterTimeout ActionCancellationPolicy = "AfterTimeout"
	// ActionCancellationWhenActionExpired lets row A10 cancel a Revoking action once
	// its own timeoutSeconds has elapsed and its lease is provably dead, unless it is
	// held work (RFC-0001 #safety-action-table, Terms).
	ActionCancellationWhenActionExpired ActionCancellationPolicy = "WhenActionExpired"
)

// SwarmadaActionCancellationConfig configures disconnect handling (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect).
type SwarmadaActionCancellationConfig struct {
	// OnDisconnect selects the policy. Never (default) is safest for
	// non-idempotent actions.
	// +kubebuilder:default=Never
	OnDisconnect ActionCancellationPolicy `json:"onDisconnect,omitempty"`
	// DisconnectTimeoutSeconds is the wait since status.disconnectedAt that row A9
	// (RFC-0001 #safety-action-table) adds to its lease and estop
	// gates before a Revoking action is reassigned to another robot; it is not
	// cancelled, so a non-idempotent action can run twice. Required when
	// OnDisconnect is AfterTimeout.
	// +optional
	// +kubebuilder:validation:Minimum=30
	DisconnectTimeoutSeconds *int32 `json:"disconnectTimeoutSeconds,omitempty"`
}

// ── Traffic Deconfliction Engine tunables (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration, #tde) ─────────

// ReservationExpiryAction selects what happens when a Reserved slot expires
// before zone entry is confirmed.
// +kubebuilder:validation:Enum=ReleaseAndRequeue;ReleaseOnly
type ReservationExpiryAction string

// ReservationExpiryAction values.
const (
	// ReservationReleaseAndRequeue frees the slot and returns the action to Pending.
	ReservationReleaseAndRequeue ReservationExpiryAction = "ReleaseAndRequeue"
	// ReservationReleaseOnly frees the slot and leaves the action Assigned.
	ReservationReleaseOnly ReservationExpiryAction = "ReleaseOnly"
)

// TDERecoveryFallback selects the TDE startup recovery action when the Zone
// Controller is not Ready in time.
// +kubebuilder:validation:Enum=ReleaseAll;ReleaseReservedOnly
type TDERecoveryFallback string

// TDERecoveryFallback values.
const (
	// TDEReleaseAll releases all reservations (safe, disruptive).
	TDEReleaseAll TDERecoveryFallback = "ReleaseAll"
	// TDEReleaseReservedOnly releases Reserved only, assuming Occupied are valid.
	TDEReleaseReservedOnly TDERecoveryFallback = "ReleaseReservedOnly"
)

// TDERecoveryConfig tunes TDE startup recovery (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration).
type TDERecoveryConfig struct {
	// ZoneControllerWaitTimeoutSeconds is how long the TDE waits for the Zone
	// Controller to report Ready before applying ConservativeRecoveryFallback.
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:default=30
	ZoneControllerWaitTimeoutSeconds int32 `json:"zoneControllerWaitTimeoutSeconds,omitempty"`
	// ConservativeRecoveryFallback is the action if the Zone Controller is not
	// Ready within the timeout.
	// +kubebuilder:default=ReleaseAll
	ConservativeRecoveryFallback TDERecoveryFallback `json:"conservativeRecoveryFallback,omitempty"`
}

// SwarmadaTrafficDeconflictionConfig exposes the TDE tunables (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration). All
// TDE semantics are specified in RFC-0001 #tde; this is only the configuration surface.
type SwarmadaTrafficDeconflictionConfig struct {
	// TDECallTimeoutMs is specified to bound a RequestReservation call, denying the
	// reservation when exceeded. No code reads it at this version: no TDE call site
	// applies a timeout from it.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:default=200
	TDECallTimeoutMs int32 `json:"tdeCallTimeoutMs,omitempty"`
	// ReservationTTLSeconds is how long a Reserved slot is held before expiry if
	// the robot does not enter the zone.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:default=120
	ReservationTTLSeconds int32 `json:"reservationTTLSeconds,omitempty"`
	// DisconnectedReservationTTLSeconds is the extended TTL applied when the action
	// transitions to Revoking. MUST exceed ActionCancellation.DisconnectTimeoutSeconds
	// when AfterTimeout is used (enforced by the SwarmadaConfig admission webhook).
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:default=360
	DisconnectedReservationTTLSeconds int32 `json:"disconnectedReservationTTLSeconds,omitempty"`
	// OnReservationExpiry is specified to select the action when a Reserved slot
	// expires. No code reads it at this version, and neither value is applied: an
	// expired reservation stops counting against zone capacity, but nothing releases
	// it from FleetZone status or returns its action to Pending.
	// +kubebuilder:default=ReleaseAndRequeue
	OnReservationExpiry ReservationExpiryAction `json:"onReservationExpiry,omitempty"`
	// MinRetryAfterSeconds bounds the FleetAction reconciler backoff floor on Denied.
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:default=10
	MinRetryAfterSeconds int32 `json:"minRetryAfterSeconds,omitempty"`
	// MaxRetryAfterSeconds bounds the FleetAction reconciler backoff ceiling on Denied.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:default=120
	MaxRetryAfterSeconds int32 `json:"maxRetryAfterSeconds,omitempty"`
	// Recovery tunes TDE startup recovery.
	// +optional
	Recovery TDERecoveryConfig `json:"recovery,omitempty"`
}

// ── Coordinate system (RFC-0001 #swarmadaconfig-coordinate-system) ───────────────────────────────────

// LengthUnit is the distance unit for all x/y coordinates in the namespace.
// +kubebuilder:validation:Enum=Meters;Millimeters
type LengthUnit string

// LengthUnit values.
const (
	// LengthUnitMeters is the default distance unit (metres).
	LengthUnitMeters LengthUnit = "Meters"
	// LengthUnitMillimeters is the millimetre distance unit.
	LengthUnitMillimeters LengthUnit = "Millimeters"
)

// AngleUnit is the unit for all yaw values in the namespace.
// +kubebuilder:validation:Enum=Radians;Degrees
type AngleUnit string

// AngleUnit values.
const (
	// AngleUnitRadians is the default yaw unit (radians; matches proto RobotPosition.yaw).
	AngleUnitRadians AngleUnit = "Radians"
	// AngleUnitDegrees is the degrees yaw unit.
	AngleUnitDegrees AngleUnit = "Degrees"
)

// ReferenceFrame selects the spatial reference convention for a namespace
// (RFC-0001 #swarmadaconfig-coordinate-system). A namespace picks exactly one; the control plane never mixes or
// auto-converts between them.
// +kubebuilder:validation:Enum=Local;Geodetic
type ReferenceFrame string

// ReferenceFrame values.
const (
	// ReferenceFrameLocal is a facility-relative planar Cartesian frame (default;
	// uses lengthUnit/angleUnit/groundFloor/origin). Suits indoor/ground fleets.
	ReferenceFrameLocal ReferenceFrame = "Local"
	// ReferenceFrameGeodetic is a WGS84 latitude/longitude/altitude frame. Suits
	// aerial fleets and any deployment needing a global reference frame.
	ReferenceFrameGeodetic ReferenceFrame = "Geodetic"
)

// GeodeticDatum is the horizontal datum for a Geodetic reference frame. WGS84
// only at v0.1 — the datum used by GPS, ADS-B, and consumer/commercial drone
// autopilots.
// +kubebuilder:validation:Enum=WGS84
type GeodeticDatum string

// GeodeticDatum values.
const (
	// GeodeticDatumWGS84 is the WGS84 horizontal datum.
	GeodeticDatumWGS84 GeodeticDatum = "WGS84"
)

// AltitudeReference is the vertical reference for a Geodetic reference frame.
// +kubebuilder:validation:Enum=AGL;MSL
type AltitudeReference string

// AltitudeReference values.
const (
	// AltitudeReferenceAGL is above-ground-level (relative to the takeoff/home
	// point); the common convention for local drone operations.
	AltitudeReferenceAGL AltitudeReference = "AGL"
	// AltitudeReferenceMSL is mean-sea-level; required when coordinating across
	// sites or with ATC/UTM systems that expect an absolute vertical reference.
	AltitudeReferenceMSL AltitudeReference = "MSL"
)

// CoordinateOrigin describes the physical site origin — the (0,0) point all x/y
// are measured from (RFC-0001 #swarmadaconfig-coordinate-system).
type CoordinateOrigin struct {
	// Description is a human-readable description of the site origin.
	// Informational; aligns operators and tooling.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`
}

// GeodeticFrame holds the datum and altitude reference for a Geodetic coordinate
// system (RFC-0001 #swarmadaconfig-coordinate-system). Required when referenceFrame is Geodetic and MUST be unset
// when referenceFrame is Local (enforced by the SwarmadaConfig validating webhook).
type GeodeticFrame struct {
	// Datum is the horizontal datum. WGS84 only at v0.1.
	// +kubebuilder:default=WGS84
	Datum GeodeticDatum `json:"datum,omitempty"`
	// AltitudeReference is the vertical reference (AGL or MSL).
	// +kubebuilder:default=AGL
	AltitudeReference AltitudeReference `json:"altitudeReference,omitempty"`
}

// SwarmadaCoordinateSystemConfig declares the facility-wide spatial conventions in
// which every coordinate elsewhere in the API is ALREADY expressed — FleetZone
// physicalBounds/waypoints, Robot.status.position, and the edge PositionFrame
// stream (RFC-0001 #swarmadaconfig-coordinate-system). It is declarative and descriptive: the control plane never
// transforms coordinates, only validates, annotates, and informs consumers.
type SwarmadaCoordinateSystemConfig struct {
	// ReferenceFrame selects Local (default) or Geodetic conventions for the
	// namespace. Local uses the lengthUnit/angleUnit/groundFloor/origin fields;
	// Geodetic uses the geodetic block.
	// +kubebuilder:default=Local
	ReferenceFrame ReferenceFrame `json:"referenceFrame,omitempty"`
	// LengthUnit is the distance unit for all x/y values (Local frame).
	// +kubebuilder:default=Meters
	LengthUnit LengthUnit `json:"lengthUnit,omitempty"`
	// AngleUnit is the unit for all yaw values.
	// +kubebuilder:default=Radians
	AngleUnit AngleUnit `json:"angleUnit,omitempty"`
	// GroundFloor is the integer floor value that denotes ground level;
	// Robot.status.position.floor and FleetZone.spec.physicalBounds.floor are interpreted
	// relative to it. 0 = ground.
	// +kubebuilder:default=0
	GroundFloor int32 `json:"groundFloor,omitempty"`
	// Origin describes the physical (0,0) point (Local frame).
	// +optional
	Origin CoordinateOrigin `json:"origin,omitempty"`
	// Geodetic holds the datum and altitude reference. Required when
	// referenceFrame is Geodetic; MUST be unset when referenceFrame is Local.
	// +optional
	Geodetic *GeodeticFrame `json:"geodetic,omitempty"`
}

// ── Status (RFC-0001 #swarmadaconfig-schema conditions table, RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split Invariant 1) ──────────

// SwarmadaConfig status condition types and reasons.
const (
	// ConditionTelemetrySinkUnconfigured is True when spec.telemetry.sink.type is
	// unset — high-cadence telemetry is discarded by the no-op writer and is not
	// counted in swarmada_telemetry_dropped_frames_total. It is False once a real store or an
	// explicit Drop is set (RFC-0001 #swarmadaconfig-schema, RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split Invariant 1).
	ConditionTelemetrySinkUnconfigured = "TelemetrySinkUnconfigured"
	// ReasonSinkNotConfigured is the reason when sink.type is unset.
	ReasonSinkNotConfigured = "SinkNotConfigured"
	// ReasonSinkConfigured is the reason when a real store or Drop is set.
	ReasonSinkConfigured = "SinkConfigured"

	// ConditionSigningIncomplete is True when spec.signing requires signature
	// verification and configures no trustRoots. Every consumer of the policy then
	// fails closed, so no FleetAdapter conformance report, firmware or model artifact
	// can verify in the namespace until a trustRoot is added (ADR-0051: incomplete,
	// not invalid). The auto-created SwarmadaConfig starts in this state.
	ConditionSigningIncomplete = "SigningIncomplete"
	// ReasonNoTrustRoots is the reason when verification is required and no trustRoot is configured.
	ReasonNoTrustRoots = "NoTrustRoots"
	// ReasonTrustRootsConfigured is the reason when verification is required and a trustRoot is configured.
	ReasonTrustRootsConfigured = "TrustRootsConfigured"
	// ReasonVerificationDisabled is the reason when requireSignatureVerification is false.
	ReasonVerificationDisabled = "VerificationDisabled"
)

// SwarmadaConfigStatus is the observed state of a SwarmadaConfig.
type SwarmadaConfigStatus struct {
	// Conditions surface pipeline health, notably TelemetrySinkUnconfigured.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SupportedContractRange is the fleet-adapter contract-version range this control plane
	// accepts, as a two-clause semver range ">=MAJOR.(N-1).0 <MAJOR.(N+1).0": the implemented
	// minor N and its predecessor within the current major (ADR-0032); at minor 0, that minor
	// alone. An adapter reporting a version outside it keeps its ControlStream connection
	// (telemetry, heartbeat and estop are unaffected), and every RegisterRobot and DiscoverRobot
	// it sends is refused with REGISTRATION_REJECTION_VERSION_MISMATCH.
	//
	// It is ADVERTISED, not configured: the controller populates it from the compiled-in contract
	// version (internal/contract.Version), so an operator can answer "which adapters can this
	// control plane drive?" without reading code, and a vendor can key its qualification on a
	// value the cluster states. Editing it has no effect — the controller restores it.
	//
	// Written transition-only (RA-1): only when the computed range differs from what is stored,
	// and never on a telemetry tick.
	// +optional
	SupportedContractRange string `json:"supportedContractRange,omitempty"`
}

// SwarmadaConfigSpec defines the desired state of SwarmadaConfig.
type SwarmadaConfigSpec struct {
	Health       SwarmadaHealthConfig       `json:"health,omitempty"`
	Scheduling   SwarmadaSchedulingConfig   `json:"scheduling,omitempty"`
	Provisioning SwarmadaProvisioningConfig `json:"provisioning,omitempty"`
	Maintenance  SwarmadaMaintenanceConfig  `json:"maintenance,omitempty"`
	// Telemetry configures the two-data-plane split (RFC-0001 #swarmadaconfig-telemetry-pipeline-and-status-projection, RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split).
	// +optional
	Telemetry SwarmadaTelemetryConfig `json:"telemetry,omitempty"`
	// Signing configures artifact signature trust roots (RFC-0001 #swarmadaconfig-schema).
	// +optional
	Signing SwarmadaSigningConfig `json:"signing,omitempty"`
	// Estop configures emergency-stop delivery (RFC-0001 #swarmadaconfig-estop-delivery-configuration).
	// +optional
	Estop SwarmadaEstopConfig `json:"estop,omitempty"`
	// ActionCancellation configures disconnect handling (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect).
	// +optional
	ActionCancellation SwarmadaActionCancellationConfig `json:"actionCancellation,omitempty"`
	// TrafficDeconfliction exposes the TDE tunables (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration).
	// +optional
	TrafficDeconfliction SwarmadaTrafficDeconflictionConfig `json:"trafficDeconfliction,omitempty"`
	// CoordinateSystem declares facility-wide spatial conventions (RFC-0001 #swarmadaconfig-coordinate-system).
	// +optional
	CoordinateSystem SwarmadaCoordinateSystemConfig `json:"coordinateSystem,omitempty"`
}

// SwarmadaConfig is the Schema for the swarmadaconfigs API.
//
// Exactly one SwarmadaConfig per namespace, with the fixed name "swarmada-config".
// Auto-created with defaults when a namespace is initialized (first FleetZone created).
// Provides namespace-level tunables for health tracking, provisioning, scheduling,
// maintenance, estop delivery and traffic deconfliction. Several fields are
// validated and defaulted but read by no controller at this version; each one's
// description says so.
//
// The name MUST be "swarmada-config" (RFC-0001 #control-plane-swarmada-api-server). Cross-field rules from RFC-0001 #swarmadaconfig
// (AfterTimeout ⇒ disconnectTimeoutSeconds; disconnectedReservationTTLSeconds >
// disconnectTimeoutSeconds; sink.endpoint required for a real store;
// connectivityCriticalThresholdSeconds > connectivityOfflineThresholdSeconds;
// referenceFrame Geodetic ⇔ geodetic block) are enforced by the SwarmadaConfig
// admission webhook. requireSignatureVerification with no trustRoots is warned
// about, not rejected. The empty
// default sink.type is deliberately NOT rejected here: auto-creation (RFC-0001 #swarmadaconfig-auto-creation-on-namespace-initialization)
// produces a defaulted config the operator then completes.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sc
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'swarmada-config'",message="SwarmadaConfig must be named 'swarmada-config'"
type SwarmadaConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwarmadaConfigSpec   `json:"spec,omitempty"`
	Status            SwarmadaConfigStatus `json:"status,omitempty"`
}

// SwarmadaConfigList contains a list of SwarmadaConfig resources.
// +kubebuilder:object:root=true
type SwarmadaConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwarmadaConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SwarmadaConfig{}, &SwarmadaConfigList{})
}

// ExplicitClearRequiredAfterEstop reports whether an estopped robot needs an explicit
// operator clear before it may resume.
//
// It is ALWAYS true. The prohibition is enforced by the absence of any auto-clear path, not
// by this field, so the stored value cannot change the answer. It is an accessor rather
// than a constant so that every read goes through one documented place, and so the shape
// matches the other default-true booleans in this API (ADR-0052).
func (e SwarmadaEstopConfig) ExplicitClearRequiredAfterEstop() bool {
	return true
}
