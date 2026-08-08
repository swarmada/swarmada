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
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// fakePusher records model_update pushes and returns a scripted outcome/error.
type fakePusher struct {
	ack     bool
	err     error
	pushes  []command.ModelUpdate
	targets []string
}

func (f *fakePusher) PushModelUpdate(_ context.Context, _, robotID string, u command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	f.pushes = append(f.pushes, u)
	f.targets = append(f.targets, robotID)
	if f.err != nil {
		return command.ModelUpdateOutcome{}, f.err
	}
	return command.ModelUpdateOutcome{Acknowledged: f.ack}, nil
}

// With a Pusher that acknowledges, the robot enters the batch (marked Updating)
// and the pushed payload carries the rollout's model identity.
func TestModelRollout_PushAcknowledgedEntersBatch(t *testing.T) {
	pusher := &fakePusher{ack: true}
	r, c := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = pusher
	reconcileRollout(t, r)

	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Fatal("acknowledged push should have entered the robot into the batch (Updating)")
	}
	if len(pusher.pushes) != 1 || pusher.targets[0] != "r1" {
		t.Fatalf("pushes = %+v targets = %v", pusher.pushes, pusher.targets)
	}
	if got := pusher.pushes[0]; got.NewVersion != "3.2.1" || got.ModelURI != "oci://registry/models/item-recognition:3.2.1" || got.ModelName != "item-recognition" {
		t.Errorf("pushed payload = %+v", got)
	}
}

// A push that cannot be delivered (unreachable) must NOT suspend capabilities:
// the robot stays out of the batch and is retried.
func TestModelRollout_PushUnreachableDefersRobot(t *testing.T) {
	pusher := &fakePusher{err: command.ErrUnreachable}
	r, c := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = pusher

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "roll", Namespace: rolloutNS}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
		t.Fatal("an undeliverable push must not mark the model Updating (never suspend capabilities)")
	}
	if res.RequeueAfter <= 0 {
		t.Error("a deferred push should requeue for retry")
	}
	if len(pusher.pushes) != 1 {
		t.Errorf("expected one push attempt, got %d", len(pusher.pushes))
	}
}

// A push the adapter declines (not acknowledged) also defers the robot.
func TestModelRollout_PushDeclinedDefersRobot(t *testing.T) {
	pusher := &fakePusher{ack: false}
	r, c := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = pusher
	reconcileRollout(t, r)

	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
		t.Fatal("a declined push must not mark the model Updating")
	}
}

// A deferred robot must not consume a maxUnavailable slot: with two eligible
// robots, one unreachable and one reachable, the reachable one still enters.
func TestModelRollout_DeferredRobotDoesNotConsumeSlot(t *testing.T) {
	// Pusher acks r-ok but is unreachable for r-bad. Route by robot id.
	pusher := &routingPusher{ackFor: map[string]bool{"r-ok": true}}
	r, c := newRolloutReconciler(t, pickerRollout(),
		targetRobot("r-bad", fleetv1.RobotPhaseIdle, 90),
		targetRobot("r-ok", fleetv1.RobotPhaseIdle, 90),
	)
	r.Pusher = pusher
	reconcileRollout(t, r)

	if e := modelEntry(rolloutRobot(t, c, "r-ok"), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Fatal("the reachable robot should have entered even though another was deferred (maxUnavailable=1)")
	}
	if e := modelEntry(rolloutRobot(t, c, "r-bad"), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
		t.Fatal("the unreachable robot must not have entered the batch")
	}
}

// routingPusher acknowledges only robots in ackFor; all others are unreachable.
type routingPusher struct {
	ackFor map[string]bool
}

func (p *routingPusher) PushModelUpdate(_ context.Context, _, robotID string, _ command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	if p.ackFor[robotID] {
		return command.ModelUpdateOutcome{Acknowledged: true}, nil
	}
	return command.ModelUpdateOutcome{}, errors.New("unreachable")
}
