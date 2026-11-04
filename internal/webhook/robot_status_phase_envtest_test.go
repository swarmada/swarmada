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

package webhook

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The operator's resume reads status.phase as well as the hardware, the models and the
// grants: it refuses while the control plane has marked the robot Error or Offline, and
// it derives a pauseable capability as Paused, which does not hold the release, while
// the robot is in Maintenance. A forged Maintenance let a resume pass with a capability
// that was Active at the stop now Degraded. So a phase change into or out of
// Maintenance or Offline is the control plane's to write; leaving Error already was
// (RFC-0001 #safety-estop-state-machine).
func TestEnvtest_OnlyTheControlPlaneMovesAPhaseTheResumeReads(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	if err := w.admin.Create(ctx, markerRobot(w.ns, "amr-1", nil)); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}
	bindShippedClusterRole(t, w, "swarmada_admin_role.yaml", map[string]interface{}{
		"kind": "User", "name": "ops-admin", "apiGroup": "rbac.authorization.k8s.io"})
	admin := asIdentity(t, w, "ops-admin")
	manager := w.as(t, controllerUser)
	setPhase := func(c client.Client, p fleetv1.RobotPhase) error {
		return patchStatus(c, w.ns, func(r *fleetv1.Robot) { r.Status.Phase = p })
	}
	phase := func(t *testing.T) fleetv1.RobotPhase {
		t.Helper()
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "amr-1"}, &r); err != nil {
			t.Fatalf("reading the robot: %v", err)
		}
		return r.Status.Phase
	}
	// The stop the forgery is aimed at.
	if err := patchStatus(manager, w.ns, func(r *fleetv1.Robot) {
		r.Status.EstopState = fleetv1.RobotEstopResuming
		r.Status.Phase = fleetv1.RobotPhaseIdle
	}); err != nil {
		t.Fatalf("the control plane's own write was refused: %v", err)
	}

	for _, tc := range []struct {
		name     string
		from, to fleetv1.RobotPhase
	}{
		{"IntoMaintenance", fleetv1.RobotPhaseIdle, fleetv1.RobotPhaseMaintenance},
		{"OutOfMaintenance", fleetv1.RobotPhaseMaintenance, fleetv1.RobotPhaseIdle},
		{"IntoOffline", fleetv1.RobotPhaseIdle, fleetv1.RobotPhaseOffline},
		{"OutOfOffline", fleetv1.RobotPhaseOffline, fleetv1.RobotPhaseIdle},
		{"MaintenanceToOffline", fleetv1.RobotPhaseMaintenance, fleetv1.RobotPhaseOffline},
	} {
		t.Run("Admin"+tc.name+"Refused", func(t *testing.T) {
			if err := setPhase(manager, tc.from); err != nil {
				t.Fatalf("the control plane could not put the robot in %s: %v", tc.from, err)
			}
			err := setPhase(admin, tc.to)
			if err == nil {
				t.Fatalf("swarmada:admin moved status.phase %s -> %s", tc.from, tc.to)
			}
			for _, want := range []string{"status.phase", string(tc.from), string(tc.to), "swarmctl estop resume"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			if got := phase(t); got != tc.from {
				t.Fatalf("the refused write landed anyway: phase = %s, want %s", got, tc.from)
			}
		})
		t.Run("ControlPlane"+tc.name+"Admitted", func(t *testing.T) {
			if err := setPhase(manager, tc.from); err != nil {
				t.Fatalf("the control plane could not put the robot in %s: %v", tc.from, err)
			}
			if err := setPhase(manager, tc.to); err != nil {
				t.Fatalf("the control plane's own phase write %s -> %s was refused: %v", tc.from, tc.to, err)
			}
		})
	}

	// A phase change that touches none of the three stays writable.
	t.Run("AdminIdleToChargingAdmitted", func(t *testing.T) {
		if err := setPhase(manager, fleetv1.RobotPhaseIdle); err != nil {
			t.Fatalf("reseeding: %v", err)
		}
		if err := setPhase(admin, fleetv1.RobotPhaseCharging); err != nil {
			t.Fatalf("a phase write the resume does not read was refused: %v", err)
		}
	})
}
