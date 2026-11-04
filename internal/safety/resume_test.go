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

package safety

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// The post-clear supervised window (RFC-0001 #safety-estop-state-machine).
//
// The state machine is Stopped -> Resuming -> Normal, and the two edges have
// different authors: the control plane authors Resuming on Robot.status.estopState
// when an authorized estop-clear is accepted, and leaves it only when an operator
// confirms the release — refused while the robot's own post-clear reports show it is
// not ready. Nothing releases it automatically.
//
// Until this file existed the implementation wrote Normal directly and RobotEstopResuming
// was produced by nothing in the tree: an operator clearing an estop returned the
// robot to the scheduler's candidate set in the same instant, with no interval in
// which capabilities could restore. These tests hold both edges, and — more
// importantly — hold that the SECOND one is reached only on an operator's request that
// the evidence does not refuse.

// testFrames is the robot's own telemetry under test control: when each robot's
// frame was first and last received, and the phase it last reported.
type testFrames struct {
	mu     sync.Mutex
	at     map[string]time.Time
	first  map[string]time.Time
	phases map[string]fleetv1.RobotPhase
	// unwritten marks a robot whose latest frame's status write has not landed.
	unwritten map[string]bool
}

func (f *testFrames) LastFrame(namespace, robotID string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.at[namespace+"/"+robotID]
	return at, ok
}

func (f *testFrames) FirstFrame(namespace, robotID string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.first[namespace+"/"+robotID]
	return at, ok
}

func (f *testFrames) LastReportedPhase(namespace, robotID string) (fleetv1.RobotPhase, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.phases[namespace+"/"+robotID]
	return p, ok
}

func (f *testFrames) LastReportUnwritten(namespace, robotID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unwritten[namespace+"/"+robotID]
}

// reports records that the robot's own latest frame reported phase.
func (f *testFrames) reports(namespace, robotID string, phase fleetv1.RobotPhase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.phases == nil {
		f.phases = map[string]fleetv1.RobotPhase{}
	}
	f.phases[namespace+"/"+robotID] = phase
}

func (f *testFrames) heard(namespace, robotID string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at == nil {
		f.at, f.first = map[string]time.Time{}, map[string]time.Time{}
	}
	if _, ok := f.first[namespace+"/"+robotID]; !ok {
		f.first[namespace+"/"+robotID] = at
	}
	f.at[namespace+"/"+robotID] = at
}

// projected writes the telemetry projection a robot would carry — its phase and its
// capabilities — and records that the robot's frame carrying it was received at
// seenAt. An operator's confirmation is refused until such a frame has arrived since
// the clear, so every test that expects a release goes through it.
func projected(t *testing.T, d *Dispatcher, c client.Client, seenAt time.Time, phase fleetv1.RobotPhase,
	caps ...fleetv1.CapabilityStatusEntry) {
	t.Helper()
	// Read and written again on a conflict: the stop's record lands beside it, as on an API server.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		robot := &fleetv1.Robot{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
			return err
		}
		robot.Status.Phase = phase
		robot.Status.Capabilities = caps
		return c.Status().Update(context.Background(), robot)
	}); err != nil {
		t.Fatal(err)
	}
	d.Telemetry.(*testFrames).heard(ns, "amr-1", seenAt)
}

func activeCap(name string) fleetv1.CapabilityStatusEntry {
	return fleetv1.CapabilityStatusEntry{Name: name, Status: fleetv1.CapabilityStatusActive}
}

// The whole specified path, driven end to end: a robot under a CONFIRMED stop, an
// operator clear, the supervised window, and only then Normal — reached on an
// operator's confirmation, which telemetry arriving AFTER the clear did not refuse.
func TestResume_StoppedToResumingToNormal(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), stoppedSender(d))
	ctx := context.Background()
	clearedAt := time.Now()

	// The projection standing when the estop is raised: the robot looked perfectly
	// healthy a moment ago. This is the state a clear-straight-to-Normal would have
	// handed back to the scheduler.
	projected(t, d, c, clearedAt.Add(-time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))

	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s, want Stopped", got)
	}

	// ── the operator clear: Resuming, observably, BEFORE Normal ──────────────
	state, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{})
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop returned %s, want Resuming", state)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming: the clear must not release straight to Normal", got)
	}

	// The pre-estop projection is still standing and still reads Idle + Active. An
	// operator's confirmation must be refused on it: the checks read post-clear
	// evidence, and judging the reading the clear was issued on top of would judge
	// nothing.
	if got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume on the pre-clear projection = %s (err %v), want Resuming", got, err)
	}

	// ── subsequent telemetry, then the operator's confirmation ───────────────
	projected(t, d, c, clearedAt.Add(time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))

	if got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (err %v), want Normal", got, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s, want Normal after a confirmed resume", got)
	}
}

