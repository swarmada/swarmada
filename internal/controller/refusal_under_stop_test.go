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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Row A24 (RFC-0001 #safety-action-table): "A refusal under a stop, a refusal under the adapter's
// latch (I6) included, moves nothing. A1 has paused the action, and it stays with the robot until an
// operator decides (I1)." The stop here is issued after the candidate passed filter 10 and before the
// adapter answers, so dispatch cannot have seen it; only the refusal path can. Each scope is issued
// the way an operator issues it, the trigger annotation alone, so it is not yet recorded on the
// Robot; the robot-recorded case is the robot's own stop already written (estopState Stopping), and
// robot-resuming a stop cleared and the resume not yet confirmed (estopHoldsWork).

// stopThenRefuseCommander issues a stop while the AssignAction is in flight, then refuses it, as
// an adapter whose latch now holds the stop's fence does.
type stopThenRefuseCommander struct {
	fakeCommander
	issue func(ctx context.Context) error
}

func (f *stopThenRefuseCommander) PushAssignAction(
	ctx context.Context, _, robotID string, a command.AssignAction,
) (command.AssignActionOutcome, error) {
	f.assigns = append(f.assigns, a)
	f.assignTo = append(f.assignTo, robotID)
	if f.issue != nil {
		if err := f.issue(ctx); err != nil {
			return command.AssignActionOutcome{}, err
		}
	}
	return command.AssignActionOutcome{Accepted: false, Rejection: "ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE"}, nil
}

// refusalRig is robot r1 in zone z, a zoned action t1, the namespace's SwarmadaConfig, and a zone
// "out" r1 is not in. Nothing is stopped when dispatch runs.
func refusalRig(t *testing.T) (*FleetActionReconciler, client.Client, *stopThenRefuseCommander) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(readyActionAdapter(), stopZone("z", "", false, false, nil), stopZone("out", "", false, false, nil),
			&fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: actionNS}},
			idleRobotInZone("r1", "z"), zonedAction("t1", "z")).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	fc := &stopThenRefuseCommander{}
	return &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler(), Commander: fc}, c, fc
}

// triggerOn returns an issue hook that writes the stop's trigger annotation on obj.
func triggerOn(c client.Client, obj client.Object, name string) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: name}, obj); err != nil {
			return err
		}
		base := obj.DeepCopyObject().(client.Object)
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[annEstopTriggered] = "hazard"
		obj.SetAnnotations(ann)
		return c.Patch(ctx, obj, client.MergeFrom(base))
	}
}

// estopStateOn returns an issue hook that writes r1's estop state.
func estopStateOn(c client.Client, state fleetv1.RobotEstopState) func(context.Context) error {
	return func(ctx context.Context) error {
		rb := &fleetv1.Robot{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: "r1"}, rb); err != nil {
			return err
		}
		base := rb.DeepCopy()
		rb.Status.EstopState = state
		return c.Status().Patch(ctx, rb, client.MergeFrom(base))
	}
}

func refusalRobot(t *testing.T, c client.Client) *fleetv1.Robot {
	t.Helper()
	rb := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: "r1"}, rb); err != nil {
		t.Fatal(err)
	}
	return rb
}

// A refusal under a robot's, a zone's or the namespace's stop keeps the work with r1, and so
// does one while r1 is Resuming.
func TestRefusalUnderStop_TheWorkStaysWithItsRobot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue func(c client.Client) func(context.Context) error
	}{
		{"robot", func(c client.Client) func(context.Context) error { return triggerOn(c, &fleetv1.Robot{}, "r1") }},
		{"robot-recorded", func(c client.Client) func(context.Context) error { return estopStateOn(c, fleetv1.RobotEstopStopping) }},
		// Cleared, and the operator has not confirmed the resume: the stop still holds the work.
		{"robot-resuming", func(c client.Client) func(context.Context) error { return estopStateOn(c, fleetv1.RobotEstopResuming) }},
		{"zone", func(c client.Client) func(context.Context) error { return triggerOn(c, &fleetv1.FleetZone{}, "z") }},
		{"namespace", func(c client.Client) func(context.Context) error {
			return triggerOn(c, &fleetv1.SwarmadaConfig{}, SwarmadaConfigName)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, fc := refusalRig(t)
			fc.issue = tc.issue(c)
			reconcileAction(t, r, "t1")
			if len(fc.assigns) != 1 {
				t.Fatalf("%d AssignActions pushed, want 1: the stop is issued during the one dispatch", len(fc.assigns))
			}
			a := getAction(t, c, "t1")
			if a.Status.Phase != fleetv1.ActionPhasePaused || a.Status.AssignedRobot != "r1" {
				t.Fatalf("INVARIANT (A24, I1): after a refusal under a %s stop the action is %s on robot %q (%q); "+
					"want Paused on r1. A refusal under a stop moves nothing: A1 pauses the action and it stays "+
					"with the robot until an operator decides", tc.name, a.Status.Phase, a.Status.AssignedRobot, a.Status.Message)
			}
			if rb := refusalRobot(t, c); rb.Status.AssignedAction != "t1" {
				t.Fatalf("robot r1 records %q, want t1: the robot's record is not released under a stop", rb.Status.AssignedAction)
			}
			// The next pass does not move it either, and nothing is sent again.
			reconcileAction(t, r, "t1")
			a = getAction(t, c, "t1")
			if a.Status.Phase != fleetv1.ActionPhasePaused || a.Status.AssignedRobot != "r1" || len(fc.assigns) != 1 {
				t.Fatalf("second pass: %s on %q with %d AssignActions; want Paused on r1 with 1",
					a.Status.Phase, a.Status.AssignedRobot, len(fc.assigns))
			}
		})
	}
}

// The guards: a refusal under no stop that covers r1 is released as before, a stop on a zone r1 is
// not in included.
func TestRefusalUnderStop_NoStopReleasesAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue func(c client.Client) func(context.Context) error
	}{
		{"no-stop", func(client.Client) func(context.Context) error { return nil }},
		{"stop-elsewhere", func(c client.Client) func(context.Context) error { return triggerOn(c, &fleetv1.FleetZone{}, "out") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, fc := refusalRig(t)
			fc.issue = tc.issue(c)
			reconcileAction(t, r, "t1")
			a := getAction(t, c, "t1")
			if a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignedRobot != "" ||
				a.Status.Message != "adapter rejected the assignment; rescheduling" {
				t.Fatalf("refusal with no stop over r1: %s on %q (%q); want Pending, unbound, rescheduling",
					a.Status.Phase, a.Status.AssignedRobot, a.Status.Message)
			}
			if a.Status.AssignmentGeneration == 0 {
				t.Fatal("assignmentGeneration was not kept across the release")
			}
			if rb := refusalRobot(t, c); rb.Status.AssignedAction != "" || rb.Status.Phase != fleetv1.RobotPhaseIdle {
				t.Fatalf("robot r1 records %q in phase %s; want released to Idle", rb.Status.AssignedAction, rb.Status.Phase)
			}
		})
	}
}
