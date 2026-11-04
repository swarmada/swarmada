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

package v1

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ADR-0058 §7 states ONE axis-3 subject vocabulary. It is now written down in two
// schema places — the rule an operator authors (FleetZone.spec.restrictions.operating[].subject)
// and the declaration an adapter reports (Robot.status.honouredOperatingConstraints[]) — and
// filter 12 compares a value from one against a value from the other. Two patterns for one
// vocabulary do not merely drift; the drift IS a bypass, because a subject that the zone side
// accepts and the robot side rejects can never be matched, and the robot is withheld forever
// for a rule nobody can satisfy.
//
// A kubebuilder marker has to be a literal string in a comment, so the duplication cannot be
// removed. It can be GATED: one exported constant is the source, and this test asserts that
// every marker spelling in this package is that constant, character for character.
func TestZoneOperatingSubjectPatternIsWrittenOnce(t *testing.T) {
	// Every `+kubebuilder:validation:Pattern=` / `items:Pattern=` marker in this package that
	// guards an axis-3 subject. Keyed by the field it guards, so a failure names it.
	sites := map[string]string{
		"FleetZone.spec.restrictions.operating[].subject": "fleetzone_restrictions_types.go",
		"Robot.status.honouredOperatingConstraints[]":     "robot_types.go",
	}
	marker := regexp.MustCompile("(?m)^\\s*//\\s*\\+kubebuilder:validation:(?:items:)?Pattern=`([^`]*)`")

	found := map[string]int{}
	for field, file := range sites {
		body, err := os.ReadFile(file) // #nosec G304 -- fixed in-package source from the table above
		if err != nil {
			t.Fatalf("%s: cannot read %s: %v", field, file, err)
		}
		var hits int
		for _, m := range marker.FindAllStringSubmatch(string(body), -1) {
			if !strings.Contains(m[1], "a-z0-9") {
				continue // not a DNS-ish subject pattern
			}
			hits++
			if m[1] != ZoneOperatingSubjectPattern {
				t.Errorf("%s (%s) carries a DIFFERENT subject pattern from ZoneOperatingSubjectPattern.\n"+
					"  marker: %s\n  const:  %s\n"+
					"ADR-0058 §7 defines one vocabulary; two patterns for it are a bypass, because a "+
					"subject one side accepts and the other rejects can never be matched by filter 12.",
					field, file, m[1], ZoneOperatingSubjectPattern)
			}
		}
		found[field] = hits
		if hits == 0 {
			t.Errorf("%s (%s) carries no subject Pattern marker at all — the vocabulary is unguarded there",
				field, file)
		}
	}
}

// The constant must actually be the vocabulary ADR-0058 §7 describes: the bare core seven, and
// a namespaced extension whose prefix is a DNS subdomain. Asserted against the compiled regexp
// rather than by eye, because the pattern is what the API server enforces.
func TestZoneOperatingSubjectPatternAcceptsTheVocabulary(t *testing.T) {
	re, err := regexp.Compile(ZoneOperatingSubjectPattern)
	if err != nil {
		t.Fatalf("ZoneOperatingSubjectPattern does not compile: %v", err)
	}
	for _, s := range ZoneCoreOperatingSubjects {
		if !re.MatchString(s) {
			t.Errorf("core subject %q does not match the pattern the schema enforces", s)
		}
	}
	for _, ok := range []string{"example.com/leaf-blower", "vendor.example.co.uk/arm-heater"} {
		if !re.MatchString(ok) {
			t.Errorf("namespaced extension %q was rejected; the vocabulary is closed AT THE CORE, not closed", ok)
		}
	}
	for _, bad := range []string{"Camera", "micro phone", "/leading", "trailing/", "UV", "sensor.microphone "} {
		if re.MatchString(bad) {
			t.Errorf("%q matched; a subject that is not in the vocabulary must not reach status", bad)
		}
	}
}
