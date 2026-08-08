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

package telemetry

// ADR-0031: a component going Disabled (intentionally off) is a material change but NOT
// critical, so it is throttled like any non-critical delta and does not bypass the RA-1
// status-write throttle. Only Degraded/Failed set the critical flag. This is an internal
// (package telemetry) test because mergeHardware is unexported.

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestMergeHardware_DisabledIsNotCritical(t *testing.T) {
	old := map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareHealthy}

	// Healthy → Disabled: a real change, but benign — must not be critical.
	changed, merged, critical := mergeHardware(old, map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareDisabled})
	if !changed {
		t.Error("Healthy→Disabled should be a change")
	}
	if critical {
		t.Error("Disabled must NOT be critical (it would bypass the RA-1 write throttle)")
	}
	if merged["camera"] != fleetv1.HardwareDisabled {
		t.Errorf("merged camera = %q, want Disabled", merged["camera"])
	}

	// Contrast: Healthy → Failed is critical (fault), proving the predicate still fires.
	if _, _, crit := mergeHardware(old, map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareFailed}); !crit {
		t.Error("Healthy→Failed must be critical")
	}
	// Contrast: Healthy → Degraded is critical (impaired).
	if _, _, crit := mergeHardware(old, map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareDegraded}); !crit {
		t.Error("Healthy→Degraded must be critical")
	}

	// A no-op re-report of Disabled is not even a change (idempotent).
	if ch, _, crit := mergeHardware(map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareDisabled},
		map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareDisabled}); ch || crit {
		t.Errorf("re-reporting Disabled: changed=%v critical=%v, want false/false", ch, crit)
	}
}
