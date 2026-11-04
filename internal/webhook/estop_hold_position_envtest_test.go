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
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// Rows of the estop hold invariant for a robot's own position report inside the window
// between a zone's stop being issued and its fan-out (RFC-0001 #safety-estop-scopes). The
// fan-out finds robots by status.currentZone; a report that moves the robot out of the
// stopped zone before the fan-out runs would leave it outside the stop for good. The Zone
// Controller holds that move until the stop is applied. Each row issues the stop with no
// control plane running, so the window stays open for as long as the row needs it, and feeds
// position frames to a Zone Controller of its own.

// geoZone creates a zone with a square polygon [x0, x0+10] x [0, 10] on floor 0.
func (r *rig) geoZone(name string, x0 float64) {
	r.t.Helper()
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns},
		Spec: fleetv1.FleetZoneSpec{PhysicalBounds: &fleetv1.PhysicalBounds{
			Floor:   0,
			Polygon: []fleetv1.Point{{X: x0, Y: 0}, {X: x0 + 10, Y: 0}, {X: x0 + 10, Y: 10}, {X: x0, Y: 10}},
			Frame:   &fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned},
		}}}
	r.must("creating zone "+name, r.w.admin.Create(context.Background(), z))
}

// positionFeed is a Zone Controller of the row's own, fed frames by the row.
type positionFeed struct {
	r  *rig
	zc *controller.ZoneController
}

func (r *rig) positionFeed(c client.Client, zones ...string) *positionFeed {
	r.t.Helper()
	m := r.w.as(r.t, controllerUser)
	if c == nil {
		c = m
	}
	f := &positionFeed{r: r, zc: &controller.ZoneController{Client: c, APIReader: m, Scheme: r.w.scheme,
		Recorder: record.NewFakeRecorder(1024)}}
	for _, z := range zones {
		if _, err := f.zc.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: z}}); err != nil {
			r.t.Fatalf("indexing zone %s: %v", z, err)
		}
	}
	return f
}

// at reports robot's position at x (y = 5) and returns the currentZone recorded afterwards.
func (f *positionFeed) at(robot string, x float64) string {
	f.r.t.Helper()
	id := f.r.get(robot).Annotations[fleetv1.RobotIDAnnotation]
	f.zc.ObservePosition(context.Background(), telemetry.Frame{RobotID: id, Namespace: f.r.w.ns,
		Position: &telemetry.Position{X: x, Y: 5}})
	z := f.r.get(robot).Status.CurrentZone
	f.r.t.Logf("RESULT %s reports x=%v: currentZone=%q", robot, x, z)
	return z
}

// staleZones is a client whose reads of FleetZones answer from a copy taken before the
// stop was issued, as a lagging cache would.
type staleZones struct {
	client.Client
	before map[string]*fleetv1.FleetZone
}

