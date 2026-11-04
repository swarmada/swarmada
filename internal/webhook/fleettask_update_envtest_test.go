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
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A check that runs at create and not at update is a check an author can walk
// around: create the valid object, then update it into the one create refuses.
// These tests make each such update over the wire, against the real API server and
// the served webhook, and assert it is refused.

// wireTask is a FleetTask with one external member bound to an App endpoint.
func wireTask(ns, name string) *fleetv1.FleetTask {
	region := "region"
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: fleetv1.FleetTaskSpec{
			Inputs: []fleetv1.TaskInput{{Name: "region", Kind: fleetv1.ValueKindString}},
			Actions: []fleetv1.FleetTaskAction{{Name: "read", Action: fleetv1.FleetActionSpec{
				Type: fleetv1.ActionTypeExternal,
				Executor: &fleetv1.ActionExecutor{
					EndpointRef: "plate-reader",
					Parameters:  map[string]fleetv1.ParameterBinding{"region": {FromTaskInput: &region}},
				},
			}}},
		},
	}
}

func wireAppEndpoint(ns string) *fleetv1.ComputeEndpoint {
	return &fleetv1.ComputeEndpoint{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "plate-reader"},
		Spec: fleetv1.ComputeEndpointSpec{
			Binding:  fleetv1.ExecutorBindingHttp,
			Contract: fleetv1.ExecutorContractApp,
			Provision: fleetv1.ComputeEndpointProvision{
				Existing: &fleetv1.ExistingServiceProvision{Service: "plate-reader", Port: "8080"},
			},
		},
	}
}

func updateTask(ctx context.Context, c client.Client, key client.ObjectKey, mutate func(*fleetv1.FleetTask)) error {
	cur := &fleetv1.FleetTask{}
	if err := c.Get(ctx, key, cur); err != nil {
		return err
	}
	base := cur.DeepCopy()
	mutate(cur)
	return c.Patch(ctx, cur, client.MergeFrom(base))
}

func TestEnvtest_FleetTaskUpdateRunsTheCreateChecks(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	if err := w.admin.Create(ctx, wireAppEndpoint(w.ns)); err != nil {
		t.Fatalf("creating the endpoint: %v", err)
	}
	task := wireTask(w.ns, "plates")
	task.Spec.Flow = selectFlow(resultSubject("read", "plate"))
	if err := w.admin.Create(ctx, task); err != nil {
		t.Fatalf("a valid task must be admitted: %v", err)
	}
	key := client.ObjectKeyFromObject(task)

	// A no-op write and an unrelated one still pass: the checks gate what they read.
	if err := updateTask(ctx, w.admin, key, func(*fleetv1.FleetTask) {}); err != nil {
		t.Fatalf("a no-op update of a valid task was refused: %v", err)
	}
	if err := updateTask(ctx, w.admin, key, func(t *fleetv1.FleetTask) {
		t.Labels = map[string]string{"shift": "night"}
	}); err != nil {
		t.Fatalf("a label update of a valid task was refused: %v", err)
	}

	// spec.inputs and spec.flow cannot change at all after create: the API server
	// refuses the write before the webhook sees it, so neither can carry a binding
	// create would refuse.
	err := updateTask(ctx, w.admin, key, func(t *fleetv1.FleetTask) {
		t.Spec.Inputs = []fleetv1.TaskInput{{Name: "zone", Kind: fleetv1.ValueKindString}}
	})
	if err == nil || !strings.Contains(err.Error(), "spec.inputs is immutable after creation") {
		t.Errorf("an update removing an input a binding reads was not refused as an inputs change: %v", err)
	}
	err = updateTask(ctx, w.admin, key, func(t *fleetv1.FleetTask) {
		t.Spec.Flow = selectFlow(eventSubject("PlateRead", "read", "plate"))
	})
	if err == nil || !strings.Contains(err.Error(), "spec.flow is immutable after creation") {
		t.Errorf("an update rebinding the flow was not refused as a flow change: %v", err)
	}

	// spec.actions still grows, so the webhook still re-runs the binding check: an
	// appended member whose binding names an input the task does not declare.
	undeclared := "speed"
	err = updateTask(ctx, w.admin, key, func(t *fleetv1.FleetTask) {
		t.Spec.Actions = append(t.Spec.Actions, fleetv1.FleetTaskAction{Name: "read-again", Action: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeExternal,
			Executor: &fleetv1.ActionExecutor{
				EndpointRef: "plate-reader",
				Parameters:  map[string]fleetv1.ParameterBinding{"speed": {FromTaskInput: &undeclared}},
			},
		}})
	})
	if err == nil {
		t.Error("an appended member naming an undeclared input was admitted; create refuses the same task")
	} else if !strings.Contains(err.Error(), reasonUndeclaredTaskInput) {
		t.Errorf("refused for the wrong reason (want %s): %v", reasonUndeclaredTaskInput, err)
	}
}

