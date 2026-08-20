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
	// image tags matching modelName. On new tag: pulls image labels for metrics.
	ModelPolicyTriggerRegistryWatch ModelPolicyTriggerType = "RegistryWatch"

	// ModelPolicyTriggerManual — no automatic trigger. Operator calls swarmctl evaluate policy <name>
	// to manually submit metrics and trigger evaluation.
	ModelPolicyTriggerManual ModelPolicyTriggerType = "Manual"
)

// ModelPolicyDecision is the outcome of a quality gate evaluation.
// +kubebuilder:validation:Enum=Deploy;Reject;Pending
type ModelPolicyDecision string

// ModelPolicyDecision values (§9.1.10): the quality gate's verdict on a candidate model. Deploy
// auto-creates a ModelRollout; Reject counts toward the consecutive-rejection suspension; Pending
// means the gate has not yet evaluated this trigger.
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
	// AutoDeployManual — quality gate is evaluated and logged but deployment requires
	// an explicit swarmctl policy approve command even if gate passes.
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
	// Example: LABEL swarmada.metrics='{"pickSuccessRate":0.94,"failureRate":0.018}'
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
type ModelPolicySpec struct {
	// ModelName is the logical model name this policy governs.
	// Must match InstalledModel.name on target robots.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`

	// TargetRobotSelector selects which robots will receive auto-deployments.
	// +kubebuilder:validation:Required
	TargetRobotSelector metav1.LabelSelector `json:"targetRobotSelector"`

	// Trigger configures how training completion events reach this policy.
	// +optional
	Trigger ModelPolicyTrigger `json:"trigger,omitempty"`

	// QualityGate defines the metrics thresholds a model must satisfy.
	// If empty, any model version triggers deployment (use with caution).
	// +optional
	QualityGate *QualityGate `json:"qualityGate,omitempty"`

	// AutoDeployOn defines when automatic deployment is triggered.
	// +kubebuilder:default=QualityGatePass
	AutoDeployOn AutoDeployCondition `json:"autoDeployOn,omitempty"`

	// RolloutTemplate is the ModelRollout spec applied when auto-deploying.
	// +optional
	RolloutTemplate *RolloutTemplateSpec `json:"rolloutTemplate,omitempty"`

	// MaxConcurrentRollouts prevents multiple auto-triggered rollouts from
	// running simultaneously. Default 1 (queue new deployments until current
	// completes).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MaxConcurrentRollouts int32 `json:"maxConcurrentRollouts,omitempty"`

	// ConsecutiveRejectionLimit is the number of consecutive quality-gate
	// rejections after which the controller sets a FailedRepeatedly condition and
	// SUSPENDS evaluation — further triggers are silently dropped until an operator
	// resets the policy (swarmctl reset policy). 0 disables suspension (unlimited
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

	// WebhookEndpoint is the auto-generated endpoint URL when Trigger.Type=Webhook.
	// Training systems POST to this URL when training completes.
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
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mp
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".spec.modelName"
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
