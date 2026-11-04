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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// An over-the-air update is dispatch: a robot a stop holds is not entered into a rollout
// batch, is not left with a firmware pull it was told to make before the stop, and is not
// pushed a model rollback, wherever the stop that holds it comes from (RFC-0001
// #firmwarerollout: a robot receiving an emergency stop is not rebooted, and every update
// attempt is gated on the robot's live state).

// stopCase is one way a stop can hold robot r1. Each sets up r1 and adds the objects that
// carry the stop.
type stopCase struct {
	name  string
	setup func(rob *fleetv1.Robot) []client.Object
}

func stopCases() []stopCase {
	zone := func(name, parent string, propagate *bool, triggered bool) *fleetv1.FleetZone {
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rolloutNS},
			Spec: fleetv1.FleetZoneSpec{ParentZone: parent}}
		if propagate != nil {
			z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: propagate}
		}
		if triggered {
			z.Annotations = map[string]string{annEstopTriggered: "stop"}
		}
		return z
	}
	return []stopCase{
		{"its estopState is Stopped", func(rob *fleetv1.Robot) []client.Object {
			rob.Status.EstopState = fleetv1.RobotEstopStopped
			return nil
		}},
		{"its estopState is Failed", func(rob *fleetv1.Robot) []client.Object {
			rob.Status.EstopState = fleetv1.RobotEstopFailed
			return nil
		}},
		{"it is Resuming", func(rob *fleetv1.Robot) []client.Object {
			rob.Status.EstopState = fleetv1.RobotEstopResuming
			return nil
		}},
		{"its own trigger", func(rob *fleetv1.Robot) []client.Object {
			rob.Annotations[annEstopTriggered] = "own"
			return nil
		}},
		{"a zone stop it records", func(rob *fleetv1.Robot) []client.Object {
			rob.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z"}}
			return nil
		}},
		{"its zone's trigger, before the fan-out reaches it", func(rob *fleetv1.Robot) []client.Object {
			rob.Spec.Zone, rob.Status.CurrentZone = "z", "z"
			return []client.Object{zone("z", "", nil, true)}
		}},
		{"an ancestor's stop that does not propagate to children", func(rob *fleetv1.Robot) []client.Object {
			rob.Spec.Zone, rob.Status.CurrentZone = "child", "child"
			return []client.Object{zone("anc", "", apiBool(false), true), zone("child", "anc", nil, false)}
		}},
		{"the namespace's stop", func(rob *fleetv1.Robot) []client.Object {
			return []client.Object{&fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName,
				Namespace: rolloutNS, Annotations: map[string]string{annEstopTriggered: "ns"}}}}
		}},
		{"the namespace's stop, cleared and not yet applied", func(rob *fleetv1.Robot) []client.Object {
			return []client.Object{&fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName,
				Namespace: rolloutNS, Annotations: map[string]string{annEstopProcessed: "ns"}}}}
		}},
	}
}

// unsignedConfig is the namespace's SwarmadaConfig with signing off, carrying a stop's
// annotations when a case put them on a config of its own.
func unsignedConfig(extra []client.Object) []client.Object {
	for _, o := range extra {
		if cfg, ok := o.(*fleetv1.SwarmadaConfig); ok {
			cfg.Spec.Signing.RequireSignatureVerification = apiBool(false)
			return extra
		}
	}
	return append(extra, signingConfig(false))
}

// inFirmwareBatch is robot r1 told to pull firmware 2.5.0 by rollout fw, its batch entry
// at the initial phase: the robot has reported nothing yet.
func inFirmwareBatch(started time.Time) (*fleetv1.FirmwareRollout, *fleetv1.Robot) {
	ro := fwRollout("")
	ro.Status.Phase = fleetv1.RolloutPhaseInProgress
	ro.Status.CurrentBatch = []fleetv1.RolloutBatchRobot{{RobotName: "r1", Namespace: rolloutNS,
		UpdateStartedAt: &metav1.Time{Time: started}, PreviousVersion: "2.4.0", UpdatePhase: firmwareInitialPhase}}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Annotations = map[string]string{
		annPendingFirmwareVersion:  "2.5.0",
		annPendingFirmwareURI:      "oci://reg/acme:2.5.0",
		annPendingFirmwareChecksum: fwChecksum,
	}
	rob.Status.FirmwareVersion = "2.4.0"
	return ro, rob
}