// A cancel over the wire, per built-in role (config/rbac copies, no system:masters),
// the write `swarmctl cancel task` makes: a merge patch of spec.desiredState.
const (
	taskCancelAdmitted      = "admitted"
	taskCancelRefusedByHook = "refused by the FleetTask webhook (SAR on cancel)"
	taskCancelRefusedByRBAC = "refused by RBAC before admission (no patch on fleettasks)"
)

// wantTaskCancel is RFC-0001 #security-permission-matrix for cancelling a FleetTask:
// the `cancel` verb on fleettasks is held by exactly the roles that hold `update` on it.
func wantTaskCancel(role string) bool {
	return role == "operator" || role == "fleet-manager" || role == "admin"
}

func TestEnvtest_FleetTaskCancelOverTheWire(t *testing.T) {
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
	impersonate := func(user string) client.Client {
		ucfg := rest.CopyConfig(w.cfg)
		ucfg.Impersonate = rest.ImpersonationConfig{UserName: user}
		uc, err := client.New(ucfg, client.Options{Scheme: w.scheme})
		if err != nil {
			t.Fatal(err)
		}
		return uc
	}
	bind := func(cr *rbacv1.ClusterRole, user string) {
		t.Helper()
		if err := rc.Create(ctx, cr); err != nil {
			t.Fatalf("installing %s: %v", cr.Name, err)
		}
		if err := rc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-" + strings.ReplaceAll(cr.Name, ":", "-"), Namespace: w.ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cr.Name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
		}); err != nil {
			t.Fatalf("binding %s: %v", cr.Name, err)
		}
	}
	cancel := func(c client.Client, name string) error {
		return updateTask(ctx, c, client.ObjectKey{Namespace: w.ns, Name: name}, func(t *fleetv1.FleetTask) {
			t.Spec.DesiredState = fleetv1.DesiredStateCancelled
		})
	}

	// A principal with update and patch on fleettasks but not cancel is refused by the
	// webhook, not by RBAC: this is the route that needed only update before.
	updater := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "task-updater-no-cancel"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{fleetv1.GroupVersion.Group},
			Resources: []string{"fleettasks"}, Verbs: []string{"get", "list", "watch", "update", "patch"}}},
	}
	bind(updater, "wire-task-updater")
	if err := w.admin.Create(ctx, wireTask(w.ns, "updater-target")); err != nil {
		t.Fatal(err)
	}
	err = cancel(impersonate("wire-task-updater"), "updater-target")
	switch {
	case err == nil:
		t.Error("a principal holding update but not cancel on fleettasks cancelled a FleetTask")
	case !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "admission webhook"):
		t.Errorf("refused, but not by the webhook's cancel check: %v", err)
	}
	// ...and may still make every other update: only the transition is gated.
	if err := updateTask(ctx, impersonate("wire-task-updater"), client.ObjectKey{Namespace: w.ns, Name: "updater-target"},
		func(t *fleetv1.FleetTask) { t.Spec.DesiredState = fleetv1.DesiredStatePaused }); err != nil {
		t.Errorf("a principal holding update was refused a Paused write: %v", err)
	}

	outcome := map[string]string{}
	for _, role := range builtinRoles {
		file := "swarmada_" + strings.ReplaceAll(role, "-", "_") + "_role.yaml"
		for _, cr := range loadClusterRoles(t, filepath.Join("..", "..", "config", "rbac", file)) {
			installed := cr.DeepCopy()
			installed.ResourceVersion = ""
			bind(installed, "wire-"+role)
		}
		name := "cancel-by-" + role
		if err := w.admin.Create(ctx, wireTask(w.ns, name)); err != nil {
			t.Fatal(err)
		}
		switch perr := cancel(impersonate("wire-"+role), name); {
		case perr == nil:
			outcome[role] = taskCancelAdmitted
		case apierrors.IsForbidden(perr) && strings.Contains(perr.Error(), "admission webhook"):
			outcome[role] = taskCancelRefusedByHook
		case apierrors.IsForbidden(perr):
			outcome[role] = taskCancelRefusedByRBAC
		default:
			t.Fatalf("cancel as %s failed for a reason that is neither RBAC nor the webhook: %v", role, perr)
		}
	}

	var table strings.Builder
	table.WriteString("\nFleetTask cancel (desiredState: Cancelled, merge patch) over the wire, config/rbac roles:\n")
	for _, role := range builtinRoles {
		fmt.Fprintf(&table, "  %-14s %s\n", role, outcome[role])
	}
	t.Log(table.String())
	for _, role := range builtinRoles {
		if want, got := wantTaskCancel(role), outcome[role]; want != (got == taskCancelAdmitted) {
			t.Errorf("swarmada:%s cancelling a FleetTask: %s; RFC-0001 #security-permission-matrix says %s",
				role, got, yn(want))
		}
	}
}

