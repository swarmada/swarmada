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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A robot's stop lives only in its status: the estop state, the Error phase and a Failed
// component. Deleting the Robot and creating it again brought it back with none of them,
// Normal and dispatchable, without a clear, a resume or an audit record (RFC-0001
// #safety-estop-state-machine). So the Robot gate refuses to delete a robot that is held,
// from every identity: the release comes first, then the delete. A namespace that is
// being deleted still empties, because every object in it is going anyway.

// heldRobots are the robots the gate must not let go of, and what each one holds.
var heldRobots = []struct {
	name  string
	state string // what the refusal must name
	seed  func(*fleetv1.Robot)
}{
	{"stopping", "Stopping", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopping }},
	{"stopped", "Stopped", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopped }},
	{"resuming", "Resuming", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopResuming }},
	{"stop-unconfirmed", "estopState Failed", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopFailed }},
	{"in-error", "Error", func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseError }},
	{"failed-lidar", "lidar", func(r *fleetv1.Robot) {
		r.Status.Hardware = []fleetv1.HardwareComponentStatus{
			{Name: "camera", Status: fleetv1.HardwareHealthy},
			{Name: "lidar", Status: fleetv1.HardwareFailed},
		}
	}},
}

// seedRobot creates name in ns and writes its status as the control plane would.
func seedRobot(t *testing.T, w *safetyWire, ns, name string, seed func(*fleetv1.Robot)) {
	t.Helper()
	ctx := context.Background()
	if err := w.admin.Create(ctx, markerRobot(ns, name, nil)); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	if seed == nil {
		return
	}
	manager := w.as(t, controllerUser)
	var r fleetv1.Robot
	if err := manager.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &r); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	base := r.DeepCopy()
	seed(&r)
	if err := manager.Status().Patch(ctx, &r, client.MergeFrom(base)); err != nil {
		t.Fatalf("the control plane's status write on %s was refused: %v", name, err)
	}
}

// robotRef names a Robot for a delete.
func robotRef(ns, name string) *fleetv1.Robot {
	return &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func TestEnvtest_AHeldRobotCannotBeDeleted(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	bindShippedClusterRole(t, w, "swarmada_fleet_manager_role.yaml", map[string]interface{}{
		"kind": "User", "name": "fm", "apiGroup": "rbac.authorization.k8s.io"})
	bindShippedClusterRole(t, w, "swarmada_admin_role.yaml", map[string]interface{}{
		"kind": "User", "name": "ops-admin", "apiGroup": "rbac.authorization.k8s.io"})
	identities := []struct {
		who string
		c   client.Client
	}{
		{"fleet-manager", asIdentity(t, w, "fm")},
		{"admin", asIdentity(t, w, "ops-admin")},
		{"control plane", w.as(t, controllerUser)},
		{"system:masters", w.as(t, "break-glass")},
	}
	fm := identities[0].c
	for _, h := range heldRobots {
		seedRobot(t, w, w.ns, h.name, h.seed)
	}
	seedRobot(t, w, w.ns, "normal", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopNormal })
	seedRobot(t, w, w.ns, "never-stopped", nil)

	for _, h := range heldRobots {
		for _, id := range identities {
			t.Run(h.name+"/"+id.who, func(t *testing.T) {
				err := id.c.Delete(ctx, robotRef(w.ns, h.name))
				if err == nil {
					t.Fatalf("%s deleted robot %s, which holds %s", id.who, h.name, h.state)
				}
				if apierrors.IsNotFound(err) {
					t.Fatalf("robot %s is gone: an earlier identity deleted it", h.name)
				}
				msg := err.Error()
				if apierrors.IsForbidden(err) && strings.Contains(msg, "cannot delete resource") {
					t.Fatalf("refused by RBAC, not by the gate; the role must hold delete for this to prove anything: %v", err)
				}
				for _, want := range []string{h.state, "swarmctl estop resume", h.name} {
					if !strings.Contains(msg, want) {
						t.Errorf("the refusal does not name %q: %v", want, err)
					}
				}
			})
		}
	}

	// The item's scenario: the stopped robot cannot be replaced by a fresh object.
	t.Run("ReCreateAfterRefusedDeleteKeepsTheStop", func(t *testing.T) {
		_ = fm.Delete(ctx, robotRef(w.ns, "stopped"))
		err := fm.Create(ctx, markerRobot(w.ns, "stopped", nil))
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("re-creating the stopped robot: err=%v, want AlreadyExists", err)
		}
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "stopped"}, &r); err != nil {
			t.Fatalf("reading the robot: %v", err)
		}
		if r.Status.EstopState != fleetv1.RobotEstopStopped {
			t.Fatalf("estopState = %q after the delete and re-create, want Stopped", r.Status.EstopState)
		}
	})

	// A robot that holds nothing is deleted as before.
	for _, name := range []string{"normal", "never-stopped"} {
		t.Run("Deleted/"+name, func(t *testing.T) {
			if err := fm.Delete(ctx, robotRef(w.ns, name)); err != nil {
				t.Fatalf("deleting robot %s, which holds no stop: %v", name, err)
			}
		})
	}
}

// Namespace teardown is exempt: the namespace controller deletes every Robot in a
// namespace that is being deleted, whatever it holds, and a refusal would leave the
// namespace Terminating. The signal is the namespace's own deletionTimestamp, the same one
// the FleetZone delete guard reads. envtest runs no namespace controller, so the deletes
// it would make are made here, one by name and the rest as a collection.
func TestEnvtest_ANamespaceWithAStoppedRobotStillDeletes(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	ns := freshNamespace(t, w)
	markerFixture(t, w, ns)
	for _, h := range heldRobots {
		seedRobot(t, w, ns, h.name, h.seed)
	}
	if err := w.admin.Delete(ctx, robotRef(ns, "stopped")); err == nil {
		t.Fatalf("the stopped robot was deleted before its namespace was; the gate is not in the path")
	}
	if err := w.admin.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("deleting the namespace: %v", err)
	}
	if err := w.admin.Delete(ctx, robotRef(ns, "stopped")); err != nil {
		t.Fatalf("a stopped robot in a terminating namespace was refused; the namespace would stay Terminating: %v", err)
	}
	if err := w.admin.DeleteAllOf(ctx, &fleetv1.Robot{}, client.InNamespace(ns)); err != nil {
		t.Fatalf("the collection delete of the namespace's held robots was refused: %v", err)
	}
	var left fleetv1.RobotList
	if err := w.admin.List(ctx, &left, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(left.Items) != 0 {
		t.Fatalf("%d robots left in the terminating namespace", len(left.Items))
	}
}
