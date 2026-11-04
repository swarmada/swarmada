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

// ModelPolicyTriggerType defines how the ModelPolicy receives training completion events.
// +kubebuilder:validation:Enum=Webhook;RegistryWatch;Manual
type ModelPolicyTriggerType string

const (
	// ModelPolicyTriggerWebhook — training system POSTs metrics to a Swarmada-managed endpoint.
	// ModelPolicyTriggerWebhook — POST /webhooks/v1/model-policy/{namespace}/{name}
	ModelPolicyTriggerWebhook ModelPolicyTriggerType = "Webhook"

	// ModelPolicyTriggerRegistryWatch — the ModelPolicy controller polls an OCI registry for new
	// image tags matching modelName. On new tag: pulls image labels for metrics. It reads no
	// signature label, so a RegistryWatch trigger never carries a modelSignatureRef.
	ModelPolicyTriggerRegistryWatch ModelPolicyTriggerType = "RegistryWatch"

	// ModelPolicyTriggerManual — no automatic trigger. The operator writes the
	// swarmada.io/model-trigger annotation (a JSON payload with the metrics) to submit a
	// candidate for evaluation; swarmctl has no command for this at v0.4.
	ModelPolicyTriggerManual ModelPolicyTriggerType = "Manual"
)

// ModelPolicyDecision is the outcome of a quality gate evaluation.
// +kubebuilder:validation:Enum=Deploy;Reject;Pending
type ModelPolicyDecision string

// ModelPolicyDecision values (§9.1.10): the quality gate's verdict on a candidate model. Deploy
// auto-creates a ModelRollout (unless autoDeployOn is Manual); Reject counts toward the
// consecutive-rejection suspension; Pending is specified to mean the gate has not yet evaluated
// this trigger, and is written by no controller at v0.4.
const (
	ModelPolicyDecisionDeploy  ModelPolicyDecision = "Deploy"
	ModelPolicyDecisionReject  ModelPolicyDecision = "Reject"
	ModelPolicyDecisionPending ModelPolicyDecision = "Pending"
)

// AutoDeployCondition defines when the policy triggers automatic deployment.
// +kubebuilder:validation:Enum=QualityGatePass;NewVersion;Manual
type AutoDeployCondition string

const (
	// AutoDeployQualityGatePass — deploy automatically only when all quality gate metrics pass.
	AutoDeployQualityGatePass AutoDeployCondition = "QualityGatePass"
	// AutoDeployNewVersion — deploy any new version that arrives (no quality gate enforcement).
	// Use only for development/staging namespaces.
	AutoDeployNewVersion AutoDeployCondition = "NewVersion"
	// AutoDeployManual — quality gate is evaluated and recorded, and a pass emits a
	// QualityGatePass Event, but no ModelRollout is created. No approval command exists
	// at v0.4; deploying the version takes a hand-written ModelRollout.
	AutoDeployManual AutoDeployCondition = "Manual"
)

// CustomMetricOperator defines the comparison operation for custom metrics.
// +kubebuilder:validation:Enum=GreaterThan;LessThan;GreaterThanOrEqual;LessThanOrEqual;Equal;NotEqual
type CustomMetricOperator string

// CustomMetricOperator values: the comparison a custom quality-gate metric is evaluated with.
const (
	MetricOpGreaterThan        CustomMetricOperator = "GreaterThan"
	MetricOpLessThan           CustomMetricOperator = "LessThan"
	MetricOpGreaterThanOrEqual CustomMetricOperator = "GreaterThanOrEqual"
	MetricOpLessThanOrEqual    CustomMetricOperator = "LessThanOrEqual"
	MetricOpEqual              CustomMetricOperator = "Equal"
	MetricOpNotEqual           CustomMetricOperator = "NotEqual"
)

// ─────────────────────────────────────────────────────────────────────────────
// Spec sub-types
// ─────────────────────────────────────────────────────────────────────────────

