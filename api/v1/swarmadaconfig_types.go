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
	// TelemetryIntervalSeconds: default passive telemetry rate for robots.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	TelemetryIntervalSeconds int32 `json:"telemetryIntervalSeconds,omitempty"`
	// CapabilityRescanIntervalSeconds: full capability snapshot via
	// ScanCapabilities RPC. 0 disables periodic rescanning.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	CapabilityRescanIntervalSeconds int32 `json:"capabilityRescanIntervalSeconds,omitempty"`
	// DefaultHardwareProbeIntervalSeconds: default for RobotProbe resources.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=3600
	DefaultHardwareProbeIntervalSeconds int32 `json:"defaultHardwareProbeIntervalSeconds,omitempty"`
	// DefaultProbeTimeoutSeconds is the default probe timeout.
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
	// ConnectivityOfflineThresholdSeconds: seconds without telemetry before Offline.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=3600
	ConnectivityOfflineThresholdSeconds int32 `json:"connectivityOfflineThresholdSeconds,omitempty"`
	// ConnectivityCriticalThresholdSeconds: seconds Offline before Critical.
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
	// ActionRequeueBackoffSeconds: wait time between action requeue attempts.
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

	// LeaseDurationSeconds is the task-lease horizon (§9.6.3.5): how long a robot may
	// execute an assigned action without a renewal before it MUST self-stop.
	//
	// This is a SAFETY bound, not a tuning knob. The resolved value is both written to
	// FleetAction.status.leaseExpiresAt and sent to the robot as Command.lease_duration_ms,
	// which arms the adapter's self-stop timer (§9.2.8 "Task-lease self-stop", §9.6.3.3
	// item 1). Raising it directly extends how long a robot that has lost its link keeps
	// moving before halting itself — hence the 300s ceiling. The 10s floor keeps the
	// horizon above the derived renewal interval by a workable margin.
	//
	// The renewal interval is always leaseDurationSeconds/3 (§9.3.2) and is NOT separately
	// configurable: deriving it makes it impossible to widen the horizon without also
	// renewing proportionally sooner (ADR-0044).
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=300
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds,omitempty"`

	// ClockSkewMarginSeconds is the margin added before a lease is treated as provably
	// expired (§9.6.3.5 condition 3: now ≥ leaseExpiresAt + skew). It absorbs clock
	// disagreement between the control plane and the robot.
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
	// Mode controls the discovery-to-admission flow.
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
	// dead (§9.6.3.5). Off by default. Only Robots that were auto-admitted (ADR-0014) are
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
	// DefaultAutoResumeMinutes: 0 means no auto-resume (operator must resume).
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=480
	DefaultAutoResumeMinutes int32 `json:"defaultAutoResumeMinutes,omitempty"`
	// RequireEstopClearBeforeResume is the namespace default for the ZoneMaintenance
	// field of the same name: block resume of a paused robot until any active estop
	// on it is cleared.
	// Enforced by the ZoneMaintenance controller as the namespace-level fallback
	// when ZoneMaintenance.spec.requireEstopClearBeforeResume is unset.
	// +optional
	// +kubebuilder:default=true
	RequireEstopClearBeforeResume *bool `json:"requireEstopClearBeforeResume,omitempty"`
}

// ── Telemetry pipeline (RFC-0001 §9.1.11.7, §9.3.7) ───────────────────────────

// TelemetrySinkType selects the high-cadence TSDB sink. The empty default is a
// deliberate "unconfigured" signal, NOT a silent drop: the control plane enters
// observed-degraded mode and raises the TelemetrySinkUnconfigured condition until
// a real store or an explicit Drop is set (§9.3.7 Invariant 1).
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
	// TelemetrySinkVictoriaMetrics writes via the VictoriaMetrics ingest API.
	TelemetrySinkVictoriaMetrics TelemetrySinkType = "VictoriaMetrics"
	// TelemetrySinkMimir writes via a Grafana Mimir remote-write endpoint.
	TelemetrySinkMimir TelemetrySinkType = "Mimir"
)

// TelemetrySink is the high-cadence TSDB sink target (§9.1.11.1).
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

// SwarmadaTelemetryConfig configures the two-data-plane split (§9.3.7): where
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

// ── Artifact signing (RFC-0001 §9.1.11.1, §9.5.5) ─────────────────────────────

// SigningSecretKeyRef selects a key within a namespace-local Secret holding a
// PEM-encoded cosign public key or x509 signing certificate.
type SigningSecretKeyRef struct {
	// Name is the Secret name in the same namespace.
	Name string `json:"name"`
	// Key is the data key holding the PEM material.
	Key string `json:"key"`
}

// SigningTrustRoot is a public-key anchor against which artifact signatures are
// verified (§9.1.11.1).
type SigningTrustRoot struct {
	// Name is a logical identifier, written to the audit-log detail.
	Name string `json:"name"`
	// SecretRef references the Secret and key holding the trust anchor.
	SecretRef SigningSecretKeyRef `json:"secretRef"`
}

