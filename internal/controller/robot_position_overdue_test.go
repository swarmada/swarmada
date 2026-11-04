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
	"sync/atomic"
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
	"github.com/swarmada/swarmada/internal/telemetry"
)

// RA-1 for ZonePositionOverdue, asserted on the number of Robot status patches issued,
// not on the values they leave. A reconcile that finds the same state, and a telemetry
// frame, each write nothing.
//
// The fixture robot declares one capability, so these counts never included the empty
// status patch a robot that declared none used to receive on every reconcile. They are
// unchanged now that deriveCapabilities returns nil for an empty set; that case has its
// own test in robot_capabilities_empty_test.go.

type overdueFixture struct {
	t       *testing.T
	c       client.Client
	zc      *ZoneController
	patches *atomic.Int64
}

const overdueNS, overdueRobot, overdueZone = "ra1-overdue", "ra1-amr", "ra1-bay"

func newOverdueFixture(t *testing.T) *overdueFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	robot := robotWithID(overdueRobot, overdueNS, overdueRobot)
	robot.Spec.Zone = overdueZone
	robot.Spec.Capabilities = []fleetv1.ClassCapability{{Name: "navigation.2d", Type: fleetv1.CapabilityKindManual}}
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &metav1.Time{Time: time.Now()}}
	zoneObj := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: overdueZone, Namespace: overdueNS},
		Spec: fleetv1.FleetZoneSpec{PhysicalBounds: &fleetv1.PhysicalBounds{
			Polygon: []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}},
			Frame:   &fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned},
		}},
	}
	patches := &atomic.Int64{}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(robot, zoneObj).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.FleetZone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*fleetv1.Robot); ok {
					patches.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	zc := &ZoneController{Client: c, Scheme: scheme}
	if _, err := zc.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: overdueNS, Name: overdueZone},
	}); err != nil {
		t.Fatalf("zone reconcile: %v", err)
	}
	if n := patches.Load(); n != 0 {
		t.Fatalf("zone reconcile patched the robot %d times; a Containment zone places nobody by declaration", n)
	}
	return &overdueFixture{t: t, c: c, zc: zc, patches: patches}
}

func (f *overdueFixture) reconcile(threshold time.Duration) {
	f.t.Helper()
	rr := &RobotReconciler{Client: f.c, Positions: f.zc, positionOverdueThreshold: threshold}
	if _, err := rr.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: overdueNS, Name: overdueRobot},
	}); err != nil {
		f.t.Fatalf("robot reconcile: %v", err)
	}
}

func (f *overdueFixture) frame(pos *telemetry.Position) {
	f.zc.ObservePosition(context.Background(), telemetry.Frame{RobotID: overdueRobot, Namespace: overdueNS, Position: pos})
}

func (f *overdueFixture) wantPatches(step string, want int64) {
	f.t.Helper()
	if got := f.patches.Load(); got != want {
		f.t.Fatalf("%s: %d robot status patches in total, want %d", step, got, want)
	}
}

func (f *overdueFixture) wantReason(reason string) {
	f.t.Helper()
	r := getRobot(f.t, f.c, overdueRobot, overdueNS)
	c := findCondition(r, conditionTypeZonePositionOverdue)
	if c == nil || c.Reason != reason {
		f.t.Fatalf("%s = %v, want reason %s", conditionTypeZonePositionOverdue, c, reason)
	}
}

// Awaiting: the reconcile that publishes the condition writes once; the next writes nothing.
func TestZonePositionOverdue_RA1_SecondReconcileWritesNothing(t *testing.T) {
	f := newOverdueFixture(t)
	f.reconcile(time.Hour)
	f.wantPatches("first reconcile", 1)
	f.wantReason(reasonAwaitingFirstPosition)
	f.reconcile(time.Hour)
	f.wantPatches("second reconcile, same state", 1)
}

// Escalated: the escalation writes once; later reconciles write nothing, and neither does a
// frame that carries no position.
func TestZonePositionOverdue_RA1_EscalatedStateWritesNothingMore(t *testing.T) {
	f := newOverdueFixture(t)
	f.reconcile(time.Nanosecond)
	f.wantPatches("publish", 1)
	f.reconcile(time.Nanosecond)
	f.wantPatches("escalate", 2)
	f.wantReason(reasonNoPositionReported)
	f.reconcile(time.Nanosecond)
	f.wantPatches("reconcile after escalation", 2)
	f.frame(nil)
	f.frame(nil)
	f.wantPatches("frames with no position", 2)
	f.reconcile(time.Nanosecond)
	f.wantPatches("reconcile after position-less frames", 2)
}

// The first position frame writes currentZone and nothing else; a second identical frame
// writes nothing; the reconcile that frame enqueues clears the condition once; the next
// reconcile and the next frame write nothing.
func TestZonePositionOverdue_RA1_FramesNeverWriteTheCondition(t *testing.T) {
	f := newOverdueFixture(t)
	f.reconcile(time.Nanosecond)
	f.reconcile(time.Nanosecond)
	f.wantPatches("escalated", 2)

	inside := &telemetry.Position{X: 5, Y: 5}
	f.frame(inside)
	f.wantPatches("first position frame (currentZone transition)", 3)
	f.wantReason(reasonNoPositionReported) // the frame path does not touch the condition
	f.frame(inside)
	f.wantPatches("second identical frame", 3)

	f.reconcile(time.Nanosecond)
	f.wantPatches("reconcile after placement (clear)", 4)
	f.wantReason(reasonPositionReported)
	f.reconcile(time.Nanosecond)
	f.frame(inside)
	f.wantPatches("reconcile and frame after the clear", 4)
}

// A position that matches no polygon is not "no position". It writes nothing on the
// frame, and the next reconcile clears the condition from the in-memory record, since the
// persisted status of such a robot is the same as that of one that sent nothing.
func TestZonePositionOverdue_UnmatchedPositionClearsOnTheNextReconcile(t *testing.T) {
	f := newOverdueFixture(t)
	f.reconcile(time.Hour)
	f.wantPatches("awaiting", 1)
	f.frame(&telemetry.Position{X: 50, Y: 50})
	f.wantPatches("unmatched frame, currentZone already empty", 1)
	f.reconcile(time.Hour)
	f.wantPatches("reconcile after the unmatched frame", 2)
	f.wantReason(reasonPositionReported)
	if got := getRobot(t, f.c, overdueRobot, overdueNS).Status.CurrentZone; got != "" {
		t.Fatalf("currentZone = %q, want \"\" for an unmatched position", got)
	}
}
