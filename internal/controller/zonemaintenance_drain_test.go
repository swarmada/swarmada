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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func zmActionRequeued(t *testing.T, c client.Client, name string) (string, bool) {
	t.Helper()
	ft := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: name}, ft); err != nil {
		t.Fatal(err)
	}
	v, ok := ft.Annotations[annRequeueRequested]
	return v, ok
}

// ADR-0013: a Graceful maintenance leaves a action alone until the drain timeout
// elapses, then force-requeues it (like Immediate). Uses the built-in 300s default.
func TestZM_GracefulDrainTimeoutForcePauses(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		Mode:  fleetv1.ZoneMaintenanceModeGraceful, Reason: "db upgrade",
	})
	action := zmAction("task-1", fleetv1.ActionPhaseInProgress)
	rob := zmRobot("r-busy", "z1", fleetv1.RobotPhaseInProgress, "task-1")
	r, c := newZMReconciler(t, &now, zm, rob, action)

	driveToActive(t, r, "db-maint")

	// Before the deadline: the action is left to finish.
	if _, requeued := zmActionRequeued(t, c, "task-1"); requeued {
		t.Fatal("Graceful must not requeue before the drain timeout")
	}

	// Past the default 300s drain timeout: force-requeue.
	now = zmBase.Add(301 * time.Second)
	reconcileZM(t, r, "db-maint")
	if v, requeued := zmActionRequeued(t, c, "task-1"); !requeued || v != "db upgrade" {
		t.Fatalf("after drain timeout: requeue annotation = %q (present=%v), want 'db upgrade'", v, requeued)
	}
}

// ADR-0013: the drain timeout is sourced from SwarmadaConfig — a lower namespace
// value force-pauses sooner.
func TestZM_GracefulDrainTimeoutFromConfig(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		Mode:  fleetv1.ZoneMaintenanceModeGraceful, Reason: "db upgrade",
	})
	action := zmAction("task-1", fleetv1.ActionPhaseInProgress)
	rob := zmRobot("r-busy", "z1", fleetv1.RobotPhaseInProgress, "task-1")
	cfg := configWithSpec(zmNS, fleetv1.SwarmadaConfigSpec{
		Maintenance: fleetv1.SwarmadaMaintenanceConfig{DefaultGracefulDrainTimeoutSeconds: 60},
	})
	r, c := newZMReconciler(t, &now, zm, rob, action, cfg)

	driveToActive(t, r, "db-maint")

	// At 45s (< 60s) still winding down.
	now = zmBase.Add(45 * time.Second)
	reconcileZM(t, r, "db-maint")
	if _, requeued := zmActionRequeued(t, c, "task-1"); requeued {
		t.Fatal("must not requeue before the configured 60s drain timeout")
	}

	// At 61s (> 60s) force-requeue.
	now = zmBase.Add(61 * time.Second)
	reconcileZM(t, r, "db-maint")
	if _, requeued := zmActionRequeued(t, c, "task-1"); !requeued {
		t.Fatal("must requeue after the configured 60s drain timeout")
	}
}
