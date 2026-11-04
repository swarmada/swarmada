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

// A robot that falls silent behind an adapter that keeps heartbeating.
//
// status.connectivity.lastSeenAt measures the ADAPTER's presence, not the robot's: it is
// stamped for every robot the adapter serves (RFC-0001
// #control-plane-robot-health-and-connectivity, "Connectivity fencing"). So on its own it
// cannot tell a silent robot from a live one. The per-robot last-frame clock can: the
// telemetry Router records when each robot's own frame last arrived, in memory, and the
// Robot reconciler marks a robot Offline once it has been silent past its threshold, and
// back once it talks again. The phase is written on those two edges only (RA-1).
//
// Fake client, not envtest: projectLiveness lists Robots through the spec.adapter.name
// field index, which only a cache (or the fake client's WithIndex) can serve. The
// liveness fan-out is part of the scenario, so it has to run for real.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// countingProber is a LivenessProber that never gets an answer and counts how often the
// Robot reconciler's confirm step asked.
type countingProber struct{ calls int }

func (p *countingProber) Heartbeat(context.Context, string, string) (bool, error) {
	p.calls++
	return false, nil
}

// silentFleet is one dispatch-ready adapter managing two Idle robots, both last seen at
// `seen`, over a fake client carrying the index projectLiveness needs. Robot status
// patches are counted per robot in patches, when it is non-nil.
func silentFleet(t *testing.T, seen time.Time, patches ...map[string]int) client.Client {
	t.Helper()
	scheme := livenessScheme(t)
	robot := func(name string) *fleetv1.Robot {
		r := robotInPhase(name, fleetv1.RobotPhaseIdle, "")
		r.Annotations = map[string]string{RobotIDAnnotation: "rid-" + name}
		r.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &metav1.Time{Time: seen}}
		return r
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(readyActionAdapter(), robot("talker"), robot("silent")).
		WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}, &fleetv1.FleetAction{}).
		WithIndex(&fleetv1.Robot{}, robotAdapterNameField, func(o client.Object) []string {
			if n := o.(*fleetv1.Robot).Spec.Adapter.Name; n != "" {
				return []string{n}
			}
			return nil
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*fleetv1.Robot); ok && len(patches) > 0 {
					patches[0][obj.GetName()]++
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
}

func lastSeen(t *testing.T, c client.Client, name string) time.Time {
	t.Helper()
	conn := getRobot(t, c, name, actionNS).Status.Connectivity
	if conn == nil || conn.LastSeenAt == nil {
		t.Fatalf("%s: no lastSeenAt", name)
	}
	return conn.LastSeenAt.Time
}

// pinnedAction is a Normal navigate action that only the named robot may take (filter 6),
// so its dispatch outcome is a statement about that one robot.
func pinnedAction(name, robot string) *fleetv1.FleetAction {
	a := pendingInBand(name, fleetv1.ActionPriorityNormal)
	a.Spec.RobotSelector = robot
	return a
}

func reconcileRobot(t *testing.T, r *RobotReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: actionNS},
	})
	if err != nil {
		t.Fatalf("reconcile robot %s: %v", name, err)
	}
	return res
}

