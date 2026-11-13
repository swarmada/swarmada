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

// On a connection below 1.4.0 the clear's one status write removes the stops it ends, and their
// IDs are remembered once that write has landed (removed.go). Not before: those entries carry no
// clearedAt and are in force until the write lands, and the write can conflict and never land, so
// remembering them early would answer a report with an EstopClear for a clear never written (S5).
// A report of such an ID that arrives while the clear is writing waits for the clear instead of
// being dropped, and is answered only if the clear's write landed, only on the SafetyStream it came
// on while that is still the adapter's current one, and only within removalWait.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// legacyRig is clearRigAt on a 1.3.0 connection, with a fence spy that sends cleared fences.
func legacyRig(t *testing.T) (*Dispatcher, *hookClient, *wire, *clearedFenceSpy, string) {
	t.Helper()
	d, h, w, id := clearRigAt(t, "1.3.0")
	spy := &clearedFenceSpy{}
	d.Fence = spy
	return d, h, w, spy, id
}

// onClearWrite sets h.before to run f ahead of each status patch that writes Resuming (the clear's
// write); f's error is the patch's.
func onClearWrite(h *hookClient, f func(attempt int) error) {
	attempt := 0
	h.mu.Lock()
	h.before = func(_ int, obj client.Object) error {
		r, ok := obj.(*fleetv1.Robot)
		if !ok || r.Status.EstopState != fleetv1.RobotEstopResuming {
			return nil
		}
		attempt++
		return f(attempt)
	}
	h.mu.Unlock()
}

func conflict(name string) error {
	return apierrors.NewConflict(schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "robots"}, name,
		errors.New("the object has been modified"))
}

// report is the adapter's report of id as latched, on w.
func report(d *Dispatcher, w *wire, id string) {
	_ = d.RouteAck(identity("acme"), w, "amr-1", ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
}

// answeredOnceAt140 reports 1.4.0 for the adapter and waits for the kept report of id to be
// answered on w with the stop's fence and its EstopClear.
func answeredOnceAt140(t *testing.T, d *Dispatcher, w *wire, spy *clearedFenceSpy, id string) {
	t.Helper()
	if !eventually(func() bool { return keptFor(d, id) }) {
		t.Fatalf("the report of %s, made while the clear was removing it on a 1.3.0 connection, was not kept; it "+
			"is answered only when the adapter next reports it", id)
	}
	d.ContractReported(identity("acme"), "1.4.0")
	if !eventually(func() bool { return len(w.clears()) > 0 && len(spy.sentCleared()) > 0 }) {
		t.Fatalf("clears %v, cleared fences %v: the kept report is answered once the adapter reports 1.4.0",
			w.clears(), spy.sentCleared())
	}
	if cs := w.clears(); len(cs) != 1 || cs[0].GetEstopId() != id {
		t.Fatalf("EstopClears %v, want one naming %s", cs, id)
	}
}

// notAnsweredAt140 checks that no report of id is kept or answered, also once the adapter reports
// 1.4.0, on any of ws.
func notAnsweredAt140(t *testing.T, d *Dispatcher, spy *clearedFenceSpy, id string, ws ...*wire) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if keptFor(d, id) {
		t.Fatalf("a report of %s is kept for 1.4.0, but it must not be answered", id)
	}
	d.ContractReported(identity("acme"), "1.4.0")
	time.Sleep(100 * time.Millisecond)
	for _, w := range ws {
		for _, c := range w.clears() {
			if c.GetEstopId() == id {
				t.Fatalf("an EstopClear for %s was sent: %v", id, w.clears())
			}
		}
	}
	if got := spy.sentCleared(); len(got) != 0 {
		t.Fatalf("cleared fences %v: none is sent for a report that is not answered", got)
	}
}

