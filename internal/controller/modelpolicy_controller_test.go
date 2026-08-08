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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func f64(v float64) *float64 { return &v }

func b(v bool) *bool { return &v }

// testChecksum is a well-formed sha256 digest for fixtures: ADR-0020 makes a valid
// modelChecksum a precondition for a deployable model.
var testChecksum = "sha256:" + strings.Repeat("a", 64)

func policyWithGate(autoDeploy fleetv1.AutoDeployCondition, gate *fleetv1.QualityGate, trigger modelTriggerPayload) *fleetv1.ModelPolicy {
	raw, _ := json.Marshal(trigger)
	return &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "item-recognition-policy", Namespace: rolloutNS,
			Annotations: map[string]string{triggerAnnotation: string(raw)},
		},
		Spec: fleetv1.ModelPolicySpec{
			ModelName:           "item-recognition",
			TargetRobotSelector: metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			QualityGate:         gate,
			AutoDeployOn:        autoDeploy,
			RolloutTemplate: &fleetv1.RolloutTemplateSpec{
				Strategy:       fleetv1.RolloutStrategy{RollingUpdate: &fleetv1.RollingUpdateStrategy{MaxUnavailable: "1"}},
				RollbackPolicy: fleetv1.ModelRollbackManual,
			},
		},
	}
}

func newPolicyReconciler(t *testing.T, objs ...client.Object) (*ModelPolicyReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.ModelPolicy{}).Build()
	return &ModelPolicyReconciler{Client: c, Scheme: scheme}, c
}

func reconcilePolicy(t *testing.T, r *ModelPolicyReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "item-recognition-policy", Namespace: rolloutNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getPolicy(t *testing.T, c client.Client) *fleetv1.ModelPolicy {
	t.Helper()
	p := &fleetv1.ModelPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "item-recognition-policy", Namespace: rolloutNS}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	return p
}

func passingMetrics() map[string]float64 {
	return map[string]float64{
		"pick_success_rate": 0.961, "failure_rate": 0.018, "eval_episodes": 800,
		"sim_pick_success_rate": 0.979, "real_pick_success_rate": 0.961,
	}
}

func strictGate() *fleetv1.QualityGate {
	return &fleetv1.QualityGate{
		MinPickSuccessRate: f64(0.95), MaxFailureRate: f64(0.03),
		MinEvalEpisodes: p32(500), MaxSimToRealGap: f64(0.08),
	}
}

// A passing gate auto-creates an owner-referenced ModelRollout from the template.
func TestModelPolicy_GatePassCreatesRollout(t *testing.T) {
	trig := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://reg/item-recognition:4.1.0", ModelChecksum: testChecksum, Metrics: passingMetrics()}
	r, c := newPolicyReconciler(t, policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), trig))

	reconcilePolicy(t, r)

	ro := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "item-recognition-policy-4-1-0", Namespace: rolloutNS}, ro); err != nil {
		t.Fatalf("expected ModelRollout created: %v", err)
	}
	if ro.Spec.ModelName != "item-recognition" || ro.Spec.NewVersion != "4.1.0" || ro.Spec.ModelURI != trig.ModelURI {
		t.Fatalf("rollout spec = %+v", ro.Spec)
	}
	if ro.Spec.ModelChecksum != testChecksum {
		t.Errorf("modelChecksum not carried onto the rollout: %q", ro.Spec.ModelChecksum)
	}
	if ro.Spec.Strategy.RollingUpdateOrDefault().MaxUnavailable != "1" {
		t.Errorf("rolloutTemplate not applied: %+v", ro.Spec.Strategy)
	}
	if len(ro.OwnerReferences) != 1 || ro.OwnerReferences[0].Name != "item-recognition-policy" {
		t.Errorf("missing owner reference: %+v", ro.OwnerReferences)
	}

	p := getPolicy(t, c)
	if p.Status.LastDecision != fleetv1.ModelPolicyDecisionDeploy || p.Status.DeploymentCount != 1 {
		t.Errorf("status = %+v", p.Status)
	}
	if p.Status.ActiveRollout != "item-recognition-policy-4-1-0" {
		t.Errorf("activeRollout = %q", p.Status.ActiveRollout)
	}
	if _, still := p.Annotations[triggerAnnotation]; still {
		t.Error("trigger annotation not cleared after evaluation")
	}
	if len(p.Status.History) != 1 || p.Status.History[0].CreatedRollout == "" {
		t.Errorf("history = %+v", p.Status.History)
	}
}

// A failing gate rejects: no rollout, rejectionCount incremented, failedRules set.
func TestModelPolicy_GateFailRejects(t *testing.T) {
	m := passingMetrics()
	m["pick_success_rate"] = 0.87 // below 0.95
	trig := modelTriggerPayload{ModelVersion: "4.0.9", ModelURI: "oci://reg/x:4.0.9", ModelChecksum: testChecksum, Metrics: m}
	r, c := newPolicyReconciler(t, policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), trig))

	reconcilePolicy(t, r)

	roList := &fleetv1.ModelRolloutList{}
	_ = c.List(context.Background(), roList, client.InNamespace(rolloutNS))
	if len(roList.Items) != 0 {
		t.Fatalf("rejected model must not create a rollout, got %d", len(roList.Items))
	}
	p := getPolicy(t, c)
	if p.Status.LastDecision != fleetv1.ModelPolicyDecisionReject || p.Status.RejectionCount != 1 {
		t.Errorf("status = %+v", p.Status)
	}
	if len(p.Status.History) != 1 || len(p.Status.History[0].FailedRules) == 0 {
		t.Errorf("expected failedRules recorded: %+v", p.Status.History)
	}
}