// An operator's confirmation is refused while the robot does not look ready. Each row
// is fresh telemetry — later than the clear, so freshness is satisfied and only the
// condition itself is under test — that fails one check. None of them may reach
// Normal, however many times the confirmation is asked for, and each says why.
func TestResume_UnmetConditionRefusesTheConfirmation(t *testing.T) {
	cases := []struct {
		name     string
		phase    fleetv1.RobotPhase // the control plane's own phase
		reported fleetv1.RobotPhase // the robot's own last report
		caps     []fleetv1.CapabilityStatusEntry
		refusal  string
	}{
		{"capability still degraded", fleetv1.RobotPhaseIdle, "",
			[]fleetv1.CapabilityStatusEntry{{Name: "lidar", Status: fleetv1.CapabilityStatusDegraded}}, "lidar"},
		{"capability unavailable", fleetv1.RobotPhaseIdle, "",
			[]fleetv1.CapabilityStatusEntry{activeCap("nav"), {Name: "lidar", Status: fleetv1.CapabilityStatusUnavailable}},
			"lidar"},
		{"capability paused", fleetv1.RobotPhaseIdle, "",
			[]fleetv1.CapabilityStatusEntry{{Name: "lift", Status: fleetv1.CapabilityStatusPaused}}, "lift"},
		{"robot reports Error", fleetv1.RobotPhaseIdle, fleetv1.RobotPhaseError,
			[]fleetv1.CapabilityStatusEntry{activeCap("lidar")}, "most recent report puts it in Error"},
		{"robot reports Offline", fleetv1.RobotPhaseIdle, fleetv1.RobotPhaseOffline,
			[]fleetv1.CapabilityStatusEntry{activeCap("lidar")}, "most recent report puts it in Offline"},
		{"control plane marks Offline", fleetv1.RobotPhaseOffline, fleetv1.RobotPhaseIdle,
			[]fleetv1.CapabilityStatusEntry{activeCap("lidar")}, "marked the robot Offline"},
		{"control plane marks Error", fleetv1.RobotPhaseError, fleetv1.RobotPhaseIdle,
			[]fleetv1.CapabilityStatusEntry{activeCap("lidar")}, "marked the robot Error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, c, _ := newDispatcher(t, "acme")
			ctx := context.Background()
			clearedAt := time.Now()

			setState(t, c, fleetv1.RobotEstopStopped)
			projected(t, d, c, clearedAt, fleetv1.RobotPhaseIdle, activeCap("lidar"))
			if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
				t.Fatalf("ClearEstop: %v", err)
			}

			projected(t, d, c, clearedAt.Add(time.Minute), tc.phase, tc.caps...)
			if tc.reported != "" {
				d.Telemetry.(*testFrames).reports(ns, "amr-1", tc.reported)
			}
			for i := 0; i < 5; i++ {
				got, why, err := d.ConfirmResume(ctx, ns, "amr-1", "admin")
				if err != nil {
					t.Fatalf("ConfirmResume: %v", err)
				}
				if got != fleetv1.RobotEstopResuming {
					t.Fatalf("attempt %d: ConfirmResume = %s, want Resuming", i, got)
				}
				if !strings.Contains(why, tc.refusal) {
					t.Fatalf("attempt %d: refusal %q does not name %q", i, why, tc.refusal)
				}
			}
			if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
				t.Fatalf("estopState = %s, want Resuming", got)
			}
		})
	}
}

// The control plane's own phase is not evidence about the robot, and a robot paused
// by a maintenance window sits in Maintenance for as long as the window is open. A
// confirmation for such a robot — and for one Charging or Assigned — is judged on the
// robot's own report and its capabilities, not refused for the phase. Refusing it would
// leave a maintenance window, whose close waits for the robot's estop to be Normal,
// and the robot, whose release would wait for the window, each waiting on the other.
func TestResume_ControlPlanePhaseAloneDoesNotRefuse(t *testing.T) {
	for _, phase := range []fleetv1.RobotPhase{
		fleetv1.RobotPhaseMaintenance, fleetv1.RobotPhaseCharging, fleetv1.RobotPhaseAssigned,
	} {
		t.Run(string(phase), func(t *testing.T) {
			d, c, _ := newDispatcher(t, "acme")
			ctx := context.Background()
			clearedAt := time.Now()
			setState(t, c, fleetv1.RobotEstopStopped)
			projected(t, d, c, clearedAt, fleetv1.RobotPhaseIdle, activeCap("lidar"))
			if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
				t.Fatalf("ClearEstop: %v", err)
			}
			projected(t, d, c, clearedAt.Add(time.Minute), phase, activeCap("lidar"))
			if got, why, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopNormal {
				t.Fatalf("ConfirmResume with phase %s = %s (%q, err %v), want Normal", phase, got, why, err)
			}
		})
	}
}

