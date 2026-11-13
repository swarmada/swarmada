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

package safety

// An entry with a clearedAt is removed from status.stops by one write, and its ID is remembered so
// that a later report of it is answered as a cleared stop (removed.go). A report can arrive the
// moment that write has landed: an adapter reports its latched IDs as each SafetyStream opens, and
// nothing orders that against the removal. The report is answered all the same: the ID is
// remembered before the write that removes the entry, not after it, so no report finds the entry
// gone and the ID not yet remembered. Such a report is otherwise dropped, and the stop it names
// stays latched at the adapter until its next SafetyStream.

import (
	"context"
	"errors"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// afterRemovalClient runs onRemoved, once, as soon as a status patch that leaves the robot without
// the entry id has landed, and before the patch returns to the dispatcher: the report it makes
// arrives in the window between the write and whatever the dispatcher does next.
type afterRemovalClient struct {
	client.Client
	id        string
	once      sync.Once
	onRemoved func()
}

func (c *afterRemovalClient) Status() client.SubResourceWriter {
	return &afterRemovalStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type afterRemovalStatus struct {
	client.SubResourceWriter
	c *afterRemovalClient
}

func (s *afterRemovalStatus) Patch(ctx context.Context, obj client.Object, p client.Patch,
	opts ...client.SubResourcePatchOption) error {
	err := s.SubResourceWriter.Patch(ctx, obj, p, opts...)
	if r, ok := obj.(*fleetv1.Robot); ok && err == nil {
		for _, st := range r.Status.Stops {
			if st.ID == s.c.id {
				return nil
			}
		}
		s.c.once.Do(s.c.onRemoved)
	}
	return err
}

// keptFor reports whether a report of id is kept for the adapter until it reports 1.4.0.
func keptFor(d *Dispatcher, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range d.kept[adapterKey(ns, "acme")] {
		if k.rm.id == id {
			return true
		}
	}
	return false
}

// On the 1.4.0 path the entry is removed on its EstopClearAck (RouteClearAck). The adapter's report
// of the ID, arriving as that removal lands, is answered with the stop's fence and its EstopClear.
func TestRemovedStop_AReportAsTheEstopClearAckRemovalLandsIsAnswered(t *testing.T) {
	d, h, w, id := clearRig(t)
	spy := &clearedFenceSpy{}
	d.Fence = spy
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	before := len(w.clears())
	reported := make(chan struct{})
	d.Client = &afterRemovalClient{Client: h, id: id, onRemoved: func() {
		_ = d.RouteAck(identity("acme"), w, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
		close(reported)
	}}

	_ = d.RouteClearAck(identity("acme"), w, "amr-1", &fav1.EstopClearAck{EstopId: id, LiftedAtMs: 1})
	waitRecorded(t, h.Client, 0)
	<-reported
	if !eventually(func() bool { return len(w.clears()) > before && len(spy.sentCleared()) > 0 }) {
		t.Fatalf("clears %v, cleared fences %v: a report of the ID made as its entry's removal landed was dropped; "+
			"the stop stays latched at the adapter until its next SafetyStream", w.clears(), spy.sentCleared())
	}
	if cs := w.clears(); len(cs) != before+1 || cs[before].GetEstopId() != id {
		t.Fatalf("EstopClears %v, want one more, naming %s", cs, id)
	}
}

// remembered reports whether id is remembered as removed from amr-1.
func remembered(d *Dispatcher, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.rememberedLocked(ns, "amr-1", id)
	return ok
}

// An ID remembered ahead of its removal is forgotten when the removal does not land: the entry
// stays on the robot, with its clearedAt, and is not a removed one.
func TestRemovedStop_AnIDWhoseRemovalIsRefusedIsNotRemembered(t *testing.T) {
	d, h, w, id := clearRig(t)
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	attempted := make(chan struct{})
	var once sync.Once
	h.mu.Lock()
	h.before = func(_ int, obj client.Object) error {
		r, ok := obj.(*fleetv1.Robot)
		if !ok || len(r.Status.Stops) != 0 {
			return nil
		}
		once.Do(func() { close(attempted) })
		return apierrors.NewForbidden(schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "robots"},
			r.Name, errors.New("refused"))
	}
	h.mu.Unlock()

	_ = d.RouteClearAck(identity("acme"), w, "amr-1", &fav1.EstopClearAck{EstopId: id, LiftedAtMs: 1})
	<-attempted
	if !eventually(func() bool { return !remembered(d, id) }) {
		t.Fatalf("stop %s is remembered as removed, but its removal was refused and the entry stands", id)
	}
	if n := len(stored(t, h.Client).Status.Stops); n != 1 {
		t.Fatalf("robot records %d stops after a refused removal, want 1", n)
	}
}

// On a connection that falls back to 1.3.x, the entries awaiting their EstopClearAck are removed
// (removeCleared). A report of the ID arriving as that removal lands is kept, as any report on a
// connection below 1.4.0 is, and answered once the adapter reports 1.4.0.
func TestRemovedStop_AReportAsTheFallbackRemovalLandsIsKeptAndAnswered(t *testing.T) {
	d, h, w, id := clearRig(t)
	spy := &clearedFenceSpy{}
	d.Fence = spy
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	before := len(w.clears())
	reported := make(chan struct{})
	d.Client = &afterRemovalClient{Client: h, id: id, onRemoved: func() {
		_ = d.RouteAck(identity("acme"), w, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
		close(reported)
	}}

	d.ContractReported(identity("acme"), "1.3.0")
	waitRecorded(t, h.Client, 0)
	<-reported
	if !eventually(func() bool { return keptFor(d, id) }) {
		t.Fatalf("a report of the ID made as its entry's removal landed was neither kept nor answered; the stop " +
			"stays latched at the adapter until its next SafetyStream")
	}
	d.ContractReported(identity("acme"), "1.4.0")
	if !eventually(func() bool { return len(w.clears()) > before && len(spy.sentCleared()) > 0 }) {
		t.Fatalf("clears %v, cleared fences %v: the kept report is answered once the adapter reports 1.4.0",
			w.clears(), spy.sentCleared())
	}
	if cs := w.clears(); len(cs) != before+1 || cs[before].GetEstopId() != id {
		t.Fatalf("EstopClears %v, want one more, naming %s", cs, id)
	}
}
