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
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// THE ESTOP GOES TO THE ROBOT_ID THE ADAPTER ANNOUNCED.
//
// A Robot's object name and its wire robot_id (the swarmada.io/robot-id annotation) differ
// whenever an operator admits it under another name (`swarmctl admit --name`). The adapter
// knows the robot only by the robot_id it announced, so the estop envelope must carry that
// id, and the adapter's acknowledgement, which names the same id, must confirm the stop
// (RFC-0001 #safety-emergency-stop-protocol). These tests run the real SafetyStream, the
// real streamauth.Authorizer and the real Dispatcher with mTLS; the adapter echoes the
// envelope's robot_id, as the reference simulation adapter does.

// setWireID stamps robot_id on the fixture's Robot amr-1.
func (f *ackFixture) setWireID(t *testing.T, robotID string) {
	t.Helper()
	var r fleetv1.Robot
	f.update(t, client.ObjectKey{Namespace: ns, Name: "amr-1"}, &r, func() {
		r.Annotations = map[string]string{fleetv1.RobotIDAnnotation: robotID}
	})
}

// estopAndAnswer estops amr-1 and waits for adapter-a to receive it. Before the ack it runs
// between (nil for nothing), then has adapter-a acknowledge STOPPED under ackRobotID. It
// returns the robot_id the envelope carried and the trigger's result.
func (f *ackFixture) estopAndAnswer(t *testing.T, ackRobotID string, between func()) (string, Result) {
	t.Helper()
	a := f.openSafety(t, "adapter-a")
	resCh := make(chan Result, 1)
	go func() {
		res, _ := f.d.TriggerEstop(context.Background(), ns, "amr-1", "test", "tester", metrics.ScopeRobot, Carrier{})
		resCh <- res
	}()
	pushed, err := a.Recv()
	if err != nil {
		t.Fatalf("adapter-a never received the estop: %v", err)
	}
	if between != nil {
		between()
	}
	if ackRobotID == "" {
		ackRobotID = pushed.GetRobotId()
	}
	if err := a.Send(&fav1.AdapterSafetyMessage{
		RobotId: ackRobotID,
		Payload: &fav1.AdapterSafetyMessage_EstopAck{EstopAck: &fav1.EstopAck{
			EstopId: pushed.GetEstop().GetEstopId(), State: fav1.EstopState_ESTOP_STATE_STOPPED,
		}},
	}); err != nil {
		t.Fatalf("send ack: %v", err)
	}
	select {
	case res := <-resCh:
		return pushed.GetRobotId(), res
	case <-time.After(10 * time.Second):
		t.Fatal("TriggerEstop never returned")
	}
	return "", Result{}
}

// Both admission paths: under its own id (the annotation equals the name) and under
// another name. The envelope carries the wire id, and an ack naming it confirms the stop.
func TestEstopWireID_EnvelopeCarriesTheAnnouncedRobotID(t *testing.T) {
	for _, tc := range []struct{ name, wireID string }{
		{"admitted under its own id", "amr-1"},
		{"admitted under another name", "wire-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAckFixture(t)
			f.setWireID(t, tc.wireID)
			sentAs, res := f.estopAndAnswer(t, tc.wireID, nil)
			if sentAs != tc.wireID {
				t.Errorf("the estop envelope named robot_id %q; the adapter announced %q", sentAs, tc.wireID)
			}
			if !res.Confirmed || res.State != fleetv1.RobotEstopStopped {
				t.Errorf("result = %+v: an ack naming the announced robot_id %q did not confirm the stop", res, tc.wireID)
			}
			if got := f.robotEstopState(t, "amr-1"); got != fleetv1.RobotEstopStopped {
				t.Errorf("amr-1 estopState = %s, want Stopped", got)
			}
		})
	}
}

