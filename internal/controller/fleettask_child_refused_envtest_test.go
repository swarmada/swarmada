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

import (
	"context"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The refused-child path against a REAL API server.
//
// envtest runs no webhook server, so the FleetAction webhook's catalog pre-filter is
// not here. A ValidatingAdmissionPolicy that refuses a Clean stands in for it: the
// API server itself refuses the child, with the status and message shape a real
// refusal has, which is the part the fake client can only imitate.

const cleanRefusal = `action type "Clean" is in no FleetAdapter's status.supportedActions`

func refuseCleanInNamespace(t *testing.T, ns string) (binding *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	// The module's k8s.io/api predates the v1 admission policy types; the API server
	// serves them, so they are written unstructured.
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicy",
		"metadata":   map[string]interface{}{"name": "refuse-clean-" + ns},
		"spec": map[string]interface{}{
			"failurePolicy": "Fail",
			"matchConstraints": map[string]interface{}{
				"resourceRules": []interface{}{map[string]interface{}{
					"operations":  []interface{}{"CREATE"},
					"apiGroups":   []interface{}{"swarmada.io"},
					"apiVersions": []interface{}{"v1"},
					"resources":   []interface{}{"fleetactions"},
				}},
			},
			"validations": []interface{}{map[string]interface{}{
				"expression": "object.spec.type != 'Clean'",
				"message":    cleanRefusal,
			}},
		},
	}}
	if err := envK8s.Create(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	t.Cleanup(func() { _ = envK8s.Delete(context.Background(), policy) })
	binding = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicyBinding",
		"metadata":   map[string]interface{}{"name": "refuse-clean-" + ns},
		"spec": map[string]interface{}{
			"policyName":        "refuse-clean-" + ns,
			"validationActions": []interface{}{"Deny"},
			"matchResources": map[string]interface{}{
				"namespaceSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{"kubernetes.io/metadata.name": ns},
				},
			},
		},
	}}
	if err := envK8s.Create(ctx, binding); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	t.Cleanup(func() { _ = envK8s.Delete(context.Background(), binding) })

	// A policy takes effect asynchronously. Wait until a server-side dry run of a
	// Clean is refused, so the reconcile below meets the refusal and not a race.
	probe := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeClean, Zone: "warehouse-a"},
	}
	waitForAdmission(t, "the policy to refuse a Clean", func() bool {
		err := envK8s.Create(ctx, probe.DeepCopy(), client.DryRunAll)
		return err != nil && isAdmissionRefusal(err)
	})
	return binding
}

