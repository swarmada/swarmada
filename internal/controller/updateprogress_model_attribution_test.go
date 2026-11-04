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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ADR-0055 — UpdateProgress correlation must not attribute a model install to
// whichever rollout the API server's name-sorted list order happened to put in front.
//
// The precondition is a SUPPORTED configuration, not a misuse: classifyModel and
// upsertModelStatus key strictly on modelName, so a robot marked Updating for one model
// still classifies as pending for another, becomes eligible, and enters the second
// rollout's batch while still in the first.

// twoModelRollout builds an InProgress ModelRollout for one model whose currentBatch holds
// robotName. The names are chosen so "a-nav-stack" sorts before "z-vision-stack": the old
// code returned the first match, so list order WAS the answer.
func twoModelRollout(name, modelName, newVersion, robotName string) *fleetv1.ModelRollout {
	return &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rolloutNS},
		Spec: fleetv1.ModelRolloutSpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			ModelName:      modelName,
			NewVersion:     newVersion,
			ModelURI:       "oci://registry/models/" + modelName + ":" + newVersion,
		},
		Status: fleetv1.ModelRolloutStatus{
			Phase:        fleetv1.RolloutPhaseInProgress,
			CurrentBatch: []fleetv1.RolloutBatchRobot{{RobotName: robotName}},
		},
	}
}

// concurrentRolloutIngestor seeds one robot sitting in BOTH rollouts' batches at once.
func concurrentRolloutIngestor(t *testing.T) (*UpdateProgressIngestor, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Annotations = map[string]string{RobotIDAnnotation: "r1"}
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "a-nav-stack", Status: fleetv1.ModelStatusUpdating},
		{Name: "z-vision-stack", Status: fleetv1.ModelStatusUpdating},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		rob,
		twoModelRollout("roll-nav", "a-nav-stack", "1.4.0", "r1"),
		twoModelRollout("roll-vision", "z-vision-stack", "2.0.0", "r1"),
	).WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).Build()
	return &UpdateProgressIngestor{Client: c}, c
}

func modelEntryOf(t *testing.T, c client.Client, robot, model string) *fleetv1.InstalledModelStatusEntry {
	t.Helper()
	return modelEntry(getRobot(t, c, robot, rolloutNS), model)
}

// The sharp end of it: a FAILED outcome attributed to the wrong model puts THAT model
// in the failed set, and under rollbackPolicy: Auto the rollback pass then pushes a revert of
// a model that never failed — an install onto a healthy robot to fix a failure that happened
// somewhere else.
func TestUpdateProgress_FailureLandsOnTheReportedModelOnly(t *testing.T) {
	ing, c := concurrentRolloutIngestor(t)

	// The report is for the SECOND model by list order. Before ADR-0055 this landed on
	// "a-nav-stack" because that rollout sorted first.
	err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
		RobotId:          "r1",
		Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
		ModelName:        "z-vision-stack",
		Outcome:          fav1.InstallOutcome_INSTALL_OUTCOME_FAILED,
		FailureReason:    "inference health check failed",
		ResultingVersion: "1.9.0",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	failed := modelEntryOf(t, c, "r1", "z-vision-stack")
	if failed == nil || failed.Status != fleetv1.ModelStatusFailed {
		t.Fatalf("reported model entry = %+v, want Failed", failed)
	}
	if failed.FailureReason != "inference health check failed" {
		t.Errorf("failure reason = %q, want the reported one", failed.FailureReason)
	}

	other := modelEntryOf(t, c, "r1", "a-nav-stack")
	if other == nil || other.Status != fleetv1.ModelStatusUpdating {
		t.Fatalf("the OTHER model's entry = %+v, want it untouched at Updating — "+
			"a misattributed failure is what pushes a revert of a model that never failed", other)
	}
	if other.FailureReason != "" {
		t.Errorf("the other model picked up a failure reason %q", other.FailureReason)
	}
}

// The success half, same shape: resulting_version must not be written against the model the
// robot did not report.
func TestUpdateProgress_SuccessLandsOnTheReportedModelOnly(t *testing.T) {
	ing, c := concurrentRolloutIngestor(t)

	if err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
		RobotId:          "r1",
		Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
		ModelName:        "z-vision-stack",
		Outcome:          fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
		ResultingVersion: "2.0.0",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	got := modelEntryOf(t, c, "r1", "z-vision-stack")
	if got == nil || got.Status != fleetv1.ModelStatusActive || got.RunningVersion != "2.0.0" {
		t.Fatalf("reported model entry = %+v, want Active@2.0.0", got)
	}
	other := modelEntryOf(t, c, "r1", "a-nav-stack")
	if other == nil || other.Status != fleetv1.ModelStatusUpdating || other.RunningVersion != "" {
		t.Fatalf("the OTHER model's entry = %+v, want untouched at Updating with no runningVersion", other)
	}
}

