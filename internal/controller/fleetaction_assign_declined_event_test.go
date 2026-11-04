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
	"fmt"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// A declined assignment is released and the action goes back to Pending, where another robot
// can take it, and status.message is cleared when it does. The robot that declined and the
// adapter's reason are kept as a Warning Event on the FleetAction and one on the Robot, so the
// robot that refused stays visible after the action has moved on.

// objectEvent is one Event as a recorder was asked to write it, with the object it is on.
type objectEvent struct {
	kind, name, eventType, reason, message string
}

// objectRecorder is a record.EventRecorder that keeps which object each Event is on;
// record.FakeRecorder keeps only the text.
type objectRecorder struct {
	mu     sync.Mutex
	events []objectEvent
}

func (o *objectRecorder) Event(obj runtime.Object, eventType, reason, message string) {
	kind := fmt.Sprintf("%T", obj)
	name := ""
	if m, err := meta.Accessor(obj); err == nil {
		name = m.GetName()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, objectEvent{kind, name, eventType, reason, message})
}

func (o *objectRecorder) Eventf(obj runtime.Object, eventType, reason, format string, args ...interface{}) {
	o.Event(obj, eventType, reason, fmt.Sprintf(format, args...))
}

func (o *objectRecorder) AnnotatedEventf(obj runtime.Object, _ map[string]string, eventType, reason, format string,
	args ...interface{}) {
	o.Eventf(obj, eventType, reason, format, args...)
}

func (o *objectRecorder) on(kind, name, reason string) []objectEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []objectEvent
	for _, e := range o.events {
		if strings.HasSuffix(e.kind, "."+kind) && e.name == name && e.reason == reason {
			out = append(out, e)
		}
	}
	return out
}

// decliningCommander declines every assignment with rejection and message, as an adapter
// that does not serve the robot does.
type decliningCommander struct {
	*fakeCommander
	rejection, message string
}

func (d *decliningCommander) PushAssignAction(ctx context.Context, ns, robotID string,
	a command.AssignAction) (command.AssignActionOutcome, error) {
	_, _ = d.fakeCommander.PushAssignAction(ctx, ns, robotID, a)
	return command.AssignActionOutcome{Accepted: false, Rejection: d.rejection, Message: d.message}, nil
}

func TestAssign_ADeclineIsRecordedOnTheActionAndTheRobot(t *testing.T) {
	const why = "robot_id 'r1' is not served by this adapter, which serves 'r0'; nothing was done"
	r, c := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	rec := &objectRecorder{}
	r.Recorder = rec
	r.Commander = &decliningCommander{fakeCommander: &fakeCommander{},
		rejection: "ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE", message: why}

	reconcileAction(t, r, "t1")

	if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhasePending || ft.Status.AssignedRobot != "" {
		t.Fatalf("declined assignment not released: phase=%s robot=%q", ft.Status.Phase, ft.Status.AssignedRobot)
	}
	for _, target := range []struct{ kind, name string }{{"FleetAction", "t1"}, {"Robot", "r1"}} {
		evs := rec.on(target.kind, target.name, "AssignmentDeclined")
		if len(evs) != 1 {
			t.Fatalf("%s %s: %d AssignmentDeclined Events, want 1 (all Events: %+v)",
				target.kind, target.name, len(evs), rec.events)
		}
		e := evs[0]
		if e.eventType != corev1.EventTypeWarning {
			t.Errorf("%s %s: Event type %s, want Warning", target.kind, target.name, e.eventType)
		}
		for _, want := range []string{`"r1"`, `"t1"`, "ROBOT_UNAVAILABLE", why} {
			if !strings.Contains(e.message, want) {
				t.Errorf("%s %s: Event %q does not name %s", target.kind, target.name, e.message, want)
			}
		}
	}
}

// An adapter that declines without a reason is recorded as such, not with an empty string.
func TestAssign_ADeclineWithNoReasonSaysSo(t *testing.T) {
	r, _ := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	rec := &objectRecorder{}
	r.Recorder = rec
	r.Commander = &decliningCommander{fakeCommander: &fakeCommander{},
		rejection: "ASSIGN_ACTION_REJECTION_ROBOT_BUSY"}

	reconcileAction(t, r, "t1")

	evs := rec.on("FleetAction", "t1", "AssignmentDeclined")
	if len(evs) != 1 || !strings.Contains(evs[0].message, "gave no reason") {
		t.Fatalf("a decline with no adapter message: %+v", evs)
	}
}
