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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const rolloutNS = "warehouse-a"

func p32(v int32) *int32 { return &v }

// targetRobot builds a robot labelled for the rollout selector.
func targetRobot(name string, phase fleetv1.RobotPhase, battery int32) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rolloutNS, Labels: map[string]string{"fleet": "pickers"}},
		Spec: fleetv1.RobotSpec{
			Hardware: []fleetv1.HardwareComponent{{Name: "cam-front", Type: fleetv1.HardwareTypeCamera}},
		},
		Status: fleetv1.RobotStatus{
			Phase:          phase,
			BatteryPercent: p32(battery),
			Hardware:       []fleetv1.HardwareComponentStatus{{Name: "cam-front", Status: fleetv1.HardwareHealthy}},
		},
	}
}

func pickerRollout() *fleetv1.ModelRollout {
	return &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "roll", Namespace: rolloutNS},
		Spec: fleetv1.ModelRolloutSpec{
			TargetSelector:     metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			ModelName:          "item-recognition",
			NewVersion:         "3.2.1",
			ModelURI:           "oci://registry/models/item-recognition:3.2.1",
			GrantsCapabilities: []string{"item-pick.ai-guided", "inspect.defects"},
			Strategy:           fleetv1.RolloutStrategy{RollingUpdate: &fleetv1.RollingUpdateStrategy{MaxUnavailable: "1", PauseOnError: true}},
			SafetyConstraints:  fleetv1.RolloutSafetyConstraints{MinBatteryPct: 30, RequireIdleState: true},
		},
	}
}

func newRolloutReconciler(t *testing.T, objs ...client.Object) (*ModelRolloutReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).Build()
	return &ModelRolloutReconciler{Client: c, Scheme: scheme}, c
}

func reconcileRollout(t *testing.T, r *ModelRolloutReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "roll", Namespace: rolloutNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func rolloutRobot(t *testing.T, c client.Client, name string) *fleetv1.Robot {
	t.Helper()
	return getRobot(t, c, name, rolloutNS)
}

// Batch entry marks the model Updating and respects maxUnavailable=1.
func TestModelRollout_BatchEntryRespectsMaxUnavailable(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(),
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90),
		targetRobot("r2", fleetv1.RobotPhaseIdle, 90),
	)
	reconcileRollout(t, r)

	updating := 0
	for _, name := range []string{"r1", "r2"} {
		if e := modelEntry(getRobot(t, c, name, rolloutNS), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
			updating++
		}
	}
	if updating != 1 {
		t.Fatalf("updating robots = %d, want 1 (maxUnavailable=1)", updating)
	}
}

// Robots failing safety constraints are skipped (no batch entry).
func TestModelRollout_SkipsUnsafeRobots(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(),
		targetRobot("low-batt", fleetv1.RobotPhaseIdle, 10),   // below minBatteryPct
		targetRobot("busy", fleetv1.RobotPhaseInProgress, 90), // not Idle
	)
	reconcileRollout(t, r)

	for _, name := range []string{"low-batt", "busy"} {
		if e := modelEntry(getRobot(t, c, name, rolloutNS), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
			t.Fatalf("%s entered the batch despite failing safety constraints", name)
		}
	}
}

// A robot that has no camera is silently skipped (requiredHardware unmet).
func TestModelRollout_SkipsMissingRequiredHardware(t *testing.T) {
	ro := pickerRollout()
	ro.Spec.RequiredHardware = []fleetv1.HardwareComponentType{fleetv1.HardwareTypeCamera}
	noCam := targetRobot("no-cam", fleetv1.RobotPhaseIdle, 90)
	noCam.Spec.Hardware = nil
	noCam.Status.Hardware = nil

	r, c := newRolloutReconciler(t, ro, noCam)
	reconcileRollout(t, r)

	if e := modelEntry(rolloutRobot(t, c, "no-cam"), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
		t.Fatal("robot without the required camera entered the batch")
	}
}

// On success (adapter reports Active@newVersion), grants are projected to
// modelGrantedCapabilities with revokes removed; rollout reaches Succeeded.
func TestModelRollout_ProjectsGrantsOnSuccess(t *testing.T) {
	ro := pickerRollout()
	ro.Spec.RevokesCapabilities = []string{"inspect.defects"} // removed from the grant
	done := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	done.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.2.1"},
	}
	r, c := newRolloutReconciler(t, ro, done)
	reconcileRollout(t, r)

	rob := rolloutRobot(t, c, "r1")
	if len(rob.Status.ModelGrantedCapabilities) != 1 {
		t.Fatalf("modelGrantedCapabilities = %+v, want one entry", rob.Status.ModelGrantedCapabilities)
	}
	mg := rob.Status.ModelGrantedCapabilities[0]
	if mg.ModelName != "item-recognition" || mg.GrantedBy != "roll" {
		t.Fatalf("grant metadata = %+v", mg)
	}
	if !equalStringSets(mg.Capabilities, []string{"item-pick.ai-guided"}) {
		t.Fatalf("granted caps = %v, want only [item-pick.ai-guided] (inspect.defects revoked)", mg.Capabilities)
	}

	rollout := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, rollout); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if rollout.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", rollout.Status.Phase)
	}
	if rollout.Status.RobotsUpdated != 1 {
		t.Fatalf("RobotsUpdated = %d, want 1", rollout.Status.RobotsUpdated)
	}
}

// A failed robot + pauseOnError pauses the rollout and starts no new robots.
func TestModelRollout_PauseOnError(t *testing.T) {
	failed := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	failed.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusFailed, FailureReason: "inference health check failed"},
	}
	fresh := targetRobot("r2", fleetv1.RobotPhaseIdle, 90)
	r, c := newRolloutReconciler(t, pickerRollout(), failed, fresh)
	reconcileRollout(t, r)

	rollout := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, rollout); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if rollout.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused", rollout.Status.Phase)
	}
	if e := modelEntry(rolloutRobot(t, c, "r2"), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
		t.Fatal("paused rollout must not start a new robot")
	}
}

func TestMaxUnavailable(t *testing.T) {
	cases := []struct {
		spec  string
		total int
		want  int
	}{
		{"", 10, 1}, // default 10%
		{"10%", 100, 10},
		{"25%", 10, 2}, // 2.5 → 2
		{"5%", 10, 1},  // floors to 1
		{"3", 10, 3},
		{"bad", 10, 1},
	}
	for _, tc := range cases {
		if got := maxUnavailable(tc.spec, tc.total); got != tc.want {
			t.Errorf("maxUnavailable(%q,%d) = %d, want %d", tc.spec, tc.total, got, tc.want)
		}
	}
}
