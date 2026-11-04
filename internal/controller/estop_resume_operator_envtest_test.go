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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// Release from the post-clear window is operator-confirmed only (RFC-0001
// #safety-estop-state-machine). Telemetry, however healthy and however fresh, never
// releases a robot on its own; an operator's request does, once, and only while the
// robot's own latest report shows nothing wrong.
//
// The request annotation is spelled out rather than taken from the API constant: an
// operator writing it with kubectl uses this spelling, so the test pins it.
const (
	resumeRequestKey   = "swarmada.io/estop-resume"
	resumeProcessedKey = "swarmada.io/estop-resume-processed"
)

// resumeFixture is a robot cleared into Resuming against a real API server, with the
// real telemetry router as the dispatcher's view of the robot's own reports.
type resumeFixture struct {
	ns       string
	frames   *telemetry.Router
	disp     *safety.Dispatcher
	estops   *RobotEstopReconciler
	recorder *record.FakeRecorder
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	ctx := context.Background()
	ns := envtestNamespace(t)
	f := &resumeFixture{ns: ns, frames: resumeWitness(), recorder: record.NewFakeRecorder(16)}
	f.disp = safety.New(envK8s, nil)
	f.disp.Telemetry = f.frames
	f.disp.DeriveCapabilities = DeriveCapabilities
	f.estops = &RobotEstopReconciler{Client: envK8s, Scheme: envScheme, Resumer: f.disp}
	setResumeRecorder(f.estops, f.recorder)

	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
	if state, err := f.disp.ClearEstop(ctx, ns, "amr-1", "alice", safety.Ends{}); err != nil || state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", state, err)
	}
	return f
}

func (f *resumeFixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	out, err := f.estops.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: f.ns, Name: "amr-1"}})
	if err != nil {
		t.Fatalf("reconcile estop: %v", err)
	}
	return out
}

func (f *resumeFixture) request(t *testing.T, value string) {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: f.ns, Name: "amr-1"}, &robot); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	base := robot.DeepCopy()
	if robot.Annotations == nil {
		robot.Annotations = map[string]string{}
	}
	robot.Annotations[resumeRequestKey] = value
	if err := envK8s.Patch(context.Background(), &robot, client.MergeFrom(base)); err != nil {
		t.Fatalf("write resume request: %v", err)
	}
}

// Healthy telemetry received after the clear does not release the robot, however
// many times the controller looks at it, and the controller does not keep polling
// a robot nobody has asked to release.
func TestEnvtest_EstopResume_TelemetryAloneNeverReleases(t *testing.T) {
	requireEnvtest(t)
	f := newResumeFixture(t)

	for i := 0; i < 3; i++ {
		sendResumeFrame(t, f.frames, f.ns, "amr-1")
		out := f.reconcile(t)
		if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopResuming {
			t.Fatalf("frame %d: estopState = %s, want Resuming: healthy post-clear telemetry released the robot "+
				"with no operator confirmation", i, got)
		}
		if out.RequeueAfter != 0 {
			t.Fatalf("frame %d: RequeueAfter = %v, want 0: nothing but an operator's request can move a Resuming robot, "+
				"so there is nothing to poll for", i, out.RequeueAfter)
		}
	}
}

// An operator's request releases the robot once; the request is recorded as handled,
// the operator is told, and a later reconcile writes nothing (RA-1).
func TestEnvtest_EstopResume_OperatorRequestReleasesOnce(t *testing.T) {
	requireEnvtest(t)
	f := newResumeFixture(t)
	sendResumeFrame(t, f.frames, f.ns, "amr-1")

	f.request(t, "2026-09-27T20:00:00Z inspected")
	f.reconcile(t)
	if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s, want Normal after an operator's request on a healthy robot", got)
	}
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: f.ns, Name: "amr-1"}, &robot); err != nil {
		t.Fatal(err)
	}
	if robot.Annotations[resumeProcessedKey] != "2026-09-27T20:00:00Z inspected" {
		t.Fatalf("processed marker = %q, want the request it handled", robot.Annotations[resumeProcessedKey])
	}
	expectEvent(t, f.recorder, "Normal EstopResumeConfirmed")

	rv := robot.ResourceVersion
	f.reconcile(t)
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: f.ns, Name: "amr-1"}, &robot); err != nil {
		t.Fatal(err)
	}
	if robot.ResourceVersion != rv {
		t.Fatalf("resourceVersion %s -> %s: a handled request was acted on again", rv, robot.ResourceVersion)
	}
}

// A request is refused, with the reason, while the robot's own latest report puts it
// in Error. The refused request is closed: a later healthy report does not release the
// robot by itself; only a new request does.
func TestEnvtest_EstopResume_RefusedWhileTheRobotReportsError(t *testing.T) {
	requireEnvtest(t)
	f := newResumeFixture(t)
	if err := f.frames.Ingest(context.Background(), telemetry.Frame{
		RobotID: "amr-1", Namespace: f.ns, Timestamp: time.Now(), Phase: fleetv1.RobotPhaseError,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	f.request(t, "first")
	f.reconcile(t)
	if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming while the robot reports Error", got)
	}
	expectEvent(t, f.recorder, "Warning EstopResumeRefused", "Error")

	if err := f.frames.Ingest(context.Background(), telemetry.Frame{
		RobotID: "amr-1", Namespace: f.ns, Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	f.reconcile(t)
	if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming: a refused request must not release the robot later", got)
	}

	f.request(t, "second")
	f.reconcile(t)
	if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s, want Normal on a new request once the robot reports Idle", got)
	}
}

// A request is refused while a hardware component the robot reported is Failed.
func TestEnvtest_EstopResume_RefusedWhileHardwareIsFailed(t *testing.T) {
	requireEnvtest(t)
	f := newResumeFixture(t)
	if err := f.frames.Ingest(context.Background(), telemetry.Frame{
		RobotID: "amr-1", Namespace: f.ns, Timestamp: time.Now(),
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareFailed},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	f.request(t, "inspected")
	f.reconcile(t)
	if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming while lidar is reported Failed", got)
	}
	expectEvent(t, f.recorder, "Warning EstopResumeRefused", "lidar")
}

// A request written while the robot is not Resuming is handled at once and never
// releases a window entered later.
func TestEnvtest_EstopResume_StaleRequestDoesNotReleaseALaterWindow(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	f := newResumeFixture(t)
	sendResumeFrame(t, f.frames, f.ns, "amr-1")
	f.request(t, "early")
	f.reconcile(t)
	if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s, want Normal", got)
	}

	// A new stop and clear. The old request is still on the object, already handled.
	seedResumeStatus(t, f.ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
	if state, err := f.disp.ClearEstop(ctx, f.ns, "amr-1", "alice", safety.Ends{}); err != nil || state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", state, err)
	}
	sendResumeFrame(t, f.frames, f.ns, "amr-1")
	f.reconcile(t)
	if got := readEstopState(t, f.ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming: a request from before this window released it", got)
	}
}

// expectEvent asserts the next recorded Event starts with prefix and contains each of
// the given fragments.
func expectEvent(t *testing.T, r *record.FakeRecorder, prefix string, contains ...string) {
	t.Helper()
	select {
	case e := <-r.Events:
		if !strings.HasPrefix(e, prefix) {
			t.Fatalf("event %q, want prefix %q", e, prefix)
		}
		for _, c := range contains {
			if !strings.Contains(e, c) {
				t.Fatalf("event %q does not name %q", e, c)
			}
		}
	default:
		t.Fatalf("no event recorded; want %q", prefix)
	}
}
