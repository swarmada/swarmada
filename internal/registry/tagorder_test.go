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

package registry

import (
	"testing"
	"time"
)

// Build-time ordering is a TIE-BREAK, never the primary rule. These tests pin both halves: that it
// resolves what version ordering cannot, and that it is not allowed to override version ordering —
// because `created` is build time, and a rebuild-and-repush of an old release would otherwise
// out-rank the release that superseded it and roll robots backwards.

func TestVersionless_Identification(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"latest", true},
		{"stable", true},
		{"prod", true},
		{"1.2.3", false},
		{"v1.2.3", false},
		{"v2", false},
		// Versionless tracks what compareVersions can MEANINGFULLY order, and that splits on "."
		// only. "release-1" has no dot-separated numeric component, so version ordering would fall
		// back to lexical comparison — which sorts release-10 BEFORE release-9. Treating it as
		// versionless hands it to the build-time tie-break instead, which is strictly better.
		{"release-1", true},
		{"1.2.3-rc1", false}, // dot-separated numerics are present, so version order applies
	} {
		t.Run(tc.tag, func(t *testing.T) {
			if got := Versionless(tc.tag); got != tc.want {
				t.Errorf("Versionless(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}

// The tie-break resolves tags version ordering treats as equal.
func TestHighestByTime_PicksLatestBuild(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	got := HighestByTime([]string{"latest", "stable", "prod"}, map[string]time.Time{
		"latest": base.Add(2 * time.Hour),
		"stable": base,
		"prod":   base.Add(time.Hour),
	})
	if got != "latest" {
		t.Errorf("HighestByTime = %q, want latest (the newest build)", got)
	}
}

// A tag with no usable timestamp does not participate — it is skipped, not treated as epoch.
func TestHighestByTime_SkipsTagsWithoutTime(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	got := HighestByTime([]string{"latest", "stable"}, map[string]time.Time{"stable": base})
	if got != "stable" {
		t.Errorf("HighestByTime = %q, want stable (latest has no timestamp)", got)
	}
}

// All timestamps missing — e.g. every image is an epoch-pinned reproducible build — yields no
// answer at all, so the caller does nothing rather than guessing.
func TestHighestByTime_NoUsableTimesYieldsNothing(t *testing.T) {
	if got := HighestByTime([]string{"latest", "stable"}, map[string]time.Time{}); got != "" {
		t.Errorf("HighestByTime = %q, want empty when no tag carries a build time", got)
	}
}

// A zero time is explicitly "no signal", not "the beginning of time".
func TestHighestByTime_ZeroTimeIsNotASignal(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	got := HighestByTime([]string{"latest", "stable"}, map[string]time.Time{
		"latest": {}, // zero
		"stable": base,
	})
	if got != "stable" {
		t.Errorf("HighestByTime = %q, want stable — a zero time must not compete", got)
	}
}

// THE REGRESSION THIS DESIGN AVOIDS. A rebuilt v1.2.0 has a newer build time than v2.0.0, so a
// time-FIRST rule would select it and trigger a downgrade. Version ordering must still win.
func TestVersionOrdering_StillBeatsBuildTime(t *testing.T) {
	tags := []string{"v1.2.0", "v2.0.0"}
	if got := Highest(tags); got != "v2.0.0" {
		t.Fatalf("Highest = %q, want v2.0.0", got)
	}
	// Neither is versionless, so the tie-break is never consulted for them.
	for _, tag := range tags {
		if Versionless(tag) {
			t.Errorf("%q must not be treated as versionless; the build-time tie-break would then "+
				"be able to select an older release and downgrade robot firmware", tag)
		}
	}
}
