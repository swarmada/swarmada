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

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// A leader that finds a robot in Resuming and did not accept its clear (RFC-0001
// #safety-estop-state-machine): "That leader MUST take, as the robot's post-clear baseline,
// the first projection it observes. It MUST refuse the operator release (row S6) until
// telemetry strictly later than that baseline shows the robot ready."
//
// The new leader's frames are its own testFrames: a process is served adapter streams only
// while it leads, so every frame it has received arrived after its takeover. The robot's
// status is the predecessor's projection, which already satisfies the release condition, so
// only the baseline stands between the robot and Normal.

// failoverClock is a test clock a dispatcher reads through its now hook.
type failoverClock struct{ at time.Time }

func (c *failoverClock) now() time.Time { return c.at }

// newLeaderOverResuming returns a robot left in Resuming by a predecessor, and a new leader
// over it that holds no record of the clear and has received no frame yet.
func newLeaderOverResuming(t *testing.T) (*Dispatcher, *failoverClock, time.Time) {
	t.Helper()
	pred, c, _ := newDispatcher(t, "acme")
	t0 := time.Now()
	setState(t, c, fleetv1.RobotEstopResuming)
	projected(t, pred, c, t0.Add(-time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))

	clk := &failoverClock{at: t0}
	fresh := New(c, nil)
	fresh.DeriveCapabilities = testDerivation
	fresh.Telemetry = &testFrames{}
	fresh.now = clk.now
	return fresh, clk, t0
}

// heardAt records one frame of the robot's on the new leader, carrying the projection
// that already stands.
func heardAt(d *Dispatcher, at time.Time) {
	d.Telemetry.(*testFrames).heard(ns, "amr-1", at)
}

// stopSettled waits until the stop a trigger sent is recorded on the robot and its recorder has
// returned. The record is written after the send, in its own goroutine; a clear that lands
// before it is honoured, but under load the release can find the stop still in flight and
// refuse, which is not what these tests are about.
func stopSettled(t *testing.T, d *Dispatcher, c client.Client) {
	t.Helper()
	key := robotEstopKey(ns, "amr-1")
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		d.mu.Lock()
		inflight := len(d.inflight[key])
		d.mu.Unlock()
		if inflight == 0 && len(stored(t, c).Status.Stops) == 1 {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("the stop is not settled: %d in flight, %d recorded; want 0 and 1", inflight, len(stored(t, c).Status.Stops))
		}
	}
}

// listedReader is the dispatcher's reader, with listed closed once a Robot list has returned.
type listedReader struct {
	client.Reader
	once   sync.Once
	listed chan struct{}
}

func (r *listedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := r.Reader.List(ctx, list, opts...)
	if _, ok := list.(*fleetv1.RobotList); ok {
		r.once.Do(func() { close(r.listed) })
	}
	return err
}

// registerSettled registers a stream that confirms every estop, and waits until the re-send pass
// the registration starts (resendHeld) has listed the robots. A trigger after that list is not
// read by the pass. A trigger before it can be read between its estopState write and its
// record; the pass then sends and records a second stop of its own, which is not what these
// tests are about.
func registerSettled(t *testing.T, d *Dispatcher) {
	t.Helper()
	r := &listedReader{Reader: d.Client, listed: make(chan struct{})}
	if d.APIReader != nil {
		r.Reader = d.APIReader
	}
	d.APIReader = r
	d.RegisterStream(identity("acme"), stoppedSender(d))
	select {
	case <-r.listed:
	case <-time.After(3 * time.Second):
		t.Fatal("the new stream's re-send pass has not listed the robots")
	}
}

func confirmWant(t *testing.T, d *Dispatcher, want fleetv1.RobotEstopState, step string) string {
	t.Helper()
	got, why, err := d.ConfirmResume(context.Background(), ns, "amr-1", "admin")
	if err != nil || got != want {
		t.Fatalf("%s: ConfirmResume = %s (%q, err %v), want %s", step, got, why, err, want)
	}
	return why
}

// The early release. The release request comes before any frame: the first frame after it is
// the baseline, so it does not release the robot. A frame later than it does.
func TestResumeFailover_RequestBeforeAnyFrameNeedsTwoFrames(t *testing.T) {
	d, clk, t0 := newLeaderOverResuming(t)

	clk.at = t0.Add(1 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "the request that adopts the robot")
	clk.at = t0.Add(2 * time.Second)
	if why := confirmWant(t, d, fleetv1.RobotEstopResuming, "a request before any frame"); !strings.Contains(why, "took over") {
		t.Errorf("a request before any frame is refused with %q; want it to say no frame has arrived since "+
			"this leader took over", why)
	}

	heardAt(d, t0.Add(3*time.Second))
	clk.at = t0.Add(4 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming,
		"the first frame after takeover is the baseline, not telemetry later than it")

	heardAt(d, t0.Add(5*time.Second))
	clk.at = t0.Add(6 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopNormal, "a frame strictly later than the first")
}

