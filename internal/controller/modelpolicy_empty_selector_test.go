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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// AN EMPTY SELECTOR IS NOT "NOTHING", IT IS "EVERYTHING".
//
// An empty LabelSelector matches every object of the kind, so a ModelPolicy whose
// targetRobotSelector is present but empty would auto-deploy to every Robot in the
// namespace (RFC-0001 #edgenode-modelpolicy-gating-for-a-non-robot-target). A presence rule cannot tell `{}`
// from a real selector, so the API server must refuse the empty one outright, and the
// controller must refuse it for an object stored before the rule existed.

func emptySelectorPolicy(ns string, kind fleetv1.RolloutTargetKind, robotSel, nodeSel *metav1.LabelSelector) *fleetv1.ModelPolicy {
	return &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "empty-sel-", Namespace: ns},
		Spec: fleetv1.ModelPolicySpec{
			ModelName:           "item-recognition",
			TargetKind:          kind,
			TargetRobotSelector: robotSel,
			TargetNodeSelector:  nodeSel,
			AutoDeployOn:        fleetv1.AutoDeployQualityGatePass,
			QualityGate:         strictGate(),
			Trigger:             fleetv1.ModelPolicyTrigger{Type: fleetv1.ModelPolicyTriggerManual},
		},
	}
}

func TestModelPolicyAdmission_EmptySelectorIsRefused(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	ctx := context.Background()

	refused := []struct {
		name     string
		policy   *fleetv1.ModelPolicy
		selector string
	}{
		{"robot selector {}", emptySelectorPolicy(ns, fleetv1.RolloutTargetRobot,
			&metav1.LabelSelector{}, nil), "targetRobotSelector"},
		{"robot selector with empty matchLabels", emptySelectorPolicy(ns, fleetv1.RolloutTargetRobot,
			&metav1.LabelSelector{MatchLabels: map[string]string{}}, nil), "targetRobotSelector"},
		{"robot selector {} with targetKind defaulted", emptySelectorPolicy(ns, "",
			&metav1.LabelSelector{}, nil), "targetRobotSelector"},
		{"node selector {}", emptySelectorPolicy(ns, fleetv1.RolloutTargetEdgeNode,
			nil, &metav1.LabelSelector{}), "targetNodeSelector"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := envK8s.Create(ctx, tc.policy)
			if err == nil {
				t.Fatalf("an empty %s was admitted; it selects every object of the kind", tc.selector)
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("want an Invalid refusal from the schema, got %v", err)
			}
			msg := err.Error()
			for _, want := range []string{tc.selector, "matches every", "matchLabels or matchExpressions"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal must name the field, the consequence and the fix; missing %q in: %s", want, msg)
				}
			}
		})
	}

	admitted := []struct {
		name   string
		policy *fleetv1.ModelPolicy
	}{
		{"robot matchLabels", emptySelectorPolicy(ns, fleetv1.RolloutTargetRobot,
			&metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}}, nil)},
		{"robot matchExpressions", emptySelectorPolicy(ns, fleetv1.RolloutTargetRobot,
			&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "fleet", Operator: metav1.LabelSelectorOpExists}}}, nil)},
		{"node matchLabels", emptySelectorPolicy(ns, fleetv1.RolloutTargetEdgeNode,
			nil, &metav1.LabelSelector{MatchLabels: map[string]string{"site": "a"}})},
	}
	for _, tc := range admitted {
		t.Run(tc.name, func(t *testing.T) {
			if err := envK8s.Create(ctx, tc.policy); err != nil {
				t.Fatalf("a selector that names robots must be admitted: %v", err)
			}
		})
	}
}

// A policy stored with `{}` before the schema refused it is still served (CRD validation
// ratcheting keeps an unchanged field valid). The controller is the last line: it must
// not create a rollout from it.
func TestModelPolicy_StoredEmptySelectorCreatesNoRollout(t *testing.T) {
	trig := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://reg/item-recognition:4.1.0",
		ModelChecksum: testChecksum, Metrics: passingMetrics()}
	pol := policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), trig)
	pol.Spec.TargetRobotSelector = &metav1.LabelSelector{}
	r, c := newPolicyReconciler(t, pol)

	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: pol.Namespace}})

	var rollouts fleetv1.ModelRolloutList
	if err := c.List(context.Background(), &rollouts); err != nil {
		t.Fatal(err)
	}
	if len(rollouts.Items) != 0 {
		t.Fatalf("a stored empty selector created a rollout targeting every Robot: %+v",
			rollouts.Items[0].Spec.TargetSelector)
	}
}
