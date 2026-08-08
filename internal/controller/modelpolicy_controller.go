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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/registry"
	"github.com/swarmada/swarmada/internal/signing"
)

// registryLastTagAnnotation records the highest model tag the RegistryWatch poller
// has already processed, so a re-poll only triggers on a strictly-newer version.
const registryLastTagAnnotation = "swarmada.io/registry-last-tag"

// registryLastCreatedAnnotation records the build time of the last versionless tag acted on.
// A versionless tag ("latest") keeps its name across pushes, so the tag annotation cannot detect a
// move; the build timestamp can.
const registryLastCreatedAnnotation = "swarmada.io/registry-last-created"

// maxTimeOrderedTags bounds how many versionless tags a single poll will fetch config blobs for.
// Without it a repository with thousands of such tags turns one poll into thousands of round trips
// against a third-party registry.
const maxTimeOrderedTags = 20

// RegistryClient is the read-only OCI Distribution surface the RegistryWatch poller
// needs. [github.com/swarmada/swarmada/internal/registry.Client] satisfies it.
type RegistryClient interface {
	ListTags(ctx context.Context, reg, repo string, cred *registry.Credential) ([]string, error)
	Descriptor(ctx context.Context, reg, repo, ref string, cred *registry.Credential) (manifestDigest, configDigest string, err error)
	ConfigLabels(ctx context.Context, reg, repo, configDigest string, cred *registry.Credential) (map[string]string, error)
	// ConfigCreated is the image BUILD time, used only to break a tie version ordering cannot
	// resolve (see registry.HighestByTime). A zero time means "no usable signal".
	ConfigCreated(ctx context.Context, reg, repo, configDigest string, cred *registry.Credential) (time.Time, error)
}

// triggerAnnotation carries a training-completion event to the ModelPolicy
// controller (§9.1.9). The Manual trigger path (swarmctl evaluate policy) writes
// it; the Webhook and RegistryWatch front-ends (not yet built) would write the
// same annotation, so the reconciler is the single evaluation path. Its value is
// a JSON modelTriggerPayload; the controller clears it once consumed.
const triggerAnnotation = "swarmada.io/model-trigger"

// resetAnnotation is the operator's suspension override (§9.1.9.4). `swarmctl modelpolicy reset`
// writes it after a SelfSubjectAccessReview on the policy-reset custom verb, so the RBAC gate and
// the audit trail apply — the CLI is the enforcement point, as it is for admit/reject and estop.
//
// An annotation rather than a direct status edit, mirroring the estop path: status stays
// control-plane-owned, and the operator's intent is a spec-side fact the controller reconciles.
const resetAnnotation = "swarmada.io/policy-reset"

// resetProcessedAnnotation records the reset value already applied, so re-reconciling the same
// request is a no-op while a NEW value (a second reset after a second suspension) fires again.
const resetProcessedAnnotation = "swarmada.io/policy-reset-processed"

// historyLimit caps status.history at the most recent N records (§9.1.9 schema).
const historyLimit = 20

// conditionFailedRepeatedly is set True after spec.consecutiveRejectionLimit
// consecutive gate rejections; while True, evaluation is suspended (§9.1.9.4).
const conditionFailedRepeatedly = "FailedRepeatedly"

// conditionMetricSchemaMismatch is set True on a rejection caused by metric keys
// the gate references being absent from the payload — a naming/schema mismatch,
// distinct from a genuine threshold breach (ADR-0021).
const conditionMetricSchemaMismatch = "MetricSchemaMismatch"

// modelTriggerPayload is the training-completion event (§9.1.9.3 webhook payload,
// reduced to the fields the control plane acts on).
type modelTriggerPayload struct {
	ModelVersion      string             `json:"modelVersion"`
	ModelURI          string             `json:"modelUri"`
	ModelChecksum     string             `json:"modelChecksum,omitempty"`
	ModelSignatureRef string             `json:"modelSignatureRef,omitempty"`
	Metrics           map[string]float64 `json:"metrics"`
}

// ModelPolicyReconciler is the policy-governed bridge between a training pipeline
// and ModelRollout (§9.1.9): on each trigger event it evaluates the reported
// metrics against the quality gate and, on pass, auto-creates a ModelRollout.
type ModelPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Registry is the OCI client backing the RegistryWatch trigger. Nil defaults to
	// the production client; tests inject a fake.
	Registry RegistryClient
	// Audit records MODEL_ROLLOUT_CREATED into the tamper-evident §9.5.4 chain when
	// this controller auto-creates a ModelRollout. Nil disables audit recording.
	Audit audit.Recorder
}

