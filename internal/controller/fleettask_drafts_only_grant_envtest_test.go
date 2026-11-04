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
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The drafts-only grant, as the specification prints it, against a real API server
// with RBAC and admission policies enforced.
//
// The grant has two halves (RFC-0001 #security-authorization-swarmada-rbac-roles). The
// admission policy holds a FleetTask to spec.draft: true, which RBAC cannot express.
// A FleetAction has no draft state, so the policy cannot hold one; the Role holds it,
// by granting no write on fleetactions. This test applies both halves exactly as the
// chapter's YAML blocks print them and dry-runs, as the client, each request the
// grant must refuse, so a change to either block that reopens a dispatch path fails
// here.
//
// The blocks are applied from a pinned copy, not from the chapter source, which a
// published checkout does not carry. The copy is held to the assembled specification,
// which a published checkout does carry, by
// TestDraftsOnlyGrant_PinnedCopyIsTheGrantAsPrinted.

const (
	draftsOnlyGrantPinned = "testdata/drafts-only-grant.md"
	assembledSpec         = "../../rfcs/dist/RFC-0001-core-spec.md"
)

var yamlFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// specObjects decodes every object in the YAML blocks of a specification chapter,
// with the chapter's placeholders filled in.
func specObjects(t *testing.T, path string, fill map[string]string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var objs []*unstructured.Unstructured
	for _, m := range yamlFence.FindAllSubmatch(raw, -1) {
		block := string(m[1])
		for k, v := range fill {
			block = strings.ReplaceAll(block, k, v)
		}
		// A block that does not decode is an illustration (an elided field, a
		// comment standing for a value), not an object; the lookup below fails if
		// the object a test needs is not among the ones that do.
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(block), 4096)
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

func specObject(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no printed %s named %q", kind, name)
	return nil
}

func applyDraftsOnlyGrant(t *testing.T, ns, sa string, role, policy *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	create := func(o *unstructured.Unstructured) {
		t.Helper()
		if err := envK8s.Create(ctx, o); err != nil {
			t.Fatalf("create %s %s: %v", o.GetKind(), o.GetName(), err)
		}
		t.Cleanup(func() { _ = envK8s.Delete(context.Background(), o) })
	}
	role.SetNamespace(ns)
	create(role)
	create(&unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata":   map[string]interface{}{"name": sa, "namespace": ns},
		"subjects": []interface{}{map[string]interface{}{
			"kind": "ServiceAccount", "name": sa, "namespace": ns,
		}},
		"roleRef": map[string]interface{}{
			"kind": "Role", "name": role.GetName(), "apiGroup": "rbac.authorization.k8s.io",
		},
	}})
	// Cluster-scoped: give the policy this test's name so parallel packages and
	// reruns cannot collide.
	policy.SetName(policy.GetName() + "-" + ns)
	create(policy)
	create(&unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicyBinding",
		"metadata":   map[string]interface{}{"name": policy.GetName()},
		"spec": map[string]interface{}{
			"policyName":        policy.GetName(),
			"validationActions": []interface{}{"Deny"},
		},
	}})
}

// asServiceAccount is a client that authenticates as the admin envtest provides and
// impersonates the service account, so RBAC and the policy's userInfo both see it.
func asServiceAccount(t *testing.T, ns, name string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(envCfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + ns + ":" + name,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"},
	}
	c, err := client.New(cfg, client.Options{Scheme: envScheme})
	if err != nil {
		t.Fatalf("impersonating client: %v", err)
	}
	return c
}

func draftsOnlyTask(ns, name string, draft bool) *fleetv1.FleetTask {
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.FleetTaskSpec{
			Draft:            draft,
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
}

// checkDraftsOnlyGrant dry-runs, as the client, each request the grant must refuse
// and the one it must admit.
func checkDraftsOnlyGrant(t *testing.T, role, policy *unstructured.Unstructured, ns, sa string) {
	t.Helper()
	ctx := context.Background()
	applyDraftsOnlyGrant(t, ns, sa, role, policy)
	as := asServiceAccount(t, ns, sa)

	// The policy takes effect asynchronously; wait until a non-draft is refused.
	waitForAdmission(t, "the policy to refuse a non-draft FleetTask", func() bool {
		err := as.Create(ctx, draftsOnlyTask(ns, "probe", false), client.DryRunAll)
		return err != nil && strings.Contains(err.Error(), "spec.draft true")
	})

	if err := as.Create(ctx, draftsOnlyTask(ns, "draft", true), client.DryRunAll); err != nil {
		t.Errorf("a draft FleetTask was refused; the grant must admit it: %v", err)
	}
	if err := as.Create(ctx, draftsOnlyTask(ns, "live", false), client.DryRunAll); err == nil {
		t.Error("a non-draft FleetTask was admitted")
	}

	// Activating a draft is the same dispatch by another write.
	existing := draftsOnlyTask(ns, "held", true)
	if err := envK8s.Create(ctx, existing); err != nil {
		t.Fatalf("create draft as admin: %v", err)
	}
	activate := client.RawPatch("application/merge-patch+json", []byte(`{"spec":{"draft":false}}`))
	if err := as.Patch(ctx, existing.DeepCopy(), activate, client.DryRunAll); err == nil {
		t.Error("the client cleared spec.draft on a draft; the grant must refuse it")
	}

	// A FleetAction has no draft state: creating one dispatches.
	fa := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "fa-direct", Namespace: ns},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeClean, Zone: "warehouse-a",
			RobotSelector: "vacuum-01", Priority: fleetv1.ActionPriorityNormal,
		},
	}
	err := as.Create(ctx, fa, client.DryRunAll)
	switch {
	case err == nil:
		t.Error("DISPATCH: the drafts-only client created a FleetAction (server dry run admitted it)")
	case !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `cannot create resource "fleetactions"`):
		t.Errorf("FleetAction create refused, but not by RBAC: %v", err)
	default:
		t.Logf("FleetAction create refused by RBAC: %v", err)
	}
}

func TestEnvtest_DraftsOnlyGrantAsPrintedDispatchesNothing(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	fill := map[string]string{"<namespace>": ns, "<task-source>": "wms-drafts"}
	objs := specObjects(t, draftsOnlyGrantPinned, fill)
	role := specObject(t, objs, "Role", "swarmada:task-source-drafts-only")
	policy := specObject(t, objs, "ValidatingAdmissionPolicy", "task-source-creates-drafts-only")
	checkDraftsOnlyGrant(t, role, policy, ns, "wms-drafts")
}

// The pinned copy the envtest applies is the grant the specification prints: each
// object decodes to the same content in both, placeholders unfilled.
func TestDraftsOnlyGrant_PinnedCopyIsTheGrantAsPrinted(t *testing.T) {
	pinned := specObjects(t, draftsOnlyGrantPinned, nil)
	printed := specObjects(t, assembledSpec, nil)
	for _, want := range []struct{ kind, name string }{
		{"Role", "swarmada:task-source-drafts-only"},
		{"ValidatingAdmissionPolicy", "task-source-creates-drafts-only"},
	} {
		pin := specObject(t, pinned, want.kind, want.name)
		spec := specObject(t, printed, want.kind, want.name)
		if !equality.Semantic.DeepEqual(pin.Object, spec.Object) {
			t.Errorf("%s %s in %s differs from the one %s prints; copy the printed block into the pin",
				want.kind, want.name, draftsOnlyGrantPinned, assembledSpec)
		}
	}
}
