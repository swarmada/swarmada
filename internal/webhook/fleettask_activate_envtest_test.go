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

// A drafted FleetTask is held until a person clears spec.draft. Clearing it is the
// whole human checkpoint between an observation and a robot moving, so it needs the
// `activate` verb on fleettasks, and a ServiceAccount is refused whatever its roles
// hold (RFC-0001 #security-permission-matrix). Before this, update or patch on
// fleettasks was enough, and any automation identity holding it could activate.
//
// One API server for every case below: the webhook package runs close to the race
// timeout, and each startSafetyWire is a fresh one.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// activateVerbLiteral is spelled out rather than taken from the validator, so a
// rename there cannot quietly move what this test asks the authorizer about.
const activateVerbLiteral = "activate"

// wantActivate is the permission-matrix row for `activate` on fleettasks: the roles
// that hold `cancel` on it.
func wantActivate(role string) bool {
	return role == "operator" || role == "fleet-manager" || role == "admin"
}

func TestEnvtest_DraftActivationNeedsActivateAndAPerson(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{rbacv1.AddToScheme, authorizationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	rc, err := client.New(w.cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.admin.Create(ctx, wireAppEndpoint(w.ns)); err != nil {
		t.Fatalf("creating the endpoint: %v", err)
	}

	// ── the matrix: activate is held by exactly the roles that hold cancel ────────
	authz := &SARAuthorizer{Client: rc}
	sources := []struct{ name, path string }{
		{"config/rbac", ""},
		{"helm", filepath.Join("..", "..", "deploy", "swarmada", "templates", "rbac-builtin-clusterroles.yaml")},
	}
	installed := map[string]string{} // config/rbac role -> installed ClusterRole name
	var mismatches []string
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
		for _, role := range builtinRoles {
			cr, ok := roles[role]
			if !ok {
				continue // the Helm chart mirrors five roles; edge is config/rbac only
			}
			tag := strings.ReplaceAll(src.name, "/", "-") + "-" + role
			inst := cr.DeepCopy()
			inst.ObjectMeta = metav1.ObjectMeta{Name: "act-" + tag}
			if err := rc.Create(ctx, inst); err != nil {
				t.Fatalf("installing %s role %s: %v", src.name, role, err)
			}
			user := "act-user-" + tag
			bindClusterRoleIn(t, rc, w.ns, inst.Name, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user})
			got, err := authz.Authorize(ctx, authenticationv1.UserInfo{Username: user},
				activateVerbLiteral, fleetTaskCompositeGR, w.ns, "any")
			if err != nil {
				t.Fatalf("SAR %s activate fleettasks: %v", role, err)
			}
			if got != wantActivate(role) {
				mismatches = append(mismatches, fmt.Sprintf("%s: %s activate on fleettasks = %v, the matrix says %v",
					src.name, role, got, wantActivate(role)))
			}
			if src.path == "" {
				installed[role] = inst.Name
			}
		}
	}
	sort.Strings(mismatches)
	for _, m := range mismatches {
		t.Error(m)
	}

	// ── the principals ────────────────────────────────────────────────────────────
	// A ServiceAccount bound to swarmada:admin holds update, patch and activate.
	saName := "drafter-bot"
	bindClusterRoleIn(t, rc, w.ns, installed["admin"], rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: w.ns})
	sa := asIdentity(t, w, "system:serviceaccount:"+w.ns+":"+saName,
		"system:serviceaccounts", "system:serviceaccounts:"+w.ns)

	// A person holding update and patch on fleettasks, and not activate.
	updOnly := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "act-update-only"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{fleetv1.GroupVersion.Group}, Resources: []string{"fleettasks"},
			Verbs: []string{"get", "list", "watch", "update", "patch"}}},
	}
	if err := rc.Create(ctx, updOnly); err != nil {
		t.Fatal(err)
	}
	bindClusterRoleIn(t, rc, w.ns, updOnly.Name, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "act-update-only"})
	noVerb := asIdentity(t, w, "act-update-only")

	newDraft := func(name string) client.ObjectKey {
		t.Helper()
		task := wireTask(w.ns, name)
		task.Spec.Draft = true
		if err := w.admin.Create(ctx, task); err != nil {
			t.Fatalf("creating draft %s: %v", name, err)
		}
		return client.ObjectKeyFromObject(task)
	}
	clearDraft := func(c client.Client, key client.ObjectKey) error {
		return updateTask(ctx, c, key, func(ft *fleetv1.FleetTask) { ft.Spec.Draft = false })
	}
	refused := func(what, want string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s was admitted: the draft gate is then not a human checkpoint", what)
			return
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s was refused, but not by the activation check (want %q): %v", what, want, err)
		}
	}

	// ── a ServiceAccount never activates, whatever it holds ─────────────────────
	k := newDraft("sa-activates")
	refused("a ServiceAccount bound to swarmada:admin clearing spec.draft", "ServiceAccount", clearDraft(sa, k))
	if err := updateTask(ctx, sa, k, func(ft *fleetv1.FleetTask) { ft.Labels = map[string]string{"seen": "yes"} }); err != nil {
		t.Errorf("a label update on a draft by a ServiceAccount was refused; only the activation is gated: %v", err)
	}

	// ── a person without the verb cannot, by merge patch or by JSON-patch remove ──
	k = newDraft("no-verb-activates")
	refused("a user holding update and patch but not activate clearing spec.draft", activateVerbLiteral, clearDraft(noVerb, k))
	removeDraft := client.RawPatch(types.JSONPatchType, []byte(`[{"op":"remove","path":"/spec/draft"}]`))
	cur := &fleetv1.FleetTask{}
	if err := w.admin.Get(ctx, k, cur); err != nil {
		t.Fatal(err)
	}
	refused("the same user removing spec.draft (it defaults to false)", activateVerbLiteral, noVerb.Patch(ctx, cur, removeDraft))
	if err := updateTask(ctx, noVerb, k, func(ft *fleetv1.FleetTask) { ft.Labels = map[string]string{"seen": "yes"} }); err != nil {
		t.Errorf("a label update on a draft by a user without activate was refused: %v", err)
	}

	// ── a person holding a built-in role that carries activate can ──────────────
	for _, role := range []string{"operator", "fleet-manager", "admin"} {
		k := newDraft("by-" + role)
		user := "act-user-config-rbac-" + role
		if err := clearDraft(asIdentity(t, w, user), k); err != nil {
			t.Errorf("a person bound to swarmada:%s could not activate a draft: %v", role, err)
		}
	}
	// ── a ServiceAccount cannot grow a draft; everything else it writes is admitted ──
	grow := func(member string) func(*fleetv1.FleetTask) {
		return func(ft *fleetv1.FleetTask) {
			ft.Spec.Actions = append(ft.Spec.Actions, fleetv1.FleetTaskAction{Name: member,
				Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}})
		}
	}
	k = newDraft("sa-grows")
	refused("a ServiceAccount bound to swarmada:admin appending a member to a draft", "ServiceAccount", updateTask(ctx, sa, k, grow("by-automation")))
	if err := updateTask(ctx, asIdentity(t, w, "act-user-config-rbac-operator"), k, grow("by-a-person")); err != nil {
		t.Errorf("a person bound to swarmada:operator could not append to a draft; people keep drafting: %v", err)
	}
	// The manager writes status and finalizers as a ServiceAccount: system:masters here keeps
	// RBAC out of the way, so only admission can refuse.
	ctrlSA := asIdentity(t, w, "system:serviceaccount:"+w.ns+":manager", "system:serviceaccounts", "system:masters")
	cur = &fleetv1.FleetTask{}
	if err := w.admin.Get(ctx, k, cur); err != nil {
		t.Fatal(err)
	}
	base := cur.DeepCopy()
	cur.Status.Phase = fleetv1.FleetTaskPhaseDraft
	if err := ctrlSA.Status().Patch(ctx, cur, client.MergeFrom(base)); err != nil {
		t.Errorf("a status write to a draft by a ServiceAccount was refused: %v", err)
	}
	if err := updateTask(ctx, ctrlSA, k, func(ft *fleetv1.FleetTask) { ft.Finalizers = append(ft.Finalizers, "example.com/hold") }); err != nil {
		t.Errorf("a finalizer added to a draft by a ServiceAccount was refused: %v", err)
	}
	if err := updateTask(ctx, ctrlSA, k, func(ft *fleetv1.FleetTask) { ft.Finalizers = nil }); err != nil {
		t.Errorf("a finalizer removed from a draft by a ServiceAccount was refused: %v", err)
	}
	if err := updateTask(ctx, sa, k, func(ft *fleetv1.FleetTask) { ft.Spec.DesiredState = fleetv1.DesiredStateCancelled }); err != nil {
		t.Errorf("a ServiceAccount holding cancel could not cancel a draft: %v", err)
	}

	// viewer holds no write at all: RBAC refuses before admission.
	k = newDraft("by-viewer")
	if err := clearDraft(asIdentity(t, w, "act-user-config-rbac-viewer"), k); err == nil {
		t.Error("a viewer activated a draft")
	}
}

func bindClusterRoleIn(t *testing.T, c client.Client, ns, clusterRole string, subject rbacv1.Subject) {
	t.Helper()
	name := clusterRole + "-" + strings.ToLower(subject.Kind) + "-" + strings.ReplaceAll(subject.Name, ":", "-")
	if err := c.Create(context.Background(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
		Subjects:   []rbacv1.Subject{subject},
	}); err != nil {
		t.Fatalf("binding %s to %s: %v", subject.Name, clusterRole, err)
	}
}
