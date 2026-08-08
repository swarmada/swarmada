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
	"strconv"
	"strings"
	"time"
)

// Highest returns the greatest tag by dotted-numeric (semver-ish) order, with a
// lexical tie-break. Empty tags are ignored. Returns "" for an empty input.
//
// This is RegistryWatch's PRIMARY newness rule. The tag-list endpoint exposes no push time, so
// ordering is version-based; HighestByTime layers a build-time tie-break on top for tags that
// carry no version at all (latest, stable, prod).
func Highest(tags []string) string {
	best := ""
	for _, t := range tags {
		if t == "" {
			continue
		}
		if best == "" || compareVersions(t, best) > 0 {
			best = t
		}
	}
	return best
}

// Newer reports whether tag a is a strictly greater version than tag b.
func Newer(a, b string) bool { return compareVersions(a, b) > 0 }

// Versionless reports whether a tag carries no numeric version component at all ("latest",
// "stable", "prod"). Such tags all compare EQUAL to each other, so version ordering cannot tell
// which moved — that is where the build-time tie-break earns its keep.
func Versionless(tag string) bool {
	for _, part := range strings.Split(strings.TrimPrefix(tag, "v"), ".") {
		if _, err := strconv.Atoi(part); err == nil {
			return false
		}
	}
	return true
}

// HighestByTime picks among tags that VERSION ORDERING CANNOT SEPARATE, using each tag's image
// build timestamp as a tie-break. times maps tag → build time; a tag with no usable time is
// skipped.
//
// Deliberately a tie-break and never the primary rule. The timestamp comes from the image config's
// `created` field, which is BUILD time, not push time, and the two diverge in ways that would be
// dangerous as a primary signal:
//
//   - a rebuild-and-repush of an older release stamps it with a NEWER created time than the
//     release that superseded it, so time-first ordering would select v1.2.0 over v2.0.0 and
//     trigger a DOWNGRADE rollout onto robot firmware;
//   - reproducible builds (Bazel, ko, nix, SOURCE_DATE_EPOCH) deliberately pin `created` to the
//     epoch, so every image ties and the signal carries no information at all.
//
// Version order stays authoritative; this only breaks ties it cannot resolve.
func HighestByTime(tags []string, times map[string]time.Time) string {
	best, bestAt := "", time.Time{}
	for _, t := range tags {
		at, ok := times[t]
		if !ok || at.IsZero() {
			continue // no usable build time (absent, or an epoch-pinned reproducible build)
		}
		if best == "" || at.After(bestAt) {
			best, bestAt = t, at
		}
	}
	return best
}

// compareVersions compares two version-ish tags. It strips a leading "v", splits
// on ".", and compares components numerically where both are numeric, else
// lexically. Returns -1/0/1. A missing component counts as lower (1.2 < 1.2.1).
func compareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := component(as, i), component(bs, i)
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		switch {
		case aerr == nil && berr == nil:
			if ai != bi {
				return sign(ai - bi)
			}
		default:
			if av != bv {
				return strings.Compare(av, bv)
			}
		}
	}
	return 0
}

func component(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return "" // missing component sorts lowest (Atoi("")=err, "" < "1" lexically)
}

func sign(d int) int {
	if d < 0 {
		return -1
	}
	if d > 0 {
		return 1
	}
	return 0
}
