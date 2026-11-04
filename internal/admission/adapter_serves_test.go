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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func adapterServing(classes ...string) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "sim-fleet-adapter"},
		Spec:       fleetv1.FleetAdapterSpec{ServesRobotClasses: classes},
	}
}

// The rule the Robot admission gate applies at create (RFC-0001 #fleetadapter-admission-gating):
// an adapter listing no classes serves any, including none; one listing classes serves only those.
func TestCheckAdapterServes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter *fleetv1.FleetAdapter
		class   string
		want    []string // substrings of the refusal; nil means accepted
	}{
		{"serves-any, no class", adapterServing(), "", nil},
		{"serves-any, a class", adapterServing(), "anything", nil},
		{"listed class", adapterServing("simbot-v1", "amr-standard"), "amr-standard", nil},
		{"one class, none given", adapterServing("simbot-v1"), "", []string{"--class is required", "--class simbot-v1"}},
		{"several, none given", adapterServing("a", "b"), "", []string{"--class is required", "a, b", "naming one of them"}},
		{"unlisted class", adapterServing("simbot-v1"), "amr-standard", []string{`does not serve RobotClass "amr-standard"`, "simbot-v1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckAdapterServes(tc.adapter, tc.class)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want refused, got accepted")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err, w)
				}
			}
		})
	}
}
