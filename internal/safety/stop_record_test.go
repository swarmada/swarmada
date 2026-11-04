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

// Row S1 of RFC-0001 #safety-robot-stop-table: the control plane sends the stop at once, and
// only after sending records it in Robot.status.stops, with its stop ID, scope, carrier and
// sentAt, retrying the write until it lands; the record gates nothing. Row S2 records
// acknowledgedAt with the first STOPPING. The stop ID is random, at least 128 bits, never a
// counter, and every copy and re-send of the stop carries it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func TestStopID_IsARandomUUIDNeverRepeated(t *testing.T) {
	seen := make(map[string]bool, 20000)
	for i := 0; i < 20000; i++ { // two dispatchers' worth of 10 000 stops each
		id := NewStopID()
		if !stopIDForm.MatchString(id) {
			t.Fatalf("stop ID %q is not a version 4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("stop ID %q minted twice", id)
		}
		seen[id] = true
	}
}

// recordedStops waits until the robot's status.stops satisfies ok, and returns it.
func recordedStops(t *testing.T, c client.Client, ok func([]fleetv1.RobotStop) bool) []fleetv1.RobotStop {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r := &fleetv1.Robot{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "amr-1"}, r); err != nil {
			t.Fatal(err)
		}
		if ok(r.Status.Stops) || time.Now().After(deadline) {
			return r.Status.Stops
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTriggerEstop_RecordsTheStopItSentWithItsIDAndAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scope   metrics.EstopScope
		carrier Carrier
		want    fleetv1.StopCarrier
		scopeOf fleetv1.StopScope
	}{
		{"robot", metrics.ScopeRobot, Carrier{}, fleetv1.StopCarrier{Kind: fleetv1.StopCarrierRobot, Name: "amr-1"}, fleetv1.StopScopeRobot},
		{"zone", metrics.ScopeZone, Carrier{Kind: fleetv1.StopCarrierFleetZone, Name: "bay-4", UID: "z-uid"},
			fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: "bay-4", UID: "z-uid"}, fleetv1.StopScopeZone},
		{"namespace", metrics.ScopeNamespace, Carrier{Kind: fleetv1.StopCarrierSwarmadaConfig, Name: "default", UID: "c-uid"},
			fleetv1.StopCarrier{Kind: fleetv1.StopCarrierSwarmadaConfig, Name: "default", UID: "c-uid"}, fleetv1.StopScopeNamespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, c, _ := newDispatcher(t, "acme")
			var sentID atomic.Value
			sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
				sentID.Store(id)
				return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPING), ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
			}}
			d.RegisterStream(identity("acme"), sender)
			if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", tc.scope, tc.carrier); err != nil {
				t.Fatal(err)
			}
			id, _ := sentID.Load().(string)
			stops := recordedStops(t, c, func(s []fleetv1.RobotStop) bool { return len(s) == 1 && s[0].AcknowledgedAt != nil })
			if len(stops) != 1 {
				t.Fatalf("status.stops = %+v, want the one stop sent", stops)
			}
			got := stops[0]
			if got.ID != id || !stopIDForm.MatchString(got.ID) {
				t.Errorf("recorded ID %q, sent %q; want the sent stop ID, a version 4 UUID", got.ID, id)
			}
			if got.Scope != tc.scopeOf || got.Carrier == nil || got.Carrier.Kind != tc.want.Kind || got.Carrier.Name != tc.want.Name ||
				(tc.want.UID != "" && got.Carrier.UID != tc.want.UID) {
				t.Errorf("recorded scope %q carrier %+v, want %q %+v", got.Scope, got.Carrier, tc.scopeOf, tc.want)
			}
			if got.SentAt.IsZero() || got.AcknowledgedAt == nil || got.ClearedAt != nil {
				t.Errorf("sentAt %v acknowledgedAt %v clearedAt %v; want sentAt and acknowledgedAt set, no clearedAt",
					got.SentAt, got.AcknowledgedAt, got.ClearedAt)
			}
		})
	}
}

// orderClient records, in one sequence, every status patch that writes status.stops, beside the
// stop's sends.
type orderLog struct {
	mu  sync.Mutex
	seq []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	o.seq = append(o.seq, s)
	o.mu.Unlock()
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seq...)
}

type orderedSender struct {
	log  *orderLog
	sent chan *fav1.ControlPlaneSafetyMessage
}

func (s *orderedSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	s.log.add("send")
	s.sent <- m
	return nil
}

// newBlockingRecordDispatcher is newDispatcher with a client whose status.stops writes wait on
// release (nil: no wait) and are logged in order.
func newBlockingRecordDispatcher(t *testing.T, log *orderLog, release <-chan struct{}, stops []fleetv1.RobotStop) (*Dispatcher, client.Client, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns, Annotations: map[string]string{fleetv1.RobotIDAnnotation: "amr-1"}},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"}},
		Status:     fleetv1.RobotStatus{Stops: stops},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).WithStatusSubresource(&fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				if data, _ := p.Data(obj); sub == "status" && containsStops(data) {
					log.add("record")
					if release != nil {
						<-release
					}
				}
				return cl.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		}).Build()
	rec := record.NewFakeRecorder(10)
	d := New(c, rec)
	d.Telemetry = &testFrames{}
	d.DeriveCapabilities = testDerivation
	d.deliveryTimeout = 300 * time.Millisecond
	d.confirmTimeout = 300 * time.Millisecond
	return d, c, rec
}

