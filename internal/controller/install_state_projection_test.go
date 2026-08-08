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

// Projection of adapter-reported install state onto Robot.status (ADR-0033). Before this,
// Robot.status.installedModels[] carried only the control plane's record of what it pushed,
// so a rollout could never observe an install finishing.

const ipNS = "warehouse-a"

var ipBase = time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)

func ipRobot(name string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ipNS,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: name},
		},
		Spec: fleetv1.RobotSpec{
			Zone: "z1", Manufacturer: "Acme", Model: "X1",
			Adapter: fleetv1.AdapterRef{Name: "sim-adapter", Version: "1.0.0"},
		},
	}
}

func ipClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
}

func ipGet(t *testing.T, c client.Client, name string) *fleetv1.Robot {
	t.Helper()
	var rob fleetv1.Robot
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ipNS, Name: name}, &rob); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return &rob
}

func ipProgress(kind fav1.UpdateKind, outcome fav1.InstallOutcome, version, reason string) *fav1.UpdateProgress {
	return &fav1.UpdateProgress{
		RobotId: "amr-1", Kind: kind, Outcome: outcome,
		ResultingVersion: version, FailureReason: reason,
	}
}

func ipIngestor(c client.Client) *UpdateProgressIngestor {
	return &UpdateProgressIngestor{Client: c, now: func() time.Time { return ipBase }}
}

func TestProject_TerminalFirmwareFailureLandsOnRobotStatus(t *testing.T) {
	c := ipClient(t, ipRobot("amr-1"))
	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_FIRMWARE,
		fav1.InstallOutcome_INSTALL_OUTCOME_FAILED, "2.1.0", "checksum mismatch after reboot")

	if err := ipIngestor(c).IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	fi := ipGet(t, c, "amr-1").Status.FirmwareInstall
	if fi == nil {
		t.Fatal("a terminal failure must be projected onto Robot.status")
	}
	if fi.Status != fleetv1.FirmwareInstallFailed {
		t.Errorf("status = %q, want Failed", fi.Status)
	}
	if fi.FailureReason != "checksum mismatch after reboot" {
		t.Errorf("the adapter's reason must survive verbatim, got %q", fi.FailureReason)
	}
	// The version the robot FELL BACK TO, taken from the report rather than assumed. A
	// failed install may leave it on the old version, a recovery image, or elsewhere; only
	// the robot knows, and this is the fact an operator acts on.
	if fi.RunningVersion != "2.1.0" {
		t.Errorf("runningVersion = %q, want the reported fallback", fi.RunningVersion)
	}
}

func TestProject_TerminalReportNeedsNoPhase(t *testing.T) {
	// The ingestor used to drop any message without a phase string. An install that has
	// finished has no phase left to be in, so that guard discarded precisely the message
	// that ends a rollout.
	c := ipClient(t, ipRobot("amr-1"))
	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_FIRMWARE,
		fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED, "2.2.0", "")
	u.Phase = "" // explicit: terminal, no phase

	if err := ipIngestor(c).IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	fi := ipGet(t, c, "amr-1").Status.FirmwareInstall
	if fi == nil || fi.Status != fleetv1.FirmwareInstallRunning || fi.RunningVersion != "2.2.0" {
		t.Fatalf("a phase-less terminal report must still project, got %+v", fi)
	}
}

func TestProject_AdvisoryProgressProjectsNoInstallState(t *testing.T) {
	// UNSPECIFIED means "not finished" and must read as neither success nor failure.
	c := ipClient(t, ipRobot("amr-1"))
	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_FIRMWARE,
		fav1.InstallOutcome_INSTALL_OUTCOME_UNSPECIFIED, "", "")
	u.Phase = "Installing"

	if err := ipIngestor(c).IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if fi := ipGet(t, c, "amr-1").Status.FirmwareInstall; fi != nil {
		t.Fatalf("advisory progress must not claim a terminal state, got %+v", fi)
	}
}

