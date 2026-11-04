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
	"sync"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
)

// Rules for every S row (RFC-0001 #safety-robot-stop-table), on a real API server, where a status
// patch with a resourceVersion precondition is refused on a conflict: the fake client's update is
// not atomic, so these rows run on envtest.

// beforeStatusPatch runs hook once, ahead of the first status patch whose estopState is state.
type beforeStatusPatch struct {
	client.Client
	state fleetv1.RobotEstopState
	hook  func()
	once  sync.Once
}

func (b *beforeStatusPatch) Status() client.SubResourceWriter {
	return &beforeStatusWriter{SubResourceWriter: b.Client.Status(), b: b}
}

type beforeStatusWriter struct {
	client.SubResourceWriter
	b *beforeStatusPatch
}

func (w *beforeStatusWriter) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	if r, ok := obj.(*fleetv1.Robot); ok && r.Status.EstopState == w.b.state {
		w.b.once.Do(w.b.hook)
	}
	return w.SubResourceWriter.Patch(ctx, obj, p, opts...)
}

// recordAnotherStop does what another leader's stop does to the Robot between a write's read and
// its patch: it writes Stopping and records the stop, uncleared.
func recordAnotherStop(t *testing.T, ns string, state fleetv1.RobotEstopState) {
	t.Helper()
	var r fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "amr-1"}, &r); err != nil {
		t.Error(err)
		return
	}
	if state != "" {
		r.Status.EstopState = state
	}
	r.Status.Stops = append(r.Status.Stops, fleetv1.RobotStop{ID: "b-stop", Scope: fleetv1.StopScopeRobot,
		Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierRobot, Name: "amr-1", UID: string(r.UID)},
		SentAt:  metav1.Now()})
	if err := envK8s.Status().Update(context.Background(), &r); err != nil {
		t.Error(err)
	}
}

// Two dispatchers on one API server. A reads the stopped robot to clear it; B writes Stopping and
// records its stop before A's patch. A's Resuming fails its precondition and is not written: read
// again, the robot is Stopping, which no clear moves.
func TestEnvtest_EstopPrecondition_AStaleClearConflictsAndIsNotWritten(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatal(err)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
	a := safety.New(&beforeStatusPatch{Client: envK8s, state: fleetv1.RobotEstopResuming,
		hook: func() { recordAnotherStop(t, ns, fleetv1.RobotEstopStopping) }}, nil)
	a.APIReader = envK8s
	st, err := a.ClearEstop(ctx, ns, "amr-1", "alice", safety.Ends{})
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if got := readEstopState(t, ns, "amr-1"); st != fleetv1.RobotEstopStopping || got != fleetv1.RobotEstopStopping {
		t.Fatalf("ClearEstop returned %s, stored %s; want Stopping on both: the stale clear is not written", st, got)
	}
}

// S6 under a newly issued stop: a stop recorded between the release's read and its write leaves
// the robot Resuming.
func TestEnvtest_EstopPrecondition_TheReleaseUnderANewStopDoesNotRelease(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatal(err)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
	frames := resumeWitness()
	d := safety.New(&beforeStatusPatch{Client: envK8s, state: fleetv1.RobotEstopNormal,
		hook: func() { recordAnotherStop(t, ns, "") }}, nil)
	d.APIReader = envK8s
	d.Telemetry = frames
	d.DeriveCapabilities = DeriveCapabilities
	if st, err := d.ClearEstop(ctx, ns, "amr-1", "alice", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	time.Sleep(10 * time.Millisecond)
	sendResumeFrame(t, frames, ns, "amr-1")
	st, why, err := d.ConfirmResume(ctx, ns, "amr-1", "alice")
	if err != nil || st == fleetv1.RobotEstopNormal || readEstopState(t, ns, "amr-1") != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume = %s (%s), %v, stored %s; want the robot left Resuming",
			st, why, err, readEstopState(t, ns, "amr-1"))
	}
}

// F12 on a real API server: an InProgress action whose robot's stop was cleared before row A1
// paused it gets no lease renewal, and the hold outlives the release.
func TestEnvtest_LeaseAfterClear_NoRenewalForWorkA1NeverPaused(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatal(err)
	}
	robot := envtestValidRobot(ns, "amr-1", "z-a")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatal(err)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopResuming, time.Now())
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "amr-1"}, robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.AssignedAction, robot.Status.Phase = "task-1", fleetv1.RobotPhaseInProgress
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatal(err)
	}
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z-a", Priority: fleetv1.ActionPriorityNormal},
	}
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatal(err)
	}
	lease := metav1.NewTime(time.Now().Add(defaultLeaseDuration).Truncate(time.Second))
	action.Status = fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, AssignedRobot: "amr-1",
		AssignmentGeneration: 4, LeaseExpiresAt: &lease}
	if err := envK8s.Status().Update(ctx, action); err != nil {
		t.Fatal(err)
	}
	cmd := &fakeCommander{}
	actions := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Commander: cmd}
	for i := 0; i < 2; i++ {
		reconcileResumeAction(t, actions, ns)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopNormal, time.Now())
	reconcileResumeAction(t, actions, ns)
	var got fleetv1.FleetAction
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "task-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if len(cmd.renews) != 0 || !got.Status.LeaseExpiresAt.Equal(&lease) ||
		apimeta.FindStatusCondition(got.Status.Conditions, conditionRenewalHeld) == nil {
		t.Fatalf("%d renewals, lease %v (was %v), conditions %+v; want none, frozen, RenewalHeld",
			len(cmd.renews), got.Status.LeaseExpiresAt, lease, got.Status.Conditions)
	}
}