// WebhookTriggerConfig configures the Webhook trigger mode.
type WebhookTriggerConfig struct {
	// Enabled enables the webhook endpoint for this policy.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// AuthSecretRef references a Secret containing the shared HMAC secret the
	// training system signs its webhook body with. Authentication is REQUIRED by
	// default (ADR-0020): if this is empty the endpoint rejects every request unless
	// allowUnauthenticated is explicitly set.
	// +optional
	AuthSecretRef string `json:"authSecretRef,omitempty"`

	// AllowUnauthenticated opts a policy INTO accepting unsigned webhook requests
	// when no authSecretRef is configured. It exists only for development and
	// simulation; it is never the default, and production policies must leave it
	// false and set authSecretRef (ADR-0020).
	// +optional
	// +kubebuilder:default=false
	AllowUnauthenticated bool `json:"allowUnauthenticated,omitempty"`
}

// RegistryWatchConfig configures the RegistryWatch trigger mode.
type RegistryWatchConfig struct {
	// Registry is the OCI registry hostname (e.g. "localhost:5000").
	// +kubebuilder:validation:Required
	Registry string `json:"registry"`

	// PollIntervalSeconds controls how often the controller checks for new tags.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:default=60
	PollIntervalSeconds int32 `json:"pollIntervalSeconds,omitempty"`

	// MetricsLabel is the OCI image label name containing quality metrics as JSON.
	// Keys are the quality-gate metric names (snake_case), values numbers.
	// Example: LABEL swarmada.metrics='{"pick_success_rate":0.94,"failure_rate":0.018}'
	// +kubebuilder:default="swarmada.metrics"
	MetricsLabel string `json:"metricsLabel,omitempty"`
}

// ModelPolicyTrigger configures how the policy receives training completion events.
type ModelPolicyTrigger struct {
	// Type selects the trigger mechanism.
	// +kubebuilder:default=Webhook
	Type ModelPolicyTriggerType `json:"type"`

	// Webhook configures the HTTP webhook endpoint (only when Type=Webhook).
	// +optional
	Webhook *WebhookTriggerConfig `json:"webhook,omitempty"`

	// RegistryWatch configures OCI registry polling (only when Type=RegistryWatch).
	// +optional
	RegistryWatch *RegistryWatchConfig `json:"registryWatch,omitempty"`
}

// CustomMetricRule defines a custom quality gate metric threshold.
type CustomMetricRule struct {
	// Name is the metric key in the webhook payload or OCI image label.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Operator is the comparison operation.
	// +kubebuilder:validation:Required
	Operator CustomMetricOperator `json:"operator"`

	// Threshold is the value to compare against.
	// +kubebuilder:validation:Required
	Threshold float64 `json:"threshold"`
}

// QualityGate defines the metrics thresholds that a trained model must satisfy
// before Swarmada will deploy it to the robot fleet.
// ALL rules must pass for the gate to be considered passed.
type QualityGate struct {
	// MinPickSuccessRate is the minimum fraction of successful pick operations
	// in the evaluation run (0.0–1.0).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	MinPickSuccessRate *float64 `json:"minPickSuccessRate,omitempty"`

	// MaxFailureRate is the maximum fraction of failed operations (0.0–1.0).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	MaxFailureRate *float64 `json:"maxFailureRate,omitempty"`

	// MinEvalEpisodes is the minimum number of evaluation episodes reported.
	// Guards against deploying a model that was evaluated on too few scenarios.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinEvalEpisodes *int32 `json:"minEvalEpisodes,omitempty"`

	// MaxSimToRealGap is the maximum acceptable performance gap between
	// simulation and real-world evaluation (0.0–1.0). It is evaluated fail-closed
	// (ADR-0021): when set, an absent real_pick_success_rate is treated as an
	// infinite gap and FAILS the gate — real-hardware validation is required, it is
	// never skipped because real metrics were not reported.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	MaxSimToRealGap *float64 `json:"maxSimToRealGap,omitempty"`

	// RequireRealEval, when true, fails the gate closed unless the payload carries
	// real-hardware evaluation metrics (real_pick_success_rate), independently of
	// whether maxSimToRealGap is set. It defaults true whenever maxSimToRealGap is
	// set (that guard already needs real metrics); a simulation or development
	// namespace may set it false to permit simulation-only promotion — an explicit,
	// named opt-out, never the silent default (ADR-0021).
	// +optional
	RequireRealEval *bool `json:"requireRealEval,omitempty"`

	// CustomMetrics allows arbitrary key/value threshold rules from the
	// webhook payload or OCI image labels.
	// +optional
	CustomMetrics []CustomMetricRule `json:"customMetrics,omitempty"`
}

