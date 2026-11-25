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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/kubeclientmetrics"
	"github.com/swarmada/swarmada/internal/zone"
)

// The zone controller reads the API server past its cache only where a currentZone write may
// follow: a position frame that moves nothing, and a robot a declared zone has nothing to write
// for, are decided on the cache. Where it does write, the stop hold still decides on the zone
// the API server records (the stop guard, estopscope), so a cache behind a zone change cannot
// release or misplace a held robot.

// zoneReadCounter counts the reads made through it, by verb and by the list's type.
type zoneReadCounter struct {
	r     client.Reader
	mu    sync.Mutex
	gets  int
	lists map[string]int
}

func (c *zoneReadCounter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.r.Get(ctx, key, obj, opts...)
}

func (c *zoneReadCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.mu.Lock()
	if c.lists == nil {
		c.lists = map[string]int{}
	}
	switch list.(type) {
	case *fleetv1.FleetZoneList:
		c.lists["FleetZoneList"]++
	case *fleetv1.SwarmadaConfigList:
		c.lists["SwarmadaConfigList"]++
	default:
		c.lists["other"]++
	}
	c.mu.Unlock()
	return c.r.List(ctx, list, opts...)
}

func (c *zoneReadCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets, c.lists = 0, nil
}

func (c *zoneReadCounter) counts() (gets, lists int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.lists {
		lists += n
	}
	return c.gets, lists
}

// zoneReads is a Zone Controller whose cache and API server are separate fakes, so a test can
// put the cache behind. As in the manager, the controller's reads (Get, List) come from the
// cache and its writes, like its reads past the cache, go to the API server; currentZoneWrites
// counts the API server's status writes that carry a currentZone.
type zoneReads struct {
	z                 *ZoneController
	cache, api        client.Client
	reads             *zoneReadCounter
	currentZoneWrites atomic.Int64
}

// cachedClient reads from the cache and writes to the API server.
type cachedClient struct {
	client.Client // the API server: writes, Status(), SubResource()
	cache         client.Reader
}

func (c cachedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.cache.Get(ctx, key, obj, opts...)
}

func (c cachedClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.cache.List(ctx, list, opts...)
}

// newZoneReads builds the controller. sameStore makes the cache the API server's own fake, as
// when the cache is current; otherwise cacheObjs seeds a second fake for the cache. Zones a =
// [0,10], b = [20,30] and c = [40,50] (x, with y in [0,10], floor 0) and d (declared only)
// exist in both; b carries a stop that has been issued and not yet applied.
func newZoneReads(t *testing.T, cacheObjs, apiObjs []client.Object, sameStore bool) *zoneReads {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	zones := func() []client.Object {
		b := zoneObj("b", "")
		b.Annotations = map[string]string{annEstopTriggered: "now"}
		return []client.Object{zoneObj("a", ""), b, zoneObj("c", ""), zoneObj("d", "")}
	}
	if sameStore {
		apiObjs = cacheObjs
	}
	zr := &zoneReads{}
	zr.api = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(zones(), apiObjs...)...).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if data, _ := patch.Data(obj); bytes.Contains(data, []byte(`"currentZone"`)) {
					zr.currentZoneWrites.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	c := zr.api
	zr.cache = zr.api
	if !sameStore {
		zr.cache = fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(append(zones(), cacheObjs...)...).
			WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
			Build()
		c = cachedClient{Client: zr.api, cache: zr.cache}
	}
	zr.reads = &zoneReadCounter{r: zr.api}
	zr.z = &ZoneController{Client: c, APIReader: zr.reads, Scheme: scheme, Recorder: record.NewFakeRecorder(64)}
	zr.z.setZones(actionNS, threeSquares())
	return zr
}

func threeSquares() []zone.Candidate {
	square := func(x0 float64) []zone.Point {
		return []zone.Point{{X: x0, Y: 0}, {X: x0 + 10, Y: 0}, {X: x0 + 10, Y: 10}, {X: x0, Y: 10}}
	}
	return []zone.Candidate{
		{Name: "a", Floor: 0, Polygon: square(0), IsLeaf: true},
		{Name: "b", Floor: 0, Polygon: square(20), IsLeaf: true},
		{Name: "c", Floor: 0, Polygon: square(40), IsLeaf: true},
	}
}