// No timer releases the window. RFC-0001 specifies no deadline for Resuming, and
// the confirmed-only discipline that governs the stop direction — a stop that
// cannot be confirmed resolves to Failed, never to a false Stopped — governs the
// release direction too. A robot whose adapter goes quiet after the clear stays in
// the window indefinitely: it is a robot an operator must look at, not one that
// ages back into service.
func TestResume_NoSubsequentTelemetryNeverReleases(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	ctx := context.Background()
	clearedAt := time.Now()

	setState(t, c, fleetv1.RobotEstopStopped)
	projected(t, d, c, clearedAt, fleetv1.RobotPhaseIdle, activeCap("lidar"))
	if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}

	// The clock moves; the robot's projection does not. Confirmation is driven far
	// more often than any plausible timeout would fire.
	d.now = func() time.Time { return clearedAt.Add(24 * time.Hour) }
	for i := 0; i < 50; i++ {
		got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin")
		if err != nil {
			t.Fatalf("ConfirmResume: %v", err)
		}
		if got != fleetv1.RobotEstopResuming {
			t.Fatalf("attempt %d: ConfirmResume = %s, want Resuming; the window must not expire into Normal", i, got)
		}
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming after 24h of silence", got)
	}
}

// A hazard raised during the window wins, and the resume that was in flight cannot
// undo it. RFC-0001 #safety-estop-state-machine: an estop arriving in RESUMING
// re-enters STOPPING and proceeds to STOPPED on confirmation — "re-enable never
// overrides a hazard raised during re-enable: the later estop wins".
func TestResume_FreshEstopDuringResumingWins(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), stoppedSender(d))
	ctx := context.Background()
	clearedAt := time.Now()

	setState(t, c, fleetv1.RobotEstopStopped)
	projected(t, d, c, clearedAt, fleetv1.RobotPhaseIdle, activeCap("lidar"))
	if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming", got)
	}

	// Telemetry that WOULD satisfy the exit lands, and a fresh hazard lands after it.
	projected(t, d, c, clearedAt.Add(time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))
	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard during re-enable", "operator",
		metrics.ScopeRobot, Carrier{}); err != nil || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}

	// The resume must not resurrect the cleared window over a live, confirmed stop.
	if got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopStopped {
		t.Fatalf("ConfirmResume = %s (err %v), want Stopped", got, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s, want Stopped: a hazard raised during re-enable must win", got)
	}
}

// The epoch gate covers Resuming, not just Normal. A clear issued BEFORE a trigger
// must not write Resuming over a CONFIRMED Stopped: RFC-0001
// #safety-estop-state-machine is categorical that "a confirmed stop is never
// discarded", and Resuming is outside candidate filter 10 — so an ungated Resuming
// write would both erase the confirmed stop and quietly move the robot out of the
// state the filter excludes.
func TestResume_SupersededClearDoesNotWriteResumingOverAConfirmedStop(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopStopped)
	gated := &gatedGetClient{Client: c, entered: make(chan struct{}), release: make(chan struct{})}
	d.Client = gated
	d.RegisterStream(identity("acme"), stoppedSender(d))

	done := make(chan struct{})
	go func() {
		defer close(done)
		heldCtx := context.WithValue(context.Background(), holdGetKey{}, true)
		if _, err := d.ClearEstop(heldCtx, ns, "amr-1", "admin", Ends{}); err != nil {
			t.Errorf("ClearEstop: %v", err)
		}
	}()
	<-gated.entered

	if res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator",
		metrics.ScopeRobot, Carrier{}); err != nil || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}

	close(gated.release)
	<-done

	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s, want Stopped: a clear issued before the trigger wrote Resuming over a confirmed stop", got)
	}
}

