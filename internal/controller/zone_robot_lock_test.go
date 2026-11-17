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
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// raceWindow is how long a test lets a second writer run while a first writer's status write is
// in flight. A writer that does not wait for the robot's lock reaches its read within it.
const raceWindow = 300 * time.Millisecond

// gatedReader passes reads to r. When armed, the next Get, once it has returned the API server's
// copy, closes read and waits for release before handing that copy back.
type gatedReader struct {
	r             client.Reader
	armed         atomic.Bool
	read, release chan struct{}
}

func newGatedReader() *gatedReader {
	return &gatedReader{read: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := g.r.Get(ctx, key, obj, opts...)
	if g.armed.CompareAndSwap(true, false) {
		close(g.read)
		<-g.release
	}
	return err
}

func (g *gatedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return g.r.List(ctx, list, opts...)
}

func lockTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// Two streams deliver frames for one robot, which is in c (an adapter's reconnect). A, a stale
// frame from the old stream, derives a; B, from the new stream, derives c and arrives while A's
// write of a is in flight. Had B read the robot then, its copy would still record c: it would
// write nothing and note that copy's version, or note it after A's write, and A's write of a
// would then commit. The robot would be recorded in a while it is in c, the cache, not yet
// delivering A's write, would show c, and the next frame, C, decided on the cache, would write
// nothing: a stop over c (the fan-out finds robots by currentZone) would not find the robot
// until the informer delivered A's write. B waits for A's write instead, reads a, and writes c.
func TestZoneObserver_TwoStreamsCannotLeaveTheRobotRecordedWhereItIsNot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bNotesLast bool // B, had it read during A's write, would note its read after A's
	}{
		{name: "B would note its read after A's write is noted", bNotesLast: true},
		{name: "B would note its read before A's write commits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := lockTestScheme(t)
			inC := func() client.Object {
				r := robotAt("rk", "rid-k", "c")
				r.Status.ContainingZones = []string{"c"}
				return r
			}
			zones := func() []client.Object {
				return []client.Object{zoneObj("a", ""), zoneObj("b", ""), zoneObj("c", "")}
			}
			var (
				z          *ZoneController
				armed      atomic.Bool
				aCommitted atomic.Bool
				wg         sync.WaitGroup
			)
			gate := newGatedReader()
			frame := func(x float64) {
				f := frameAt(x)
				f.RobotID = "rid-k"
				z.ObservePosition(context.Background(), f)
			}
			api := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(append(zones(), inC())...).
				WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
						patch client.Patch, opts ...client.SubResourcePatchOption,
					) error {
						if armed.CompareAndSwap(true, false) {
							// A's write of a is in flight: B arrives.
							done := make(chan struct{})
							if tc.bNotesLast {
								gate.armed.Store(true)
							}
							wg.Add(1)
							go func() { defer wg.Done(); defer close(done); frame(45) }()
							reached := done // B decided
							if tc.bNotesLast {
								reached = gate.read // B holds the API server's copy, and notes it once released
							}
							select {
							case <-reached:
							case <-time.After(raceWindow): // B waits for A
							}
							err := cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
							aCommitted.Store(err == nil)
							return err
						}
						return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			cache := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(append(zones(), inC())...).
				WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
				Build()
			gate.r = api
			z = &ZoneController{Client: cachedClient{Client: api, cache: cache}, APIReader: gate, Scheme: scheme,
				Recorder: record.NewFakeRecorder(64)}
			z.setZones(actionNS, threeSquares())
			recorded := func() string {
				rb := &fleetv1.Robot{}
				if err := api.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: "rk"}, rb); err != nil {
					t.Fatal(err)
				}
				return rb.Status.CurrentZone
			}

			frame(45) // decided once against the API server: c
			armed.Store(true)
			frame(5) // A, stale: c → a
			close(gate.release)
			wg.Wait()
			if !aCommitted.Load() {
				t.Fatal("A's write of a did not commit; the race was not reached")
			}

			frame(45) // C: the robot is in c
			if got := recorded(); got != "c" {
				t.Fatalf("currentZone = %q with the robot in c: a frame that arrived while another stream's write "+
					"was in flight decided on the robot as it was before that write, and a stop over c would not find it", got)
			}
		})
	}
}

