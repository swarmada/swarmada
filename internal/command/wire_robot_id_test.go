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

package command

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A COMMAND GOES TO THE ROBOT_ID THE ADAPTER ANNOUNCED.
//
// Callers name a robot by its object name. The adapter knows it only by the robot_id it
// announced, which the Robot carries in its swarmada.io/robot-id annotation; the two
// differ for a robot admitted under another name (`swarmctl admit --name`). So every
// Command carries the wire id, and a result is matched on the wire id
// (RFC-0001 #security-fleet-adapter-authentication-robot-identity).

// newWireDispatcher builds a dispatcher over one Robot named name carrying robot_id wireID
// (no annotation when wireID is empty), bound to cmdAdapter.
func newWireDispatcher(t *testing.T, name, wireID string) (*Dispatcher, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: cmdNS, Name: name},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: cmdAdapter}},
	}
	if wireID != "" {
		r.Annotations = map[string]string{fleetv1.RobotIDAnnotation: wireID}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(r).Build()
	d := New(c)
	d.timeout = 500 * time.Millisecond
	return d, c
}

// answerAs is an adapter that knows its robot as knownID. It answers a model_update naming
// knownID; a Command naming any other robot_id is not a robot it serves, and gets no answer.
func answerAs(d *Dispatcher, knownID, resultID string) *fakeSender {
	return &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		if cmd.GetRobotId() != knownID {
			return nil
		}
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: resultID,
			Result: &fav1.CommandResult_ModelUpdate{ModelUpdate: &fav1.ModelUpdateResult{Acknowledged: true}},
		}
	}}
}

// Both admission paths: under its own id (annotation absent, or equal to the name) and
// under another name. The Command carries the wire id and the result naming it binds.
func TestCommandWireID_CommandCarriesTheAnnouncedRobotID(t *testing.T) {
	for _, tc := range []struct{ desc, name, annotation, wireID string }{
		{"no annotation: the name is the id", "amr-1", "", "amr-1"},
		{"admitted under its own id", "amr-1", "amr-1", "amr-1"},
		{"admitted under another name", "amr-1", "wire-7", "wire-7"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			d, _ := newWireDispatcher(t, tc.name, tc.annotation)
			sender := answerAs(d, tc.wireID, tc.wireID)
			d.RegisterStream(verifiedIdentity(), sender)
			out, err := d.PushModelUpdate(context.Background(), cmdNS, tc.name, ModelUpdate{ModelName: "nav", NewVersion: "2"})
			if err != nil {
				t.Fatalf("model_update to %s (robot_id %s) was not confirmed: %v", tc.name, tc.wireID, err)
			}
			if !out.Acknowledged {
				t.Errorf("outcome = %+v, want acknowledged", out)
			}
			sender.mu.Lock()
			defer sender.mu.Unlock()
			if len(sender.sent) != 1 || sender.sent[0].GetRobotId() != tc.wireID {
				t.Errorf("sent %v, want one Command naming robot_id %q", sender.sent, tc.wireID)
			}
		})
	}
}

// A result naming the object name of a renamed robot answers nothing: it is not the robot
// the Command was sent to.
func TestCommandWireID_ResultNamingTheObjectNameIsRefused(t *testing.T) {
	d, _ := newWireDispatcher(t, "amr-1", "wire-7")
	sender := &fakeSender{dispatch: d}
	d.RegisterStream(verifiedIdentity(), sender)
	errCh := make(chan error, 1)
	go func() {
		_, err := d.PushModelUpdate(context.Background(), cmdNS, "amr-1", ModelUpdate{ModelName: "nav", NewVersion: "2"})
		errCh <- err
	}()
	var cmd *fav1.Command
	deadline := time.Now().Add(2 * time.Second)
	for cmd == nil {
		sender.mu.Lock()
		if len(sender.sent) > 0 {
			cmd = sender.sent[0]
		}
		sender.mu.Unlock()
		if cmd == nil && time.Now().After(deadline) {
			t.Fatal("no Command was pushed")
		}
		time.Sleep(time.Millisecond)
	}
	err := d.RouteResult(sender, &fav1.CommandResult{
		CommandId: cmd.GetCommandId(), RobotId: "amr-1",
		Result: &fav1.CommandResult_ModelUpdate{ModelUpdate: &fav1.ModelUpdateResult{Acknowledged: true}},
	})
	if err == nil {
		t.Error("a result naming the object name amr-1 was accepted for a Command sent to robot_id wire-7")
	}
	if err := <-errCh; !errors.Is(err, ErrUnreachable) {
		t.Errorf("PushModelUpdate = %v, want ErrUnreachable (no result named the robot it was sent to)", err)
	}
}

// heartbeatEcho answers a per-robot HeartbeatRequest by routing the robot_id it received
// back through the dispatcher, as the ControlStream does for the adapter's response.
type heartbeatEcho struct {
	mu   sync.Mutex
	d    *Dispatcher
	seen []string
}

func (h *heartbeatEcho) Send(msg *fav1.ControlPlaneMessage) error {
	hb := msg.GetHeartbeat()
	if hb == nil || hb.GetRobotId() == "" {
		return nil
	}
	h.mu.Lock()
	h.seen = append(h.seen, hb.GetRobotId())
	h.mu.Unlock()
	go h.d.RouteHeartbeat(cmdNS, hb.GetRobotId())
	return nil
}

// The per-robot confirming exchange names the wire id, and the response naming it answers.
func TestCommandWireID_HeartbeatNamesTheAnnouncedRobotID(t *testing.T) {
	d, _ := newWireDispatcher(t, "amr-1", "wire-7")
	echo := &heartbeatEcho{d: d}
	d.RegisterStream(verifiedIdentity(), echo)
	alive, err := d.Heartbeat(context.Background(), cmdNS, "amr-1")
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	echo.mu.Lock()
	defer echo.mu.Unlock()
	if len(echo.seen) != 1 || echo.seen[0] != "wire-7" {
		t.Errorf("heartbeat requests named %v, want one naming robot_id wire-7", echo.seen)
	}
	if !alive {
		t.Error("the robot's response under its own robot_id did not answer the exchange")
	}
}

// A reservation is held by the robot_id the adapter reserved under, so a grant is pushed
// for a wire id. It reaches a renamed robot's adapter under that id.
func TestCommandWireID_ReservationGrantForARenamedRobot(t *testing.T) {
	d, _ := newWireDispatcher(t, "amr-1", "wire-7")
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		if cmd.GetRobotId() != "wire-7" {
			return nil
		}
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: "wire-7",
			Result: &fav1.CommandResult_ReservationGranted{ReservationGranted: &fav1.ReservationGrantedAck{Acknowledged: true}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)
	ok, err := d.PushReservationGranted(context.Background(), cmdNS, "wire-7", "lift-1")
	if err != nil || !ok {
		t.Fatalf("PushReservationGranted(wire-7) = %v, %v; want the renamed robot's adapter to be told", ok, err)
	}
}
