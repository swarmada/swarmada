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

package admission

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// robotCRDAdapterVersionPattern reads the pattern the generated Robot CRD applies to
// spec.adapter.version. Read from the CRD rather than restated, so the builder is held to what
// the API server enforces, not to a copy of it.
func robotCRDAdapterVersionPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "swarmada.io_robots.yaml"))
	if err != nil {
		t.Fatalf("reading the Robot CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Adapter struct {
										Properties struct {
											Version struct {
												Pattern string `json:"pattern"`
											} `json:"version"`
										} `json:"properties"`
									} `json:"adapter"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing the Robot CRD: %v", err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("the Robot CRD declares no versions")
	}
	pattern := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Adapter.Properties.Version.Pattern
	if pattern == "" {
		t.Fatal("the Robot CRD declares no pattern for spec.adapter.version")
	}
	return regexp.MustCompile(pattern)
}

// A version the Robot CRD refuses must be refused by the builder, because both admission paths
// act on the builder's answer. Built anyway, the CLI reported success for an admission the API
// server then refused on every attempt: the simulation adapter reports 0.1.0-sim.
func TestBuildRobot_AdapterVersionIsHeldToTheRobotCRDPattern(t *testing.T) {
	crd := robotCRDAdapterVersionPattern(t)
	for _, v := range []string{"1.2.3", "0.1.0", "10.20.30", "0.1.0-sim", "1.0", "", "v1.2.3", "1.2.3.4", "1.2.3\n"} {
		dr := discoveredFixture()
		dr.Status.AdapterVersion = v
		_, err := BuildRobot(dr, Params{Zone: "z", Adapter: "a"}, nil, "warehouse-a")
		if accepted := crd.MatchString(v); accepted != (err == nil) {
			t.Errorf("version %q: the CRD accepts=%v, the builder err=%v", v, accepted, err)
		}
	}
}

// The refusal names the field and the pattern, and — when the version came from discovery — the
// flag that fixes it.
func TestBuildRobot_RefusedVersionNamesTheFieldThePatternAndTheFix(t *testing.T) {
	dr := discoveredFixture()
	dr.Status.AdapterVersion = "0.1.0-sim"
	_, err := BuildRobot(dr, Params{Zone: "z", Adapter: "sim-fleet-adapter"}, nil, "warehouse-a")
	if err == nil {
		t.Fatal("0.1.0-sim must be refused")
	}
	for _, want := range []string{"spec.adapter.version", `"0.1.0-sim"`, `^\d+\.\d+\.\d+$`, "status.adapterVersion", "--class"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

// A class that pins a release version is the lossless route past a pre-release report: the
// class version replaces the reported one before the check.
func TestBuildRobot_ClassVersionStandsInForAReportedPrerelease(t *testing.T) {
	dr := discoveredFixture()
	dr.Status.AdapterVersion = "0.1.0-sim"
	class := classFixture()
	class.Spec.BaseAdapter.Version = "0.1.0"
	robot, err := BuildRobot(dr, Params{Zone: "z"}, class, "warehouse-a")
	if err != nil {
		t.Fatalf("a class-pinned release version must build: %v", err)
	}
	if robot.Spec.Adapter.Version != "0.1.0" {
		t.Errorf("adapter version = %q, want the class's 0.1.0", robot.Spec.Adapter.Version)
	}
}

// Not normalised. Cutting 0.1.0-sim to 0.1.0 would make the Robot claim a release build the
// adapter never reported, so no version the builder returns differs from its source.
func TestBuildRobot_ReportedVersionIsNeverRewritten(t *testing.T) {
	dr := discoveredFixture()
	dr.Status.AdapterVersion = "1.4.2"
	robot, err := BuildRobot(dr, Params{Zone: "z", Adapter: "a"}, nil, "warehouse-a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if robot.Spec.Adapter.Version != "1.4.2" {
		t.Errorf("adapter version = %q, want the reported 1.4.2 unchanged", robot.Spec.Adapter.Version)
	}
}

// A class whose own version the CRD refuses is named as the source, with no --class hint: the
// operator already passed one, and it is the class that needs fixing.
func TestBuildRobot_RefusedClassVersionNamesTheClass(t *testing.T) {
	class := classFixture()
	class.Spec.BaseAdapter.Version = "2.0"
	_, err := BuildRobot(discoveredFixture(), Params{Zone: "z"}, class, "warehouse-a")
	if err == nil || !strings.Contains(err.Error(), "robotclass/acme-class spec.baseAdapter.version") {
		t.Fatalf("want the class named as the source, got %v", err)
	}
	if strings.Contains(err.Error(), "pass --class") {
		t.Errorf("a refusal caused by the class must not tell the operator to pass a class: %v", err)
	}
}