// THE FIX. One adapter, two robots. Both have streamed; from silenceStart the robot
// "silent" sends nothing at all, "talker" sends a frame on every step, and the adapter
// answers its periodic probe on every step, for four offline thresholds.
//
// The adapter's presence still restamps both robots' lastSeenAt in lock-step (that is what
// the field measures), but the silent robot's own clock runs out: it goes Offline without a
// confirm step, is not dispatched to, and returns once it talks again. Its phase is written
// once on each edge and never per frame.
func TestSilentRobotBehindLiveAdapter_GoesOfflineAndReturnsWhenItTalks(t *testing.T) {
	const (
		beat    = 5 * time.Second // half the CRD-default heartbeatIntervalSeconds (ADR-0054 beatPeriod)
		silence = 4 * defaultHeartbeatTimeout
	)
	// Whole seconds: metav1.Time truncates to the second, so an unaligned start would add
	// a sub-second jitter to every staleness figure below.
	realNow := time.Now().Truncate(time.Second)
	silenceStart := realNow.Add(-silence)
	patches := map[string]int{}
	c := silentFleet(t, silenceStart, patches)

	// Two clocks: the adapter's, and the frame clock's, which the warm-up below rewinds.
	clock, frameClock := silenceStart, silenceStart
	far := &FleetAdapterReconciler{Client: c, Scheme: c.Scheme(), now: func() time.Time { return clock }}
	router := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&RobotStatusSink{Client: c}, telemetry.WithClock(func() time.Time { return frameClock }))
	frame := func(robot string, battery int32) {
		t.Helper()
		if err := router.Ingest(context.Background(), telemetry.Frame{
			RobotID: "rid-" + robot, Namespace: actionNS, BatteryPct: i32(battery),
		}); err != nil {
			t.Fatalf("%s telemetry: %v", robot, err)
		}
	}
	id := controlstream.TLSIdentity{AdapterName: testActionAdapter, Namespace: actionNS, Verified: true}

	// Both robots have streamed; then only the talker does.
	frame("talker", 90)
	frame("silent", 90)
	var silentStamps int
	prevSilent := lastSeen(t, c, "silent")
	for step := 1; !clock.Add(beat).After(realNow); step++ {
		clock = clock.Add(beat)
		frameClock = clock
		frame("talker", 90)
		far.AdapterHeartbeat(context.Background(), id)
		if s := lastSeen(t, c, "silent"); s.After(prevSilent) {
			silentStamps++
			prevSilent = s
		}
	}
	if silentStamps == 0 || !lastSeen(t, c, "silent").Equal(lastSeen(t, c, "talker")) {
		t.Fatalf("adapter presence restamped the silent robot %d times (silent %v, talker %v); the "+
			"scenario needs a live adapter", silentStamps, lastSeen(t, c, "silent"), lastSeen(t, c, "talker"))
	}

	prober := &countingProber{}
	rr := &RobotReconciler{Client: c, Scheme: c.Scheme(), Liveness: prober, Frames: router}
	// Warm-up inside the threshold, so the robot's first-reconcile status write is not
	// counted below: only the Offline edge may write.
	frameClock = silenceStart.Add(beat)
	reconcileRobot(t, rr, "silent")
	if p := getRobot(t, c, "silent", actionNS).Status.Phase; p != fleetv1.RobotPhaseIdle {
		t.Fatalf("warm-up phase = %s %s into the silence, want Idle", p, beat)
	}
	frameClock = realNow
	before := patches["silent"]
	const reconciles = 4
	for i := 0; i < reconciles; i++ {
		reconcileRobot(t, rr, "silent")
		reconcileRobot(t, rr, "talker")
	}
	got := getRobot(t, c, "silent", actionNS)
	t.Logf("silence %s behind a live adapter; silent robot phase=%s after %d reconciles, %d status patch(es)",
		silence, got.Status.Phase, reconciles, patches["silent"]-before)
	if got.Status.Phase != fleetv1.RobotPhaseOffline {
		t.Fatalf("silent robot phase = %s after %s of silence behind a live adapter, want Offline",
			got.Status.Phase, silence)
	}
	if cond := findCondition(got, conditionTypeReady); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "RobotSilent" {
		t.Fatalf("silent robot Ready = %+v, want False/RobotSilent", cond)
	}
	if n := patches["silent"] - before; n != 1 {
		t.Fatalf("%d status patches on the silent robot over %d reconciles, want exactly 1 (the Offline edge)", n, reconciles)
	}
	if prober.calls != 0 {
		t.Fatalf("confirm step asked %d times; a silent robot is not confirmed through its live adapter", prober.calls)
	}
	if p := getRobot(t, c, "talker", actionNS).Status.Phase; p != fleetv1.RobotPhaseIdle {
		t.Fatalf("talker phase = %s, want Idle: it never stopped talking", p)
	}

	// Dispatch excludes it.
	if err := c.Create(context.Background(), pinnedAction("work", "silent")); err != nil {
		t.Fatalf("create action: %v", err)
	}
	ar := &FleetActionReconciler{Client: c, Scheme: c.Scheme(), Scheduler: scheduler.NewDefaultScheduler()}
	reconcileAction(t, ar, "work")
	if act := getAction(t, c, "work"); act.Status.AssignedRobot != "" {
		t.Fatalf("the silent robot was dispatched %q", act.Name)
	}

	// It talks again: back on the next reconcile, one write, however many frames it sends.
	before = patches["silent"]
	const frames = 10
	for i := 0; i < frames; i++ {
		frame("silent", 90)
	}
	if n := patches["silent"] - before; n != 0 {
		t.Fatalf("%d status patches from %d unchanged frames; the clock must not write per frame", n, frames)
	}
	before = patches["silent"]
	for i := 0; i < reconciles; i++ {
		reconcileRobot(t, rr, "silent")
	}
	if p := getRobot(t, c, "silent", actionNS).Status.Phase; p != fleetv1.RobotPhaseIdle {
		t.Fatalf("phase = %s after the robot talked again, want Idle", p)
	}
	if n := patches["silent"] - before; n != 1 {
		t.Fatalf("%d status patches over %d reconciles on return, want exactly 1 (the return edge)", n, reconciles)
	}
	reconcileAction(t, ar, "work")
	if act := getAction(t, c, "work"); act.Status.AssignedRobot != "silent" {
		t.Fatalf("action assigned to %q after the robot returned, want silent", act.Status.AssignedRobot)
	}
}

