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

// ─────────────────────────────────────────────────────────────────────────────
// WHO MAY TRIGGER AND CLEAR AN ESTOP, ASKED OF THE API SERVER.
//
// An estop is exercised by writing the swarmada.io/estop-triggered annotation on its
// carrier: the Robot for robot scope, the FleetZone for zone scope, the namespace's
// SwarmadaConfig for namespace scope. Two authorizations apply, in this order:
//
//  1. the API server's RBAC for the write itself — `patch` on the carrier's resource
//     (swarmctl and `kubectl annotate` both send a merge patch);
//  2. the carrier's validating webhook, which authorizes the annotation change with a
//     SubjectAccessReview for a custom verb (estop-trigger / estop-clear) on the same
//     resource.
//
// A role that holds the custom verb but not the write cannot stop anything: RBAC
// refuses the patch before admission runs, with an error that does not mention estop.
//
// This installs the shipped built-in ClusterRoles (config/rbac and the Helm chart's
// mirror of them), binds each to a user, and asks every role × scope × {estop-trigger,
// estop-clear, patch} question through the webhook's own SARAuthorizer, so the
// attributes are exactly the ones production sends. The expected answers are RFC-0001
// #security-permission-matrix. Every built-in role must be present in both sources: the
// chart installs the same six roles config/rbac declares. The over-the-wire tests below
// then drive each scope end to end as each role, without system:masters, for the trigger
// and for the clear.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// estopScopes are the three estop carriers and the resource each authorizes against.
var estopScopes = []struct {
	scope string
	gr    schema.GroupResource
	name  string
}{
	{"robot", robotGR, "r-1"},
	{"zone", fleetZoneGR, "floor-1"},
	{"namespace", swarmadaConfigGR, "swarmada-config"},
}

// builtinRoles is the role order the matrix is printed in.
var builtinRoles = []string{"viewer", "operator", "fleet-manager", "admin", "robot", "edge"}

// carrierWriteVerb is the ordinary write an estop trigger or clear is delivered by.
const carrierWriteVerb = "patch"

// wantEstop is RFC-0001 #security-permission-matrix for the estop custom verbs and the
// carrier write:
//   - estop-trigger: admin at every scope; fleet-manager at robot and zone scope only;
//   - estop-clear:   admin alone, at every scope;
//   - patch on the carrier: admin at every scope; fleet-manager on robots and
//     fleetzones, not on swarmadaconfigs (SwarmadaConfig writes are admin-only).
func wantEstop(role, verb, scope string) bool {
	switch verb {
	case estopTriggerVerb, carrierWriteVerb:
		return role == "admin" || (role == "fleet-manager" && scope != "namespace")
	case estopClearVerb:
		return role == "admin"
	}
	return false
}

// loadClusterRoles reads every ClusterRole in a YAML file, dropping Helm template
// directive lines first (the chart's mirror is wrapped in one {{- if }} … {{- end }}).
func loadClusterRoles(t *testing.T, path string) map[string]*rbacv1.ClusterRole {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var kept [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("{{")) {
			continue
		}
		kept = append(kept, line)
	}
	out := map[string]*rbacv1.ClusterRole{}
	for _, doc := range strings.Split(string(bytes.Join(kept, []byte("\n"))), "\n---") {
		if !strings.Contains(doc, "kind: ClusterRole") {
			continue
		}
		cr := &rbacv1.ClusterRole{}
		if err := yaml.UnmarshalStrict([]byte(doc), cr); err != nil {
			t.Fatalf("parsing a ClusterRole in %s: %v", path, err)
		}
		out[strings.TrimPrefix(cr.Name, "swarmada:")] = cr
	}
	return out
}

