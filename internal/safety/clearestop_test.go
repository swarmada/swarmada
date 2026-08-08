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

package safety

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ClearEstop resets an estopped robot to Normal.
func TestClearEstop_ResetsToNormal(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	// Put the robot under a confirmed stop.
	setState(t, c, fleetv1.RobotEstopStopped)

	state, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin")
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if state != fleetv1.RobotEstopNormal {
		t.Fatalf("returned state = %s, want Normal", state)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s, want Normal after clear", got)
	}
}

// ClearEstop is a no-op when the robot is not under an estop.
func TestClearEstop_NoOpWhenNormal(t *testing.T) {
	d, _, _ := newDispatcher(t, "acme") // robot starts with empty (Normal) estopState

	state, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin")
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if state != fleetv1.RobotEstopNormal {
		t.Fatalf("returned state = %s, want Normal", state)
	}
}

// ClearEstop clears the Failed state too (a robot that could not be confirmed
// stopped) so it can be returned to service after operator inspection.
func TestClearEstop_ClearsFailed(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopFailed)

	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin"); err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s, want Normal", got)
	}
}

func setState(t *testing.T, c client.Client, state fleetv1.RobotEstopState) {
	t.Helper()
	robot := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.EstopState = state
	if err := c.Status().Update(context.Background(), robot); err != nil {
		t.Fatal(err)
	}
}
