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
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
)

// A FleetTask's child actions are the control plane's to write (RFC-0001 #fleettask: the
// controller is the only writer of the actions it owns). A task source holds `patch` on
// fleetactions for its server-side apply upsert, and RBAC cannot narrow that grant to the
// source's own objects (RFC-0004, Identity and Authorization), so the FleetAction webhook
// holds the line on children: a change to a child's spec, or to its swarmada.io/ labels and
// annotations, is refused from any identity but the control plane, except a desiredState
// change, a cancel request or a requeue request by a holder of `cancel`. Only the control
// plane, or the garbage collector, removes the FleetTask's controller reference, so a child
// cannot be made an ordinary action first and edited after. The garbage collector, under
// either username it runs as, also deletes a deleted task's members.
//
// Each row runs against the real API server with the webhooks served. The task source is a
// ServiceAccount bound to the task-source Role exactly as RFC-0004 prints it (a pinned copy,
// held to the assembled specification below), and reaches the API server under RBAC alone.
// The operator is a user bound to the shipped swarmada:operator ClusterRole. Neither is in
// system:masters.

const (
	taskSourceRolePinned = "testdata/task-source-role.md"
	taskSourceSpec       = "../../rfcs/dist/RFC-0004-task-source-surface.md"
	taskSourceRoleName   = "swarmada:task-source"
	taskSourceBinding    = "wms-east-to-task-source"
	// childTaskLabel is the label the controller puts on a child, naming its task.
	childTaskLabel = "swarmada.io/fleettask"
)

var specYAMLFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// printedObjects decodes every object in the YAML blocks of a specification file. A block
// that does not decode is an illustration, not an object; the lookup fails if the object a
// test needs is not among the ones that do.
func printedObjects(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var objs []*unstructured.Unstructured
	for _, m := range specYAMLFence.FindAllSubmatch(raw, -1) {
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewBuffer(m[1]), 4096)
		for {
			var obj map[string]interface{}
			if err := dec.Decode(&obj); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Logf("%s: skipping a YAML block that does not decode: %v", path, err)
				}
				break
			}
			if obj != nil {
				objs = append(objs, &unstructured.Unstructured{Object: obj})
			}
		}
	}
	return objs
}

func printedObject(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no printed %s named %q", kind, name)
	return nil
}

// The pinned copy the envtest binds is the Role and RoleBinding the specification prints.
func TestTaskSourceRole_PinnedCopyIsTheRoleAsPrinted(t *testing.T) {
	pinned := printedObjects(t, taskSourceRolePinned)
	printed := printedObjects(t, taskSourceSpec)
	for _, want := range []struct{ kind, name string }{
		{"Role", taskSourceRoleName},
		{"RoleBinding", taskSourceBinding},
	} {
		if !equality.Semantic.DeepEqual(printedObject(t, pinned, want.kind, want.name).Object,
			printedObject(t, printed, want.kind, want.name).Object) {
			t.Errorf("%s %s in %s differs from the one %s prints; copy the printed block into the pin",
				want.kind, want.name, taskSourceRolePinned, taskSourceSpec)
		}
	}
}

