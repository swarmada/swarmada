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

package safety

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The evidence a release records (the EstopResumeConfirmed Event, the
// ESTOP_RESUME_CONFIRMED audit entry and what `swarmctl estop resume` prints) says what
// was checked. A capability Paused by a maintenance window does not hold the release,
// so it is not counted as Active again: it is named as Paused, and not checked.
func TestResume_EvidenceCountsAPausedCapabilityAsPausedNotActive(t *testing.T) {
	nav := fleetv1.CapabilityStatusEntry{Name: "nav", Status: fleetv1.CapabilityStatusActive}
	transport := fleetv1.CapabilityStatusEntry{Name: "transport", Status: fleetv1.CapabilityStatusPaused, Paused: true}
	caps := []fleetv1.CapabilityStatusEntry{nav, transport}

	cases := []struct {
		name     string
		baseline *fleetv1.EstopBaseline
		want     string
	}{
		{"with a stop record", &fleetv1.EstopBaseline{ActiveCapabilities: []string{"nav", "transport"}},
			"of the 2 capabilities Active at the stop, 1 is Active again and 1 is Paused by a maintenance window " +
				"(not checked); no hardware Failed"},
		{"without a stop record", nil,
			"1 capability Active and 1 Paused by a maintenance window (not checked); no hardware Failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fleetv1.Robot{}
			r.Status.Phase = fleetv1.RobotPhaseMaintenance
			r.Status.Capabilities = caps
			r.Status.EstopBaseline = tc.baseline
			why, ok := resumeRefusal(r, caps, "")
			if !ok {
				t.Fatalf("refused (%q); a Paused capability does not hold the release", why)
			}
			if why != tc.want {
				t.Errorf("evidence = %q\n          want %q", why, tc.want)
			}
		})
	}
}
