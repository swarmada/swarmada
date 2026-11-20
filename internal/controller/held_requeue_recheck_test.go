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
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// stopsOverWork lists every Robot past the cache for a Paused action no robot is bound
// to, when a requeue request stands on it. The LIST stays live: deciding from the cache which
// robots record the action could miss one, and its stops, and run the requeue under a stop. What
// changes is the passes around it: the hold schedules none of its own (the status write's event
// brings the next one). The request still goes ahead once the stop clears, with no Robot event:
// a held request always has a robot under a covering stop that records the action, and the
// executor guard looks at such an action again every executorguard.CoveredRecheck (5 s).

// robotLists counts the Robot LISTs that bypass the cache.
type robotLists struct {
	client.Reader
	n int
}

func (rl *robotLists) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*fleetv1.RobotList); ok {
		rl.n++
	}
	return rl.Reader.List(ctx, list, opts...)
}

// newRobotLists makes r read past its cache through a counter over its own client.
func newRobotLists(r *FleetActionReconciler) *robotLists {
	rl := &robotLists{Reader: r.Client}
	r.APIReader = rl
	return rl
}

// A requeue request on a Pending action never reaches the stop check's Robot LIST.
func TestHeldRequeue_APendingActionReadsNoRobotListForTheStopCheck(t *testing.T) {
	a := assignedAction("t1", "", fleetv1.ActionPhasePending, 3, nil)
	a.Annotations = map[string]string{annRequeueRequested: "operator"}
	r, _ := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	rl := newRobotLists(r)

	verdict, _, err := r.requeueOvertakenByEstop(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if verdict != requeueRuns {
		t.Errorf("verdict = %v, want requeueRuns", verdict)
	}
	if rl.n != 0 {
		t.Errorf("Robot LISTs past the cache = %d, want 0", rl.n)
	}
}

// A request asked before the stop that now covers the robot recording the work is held. The hold
// schedules no pass at 1 s; the pass its status write brings asks for one within the recheck; and
// once the stop clears, the first recheck carries the request out, with no Robot event between.
// The recheck is the executor guard's: a robot covered by a stop keeps its record and the guard
// looks again after executorguard.CoveredRecheck (5 s). That meets "within the 15 s recheck"; a
// CoveredRecheck above pendingRecheckInterval fails this test.
func TestHeldRequeue_NoOneSecondPassAndGoesAheadOnTheRecheckAfterTheStopClears(t *testing.T) {
	a := assignedAction("t1", "", fleetv1.ActionPhasePaused, 3, nil)
	a.Annotations = map[string]string{annRequeueRequested: "operator"}
	a.Status.Message = requeuingMessage // seen before the stop: asked with no stop standing
	rb := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1")
	rb.Status.EstopState = fleetv1.RobotEstopStopped
	r, c := newActionReconciler(t, a, rb)
	r.Commander = &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
	rl := newRobotLists(r)

	// The pass that finds the stop holds the request, and schedules nothing of its own.
	res := reconcileAction(t, r, "t1")
	if got := getAction(t, c, "t1"); !strings.HasPrefix(got.Status.Message, requeueHeldPrefix) {
		t.Fatalf("message = %q, want the request held under the stop", got.Status.Message)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("the hold scheduled a pass of its own: %+v (its status write's event brings the next)", res)
	}
	t.Logf("hold pass: %d Robot LIST(s) past the cache", rl.n)

	// The pass the status write's event brings: still held, and looked at again within the recheck.
	before := rl.n
	res = reconcileAction(t, r, "t1")
	if res.RequeueAfter <= time.Second || res.RequeueAfter > pendingRecheckInterval {
		t.Errorf("RequeueAfter = %s while the request stands, want more than 1 s and at most %s",
			res.RequeueAfter, pendingRecheckInterval)
	}
	if got := getAction(t, c, "t1"); got.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s under the stop, want Paused", got.Status.Phase)
	}
	t.Logf("waiting pass: %d Robot LIST(s) past the cache", rl.n-before)
	if rl.n-before > 2 {
		t.Errorf("a waiting pass made %d Robot LISTs past the cache, want at most 2 "+
			"(the stop check's and the executor guard's)", rl.n-before)
	}

	// The stop clears. No Robot event is delivered: the next pass is the recheck.
	cleared := getRobot(t, c, "r1", actionNS)
	cleared.Status.EstopState = fleetv1.RobotEstopNormal
	if err := c.Status().Update(context.Background(), cleared); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // the recheck, and the passes the requeue's own writes bring
		reconcileAction(t, r, "t1")
	}
	// Carried out: the request is removed and the action has left Paused (Pending, or already given
	// to a free robot by the pass after).
	got := getAction(t, c, "t1")
	if _, ok := got.Annotations[annRequeueRequested]; ok || got.Status.Phase == fleetv1.ActionPhasePaused {
		t.Errorf("after the stop cleared: phase %s, requeue request still standing %v; want the requeue carried out "+
			"(request removed, no longer Paused), message %q", got.Status.Phase, ok, got.Status.Message)
	}
}