func TestProject_RepeatedIdenticalReportWritesOnce(t *testing.T) {
	// RA-1. Both carriers report the same install, so a redundant report is the common
	// case, not the exception. resourceVersion is the check that no write occurred —
	// comparing the projected value would pass even if it were rewritten identically.
	c := ipClient(t, ipRobot("amr-1"))
	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_FIRMWARE,
		fav1.InstallOutcome_INSTALL_OUTCOME_FAILED, "2.1.0", "flash write error")
	ing := ipIngestor(c)

	if err := ing.IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	rv := ipGet(t, c, "amr-1").ResourceVersion

	for i := 0; i < 3; i++ {
		if err := ing.IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
			t.Fatalf("re-ingest: %v", err)
		}
	}
	if got := ipGet(t, c, "amr-1").ResourceVersion; got != rv {
		t.Errorf("an unchanged report caused a status write: resourceVersion %s -> %s", rv, got)
	}
}

func TestProject_SnapshotRecoversAnOutcomeWhoseStreamWasLost(t *testing.T) {
	// THE RECOVERY PATH. If the control plane restarts mid-install, the terminal
	// UpdateProgress is gone. Without the snapshot the rollout waits forever for a message
	// that already came and went — which is the wedged-rollout failure ADR-0033 exists to
	// close.
	c := ipClient(t, ipRobot("amr-1"))
	ing := &CapabilitiesIngestor{Client: c, now: func() time.Time { return ipBase }}
	snap := &fav1.CapabilitiesSnapshot{
		RobotId: "amr-1",
		Firmware: &fav1.FirmwareState{
			RunningVersion:   "2.0.0",
			Status:           fav1.FirmwareInstallStatus_FIRMWARE_INSTALL_STATUS_FAILED,
			AttemptedVersion: "2.1.0",
			FailureReason:    "install aborted",
		},
		InstalledModels: []*fav1.InstalledModel{{
			Name: "item-recognition", Status: fav1.ModelStatus_MODEL_STATUS_FAILED,
			RunningVersion: "3.0.0", FailureReason: "inference health check failed",
		}},
	}
	if err := ing.IngestCapabilities(context.Background(), ipNS, snap); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	rob := ipGet(t, c, "amr-1")

	fi := rob.Status.FirmwareInstall
	if fi == nil || fi.Status != fleetv1.FirmwareInstallFailed || fi.AttemptedVersion != "2.1.0" {
		t.Fatalf("firmware state not recovered from the snapshot: %+v", fi)
	}
	if len(rob.Status.InstalledModels) != 1 {
		t.Fatalf("model state not recovered: %+v", rob.Status.InstalledModels)
	}
	m := rob.Status.InstalledModels[0]
	if m.Status != fleetv1.ModelStatusFailed || m.FailureReason != "inference health check failed" {
		t.Errorf("model entry = %+v", m)
	}
}

func TestProject_UnreportedFirmwareStaysAbsent(t *testing.T) {
	// Explicit presence. An adapter that does not implement push_firmware reports
	// UNSPECIFIED, which is "not reporting" — not "reporting nothing". Writing a zero-value
	// state would make a silent adapter indistinguishable from one claiming a clean install.
	c := ipClient(t, ipRobot("amr-1"))
	ing := &CapabilitiesIngestor{Client: c, now: func() time.Time { return ipBase }}
	snap := &fav1.CapabilitiesSnapshot{
		RobotId:  "amr-1",
		Firmware: &fav1.FirmwareState{Status: fav1.FirmwareInstallStatus_FIRMWARE_INSTALL_STATUS_UNSPECIFIED},
	}
	if err := ing.IngestCapabilities(context.Background(), ipNS, snap); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if fi := ipGet(t, c, "amr-1").Status.FirmwareInstall; fi != nil {
		t.Fatalf("an unreporting adapter must leave the field absent, got %+v", fi)
	}
}

