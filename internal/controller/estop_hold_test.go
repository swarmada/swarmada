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
	"errors"
	"fmt"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

// A stop the robot records is active whatever its estopState reads: the held-action
// pause, filter 10 and preemption-victim selection read it.
func TestRobotUnderEstop_ReadsTheStopTheRobotRecords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		robot fleetv1.Robot
		want  bool
	}{
		{name: "Normal, nothing recorded", want: false},
		{name: "Resuming, nothing recorded", robot: fleetv1.Robot{Status: fleetv1.RobotStatus{EstopState: fleetv1.RobotEstopResuming}}, want: false},
		{name: "Stopped", robot: fleetv1.Robot{Status: fleetv1.RobotStatus{EstopState: fleetv1.RobotEstopStopped}}, want: true},
		{name: "Normal with its own trigger (its stop state not yet written)", robot: *robotWithEstop("r", "own", ""), want: true},
		{name: "Normal with a recorded zone stop (its stop state never written)", robot: fleetv1.Robot{Status: fleetv1.RobotStatus{
			EstopHeldBy: []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z"}}}}, want: true},
	} {
		if got := robotUnderEstop(&tc.robot); got != tc.want {
			t.Errorf("%s: robotUnderEstop = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// unwrittenEstopper answers the first n stops as confirmed but unwritten.
type unwrittenEstopper struct {
	fakeEstopper
	mu sync.Mutex
	n  int
}

func (u *unwrittenEstopper) TriggerEstop(ctx context.Context, ns, robot, reason, by string,
	scope metrics.EstopScope, carrier safety.Carrier) (safety.Result, error) {
	res, _ := u.fakeEstopper.TriggerEstop(ctx, ns, robot, reason, by, scope, carrier)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n > 0 {
		u.n--
		return res, fmt.Errorf("%w (Stopped): injected", safety.ErrStateUnwritten)
	}
	return res, nil
}

// A robot-scope stop whose outcome was not written is not recorded as issued: no marker,
// an error, and the next reconcile issues it again.
func TestRobotEstop_AnUnwrittenOutcomeIsIssuedAgain(t *testing.T) {
	est := &unwrittenEstopper{n: 1}
	r, c := newRobotEstop(t, &est.fakeEstopper, robotWithEstop("amr-1", "own", ""))
	r.Estopper = est
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: zeNS, Name: "amr-1"}}
	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, safety.ErrStateUnwritten) {
		t.Fatalf("first reconcile err = %v, want ErrStateUnwritten", err)
	}
	if _, marked := zeGetRobot(t, c, "amr-1").Annotations[annEstopProcessed]; marked {
		t.Fatal("the processed marker was written for a stop whose outcome was not")
	}
	reconcileRobotEstop(t, r, "amr-1")
	if got := est.names(); len(got) != 2 {
		t.Fatalf("stops issued = %v, want two (the first unwritten)", got)
	}
	if zeGetRobot(t, c, "amr-1").Annotations[annEstopProcessed] != "own" {
		t.Fatal("the processed marker was not written once the outcome was")
	}
}

// A new stop comes before a pending resume request: the request is answered after it.
func TestRobotEstop_ANewStopComesBeforeAPendingResume(t *testing.T) {
	est := &fakeEstopper{}
	robot := robotWithEstop("amr-1", "own", "")
	robot.Annotations[fleetv1.EstopResumeAnnotation] = "req-1"
	r, _ := newRobotEstop(t, est, robot)
	reconcileRobotEstop(t, r, "amr-1")
	if got := est.names(); len(got) != 1 || got[0] != "amr-1" {
		t.Fatalf("the first reconcile issued %v, want the stop", got)
	}
}

func zeGetRobot(t *testing.T, c client.Client, name string) *fleetv1.Robot {
	t.Helper()
	r := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zeNS, Name: name}, r); err != nil {
		t.Fatalf("get robot %s: %v", name, err)
	}
	return r
}
