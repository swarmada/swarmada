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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// swarmada.io/robot-id is the key the control plane routes a physical robot by: the
// registrar, the status sink and the command dispatcher resolve a frame or a command to
// the Robot that carries it. A stop is held on that Robot's status. Moving the id to a
// fresh Robot, or off the stopped one so a fresh Robot could take it, handed the physical
// robot a clean status: Normal and dispatchable, with no clear, no resume and no audit
// record (RFC-0001 #safety-estop-state-machine). So the id cannot change after create,
// for any identity, the manager's and system:masters' included.

// robotIDPatch merge-patches the robot-id annotation of ns/name as c: to value, or off
// the object when value is empty.
func robotIDPatch(c client.Client, ns, name, value string) error {
	ctx := context.Background()
	var r fleetv1.Robot
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &r); err != nil {
		return err
	}
	base := r.DeepCopy()
	if value == "" {
		delete(r.Annotations, fleetv1.RobotIDAnnotation)
	} else {
		if r.Annotations == nil {
			r.Annotations = map[string]string{}
		}
		r.Annotations[fleetv1.RobotIDAnnotation] = value
	}
	return c.Patch(ctx, &r, client.MergeFrom(base))
}

func TestEnvtest_ARobotIDCannotChangeAfterCreate(t *testing.T) {
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

	// Each robot carries a wire id other than its name, as auto-admit stamps one.
	held := []struct {
		name string
		seed func(*fleetv1.Robot)
	}{
		{"stopped", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopped }},
		{"resuming", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopResuming }},
		{"in-error", func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseError }},
		{"normal", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopNormal }},
	}
	wire := func(name string) string { return "wire-" + name }
	for _, h := range held {
		if err := w.admin.Create(ctx, markerRobot(w.ns, h.name,
			map[string]string{fleetv1.RobotIDAnnotation: wire(h.name)})); err != nil {
			t.Fatalf("creating %s: %v", h.name, err)
		}
		var r fleetv1.Robot
		manager := w.as(t, controllerUser)
		if err := manager.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: h.name}, &r); err != nil {
			t.Fatalf("reading %s: %v", h.name, err)
		}
		base := r.DeepCopy()
		h.seed(&r)
		if err := manager.Status().Patch(ctx, &r, client.MergeFrom(base)); err != nil {
			t.Fatalf("the control plane's status write on %s was refused: %v", h.name, err)
		}
	}
	storedID := func(t *testing.T, name string) (string, bool) {
		t.Helper()
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &r); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		v, ok := r.Annotations[fleetv1.RobotIDAnnotation]
		return v, ok
	}
	refused := func(t *testing.T, err error, who, name, what string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s %s the robot-id of %s", who, what, name)
		}
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "cannot patch resource") {
			t.Fatalf("refused by RBAC, not by the gate; the role must hold patch for this to prove anything: %v", err)
		}
		for _, want := range []string{fleetv1.RobotIDAnnotation, wire(name)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %q: %v", want, err)
			}
		}
		if got, _ := storedID(t, name); got != wire(name) {
			t.Fatalf("the refused write landed anyway: robot-id = %q, want %q", got, wire(name))
		}
	}

	for _, h := range held {
		for _, id := range identities {
			t.Run("Relabel/"+h.name+"/"+id.who, func(t *testing.T) {
				refused(t, robotIDPatch(id.c, w.ns, h.name, wire(h.name)+"-x"), id.who, h.name, "changed")
			})
			// Removed, the defaulter backfills the name; the wire id still moves.
			t.Run("Remove/"+h.name+"/"+id.who, func(t *testing.T) {
				refused(t, robotIDPatch(id.c, w.ns, h.name, ""), id.who, h.name, "removed")
			})
		}
	}

	// The item's scenario: relabel the stopped robot, then create a twin carrying its
	// old id. Neither write lands, so no Robot that the physical robot's frames route
	// to is Normal.
	t.Run("RelabelAndTwinLeavesNoNormalRobotOnTheWireID", func(t *testing.T) {
		_ = robotIDPatch(fm, w.ns, "stopped", "wire-stopped-x")
		twinErr := fm.Create(ctx, markerRobot(w.ns, "stopped-b",
			map[string]string{fleetv1.RobotIDAnnotation: wire("stopped")}))
		if twinErr == nil {
			t.Errorf("a twin carrying the stopped robot's wire id was created")
		}
		var robots fleetv1.RobotList
		if err := w.admin.List(ctx, &robots, client.InNamespace(w.ns)); err != nil {
			t.Fatalf("listing: %v", err)
		}
		carriers := 0
		for i := range robots.Items {
			r := &robots.Items[i]
			if r.Annotations[fleetv1.RobotIDAnnotation] != wire("stopped") {
				continue
			}
			carriers++
			if r.Status.EstopState != fleetv1.RobotEstopStopped {
				t.Errorf("robot %s carries wire id %q with estopState %q: the physical robot's frames and "+
					"commands route to a robot that is not stopped", r.Name, wire("stopped"), r.Status.EstopState)
			}
		}
		if carriers != 1 {
			t.Errorf("%d robots carry wire id %q, want exactly the stopped one", carriers, wire("stopped"))
		}
	})

	// Re-writing the same id, and any write that leaves it alone, is admitted.
	t.Run("SameIDAdmitted", func(t *testing.T) {
		if err := robotIDPatch(fm, w.ns, "normal", wire("normal")); err != nil {
			t.Fatalf("re-writing the robot's own id was refused: %v", err)
		}
	})
	t.Run("OtherWriteAdmitted", func(t *testing.T) {
		var r fleetv1.Robot
		if err := fm.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "stopped"}, &r); err != nil {
			t.Fatalf("reading: %v", err)
		}
		base := r.DeepCopy()
		if r.Labels == nil {
			r.Labels = map[string]string{}
		}
		r.Labels["team"] = "night-shift"
		if err := fm.Patch(ctx, &r, client.MergeFrom(base)); err != nil {
			t.Fatalf("a label write on a stopped robot was refused: %v", err)
		}
	})
}