// A report made while the clear removes the ID, just before its write or just as it lands, is
// kept on the 1.3.0 connection and answered once the adapter reports 1.4.0.
func TestLegacyClear_AReportWhileTheClearRemovesTheIDIsKeptAndAnswered(t *testing.T) {
	t.Run("as the write lands", func(t *testing.T) {
		d, h, w, spy, id := legacyRig(t)
		reported := make(chan struct{})
		d.Client = &afterRemovalClient{Client: h, id: id, onRemoved: func() {
			report(d, w, id)
			close(reported)
		}}
		if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil ||
			st != fleetv1.RobotEstopResuming {
			t.Fatalf("ClearEstop = %s, %v", st, err)
		}
		<-reported
		answeredOnceAt140(t, d, w, spy, id)
	})
	t.Run("before the write lands", func(t *testing.T) {
		d, h, w, spy, id := legacyRig(t)
		onClearWrite(h, func(int) error {
			report(d, w, id)
			return nil
		})
		if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil ||
			st != fleetv1.RobotEstopResuming {
			t.Fatalf("ClearEstop = %s, %v", st, err)
		}
		answeredOnceAt140(t, d, w, spy, id)
	})
}

// The clear's first write conflicts and its retry lands: a report made during the first attempt
// waits across the retry and is answered.
func TestLegacyClear_AReportDuringAConflictedAttemptIsAnsweredOnceTheRetryLands(t *testing.T) {
	d, h, w, spy, id := legacyRig(t)
	onClearWrite(h, func(attempt int) error {
		if attempt == 1 {
			report(d, w, id)
			return conflict("amr-1")
		}
		return nil
	})
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil ||
		st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	answeredOnceAt140(t, d, w, spy, id)
}

// Every attempt of the clear's write conflicts, so the clear is never written and the stop stays in
// force: a report made while it was trying is not answered, and the ID is not remembered.
func TestLegacyClear_AReportOfAClearNeverWrittenIsNotAnswered(t *testing.T) {
	d, h, w, spy, id := legacyRig(t)
	var once sync.Once
	onClearWrite(h, func(int) error {
		once.Do(func() { report(d, w, id) })
		return conflict("amr-1")
	})
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err == nil {
		t.Fatal("ClearEstop succeeded, but every write conflicted")
	}
	if remembered(d, id) {
		t.Fatalf("%s is remembered as removed, but the clear was never written", id)
	}
	if r := stored(t, h.Client); r.Status.EstopState != fleetv1.RobotEstopStopped || len(r.Status.Stops) != 1 {
		t.Fatalf("robot %s with %d stops; the clear was never written", r.Status.EstopState, len(r.Status.Stops))
	}
	notAnsweredAt140(t, d, spy, id, w)
}

// The adapter's SafetyStream is replaced while the report waits for the clear: the report is not
// answered on the stream it came on, nor on the new one. The adapter reports again on the new
// stream, where the ID, remembered once the write landed, is answered then.
func TestLegacyClear_AReportWhoseStreamIsReplacedWhileItWaitsIsNotAnswered(t *testing.T) {
	d, h, w, spy, id := legacyRig(t)
	w2 := &wire{d: d, mu: w.mu, log: w.log}
	d.Client = &afterRemovalClient{Client: h, id: id, onRemoved: func() {
		report(d, w, id)
		d.RegisterStream(identity("acme"), w2)
	}}
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil ||
		st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	if !remembered(d, id) {
		t.Fatalf("%s is not remembered, but the clear's write landed", id)
	}
	notAnsweredAt140(t, d, spy, id, w, w2)
}

// The clear's write takes longer than removalWait: the report stops waiting and is not answered,
// even once the write lands and the ID is remembered.
func TestLegacyClear_AReportThatTimesOutWaitingIsNotAnswered(t *testing.T) {
	d, h, w, spy, id := legacyRig(t)
	d.removalWait = 50 * time.Millisecond
	onClearWrite(h, func(int) error {
		report(d, w, id)
		time.Sleep(250 * time.Millisecond) // the write is slow: the report gives up first
		return nil
	})
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil ||
		st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	if !remembered(d, id) {
		t.Fatalf("%s is not remembered, but the clear's write landed", id)
	}
	notAnsweredAt140(t, d, spy, id, w)
}

// endRemoval does nothing the second time: ClearEstop calls it once its attempts end and again,
// deferred, as a safety net. A second close of the clear's channel would panic.
func TestLegacyClear_EndRemovalTwiceDoesNothing(t *testing.T) {
	d, p := New(nil, nil), &pendingRemoval{}
	d.markRemoving(p, &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns}}, []string{"s1"})
	d.endRemoval(p)
	d.endRemoval(p)
	if d.removingLocked(ns, "amr-1", "s1") != nil {
		t.Fatal("s1 is still marked after its clear ended")
	}
}
