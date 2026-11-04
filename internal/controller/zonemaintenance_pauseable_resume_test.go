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
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// The scenario of TestZM_WindowAndEstopReleaseDoNotWaitOnEachOther with one pauseable
// capability, as any real RobotClass declares. While the window holds the robot in
// Maintenance the capability derives Paused. It must not hold the release: the window's
// close waits for the release, so requiring the capability Active leaves each waiting
// on the other for ever.
func TestZM_APauseableCapabilityDoesNotHoldTheEstopRelease(t *testing.T) {
	ctx := context.Background()
	now := zmBase
	zm := zoneMaint("m", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, Mode: fleetv1.ZoneMaintenanceModeGraceful,
	})
	robot := zmRobot("r1", "", fleetv1.RobotPhaseIdle, "")
	robot.Annotations = map[string]string{fleetv1.RobotIDAnnotation: "r1"}
	robot.Spec.Capabilities = []fleetv1.ClassCapability{{Name: "transport", Type: fleetv1.CapabilityKindManual, Pauseable: true}}
	r, c := newZMReconciler(t, &now, zm, robot)
	driveToActive(t, r, "m")
	if got := robotPhase(t, c, "r1"); got != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot phase after activation = %s, want Maintenance", got)
	}

	setRobotEstop(t, c, "r1", fleetv1.RobotEstopStopped)
	frames := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} }, &RobotStatusSink{Client: c})
	disp := safety.New(c, nil)
	disp.Telemetry = frames
	disp.DeriveCapabilities = DeriveCapabilities
	if st, err := disp.ClearEstop(ctx, zmNS, "r1", "alice", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", st, err)
	}
	got := getZM(t, c, "m")
	got.Spec.DesiredState = fleetv1.ZoneMaintenanceDesiredStateCompleted
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	pct := int32(80)
	if err := frames.Ingest(ctx, telemetry.Frame{RobotID: "r1", Namespace: zmNS, Timestamp: time.Now(), BatteryPct: &pct,
		Phase: fleetv1.RobotPhaseIdle}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	st, why, err := disp.ConfirmResume(ctx, zmNS, "r1", "alice")
	if err != nil || st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (%q, err %v), want Normal: the capability is Paused by the window whose "+
			"close waits for this release", st, why, err)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(time.Minute)
		reconcileZM(t, r, "m")
	}
	if got := getZM(t, c, "m").Status.Phase; got != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("window phase = %s after the release and the close, want Completed", got)
	}
}
