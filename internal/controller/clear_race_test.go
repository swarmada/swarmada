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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/safety"
)

// A clear releases a robot only from stops that have ended when the clear writes (RFC-0001
// #safety-robot-stop-table, row S5: "no other stop in force; no stop issued since the clear's
// read"). A clear pass reads the zones and the namespace's SwarmadaConfig once, at its start, and
// clears its robots one at a time. A stop another carrier issues to one of those robots after
// that read must still hold the robot when the clear reaches it, whether it is recorded in
// status.stops only or in status.estopHeldBy as well.

const clearRaceNS = "acme"

// clearRaceFixture is robot amr-1, Stopped, and the namespace's SwarmadaConfig carrying a
// stop's trigger. The robot records the stops given; the config is the one on the API server.
func clearRaceFixture(t *testing.T, stops []fleetv1.RobotStop, held []fleetv1.EstopHold, robotAnn map[string]string,
	funcs *interceptor.Funcs) (client.Client, *fleetv1.FleetZone, *fleetv1.SwarmadaConfig) {
	t.Helper()
	ctx := context.Background()
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-1", Namespace: clearRaceNS, UID: "zone-uid"}}
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: clearRaceNS,
		UID: "cfg-uid", Annotations: map[string]string{estopscope.TriggerAnnotation: "ns-stop"}}}
	ann := map[string]string{RobotIDAnnotation: "amr-1"}
	for k, v := range robotAnn {
		ann[k] = v
	}
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: clearRaceNS, Annotations: ann},
		Spec:       fleetv1.RobotSpec{Zone: "bay-1", Adapter: fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"}},
	}
	b := fake.NewClientBuilder().WithScheme(livenessScheme(t)).WithObjects(zone, cfg, robot).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.FleetZone{}, &fleetv1.SwarmadaConfig{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	if err := c.Get(ctx, client.ObjectKeyFromObject(robot), robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	robot.Status.Stops = stops
	robot.Status.EstopHeldBy = held
	if err := c.Status().Update(ctx, robot); err != nil {
		t.Fatal(err)
	}
	return c, zone, cfg
}

func clearRaceStop(id string, scope fleetv1.StopScope, kind fleetv1.StopCarrierKind, name, uid string) fleetv1.RobotStop {
	return fleetv1.RobotStop{ID: id, Scope: scope, SentAt: metav1.Now(),
		Carrier: &fleetv1.StopCarrier{Kind: kind, Name: name, UID: uid}}
}

func clearRaceRobot(t *testing.T, c client.Client) *fleetv1.Robot {
	t.Helper()
	r := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: clearRaceNS, Name: "amr-1"}, r); err != nil {
		t.Fatal(err)
	}
	return r
}

// assertNamespaceStopStands fails when the clear released amr-1 from the namespace's stop: the
// robot moved out of Stopped, or that stop was marked cleared.
func assertNamespaceStopStands(t *testing.T, c client.Client) {
	t.Helper()
	r := clearRaceRobot(t, c)
	if r.Status.EstopState != fleetv1.RobotEstopStopped {
		t.Errorf("estopState = %q, want Stopped: the namespace's stop was in force when the clear reached the robot",
			r.Status.EstopState)
	}
	for _, st := range r.Status.Stops {
		if st.ID == "stop-ns" && st.ClearedAt != nil {
			t.Errorf("the namespace's stop %s was marked cleared at %v by another carrier's clear", st.ID, st.ClearedAt)
		}
	}
}

// A zone's clear judges the namespace's stop on the configs it read at its start, before that
// stop was triggered. The namespace's stop reached the robot since and is recorded in
// status.stops; its hold record has not landed yet.
func TestClear_ZoneClearKeepsANamespaceStopIssuedDuringIt(t *testing.T) {
	ctx := context.Background()
	stops := []fleetv1.RobotStop{
		clearRaceStop("stop-zone", fleetv1.StopScopeZone, fleetv1.StopCarrierFleetZone, "bay-1", "zone-uid"),
		clearRaceStop("stop-ns", fleetv1.StopScopeNamespace, fleetv1.StopCarrierSwarmadaConfig, SwarmadaConfigName, "cfg-uid"),
	}
	held := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-1", UID: "zone-uid"}}
	c, zone, cfg := clearRaceFixture(t, stops, held, nil, nil)

	// What the pass read at its start: the namespace's stop not yet triggered.
	stale := cfg.DeepCopy()
	delete(stale.Annotations, estopscope.TriggerAnnotation)

	d := safety.New(c, nil)
	robots := []fleetv1.Robot{*clearRaceRobot(t, c)}
	if _, _, err := clearCarrier(ctx, c, c, d, nil, clearRaceNS, zoneHold(zone), robots,
		[]fleetv1.FleetZone{*zone}, []fleetv1.SwarmadaConfig{*stale}, "alice", "zone bay-1"); err != nil {
		t.Fatalf("clearCarrier: %v", err)
	}
	assertNamespaceStopStands(t, c)
}

