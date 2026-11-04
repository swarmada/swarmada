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
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Only the control plane moves a robot's estop state (RFC-0001 #safety-estop-state-machine).
// The shipped roles are bound here as they ship, and their holders write over the wire with
// no system:masters. swarmada:admin holds robots/status, so its refusals are admission's.
// swarmada:robot, the Fleet Adapter's role, no longer holds it: the adapter reports over
// ControlStream, so its status writes are refused by RBAC before admission is asked.

// bindShippedClusterRole creates the ClusterRole in config/rbac/<file>, renamed for this
// namespace, and binds it in ns to subject.
func bindShippedClusterRole(t *testing.T, w *safetyWire, file string, subject map[string]interface{}) {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", file))
	if err != nil {
		t.Fatalf("reading the shipped role: %v", err)
	}
	var role unstructured.Unstructured
	if err := yaml.Unmarshal(raw, &role.Object); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	role.SetName(role.GetName() + "-" + w.ns)
	if err := w.admin.Create(ctx, &role); err != nil {
		t.Fatalf("creating %s: %v", role.GetName(), err)
	}
	binding := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]interface{}{"name": strings.ReplaceAll(role.GetName(), ":", "-"), "namespace": w.ns},
		"subjects": []interface{}{subject},
		"roleRef":  map[string]interface{}{"kind": "ClusterRole", "name": role.GetName(), "apiGroup": "rbac.authorization.k8s.io"},
	}}
	if err := w.admin.Create(ctx, binding); err != nil {
		t.Fatalf("binding %s: %v", role.GetName(), err)
	}
}

// asIdentity reaches the API server as username in groups, with no system:masters.
func asIdentity(t *testing.T, w *safetyWire, username string, groups ...string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: append(groups, "system:authenticated")}
	c, err := client.New(cfg, client.Options{Scheme: w.scheme})
	if err != nil {
		t.Fatalf("client as %s: %v", username, err)
	}
	return c
}

// patchStatus reads the robot as c and merge-patches its status with mutate.
func patchStatus(c client.Client, ns string, mutate func(*fleetv1.Robot)) error {
	ctx := context.Background()
	var robot fleetv1.Robot
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "amr-1"}, &robot); err != nil {
		return err
	}
	base := robot.DeepCopy()
	mutate(&robot)
	return c.Status().Patch(ctx, &robot, client.MergeFrom(base))
}

