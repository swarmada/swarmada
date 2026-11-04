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

const rbacDir = "../../config/rbac"

func loadRole(t *testing.T, file string) rbacv1.ClusterRole {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rbacDir, file)) //nolint:gosec // fixed in-repo manifest paths, test-only
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return role
}

func allRoles(t *testing.T) map[string]rbacv1.ClusterRole {
	t.Helper()
	files := map[string]string{
		"viewer":        "swarmada_viewer_role.yaml",
		"operator":      "swarmada_operator_role.yaml",
		"fleet-manager": "swarmada_fleet_manager_role.yaml",
		"admin":         "swarmada_admin_role.yaml",
		"robot":         "swarmada_robot_role.yaml",
	}
	out := map[string]rbacv1.ClusterRole{}
	for short, file := range files {
		out[short] = loadRole(t, file)
	}
	return out
}

func inList(v string, list []string) bool {
	for _, x := range list {
		if x == v || x == "*" {
			return true
		}
	}
	return false
}

// grants reports whether the role permits verb on resource in the swarmada.io group.
func grants(role rbacv1.ClusterRole, resource, verb string) bool {
	for _, r := range role.Rules {
		if !inList("swarmada.io", r.APIGroups) {
			continue
		}
		if inList(resource, r.Resources) && inList(verb, r.Verbs) {
			return true
		}
	}
	return false
}

// CARDINAL SAFETY INVARIANT: estop-clear is granted by admin and NO other builtin
// role (RFC-0001 #security-permission-matrix — the admin-only estop-clear constraint). A regression that
// widened this would be a closed-open safety failure.
func TestRBAC_EstopClearIsAdminOnly(t *testing.T) {
	roles := allRoles(t)
	for short, role := range roles {
		got := grants(role, "fleetzones", "estop-clear") || grants(role, "robots", "estop-clear")
		want := short == "admin"
		if got != want {
			t.Errorf("estop-clear granted by %s = %v, want %v (estop-clear is admin-only)", short, got, want)
		}
	}
}

// estop-trigger: fleet-manager and admin only (not viewer/operator/robot).
func TestRBAC_EstopTriggerScope(t *testing.T) {
	roles := allRoles(t)
	for short, role := range roles {
		got := grants(role, "fleetzones", "estop-trigger") || grants(role, "robots", "estop-trigger")
		want := short == "admin" || short == "fleet-manager"
		if got != want {
			t.Errorf("estop-trigger granted by %s = %v, want %v", short, got, want)
		}
	}
}

// The adapter role is tightly scoped: read robots, but NO robot status write (the adapter
// reports over ControlStream and the control plane writes Robot.status), NO mutation of
// robots, NO create, and NONE of the custom verbs.
func TestRBAC_RobotRoleIsScoped(t *testing.T) {
	robot := allRoles(t)["robot"]
	if !grants(robot, "robots", "get") {
		t.Error("robot role must read robots")
	}
	for _, verb := range []string{"update", "patch"} {
		if grants(robot, "robots/status", verb) {
			t.Errorf("robot role must NOT grant %q on robots/status: the adapter reports status over ControlStream", verb)
		}
	}
	for _, verb := range []string{"create", "update", "patch", "delete", "estop-trigger", "estop-clear"} {
		if grants(robot, "robots", verb) {
			t.Errorf("robot role must NOT grant %q on robots", verb)
		}
	}
	if grants(robot, "fleetactions", "cancel") || grants(robot, "discoveredrobots", "admit") {
		t.Error("robot role must hold no custom verbs")
	}
}

// viewer is strictly read-only across every resource.
func TestRBAC_ViewerIsReadOnly(t *testing.T) {
	viewer := allRoles(t)["viewer"]
	for _, role := range viewer.Rules {
		for _, v := range role.Verbs {
			switch v {
			case "get", "list", "watch":
			default:
				t.Errorf("viewer grants non-read verb %q", v)
			}
		}
	}
}

// operator can manage FleetActions (incl. cancel) but cannot touch robots or estops.
func TestRBAC_OperatorScope(t *testing.T) {
	op := allRoles(t)["operator"]
	if !grants(op, "fleetactions", "create") || !grants(op, "fleetactions", "cancel") {
		t.Error("operator must manage and cancel FleetActions")
	}
	if grants(op, "robots", "update") || grants(op, "fleetzones", "estop-trigger") {
		t.Error("operator must not modify robots or trigger estops")
	}
	if !grants(op, "robots", "get") {
		t.Error("operator must retain read access")
	}
}

// audit-log export is admin-only; read/verify reach fleet-manager (RFC-0001 #security-permission-matrix).
func TestRBAC_AuditLogScope(t *testing.T) {
	roles := allRoles(t)
	if grants(roles["fleet-manager"], "safetyauditlogs", "export") {
		t.Error("fleet-manager must NOT export the audit log (admin-only)")
	}
	if !grants(roles["admin"], "safetyauditlogs", "export") {
		t.Error("admin must be able to export the audit log")
	}
	if !grants(roles["fleet-manager"], "safetyauditlogs", "verify") {
		t.Error("fleet-manager must be able to verify the audit log")
	}
}

