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
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// HoldStands applies to one record the rule Holders applies to recorded entries.
func TestHoldStands(t *testing.T) {
	zone := func(uid string, ann map[string]string) fleetv1.FleetZone {
		return fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-1", UID: types.UID(uid), Annotations: ann}}
	}
	config := func(uid string, ann map[string]string) fleetv1.SwarmadaConfig {
		return fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", UID: types.UID(uid), Annotations: ann}}
	}
	triggered := map[string]string{TriggerAnnotation: "stop"}
	marked := map[string]string{ProcessedAnnotation: "stop"}
	zoneHold := fleetv1.EstopHold{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-1", UID: "z1"}
	cfgHold := fleetv1.EstopHold{Kind: fleetv1.EstopHoldSwarmadaConfig, Name: "swarmada-config", UID: "c1"}
	for _, tc := range []struct {
		name    string
		hold    fleetv1.EstopHold
		zones   []fleetv1.FleetZone
		configs []fleetv1.SwarmadaConfig
		want    bool
	}{
		{"zone triggered, same UID", zoneHold, []fleetv1.FleetZone{zone("z1", triggered)}, nil, true},
		{"zone marked only (clear not applied), same UID", zoneHold, []fleetv1.FleetZone{zone("z1", marked)}, nil, true},
		{"zone cleared", zoneHold, []fleetv1.FleetZone{zone("z1", nil)}, nil, false},
		{"zone recreated under another UID", zoneHold, []fleetv1.FleetZone{zone("z2", triggered)}, nil, false},
		{"zone deleted", zoneHold, nil, nil, false},
		{"record without a UID, zone triggered", fleetv1.EstopHold{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-1"},
			[]fleetv1.FleetZone{zone("z1", triggered)}, nil, true},
		{"record without a kind reads as a zone", fleetv1.EstopHold{Name: "bay-1", UID: "z1"},
			[]fleetv1.FleetZone{zone("z1", triggered)}, nil, true},
		{"config triggered, same UID", cfgHold, nil, []fleetv1.SwarmadaConfig{config("c1", triggered)}, true},
		{"config recreated under another UID", cfgHold, nil, []fleetv1.SwarmadaConfig{config("c2", marked)}, false},
		{"config record does not match a zone of that name", cfgHold, []fleetv1.FleetZone{zone("c1", triggered)}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HoldStands(tc.hold, tc.zones, tc.configs); got != tc.want {
				t.Errorf("HoldStands = %v, want %v", got, tc.want)
			}
		})
	}
}
