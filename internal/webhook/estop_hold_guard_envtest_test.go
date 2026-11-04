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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
)

// The hold record and the robot's zone are written by the control plane only: the
// shipped admin role writes robots/status, and the guard refuses it both.
func TestEnvtest_OnlyTheControlPlaneWritesTheHoldRecordAndTheZone(t *testing.T) {
	w := startSafetyWire(t)
	markerFixture(t, w, w.ns)
	if err := w.admin.Create(context.Background(), markerRobot(w.ns, "amr-1", nil)); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}
	bindShippedClusterRole(t, w, "swarmada_admin_role.yaml", map[string]interface{}{
		"kind": "User", "name": "ops-admin", "apiGroup": "rbac.authorization.k8s.io"})
	admin := asIdentity(t, w, "ops-admin")
	manager := w.as(t, controllerUser)

	record := func(r *fleetv1.Robot) {
		r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: markerRobotZone, Since: metav1.Now()}}
	}
	if err := patchStatus(manager, w.ns, record); err != nil {
		t.Fatalf("the control plane's record write was refused: %v", err)
	}
	if err := patchStatus(manager, w.ns, func(r *fleetv1.Robot) { r.Status.CurrentZone = markerRobotZone }); err != nil {
		t.Fatalf("the control plane's zone write was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*fleetv1.Robot)
		field  string
	}{
		{name: "admin removes the record", mutate: func(r *fleetv1.Robot) { r.Status.EstopHeldBy = nil }, field: "estopHeldBy"},
		{name: "admin adds to the record", mutate: func(r *fleetv1.Robot) {
			r.Status.EstopHeldBy = append(r.Status.EstopHeldBy, fleetv1.EstopHold{Kind: fleetv1.EstopHoldFleetZone, Name: "other", Since: metav1.Now()})
		}, field: "estopHeldBy"},
		{name: "admin moves the robot's zone", mutate: func(r *fleetv1.Robot) { r.Status.CurrentZone = "elsewhere" }, field: "currentZone"},
		{name: "admin clears the robot's zone", mutate: func(r *fleetv1.Robot) { r.Status.CurrentZone = "" }, field: "currentZone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := patchStatus(admin, w.ns, tc.mutate)
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("swarmada:admin's robots/status write was not refused on %s: %v", tc.field, err)
			}
			got := holdRobot(t, w, "amr-1")
			if len(got.Status.EstopHeldBy) != 1 || got.Status.CurrentZone != markerRobotZone {
				t.Fatalf("the refused write landed: record %v, currentZone %q", got.Status.EstopHeldBy, got.Status.CurrentZone)
			}
		})
	}
	// Another field of the same status is still admin's to write.
	if err := patchStatus(admin, w.ns, func(r *fleetv1.Robot) {
		pct := int32(42)
		r.Status.BatteryPercent = &pct
	}); err != nil {
		t.Errorf("an unguarded robots/status field was refused: %v", err)
	}
}

// The SwarmadaConfig that carries the namespace stop is not deleted, by any identity,
// until the stop is cleared; one with no stop is.
func TestEnvtest_TheConfigCarryingTheNamespaceStopIsNotDeleted(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	cfg := func() *fleetv1.SwarmadaConfig {
		return &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: controller.SwarmadaConfigName, Namespace: w.ns}}
	}
	c := cfg()
	c.Annotations = map[string]string{estopTriggeredAnnotation: "ns-stop"}
	if err := w.admin.Create(ctx, c); err != nil {
		t.Fatalf("creating the stopped config: %v", err)
	}
	for _, who := range []client.Client{w.admin, w.as(t, controllerUser)} {
		err := who.Delete(ctx, cfg())
		if err == nil || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "kubectl annotate swarmadaconfig") {
			t.Fatalf("the config carrying the stop: delete err = %v, want Forbidden naming the clear", err)
		}
	}
	// A clear in flight (the marker without the trigger) is still the stop.
	cur := cfg()
	if err := w.as(t, controllerUser).Get(ctx, client.ObjectKeyFromObject(cur), cur); err != nil {
		t.Fatal(err)
	}
	base := cur.DeepCopy()
	cur.Annotations = map[string]string{estopProcessedAnnotation: "ns-stop"}
	if err := w.as(t, controllerUser).Patch(ctx, cur, client.MergeFrom(base)); err != nil {
		t.Fatalf("setting a clear in flight as the control plane: %v", err)
	}
	if err := w.admin.Delete(ctx, cfg()); err == nil || !strings.Contains(err.Error(), "clear has not been applied") {
		t.Fatalf("a config whose clear is in flight: delete err = %v, want a refusal", err)
	}
	cur = cfg()
	_ = w.as(t, controllerUser).Get(ctx, client.ObjectKeyFromObject(cur), cur)
	base = cur.DeepCopy()
	cur.Annotations = nil
	if err := w.as(t, controllerUser).Patch(ctx, cur, client.MergeFrom(base)); err != nil {
		t.Fatalf("finishing the clear as the control plane: %v", err)
	}
	if err := w.admin.Delete(ctx, cfg()); err != nil {
		t.Fatalf("a config with no stop was not deleted: %v", err)
	}
}
