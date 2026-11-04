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

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// The rollout controllers, run on a client that holds exactly the manager's ClusterRole as
// config/rbac/role.yaml ships it, against a real API server. The fake client authorizes every
// write, so a write the shipped role does not grant passes every fake-client test and fails on
// a cluster. `swarmctl rollout resume` is the case this pins: the controller records the request
// as applied on the rollout itself, and that write has to be one the role grants.

// managerRoleClient binds a copy of the manager's ClusterRole to a ServiceAccount in ns and
// returns a client impersonating it: RBAC applies, and no system:masters group bypasses it.
func managerRoleClient(t *testing.T, ns string) client.Client {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(raw, role); err != nil {
		t.Fatalf("parse manager role: %v", err)
	}
	// The shipped name is a cluster singleton; this test's copy is its own.
	role.Name = "swarmada-manager-role:" + ns
	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(envCfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-manager:" + ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "swarmada-manager", Namespace: ns}},
	}
	for _, o := range []client.Object{role, binding} {
		if err := rc.Create(ctx, o); err != nil {
			t.Fatalf("create %T: %v", o, err)
		}
		obj := o
		t.Cleanup(func() { _ = rc.Delete(context.Background(), obj) })
	}
	mgr := asServiceAccount(t, ns, "swarmada-manager")
	// The authorizer reads bindings from an informer: wait until the grant is visible.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := mgr.List(ctx, &fleetv1.FirmwareRolloutList{}, client.InNamespace(ns))
		if err == nil {
			return mgr
		}
		if time.Now().After(deadline) {
			t.Fatalf("the manager role never became effective: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// seedRolloutStatus writes a status as the test's admin client; the manager role is not
// what is under test here.
func seedRolloutStatus(t *testing.T, obj client.Object) {
	t.Helper()
	if err := envK8s.Status().Update(context.Background(), obj); err != nil {
		t.Fatalf("seed %T status: %v", obj, err)
	}
}

// createRolloutRobot creates a target robot, with the spec fields the CRD requires that the
// fake-client fixtures leave out, and writes its status.
func createRolloutRobot(t *testing.T, ns string, rob *fleetv1.Robot) {
	t.Helper()
	ctx := context.Background()
	st := rob.Status
	valid := envtestValidRobot(ns, rob.Name, "z-a")
	rob.Namespace = ns
	rob.Annotations = valid.Annotations
	rob.Spec.Manufacturer, rob.Spec.Model = valid.Spec.Manufacturer, valid.Spec.Model
	rob.Spec.Adapter, rob.Spec.Zone = valid.Spec.Adapter, valid.Spec.Zone
	if err := envK8s.Create(ctx, rob); err != nil {
		t.Fatalf("create robot %s: %v", rob.Name, err)
	}
	rob.Status = st
	if err := envK8s.Status().Update(ctx, rob); err != nil {
		t.Fatalf("robot %s status: %v", rob.Name, err)
	}
}

func rolloutSigningOff(ns string) *fleetv1.SwarmadaConfig {
	cfg := signingConfig(false)
	cfg.Namespace = ns
	return cfg
}

func TestEnvtest_RolloutResumeUnderManagerRole(t *testing.T) {
	requireEnvtest(t)

	t.Run("firmware", func(t *testing.T) {
		ctx := context.Background()
		ns := envtestNamespace(t)
		mgr := managerRoleClient(t, ns)
		if err := envK8s.Create(ctx, rolloutSigningOff(ns)); err != nil {
			t.Fatal(err)
		}
		createRolloutRobot(t, ns, failedFirmware("r1", "2.4.0", "flash verify failed", time.Now().Add(-time.Minute)))
		createRolloutRobot(t, ns, targetRobot("r2", fleetv1.RobotPhaseIdle, 90))

		ro := pausingFwRollout("")
		ro.Namespace = ns
		if err := envK8s.Create(ctx, ro); err != nil {
			t.Fatal(err)
		}
		ro.Status = fleetv1.FirmwareRolloutStatus{
			Phase:        fleetv1.RolloutPhasePaused,
			PausedAt:     &metav1.Time{Time: time.Now().Add(-time.Minute)},
			FailedRobots: []fleetv1.RolloutRobotResult{{RobotName: "r1", Namespace: ns, Reason: "flash verify failed"}},
		}
		seedRolloutStatus(t, ro)
		requestRolloutResume(t, ro, "r1 flash chip is dead; excluding it")

		rec := &captureRecorder{}
		r := &FirmwareRolloutReconciler{Client: mgr, Scheme: envScheme, Audit: rec}
		key := types.NamespacedName{Namespace: ns, Name: ro.Name}
		for i := 0; i < 3; i++ {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("reconcile %d under the manager role: %v", i+1, err)
			}
		}

		got := &fleetv1.FirmwareRollout{}
		if err := envK8s.Get(ctx, key, got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase == fleetv1.RolloutPhasePaused {
			t.Errorf("the rollout is still Paused after the resume: %+v", got.Status)
		}
		if p := got.Annotations[rolloutResumeProcessedAnnotation]; p != got.Annotations[rolloutResumeAnnotation] {
			t.Errorf("processed marker = %q, want the request %q", p, got.Annotations[rolloutResumeAnnotation])
		}
		if n := len(entriesOfType(rec, audit.EventRolloutResumed)); n != 1 {
			t.Errorf("ROLLOUT_RESUMED entries = %d, want exactly 1", n)
		}
	})

	t.Run("model", func(t *testing.T) {
		ctx := context.Background()
		ns := envtestNamespace(t)
		mgr := managerRoleClient(t, ns)
		if err := envK8s.Create(ctx, rolloutSigningOff(ns)); err != nil {
			t.Fatal(err)
		}
		failed := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
		failed.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
			{Name: "item-recognition", Status: fleetv1.ModelStatusFailed, FailureReason: "inference health check failed"},
		}
		createRolloutRobot(t, ns, failed)
		createRolloutRobot(t, ns, targetRobot("r2", fleetv1.RobotPhaseIdle, 90))

		ro := pickerRollout()
		ro.Namespace = ns
		ro.Spec.ModelChecksum = fwChecksum
		if err := envK8s.Create(ctx, ro); err != nil {
			t.Fatal(err)
		}
		ro.Status = fleetv1.ModelRolloutStatus{
			Phase:        fleetv1.RolloutPhasePaused,
			PausedAt:     &metav1.Time{Time: time.Now().Add(-time.Minute)},
			FailedRobots: []fleetv1.RolloutRobotResult{{RobotName: "r1", Namespace: ns, Reason: "inference health check failed"}},
		}
		seedRolloutStatus(t, ro)
		requestRolloutResume(t, ro, "r1 has a failed SSD; excluding it")

		rec := &captureRecorder{}
		r := &ModelRolloutReconciler{Client: mgr, Scheme: envScheme, Audit: rec}
		key := types.NamespacedName{Namespace: ns, Name: ro.Name}
		for i := 0; i < 3; i++ {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("reconcile %d under the manager role: %v", i+1, err)
			}
		}

		got := &fleetv1.ModelRollout{}
		if err := envK8s.Get(ctx, key, got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase == fleetv1.RolloutPhasePaused {
			t.Errorf("the rollout is still Paused after the resume: %+v", got.Status)
		}
		if p := got.Annotations[rolloutResumeProcessedAnnotation]; p != got.Annotations[rolloutResumeAnnotation] {
			t.Errorf("processed marker = %q, want the request %q", p, got.Annotations[rolloutResumeAnnotation])
		}
		if n := len(entriesOfType(rec, audit.EventRolloutResumed)); n != 1 {
			t.Errorf("ROLLOUT_RESUMED entries = %d, want exactly 1", n)
		}
	})

	// A request left on a rollout that is not Paused is recorded as processed, so it cannot
	// resume a later pause, and the rollout goes on dispatching.
	t.Run("stale-request-on-InProgress", func(t *testing.T) {
		ctx := context.Background()
		ns := envtestNamespace(t)
		mgr := managerRoleClient(t, ns)
		if err := envK8s.Create(ctx, rolloutSigningOff(ns)); err != nil {
			t.Fatal(err)
		}
		createRolloutRobot(t, ns, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

		ro := pausingFwRollout("")
		ro.Namespace = ns
		if err := envK8s.Create(ctx, ro); err != nil {
			t.Fatal(err)
		}
		ro.Status = fleetv1.FirmwareRolloutStatus{Phase: fleetv1.RolloutPhaseInProgress}
		seedRolloutStatus(t, ro)
		requestRolloutResume(t, ro, "left over from an earlier pause")

		r := &FirmwareRolloutReconciler{Client: mgr, Scheme: envScheme}
		key := types.NamespacedName{Namespace: ns, Name: ro.Name}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile under the manager role: %v", err)
		}

		got := &fleetv1.FirmwareRollout{}
		if err := envK8s.Get(ctx, key, got); err != nil {
			t.Fatal(err)
		}
		if p := got.Annotations[rolloutResumeProcessedAnnotation]; p != "left over from an earlier pause" {
			t.Errorf("processed marker = %q, want the stale request recorded", p)
		}
		rob := &fleetv1.Robot{}
		if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "r1"}, rob); err != nil {
			t.Fatal(err)
		}
		if rob.Annotations[annPendingFirmwareVersion] != ro.Spec.NewVersion {
			t.Errorf("the rollout did not dispatch to r1 after recording the request (annotations %v)", rob.Annotations)
		}
	})
}

// requestRolloutResume writes the operator's annotation as `swarmctl rollout resume` does.
func requestRolloutResume(t *testing.T, obj client.Object, reason string) {
	t.Helper()
	ctx := context.Background()
	if err := envK8s.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[rolloutResumeAnnotation] = reason
	obj.SetAnnotations(ann)
	if err := envK8s.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		t.Fatalf("requesting resume: %v", err)
	}
}