// A declared zone's placement and a frame for the robot it places are two writers of one robot's
// currentZone and memos. The frame waits for the placement's write to finish before it reads the
// robot, as it waits for another frame's.
func TestZonePlacement_AFrameWaitsForAPlacementsWrite(t *testing.T) {
	scheme := lockTestScheme(t)
	declaredOnly := map[string]bool{"d": true}
	var (
		z          *ZoneController
		armed      atomic.Bool
		readDuring atomic.Bool
		wg         sync.WaitGroup
	)
	gate := newGatedReader()
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(zoneObj("a", ""), zoneObj("b", ""), zoneObj("c", ""), zoneObj("d", ""),
			declaredAt("rk", "rid-k", "d", "", false)).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if armed.CompareAndSwap(true, false) {
					// The placement's write of d is in flight: a frame arrives.
					gate.armed.Store(true)
					wg.Add(1)
					go func() {
						defer wg.Done()
						f := frameAt(5)
						f.RobotID = "rid-k"
						z.ObservePosition(context.Background(), f)
					}()
					select {
					case <-gate.read:
						readDuring.Store(true)
					case <-time.After(raceWindow):
					}
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	gate.r = api
	z = &ZoneController{Client: api, APIReader: gate, Scheme: scheme, Recorder: record.NewFakeRecorder(64)}
	z.setZoneIndex(actionNS, threeSquares(), declaredOnly)

	armed.Store(true)
	if err := z.placeDeclaredMembers(context.Background(), declaredZone("d"), declaredOnly); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	wg.Wait()
	if armed.Load() {
		t.Fatal("the placement wrote nothing; the test did not reach its write")
	}
	if readDuring.Load() {
		t.Fatal("a frame read the robot while a declared placement's write of it was in flight")
	}
}

// deadlineReader passes reads to r and counts those whose context carries no deadline, or one
// shorter than the API server's own request timeout.
type deadlineReader struct {
	r                    client.Reader
	gets, lists, undated atomic.Int64
}

// undatedOrShort reports whether ctx carries no deadline, or one that would cut short a call the
// API server would still answer (its default request timeout is 60 s).
func undatedOrShort(ctx context.Context) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) < 50*time.Second
}

func (d *deadlineReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	d.gets.Add(1)
	if undatedOrShort(ctx) {
		d.undated.Add(1)
	}
	return d.r.Get(ctx, key, obj, opts...)
}

func (d *deadlineReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	d.lists.Add(1)
	if undatedOrShort(ctx) {
		d.undated.Add(1)
	}
	return d.r.List(ctx, list, opts...)
}

// The robot's lock is held across API calls: the read past the cache, the stops a move is checked
// against, and the status write. Each carries a deadline, so a call the API server never answers
// releases the lock (and the stream whose frame holds it) instead of holding it for good. The
// deadline is no shorter than the API server's own request timeout: a write that takes 20 s under
// a stop's load must still land, or the robot stays recorded where it was and the late-arrival
// pass never finds it in a stopped zone.
func TestZoneObserver_TheCallsMadeUnderTheRobotLockHaveADeadline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		robot *fleetv1.Robot
		write func(*ZoneController)
	}{
		{name: "a frame", robot: robotAt("rk", "rid-k", "a"), write: func(z *ZoneController) {
			f := frameAt(45)
			f.RobotID = "rid-k"
			z.ObservePosition(context.Background(), f)
		}},
		{name: "a declared placement", robot: declaredAt("rk", "rid-k", "d", "a", false), write: func(z *ZoneController) {
			if err := z.placeDeclaredMembers(context.Background(), declaredZone("d"), map[string]bool{"d": true}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := lockTestScheme(t)
			var patches, undatedPatches atomic.Int64
			api := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(zoneObj("a", ""), zoneObj("b", ""), zoneObj("c", ""), zoneObj("d", ""), tc.robot).
				WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
						patch client.Patch, opts ...client.SubResourcePatchOption,
					) error {
						patches.Add(1)
						if undatedOrShort(ctx) {
							undatedPatches.Add(1)
						}
						return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			rd := &deadlineReader{r: api}
			z := &ZoneController{Client: api, APIReader: rd, Scheme: scheme, Recorder: record.NewFakeRecorder(64)}
			z.setZoneIndex(actionNS, threeSquares(), map[string]bool{"d": true})
			tc.write(z)
			if rd.gets.Load() == 0 || rd.lists.Load() == 0 || patches.Load() != 1 {
				t.Fatalf("the write made %d reads, %d lists and %d status writes past the cache; "+
					"want at least one read and one list, and one write", rd.gets.Load(), rd.lists.Load(), patches.Load())
			}
			if n := rd.undated.Load() + undatedPatches.Load(); n != 0 {
				t.Fatalf("%d API calls made under the robot's lock carry no deadline, or one under 50 s", n)
			}
		})
	}
}