// SwarmadaSigningConfig configures artifact signature verification for firmware
// and model updates (§9.1.11.1). Adapters verify artifacts against these trust
// roots before install and fail closed on failure.
type SwarmadaSigningConfig struct {
	// RequireSignatureVerification, when true (default), forces adapters to verify
	// every firmware/model artifact against a trust root before install.
	// +kubebuilder:default=true
	RequireSignatureVerification bool `json:"requireSignatureVerification,omitempty"`
	// TrustRoots are the public-key anchors. At least one is required when
	// RequireSignatureVerification is true (enforced by the SwarmadaConfig
	// admission webhook — see the note on SwarmadaConfig).
	// +optional
	TrustRoots []SigningTrustRoot `json:"trustRoots,omitempty"`
	// RekorURL, when set, requires the artifact's signature entry to appear in the
	// named transparency log before acceptance.
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
	// Optional for compatibility: when unset the check degrades to index presence only, and the
	// controller reports that it did so rather than implying a verified entry.
	// +optional
	RekorPublicKey *SigningSecretKeyRef `json:"rekorPublicKey,omitempty"`
}

// ── Emergency-stop delivery (RFC-0001 §9.1.11.8) ──────────────────────────────

// EstopPartialDeliveryBehavior selects the response when some adapters in a zone
// have not ACKd an estop while others have.
// +kubebuilder:validation:Enum=BlockNewActions;Alert
type EstopPartialDeliveryBehavior string

// EstopPartialDeliveryBehavior values.
const (
	// EstopBlockNewActions blocks new assignments to the zone until full delivery.
	EstopBlockNewActions EstopPartialDeliveryBehavior = "BlockNewActions"
	// EstopAlert emits a Warning only and does not block assignments.
	EstopAlert EstopPartialDeliveryBehavior = "Alert"
)

// EstopRetryPolicy bounds estop redelivery on non-ACK (§9.1.11.8).
type EstopRetryPolicy struct {
	// MaxAttempts is the retry ceiling; 0 = retry indefinitely until ACK or clear.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
	// RetryIntervalMs is the delay between attempts.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:default=250
	RetryIntervalMs int32 `json:"retryIntervalMs,omitempty"`
}

// EstopDeliveryConfig governs estop delivery over the SafetyStream (§9.1.11.8).
type EstopDeliveryConfig struct {
	// PerAdapterTimeoutMs is the max wait for one adapter to ACK before retrying.
	// ACK means stop INITIATED, not complete.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:default=500
	PerAdapterTimeoutMs int32 `json:"perAdapterTimeoutMs,omitempty"`
	// RetryPolicy bounds redelivery.
	// +optional
	RetryPolicy EstopRetryPolicy `json:"retryPolicy,omitempty"`
	// PartialDeliveryBehavior selects the response when delivery is partial.
	// +kubebuilder:default=BlockNewActions
	PartialDeliveryBehavior EstopPartialDeliveryBehavior `json:"partialDeliveryBehavior,omitempty"`
}

// SwarmadaEstopConfig configures emergency-stop delivery (§9.1.11.8).
type SwarmadaEstopConfig struct {
	// Delivery governs SafetyStream estop delivery and retry.
	// +optional
	Delivery EstopDeliveryConfig `json:"delivery,omitempty"`
	// RequireExplicitClearAfterEstop is documented for traceability (always true):
	// robots that ACKd an estop MUST NOT resume until an operator explicitly clears
	// it.
	// +kubebuilder:default=true
	RequireExplicitClearAfterEstop bool `json:"requireExplicitClearAfterEstop,omitempty"`
}

// ── Action cancellation on disconnect (RFC-0001 §9.1.11.9) ──────────────────────

// ActionCancellationPolicy governs FleetAction fate when its assigned robot
// disconnects. The lease model (§9.6.3.5) remains the primary dual-execution
// safeguard; this is an additional wall-clock policy layer.
// +kubebuilder:validation:Enum=Never;AfterTimeout;WhenActionExpired
type ActionCancellationPolicy string

// ActionCancellationPolicy values.
const (
	// ActionCancellationNever keeps the action Revoking until an operator cancels
	// (safest for non-idempotent actions).
	ActionCancellationNever ActionCancellationPolicy = "Never"
	// ActionCancellationAfterTimeout auto-cancels after DisconnectTimeoutSeconds.
	ActionCancellationAfterTimeout ActionCancellationPolicy = "AfterTimeout"
	// ActionCancellationWhenActionExpired auto-cancels when the action's own timeout hits.
	ActionCancellationWhenActionExpired ActionCancellationPolicy = "WhenActionExpired"
)

