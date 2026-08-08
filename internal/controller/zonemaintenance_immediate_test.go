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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func zmAction(name string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: zmNS, Name: name},
		Status:     fleetv1.FleetActionStatus{Phase: phase},
	}
}

// Immediate mode forcibly requeues an in-progress robot's action by setting the
// requeue-requested annotation (which the FleetAction controller confirmed-stops).
func TestZM_ImmediateRequeuesInProgressAction(t *testing.T) {
	now := zmBase
	zm := zoneMaint("evac", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		Mode:  fleetv1.ZoneMaintenanceModeImmediate, Reason: "fire drill",
	})
	action := zmAction("task-1", fleetv1.ActionPhaseInProgress)
	rob := zmRobot("r-busy", "z1", fleetv1.RobotPhaseInProgress, "task-1")
	r, c := newZMReconciler(t, &now, zm, rob, action)

	driveToActive(t, r, "evac")

	ft := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "task-1"}, ft); err != nil {
		t.Fatal(err)
	}
	if ft.Annotations[annRequeueRequested] != "fire drill" {
		t.Fatalf("requeue annotation = %q, want 'fire drill'", ft.Annotations[annRequeueRequested])
	}
	// The robot is tracked as winding down until its action requeues and it goes Idle.
	if wd := getZM(t, c, "evac").Status.WindingDownRobots; len(wd) != 1 || wd[0].Name != "r-busy" {
		t.Fatalf("windingDownRobots = %+v", wd)
	}
}

// Graceful mode does NOT forcibly requeue — the action is left to finish.
func TestZM_GracefulDoesNotRequeue(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		Mode:  fleetv1.ZoneMaintenanceModeGraceful,
	})
	action := zmAction("task-1", fleetv1.ActionPhaseInProgress)
	rob := zmRobot("r-busy", "z1", fleetv1.RobotPhaseInProgress, "task-1")
	r, c := newZMReconciler(t, &now, zm, rob, action)

	driveToActive(t, r, "db-maint")

	ft := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "task-1"}, ft); err != nil {
		t.Fatal(err)
	}
	if _, requeued := ft.Annotations[annRequeueRequested]; requeued {
		t.Error("Graceful mode must not forcibly requeue the task")
	}
}
