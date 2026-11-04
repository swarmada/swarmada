package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// verbAuthorizer answers each SelfSubjectAccessReview from a fixed set of verbs
// held on fleettasks, and records which verbs were asked about.
func verbAuthorizer(held ...string) (*k8sfake.Clientset, *[]string) {
	grant := map[string]bool{}
	for _, v := range held {
		grant[v] = true
	}
	var asked []string
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ra := a.(k8stesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview).Spec.ResourceAttributes
		asked = append(asked, ra.Verb)
		allowed := ra.Resource == "fleettasks" && ra.Group == "swarmada.io" && grant[ra.Verb]
		return true, &authzv1.SelfSubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	return cs, &asked
}

func TestTaskCancelPreflightsTheVerbsItsWriteUses(t *testing.T) {
	ns := "warehouse-a"
	for _, tc := range []struct {
		held    []string
		wantErr string
	}{
		{held: []string{"patch", "cancel"}},
		{held: []string{"update", "patch"}, wantErr: `"cancel" verb`},
		{held: []string{"update", "cancel"}, wantErr: `"patch" verb`},
	} {
		t.Run(strings.Join(tc.held, "+"), func(t *testing.T) {
			task := testTask(ns, "restock-7", fleetv1.DesiredStateRunning, 1)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).Build()
			o := newTestOptions(&bytes.Buffer{}, cli.OutputTable)
			cs, asked := verbAuthorizer(tc.held...)
			err := o.taskCancel(context.Background(), c, cs, ns, "restock-7", "", true)
			t.Logf("verbs asked: %v; err: %v", *asked, err)
			got := &fleetv1.FleetTask{}
			_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "restock-7"}, got)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("holding %v, cancel task was refused: %v", tc.held, err)
				}
				if got.Spec.DesiredState != fleetv1.DesiredStateCancelled {
					t.Errorf("desiredState = %q, want Cancelled", got.Spec.DesiredState)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("holding %v: err = %v, want a refusal naming %s", tc.held, err, tc.wantErr)
			}
			if got.Spec.DesiredState == fleetv1.DesiredStateCancelled {
				t.Error("a refused cancel wrote desiredState")
			}
		})
	}
}