func (zr *zoneReads) frame(robotID string, x float64) {
	f := frameAt(x)
	f.RobotID = robotID
	zr.z.ObservePosition(context.Background(), f)
}

func (zr *zoneReads) recorded(t *testing.T, store client.Client, name string) string {
	t.Helper()
	rb := &fleetv1.Robot{}
	if err := store.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: name}, rb); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return rb.Status.CurrentZone
}

func declaredAt(name, robotID, specZone, currentZone string, matches bool) *fleetv1.Robot {
	r := robotAt(name, robotID, currentZone)
	r.Spec.Zone = specZone
	r.Status.SpecZoneMatchesCurrent = &matches
	return r
}

// ── position frames ──────────────────────────────────────────────────────────

// RA-1 at the API: a frame that moves nothing reads nothing past the cache. The first frame
// after the robot is first seen reads it once (its memos are seeded from what is recorded).
func TestZoneObserver_AFrameThatMovesNothingReadsNothingPastTheCache(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "a")}, nil, true)

	zr.frame("rid-k", 5)
	if gets, _ := zr.reads.counts(); gets != 1 {
		t.Fatalf("the first frame read the robot %d times past the cache, want 1", gets)
	}
	zr.reads.reset()
	writes := zr.currentZoneWrites.Load()
	for i := 0; i < 5; i++ {
		zr.frame("rid-k", 5+float64(i)*0.1)
	}
	if gets, lists := zr.reads.counts(); gets != 0 || lists != 0 {
		t.Fatalf("five frames inside zone a read %d robots and %d lists past the cache, want none", gets, lists)
	}
	if n := zr.currentZoneWrites.Load() - writes; n != 0 {
		t.Fatalf("five frames inside zone a wrote currentZone %d times", n)
	}
	if !zr.z.PositionObserved(actionNS, "rid-k") {
		t.Fatal("a frame decided on the cache did not mark the robot positioned")
	}
}

// A frame that moves the robot reads it past the cache, and reads the stops its move is checked
// against (estopscope.ReadWorld: the zones and the SwarmadaConfigs), then writes the move.
func TestZoneObserver_AMovingFrameReadsTheRobotAndTheStops(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "a")}, nil, true)
	zr.frame("rid-k", 5)
	zr.reads.reset()

	zr.frame("rid-k", 45) // a → c; neither carries a stop
	gets, _ := zr.reads.counts()
	if gets != 1 || zr.reads.lists["FleetZoneList"] != 1 || zr.reads.lists["SwarmadaConfigList"] != 1 {
		t.Fatalf("a move read %d robots and lists %v past the cache, want 1 robot and one list of each",
			gets, zr.reads.lists)
	}
	if got := zr.recorded(t, zr.api, "rk"); got != "c" {
		t.Fatalf("currentZone = %q after the move, want c", got)
	}
}

// The item's test: the hold decides on the zone the API server records. The cache still shows
// the robot in c; the API server records it in b, under a stop issued and not yet applied. A
// frame in a derives a move out of b, which the hold keeps until the stop is applied. Decided
// on the cache, the move would be c → a, which no stop holds, and would be written.
func TestZoneObserver_TheHoldDecidesOnTheZoneTheAPIServerRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		memo string // "" = first sight; otherwise the zone this process last wrote
	}{
		{name: "first sight"},
		{name: "after a write of its own", memo: "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zr := newZoneReads(t,
				[]client.Object{robotAt("rk", "rid-k", "c")},
				[]client.Object{robotAt("rk", "rid-k", "b")}, false)
			if tc.memo != "" {
				key := zoneMemoKey(actionNS, "rid-k")
				zr.z.recordZone(key, tc.memo, tc.memo)
				zr.z.recordContainment(key, []string{tc.memo}, nil)
			}
			zr.frame("rid-k", 5)
			if n := zr.currentZoneWrites.Load(); n != 0 {
				t.Fatalf("a move out of a zone whose stop is being applied was written (%d writes); "+
					"the hold decided on the cache's zone, not the API server's", n)
			}
			if gets, _ := zr.reads.counts(); gets == 0 {
				t.Fatal("the frame was decided without reading the robot past the cache")
			}
		})
	}
}