// The task-source Role grants what the profile names and nothing else, and is bound to a
// ServiceAccount: no subresource, no custom verb, no impersonation.
func TestTaskSourceRole_GrantsOnlyWhatTheProfileNames(t *testing.T) {
	objs := printedObjects(t, taskSourceRolePinned)
	var role rbacv1.Role
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(printedObject(t, objs, "Role", taskSourceRoleName).Object, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("the task-source Role has %d rules, want 1: %+v", len(role.Rules), role.Rules)
	}
	r := role.Rules[0]
	sorted := func(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }
	if got := sorted(r.APIGroups); !slices.Equal(got, []string{fleetv1.GroupVersion.Group}) {
		t.Errorf("apiGroups %v, want [%s]", got, fleetv1.GroupVersion.Group)
	}
	if got := sorted(r.Resources); !slices.Equal(got, []string{"fleetactions", "fleettasks"}) {
		t.Errorf("resources %v, want [fleetactions fleettasks]", got)
	}
	if got := sorted(r.Verbs); !slices.Equal(got, []string{"create", "get", "list", "patch", "watch"}) {
		t.Errorf("verbs %v, want [create get list patch watch]", got)
	}
	if len(r.ResourceNames) != 0 || len(r.NonResourceURLs) != 0 {
		t.Errorf("unexpected resourceNames or nonResourceURLs: %+v", r)
	}
	var binding rbacv1.RoleBinding
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(printedObject(t, objs, "RoleBinding", taskSourceBinding).Object, &binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != rbacv1.ServiceAccountKind {
		t.Errorf("the task-source Role is bound to %+v, want one ServiceAccount", binding.Subjects)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != taskSourceRoleName {
		t.Errorf("the binding refers to %+v, want Role %s", binding.RoleRef, taskSourceRoleName)
	}
}

func TestEnvtest_AChildActionIsWrittenOnlyByTheControlPlane(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()

	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(w.cfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}

	// The task source: a ServiceAccount bound to the Role as printed, in this namespace.
	const sourceSA = "wms-east"
	sourceUser := "system:serviceaccount:" + w.ns + ":" + sourceSA
	objs := printedObjects(t, taskSourceRolePinned)
	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(printedObject(t, objs, "Role", taskSourceRoleName).Object, &role); err != nil {
		t.Fatal(err)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(printedObject(t, objs, "RoleBinding", taskSourceBinding).Object, &binding); err != nil {
		t.Fatal(err)
	}
	role.Namespace, binding.Namespace = w.ns, w.ns
	for i := range binding.Subjects {
		binding.Subjects[i].Namespace = w.ns
	}
	if err := rc.Create(ctx, &role); err != nil {
		t.Fatalf("installing the task-source Role: %v", err)
	}
	if err := rc.Create(ctx, &binding); err != nil {
		t.Fatalf("binding the task-source Role: %v", err)
	}
	source := func() client.Client {
		cfg := rest.CopyConfig(w.cfg)
		cfg.Impersonate = rest.ImpersonationConfig{UserName: sourceUser,
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + w.ns, "system:authenticated"}}
		c, err := client.New(cfg, client.Options{Scheme: w.scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}()

	// The operator: a user bound to the shipped swarmada:operator ClusterRole.
	const operatorUser = "olivia"
	for _, cr := range loadClusterRoles(t, filepath.Join("..", "..", "config", "rbac", "swarmada_operator_role.yaml")) {
		if err := rc.Create(ctx, cr); err != nil {
			t.Fatalf("installing %s: %v", cr.Name, err)
		}
		if err := rc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "operator-olivia", Namespace: w.ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cr.Name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: operatorUser}},
		}); err != nil {
			t.Fatalf("binding %s: %v", cr.Name, err)
		}
	}
	operator := func() client.Client {
		cfg := rest.CopyConfig(w.cfg)
		cfg.Impersonate = rest.ImpersonationConfig{UserName: operatorUser, Groups: []string{"system:authenticated"}}
		c, err := client.New(cfg, client.Options{Scheme: w.scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}()

	manager := w.as(t, controllerUser)
	reconcileTask := func(name string) {
		t.Helper()
		tr := &controller.FleetTaskReconciler{Client: manager, Scheme: w.scheme, Recorder: record.NewFakeRecorder(64)}
		for i := 0; i < 2; i++ {
			if _, err := tr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: w.ns, Name: name}}); err != nil {
				t.Logf("task %s reconcile: %v", name, err)
			}
		}
	}
	action := func(t *testing.T, name string) *fleetv1.FleetAction {
		t.Helper()
		var fa fleetv1.FleetAction
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &fa); err != nil {
			t.Fatalf("reading action %s: %v", name, err)
		}
		return &fa
	}
	navigate := func() fleetv1.FleetActionSpec {
		return fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert,
			Priority: fleetv1.ActionPriorityNormal}
	}
	// child creates a FleetTask with one member as a person would, lets the controller fan
	// it out, and returns the child's name.
	child := func(t *testing.T, task string) string {
		t.Helper()
		if err := w.admin.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: task},
			Spec:       fleetv1.FleetTaskSpec{Actions: []fleetv1.FleetTaskAction{{Name: "a", Action: navigate()}}},
		}); err != nil {
			t.Fatalf("creating task %s: %v", task, err)
		}
		reconcileTask(task)
		name := task + "-a"
		if metav1.GetControllerOf(action(t, name)) == nil {
			t.Fatalf("precondition: %s is not controlled by its task", name)
		}
		return name
	}
	patch := func(c client.Client, name, body string) error {
		return c.Patch(ctx, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name}},
			client.RawPatch(types.MergePatchType, []byte(body)))
	}
	refused := func(t *testing.T, err error, what string) {
		t.Helper()
		switch {
		case err == nil:
			t.Errorf("%s was admitted", what)
		case !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "admission webhook"):
			t.Errorf("%s was refused, but not by admission: %v", what, err)
		}
	}
	admitted := func(t *testing.T, err error, what string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s was refused: %v", what, err)
		}
	}
	// specKept asserts the child's spec is still what the controller wrote.
	specKept := func(t *testing.T, name string, want fleetv1.FleetActionSpec) {
		t.Helper()
		got := action(t, name)
		if !equality.Semantic.DeepEqual(got.Spec, want) {
			t.Errorf("the spec of %s changed: %+v, want %+v", name, got.Spec, want)
		}
		if metav1.GetControllerOf(got) == nil {
			t.Errorf("%s lost its task's controller reference", name)
		}
	}
	sar := func(t *testing.T, ra authorizationv1.ResourceAttributes) bool {
		t.Helper()
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: sourceUser, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + w.ns, "system:authenticated"},
			ResourceAttributes: &ra,
		}}
		if err := rc.Create(ctx, review); err != nil {
			t.Fatalf("SubjectAccessReview: %v", err)
		}
		return review.Status.Allowed
	}

	// The Role as printed grants the source no cancel, no status write and no impersonation.
	t.Run("the-source-holds-only-what-the-role-grants", func(t *testing.T) {
		for _, ra := range []authorizationv1.ResourceAttributes{
			{Namespace: w.ns, Verb: "patch", Group: "swarmada.io", Resource: "fleetactions"},
			{Namespace: w.ns, Verb: "create", Group: "swarmada.io", Resource: "fleettasks"},
		} {
			if !sar(t, ra) {
				t.Errorf("precondition: the source must hold %s on %s", ra.Verb, ra.Resource)
			}
		}
		for _, ra := range []authorizationv1.ResourceAttributes{
			{Namespace: w.ns, Verb: "cancel", Group: "swarmada.io", Resource: "fleetactions"},
			{Namespace: w.ns, Verb: "cancel", Group: "swarmada.io", Resource: "fleettasks"},
			{Namespace: w.ns, Verb: "activate", Group: "swarmada.io", Resource: "fleettasks"},
			{Namespace: w.ns, Verb: "patch", Group: "swarmada.io", Resource: "fleetactions", Subresource: "status"},
			{Namespace: w.ns, Verb: "delete", Group: "swarmada.io", Resource: "fleetactions"},
			{Verb: "impersonate", Resource: "users"},
			{Verb: "impersonate", Resource: "groups"},
			{Namespace: w.ns, Verb: "impersonate", Resource: "serviceaccounts"},
		} {
			if sar(t, ra) {
				t.Errorf("the source holds %s on %s %s", ra.Verb, ra.Resource, ra.Subresource)
			}
		}
	})

	t.Run("a-source-may-not-change-a-childs-priority", func(t *testing.T) {
		name := child(t, "prio")
		want := action(t, name).Spec
		refused(t, patch(source, name, `{"spec":{"priority":"Critical"}}`), "the source's change of a child's priority")
		specKept(t, name, want)
	})

	t.Run("a-source-may-not-change-a-childs-payload-or-selector", func(t *testing.T) {
		name := child(t, "payload")
		want := action(t, name).Spec
		refused(t, patch(source, name, `{"spec":{"robotSelector":"amr-07"}}`), "the source's change of a child's robotSelector")
		refused(t, patch(source, name, `{"spec":{"payload":{"targetPose":{"x":4,"y":2}}}}`), "the source's change of a child's payload")
		specKept(t, name, want)
	})

	t.Run("a-source-may-not-pause-a-child", func(t *testing.T) {
		name := child(t, "pause")
		want := action(t, name).Spec
		refused(t, patch(source, name, `{"spec":{"desiredState":"Paused"}}`), "the source's pause of a child")
		specKept(t, name, want)
	})

	t.Run("a-source-may-not-requeue-a-child", func(t *testing.T) {
		name := child(t, "requeue")
		refused(t, patch(source, name, `{"metadata":{"annotations":{"swarmada.io/requeue-requested":"upstream"}}}`),
			"the source's requeue of a child")
		if _, ok := action(t, name).Annotations[requeueRequestedAnnotation]; ok {
			t.Errorf("the refused requeue landed")
		}
	})

	t.Run("a-child-cannot-be-detached-and-then-edited", func(t *testing.T) {
		name := child(t, "detach")
		want := action(t, name).Spec
		refused(t, patch(source, name, `{"metadata":{"ownerReferences":null}}`), "the source's removal of a child's controller reference")
		refused(t, patch(source, name, `{"spec":{"priority":"Critical"}}`), "the source's edit of the child after the removal")
		specKept(t, name, want)
	})

	t.Run("a-child-cannot-be-detached-and-edited-in-one-write", func(t *testing.T) {
		name := child(t, "detach-one")
		want := action(t, name).Spec
		refused(t, patch(source, name, `{"metadata":{"ownerReferences":null},"spec":{"priority":"Critical"}}`),
			"the source's removal of the controller reference together with an edit")
		specKept(t, name, want)
	})

	t.Run("a-source-applies-its-own-action-again", func(t *testing.T) {
		apply := func(priority fleetv1.ActionPriority) error {
			fa := &fleetv1.FleetAction{
				TypeMeta: metav1.TypeMeta{APIVersion: fleetv1.GroupVersion.String(), Kind: "FleetAction"},
				ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "fa-9f2a1c7b",
					Labels: map[string]string{"swarmada.io/source-id": sourceSA}},
				Spec: navigate(),
			}
			fa.Spec.Priority = priority
			return source.Patch(ctx, fa, client.Apply, client.FieldOwner(sourceSA), client.ForceOwnership)
		}
		admitted(t, apply(fleetv1.ActionPriorityNormal), "the source's server-side apply of its own action")
		admitted(t, apply(fleetv1.ActionPriorityHigh), "the source's re-apply of its own action with a new priority")
		if got := action(t, "fa-9f2a1c7b").Spec.Priority; got != fleetv1.ActionPriorityHigh {
			t.Errorf("priority %q after the re-apply, want High", got)
		}
		admitted(t, patch(source, "fa-9f2a1c7b", `{"metadata":{"labels":{"swarmada.io/source-id":"wms-east-2"}}}`),
			"the source's change of a swarmada.io/ label on its own action")
	})

	t.Run("a-source-may-not-relabel-a-child", func(t *testing.T) {
		name := child(t, "relabel")
		refused(t, patch(source, name, `{"metadata":{"labels":{"swarmada.io/fleettask":"pick-8821"}}}`),
			"the source's change of a child's task label")
		refused(t, patch(source, name, `{"metadata":{"labels":{"swarmada.io/fleettask":null}}}`),
			"the source's removal of a child's task label")
		refused(t, patch(source, name, `{"metadata":{"labels":{"swarmada.io/fleettask-member":"a"}}}`),
			"the source's new swarmada.io/ label on a child")
		got := action(t, name)
		if got.Labels[childTaskLabel] != "relabel" || len(got.Labels) != 1 {
			t.Errorf("the labels of %s changed: %v", name, got.Labels)
		}
	})

	t.Run("a-source-may-not-annotate-a-child-under-the-swarmada-prefix", func(t *testing.T) {
		name := child(t, "annotate")
		refused(t, patch(source, name, `{"metadata":{"annotations":{"swarmada.io/failure-alerted":"true"}}}`),
			"the source's new swarmada.io/ annotation on a child")
		if _, ok := action(t, name).Annotations["swarmada.io/failure-alerted"]; ok {
			t.Errorf("the refused annotation landed")
		}
	})

	t.Run("a-source-may-not-apply-its-own-labels-onto-a-child", func(t *testing.T) {
		name := child(t, "apply-onto")
		fa := &fleetv1.FleetAction{
			TypeMeta: metav1.TypeMeta{APIVersion: fleetv1.GroupVersion.String(), Kind: "FleetAction"},
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name,
				Labels: map[string]string{"swarmada.io/source-id": sourceSA}},
			Spec: action(t, name).Spec,
		}
		refused(t, source.Patch(ctx, fa, client.Apply, client.FieldOwner(sourceSA), client.ForceOwnership),
			"the source's server-side apply of its label onto a child")
		if _, ok := action(t, name).Labels["swarmada.io/source-id"]; ok {
			t.Errorf("the refused label landed")
		}
	})

	t.Run("a-source-may-not-change-or-withdraw-a-childs-requeue-request", func(t *testing.T) {
		name := child(t, "requeue-held")
		admitted(t, patch(manager, name, `{"metadata":{"annotations":{"swarmada.io/requeue-requested":"control plane"}}}`),
			"the control plane's requeue request on a child")
		refused(t, patch(source, name, `{"metadata":{"annotations":{"swarmada.io/requeue-requested":"upstream"}}}`),
			"the source's change of a child's requeue request")
		refused(t, patch(source, name, `{"metadata":{"annotations":{"swarmada.io/requeue-requested":null}}}`),
			"the source's withdrawal of a child's requeue request")
		if got := action(t, name).Annotations[requeueRequestedAnnotation]; got != "control plane" {
			t.Errorf("the requeue request on %s is %q, want the control plane's", name, got)
		}
	})

	t.Run("an-operator-pauses-and-resumes-a-child", func(t *testing.T) {
		name := child(t, "op-pause")
		admitted(t, patch(operator, name, `{"spec":{"desiredState":"Paused"}}`), "the operator's pause of a child")
		admitted(t, patch(operator, name, `{"spec":{"desiredState":"Running"}}`), "the operator's resume of a child")
	})

	t.Run("an-operator-may-not-relabel-a-child", func(t *testing.T) {
		name := child(t, "op-relabel")
		refused(t, patch(operator, name, `{"metadata":{"labels":{"swarmada.io/fleettask":"pick-8821"}}}`),
			"the operator's change of a child's task label")
		if got := action(t, name).Labels[childTaskLabel]; got != "op-relabel" {
			t.Errorf("the task label of %s is %q, want op-relabel", name, got)
		}
	})

	t.Run("an-operator-labels-a-child-outside-the-swarmada-prefix", func(t *testing.T) {
		name := child(t, "op-label")
		admitted(t, patch(operator, name, `{"metadata":{"labels":{"team":"east"},"annotations":{"example.com/note":"aisle 4"}}}`),
			"the operator's label and annotation outside the swarmada.io/ prefix")
	})

	t.Run("an-operator-requests-a-cancel-or-a-requeue-of-a-child", func(t *testing.T) {
		requeue := child(t, "op-requeue")
		admitted(t, patch(operator, requeue, `{"metadata":{"annotations":{"swarmada.io/requeue-requested":"aisle blocked"}}}`),
			"the operator's requeue request on a child")
		cancel := child(t, "op-cancel")
		admitted(t, patch(operator, cancel, `{"metadata":{"annotations":{"swarmada.io/cancel-requested":"order withdrawn"}}}`),
			"the operator's cancel request on a child")
	})

	t.Run("an-operator-may-not-change-a-childs-spec", func(t *testing.T) {
		name := child(t, "op-prio")
		want := action(t, name).Spec
		refused(t, patch(operator, name, `{"spec":{"priority":"Critical"}}`), "the operator's change of a child's priority")
		specKept(t, name, want)
	})

	t.Run("the-control-plane-fans-a-cancel-out-to-a-child", func(t *testing.T) {
		name := child(t, "fan")
		var task fleetv1.FleetTask
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "fan"}, &task); err != nil {
			t.Fatal(err)
		}
		base := task.DeepCopy()
		task.Spec.DesiredState = fleetv1.DesiredStateCancelled
		if err := w.admin.Patch(ctx, &task, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
		reconcileTask("fan")
		if got := action(t, name).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
			t.Errorf("the task's cancel did not reach its child: desiredState %q", got)
		}
	})

	t.Run("the-control-plane-relabels-a-child", func(t *testing.T) {
		name := child(t, "cp-relabel")
		admitted(t, patch(manager, name, `{"metadata":{"labels":{"swarmada.io/fleettask-member":"a"}}}`),
			"the control plane's new label on a child")
	})

	t.Run("the-garbage-collector-orphans-a-child", func(t *testing.T) {
		// The garbage collector runs under its own ServiceAccount, or as the controller
		// manager where the cluster does not give each controller its own credentials.
		for task, gc := range map[string]string{"orphan-sa": garbageCollectorUser, "orphan-kcm": kubeControllerManagerUser} {
			name := child(t, task)
			admitted(t, patch(w.as(t, gc), name, `{"metadata":{"ownerReferences":null}}`), gc+"'s removal of a child's owner reference")
			if refs := action(t, name).OwnerReferences; len(refs) != 0 {
				t.Errorf("%s still has owner references after the orphaning write: %+v", name, refs)
			}
		}
	})

	// The garbage collector deletes a deleted task's members. A robot under its own stop records
	// each child below, so the delete guard checks the delete rather than admitting it outright.
	// Each identity reaches the API server under RBAC alone, with delete on fleetactions and
	// no estop-clear, so only the guard's own rule can admit it.
	t.Run("the-garbage-collector-deletes-a-held-child", func(t *testing.T) {
		markerFixture(t, w, w.ns)
		deleter := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "fleetaction-deleter-" + w.ns},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{fleetv1.GroupVersion.Group}, Resources: []string{"fleetactions"},
				Verbs: []string{"get", "list", "watch", "delete"}}},
		}
		if err := rc.Create(ctx, deleter); err != nil {
			t.Fatalf("installing %s: %v", deleter.Name, err)
		}
		const lookAlike = "system:serviceaccount:other:generic-garbage-collector"
		if err := rc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "fleetaction-deleters", Namespace: w.ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: deleter.Name},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.ServiceAccountKind, Namespace: "kube-system", Name: "generic-garbage-collector"},
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: kubeControllerManagerUser},
				{Kind: rbacv1.ServiceAccountKind, Namespace: "other", Name: "generic-garbage-collector"},
			},
		}); err != nil {
			t.Fatalf("binding %s: %v", deleter.Name, err)
		}
		// held creates a child and a robot that records it, then stops the robot.
		held := func(t *testing.T, task string) string {
			t.Helper()
			name := child(t, task)
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
			robot.Annotations = map[string]string{estopTriggeredAnnotation: "gc-row"}
			if err := w.admin.Patch(ctx, robot, client.MergeFrom(base)); err != nil {
				t.Fatalf("stopping %s: %v", robot.Name, err)
			}
			return name
		}
		del := func(c client.Client, name string) error {
			return c.Delete(ctx, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name}})
		}
		for task, c := range map[string]client.Client{
			"gc-sa":  asIdentity(t, w, garbageCollectorUser, "system:serviceaccounts", "system:serviceaccounts:kube-system"),
			"gc-kcm": asIdentity(t, w, kubeControllerManagerUser),
		} {
			name := held(t, task)
			admitted(t, del(c, name), task+"'s delete of a held child")
		}
		name := held(t, "gc-look-alike")
		err := del(asIdentity(t, w, lookAlike, "system:serviceaccounts", "system:serviceaccounts:other"), name)
		refused(t, err, "a look-alike's delete of a held child")
		if err != nil && !strings.Contains(err.Error(), "estop-clear") {
			t.Errorf("the refusal does not name the estop-clear verb: %v", err)
		}
	})

	t.Run("an-action-no-task-controls-is-not-held-to-the-child-rule", func(t *testing.T) {
		fa := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "pick-8821"}, Spec: navigate()}
		if err := w.admin.Create(ctx, fa); err != nil {
			t.Fatal(err)
		}
		admitted(t, patch(source, "pick-8821", `{"spec":{"priority":"High"}}`), "an edit of an action no task controls")
		admitted(t, patch(source, "pick-8821", `{"metadata":{"labels":{"swarmada.io/source-id":"wms-east"}}}`),
			"a swarmada.io/ label on an action no task controls")
	})
}
