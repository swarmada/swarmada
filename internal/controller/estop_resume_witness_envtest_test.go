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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// An operator's release request is refused until the robot itself has reported since
// the clear; its adapter being heard from is not that (RFC-0001
// #safety-estop-state-machine).
//
// Adapter presence stamps status.connectivity.lastSeenAt for every robot the adapter
// serves, whether or not any of them has sent a frame. A robot whose projection still
// reads Idle with Active capabilities from before the stop would pass every other
// check on evidence that says nothing about the robot. This test drives the real
// presence writer after a clear and requires a request to be refused, then sends the
// robot's own frame through the real telemetry plane — which releases nothing by
// itself — and requires a new request to release the robot, once.
func TestEnvtest_EstopResume_AdapterPresenceAloneDoesNotLeaveTheWindow(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	key := types.NamespacedName{Namespace: ns, Name: "amr-1"}

	frames := resumeWitness()
	dispatcher := safety.New(envK8s, nil)
	dispatcher.Telemetry = frames
	dispatcher.DeriveCapabilities = DeriveCapabilities
	estops := &RobotEstopReconciler{Client: envK8s, Scheme: envScheme, Resumer: dispatcher}
	recorder := record.NewFakeRecorder(8)
	setResumeRecorder(estops, recorder)
	presence := &FleetAdapterReconciler{Client: envK8s, Scheme: envScheme}

	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	gateEnvSeedAdapterStatus(t, ns, "acme-adapter", contract.Version)
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	// Stopped, over a projection that reads healthy: Idle, every capability Active.
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now().Add(-time.Minute))

	if state, err := dispatcher.ClearEstop(ctx, ns, "amr-1", "alice", safety.Ends{}); err != nil || state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v; want Resuming", state, err)
	}

	// ── adapter presence after the clear, and nothing from the robot ─────────
	if err := presence.stampLastSeen(ctx, key, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("stamp adapter presence: %v", err)
	}
	writeResumeRequest(t, key, "on presence")
	if _, err := estops.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile estop: %v", err)
	}
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s after a request on adapter presence alone, want Resuming: an adapter "+
			"heartbeat is not telemetry from the robot, and the projection it would judge predates the stop", got)
	}
	expectEvent(t, recorder, "Warning EstopResumeRefused", "no telemetry since the clear")

	// ── the robot's own frame, received after the clear: nothing by itself ────
	sendResumeFrame(t, frames, ns, "amr-1")
	if _, err := estops.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile estop: %v", err)
	}
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s after post-clear telemetry with no new request, want Resuming", got)
	}

	// ── a new request, now that the robot has reported ───────────────────────
	writeResumeRequest(t, key, "after the robot reported")
	if _, err := estops.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile estop: %v", err)
	}
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s after a request on post-clear telemetry, want Normal", got)
	}

	// RA-1: the transition is written once. Further reconciles of the released robot
	// write nothing.
	var before fleetv1.Robot
	if err := envK8s.Get(ctx, key, &before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := estops.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile estop: %v", err)
		}
	}
	var after fleetv1.Robot
	if err := envK8s.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("resourceVersion %s -> %s: a released robot was written again on reconcile",
			before.ResourceVersion, after.ResourceVersion)
	}
}

// resumeWitness is the telemetry plane the manager wires as the resume witness: a
// Router whose status sink is the real one, with the default (drop) TSDB plane.
func resumeWitness() *telemetry.Router {
	return telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&RobotStatusSink{Client: envK8s})
}

// sendResumeFrame delivers one frame of the robot's own telemetry, as the
// ControlStream server does for each TelemetryPayload.
func sendResumeFrame(t *testing.T, r *telemetry.Router, ns, robotID string) {
	t.Helper()
	pct := int32(80)
	if err := r.Ingest(context.Background(), telemetry.Frame{
		RobotID: robotID, Namespace: ns, Timestamp: time.Now(), BatteryPct: &pct,
	}); err != nil {
		t.Fatalf("ingest frame: %v", err)
	}
}
