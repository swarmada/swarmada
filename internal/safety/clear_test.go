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

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Row S5 (RFC-0001 #safety-robot-stop-table): the clear is one robots/status write under a
// resourceVersion precondition, carrying Resuming and clearedAt on each stop it ends; only once
// that write has landed does the control plane send an EstopClear naming each of them.

// wire is a SafetyStream that confirms every Estop STOPPED and records, in order, every message
// it carried and every status patch the client made (log is shared with the client).
type wire struct {
	d   *Dispatcher
	mu  *sync.Mutex // shared with the hook client that writes the same log
	log *[]string
	out []*fav1.ControlPlaneSafetyMessage
}

func (w *wire) Send(m *fav1.ControlPlaneSafetyMessage) error {
	w.mu.Lock()
	w.out = append(w.out, m)
	switch {
	case m.GetEstop() != nil:
		*w.log = append(*w.log, "estop:"+m.GetEstop().GetEstopId())
	case m.GetEstopClear() != nil:
		*w.log = append(*w.log, "clear:"+m.GetEstopClear().GetEstopId())
	}
	w.mu.Unlock()
	if e := m.GetEstop(); e != nil {
		_ = w.d.RouteAck(identity("acme"), w, m.GetRobotId(), ackState(e.GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED))
	}
	return nil
}

func (w *wire) clears() []*fav1.EstopClear {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*fav1.EstopClear
	for _, m := range w.out {
		if c := m.GetEstopClear(); c != nil {
			out = append(out, c)
		}
	}
	return out
}

func (w *wire) estops() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, m := range w.out {
		if m.GetEstop() != nil {
			n++
		}
	}
	return n
}

// hookClient records each status patch's estopState in log, and runs before (when set) ahead of
// the patch, once per patch. A patch is stored and logged under mu in one step, so the log is in
// the order the patches landed: an entry logged after its patch returned could follow a reader
// that saw the patch and reset the log, as clearRigAt does once the stop's record has landed.
type hookClient struct {
	client.Client
	mu     *sync.Mutex
	log    *[]string
	before func(n int, obj client.Object) error
	n      int
	// robotLists counts the Robot lists that have returned: a SafetyStream's opening pass and a
	// contract report each read the adapter's robots once.
	robotLists int
}

func (h *hookClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := h.Client.List(ctx, list, opts...)
	if _, ok := list.(*fleetv1.RobotList); ok {
		h.mu.Lock()
		h.robotLists++
		h.mu.Unlock()
	}
	return err
}

func (h *hookClient) Status() client.SubResourceWriter {
	return &hookStatus{SubResourceWriter: h.Client.Status(), h: h}
}

type hookStatus struct {
	client.SubResourceWriter
	h *hookClient
}

func (s *hookStatus) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	s.h.mu.Lock()
	s.h.n++
	n, before := s.h.n, s.h.before
	s.h.mu.Unlock()
	if before != nil {
		if err := before(n, obj); err != nil {
			return err
		}
	}
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	err := s.h.Client.Status().Patch(ctx, obj, p, opts...)
	if r, ok := obj.(*fleetv1.Robot); ok && err == nil {
		*s.h.log = append(*s.h.log, "patch:"+string(r.Status.EstopState))
	}
	return err
}

// clearRig is a dispatcher on a hook client and a wire, with the robot stopped by a confirmed
// robot-scope stop whose record has landed. Its adapter reported contract 1.4.0.
func clearRig(t *testing.T) (*Dispatcher, *hookClient, *wire, string) {
	t.Helper()
	return clearRigAt(t, "1.4.0")
}

// clearRigAt is clearRig with the adapter's reported contract version; "" reports none, as on a
// new leader before the adapter's next ControlStream.
func clearRigAt(t *testing.T, version string) (*Dispatcher, *hookClient, *wire, string) {
	t.Helper()
	d, c, _ := newDispatcher(t, "acme")
	mu, log := &sync.Mutex{}, &[]string{}
	h := &hookClient{Client: c, mu: mu, log: log}
	d.Client = h
	w := &wire{d: d, mu: mu, log: log}
	d.RegisterStream(identity("acme"), w)
	reads := 1
	if version != "" {
		d.ContractReported(identity("acme"), version)
		if contractClearPath(version) != clearHold {
			reads++
		}
	}
	// The stream's opening pass reads the robot in a goroutine of its own. Read after the stop below
	// is written and before its record lands, the robot would be a stopped robot that records no stop,
	// and the pass would send it a stop of its own (resendHeld). The stop waits until the pass has read.
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		h.mu.Lock()
		n := h.robotLists
		h.mu.Unlock()
		if n >= reads {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("setup: %d of %d robot reads by the stream's opening pass and the contract report", n, reads)
		}
	}
	if res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "alice", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("setup: %+v %v", res, err)
	}
	id := waitRecorded(t, c, 1)[0].ID
	mu.Lock()
	*log = nil
	mu.Unlock()
	return d, h, w, id
}

