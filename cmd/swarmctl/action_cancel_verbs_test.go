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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// `swarmctl cancel action` writes the cancel-requested annotation with a merge patch,
// which the API server authorizes as patch on fleetactions. So the preflight asks for
// patch as well as the cancel verb, and a principal missing either is refused before
// the prompt, not after the operator has confirmed.
func TestActionCancelPreflightsTheVerbsItsWriteUses(t *testing.T) {
	ns := "warehouse-a"
	for _, tc := range []struct {
		held    []string
		wantErr string
	}{
		{held: []string{"patch", "cancel"}},
		{held: []string{"patch"}, wantErr: `"cancel" verb`},
		{held: []string{"cancel"}, wantErr: `"patch" verb`},
	} {
		t.Run(strings.Join(tc.held, "+"), func(t *testing.T) {
			action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "deliver-1", Namespace: ns}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(action).Build()
			var out bytes.Buffer
			o := newTestOptions(&out, cli.OutputTable)
			grant := map[string]bool{}
			for _, v := range tc.held {
				grant[v] = true
			}
			cs := k8sfake.NewSimpleClientset()
			cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				ra := a.(k8stesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview).Spec.ResourceAttributes
				allowed := ra.Resource == "fleetactions" && ra.Group == "swarmada.io" && grant[ra.Verb]
				return true, &authzv1.SelfSubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
			})

			// No --yes and no terminal: a preflight that passes reaches the prompt and
			// stops there, so a refusal naming a verb can only come from before it.
			err := o.actionCancel(context.Background(), c, cs, ns, "deliver-1", "", false)
			if tc.wantErr == "" {
				if err == nil || !strings.Contains(err.Error(), "without --yes") {
					t.Fatalf("holding %v, cancel action did not reach the prompt: %v", tc.held, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("holding %v: err = %v, want a refusal naming %s before the prompt", tc.held, err, tc.wantErr)
			}
			got := &fleetv1.FleetAction{}
			_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "deliver-1"}, got)
			if _, ok := got.Annotations[annCancelRequested]; ok {
				t.Error("a refused cancel wrote the cancel-requested annotation")
			}
		})
	}
}
