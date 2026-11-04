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

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// An operator clear enters the post-clear supervised window; it does NOT release
// the robot to Normal. RFC-0001 #safety-estop-state-machine gives the machine as
// Stopped -> Resuming -> Normal and assigns the two edges to different actors: the
// control plane authors Resuming when the clear is accepted, and leaves it only
// when an operator confirms the robot's release. Clearing
// straight to Normal collapses the two and returns a robot to the scheduler's
// candidate set before anything has confirmed it can move.
func TestClearEstop_EntersResumingNotNormal(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	// Put the robot under a confirmed stop.
	setState(t, c, fleetv1.RobotEstopStopped)

	state, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{})
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if state != fleetv1.RobotEstopResuming {
		t.Fatalf("returned state = %s, want Resuming", state)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming after clear", got)
	}
}

// ClearEstop is a no-op when the robot is not under an estop.
func TestClearEstop_NoOpWhenNormal(t *testing.T) {
	d, _, _ := newDispatcher(t, "acme") // robot starts with empty (Normal) estopState

	state, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{})
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if state != fleetv1.RobotEstopNormal {
		t.Fatalf("returned state = %s, want Normal", state)
	}
}

// ClearEstop clears the Failed state too (a robot that could not be confirmed
// stopped) so it can be returned to service after operator inspection — and it
// goes through the same supervised window. A robot whose stop was never confirmed
// is the LAST one that should skip the interval in which its capabilities restore.
func TestClearEstop_ClearsFailedIntoResuming(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopFailed)

	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming", got)
	}
}

func setState(t *testing.T, c client.Client, state fleetv1.RobotEstopState) {
	t.Helper()
	// Read and written again on a conflict: the stop's record lands beside it, as on an API server.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		robot := &fleetv1.Robot{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
			return err
		}
		robot.Status.EstopState = state
		return c.Status().Update(context.Background(), robot)
	}); err != nil {
		t.Fatal(err)
	}
}