func containsStops(patch []byte) bool { return strings.Contains(string(patch), `"stops"`) }

// The stop goes out before anything is written about it, and a record write that blocks for
// longer than the delivery window moves neither the outcome nor the measured latency.
func TestTriggerEstop_SendsBeforeItRecordsAndASlowRecordDelaysNothing(t *testing.T) {
	log := &orderLog{}
	release := make(chan struct{})
	d, c, _ := newBlockingRecordDispatcher(t, log, release, nil)
	s := &orderedSender{log: log, sent: make(chan *fav1.ControlPlaneSafetyMessage, 4)}
	d.RegisterStream(identity("acme"), s)
	go func() {
		m := <-s.sent
		_ = d.RouteAck(identity("acme"), s, "amr-1", ackState(m.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED))
	}()
	start := time.Now()
	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{})
	if err != nil || !res.Confirmed {
		t.Fatalf("res=%+v err=%v; want a confirmed stop while its record is blocked", res, err)
	}
	if res.Latency >= d.deliveryTimeout || time.Since(start) >= d.deliveryTimeout {
		t.Fatalf("latency %v (call %v) with the record blocked; want well inside the %v delivery window",
			res.Latency, time.Since(start), d.deliveryTimeout)
	}
	if seq := log.snapshot(); len(seq) == 0 || seq[0] != "send" {
		t.Fatalf("order %v; want the send first", seq)
	}
	close(release)
	if stops := recordedStops(t, c, func(s []fleetv1.RobotStop) bool { return len(s) == 1 }); len(stops) != 1 {
		t.Fatalf("the stop was not recorded once the write was released: %+v", stops)
	}
}

// At the cap the stop is still sent, and a Warning Event StopRecordFull names the robot.
func TestTriggerEstop_AtTheCapTheStopIsSentAndTheEventSaysItIsNotRecorded(t *testing.T) {
	full := make([]fleetv1.RobotStop, maxRecordedStops)
	for i := range full {
		// Cleared, each awaiting its EstopClearAck: none is in force, so the trigger issues a stop of
		// its own rather than running a recorded one out (takeover.go).
		cleared := metav1.Now()
		full[i] = fleetv1.RobotStop{ID: fmt.Sprintf("old-%d", i), Scope: fleetv1.StopScopeRobot, SentAt: metav1.Now(),
			Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierRobot, Name: "amr-1"}, ClearedAt: &cleared}
	}
	d, c, rec := newBlockingRecordDispatcher(t, &orderLog{}, nil, full)
	s := &orderedSender{log: &orderLog{}, sent: make(chan *fav1.ControlPlaneSafetyMessage, 4)}
	d.RegisterStream(identity("acme"), s)
	go func() {
		m := <-s.sent
		_ = d.RouteAck(identity("acme"), s, "amr-1", ackState(m.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED))
	}()
	if res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil || !res.Confirmed {
		t.Fatalf("res=%+v err=%v; want the stop sent and confirmed at the cap", res, err)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "StopRecordFull") {
			t.Fatalf("event %q, want StopRecordFull", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no StopRecordFull Event at the cap")
	}
	if n := len(recordedStops(t, c, func([]fleetv1.RobotStop) bool { return true })); n != maxRecordedStops {
		t.Fatalf("status.stops has %d entries, want the cap %d", n, maxRecordedStops)
	}
}

// Two control-plane processes (two leaders across a change of leadership) mint disjoint stop
// IDs, and neither takes the other's acknowledgement: dispatcher A's stop acknowledged on
// dispatcher B's stream under A's ID confirms nothing at B.
func TestTwoDispatchers_NeitherTakesTheOthersAcknowledgement(t *testing.T) {
	a, _, _ := newDispatcher(t, "acme")
	b, _, _ := newDispatcher(t, "acme")
	a.deliveryTimeout, b.deliveryTimeout = 2*time.Second, 2*time.Second
	sa := &recordingSender{sent: make(chan *fav1.ControlPlaneSafetyMessage, 1)}
	sb := &recordingSender{sent: make(chan *fav1.ControlPlaneSafetyMessage, 1)}
	a.RegisterStream(identity("acme"), sa)
	b.RegisterStream(identity("acme"), sb)
	doneA := make(chan error, 1)
	go func() {
		_, err := a.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{})
		doneA <- err
	}()
	doneB := make(chan Result, 1)
	go func() {
		res, _ := b.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{})
		doneB <- res
	}()
	ma, mb := <-sa.sent, <-sb.sent
	if ma.GetEstop().GetEstopId() == mb.GetEstop().GetEstopId() {
		t.Fatalf("two dispatchers minted the same stop ID %q", ma.GetEstop().GetEstopId())
	}
	if err := b.RouteAck(identity("acme"), sb, "amr-1", ackState(ma.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatalf("B on an ack naming A's stop: %v", err)
	}
	select {
	case res := <-doneB:
		t.Fatalf("B's stop resolved on A's acknowledgement: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}
	_ = a.RouteAck(identity("acme"), sa, "amr-1", ackState(ma.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED))
	_ = b.RouteAck(identity("acme"), sb, "amr-1", ackState(mb.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED))
	if err := <-doneA; err != nil && !errors.Is(err, ErrStateUnwritten) {
		t.Fatalf("A: %v", err)
	}
	<-doneB
}
