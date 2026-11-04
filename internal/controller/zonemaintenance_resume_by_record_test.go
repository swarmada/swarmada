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

// ─────────────────────────────────────────────────────────────────────────────
// A MAINTENANCE WINDOW RESUMES WHAT IT PAUSED, NOT WHAT ITS SCOPE COVERS NOW.
//
// The scope is re-resolved on every pass. A robot the window paused can leave it while
// the window is Active: its member zone is removed from the composite, the robot is
// re-homed, or the member zone is deleted. If the close resumed only what the scope
// covers at that moment, such a robot would stay in Maintenance after the window
// completed, withheld from work with no window naming it (RFC-0001
// #zonemaintenance-schema, status.pausedRobots).
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// newCountingZMReconciler is newZMReconciler with a count of ZoneMaintenance status
// patches, so a test can show an unchanged record writes nothing (RA-1).
func newCountingZMReconciler(t *testing.T, nowVal *time.Time, objs ...client.Object) (*ZoneMaintenanceReconciler, client.Client, *atomic.Int32) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	var zmStatusPatches atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.ZoneMaintenance{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*fleetv1.ZoneMaintenance); ok && sub == "status" {
					zmStatusPatches.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := &ZoneMaintenanceReconciler{Client: c, Scheme: scheme, now: func() time.Time { return *nowVal }}
	return r, c, &zmStatusPatches
}

func zmZone(name, parent string, members ...string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: zmNS, Name: name},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent, MemberZones: members},
	}
}

func pausedNames(zm *fleetv1.ZoneMaintenance) []string {
	out := make([]string, 0, len(zm.Status.PausedRobots))
	for _, e := range zm.Status.PausedRobots {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func TestZM_ResumesWhatItPausedAfterRobotsLeaveTheScope(t *testing.T) {
	for _, closeBy := range []string{"auto-resume", "delete"} {
		t.Run(closeBy, func(t *testing.T) {
			now := zmBase
			ctx := context.Background()
			objs := []client.Object{
				zmZone("ward-2", "", "room-a", "room-b", "room-d"),
				zmZone("room-a", ""), zmZone("room-b", ""), zmZone("room-c", ""), zmZone("room-d", ""),
				zmRobot("r-a", "room-a", fleetv1.RobotPhaseIdle, ""), // its member zone is deleted
				zmRobot("r-b", "room-b", fleetv1.RobotPhaseIdle, ""), // its member is removed from the composite
				zmRobot("r-d", "room-d", fleetv1.RobotPhaseIdle, ""), // it is re-homed out of the scope
				zmRobot("r-c", "room-c", fleetv1.RobotPhaseIdle, ""), // never in scope
				zoneMaint("ward-maint", fleetv1.ZoneMaintenanceSpec{
					Scope:                  fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "ward-2"},
					Mode:                   fleetv1.ZoneMaintenanceModeGraceful,
					AutoResumeAfterMinutes: 30,
				}),
			}
			r, c, zmPatches := newCountingZMReconciler(t, &now, objs...)

			driveToActive(t, r, "ward-maint")
			if got := pausedNames(getZM(t, c, "ward-maint")); len(got) != 3 {
				t.Fatalf("paused at activation = %v, want [r-a r-b r-d]", got)
			}

			// Mid-window: three ways out of the scope.
			ward := &fleetv1.FleetZone{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: zmNS, Name: "ward-2"}, ward); err != nil {
				t.Fatal(err)
			}
			ward.Spec.MemberZones = []string{"room-a", "room-d"}
			if err := c.Update(ctx, ward); err != nil {
				t.Fatalf("removing room-b from the composite: %v", err)
			}
			rd := &fleetv1.Robot{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: zmNS, Name: "r-d"}, rd); err != nil {
				t.Fatal(err)
			}
			rd.Spec.Zone = "room-c"
			if err := c.Update(ctx, rd); err != nil {
				t.Fatalf("re-homing r-d: %v", err)
			}
			if err := c.Delete(ctx, zmZone("room-a", "")); err != nil {
				t.Fatalf("deleting member room-a: %v", err)
			}

			// The Active loop runs; the record must still name every robot it paused.
			now = now.Add(time.Minute)
			reconcileZM(t, r, "ward-maint")
			want := []string{"r-a", "r-b", "r-d"}
			if got := pausedNames(getZM(t, c, "ward-maint")); !equalStrings(got, want) {
				t.Errorf("mid-window record = %v, want %v: a robot that left the scope was dropped "+
					"from the record while still paused", got, want)
			}

			// RA-1: once the record has settled, further passes write no status.
			before := zmPatches.Load()
			for i := 0; i < 3; i++ {
				now = now.Add(time.Minute)
				reconcileZM(t, r, "ward-maint")
			}
			if n := zmPatches.Load() - before; n != 0 {
				t.Fatalf("%d ZoneMaintenance status patch(es) on unchanged passes, want 0 (RA-1)", n)
			}

			switch closeBy {
			case "auto-resume":
				now = now.Add(31 * time.Minute)
				reconcileZM(t, r, "ward-maint")
				if p := getZM(t, c, "ward-maint").Status.Phase; p != fleetv1.ZoneMaintenancePhaseCompleted {
					t.Fatalf("window phase = %s, want Completed", p)
				}
			case "delete":
				zm := getZM(t, c, "ward-maint")
				if err := c.Delete(ctx, zm); err != nil {
					t.Fatalf("deleting the window: %v", err)
				}
				reconcileZM(t, r, "ward-maint")
			}

			for _, name := range []string{"r-a", "r-b", "r-d"} {
				if p := robotPhase(t, c, name); p != fleetv1.RobotPhaseIdle {
					t.Errorf("robot %s = %s after the window closed (%s), want Idle: the window paused "+
						"it and did not resume it", name, p, closeBy)
				}
			}
			if p := robotPhase(t, c, "r-c"); p != fleetv1.RobotPhaseIdle {
				t.Errorf("robot r-c was never in scope and is %s", p)
			}
		})
	}
}

