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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// An EdgeNode-targeted ModelRollout is specified but not implemented at v0.4. The
// hazard it guards is specific: spec.targetSelector is kind-neutral, so a rollout
// written for edge-node labels would, if the controller fell through, be resolved
// against ROBOTS and could push an edge model onto any robot wearing those labels.
// The robot here is labelled to match on purpose — it is the robot that would be
// updated if the guard were removed.
func TestEdgeNodeTargetKindUpdatesNoRobot(t *testing.T) {
	robot := targetRobot("amr-001", fleetv1.RobotPhaseIdle, 90)
	rollout := pickerRollout()
	rollout.Spec.TargetKind = fleetv1.RolloutTargetEdgeNode

	r, c := newRolloutReconciler(t, robot, rollout)
	reconcileRollout(t, r)

	var got fleetv1.Robot
	if err := c.Get(t.Context(), types.NamespacedName{Name: "amr-001", Namespace: rolloutNS}, &got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if len(got.Status.InstalledModels) != 0 {
		t.Fatalf("EdgeNode-targeted rollout touched a robot's installedModels: %+v", got.Status.InstalledModels)
	}

	var ro fleetv1.ModelRollout
	if err := c.Get(t.Context(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, &ro); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if ro.Status.RobotsTotal != 0 || ro.Status.RobotsUpdated != 0 {
		t.Fatalf("EdgeNode-targeted rollout reported robot progress: total=%d updated=%d",
			ro.Status.RobotsTotal, ro.Status.RobotsUpdated)
	}
}

// The default is Robot, so a rollout that omits targetKind behaves exactly as it did
// before the field existed. This is the backward-compatibility claim, asserted rather
// than assumed.
func TestUnsetTargetKindStillTargetsRobots(t *testing.T) {
	robot := targetRobot("amr-002", fleetv1.RobotPhaseIdle, 90)
	rollout := pickerRollout() // TargetKind deliberately left empty

	r, c := newRolloutReconciler(t, robot, rollout)
	reconcileRollout(t, r)

	var ro fleetv1.ModelRollout
	if err := c.Get(t.Context(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, &ro); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if ro.Status.RobotsTotal != 1 {
		t.Fatalf("unset targetKind did not select the robot: robotsTotal=%d", ro.Status.RobotsTotal)
	}
	if ro.Status.NodesTotal != 0 {
		t.Fatalf("robot rollout populated the EdgeNode status family: nodesTotal=%d", ro.Status.NodesTotal)
	}
}

// createRollout carries the policy's target kind and the selector for that kind onto
// the ModelRollout it creates; it never silently substitutes the robot selector.
func TestPolicyEdgeTargetCarriesNodeSelector(t *testing.T) {
	policy := &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cascade", Namespace: rolloutNS},
		Spec: fleetv1.ModelPolicySpec{
			ModelName:          "defect-cascade",
			TargetKind:         fleetv1.RolloutTargetEdgeNode,
			TargetNodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"zone": "receiving"}},
		},
	}

	r, c := newPolicyReconciler(t, policy)

	name, err := r.createRollout(t.Context(), policy, modelTriggerPayload{
		ModelVersion:  "1.1.0",
		ModelURI:      "oci://registry/models/defect-cascade:1.1.0",
		ModelChecksum: testChecksum,
	})
	if err != nil {
		t.Fatalf("createRollout: %v", err)
	}

	var ro fleetv1.ModelRollout
	if err := c.Get(t.Context(), types.NamespacedName{Name: name, Namespace: rolloutNS}, &ro); err != nil {
		t.Fatalf("get created rollout: %v", err)
	}
	if ro.Spec.TargetKind != fleetv1.RolloutTargetEdgeNode {
		t.Fatalf("targetKind not carried through: %q", ro.Spec.TargetKind)
	}
	if got := ro.Spec.TargetSelector.MatchLabels["zone"]; got != "receiving" {
		t.Fatalf("node selector not carried through: %+v", ro.Spec.TargetSelector)
	}
}

// A policy whose selector for its target kind is missing must fail closed. An empty
// LabelSelector matches EVERY object of the kind, so defaulting to one here would
// turn a misconfigured policy into a fleet-wide deployment.
func TestPolicyMissingSelectorForTargetKindFailsClosed(t *testing.T) {
	policy := &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cascade", Namespace: rolloutNS},
		Spec: fleetv1.ModelPolicySpec{
			ModelName:  "defect-cascade",
			TargetKind: fleetv1.RolloutTargetEdgeNode,
			// TargetNodeSelector omitted; CEL rejects this at admission, and the
			// controller must not paper over it if it arrives another way.
			TargetRobotSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
		},
	}

	r, _ := newPolicyReconciler(t, policy)

	if _, err := r.createRollout(t.Context(), policy, modelTriggerPayload{
		ModelVersion: "1.1.0",
		ModelURI:     "oci://registry/models/defect-cascade:1.1.0",
	}); err == nil {
		t.Fatal("createRollout accepted a policy with no selector for its target kind")
	}
}
