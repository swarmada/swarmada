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
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// managerRoleFiles are the two copies of the controller-manager's ClusterRole: controller-gen
// writes config/rbac/role.yaml from the markers, and the chart keeps its own copy by hand. A
// grant one copy lacks is a controller that works on one install path and gets 403s on the other.
var managerRoleFiles = []string{
	filepath.Join(rbacDir, "role.yaml"),
	filepath.Join("..", "..", "deploy", "swarmada", "templates", "rbac-manager.yaml"),
}

// loadManagerRoles parses the ClusterRole out of each manager file. The chart file is a Go
// template; its actions are commented out so the document parses as YAML.
func loadManagerRoles(t *testing.T) map[string]rbacv1.ClusterRole {
	t.Helper()
	out := map[string]rbacv1.ClusterRole{}
	for _, path := range managerRoleFiles {
		raw, err := os.ReadFile(path) //nolint:gosec // fixed in-repo manifest paths, test-only
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.ReplaceAll(string(raw), "{{", "#{{")
		for _, chunk := range strings.Split(text, "\n---") {
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(chunk), &role); err != nil || role.Kind != "ClusterRole" {
				continue
			}
			out[filepath.Base(path)] = role
		}
		if _, ok := out[filepath.Base(path)]; !ok {
			t.Fatalf("%s holds no ClusterRole", path)
		}
	}
	return out
}

// The controller records an applied `swarmctl rollout resume` on the rollout itself, with a
// merge patch of its metadata. Both copies of the manager role must grant that write, or the
// resume never takes effect on that install.
func TestRBAC_ManagerPatchesRolloutsToRecordAResume(t *testing.T) {
	for file, role := range loadManagerRoles(t) {
		for _, res := range []string{"firmwarerollouts", "modelrollouts"} {
			if !grants(role, res, "patch") {
				t.Errorf("%s: the manager cannot patch %s, so a rollout resume is never recorded as applied", file, res)
			}
		}
	}
}
