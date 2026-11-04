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

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// A robot paused by a maintenance window is estopped and cleared. The window's gated
// close waits for the robot's estop to be Normal (requireEstopClearBeforeResume), and
// the robot sits in Maintenance for as long as the window is open. The operator's
// release is judged on the robot's own reports, not on that phase, so it goes
// through; the window then closes and returns the robot to Idle. Neither waits on
// the other (RFC-0001 crds/zonemaintenance.md, #safety-estop-state-machine).
func TestZM_WindowAndEstopReleaseDoNotWaitOnEachOther(t *testing.T) {
	ctx := context.Background()
	now := zmBase
	zm := zoneMaint("m", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, Mode: fleetv1.ZoneMaintenanceModeGraceful,
	})
	robot := zmRobot("r1", "", fleetv1.RobotPhaseIdle, "")
	robot.Annotations = map[string]string{fleetv1.RobotIDAnnotation: "r1"}
	r, c := newZMReconciler(t, &now, zm, robot)
	driveToActive(t, r, "m")
	if p := robotPhase(t, c, "r1"); p != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("phase = %s, want Maintenance", p)
	}

	// Stopped, then cleared by an operator: Resuming.
	setRobotEstop(t, c, "r1", fleetv1.RobotEstopStopped)
	frames := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} }, &RobotStatusSink{Client: c})
	disp := safety.New(c, nil)
	disp.Telemetry = frames
	disp.DeriveCapabilities = DeriveCapabilities
	if st, err := disp.ClearEstop(ctx, zmNS, "r1", "alice", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", st, err)
	}

	// The operator closes the window. The gate holds it while the robot is Resuming.
	got := getZM(t, c, "m")
	got.Spec.DesiredState = fleetv1.ZoneMaintenanceDesiredStateCompleted
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	reconcileZM(t, r, "m")
	if ph := getZM(t, c, "m").Status.Phase; ph == fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("window phase = %s while the robot is still Resuming; the gate must hold it", ph)
	}

	// The robot reports after the clear; the operator releases it while it is still
	// in Maintenance.
	pct := int32(80)
	if err := frames.Ingest(ctx, telemetry.Frame{RobotID: "r1", Namespace: zmNS, Timestamp: time.Now(), BatteryPct: &pct,
		Phase: fleetv1.RobotPhaseIdle}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if st, why, err := disp.ConfirmResume(ctx, zmNS, "r1", "alice"); err != nil || st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume of a robot in Maintenance = %s (%q, err %v), want Normal", st, why, err)
	}

	// The window's close now completes and returns the robot to Idle.
	now = now.Add(time.Minute)
	reconcileZM(t, r, "m")
	reconcileZM(t, r, "m")
	if ph := getZM(t, c, "m").Status.Phase; ph != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("window phase = %s after the robot was released, want Completed", ph)
	}
	if p := robotPhase(t, c, "r1"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s after the window closed, want Idle", p)
	}
}

func setRobotEstop(t *testing.T, c client.Client, name string, state fleetv1.RobotEstopState) {
	t.Helper()
	robot := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: zmNS, Name: name}, robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.EstopState = state
	if err := c.Status().Update(context.Background(), robot); err != nil {
		t.Fatal(err)
	}
}
