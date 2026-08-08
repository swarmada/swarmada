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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func zmBoolPtr(b bool) *bool { return &b }

func zmMaintConfig(requireClear *bool) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: zmNS},
		Spec:       fleetv1.SwarmadaConfigSpec{Maintenance: fleetv1.SwarmadaMaintenanceConfig{RequireEstopClearBeforeResume: requireClear}},
	}
}

// The effective gate is resolved spec → SwarmadaConfig.spec.maintenance → true.
// (In a real cluster the spec field's own CRD default of true shadows the config
// branch; the resolver still honours config when the spec pointer is unset.)
func TestZM_ResumeGate_DefaultFromConfig(t *testing.T) {
	now := zmBase
	cases := []struct {
		name    string
		specVal *bool
		cfg     *bool // nil ⇒ no SwarmadaConfig present
		hasCfg  bool
		want    bool
	}{
		{"spec unset, config true ⇒ gated", nil, zmBoolPtr(true), true, true},
		{"spec unset, config false ⇒ ungated", nil, zmBoolPtr(false), true, false},
		{"spec false overrides config true", zmBoolPtr(false), zmBoolPtr(true), true, false},
		{"spec unset, no config ⇒ default true", nil, nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zm := zoneMaint("m", fleetv1.ZoneMaintenanceSpec{
				Scope:                         fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
				RequireEstopClearBeforeResume: tc.specVal,
			})
			objs := []client.Object{zm}
			if tc.hasCfg {
				objs = append(objs, zmMaintConfig(tc.cfg))
			}
			r, _ := newZMReconciler(t, &now, objs...)
			if got := r.requireEstopClearBeforeResume(context.Background(), zm); got != tc.want {
				t.Fatalf("requireEstopClearBeforeResume = %v, want %v", got, tc.want)
			}
		})
	}
}

// setEstop updates a robot's observed estop state, preserving its phase.
func setEstop(t *testing.T, c client.Client, name string, state fleetv1.RobotEstopState) {
	t.Helper()
	rob := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: name}, rob); err != nil {
		t.Fatalf("get robot %q: %v", name, err)
	}
	rob.Status.EstopState = state
	if err := c.Status().Update(context.Background(), rob); err != nil {
		t.Fatalf("set estop on %q: %v", name, err)
	}
}

// With the gate on, an auto-resume is HELD while a paused robot's estop is not
// Clear: the maintenance stays Active, sets ResumeBlockedByEstop, and keeps the
// robot in Maintenance — an operational hold on the phase flip, not a safety stop.
func TestZM_ResumeGate_BlocksWhileEstopNotClear(t *testing.T) {
	now := zmBase
	zm := zoneMaint("gated", fleetv1.ZoneMaintenanceSpec{
		Scope:                         fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		AutoResumeAfterMinutes:        10,
		RequireEstopClearBeforeResume: zmBoolPtr(true),
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r1", "z1", fleetv1.RobotPhaseIdle, ""))
	driveToActive(t, r, "gated")

	// An estop lands on the paused robot during the maintenance window.
	setEstop(t, c, "r1", fleetv1.RobotEstopStopped)

	now = zmBase.Add(11 * time.Minute) // past the auto-resume deadline
	reconcileZM(t, r, "gated")

	if p := robotPhase(t, c, "r1"); p != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot phase = %s, want Maintenance (resume held by estop)", p)
	}
	got := getZM(t, c, "gated")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("ZM phase = %s, want Active (must not Complete while a robot is held)", got.Status.Phase)
	}
	cond := findCond(got.Status.Conditions, zmCondResumeBlockedByEstop)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ResumeBlockedByEstop = %+v, want True", cond)
	}
	if got.Status.PausedRobotsCount != 1 {
		t.Fatalf("PausedRobotsCount = %d, want 1 (robot still held)", got.Status.PausedRobotsCount)
	}
}