// A manager restart, or a leadership change, starts with no memos: every robot's first frame
// is read past the cache, even one that moves nothing, and writes nothing. The frames after it
// read nothing.
func TestZoneObserver_AfterARestartEveryRobotIsReadPastTheCacheOnce(t *testing.T) {
	robots := []client.Object{robotAt("r1", "rid-1", ""), robotAt("r2", "rid-2", ""), robotAt("r3", "rid-3", "")}
	zr := newZoneReads(t, robots, nil, true)
	xs := map[string]float64{"rid-1": 5, "rid-2": 25, "rid-3": 45}
	for id, x := range xs {
		zr.frame(id, x)
	}

	restarted := &ZoneController{Client: zr.z.Client, APIReader: zr.reads, Scheme: zr.z.Scheme,
		Recorder: record.NewFakeRecorder(64)}
	restarted.setZones(actionNS, threeSquares())
	zr.z = restarted
	zr.reads.reset()
	writes := zr.currentZoneWrites.Load()
	for id, x := range xs {
		zr.frame(id, x)
	}
	if gets, _ := zr.reads.counts(); gets != len(xs) {
		t.Fatalf("after a restart the first frames read %d robots past the cache, want %d (one each)", gets, len(xs))
	}
	if n := zr.currentZoneWrites.Load() - writes; n != 0 {
		t.Fatalf("after a restart, frames deriving what is recorded wrote currentZone %d times", n)
	}
	zr.reads.reset()
	for id, x := range xs {
		zr.frame(id, x)
	}
	if gets, _ := zr.reads.counts(); gets != 0 {
		t.Fatalf("the second frames after a restart read %d robots past the cache, want 0", gets)
	}
}

// A currentZone edited by someone other than this controller is written back by the robot's
// next frame: the edit changes the robot's version, so the frame reads it past the cache, and
// the memos, which disagree with what is recorded, give way to it. A write of the controller's
// own leaves the next frame decided on the cache.
func TestZoneObserver_ACurrentZoneEditedBySomeoneElseIsWrittenBack(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "")}, nil, true)
	ctx := context.Background()
	zr.frame("rid-k", 5) // writes a
	zr.reads.reset()
	zr.frame("rid-k", 5)
	if gets, _ := zr.reads.counts(); gets != 0 {
		t.Fatalf("the frame after the controller's own write read %d robots past the cache, want 0", gets)
	}

	edited := &fleetv1.Robot{}
	if err := zr.api.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, edited); err != nil {
		t.Fatal(err)
	}
	edited.Status.CurrentZone = "c"
	if err := zr.api.Status().Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	zr.frame("rid-k", 5)
	if got := zr.recorded(t, zr.api, "rk"); got != "a" {
		t.Fatalf("currentZone = %q after a frame in a; an edit by someone else was left standing", got)
	}
}

// containingZones edited by someone else is written back too.
func TestZoneObserver_ContainingZonesEditedBySomeoneElseIsWrittenBack(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "")}, nil, true)
	ctx := context.Background()
	zr.frame("rid-k", 5)
	edited := &fleetv1.Robot{}
	if err := zr.api.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, edited); err != nil {
		t.Fatal(err)
	}
	edited.Status.ContainingZones = []string{"c"}
	if err := zr.api.Status().Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	zr.frame("rid-k", 5)
	got := &fleetv1.Robot{}
	if err := zr.api.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, got); err != nil {
		t.Fatal(err)
	}
	if !sameZoneSet(got.Status.ContainingZones, []string{"a"}) {
		t.Fatalf("containingZones = %v after a frame in a; an edit by someone else was left standing",
			got.Status.ContainingZones)
	}
}