// +kubebuilder:rbac:groups=swarmada.io,resources=modelpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=modelpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=modelrollouts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile evaluates one ModelPolicy trigger event.
func (r *ModelPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("modelpolicy", req.NamespacedName)

	policy := &fleetv1.ModelPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Housekeeping: release activeRollout once its ModelRollout is terminal/gone,
	// so a queued deployment can proceed.
	if policy.Status.ActiveRollout != "" && r.rolloutTerminal(ctx, req.Namespace, policy.Status.ActiveRollout) {
		base := policy.DeepCopy()
		policy.Status.ActiveRollout = ""
		if err := r.Status().Patch(ctx, policy, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("clearing activeRollout: %w", err)
		}
	}

	// Operator reset (§9.1.9.4): clear a FailedRepeatedly suspension. Handled BEFORE the suspension
	// guard below, which would otherwise return early and leave the policy suspended forever — the
	// reset is the one thing that must still be processed while suspended.
	if done, err := r.applyPolicyReset(ctx, policy); err != nil || done {
		return ctrl.Result{}, err
	}

	// Suspension (§9.1.9.4): after consecutiveRejectionLimit consecutive gate
	// failures the policy is suspended — no polling, no evaluation; a pending
	// trigger is silently dropped until an operator resets the policy.
	if apimeta.IsStatusConditionTrue(policy.Status.Conditions, conditionFailedRepeatedly) {
		if _, pending := policy.Annotations[triggerAnnotation]; pending {
			return ctrl.Result{}, r.clearTrigger(ctx, policy)
		}
		return ctrl.Result{}, nil
	}

	// RegistryWatch: poll the OCI registry for a new model version and convert it
	// into a trigger. Polling is skipped while a trigger is already pending (it is
	// evaluated below) or a rollout is active (the poll would only queue anyway).
	if policy.Spec.Trigger.Type == fleetv1.ModelPolicyTriggerRegistryWatch {
		if _, pending := policy.Annotations[triggerAnnotation]; !pending {
			triggered, err := r.pollRegistry(ctx, policy)
			if err != nil {
				logger.V(1).Info("registry poll failed; will retry", "reason", err.Error())
			}
			if triggered {
				return ctrl.Result{RequeueAfter: time.Second}, nil // evaluate on the next pass
			}
			return ctrl.Result{RequeueAfter: registryPollInterval(policy)}, nil
		}
	}

	raw, ok := policy.Annotations[triggerAnnotation]
	if !ok {
		return ctrl.Result{}, nil // no pending trigger
	}

	var payload modelTriggerPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.ModelVersion == "" {
		r.event(policy, corev1.EventTypeWarning, "TriggerInvalid",
			fmt.Sprintf("discarding malformed model-trigger annotation: %v", err))
		return ctrl.Result{}, r.clearTrigger(ctx, policy)
	}

	// maxConcurrentRollouts: while a prior auto-rollout is still running, queue —
	// keep the annotation and retry after housekeeping frees the slot.
	if policy.Status.ActiveRollout != "" {
		logger.V(1).Info("deployment queued behind active rollout", "active", policy.Status.ActiveRollout)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Evaluate. NewVersion bypasses the gate; otherwise all rules must pass.
	var failed, absent []string
	if policy.Spec.AutoDeployOn != fleetv1.AutoDeployNewVersion {
		failed, absent = evaluateQualityGate(policy.Spec.QualityGate, payload.Metrics)
	}
	// ADR-0020: a model with no well-formed checksum is never deployable — the
	// adapter would have nothing to verify the downloaded bytes against (§9.2.8).
	// This holds even on the NewVersion path: an unverified artifact is never shipped.
	if !signing.ValidChecksum(payload.ModelChecksum) {
		failed = append(failed, fmt.Sprintf(
			"modelChecksum missing or malformed (%q); a checksummed, signable artifact is required", payload.ModelChecksum))
	}
	decision := fleetv1.ModelPolicyDecisionDeploy
	if len(failed) > 0 {
		decision = fleetv1.ModelPolicyDecisionReject
	}

	now := metav1.Now()
	rec := fleetv1.PolicyEvaluationRecord{
		Version:         payload.ModelVersion,
		ModelURI:        payload.ModelURI,
		Decision:        decision,
		ReportedMetrics: payload.Metrics,
		FailedRules:     failed,
		EvaluatedAt:     now,
	}

	base := policy.DeepCopy()
	switch {
	case decision == fleetv1.ModelPolicyDecisionReject:
		policy.Status.RejectionCount++
		policy.Status.ConsecutiveRejections++
		policy.Status.LastDecisionReason = strings.Join(failed, "; ")
		r.event(policy, corev1.EventTypeWarning, "QualityGateFail",
			fmt.Sprintf("model %s rejected: %s", payload.ModelVersion, policy.Status.LastDecisionReason))
		// §9.1.9.4: suspend after too many consecutive rejections (0 = never).
		if limit := policy.Spec.ConsecutiveRejectionLimit; limit > 0 && policy.Status.ConsecutiveRejections >= limit {
			apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    conditionFailedRepeatedly,
				Status:  metav1.ConditionTrue,
				Reason:  "ConsecutiveRejections",
				Message: fmt.Sprintf("%d consecutive rejections; evaluation suspended until the policy is reset", policy.Status.ConsecutiveRejections),
			})
			r.event(policy, corev1.EventTypeWarning, "PolicySuspended",
				fmt.Sprintf("suspended after %d consecutive quality-gate rejections", policy.Status.ConsecutiveRejections))
		}

	case policy.Spec.AutoDeployOn == fleetv1.AutoDeployManual:
		// Gate passed but deployment needs an explicit approval (§9.1.9 AutoDeployManual).
		policy.Status.LastDecisionReason = "quality gate passed; awaiting manual approval"
		r.event(policy, corev1.EventTypeNormal, "QualityGatePass",
			fmt.Sprintf("model %s passed the gate; manual approval required", payload.ModelVersion))

	default: // Deploy + (QualityGatePass | NewVersion): auto-create the ModelRollout
		name, err := r.createRollout(ctx, policy, payload)
		if err != nil {
			return ctrl.Result{}, err
		}
		rec.CreatedRollout = name
		policy.Status.ActiveRollout = name
		policy.Status.DeploymentCount++
		policy.Status.LastDecisionReason = fmt.Sprintf("quality gate passed; ModelRollout %s created", name)
		r.event(policy, corev1.EventTypeNormal, "ModelRolloutCreated",
			fmt.Sprintf("ModelRollout %s created for %s@%s", name, policy.Spec.ModelName, payload.ModelVersion))
	}

	// ADR-0021: surface a metric naming/schema mismatch (gate references keys the
	// payload does not carry) distinctly from a genuine threshold breach, so an
	// operator can tell "the model underperformed" from "your metric names don't line up".
	if decision == fleetv1.ModelPolicyDecisionReject && len(absent) > 0 {
		apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    conditionMetricSchemaMismatch,
			Status:  metav1.ConditionTrue,
			Reason:  "MetricsAbsent",
			Message: fmt.Sprintf("gate references metric(s) absent from the payload: %s", strings.Join(absent, ", ")),
		})
	} else {
		apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    conditionMetricSchemaMismatch,
			Status:  metav1.ConditionFalse,
			Reason:  "MetricsResolved",
			Message: "all gate-referenced metrics were present",
		})
	}

	// Any Deploy decision (gate pass, incl. Manual) clears the consecutive-rejection
	// streak (§9.1.9.4). A Deploy is only reachable while not suspended.
	if decision == fleetv1.ModelPolicyDecisionDeploy {
		policy.Status.ConsecutiveRejections = 0
	}

	policy.Status.LastDecision = decision
	policy.Status.LastTriggerAt = &now
	policy.Status.History = appendHistory(policy.Status.History, rec)
	if err := r.Status().Patch(ctx, policy, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching policy status: %w", err)
	}

	// Consume the trigger so it evaluates exactly once.
	return ctrl.Result{}, r.clearTrigger(ctx, policy)
}

