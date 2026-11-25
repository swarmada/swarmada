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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A stop writes each robot it holds several times (its record, estopState, estopHeldBy), and the
// adapter's heartbeats stamp lastSeenAt on it; each write is a new version of the Robot. A frame
// that moves nothing is decided on the cache whatever the version: it writes nothing, so it
// cannot take a robot out of a stop, and the reads it would cost are on the budget the stop's
// own reads use.

// statusWrite returns a write of the robot's status through api, as change makes it.
func statusWrite(t *testing.T, api client.Client, name string, change func(*fleetv1.Robot)) func() {
	t.Helper()
	return func() {
		ctx := context.Background()
		rb := &fleetv1.Robot{}
		if err := api.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: name}, rb); err != nil {
			t.Fatal(err)
		}
		change(rb)
		if err := api.Status().Update(ctx, rb); err != nil {
			t.Fatal(err)
		}
	}
}

// stopWrites writes the robot's status as a stop over zone does: its record, estopState
// Stopping, estopState Stopped and estopHeldBy, one write each.
func stopWrites(t *testing.T, api client.Client, name, zoneName string) []func() {
	t.Helper()
	now := metav1.Now()
	return []func(){
		statusWrite(t, api, name, func(rb *fleetv1.Robot) {
			rb.Status.Stops = append(rb.Status.Stops, fleetv1.RobotStop{ID: "stop-1", Scope: fleetv1.StopScopeZone,
				Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: zoneName}, SentAt: now})
		}),
		statusWrite(t, api, name, func(rb *fleetv1.Robot) { rb.Status.EstopState = fleetv1.RobotEstopStopping }),
		statusWrite(t, api, name, func(rb *fleetv1.Robot) { rb.Status.EstopState = fleetv1.RobotEstopStopped }),
		statusWrite(t, api, name, func(rb *fleetv1.Robot) {
			rb.Status.EstopHeldBy = append(rb.Status.EstopHeldBy,
				fleetv1.EstopHold{Kind: fleetv1.EstopHoldFleetZone, Name: zoneName, Since: now})
		}),
	}
}

// The item's test: a robot held by a stop over the zone it is recorded in reports frames that
// move nothing between the stop's writes. None of them reads the robot past the cache, and none
// writes.
func TestZoneObserver_AStopsStatusWritesCostTheRobotNoReads(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "a")}, nil, true)
	zr.frame("rid-k", 5) // decided once against the API server
	zr.reads.reset()
	writes := zr.currentZoneWrites.Load()
	for _, w := range stopWrites(t, zr.api, "rk", "a") {
		w()
		zr.frame("rid-k", 5)
		zr.frame("rid-k", 5.1)
	}
	if gets, lists := zr.reads.counts(); gets != 0 || lists != 0 {
		t.Fatalf("frames that move nothing between a stop's four status writes read %d robots and %d lists "+
			"past the cache, want none", gets, lists)
	}
	if n := zr.currentZoneWrites.Load() - writes; n != 0 {
		t.Fatalf("frames that move nothing wrote currentZone %d times", n)
	}
}

// The adapter's liveness stamps (lastSeenAt) are writes to the robot too; frames that move
// nothing between them read nothing past the cache.
func TestZoneObserver_ALivenessStampCostsTheRobotNoRead(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "a")}, nil, true)
	zr.frame("rid-k", 5) // decided once against the API server
	zr.reads.reset()
	writes := zr.currentZoneWrites.Load()
	base := time.Now()
	for i := 0; i < 3; i++ {
		seen := metav1.NewTime(base.Add(time.Duration(i) * 15 * time.Second))
		statusWrite(t, zr.api, "rk", func(rb *fleetv1.Robot) {
			if rb.Status.Connectivity == nil {
				rb.Status.Connectivity = &fleetv1.ConnectivityStatus{}
			}
			rb.Status.Connectivity.LastSeenAt = &seen
		})()
		zr.frame("rid-k", 5)
	}
	if gets, lists := zr.reads.counts(); gets != 0 || lists != 0 {
		t.Fatalf("frames that move nothing between three liveness stamps read %d robots and %d lists past the "+
			"cache, want none", gets, lists)
	}
	if n := zr.currentZoneWrites.Load() - writes; n != 0 {
		t.Fatalf("frames that move nothing wrote currentZone %d times", n)
	}
}