// A robot never heard from and carrying no telemetry-owned status is not judged by the
// clock: an adapter that sends no telemetry leaves Offline to adapter presence.
func TestSilentRobot_NeverStreamedIsLeftToAdapterPresence(t *testing.T) {
	c := silentFleet(t, time.Now())
	clock := time.Now().Add(-time.Hour)
	router := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&RobotStatusSink{Client: c}, telemetry.WithClock(func() time.Time { return clock }))
	clock = clock.Add(time.Hour)
	rr := &RobotReconciler{Client: c, Scheme: c.Scheme(), Frames: router}
	reconcileRobot(t, rr, "silent")
	if p := getRobot(t, c, "silent", actionNS).Status.Phase; p != fleetv1.RobotPhaseIdle {
		t.Fatalf("phase = %s for a robot that never streamed, want Idle", p)
	}
}

// After a restart the clock has heard nothing yet. A robot whose status shows it streamed
// before is timed from the clock's start, so its silence is not forgiven by a restart.
func TestSilentRobot_StreamedBeforeRestartIsTimedFromTheClocksStart(t *testing.T) {
	c := silentFleet(t, time.Now())
	rob := getRobot(t, c, "silent", actionNS)
	base := rob.DeepCopy()
	rob.Status.BatteryPercent = i32(80)
	if err := c.Status().Patch(context.Background(), rob, client.MergeFrom(base)); err != nil {
		t.Fatalf("seed battery: %v", err)
	}
	clock := time.Now()
	router := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&RobotStatusSink{Client: c}, telemetry.WithClock(func() time.Time { return clock }))
	rr := &RobotReconciler{Client: c, Scheme: c.Scheme(), Frames: router}

	reconcileRobot(t, rr, "silent")
	if p := getRobot(t, c, "silent", actionNS).Status.Phase; p != fleetv1.RobotPhaseIdle {
		t.Fatalf("phase = %s just after the restart, want Idle", p)
	}
	clock = clock.Add(2 * defaultHeartbeatTimeout)
	reconcileRobot(t, rr, "silent")
	if p := getRobot(t, c, "silent", actionNS).Status.Phase; p != fleetv1.RobotPhaseOffline {
		t.Fatalf("phase = %s after %s with no frame since the restart, want Offline", p, 2*defaultHeartbeatTimeout)
	}
}

