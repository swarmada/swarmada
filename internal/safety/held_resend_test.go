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

// A robot whose status holds a stop is sent that stop again when its adapter's
// SafetyStream opens, Failed included, once per stream (RFC-0001
// #safety-emergency-stop-protocol). A robot in Normal is sent nothing, nor is one in
// Resuming, whose stop an operator cleared; and an estop still awaited that a takeover
// moves onto the stream is not sent a second time.

import (
	"strings"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// heldStopResent waits for the stop a held robot is sent when its adapter's stream opens.
func heldStopResent(t *testing.T, s *recordingSender) *fav1.ControlPlaneSafetyMessage {
	t.Helper()
	m := nextEstop(t, s, "the held stop re-sent on the new stream")
	if m.GetEstop().GetIssuedBy() != heldStopIssuer || m.GetRobotId() != "amr-1" {
		t.Fatalf("the first push on the new stream is not the held stop for amr-1: %v", m)
	}
	return m
}

func TestHeldStop_ResentOnceWhenTheStreamOpens(t *testing.T) {
	for _, held := range []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed} {
		t.Run(string(held), func(t *testing.T) {
			d, c, rec := newDispatcher(t, "acme")
			setState(t, c, held)
			for i, s := range []*recordingSender{newRecordingSender(), newRecordingSender()} {
				d.RegisterStream(identity("acme"), s) // the second is a reconnect
				m := heldStopResent(t, s)
				if !strings.Contains(m.GetEstop().GetReason(), string(held)) {
					t.Fatalf("reconnect %d: reason %q does not name the held state", i, m.GetEstop().GetReason())
				}
				if err := d.RouteAck(identity("acme"), s, "amr-1",
					ackState(m.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
					t.Fatalf("reconnect %d: the ack for the held stop was refused: %v", i, err)
				}
				nothingPushed(t, s, "a second push on one stream", 300*time.Millisecond)
				select {
				case e := <-rec.Events:
					if !strings.Contains(e, "Normal EstopResent") {
						t.Fatalf("reconnect %d: event %q; want EstopResent", i, e)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("reconnect %d: the confirmed re-send left no Event", i)
				}
			}
			if got := estopState(t, c); got != held {
				t.Fatalf("estopState = %s after the re-send; it writes no state, want %s", got, held)
			}
		})
	}
}

func TestHeldStop_NothingSentToARobotNoStopHolds(t *testing.T) {
	for _, state := range []fleetv1.RobotEstopState{"", fleetv1.RobotEstopNormal, fleetv1.RobotEstopResuming} {
		t.Run("state "+string(state), func(t *testing.T) {
			d, c, _ := newDispatcher(t, "acme")
			setState(t, c, state)
			s := newRecordingSender()
			d.RegisterStream(identity("acme"), s)
			nothingPushed(t, s, "a robot in "+string(state), 500*time.Millisecond)
		})
	}
}

// A trigger still awaiting its ack when the stream is taken over is moved onto the new
// stream; the robot's Stopping is that same stop, and is not sent a second time.
func TestHeldStop_AnEstopMovedByATakeoverIsNotSentTwice(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout, d.confirmTimeout = 2*time.Second, 5*time.Second
	old, fresh := newRecordingSender(), newRecordingSender()
	d.RegisterStream(identity("acme"), old)
	done := triggerAsync(d, "aisle blocked")
	first := nextEstop(t, old, "the estop on the first stream")
	if err := d.RouteAck(identity("acme"), old, "amr-1",
		ackState(first.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPING)); err != nil {
		t.Fatal(err)
	}
	waitState(t, c, fleetv1.RobotEstopStopping)

	d.RegisterStream(identity("acme"), fresh)
	again := nextEstop(t, fresh, "the moved estop")
	if again.GetEstop().GetIssuedBy() != "operator" {
		t.Fatalf("the first push on the new stream is not the moved trigger: %v", again)
	}
	nothingPushed(t, fresh, "the same stop sent twice", 500*time.Millisecond)
	if err := d.RouteAck(identity("acme"), fresh, "amr-1",
		ackState(again.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, done); !o.res.Confirmed {
		t.Fatalf("res=%+v; want confirmed", o.res)
	}
}
