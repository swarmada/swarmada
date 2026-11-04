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
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// RFC-0001 control plane, *Current zone derivation*: the result of each position frame is
// written to Robot.status.currentZone when it differs from the stored value, and of a move
// held under a stop being applied, "the first one that arrives once the move is no longer
// held writes the zone it derives". The write is conditioned on the resourceVersion the
// frame read, and a stop being applied writes the same robot's status (its record in
// status.stops, estopState, estopHeldBy) at the same moment. A frame whose write loses that
// race is derived again against the robot as it now is, and written; it is not dropped to
// wait for a next frame that a stopped robot may never send.

// frameRace is a Zone Controller over a fake API server whose robot status writes can be
// raced: before a currentZone write of robot rk is applied, race() lands another status
// write on rk, so the frame's write, conditioned on the resourceVersion it read, is refused.
type frameRace struct {
	z         *ZoneController
	c         client.Client
	race      func(ctx context.Context, cl client.Client) bool // true: a write was landed first
	conflicts atomic.Int64                                     // currentZone writes refused on a conflict
	writes    atomic.Int64                                     // currentZone writes applied
	patches   atomic.Int64                                     // rk status patches, whatever they carry
}

func newFrameRace(t *testing.T) *frameRace {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	fr := &frameRace{}
	a, b := zoneObj("a", ""), zoneObj("b", "")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(a, b, robotAt("rk", "rid-k", "a")).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				data, _ := patch.Data(obj)
				if obj.GetName() == "rk" {
					fr.patches.Add(1) // race() writes with Update, so every patch of rk is the frame's
				}
				frame := obj.GetName() == "rk" && bytes.Contains(data, []byte(`"currentZone"`))
				if frame && fr.race != nil {
					fr.race(ctx, cl)
				}
				err := cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
				switch {
				case frame && apierrors.IsConflict(err):
					fr.conflicts.Add(1)
				case frame && err == nil:
					fr.writes.Add(1)
				}
				return err
			},
		}).
		Build()
	fr.c = c
	// The API reader is the same fake: the frame re-reads the robot past any cache, as in
	// the manager (the stop guard, estopscope).
	fr.z = &ZoneController{Client: c, APIReader: c, Scheme: scheme, Recorder: record.NewFakeRecorder(64)}
	// a = [0,10] x [0,10], b = [20,30] x [0,10], floor 0.
	square := func(x0 float64) []zone.Point {
		return []zone.Point{{X: x0, Y: 0}, {X: x0 + 10, Y: 0}, {X: x0 + 10, Y: 10}, {X: x0, Y: 10}}
	}
	fr.z.setZones(actionNS, []zone.Candidate{
		{Name: "a", Floor: 0, Polygon: square(0), IsLeaf: true},
		{Name: "b", Floor: 0, Polygon: square(20), IsLeaf: true},
	})
	return fr
}

// stopWrite is the status write a stop being applied makes on rk: its estopState, and the
// stop's record in status.stops. It is written as a plain status update, so it is the API
// server, not the test, that refuses the frame's write that read rk before it.
func stopWrite(n int) func(ctx context.Context, cl client.Client) bool {
	left := n
	return func(ctx context.Context, cl client.Client) bool {
		if left == 0 {
			return false
		}
		left--
		rb := &fleetv1.Robot{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, rb); err != nil {
			return false
		}
		rb.Status.EstopState = fleetv1.RobotEstopStopped
		rb.Status.Stops = append(rb.Status.Stops, fleetv1.RobotStop{ID: fmt.Sprintf("stop-%d", left),
			Scope: fleetv1.StopScopeZone, SentAt: metav1.Now(),
			Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: "a"}})
		return cl.Status().Update(ctx, rb) == nil
	}
}

func frameAt(x float64) telemetry.Frame {
	return telemetry.Frame{RobotID: "rid-k", Namespace: actionNS, Position: &telemetry.Position{X: x, Y: 5}}
}

