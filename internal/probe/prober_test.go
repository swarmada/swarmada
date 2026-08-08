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

package probe

import (
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Every proto ProbeStatus binds correctly; UNSPECIFIED binds to Unknown, never a
// healthy state.
func TestBindProbeStatus(t *testing.T) {
	cases := []struct {
		in   fav1.ProbeStatus
		want Status
	}{
		{fav1.ProbeStatus_PROBE_STATUS_HEALTHY, StatusHealthy},
		{fav1.ProbeStatus_PROBE_STATUS_DEGRADED, StatusDegraded},
		{fav1.ProbeStatus_PROBE_STATUS_FAILED, StatusFailed},
		{fav1.ProbeStatus_PROBE_STATUS_UNSPECIFIED, StatusUnknown},
		{fav1.ProbeStatus(99), StatusUnknown}, // any out-of-range value fails safe
	}
	for _, tc := range cases {
		if got := BindProbeStatus(tc.in); got != tc.want {
			t.Errorf("BindProbeStatus(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestMetricsMet(t *testing.T) {
	expected := map[string]float64{"frame_rate_pct": 80, "depth_valid_pct": 90}
	if !MetricsMet(expected, map[string]float64{"frame_rate_pct": 95, "depth_valid_pct": 92}) {
		t.Error("all metrics above threshold should pass")
	}
	if MetricsMet(expected, map[string]float64{"frame_rate_pct": 70, "depth_valid_pct": 92}) {
		t.Error("a metric below threshold must fail")
	}
	// A missing actual metric is never assumed met.
	if MetricsMet(expected, map[string]float64{"frame_rate_pct": 95}) {
		t.Error("a missing metric must fail (never assumed met)")
	}
	if !MetricsMet(nil, nil) {
		t.Error("no thresholds means nothing to fail")
	}
}
