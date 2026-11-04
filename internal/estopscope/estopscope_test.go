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

package estopscope

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func zone(name, parent string, trigger *string, propagate *bool) fleetv1.FleetZone {
	z := fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: fleetv1.FleetZoneSpec{ParentZone: parent}}
	if trigger != nil {
		z.Annotations = map[string]string{TriggerAnnotation: *trigger}
	}
	if propagate != nil {
		z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: propagate}
	}
	return z
}

func robot(currentZone, specZone string, trigger *string) *fleetv1.Robot {
	r := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec: fleetv1.RobotSpec{Zone: specZone}, Status: fleetv1.RobotStatus{CurrentZone: currentZone}}
	if trigger != nil {
		r.Annotations = map[string]string{TriggerAnnotation: *trigger}
	}
	return r
}

func config(trigger *string) fleetv1.SwarmadaConfig {
	c := fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config"}}
	if trigger != nil {
		c.Annotations = map[string]string{TriggerAnnotation: *trigger}
	}
	return c
}

func p[T any](v T) *T { return &v }

func TestStandingStop(t *testing.T) {
	cases := []struct {
		name    string
		robot   *fleetv1.Robot
		zones   []fleetv1.FleetZone
		configs []fleetv1.SwarmadaConfig
		want    Holder
		held    bool
	}{
		{name: "nothing holds it", robot: robot("bay-4", "", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", nil, nil)}, configs: []fleetv1.SwarmadaConfig{config(nil)}},
		{name: "its own trigger, even empty, comes first", robot: robot("bay-4", "", p("")),
			zones: []fleetv1.FleetZone{zone("bay-4", "", p("z"), nil)}, configs: []fleetv1.SwarmadaConfig{config(p("n"))},
			want: Holder{Kind: KindRobot, Name: "r1", Value: ""}, held: true},
		{name: "its zone", robot: robot("bay-4", "", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", p(""), nil)},
			want:  Holder{Kind: KindZone, Name: "bay-4", Value: ""}, held: true},
		{name: "an ancestor that propagates down", robot: robot("bay-4a", "", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", p("up"), nil), zone("bay-4a", "bay-4", nil, nil)},
			want:  Holder{Kind: KindZone, Name: "bay-4", Value: "up"}, held: true},
		{name: "an ancestor that does not propagate down", robot: robot("bay-4a", "", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", p("up"), p(false)), zone("bay-4a", "bay-4", nil, nil)}},
		{name: "its own zone stops it whatever its policy", robot: robot("bay-4", "", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", p("z"), p(false))},
			want:  Holder{Kind: KindZone, Name: "bay-4", Value: "z"}, held: true},
		{name: "a sibling zone does not", robot: robot("bay-4a", "", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", nil, nil), zone("bay-4a", "bay-4", nil, nil), zone("bay-4b", "bay-4", p("s"), nil)}},
		{name: "spec.zone while currentZone is empty", robot: robot("", "bay-4", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", p("z"), nil)},
			want:  Holder{Kind: KindZone, Name: "bay-4", Value: "z"}, held: true},
		{name: "currentZone wins over spec.zone, as the fan-out reads it", robot: robot("bay-9", "bay-4", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", p("z"), nil), zone("bay-9", "", nil, nil)}},
		{name: "the namespace", robot: robot("bay-4", "", nil),
			zones: []fleetv1.FleetZone{zone("bay-4", "", nil, nil)}, configs: []fleetv1.SwarmadaConfig{config(p("ns"))},
			want: Holder{Kind: KindNamespace, Name: "swarmada-config", Value: "ns"}, held: true},
		{name: "a parentZone cycle ends the walk", robot: robot("a", "", nil),
			zones: []fleetv1.FleetZone{zone("a", "b", nil, nil), zone("b", "a", nil, nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, held := StandingStop(tc.robot, tc.zones, tc.configs)
			if held != tc.held || got != tc.want {
				t.Fatalf("StandingStop = %+v, %v; want %+v, %v", got, held, tc.want, tc.held)
			}
		})
	}
}

func TestHolderString(t *testing.T) {
	for h, want := range map[Holder]string{
		{Kind: KindRobot, Name: "r1", Value: "own"}:                `its own robot-scope estop (swarmada.io/estop-triggered="own")`,
		{Kind: KindZone, Name: "bay-4", Value: ""}:                 `the estop on zone bay-4 (swarmada.io/estop-triggered="")`,
		{Kind: KindNamespace, Name: "swarmada-config", Value: "n"}: `the namespace estop (SwarmadaConfig swarmada-config, swarmada.io/estop-triggered="n")`,
	} {
		if got := h.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", h, got, want)
		}
	}
}