// One frame, one concurrent status write between its read and its write: the frame's move is
// recorded, and the concurrent write is kept.
func TestZoneObserver_AFrameOutlivesAConcurrentStatusWrite(t *testing.T) {
	fr := newFrameRace(t)
	fr.race = stopWrite(1)

	fr.z.ObservePosition(context.Background(), frameAt(25))

	if n := fr.conflicts.Load(); n != 1 {
		t.Fatalf("%d currentZone writes were refused on a conflict, want 1: the race did not run", n)
	}
	got := getRobot(t, fr.c, "rk", actionNS)
	if got.Status.CurrentZone != "b" {
		t.Fatalf("a frame in b whose write met a concurrent status write left rk recorded in %q, want b "+
			"(the frame was dropped)", got.Status.CurrentZone)
	}
	if got.Status.EstopState != fleetv1.RobotEstopStopped || len(got.Status.Stops) != 1 {
		t.Errorf("the concurrent write was lost: estopState %q, %d stops recorded", got.Status.EstopState,
			len(got.Status.Stops))
	}
	if cz := got.Status.ContainingZones; len(cz) != 1 || cz[0] != "b" {
		t.Errorf("containingZones = %v, want [b]", cz)
	}
	// RA-1: the same position again writes nothing.
	before := fr.writes.Load()
	fr.z.ObservePosition(context.Background(), frameAt(25))
	if n := fr.writes.Load() - before; n != 0 {
		t.Errorf("an unchanged frame after the retried write wrote currentZone %d more times — RA-1", n)
	}
}

// The retry is bounded: a frame whose write is refused on every attempt gives up, and the next
// frame derives and writes the move against what is recorded.
func TestZoneObserver_AFrameRefusedOnEveryAttemptGivesUpAndTheNextFrameWrites(t *testing.T) {
	fr := newFrameRace(t)
	fr.race = stopWrite(1000)

	fr.z.ObservePosition(context.Background(), frameAt(25))
	n := fr.conflicts.Load()
	if n < 2 || n > 10 {
		t.Errorf("a frame refused on every attempt was tried %d times, want a bounded retry (2 to 10)", n)
	}
	if z := getRobot(t, fr.c, "rk", actionNS).Status.CurrentZone; z != "a" {
		t.Fatalf("setup: rk is recorded in %q though every write was refused", z)
	}
	fr.race = nil
	fr.z.ObservePosition(context.Background(), frameAt(25))
	if z := getRobot(t, fr.c, "rk", actionNS).Status.CurrentZone; z != "b" {
		t.Errorf("the frame after the refused ones left rk recorded in %q, want b", z)
	}
}

// The concurrent write already recorded the move the frame derived: the frame is derived again
// against it and writes nothing more (RA-1), and the memo is seeded from what is recorded.
func TestZoneObserver_AFrameWhoseMoveARaceRecordedWritesNothingMore(t *testing.T) {
	fr := newFrameRace(t)
	raced := false
	fr.race = func(ctx context.Context, cl client.Client) bool {
		if raced {
			return false
		}
		raced = true
		rb := &fleetv1.Robot{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, rb); err != nil {
			return false
		}
		rb.Status.CurrentZone = "b"
		rb.Status.ContainingZones = []string{"b"}
		return cl.Status().Update(ctx, rb) == nil
	}

	fr.z.ObservePosition(context.Background(), frameAt(25))

	if n := fr.conflicts.Load(); n != 1 {
		t.Fatalf("%d currentZone writes were refused on a conflict, want 1: the race did not run", n)
	}
	if n := fr.writes.Load(); n != 0 {
		t.Errorf("the frame wrote currentZone %d times after a concurrent write recorded its move — RA-1", n)
	}
	// Nor any other status write: a retry that compared against a memo set back to the read,
	// not against what is recorded, would send a patch that changes nothing.
	if n := fr.patches.Load(); n != 1 {
		t.Errorf("the frame sent %d status patches of rk, want 1 (the refused one) — RA-1", n)
	}
	if z := getRobot(t, fr.c, "rk", actionNS).Status.CurrentZone; z != "b" {
		t.Errorf("rk recorded in %q, want b", z)
	}
	fr.z.ObservePosition(context.Background(), frameAt(25))
	if n := fr.writes.Load(); n != 0 {
		t.Errorf("an unchanged frame wrote currentZone %d times — RA-1", n)
	}
}
