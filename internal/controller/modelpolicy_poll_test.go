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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/registry"
)

// fakeRegistry is a scripted RegistryClient.
type fakeRegistry struct {
	tags           []string
	manifestDigest string
	configDigest   string
	labels         map[string]string
	listErr        error
	// created maps a tag to its image BUILD time, for the versionless tie-break. Keyed by tag
	// because Descriptor returns the same configDigest for every tag in this fake.
	created    map[string]time.Time
	createdFor string // the tag Descriptor was last asked about
}

func (f *fakeRegistry) ListTags(_ context.Context, _, _ string, _ *registry.Credential) ([]string, error) {
	return f.tags, f.listErr
}
func (f *fakeRegistry) Descriptor(_ context.Context, _, _, ref string, _ *registry.Credential) (string, string, error) {
	f.createdFor = ref
	return f.manifestDigest, f.configDigest, nil
}
func (f *fakeRegistry) ConfigLabels(_ context.Context, _, _, _ string, _ *registry.Credential) (map[string]string, error) {
	return f.labels, nil
}
func (f *fakeRegistry) ConfigCreated(_ context.Context, _, _, _ string, _ *registry.Credential) (time.Time, error) {
	return f.created[f.createdFor], nil
}

func registryWatchPolicy() *fleetv1.ModelPolicy {
	return &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "item-recognition-policy", Namespace: rolloutNS},
		Spec: fleetv1.ModelPolicySpec{
			ModelName:           "item-recognition",
			TargetRobotSelector: metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			QualityGate:         strictGate(),
			AutoDeployOn:        fleetv1.AutoDeployQualityGatePass,
			Trigger: fleetv1.ModelPolicyTrigger{
				Type:          fleetv1.ModelPolicyTriggerRegistryWatch,
				RegistryWatch: &fleetv1.RegistryWatchConfig{Registry: "reg.example:5000", MetricsLabel: "swarmada.metrics"},
			},
			RolloutTemplate: &fleetv1.RolloutTemplateSpec{
				Strategy:       fleetv1.RolloutStrategy{RollingUpdate: &fleetv1.RollingUpdateStrategy{MaxUnavailable: "1"}},
				RollbackPolicy: fleetv1.ModelRollbackManual,
			},
		},
	}
}

func metricsLabelJSON(t *testing.T, m map[string]float64) string {
	t.Helper()
	raw, _ := json.Marshal(m)
	return string(raw)
}

// A new highest tag with a valid metrics label writes the model-trigger annotation
// (the reconciler's single evaluation path) plus the last-tag marker.
func TestRegistryWatch_NewTagWritesTrigger(t *testing.T) {
	reg := &fakeRegistry{
		tags:           []string{"4.0.0", "4.1.0", "3.9.0"},
		manifestDigest: "sha256:manifestABC",
		configDigest:   "sha256:cfg",
		labels:         map[string]string{"swarmada.metrics": metricsLabelJSON(t, passingMetrics())},
	}
	r, c := newPolicyReconciler(t, registryWatchPolicy())
	r.Registry = reg

	reconcilePolicy(t, r) // poll pass: converts the new tag into a trigger

	p := getPolicy(t, c)
	if p.Annotations[registryLastTagAnnotation] != "4.1.0" {
		t.Fatalf("last-tag = %q, want 4.1.0", p.Annotations[registryLastTagAnnotation])
	}
	raw, ok := p.Annotations[triggerAnnotation]
	if !ok {
		t.Fatal("no model-trigger annotation written by the poller")
	}
	var payload modelTriggerPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ModelVersion != "4.1.0" || payload.ModelChecksum != "sha256:manifestABC" ||
		payload.ModelURI != "reg.example:5000/item-recognition:4.1.0" || payload.Metrics["pick_success_rate"] != 0.961 {
		t.Fatalf("trigger payload = %+v", payload)
	}
}

// A full poll→evaluate cycle auto-creates the ModelRollout for the new version.
func TestRegistryWatch_PollThenEvaluateCreatesRollout(t *testing.T) {
	reg := &fakeRegistry{
		tags:           []string{"4.1.0"},
		manifestDigest: testChecksum, // a valid sha256: digest so the evaluate path deploys (ADR-0020)
		configDigest:   "sha256:c",
		labels:         map[string]string{"swarmada.metrics": metricsLabelJSON(t, passingMetrics())},
	}
	r, c := newPolicyReconciler(t, registryWatchPolicy())
	r.Registry = reg

	reconcilePolicy(t, r) // poll writes the trigger
	reconcilePolicy(t, r) // evaluate creates the rollout

	ro := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "item-recognition-policy-4-1-0", Namespace: rolloutNS}, ro); err != nil {
		t.Fatalf("expected ModelRollout from the RegistryWatch trigger: %v", err)
	}
	if ro.Spec.NewVersion != "4.1.0" || ro.Spec.ModelURI != "reg.example:5000/item-recognition:4.1.0" {
		t.Fatalf("rollout spec = %+v", ro.Spec)
	}
}

// No tag strictly newer than the last-processed one → no new trigger.
func TestRegistryWatch_NoNewerTagNoTrigger(t *testing.T) {
	reg := &fakeRegistry{tags: []string{"4.1.0", "4.0.0"}}
	p := registryWatchPolicy()
	p.Annotations = map[string]string{registryLastTagAnnotation: "4.1.0"} // already processed the highest
	r, c := newPolicyReconciler(t, p)
	r.Registry = reg

	reconcilePolicy(t, r)

	if _, ok := getPolicy(t, c).Annotations[triggerAnnotation]; ok {
		t.Fatal("wrote a trigger for a non-newer tag")
	}
}

// A new tag whose metrics label is missing advances the last-tag marker (so it is
// not re-polled forever) but does NOT trigger a deployment.
func TestRegistryWatch_MissingMetricsLabelSkips(t *testing.T) {
	reg := &fakeRegistry{
		tags:           []string{"4.1.0"},
		manifestDigest: "sha256:m",
		configDigest:   "sha256:c",
		labels:         map[string]string{"other.label": "{}"}, // no swarmada.metrics
	}
	r, c := newPolicyReconciler(t, registryWatchPolicy())
	r.Registry = reg

	reconcilePolicy(t, r)

	p := getPolicy(t, c)
	if p.Annotations[registryLastTagAnnotation] != "4.1.0" {
		t.Errorf("last-tag not advanced past a metrics-less tag: %q", p.Annotations[registryLastTagAnnotation])
	}
	if _, ok := p.Annotations[triggerAnnotation]; ok {
		t.Fatal("triggered despite a missing metrics label")
	}
}
