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
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// failingLists fails every List until it has failed n times: the Robot read RobotRegistered makes
// (robotid.Lookup lists the namespace's robots).
type failingLists struct {
	client.Client
	n atomic.Int64
}

func (f *failingLists) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if f.n.Add(-1) >= 0 {
		return errors.New("the API server is unavailable")
	}
	return f.Client.List(ctx, list, opts...)
}

// A register accepted on a ControlStream, and then the Robot read for its fences fails: the robot
// still gets its fences, first, and is assigned work on that stream, without a reconnect. A robot
// whose register was never processed there is still refused.
func TestRegister_AFailedRobotReadLeavesNoRobotUnreadyOnItsStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fails  int64
		fences int
	}{
		{"read-fails-once", 1, 1},
		{"read-fails-every-retry", 100, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, c := newDispatcher(t, cmdAdapter)
			robot := cmdRobot(t, c)
			robot.Status.EstopState = fleetv1.RobotEstopStopped
			robot.Status.Stops = []fleetv1.RobotStop{{ID: "s1", Scope: fleetv1.StopScopeRobot, SentAt: metav1.Now()}}
			if err := c.Update(context.Background(), robot); err != nil {
				t.Fatal(err)
			}
			fl := &failingLists{Client: c}
			fl.n.Store(tc.fails)
			d.Client = fl
			d.RequireRegister = true
			s := acceptAll(d)
			d.RegisterStream(verifiedIdentity(), s)

			d.RobotRegistered(context.Background(), s, cmdNS, cmdRobotID)
			fl.n.Store(0)
			// The stop holds the robot: the assign is refused for it, not for the register.
			if err := assignOnce(d); !errors.Is(err, ErrHeldByStop) {
				t.Fatalf("assign: %v; want ErrHeldByStop (the register was processed, the stop holds it)", err)
			}
			fences := 0
			for _, cmd := range s.sent {
				if cmd.GetEstop() != nil {
					fences++
				}
			}
			if fences != tc.fences {
				t.Fatalf("%d fences on the stream; want %d", fences, tc.fences)
			}
			// The clear: work goes on the same stream, with no second register.
			d.ClearHeld(cmdNS, cmdRobotID)
			robot = cmdRobot(t, c)
			robot.Status.EstopState, robot.Status.Stops[0].ClearedAt = fleetv1.RobotEstopResuming, &metav1.Time{}
			if err := c.Update(context.Background(), robot); err != nil {
				t.Fatal(err)
			}
			if err := assignOnce(d); err != nil {
				t.Fatalf("assign after the clear: %v; want it sent on the same stream", err)
			}
		})
	}
}

// A robot whose register was never processed on the stream is still refused, whatever the reads.
func TestRegister_NoRegisterStillRefusesWork(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	d.RequireRegister = true
	s := acceptAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	if err := assignOnce(d); !errors.Is(err, ErrNotRegistered) || len(s.sent) != 0 {
		t.Fatalf("err %v, %d commands; want ErrNotRegistered and nothing sent", err, len(s.sent))
	}
}

// When the fences cannot be sent once the register is processed, the failure is on the Robot: a
// Warning Event, not only a log line.
func TestRegister_FencesThatCannotBeSentAreReportedOnTheRobot(t *testing.T) {
	d, c := newDispatcher(t, cmdAdapter)
	rec := record.NewFakeRecorder(10)
	d.Recorder = rec
	robot := cmdRobot(t, c)
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	robot.Status.Stops = []fleetv1.RobotStop{{ID: "s1", Scope: fleetv1.StopScopeRobot, SentAt: metav1.Now()}}
	if err := c.Update(context.Background(), robot); err != nil {
		t.Fatal(err)
	}
	d.RequireRegister = true
	s := &fakeSender{dispatch: d, sendErr: errors.New("stream broken")}
	d.RegisterStream(verifiedIdentity(), s)
	d.RobotRegistered(context.Background(), s, cmdNS, cmdRobotID)
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Warning") || !strings.Contains(e, "FencesNotSent") {
			t.Fatalf("event %q; want a Warning FencesNotSent", e)
		}
	default:
		t.Fatal("no Event on the Robot for fences that could not be sent")
	}
}