func eventsWith(rec *record.FakeRecorder, reason string) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, " "+reason+" ") {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

func inBatch(batch []fleetv1.RolloutBatchRobot, name string) bool {
	return batchEntryFor(batch, name) != nil
}

// A robot told to pull firmware, and stopped before its install began, has the pull
// withdrawn: the annotations go, the robot leaves the batch and counts as skipped.
func TestRolloutEstop_FirmwarePullIsWithdrawnFromAStoppedRobot(t *testing.T) {
	for _, tc := range stopCases() {
		t.Run(tc.name, func(t *testing.T) {
			ro, rob := inFirmwareBatch(time.Now().Add(-time.Minute))
			extra := tc.setup(rob)
			rec := record.NewFakeRecorder(64)
			r, c := newFirmwareReconciler(t, append(unsignedConfig(extra), ro, rob)...)
			r.Recorder = rec

			reconcileFirmware(t, r)

			got := getRobot(t, c, "r1", rolloutNS)
			fw := getFirmwareRollout(t, c)
			t.Logf("RESULT %s: pending-firmware=%q inBatch=%v skipped=%d", tc.name,
				got.Annotations[annPendingFirmwareVersion], inBatch(fw.Status.CurrentBatch, "r1"), fw.Status.RobotsSkipped)
			for _, k := range []string{annPendingFirmwareVersion, annPendingFirmwareURI, annPendingFirmwareChecksum} {
				if _, ok := got.Annotations[k]; ok {
					t.Errorf("%s is still on the robot: a robot under a stop keeps the pull it was told to make", k)
				}
			}
			if inBatch(fw.Status.CurrentBatch, "r1") {
				t.Error("the robot is still in status.currentBatch")
			}
			if fw.Status.RobotsSkipped != 1 {
				t.Errorf("robotsSkipped = %d, want 1", fw.Status.RobotsSkipped)
			}
			if ev := eventsWith(rec, "FirmwareUpdateWithheld"); len(ev) != 1 || !strings.Contains(ev[0], "r1") {
				t.Errorf("FirmwareUpdateWithheld events = %q, want one naming r1", ev)
			}
		})
	}
}

// An install the robot has reported under way is not withdrawn (nothing aborts it), and
// the operator is told once. A report that does not order after the dispatch is not
// taken as the install having begun.
func TestRolloutEstop_FirmwareInstallUnderWayIsKept(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	for name, tc := range map[string]struct {
		mut  func(*fleetv1.FirmwareRollout, *fleetv1.Robot)
		kept bool
	}{
		"the adapter reports Installing": {func(ro *fleetv1.FirmwareRollout, _ *fleetv1.Robot) {
			ro.Status.CurrentBatch[0].UpdatePhase = "Installing"
		}, true},
		"the robot reports Updating after the dispatch": {func(_ *fleetv1.FirmwareRollout, rob *fleetv1.Robot) {
			rob.Status.FirmwareInstall = &fleetv1.FirmwareInstallState{Status: fleetv1.FirmwareInstallUpdating,
				AttemptedVersion: "2.5.0", ReportedAt: &metav1.Time{Time: started.Add(10 * time.Second)}}
		}, true},
		"the robot's Updating report is older than the dispatch": {func(_ *fleetv1.FirmwareRollout, rob *fleetv1.Robot) {
			rob.Status.FirmwareInstall = &fleetv1.FirmwareInstallState{Status: fleetv1.FirmwareInstallUpdating,
				AttemptedVersion: "2.5.0", ReportedAt: &metav1.Time{Time: started.Add(-time.Hour)}}
		}, false},
		"the adapter reports Pulling": {func(ro *fleetv1.FirmwareRollout, _ *fleetv1.Robot) {
			ro.Status.CurrentBatch[0].UpdatePhase = "Pulling"
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			ro, rob := inFirmwareBatch(started)
			rob.Status.EstopState = fleetv1.RobotEstopStopped
			tc.mut(ro, rob)
			rec := record.NewFakeRecorder(64)
			r, c := newFirmwareReconciler(t, signingConfig(false), ro, rob)
			r.Recorder = rec
			reconcileFirmware(t, r)
			reconcileFirmware(t, r)

			kept := robotPending(t, c, "r1")
			t.Logf("RESULT %s: kept=%v", name, kept)
			if kept != tc.kept {
				t.Fatalf("pending firmware kept = %v, want %v", kept, tc.kept)
			}
			if tc.kept {
				if ev := eventsWith(rec, "FirmwareUpdateUnderWayUnderStop"); len(ev) != 1 {
					t.Errorf("FirmwareUpdateUnderWayUnderStop events over two passes = %d, want 1", len(ev))
				}
			}
		})
	}
}

// Once nothing holds the robot, the withdrawn update is offered again.
func TestRolloutEstop_WithdrawnFirmwareIsOfferedAgainAfterTheRelease(t *testing.T) {
	ro, rob := inFirmwareBatch(time.Now().Add(-time.Minute))
	rob.Status.EstopState = fleetv1.RobotEstopStopped
	r, c := newFirmwareReconciler(t, signingConfig(false), ro, rob)
	reconcileFirmware(t, r)
	if robotPending(t, c, "r1") {
		t.Fatal("setup: the pull was not withdrawn from the stopped robot")
	}
	reconcileFirmware(t, r)
	if robotPending(t, c, "r1") {
		t.Fatal("a robot still under its stop was told to pull again")
	}

	got := getRobot(t, c, "r1", rolloutNS)
	got.Status.EstopState = fleetv1.RobotEstopNormal
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("releasing the robot: %v", err)
	}
	reconcileFirmware(t, r)
	if !robotPending(t, c, "r1") {
		t.Fatal("the released robot was not offered the update again")
	}
}

