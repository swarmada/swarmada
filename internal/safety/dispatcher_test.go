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
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"

	"github.com/swarmada/swarmada/internal/metrics"
)

const ns = "warehouse-a"

func identity(adapter string) controlstream.TLSIdentity {
	return controlstream.TLSIdentity{AdapterName: adapter, Namespace: ns, Verified: true}
}

// scriptedSender routes a canned set of EstopAcks (echoing the estop_id) inline on
// each Send, so TriggerEstop resolves deterministically with no timing.
type scriptedSender struct {
	d     *Dispatcher
	reply func(estopID string) []*fav1.EstopAck
	sent  int
}

func (s *scriptedSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	s.sent++
	if s.reply != nil {
		for _, ack := range s.reply(m.GetEstop().GetEstopId()) {
			s.d.RouteAck(ack)
		}
	}
	return nil
}

func ackState(estopID string, st fav1.EstopState) *fav1.EstopAck {
	return &fav1.EstopAck{EstopId: estopID, State: st, ConfirmedAtMs: 1}
}

func newDispatcher(t *testing.T, adapter string) (*Dispatcher, client.Client, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: adapter, Version: "1.0.0"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	rec := record.NewFakeRecorder(10)
	d := New(c, rec)
	d.deliveryTimeout = 60 * time.Millisecond // keep the dropped-estop test fast
	d.confirmTimeout = 60 * time.Millisecond
	return d, c, rec
}

func estopState(t *testing.T, c client.Client) fleetv1.RobotEstopState {
	t.Helper()
	robot := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return robot.Status.EstopState
}

// A CONFIRMED EstopAck(STOPPED) marks the robot Stopped.
func TestTriggerEstop_ConfirmedStopped(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}}
	d.RegisterStream(identity("acme"), sender)

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot)
	if err != nil || !res.Confirmed || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("res=%+v err=%v; want confirmed Stopped", res, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s, want Stopped", got)
	}
}

// STOPPING then a CONFIRMED STOPPED resolves to Stopped.
func TestTriggerEstop_StoppingThenStopped(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{
			ackState(id, fav1.EstopState_ESTOP_STATE_STOPPING),
			ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED),
		}
	}}
	d.RegisterStream(identity("acme"), sender)

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot)
	if err != nil || !res.Confirmed || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("res=%+v err=%v; want confirmed Stopped", res, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s, want Stopped", got)
	}
}

// CARDINAL SAFETY TEST: a dropped estop (no EstopAck) must NEVER be reported as
// Stopped — it resolves to Failed and signals escalation.
func TestTriggerEstop_DroppedNeverStopped(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	sender := &scriptedSender{d: d, reply: nil} // never acks
	d.RegisterStream(identity("acme"), sender)

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot)
	if res.Confirmed || res.State == fleetv1.RobotEstopStopped {
		t.Fatalf("SECURITY: dropped estop reported as stopped: %+v", res)
	}
	if !errors.Is(err, ErrUndelivered) || res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("res=%+v err=%v; want Failed + ErrUndelivered", res, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
		t.Fatalf("estopState = %s, want Failed (never Stopped on silence)", got)
	}
}

// No connected SafetyStream: undelivered, robot Failed (escalate), never Stopped.
func TestTriggerEstop_NoStreamFailsSafe(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme") // nothing registered
	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot)
	if !errors.Is(err, ErrUndelivered) || res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("res=%+v err=%v; want Failed + ErrUndelivered", res, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
		t.Fatalf("estopState = %s, want Failed", got)
	}
}

// STOPPING with no STOPPED confirmation within the window ⇒ Failed, never Stopped.
func TestTriggerEstop_StoppingOnlyTimesOutToFailed(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPING)}
	}}
	d.RegisterStream(identity("acme"), sender)

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Confirmed || res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("res=%+v; want unconfirmed Failed (STOPPING never assumed stopped)", res)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
		t.Fatalf("estopState = %s, want Failed", got)
	}
}

// A round trip over the 500ms SLA emits an EstopLatencyViolation event.
func TestTriggerEstop_LatencyViolation(t *testing.T) {
	d, _, rec := newDispatcher(t, "acme")
	base := time.Unix(1000, 0)
	calls := 0
	d.now = func() time.Time {
		calls++
		if calls == 1 {
			return base // send time
		}
		return base.Add(600 * time.Millisecond) // ack time → 600ms latency
	}
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}}
	d.RegisterStream(identity("acme"), sender)

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot)
	if err != nil || !res.LatencyViolation || res.Latency != 600*time.Millisecond {
		t.Fatalf("res=%+v err=%v; want LatencyViolation at 600ms", res, err)
	}
	select {
	case ev := <-rec.Events:
		if !contains(ev, "EstopLatencyViolation") {
			t.Fatalf("event = %q, want EstopLatencyViolation", ev)
		}
	default:
		t.Fatal("no EstopLatencyViolation event emitted")
	}
	// Confirmed STOPPED still holds — a latency violation does not un-stop the robot.
	if !res.Confirmed {
		t.Fatal("a slow but confirmed stop must still be Confirmed")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
