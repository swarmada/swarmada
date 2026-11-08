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

// A pass that reads its carrier one write behind (the status written, the processed marker not yet
// in the manager's cache) must not issue the stop again: row S1 fires once for a trigger
// (RFC-0001 #safety-robot-stop-table), and each send writes an ESTOP_TRIGGERED entry
// (#safety-required-events). The stale read is handed to the reconciler by the test, so no race is
// needed.

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/safety"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// behindClient answers the next Get of the held object's name and type with the held copy, once,
// as a cache one write behind would.
type behindClient struct {
	client.Client
	mu   sync.Mutex
	held client.Object
}

func (b *behindClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, o ...client.GetOption) error {
	b.mu.Lock()
	h := b.held
	if h != nil && key.Name == h.GetName() && reflect.TypeOf(obj) == reflect.TypeOf(h) {
		b.held = nil
		b.mu.Unlock()
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(h.DeepCopyObject()).Elem())
		return nil
	}
	b.mu.Unlock()
	return b.Client.Get(ctx, key, obj, o...)
}

func (b *behindClient) hold(obj client.Object) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held = obj
}

// ackingStream is an adapter's SafetyStream that confirms every Estop STOPPED and counts the
// Estops per robot.
type ackingStream struct {
	d     *safety.Dispatcher
	id    controlstream.TLSIdentity
	mu    sync.Mutex
	sends map[string]int
}

func (s *ackingStream) Send(m *fav1.ControlPlaneSafetyMessage) error {
	e := m.GetEstop()
	if e == nil {
		return nil
	}
	s.mu.Lock()
	s.sends[m.GetRobotId()]++
	s.mu.Unlock()
	go func() {
		time.Sleep(time.Millisecond)
		_ = s.d.RouteAck(s.id, s, m.GetRobotId(), &fav1.EstopAck{EstopId: e.GetEstopId(), State: fav1.EstopState_ESTOP_STATE_STOPPED})
	}()
	return nil
}

type onceRig struct {
	ns   string
	aud  *lockedAudit
	s    *ackingStream
	disp *safety.Dispatcher
	c    *behindClient
}

func newOnceRig(t *testing.T, robots ...string) *onceRig {
	t.Helper()
	ns := envtestNamespace(t)
	for _, name := range robots {
		if err := envK8s.Create(context.Background(), envtestValidRobot(ns, name, "bay")); err != nil {
			t.Fatal(err)
		}
		seedResumeStatus(t, ns, name, fleetv1.RobotEstopNormal, time.Now())
	}
	rig := &onceRig{ns: ns, aud: &lockedAudit{}, c: &behindClient{Client: envK8s}}
	rig.disp = safety.New(envK8s, record.NewFakeRecorder(10000))
	rig.disp.APIReader = envK8s
	rig.disp.DeriveCapabilities = DeriveCapabilities
	id := controlstream.TLSIdentity{AdapterName: "acme-adapter", Namespace: ns, Verified: true}
	rig.s = &ackingStream{d: rig.disp, id: id, sends: map[string]int{}}
	t.Cleanup(rig.disp.RegisterStream(id, rig.s))
	return rig
}

func (rig *onceRig) triggered() int {
	rig.aud.mu.Lock()
	defer rig.aud.mu.Unlock()
	n := 0
	for _, e := range rig.aud.entries {
		if e.EventType == audit.EventEstopTriggered {
			n++
		}
	}
	return n
}

func (rig *onceRig) sent() int {
	rig.s.mu.Lock()
	defer rig.s.mu.Unlock()
	n := 0
	for _, v := range rig.s.sends {
		n += v
	}
	return n
}

