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

package admission

import (
	"math"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// MapReportedHardware is the ADMISSION hop: DiscoveredRobot.status.reportedHardware[] ->
// Robot.spec.hardware[]. Until now it copied name+type only, so an admitted Robot inherited a
// hardware inventory stripped of everything the adapter had actually reported.
//
// What these tests hold down is the same property as the wire hop: presence survives. An attribute
// the adapter never reported must stay nil here too — a robot whose payload ceiling is *unknown* is
// a different scheduling input from one rated at 0 kg.

func fp(v float64) *float64 { return &v }
func bp(v bool) *bool       { return &v }

func fullyReported() fleetv1.DiscoveredHardwareComponent {
	return fleetv1.DiscoveredHardwareComponent{
		Name:             "front-lidar",
		Type:             fleetv1.HardwareTypeLidar,
		Status:           fleetv1.HardwareHealthy,
		Model:            "SICK TIM551",
		CustomType:       "",
		MaxPayloadKg:     fp(120.5),
		ResolutionMp:     fp(12),
		RangeM:           fp(25.4),
		HorizontalFovDeg: fp(360),
		DepthCapable:     bp(true),
		FrameRateFps:     fp(30),
		PlatformLengthMm: fp(1200),
		PlatformWidthMm:  fp(800),
	}
}

// Every shared attribute lands.
func TestMapReportedHardware_CarriesEverySharedField(t *testing.T) {
	got := MapReportedHardware([]fleetv1.DiscoveredHardwareComponent{fullyReported()})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	h := got[0]

	if h.Name != "front-lidar" || h.Type != fleetv1.HardwareTypeLidar {
		t.Errorf("identity = %q/%q", h.Name, h.Type)
	}
	if h.Model != "SICK TIM551" {
		t.Errorf("model = %q, want SICK TIM551 (was dropped before this change)", h.Model)
	}
	for _, tc := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"maxPayloadKg", h.MaxPayloadKg, 120.5},
		{"resolutionMp", h.ResolutionMp, 12},
		{"rangeM", h.RangeM, 25.4},
		{"horizontalFovDeg", h.HorizontalFovDeg, 360},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if h.DepthCapable == nil || !*h.DepthCapable {
		t.Errorf("depthCapable = %v, want true", h.DepthCapable)
	}
	for _, tc := range []struct {
		name string
		got  *int32
		want int32
	}{
		{"frameRateFps", h.FrameRateFps, 30},
		{"platformLengthMm", h.PlatformLengthMm, 1200},
		{"platformWidthMm", h.PlatformWidthMm, 800},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// CustomType is carried so an operator-defined subtype survives admission — it is what makes a
// Custom-typed component identifiable at all.
func TestMapReportedHardware_CarriesCustomType(t *testing.T) {
	got := MapReportedHardware([]fleetv1.DiscoveredHardwareComponent{{
		Name: "safety-bus", Type: fleetv1.HardwareTypeCustom, CustomType: "SafetyBus",
	}})
	if got[0].CustomType != "SafetyBus" {
		t.Errorf("customType = %q, want SafetyBus", got[0].CustomType)
	}
}

// Status is deliberately NOT copied: it is health, owned by Robot.status.hardware[]. Copying a
// discovery-time snapshot into spec would make a stale reading part of desired state. The Robot
// spec shape has no Status field at all, so this test pins the intent rather than the mechanism.
func TestMapReportedHardware_DropsStatus(t *testing.T) {
	src := fullyReported()
	src.Status = fleetv1.HardwareFailed
	got := MapReportedHardware([]fleetv1.DiscoveredHardwareComponent{src})
	// A Failed discovery reading must not suppress or alter the spec entry.
	if len(got) != 1 || got[0].Name != "front-lidar" {
		t.Fatalf("a Failed component must still be inventoried in spec: %+v", got)
	}
}

// Unreported attributes stay unset across the admission hop, exactly as across the wire hop.
func TestMapReportedHardware_AbsentAttributesStayUnset(t *testing.T) {
	got := MapReportedHardware([]fleetv1.DiscoveredHardwareComponent{{
		Name: "bare", Type: fleetv1.HardwareTypeLidar,
	}})
	h := got[0]
	if h.MaxPayloadKg != nil || h.ResolutionMp != nil || h.RangeM != nil ||
		h.HorizontalFovDeg != nil || h.DepthCapable != nil {
		t.Errorf("unreported attributes materialised: %+v", h)
	}
	if h.FrameRateFps != nil || h.PlatformLengthMm != nil || h.PlatformWidthMm != nil {
		t.Errorf("unreported narrowed attributes materialised: fps=%v len=%v wid=%v",
			h.FrameRateFps, h.PlatformLengthMm, h.PlatformWidthMm)
	}
}

// A reported zero is data and must survive as zero — distinct from "not reported".
func TestMapReportedHardware_ReportedZeroSurvives(t *testing.T) {
	got := MapReportedHardware([]fleetv1.DiscoveredHardwareComponent{{
		Name: "flat-bed", Type: fleetv1.HardwareTypeLoadPlatform,
		MaxPayloadKg: fp(0), DepthCapable: bp(false), FrameRateFps: fp(0),
	}})
	h := got[0]
	if h.MaxPayloadKg == nil || *h.MaxPayloadKg != 0 {
		t.Errorf("maxPayloadKg = %v, want a preserved 0", h.MaxPayloadKg)
	}
	if h.DepthCapable == nil || *h.DepthCapable {
		t.Errorf("depthCapable = %v, want a preserved false", h.DepthCapable)
	}
	if h.FrameRateFps == nil || *h.FrameRateFps != 0 {
		t.Errorf("frameRateFps = %v, want a preserved 0", h.FrameRateFps)
	}
}

// The narrowing contract: round half away from zero, and refuse anything unrepresentable rather
// than letting an out-of-range float become an implementation-defined integer.
func TestInt32FromFloat(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *float64
		want *int32
	}{
		{"nil stays nil", nil, nil},
		{"exact", fp(30), i32p(30)},
		{"rounds up at .5", fp(29.5), i32p(30)},
		{"rounds down below .5", fp(29.4), i32p(29)},
		{"29.97 fps", fp(29.97), i32p(30)},
		{"negative rounds away from zero", fp(-2.5), i32p(-3)},
		{"zero preserved", fp(0), i32p(0)},
		{"int32 max", fp(2147483647), i32p(2147483647)},
		{"int32 min", fp(-2147483648), i32p(-2147483648)},
		// Unrepresentable → unknown, never a wrapped integer.
		{"above int32", fp(2147483648), nil},
		{"below int32", fp(-2147483649), nil},
		{"huge", fp(1e300), nil},
		{"NaN", fp(math.NaN()), nil},
		{"+Inf", fp(math.Inf(1)), nil},
		{"-Inf", fp(math.Inf(-1)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Int32FromFloat(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %d, want nil (unrepresentable must become unknown)", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got nil, want %d", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("got %d, want %d", *got, *tc.want)
			}
		})
	}
}

func i32p(v int32) *int32 { return &v }

// An empty inventory maps to nil, not an empty slice — so admission does not write an empty
// hardware list where the robot reported none.
func TestMapReportedHardware_EmptyStaysNil(t *testing.T) {
	if got := MapReportedHardware(nil); got != nil {
		t.Errorf("nil input -> %v, want nil", got)
	}
	if got := MapReportedHardware([]fleetv1.DiscoveredHardwareComponent{}); got != nil {
		t.Errorf("empty input -> %v, want nil", got)
	}
}