// A robot another Active window has on its record is not resumed by this window's
// close, even after it left both scopes: the other window will resume it.
func TestZM_RecordOfAnotherActiveWindowHoldsTheRobot(t *testing.T) {
	now := zmBase
	ctx := context.Background()
	objs := []client.Object{
		zmZone("room-a", ""), zmZone("room-c", ""),
		zmRobot("r-a", "room-a", fleetv1.RobotPhaseIdle, ""),
		zoneMaint("first", fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "room-a"},
			Mode:  fleetv1.ZoneMaintenanceModeGraceful, AutoResumeAfterMinutes: 10,
		}),
		zoneMaint("second", fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "room-a"},
			Mode:  fleetv1.ZoneMaintenanceModeGraceful, AutoResumeAfterMinutes: 60,
		}),
	}
	r, c := newZMReconciler(t, &now, objs...)
	driveToActive(t, r, "first")
	driveToActive(t, r, "second")

	ra := &fleetv1.Robot{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: zmNS, Name: "r-a"}, ra); err != nil {
		t.Fatal(err)
	}
	ra.Spec.Zone = "room-c"
	if err := c.Update(ctx, ra); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	reconcileZM(t, r, "second")

	now = now.Add(10 * time.Minute)
	reconcileZM(t, r, "first")
	if p := robotPhase(t, c, "r-a"); p != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("r-a = %s after the first window closed; the second window still has it paused", p)
	}
	now = now.Add(time.Hour)
	reconcileZM(t, r, "second")
	if p := robotPhase(t, c, "r-a"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("r-a = %s after both windows closed, want Idle", p)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A window on a zone that becomes a composite while the window is open covers the new
// members from the next pass: the scope is expanded on every pass (RFC-0001 FZ-C2), and
// the robots that declare the zone itself stay in it.
func TestZM_ZoneThatBecomesACompositeMidWindowCoversItsMembers(t *testing.T) {
	now := zmBase
	ctx := context.Background()
	objs := []client.Object{
		zmZone("ward-2", ""), zmZone("room-a", ""),
		zmRobot("r-ward", "ward-2", fleetv1.RobotPhaseIdle, ""),
		zmRobot("r-a", "room-a", fleetv1.RobotPhaseIdle, ""),
		zoneMaint("ward-maint", fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "ward-2"},
			Mode:  fleetv1.ZoneMaintenanceModeGraceful, AutoResumeAfterMinutes: 30,
		}),
	}
	r, c := newZMReconciler(t, &now, objs...)
	driveToActive(t, r, "ward-maint")
	if p := robotPhase(t, c, "r-a"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("r-a = %s before room-a is a member", p)
	}

	ward := &fleetv1.FleetZone{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: zmNS, Name: "ward-2"}, ward); err != nil {
		t.Fatal(err)
	}
	ward.Spec.MemberZones = []string{"room-a"}
	if err := c.Update(ctx, ward); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	reconcileZM(t, r, "ward-maint")
	for _, name := range []string{"r-ward", "r-a"} {
		if p := robotPhase(t, c, name); p != fleetv1.RobotPhaseMaintenance {
			t.Errorf("robot %s = %s, want Maintenance: the window names ward-2, which now groups room-a", name, p)
		}
	}
}