// Memos that disagree with what is recorded never decide a frame on the cache. Here a race left
// them at a while the API server records the robot in b, under a stop issued and not yet
// applied. The frame in a is read past the cache, the memos give way to the record, and the move
// out of b is held. Once the stop is cleared, the next frame writes a: the robot is not left
// recorded in b, where a stop of a would never find it.
func TestZoneObserver_MemosThatDisagreeWithTheRecordAreCorrected(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "b")}, nil, true)
	ctx := context.Background()
	cached := &fleetv1.Robot{}
	if err := zr.api.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, cached); err != nil {
		t.Fatal(err)
	}
	key := zoneMemoKey(actionNS, "rid-k")
	zr.z.recordZone(key, "a", "a")
	zr.z.recordContainment(key, []string{"a"}, nil)
	zr.z.noteRead(key, cached.ResourceVersion) // the worst case: even the version agrees

	zr.frame("rid-k", 5)
	if n := zr.currentZoneWrites.Load(); n != 0 {
		t.Fatalf("a move out of b, whose stop is being applied, was written (%d writes)", n)
	}
	if gets, _ := zr.reads.counts(); gets == 0 {
		t.Fatal("memos that disagree with the record decided the frame on the cache")
	}

	b := &fleetv1.FleetZone{}
	if err := zr.api.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "b"}, b); err != nil {
		t.Fatal(err)
	}
	delete(b.Annotations, annEstopTriggered)
	if err := zr.api.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	zr.frame("rid-k", 5)
	if got := zr.recorded(t, zr.api, "rk"); got != "a" {
		t.Fatalf("currentZone = %q once b's stop is cleared, want a", got)
	}
}

// A write by anyone else to the robot that leaves its zone fields as they are (here its phase)
// costs its frames no read past the cache: they move nothing, and are decided on the cache.
func TestZoneObserver_AnotherWriteToTheRobotCostsNoRead(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "a")}, nil, true)
	ctx := context.Background()
	zr.frame("rid-k", 5)
	rb := &fleetv1.Robot{}
	if err := zr.api.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, rb); err != nil {
		t.Fatal(err)
	}
	rb.Status.Phase = fleetv1.RobotPhaseIdle
	if err := zr.api.Status().Update(ctx, rb); err != nil {
		t.Fatal(err)
	}
	zr.reads.reset()
	writes := zr.currentZoneWrites.Load()
	for i := 0; i < 3; i++ {
		zr.frame("rid-k", 5)
	}
	if gets, _ := zr.reads.counts(); gets != 0 {
		t.Fatalf("three frames after a phase write read %d robots past the cache, want none", gets)
	}
	if n := zr.currentZoneWrites.Load() - writes; n != 0 {
		t.Fatalf("frames that move nothing wrote currentZone %d times", n)
	}
}

