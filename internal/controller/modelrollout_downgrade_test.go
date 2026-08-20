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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Batch selection must refuse a target whose newVersion is not strictly greater than the
// version a robot already runs (RFC-0001 §9.3.1). Before this, the controller compared for
// equality only: anything not exactly equal counted as "still needs updating", so a rollout
// naming an older version downgraded every robot already past it.
//
// The rule has two failure directions and both are tested. Refusing too little downgrades
// robots. Refusing too much silently empties the batch — and a rollout that did nothing
// looks exactly like a rollout with nothing to do.

func robotRunningModel(name, modelName, version string, status fleetv1.ModelStatus) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "warehouse-a"},
		Status: fleetv1.RobotStatus{
			InstalledModels: []fleetv1.InstalledModelStatusEntry{{
				Name: modelName, RunningVersion: version, Status: status,
			}},
		},
	}
}

func TestClassifyModel_RefusesADowngrade(t *testing.T) {
	// The robot is ahead of the rollout. Dispatching would revert it.
	rob := robotRunningModel("amr-1", "item-recognition", "4.1.0", fleetv1.ModelStatusActive)
	if got := classifyModel(rob, "item-recognition", "4.0.0"); got != modelNewer {
		t.Fatalf("a robot on 4.1.0 targeted at 4.0.0 must be refused, got state %v", got)
	}
}

func TestClassifyModel_StillUpdatesAnOlderRobot(t *testing.T) {
	// The ordinary case must keep working — this is the "refusing too much" direction.
	rob := robotRunningModel("amr-1", "item-recognition", "3.2.1", fleetv1.ModelStatusActive)
	if got := classifyModel(rob, "item-recognition", "4.0.0"); got != modelPending {
		t.Fatalf("a robot on 3.2.1 targeted at 4.0.0 must still be updated, got state %v", got)
	}
}

func TestClassifyModel_EqualIsDoneNotRefused(t *testing.T) {
	// Equality was already handled; it must stay `done`, not become `newer`. Reporting a
	// robot that is correctly at the target as "refused" would be a regression in meaning.
	rob := robotRunningModel("amr-1", "item-recognition", "4.0.0", fleetv1.ModelStatusActive)
	if got := classifyModel(rob, "item-recognition", "4.0.0"); got != modelDone {
		t.Fatalf("a robot already at the target must be done, got state %v", got)
	}
}

func TestClassifyModel_UnorderableVersionIsNotTreatedAsADowngrade(t *testing.T) {
	// RunningVersion is reported by the adapter, so it is whatever the vendor sends. An
	// unparseable value is not evidence the robot is ahead — refusing on it would empty
	// batches for entire vendors, and the emptiness would be silent.
	for _, v := range []string{"", "latest", "v4.1.0", "4.1", "4.1.0-rc1", "4.1.0+b7"} {
		rob := robotRunningModel("amr-1", "item-recognition", v, fleetv1.ModelStatusActive)
		if got := classifyModel(rob, "item-recognition", "4.0.0"); got != modelPending {
			t.Fatalf("RunningVersion %q is unorderable and must stay updatable, got state %v", v, got)
		}
	}
}

func TestClassifyModel_MajorAndMinorAreOrderedNumerically(t *testing.T) {
	// String comparison would call "10.0.0" older than "9.0.0". Numeric ordering is the
	// whole reason the version is constrained to major.minor.patch at admission.
	cases := []struct {
		running, target string
		want            modelState
	}{
		{"10.0.0", "9.0.0", modelNewer},   // lexically "10" < "9"
		{"9.0.0", "10.0.0", modelPending}, // and the reverse must still update
		{"4.10.0", "4.9.0", modelNewer},
		{"4.9.0", "4.10.0", modelPending},
		{"4.0.10", "4.0.9", modelNewer},
	}
	for _, c := range cases {
		rob := robotRunningModel("amr-1", "item-recognition", c.running, fleetv1.ModelStatusActive)
		if got := classifyModel(rob, "item-recognition", c.target); got != c.want {
			t.Fatalf("running %s targeting %s: want state %v, got %v", c.running, c.target, c.want, got)
		}
	}
}

