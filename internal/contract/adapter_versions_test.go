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

package contract

import (
	"os"
	"regexp"
	"testing"
)

// Every adapter in this tree declares the contract version it implements as a CONTRACT_VERSION
// module constant, and sends it on AdapterHello. The handshake gate refuses REGISTRATION for an
// adapter that reports nothing or reports out of range (ADR-0032), so a missing or drifted constant
// means that adapter silently stops being able to bring a robot under management — it would still
// connect and still stream telemetry, which is exactly why the failure is easy to miss in a demo.
//
// This is asserted through the real [Supports], not against [Version] directly: the supported range
// spans minor N and N-1, so an adapter one minor behind is CORRECT and must not fail here. The test
// fails precisely when a live control plane built from this tree would refuse the adapter.
//
// The constants are deliberately per-package rather than a shared import: the packages under
// adapters/external/ are standalone distributions modelling third-party adapters (they cannot import
// swarmada internals), and adapters/template/ becomes third-party code as soon as it is used. So the
// duplication is the shape an adapter author must actually write, and this test is what keeps it
// honest.
func TestInTreeAdaptersReportASupportedContractVersion(t *testing.T) {
	// Every path is a fixed in-repo source, never caller-supplied; they are iterated (rather than
	// read one by one) only so the failure message can name the adapter.
	for name, path := range map[string]string{
		"simulation":   "../../adapters/simulation/sim_adapter.py",
		"example-noop": "../../adapters/example-noop/noop_adapter.py",
		"template":     "../../adapters/template/{{cookiecutter.adapter_slug}}/{{cookiecutter.python_package}}/adapter.py",
		"ros2":         "../../adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py",
		"vda5050":      "../../adapters/external/fleet-adapter-vda5050/fleet_adapter_vda5050/adapter.py",
		"mavlink":      "../../adapters/external/fleet-adapter-mavlink/fleet_adapter_mavlink/adapter.py",
	} {
		t.Run(name, func(t *testing.T) {
			// Only adapters/simulation/ ships in this repo; example-noop, template and the
			// external reference adapters are released separately. Skip the ones that are not
			// present here rather than failing — an adapter that IS in the tree is still checked.
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Skipf("%s is not part of this repository", path)
			}
			body, err := os.ReadFile(path) // #nosec G304 -- fixed in-repo path from the table above
			if err != nil {
				t.Fatalf("cannot read %s: %v", path, err)
			}

			m := regexp.MustCompile(`(?m)^CONTRACT_VERSION\s*=\s*"([^"]*)"`).FindSubmatch(body)
			if m == nil {
				t.Fatalf("%s declares no CONTRACT_VERSION constant; without one it cannot report "+
					"contract_version on AdapterHello and its registrations will be refused "+
					"VERSION_MISMATCH", path)
			}
			declared := string(m[1])
			if ok, reason := Supports(declared); !ok {
				t.Errorf("%s declares CONTRACT_VERSION = %q, which this control plane would refuse: %s",
					path, declared, reason)
			}

			// Declaring the constant is not enough — it has to reach the wire. A constant that is
			// never passed to AdapterHello is the same outage with a passing grep.
			if !regexp.MustCompile(`contract_version=CONTRACT_VERSION`).Match(body) {
				t.Errorf("%s never passes contract_version=CONTRACT_VERSION to AdapterHello; the "+
					"constant alone does not reach the handshake", path)
			}
		})
	}
}