func TestEnvtest_EstopVerbSubjectAccessReviewMatrix(t *testing.T) {
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "sar-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	authz := &SARAuthorizer{Client: c}

	sources := []struct{ name, path string }{
		{"config/rbac", ""},
		{"helm", filepath.Join("..", "..", "deploy", "swarmada", "templates", "rbac-builtin-clusterroles.yaml")},
	}
	verbs := []string{estopTriggerVerb, estopClearVerb, carrierWriteVerb}
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
		fmt.Fprintf(&table, "\n%s — estop custom verbs and the carrier write (got / RFC-0001 #security-permission-matrix):\n", src.name)
		fmt.Fprintf(&table, "  %-14s", "role")
		for _, s := range estopScopes {
			fmt.Fprintf(&table, " %-30s", s.scope+" trigger|clear|patch")
		}
		table.WriteString("\n")

		var mismatches []string
		for _, role := range builtinRoles {
			cr, ok := roles[role]
			if !ok {
				mismatches = append(mismatches, fmt.Sprintf("%s: built-in role swarmada:%s is missing, so an "+
					"install from this source has no such role to bind", src.name, role))
				continue
			}
			// A distinct ClusterRole per source, so the two sources cannot mask each other.
			installed := cr.DeepCopy()
			installed.ObjectMeta = metav1.ObjectMeta{Name: "sar-" + strings.ReplaceAll(src.name, "/", "-") + "-" + role}
			if err := c.Create(ctx, installed); err != nil {
				t.Fatalf("installing %s role %s: %v", src.name, role, err)
			}
			user := "sar-user-" + strings.ReplaceAll(src.name, "/", "-") + "-" + role
			if err := c.Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: installed.Name, Namespace: ns.Name},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: installed.Name},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
			}); err != nil {
				t.Fatalf("binding %s: %v", installed.Name, err)
			}

			fmt.Fprintf(&table, "  %-14s", role)
			for _, s := range estopScopes {
				var cell []string
				got := map[string]bool{}
				for _, verb := range verbs {
					ok, err := authz.Authorize(ctx, authenticationv1.UserInfo{Username: user},
						verb, s.gr, ns.Name, s.name)
					if err != nil {
						t.Fatalf("SAR %s %s %s: %v", role, verb, s.gr.Resource, err)
					}
					got[verb] = ok
					want := wantEstop(role, verb, s.scope)
					mark := yn(ok) + "/" + yn(want)
					if ok != want {
						mark += "!"
						mismatches = append(mismatches, fmt.Sprintf("%s: %s %s on %s (%s scope) = %v, RFC says %v",
							src.name, role, verb, s.gr.Resource, s.scope, ok, want))
					}
					cell = append(cell, mark)
				}
				// Independent of the RFC: a custom estop verb the role cannot deliver is a
				// grant that fails in the emergency it exists for.
				for _, verb := range []string{estopTriggerVerb, estopClearVerb} {
					if got[verb] && !got[carrierWriteVerb] {
						mismatches = append(mismatches, fmt.Sprintf("%s: %s holds %s on %s (%s scope) but not %s, "+
							"so RBAC refuses the annotation write before the webhook runs and the verb cannot be exercised",
							src.name, role, verb, s.gr.Resource, s.scope, carrierWriteVerb))
					}
				}
				fmt.Fprintf(&table, " %-30s", strings.Join(cell, " | "))
			}
			table.WriteString("\n")
		}
		t.Log(table.String())
		sort.Strings(mismatches)
		for _, m := range mismatches {
			t.Error(m)
		}
	}
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ─────────────────────────────────────────────────────────────────────────────
// The SAR answers "does the role hold the verb". Over the wire, each built-in role —
// bound in the namespace and impersonated WITHOUT system:masters — patches the trigger
// onto the scope's carrier, exactly as `swarmctl estop trigger` does for a zone
// (patchAnnotation, a merge patch), and then removes it from a carrier the cluster
// admin has triggered, as `swarmctl estop-clear` does. Each outcome is asserted against
// RFC-0001 #security-permission-matrix: admitted for the roles that hold the verb at
// that scope, refused for every other role.
// ─────────────────────────────────────────────────────────────────────────────

const (
	wireAdmitted      = "admitted"
	wireRefusedByHook = "refused by the estop webhook (SAR on the custom verb)"
	wireRefusedByRBAC = "refused by RBAC before admission (no patch on the carrier)"
)

// wireOutcome is one role's result at one scope: the trigger, then the clear.
type wireOutcome struct{ trigger, clear string }

