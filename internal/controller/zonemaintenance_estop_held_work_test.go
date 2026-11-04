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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The window's gated close resumes a robot only when no stop holds it. A robot can read
// estopState Normal while a stop it records still holds it — its own trigger, or a zone's
// or the namespace's stop in status.estopHeldBy — and its own zone can read Clear while an
// ancestor's stop holds it (RFC-0001 #safety-estop-scopes). The close reads the record.
func TestZM_GatedCloseReadsTheStopRecord(t *testing.T) {
	cases := map[string]func(r *fleetv1.Robot){
		"an ancestor zone's stop on record": func(r *fleetv1.Robot) {
			r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "hall", Since: metav1.Now()}}
		},
		"the namespace's stop on record": func(r *fleetv1.Robot) {
			r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldSwarmadaConfig, Name: SwarmadaConfigName, Since: metav1.Now()}}
		},
		"its own trigger": func(r *fleetv1.Robot) {
			r.Annotations = map[string]string{annEstopTriggered: "operator"}
		},
	}
	for name, hold := range cases {
		t.Run(name, func(t *testing.T) {
			now := zmBase
			robot := zmRobot("r1", "z1", fleetv1.RobotPhaseMaintenance, "")
			robot.Status.EstopState = fleetv1.RobotEstopNormal
			hold(robot)
			r, _ := newZMReconciler(t, &now, robot)
			clear, err := r.estopClear(context.Background(), zmNS, robot)
			if err != nil {
				t.Fatal(err)
			}
			if clear {
				t.Errorf("the gated close treats a robot held by %s as clear of any stop", name)
			}
		})
	}
	t.Run("control: nothing holds it", func(t *testing.T) {
		now := zmBase
		robot := zmRobot("r1", "z1", fleetv1.RobotPhaseMaintenance, "")
		r, _ := newZMReconciler(t, &now, robot)
		if clear, err := r.estopClear(context.Background(), zmNS, robot); err != nil || !clear {
			t.Errorf("a robot no stop holds is not clear: %v %v", clear, err)
		}
	})
}

// staleGetClient answers a Get on one FleetAction with an older copy of it.
type staleGetClient struct {
	client.Client
	stale *fleetv1.FleetAction
}

func (s *staleGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if fa, ok := obj.(*fleetv1.FleetAction); ok && key.Name == s.stale.Name {
		s.stale.DeepCopyInto(fa)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// A window that read an action before an estop paused it must not mark it for a requeue:
// the write is made under the resourceVersion it read, so a read older than the pause
// writes nothing. A held action is never auto-requeued (RFC-0001 #fleetaction-state-machine).
func TestZM_RequeueMarkIsNotWrittenFromAReadOlderThanThePause(t *testing.T) {
	now := zmBase
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: zmNS, Name: "a-held"},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, AssignedRobot: "r1"},
	}
	r, c := newZMReconciler(t, &now, action)
	ctx := context.Background()
	stale := &fleetv1.FleetAction{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(action), stale); err != nil {
		t.Fatal(err)
	}
	// The estop pause lands after the window's read.
	paused := stale.DeepCopy()
	paused.Status.Phase = fleetv1.ActionPhasePaused
	paused.Status.Message = estopPausedInProgressMessage
	if err := c.Update(ctx, paused); err != nil {
		t.Fatal(err)
	}
	r.Client = &staleGetClient{Client: c, stale: stale}
	if err := r.requeueAction(ctx, zmNS, "a-held", "floor inspection", "win"); err != nil {
		t.Fatalf("requeueAction: %v", err)
	}
	got := &fleetv1.FleetAction{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(action), got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got.Annotations[annRequeueRequested]; ok {
		t.Errorf("a window marked the estop-paused action for a requeue (%q) from a read older than the pause", v)
	}
}
