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
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Cancelling a FleetAction by writing spec.desiredState: Cancelled needs the `cancel`
// verb, as the cancel-requested annotation does (RFC-0001 #security-permission-matrix).
// The control plane's own fan-out of a FleetTask cancel to its members writes the same
// field as the manager's ServiceAccount, which holds no `cancel`, and stays admitted.

const (
	actionCancelAdmitted      = "admitted"
	actionCancelRefusedByHook = "refused by the FleetAction webhook (SAR on cancel)"
	actionCancelRefusedByRBAC = "refused by RBAC before admission (no patch on fleetactions)"
)

func desiredStateAction(name string, ds fleetv1.DesiredState) *fleetv1.FleetAction {
	a := cancelAction(name, "")
	a.Spec.DesiredState = ds
	return a
}

func TestFleetActionDesiredStateCancel_NeedsCancelVerb(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false}
	v := newActionValidatorWithAuthz(authz)
	_, err := v.ValidateUpdate(ctxAsUser("updater"),
		desiredStateAction("a1", fleetv1.DesiredStateRunning), desiredStateAction("a1", fleetv1.DesiredStateCancelled))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("desiredState -> Cancelled without the cancel verb must be Forbidden, got: %v", err)
	}
	if authz.gotVerb != cancelVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, cancelVerb)
	}
}

func TestFleetActionDesiredStateCancel_ManagerIsExempt(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false}
	v := newActionValidatorWithAuthz(authz)
	if _, err := v.ValidateUpdate(ctxAsUser(controllerUser),
		desiredStateAction("a1", fleetv1.DesiredStateRunning), desiredStateAction("a1", fleetv1.DesiredStateCancelled)); err != nil {
		t.Fatalf("the manager's own cancel fan-out was refused: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authz consulted %d times for the manager, want 0", authz.calls)
	}
	// The exemption is the exact principal, not any ServiceAccount of that name.
	other := strings.Replace(controllerUser, "swarmada-system", "elsewhere", 1)
	if _, err := v.ValidateUpdate(ctxAsUser(other),
		desiredStateAction("a1", fleetv1.DesiredStateRunning), desiredStateAction("a1", fleetv1.DesiredStateCancelled)); err == nil {
		t.Errorf("%s, a same-named ServiceAccount in another namespace, cancelled without the verb", other)
	}
}

func TestFleetActionDesiredStateCancel_OnlyTheTransitionIsGated(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false}
	v := newActionValidatorWithAuthz(authz)
	for _, tc := range []struct{ from, to fleetv1.DesiredState }{
		{fleetv1.DesiredStateCancelled, fleetv1.DesiredStateCancelled}, // re-writing Cancelled is not a new cancel
		{fleetv1.DesiredStateRunning, fleetv1.DesiredStatePaused},
		{fleetv1.DesiredStatePaused, fleetv1.DesiredStateRunning},
	} {
		if _, err := v.ValidateUpdate(ctxAsUser("updater"),
			desiredStateAction("a1", tc.from), desiredStateAction("a1", tc.to)); err != nil {
			t.Errorf("%s -> %s was refused: %v", tc.from, tc.to, err)
		}
	}
	if authz.calls != 0 {
		t.Errorf("authz consulted %d times on non-cancel transitions, want 0", authz.calls)
	}
}