func TestProject_ModelOutcomeIsNamedByTheActiveRollout(t *testing.T) {
	// UpdateProgress does not carry a model name; the in-flight rollout supplies it. Without
	// that correlation a terminal model report has nothing to attach to.
	rollout := &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Namespace: ipNS, Name: "mr-1"},
		Spec:       fleetv1.ModelRolloutSpec{ModelName: "item-recognition", NewVersion: "3.2.1"},
		Status: fleetv1.ModelRolloutStatus{
			Phase:        fleetv1.RolloutPhaseInProgress,
			CurrentBatch: []fleetv1.RolloutBatchRobot{{RobotName: "amr-1", Namespace: ipNS}},
		},
	}
	c := ipClient(t, ipRobot("amr-1"), rollout)
	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_MODEL,
		fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED, "3.2.1", "")

	if err := ipIngestor(c).IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	models := ipGet(t, c, "amr-1").Status.InstalledModels
	if len(models) != 1 || models[0].Name != "item-recognition" {
		t.Fatalf("model entry not named from the active rollout: %+v", models)
	}
	// Active at the target version is exactly what classifyModel needs to reach modelDone —
	// the state that was previously unreachable and left every rollout stuck in Updating.
	if models[0].Status != fleetv1.ModelStatusActive || models[0].RunningVersion != "3.2.1" {
		t.Errorf("entry = %+v, want Active at 3.2.1", models[0])
	}
}

func TestProject_ModelOutcomeWithNoActiveRolloutIsDropped(t *testing.T) {
	// Nothing names the model, so there is no entry to write. Guessing one would attach a
	// real install outcome to the wrong model.
	c := ipClient(t, ipRobot("amr-1"))
	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_MODEL,
		fav1.InstallOutcome_INSTALL_OUTCOME_FAILED, "3.0.0", "boom")

	if err := ipIngestor(c).IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if m := ipGet(t, c, "amr-1").Status.InstalledModels; len(m) != 0 {
		t.Fatalf("an uncorrelatable model outcome must be dropped, got %+v", m)
	}
}

func TestProject_UnknownRobotIsDroppedNotFailed(t *testing.T) {
	// A report can arrive before the Robot exists, or for one already removed. Neither is
	// an error worth failing the stream over.
	c := ipClient(t)
	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_FIRMWARE,
		fav1.InstallOutcome_INSTALL_OUTCOME_FAILED, "2.1.0", "x")
	if err := ipIngestor(c).IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("an unknown robot_id must be dropped, not surfaced: %v", err)
	}
}

func TestProject_SameRobotIDInTwoNamespacesResolvesIndependently(t *testing.T) {
	// robot-id uniqueness is enforced PER NAMESPACE, so two namespaces may each hold an
	// "amr-1" legitimately. A cluster-wide resolver sees two matches and refuses as though
	// the state were invalid, breaking projection for both tenants at once.
	other := ipRobot("amr-1")
	other.Namespace = "warehouse-b"
	c := ipClient(t, ipRobot("amr-1"), other)

	u := ipProgress(fav1.UpdateKind_UPDATE_KIND_FIRMWARE,
		fav1.InstallOutcome_INSTALL_OUTCOME_FAILED, "2.1.0", "flash error")
	if err := ipIngestor(c).IngestUpdateProgress(context.Background(), ipNS, u); err != nil {
		t.Fatalf("a duplicate id in another namespace must not break resolution: %v", err)
	}

	if fi := ipGet(t, c, "amr-1").Status.FirmwareInstall; fi == nil {
		t.Fatal("the reporting namespace's robot was not projected")
	}
	// And the other tenant's robot is untouched.
	var them fleetv1.Robot
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "warehouse-b", Name: "amr-1"}, &them); err != nil {
		t.Fatalf("get: %v", err)
	}
	if them.Status.FirmwareInstall != nil {
		t.Error("a report in one namespace must not write another namespace's robot")
	}
}
