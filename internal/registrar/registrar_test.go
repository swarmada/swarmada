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

package registrar

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

const (
	regNS      = "warehouse-a"
	regRobotID = "amr-07"
)

func newRegistrar(t *testing.T, objs ...client.Object) (*Registrar, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&fleetv1.DiscoveredRobot{}).
		WithObjects(objs...).
		Build()
	fixed := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	return &Registrar{Client: c, Now: func() time.Time { return fixed }}, c
}

func adapterID() controlstream.AdapterIdentity {
	return controlstream.AdapterIdentity{
		Namespace:      regNS,
		AdapterVersion: "1.4.2",
		PeerAddr:       "10.0.0.5:52412",
	}
}

func getDR(t *testing.T, c client.Client, name string) *fleetv1.DiscoveredRobot {
	t.Helper()
	dr := &fleetv1.DiscoveredRobot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: regNS, Name: name}, dr); err != nil {
		t.Fatalf("getting DiscoveredRobot %q: %v", name, err)
	}
	return dr
}

func TestDiscover_CreatesDiscoveredRobotWithMac(t *testing.T) {
	r, c := newRegistrar(t)
	mac := "aa:bb:cc:dd:ee:ff"
	msg := &fav1.DiscoverRobot{
		RobotId:              regRobotID,
		Manufacturer:         "Acme",
		Model:                "Hauler-3000",
		FirmwareVersion:      "2.1.0",
		Mac:                  &mac,
		ReportedCapabilities: []string{"navigate", "transport"},
		Hardware: []*fav1.HardwareComponent{
			{Name: "front-lidar", Type: "Lidar", Model: "RPLIDAR-S2", Status: fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY},
			{Name: "odd-sensor", Type: "Warp-Drive", Status: fav1.HardwareStatus_HARDWARE_STATUS_DEGRADED},
		},
		InstalledModels: []*fav1.InstalledModel{
			{Name: "pick-net", Version: "4.1.0", Status: fav1.ModelStatus_MODEL_STATUS_ACTIVE},
		},
	}

	ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, msg)
	if !ack.GetAccepted() {
		t.Fatalf("Discover not accepted: %+v", ack)
	}
	if ack.GetDiscoveredRobotName() != regRobotID {
		t.Fatalf("discovered_robot_name = %q, want %q", ack.GetDiscoveredRobotName(), regRobotID)
	}

	dr := getDR(t, c, regRobotID)
	if dr.Status.Phase != fleetv1.DiscoveredRobotPhaseDiscovered {
		t.Errorf("phase = %q, want Discovered", dr.Status.Phase)
	}
	if dr.Status.MacAddress != mac {
		t.Errorf("macAddress = %q, want %q", dr.Status.MacAddress, mac)
	}
	if dr.Status.AdapterAddress != "10.0.0.5:52412" || dr.Status.AdapterVersion != "1.4.2" {
		t.Errorf("adapter fields = %q/%q", dr.Status.AdapterAddress, dr.Status.AdapterVersion)
	}
	if dr.Status.Manufacturer != "Acme" || dr.Status.Model != "Hauler-3000" || dr.Status.FirmwareVersion != "2.1.0" {
		t.Errorf("identity fields = %+v", dr.Status)
	}
	if dr.Status.TTLExpiresAt == nil {
		t.Error("ttlExpiresAt not set")
	}
	if len(dr.Status.ReportedCapabilities) != 2 {
		t.Errorf("reportedCapabilities = %v", dr.Status.ReportedCapabilities)
	}
	if len(dr.Status.ReportedHardware) != 2 {
		t.Fatalf("reportedHardware = %+v", dr.Status.ReportedHardware)
	}
	if dr.Status.ReportedHardware[0].Type != fleetv1.HardwareTypeLidar {
		t.Errorf("known hw type = %q, want Lidar", dr.Status.ReportedHardware[0].Type)
	}
	// Tier A (ADR-0022): Model is carried through; a known type carries no CustomType.
	if dr.Status.ReportedHardware[0].Model != "RPLIDAR-S2" {
		t.Errorf("hw model = %q, want RPLIDAR-S2", dr.Status.ReportedHardware[0].Model)
	}
	if dr.Status.ReportedHardware[0].CustomType != "" {
		t.Errorf("known type must not set customType, got %q", dr.Status.ReportedHardware[0].CustomType)
	}
	if dr.Status.ReportedHardware[1].Type != fleetv1.HardwareTypeCustom {
		t.Errorf("unknown hw type = %q, want Custom fallback", dr.Status.ReportedHardware[1].Type)
	}
	// The unrecognised type string is preserved as the operator-defined subtype.
	if dr.Status.ReportedHardware[1].CustomType != "Warp-Drive" {
		t.Errorf("customType = %q, want the preserved raw type Warp-Drive", dr.Status.ReportedHardware[1].CustomType)
	}
	if len(dr.Status.ReportedModels) != 1 || dr.Status.ReportedModels[0].Status != fleetv1.ModelStatusActive {
		t.Errorf("reportedModels = %+v", dr.Status.ReportedModels)
	}
}

