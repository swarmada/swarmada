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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// buildRolloutBatch initializes new entrants at the initial phase and preserves an
// existing robot's reported phase / updateStartedAt across a recompute.
func TestBuildRolloutBatch_InitializesAndPreserves(t *testing.T) {
	updating := []*fleetv1.Robot{
		{ObjectMeta: metav1.ObjectMeta{Name: "amr-2", Namespace: "ns"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: "ns"}, Status: fleetv1.RobotStatus{FirmwareVersion: "2.4.1"}},
	}
	started := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	prior := []fleetv1.RolloutBatchRobot{
		{RobotName: "amr-1", UpdatePhase: "Verifying", UpdateStartedAt: &started, PreviousVersion: "2.4.1"},
	}

	batch := buildRolloutBatch(updating, prior, firmwareInitialPhase, firmwarePrevVersion, false)

	if len(batch) != 2 || batch[0].RobotName != "amr-1" || batch[1].RobotName != "amr-2" {
		t.Fatalf("batch = %+v, want sorted [amr-1 amr-2]", batch)
	}
	if batch[0].UpdatePhase != "Verifying" {
		t.Fatalf("amr-1 phase = %q, want the preserved Verifying", batch[0].UpdatePhase)
	}
	if batch[0].UpdateStartedAt == nil || !batch[0].UpdateStartedAt.Time.Equal(started.Time) {
		t.Fatal("amr-1 updateStartedAt must be preserved across a recompute")
	}
	if batch[1].UpdatePhase != firmwareInitialPhase {
		t.Fatalf("new entrant amr-2 phase = %q, want %q", batch[1].UpdatePhase, firmwareInitialPhase)
	}
	// Firmware rollouts suspend nothing (ADR-0023): no entry carries a suspension stamp.
	for _, b := range batch {
		if b.CapabilitiesSuspendedAt != nil {
			t.Fatalf("firmware batch entry %q must not stamp capabilitiesSuspendedAt", b.RobotName)
		}
	}
}

// The model path stamps capabilitiesSuspendedAt on a NEW entrant only, and the
// stamp is preserved for a continuing entrant — so it reflects the current
// attempt's suspension, not a re-computation time (ADR-0023).
func TestBuildRolloutBatch_ModelStampsSuspensionPerAttempt(t *testing.T) {
	updating := []*fleetv1.Robot{
		{ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: "ns"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "amr-2", Namespace: "ns"}},
	}
	// amr-1 was already in the batch with an earlier suspension stamp; amr-2 is new.
	suspended := metav1.NewTime(time.Now().Add(-2 * time.Minute).Truncate(time.Second))
	prior := []fleetv1.RolloutBatchRobot{
		{RobotName: "amr-1", UpdatePhase: "Installing", CapabilitiesSuspendedAt: &suspended},
	}

	batch := buildRolloutBatch(updating, prior, modelInitialPhase, func(*fleetv1.Robot) string { return "" }, true)

	byName := map[string]fleetv1.RolloutBatchRobot{}
	for _, b := range batch {
		byName[b.RobotName] = b
	}
	if got := byName["amr-1"].CapabilitiesSuspendedAt; got == nil || !got.Time.Equal(suspended.Time) {
		t.Fatalf("amr-1 suspension stamp = %v, want the preserved %v (same attempt)", got, suspended)
	}
	if byName["amr-2"].CapabilitiesSuspendedAt == nil {
		t.Fatal("new entrant amr-2 must be stamped with a fresh suspension time")
	}
	if byName["amr-2"].CapabilitiesSuspendedAt.Time.Equal(suspended.Time) {
		t.Fatal("new entrant amr-2 must get a FRESH stamp, not the prior attempt's")
	}
}

func newProgressIngestor(t *testing.T, objs ...client.Object) (*UpdateProgressIngestor, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.FirmwareRollout{}, &fleetv1.ModelRollout{}).
		WithObjects(objs...).Build()
	return &UpdateProgressIngestor{Client: c}, c
}

func progressRobot(name, robotID string) *fleetv1.Robot {
	return &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns", Annotations: map[string]string{RobotIDAnnotation: robotID},
	}}
}

func inProgressFirmware(name, robotInBatch, phase string) *fleetv1.FirmwareRollout {
	return &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: fleetv1.FirmwareRolloutStatus{
			Phase:        fleetv1.RolloutPhaseInProgress,
			CurrentBatch: []fleetv1.RolloutBatchRobot{{RobotName: robotInBatch, UpdatePhase: phase}},
		},
	}
}

func getFirmware(t *testing.T, c client.Client, name string) *fleetv1.FirmwareRollout {
	t.Helper()
	fr := &fleetv1.FirmwareRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, fr); err != nil {
		t.Fatalf("get firmwarerollout: %v", err)
	}
	return fr
}

// A firmware UpdateProgress advances the matching batch entry's updatePhase.
func TestUpdateProgress_AdvancesFirmwarePhase(t *testing.T) {
	i, c := newProgressIngestor(t,
		progressRobot("amr-1", "rid-1"),
		inProgressFirmware("fw", "amr-1", "Pulling"),
	)

	if err := i.IngestUpdateProgress(context.Background(), "ns", &fav1.UpdateProgress{
		RobotId: "rid-1", Kind: fav1.UpdateKind_UPDATE_KIND_FIRMWARE, Phase: "Verifying",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got := getFirmware(t, c, "fw").Status.CurrentBatch[0].UpdatePhase; got != "Verifying" {
		t.Fatalf("updatePhase = %q, want Verifying", got)
	}
}

// A robot not in any active batch is a no-op (the phase is left untouched).
func TestUpdateProgress_RobotNotInBatch_Noop(t *testing.T) {
	i, c := newProgressIngestor(t,
		progressRobot("amr-2", "rid-2"),
		inProgressFirmware("fw", "amr-1", "Pulling"), // batch holds amr-1, not amr-2
	)

	if err := i.IngestUpdateProgress(context.Background(), "ns", &fav1.UpdateProgress{
		RobotId: "rid-2", Kind: fav1.UpdateKind_UPDATE_KIND_FIRMWARE, Phase: "Verifying",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got := getFirmware(t, c, "fw").Status.CurrentBatch[0].UpdatePhase; got != "Pulling" {
		t.Fatalf("unrelated batch entry mutated to %q, want Pulling", got)
	}
}

// An unknown robot_id is dropped without error.
func TestUpdateProgress_UnknownRobot_Noop(t *testing.T) {
	i, _ := newProgressIngestor(t, inProgressFirmware("fw", "amr-1", "Pulling"))

	if err := i.IngestUpdateProgress(context.Background(), "ns", &fav1.UpdateProgress{
		RobotId: "ghost", Kind: fav1.UpdateKind_UPDATE_KIND_FIRMWARE, Phase: "Verifying",
	}); err != nil {
		t.Fatalf("unknown robot should be a no-op, got: %v", err)
	}
}