// createRollout auto-creates a ModelRollout from the policy's rolloutTemplate and
// the trigger event. Owner-referenced for GC; idempotent on an existing name.
func (r *ModelPolicyReconciler) createRollout(ctx context.Context, policy *fleetv1.ModelPolicy, payload modelTriggerPayload) (string, error) {
	name := rolloutName(policy.Name, payload.ModelVersion)
	ro := &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: policy.Namespace},
		Spec: fleetv1.ModelRolloutSpec{
			TargetSelector:    policy.Spec.TargetRobotSelector,
			ModelName:         policy.Spec.ModelName,
			NewVersion:        payload.ModelVersion,
			ModelURI:          payload.ModelURI,
			ModelChecksum:     payload.ModelChecksum,
			ModelSignatureRef: payload.ModelSignatureRef,
		},
	}
	if t := policy.Spec.RolloutTemplate; t != nil {
		ro.Spec.Strategy = t.Strategy
		ro.Spec.SafetyConstraints = t.SafetyConstraints
		ro.Spec.RollbackPolicy = t.RollbackPolicy
	}
	if err := ctrl.SetControllerReference(policy, ro, r.Scheme); err != nil {
		return "", fmt.Errorf("setting owner reference: %w", err)
	}
	if err := r.Create(ctx, ro); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return name, nil // already created for this version — idempotent
		}
		return "", fmt.Errorf("creating ModelRollout: %w", err)
	}
	r.recordRolloutCreated(ctx, policy, name, payload.ModelVersion)
	return name, nil
}