// A leader that adopts a robot already in the window has no in-memory baseline. It
// must not treat "I have no record" as "nothing to wait for": it re-arms, with the
// first frame it received from the robot as the baseline, and demands a frame received
// LATER, so a failover asks for more evidence than the original clear did, never less
// (resume_failover_baseline_test.go holds the orderings).
func TestResume_FailoverRearmsRatherThanReleasing(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	ctx := context.Background()
	adoptedAt := time.Now()

	// A robot persisted mid-window by a previous leader, whose projection already
	// satisfies the CONDITION. Only freshness stands between it and Normal.
	setState(t, c, fleetv1.RobotEstopResuming)
	projected(t, d, c, adoptedAt, fleetv1.RobotPhaseIdle, activeCap("lidar"))

	fresh := New(c, nil) // a new leader: empty ordering map, no resume baseline
	fresh.DeriveCapabilities = testDerivation
	fresh.Telemetry = d.Telemetry // the same robot's frames, seen by the new leader

	if got, _, err := fresh.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopResuming {
		t.Fatalf("first ConfirmResume after failover = %s (err %v), want Resuming", got, err)
	}
	// Still the same frame: adoption is not evidence.
	if got, _, err := fresh.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopResuming {
		t.Fatalf("second ConfirmResume on an unchanged stamp = %s (err %v), want Resuming", got, err)
	}

	// A frame later than the first one the new leader received does release it.
	projected(t, fresh, c, adoptedAt.Add(time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))
	if got, _, err := fresh.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume on fresh telemetry after failover = %s (err %v), want Normal", got, err)
	}
}

// A confirmation for a robot that is not in the window is a no-op that reports the
// state that stands — and it must not accrete an ordering record per robot.
func TestResume_ConfirmIsANoOpOutsideTheWindow(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	ctx := context.Background()

	for _, state := range []fleetv1.RobotEstopState{
		"", fleetv1.RobotEstopNormal, fleetv1.RobotEstopStopping,
		fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed,
	} {
		setState(t, c, state)
		got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin")
		if err != nil {
			t.Fatalf("estopState %q: ConfirmResume: %v", state, err)
		}
		if got != state {
			t.Errorf("estopState %q: ConfirmResume returned %s, want the state that stands", state, got)
		}
		if now := estopState(t, c); now != state {
			t.Errorf("estopState %q: ConfirmResume changed it to %s; it must write nothing outside the window",
				state, now)
		}
	}
	if n := len(d.orders); n != 0 {
		t.Errorf("ConfirmResume created %d ordering record(s) for a robot it never estopped; "+
			"a read-only probe must not grow the map to fleet size", n)
	}
}

// Adapter presence is not the witness. status.connectivity.lastSeenAt is stamped for
// every robot an adapter serves whether or not the robot has sent anything, so a
// stamp later than the clear says the adapter is alive, not that the robot has
// reported its state since. With no frame from the robot the window holds, however
// fresh the stamp.
func TestResume_AdapterPresenceAloneNeverReleases(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	ctx := context.Background()
	clearedAt := time.Now()

	setState(t, c, fleetv1.RobotEstopStopped)
	projected(t, d, c, clearedAt.Add(-time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))
	if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}

	robot := &fleetv1.Robot{}
	if err := c.Get(ctx, client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatal(err)
	}
	stamp := metav1.NewTime(clearedAt.Add(time.Hour))
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &stamp}
	if err := c.Status().Update(ctx, robot); err != nil {
		t.Fatal(err)
	}

	if got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume on adapter presence alone = %s (err %v), want Resuming", got, err)
	}
}

// A dispatcher with no witness cannot see any frame, so it never releases. That is
// the fail-safe direction: a control plane wired without the telemetry plane
// withholds a cleared robot rather than releasing it on nothing.
func TestResume_NoWitnessNeverReleases(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	ctx := context.Background()
	clearedAt := time.Now()

	setState(t, c, fleetv1.RobotEstopStopped)
	if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	projected(t, d, c, clearedAt.Add(time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))
	d.Telemetry = nil

	if got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume with no witness = %s (err %v), want Resuming", got, err)
	}
}

// The refusals on a Failed component and on a capability that is not Active have no
// override. Each tells the operator what ends it: a repair, or removing the item from
// the robot's RobotClass and applying the class with `swarmctl robotclass rollout`.
func TestResume_StrictRefusalsNameTheirRemedy(t *testing.T) {
	robot := &fleetv1.Robot{Spec: fleetv1.RobotSpec{RobotClass: "acme-picker"}}
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "lidar", Status: fleetv1.HardwareFailed}}
	why, ok := resumeRefusal(robot, nil, "")
	if ok || !strings.Contains(why, "stays Resuming until the robot reports the component repaired") ||
		!strings.Contains(why, `RobotClass "acme-picker" (spec.hardware)`) ||
		!strings.Contains(why, "swarmctl robotclass rollout acme-picker") {
		t.Fatalf("hardware refusal %q does not say what ends it", why)
	}

	robot.Status.Hardware = nil
	why, ok = resumeRefusal(robot, []fleetv1.CapabilityStatusEntry{{Name: "navigation", Status: fleetv1.CapabilityStatusInactive}}, "")
	if ok || !strings.Contains(why, `RobotClass "acme-picker" (spec.baseCapabilities)`) ||
		!strings.Contains(why, "swarmctl robotclass rollout acme-picker") {
		t.Fatalf("capability refusal %q does not say what ends it", why)
	}
}