// Absent real-hardware metric fails the sim-to-real gate (never a silent pass).
func TestModelPolicy_AbsentRealMetricFailsSimToReal(t *testing.T) {
	m := passingMetrics()
	delete(m, "real_pick_success_rate")
	fails, absent := evaluateQualityGate(strictGate(), m)
	if len(fails) != 1 || !strings.Contains(fails[0], "maxSimToRealGap") {
		t.Fatalf("expected a single maxSimToRealGap failure, got %v", fails)
	}
	if len(absent) != 1 || absent[0] != "real_pick_success_rate" {
		t.Fatalf("expected real_pick_success_rate reported absent, got %v", absent)
	}
}

// requireRealEval fails a sim-only model closed even with no sim-to-real gap set;
// the explicit opt-out (false) lets a sim-only model pass.
func TestModelPolicy_RequireRealEval(t *testing.T) {
	m := map[string]float64{"pick_success_rate": 0.98}
	gate := &fleetv1.QualityGate{MinPickSuccessRate: f64(0.95), RequireRealEval: b(true)}
	if fails, absent := evaluateQualityGate(gate, m); len(fails) != 1 || len(absent) != 1 {
		t.Fatalf("requireRealEval should fail a sim-only model: fails=%v absent=%v", fails, absent)
	}
	gate.RequireRealEval = b(false)
	if fails, _ := evaluateQualityGate(gate, m); len(fails) != 0 {
		t.Fatalf("requireRealEval=false should permit a sim-only model, got %v", fails)
	}
}

// A missing/malformed modelChecksum rejects the model even when the gate passes
// (ADR-0020): an unverifiable artifact is never deployed.
func TestModelPolicy_MissingChecksumRejects(t *testing.T) {
	trig := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://reg/x:4.1.0", Metrics: passingMetrics()} // no checksum
	r, c := newPolicyReconciler(t, policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), trig))

	reconcilePolicy(t, r)

	roList := &fleetv1.ModelRolloutList{}
	_ = c.List(context.Background(), roList, client.InNamespace(rolloutNS))
	if len(roList.Items) != 0 {
		t.Fatalf("a model with no checksum must not deploy, got %d rollouts", len(roList.Items))
	}
	if p := getPolicy(t, c); p.Status.LastDecision != fleetv1.ModelPolicyDecisionReject {
		t.Errorf("decision = %q, want Reject", p.Status.LastDecision)
	}
}

// AutoDeployManual evaluates and records a pass but does NOT create a rollout.
func TestModelPolicy_ManualDoesNotAutoCreate(t *testing.T) {
	trig := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://reg/x:4.1.0", ModelChecksum: testChecksum, Metrics: passingMetrics()}
	r, c := newPolicyReconciler(t, policyWithGate(fleetv1.AutoDeployManual, strictGate(), trig))

	reconcilePolicy(t, r)

	roList := &fleetv1.ModelRolloutList{}
	_ = c.List(context.Background(), roList, client.InNamespace(rolloutNS))
	if len(roList.Items) != 0 {
		t.Fatalf("AutoDeployManual must not auto-create a rollout, got %d", len(roList.Items))
	}
	if p := getPolicy(t, c); p.Status.LastDecision != fleetv1.ModelPolicyDecisionDeploy || p.Status.ActiveRollout != "" {
		t.Errorf("status = %+v (want Deploy decision, no activeRollout)", p.Status)
	}
}

// AutoDeployNewVersion deploys even when the gate would fail (gate bypassed).
func TestModelPolicy_NewVersionBypassesGate(t *testing.T) {
	m := passingMetrics()
	m["pick_success_rate"] = 0.10 // would fail the gate
	trig := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://reg/x:4.1.0", ModelChecksum: testChecksum, Metrics: m}
	r, c := newPolicyReconciler(t, policyWithGate(fleetv1.AutoDeployNewVersion, strictGate(), trig))

	reconcilePolicy(t, r)

	ro := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "item-recognition-policy-4-1-0", Namespace: rolloutNS}, ro); err != nil {
		t.Fatalf("NewVersion should deploy despite a failing gate: %v", err)
	}
}

func TestEvalMetricOperator(t *testing.T) {
	cases := []struct {
		op       fleetv1.CustomMetricOperator
		r, thr   float64
		expected bool
	}{
		{fleetv1.MetricOpGreaterThan, 0.9, 0.8, true},
		{fleetv1.MetricOpLessThan, 0.9, 0.8, false},
		{fleetv1.MetricOpGreaterThanOrEqual, 0.8, 0.8, true},
		{fleetv1.MetricOpLessThanOrEqual, 0.8, 0.8, true},
		{fleetv1.MetricOpEqual, 0.8, 0.8, true},
		{"Bogus", 1, 0, false}, // unknown fails closed
	}
	for _, tc := range cases {
		if got := evalMetricOperator(tc.r, tc.op, tc.thr); got != tc.expected {
			t.Errorf("%s(%g,%g) = %v, want %v", tc.op, tc.r, tc.thr, got, tc.expected)
		}
	}
}
