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
	"strings"
	"testing"
)

// TestOutcomeCertaintyEmptyIsNotEstablished is the assertion that matters here.
//
// No v0.4 controller writes FleetAction.status.outcomeCertainty, so EVERY action an
// operator looks at today has the field empty. The one reading that must never be
// rendered is "Established": empty means the control plane recorded nothing, and
// showing it as an observation would put the exact false statement the field was
// added to remove back on the screen the field was added for. It renders as None.
func TestOutcomeCertaintyEmptyIsNotEstablished(t *testing.T) {
	got := OutcomeCertaintyCell("").Text()
	if got != None {
		t.Errorf("OutcomeCertaintyCell(%q).Text() = %q, want %q", "", got, None)
	}
	if strings.Contains(got, "Established") {
		t.Errorf("an unwritten outcomeCertainty rendered as %q — empty is not an observation", got)
	}
}

// TestOutcomeCertaintyUnestablishedIsDistinguishable checks that the value an
// operator must act on does not render identically to the one they need not.
// Unestablished always appears beside phase: Failed (the R4 CEL pin), so if the two
// certainties styled the same, the column would carry no information the PHASE
// column did not already carry.
func TestOutcomeCertaintyUnestablishedIsDistinguishable(t *testing.T) {
	unestablished := OutcomeCertaintyCell("Unestablished")
	established := OutcomeCertaintyCell("Established")

	if unestablished.Text() != "Unestablished" {
		t.Errorf("Unestablished rendered as %q", unestablished.Text())
	}
	if established.Text() != "Established" {
		t.Errorf("Established rendered as %q", established.Text())
	}
	if unestablished.Render(true) == established.Render(true) {
		t.Errorf("Unestablished and Established render identically (%q) — an outcome nobody "+
			"could establish must not look like one that was observed",
			unestablished.Render(true))
	}
}