func TestEnvtest_FleetActionDesiredStateCancelOverTheWire(t *testing.T) {
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
	bind := func(cr *rbacv1.ClusterRole, subject rbacv1.Subject) {
		t.Helper()
		if err := rc.Create(ctx, cr); err != nil {
			t.Fatalf("installing %s: %v", cr.Name, err)
		}
		if err := rc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-" + strings.ReplaceAll(cr.Name, ":", "-"), Namespace: w.ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cr.Name},
			Subjects:   []rbacv1.Subject{subject},
		}); err != nil {
			t.Fatalf("binding %s: %v", cr.Name, err)
		}
	}
	user := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
	}
	seed := func(name string) {
		t.Helper()
		if err := w.admin.Create(ctx, &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name},
			Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		}); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	setDesired := func(c client.Client, name string, ds fleetv1.DesiredState) error {
		cur := &fleetv1.FleetAction{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		cur.Spec.DesiredState = ds
		return c.Patch(ctx, cur, client.MergeFrom(base))
	}
	refusedByHook := func(err error) bool {
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "admission webhook")
	}

	// A principal with update and patch on fleetactions but not cancel: refused by the
	// webhook, not by RBAC. This is the route that needed only update before.
	bind(&rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "action-updater-no-cancel"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{fleetv1.GroupVersion.Group},
			Resources: []string{"fleetactions"}, Verbs: []string{"get", "list", "watch", "update", "patch"}}},
	}, user("wire-action-updater"))
	seed("updater-target")
	switch err := setDesired(impersonate("wire-action-updater"), "updater-target", fleetv1.DesiredStateCancelled); {
	case err == nil:
		t.Error("a principal holding update but not cancel on fleetactions cancelled a FleetAction")
	case !refusedByHook(err):
		t.Errorf("refused, but not by the webhook's cancel check: %v", err)
	}
	// ...and may still make every other update: only the transition is gated.
	if err := setDesired(impersonate("wire-action-updater"), "updater-target", fleetv1.DesiredStatePaused); err != nil {
		t.Errorf("a principal holding update was refused a Paused write: %v", err)
	}

	// The manager, bound to its own shipped role (config/rbac/role.yaml, which holds no
	// cancel), fans a FleetTask cancel out to a member: admitted.
	mgrRoles := loadClusterRoles(t, filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	mgrRole, ok := mgrRoles["swarmada-manager-role"]
	if !ok {
		t.Fatalf("config/rbac/role.yaml has no swarmada-manager-role; found %v", mgrRoles)
	}
	installed := mgrRole.DeepCopy()
	installed.ObjectMeta = metav1.ObjectMeta{Name: "wire-manager-role"}
	bind(installed, user(controllerUser))
	seed("member")
	if err := setDesired(impersonate(controllerUser), "member", fleetv1.DesiredStateCancelled); err != nil {
		t.Errorf("the manager's cancel fan-out to a member FleetAction was refused: %v", err)
	}

	// Per built-in role: cancel is admitted for exactly the roles that hold `cancel`.
	outcome := map[string]string{}
	for _, role := range builtinRoles {
		file := "swarmada_" + strings.ReplaceAll(role, "-", "_") + "_role.yaml"
		for _, cr := range loadClusterRoles(t, filepath.Join("..", "..", "config", "rbac", file)) {
			c := cr.DeepCopy()
			c.ResourceVersion = ""
			bind(c, user("wire-action-"+role))
		}
		name := "cancel-by-" + role
		seed(name)
		switch perr := setDesired(impersonate("wire-action-"+role), name, fleetv1.DesiredStateCancelled); {
		case perr == nil:
			outcome[role] = actionCancelAdmitted
		case refusedByHook(perr):
			outcome[role] = actionCancelRefusedByHook
		case apierrors.IsForbidden(perr):
			outcome[role] = actionCancelRefusedByRBAC
		default:
			t.Fatalf("cancel as %s failed for a reason that is neither RBAC nor the webhook: %v", role, perr)
		}
	}
	var table strings.Builder
	table.WriteString("\nFleetAction cancel (desiredState: Cancelled, merge patch) over the wire, config/rbac roles:\n")
	for _, role := range builtinRoles {
		fmt.Fprintf(&table, "  %-14s %s\n", role, outcome[role])
	}
	t.Log(table.String())
	for _, role := range builtinRoles {
		// The roles holding cancel on fleetactions are the ones holding it on fleettasks.
		if want, got := wantTaskCancel(role), outcome[role]; want != (got == actionCancelAdmitted) {
			t.Errorf("swarmada:%s cancelling a FleetAction: %s; RFC-0001 #security-permission-matrix says %s",
				role, got, yn(want))
		}
	}
}