// The SAR matrix for `cancel` on fleettasks, over both copies of the built-in roles,
// in the shape of TestEnvtest_EstopVerbSubjectAccessReviewMatrix.
func TestEnvtest_FleetTaskCancelSubjectAccessReviewMatrix(t *testing.T) {
	requireWebhookEnvtest(t)
	ctx := context.Background()

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting the API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, rbacv1.AddToScheme, authorizationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "sar-task-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	authz := &SARAuthorizer{Client: c}

	sources := []struct{ name, path string }{
		{"config/rbac", ""},
		{"helm", filepath.Join("..", "..", "deploy", "swarmada", "templates", "rbac-builtin-clusterroles.yaml")},
	}
	verbs := []string{cancelVerb, "update", "patch"}
	for _, src := range sources {
		roles := map[string]*rbacv1.ClusterRole{}
		if src.path == "" {
			for _, role := range builtinRoles {
				file := "swarmada_" + strings.ReplaceAll(role, "-", "_") + "_role.yaml"
				for k, v := range loadClusterRoles(t, filepath.Join("..", "..", "config", "rbac", file)) {
					roles[k] = v
				}
			}
		} else {
			roles = loadClusterRoles(t, src.path)
		}

		var table strings.Builder
		fmt.Fprintf(&table, "\n%s — cancel a FleetTask (got / RFC-0001 #security-permission-matrix):\n", src.name)
		fmt.Fprintf(&table, "  %-14s %s\n", "role", "fleettasks cancel|update|patch")
		var mismatches []string
		for _, role := range builtinRoles {
			cr, ok := roles[role]
			if !ok {
				continue // the Helm chart mirrors five roles; edge is config/rbac only
			}
			installed := cr.DeepCopy()
			installed.ObjectMeta = metav1.ObjectMeta{Name: "sar-task-" + strings.ReplaceAll(src.name, "/", "-") + "-" + role}
			if err := c.Create(ctx, installed); err != nil {
				t.Fatalf("installing %s role %s: %v", src.name, role, err)
			}
			user := "sar-task-user-" + strings.ReplaceAll(src.name, "/", "-") + "-" + role
			if err := c.Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: installed.Name, Namespace: ns.Name},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: installed.Name},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
			}); err != nil {
				t.Fatalf("binding %s: %v", installed.Name, err)
			}
			var cell []string
			for _, verb := range verbs {
				ok, err := authz.Authorize(ctx, authenticationv1.UserInfo{Username: user},
					verb, fleetTaskCompositeGR, ns.Name, "t-1")
				if err != nil {
					t.Fatalf("SAR %s %s fleettasks: %v", role, verb, err)
				}
				want := wantTaskCancel(role)
				mark := yn(ok) + "/" + yn(want)
				if ok != want {
					mark += "!"
					mismatches = append(mismatches, fmt.Sprintf("%s: %s %s on fleettasks = %v, RFC says %v",
						src.name, role, verb, ok, want))
				}
				cell = append(cell, mark)
			}
			fmt.Fprintf(&table, "  %-14s %s\n", role, strings.Join(cell, " | "))
		}
		t.Log(table.String())
		sort.Strings(mismatches)
		for _, m := range mismatches {
			t.Error(m)
		}
	}
}
