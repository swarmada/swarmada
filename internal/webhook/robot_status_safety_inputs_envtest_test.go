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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A robot's safety inputs -- the Error halt, a Failed component and its capabilities --
// are the control plane's to write, from the robot's own reports over ControlStream
// (RFC-0001 #control-plane-scheduler, #safety-estop-state-machine). Two things keep other
// identities out: the Fleet Adapter's shipped role holds no robots/status write at all,
// which holds on every install, and the robots/status guard refuses the other holders of
// that write, swarmada:admin among them, where the webhooks are installed.

// The Fleet Adapter's shipped role cannot write robots/status: RBAC refuses it before
// admission is asked, so this holds on an install with no webhooks too.
func TestEnvtest_TheRobotRoleCannotWriteRobotStatus(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	if err := w.admin.Create(ctx, markerRobot(w.ns, "amr-1", nil)); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}
	bindShippedClusterRole(t, w, "swarmada_robot_role.yaml", map[string]interface{}{
		"kind": "ServiceAccount", "name": "acme-adapter", "namespace": w.ns})
	adapter := asIdentity(t, w, "system:serviceaccount:"+w.ns+":acme-adapter",
		"system:serviceaccounts", "system:serviceaccounts:"+w.ns)

	var robot fleetv1.Robot
	if err := adapter.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "amr-1"}, &robot); err != nil {
		t.Fatalf("the adapter's role no longer reads robots: %v", err)
	}
	pct := int32(64)
	err := patchStatus(adapter, w.ns, func(r *fleetv1.Robot) { r.Status.BatteryPercent = &pct })
	if err == nil {
		t.Fatalf("the adapter's service account wrote robots/status; its role must not grant it")
	}
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "robots/status") {
		t.Fatalf("refused, but not by RBAC on robots/status: %v", err)
	}
}

func TestEnvtest_OnlyTheControlPlaneMovesARobotsSafetyInputs(t *testing.T) {
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

	// What the control plane last wrote from the robot's reports: it is in Error, its
	// lidar Failed, navigation Active.
	seed := func(r *fleetv1.Robot) {
		r.Status.Phase = fleetv1.RobotPhaseError
		r.Status.Hardware = []fleetv1.HardwareComponentStatus{
			{Name: "lidar", Status: fleetv1.HardwareFailed},
			{Name: "camera", Status: fleetv1.HardwareHealthy},
		}
		r.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{Name: "navigation", Status: fleetv1.CapabilityStatusActive}}
	}
	reseed := func(t *testing.T) {
		t.Helper()
		if err := patchStatus(manager, w.ns, seed); err != nil {
			t.Fatalf("the control plane's own status write was refused: %v", err)
		}
	}
	stored := func(t *testing.T) fleetv1.RobotStatus {
		t.Helper()
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "amr-1"}, &r); err != nil {
			t.Fatalf("reading the robot: %v", err)
		}
		return r.Status
	}
	refused := func(t *testing.T, err error, field string) {
		t.Helper()
		if err == nil {
			t.Fatalf("swarmada:admin changed %s by a robots/status patch", field)
		}
		if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "swarmctl estop resume") {
			t.Errorf("refused, but the refusal does not name %s and the operator's resume: %v", field, err)
		}
	}
	reseed(t)

	t.Run("AdminLeavesErrorRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseIdle }),
			"status.phase")
		if got := stored(t).Phase; got != fleetv1.RobotPhaseError {
			t.Fatalf("phase = %s, want Error", got)
		}
	})
	t.Run("AdminClearsAFailedComponentRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.Hardware = r.Status.Hardware[1:] }),
			"status.hardware[lidar]")
	})
	t.Run("AdminHealsAFailedComponentRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.Hardware[0].Status = fleetv1.HardwareHealthy }),
			"status.hardware[lidar]")
		if got := stored(t).Hardware[0].Status; got != fleetv1.HardwareFailed {
			t.Fatalf("lidar = %s, want Failed", got)
		}
	})
	t.Run("AdminChangesCapabilitiesRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) {
			r.Status.Capabilities = append(r.Status.Capabilities,
				fleetv1.CapabilityStatusEntry{Name: "picking", Status: fleetv1.CapabilityStatusActive})
		}), "status.capabilities")
	})

	// Everything else stays writable: battery, a component's fields other than its state,
	// and a phase change that does not leave Error. (A component's state is an input of
	// the resume check; robot_status_derivation_inputs_envtest_test.go refuses it.)
	t.Run("AdminOtherStatusWriteAdmitted", func(t *testing.T) {
		pct := int32(40)
		if err := patchStatus(admin, w.ns, func(r *fleetv1.Robot) {
			r.Status.BatteryPercent = &pct
			r.Status.Hardware[1].DegradationReason = "noted by an operator"
		}); err != nil {
			t.Fatalf("a write that changes no safety input was refused: %v", err)
		}
	})

	// The control plane writes every one of them.
	t.Run("ControlPlaneWritesAdmitted", func(t *testing.T) {
		if err := patchStatus(manager, w.ns, func(r *fleetv1.Robot) {
			r.Status.Phase = fleetv1.RobotPhaseIdle
			r.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "camera", Status: fleetv1.HardwareHealthy}}
			r.Status.Capabilities = nil
		}); err != nil {
			t.Fatalf("the control plane's own write was refused: %v", err)
		}
	})
	t.Run("AdminMovesIntoErrorAdmitted", func(t *testing.T) {
		if err := patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseError }); err != nil {
			t.Fatalf("a write into Error was refused; only leaving it is guarded: %v", err)
		}
	})
}