// An adapter that does not set model_name leaves the control plane with an ambiguous
// question. It must DEFER (to the ADR-0033 CapabilitiesSnapshot carrier), not guess — a
// silent wrong write is worse than a delayed right one.
func TestUpdateProgress_UnnamedModelWithTwoInFlightRolloutsRefusesToGuess(t *testing.T) {
	for _, outcome := range []fav1.InstallOutcome{
		fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
		fav1.InstallOutcome_INSTALL_OUTCOME_FAILED,
	} {
		t.Run(outcome.String(), func(t *testing.T) {
			ing, c := concurrentRolloutIngestor(t)
			if err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
				RobotId:          "r1",
				Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
				Outcome:          outcome,
				ResultingVersion: "9.9.9",
			}); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			for _, m := range []string{"a-nav-stack", "z-vision-stack"} {
				e := modelEntryOf(t, c, "r1", m)
				if e == nil || e.Status != fleetv1.ModelStatusUpdating || e.RunningVersion != "" {
					t.Fatalf("%s entry = %+v after an ambiguous report; want untouched at Updating", m, e)
				}
			}
		})
	}
}

// A robot in exactly ONE in-flight rollout is unambiguous, so an adapter that sets no
// model_name is still correlated. This is the ordinary path and must not regress.
func TestUpdateProgress_UnnamedModelStillCorrelatesWhenUnambiguous(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Annotations = map[string]string{RobotIDAnnotation: "r1"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		rob, twoModelRollout("roll-nav", "a-nav-stack", "1.4.0", "r1"),
	).WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).Build()
	ing := &UpdateProgressIngestor{Client: c}

	if err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
		RobotId:          "r1",
		Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
		Outcome:          fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
		ResultingVersion: "1.4.0",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := modelEntryOf(t, c, "r1", "a-nav-stack")
	if got == nil || got.Status != fleetv1.ModelStatusActive || got.RunningVersion != "1.4.0" {
		t.Fatalf("entry = %+v, want Active@1.4.0 — an unambiguous unnamed report must still correlate", got)
	}
}

// The advisory phase path carries the same defect and gets the same treatment: a
// Downloading for one model must not overwrite the other model's updatePhase.
func TestUpdateProgress_AdvisoryPhaseLandsOnTheReportedRolloutOnly(t *testing.T) {
	ing, c := concurrentRolloutIngestor(t)

	if err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
		RobotId:   "r1",
		Kind:      fav1.UpdateKind_UPDATE_KIND_MODEL,
		ModelName: "z-vision-stack",
		Phase:     "Downloading",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	for name, want := range map[string]string{"roll-vision": "Downloading", "roll-nav": ""} {
		roll := &fleetv1.ModelRollout{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: rolloutNS}, roll); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got := roll.Status.CurrentBatch[0].UpdatePhase; got != want {
			t.Errorf("%s updatePhase = %q, want %q", name, got, want)
		}
	}
}

// An unnamed advisory phase with two candidates is dropped rather than written to the wrong
// rollout. A dropped progress display costs a spinner; a wrong one misreports which install
// a robot is in the middle of.
func TestUpdateProgress_UnnamedAdvisoryPhaseWithTwoCandidatesIsDropped(t *testing.T) {
	ing, c := concurrentRolloutIngestor(t)

	if err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
		RobotId: "r1",
		Kind:    fav1.UpdateKind_UPDATE_KIND_MODEL,
		Phase:   "Downloading",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	for _, name := range []string{"roll-vision", "roll-nav"} {
		roll := &fleetv1.ModelRollout{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: rolloutNS}, roll); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got := roll.Status.CurrentBatch[0].UpdatePhase; got != "" {
			t.Errorf("%s updatePhase = %q, want empty (ambiguous report dropped)", name, got)
		}
	}
}

// A named FAILED outcome does NOT bypass the guard Y4 added: an adapter that DECLINES an
// artifact has installed nothing and changed nothing about itself, so its refusal must not
// mark the model Failed on a robot the control plane never entered into a batch. model_name
// says WHICH model, never WHETHER to record.
func TestUpdateProgress_NamedFailureOutsideAnyBatchIsStillNotRecorded(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Annotations = map[string]string{RobotIDAnnotation: "r1"}
	roll := twoModelRollout("roll-nav", "a-nav-stack", "1.4.0", "r1")
	roll.Status.CurrentBatch = nil // pushed, declined, never entered
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rob, roll).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).Build()
	ing := &UpdateProgressIngestor{Client: c}

	if err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
		RobotId:       "r1",
		Kind:          fav1.UpdateKind_UPDATE_KIND_MODEL,
		ModelName:     "a-nav-stack",
		Outcome:       fav1.InstallOutcome_INSTALL_OUTCOME_FAILED,
		FailureReason: "refused: checksum mismatch",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if e := modelEntryOf(t, c, "r1", "a-nav-stack"); e != nil && e.Status == fleetv1.ModelStatusFailed {
		t.Fatalf("a declined push was recorded as a model failure: %+v — this is the guard "+
			"that stops a revert being pushed to a robot still running the previous version", e)
	}
}