// recordsHold compares the containment set as well as the zone: a frame whose containment set
// moves while its zone does not is not decided on the cache. It does not compare the cached
// copy's version, and a memo a move has advanced decides nothing on the cache until the move's
// write returns.
func TestRecordsHold_ComparesTheContainmentSet(t *testing.T) {
	z := &ZoneController{}
	key := zoneMemoKey(actionNS, "rid-k")
	z.recordZone(key, "a", "a")
	z.recordContainment(key, []string{"a"}, nil)
	z.noteRead(key, "7")
	robot := robotAt("rk", "rid-k", "a")
	robot.ResourceVersion = "7"
	robot.Status.ContainingZones = []string{"a"}
	if !z.recordsHold(key, robot, "a", []string{"a"}) {
		t.Fatal("an unchanged frame on the version last read does not hold")
	}
	if z.recordsHold(key, robot, "a", []string{"a", "big"}) {
		t.Fatal("a frame whose containment set moved was decided on the cache")
	}
	if z.recordsHold(key, robot, "b", []string{"a"}) {
		t.Fatal("a frame whose zone moved was decided on the cache")
	}
	other := robot.DeepCopy()
	other.ResourceVersion = "8"
	if !z.recordsHold(key, other, "a", []string{"a"}) {
		t.Fatal("a cached copy newer than the version last read, recording what the memos hold, does not hold")
	}
	edited := robot.DeepCopy()
	edited.Status.CurrentZone = "c"
	if z.recordsHold(key, edited, "a", []string{"a"}) {
		t.Fatal("a cached copy recording another zone than the memos was decided on the cache")
	}
	otherSet := robot.DeepCopy()
	otherSet.Status.ContainingZones = []string{"a", "big"}
	if z.recordsHold(key, otherSet, "a", []string{"a"}) {
		t.Fatal("a cached copy recording another containment set than the memos was decided on the cache")
	}

	// A move advances the zone memo before its write: a cached copy that already shows the new
	// zone (a cache behind an earlier write of it) decides nothing until the write returns.
	if !z.recordZone(key, "b", "a") {
		t.Fatal("the move a → b did not advance the zone memo")
	}
	inB := robot.DeepCopy()
	inB.Status.CurrentZone = "b"
	if z.recordsHold(key, inB, "b", []string{"a"}) {
		t.Fatal("a zone memo ahead of what is recorded, its write in flight, decided a frame on the cache")
	}
	z.noteRead(key, "9")
	if !z.recordsHold(key, inB, "b", []string{"a"}) {
		t.Fatal("once the move's write returned, an unchanged frame does not hold")
	}
	// The same for the containment memo.
	if !z.recordContainment(key, []string{"a", "big"}, []string{"a"}) {
		t.Fatal("the containment move did not advance the set memo")
	}
	inBig := inB.DeepCopy()
	inBig.Status.ContainingZones = []string{"a", "big"}
	if z.recordsHold(key, inBig, "b", []string{"a", "big"}) {
		t.Fatal("a set memo ahead of what is recorded, its write in flight, decided a frame on the cache")
	}
	z.noteRead(key, "10")
	if !z.recordsHold(key, inBig, "b", []string{"a", "big"}) {
		t.Fatal("once the containment write returned, an unchanged frame does not hold")
	}
}

// ── declared zones (placeDeclaredMembers) ────────────────────────────────────

func declaredZone(name string) *fleetv1.FleetZone { return zoneObj(name, "") }

// A declared zone's reconcile reads past the cache only the robots it may write.
func TestZonePlacement_ReadsOnlyTheRobotsItMayWrite(t *testing.T) {
	objs := []client.Object{
		declaredAt("p1", "rid-1", "d", "d", true),
		declaredAt("p2", "rid-2", "d", "d", true),
		declaredAt("p3", "rid-3", "d", "d", true),
		declaredAt("o1", "rid-4", "a", "a", true), // another zone's robot
		declaredAt("u1", "rid-5", "d", "", false), // declared in d, not yet placed
	}
	zr := newZoneReads(t, objs, nil, true)
	ctx := context.Background()
	declaredOnly := map[string]bool{"d": true}

	if err := zr.z.placeDeclaredMembers(ctx, declaredZone("d"), declaredOnly); err != nil {
		t.Fatal(err)
	}
	if gets, _ := zr.reads.counts(); gets != 1 {
		t.Fatalf("the reconcile read %d robots past the cache, want 1 (the one it places)", gets)
	}
	if got := zr.recorded(t, zr.api, "u1"); got != "d" {
		t.Fatalf("u1 currentZone = %q, want d", got)
	}
	zr.reads.reset()
	if err := zr.z.placeDeclaredMembers(ctx, declaredZone("d"), declaredOnly); err != nil {
		t.Fatal(err)
	}
	if gets, _ := zr.reads.counts(); gets != 0 {
		t.Fatalf("a reconcile with every robot placed read %d robots past the cache, want 0", gets)
	}
}