// The threshold is the longer of the namespace Offline threshold and three of the robot's
// telemetry intervals.
func TestSilentRobot_ThresholdIsThreeIntervalsOrTheOfflineThreshold(t *testing.T) {
	clock := &fixedClock{}
	rr := &RobotReconciler{Frames: clock}
	robot := func(interval int32) *fleetv1.Robot {
		r := robotInPhase("r", fleetv1.RobotPhaseIdle, "")
		r.Annotations = map[string]string{RobotIDAnnotation: "rid"}
		r.Status.BatteryPercent = i32(50)
		if interval > 0 {
			r.Spec.TelemetryIntervalSeconds = &interval
		}
		return r
	}
	for _, tc := range []struct {
		interval int32
		silence  time.Duration
		want     bool
	}{
		{0, 30 * time.Second, false}, {0, 31 * time.Second, true}, // no interval: the 30s threshold
		{5, 31 * time.Second, true},                                 // 3×5s is under 30s: 30s still rules
		{30, 89 * time.Second, false}, {30, 91 * time.Second, true}, // 3×30s = 90s
	} {
		clock.silence = tc.silence
		_, threshold, silent := rr.robotSilence(robot(tc.interval), defaultHeartbeatTimeout)
		if silent != tc.want {
			t.Errorf("interval=%ds silence=%s: silent=%v (threshold %s), want %v",
				tc.interval, tc.silence, silent, threshold, tc.want)
		}
	}
}

type fixedClock struct{ silence time.Duration }

func (f *fixedClock) Silence(string, string) (time.Duration, bool) { return f.silence, true }

// NEGATIVE CONTROL. The same fleet, but the ADAPTER goes quiet at silenceStart. Nothing
// restamps either robot, the confirm step runs its three unanswered attempts, the robot goes
// Offline on the third, and the pinned action is not dispatched. This is what the silent
// robot above would have needed from the Offline clock and did not get.
func TestAdapterSilence_RobotGoesOfflineAfterConfirmStepAndIsNotDispatched(t *testing.T) {
	silence := 4 * defaultHeartbeatTimeout
	c := silentFleet(t, time.Now().Add(-silence))

	prober := &countingProber{}
	rr := &RobotReconciler{Client: c, Scheme: c.Scheme(), Liveness: prober}
	for attempt := 1; attempt < heartbeatConfirmAttempts; attempt++ {
		res := reconcileRobot(t, rr, "silent")
		if p := getRobot(t, c, "silent", actionNS).Status.Phase; p == fleetv1.RobotPhaseOffline {
			t.Fatalf("Offline on attempt %d; the confirm step should hold until attempt %d",
				attempt, heartbeatConfirmAttempts)
		}
		if res.RequeueAfter != heartbeatConfirmInterval {
			t.Fatalf("attempt %d requeued after %s, want %s", attempt, res.RequeueAfter, heartbeatConfirmInterval)
		}
	}
	reconcileRobot(t, rr, "silent")
	got := getRobot(t, c, "silent", actionNS)
	if got.Status.Phase != fleetv1.RobotPhaseOffline {
		t.Fatalf("phase = %s after %d unanswered attempts, want Offline", got.Status.Phase, prober.calls)
	}
	if prober.calls != heartbeatConfirmAttempts {
		t.Fatalf("confirm step asked %d times, want %d", prober.calls, heartbeatConfirmAttempts)
	}
	t.Logf("Offline after %d unanswered confirmations %s apart: earliest Offline is threshold (%s) "+
		"+ %s of confirmation", prober.calls, heartbeatConfirmInterval, defaultHeartbeatTimeout,
		time.Duration(heartbeatConfirmAttempts-1)*heartbeatConfirmInterval)

	if err := c.Create(context.Background(), pinnedAction("work", "silent")); err != nil {
		t.Fatalf("create action: %v", err)
	}
	ar := &FleetActionReconciler{Client: c, Scheme: c.Scheme(), Scheduler: scheduler.NewDefaultScheduler()}
	reconcileAction(t, ar, "work")
	if act := getAction(t, c, "work"); act.Status.AssignedRobot != "" {
		t.Fatalf("an Offline robot was dispatched %q", act.Name)
	}
}
