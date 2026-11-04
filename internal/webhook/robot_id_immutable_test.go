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

package webhook

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// robotIDChange is the rule the gate runs on every update. A Robot stored before the
// defaulter backfilled the id has none, and its wire id is its name; the backfill writes
// exactly that, so it is the one value an unset id may take.
func TestRobotIDChange(t *testing.T) {
	withID := func(id string) *fleetv1.Robot {
		r := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: "ns"}}
		if id != "" {
			r.Annotations = map[string]string{fleetv1.RobotIDAnnotation: id}
		}
		return r
	}
	for _, tc := range []struct {
		name      string
		old, new  string
		wantError bool
	}{
		{"unchanged", "wire-1", "wire-1", false},
		{"changed", "wire-1", "wire-2", true},
		{"removed", "wire-1", "", true},
		{"unset stays unset", "", "", false},
		{"unset backfilled to the name", "", "amr-1", false},
		{"unset set to another value", "", "wire-9", true},
		{"name kept", "amr-1", "amr-1", false},
		{"name removed", "amr-1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := robotIDChange(withID(tc.old), withID(tc.new))
			if (err != nil) != tc.wantError {
				t.Fatalf("robot-id %q -> %q: err=%v, want error=%v", tc.old, tc.new, err, tc.wantError)
			}
		})
	}
}
