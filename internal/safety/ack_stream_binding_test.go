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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// recordingSender records the estops pushed down it and answers nothing.
type recordingSender struct {
	sent chan *fav1.ControlPlaneSafetyMessage
}

func (s *recordingSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	s.sent <- m
	return nil
}

// An EstopAck confirms an estop only when it arrives on the SafetyStream the estop was
// pushed down (RFC-0001 #fleet-adapter-protocol-the-controlstream-service). The same adapter identity on
// another stream, naming the right robot and estop_id, is refused with ErrMisroutedAck.
func TestRouteAck_OnlyTheStreamTheEstopWentDownConfirmsIt(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 2 * time.Second
	sentOn := &recordingSender{sent: make(chan *fav1.ControlPlaneSafetyMessage, 1)}
	other := &recordingSender{sent: make(chan *fav1.ControlPlaneSafetyMessage, 1)}
	d.RegisterStream(identity("acme"), sentOn)

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot, Carrier{})
		done <- outcome{res, err}
	}()
	m := <-sentOn.sent
	stopped := ackState(m.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)

	err := d.RouteAck(identity("acme"), other, "amr-1", stopped)
	if !errors.Is(err, controlstream.ErrMisroutedAck) {
		t.Fatalf("an ack on another stream: err = %v, want ErrMisroutedAck", err)
	}
	select {
	case o := <-done:
		t.Fatalf("the estop resolved on an ack from another stream: %+v", o.res)
	case <-time.After(100 * time.Millisecond):
	}

	if err := d.RouteAck(identity("acme"), sentOn, "amr-1", stopped); err != nil {
		t.Fatalf("the ack on the stream the estop went down was refused: %v", err)
	}
	o := <-done
	if o.err != nil || !o.res.Confirmed || estopState(t, c) != fleetv1.RobotEstopStopped {
		t.Fatalf("res=%+v err=%v state=%s; want confirmed Stopped from its own stream", o.res, o.err, estopState(t, c))
	}

	// An ack for no pending estop is dropped without a denial: late, not misrouted.
	if err := d.RouteAck(identity("acme"), other, "amr-1", stopped); err != nil {
		t.Fatalf("a late ack is not a denial: %v", err)
	}
}
