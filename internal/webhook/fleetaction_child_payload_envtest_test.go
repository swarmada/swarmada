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
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// A FleetTask's child with a payload, against the real API server with the webhooks served. The
// old and new objects reach the guard as the API server encodes them, not as a test spells them,
// so these rows hold what the guard's unit rows cannot: that a write leaving the payload as it was
// reads as leaving it as it was, whoever wrote it and however they spelled it.
func TestEnvtest_AChildActionWithAPayload(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()

	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(w.cfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}
	bind := func(name, user, role string) {
		t.Helper()
		if err := rc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
		}); err != nil {
			t.Fatalf("binding %s to %s: %v", user, role, err)
		}
	}
	for _, cr := range loadClusterRoles(t, filepath.Join("..", "..", "config", "rbac", "swarmada_operator_role.yaml")) {
		if err := rc.Create(ctx, cr); err != nil {
			t.Fatalf("installing %s: %v", cr.Name, err)
		}
		bind("operator-olivia", "olivia", cr.Name)
	}
	operator := asIdentity(t, w, "olivia")
	// A person who may write fleetactions and holds no `cancel`.
	writerRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "fleetaction-writer-" + w.ns},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{fleetv1.GroupVersion.Group}, Resources: []string{"fleetactions"},
			Verbs: []string{"get", "list", "watch", "patch", "update"}}},
	}
	if err := rc.Create(ctx, writerRole); err != nil {
		t.Fatalf("installing %s: %v", writerRole.Name, err)
	}
	bind("writer-wes", "wes", writerRole.Name)
	writer := asIdentity(t, w, "wes")
	manager := w.as(t, controllerUser)

	action := func(t *testing.T, name string) *fleetv1.FleetAction {
		t.Helper()
		var fa fleetv1.FleetAction
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &fa); err != nil {
			t.Fatalf("reading action %s: %v", name, err)
		}
		return &fa
	}
	// wire is the payload as the API serves it; "" when there is none.
	wire := func(t *testing.T, fa *fleetv1.FleetAction) string {
		t.Helper()
		if fa.Spec.Payload == nil {
			return ""
		}
		b, err := json.Marshal(fa.Spec.Payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	reconcileTask := func(name string) {
		t.Helper()
		tr := &controller.FleetTaskReconciler{Client: manager, Scheme: w.scheme, Recorder: record.NewFakeRecorder(64)}
		for i := 0; i < 2; i++ {
			if _, err := tr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: w.ns, Name: name}}); err != nil {
				t.Logf("task %s reconcile: %v", name, err)
			}
		}
	}
	// child creates a FleetTask with one member carrying the payload written as payloadJSON
	// (none when empty), lets the controller fan it out, and returns the child's name.
	child := func(t *testing.T, task, payloadJSON string) string {
		t.Helper()
		member := fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert,
			Priority: fleetv1.ActionPriorityNormal}
		if payloadJSON != "" {
			member.Payload = &fleetv1.ActionPayload{}
			if err := json.Unmarshal([]byte(payloadJSON), member.Payload); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.admin.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: task},
			Spec:       fleetv1.FleetTaskSpec{Actions: []fleetv1.FleetTaskAction{{Name: "a", Action: member}}},
		}); err != nil {
			t.Fatalf("creating task %s: %v", task, err)
		}
		reconcileTask(task)
		name := task + "-a"
		fa := action(t, name)
		if metav1.GetControllerOf(fa) == nil {
			t.Fatalf("precondition: %s is not controlled by its task", name)
		}
		if payloadJSON != "" && fa.Spec.Payload == nil {
			t.Fatalf("precondition: %s did not take its task's payload", name)
		}
		return name
	}
	patch := func(c client.Client, name, body string) error {
		return c.Patch(ctx, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name}},
			client.RawPatch(types.MergePatchType, []byte(body)))
	}
	// apply server-side-applies a manifest whose payload is spelled exactly as payloadJSON.
	apply := func(c client.Client, name, payloadJSON string, opts ...client.PatchOption) error {
		body := fmt.Sprintf(`{"apiVersion":%q,"kind":"FleetAction","metadata":{"name":%q,"namespace":%q},`+
			`"spec":{"type":%q,"onFailure":%q,"priority":%q,"payload":%s}}`,
			fleetv1.GroupVersion.String(), name, w.ns,
			fleetv1.ActionTypeNavigate, fleetv1.ActionFailureAlert, fleetv1.ActionPriorityNormal, payloadJSON)
		return c.Patch(ctx, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name}},
			client.RawPatch(types.ApplyPatchType, []byte(body)), append(opts, client.FieldOwner("a-person"))...)
	}
	// forced takes the fields from the controller's field manager, as --force-conflicts does, so
	// that the apply reaches admission rather than stopping at a conflict.
	forced := client.ForceOwnership
	admitted := func(t *testing.T, err error, what string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s was refused: %v", what, err)
		}
	}
	// refusedAsASpecChange holds the refusal to the guard's own: a webhook that fails also
	// "refuses", and that is the defect these rows exist for.
	refusedAsASpecChange := func(t *testing.T, err error, what string) {
		t.Helper()
		switch {
		case err == nil:
			t.Errorf("%s was admitted", what)
		case !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "may not change the spec"):
			t.Errorf("%s was refused, but not by the child rule: %v", what, err)
		}
	}
	const payload = `{"targetPose":{"x":4,"y":2},"zone":"aisle-4"}`

	t.Run("an-operator-cancels-a-held-child", func(t *testing.T) {
		markerFixture(t, w, w.ns)
		// held creates a child with a payload and a robot that records it, then stops the robot.
		held := func(t *testing.T, task string) string {
			t.Helper()
			name := child(t, task, payload)
			// The Robot webhook reads the adapter through the manager's cache, which can lag the
			// fixture's status write; only that refusal is retried, as the estop rows do.
			var robot *fleetv1.Robot
			var err error
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
				robot = markerRobot(w.ns, "amr-"+task, nil)
				err = w.admin.Create(ctx, robot)
				if err == nil || !strings.Contains(err.Error(), "is not ready: phase=Pending conformance=Unknown") ||
					time.Now().After(deadline) {
					break
				}
			}
			if err != nil {
				t.Fatalf("creating %s: %v", robot.Name, err)
			}
			base := robot.DeepCopy()
			robot.Status.AssignedAction = name
			if err := manager.Status().Patch(ctx, robot, client.MergeFrom(base)); err != nil {
				t.Fatalf("recording %s on %s: %v", name, robot.Name, err)
			}
			base = robot.DeepCopy()
			robot.Annotations = map[string]string{estopTriggeredAnnotation: "payload-row"}
			if err := w.admin.Patch(ctx, robot, client.MergeFrom(base)); err != nil {
				t.Fatalf("stopping %s: %v", robot.Name, err)
			}
			return name
		}
		byRequest := held(t, "held-request")
		admitted(t, patch(operator, byRequest, `{"metadata":{"annotations":{"swarmada.io/cancel-requested":"order withdrawn"}}}`),
			"the operator's cancel request on a held child")
		if _, ok := action(t, byRequest).Annotations[cancelRequestedAnnotation]; !ok {
			t.Errorf("the cancel request on %s did not land", byRequest)
		}
		byState := held(t, "held-state")
		admitted(t, patch(operator, byState, `{"spec":{"desiredState":"Cancelled"}}`), "the operator's cancel of a held child")
		fa := action(t, byState)
		if fa.Spec.DesiredState != fleetv1.DesiredStateCancelled {
			t.Errorf("desiredState %q on %s, want Cancelled", fa.Spec.DesiredState, byState)
		}
		if got := wire(t, fa); got != payload {
			t.Errorf("the cancel moved the payload: %s, want %s", got, payload)
		}
	})

	t.Run("cancelling-without-the-verb-is-refused-by-the-child-rule", func(t *testing.T) {
		name := child(t, "no-verb", payload)
		err := patch(writer, name, `{"metadata":{"annotations":{"swarmada.io/cancel-requested":"order withdrawn"}}}`)
		if err == nil || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "needs the cancel verb") {
			t.Errorf("want the child rule's refusal for want of the cancel verb, got %v", err)
		}
	})

	// The vfleetaction hook is not registered for the status subresource, so these two never
	// reached the guard. They are held here so that stays true of a child with a payload.
	t.Run("the-controller-writes-a-childs-status", func(t *testing.T) {
		name := child(t, "cp-status", payload)
		fa := action(t, name)
		base := fa.DeepCopy()
		fa.Status.AssignedRobot = "amr-1"
		admitted(t, manager.Status().Patch(ctx, fa, client.MergeFrom(base)), "the controller's status patch")
		fa = action(t, name)
		fa.Status.AssignedRobot = "amr-2"
		admitted(t, manager.Status().Update(ctx, fa), "the controller's status update")
		if got := action(t, name).Status.AssignedRobot; got != "amr-2" {
			t.Errorf("assignedRobot %q, want amr-2", got)
		}
	})

	t.Run("the-controller-updates-a-childs-spec-and-metadata", func(t *testing.T) {
		name := child(t, "cp-spec", payload)
		fa := action(t, name)
		fa.Spec.Priority = fleetv1.ActionPriorityHigh
		admitted(t, manager.Update(ctx, fa), "the controller's update of a child's priority")
		// The stop finalizer the controller holds on work a stopped robot records, and a label.
		fa = action(t, name)
		base := fa.DeepCopy()
		controllerutil.AddFinalizer(fa, estopscope.StopFinalizer)
		fa.Labels["swarmada.io/fleettask-member"] = "a"
		admitted(t, manager.Patch(ctx, fa, client.MergeFrom(base)), "the controller's stop finalizer and label on a child")
		fa = action(t, name)
		base = fa.DeepCopy()
		controllerutil.RemoveFinalizer(fa, estopscope.StopFinalizer)
		admitted(t, manager.Patch(ctx, fa, client.MergeFrom(base)), "the controller's release of the stop finalizer")
		fa = action(t, name)
		fa.Spec.Payload = &fleetv1.ActionPayload{Raw: []byte(`{"zone":"aisle-5"}`)}
		admitted(t, manager.Update(ctx, fa), "the controller's change of a child's payload")
		if got := wire(t, action(t, name)); got != `{"zone":"aisle-5"}` {
			t.Errorf("payload %s after the controller's change", got)
		}
	})

	t.Run("the-control-plane-fans-a-cancel-out-to-a-child", func(t *testing.T) {
		name := child(t, "cp-fan", payload)
		var task fleetv1.FleetTask
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "cp-fan"}, &task); err != nil {
			t.Fatal(err)
		}
		base := task.DeepCopy()
		task.Spec.DesiredState = fleetv1.DesiredStateCancelled
		if err := w.admin.Patch(ctx, &task, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
		reconcileTask("cp-fan")
		fa := action(t, name)
		if fa.Spec.DesiredState != fleetv1.DesiredStateCancelled {
			t.Errorf("the task's cancel did not reach its child: desiredState %q", fa.Spec.DesiredState)
		}
		if got := wire(t, fa); got != payload {
			t.Errorf("the fan-out moved the payload: %s, want %s", got, payload)
		}
	})

	// A person's apply that changes nothing, in every spelling that decodes to the stored value.
	// The API server stores the decoded value and encodes old and new alike, so none of these
	// spellings reaches the guard. The second apply writes nothing at all.
	for _, c := range []struct{ name, stored, applied string }{
		{"plain", `{"zone":"aisle-4"}`, `{"zone":"aisle-4"}`},
		{"keys-in-another-order", `{"b":1,"a":{"d":[1,2],"c":"x"}}`, `{ "a": { "c": "x", "d": [1, 2] }, "b": 1 }`},
		{"an-integer-spelled-as-a-float", `{"n":2}`, `{"n":2.0}`},
		{"an-integer-spelled-with-an-exponent", `{"n":1000}`, `{"n":1e3}`},
		{"floats", `{"f":0.1,"g":1.5e-7,"h":1e21}`, `{"f":0.10,"g":0.00000015,"h":1e+21}`},
		{"integers-past-float-precision", `{"big":9007199254740993}`, `{"big":9007199254740993}`},
		{"an-integer-past-int64", `{"huge":18446744073709551616}`, `{"huge":18446744073709551616}`},
		{"escapes", `{"s":"<a&b> é /"}`, `{"s":"\u003ca\u0026b\u003e \u00e9 /"}`},
		{"the-raw-form", `{"raw":"eyJ4IjoxfQ=="}`, `{"raw":"eyJ4IjoxfQ=="}`},
		{"a-raw-key-beside-another", `{"raw":"eyJ4IjoxfQ==","y":2}`, `{"y":2,"raw":"eyJ4IjoxfQ=="}`},
		{"an-empty-payload", `{}`, `{}`},
		{"a-null-value", `{"a":null,"b":1}`, `{"b":1,"a":null}`},
		{"negative-zero", `{"z":-0.0}`, `{"z":-0.0}`},
		{"negative-zero-over-zero", `{"z":0}`, `{"z":-0.0}`},
	} {
		t.Run("a-persons-no-op-apply/"+c.name, func(t *testing.T) {
			name := child(t, "noop-"+c.name, c.stored)
			was := action(t, name)
			admitted(t, apply(operator, name, c.applied), "the no-op apply")
			fa := action(t, name)
			if got, want := wire(t, fa), wire(t, was); got != want {
				t.Errorf("the no-op apply moved the payload: %s, want %s", got, want)
			}
			admitted(t, apply(operator, name, c.applied), "the second no-op apply")
			if again := action(t, name); again.ResourceVersion != fa.ResourceVersion {
				t.Errorf("the second no-op apply wrote: resourceVersion %s, was %s", again.ResourceVersion, fa.ResourceVersion)
			}
		})
	}

	t.Run("a-persons-no-op-update", func(t *testing.T) {
		name := child(t, "noop-update", payload)
		admitted(t, operator.Update(ctx, action(t, name)), "the read-and-write-back update")
	})

	t.Run("a-person-may-not-change-a-payload", func(t *testing.T) {
		name := child(t, "change", payload)
		refusedAsASpecChange(t, patch(operator, name, `{"spec":{"payload":{"zone":"aisle-5"}}}`), "the patch of a payload")
		refusedAsASpecChange(t, apply(operator, name, `{"targetPose":{"x":4,"y":2},"zone":"aisle-5"}`, forced), "the apply of another payload")
		refusedAsASpecChange(t, patch(operator, name, `{"spec":{"payload":{"targetPose":{"x":4.5}}}}`), "the patch of a nested value")
		fa := action(t, name)
		fa.Spec.Payload = &fleetv1.ActionPayload{Raw: []byte(`{"zone":"aisle-5"}`)}
		refusedAsASpecChange(t, operator.Update(ctx, fa), "the update of a payload")
		if got := wire(t, action(t, name)); got != payload {
			t.Errorf("a refused write moved the payload: %s, want %s", got, payload)
		}
	})

	t.Run("a-person-may-not-add-a-payload", func(t *testing.T) {
		name := child(t, "add", "")
		refusedAsASpecChange(t, patch(operator, name, `{"spec":{"payload":{"zone":"aisle-5"}}}`), "the addition of a payload")
		refusedAsASpecChange(t, patch(operator, name, `{"spec":{"payload":{}}}`), "the addition of an empty payload")
		refusedAsASpecChange(t, apply(operator, name, `{"zone":"aisle-5"}`, forced), "the apply of a new payload")
		if got := wire(t, action(t, name)); got != "" {
			t.Errorf("a refused write added a payload: %s", got)
		}
	})

	t.Run("a-person-may-not-remove-a-payload", func(t *testing.T) {
		name := child(t, "remove", payload)
		refusedAsASpecChange(t, patch(operator, name, `{"spec":{"payload":null}}`), "the removal of a payload")
		fa := action(t, name)
		fa.Spec.Payload = nil
		refusedAsASpecChange(t, operator.Update(ctx, fa), "the update that drops the payload")
		empty := child(t, "remove-empty", `{}`)
		refusedAsASpecChange(t, patch(operator, empty, `{"spec":{"payload":null}}`), "the removal of an empty payload")
		if got := wire(t, action(t, name)); got != payload {
			t.Errorf("a refused write moved the payload: %s, want %s", got, payload)
		}
	})

	// The same bytes in the other form are another spec: the stored spelling is what every later
	// read returns, and the child's spec is the control plane's to write.
	t.Run("a-person-may-not-respell-a-payload", func(t *testing.T) {
		rawForm := child(t, "respell-raw", `{"raw":"eyJ4IjoxfQ=="}`)
		refusedAsASpecChange(t, apply(operator, rawForm, `{"x":1}`, forced), "the apply that adds a key beside the raw form")
		fa := action(t, rawForm)
		var plain fleetv1.ActionPayload
		if err := json.Unmarshal([]byte(`{"x":1}`), &plain); err != nil {
			t.Fatal(err)
		}
		fa.Spec.Payload = &plain
		refusedAsASpecChange(t, operator.Update(ctx, fa), "the update that unwraps the raw form")
		plainForm := child(t, "respell-plain", `{"x":1}`)
		fa = action(t, plainForm)
		var wrapped fleetv1.ActionPayload
		if err := json.Unmarshal([]byte(`{"raw":"eyJ4IjoxfQ=="}`), &wrapped); err != nil {
			t.Fatal(err)
		}
		fa.Spec.Payload = &wrapped
		refusedAsASpecChange(t, operator.Update(ctx, fa), "the update that wraps a payload in the raw form")
		if got := wire(t, action(t, rawForm)); got != `{"raw":"eyJ4IjoxfQ=="}` {
			t.Errorf("a refused write moved the payload: %s", got)
		}
	})

	// The FleetTask's append-only rule compares its members with reflect.DeepEqual, which does
	// not panic on the payload's unexported field. A no-op apply of a task with a payload stays open.
	t.Run("a-persons-no-op-apply-of-the-task", func(t *testing.T) {
		child(t, "task-noop", payload)
		body := fmt.Sprintf(`{"apiVersion":%q,"kind":"FleetTask","metadata":{"name":"task-noop","namespace":%q},`+
			`"spec":{"actions":[{"name":"a","action":{"type":%q,"onFailure":%q,"priority":%q,"payload":{ "zone":"aisle-4", "targetPose":{"y":2,"x":4.0} }}}]}}`,
			fleetv1.GroupVersion.String(), w.ns, fleetv1.ActionTypeNavigate, fleetv1.ActionFailureAlert, fleetv1.ActionPriorityNormal)
		err := operator.Patch(ctx, &fleetv1.FleetTask{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "task-noop"}},
			client.RawPatch(types.ApplyPatchType, []byte(body)), client.FieldOwner("a-person"))
		admitted(t, err, "the no-op apply of the task")
	})
}