// recordRolloutCreated seals a MODEL_ROLLOUT_CREATED entry into the §9.5.4 audit
// chain. The actor is this controller (it IS the creator, via the ModelPolicy
// quality-gate path). Audit is best-effort: a record failure is logged, never
// blocking the rollout (the create already succeeded).
func (r *ModelPolicyReconciler) recordRolloutCreated(ctx context.Context, policy *fleetv1.ModelPolicy, rollout, version string) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventModelRolloutCreated,
		Namespace: policy.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "modelpolicy-controller"},
		Resource:  audit.Resource{Kind: "ModelRollout", Namespace: policy.Namespace, Name: rollout},
		Action:    "create",
		Outcome:   audit.OutcomeAllowed,
		Detail: map[string]string{
			"policy":  policy.Name,
			"model":   policy.Spec.ModelName,
			"version": version,
		},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording MODEL_ROLLOUT_CREATED audit entry",
			"policy", policy.Name, "rollout", rollout)
	}
}

func (r *ModelPolicyReconciler) rolloutTerminal(ctx context.Context, namespace, name string) bool {
	ro := &fleetv1.ModelRollout{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ro); err != nil {
		return apierrors.IsNotFound(err) // gone → slot free; transient error → keep waiting
	}
	return ro.Status.Phase == fleetv1.RolloutPhaseSucceeded || ro.Status.Phase == fleetv1.RolloutPhaseFailed
}

// applyPolicyReset clears a FailedRepeatedly suspension when the operator has requested it.
//
// Returns done=true when it handled the reset (the caller stops this reconcile; the status write
// re-queues). Idempotent: a request already applied writes nothing, so a re-reconcile of the same
// annotation value is free (RA-1). A policy that is NOT suspended still records the request as
// processed, so a stale annotation cannot silently clear a FUTURE suspension.
func (r *ModelPolicyReconciler) applyPolicyReset(ctx context.Context, policy *fleetv1.ModelPolicy) (bool, error) {
	req, requested := policy.Annotations[resetAnnotation]
	if !requested {
		return false, nil
	}
	if policy.Annotations[resetProcessedAnnotation] == req {
		return false, nil // already applied; nothing to do
	}

	base := policy.DeepCopy()
	suspended := apimeta.IsStatusConditionTrue(policy.Status.Conditions, conditionFailedRepeatedly)
	if suspended {
		apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    conditionFailedRepeatedly,
			Status:  metav1.ConditionFalse,
			Reason:  "OperatorReset",
			Message: "suspension cleared by an operator (policy-reset)",
		})
		// The counter is what re-suspends: leaving it at the limit would re-suspend on the very
		// next rejection, making the reset look like it did nothing.
		policy.Status.ConsecutiveRejections = 0
		if err := r.Status().Patch(ctx, policy, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("clearing policy suspension: %w", err)
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(policy, corev1.EventTypeNormal, "PolicyReset",
				"evaluation resumed by operator reset (was suspended after %d consecutive rejections)",
				base.Status.ConsecutiveRejections)
		}
	}

	// Mark the request processed either way, so it cannot fire again.
	annBase := policy.DeepCopy()
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	policy.Annotations[resetProcessedAnnotation] = req
	if err := r.Patch(ctx, policy, client.MergeFrom(annBase)); err != nil {
		return false, fmt.Errorf("recording policy-reset processed marker: %w", err)
	}
	return suspended, nil
}