// SwarmadaActionCancellationConfig configures disconnect handling (§9.1.11.9).
type SwarmadaActionCancellationConfig struct {
	// OnDisconnect selects the policy. Never (default) is safest for
	// non-idempotent actions.
	// +kubebuilder:default=Never
	OnDisconnect ActionCancellationPolicy `json:"onDisconnect,omitempty"`
	// DisconnectTimeoutSeconds is the wall-clock ceiling before a Revoking action is
	// auto-cancelled. Required when OnDisconnect is AfterTimeout.
	// +optional
	// +kubebuilder:validation:Minimum=30
	DisconnectTimeoutSeconds *int32 `json:"disconnectTimeoutSeconds,omitempty"`
}

// ── Traffic Deconfliction Engine tunables (RFC-0001 §9.1.11.10, §9.4) ─────────

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

// TDERecoveryConfig tunes TDE startup recovery (§9.1.11.10).
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

// SwarmadaTrafficDeconflictionConfig exposes the TDE tunables (§9.1.11.10). All
// TDE semantics are specified in §9.4; this is only the configuration surface.
type SwarmadaTrafficDeconflictionConfig struct {
	// TDECallTimeoutMs bounds a RequestReservation call; exceeding it denies the
	// reservation and returns the action to Pending.
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
	// OnReservationExpiry selects the action when a Reserved slot expires.
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

// ── Coordinate system (RFC-0001 §9.1.11.11) ───────────────────────────────────

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
// (§9.1.11.11). A namespace picks exactly one; the control plane never mixes or
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
// are measured from (§9.1.11.11).
type CoordinateOrigin struct {
	// Description is a human-readable description of the site origin.
	// Informational; aligns operators and tooling.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`
}

// GeodeticFrame holds the datum and altitude reference for a Geodetic coordinate
// system (§9.1.11.11). Required when referenceFrame is Geodetic and MUST be unset
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
// stream (§9.1.11.11). It is declarative and descriptive: the control plane never
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
	// Robot.status.position.floor and FleetZone.spec.floor are interpreted
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

// ── Status (RFC-0001 §9.1.11.1 conditions table, §9.3.7 Invariant 1) ──────────

// SwarmadaConfig status condition types and reasons.
const (
	// ConditionTelemetrySinkUnconfigured is True when spec.telemetry.sink.type is
	// unset — high-cadence telemetry is not forwarded and frames are counted in
	// swarmada_telemetry_dropped_frames_total. It is False once a real store or an
	// explicit Drop is set (§9.1.11.1, §9.3.7 Invariant 1).
	ConditionTelemetrySinkUnconfigured = "TelemetrySinkUnconfigured"
	// ReasonSinkNotConfigured is the reason when sink.type is unset.
	ReasonSinkNotConfigured = "SinkNotConfigured"
	// ReasonSinkConfigured is the reason when a real store or Drop is set.
	ReasonSinkConfigured = "SinkConfigured"
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
	// accepts (a semver range, e.g. ">=1.0.0 <1.1.0"): the implemented minor and its predecessor
	// within the current major (ADR-0032). An adapter reporting a version outside it is rejected
	// at the handshake with REGISTRATION_REJECTION_VERSION_MISMATCH.
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
	// Telemetry configures the two-data-plane split (§9.1.11.7, §9.3.7).
	// +optional
	Telemetry SwarmadaTelemetryConfig `json:"telemetry,omitempty"`
	// Signing configures artifact signature trust roots (§9.1.11.1).
	// +optional
	Signing SwarmadaSigningConfig `json:"signing,omitempty"`
	// Estop configures emergency-stop delivery (§9.1.11.8).
	// +optional
	Estop SwarmadaEstopConfig `json:"estop,omitempty"`
	// ActionCancellation configures disconnect handling (§9.1.11.9).
	// +optional
	ActionCancellation SwarmadaActionCancellationConfig `json:"actionCancellation,omitempty"`
	// TrafficDeconfliction exposes the TDE tunables (§9.1.11.10).
	// +optional
	TrafficDeconfliction SwarmadaTrafficDeconflictionConfig `json:"trafficDeconfliction,omitempty"`
	// CoordinateSystem declares facility-wide spatial conventions (§9.1.11.11).
	// +optional
	CoordinateSystem SwarmadaCoordinateSystemConfig `json:"coordinateSystem,omitempty"`
}

// SwarmadaConfig is the Schema for the swarmadaconfigs API.
//
// Exactly one SwarmadaConfig per namespace, with the fixed name "swarmada-config".
// Auto-created with defaults when a namespace is initialized (first FleetZone created).
// Provides namespace-level tunables for health tracking, re-scanning,
// provisioning, scheduling, and maintenance behaviour.
//
// The name MUST be "swarmada-config" (§9.3.1). Cross-field rules from §9.1.11
// (AfterTimeout ⇒ disconnectTimeoutSeconds; disconnectedReservationTTLSeconds >
// disconnectTimeoutSeconds; sink.endpoint required for a real store;
// requireSignatureVerification ⇒ non-empty trustRoots) are enforced by a
// SwarmadaConfig admission webhook — deferred, not yet built (§F). The empty
// default sink.type is deliberately NOT rejected here: auto-creation (§9.1.11.3)
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