func TestClassifyModel_NonActiveStatesAreUnaffected(t *testing.T) {
	// The version comparison lives on the Active branch only. A failed or updating robot is
	// classified by its status regardless of version, and must not be re-routed by this rule.
	newer := "9.9.9"
	if got := classifyModel(robotRunningModel("a", "m", newer, fleetv1.ModelStatusFailed), "m", "1.0.0"); got != modelFailed {
		t.Fatalf("a failed robot must stay failed, got %v", got)
	}
	if got := classifyModel(robotRunningModel("a", "m", newer, fleetv1.ModelStatusUpdating), "m", "1.0.0"); got != modelUpdating {
		t.Fatalf("an updating robot must stay updating, got %v", got)
	}
}

func TestVersionIsNewer_ReportsUnorderableSeparately(t *testing.T) {
	// ok=false must mean "cannot be ordered", never "not newer" — a caller that conflates
	// the two would treat an unparseable version as a definite negative.
	if _, ok := versionIsNewer("latest", "1.0.0"); ok {
		t.Fatal("an unparseable `have` must report ok=false")
	}
	if _, ok := versionIsNewer("1.0.0", "latest"); ok {
		t.Fatal("an unparseable `want` must report ok=false")
	}
	if newer, ok := versionIsNewer("1.0.0", "1.0.0"); !ok || newer {
		t.Fatalf("equal versions: want (false, true), got (%v, %v)", newer, ok)
	}
	if newer, ok := versionIsNewer("1.0.1", "1.0.0"); !ok || !newer {
		t.Fatalf("1.0.1 > 1.0.0: want (true, true), got (%v, %v)", newer, ok)
	}
	// A negative component is not a version; Atoi would accept "-1" without the guard.
	if _, ok := versionIsNewer("1.-1.0", "1.0.0"); ok {
		t.Fatal("a negative component must not parse")
	}
}

// ── status reporting ───────────────────────────────────────────────────────────────────

func TestRolloutStatus_RefusedRobotsAreReportedNotHidden(t *testing.T) {
	// A refused robot must be visible and must NOT sit in RobotsPending forever — a pending
	// count that can never drain is a progress bar with no end.
	newer := []*fleetv1.Robot{
		robotRunningModel("amr-1", "m", "5.0.0", fleetv1.ModelStatusActive),
		robotRunningModel("amr-2", "m", "5.0.0", fleetv1.ModelStatusActive),
	}
	done := []*fleetv1.Robot{robotRunningModel("amr-3", "m", "4.0.0", fleetv1.ModelStatusActive)}

	st := computeRolloutStatus("m", 3, done, nil, nil, nil, newer, nil, false, nil, nil)
	if st.RobotsIneligible != 2 {
		t.Fatalf("refused robots must be counted ineligible, got %d", st.RobotsIneligible)
	}
	if st.RobotsPending != 0 {
		t.Fatalf("refused robots must not be counted pending, got %d", st.RobotsPending)
	}
	if st.RobotsUpdated != 1 {
		t.Fatalf("a refusal must not inflate robotsUpdated, got %d", st.RobotsUpdated)
	}
}

func TestRolloutStatus_AllRefusedStillSettles(t *testing.T) {
	// Every selector-matched robot is already ahead. The rollout has nothing to do and must
	// reach a terminal phase rather than spin In-Progress against robots it will never touch.
	newer := []*fleetv1.Robot{
		robotRunningModel("amr-1", "m", "5.0.0", fleetv1.ModelStatusActive),
		robotRunningModel("amr-2", "m", "5.0.0", fleetv1.ModelStatusActive),
	}
	st := computeRolloutStatus("m", 2, nil, nil, nil, nil, newer, nil, false, nil, nil)
	if st.Phase == fleetv1.RolloutPhaseInProgress {
		t.Fatalf("a rollout whose targets are all ahead must settle, got phase %s", st.Phase)
	}
	if st.RobotsIneligible != 2 {
		t.Fatalf("want 2 ineligible, got %d", st.RobotsIneligible)
	}
}
