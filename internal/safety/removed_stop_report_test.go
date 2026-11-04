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

// A stop ID whose entry the control plane removed is remembered for that robot, and a later report
// naming it is answered as a cleared stop: with the stop's fence and its EstopClear (RFC-0001
// #safety-robot-stop-table, Removing a recorded stop; Reported stop IDs and local-latch reports).
// It is the one way RFC-0001 gives to lift an ID an adapter latched while its connection was at
// 1.3.x, short of a restart: that connection is sent no EstopClear, the clear removes the entry,
// and the adapter keeps the ID latched across reconnects and reports it on each SafetyStream that
// opens.

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// clearedFenceSpy is a fenceSpy that also sends a cleared stop's fence: one that adds the stop to
// nothing in force.
type clearedFenceSpy struct {
	fenceSpy
	cmu           sync.Mutex
	clearedFences []string // robot/stop ID
}

func (f *clearedFenceSpy) SendClearedFence(robot *fleetv1.Robot, stopID string, _ *fav1.Estop) error {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.clearedFences = append(f.clearedFences, robot.Name+"/"+stopID)
	return nil
}

func (f *clearedFenceSpy) sentCleared() []string {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	return append([]string(nil), f.clearedFences...)
}

// reconnectAt opens a new SafetyStream for the adapter after it reported version on a new
// ControlStream, as an adapter that restarted its connection would.
func reconnectAt(t *testing.T, d *Dispatcher, w *wire, version string) *wire {
	t.Helper()
	d.ContractReported(identity("acme"), version)
	w2 := &wire{d: d, mu: w.mu, log: w.log}
	d.RegisterStream(identity("acme"), w2)
	return w2
}

// eventually waits up to 2 s for cond.
func eventually(cond func() bool) bool {
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// clearedOnA13Connection stops amr-1 on a 1.3.0 connection and clears it, which removes the entry.
func clearedOnA13Connection(t *testing.T) (*Dispatcher, *hookClient, *wire, *clearedFenceSpy, string) {
	t.Helper()
	d, h, w, id := clearRigAt(t, "1.3.0")
	spy := &clearedFenceSpy{}
	d.Fence = spy
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("setup: ClearEstop = %s, %v", st, err)
	}
	if n := len(stored(t, h.Client).Status.Stops); n != 0 || len(w.clears()) != 0 {
		t.Fatalf("setup: %d entries, %d EstopClears after a 1.3.0 clear; want the entry removed and none sent",
			n, len(w.clears()))
	}
	return d, h, w, spy, id
}

// The adapter latched the stop on its 1.3.0 connection and kept it across the reconnect. On its
// later 1.4.0 connection it reports the ID: the control plane answers with the stop's fence and its
// EstopClear, and the fence holds no work (the robot is not put back under the stop).
func TestRemovedStop_ReportedOnA140ConnectionIsAnsweredWithItsFenceAndEstopClear(t *testing.T) {
	d, _, w, spy, id := clearedOnA13Connection(t)
	w2 := reconnectAt(t, d, w, "1.4.0")

	if err := d.RouteAck(identity("acme"), w2, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatalf("RouteAck: %v", err)
	}
	if !eventually(func() bool { return len(w2.clears()) > 0 && len(spy.sentCleared()) > 0 }) {
		t.Fatalf("EstopClears %v, cleared fences %v: a reported ID removed and remembered as cleared is answered "+
			"with that stop's fence and its EstopClear", w2.clears(), spy.sentCleared())
	}
	if cs := w2.clears(); len(cs) != 1 || cs[0].GetEstopId() != id {
		t.Fatalf("EstopClears %v, want one naming %s", cs, id)
	}
	if got := spy.sentCleared(); len(got) != 1 || got[0] != "amr-1/"+id {
		t.Fatalf("cleared fences %v, want amr-1/%s", got, id)
	}
	if held := spy.HeldStops(ns, "amr-1"); len(held) != 0 {
		t.Fatalf("stops in force after the answer: %v; a cleared stop's fence holds no work", held)
	}
	if n := w2.estops(); n != 0 {
		t.Fatalf("%d Estops on the new stream; a cleared stop is not sent again as a stop", n)
	}
}