// The item's test, for a declared zone: the cache shows the robot in c, the API server records
// it in b, whose stop is being applied. Placing it in its declared zone d is a move out of b,
// held until the stop is applied. Decided on the cache it would be c → d and written.
func TestZonePlacement_TheHoldDecidesOnTheZoneTheAPIServerRecords(t *testing.T) {
	zr := newZoneReads(t,
		[]client.Object{declaredAt("rk", "rid-k", "d", "c", false)},
		[]client.Object{declaredAt("rk", "rid-k", "d", "b", false)}, false)
	if err := zr.z.placeDeclaredMembers(context.Background(), declaredZone("d"), map[string]bool{"d": true}); err != nil {
		t.Fatal(err)
	}
	if n := zr.currentZoneWrites.Load(); n != 0 {
		t.Fatalf("a placement out of a zone whose stop is being applied was written (%d writes)", n)
	}
}

// A cache behind a spec.zone change delays the placement and makes no wrong one: the robot is
// skipped while the cache shows its old declaration, and placed once the cache catches up.
func TestZonePlacement_ACacheBehindASpecZoneChangePlacesTheRobotOnceItCatchesUp(t *testing.T) {
	zr := newZoneReads(t,
		[]client.Object{declaredAt("rk", "rid-k", "a", "a", true)},
		[]client.Object{declaredAt("rk", "rid-k", "d", "a", true)}, false)
	ctx := context.Background()
	declaredOnly := map[string]bool{"d": true}
	if err := zr.z.placeDeclaredMembers(ctx, declaredZone("d"), declaredOnly); err != nil {
		t.Fatal(err)
	}
	if gets, _ := zr.reads.counts(); gets != 0 || zr.currentZoneWrites.Load() != 0 {
		t.Fatalf("a robot the cache shows declared elsewhere was read (%d) or written (%d)",
			gets, zr.currentZoneWrites.Load())
	}

	caught := &fleetv1.Robot{}
	if err := zr.cache.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, caught); err != nil {
		t.Fatal(err)
	}
	caught.Spec.Zone = "d"
	if err := zr.cache.Update(ctx, caught); err != nil {
		t.Fatal(err)
	}
	if err := zr.z.placeDeclaredMembers(ctx, declaredZone("d"), declaredOnly); err != nil {
		t.Fatal(err)
	}
	if got := zr.recorded(t, zr.api, "rk"); got != "d" {
		t.Fatalf("currentZone = %q once the cache caught up, want d", got)
	}
}

// specZoneMatchesCurrent is read by placeDeclaredMembers' decision on the cache, so a change to
// it alone enqueues the zones; other status writes still do not.
func TestRobotZoneChanged_AdmitsASpecZoneMatchesCurrentChange(t *testing.T) {
	yes, no := true, false
	old := declaredAt("rk", "rid-k", "d", "d", true)
	flipped := old.DeepCopy()
	flipped.Status.SpecZoneMatchesCurrent = &no
	if !robotZoneChanged.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: flipped}) {
		t.Fatal("a specZoneMatchesCurrent change alone did not enqueue the zones")
	}
	cleared := old.DeepCopy()
	cleared.Status.SpecZoneMatchesCurrent = nil
	if !robotZoneChanged.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: cleared}) {
		t.Fatal("specZoneMatchesCurrent cleared did not enqueue the zones")
	}
	same := old.DeepCopy()
	same.Status.SpecZoneMatchesCurrent = &yes
	same.Status.Phase = fleetv1.RobotPhaseIdle
	if robotZoneChanged.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: same}) {
		t.Fatal("a status write that moves no zone field enqueued the zones")
	}
}

// ── attribution ──────────────────────────────────────────────────────────────