// Each robot has its own lock: a frame for another robot does not wait for this robot's write,
// even under API load, when that write takes seconds. rk and rj52 would share one lock if locks
// were shared between robots by a hash of their names.
func TestZoneObserver_AnotherRobotsFrameDoesNotWaitForARobotsWrite(t *testing.T) {
	scheme := lockTestScheme(t)
	var (
		z      *ZoneController
		armed  atomic.Bool
		during atomic.Bool
		wg     sync.WaitGroup
	)
	frame := func(id string, x float64) {
		f := frameAt(x)
		f.RobotID = id
		z.ObservePosition(context.Background(), f)
	}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(zoneObj("a", ""), zoneObj("b", ""), zoneObj("c", ""),
			robotAt("rk", "rid-k", "a"), robotAt("rj52", "rid-j", "a")).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if obj.GetName() == "rk" && armed.CompareAndSwap(true, false) {
					// rk's write is in flight: a frame moves rj52.
					done := make(chan struct{})
					wg.Add(1)
					go func() { defer wg.Done(); defer close(done); frame("rid-j", 45) }()
					select {
					case <-done:
						during.Store(true)
					case <-time.After(raceWindow):
					}
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	z = &ZoneController{Client: api, APIReader: api, Scheme: scheme, Recorder: record.NewFakeRecorder(64)}
	z.setZones(actionNS, threeSquares())

	armed.Store(true)
	frame("rid-k", 45) // rk: a → c
	wg.Wait()
	if armed.Load() {
		t.Fatal("rk's frame wrote nothing; the test did not reach its write")
	}
	if !during.Load() {
		t.Fatal("a frame for rj52 waited for rk's write")
	}
	rb := &fleetv1.Robot{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: "rj52"}, rb); err != nil {
		t.Fatal(err)
	}
	if rb.Status.CurrentZone != "c" {
		t.Fatalf("rj52's currentZone = %q, want c", rb.Status.CurrentZone)
	}
}

// A robot's lock exists only while a writer holds it or waits for it: once the frames and the
// declared placement are done, no robot has one, so the locks do not grow with the robots seen.
func TestRobotLocks_ARobotsLockIsRemovedOnceNoWriterHoldsIt(t *testing.T) {
	scheme := lockTestScheme(t)
	declaredOnly := map[string]bool{"d": true}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(zoneObj("a", ""), zoneObj("b", ""), zoneObj("c", ""), zoneObj("d", ""),
			robotAt("rk", "rid-k", "a"), robotAt("rj52", "rid-j", "a"), declaredAt("rd", "rid-d", "d", "", false)).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		Build()
	z := &ZoneController{Client: api, APIReader: api, Scheme: scheme, Recorder: record.NewFakeRecorder(64)}
	z.setZoneIndex(actionNS, threeSquares(), declaredOnly)
	var wg sync.WaitGroup
	for _, id := range []string{"rid-k", "rid-j", "rid-k", "rid-j"} {
		for _, x := range []float64{5, 45, 25} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f := frameAt(x)
				f.RobotID = id
				z.ObservePosition(context.Background(), f)
			}()
		}
	}
	wg.Wait()
	if err := z.placeDeclaredMembers(context.Background(), declaredZone("d"), declaredOnly); err != nil {
		t.Fatal(err)
	}
	z.robotMu.mu.Lock()
	n := len(z.robotMu.locks)
	z.robotMu.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d robots keep a lock with no writer holding it", n)
	}
}