// waitRecorded waits until the robot records n stops, and returns them.
func waitRecorded(t *testing.T, c client.Client, n int) []fleetv1.RobotStop {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		r := &fleetv1.Robot{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-1"}, r); err != nil {
			t.Fatal(err)
		}
		if len(r.Status.Stops) == n {
			return r.Status.Stops
		}
		if time.Now().After(end) {
			t.Fatalf("robot records %d stops, want %d", len(r.Status.Stops), n)
		}
	}
}

func stored(t *testing.T, c client.Client) *fleetv1.Robot {
	t.Helper()
	r := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-1"}, r); err != nil {
		t.Fatal(err)
	}
	return r
}

// One write carries Resuming and the stop's clearedAt; the EstopClear goes after it, once, naming
// the stop, with the clearer, the clearedAt written and the leader term (M23, M7).
func TestClear_WritesOnceThenSendsTheEstopClear(t *testing.T) {
	d, h, w, id := clearRig(t)
	d.Term = func() (uint64, bool) { return 9, true }
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	r := stored(t, h.Client)
	if r.Status.EstopState != fleetv1.RobotEstopResuming || len(r.Status.Stops) != 1 || r.Status.Stops[0].ClearedAt == nil {
		t.Fatalf("stored: %s %+v; want Resuming and the stop's clearedAt", r.Status.EstopState, r.Status.Stops)
	}
	h.mu.Lock()
	got := strings.Join(*h.log, " ")
	h.mu.Unlock()
	if got != "patch:Resuming clear:"+id {
		t.Fatalf("order on the API server and the wire: %q; want one Resuming write, then the EstopClear", got)
	}
	c := w.clears()[0]
	if c.GetClearedBy() != "alice" || c.GetLeaderTerm() != 9 || c.GetClearedAtMs() != r.Status.Stops[0].ClearedAt.UnixMilli() {
		t.Fatalf("EstopClear %+v; want clearedBy alice, term 9, clearedAt %d", c, r.Status.Stops[0].ClearedAt.UnixMilli())
	}
}

// The term is the one held at the send, read again for each clear (M7).
func TestClear_CarriesTheTermHeldAtTheSend(t *testing.T) {
	for _, term := range []uint64{1, 2, 9} {
		d, _, w, _ := clearRig(t)
		d.Term = func() (uint64, bool) { return term, true }
		if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
			t.Fatal(err)
		}
		if cs := w.clears(); len(cs) != 1 || cs[0].GetLeaderTerm() != term {
			t.Fatalf("term %d: clears %v", term, cs)
		}
	}
}

// A clear does not move a Stopping robot (S5's from-state is Stopped or Failed).
func TestClear_DoesNotMoveAStoppingRobot(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopStopping)
	st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{})
	if err != nil || st != fleetv1.RobotEstopStopping || estopState(t, c) != fleetv1.RobotEstopStopping {
		t.Fatalf("ClearEstop = %s, %v, stored %s; want Stopping, untouched", st, err, estopState(t, c))
	}
}

// A stop recorded between the clear's read and its write makes the write fail its precondition;
// read again, the stop is in force, and nothing is written or sent.
func TestClear_AStopRecordedSinceTheReadWritesAndSendsNothing(t *testing.T) {
	d, h, w, _ := clearRig(t)
	h.before = func(n int, obj client.Object) error {
		if r, ok := obj.(*fleetv1.Robot); !ok || r.Status.EstopState != fleetv1.RobotEstopResuming || n > 1 {
			return nil
		}
		cur := stored(t, h.Client)
		cur.Status.Stops = append(cur.Status.Stops, fleetv1.RobotStop{ID: "zone-stop", Scope: fleetv1.StopScopeZone,
			Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: "aisle-3"}, SentAt: metav1.Now()})
		return h.Client.Status().Update(context.Background(), cur)
	}
	h.mu.Lock()
	h.n = 0
	h.mu.Unlock()
	st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{})
	if err != nil || st != fleetv1.RobotEstopStopped {
		t.Fatalf("ClearEstop = %s, %v; want Stopped standing", st, err)
	}
	if got := estopState(t, h.Client); got != fleetv1.RobotEstopStopped || len(w.clears()) != 0 {
		t.Fatalf("stored %s, %d EstopClears; want Stopped and none", got, len(w.clears()))
	}
}