func (r *ModelPolicyReconciler) clearTrigger(ctx context.Context, policy *fleetv1.ModelPolicy) error {
	base := policy.DeepCopy()
	delete(policy.Annotations, triggerAnnotation)
	if err := r.Patch(ctx, policy, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("clearing trigger annotation: %w", err)
	}
	return nil
}

func (r *ModelPolicyReconciler) event(obj client.Object, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, msg)
	}
}

// ── Pure quality-gate logic (§9.1.9.2) ──────────────────────────────────────────

// evaluateQualityGate returns the failed gate rules and the subset of referenced
// metric keys that were absent from the payload. Empty fails means the gate passes;
// a nil gate passes unconditionally. Evaluation is fail-closed (ADR-0021): an absent
// required metric is always a failure, never a silent pass, and the absent keys are
// reported so a naming/schema mismatch can be surfaced distinctly from a real breach.
func evaluateQualityGate(gate *fleetv1.QualityGate, metrics map[string]float64) (fails, absent []string) {
	if gate == nil {
		return nil, nil
	}

	if gate.MinPickSuccessRate != nil {
		v, ok := metrics["pick_success_rate"]
		if !ok {
			absent = append(absent, "pick_success_rate")
		}
		if !ok || v < *gate.MinPickSuccessRate {
			fails = append(fails, fmt.Sprintf("minPickSuccessRate failed: reported=%s < threshold=%g",
				reportedStr(metrics, "pick_success_rate"), *gate.MinPickSuccessRate))
		}
	}
	if gate.MaxFailureRate != nil {
		v, ok := metrics["failure_rate"]
		if !ok {
			absent = append(absent, "failure_rate")
		}
		if !ok || v > *gate.MaxFailureRate {
			fails = append(fails, fmt.Sprintf("maxFailureRate failed: reported=%s > threshold=%g",
				reportedStr(metrics, "failure_rate"), *gate.MaxFailureRate))
		}
	}
	if gate.MinEvalEpisodes != nil {
		v, ok := metrics["eval_episodes"]
		if !ok {
			absent = append(absent, "eval_episodes")
		}
		if !ok || int32(v) < *gate.MinEvalEpisodes {
			fails = append(fails, fmt.Sprintf("minEvalEpisodes failed: reported=%s < threshold=%d",
				reportedStr(metrics, "eval_episodes"), *gate.MinEvalEpisodes))
		}
	}

	// Real-hardware evaluation. requireReal defaults true (fail-closed); a
	// simulation/development namespace opts out with requireRealEval=false (ADR-0021).
	requireReal := gate.RequireRealEval == nil || *gate.RequireRealEval
	if gate.MaxSimToRealGap != nil {
		real, ok := metrics["real_pick_success_rate"]
		switch {
		case !ok && requireReal:
			absent = append(absent, "real_pick_success_rate")
			fails = append(fails, "maxSimToRealGap failed: real_pick_success_rate absent; real-hardware validation required")
		case ok:
			if gap := math.Abs(metrics["sim_pick_success_rate"] - real); gap > *gate.MaxSimToRealGap {
				fails = append(fails, fmt.Sprintf("maxSimToRealGap failed: gap=%.3f > threshold=%g", gap, *gate.MaxSimToRealGap))
			}
		}
	} else if gate.RequireRealEval != nil && *gate.RequireRealEval {
		if _, ok := metrics["real_pick_success_rate"]; !ok {
			absent = append(absent, "real_pick_success_rate")
			fails = append(fails, "requireRealEval failed: real_pick_success_rate absent; real-hardware validation required")
		}
	}

	for _, cm := range gate.CustomMetrics {
		v, ok := metrics[cm.Name]
		if !ok {
			absent = append(absent, cm.Name)
			fails = append(fails, fmt.Sprintf("customMetric %q failed: metric absent from payload", cm.Name))
			continue
		}
		if !evalMetricOperator(v, cm.Operator, cm.Threshold) {
			fails = append(fails, fmt.Sprintf("customMetric %q failed: reported=%g not %s %g",
				cm.Name, v, cm.Operator, cm.Threshold))
		}
	}
	return fails, absent
}

