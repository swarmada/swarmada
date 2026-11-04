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
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// The resume confirmation judges the robot against the stop it is leaving (RFC-0001
// #safety-estop-state-machine): the stop records which capabilities were Active when it
// was taken (status.estopBaseline), and those must be Active again.

func storedBaseline(t *testing.T, c client.Client) *fleetv1.EstopBaseline {
	t.Helper()
	robot := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatal(err)
	}
	return robot.Status.EstopBaseline
}

// The first stop of an episode records the capabilities Active at it; a fresh stop
// during Resuming keeps that record, so a capability lost during the stop still gates
// the release; Normal removes it.
func TestResume_TheStopRecordsTheCapabilitiesActiveAtIt(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), stoppedSender(d))
	ctx := context.Background()
	start := time.Now()
	projected(t, d, c, start.Add(-time.Minute), fleetv1.RobotPhaseIdle,
		activeCap("lidar"), fleetv1.CapabilityStatusEntry{Name: "camera", Status: fleetv1.CapabilityStatusInactive})

	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	if b := storedBaseline(t, c); b == nil || !slices.Equal(b.ActiveCapabilities, []string{"lidar"}) {
		t.Fatalf("estopBaseline = %+v, want lidar, the one capability Active at the stop", b)
	}

	if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatal(err)
	}
	// lidar is lost while Resuming, and a fresh stop is taken.
	projected(t, d, c, start, fleetv1.RobotPhaseIdle,
		fleetv1.CapabilityStatusEntry{Name: "lidar", Status: fleetv1.CapabilityStatusInactive})
	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard again", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("second TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	if b := storedBaseline(t, c); b == nil || !slices.Equal(b.ActiveCapabilities, []string{"lidar"}) {
		t.Fatalf("estopBaseline = %+v after a stop during Resuming, want the episode's record kept", b)
	}

	if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatal(err)
	}
	projected(t, d, c, time.Now().Add(time.Minute), fleetv1.RobotPhaseIdle,
		fleetv1.CapabilityStatusEntry{Name: "lidar", Status: fleetv1.CapabilityStatusInactive})
	if got, why, _ := d.ConfirmResume(ctx, ns, "amr-1", "admin"); got != fleetv1.RobotEstopResuming ||
		!strings.Contains(why, `"lidar" was Active when the stop was taken`) {
		t.Fatalf("ConfirmResume = %s (%q), want Resuming: lidar was Active at the episode's first stop", got, why)
	}

	projected(t, d, c, time.Now().Add(2*time.Minute), fleetv1.RobotPhaseIdle,
		activeCap("lidar"), fleetv1.CapabilityStatusEntry{Name: "camera", Status: fleetv1.CapabilityStatusInactive})
	if got, why, _ := d.ConfirmResume(ctx, ns, "amr-1", "admin"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (%q), want Normal", got, why)
	}
	if b := storedBaseline(t, c); b != nil {
		t.Fatalf("estopBaseline = %+v after Normal, want it removed", b)
	}
}

// The capability rule, row by row, against a stop that recorded navigation.
func TestResume_RefusalReadsTheStopRecord(t *testing.T) {
	recorded := &fleetv1.EstopBaseline{ActiveCapabilities: []string{"navigation"}}
	for _, tc := range []struct {
		name     string
		baseline *fleetv1.EstopBaseline
		derived  fleetv1.CapabilityStatusEntry
		refusal  string // empty: released
	}{
		{"Active at the stop, Failed now", recorded,
			fleetv1.CapabilityStatusEntry{Name: "navigation", Status: fleetv1.CapabilityStatusFailed},
			`capability "navigation" was Active when the stop was taken and is Failed on the reported hardware now`},
		{"Active at the stop, Active now", recorded, activeCap("navigation"), ""},
		{"not Active at the stop", recorded,
			fleetv1.CapabilityStatusEntry{Name: "camera", Status: fleetv1.CapabilityStatusInactive}, ""},
		{"Active at the stop, paused by a maintenance window now", recorded,
			fleetv1.CapabilityStatusEntry{Name: "navigation", Status: fleetv1.CapabilityStatusPaused, Paused: true}, ""},
		{"no record of the stop", nil,
			fleetv1.CapabilityStatusEntry{Name: "camera", Status: fleetv1.CapabilityStatusInactive},
			"this stop has no record of which capabilities were Active when it was taken"},
		{"no record of the stop, paused by a maintenance window", nil,
			fleetv1.CapabilityStatusEntry{Name: "camera", Status: fleetv1.CapabilityStatusPaused, Paused: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			robot := &fleetv1.Robot{}
			robot.Status.EstopBaseline = tc.baseline
			why, ok := resumeRefusal(robot, []fleetv1.CapabilityStatusEntry{tc.derived}, "")
			if tc.refusal == "" {
				if !ok {
					t.Fatalf("refused (%q), want released", why)
				}
				return
			}
			if ok || !strings.Contains(why, tc.refusal) {
				t.Fatalf("resumeRefusal = %v (%q), want refused with %q", ok, why, tc.refusal)
			}
		})
	}
}

// A confirmation is refused while the robot's most recent report has not reached
// Robot.status: the checks would read the status from before it.
func TestResume_RefusedWhileTheLatestReportIsUnwritten(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	ctx := context.Background()
	setState(t, c, fleetv1.RobotEstopStopped)
	if _, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil {
		t.Fatal(err)
	}
	projected(t, d, c, time.Now().Add(time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))
	frames := d.Telemetry.(*testFrames)
	frames.mu.Lock()
	frames.unwritten = map[string]bool{ns + "/amr-1": true}
	frames.mu.Unlock()

	if got, why, _ := d.ConfirmResume(ctx, ns, "amr-1", "admin"); got != fleetv1.RobotEstopResuming ||
		!strings.Contains(why, "has not reached Robot.status") {
		t.Fatalf("ConfirmResume = %s (%q), want Resuming refused on the unwritten report", got, why)
	}
	frames.mu.Lock()
	frames.unwritten = nil
	frames.mu.Unlock()
	if got, why, _ := d.ConfirmResume(ctx, ns, "amr-1", "admin"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (%q), want Normal once the report is written", got, why)
	}
}
