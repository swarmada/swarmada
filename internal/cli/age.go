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

package cli

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
)

// clock is time.Now, indirected so tests can pin "now" for deterministic AGE.
var clock = time.Now

// Age renders a creation-style timestamp as a compact relative age ("14d",
// "4m"), exactly as kubectl's AGE column does (via apimachinery's
// duration.HumanDuration). A zero time renders as the em-dash placeholder.
func Age(t metav1.Time) string {
	if t.IsZero() {
		return Unknown
	}
	return duration.HumanDuration(clock().Sub(t.Time))
}

// AgePtr is Age for an optional timestamp; nil renders "<none>".
func AgePtr(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return None
	}
	return Age(*t)
}

// DateColumn renders a kubebuilder `type=date` print column. For a past instant
// it reads as an age ("9d"); for a future instant (e.g. a TTL or lease expiry)
// it reads as time remaining ("in 3m"), which is what an operator scanning a
// DiscoveredRobot TTL actually wants. nil renders "<none>".
func DateColumn(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return None
	}
	now := clock()
	if t.After(now) {
		return "in " + duration.HumanDuration(t.Sub(now))
	}
	return duration.HumanDuration(now.Sub(t.Time))
}