// Rejecting a DiscoveredRobot is a mark, not a delete (RFC-0001 #safety-required-events). The SAR-gated `reject`
// verb carries the decision, but the write it performs is a patch — so a role that holds
// `reject` without `patch` passes the authorization check and then fails the write.
func TestRBAC_RejectCanMarkButNotDelete(t *testing.T) {
	roles := allRoles(t)
	for _, short := range []string{"fleet-manager", "admin"} {
		role := roles[short]
		if !grants(role, "discoveredrobots", "reject") {
			t.Errorf("%s must hold the reject verb", short)
		}
		if !grants(role, "discoveredrobots", "patch") {
			t.Errorf("%s holds reject but cannot patch, so the rejection mark would be refused", short)
		}
	}
	// The delete belongs to the manager, which seals ROBOT_REJECTED first. A fleet manager
	// able to delete outright could make a refusal indistinguishable from a TTL sweep —
	// exactly the evidence gap the mark exists to close.
	if grants(roles["fleet-manager"], "discoveredrobots", "delete") {
		t.Error("fleet-manager must NOT delete a DiscoveredRobot outright; the control plane removes it")
	}
	// Read-only roles hold neither the decision nor the write.
	for _, short := range []string{"viewer", "robot"} {
		if grants(roles[short], "discoveredrobots", "patch") {
			t.Errorf("%s must not be able to mark a DiscoveredRobot", short)
		}
	}
}

// All five roles are present and correctly named.
func TestRBAC_AllRolesNamed(t *testing.T) {
	want := map[string]string{
		"viewer": "swarmada:viewer", "operator": "swarmada:operator",
		"fleet-manager": "swarmada:fleet-manager", "admin": "swarmada:admin", "robot": "swarmada:robot",
	}
	roles := allRoles(t)
	for short, name := range want {
		if roles[short].Name != name {
			t.Errorf("role %s name = %q, want %q", short, roles[short].Name, name)
		}
	}
}

// grantsCore reports whether the role permits verb on resource in the core ("") group.
func grantsCore(role rbacv1.ClusterRole, resource, verb string) bool {
	for _, r := range role.Rules {
		if !inList("", r.APIGroups) {
			continue
		}
		if inList(resource, r.Resources) && inList(verb, r.Verbs) {
			return true
		}
	}
	return false
}

// The human roles read core Events: `swarmctl describe` lists an object's Events, and a
// role that cannot list them sees "<unavailable: forbidden>" where a Warning would be.
// Read only: no role writes, deletes or watches Events. The grant is to the human roles;
// the machine roles (robot, edge) get none.
func TestRBAC_EventsReadScope(t *testing.T) {
	roles := allRoles(t)
	roles["edge"] = loadRole(t, "swarmada_edge_role.yaml")
	for short, role := range roles {
		reads := short != "robot" && short != "edge"
		for _, verb := range []string{"get", "list"} {
			if got := grantsCore(role, "events", verb); got != reads {
				t.Errorf("%s: %s events = %v, want %v", short, verb, got, reads)
			}
		}
		for _, verb := range []string{"watch", "create", "update", "patch", "delete", "deletecollection"} {
			if grantsCore(role, "events", verb) {
				t.Errorf("%s grants %s on events; the grant is get/list only", short, verb)
			}
		}
	}
}

// The edge node writes one thing to the API: status.edgeFeedUnavailable on the zones it
// guards, as a merge patch (internal/edge FeedReporter; RFC-0001
// #fleet-adapter-protocol-the-edge-stream). RBAC cannot narrow a grant to one field or
// one zone, so the role holds only the verb that write needs: patch, and not update,
// which replaces the whole status, nor get on the subresource, which the reporter never
// reads (it reads the zone itself).
func TestRBAC_EdgeWritesZoneStatusOnlyByPatch(t *testing.T) {
	edge := loadRole(t, "swarmada_edge_role.yaml")
	if !grants(edge, "fleetzones/status", "patch") {
		t.Error("edge cannot patch fleetzones/status: edge feed loss would go unreported")
	}
	for _, verb := range []string{"get", "update", "create", "delete", "deletecollection", "list", "watch"} {
		if grants(edge, "fleetzones/status", verb) {
			t.Errorf("edge grants %s on fleetzones/status; the feed report needs patch only", verb)
		}
	}
	for _, r := range edge.Rules {
		for _, res := range r.Resources {
			if strings.Contains(res, "/") && res != "fleetzones/status" {
				t.Errorf("edge grants the subresource %s; its one write is fleetzones/status", res)
			}
		}
	}
}
