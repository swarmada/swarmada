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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

type fenceSpy struct {
	mu       sync.Mutex
	fences   []string // robot/stop ID, every fence sent
	inForce  []string // robot/stop ID, the fences not ended by a ClearHeld
	cleared  []string // robots
	released []string // robots
}

func (f *fenceSpy) SendFence(robot *fleetv1.Robot, stopID string, _ *fav1.Estop) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fences = append(f.fences, robot.Name+"/"+stopID)
	f.inForce = append(f.inForce, robot.Name+"/"+stopID)
	return nil
}

func (f *fenceSpy) ClearHeld(_, robot string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, robot)
	var kept []string
	for _, fence := range f.inForce {
		if !strings.HasPrefix(fence, robot+"/") {
			kept = append(kept, fence)
		}
	}
	f.inForce = kept
}

func (f *fenceSpy) HeldStops(_, robot string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, fence := range f.inForce {
		if strings.HasPrefix(fence, robot+"/") {
			out = append(out, strings.TrimPrefix(fence, robot+"/"))
		}
	}
	return out
}

func (f *fenceSpy) RobotReleased(_, robot string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, robot)
}

func (f *fenceSpy) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fences...), append([]string(nil), f.cleared...)
}

// Row S1: the stop goes on both of the robot's streams, its fence on the ControlStream under the
// stop's own ID, even with no SafetyStream. A clear that returns the robot to Resuming ends the
// robot's stops in force, so work may follow the operator's resume.
func TestFence_TheStopIsFencedUnderItsIDAndTheClearEndsIt(t *testing.T) {
	for _, withStream := range []bool{true, false} {
		d, c, _ := newDispatcher(t, "acme")
		spy := &fenceSpy{}
		d.Fence = spy
		var sentID string
		if withStream {
			h := &hookClient{Client: c, mu: &sync.Mutex{}, log: &[]string{}}
			d.Client = h
			d.RegisterStream(identity("acme"), &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
				sentID = id
				return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
			}})
			// The stream's opening pass reads the robot in a goroutine of its own. Read after the stop
			// below is written Stopped and before its record lands, the robot would be a stopped robot
			// that records no stop, and the pass would send it a stop of its own with its own fence
			// (resendHeld). The stop waits until the pass has read, as clearRigAt's does.
			for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
				h.mu.Lock()
				n := h.robotLists
				h.mu.Unlock()
				if n >= 1 {
					break
				}
				if time.Now().After(end) {
					t.Fatal("setup: the stream's opening pass did not read the robots")
				}
			}
		}
		res, _ := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{})
		var fences []string
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if fences, _ = spy.snapshot(); len(fences) > 0 {
				break
			}
		}
		if len(fences) != 1 || (withStream && fences[0] != "amr-1/"+sentID) {
			t.Fatalf("withStream=%v: fences %v, want one for amr-1 under the stop's ID %q", withStream, fences, sentID)
		}
		if !withStream {
			continue
		}
		if res.State != fleetv1.RobotEstopStopped {
			t.Fatalf("setup: state %s", res.State)
		}
		if state, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{}); err != nil || state != fleetv1.RobotEstopResuming {
			t.Fatalf("ClearEstop = %s, %v", state, err)
		}
		if _, cleared := spy.snapshot(); len(cleared) != 1 || cleared[0] != "amr-1" {
			t.Fatalf("stops in force ended for %v, want amr-1 after the clear", cleared)
		}
	}
}

// The operator release (S6) hands the robot to the command dispatcher once Normal is written, so
// what a stop withheld from it (a zone admission, a reservation grant) is sent; a refused release
// hands nothing.
func TestFence_TheReleaseSendsWhatAStopWithheld(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	spy := &fenceSpy{}
	d.Fence = spy
	d.RegisterStream(identity("acme"), stoppedSender(d))
	ctx := context.Background()
	if _, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil {
		t.Fatal(err)
	}
	waitRecorded(t, c, 1)
	if st, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	// Refused: no telemetry since the clear.
	if st, _, _ := d.ConfirmResume(ctx, ns, "amr-1", "admin"); st == fleetv1.RobotEstopNormal {
		t.Fatal("released with no telemetry since the clear")
	}
	if spy.released != nil {
		t.Fatalf("a refused release handed %v to the command dispatcher", spy.released)
	}
	projected(t, d, c, time.Now().Add(time.Minute), fleetv1.RobotPhaseIdle)
	if st, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s, %v; want Normal", st, err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.released) != 1 || spy.released[0] != "amr-1" {
		t.Fatalf("released %v; want amr-1 once, after Normal", spy.released)
	}
}