// A robot's own clear, the same race through status.estopHeldBy: the namespace's stop reached
// the robot after the clear's pass read the configs, and its record has landed on the robot. The
// clear must neither move the robot nor drop that record.
func TestClear_RobotClearKeepsANamespaceStopRecordedDuringIt(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	configLists := 0
	funcs := interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := cl.List(ctx, list, opts...); err != nil {
			return err
		}
		l, ok := list.(*fleetv1.SwarmadaConfigList)
		if !ok {
			return nil
		}
		mu.Lock()
		configLists++
		first := configLists == 1
		mu.Unlock()
		if first {
			// The pass's own read, at its start: the namespace's stop not triggered yet.
			for i := range l.Items {
				delete(l.Items[i].Annotations, estopscope.TriggerAnnotation)
			}
		}
		return nil
	}}
	stops := []fleetv1.RobotStop{
		clearRaceStop("stop-own", fleetv1.StopScopeRobot, fleetv1.StopCarrierRobot, "amr-1", ""),
		clearRaceStop("stop-ns", fleetv1.StopScopeNamespace, fleetv1.StopCarrierSwarmadaConfig, SwarmadaConfigName, "cfg-uid"),
	}
	held := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldSwarmadaConfig, Name: SwarmadaConfigName, UID: "cfg-uid"}}
	// The robot's own trigger is gone (the operator's clear); its marker remains.
	c, _, _ := clearRaceFixture(t, stops, held, map[string]string{annEstopProcessed: "own-stop"}, &funcs)

	r := &RobotEstopReconciler{Client: c, Scheme: c.Scheme(), Estopper: safety.New(c, nil)}
	robot := clearRaceRobot(t, c)
	if _, err := r.clearRobotEstop(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(robot)}, robot); err != nil {
		t.Fatalf("clearRobotEstop: %v", err)
	}
	assertNamespaceStopStands(t, c)
	want := fleetv1.EstopHold{Kind: fleetv1.EstopHoldSwarmadaConfig, Name: SwarmadaConfigName, UID: "cfg-uid"}
	if got := clearRaceRobot(t, c).Status.EstopHeldBy; len(got) != 1 || got[0] != want {
		t.Errorf("estopHeldBy = %+v, want %+v kept: its stop is in force", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if configLists < 2 {
		t.Fatalf("the race was not exercised: %d SwarmadaConfig list(s), want the pass's stale read and a fresh one",
			configLists)
	}
}

// Without another stop, a zone's clear still releases the robot: the fresh read must not keep a
// robot that nothing holds.
func TestClear_ZoneClearReleasesARobotNoOtherStopHolds(t *testing.T) {
	ctx := context.Background()
	stops := []fleetv1.RobotStop{
		clearRaceStop("stop-zone", fleetv1.StopScopeZone, fleetv1.StopCarrierFleetZone, "bay-1", "zone-uid"),
	}
	held := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-1", UID: "zone-uid"}}
	c, zone, cfg := clearRaceFixture(t, stops, held, nil, nil)
	// The namespace's config carries no stop.
	base := cfg.DeepCopy()
	delete(cfg.Annotations, estopscope.TriggerAnnotation)
	if err := c.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}

	d := safety.New(c, nil)
	robots := []fleetv1.Robot{*clearRaceRobot(t, c)}
	if _, _, err := clearCarrier(ctx, c, c, d, nil, clearRaceNS, zoneHold(zone), robots,
		[]fleetv1.FleetZone{*zone}, []fleetv1.SwarmadaConfig{*cfg}, "alice", "zone bay-1"); err != nil {
		t.Fatalf("clearCarrier: %v", err)
	}
	r := clearRaceRobot(t, c)
	if r.Status.EstopState != fleetv1.RobotEstopResuming {
		t.Errorf("estopState = %q, want Resuming: no other stop holds the robot", r.Status.EstopState)
	}
	if len(r.Status.EstopHeldBy) != 0 {
		t.Errorf("estopHeldBy = %+v, want the zone's record dropped", r.Status.EstopHeldBy)
	}
}