// RolloutTemplateSpec is the ModelRollout configuration applied when auto-deploying.
// It matches the key fields of ModelRolloutSpec without the model-specific fields
// (modelName, newVersion, modelUri — those come from the trigger event).
type RolloutTemplateSpec struct {
	// Strategy controls rolling update parameters.
	// +optional
	Strategy RolloutStrategy `json:"strategy,omitempty"`

	// SafetyConstraints restricts when updates may be applied.
	// +optional
	SafetyConstraints RolloutSafetyConstraints `json:"safetyConstraints,omitempty"`

	// RollbackPolicy on auto-created rollouts. Defaults to Manual — silent
	// rollbacks from auto-deployments are especially dangerous.
	// +kubebuilder:default=Manual
	RollbackPolicy ModelRollbackPolicy `json:"rollbackPolicy,omitempty"`
}

// ModelPolicySpec defines the desired state of a ModelPolicy.
//
// The quality gate is a judgement about the ARTIFACT, not about where it lands, so
// targeting is expressed with a discriminator and one selector per kind rather than
// with a second policy type. See the normative statement on the ModelPolicy root
// type: a gate that blocks a robot rollout blocks an edge rollout of the same
// artifact, and the reverse.
//
// Which selector is required follows targetKind. targetRobotSelector could not be
// reused for edge targets — its name says robots — so the edge case gets its own
// field, and neither is schema-required on its own; the pairing is enforced here,
// the way ComputeEndpointSpec enforces its binding/contract pairing.
// +kubebuilder:validation:XValidation:rule="!has(self.targetKind) || self.targetKind != 'Robot' || has(self.targetRobotSelector)",message="targetRobotSelector is required when targetKind is Robot"
// +kubebuilder:validation:XValidation:rule="!has(self.targetKind) || self.targetKind != 'EdgeNode' || has(self.targetNodeSelector)",message="targetNodeSelector is required when targetKind is EdgeNode"
// +kubebuilder:validation:XValidation:rule="!has(self.targetNodeSelector) || (has(self.targetKind) && self.targetKind == 'EdgeNode')",message="targetNodeSelector applies only to targetKind: EdgeNode"
type ModelPolicySpec struct {
	// ModelName is the logical model name this policy governs.
	// Must match InstalledModel.name on target robots.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`

	// TargetKind selects the kind of object this policy auto-deploys to. Defaults
	// to Robot, so a policy authored before this field existed is unchanged.
	// EdgeNode is specified but not implemented at this version: the policy creates a
	// ModelRollout with targetKind EdgeNode, which the ModelRollout controller refuses,
	// so no node is updated.
	// +optional
	// +kubebuilder:default=Robot
	TargetKind RolloutTargetKind `json:"targetKind,omitempty"`

	// TargetRobotSelector selects which robots will receive auto-deployments.
	// REQUIRED when TargetKind is Robot (the default), which is every policy that
	// predates TargetKind; the marker moved to a CEL rule on the type so an
	// EdgeNode policy is not forced to carry a robot selector it never reads.
	// Relaxing a required field is additive — no stored object becomes invalid.
	// An empty selector ({}) is refused: it matches every Robot in the namespace.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="targetRobotSelector is empty, and an empty selector matches every Robot in the namespace; select robots with matchLabels or matchExpressions"
	TargetRobotSelector *metav1.LabelSelector `json:"targetRobotSelector,omitempty"`

	// TargetNodeSelector selects which EdgeNodes are specified to receive
	// auto-deployments. REQUIRED when TargetKind is EdgeNode, and rejected otherwise.
	// EdgeNode targeting is not implemented at this version (see TargetKind), so no
	// node receives one. An empty selector ({}) is refused: it matches every EdgeNode
	// in the namespace.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="targetNodeSelector is empty, and an empty selector matches every EdgeNode in the namespace; select nodes with matchLabels or matchExpressions"
	TargetNodeSelector *metav1.LabelSelector `json:"targetNodeSelector,omitempty"`

	// Trigger configures how training completion events reach this policy.
	// +optional
	Trigger ModelPolicyTrigger `json:"trigger,omitempty"`

	// QualityGate defines the metrics thresholds a model must satisfy.
	// If empty, any model version with a well-formed modelChecksum triggers
	// deployment (use with caution).
	// +optional
	QualityGate *QualityGate `json:"qualityGate,omitempty"`

	// AutoDeployOn defines when automatic deployment is triggered.
	// +kubebuilder:default=QualityGatePass
	AutoDeployOn AutoDeployCondition `json:"autoDeployOn,omitempty"`

	// RolloutTemplate is the ModelRollout spec applied when auto-deploying.
	// +optional
	RolloutTemplate *RolloutTemplateSpec `json:"rolloutTemplate,omitempty"`

	// MaxConcurrentRollouts is specified to bound how many auto-triggered rollouts
	// run simultaneously. Specified, not implemented at v0.4: no controller reads it,
	// and every value behaves as 1. The controller admits one active rollout per
	// policy. While it runs, one further trigger waits; a trigger written while
	// another is waiting replaces it, and the replaced trigger is never evaluated,
	// recorded or reported by an Event.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MaxConcurrentRollouts int32 `json:"maxConcurrentRollouts,omitempty"`

	// ConsecutiveRejectionLimit is the number of consecutive quality-gate
	// rejections after which the controller sets a FailedRepeatedly condition and
	// SUSPENDS evaluation. Until an operator resets the policy (swarmctl modelpolicy
	// reset), each further trigger is dropped with a TriggerDroppedWhileSuspended
	// Warning Event on the policy; none is queued. 0 disables suspension (unlimited
	// retries). §9.1.10.4.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=5
	ConsecutiveRejectionLimit int32 `json:"consecutiveRejectionLimit,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Status sub-types
// ─────────────────────────────────────────────────────────────────────────────

// PolicyEvaluationRecord stores the result of one quality gate evaluation.
type PolicyEvaluationRecord struct {
	// Version is the model version that was evaluated.
	Version string `json:"version"`
	// ModelURI is the OCI URI of the evaluated model.
	ModelURI string `json:"modelUri"`
	// Decision is the outcome of the quality gate.
	Decision ModelPolicyDecision `json:"decision"`
	// ReportedMetrics are the raw metrics received from the training system.
	ReportedMetrics map[string]float64 `json:"reportedMetrics,omitempty"`
	// FailedRules lists which quality gate rules failed (empty on Deploy).
	FailedRules []string `json:"failedRules,omitempty"`
	// CreatedRollout is the ModelRollout name created on Deploy (empty on Reject).
	CreatedRollout string `json:"createdRollout,omitempty"`
	// EvaluatedAt is the time of the evaluation.
	EvaluatedAt metav1.Time `json:"evaluatedAt"`
}

// ModelPolicyStatus defines the observed state of a ModelPolicy.
type ModelPolicyStatus struct {
	// LastTriggerAt is the time the most recent trigger event was received.
	// +optional
	LastTriggerAt *metav1.Time `json:"lastTriggerAt,omitempty"`

	// LastDecision is the most recent quality gate decision.
	// +optional
	LastDecision ModelPolicyDecision `json:"lastDecision,omitempty"`

	// LastDecisionReason is a human-readable explanation of the decision.
	// +optional
	LastDecisionReason string `json:"lastDecisionReason,omitempty"`

	// ActiveRollout is the name of the currently running auto-created ModelRollout.
	// Empty when no rollout is in progress.
	// +optional
	ActiveRollout string `json:"activeRollout,omitempty"`

	// DeploymentCount is the total number of successful auto-deployments.
	// +optional
	// +kubebuilder:validation:Minimum=0
	DeploymentCount int32 `json:"deploymentCount,omitempty"`

	// RejectionCount is the total number of models blocked by the quality gate.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RejectionCount int32 `json:"rejectionCount,omitempty"`

	// ConsecutiveRejections counts quality-gate rejections since the last Deploy
	// decision (reset to 0 on any Deploy). When it reaches
	// spec.consecutiveRejectionLimit the FailedRepeatedly condition suspends
	// evaluation. §9.1.10.4.
	// +optional
	ConsecutiveRejections int32 `json:"consecutiveRejections,omitempty"`

	// WebhookEndpoint is specified to hold the endpoint URL when Trigger.Type=Webhook.
	// Written by no controller at v0.4: it stays empty, and the URL is
	// /webhooks/v1/model-policy/{namespace}/{name} on the manager's ModelPolicy
	// webhook listener.
	// +optional
	WebhookEndpoint string `json:"webhookEndpoint,omitempty"`

	// History stores the last 20 evaluation records (oldest removed first).
	// +optional
	History []PolicyEvaluationRecord `json:"history,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Root type
// ─────────────────────────────────────────────────────────────────────────────

// ModelPolicy is the Schema for the modelpolicies API.
//
// A ModelPolicy is the policy-governed bridge between an AI training pipeline
// (Isaac Lab, custom training systems) and Swarmada's ModelRollout deployment.
// It receives training completion events (via webhook or registry watch),
// evaluates reported metrics against a quality gate, and automatically creates
// a ModelRollout when the gate passes — without requiring operator intervention.
//
// This is the robotics equivalent of a CI/CD deployment policy: the training
// pipeline is CI (build the model), Swarmada ModelPolicy is CD (evaluate and
// deploy it to the fleet if it meets quality standards).
//
// The quality gate is the critical safety mechanism: a model that achieved 87%
// pick success rate in simulation will be blocked from reaching the fleet by
// the policy, regardless of how recently it was trained.
//
// GATING IS TARGET-KIND-INDEPENDENT (normative). A ModelPolicy whose quality gate
// rejects a candidate version of spec.modelName blocks the auto-deployment of that
// version to EVERY target kind, robots and EdgeNodes alike; a policy that would
// block a robot rollout blocks an edge rollout of the same artifact, and a policy
// that would block an edge rollout blocks the robot rollout. The gate is evaluated
// against the artifact's reported metrics, which say whether the model is fit to
// run at all — nothing in minPickSuccessRate, maxFailureRate, minEvalEpisodes or
// maxSimToRealGap becomes more permissive because the inference moved off the robot
// and onto the box beside it. spec.targetKind chooses WHERE a passing model is
// deployed; it never changes WHETHER it passes.
//
// Two consequences worth stating, because both are the kind of thing a reader
// otherwise assumes the other way:
//
//   - The suspension in §9.1.10.4 is likewise kind-independent. Once
//     consecutiveRejections reaches consecutiveRejectionLimit the policy stops
//     evaluating, and no target kind is exempt from the halt.
//   - Two policies over the same modelName with different targetKinds are two
//     independent gates, not one gate with two outputs. A version that one admits
//     and the other rejects deploys only where it was admitted — which is a real
//     configuration (an edge box may be held to a stricter latency bar than a
//     robot), and is legible only because each policy reports its own decision.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mp
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".spec.modelName"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetKind"
// +kubebuilder:printcolumn:name="Trigger",type="string",JSONPath=".spec.trigger.type"
// +kubebuilder:printcolumn:name="AutoDeploy",type="string",JSONPath=".spec.autoDeployOn"
// +kubebuilder:printcolumn:name="Deployed",type="integer",JSONPath=".status.deploymentCount"
// +kubebuilder:printcolumn:name="Rejected",type="integer",JSONPath=".status.rejectionCount"
// +kubebuilder:printcolumn:name="LastDecision",type="string",JSONPath=".status.lastDecision"
type ModelPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelPolicySpec   `json:"spec,omitempty"`
	Status            ModelPolicyStatus `json:"status,omitempty"`
}

// ModelPolicyList contains a list of ModelPolicy resources.
// +kubebuilder:object:root=true
type ModelPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelPolicy{}, &ModelPolicyList{})
}
