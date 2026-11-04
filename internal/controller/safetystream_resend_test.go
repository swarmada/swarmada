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

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// An estop awaiting its ack on a SafetyStream that a second stream under the same
// identity replaces is re-sent down the second stream, under a new estop_id, and confirmed
// there. Before, the ack could not arrive: the first stream was closed, and an ack on the
// second was refused because the estop had not gone down it, so the estop resolved Failed.
func TestSafetyStream_PendingEstopIsResentDownTheStreamThatReplacedIt(t *testing.T) {
	w, d := newSafetyWorld(t)

	first := w.connectSafety(t, supAdapter, false) // receives the estop, never answers

	type result struct {
		res safety.Result
		err error
	}
	got := make(chan result, 1)
	go func() {
		res, err := triggerEstop(t, d, "amr-1", 6*time.Second)
		got <- result{res, err}
	}()
	var firstID string
	deadline := time.Now().Add(3 * time.Second)
	for firstID == "" {
		if msgs := first.received(); len(msgs) > 0 {
			firstID = msgs[0].GetEstop().GetEstopId()
		} else if time.Now().After(deadline) {
			t.Fatal("the estop never reached the first stream")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := w.connectSafety(t, supAdapter, false) // answers only when told to
	var secondID string
	deadline = time.Now().Add(3 * time.Second)
	for secondID == "" {
		if msgs := second.received(); len(msgs) > 0 {
			secondID = msgs[0].GetEstop().GetEstopId()
			if len(msgs) != 1 || msgs[0].GetRobotId() != "amr-1" {
				t.Fatalf("the second stream received %d estop(s), want the one re-send for amr-1: %v", len(msgs), msgs)
			}
		} else if time.Now().After(deadline) {
			o := <-got
			t.Fatalf("the estop pending on the replaced SafetyStream was never re-sent down the stream that "+
				"replaced it, and resolved: confirmed=%v state=%s err=%v", o.res.Confirmed, o.res.State, o.err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if secondID != firstID {
		t.Fatalf("re-sent under estop_id %q, sent under %q: every re-send carries the stop's ID", secondID, firstID)
	}
	if !first.closed(3 * time.Second) {
		t.Error("the replaced SafetyStream is still open")
	}

	if err := second.ack("amr-1", secondID, fav1.EstopState_ESTOP_STATE_STOPPED); err != nil {
		t.Fatalf("sending the ack for the re-send: %v", err)
	}
	o := <-got
	if !o.res.Confirmed || o.res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("the estop re-sent down the stream that replaced its own was not confirmed there: "+
			"confirmed=%v state=%s err=%v", o.res.Confirmed, o.res.State, o.err)
	}
	robot := &fleetv1.Robot{}
	if err := w.c.Get(context.Background(), client.ObjectKey{Namespace: supNS, Name: "amr-1"}, robot); err != nil {
		t.Fatal(err)
	}
	if robot.Status.EstopState != fleetv1.RobotEstopStopped {
		t.Fatalf("robot estopState = %s, want Stopped", robot.Status.EstopState)
	}
	t.Logf("estop %s on the replaced stream was re-sent under its ID and confirmed there", firstID)
}
