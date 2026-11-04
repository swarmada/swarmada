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
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A Robot that declares no capabilities derives an empty capability set, and
// status.capabilities is omitempty, so the persisted status reads it back as absent.
// The reconcile's material-change guard must see those as the same state: otherwise
// every periodic reconcile of such a robot issues a status patch with an empty body.
// Counted as patches issued, not as values left behind.

type robotPatchLog struct {
	mu     sync.Mutex
	bodies []string
}

func (l *robotPatchLog) counts() (all, empty int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range l.bodies {
		if b == "{}" {
			empty++
		}
	}
	return len(l.bodies), empty
}

func capabilityFixture(t *testing.T, caps []fleetv1.ClassCapability) (client.Client, *robotPatchLog, *RobotReconciler) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	robot := robotWithID("caps-amr", "caps", "caps-amr")
	robot.Spec.Capabilities = caps
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &metav1.Time{Time: time.Now()}}
	log := &robotPatchLog{}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				body, err := patch.Data(obj)
				if err != nil {
					return err
				}
				log.mu.Lock()
				log.bodies = append(log.bodies, string(body))
				log.mu.Unlock()
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	return c, log, &RobotReconciler{Client: c, Scheme: scheme}
}

func reconcileCapsRobot(t *testing.T, r *RobotReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "caps", Name: "caps-amr"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// The first reconcile establishes status; every later one, with nothing changed,
// issues no patch at all.
func TestRobotReconcile_RA1_NoCapabilitiesIssuesNoPatchOnARepeatReconcile(t *testing.T) {
	_, log, r := capabilityFixture(t, nil)
	reconcileCapsRobot(t, r)
	first, _ := log.counts()

	const n = 20
	for i := 0; i < n; i++ {
		reconcileCapsRobot(t, r)
	}
	all, empty := log.counts()
	t.Logf("%d repeat reconciles of a robot with no capabilities: %d status patches, %d with an empty body",
		n, all-first, empty)
	if all != first {
		t.Fatalf("%d repeat reconciles issued %d status patches (%d with an empty body), want 0 (RA-1)",
			n, all-first, empty)
	}
}

// The normalisation cannot hide a real transition: a capability set that becomes
// empty, and one that stops being empty, are each written once.
func TestRobotReconcile_CapabilitySetToAndFromEmptyIsStillWritten(t *testing.T) {
	nav := []fleetv1.ClassCapability{{Name: "navigation.2d", Type: fleetv1.CapabilityKindManual}}
	c, log, r := capabilityFixture(t, nav)
	reconcileCapsRobot(t, r)
	reconcileCapsRobot(t, r)
	settled, _ := log.counts()
	if got := getRobot(t, c, "caps-amr", "caps").Status.Capabilities; len(got) != 1 {
		t.Fatalf("status.capabilities = %+v, want the one declared capability", got)
	}

	setCaps := func(caps []fleetv1.ClassCapability) {
		t.Helper()
		robot := getRobot(t, c, "caps-amr", "caps")
		robot.Spec.Capabilities = caps
		if err := c.Update(context.Background(), robot); err != nil {
			t.Fatalf("update spec: %v", err)
		}
	}

	setCaps(nil)
	reconcileCapsRobot(t, r)
	if all, _ := log.counts(); all != settled+1 {
		t.Fatalf("removing the last capability issued %d patches, want 1", all-settled)
	}
	if got := getRobot(t, c, "caps-amr", "caps").Status.Capabilities; len(got) != 0 {
		t.Fatalf("status.capabilities = %+v after the last capability was removed, want none", got)
	}
	reconcileCapsRobot(t, r)
	if all, _ := log.counts(); all != settled+1 {
		t.Fatalf("a repeat reconcile after the set became empty issued a patch (%d in total since settling)", all-settled)
	}

	setCaps(nav)
	reconcileCapsRobot(t, r)
	if all, _ := log.counts(); all != settled+2 {
		t.Fatalf("restoring a capability issued %d patches since the removal, want 1", all-settled-1)
	}
	if got := getRobot(t, c, "caps-amr", "caps").Status.Capabilities; len(got) != 1 || got[0].Name != "navigation.2d" {
		t.Fatalf("status.capabilities = %+v after restoring navigation.2d", got)
	}
}

// What the API server does with an empty merge patch to the status subresource. The
// resourceVersion is the storage revision: an unchanged one means nothing was
// written to etcd and no watch event was sent.
func TestEnvtest_EmptyStatusMergePatchPersistsNothing(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	robot := envtestValidRobot(ns, "empty-patch-amr", "dock")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	before := etRobot(t, ns, robot.Name)
	for i := 0; i < 5; i++ {
		if err := envK8s.Status().Patch(ctx, before.DeepCopy(),
			client.RawPatch(types.MergePatchType, []byte("{}"))); err != nil {
			t.Fatalf("empty status patch %d: %v", i, err)
		}
	}
	after := etRobot(t, ns, robot.Name)
	t.Logf("resourceVersion before %s, after 5 empty status merge patches %s; managedFields entries %d -> %d",
		before.ResourceVersion, after.ResourceVersion, len(before.ManagedFields), len(after.ManagedFields))
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("an empty status merge patch moved resourceVersion %s -> %s: the API server persisted it",
			before.ResourceVersion, after.ResourceVersion)
	}
}