func waitForAdmission(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestEnvtest_ClearedDraftWhoseChildIsRefusedSaysSo(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	binding := refuseCleanInNamespace(t, ns)

	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "workorder-clean", Namespace: ns},
		Spec: fleetv1.FleetTaskSpec{
			Draft:            true,
			CompletionPolicy: fleetv1.CompletionPolicyAll,
			Actions: []fleetv1.FleetTaskAction{{
				Name: "clean",
				Action: fleetv1.FleetActionSpec{
					Type: fleetv1.ActionTypeClean, Zone: "warehouse-a",
					RobotSelector: "vacuum-01", Priority: fleetv1.ActionPriorityNormal,
				},
			}},
		},
	}
	if err := envK8s.Create(ctx, task); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	rec := record.NewFakeRecorder(32)
	r := &FleetTaskReconciler{Client: envK8s, Scheme: envScheme, Recorder: rec}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "workorder-clean", Namespace: ns}}
	get := func() *fleetv1.FleetTask {
		t.Helper()
		var ft fleetv1.FleetTask
		if err := envK8s.Get(ctx, req.NamespacedName, &ft); err != nil {
			t.Fatalf("get task: %v", err)
		}
		return &ft
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("hold the draft: %v", err)
	}
	held := get()
	held.Spec.Draft = false // a person clears it
	if err := envK8s.Update(ctx, held); err != nil {
		t.Fatalf("clear the draft: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("reconcile returned nil although the API server refused the child")
	}
	got := get()
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionChildrenAdmitted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "AdmissionRefused" {
		t.Fatalf("ChildrenAdmitted = %+v, want False/AdmissionRefused: the refusal is not on the task", cond)
	}
	for _, want := range []string{`"clean"`, "Clean", "supportedActions"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition message %q does not contain %q", cond.Message, want)
		}
	}
	if got.Status.Phase != fleetv1.FleetTaskPhasePending {
		t.Errorf("phase = %q, want Pending: the refusal is reported, not turned into a failure", got.Status.Phase)
	}

	rv := got.ResourceVersion
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, req); err == nil {
			t.Fatal("a repeated refusal returned nil")
		}
	}
	if now := get().ResourceVersion; now != rv {
		t.Errorf("an unchanged refusal wrote the task again (resourceVersion %s → %s); RA-1", rv, now)
	}
	if n := countPrefixed(drainRecorded(rec), "Warning ChildAdmissionRefused"); n != 1 {
		t.Errorf("Warning ChildAdmissionRefused events = %d over four refused passes, want 1", n)
	}

	// The cause clears.
	if err := envK8s.Delete(ctx, binding); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	waitForAdmission(t, "the child to be admitted", func() bool {
		_, err := r.Reconcile(ctx, req)
		return err == nil
	})
	var child fleetv1.FleetAction
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "workorder-clean-clean", Namespace: ns}, &child); err != nil {
		t.Fatalf("child not created once admitted: %v", err)
	}
	if c := apimeta.FindStatusCondition(get().Status.Conditions, ConditionChildrenAdmitted); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ChildrenAdmitted = %+v after the child was admitted, want True", c)
	}
}

// statusWrites counts FleetTask status writes: every patch or update call, and the
// ones that carry a change. An empty merge patch is still a call to the API server,
// so the two are counted apart and neither can hide in the other.
type statusWrites struct {
	client.Client
	calls, changes *int
}

func (w statusWrites) Status() client.SubResourceWriter {
	return statusWriter{SubResourceWriter: w.Client.Status(), w: w}
}

type statusWriter struct {
	client.SubResourceWriter
	w statusWrites
}

