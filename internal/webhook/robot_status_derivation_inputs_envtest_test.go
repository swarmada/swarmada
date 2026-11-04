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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// The operator's resume re-derives the robot's capabilities from its stored status
// before it releases the stop: from every status.hardware entry, each installed model's
// status, and the capabilities a model rollout granted (RFC-0001
// #safety-estop-state-machine). The robots/status guard kept status.capabilities
// itself to the control plane, but not those inputs, so one write by another holder of
// robots/status turned a Degraded component Healthy and the resume passed on the
// forged value. The guard now keeps the inputs too.
func TestEnvtest_OnlyTheControlPlaneMovesTheResumeChecksInputs(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	robot := markerRobot(w.ns, "amr-1", map[string]string{fleetv1.RobotIDAnnotation: "amr-1"})
	robot.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}}
	robot.Spec.Capabilities = []fleetv1.ClassCapability{{
		Name: "navigation", Type: fleetv1.CapabilityKindHardwareNative, RequiredHardware: []string{"lidar"}}}
	if err := w.admin.Create(ctx, robot); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}
	bindShippedClusterRole(t, w, "swarmada_admin_role.yaml", map[string]interface{}{
		"kind": "User", "name": "ops-admin", "apiGroup": "rbac.authorization.k8s.io"})
	admin := asIdentity(t, w, "ops-admin")
	manager := w.as(t, controllerUser)

	// The stop, as the control plane took it: navigation Active on a healthy lidar, and a
	// model mid-rollout.
	if err := patchStatus(manager, w.ns, func(r *fleetv1.Robot) {
		r.Status.EstopState = fleetv1.RobotEstopStopped
		r.Status.EstopBaseline = &fleetv1.EstopBaseline{ActiveCapabilities: []string{"navigation"}, RecordedAt: metav1.Now()}
		r.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "lidar", Status: fleetv1.HardwareHealthy}}
		r.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{Name: "navigation", Status: fleetv1.CapabilityStatusActive}}
		r.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{Name: "picker-net", Status: fleetv1.ModelStatusUpdating}}
	}); err != nil {
		t.Fatalf("the control plane's stop was refused: %v", err)
	}

	// The control plane's estop dispatcher and telemetry path, writing as the manager.
	frames := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&controller.RobotStatusSink{Client: manager})
	dispatcher := safety.New(manager, nil)
	dispatcher.Telemetry = frames
	dispatcher.DeriveCapabilities = controller.DeriveCapabilities
	if st, err := dispatcher.ClearEstop(ctx, w.ns, "amr-1", "ops-admin", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("clearing the estop: state %s, err %v", st, err)
	}
	// The robot's own report after the clear: its lidar is Degraded.
	if err := frames.Ingest(ctx, telemetry.Frame{RobotID: "amr-1", Namespace: w.ns, Timestamp: time.Now(),
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareDegraded}}); err != nil {
		t.Fatalf("the robot's report: %v", err)
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
	t.Run("AdminHealsADegradedComponentRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.Hardware[0].Status = fleetv1.HardwareHealthy }),
			"status.hardware[lidar]")
	})
	// What the resume check reads is still what the robot reported, so it refuses as before.
	// (The forgery that released the robot was the heal just above.)
	t.Run("TheResumeCheckStillRefuses", func(t *testing.T) {
		st, why, err := dispatcher.ConfirmResume(ctx, w.ns, "amr-1", "ops-admin")
		if err != nil {
			t.Fatalf("confirming the resume: %v", err)
		}
		if st != fleetv1.RobotEstopResuming || !strings.Contains(why, "navigation") {
			t.Fatalf("the resume left the robot %s (%s); with the lidar reported Degraded it must stay Resuming, refused on navigation", st, why)
		}
	})

	t.Run("AdminAddsAComponentRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) {
			r.Status.Hardware = append(r.Status.Hardware, fleetv1.HardwareComponentStatus{Name: "camera", Status: fleetv1.HardwareHealthy})
		}), "status.hardware[camera]")
	})
	t.Run("AdminRemovesAComponentRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.Hardware = nil }), "status.hardware[lidar]")
	})
	t.Run("AdminActivatesAModelRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.InstalledModels[0].Status = fleetv1.ModelStatusActive }),
			"status.installedModels[picker-net]")
	})
	t.Run("AdminGrantsACapabilityRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) {
			r.Status.ModelGrantedCapabilities = []fleetv1.ModelGrantedCapabilityEntry{
				{ModelName: "picker-net", GrantedBy: "forged", Capabilities: []string{"item-pick"}}}
		}), "status.modelGrantedCapabilities")
	})

	// Every other status write is admitted: battery, and a component's other fields.
	t.Run("AdminOtherStatusWriteAdmitted", func(t *testing.T) {
		pct := int32(40)
		if err := patchStatus(admin, w.ns, func(r *fleetv1.Robot) {
			r.Status.BatteryPercent = &pct
			r.Status.Hardware[0].DegradationReason = "noted by an operator"
		}); err != nil {
			t.Fatalf("a write that changes no input of the resume check was refused: %v", err)
		}
	})

	// The control plane writes every one of them.
	t.Run("ControlPlaneWritesAdmitted", func(t *testing.T) {
		if err := patchStatus(manager, w.ns, func(r *fleetv1.Robot) {
			r.Status.Hardware = []fleetv1.HardwareComponentStatus{
				{Name: "lidar", Status: fleetv1.HardwareHealthy}, {Name: "camera", Status: fleetv1.HardwareHealthy}}
			r.Status.InstalledModels[0].Status = fleetv1.ModelStatusActive
			r.Status.ModelGrantedCapabilities = []fleetv1.ModelGrantedCapabilityEntry{
				{ModelName: "picker-net", GrantedBy: "rollout-1", Capabilities: []string{"item-pick"}}}
		}); err != nil {
			t.Fatalf("the control plane's own write was refused: %v", err)
		}
	})
}
