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

package heldwork_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// row is one parallel row on the shared rig: its own namespace, and RoleBindings in it only.
type row struct {
	t  *testing.T
	ns string
}

// identity is a principal a row impersonates, with the groups the API server would give it.
type identity struct {
	user   string
	groups []string
}

// newRow starts a row: t.Parallel(), and a fresh namespace deleted when the row ends.
func newRow(t *testing.T) *row {
	t.Helper()
	t.Parallel()
	requireRig(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "held-"}}
	if err := rigAdmin.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating the row's namespace: %v", err)
	}
	t.Cleanup(func() { _ = rigAdmin.Delete(context.Background(), ns) })
	return &row{t: t, ns: ns.Name}
}

// user is a person, named for the row so rows never share a principal.
func (r *row) user(name string) identity {
	return identity{user: name + "-" + r.ns, groups: []string{"system:authenticated"}}
}

// serviceAccount is a ServiceAccount of the row's namespace, with the groups the API server sets.
func (r *row) serviceAccount(name string) identity {
	return identity{user: "system:serviceaccount:" + r.ns + ":" + name, groups: []string{
		"system:serviceaccounts", "system:serviceaccounts:" + r.ns, "system:authenticated"}}
}

// manager is the manager's principal, the one the estop guards admit.
func (r *row) manager() identity {
	return identity{user: controllerUser, groups: []string{
		"system:serviceaccounts", "system:serviceaccounts:swarmada-system", "system:authenticated"}}
}

// subject is id as a RoleBinding subject.
func (id identity) subject() rbacv1.Subject {
	if ns, name, ok := strings.Cut(strings.TrimPrefix(id.user, "system:serviceaccount:"), ":"); ok &&
		strings.HasPrefix(id.user, "system:serviceaccount:") {
		return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: name}
	}
	return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: id.user}
}

// bind grants id a shipped ClusterRole in the row's namespace.
func (r *row) bind(id identity, clusterRole string) {
	r.t.Helper()
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "held-", Namespace: r.ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
		Subjects:   []rbacv1.Subject{id.subject()},
	}
	if err := rigAdmin.Create(context.Background(), rb); err != nil {
		r.t.Fatalf("binding %s to %s: %v", id.user, clusterRole, err)
	}
}

// config is the rig's config impersonating id, without system:masters.
func (r *row) config(id identity) *rest.Config {
	cfg := rest.CopyConfig(rigCfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: id.user, Groups: id.groups}
	return cfg
}

// as is a client that reaches the API server as id.
func (r *row) as(id identity) client.Client {
	r.t.Helper()
	c, err := client.New(r.config(id), client.Options{Scheme: rigScheme})
	if err != nil {
		r.t.Fatalf("client as %s: %v", id.user, err)
	}
	return c
}

// TestRig_IdentitiesAreJudgedByTheShippedRoles checks the rig itself: each identity reaches the
// API server as itself, without system:masters, and RBAC decides what it may do from the shipped
// role bound in the row's namespace and nothing else.
func TestRig_IdentitiesAreJudgedByTheShippedRoles(t *testing.T) {
	r := newRow(t)
	ctx := context.Background()
	for _, c := range []struct {
		id   identity
		role string // "" leaves the identity unbound
	}{
		{r.user("op"), "swarmada:operator"},
		{r.user("fm"), "swarmada:fleet-manager"},
		{r.user("admin"), "swarmada:admin"},
		{r.user("nobody"), ""},
		{r.serviceAccount("sa-op"), "swarmada:operator"},
		{r.serviceAccount("sa-none"), ""},
		{r.manager(), "swarmada-manager-role"},
	} {
		if c.role != "" {
			r.bind(c.id, c.role)
		}
		cs, err := kubernetes.NewForConfig(r.config(c.id))
		if err != nil {
			t.Fatalf("clientset as %s: %v", c.id.user, err)
		}
		review, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("SelfSubjectReview as %s: %v", c.id.user, err)
		}
		got := review.Status.UserInfo
		if got.Username != c.id.user || slices.Contains(got.Groups, "system:masters") {
			t.Errorf("RIG: %s reaches the API server as %q in groups %v, not as itself without system:masters",
				c.id.user, got.Username, got.Groups)
		}
		err = r.as(c.id).List(ctx, &fleetv1.FleetActionList{}, client.InNamespace(r.ns))
		switch {
		case c.role == "" && !apierrors.IsForbidden(err):
			t.Errorf("RIG: unbound %s listed FleetActions in the row's namespace: %v", c.id.user, err)
		case c.role != "" && err != nil:
			t.Errorf("RIG: %s bound to %s was refused a FleetAction list: %v", c.id.user, c.role, err)
		}
		t.Logf("RESULT %s (role %q): groups %v, list: %v", c.id.user, c.role, got.Groups, err)
	}
}
