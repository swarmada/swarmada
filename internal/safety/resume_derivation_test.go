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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// The frame that witnesses a resume is also evidence about the robot's hardware, and
// the leave condition must be judged on what THAT frame says (RFC-0001
// #safety-estop-state-machine: "evaluated against telemetry projected since the clear,
// so a pre-stop projection cannot satisfy it").
//
// The telemetry sink writes status.hardware; status.capabilities is derived from it
// later, by the Robot reconciler, in a reconcile of its own. Nothing orders that
// reconcile before the robot-estop controller's, and both run on the same status
// event. So for a moment the stored Robot says "lidar Failed" in hardware and
// "navigation Active" in capabilities — the capability list is the pre-stop
// derivation — and a confirmation that reads the stored list releases the robot on
// the very state its witnessing frame contradicts.

// clearedWithNavigation stops and clears amr-1, whose navigation capability needs the
// lidar, over a projection that read healthy before the stop, and arms the window.
func clearedWithNavigation(t *testing.T) (*Dispatcher, client.Client, time.Time) {
	t.Helper()
	d, c, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), stoppedSender(d))
	ctx := context.Background()
	clearedAt := time.Now()

	robot := &fleetv1.Robot{}
	if err := c.Get(ctx, client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatal(err)
	}
	robot.Spec.Capabilities = []fleetv1.ClassCapability{{Name: "navigation", Type: fleetv1.CapabilityKindHardwareNative, RequiredHardware: []string{"lidar"}}}
	if err := c.Update(ctx, robot); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "lidar", Status: fleetv1.HardwareHealthy}}
	if err := c.Status().Update(ctx, robot); err != nil {
		t.Fatal(err)
	}
	projected(t, d, c, clearedAt.Add(-time.Minute), fleetv1.RobotPhaseIdle, activeCap("navigation"))

	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	if st, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", st, err)
	}
	return d, c, clearedAt
}

// postClearHardwareFrame is exactly what the telemetry sink does with a post-clear
// frame — a merge patch of status.hardware and nothing else — followed by the
// witness recording that the frame was received.
func postClearHardwareFrame(t *testing.T, d *Dispatcher, c client.Client, at time.Time, lidar fleetv1.HardwareStatus) {
	t.Helper()
	ctx := context.Background()
	robot := &fleetv1.Robot{}
	if err := c.Get(ctx, client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatal(err)
	}
	orig := robot.DeepCopy()
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "lidar", Status: lidar}}
	if err := c.Status().Patch(ctx, robot, client.MergeFrom(orig)); err != nil {
		t.Fatal(err)
	}
	d.Telemetry.(*testFrames).heard(ns, "amr-1", at)
}

// A post-clear frame reporting the lidar Failed must not release the robot while
// the stored capability list still carries the pre-stop derivation.
func TestResume_StaleCapabilitiesDoNotLeaveTheWindow(t *testing.T) {
	d, c, clearedAt := clearedWithNavigation(t)
	ctx := context.Background()

	postClearHardwareFrame(t, d, c, clearedAt.Add(time.Minute), fleetv1.HardwareFailed)

	got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin")
	if err != nil {
		t.Fatalf("ConfirmResume: %v", err)
	}
	if got != fleetv1.RobotEstopResuming || estopState(t, c) != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume = %s, stored %s; want Resuming: the frame that witnessed the resume reports "+
			"the lidar Failed, and the stored capabilities it would release on predate that frame",
			got, estopState(t, c))
	}
}

// A frame reporting the lidar Healthy again releases the robot, exactly once.
func TestResume_RecoveredHardwareLeavesTheWindowOnce(t *testing.T) {
	d, c, clearedAt := clearedWithNavigation(t)
	ctx := context.Background()

	postClearHardwareFrame(t, d, c, clearedAt.Add(time.Minute), fleetv1.HardwareFailed)
	if got, _, _ := d.ConfirmResume(ctx, ns, "amr-1", "admin"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume on a Failed lidar = %s, want Resuming", got)
	}
	postClearHardwareFrame(t, d, c, clearedAt.Add(2*time.Minute), fleetv1.HardwareHealthy)
	if got, _, err := d.ConfirmResume(ctx, ns, "amr-1", "admin"); err != nil || got != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume on a recovered lidar = %s (err %v), want Normal", got, err)
	}
	var before fleetv1.Robot
	if err := c.Get(ctx, client.ObjectKey{Name: "amr-1", Namespace: ns}, &before); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := d.ConfirmResume(ctx, ns, "amr-1", "admin"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("a second confirmation = %s, want Normal (no-op)", got)
	}
	var after fleetv1.Robot
	if err := c.Get(ctx, client.ObjectKey{Name: "amr-1", Namespace: ns}, &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("resourceVersion %s -> %s: the released robot was written again", before.ResourceVersion, after.ResourceVersion)
	}
}

// Without a derivation the condition cannot be judged, and the robot stays in the
// window: an unwired dispatcher withholds a robot, it does not release one.
func TestResume_NoDerivationLeavesTheRobotInTheWindow(t *testing.T) {
	d, c, clearedAt := clearedWithNavigation(t)
	d.DeriveCapabilities = nil
	postClearHardwareFrame(t, d, c, clearedAt.Add(time.Minute), fleetv1.HardwareHealthy)
	if got, _, _ := d.ConfirmResume(context.Background(), ns, "amr-1", "admin"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume without a derivation = %s, want Resuming", got)
	}
}

// testDerivation stands in for the Robot reconciler's derivation in this package,
// which cannot import it: a declared capability is Active unless a component it
// requires is reported Failed, Disabled or not at all. The controller package tests
// the real derivation against this confirmation in a running manager.
func testDerivation(robot *fleetv1.Robot) []fleetv1.CapabilityStatusEntry {
	hw := map[string]fleetv1.HardwareStatus{}
	for _, h := range robot.Status.Hardware {
		hw[h.Name] = h.Status
	}
	var out []fleetv1.CapabilityStatusEntry
	for _, capDef := range robot.Spec.Capabilities {
		st := fleetv1.CapabilityStatusActive
		for _, name := range capDef.RequiredHardware {
			if s, ok := hw[name]; !ok || s == fleetv1.HardwareFailed || s == fleetv1.HardwareDisabled {
				st = fleetv1.CapabilityStatusInactive
			}
		}
		out = append(out, fleetv1.CapabilityStatusEntry{Name: capDef.Name, Status: st})
	}
	return out
}
