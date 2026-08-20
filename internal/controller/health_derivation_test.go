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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The aggregate health rule in RFC-0001 §9.3.3, which was specified in two places and
// implemented in neither: status.health had no writer at all, so swarmtop rendered a
// blank health column for every robot in every cluster.
//
//	Healthy  — all hardware Healthy; all pauseable:false capabilities Active
//	Degraded — >=1 component Degraded, none Failed; all pauseable:false capabilities Active
//	Critical — any component Failed, OR any pauseable:false capability not Active

func hw(name string, status fleetv1.HardwareStatus) fleetv1.HardwareComponentStatus {
	return fleetv1.HardwareComponentStatus{Name: name, Status: status}
}

func capEntry(name string, status fleetv1.CapabilityStatus) fleetv1.CapabilityStatusEntry {
	return fleetv1.CapabilityStatusEntry{Name: name, Status: status}
}

// healthRobot builds a Robot carrying the given derived status, with spec capability
// definitions supplying the pauseable flags the rule keys on.
func healthRobot(defs []fleetv1.ClassCapability, hardware []fleetv1.HardwareComponentStatus,
	caps []fleetv1.CapabilityStatusEntry) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: "warehouse-a"},
		Spec:       fleetv1.RobotSpec{Capabilities: defs},
		Status:     fleetv1.RobotStatus{Hardware: hardware, Capabilities: caps},
	}
}

func TestDeriveHealth_TruthTable(t *testing.T) {
	// "estop" is pauseable:false — the safety-critical class the scheduler may never
	// suspend, and the only kind of capability that gates health. "nav" is pauseable.
	defs := []fleetv1.ClassCapability{
		hwNative("estop", false, "brake"),
		hwNative("nav", true, "lidar"),
	}

	for _, tc := range []struct {
		name        string
		hardware    []fleetv1.HardwareComponentStatus
		caps        []fleetv1.CapabilityStatusEntry
		want        fleetv1.HealthState
		wantMessage string // substring; "" means the message must be empty
	}{
		{
			name:     "all healthy",
			hardware: []fleetv1.HardwareComponentStatus{hw("lidar", fleetv1.HardwareHealthy), hw("brake", fleetv1.HardwareHealthy)},
			caps:     []fleetv1.CapabilityStatusEntry{capEntry("estop", fleetv1.CapabilityStatusActive), capEntry("nav", fleetv1.CapabilityStatusActive)},
			want:     fleetv1.HealthStateHealthy,
		},
		{
			name:        "one component degraded",
			hardware:    []fleetv1.HardwareComponentStatus{hw("lidar", fleetv1.HardwareDegraded), hw("brake", fleetv1.HardwareHealthy)},
			caps:        []fleetv1.CapabilityStatusEntry{capEntry("estop", fleetv1.CapabilityStatusActive), capEntry("nav", fleetv1.CapabilityStatusDegraded)},
			want:        fleetv1.HealthStateDegraded,
			wantMessage: "lidar",
		},
		{
			name:        "a failed component outranks a degraded one",
			hardware:    []fleetv1.HardwareComponentStatus{hw("lidar", fleetv1.HardwareDegraded), hw("brake", fleetv1.HardwareFailed)},
			caps:        []fleetv1.CapabilityStatusEntry{capEntry("estop", fleetv1.CapabilityStatusActive)},
			want:        fleetv1.HealthStateCritical,
			wantMessage: "brake",
		},
		{
			// THE PAUSEABLE:FALSE EDGE. Hardware is entirely healthy; the robot is Critical
			// purely because a capability the scheduler may never suspend is not Active.
			name:        "non-pauseable capability lost with healthy hardware",
			hardware:    []fleetv1.HardwareComponentStatus{hw("lidar", fleetv1.HardwareHealthy), hw("brake", fleetv1.HardwareHealthy)},
			caps:        []fleetv1.CapabilityStatusEntry{capEntry("estop", fleetv1.CapabilityStatusFailed), capEntry("nav", fleetv1.CapabilityStatusActive)},
			want:        fleetv1.HealthStateCritical,
			wantMessage: "estop",
		},
		{
			// The mirror of the edge above: a PAUSEABLE capability going inactive is not a
			// health event. Treating it as one would make every maintenance pause read as a
			// hardware fault.
			name:     "pauseable capability lost is not a health event",
			hardware: []fleetv1.HardwareComponentStatus{hw("lidar", fleetv1.HardwareHealthy), hw("brake", fleetv1.HardwareHealthy)},
			caps:     []fleetv1.CapabilityStatusEntry{capEntry("estop", fleetv1.CapabilityStatusActive), capEntry("nav", fleetv1.CapabilityStatusUnavailable)},
			want:     fleetv1.HealthStateHealthy,
		},
		{
			// A Disabled component is "intentionally not in service ... benign, reversible,
			// and NOT critical" (api/v1/robot_types.go), so it is not a fault and not a
			// reason to withhold Healthy.
			name:     "a disabled component does not count against health",
			hardware: []fleetv1.HardwareComponentStatus{hw("lidar", fleetv1.HardwareDisabled), hw("brake", fleetv1.HardwareHealthy)},
			caps:     []fleetv1.CapabilityStatusEntry{capEntry("estop", fleetv1.CapabilityStatusActive)},
			want:     fleetv1.HealthStateHealthy,
		},
		{
			name:     "a robot reporting nothing yet is Healthy, not blank",
			hardware: nil,
			caps:     nil,
			want:     fleetv1.HealthStateHealthy,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveHealth(healthRobot(defs, tc.hardware, tc.caps))
			if got == nil {
				t.Fatal("deriveHealth returned nil; status.health must always be populated")
			}
			if got.Status != tc.want {
				t.Errorf("health = %s, want %s (message: %q)", got.Status, tc.want, got.Message)
			}
			if tc.wantMessage == "" {
				if got.Message != "" {
					t.Errorf("message = %q, want empty for a %s robot", got.Message, tc.want)
				}
				return
			}
			if !strings.Contains(got.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to name %q", got.Message, tc.wantMessage)
			}
		})
	}
}

