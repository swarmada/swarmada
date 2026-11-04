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

package rbac

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// swarmadaCRDNames reads metadata.name from every CRD in config/crd/bases, so a CRD added
// there fails the test below until the admin role names it too.
func swarmadaCRDNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "crd", "bases", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // fixed in-repo manifest paths, test-only
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		if doc.Kind == "CustomResourceDefinition" {
			names = append(names, doc.Metadata.Name)
		}
	}
	if len(names) == 0 {
		t.Fatal("no CRD found in config/crd/bases; the check below would establish nothing")
	}
	sort.Strings(names)
	return names
}

// chartAdminRole parses swarmada:admin out of the chart's copy of the built-in roles.
func chartAdminRole(t *testing.T) rbacv1.ClusterRole {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "swarmada", "templates", "rbac-builtin-clusterroles.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed in-repo manifest path, test-only
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "{{", "#{{")
	for _, chunk := range strings.Split(text, "\n---") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(chunk), &role); err == nil && role.Name == "swarmada:admin" {
			return role
		}
	}
	t.Fatalf("%s holds no swarmada:admin", path)
	return rbacv1.ClusterRole{}
}

// swarmada:admin manages Swarmada's own CRDs and no other: every rule that writes
// customresourcedefinitions names exactly the CRDs in config/crd/bases, and no rule grants a
// write RBAC cannot narrow by name (create, deletecollection, or a wildcard).
func TestRBAC_AdminCRDWriteIsSwarmadaOnly(t *testing.T) {
	want := swarmadaCRDNames(t)
	for label, role := range map[string]rbacv1.ClusterRole{
		"config/rbac": allRoles(t)["admin"],
		"helm chart":  chartAdminRole(t),
	} {
		writes := 0
		for _, r := range role.Rules {
			if !inList("apiextensions.k8s.io", r.APIGroups) || !inList("customresourcedefinitions", r.Resources) {
				continue
			}
			for _, v := range r.Verbs {
				switch v {
				case "get", "list", "watch":
					continue
				case "create", "deletecollection", "*":
					t.Errorf("%s: swarmada:admin grants %q on customresourcedefinitions, which RBAC cannot limit by name", label, v)
					continue
				}
				writes++
				got := append([]string(nil), r.ResourceNames...)
				sort.Strings(got)
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s: %q on customresourcedefinitions is limited to %v, want exactly the Swarmada CRDs %v",
						label, v, got, want)
				}
			}
		}
		if writes == 0 {
			t.Errorf("%s: swarmada:admin grants no write on the Swarmada CRDs; the role is meant to manage them", label)
		}
	}
}