// A robot's own clear releases the record of a stop whose carrier was deleted and recreated under
// another UID: nothing else would end it (estopscope.Holders names it Orphaned). The recreated
// config still carries a processed marker, so a record judged by name alone would read as in force.
func TestClear_RobotClearReleasesARecordOfARecreatedCarrier(t *testing.T) {
	ctx := context.Background()
	stops := []fleetv1.RobotStop{
		clearRaceStop("stop-own", fleetv1.StopScopeRobot, fleetv1.StopCarrierRobot, "amr-1", ""),
	}
	held := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldSwarmadaConfig, Name: SwarmadaConfigName, UID: "cfg-uid-before"}}
	c, _, cfg := clearRaceFixture(t, stops, held, map[string]string{annEstopProcessed: "own-stop"}, nil)
	// The config now in the namespace is another object under the same name: no trigger, and the
	// processed marker of a clear not yet applied.
	base := cfg.DeepCopy()
	delete(cfg.Annotations, estopscope.TriggerAnnotation)
	cfg.Annotations[estopscope.ProcessedAnnotation] = "ns-stop"
	if err := c.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}

	entries := &clearRaceAudit{}
	r := &RobotEstopReconciler{Client: c, Scheme: c.Scheme(), Estopper: safety.New(c, nil), Audit: entries}
	robot := clearRaceRobot(t, c)
	if _, err := r.clearRobotEstop(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(robot)}, robot); err != nil {
		t.Fatalf("clearRobotEstop: %v", err)
	}
	// The audit names the record the clear released.
	if got := entries.detail(audit.EventEstopCleared, "released_orphaned_holds"); got != "namespace/"+SwarmadaConfigName {
		t.Errorf("ESTOP_CLEARED released_orphaned_holds = %q, want %q", got, "namespace/"+SwarmadaConfigName)
	}
	got := clearRaceRobot(t, c)
	if got.Status.EstopState != fleetv1.RobotEstopResuming {
		t.Errorf("estopState = %q, want Resuming: the only other record names a carrier that no longer exists",
			got.Status.EstopState)
	}
	if len(got.Status.EstopHeldBy) != 0 {
		t.Errorf("estopHeldBy = %+v, want the orphaned record released by the robot's own clear", got.Status.EstopHeldBy)
	}
}

// A zone's clear reaches a robot that a parent zone's stop covers through the zone tree. The
// parent was triggered after the pass read the zones, and its fan-out has not reached this robot
// yet: no record of it on the robot, sent or recorded. The robot stays stopped.
func TestClear_ZoneClearKeepsARobotAParentZoneStopCovers(t *testing.T) {
	ctx := context.Background()
	stops := []fleetv1.RobotStop{
		clearRaceStop("stop-zone", fleetv1.StopScopeZone, fleetv1.StopCarrierFleetZone, "bay-1", "zone-uid"),
	}
	held := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-1", UID: "zone-uid"}}
	c, zone, cfg := clearRaceFixture(t, stops, held, nil, nil)
	// No namespace stop: only the parent zone covers the robot.
	cbase := cfg.DeepCopy()
	delete(cfg.Annotations, estopscope.TriggerAnnotation)
	if err := c.Patch(ctx, cfg, client.MergeFrom(cbase)); err != nil {
		t.Fatal(err)
	}
	hall := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "hall", Namespace: clearRaceNS, UID: "hall-uid",
		Annotations: map[string]string{estopscope.TriggerAnnotation: "hall-stop"}}}
	if err := c.Create(ctx, hall); err != nil {
		t.Fatal(err)
	}
	zbase := zone.DeepCopy()
	zone.Spec.ParentZone = "hall"
	if err := c.Patch(ctx, zone, client.MergeFrom(zbase)); err != nil {
		t.Fatal(err)
	}
	// What the pass read at its start: the parent not yet triggered.
	staleHall := hall.DeepCopy()
	delete(staleHall.Annotations, estopscope.TriggerAnnotation)

	d := safety.New(c, nil)
	robots := []fleetv1.Robot{*clearRaceRobot(t, c)}
	if _, _, err := clearCarrier(ctx, c, c, d, nil, clearRaceNS, zoneHold(zone), robots,
		[]fleetv1.FleetZone{*zone, *staleHall}, []fleetv1.SwarmadaConfig{*cfg}, "alice", "zone bay-1"); err != nil {
		t.Fatalf("clearCarrier: %v", err)
	}
	if got := clearRaceRobot(t, c).Status.EstopState; got != fleetv1.RobotEstopStopped {
		t.Errorf("estopState = %q, want Stopped: the parent zone's stop reaches the robot", got)
	}
}

