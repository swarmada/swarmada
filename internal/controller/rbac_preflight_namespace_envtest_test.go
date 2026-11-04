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

// Does the RBAC preflight reach the same verdict when cmd/manager hands it the manager's
// own namespace instead of the empty (cluster-wide) one?
//
// The preflight exists for the FleetAction G1 gate, which reads ComputeEndpoints and
// ResultSinks in the namespace of every action it admits through the manager's
// cluster-wide cache, and fails open when it cannot. A grant that covers only the
// manager's own namespace leaves that gate open everywhere else, so it must fail the
// preflight whatever namespace the manager runs in.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	rbacpreflight "github.com/swarmada/swarmada/internal/rbac"
)

// managerRoleFromConfig reads the manager ClusterRole the kustomize install ships.
func managerRoleFromConfig(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		cr := &rbacv1.ClusterRole{}
		if yaml.Unmarshal([]byte(doc), cr) == nil && cr.Kind == "ClusterRole" && len(cr.Rules) > 0 {
			return cr
		}
	}
	t.Fatal("config/rbac/role.yaml has no ClusterRole")
	return nil
}

func TestEnvtest_RBACPreflightVerdictWithAndWithoutNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	mgrNS := envtestNamespace(t)
	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(envCfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}

	role := managerRoleFromConfig(t)
	role.ObjectMeta = metav1.ObjectMeta{Name: "preflight-manager-" + mgrNS}
	if err := rc.Create(ctx, role); err != nil {
		t.Fatalf("install manager role: %v", err)
	}
	t.Cleanup(func() { _ = rc.Delete(context.Background(), role) })

	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}
	cases := []struct {
		name string
		bind func(user string) client.Object
		// cluster and own are the preflight verdicts (true = passes) with an empty
		// namespace and with the manager's namespace.
		cluster, own bool
	}{
		{"shipped ClusterRoleBinding", func(user string) client.Object {
			return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "pf-crb-" + mgrNS}, RoleRef: ref,
				Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}}
		}, true, true},
		{"no grant", func(string) client.Object { return nil }, false, false},
		// The case the two differ on: the gate is open in every other namespace.
		{"grant in the manager's namespace only", func(user string) client.Object {
			return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "pf-rb", Namespace: mgrNS}, RoleRef: ref,
				Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}}
		}, false, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := "preflight-sa-" + mgrNS + "-" + string(rune('a'+i))
			if b := tc.bind(user); b != nil {
				if err := rc.Create(ctx, b); err != nil {
					t.Fatalf("bind: %v", err)
				}
				t.Cleanup(func() { _ = rc.Delete(context.Background(), b) })
			}
			ucfg := rest.CopyConfig(envCfg)
			ucfg.Impersonate = rest.ImpersonationConfig{UserName: user}
			reader, err := client.New(ucfg, client.Options{Scheme: envScheme})
			if err != nil {
				t.Fatal(err)
			}
			cluster := rbacpreflight.Preflight(ctx, reader, "") == nil
			own := rbacpreflight.Preflight(ctx, reader, mgrNS) == nil
			t.Logf("preflight passes: cluster-wide=%v, namespace %q=%v", cluster, mgrNS, own)
			if cluster != tc.cluster || own != tc.own {
				t.Errorf("verdicts (cluster-wide, own namespace) = (%v, %v), want (%v, %v)",
					cluster, own, tc.cluster, tc.own)
			}
		})
	}
}