// A clear of one zone on a robot another zone's recorded stop still holds writes no clearedAt and
// sends nothing; the last clear names both.
func TestClear_AnIntermediateZoneClearWritesAndSendsNothing(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.ContractReported(identity("acme"), "1.4.0")
	w := &wire{d: d, mu: &sync.Mutex{}, log: &[]string{}}
	d.RegisterStream(identity("acme"), w)
	r := stored(t, c)
	r.Status.EstopState = fleetv1.RobotEstopStopped
	for _, z := range []string{"a", "b"} {
		r.Status.Stops = append(r.Status.Stops, fleetv1.RobotStop{ID: "stop-" + z, Scope: fleetv1.StopScopeZone,
			Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: z}, SentAt: metav1.Now()})
		r.Status.EstopHeldBy = append(r.Status.EstopHeldBy, fleetv1.EstopHold{Kind: fleetv1.EstopHoldFleetZone, Name: z})
	}
	if err := c.Status().Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	ended := map[string]bool{"a": true}
	zone := func() Ends {
		return Ends{Hold: func(h fleetv1.EstopHold) bool { return ended[h.Name] },
			Stop: func(st fleetv1.RobotStop) bool { return ended[st.Carrier.Name] }}
	}
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", zone()); err != nil || st != fleetv1.RobotEstopStopped {
		t.Fatalf("first zone's clear = %s, %v; want Stopped standing", st, err)
	}
	for _, st := range stored(t, c).Status.Stops {
		if st.ClearedAt != nil {
			t.Fatalf("an intermediate clear wrote clearedAt on %s", st.ID)
		}
	}
	if len(w.clears()) != 0 {
		t.Fatalf("an intermediate clear sent %d EstopClears", len(w.clears()))
	}
	ended["b"] = true
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", zone()); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("last zone's clear = %s, %v; want Resuming", st, err)
	}
	if cs := w.clears(); len(cs) != 2 {
		t.Fatalf("%d EstopClears after the last clear; want 2, one per zone's stop", len(cs))
	}
}

// A process that does not lead writes nothing and sends nothing.
func TestClear_ANonLeaderWritesAndSendsNothing(t *testing.T) {
	d, h, w, _ := clearRig(t)
	d.Term = func() (uint64, bool) { return 3, false }
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); !errors.Is(err, ErrNotLeading) {
		t.Fatalf("err %v; want ErrNotLeading", err)
	}
	if got := estopState(t, h.Client); got != fleetv1.RobotEstopStopped || len(w.clears()) != 0 {
		t.Fatalf("stored %s, %d EstopClears; want Stopped and none", got, len(w.clears()))
	}
}

// On an adapter that reported a contract below 1.4.0 the clear removes the stops it ends and sends
// no EstopClear; on one that reported nothing yet, it sends none and keeps the entry
// (clear_contract_unknown_test.go).
func TestClear_A13AdapterHasItsEntriesRemovedAndNoEstopClear(t *testing.T) {
	for _, tc := range []struct {
		version     string
		entries     int
		estopClears int
	}{{"1.3.0", 0, 0}, {"", 1, 0}, {"1.4.0", 1, 1}} {
		d, h, w, _ := clearRigAt(t, tc.version)
		if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
			t.Fatalf("%q: ClearEstop = %s, %v", tc.version, st, err)
		}
		if n := len(stored(t, h.Client).Status.Stops); n != tc.entries || len(w.clears()) != tc.estopClears {
			t.Fatalf("contract %q: %d entries, %d EstopClears; want %d, %d", tc.version, n, len(w.clears()), tc.entries, tc.estopClears)
		}
	}
}

// An EstopClearAck on the adapter's current SafetyStream removes the cleared entry; one on a
// stream that has been replaced, or for an entry with no clearedAt, changes nothing.
func TestClearAck_RemovesTheClearedEntryOnlyFromTheCurrentStream(t *testing.T) {
	d, h, w, id := clearRig(t)
	ack := &fav1.EstopClearAck{EstopId: id, LiftedAtMs: 1}
	// Before the clear: the entry has no clearedAt.
	_ = d.RouteClearAck(identity("acme"), w, "amr-1", ack)
	time.Sleep(50 * time.Millisecond)
	if n := len(stored(t, h.Client).Status.Stops); n != 1 {
		t.Fatalf("an ack before the clear removed the entry (%d left)", n)
	}
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	// A replaced stream.
	old := w
	w2 := &wire{d: d, mu: w.mu, log: w.log}
	d.RegisterStream(identity("acme"), w2)
	_ = d.RouteClearAck(identity("acme"), old, "amr-1", ack)
	time.Sleep(50 * time.Millisecond)
	if n := len(stored(t, h.Client).Status.Stops); n != 1 {
		t.Fatalf("an ack on a replaced stream removed the entry (%d left)", n)
	}
	_ = d.RouteClearAck(identity("acme"), w2, "amr-1", ack)
	waitRecorded(t, h.Client, 0)
}

