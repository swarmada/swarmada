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
	"os"
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The swarmada:edge role, as config/rbac ships it, bound to an edge node's service
// account against a real API server. The edge node's one write is the feed report
// (RFC-0001 #fleet-adapter-protocol-the-edge-stream): it reads the zone and merge-patches
// status.edgeFeedUnavailable. The role admits that and nothing wider that RBAC can
// refuse: replacing a zone's status with an update is forbidden.
func TestEnvtest_EdgeRolePatchesTheFeedReportAndCannotReplaceZoneStatus(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "swarmada_edge_role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(raw, role); err != nil {
		t.Fatalf("parse edge role: %v", err)
	}
	// The built-in name is a cluster singleton; this test's copy is its own.
	role.Name = "swarmada:edge:" + ns
	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(envCfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "edge-node", Namespace: ns}},
	}
	for _, o := range []client.Object{role, binding} {
		if err := rc.Create(ctx, o); err != nil {
			t.Fatalf("create %T: %v", o, err)
		}
		obj := o
		t.Cleanup(func() { _ = rc.Delete(context.Background(), obj) })
	}

	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "dock", Namespace: ns}}
	if err := envK8s.Create(ctx, zone); err != nil {
		t.Fatalf("create zone: %v", err)
	}

	edge := asServiceAccount(t, ns, "edge-node")
	fz := &fleetv1.FleetZone{}
	if err := edge.Get(ctx, client.ObjectKeyFromObject(zone), fz); err != nil {
		t.Fatalf("the edge node cannot read the zone it guards: %v", err)
	}
	orig := fz.DeepCopy()
	fz.Status.EdgeFeedUnavailable = []string{"amr-1"}
	if err := edge.Status().Patch(ctx, fz, client.MergeFrom(orig)); err != nil {
		t.Fatalf("the feed report was refused: %v", err)
	}

	fz.Status.EdgeFeedUnavailable = nil
	switch err := edge.Status().Update(ctx, fz); {
	case err == nil:
		t.Error("the edge node replaced a zone's whole status with an update; the role grants patch only")
	case !apierrors.IsForbidden(err):
		t.Errorf("status update refused, but not by RBAC: %v", err)
	}
}