func evalMetricOperator(reported float64, op fleetv1.CustomMetricOperator, threshold float64) bool {
	switch op {
	case fleetv1.MetricOpGreaterThan:
		return reported > threshold
	case fleetv1.MetricOpLessThan:
		return reported < threshold
	case fleetv1.MetricOpGreaterThanOrEqual:
		return reported >= threshold
	case fleetv1.MetricOpLessThanOrEqual:
		return reported <= threshold
	case fleetv1.MetricOpEqual:
		return reported == threshold
	case fleetv1.MetricOpNotEqual:
		return reported != threshold
	default:
		return false // unknown operator fails closed
	}
}

func reportedStr(metrics map[string]float64, key string) string {
	if v, ok := metrics[key]; ok {
		return fmt.Sprintf("%g", v)
	}
	return "absent"
}

// appendHistory appends a record and keeps only the most recent historyLimit.
func appendHistory(history []fleetv1.PolicyEvaluationRecord, rec fleetv1.PolicyEvaluationRecord) []fleetv1.PolicyEvaluationRecord {
	history = append(history, rec)
	if len(history) > historyLimit {
		history = history[len(history)-historyLimit:]
	}
	return history
}

// rolloutName derives a deterministic, DNS-safe ModelRollout name for a policy +
// version, so re-evaluating the same version is idempotent.
func rolloutName(policy, version string) string {
	return policy + "-" + strings.NewReplacer(".", "-", "_", "-", "+", "-").Replace(version)
}

// newestByBuildTime picks the tag with the latest image build time, for the case where version
// ordering cannot separate the candidates. Returns ("", zero) when no tag carries a usable
// timestamp — the caller then does nothing rather than guessing.
//
// One config fetch per candidate. Bounded by maxTimeOrderedTags so a repository with thousands of
// versionless tags cannot turn one poll into thousands of registry round trips.
func (r *ModelPolicyReconciler) newestByBuildTime(
	ctx context.Context, rc RegistryClient, reg, repo string, tags []string, cred *registry.Credential,
) (string, time.Time, error) {
	candidates := make([]string, 0, len(tags))
	for _, t := range tags {
		if registry.Versionless(t) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) > maxTimeOrderedTags {
		candidates = candidates[:maxTimeOrderedTags]
	}

	times := make(map[string]time.Time, len(candidates))
	for _, t := range candidates {
		_, configDigest, err := rc.Descriptor(ctx, reg, repo, t, cred)
		if err != nil {
			continue // a tag we cannot resolve simply does not participate
		}
		at, err := rc.ConfigCreated(ctx, reg, repo, configDigest, cred)
		if err != nil {
			continue
		}
		if !at.IsZero() {
			times[t] = at
		}
	}
	best := registry.HighestByTime(candidates, times)
	return best, times[best], nil
}

func (r *ModelPolicyReconciler) registryClient() RegistryClient {
	if r.Registry != nil {
		return r.Registry
	}
	return registry.New()
}

// registryPollInterval is the configured poll cadence (default 60s, min 30s per
// the schema).
func registryPollInterval(policy *fleetv1.ModelPolicy) time.Duration {
	secs := int32(60)
	if cfg := policy.Spec.Trigger.RegistryWatch; cfg != nil && cfg.PollIntervalSeconds > 0 {
		secs = cfg.PollIntervalSeconds
	}
	return time.Duration(secs) * time.Second
}