// A model-granted capability is synthetic: it has no spec definition and therefore no
// pauseable flag, so it is not a "pauseable:false capability" and must not gate health.
// Without this, an additive model grant that went inactive would read as Critical.
func TestDeriveHealth_ModelGrantedCapabilityDoesNotGateHealth(t *testing.T) {
	r := healthRobot(
		[]fleetv1.ClassCapability{hwNative("estop", false, "brake")},
		[]fleetv1.HardwareComponentStatus{hw("brake", fleetv1.HardwareHealthy)},
		[]fleetv1.CapabilityStatusEntry{
			capEntry("estop", fleetv1.CapabilityStatusActive),
			capEntry("inspection.ai", fleetv1.CapabilityStatusFailed), // model-granted, undeclared
		},
	)
	if got := deriveHealth(r); got.Status != fleetv1.HealthStateHealthy {
		t.Errorf("health = %s (%q), want Healthy: an undeclared model-granted capability carries no pauseable flag",
			got.Status, got.Message)
	}
}

// The message must be a function of the SET of faults, not of telemetry arrival order.
// status.hardware[] is not sorted, and the reconciler persists on
// reflect.DeepEqual(original.Status, robot.Status) — an order-dependent message would
// differ on every reconcile and turn that guard into a write amplifier (RA-1).
func TestDeriveHealth_MessageIsOrderIndependent(t *testing.T) {
	defs := []fleetv1.ClassCapability{hwNative("estop", false, "brake")}
	caps := []fleetv1.CapabilityStatusEntry{capEntry("estop", fleetv1.CapabilityStatusActive)}

	a := deriveHealth(healthRobot(defs,
		[]fleetv1.HardwareComponentStatus{hw("lidar", fleetv1.HardwareFailed), hw("cam", fleetv1.HardwareFailed)}, caps))
	b := deriveHealth(healthRobot(defs,
		[]fleetv1.HardwareComponentStatus{hw("cam", fleetv1.HardwareFailed), hw("lidar", fleetv1.HardwareFailed)}, caps))

	if a.Message != b.Message {
		t.Errorf("message depends on hardware ordering:\n  %q\n  %q", a.Message, b.Message)
	}
}

// Reconciler integration, and the acceptance case for swarmtop: a reconcile must PERSIST
// a non-nil status.health. tools/swarmtop/internal/k8sclient/map.go:75 nil-guards the
// field (`if h := r.Status.Health; h != nil`), so while nothing wrote it every robot
// rendered a blank health column. This asserts what that guard now receives.
func TestRobotReconcile_PersistsHealth(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "warehouse-a"},
		Spec:       fleetv1.RobotSpec{Capabilities: []fleetv1.ClassCapability{hwNative("nav", true, "lidar")}},
		Status: fleetv1.RobotStatus{
			Phase: fleetv1.RobotPhaseIdle,
			Hardware: []fleetv1.HardwareComponentStatus{
				{Name: "lidar", Status: fleetv1.HardwareDegraded},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	r := &RobotReconciler{Client: c, Scheme: scheme}

	key := types.NamespacedName{Name: "r1", Namespace: "warehouse-a"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Health == nil {
		t.Fatal("status.health is nil after reconcile; swarmtop renders a blank health column")
	}
	if got.Status.Health.Status != fleetv1.HealthStateDegraded {
		t.Errorf("health = %s, want Degraded (lidar degraded)", got.Status.Health.Status)
	}
	if !strings.Contains(got.Status.Health.Message, "lidar") {
		t.Errorf("health message = %q, want it to name the degraded component", got.Status.Health.Message)
	}
}

// RA-1: health must cost no extra etcd write. The reconciler persists only on
// reflect.DeepEqual(original.Status, robot.Status), so a second reconcile over an
// already-converged robot must not write again. A health value that varied per
// reconcile would turn every telemetry tick into a status write across the fleet.
func TestRobotReconcile_HealthAddsNoExtraWrite(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "warehouse-a"},
		Spec:       fleetv1.RobotSpec{Capabilities: []fleetv1.ClassCapability{hwNative("estop", false, "brake")}},
		Status: fleetv1.RobotStatus{
			Phase: fleetv1.RobotPhaseIdle,
			Hardware: []fleetv1.HardwareComponentStatus{
				{Name: "brake", Status: fleetv1.HardwareFailed},
				{Name: "lidar", Status: fleetv1.HardwareFailed},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	r := &RobotReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Name: "r1", Namespace: "warehouse-a"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	after := &fleetv1.Robot{}
	if err := c.Get(context.Background(), key, after); err != nil {
		t.Fatalf("get: %v", err)
	}
	settled := after.ResourceVersion

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	again := &fleetv1.Robot{}
	if err := c.Get(context.Background(), key, again); err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.ResourceVersion != settled {
		t.Errorf("resourceVersion moved %s -> %s on a no-op reconcile; health is causing a write per tick",
			settled, again.ResourceVersion)
	}
}