func (s *staleZones) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if fz, ok := obj.(*fleetv1.FleetZone); ok {
		if old, have := s.before[key.Name]; have {
			old.DeepCopyInto(fz)
			return nil
		}
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// racingFrames is a client whose currentZone writes of robot can be raced: while armed, each
// such write first lands another status write of the same robot, as a stop being applied does
// (its record, its outcome), so the API server refuses the frame's write, which is conditioned
// on the resourceVersion the frame read. It counts the refusals.
type racingFrames struct {
	client.Client
	robot     string
	race      func()
	armed     atomic.Int64 // writes still to race
	conflicts atomic.Int64
}

func (c *racingFrames) Status() client.SubResourceWriter {
	return &racingFrameStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type racingFrameStatus struct {
	client.SubResourceWriter
	c *racingFrames
}

func (s *racingFrameStatus) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	data, _ := p.Data(obj)
	frame := obj.GetName() == s.c.robot && bytes.Contains(data, []byte(`"currentZone"`))
	if frame && s.c.armed.Add(-1) >= 0 {
		s.c.race()
	}
	err := s.SubResourceWriter.Patch(ctx, obj, p, opts...)
	if frame && apierrors.IsConflict(err) {
		s.c.conflicts.Add(1)
	}
	return err
}

func positionHoldRows() []invariantRow {
	var rows []invariantRow

	// ── A report out of a zone whose stop is pending is held until the stop is applied ──
	for _, wait := range []time.Duration{0, 1500 * time.Millisecond} {
		wait := wait
		rows = append(rows, invariantRow{name: "position-out-of-a-pending-stop/after-" + wait.String(),
			opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
				r.geoZone("a", 0)
				r.geoZone("b", 20)
				r.robot("rk", "a")
				feed := r.positionFeed(nil, "a", "b")
				if z := feed.at("rk", 5); z != "a" {
					t.Fatalf("setup: rk reports inside a and is recorded in %q", z)
				}
				r.hold("a-held", "rk", true)
				r.trigger(r.zoneObj("a"), "a-stop") // issued; no control plane runs to apply it yet
				if z := feed.at("rk", 25); z != "a" {
					t.Errorf("INVARIANT rk: its report moved it out of a while a's stop was pending (currentZone %q)", z)
				}
				time.Sleep(wait)
				if z := feed.at("rk", 25); z != "a" {
					t.Errorf("INVARIANT rk: a later report moved it out of a while a's stop was pending (currentZone %q)", z)
				}
				r.start()
				r.waitMarker(r.zoneObj("a"), true)
				r.assertHeld("rk", true, "a-held")
				// Released once the stop is applied: the next report is recorded, and the stop holds rk there.
				if z := feed.at("rk", 25); z != "b" {
					t.Errorf("rk's move to b was not applied after a's stop was applied (currentZone %q)", z)
				}
				r.assertHeld("rk", true, "a-held")
			}})
	}

	// ── The first frame after the stop is applied meets a status write of rk: the write the
	// frame derived is refused on a conflict, and the frame is derived again and written, not
	// dropped. A stopped robot need send no next frame. ──
	rows = append(rows, invariantRow{name: "position-out-of-a-pending-stop/frame-meets-a-status-write",
		opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			r.geoZone("a", 0)
			r.geoZone("b", 20)
			r.robot("rk", "a")
			m := r.w.as(t, controllerUser)
			rf := &racingFrames{Client: m, robot: "rk", race: func() {
				rb := r.robotObj("rk")
				r.must("a status write of rk as the control plane", r.update(m, rb, true, func() {
					now := metav1.Now()
					rb.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &now}
				}))
			}}
			feed := r.positionFeed(rf, "a", "b")
			if z := feed.at("rk", 5); z != "a" {
				t.Fatalf("setup: rk reports inside a and is recorded in %q", z)
			}
			r.hold("a-held", "rk", true)
			r.trigger(r.zoneObj("a"), "a-stop")
			if z := feed.at("rk", 25); z != "a" {
				t.Errorf("INVARIANT rk: its report moved it out of a while a's stop was pending (currentZone %q)", z)
			}
			r.start()
			r.waitMarker(r.zoneObj("a"), true)
			r.assertHeld("rk", true, "a-held")
			rf.armed.Store(1)
			if z := feed.at("rk", 25); z != "b" {
				t.Errorf("rk's move to b was not applied by the first frame after a's stop was applied, whose "+
					"write met a status write of rk (currentZone %q)", z)
			}
			if n := rf.conflicts.Load(); n < 1 {
				t.Errorf("setup: the frame's write was not refused on a conflict (%d), so the row tested nothing", n)
			}
			r.assertHeld("rk", true, "a-held")
		}})

	// ── The case of two stops: rk moves from a (stop pending) into b, and b's stop is issued
	// in the same window. b's fan-out does not find rk (it is recorded in a); a's does, and once
	// a's stop is applied the move to b is recorded and b's stop reaches rk as a late arrival. ──
	rows = append(rows, invariantRow{name: "position-from-a-pending-stop-into-another", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.geoZone("a", 0)
			r.geoZone("b", 20)
			r.robot("rk", "a")
			feed := r.positionFeed(nil, "a", "b")
			feed.at("rk", 5)
			r.trigger(r.zoneObj("a"), "a-stop")
			if z := feed.at("rk", 25); z != "a" {
				t.Errorf("INVARIANT rk: moved out of a while a's stop was pending (currentZone %q)", z)
			}
			r.trigger(r.zoneObj("b"), "b-stop")
			r.start()
			r.waitMarker(r.zoneObj("a"), true)
			r.waitMarker(r.zoneObj("b"), true)
			r.assertHeld("rk", true)
			if z := feed.at("rk", 25); z != "b" {
				t.Errorf("rk's move to b was not applied after a's stop was applied (currentZone %q)", z)
			}
			ok, last := r.waitFor("rk records both stops", 15*time.Second, func() (bool, string) {
				h := r.get("rk").Status.EstopHeldBy
				has := map[string]bool{}
				for _, e := range h {
					has[e.Name] = true
				}
				return has["a"] && has["b"], holdsOf(h)
			})
			if !ok {
				t.Errorf("INVARIANT rk: after the move into b it does not record both a's and b's stop (%s)", last)
			}
			r.clearStop(r.zoneObj("a"))
			r.waitMarker(r.zoneObj("a"), false)
			refusedNaming(t, r.resume("rk", "two-stops"), "zone b")
		}})

	// ── The cache lags: the controller's cached read has not seen the trigger ──
	rows = append(rows, invariantRow{name: "position-out-of-a-pending-stop/cache-behind", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.geoZone("a", 0)
			r.geoZone("b", 20)
			r.robot("rk", "a")
			before := map[string]*fleetv1.FleetZone{}
			for _, n := range []string{"a", "b"} {
				z := &fleetv1.FleetZone{}
				r.must("reading "+n, r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: n}, z))
				before[n] = z
			}
			feed := r.positionFeed(&staleZones{Client: r.w.as(t, controllerUser), before: before}, "a", "b")
			feed.at("rk", 5)
			r.trigger(r.zoneObj("a"), "a-stop")
			if z := feed.at("rk", 25); z != "a" {
				t.Errorf("INVARIANT rk: moved out of a on a cached read that had not seen a's stop (currentZone %q)", z)
			}
		}})

	// ── A position report out of hall while hall's and the namespace's stops are pending: the
	// Zone Controller holds the move, so hall's stop reaches r1, and after the namespace's clear
	// the release is refused naming hall. The window counterpart of
	// currentZone-change-then-namespace-clear/{admin,position-report}, which move r1 only once
	// hall's stop is applied. ──
	rows = append(rows, invariantRow{name: "currentZone-change-then-namespace-clear/observe-position",
		opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			r.geoZone("hall", 0)
			r.geoZone("dock", 20)
			r.config()
			r.robot("r1", "hall")
			feed := r.positionFeed(nil, "hall", "dock")
			if z := feed.at("r1", 5); z != "hall" {
				t.Fatalf("setup: r1 reports inside hall and is recorded in %q", z)
			}
			r.hold("a-held", "r1", false)
			r.trigger(r.zoneObj("hall"), "hall-stop") // issued; no control plane runs to apply it yet
			r.trigger(r.configObj(), "ns-stop")
			if z := feed.at("r1", 25); z != "hall" {
				t.Errorf("INVARIANT r1: its report moved it out of hall while hall's stop was pending (currentZone %q)", z)
			}
			r.start()
			r.waitMarker(r.zoneObj("hall"), true)
			r.waitMarker(r.configObj(), true)
			r.waitStopped("r1")
			r.clearStop(r.configObj())
			r.waitMarker(r.configObj(), false)
			ans := r.resume("r1", "cz-release")
			t.Logf("RESULT currentZone resume: %s", ans)
			refusedNaming(t, ans, "zone hall")
			r.dispatch("cz-work", "r1")
			r.assertHeld("r1", false, "a-held", "cz-work")
		}})

	// ── Controls: moves the hold must not touch ──
	rows = append(rows, invariantRow{name: "control-position-into-a-zone-with-a-pending-stop", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.geoZone("a", 0)
			r.geoZone("b", 20)
			r.robot("rk", "b")
			feed := r.positionFeed(nil, "a", "b")
			feed.at("rk", 25)
			r.trigger(r.zoneObj("a"), "a-stop")
			if z := feed.at("rk", 5); z != "a" {
				t.Errorf("control: a move INTO a zone with a pending stop was held (currentZone %q)", z)
			}
			r.start()
			r.waitMarker(r.zoneObj("a"), true)
			r.assertHeld("rk", true)
		}})
	rows = append(rows, invariantRow{name: "control-position-between-zones-with-no-pending-stop",
		run: func(t *testing.T, r *rig) {
			r.geoZone("a", 0)
			r.geoZone("b", 20)
			r.robot("rk", "a")
			feed := r.positionFeed(nil, "a", "b")
			feed.at("rk", 5)
			if z := feed.at("rk", 25); z != "b" {
				t.Errorf("control: a move between zones with no stop was held (currentZone %q)", z)
			}
		}})
	return rows
}
