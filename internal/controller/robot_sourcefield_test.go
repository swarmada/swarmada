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
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func f64p(v float64) *float64 { return &v }
func i32p(v int32) *int32     { return &v }

func hwMap(cs ...fleetv1.HardwareComponent) map[string]fleetv1.HardwareComponent {
	m := make(map[string]fleetv1.HardwareComponent, len(cs))
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

func capWith(params map[string]fleetv1.CapabilityParameter) *fleetv1.ClassCapability {
	return &fleetv1.ClassCapability{Name: "transport.payload", Parameters: params}
}

func TestResolveSourceField_FromHardwareSpec(t *testing.T) {
	hw := hwMap(fleetv1.HardwareComponent{Name: "load-platform", MaxPayloadKg: f64p(45.0), FrameRateFps: i32p(30)})

	got := resolveParameters(capWith(map[string]fleetv1.CapabilityParameter{
		"maxPayloadKg": {SourceField: "hardware[load-platform].spec.maxPayloadKg"},
		"fps":          {SourceField: "hardware[load-platform].spec.frameRateFps"}, // int → float
	}), hw)

	if got["maxPayloadKg"] != 45.0 {
		t.Errorf("maxPayloadKg = %v, want 45.0", got["maxPayloadKg"])
	}
	if got["fps"] != 30.0 {
		t.Errorf("fps = %v, want 30.0", got["fps"])
	}
}

func TestResolveSourceField_SkipsUnresolvable(t *testing.T) {
	hw := hwMap(fleetv1.HardwareComponent{Name: "load-platform", MaxPayloadKg: f64p(45.0)}) // rangeM unset

	cases := map[string]string{
		"unknown-component": "hardware[nope].spec.maxPayloadKg",
		"unknown-field":     "hardware[load-platform].spec.bogusField",
		"unset-attribute":   "hardware[load-platform].spec.rangeM", // nil pointer
		"malformed-path":    "load-platform.maxPayloadKg",
		"empty-path-target": "hardware[load-platform].spec.",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			got := resolveParameters(capWith(map[string]fleetv1.CapabilityParameter{"p": {SourceField: path}}), hw)
			if _, present := got["p"]; present {
				t.Errorf("param resolved from an unresolvable SourceField %q; want skipped", path)
			}
		})
	}
}

// A static Value and a dynamic SourceField coexist in one capability.
func TestResolveParameters_ValueAndSourceFieldCoexist(t *testing.T) {
	hw := hwMap(fleetv1.HardwareComponent{Name: "arm", MaxGripForceN: f64p(120.5)})
	got := resolveParameters(capWith(map[string]fleetv1.CapabilityParameter{
		"minForceN": {Value: "10"},
		"maxForceN": {SourceField: "hardware[arm].spec.maxGripForceN"},
	}), hw)
	if got["minForceN"] != 10 || got["maxForceN"] != 120.5 {
		t.Fatalf("resolved = %+v, want minForceN=10 maxForceN=120.5", got)
	}
}

// Resolution reads the CURRENT hardware each call — a changed attribute is reflected
// without re-admission (the point of dynamic SourceField resolution).
func TestResolveSourceField_LiveReResolution(t *testing.T) {
	cap := capWith(map[string]fleetv1.CapabilityParameter{
		"maxPayloadKg": {SourceField: "hardware[load-platform].spec.maxPayloadKg"},
	})
	before := resolveParameters(cap, hwMap(fleetv1.HardwareComponent{Name: "load-platform", MaxPayloadKg: f64p(45.0)}))
	after := resolveParameters(cap, hwMap(fleetv1.HardwareComponent{Name: "load-platform", MaxPayloadKg: f64p(40.0)}))
	if before["maxPayloadKg"] != 45.0 || after["maxPayloadKg"] != 40.0 {
		t.Fatalf("re-resolution before=%v after=%v, want 45.0 then 40.0", before["maxPayloadKg"], after["maxPayloadKg"])
	}
}

func TestResolveParameters_NoParamsIsNil(t *testing.T) {
	if got := resolveParameters(&fleetv1.ClassCapability{Name: "x"}, nil); got != nil {
		t.Errorf("expected nil for a capability with no parameters, got %+v", got)
	}
}