// pollRegistry checks the model's OCI repository for a strictly-newer tag than the
// last one processed. On a new tag it reads the metrics label from the image
// config and writes the model-trigger annotation the reconciler evaluates (the
// single evaluation path). It always advances the last-tag marker on a
// successfully-inspected new tag, so a tag with a missing/malformed metrics label
// is skipped once rather than re-polled forever. Returns whether a trigger was
// written.
func (r *ModelPolicyReconciler) pollRegistry(ctx context.Context, policy *fleetv1.ModelPolicy) (bool, error) {
	cfg := policy.Spec.Trigger.RegistryWatch
	if cfg == nil || cfg.Registry == "" || policy.Spec.ModelName == "" {
		return false, nil
	}
	repo := policy.Spec.ModelName
	cred, err := r.modelRegistryCredential(ctx, policy.Namespace)
	if err != nil {
		return false, err
	}
	rc := r.registryClient()

	tags, err := rc.ListTags(ctx, cfg.Registry, repo, cred)
	if err != nil {
		return false, err
	}
	highest := registry.Highest(tags)
	if highest == "" {
		return false, nil
	}
	last := policy.Annotations[registryLastTagAnnotation]
	// Carried to the annotation write below. A local, NOT a reconciler field: concurrent reconciles
	// share the reconciler, so state parked on it would race.
	newCreated := ""

	// Version ordering is authoritative. It only fails to decide when the candidates carry no
	// version at all ("latest", "stable", "prod"), which all compare equal — a repository tagged
	// that way would never trigger, because nothing is ever "strictly newer" than anything else.
	// For that case ONLY, fall back to the image's build timestamp.
	//
	// Not the primary rule on purpose: `created` is build time, so a rebuild-and-repush of an older
	// release would out-rank the release that superseded it and roll robots BACKWARDS.
	if registry.Versionless(highest) {
		fresh, at, berr := r.newestByBuildTime(ctx, rc, cfg.Registry, repo, tags, cred)
		if berr != nil {
			return false, berr
		}
		if fresh == "" || at.IsZero() {
			return false, nil // no usable build time (e.g. epoch-pinned): cannot tell, so do nothing
		}
		highest = fresh
		// The tag STRING never changes for a versionless tag, so the stored tag cannot signal
		// newness; the build time is the signal, kept in its own annotation.
		if prev := policy.Annotations[registryLastCreatedAnnotation]; prev != "" {
			prevAt, perr := time.Parse(time.RFC3339, prev)
			if perr == nil && !at.After(prevAt) {
				return false, nil // same or older build: nothing new
			}
		}
		newCreated = at.UTC().Format(time.RFC3339)
	} else if highest == last || (last != "" && !registry.Newer(highest, last)) {
		return false, nil // nothing strictly newer
	}

	manifestDigest, configDigest, err := rc.Descriptor(ctx, cfg.Registry, repo, highest, cred)
	if err != nil {
		return false, err // transient fetch error: retry the same tag next interval
	}
	labels, err := rc.ConfigLabels(ctx, cfg.Registry, repo, configDigest, cred)
	if err != nil {
		return false, err
	}

	metricsLabel := cfg.MetricsLabel
	if metricsLabel == "" {
		metricsLabel = "swarmada.metrics"
	}
	metrics, mErr := parseMetricsLabel(labels[metricsLabel])

	base := policy.DeepCopy()
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	policy.Annotations[registryLastTagAnnotation] = highest // advance regardless
	if newCreated != "" {
		// Versionless tag: the build time is what "already seen" means, since the tag name is
		// constant across pushes.
		policy.Annotations[registryLastCreatedAnnotation] = newCreated
	}

	triggered := false
	if mErr == nil && len(metrics) > 0 {
		raw, err := json.Marshal(modelTriggerPayload{
			ModelVersion:  highest,
			ModelURI:      cfg.Registry + "/" + repo + ":" + highest,
			ModelChecksum: manifestDigest,
			Metrics:       metrics,
		})
		if err != nil {
			return false, err
		}
		policy.Annotations[triggerAnnotation] = string(raw)
		triggered = true
	}
	if err := r.Patch(ctx, policy, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("recording registry poll: %w", err)
	}
	if mErr != nil {
		r.event(policy, corev1.EventTypeWarning, "RegistryMetricsMissing",
			fmt.Sprintf("tag %s: no usable %q metrics label (%v); skipped", highest, metricsLabel, mErr))
	}
	return triggered, nil
}

// parseMetricsLabel decodes the OCI image label value (JSON object of metric →
// number) into the metrics map the quality gate evaluates.
func parseMetricsLabel(value string) (map[string]float64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("metrics label is empty")
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return nil, fmt.Errorf("metrics label is not a JSON number map: %w", err)
	}
	return m, nil
}

// modelRegistryCredential reads the conventional per-namespace registry
// credentials Secret (shared with FirmwareRollout). Absent → anonymous.
func (r *ModelPolicyReconciler) modelRegistryCredential(ctx context.Context, namespace string) (*registry.Credential, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: registryCredentialsSecret}, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading registry credentials: %w", err)
	}
	cred := &registry.Credential{
		BearerToken: string(secret.Data["token"]),
		Username:    string(secret.Data["username"]),
		Password:    string(secret.Data["password"]),
	}
	if cred.BearerToken == "" && cred.Username == "" && cred.Password == "" {
		return nil, nil
	}
	return cred, nil
}

// SetupWithManager registers the ModelPolicy controller.
func (r *ModelPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.ModelPolicy{}).
		Owns(&fleetv1.ModelRollout{}).
		Complete(r)
}
