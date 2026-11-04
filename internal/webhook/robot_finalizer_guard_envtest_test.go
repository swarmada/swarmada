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

// The delete guard sees a DELETE. A Robot deleted while it held nothing, but kept by a
// finalizer, stays terminating; stopped after that, it is gone the moment its last
// finalizer is removed, and that removal is an UPDATE. Created again, it came back with
// a clean status. So while a robot is held, its last finalizer is removed only by the
// manager, and the robot stays where the stop is recorded (RFC-0001
// #safety-estop-state-machine).

const testFinalizer = "example.com/hold"

// setFinalizers merge-patches the finalizers of ns/name as c.
func setFinalizers(c client.Client, ns, name string, finalizers []string) error {
	ctx := context.Background()
	var r fleetv1.Robot
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &r); err != nil {
		return err
	}
	base := r.DeepCopy()
	r.Finalizers = finalizers
	return c.Patch(ctx, &r, client.MergeFrom(base))
}

// stopAsManager writes estopState Stopped on ns/name as the control plane would.
func stopAsManager(t *testing.T, w *safetyWire, ns, name string) {
	t.Helper()
	ctx := context.Background()
	manager := w.as(t, controllerUser)
	var r fleetv1.Robot
	if err := manager.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &r); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	base := r.DeepCopy()
	r.Status.EstopState = fleetv1.RobotEstopStopped
	if err := manager.Status().Patch(ctx, &r, client.MergeFrom(base)); err != nil {
		t.Fatalf("the control plane's stop of %s was refused: %v", name, err)
	}
}

func TestEnvtest_AHeldRobotKeepsItsLastFinalizer(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	bindShippedClusterRole(t, w, "swarmada_fleet_manager_role.yaml", map[string]interface{}{
		"kind": "User", "name": "fm", "apiGroup": "rbac.authorization.k8s.io"})
	bindShippedClusterRole(t, w, "swarmada_admin_role.yaml", map[string]interface{}{
		"kind": "User", "name": "ops-admin", "apiGroup": "rbac.authorization.k8s.io"})
	fm := asIdentity(t, w, "fm")
	others := []struct {
		who string
		c   client.Client
	}{
		{"fleet-manager", fm},
		{"admin", asIdentity(t, w, "ops-admin")},
		{"system:masters", w.as(t, "break-glass")},
	}
	stored := func(t *testing.T, name string) *fleetv1.Robot {
		t.Helper()
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &r); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return &r
	}

	// The item's scenario: a finalizer armed while the robot is Normal, the delete
	// admitted because it holds nothing, then the stop.
	robot := markerRobot(w.ns, "amr-f", nil)
	robot.Finalizers = []string{testFinalizer}
	if err := fm.Create(ctx, robot); err != nil {
		t.Fatalf("creating the robot with a finalizer: %v", err)
	}
	if err := fm.Delete(ctx, robotRef(w.ns, "amr-f")); err != nil {
		t.Fatalf("deleting the robot while it holds nothing: %v", err)
	}
	if stored(t, "amr-f").DeletionTimestamp == nil {
		t.Fatalf("the robot has no deletionTimestamp; the finalizer did not hold it")
	}
	stopAsManager(t, w, w.ns, "amr-f")

	for _, id := range others {
		t.Run("LastFinalizerRemovalRefused/"+id.who, func(t *testing.T) {
			err := setFinalizers(id.c, w.ns, "amr-f", nil)
			if err == nil {
				t.Fatalf("%s removed the last finalizer of a stopped, terminating robot", id.who)
			}
			if apierrors.IsNotFound(err) {
				t.Fatalf("the robot is gone: an earlier write removed it")
			}
			if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "cannot patch resource") {
				t.Fatalf("refused by RBAC, not by the gate: %v", err)
			}
			for _, want := range []string{"Stopped", "finalizer", "swarmctl estop resume"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			if r := stored(t, "amr-f"); len(r.Finalizers) != 1 {
				t.Fatalf("the refused write landed anyway: finalizers = %v", r.Finalizers)
			}
		})
	}

	// Re-created by name, the robot is still the stopped one.
	t.Run("ReCreateKeepsTheStop", func(t *testing.T) {
		err := fm.Create(ctx, markerRobot(w.ns, "amr-f", nil))
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("re-creating the robot: err=%v, want AlreadyExists", err)
		}
		if got := stored(t, "amr-f").Status.EstopState; got != fleetv1.RobotEstopStopped {
			t.Fatalf("estopState = %q, want Stopped", got)
		}
	})

	// A finalizer that is not the last one goes: the robot stays.
	t.Run("NotTheLastFinalizerAdmitted", func(t *testing.T) {
		r := markerRobot(w.ns, "amr-two", nil)
		r.Finalizers = []string{testFinalizer, "example.com/other"}
		if err := fm.Create(ctx, r); err != nil {
			t.Fatalf("creating: %v", err)
		}
		stopAsManager(t, w, w.ns, "amr-two")
		if err := setFinalizers(fm, w.ns, "amr-two", []string{testFinalizer}); err != nil {
			t.Fatalf("removing one of two finalizers was refused: %v", err)
		}
	})

	// A robot that holds nothing loses its finalizer as before.
	t.Run("NormalRobotFinalizerRemovalAdmitted", func(t *testing.T) {
		r := markerRobot(w.ns, "amr-n", nil)
		r.Finalizers = []string{testFinalizer}
		if err := fm.Create(ctx, r); err != nil {
			t.Fatalf("creating: %v", err)
		}
		if err := fm.Delete(ctx, robotRef(w.ns, "amr-n")); err != nil {
			t.Fatalf("deleting: %v", err)
		}
		if err := setFinalizers(fm, w.ns, "amr-n", nil); err != nil {
			t.Fatalf("removing the finalizer of a robot that holds nothing was refused: %v", err)
		}
	})

	// The manager is the one identity that may remove it.
	t.Run("ManagerRemovalAdmitted", func(t *testing.T) {
		if err := setFinalizers(w.as(t, controllerUser), w.ns, "amr-f", nil); err != nil {
			t.Fatalf("the manager's removal was refused: %v", err)
		}
	})
}

// A namespace that is being deleted still empties: the owner of a third-party finalizer
// removes it, whatever the robot holds, as the delete guard admits the delete.
func TestEnvtest_AHeldRobotsFinalizerGoesWithItsNamespace(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	ns := freshNamespace(t, w)
	markerFixture(t, w, ns)
	r := markerRobot(ns, "amr-f", nil)
	r.Finalizers = []string{testFinalizer}
	if err := w.admin.Create(ctx, r); err != nil {
		t.Fatalf("creating: %v", err)
	}
	stopAsManager(t, w, ns, "amr-f")
	if err := w.admin.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("deleting the namespace: %v", err)
	}
	if err := w.admin.Delete(ctx, robotRef(ns, "amr-f")); err != nil {
		t.Fatalf("the namespace controller's delete was refused: %v", err)
	}
	if err := setFinalizers(w.as(t, "finalizer-owner"), ns, "amr-f", nil); err != nil {
		t.Fatalf("removing the finalizer of a held robot in a terminating namespace was refused; the "+
			"namespace would stay Terminating: %v", err)
	}
}
