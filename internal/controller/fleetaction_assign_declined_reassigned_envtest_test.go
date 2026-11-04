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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// firstRobotDeclines is an adapter that refuses the first assignment it is sent, as
// INVALID_ACTION, and accepts every later one: the robot it was first sent to cannot run
// the action, and another robot can.
type firstRobotDeclines struct {
	fakeCommander
	mu       sync.Mutex
	declined string
	why      string
}

func (f *firstRobotDeclines) PushAssignAction(ctx context.Context, ns, robotID string,
	a command.AssignAction) (command.AssignActionOutcome, error) {
	out, err := f.fakeCommander.PushAssignAction(ctx, ns, robotID, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.declined == "" {
		f.declined = robotID
		return command.AssignActionOutcome{Accepted: false, Rejection: rejectionInvalidAction, Message: f.why}, nil
	}
	return out, err
}

// After robot X declines an assignment and the action is assigned to robot Y, the
// AssignmentDeclined Events on the FleetAction and on X, read back from the API server,
// still name X, the rejection and the adapter's reason. status.message no longer does:
// the assignment to Y cleared it, so the Events are the record.
func TestEnvtest_DeclinedAssignmentStillNamesTheDecliningRobotAfterReassignment(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	cs, err := kubernetes.NewForConfig(envCfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	t.Cleanup(broadcaster.Shutdown)

	const why = "this model does not implement Navigate; nothing was done"
	cmd := &firstRobotDeclines{why: why}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler(),
		Commander: cmd,
		Recorder:  broadcaster.NewRecorder(envScheme, corev1.EventSource{Component: "fleetaction-controller"})}

	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "aisle-1", "", nil, nil, true)
	f12SeedRobot(t, ns, "amr-1", "aisle-1")
	f12SeedRobot(t, ns, "amr-2", "aisle-1")
	req := zrSeedAction(t, ns, "pick-1", "aisle-1")

	var got *fleetv1.FleetAction
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		got = zrGetAction(t, req)
		if cmd.declined != "" && got.Status.AssignedRobot != "" && got.Status.AssignedRobot != cmd.declined {
			break
		}
	}
	x, y := cmd.declined, got.Status.AssignedRobot
	if x == "" {
		t.Fatal("no assignment was declined; the fixture never dispatched")
	}
	if y == "" || y == x {
		t.Fatalf("the action was not reassigned after %q declined it: assignedRobot=%q phase=%q message=%q",
			x, y, got.Status.Phase, got.Status.Message)
	}
	// The Events reach the API server asynchronously. Read them back after the reassignment.
	for _, target := range []struct{ kind, name string }{{"FleetAction", "pick-1"}, {"Robot", x}} {
		var e *corev1.Event
		deadline := time.Now().Add(10 * time.Second)
		for e == nil && time.Now().Before(deadline) {
			var list corev1.EventList
			if err := envK8s.List(ctx, &list, client.InNamespace(ns)); err != nil {
				t.Fatalf("list events: %v", err)
			}
			for i := range list.Items {
				ev := &list.Items[i]
				if ev.Reason == "AssignmentDeclined" && ev.InvolvedObject.Kind == target.kind &&
					ev.InvolvedObject.Name == target.name {
					e = ev
				}
			}
			if e == nil {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if e == nil {
			t.Fatalf("%s %s: no AssignmentDeclined Event after the action was assigned to %q; "+
				"nothing on the API server says that %q declined it", target.kind, target.name, y, x)
		}
		if e.Type != corev1.EventTypeWarning {
			t.Errorf("%s %s: Event type %s, want Warning", target.kind, target.name, e.Type)
		}
		for _, want := range []string{`"` + x + `"`, `"pick-1"`, "INVALID_ACTION", why} {
			if !strings.Contains(e.Message, want) {
				t.Errorf("%s %s: Event %q does not name %s", target.kind, target.name, e.Message, want)
			}
		}
		if strings.Contains(e.Message, `"`+y+`"`) {
			t.Errorf("%s %s: Event %q names %q, the robot that took the action, not the one that declined",
				target.kind, target.name, e.Message, y)
		}
	}
}