// A robot that is not held keeps its place in the batch: the control.
func TestRolloutEstop_ARobotNoStopHoldsKeepsItsPull(t *testing.T) {
	ro, rob := inFirmwareBatch(time.Now().Add(-time.Minute))
	r, c := newFirmwareReconciler(t, signingConfig(false), ro, rob)
	reconcileFirmware(t, r)
	if !robotPending(t, c, "r1") || !inBatch(getFirmwareRollout(t, c).Status.CurrentBatch, "r1") {
		t.Fatal("a robot no stop holds lost its pending firmware")
	}
}

// A robot a stop holds does not enter either kind of batch, whichever carrier holds it.
func TestRolloutEstop_NoEntryWhereDispatchIsWithheld(t *testing.T) {
	for _, tc := range stopCases() {
		t.Run(tc.name, func(t *testing.T) {
			fwRob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
			fwRob.Annotations = map[string]string{}
			extra := tc.setup(fwRob)
			fr, fc := newFirmwareReconciler(t, append(unsignedConfig(extra), fwRollout(""), fwRob)...)
			reconcileFirmware(t, fr)

			mRob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
			mRob.Annotations = map[string]string{}
			extra = tc.setup(mRob)
			pusher := &fakePusher{ack: true}
			mr, _ := newRolloutReconciler(t, append(extra, pickerRollout(), mRob)...)
			mr.Pusher = pusher
			reconcileRollout(t, mr)

			t.Logf("RESULT %s: firmware annotated=%v model pushes=%d", tc.name, robotPending(t, fc, "r1"), len(pusher.pushes))
			if robotPending(t, fc, "r1") {
				t.Error("a held robot was told to pull firmware")
			}
			if len(pusher.pushes) != 0 {
				t.Errorf("a held robot was pushed a model_update: %+v", pusher.pushes)
			}
		})
	}
}

// A failed robot a stop holds is not pushed its Auto rollback; it stays failed and the push
// is retried once nothing holds it.
func TestRolloutEstop_ModelRollbackIsNotPushedToAStoppedRobot(t *testing.T) {
	for _, tc := range stopCases() {
		t.Run(tc.name, func(t *testing.T) {
			rob := failedModelRobot("r1", "inference health check failed")
			rob.Annotations = map[string]string{}
			extra := tc.setup(rob)
			pusher := &fakePusher{ack: true}
			rec := record.NewFakeRecorder(64)
			r, _ := newRolloutReconciler(t, append(extra, autoRollbackRollout(map[string]string{"r1": "3.2.0"}), rob)...)
			r.Pusher, r.Recorder = pusher, rec
			reconcileRollout(t, r)
			reconcileRollout(t, r)

			t.Logf("RESULT %s: rollback pushes=%d", tc.name, len(pusher.pushes))
			if len(pusher.pushes) != 0 {
				t.Fatalf("a held robot was pushed its rollback: %+v", pusher.pushes)
			}
			if ev := eventsWith(rec, "ModelRollbackWithheld"); len(ev) != 1 {
				t.Errorf("ModelRollbackWithheld events over two passes = %d, want 1", len(ev))
			}
		})
	}
}

func TestRolloutEstop_HeldRollbackIsPushedAfterTheRelease(t *testing.T) {
	rob := failedModelRobot("r1", "inference health check failed")
	rob.Status.EstopState = fleetv1.RobotEstopStopped
	pusher := &fakePusher{ack: true}
	r, c := newRolloutReconciler(t, autoRollbackRollout(map[string]string{"r1": "3.2.0"}), rob)
	r.Pusher = pusher
	reconcileRollout(t, r)
	if len(pusher.pushes) != 0 {
		t.Fatalf("setup: the stopped robot was pushed its rollback: %+v", pusher.pushes)
	}
	got := rolloutRobot(t, c, "r1")
	got.Status.EstopState = fleetv1.RobotEstopNormal
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("releasing the robot: %v", err)
	}
	reconcileRollout(t, r)
	if len(pusher.pushes) != 1 || !pusher.pushes[0].Rollback {
		t.Fatalf("the released robot was not pushed its rollback: %+v", pusher.pushes)
	}
}

