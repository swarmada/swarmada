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
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// An update that leaves a rollout's spec unchanged is not re-checked against the signing policy:
// the check is about the artifact the spec names, and the controller's own bookkeeping on the
// rollout (the processed-resume marker) must land whatever the policy says now. A spec change is
// still checked, from any principal.
func TestRolloutSigning_UpdateWithUnchangedSpecIsNotRechecked(t *testing.T) {
	rows := []struct {
		name    string
		config  []client.Object // what the namespace holds
		oldRef  string          // signature reference on the stored object
		newRef  string          // on the incoming object
		labeled bool            // the update adds a label as well
		admit   bool
	}{
		{name: "enforced, stored unsigned, metadata only", config: []client.Object{signingConfig(fwNS, true)}, oldRef: "", newRef: "", labeled: true, admit: true},
		{name: "no SwarmadaConfig, metadata only", config: nil, oldRef: "sig", newRef: "sig", labeled: true, admit: true},
		{name: "enforced, spec change removes the reference", config: []client.Object{signingConfig(fwNS, true)}, oldRef: "sig", newRef: "", admit: false},
		{name: "no SwarmadaConfig, spec change", config: nil, oldRef: "sig", newRef: "sig-2", admit: false},
		{name: "enforced, spec change adds the reference", config: []client.Object{signingConfig(fwNS, true)}, oldRef: "", newRef: "sig", admit: true},
	}
	for _, row := range rows {
		t.Run("firmware/"+row.name, func(t *testing.T) {
			oldObj, newObj := rollout(row.oldRef), rollout(row.newRef)
			if row.labeled {
				newObj.Labels = map[string]string{"team": "line-2"}
				newObj.Annotations = map[string]string{"swarmada.io/rollout-resume-processed": "r1 excluded"}
			}
			_, err := fwValidator(t, row.config...).ValidateUpdate(context.Background(), oldObj, newObj)
			if row.admit && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !row.admit && err == nil {
				t.Fatal("admitted a spec change the signing check must see")
			}
		})
		t.Run("model/"+row.name, func(t *testing.T) {
			oldObj, newObj := modelRollout(row.oldRef), modelRollout(row.newRef)
			if row.labeled {
				newObj.Labels = map[string]string{"team": "line-2"}
				newObj.Annotations = map[string]string{"swarmada.io/rollout-resume-processed": "r1 excluded"}
			}
			_, err := mrValidator(t, row.config...).ValidateUpdate(context.Background(), oldObj, newObj)
			if row.admit && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !row.admit && err == nil {
				t.Fatal("admitted a spec change the signing check must see")
			}
		})
	}
}