func (s statusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if _, ok := obj.(*fleetv1.FleetTask); ok {
		*s.w.calls++
		if data, err := patch.Data(obj); err != nil || string(data) != "{}" {
			*s.w.changes++
		}
	}
	return s.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (s statusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*fleetv1.FleetTask); ok {
		*s.w.calls++
		*s.w.changes++
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// ChildrenAdmitted does not flap. A child refused, then admitted, then refused again
// moves the condition exactly once each time, with one Warning Event per refusal, and
// passes that change nothing write no status.
func TestEnvtest_ChildrenAdmittedMovesOncePerTransition(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	binding := refuseCleanInNamespace(t, ns)
	rebind := func() {
		t.Helper()
		b := binding.DeepCopy()
		b.SetResourceVersion("")
		b.SetUID("")
		b.SetCreationTimestamp(metav1.Time{})
		b.SetManagedFields(nil)
		if err := envK8s.Create(ctx, b); err != nil {
			t.Fatalf("re-create binding: %v", err)
		}
		probe := &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: ns},
			Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeClean, Zone: "warehouse-a"},
		}
		waitForAdmission(t, "the policy to refuse a Clean again", func() bool {
			err := envK8s.Create(ctx, probe.DeepCopy(), client.DryRunAll)
			return err != nil && isAdmissionRefusal(err)
		})
	}

	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "workorder-clean", Namespace: ns},
		Spec: fleetv1.FleetTaskSpec{
			CompletionPolicy: fleetv1.CompletionPolicyAll,
			Actions: []fleetv1.FleetTaskAction{{
				Name: "clean",
				Action: fleetv1.FleetActionSpec{
					Type: fleetv1.ActionTypeClean, Zone: "warehouse-a",
					RobotSelector: "vacuum-01", Priority: fleetv1.ActionPriorityNormal,
				},
			}},
		},
	}
	if err := envK8s.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	calls, changes := 0, 0
	rec := record.NewFakeRecorder(64)
	r := &FleetTaskReconciler{
		Client: statusWrites{Client: envK8s, calls: &calls, changes: &changes},
		Scheme: envScheme, Recorder: rec,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "workorder-clean", Namespace: ns}}
	cond := func() metav1.Condition {
		t.Helper()
		var ft fleetv1.FleetTask
		if err := envK8s.Get(ctx, req.NamespacedName, &ft); err != nil {
			t.Fatalf("get task: %v", err)
		}
		c := apimeta.FindStatusCondition(ft.Status.Conditions, ConditionChildrenAdmitted)
		if c == nil {
			t.Fatal("no ChildrenAdmitted condition")
		}
		return *c
	}
	settle := func(stage string, wantErr bool) {
		t.Helper()
		// Four passes with nothing changing between them: none may carry a change.
		c0 := calls
		ch0 := changes
		for i := 0; i < 4; i++ {
			_, err := r.Reconcile(ctx, req)
			if (err != nil) != wantErr {
				t.Fatalf("%s: pass %d returned %v", stage, i, err)
			}
		}
		if changes != ch0 {
			t.Errorf("%s: %d status write(s) carrying a change over four unchanged passes, want 0 (RA-1)", stage, changes-ch0)
		}
		t.Logf("%s: %d status patch call(s) over four unchanged passes, %d carrying a change", stage, calls-c0, changes-ch0)
	}
	type step struct {
		status metav1.ConditionStatus
		ltt    metav1.Time
	}
	var seen []step
	note := func(stage string, want metav1.ConditionStatus) {
		t.Helper()
		c := cond()
		if c.Status != want {
			t.Fatalf("%s: ChildrenAdmitted = %s, want %s", stage, c.Status, want)
		}
		seen = append(seen, step{c.Status, c.LastTransitionTime})
	}

	// 1. Refused.
	ch := changes
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("refused child: reconcile returned nil")
	}
	note("refused", metav1.ConditionFalse)
	settle("refused", true)
	if got := cond(); !got.LastTransitionTime.Equal(&seen[0].ltt) {
		t.Errorf("refused: the condition moved during unchanged passes (%v -> %v)", seen[0].ltt, got.LastTransitionTime)
	}
	if n := changes - ch; n != 1 {
		t.Errorf("refused: %d status write(s) carrying a change, want exactly 1", n)
	}

	// 2. Admitted.
	if err := envK8s.Delete(ctx, binding); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	waitForAdmission(t, "the child to be admitted", func() bool {
		_, err := r.Reconcile(ctx, req)
		return err == nil
	})
	note("admitted", metav1.ConditionTrue)
	settle("admitted", false)

	// 3. Refused again: the policy is back and the child has to be created afresh.
	rebind()
	if err := envK8s.Delete(ctx, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{
		Name: "workorder-clean-clean", Namespace: ns}}); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	ch = changes
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("refused again: reconcile returned nil")
	}
	note("refused again", metav1.ConditionFalse)
	if n := changes - ch; n != 1 {
		t.Errorf("refused again: %d status write(s) carrying a change, want exactly 1", n)
	}
	settle("refused again", true)

	if n := countPrefixed(drainRecorded(rec), "Warning ChildAdmissionRefused"); n != 2 {
		t.Errorf("Warning ChildAdmissionRefused events = %d over two refusals, want 2", n)
	}
	// Three observations, alternating: False, True, False. LastTransitionTime has
	// one-second resolution, so two transitions inside one second may share a stamp;
	// what may not happen is a stamp moving during the unchanged passes (checked above).
	for i := 1; i < len(seen); i++ {
		if seen[i].status == seen[i-1].status {
			t.Errorf("observation %d repeats %s: not a transition", i, seen[i].status)
		}
		if seen[i].ltt.Before(&seen[i-1].ltt) {
			t.Errorf("observation %d: LastTransitionTime went backwards", i)
		}
	}
	if final := cond(); !final.LastTransitionTime.Equal(&seen[2].ltt) {
		t.Errorf("refused again: the condition moved during unchanged passes")
	}
}