// Each of the zone controller's uncached reads is recorded under its caller, so a measurement
// can say what each costs and what a change to one saved.
func TestZoneReads_AreRecordedByCaller(t *testing.T) {
	sample := func(caller, verb, resource string) uint64 {
		h, ok := kubeclientmetrics.UncachedReadDuration.WithLabelValues(caller, verb, resource, "ok").(prometheus.Histogram)
		if !ok {
			t.Fatal("UncachedReadDuration is not a histogram")
		}
		m := &dto.Metric{}
		if err := h.Write(m); err != nil {
			t.Fatal(err)
		}
		return m.GetHistogram().GetSampleCount()
	}
	frameGets := sample(readerPositionFrame, "get", "Robot")
	holdZones := sample(readerZoneHold, "list", "FleetZoneList")
	holdConfigs := sample(readerZoneHold, "list", "SwarmadaConfigList")
	placementGets := sample(readerZonePlacement, "get", "Robot")

	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "a"), declaredAt("u1", "rid-5", "d", "", false)}, nil, true)
	zr.frame("rid-k", 5)  // first sight: one robot read
	zr.frame("rid-k", 45) // a move: one robot read and the stops
	if err := zr.z.placeDeclaredMembers(context.Background(), declaredZone("d"), map[string]bool{"d": true}); err != nil {
		t.Fatal(err)
	}

	if d := sample(readerPositionFrame, "get", "Robot") - frameGets; d != 2 {
		t.Errorf("position_frame recorded %d Robot gets, want 2", d)
	}
	if d := sample(readerZoneHold, "list", "FleetZoneList") - holdZones; d != 1 {
		t.Errorf("zone_hold recorded %d FleetZone lists, want 1", d)
	}
	if d := sample(readerZoneHold, "list", "SwarmadaConfigList") - holdConfigs; d != 1 {
		t.Errorf("zone_hold recorded %d SwarmadaConfig lists, want 1", d)
	}
	if d := sample(readerZonePlacement, "get", "Robot") - placementGets; d != 1 {
		t.Errorf("zone_placement recorded %d Robot gets, want 1", d)
	}
}

// The write is decided again on the API server's copy. The cache shows the robot declared in d
// and unplaced; the API server shows it re-homed to a and recorded there. Written on the
// cache's decision, its currentZone would be cleared out of a zone it is in.
func TestZonePlacement_ARobotTheAPIServerShowsDeclaredElsewhereIsNotWritten(t *testing.T) {
	zr := newZoneReads(t,
		[]client.Object{declaredAt("rk", "rid-k", "d", "", false)},
		[]client.Object{declaredAt("rk", "rid-k", "a", "a", true)}, false)
	if err := zr.z.placeDeclaredMembers(context.Background(), declaredZone("d"), map[string]bool{"d": true}); err != nil {
		t.Fatal(err)
	}
	if n := zr.currentZoneWrites.Load(); n != 0 {
		t.Fatalf("a robot the API server records in a, declared there, was written %d times by d's reconcile", n)
	}
	if got := zr.recorded(t, zr.api, "rk"); got != "a" {
		t.Fatalf("currentZone = %q, want a", got)
	}
}

// Every frame carrying a position marks the robot positioned (ZonePositionOverdue reads it),
// the frames decided on the cache included.
func TestZoneObserver_AFrameDecidedOnTheCacheMarksTheRobotPositioned(t *testing.T) {
	zr := newZoneReads(t, []client.Object{robotAt("rk", "rid-k", "a")}, nil, true)
	key := zoneMemoKey(actionNS, "rid-k")
	zr.z.recordZone(key, "a", "a")
	zr.z.recordContainment(key, []string{"a"}, nil)
	cached := &fleetv1.Robot{}
	if err := zr.api.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: "rk"}, cached); err != nil {
		t.Fatal(err)
	}
	cached.Status.ContainingZones = []string{"a"}
	if err := zr.api.Status().Update(context.Background(), cached); err != nil {
		t.Fatal(err)
	}
	zr.z.noteRead(key, cached.ResourceVersion)
	zr.frame("rid-k", 5)
	if gets, _ := zr.reads.counts(); gets != 0 {
		t.Fatalf("the frame was read past the cache (%d); the test needs the cache-decided path", gets)
	}
	if !zr.z.PositionObserved(actionNS, "rid-k") {
		t.Fatal("a frame decided on the cache did not mark the robot positioned")
	}
}

// ── concurrency ──────────────────────────────────────────────────────────────

// transitionCounter counts the zone entries signalled to the TDE.
type transitionCounter struct {
	mu      sync.Mutex
	entries []string
}