func TestDiscover_NoMacLeavesEmpty(t *testing.T) {
	r, c := newRegistrar(t)
	ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: regRobotID})
	if !ack.GetAccepted() {
		t.Fatalf("not accepted: %+v", ack)
	}
	if got := getDR(t, c, regRobotID).Status.MacAddress; got != "" {
		t.Errorf("macAddress = %q, want empty when unreported", got)
	}
}

func TestDiscover_InvalidRobotIDRejected(t *testing.T) {
	r, c := newRegistrar(t)
	for _, id := range []string{"", "Robot Seven", "UPPER", "bad_underscore", "trailing-"} {
		ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: id})
		if ack.GetAccepted() {
			t.Errorf("id %q accepted, want rejected", id)
		}
		if ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_INVALID_ROBOT_ID {
			t.Errorf("id %q rejection = %v, want INVALID_ROBOT_ID", id, ack.GetRejection())
		}
	}
	// Nothing was created.
	list := &fleetv1.DiscoveredRobotList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("a rejected Discover created %d DiscoveredRobots", len(list.Items))
	}
}

func TestDiscover_NoNamespaceRejected(t *testing.T) {
	r, _ := newRegistrar(t)
	id := adapterID()
	id.Namespace = ""
	ack := r.Discover(context.Background(), id, controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: regRobotID})
	if ack.GetAccepted() || ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_NAMESPACE_MISMATCH {
		t.Fatalf("empty-namespace ack = %+v", ack)
	}
}

func TestDiscover_AlreadyAdmittedRejected(t *testing.T) {
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: regRobotID}}
	r, _ := newRegistrar(t, robot)
	ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: regRobotID})
	if ack.GetAccepted() || ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_ALREADY_EXISTS {
		t.Fatalf("already-admitted ack = %+v, want ALREADY_EXISTS", ack)
	}
}

func TestDiscover_ReannounceIsIdempotent(t *testing.T) {
	r, c := newRegistrar(t)
	first := "de:ad:be:ef:00:01"
	second := "de:ad:be:ef:00:02"
	if ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: regRobotID, Mac: &first}); !ack.GetAccepted() {
		t.Fatalf("first announce: %+v", ack)
	}
	if ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: regRobotID, Mac: &second}); !ack.GetAccepted() {
		t.Fatalf("re-announce: %+v", ack)
	}
	list := &fleetv1.DiscoveredRobotList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("re-announce created a duplicate: %d DiscoveredRobots", len(list.Items))
	}
	if got := getDR(t, c, regRobotID).Status.MacAddress; got != second {
		t.Errorf("re-announce did not refresh status: macAddress = %q, want %q", got, second)
	}
}

func TestRegister_NotAdmitted(t *testing.T) {
	r, _ := newRegistrar(t)
	ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: regRobotID})
	if ack.GetAccepted() || ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED {
		t.Fatalf("unadmitted Register ack = %+v, want NOT_ADMITTED", ack)
	}
}

func TestRegister_InvalidRobotID(t *testing.T) {
	r, _ := newRegistrar(t)
	ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: "Bad Id"})
	if ack.GetAccepted() || ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_INVALID_ROBOT_ID {
		t.Fatalf("invalid-id Register ack = %+v, want INVALID_ROBOT_ID", ack)
	}
}

func TestRegister_AdmittedAccepted(t *testing.T) {
	interval := int32(7)
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: regRobotID},
		Spec:       fleetv1.RobotSpec{Zone: "dock-1", TelemetryIntervalSeconds: &interval},
	}
	r, _ := newRegistrar(t, robot)
	ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: regRobotID})
	if !ack.GetAccepted() {
		t.Fatalf("admitted Register not accepted: %+v", ack)
	}
	if ack.GetTelemetryIntervalSeconds() != 7 {
		t.Errorf("telemetry interval = %d, want 7", ack.GetTelemetryIntervalSeconds())
	}
	if ack.GetAssignedZone() != "dock-1" {
		t.Errorf("assigned zone = %q, want dock-1", ack.GetAssignedZone())
	}
}

func TestRegister_AssignedZonePrefersCurrent(t *testing.T) {
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: regRobotID},
		Spec:       fleetv1.RobotSpec{Zone: "spec-zone"},
		Status:     fleetv1.RobotStatus{CurrentZone: "live-zone"},
	}
	r, _ := newRegistrar(t, robot)
	ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: regRobotID})
	if ack.GetAssignedZone() != "live-zone" {
		t.Errorf("assigned zone = %q, want live-zone (status.currentZone wins)", ack.GetAssignedZone())
	}
}
