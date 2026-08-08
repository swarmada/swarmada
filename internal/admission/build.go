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

// Package admission builds the Robot an admitted DiscoveredRobot becomes.
//
// It exists because two callers need the same answer and must not diverge: `swarmctl admit`
// validates the operator's parameters before writing them down, and the DiscoveredRobot
// controller builds the real Robot from those parameters afterwards. When the merge order
// of §9.1.2.5 lived only in the CLI, the operator path and the controller's auto-admit path
// were two independent implementations of one specification.
package admission

import (
	"encoding/json"
	"fmt"
	"math"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// AdmitAnnotation records an operator's admission decision on a DiscoveredRobot. Its value
// is a JSON-encoded Params. Written only by the SAR-gated `swarmctl admit` verb; the
// DiscoveredRobot controller consumes it, creates the Robot, and deletes the staging object.
//
// The operator does not create the Robot themselves. That keeps the admission gate (§6.6)
// the only route from discovered to schedulable — a decision recorded here always passes
// through the controller that seals it.
const AdmitAnnotation = "swarmada.io/admit"

// Params are the operator's overrides for an admission, as carried by AdmitAnnotation.
//
// Every field is optional except Zone: a Robot is not schedulable without one, so §9.1.2.5
// makes it the single always-required input. The empty values are meaningful — they mean
// "take the discovered value or the class default", not "set this to empty".
type Params struct {
	Name         string `json:"name,omitempty"`
	Zone         string `json:"zone"`
	RobotClass   string `json:"robotClass,omitempty"`
	Adapter      string `json:"adapter,omitempty"`
	Dock         string `json:"dock,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
}

// Encode renders the parameters for the annotation value.
func (p Params) Encode() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encoding admission parameters: %w", err)
	}
	return string(b), nil
}

// DecodeParams parses an AdmitAnnotation value.
//
// A malformed value is an error rather than a set of defaults. The annotation is an
// instruction to create a schedulable robot, and guessing at a corrupted one could place a
// robot in the wrong zone — the one field with no safe default.
func DecodeParams(v string) (Params, error) {
	var p Params
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		return Params{}, fmt.Errorf("decoding admission parameters: %w", err)
	}
	if p.Zone == "" {
		return Params{}, fmt.Errorf("admission parameters name no zone")
	}
	return p, nil
}

// BuildRobot constructs the Robot spec using the admission merge order of RFC-0001 §9.1.2.5:
// discovered values, then the RobotClass template (when given), then operator overrides, then
// the always-required zone. It sets only fields with a real source — capabilities/models are
// populated solely from a class template (which carries their full typed form), never
// invented from the discovered string lists.
//
// class may be nil when the operator named none. Callers that validate before writing an
// admission down (the CLI) and callers that create the Robot for real (the controller) must
// both go through here, so a parameter set that validates cannot later fail to build.
func BuildRobot(dr *fleetv1.DiscoveredRobot, p Params, class *fleetv1.RobotClass, ns string) (*fleetv1.Robot, error) {
	name := p.Name
	if name == "" {
		name = dr.Name
	}

	manufacturer := firstNonEmpty(p.Manufacturer, dr.Status.Manufacturer)
	model := firstNonEmpty(p.Model, dr.Status.Model)

	adapter := fleetv1.AdapterRef{Name: p.Adapter, Version: dr.Status.AdapterVersion}
	if class != nil {
		if adapter.Name == "" {
			adapter.Name = class.Spec.BaseAdapter.Name
		}
		if class.Spec.BaseAdapter.Version != "" {
			adapter.Version = class.Spec.BaseAdapter.Version
		}
	}
	if adapter.Name == "" {
		return nil, fmt.Errorf("no adapter for admitted robot: pass --adapter or a --robot-class whose baseAdapter names one")
	}

	spec := fleetv1.RobotSpec{
		Manufacturer: manufacturer,
		Model:        model,
		RobotClass:   p.RobotClass,
		Adapter:      adapter,
		Zone:         p.Zone,
	}

	reported := MapReportedHardware(dr.Status.ReportedHardware)

	switch {
	case class != nil:
		// Hardware is a UNION, not a replacement: the class template contributes components
		// the robot did not report, and the robot's own report wins wherever the two name
		// the same component.
		spec.Hardware = MergeHardware(reported, class.Spec.Hardware)
		// The remaining collections have no counterpart in the discovery report — the robot
		// cannot tell us what it is certified to do — so the class is authoritative.
		spec.Capabilities = class.Spec.BaseCapabilities
		spec.InstalledModels = class.Spec.DefaultModels
		spec.Constraints = class.Spec.DefaultConstraints
		if tc := class.Spec.DefaultTelemetry; tc != nil {
			spec.TelemetryIntervalSeconds = tc.TelemetryIntervalSeconds
			spec.MotionThresholdMeters = tc.MotionThresholdMeters
			spec.MaxIdleIntervalSeconds = tc.MaxIdleIntervalSeconds
		}
		spec.Charging = MergeCharging(class.Spec.DefaultChargingConfig, p.Dock)
	default:
		// No class: the robot's own reported inventory is the only source.
		spec.Hardware = reported
		spec.Charging = MergeCharging(nil, p.Dock)
	}

	// Constraints are resolved LAST, against the hardware list decided just above — both the
	// clamp and the derivation are only meaningful once the merge has settled which
	// components the robot actually has.
	spec.Constraints = ResolveConstraintsFromHardware(spec.Constraints, spec.Hardware)

	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			// The DiscoveredRobot's name is the robot_id the adapter announced, and it is
			// what telemetry arrives under — so it is stamped from the staging object rather
			// than left to the defaulting webhook, which can only fall back to the Robot's
			// own name. Those differ whenever an operator passes --name, and a Robot carrying
			// the wrong robot_id receives no telemetry at all: robot_status_sink resolves the
			// announced id to a Robot by this annotation and finds nothing.
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: dr.Name},
		},
		Spec: spec,
	}, nil
}

// ResolveConstraintsFromHardware settles the payload constraint against the resolved hardware
// (§9.1.2.5 — "the class value is overridden by resolved hardware ... to match actual
// hardware"). It does two things, and they answer different questions:
//
//   - CLAMP: a declared cap the hardware cannot support is lowered to what it can.
//   - DERIVE: when nothing declares a cap at all, the hardware's own rating becomes one.
//
// This is the constraint the scheduler reads when deciding what a robot may be assigned, so
// getting spec.hardware right achieves nothing if the number consulted at dispatch still says
// 45 kg for a robot that reported a 40 kg platform — or says nothing at all, which reads as
// unlimited. An undeclared cap is the worse of the two: an overstated one is at least bounded
// by the class author's intent, whereas an absent one bounds the robot by nothing.
//
// It never raises a declared cap. A class constraint BELOW the physical capacity is deliberate
// policy — a fleet may cap a 40 kg platform at 25 kg for its own reasons — and lifting it to
// meet the hardware would silently discard that decision. Derivation applies only where there
// was no decision to discard.
//
// Unknown is still not zero: when no component reports a payload capacity, an absent
// constraint stays absent and a declared one stands, because absence of a reading is not
// evidence of a limit. A reported zero IS a reading, and produces a zero cap — the presence
// distinction MapReportedHardware preserves all the way from the wire.
//
// Returns a copy when it changes anything; the class object it was handed must not be mutated,
// since that pointer is shared with the cached RobotClass every other admission reads.
func ResolveConstraintsFromHardware(c *fleetv1.ClassConstraints, hw []fleetv1.HardwareComponent) *fleetv1.ClassConstraints {
	capacity, ok := payloadCapacity(hw)
	if !ok {
		return c
	}
	// Nothing declared: the hardware's rating is the only statement about this robot's limit,
	// so it becomes the cap rather than leaving the robot effectively unbounded.
	if c == nil {
		return &fleetv1.ClassConstraints{MaxPayloadKg: &capacity}
	}
	if c.MaxPayloadKg != nil && *c.MaxPayloadKg <= capacity {
		return c
	}
	out := c.DeepCopy()
	resolved := capacity
	out.MaxPayloadKg = &resolved
	return out
}

// payloadCapacity is the largest single-component payload rating in the resolved hardware, and
// reports whether any component rated one at all.
//
// The MAXIMUM, not the sum. Two 40 kg platforms are not an 80 kg robot unless a load can be
// split across both, which nothing in the spec establishes — and this number gates what the
// scheduler will hand the robot, so overstating it is the direction that drops a payload.
func payloadCapacity(hw []fleetv1.HardwareComponent) (float64, bool) {
	var largest float64
	var found bool
	for _, h := range hw {
		if h.MaxPayloadKg == nil {
			continue
		}
		if !found || *h.MaxPayloadKg > largest {
			largest = *h.MaxPayloadKg
			found = true
		}
	}
	return largest, found
}

// MergeHardware unions the robot's reported inventory with a RobotClass template, keyed by
// component name (§9.1.2.5 Step 2, and the union-by-name rules of the RobotClass merge).
//
// Where a name appears in both, the REPORTED entry wins entirely — real hardware over class
// template. That direction is the safety-relevant one: a class claiming a component the robot
// does not have, or claiming more of it, would make the robot schedulable for work it cannot
// physically do. A class entry is a statement about the model; the report is a statement
// about this machine.
//
// The replacement is whole-entry, with no field-level merging, which the RobotClass merge
// specifies explicitly. It costs something real: the class can carry attributes discovery has
// no way to report (grip force, reach, stroke — see MapReportedHardware), and for a component
// the robot also reports, those are dropped rather than filled in. That is the deliberate
// trade — a half-reported component merged with a template produces a spec that matches
// neither source, and no reader could tell which fields were measured and which were assumed.
//
// Ordering is stable: reported components first in report order, then class-only components
// in class order, so re-admitting the same robot produces the same spec.
func MergeHardware(reported, class []fleetv1.HardwareComponent) []fleetv1.HardwareComponent {
	if len(class) == 0 {
		return reported
	}
	if len(reported) == 0 {
		return class
	}
	seen := make(map[string]bool, len(reported))
	for _, h := range reported {
		seen[h.Name] = true
	}
	out := make([]fleetv1.HardwareComponent, 0, len(reported)+len(class))
	out = append(out, reported...)
	for _, h := range class {
		if !seen[h.Name] {
			out = append(out, h)
		}
	}
	return out
}

// MapReportedHardware projects the robot-reported hardware inventory into the Robot spec's
// hardware list, carrying every attribute the two shapes share.
//
// Presence is preserved: an attribute the adapter did not report stays nil rather than becoming
// 0/false, so "unknown" and "measured zero" remain distinguishable all the way from the wire
// (fleet_adapter.v1 HardwareComponent tags 6-13) into Robot.spec.hardware[].
//
// Two deliberate omissions:
//
//   - Status is NOT copied. It is a discovery-time HEALTH reading, and health lives on
//     Robot.status.hardware[] where the telemetry projector owns it. Freezing a snapshot of it
//     into spec would make a stale reading part of desired state.
//   - The target carries eleven attributes the discovery shape has no source for
//     (maxGripForceN, strokeMm, reachMm, degreesOfFreedom, resolutionH, resolutionV,
//     tempRangeMinC, tempRangeMaxC, channels, sampleRateHz, touchCapable). The adapter contract
//     does not transmit them, so they stay unset here and can only come from a RobotClass default
//     or an operator edit.
func MapReportedHardware(reported []fleetv1.DiscoveredHardwareComponent) []fleetv1.HardwareComponent {
	if len(reported) == 0 {
		return nil
	}
	out := make([]fleetv1.HardwareComponent, 0, len(reported))
	for _, h := range reported {
		out = append(out, fleetv1.HardwareComponent{
			Name:       h.Name,
			Type:       h.Type,
			Model:      h.Model,
			CustomType: h.CustomType,
			// Same type on both sides — assigned directly so presence rides through untouched.
			MaxPayloadKg:     h.MaxPayloadKg,
			ResolutionMp:     h.ResolutionMp,
			RangeM:           h.RangeM,
			HorizontalFovDeg: h.HorizontalFovDeg,
			DepthCapable:     h.DepthCapable,
			// The discovery shape carries these as float64 (the wire type); the Robot spec models
			// them as int32. Narrowed defensively — see Int32FromFloat.
			FrameRateFps:     Int32FromFloat(h.FrameRateFps),
			PlatformLengthMm: Int32FromFloat(h.PlatformLengthMm),
			PlatformWidthMm:  Int32FromFloat(h.PlatformWidthMm),
		})
	}
	return out
}

// Int32FromFloat narrows a reported measurement to the int32 the Robot spec uses, rounding half
// away from zero (29.97 fps -> 30).
//
// It returns nil — "unknown" — for anything it cannot represent: NaN, ±Inf, or a magnitude outside
// int32. That is the fail-closed choice: a Go float-to-int conversion outside the target range is
// implementation-defined, so a bogus reading could otherwise land as a plausible-looking wrapped
// integer that a scheduler would then treat as a real measurement. Dropping it to unset makes the
// gap visible instead.
func Int32FromFloat(p *float64) *int32 {
	if p == nil {
		return nil
	}
	r := math.Round(*p)
	if math.IsNaN(r) || r < math.MinInt32 || r > math.MaxInt32 {
		return nil
	}
	v := int32(r)
	return &v
}

// MergeCharging builds the Robot charging config from an optional class default plus an
// optional --dock override. Returns nil when neither is present, so an empty config is not
// written.
func MergeCharging(def *fleetv1.ClassChargingConfig, dock string) *fleetv1.RobotChargingConfig {
	if def == nil && dock == "" {
		return nil
	}
	cc := &fleetv1.RobotChargingConfig{DockName: dock}
	if def != nil {
		cc.MinBatteryPctToCharge = def.MinBatteryPctToCharge
		cc.TargetBatteryPct = def.TargetBatteryPct
	}
	return cc
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
