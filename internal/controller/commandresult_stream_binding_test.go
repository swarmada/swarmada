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

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/probe"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A CommandResult answers the command it names only when it arrives on the stream the
// command was pushed down, for the robot the command was pushed for (RFC-0001
// #security-fleet-adapter-authentication-robot-identity). Command ids are a per-process
// counter, so an id is easy to guess; what makes a guessed id worthless is that the result
// must come back the way the command went.
//
// Same fixture as the supersession tests: acme-adapter serves amr-1 and amr-2, and
// other-adapter serves amr-b.

func forgedHealthy(commandID uint64, robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_CommandResult{CommandResult: &fav1.CommandResult{
		CommandId: commandID, RobotId: robotID,
		Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{
			Status: fav1.ProbeStatus_PROBE_STATUS_HEALTHY, Message: "forged"}},
	}}}
}

// waitReachable waits until robotID's adapter stream is registered for commands: its
// adapter answers a per-robot heartbeat. Phase Connected is set before registration, so
// it does not prove this.
func (w *supWorld) waitReachable(t *testing.T, robotID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		alive, err := w.d.Heartbeat(ctx, supNS, robotID)
		cancel()
		if alive && err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("robot %s never became reachable: alive=%t err=%v", robotID, alive, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitCommand returns the first command e receives.
func waitCommand(t *testing.T, e *adapterEnd) *fav1.Command {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.mu.Lock()
		if len(e.commands) > 0 {
			c := e.commands[0]
			e.mu.Unlock()
			return c
		}
		e.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("no command reached the adapter")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCommandResult_FromAnotherAdaptersStreamDoesNotAnswerTheCommand(t *testing.T) {
	w := newSupWorld(t, 60)
	target := w.connect(t, supAdapter, nil) // receives the command, never answers
	forger := w.connect(t, supOther, nil)
	w.waitReachable(t, "amr-1")

	type out struct {
		res probe.Result
		err error
	}
	got := make(chan out, 1)
	go func() {
		res, err := verifyRobot(t, w, "amr-1", 1500*time.Millisecond)
		got <- out{res, err}
	}()
	cmd := waitCommand(t, target)
	t.Logf("verify for amr-1 pushed down %s's stream: command_id=%d", supAdapter, cmd.GetCommandId())
	// other-adapter names a robot it does serve, so the per-message gate passes the result.
	if err := forger.send(forgedHealthy(cmd.GetCommandId(), "amr-b")); err != nil {
		t.Fatalf("forger send: %v", err)
	}
	o := <-got
	if o.err == nil {
		t.Errorf("verify for amr-1 resolved from %s's stream: status=%s message=%q", supOther, o.res.Status, o.res.Message)
	}
	if !w.audit.denied(audit.EventRobotAuthzDenied, "amr-b") {
		t.Errorf("the refused result was not sealed ROBOT_AUTHZ_DENIED")
	}
}

func TestCommandResult_NamingAnotherRobotOnTheSameStreamDoesNotAnswerTheCommand(t *testing.T) {
	w := newSupWorld(t, 60)
	target := w.connect(t, supAdapter, func(cmd *fav1.Command) *fav1.CommandResult {
		r := answerHealthy(cmd)
		r.RobotId = "amr-2" // a robot this adapter serves, but not the one it was asked about
		r.GetVerify().Message = "forged"
		return r
	})
	_ = target
	w.waitReachable(t, "amr-1")

	res, err := verifyRobot(t, w, "amr-1", 1500*time.Millisecond)
	if err == nil {
		t.Errorf("verify for amr-1 resolved by a result naming amr-2: status=%s message=%q", res.Status, res.Message)
	}
	if !w.audit.denied(audit.EventRobotAuthzDenied, "amr-2") {
		t.Errorf("the refused result was not sealed ROBOT_AUTHZ_DENIED")
	}
}

func TestCommandResult_OnTheSendingStreamForTheRobotAnswersTheCommand(t *testing.T) {
	w := newSupWorld(t, 60)
	w.connect(t, supAdapter, answerHealthy)
	w.waitReachable(t, "amr-1")

	res, err := verifyRobot(t, w, "amr-1", 3*time.Second)
	if err != nil || res.Status != probe.StatusHealthy {
		t.Fatalf("honest verify: status=%s err=%v, want Healthy", res.Status, err)
	}
}

// RA-1: a refused result, and telemetry on the stream that sent it, write nothing. The
// liveness beat is set far out (every 30s) so no presence write falls in the window.
func TestCommandResult_RefusedResultsAndTelemetryWriteNothing(t *testing.T) {
	w := newSupWorld(t, 60)
	w.connect(t, supAdapter, nil)
	forger := w.connect(t, supOther, nil)
	w.waitReachable(t, "amr-1")
	time.Sleep(200 * time.Millisecond) // let both connects' presence writes land
	before := w.writeCount()

	for i := 0; i < 25; i++ {
		if err := forger.send(telemetryFrame("amr-b")); err != nil {
			t.Fatalf("telemetry: %v", err)
		}
		if err := forger.send(forgedHealthy(uint64(i+1), "amr-b")); err != nil {
			t.Fatalf("forged result: %v", err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if after := w.writeCount(); after != before {
		t.Errorf("refused results and telemetry made %d API writes, want 0", after-before)
	}
}

func telemetryFrame(robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{Telemetry: &fav1.TelemetryPayload{
		RobotId: robotID, TimestampMs: 1}}}
}