// After writes to the robot that change its version, a frame deriving a move out of a zone whose
// stop has been issued and not yet applied still reads the robot past the cache, and the move is
// held: the hold decides on the API server's copy, as before. This guards the hold, not the
// skip: the frame derives a zone other than the memo's, so it reads with or without the change.
func TestZoneObserver_AfterOtherWritesAMoveOutOfAStoppedZoneIsStillReadAndHeld(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "b")}, nil, true)
	zr.frame("rid-k", 25) // inside b; b's stop is issued and not yet applied
	ws := stopWrites(t, zr.api, "rk", "b")
	ws[1]() // estopState Stopping: a new version that does not record b's stop
	zr.frame("rid-k", 25)
	zr.reads.reset()
	writes := zr.currentZoneWrites.Load()

	zr.frame("rid-k", 5) // b → a
	if gets, _ := zr.reads.counts(); gets == 0 {
		t.Fatal("a frame deriving a move out of b was decided on the cache")
	}
	if n := zr.currentZoneWrites.Load() - writes; n != 0 {
		t.Fatalf("a move out of b, whose stop is being applied, was written (%d writes)", n)
	}
	if got := zr.recorded(t, zr.api, "rk"); got != "b" {
		t.Fatalf("currentZone = %q, want b until b's stop is applied", got)
	}
}

// The memos advance before the status write that records them. While that write is in flight
// they are ahead of anything recorded, so they cannot decide a frame on the cache: a cache that
// lags this controller's earlier write can show the robot where the in-flight write puts it,
// and if that write then fails, the robot would stay recorded where it no longer is.
//
// Here the controller wrote the robot from a into c, and the cache still shows a. The robot
// moves back into a. Frame A derives the move and writes it; while that write is in flight,
// frame B (a second stream, as during an adapter's reconnect) derives a too. A's write then
// fails with a server error. B must wait for A, read the robot past the cache and write a.
// Decided on the cache, it would write nothing, and the robot would stay recorded in c, where a
// stop over a would not find it, until its next frame.
func TestZoneObserver_AFrameDuringAnInFlightWriteReadsPastTheCache(t *testing.T) {
	scheme := lockTestScheme(t)
	inA := func() client.Object {
		r := robotAt("rk", "rid-k", "a")
		r.Status.ContainingZones = []string{"a"}
		return r
	}
	zones := func() []client.Object {
		return []client.Object{zoneObj("a", ""), zoneObj("b", ""), zoneObj("c", "")}
	}
	var (
		z           *ZoneController
		armed       atomic.Bool
		failedA     atomic.Bool
		bOnTheCache atomic.Bool
		skipDuringA atomic.Bool
		wg          sync.WaitGroup
		cache       client.Client
	)
	frame := func(x float64) {
		f := frameAt(x)
		f.RobotID = "rid-k"
		z.ObservePosition(context.Background(), f)
	}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(zones(), inA())...).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if armed.CompareAndSwap(true, false) {
					// A's write of a is in flight. First, without timing: B's frame, given the cache's
					// copy, must not be decided on the cache now.
					cached := &fleetv1.Robot{}
					if err := cache.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, cached); err != nil {
						t.Error(err)
					}
					fb := frameAt(5)
					fb.RobotID = "rid-k"
					skipDuringA.Store(z.frameMovesNothing(fb, cached))
					// Then B arrives.
					done := make(chan struct{})
					wg.Add(1)
					go func() { defer wg.Done(); defer close(done); frame(5) }()
					select {
					case <-done: // B returned while A holds the robot's lock: decided on the cache
						bOnTheCache.Store(true)
					case <-time.After(raceWindow): // B waits for A
					}
					failedA.Store(true)
					return apierrors.NewInternalError(errors.New("etcd timeout"))
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	cache = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(zones(), inA())...).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		Build()
	z = &ZoneController{Client: cachedClient{Client: api, cache: cache}, APIReader: &zoneReadCounter{r: api},
		Scheme: scheme, Recorder: record.NewFakeRecorder(64)}
	z.setZones(actionNS, threeSquares())
	recorded := func() string {
		rb := &fleetv1.Robot{}
		if err := api.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: "rk"}, rb); err != nil {
			t.Fatal(err)
		}
		return rb.Status.CurrentZone
	}

	frame(5)  // decided once against the API server: a
	frame(45) // a → c, written; the cache still shows a
	if got := recorded(); got != "c" {
		t.Fatalf("currentZone = %q after the move into c, want c", got)
	}

	armed.Store(true)
	frame(5) // A: c → a, whose write fails while B arrives
	wg.Wait()
	if !failedA.Load() {
		t.Fatal("frame A wrote nothing; the test did not reach the in-flight write")
	}
	if skipDuringA.Load() {
		t.Error("while a write was in flight, a frame given the cache's copy was decided on the cache")
	}
	if bOnTheCache.Load() {
		t.Error("a frame that arrived while a write was in flight was decided on the cache")
	}
	if got := recorded(); got != "a" {
		t.Fatalf("currentZone = %q with the robot in a: a frame during the failed in-flight write "+
			"was decided on the cache and wrote nothing", got)
	}
}
