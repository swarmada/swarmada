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
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/probe"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A CommandResult resolves its command only when it comes back on the stream the command
// was pushed down, naming the robot it was pushed for (RFC-0001
// #security-fleet-adapter-authentication-robot-identity).

func healthyFor(id uint64, robotID string) *fav1.CommandResult {
	return &fav1.CommandResult{CommandId: id, RobotId: robotID,
		Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{Status: fav1.ProbeStatus_PROBE_STATUS_HEALTHY}}}
}

func TestRouteResult_BoundToTheSendingStreamAndRobot(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d} // records the command, never answers
	d.RegisterStream(verifiedIdentity(), sender)
	other := &fakeSender{dispatch: d}

	type out struct {
		res probe.Result
		err error
	}
	got := make(chan out, 1)
	go func() {
		res, err := d.Verify(context.Background(), cmdNS, cmdRobotID, probe.VerifyRequest{
			ProbeType: fleetv1.ProbeTypeHardware, Target: "front-lidar"})
		got <- out{res, err}
	}()
	var id uint64
	for deadline := time.Now().Add(2 * time.Second); id == 0; {
		sender.mu.Lock()
		if len(sender.sent) > 0 {
			id = sender.sent[0].GetCommandId()
		}
		sender.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("no command pushed")
		}
		time.Sleep(time.Millisecond)
	}

	if err := d.RouteResult(other, healthyFor(id, cmdRobotID)); !errors.Is(err, controlstream.ErrMisroutedResult) {
		t.Errorf("result on another stream: err = %v, want ErrMisroutedResult", err)
	}
	if err := d.RouteResult(sender, healthyFor(id, "another-robot")); !errors.Is(err, controlstream.ErrMisroutedResult) {
		t.Errorf("result naming another robot: err = %v, want ErrMisroutedResult", err)
	}
	select {
	case o := <-got:
		t.Fatalf("a refused result resolved the command: %+v, %v", o.res, o.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := d.RouteResult(sender, healthyFor(id, cmdRobotID)); err != nil {
		t.Fatalf("the honest result was refused: %v", err)
	}
	if o := <-got; o.err != nil || o.res.Status != probe.StatusHealthy {
		t.Fatalf("verify = %+v, %v; want Healthy", o.res, o.err)
	}
}

// A result for a command that is not pending is dropped without a denial: it may simply be
// late, and it answers nothing.
func TestRouteResult_UnknownCommandIsNotADenial(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	if err := d.RouteResult(&fakeSender{}, healthyFor(42, cmdRobotID)); err != nil {
		t.Fatalf("unknown command id: err = %v, want nil", err)
	}
}