// The same early release on the other path to a missing baseline: the new leader holds an
// ordering record for the robot, made by a clear that wrote nothing because the robot was
// already Resuming. That clear is not one this leader accepted.
func TestResumeFailover_NoOpClearOnTheNewLeaderIsNotItsClear(t *testing.T) {
	d, clk, t0 := newLeaderOverResuming(t)

	if got, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{}); err != nil ||
		got != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop on a Resuming robot = %s (err %v), want Resuming", got, err)
	}
	if d.existingOrder(robotEstopKey(ns, "amr-1")) == nil {
		t.Fatal("the no-op clear left no ordering record; this test then covers nothing the first does")
	}

	clk.at = t0.Add(1 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "the request that adopts the robot")
	heardAt(d, t0.Add(2*time.Second))
	clk.at = t0.Add(3 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "the first frame after takeover is the baseline")
	heardAt(d, t0.Add(4*time.Second))
	clk.at = t0.Add(5 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopNormal, "a frame strictly later than the first")
}

// Two frames before the first request. The baseline is the first of them, and the second is
// strictly later, so the request after the one that adopts the robot releases it. The request
// that adopts it never does. (A baseline at the adopting request's clock would need a third
// frame.)
func TestResumeFailover_TwoFramesBeforeTheRequest(t *testing.T) {
	d, clk, t0 := newLeaderOverResuming(t)

	heardAt(d, t0.Add(1*time.Second))
	heardAt(d, t0.Add(2*time.Second))
	clk.at = t0.Add(3 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "the request that adopts the robot")
	confirmWant(t, d, fleetv1.RobotEstopNormal, "the second frame is strictly later than the first")
}

// Guard (the ordering in which the code was already as strict as the text): one frame before
// the request is the baseline, and is not evidence. A later frame releases.
func TestResumeFailover_OneFrameBeforeTheRequestNeedsALaterOne(t *testing.T) {
	d, clk, t0 := newLeaderOverResuming(t)

	heardAt(d, t0.Add(1*time.Second))
	clk.at = t0.Add(2 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "the request that adopts the robot")
	confirmWant(t, d, fleetv1.RobotEstopResuming, "no frame since the first")

	heardAt(d, t0.Add(3*time.Second))
	clk.at = t0.Add(4 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopNormal, "a frame strictly later than the first")
}

// Guard: with no frame on the new leader the predecessor's projection, however ready it
// looks, never releases the robot.
func TestResumeFailover_NoFrameNeverReleases(t *testing.T) {
	d, clk, t0 := newLeaderOverResuming(t)

	for i := 1; i <= 3; i++ {
		clk.at = t0.Add(time.Duration(i) * time.Second)
		confirmWant(t, d, fleetv1.RobotEstopResuming, "a request with no frame since takeover")
	}
}

// Guard (a leader that never changed): the leader that accepts the clear keeps the clear as
// the baseline. One frame after the clear releases the robot, with no frame before it.
func TestResumeFailover_OwnClearKeepsTheClearAsBaseline(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	registerSettled(t, d)
	t0 := time.Now()
	clk := &failoverClock{at: t0}
	d.now = clk.now
	ctx := context.Background()

	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	stopSettled(t, d, c)
	clk.at = t0.Add(1 * time.Second)
	if got, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil || got != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", got, err)
	}
	clk.at = t0.Add(2 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "no frame since the clear")

	projected(t, d, c, t0.Add(3*time.Second), fleetv1.RobotPhaseIdle, activeCap("lidar"))
	clk.at = t0.Add(4 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopNormal, "the one frame after this leader's own clear")
}

// Guard: a clear the new leader accepts after adopting the robot replaces the adopted
// baseline. Frames from before that clear are not telemetry since it.
func TestResumeFailover_LaterOwnClearReplacesTheAdoptedBaseline(t *testing.T) {
	d, clk, t0 := newLeaderOverResuming(t)
	registerSettled(t, d)
	ctx := context.Background()

	clk.at = t0.Add(1 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "the request that adopts the robot")
	heardAt(d, t0.Add(2*time.Second))
	heardAt(d, t0.Add(3*time.Second))

	clk.at = t0.Add(4 * time.Second)
	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	stopSettled(t, d, d.Client)
	clk.at = t0.Add(5 * time.Second)
	if got, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil || got != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", got, err)
	}
	clk.at = t0.Add(6 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopResuming, "frames from before this leader's own clear")

	heardAt(d, t0.Add(7*time.Second))
	clk.at = t0.Add(8 * time.Second)
	confirmWant(t, d, fleetv1.RobotEstopNormal, "a frame after this leader's own clear")
}