// Once the estop clears, the next reconcile resumes the robot and completes.
func TestZM_ResumeGate_CompletesOnceEstopClears(t *testing.T) {
	now := zmBase
	zm := zoneMaint("gated", fleetv1.ZoneMaintenanceSpec{
		Scope:                         fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		AutoResumeAfterMinutes:        10,
		RequireEstopClearBeforeResume: zmBoolPtr(true),
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r1", "z1", fleetv1.RobotPhaseIdle, ""))
	driveToActive(t, r, "gated")
	setEstop(t, c, "r1", fleetv1.RobotEstopStopped)
	now = zmBase.Add(11 * time.Minute)
	reconcileZM(t, r, "gated") // held

	setEstop(t, c, "r1", fleetv1.RobotEstopNormal) // operator cleared the estop
	reconcileZM(t, r, "gated")

	if p := robotPhase(t, c, "r1"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s, want Idle after estop cleared", p)
	}
	if got := getZM(t, c, "gated"); got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("ZM phase = %s, want Completed", got.Status.Phase)
	}
}

// With the gate OFF, auto-resume proceeds even though the robot's estop is set —
// it is purely an operational preference, not a safety interlock.
func TestZM_ResumeGate_OffResumesDespiteEstop(t *testing.T) {
	now := zmBase
	zm := zoneMaint("ungated", fleetv1.ZoneMaintenanceSpec{
		Scope:                         fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		AutoResumeAfterMinutes:        10,
		RequireEstopClearBeforeResume: zmBoolPtr(false),
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r1", "z1", fleetv1.RobotPhaseIdle, ""))
	driveToActive(t, r, "ungated")
	setEstop(t, c, "r1", fleetv1.RobotEstopStopped)
	now = zmBase.Add(11 * time.Minute)
	reconcileZM(t, r, "ungated")

	if p := robotPhase(t, c, "r1"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s, want Idle (gate off ⇒ resume proceeds)", p)
	}
	if got := getZM(t, c, "ungated"); got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("ZM phase = %s, want Completed", got.Status.Phase)
	}
}

// Deletion-driven resume is NEVER gated: a robot with a set estop still resumes on
// delete so the finalizer always releases.
func TestZM_ResumeGate_DeletionNeverGated(t *testing.T) {
	now := zmBase
	zm := zoneMaint("del", fleetv1.ZoneMaintenanceSpec{
		Scope:                         fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		RequireEstopClearBeforeResume: zmBoolPtr(true),
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r1", "z1", fleetv1.RobotPhaseIdle, ""))
	driveToActive(t, r, "del")
	setEstop(t, c, "r1", fleetv1.RobotEstopStopped)

	if err := c.Delete(context.Background(), getZM(t, c, "del")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reconcileZM(t, r, "del")

	if p := robotPhase(t, c, "r1"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s, want Idle (deletion resume must never be gated)", p)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "del"}, &fleetv1.ZoneMaintenance{}); err == nil {
		t.Error("ZoneMaintenance still exists — finalizer wedged by the gate")
	}
}

// The convenience counts mirror the paused/winding-down lists.
func TestZM_CountsPopulated(t *testing.T) {
	now := zmBase
	zm := zoneMaint("counts", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		Mode:  fleetv1.ZoneMaintenanceModeGraceful,
	})
	r, c := newZMReconciler(t, &now, zm,
		zmRobot("idle-1", "z1", fleetv1.RobotPhaseIdle, ""),
		zmRobot("busy-1", "z1", fleetv1.RobotPhaseInProgress, "task-1"),
	)
	driveToActive(t, r, "counts")

	got := getZM(t, c, "counts")
	if got.Status.PausedRobotsCount != int32(len(got.Status.PausedRobots)) || got.Status.PausedRobotsCount != 1 {
		t.Fatalf("PausedRobotsCount = %d, want 1 (== len PausedRobots)", got.Status.PausedRobotsCount)
	}
	if got.Status.WindingDownRobotsCount != int32(len(got.Status.WindingDownRobots)) || got.Status.WindingDownRobotsCount != 1 {
		t.Fatalf("WindingDownRobotsCount = %d, want 1 (== len WindingDownRobots)", got.Status.WindingDownRobotsCount)
	}
}