// waitStops waits for the stop records the dispatcher writes in the background.
func (rig *onceRig) waitStops(t *testing.T, want int, robots ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, name := range robots {
		for {
			rb := getEnvRobot(t, rig.ns, name)
			if len(rb.Status.Stops) >= want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("robot %s: %d stop records, want %d", name, len(rb.Status.Stops), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// oneWriteBehind drops the processed marker from obj: the carrier as the cache holds it after the
// status write and before the marker's watch event.
func oneWriteBehind(obj client.Object) {
	an := obj.GetAnnotations()
	delete(an, annEstopProcessed)
	obj.SetAnnotations(an)
}

func (rig *onceRig) zoneReconciler() *ZoneEstopReconciler {
	return &ZoneEstopReconciler{Client: rig.c, Scheme: envScheme, Estopper: rig.disp, Recorder: record.NewFakeRecorder(100),
		Audit: rig.aud, APIReader: envK8s}
}

func TestEstopOnce_ZoneStopIssuedOnceWhenTheCacheIsOneWriteBehind(t *testing.T) {
	requireEnvtest(t)
	robots := []string{"amr-1", "amr-2", "amr-3"}
	rig := newOnceRig(t, robots...)
	if err := envK8s.Create(context.Background(), &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}); err != nil {
		t.Fatal(err)
	}
	r := rig.zoneReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "bay"}}
	setEstopTrigger(t, rig.ns, "bay", "person detected")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, robots...)
	stale := treeZone(t, rig.ns, "bay")
	oneWriteBehind(stale)
	rig.c.hold(stale)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := rig.triggered(); got != 1 {
		t.Errorf("ESTOP_TRIGGERED entries = %d, want 1", got)
	}
	if got := rig.sent(); got != 3 {
		t.Errorf("Estops sent = %d, want 3 (one per robot)", got)
	}
}

func TestEstopOnce_NamespaceStopIssuedOnceWhenTheCacheIsOneWriteBehind(t *testing.T) {
	requireEnvtest(t)
	robots := []string{"amr-1", "amr-2"}
	rig := newOnceRig(t, robots...)
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: rig.ns, Name: "swarmada-config",
		Annotations: map[string]string{annEstopTriggered: "person detected"}}}
	if err := envK8s.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	r := &NamespaceEstopReconciler{Client: rig.c, Scheme: envScheme, Estopper: rig.disp, Audit: rig.aud, APIReader: envK8s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "swarmada-config"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, robots...)
	stale := &fleetv1.SwarmadaConfig{}
	if err := envK8s.Get(context.Background(), req.NamespacedName, stale); err != nil {
		t.Fatal(err)
	}
	oneWriteBehind(stale)
	rig.c.hold(stale)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := rig.triggered(); got != 1 {
		t.Errorf("ESTOP_TRIGGERED entries = %d, want 1", got)
	}
	if got := rig.sent(); got != 2 {
		t.Errorf("Estops sent = %d, want 2 (one per robot)", got)
	}
}

func TestEstopOnce_RobotStopIssuedOnceWhenTheCacheIsOneWriteBehind(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1")
	rb := getEnvRobot(t, rig.ns, "amr-1")
	base := rb.DeepCopy()
	if rb.Annotations == nil {
		rb.Annotations = map[string]string{}
	}
	rb.Annotations[annEstopTriggered] = "person detected"
	if err := envK8s.Patch(context.Background(), rb, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	r := &RobotEstopReconciler{Client: rig.c, Scheme: envScheme, Estopper: rig.disp, Audit: rig.aud, APIReader: envK8s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "amr-1"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, "amr-1")
	stale := getEnvRobot(t, rig.ns, "amr-1")
	oneWriteBehind(stale)
	rig.c.hold(stale)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := rig.triggered(); got != 1 {
		t.Errorf("ESTOP_TRIGGERED entries = %d, want 1", got)
	}
	if got := rig.sent(); got != 1 {
		t.Errorf("Estops sent = %d, want 1", got)
	}
}

// A marker that is really gone, not merely unseen, still fans out: what the reconciler remembers
// never withholds a stop.
func TestEstopOnce_ZoneMarkerReallyGoneFansOutAgain(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1")
	if err := envK8s.Create(context.Background(), &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}); err != nil {
		t.Fatal(err)
	}
	r := rig.zoneReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "bay"}}
	setEstopTrigger(t, rig.ns, "bay", "person detected")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, "amr-1")
	z := treeZone(t, rig.ns, "bay")
	base := z.DeepCopy()
	delete(z.Annotations, annEstopProcessed)
	if err := envK8s.Patch(context.Background(), z, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := rig.sent(); got != 2 {
		t.Errorf("Estops sent = %d, want 2: the marker is gone, so the stop goes again", got)
	}
}

// A new trigger of the same value after a clear is a new stop.
func TestEstopOnce_SameValueAfterAClearFansOutAgain(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1")
	if err := envK8s.Create(context.Background(), &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}); err != nil {
		t.Fatal(err)
	}
	r := rig.zoneReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "bay"}}
	setEstopTrigger(t, rig.ns, "bay", "person detected")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, "amr-1")
	setEstopTrigger(t, rig.ns, "bay", "")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	setEstopTrigger(t, rig.ns, "bay", "person detected")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := rig.sent(); got != 2 {
		t.Errorf("Estops sent = %d, want 2: a second trigger is a second stop", got)
	}
}