func TestEnvtest_OnlyTheControlPlaneMovesARobotsEstopState(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	if err := w.admin.Create(ctx, markerRobot(w.ns, "amr-1", nil)); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}

	bindShippedClusterRole(t, w, "swarmada_robot_role.yaml", map[string]interface{}{
		"kind": "ServiceAccount", "name": "acme-adapter", "namespace": w.ns})
	bindShippedClusterRole(t, w, "swarmada_admin_role.yaml", map[string]interface{}{
		"kind": "User", "name": "ops-admin", "apiGroup": "rbac.authorization.k8s.io"})
	adapter := asIdentity(t, w, "system:serviceaccount:"+w.ns+":acme-adapter",
		"system:serviceaccounts", "system:serviceaccounts:"+w.ns)
	admin := asIdentity(t, w, "ops-admin")
	manager := w.as(t, controllerUser)

	setState := func(c client.Client, state fleetv1.RobotEstopState) error {
		return patchStatus(c, w.ns, func(r *fleetv1.Robot) { r.Status.EstopState = state })
	}
	stored := func() fleetv1.RobotEstopState {
		t.Helper()
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "amr-1"}, &r); err != nil {
			t.Fatalf("reading the robot: %v", err)
		}
		return r.Status.EstopState
	}
	refused := func(t *testing.T, err error, who string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s moved the robot to %s by a robots/status patch", who, stored())
		}
		if !strings.Contains(err.Error(), "estopState") {
			t.Errorf("refused, but not by the estop guard: %v", err)
		}
	}
	// The adapter's role holds no robots/status write, so RBAC refuses it first.
	forbidden := func(t *testing.T, err error, who string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s moved the robot to %s by a robots/status patch", who, stored())
		}
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "robots/status") {
			t.Errorf("refused, but not by RBAC on robots/status: %v", err)
		}
	}

	// The control plane stops the robot and records the stop.
	if err := patchStatus(manager, w.ns, func(r *fleetv1.Robot) {
		r.Status.EstopState = fleetv1.RobotEstopStopped
		r.Status.EstopBaseline = &fleetv1.EstopBaseline{ActiveCapabilities: []string{"navigation"}, RecordedAt: metav1.Now()}
	}); err != nil {
		t.Fatalf("the control plane's own estop write was refused: %v", err)
	}

	for _, from := range []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopResuming} {
		if err := setState(manager, from); err != nil {
			t.Fatalf("the control plane could not move the robot to %s: %v", from, err)
		}
		t.Run("AdapterReleaseFrom"+string(from)+"Refused", func(t *testing.T) {
			forbidden(t, setState(adapter, fleetv1.RobotEstopNormal), "the adapter's service account")
			if got := stored(); got != from {
				t.Fatalf("estopState = %s, want %s", got, from)
			}
		})
		t.Run("AdminReleaseFrom"+string(from)+"Refused", func(t *testing.T) {
			refused(t, setState(admin, fleetv1.RobotEstopNormal), "swarmada:admin")
		})
	}
	t.Run("AdapterSkipsTheClearRefused", func(t *testing.T) {
		if err := setState(manager, fleetv1.RobotEstopStopped); err != nil {
			t.Fatalf("reset to Stopped: %v", err)
		}
		forbidden(t, setState(adapter, fleetv1.RobotEstopResuming), "the adapter's service account")
	})
	t.Run("AdminSkipsTheClearRefused", func(t *testing.T) {
		refused(t, setState(admin, fleetv1.RobotEstopResuming), "swarmada:admin")
	})
	t.Run("AdapterRewritesTheStopRecordRefused", func(t *testing.T) {
		forbidden(t, patchStatus(adapter, w.ns, func(r *fleetv1.Robot) { r.Status.EstopBaseline = nil }),
			"the adapter's service account")
	})
	t.Run("AdminRewritesTheStopRecordRefused", func(t *testing.T) {
		refused(t, patchStatus(admin, w.ns, func(r *fleetv1.Robot) { r.Status.EstopBaseline = nil }),
			"swarmada:admin")
	})

	// The adapter reports its telemetry over ControlStream only; the control plane writes
	// it to status. A direct robots/status write from the adapter's account is refused,
	// telemetry included, and the admin's non-estop status write still lands.
	t.Run("AdapterTelemetryWriteRefusedByRBAC", func(t *testing.T) {
		pct := int32(64)
		forbidden(t, patchStatus(adapter, w.ns, func(r *fleetv1.Robot) { r.Status.BatteryPercent = &pct }),
			"the adapter's service account")
	})
	t.Run("AdminTelemetryWriteAdmitted", func(t *testing.T) {
		pct := int32(64)
		if err := patchStatus(admin, w.ns, func(r *fleetv1.Robot) {
			r.Status.BatteryPercent = &pct
			now := metav1.Now()
			r.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &now}
		}); err != nil {
			t.Fatalf("a status write that changes no estop field or safety input was refused: %v", err)
		}
	})

	// The confirmed resume is the control plane's write.
	t.Run("ControlPlaneResumeAdmitted", func(t *testing.T) {
		if err := setState(manager, fleetv1.RobotEstopResuming); err != nil {
			t.Fatalf("clear to Resuming: %v", err)
		}
		if err := patchStatus(manager, w.ns, func(r *fleetv1.Robot) {
			r.Status.EstopState = fleetv1.RobotEstopNormal
			r.Status.EstopBaseline = nil
		}); err != nil {
			t.Fatalf("the control plane's confirmed resume was refused: %v", err)
		}
	})
}
