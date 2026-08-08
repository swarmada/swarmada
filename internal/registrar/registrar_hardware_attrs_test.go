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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The numeric hardware attributes (fleet_adapter.v1 HardwareComponent tags 6-13) map from the wire
// onto DiscoveredRobot.status.reportedHardware[] with EXPLICIT PRESENCE preserved end to end.
//
// The property under test is not "the numbers arrive" — it is that "not reported" and "reported as
// zero" stay distinguishable across the hop. A mapping written with the generated Get*() accessors
// would pass a values-arrive test and still be wrong: those return 0 for an unset field, publishing
// a 0 kg payload ceiling or a 0 m sensing range as though it had been measured. A scheduler reading
// that would exclude a robot that is in fact perfectly capable, or admit one that is not.

func f64(v float64) *float64 { return &v }
func boolp(v bool) *bool     { return &v }

// hwDiscover runs a Discover carrying exactly the given components and returns the mapped inventory.
func hwDiscover(t *testing.T, comps ...*fav1.HardwareComponent) []fleetv1.DiscoveredHardwareComponent {
	t.Helper()
	r, c := newRegistrar(t)
	ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{
		RobotId:      regRobotID,
		Manufacturer: "Acme",
		Model:        "Hauler-3000",
		Hardware:     comps,
	})
	if !ack.GetAccepted() {
		t.Fatalf("Discover not accepted: %+v", ack)
	}
	return getDR(t, c, regRobotID).Status.ReportedHardware
}

// A component reporting every attribute lands every value.
func TestDiscover_HardwareNumericAttributesPopulated(t *testing.T) {
	got := hwDiscover(t, &fav1.HardwareComponent{
		Name:             "front-lidar",
		Type:             "Lidar",
		Status:           fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY,
		MaxPayloadKg:     f64(120.5),
		ResolutionMp:     f64(12),
		RangeM:           f64(25.4),
		HorizontalFovDeg: f64(360),
		DepthCapable:     boolp(true),
		FrameRateFps:     f64(30),
		PlatformLengthMm: f64(1200),
		PlatformWidthMm:  f64(800),
	})
	if len(got) != 1 {
		t.Fatalf("reportedHardware len = %d, want 1", len(got))
	}
	h := got[0]

	for _, tc := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"maxPayloadKg", h.MaxPayloadKg, 120.5},
		{"resolutionMp", h.ResolutionMp, 12},
		{"rangeM", h.RangeM, 25.4},
		{"horizontalFovDeg", h.HorizontalFovDeg, 360},
		{"frameRateFps", h.FrameRateFps, 30},
		{"platformLengthMm", h.PlatformLengthMm, 1200},
		{"platformWidthMm", h.PlatformWidthMm, 800},
	} {
		if tc.got == nil {
			t.Errorf("%s is nil, want %v", tc.name, tc.want)
			continue
		}
		if *tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, *tc.got, tc.want)
		}
	}
	if h.DepthCapable == nil || !*h.DepthCapable {
		t.Errorf("depthCapable = %v, want true", h.DepthCapable)
	}
	// The identity/health subset must survive the addition unchanged.
	if h.Name != "front-lidar" || h.Type != fleetv1.HardwareTypeLidar ||
		h.Status != fleetv1.HardwareHealthy {
		t.Errorf("identity/health fields regressed: %+v", h)
	}
}

// A component reporting NO attributes leaves every one unset — never 0, never false.
func TestDiscover_AbsentHardwareAttributesStayUnset(t *testing.T) {
	got := hwDiscover(t, &fav1.HardwareComponent{
		Name:   "bare-lidar",
		Type:   "Lidar",
		Status: fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY,
	})
	if len(got) != 1 {
		t.Fatalf("reportedHardware len = %d, want 1", len(got))
	}
	h := got[0]

	for name, p := range map[string]*float64{
		"maxPayloadKg":     h.MaxPayloadKg,
		"resolutionMp":     h.ResolutionMp,
		"rangeM":           h.RangeM,
		"horizontalFovDeg": h.HorizontalFovDeg,
		"frameRateFps":     h.FrameRateFps,
		"platformLengthMm": h.PlatformLengthMm,
		"platformWidthMm":  h.PlatformWidthMm,
	} {
		if p != nil {
			t.Errorf("%s = %v for an adapter that reported nothing; unreported must stay unset, "+
				"never a measured 0", name, *p)
		}
	}
	if h.DepthCapable != nil {
		t.Errorf("depthCapable = %v for an adapter that reported nothing; want unset, not false",
			*h.DepthCapable)
	}
}

// A REPORTED zero is preserved as a zero, distinct from unset. This is the half a Get*()-based
// mapping cannot express: it would render both as 0 and destroy the distinction.
func TestDiscover_ReportedZeroIsNotUnset(t *testing.T) {
	got := hwDiscover(t, &fav1.HardwareComponent{
		Name:         "flat-bed",
		Type:         "Gripper",
		Status:       fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY,
		MaxPayloadKg: f64(0),       // genuinely measured: this component lifts nothing
		DepthCapable: boolp(false), // genuinely measured: not depth-capable
	})
	h := got[0]
	if h.MaxPayloadKg == nil {
		t.Fatal("maxPayloadKg is nil, but the adapter reported 0 — a reported zero is data, not absence")
	}
	if *h.MaxPayloadKg != 0 {
		t.Errorf("maxPayloadKg = %v, want 0", *h.MaxPayloadKg)
	}
	if h.DepthCapable == nil {
		t.Fatal("depthCapable is nil, but the adapter reported false")
	}
	if *h.DepthCapable {
		t.Error("depthCapable = true, want the reported false")
	}
	// Everything the adapter did NOT report on this component stays unset.
	if h.RangeM != nil || h.ResolutionMp != nil {
		t.Errorf("unreported attributes leaked values: rangeM=%v resolutionMp=%v", h.RangeM, h.ResolutionMp)
	}
}

// Presence is PER FIELD and PER COMPONENT, not all-or-nothing: a partially-reporting component and a
// fully-bare one in the same Discover keep their own presence.
func TestDiscover_HardwareAttributePresenceIsPerField(t *testing.T) {
	got := hwDiscover(t,
		&fav1.HardwareComponent{
			Name: "camera", Type: "Camera", Status: fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY,
			ResolutionMp: f64(48), DepthCapable: boolp(true), // reported
			// rangeM, frameRateFps and the rest deliberately absent
		},
		&fav1.HardwareComponent{
			Name: "bare", Type: "Lidar", Status: fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY,
		},
	)
	if len(got) != 2 {
		t.Fatalf("reportedHardware len = %d, want 2", len(got))
	}
	cam, bare := got[0], got[1]

	if cam.ResolutionMp == nil || *cam.ResolutionMp != 48 {
		t.Errorf("camera resolutionMp = %v, want 48", cam.ResolutionMp)
	}
	if cam.DepthCapable == nil || !*cam.DepthCapable {
		t.Errorf("camera depthCapable = %v, want true", cam.DepthCapable)
	}
	if cam.RangeM != nil || cam.FrameRateFps != nil {
		t.Errorf("camera leaked unreported attributes: rangeM=%v frameRateFps=%v",
			cam.RangeM, cam.FrameRateFps)
	}
	if bare.ResolutionMp != nil || bare.DepthCapable != nil {
		t.Errorf("the bare component inherited the camera's attributes: %+v", bare)
	}
}
