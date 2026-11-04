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

package controller

// A FleetTask pass that changes nothing in the status must not call the status
// subresource at all. An empty merge patch leaves the resourceVersion where it was,
// so it is invisible to a resourceVersion check, but it is still one API request per
// pass per task, a status write with nothing to write (RA-1, RFC-0001
// #fleettask-ownership-status-aggregation-and-ra-1). Both scheduling paths, dependsOn and flow, are measured here by counting
// calls to the status writer, not by watching the resourceVersion.

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestEnvtest_FleetTaskUnchangedPassWritesNoStatus(t *testing.T) {
	requireEnvtest(t)
	member := fleetv1.FleetTaskAction{Name: "go", Action: fleetv1.FleetActionSpec{
		Type: fleetv1.ActionTypeNavigate, Zone: "yard-west", Priority: fleetv1.ActionPriorityNormal}}
	cases := []struct {
		name  string
		child string
		spec  fleetv1.FleetTaskSpec
	}{
		{
			name:  "dependsOn",
			child: "steady-go",
			spec: fleetv1.FleetTaskSpec{
				CompletionPolicy: fleetv1.CompletionPolicyAll,
				Actions:          []fleetv1.FleetTaskAction{member},
			},
		},
		{
			name:  "flow",
			child: iterationChildName("steady", "go", 0),
			spec: fleetv1.FleetTaskSpec{
				CompletionPolicy: fleetv1.CompletionPolicyAll,
				Actions:          []fleetv1.FleetTaskAction{member},
				Flow:             &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("go")}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ns := envtestNamespace(t)
			task := &fleetv1.FleetTask{ObjectMeta: metav1.ObjectMeta{Name: "steady", Namespace: ns}, Spec: tc.spec}
			if err := envK8s.Create(ctx, task); err != nil {
				t.Fatalf("create task: %v", err)
			}
			calls, changes := 0, 0
			r := &FleetTaskReconciler{
				Client: statusWrites{Client: envK8s, calls: &calls, changes: &changes},
				Scheme: envScheme, Recorder: record.NewFakeRecorder(16),
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "steady", Namespace: ns}}
			get := func() fleetv1.FleetTask {
				t.Helper()
				var ft fleetv1.FleetTask
				if err := envK8s.Get(ctx, req.NamespacedName, &ft); err != nil {
					t.Fatalf("get task: %v", err)
				}
				return ft
			}
			reconcile := func(n int) {
				t.Helper()
				for i := 0; i < n; i++ {
					if _, err := r.Reconcile(ctx, req); err != nil {
						t.Fatalf("reconcile: %v", err)
					}
				}
			}
			// steady runs five passes over a task nothing has touched since the
			// settling passes, and requires that none of them calls the status writer.
			steady := func(stage string) {
				t.Helper()
				reconcile(2) // settle: create the child and project it
				rv0, c0, ch0 := get().ResourceVersion, calls, changes
				reconcile(5)
				rv1 := get().ResourceVersion
				t.Logf("%s/%s: 5 unchanged passes -> %d status patch calls (%d carrying a change), resourceVersion %s -> %s",
					tc.name, stage, calls-c0, changes-ch0, rv0, rv1)
				if calls != c0 {
					t.Errorf("%s: 5 unchanged passes made %d status patch calls (%d of them empty); want 0",
						stage, calls-c0, (calls-c0)-(changes-ch0))
				}
				if rv1 != rv0 {
					t.Errorf("%s: resourceVersion moved %s -> %s on unchanged passes", stage, rv0, rv1)
				}
			}

			steady("child created, phase unset")

			// A real change is still written: the child moving to InProgress takes
			// the task to Running on the next pass, with exactly one status write.
			var child fleetv1.FleetAction
			if err := envK8s.Get(ctx, types.NamespacedName{Name: tc.child, Namespace: ns}, &child); err != nil {
				t.Fatalf("get child: %v", err)
			}
			child.Status.Phase = fleetv1.ActionPhaseInProgress
			if err := envK8s.Status().Update(ctx, &child); err != nil {
				t.Fatalf("update child status: %v", err)
			}
			c0, ch0 := calls, changes
			reconcile(1)
			if calls-c0 != 1 || changes-ch0 != 1 {
				t.Errorf("child InProgress: the pass made %d status patch calls, %d carrying a change; want 1 and 1",
					calls-c0, changes-ch0)
			}
			if got := get().Status.Phase; got != fleetv1.FleetTaskPhaseRunning {
				t.Errorf("task phase = %q after the child went InProgress, want Running", got)
			}

			steady("child InProgress (task Running)")
		})
	}
}
