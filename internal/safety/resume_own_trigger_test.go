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
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// A robot that carries its own robot-scope trigger is held by it until that annotation
// is removed (RFC-0001 #safety-estop-scopes). A zone or namespace clear is not its clear,
// so a confirmation that would release it from Resuming is refused while the annotation
// is present, whatever moved it to Resuming and whatever the annotation's value. Every
// other check passes here, so the trigger alone is under test.
func TestResume_RefusedWhileTheRobotCarriesItsOwnTrigger(t *testing.T) {
	for _, own := range []string{"r1-own", ""} {
		t.Run("trigger="+own, func(t *testing.T) {
			d, c, _ := newDispatcher(t, "acme")
			d.RegisterStream(identity("acme"), stoppedSender(d))
			ctx := context.Background()
			clearedAt := time.Now()

			if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
				res.State != fleetv1.RobotEstopStopped {
				t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
			}
			if state, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil || state != fleetv1.RobotEstopResuming {
				t.Fatalf("ClearEstop = %s (err %v), want Resuming", state, err)
			}
			// The robot's own trigger is set while it is Resuming, before its stop is issued: no
			// clear moves a robot that carries one (S5), so this is how Resuming meets it.
			setOwnTrigger(t, c, &own)
			projected(t, d, c, clearedAt.Add(time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))

			got, why, err := d.ConfirmResume(ctx, ns, "amr-1", "admin")
			if err != nil || got != fleetv1.RobotEstopResuming {
				t.Fatalf("ConfirmResume with the robot's own trigger present = %s (err %v), want Resuming", got, err)
			}
			if !strings.Contains(why, "still held by its own robot-scope estop") {
				t.Errorf("refusal %q does not say the robot's own estop holds it", why)
			}
			if s := estopState(t, c); s != fleetv1.RobotEstopResuming {
				t.Fatalf("estopState = %s, want Resuming", s)
			}

			// Its own clear (the annotation removed), then a confirmation: Normal.
			setOwnTrigger(t, c, nil)
			if got, why, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopNormal {
				t.Fatalf("ConfirmResume after the robot's own clear = %s (%q, err %v), want Normal", got, why, err)
			}
		})
	}
}

// setOwnTrigger writes (val != nil) or removes the robot-scope trigger on amr-1.
func setOwnTrigger(t *testing.T, c client.Client, val *string) {
	t.Helper()
	// Read and written again on a conflict: the stop's record lands beside it, as on an API server.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		robot := &fleetv1.Robot{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
			return err
		}
		if val == nil {
			delete(robot.Annotations, "swarmada.io/estop-triggered")
		} else {
			robot.Annotations["swarmada.io/estop-triggered"] = *val
		}
		return c.Update(context.Background(), robot)
	}); err != nil {
		t.Fatal(err)
	}
}