// For a renamed robot the object name is not a robot_id the adapter knows. An ack naming
// it confirms nothing: the robot it would be confirming is not the one the estop went to.
func TestEstopWireID_AckNamingTheObjectNameDoesNotConfirm(t *testing.T) {
	f := newAckFixture(t)
	f.setWireID(t, "wire-7")
	_, res := f.estopAndAnswer(t, "amr-1", nil)
	if res.Confirmed || res.State == fleetv1.RobotEstopStopped {
		t.Errorf("result = %+v: an ack naming the object name confirmed a renamed robot's stop", res)
	}
	if got := f.robotEstopState(t, "amr-1"); got == fleetv1.RobotEstopStopped {
		t.Errorf("amr-1 estopState = Stopped on an ack that named no robot the adapter announced")
	}
}

// The annotation is re-pointed after the estop went out. The estop is bound to the id it
// was sent under; the waiter is not stranded. An ack under the old id is refused by the
// authorizer (no Robot carries that id any more), and one under the new id matches no
// estop, so the trigger resolves Failed at the end of its window: escalated, never Stopped.
func TestEstopWireID_AnnotationChangedInFlightResolvesFailedNotStranded(t *testing.T) {
	for _, ack := range []string{"wire-7", "wire-8"} {
		t.Run("ack names "+ack, func(t *testing.T) {
			f := newAckFixture(t)
			f.setWireID(t, "wire-7")
			start := time.Now()
			sentAs, res := f.estopAndAnswer(t, ack, func() { f.setWireID(t, "wire-8") })
			if sentAs != "wire-7" {
				t.Fatalf("the estop envelope named %q, want the id at send time (wire-7)", sentAs)
			}
			if res.Confirmed || res.State != fleetv1.RobotEstopFailed {
				t.Errorf("result = %+v, want Failed (escalated) after the robot_id changed under the estop", res)
			}
			if waited := time.Since(start); waited > 5*time.Second {
				t.Errorf("the trigger took %s to resolve; its windows are 750ms each", waited)
			}
		})
	}
}

// capturingSafetySender records what went down it.
type capturingSafetySender struct {
	mu   sync.Mutex
	sent []*fav1.ControlPlaneSafetyMessage
}

func (s *capturingSafetySender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

func (s *capturingSafetySender) last() *fav1.ControlPlaneSafetyMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return nil
	}
	return s.sent[len(s.sent)-1]
}

// The id is resolved once per send, not per retry. An estop re-sent down the stream that
// replaced the one it went down carries the robot_id it was first sent under, even when
// the annotation changed in between, and the ack for it confirms the stop.
func TestEstopWireID_ResendAfterTakeoverKeepsTheIDItWasSentUnder(t *testing.T) {
	f := newAckFixture(t)
	f.setWireID(t, "wire-7")
	id := controlstream.TLSIdentity{Verified: true, Namespace: ns, AdapterName: "adapter-a"}
	first := &capturingSafetySender{}
	f.d.RegisterStream(id, first)

	resCh := make(chan Result, 1)
	go func() {
		res, _ := f.d.TriggerEstop(context.Background(), ns, "amr-1", "test", "tester", metrics.ScopeRobot, Carrier{})
		resCh <- res
	}()
	deadline := time.Now().Add(5 * time.Second)
	for first.last() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the estop never went down the first stream")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := first.last().GetRobotId(); got != "wire-7" {
		t.Fatalf("the estop named robot_id %q, want wire-7", got)
	}

	f.setWireID(t, "wire-8")
	second := &capturingSafetySender{}
	f.d.RegisterStream(id, second)
	resent := second.last()
	if resent == nil {
		t.Fatal("the pending estop was not re-sent down the stream that replaced its own")
	}
	if got := resent.GetRobotId(); got != "wire-7" {
		t.Errorf("the re-sent estop named robot_id %q, want the id it was first sent under (wire-7)", got)
	}
	if err := f.d.RouteAck(id, second, resent.GetRobotId(), &fav1.EstopAck{
		EstopId: resent.GetEstop().GetEstopId(), State: fav1.EstopState_ESTOP_STATE_STOPPED,
	}); err != nil {
		t.Fatalf("RouteAck: %v", err)
	}
	select {
	case res := <-resCh:
		if !res.Confirmed || res.State != fleetv1.RobotEstopStopped {
			t.Errorf("result = %+v, want the re-sent estop's ack to confirm the stop", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("TriggerEstop never returned")
	}
}
