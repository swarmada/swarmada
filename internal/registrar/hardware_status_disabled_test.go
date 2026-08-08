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

// ADR-0031: the registrar's proto→CRD hardware-status mapping must carry the new DISABLED
// value (intentionally off), distinct from FAILED, alongside the existing values.

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func TestMapHardwareStatus_Disabled(t *testing.T) {
	cases := []struct {
		in   fav1.HardwareStatus
		want fleetv1.HardwareStatus
	}{
		{fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY, fleetv1.HardwareHealthy},
		{fav1.HardwareStatus_HARDWARE_STATUS_DEGRADED, fleetv1.HardwareDegraded},
		{fav1.HardwareStatus_HARDWARE_STATUS_FAILED, fleetv1.HardwareFailed},
		{fav1.HardwareStatus_HARDWARE_STATUS_DISABLED, fleetv1.HardwareDisabled},
		{fav1.HardwareStatus_HARDWARE_STATUS_UNSPECIFIED, ""},
	}
	for _, tc := range cases {
		if got := mapHardwareStatus(tc.in); got != tc.want {
			t.Errorf("mapHardwareStatus(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