// A SafetyStream that opens while a cleared entry awaits its EstopClearAck is sent that
// EstopClear again, and no Estop for it.
func TestClear_ANewSafetyStreamIsSentTheClearAgain(t *testing.T) {
	d, h, w, id := clearRig(t)
	d.APIReader = h.Client
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	w2 := &wire{d: d, mu: w.mu, log: w.log}
	d.RegisterStream(identity("acme"), w2)
	for end := time.Now().Add(2 * time.Second); len(w2.clears()) == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	if cs := w2.clears(); len(cs) != 1 || cs[0].GetEstopId() != id || w2.estops() != 0 {
		t.Fatalf("new stream: clears %v, %d Estops; want the one EstopClear again and no Estop", cs, w2.estops())
	}
}

// Rules for every S row: a stop's outcome is never dropped for a conflict. One that conflicts
// six times, past the five a release is allowed, still lands.
func TestEstopStateWrite_AStopThatConflictsRepeatedlyStillLands(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	mu, log := &sync.Mutex{}, &[]string{}
	h := &hookClient{Client: c, mu: mu, log: log, before: func(n int, obj client.Object) error {
		if n <= 6 {
			return apierrors.NewConflict(schema.GroupResource{Resource: "robots"}, "amr-1", errors.New("stale"))
		}
		return nil
	}}
	d.Client = h
	written, err := d.writeEstopState(context.Background(), stored(t, c), fleetv1.RobotEstopStopped, "confirmed", 1)
	if err != nil || !written || estopState(t, c) != fleetv1.RobotEstopStopped {
		t.Fatalf("written %v, err %v, stored %s; want Stopped written after the conflicts", written, err, estopState(t, c))
	}
}

// S6's guard at the write: a stop recorded on the robot between ConfirmResume's read and its write
// leaves it Resuming.
func TestRelease_AStopRecordedAtTheWriteLeavesTheRobotResuming(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}})
	if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "alice", metrics.ScopeRobot, Carrier{}); err != nil {
		t.Fatal(err)
	}
	waitRecorded(t, c, 1)
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	projected(t, d, c, time.Now().Add(time.Second), fleetv1.RobotPhaseIdle)
	mu, log := &sync.Mutex{}, &[]string{}
	d.Client = &hookClient{Client: c, mu: mu, log: log, before: func(n int, obj client.Object) error {
		if r, ok := obj.(*fleetv1.Robot); ok && r.Status.EstopState == fleetv1.RobotEstopNormal && n == 1 {
			cur := stored(t, c)
			cur.Status.Stops = append(cur.Status.Stops, fleetv1.RobotStop{ID: "late", Scope: fleetv1.StopScopeZone,
				Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: "aisle-3"}, SentAt: metav1.Now()})
			return c.Status().Update(context.Background(), cur)
		}
		return nil
	}}
	st, _, err := d.ConfirmResume(context.Background(), ns, "amr-1", "alice")
	if err != nil || st == fleetv1.RobotEstopNormal || estopState(t, c) != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume = %s, %v, stored %s; want the robot left Resuming", st, err, estopState(t, c))
	}
}

// M22: Resuming is written in one place, the clear (row S5). A go/ast scan of every non-test file
// of the module finds one write of RobotEstopResuming to an estopState, and it is in clear.go.
func TestResumingHasOneWriter(t *testing.T) {
	root := filepath.Join("..", "..")
	var sites []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					for _, a := range call.Args {
						if sel, ok := a.(*ast.SelectorExpr); ok && sel.Sel.Name == "RobotEstopResuming" {
							if fn, ok := call.Fun.(*ast.SelectorExpr); ok && strings.Contains(fn.Sel.Name, "EstopState") {
								sites = append(sites, filepath.ToSlash(path)+":"+fn.Sel.Name)
							}
						}
					}
				}
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "EstopState" || i >= len(as.Rhs) {
						continue
					}
					if rhs, ok := as.Rhs[i].(*ast.SelectorExpr); ok && rhs.Sel.Name == "RobotEstopResuming" {
						sites = append(sites, filepath.ToSlash(path))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(sites) != 1 || !strings.HasSuffix(sites[0], "internal/safety/clear.go") {
		t.Fatalf("writes of Resuming to an estopState: %v; want exactly one, in internal/safety/clear.go", sites)
	}
}
