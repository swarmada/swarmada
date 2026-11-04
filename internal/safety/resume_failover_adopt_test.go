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

import "testing"

// A clear of this leader's own arms its window just after writing Resuming. A request that
// read no baseline in between adopts the robot after that arm: the adoption leaves the
// clear's window, and its baseline, as they are.
func TestResumeFailover_AdoptionLeavesAnOwnClearsWindow(t *testing.T) {
	d, _, t0 := newLeaderOverResuming(t)
	key := robotEstopKey(ns, "amr-1")

	d.armResumeWindow(key, t0)
	d.adoptResumeWindow(key)
	if since, armed, adopted := d.resumeBaseline(key); !armed || adopted || !since.Equal(t0) {
		t.Fatalf("after the clear's arm and an adoption: baseline %s, armed %v, adopted %v; want %s, true, false",
			since, armed, adopted, t0)
	}
}
