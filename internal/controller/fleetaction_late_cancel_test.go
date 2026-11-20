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
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// lateCancelSender is an adapter's ControlStream that answers every cancel_action
// STOPPED_SAFELY, but only once the control plane's wait for that answer has ended: a loop
// behind on its queue, or an adapter slow to answer.
type lateCancelSender struct {
	d     *command.Dispatcher
	delay time.Duration
	wg    sync.WaitGroup

	mu      sync.Mutex
	cancels int
	routed  []error // RouteResult's return for each late answer
}

func (s *lateCancelSender) Send(msg *fav1.ControlPlaneMessage) error {
	cmd := msg.GetCommand()
	if cmd.GetCancelAction() == nil {
		return nil
	}
	s.mu.Lock()
	s.cancels++
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		time.Sleep(s.delay)
		err := s.d.RouteResult(s, &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_CancelAction{CancelAction: &fav1.CancelActionResult{
				Acknowledged: true, Disposition: fav1.CancelDisposition_CANCEL_DISPOSITION_STOPPED_SAFELY}},
		})
		s.mu.Lock()
		s.routed = append(s.routed, err)
		s.mu.Unlock()
	}()
	return nil
}

// A cancel whose result arrives after the control plane's wait for it (wireTimeout) is not a
// confirmed stop, through the real command dispatcher. The late STOPPED_SAFELY finds no waiter
// and is dropped, so it frees the robot neither then nor on a later cancel: the action and the
// robot are held until the lease has provably expired, and only then is the action Cancelled
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee).
func TestCancel_AResultAfterTheWaitHoldsUntilLeaseDeath(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		cancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease, "maintenance"),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	d := command.New(c)
	adapter := &lateCancelSender{d: d, delay: wireTimeout + 500*time.Millisecond}
	id := controlstream.TLSIdentity{Verified: true, Namespace: actionNS, AdapterName: testActionAdapter}
	d.RegisterStream(id, adapter)
	r.Commander = d

	start := time.Now()
	reconcileAction(t, r, "t1")
	if waited := time.Since(start); waited < wireTimeout {
		t.Fatalf("the reconcile returned after %s, before its %s wait for the cancel's result ended", waited, wireTimeout)
	}
	if ft := getAction(t, c, "t1"); ft.Status.Phase == fleetv1.ActionPhaseCancelled {
		t.Fatal("finalised Cancelled with no result within the wait, before the lease was provably dead")
	}
	assertBound(t, c, "t1", "r1")

	adapter.wg.Wait() // the first cancel's STOPPED_SAFELY has arrived, late
	reconcileAction(t, r, "t1")
	if ft := getAction(t, c, "t1"); ft.Status.Phase == fleetv1.ActionPhaseCancelled {
		t.Fatal("finalised Cancelled after a late STOPPED_SAFELY, while the lease lives")
	}
	assertBound(t, c, "t1", "r1")

	adapter.wg.Wait()
	ageLease(t, c, "t1", time.Hour)
	reconcileAction(t, r, "t1")
	if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s once the lease was provably dead, want Cancelled", ft.Status.Phase)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" {
		t.Errorf("r1 still bound to %q once the lease was provably dead", rob.Status.AssignedAction)
	}

	adapter.wg.Wait()
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.cancels != 3 {
		t.Errorf("%d cancel_action pushed, want one per reconcile (3)", adapter.cancels)
	}
	for i, err := range adapter.routed {
		if err != nil {
			t.Errorf("late answer %d: RouteResult = %v, want it dropped as a result nobody waits for", i+1, err)
		}
	}
}