// The namespace's clear, against a zone's stop recorded on the robot after the pass read the zones.
func TestClear_NamespaceClearKeepsAZoneStopIssuedDuringIt(t *testing.T) {
	ctx := context.Background()
	stops := []fleetv1.RobotStop{
		clearRaceStop("stop-ns", fleetv1.StopScopeNamespace, fleetv1.StopCarrierSwarmadaConfig, SwarmadaConfigName, "cfg-uid"),
		clearRaceStop("stop-zone", fleetv1.StopScopeZone, fleetv1.StopCarrierFleetZone, "bay-1", "zone-uid"),
	}
	held := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldSwarmadaConfig, Name: SwarmadaConfigName, UID: "cfg-uid"}}
	c, zone, cfg := clearRaceFixture(t, stops, held, nil, nil)
	// The namespace's stop is being cleared (its trigger removed); the zone's is in force.
	cbase := cfg.DeepCopy()
	delete(cfg.Annotations, estopscope.TriggerAnnotation)
	if err := c.Patch(ctx, cfg, client.MergeFrom(cbase)); err != nil {
		t.Fatal(err)
	}
	zbase := zone.DeepCopy()
	zone.Annotations = map[string]string{estopscope.TriggerAnnotation: "zone-stop"}
	if err := c.Patch(ctx, zone, client.MergeFrom(zbase)); err != nil {
		t.Fatal(err)
	}
	staleZone := zone.DeepCopy()
	delete(staleZone.Annotations, estopscope.TriggerAnnotation)

	d := safety.New(c, nil)
	robots := []fleetv1.Robot{*clearRaceRobot(t, c)}
	if _, _, err := clearCarrier(ctx, c, c, d, nil, clearRaceNS, configHold(cfg), robots,
		[]fleetv1.FleetZone{*staleZone}, []fleetv1.SwarmadaConfig{*cfg}, "alice", "namespace "+clearRaceNS); err != nil {
		t.Fatalf("clearCarrier: %v", err)
	}
	r := clearRaceRobot(t, c)
	if r.Status.EstopState != fleetv1.RobotEstopStopped {
		t.Errorf("estopState = %q, want Stopped: the zone's stop was in force when the clear reached the robot",
			r.Status.EstopState)
	}
	for _, st := range r.Status.Stops {
		if st.ID == "stop-zone" && st.ClearedAt != nil {
			t.Errorf("the zone's stop was marked cleared by the namespace's clear")
		}
	}
}

// A clear that cannot read the carriers after its epoch writes nothing: the robot stays as it was,
// the clear's own record stays, and the error is returned so the pass is retried.
func TestClear_AFailedFreshReadWritesNothing(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	failNext := false
	funcs := interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		mu.Lock()
		fail := failNext
		mu.Unlock()
		if _, zones := list.(*fleetv1.FleetZoneList); zones && fail {
			return context.DeadlineExceeded
		}
		return cl.List(ctx, list, opts...)
	}}
	stops := []fleetv1.RobotStop{
		clearRaceStop("stop-zone", fleetv1.StopScopeZone, fleetv1.StopCarrierFleetZone, "bay-1", "zone-uid"),
	}
	held := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-1", UID: "zone-uid"}}
	c, zone, cfg := clearRaceFixture(t, stops, held, nil, &funcs)
	cbase := cfg.DeepCopy()
	delete(cfg.Annotations, estopscope.TriggerAnnotation)
	if err := c.Patch(ctx, cfg, client.MergeFrom(cbase)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	failNext = true
	mu.Unlock()

	d := safety.New(c, nil)
	robots := []fleetv1.Robot{*clearRaceRobot(t, c)}
	if _, _, err := clearCarrier(ctx, c, c, d, nil, clearRaceNS, zoneHold(zone), robots,
		[]fleetv1.FleetZone{*zone}, []fleetv1.SwarmadaConfig{*cfg}, "alice", "zone bay-1"); err == nil {
		t.Fatal("clearCarrier returned no error, though the clear could not read the carriers")
	}
	r := clearRaceRobot(t, c)
	if r.Status.EstopState != fleetv1.RobotEstopStopped {
		t.Errorf("estopState = %q, want Stopped: nothing is written when the clear cannot read", r.Status.EstopState)
	}
	if len(r.Status.EstopHeldBy) != 1 {
		t.Errorf("estopHeldBy = %+v, want the zone's own record kept for the retried clear", r.Status.EstopHeldBy)
	}
}

// clearRaceAudit records the audit entries a clear writes.
type clearRaceAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *clearRaceAudit) Record(e audit.Entry) (audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return e, nil
}

func (a *clearRaceAudit) detail(event, key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if e.EventType == event {
			return e.Detail[key]
		}
	}
	return ""
}