func (c *transitionCounter) OnRobotEnteredZone(_ context.Context, _, zone, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, zone)
	return nil
}

func (c *transitionCounter) OnRobotExitedZone(context.Context, string, string, string) error {
	return nil
}

func (c *transitionCounter) count() (int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), append([]string(nil), c.entries...)
}

// serializedStatusWrites makes a status patch's read, compare and update one step, as the API
// server's is; the fake tracker's are separate, and without this even the code before the
// cache-first reads double-signals under concurrency.
func serializedStatusWrites() interceptor.Funcs {
	var mu sync.Mutex
	return interceptor.Funcs{SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
		obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
	) error {
		mu.Lock()
		defer mu.Unlock()
		return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
	}}
}

func racingZoneController(t *testing.T, robot *fleetv1.Robot, declaredOnly map[string]bool) (*ZoneController, client.Client, *transitionCounter) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(zoneObj("a", ""), zoneObj("b", ""), zoneObj("c", ""), zoneObj("d", ""), robot).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(serializedStatusWrites()).Build()
	tde := &transitionCounter{}
	z := &ZoneController{Client: c, APIReader: c, Scheme: scheme, Recorder: record.NewFakeRecorder(1024), TDE: tde}
	z.setZoneIndex(actionNS, threeSquares(), declaredOnly)
	return z, c, tde
}

// Concurrent frames for one robot, and frames racing a declared zone's placement, record each move
// once and signal it once. Since the fresh path lets memos that disagree with the record give way,
// the status write's optimistic lock is what keeps a second writer from repeating a transition.
func TestZoneObserver_ConcurrentWritersSignalEachMoveOnce(t *testing.T) {
	ctx := context.Background()
	for iter := 0; iter < 100; iter++ {
		t.Run(fmt.Sprintf("frames-%d", iter), func(t *testing.T) {
			z, c, tde := racingZoneController(t, robotAt("rk", "rid-k", "a"), nil)
			z.ObservePosition(ctx, frameAt(5)) // seeds the memos in a
			var wg sync.WaitGroup
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func() { defer wg.Done(); z.ObservePosition(ctx, frameAt(45)) }()
			}
			wg.Wait()
			if n, zones := tde.count(); n != 1 {
				t.Fatalf("four frames moving the robot to c signalled %d entries %v, want 1", n, zones)
			}
			rb := &fleetv1.Robot{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, rb); err != nil {
				t.Fatal(err)
			}
			if rb.Status.CurrentZone != "c" {
				t.Fatalf("currentZone = %q, want c", rb.Status.CurrentZone)
			}
		})
		t.Run(fmt.Sprintf("placement-%d", iter), func(t *testing.T) {
			declaredOnly := map[string]bool{"d": true}
			z, c, tde := racingZoneController(t, declaredAt("rk", "rid-k", "d", "a", false), declaredOnly)
			var wg sync.WaitGroup
			for g := 0; g < 3; g++ {
				wg.Add(1)
				go func() { defer wg.Done(); z.ObservePosition(ctx, frameAt(5)) }()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A write refused on a conflict fails the reconcile, which is requeued: run it again.
				for attempt := 0; attempt < 5; attempt++ {
					err := z.placeDeclaredMembers(ctx, zoneObj("d", ""), declaredOnly)
					if err == nil {
						return
					}
					if !apierrors.IsConflict(err) { // IsConflict unwraps
						t.Error(err)
						return
					}
				}
				t.Error("the placement was refused on a conflict at every attempt")
			}()
			wg.Wait()
			rb := &fleetv1.Robot{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: actionNS, Name: "rk"}, rb); err != nil {
				t.Fatal(err)
			}
			if rb.Status.CurrentZone != "d" {
				t.Fatalf("currentZone = %q, want d", rb.Status.CurrentZone)
			}
			if n, zones := tde.count(); n != 1 {
				t.Fatalf("frames racing d's placement signalled %d entries %v, want 1", n, zones)
			}
		})
	}
}