// estopOverTheWire installs the config/rbac built-in roles on w, then, as each role,
// patches the estop-triggered annotation onto the carrier named by key and records the
// outcome, and removes it from a triggered carrier and records that. Whatever the role
// could not do the cluster admin does, so each role starts both from the same carrier
// state. newObj returns an empty object of the carrier's kind.
func estopOverTheWire(t *testing.T, w *safetyWire, scope string, key client.ObjectKey, newObj func() client.Object) map[string]wireOutcome {
	t.Helper()
	ctx := context.Background()

	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(w.cfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}

	setTrigger := func(c client.Client, value string) error {
		obj := newObj()
		if err := w.admin.Get(ctx, key, obj); err != nil {
			t.Fatalf("reading the %s carrier: %v", scope, err)
		}
		base := obj.DeepCopyObject().(client.Object)
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		if value == "" {
			delete(ann, estopTriggeredAnnotation)
		} else {
			ann[estopTriggeredAnnotation] = value
		}
		obj.SetAnnotations(ann)
		return c.Patch(ctx, obj, client.MergeFrom(base))
	}

	classify := func(verb, role string, err error) string {
		switch {
		case err == nil:
			return wireAdmitted
		case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "admission webhook"):
			return wireRefusedByHook
		case apierrors.IsForbidden(err):
			return wireRefusedByRBAC
		}
		t.Fatalf("%s-scope %s as %s failed for a reason that is neither RBAC nor the estop webhook: %v",
			scope, verb, role, err)
		return ""
	}

	outcome := map[string]wireOutcome{}
	for _, role := range builtinRoles {
		file := "swarmada_" + strings.ReplaceAll(role, "-", "_") + "_role.yaml"
		for _, cr := range loadClusterRoles(t, filepath.Join("..", "..", "config", "rbac", file)) {
			installed := cr.DeepCopy()
			installed.ResourceVersion = ""
			if err := rc.Create(ctx, installed); err != nil {
				t.Fatalf("installing %s: %v", cr.Name, err)
			}
			if err := rc.Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "bind-" + role, Namespace: w.ns},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cr.Name},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "wire-" + role}},
			}); err != nil {
				t.Fatalf("binding %s: %v", cr.Name, err)
			}
		}

		ucfg := rest.CopyConfig(w.cfg)
		ucfg.Impersonate = rest.ImpersonationConfig{UserName: "wire-" + role}
		uc, err := client.New(ucfg, client.Options{Scheme: w.scheme})
		if err != nil {
			t.Fatal(err)
		}

		// Trigger as the role; if it could not, the cluster admin does, so the clear
		// below always starts from a triggered carrier.
		var o wireOutcome
		o.trigger = classify(estopTriggerVerb, role, setTrigger(uc, "drill-"+role))
		if o.trigger != wireAdmitted {
			if err := setTrigger(w.admin, "drill-"+role); err != nil {
				t.Fatalf("triggering the %s estop as the cluster admin for %s's clear: %v", scope, role, err)
			}
		}
		// Clear as the role; if it could not, the cluster admin does, so the next role
		// starts from an untriggered carrier.
		o.clear = classify(estopClearVerb, role, setTrigger(uc, ""))
		if o.clear != wireAdmitted {
			if err := setTrigger(w.admin, ""); err != nil {
				t.Fatalf("clearing the %s estop as the cluster admin after %s: %v", scope, role, err)
			}
		}
		outcome[role] = o
	}

	var table strings.Builder
	fmt.Fprintf(&table, "\n%s-scope estop over the wire (config/rbac roles, no system:masters):\n", scope)
	fmt.Fprintf(&table, "  %-14s %-62s %s\n", "role", "trigger", "clear")
	for _, role := range builtinRoles {
		fmt.Fprintf(&table, "  %-14s %-62s %s\n", role, outcome[role].trigger, outcome[role].clear)
	}
	t.Log(table.String())

	for _, role := range builtinRoles {
		for _, c := range []struct{ verb, got string }{
			{estopTriggerVerb, outcome[role].trigger}, {estopClearVerb, outcome[role].clear},
		} {
			want := wantEstop(role, c.verb, scope)
			switch {
			case want && c.got != wireAdmitted:
				t.Errorf("swarmada:%s cannot %s a %s-scoped estop: %s. RFC-0001 #security-permission-matrix grants it.",
					role, c.verb, scope, c.got)
			case !want && c.got == wireAdmitted:
				t.Errorf("swarmada:%s could %s a %s-scoped estop; RFC-0001 #security-permission-matrix denies it.",
					role, c.verb, scope)
			}
		}
	}
	return outcome
}

func TestEnvtest_NamespaceEstopOverTheWirePerBuiltinRole(t *testing.T) {
	w := startSafetyWire(t)
	if err := w.admin.Create(context.Background(), &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: w.ns},
	}); err != nil {
		t.Fatalf("creating the namespace's SwarmadaConfig: %v", err)
	}
	outcome := estopOverTheWire(t, w, "namespace",
		client.ObjectKey{Namespace: w.ns, Name: "swarmada-config"},
		func() client.Object { return &fleetv1.SwarmadaConfig{} })

	// Stated on its own because it is the case the grant once got wrong: fleet-manager
	// held estop-trigger on swarmadaconfigs but no write on them, so the matrix offered
	// a namespace stop that RBAC refused with an error that never mentions estop.
	if outcome["fleet-manager"].trigger == wireAdmitted {
		t.Errorf("swarmada:fleet-manager triggered a namespace-scoped estop; SwarmadaConfig writes are admin-only")
	}
}

func TestEnvtest_ZoneEstopOverTheWirePerBuiltinRole(t *testing.T) {
	w := startSafetyWire(t)
	if err := w.admin.Create(context.Background(), &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "floor-1", Namespace: w.ns},
	}); err != nil {
		t.Fatalf("creating the zone: %v", err)
	}
	outcome := estopOverTheWire(t, w, "zone",
		client.ObjectKey{Namespace: w.ns, Name: "floor-1"},
		func() client.Object { return &fleetv1.FleetZone{} })

	// The path `swarmctl estop trigger <zone>` takes: estop-trigger AND patch on fleetzones.
	if outcome["fleet-manager"].trigger != wireAdmitted {
		t.Errorf("swarmada:fleet-manager cannot estop a zone the way swarmctl does it (a merge patch): %s",
			outcome["fleet-manager"].trigger)
	}
}

func TestEnvtest_RobotEstopOverTheWirePerBuiltinRole(t *testing.T) {
	w := startSafetyWire(t)
	markerFixture(t, w, w.ns)
	if err := w.admin.Create(context.Background(), markerRobot(w.ns, "r-1", nil)); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}
	estopOverTheWire(t, w, "robot",
		client.ObjectKey{Namespace: w.ns, Name: "r-1"},
		func() client.Object { return &fleetv1.Robot{} })
}
