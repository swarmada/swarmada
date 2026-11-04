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

// A namespace being torn down deletes a Robot's RobotClass before the Robot, and a Robot
// a backup or GitOps tool holds with its own finalizer then stays terminating: removing
// the finalizer is an update, and vrobot.swarmada.io re-validated the class reference on
// it, so the removal was refused by every identity and the namespace never finished. On a
// Robot being deleted, an update that changes only the finalizers skips the reference
// checks. The held-robot finalizer rule still holds.

const foreignFinalizer = "example.com/backup"

// classedRobotFixture creates RobotClass picker and a Robot of that class carrying the
// foreign finalizer, in ns; markerFixture must already have run there.
func classedRobotFixture(t *testing.T, w *safetyWire, ns, name string) {
	t.Helper()
	ctx := context.Background()
	if err := w.admin.Create(ctx, &fleetv1.RobotClass{
		ObjectMeta: metav1.ObjectMeta{Name: "picker", Namespace: ns},
		Spec: fleetv1.RobotClassSpec{
			Manufacturer: "acme-robotics",
			Model:        "acme-picker",
			BaseAdapter:  fleetv1.BaseAdapterRef{Name: markerRobotAdapter, Version: "1.0.0"},
			Hardware:     []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}},
		},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating RobotClass picker: %v", err)
	}
	r := markerRobot(ns, name, nil)
	r.Spec.RobotClass = "picker"
	r.Finalizers = []string{foreignFinalizer}
	if err := w.admin.Create(ctx, r); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
}

func deleteClass(t *testing.T, w *safetyWire, ns string) {
	t.Helper()
	if err := w.admin.Delete(context.Background(), &fleetv1.RobotClass{
		ObjectMeta: metav1.ObjectMeta{Name: "picker", Namespace: ns}}); err != nil {
		t.Fatalf("deleting RobotClass picker: %v", err)
	}
}

func deleteRobot(t *testing.T, w *safetyWire, ns, name string) {
	t.Helper()
	if err := w.admin.Delete(context.Background(), &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}); err != nil {
		t.Fatalf("deleting %s: %v", name, err)
	}
}

func robotGone(t *testing.T, w *safetyWire, ns, name string) bool {
	t.Helper()
	err := w.admin.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &fleetv1.Robot{})
	return apierrors.IsNotFound(err)
}

func TestEnvtest_ARobotBeingDeletedLosesAForeignFinalizerAfterItsClassIsGone(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	breakGlass := w.as(t, "break-glass")

	t.Run("in a namespace that stays", func(t *testing.T) {
		markerFixture(t, w, w.ns)
		classedRobotFixture(t, w, w.ns, "r-backup")
		deleteClass(t, w, w.ns)
		deleteRobot(t, w, w.ns, "r-backup")
		if robotGone(t, w, w.ns, "r-backup") {
			t.Fatal("the Robot went with its finalizer still on it")
		}

		// The narrowness: a write on the terminating Robot that changes anything else is
		// still validated, and still refused on the missing class.
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "r-backup"}, &r); err != nil {
			t.Fatal(err)
		}
		base := r.DeepCopy()
		r.Labels = map[string]string{"team": "night"}
		r.Finalizers = nil
		if err := breakGlass.Patch(ctx, &r, client.MergeFrom(base)); err == nil ||
			!strings.Contains(err.Error(), `RobotClass "picker" not found`) {
			t.Fatalf("a label change riding the finalizer removal should be validated and refused; got %v", err)
		}

		if err := setFinalizers(breakGlass, w.ns, "r-backup", nil); err != nil {
			t.Fatalf("removing the foreign finalizer from a Robot being deleted was refused: %v", err)
		}
		if !robotGone(t, w, w.ns, "r-backup") {
			t.Fatal("the finalizer is off but the Robot is still there")
		}
	})

	t.Run("in a namespace being deleted", func(t *testing.T) {
		ns := freshNamespace(t, w)
		markerFixture(t, w, ns)
		classedRobotFixture(t, w, ns, "r-backup")
		deleteClass(t, w, ns)
		if err := w.admin.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatalf("deleting the namespace: %v", err)
		}
		// envtest runs no namespace controller, so the Robot is deleted here as the
		// namespace controller would.
		deleteRobot(t, w, ns, "r-backup")
		if err := setFinalizers(breakGlass, ns, "r-backup", nil); err != nil {
			t.Fatalf("removing the foreign finalizer in a terminating namespace was refused: %v", err)
		}
		if !robotGone(t, w, ns, "r-backup") {
			t.Fatal("the finalizer is off but the Robot is still there")
		}
	})

	t.Run("a held robot keeps its last finalizer", func(t *testing.T) {
		bindShippedClusterRole(t, w, "swarmada_fleet_manager_role.yaml", map[string]interface{}{
			"kind": "User", "name": "fm", "apiGroup": "rbac.authorization.k8s.io"})
		fm := asIdentity(t, w, "fm")
		classedRobotFixture(t, w, w.ns, "r-held")
		deleteRobot(t, w, w.ns, "r-held") // admitted: it holds nothing yet
		stopAsManager(t, w, w.ns, "r-held")
		deleteClass(t, w, w.ns)
		for who, c := range map[string]client.Client{"fleet-manager": fm, "system:masters": breakGlass} {
			err := setFinalizers(c, w.ns, "r-held", nil)
			if err == nil || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "holds a stop") {
				t.Errorf("%s removed the last finalizer of a held robot being deleted: %v", who, err)
			}
		}
		if robotGone(t, w, w.ns, "r-held") {
			t.Fatal("the held robot is gone")
		}
	})
}