// Nothing else is answered: an ID the control plane holds no memory of, a report on a connection
// still below 1.4.0 (which is sent no EstopClear), and a report for a robot of the same name created
// since (another robot, which that stop never held).
func TestRemovedStop_OnlyARememberedIDOnA140ConnectionForThatRobotIsAnswered(t *testing.T) {
	t.Run("unknown ID", func(t *testing.T) {
		d, _, w, spy, _ := clearedOnA13Connection(t)
		w2 := reconnectAt(t, d, w, "1.4.0")
		_ = d.RouteAck(identity("acme"), w2, "amr-1", ackState("never-sent", fav1.EstopState_ESTOP_STATE_STOPPED))
		time.Sleep(100 * time.Millisecond)
		if len(w2.clears()) != 0 || len(spy.sentCleared()) != 0 {
			t.Fatalf("an unknown ID was answered: clears %v, fences %v", w2.clears(), spy.sentCleared())
		}
	})
	t.Run("1.3.0 connection", func(t *testing.T) {
		d, _, w, spy, id := clearedOnA13Connection(t)
		w2 := reconnectAt(t, d, w, "1.3.0")
		_ = d.RouteAck(identity("acme"), w2, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
		time.Sleep(100 * time.Millisecond)
		if len(w2.clears()) != 0 || len(spy.sentCleared()) != 0 {
			t.Fatalf("a 1.3.0 connection was answered: clears %v, fences %v", w2.clears(), spy.sentCleared())
		}
	})
	t.Run("recreated robot", func(t *testing.T) {
		d, h, w, spy, id := clearedOnA13Connection(t)
		old := stored(t, h.Client)
		if err := h.Delete(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		again := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns, UID: types.UID("second-life"),
			Annotations: old.Annotations}, Spec: old.Spec}
		if err := h.Create(context.Background(), again); err != nil {
			t.Fatal(err)
		}
		w2 := reconnectAt(t, d, w, "1.4.0")
		_ = d.RouteAck(identity("acme"), w2, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
		time.Sleep(100 * time.Millisecond)
		if len(w2.clears()) != 0 || len(spy.sentCleared()) != 0 {
			t.Fatalf("a robot created since was answered for the first robot's stop: clears %v, fences %v",
				w2.clears(), spy.sentCleared())
		}
	})
}

// An entry removed on its EstopClearAck (the 1.4.0 path) is remembered too: "The control plane
// keeps, for that robot, the ID of a removed entry."
func TestRemovedStop_AnEntryRemovedOnItsEstopClearAckIsRemembered(t *testing.T) {
	d, h, w, id := clearRig(t)
	spy := &clearedFenceSpy{}
	d.Fence = spy
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	_ = d.RouteClearAck(identity("acme"), w, "amr-1", &fav1.EstopClearAck{EstopId: id, LiftedAtMs: 1})
	waitRecorded(t, h.Client, 0)
	before := len(w.clears())

	_ = d.RouteAck(identity("acme"), w, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
	if !eventually(func() bool { return len(w.clears()) > before && len(spy.sentCleared()) > 0 }) {
		t.Fatalf("clears %v, cleared fences %v: a report of an ID removed on its EstopClearAck is answered as a "+
			"cleared stop", w.clears(), spy.sentCleared())
	}
}

var _ client.Object = (*fleetv1.Robot)(nil)

// An adapter opens its SafetyStream beside its ControlStream and reports its latched IDs as the
// SafetyStream opens, which can be before the new ControlStream's hello says 1.4.0: this process then
// still holds the version of the adapter's previous connection, or none on a new leader. The report
// is kept, and answered once the hello says 1.4.0. The adapter reports only when a SafetyStream opens,
// so a report dropped here would wait for the next one.
func TestRemovedStop_AReportBeforeTheHelloIsAnsweredOnceTheConnectionSays140(t *testing.T) {
	d, _, w, spy, id := clearedOnA13Connection(t)
	w2 := &wire{d: d, mu: w.mu, log: w.log}
	d.RegisterStream(identity("acme"), w2) // the SafetyStream opens first: still 1.3.0 here
	_ = d.RouteAck(identity("acme"), w2, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
	time.Sleep(100 * time.Millisecond)
	if len(w2.clears()) != 0 {
		t.Fatalf("answered while the connection was still known as 1.3.0: %v", w2.clears())
	}
	d.ContractReported(identity("acme"), "1.4.0") // then the hello
	if !eventually(func() bool { return len(w2.clears()) > 0 && len(spy.sentCleared()) > 0 }) {
		t.Fatalf("EstopClears %v, cleared fences %v: the report kept from before the hello is answered once it "+
			"says 1.4.0", w2.clears(), spy.sentCleared())
	}
	if cs := w2.clears(); len(cs) != 1 || cs[0].GetEstopId() != id {
		t.Fatalf("EstopClears %v, want one naming %s", cs, id)
	}
}