// staleCacheReconciler is a model reconciler whose client serves the robot as the cache had
// it, Normal, while the API server already stores it Stopped.
func staleCacheReconciler(t *testing.T, ro *fleetv1.ModelRollout, cached *fleetv1.Robot) (*ModelRolloutReconciler, *fakePusher) {
	t.Helper()
	stored := cached.DeepCopy()
	stored.Status.EstopState = fleetv1.RobotEstopStopped
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ro.DeepCopy(), stored).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).Build()
	r, _ := newRolloutReconciler(t, ro, cached)
	pusher := &fakePusher{ack: true}
	r.Pusher, r.APIReader = pusher, api
	return r, pusher
}

// A model push cannot be taken back, so it is decided on the robot the API server stores:
// a stop that reached the robot after the cache last saw it gets no push.
func TestRolloutEstop_TheModelPushReReadsTheRobot(t *testing.T) {
	t.Run("entry", func(t *testing.T) {
		r, pusher := staleCacheReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
		reconcileRollout(t, r)
		t.Logf("RESULT entry pushes=%d", len(pusher.pushes))
		if len(pusher.pushes) != 0 {
			t.Fatalf("a robot the API server stores Stopped was pushed a model_update: %+v", pusher.pushes)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		r, pusher := staleCacheReconciler(t, autoRollbackRollout(map[string]string{"r1": "3.2.0"}),
			failedModelRobot("r1", "inference health check failed"))
		reconcileRollout(t, r)
		t.Logf("RESULT rollback pushes=%d", len(pusher.pushes))
		if len(pusher.pushes) != 0 {
			t.Fatalf("a robot the API server stores Stopped was pushed its rollback: %+v", pusher.pushes)
		}
	})
}

// Deleting a ZoneMaintenance resumes every robot it paused, Stopped ones included, to Idle
// (it never touches the stop). The robot is Idle and still under its stop, so neither
// rollout reaches it.
func TestRolloutEstop_AZoneMaintenanceDeleteDoesNotMakeAStoppedRobotEligible(t *testing.T) {
	now := time.Now()
	zm := &fleetv1.ZoneMaintenance{ObjectMeta: metav1.ObjectMeta{Name: "zm", Namespace: rolloutNS},
		Spec: fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
			Mode: fleetv1.ZoneMaintenanceModeGraceful}}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.EstopState = fleetv1.RobotEstopStopped
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(zm, rob, signingConfig(false), fwRollout(""), pickerRollout()).
		WithStatusSubresource(&fleetv1.ZoneMaintenance{}, &fleetv1.Robot{}, &fleetv1.FirmwareRollout{},
			&fleetv1.ModelRollout{}).Build()
	zr := &ZoneMaintenanceReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}
	ctx := context.Background()
	zmReq := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rolloutNS, Name: "zm"}}
	for i := 0; i < 2; i++ {
		if _, err := zr.Reconcile(ctx, zmReq); err != nil {
			t.Fatalf("activating the window: %v", err)
		}
	}
	if p := getRobot(t, c, "r1", rolloutNS).Status.Phase; p != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("setup: the window did not pause the robot (phase %s)", p)
	}
	cur := &fleetv1.ZoneMaintenance{}
	if err := c.Get(ctx, zmReq.NamespacedName, cur); err != nil {
		t.Fatalf("reading the window: %v", err)
	}
	if err := c.Delete(ctx, cur); err != nil {
		t.Fatalf("deleting the window: %v", err)
	}
	if _, err := zr.Reconcile(ctx, zmReq); err != nil {
		t.Fatalf("the window's delete: %v", err)
	}
	after := getRobot(t, c, "r1", rolloutNS)
	t.Logf("RESULT after the window's delete: phase=%s estopState=%s", after.Status.Phase, after.Status.EstopState)
	if after.Status.Phase != fleetv1.RobotPhaseIdle || after.Status.EstopState != fleetv1.RobotEstopStopped {
		t.Fatalf("setup: want Idle and Stopped after the delete, got %s and %s", after.Status.Phase, after.Status.EstopState)
	}

	fr := &FirmwareRolloutReconciler{Client: c, Scheme: scheme}
	reconcileFirmware(t, fr)
	pusher := &fakePusher{ack: true}
	mr := &ModelRolloutReconciler{Client: c, Scheme: scheme, Pusher: pusher}
	reconcileRollout(t, mr)
	t.Logf("RESULT firmware annotated=%v model pushes=%d", robotPending(t, c, "r1"), len(pusher.pushes))
	if robotPending(t, c, "r1") || len(pusher.pushes) != 0 {
		t.Fatal("a Stopped robot made Idle by a window's delete was given an update")
	}
}
