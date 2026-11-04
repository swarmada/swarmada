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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/safety"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// RFC-0001 #safety-estop-scopes: while a zone's or the namespace's stop is in force, it stops each
// robot "that the stop has not reached". The pass over a stop in force sends it to a robot whose
// status.stops holds no stop of the carrier. Row S1 records the stop after the send, in its own
// goroutine, and "a stop the control plane has sent, before its record lands, is in its own
// state". A pass that runs again before the record lands finds a robot the stop has reached, and
// sends it nothing.

// ackingSender is an adapter's SafetyStream that acknowledges each estop STOPPED on the stream
// it came down, and counts the estops per wire robot_id and issuer. The stream's registration
// re-sends r1's held stop on its own (issued by the dispatcher); the carrier's stop in force is
// issued by its controller.
type ackingSender struct {
	d  *safety.Dispatcher
	id controlstream.TLSIdentity
	mu sync.Mutex
	n  map[string]int
}

func (s *ackingSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	e := m.GetEstop()
	if e == nil {
		return nil
	}
	s.mu.Lock()
	s.n[m.GetRobotId()+"/"+e.GetIssuedBy()]++
	s.mu.Unlock()
	return s.d.RouteAck(s.id, s, m.GetRobotId(), &fav1.EstopAck{EstopId: e.GetEstopId(),
		State: fav1.EstopState_ESTOP_STATE_STOPPED, ConfirmedAtMs: time.Now().UnixMilli()})
}

func (s *ackingSender) sent(robot, issuedBy string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[robot+"/"+issuedBy]
}

func TestStopInForce_ASecondPassBeforeTheRecordLandsSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    fleetv1.StopCarrierKind
		issuer  string
		carrier client.Object
		pass    func(c client.Client, scheme *runtime.Scheme, d *safety.Dispatcher) func(context.Context) error
	}{
		{
			name: "zone", kind: fleetv1.StopCarrierFleetZone, issuer: lateArrivalActor,
			carrier: &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: actionNS, UID: "z-uid",
				Annotations: map[string]string{annEstopTriggered: "zs", annEstopProcessed: "zs"}}},
			pass: func(c client.Client, scheme *runtime.Scheme, d *safety.Dispatcher) func(context.Context) error {
				r := &ZoneEstopReconciler{Client: c, Scheme: scheme, Estopper: d, APIReader: c}
				return func(ctx context.Context) error {
					_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: "z"}})
					return err
				}
			},
		},
		{
			name: "namespace", kind: fleetv1.StopCarrierSwarmadaConfig, issuer: namespaceLateArrivalActor,
			carrier: &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: actionNS,
				UID: "c-uid", Annotations: map[string]string{annEstopTriggered: "ns", annEstopProcessed: "ns"}}},
			pass: func(c client.Client, scheme *runtime.Scheme, d *safety.Dispatcher) func(context.Context) error {
				r := &NamespaceEstopReconciler{Client: c, Scheme: scheme, Estopper: d, APIReader: c}
				return func(ctx context.Context) error {
					_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS,
						Name: SwarmadaConfigName}})
					return err
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := fleetv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add scheme: %v", err)
			}
			// r1, in z, already Stopped (by a stop of its own), with no record of the carrier's stop.
			r1 := &fleetv1.Robot{
				ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: actionNS, UID: "r1-uid",
					Annotations: map[string]string{RobotIDAnnotation: "r1"}},
				Spec:   fleetv1.RobotSpec{Zone: "z", Adapter: fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"}},
				Status: fleetv1.RobotStatus{CurrentZone: "z", EstopState: fleetv1.RobotEstopStopped},
			}
			objs := []client.Object{tc.carrier, r1}
			if tc.kind != fleetv1.StopCarrierFleetZone {
				objs = append(objs, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: actionNS}})
			}
			// The record of the carrier's stop on r1 is held until release: the window between
			// the send and the record, open for as long as the test needs it.
			release := make(chan struct{})
			var releaseOnce sync.Once
			open := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(open)
			var held atomic.Int64
			want := []byte(`"kind":"` + string(tc.kind) + `"`)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
				WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.FleetZone{}, &fleetv1.SwarmadaConfig{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
						patch client.Patch, opts ...client.SubResourcePatchOption,
					) error {
						if data, err := patch.Data(obj); err == nil && obj.GetName() == "r1" &&
							bytes.Contains(data, []byte(`"stops":[`)) && bytes.Contains(data, want) {
							held.Add(1)
							<-release
						}
						return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			d := safety.New(c, record.NewFakeRecorder(64))
			id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: actionNS, Verified: true}
			s := &ackingSender{d: d, id: id, n: map[string]int{}}
			d.RegisterStream(id, s)
			pass := tc.pass(c, scheme, d)
			ctx := context.Background()
			carrierStops := func(st []fleetv1.RobotStop) int {
				n := 0
				for _, e := range st {
					if e.Carrier != nil && e.Carrier.Kind == tc.kind {
						n++
					}
				}
				return n
			}

			if err := pass(ctx); err != nil {
				t.Fatalf("first pass: %v", err)
			}
			if n := s.sent("r1", tc.issuer); n != 1 {
				t.Fatalf("setup: the first pass sent r1 %d stops, want 1", n)
			}
			// The window is open: the stop has been sent, and its record has not landed.
			got := getRobot(t, c, "r1", actionNS)
			if n := carrierStops(got.Status.Stops); n != 0 {
				t.Fatalf("setup: r1 records %d stops of the carrier before the record was released", n)
			}
			if !stopStateIsStop(got.Status.EstopState) {
				t.Fatalf("setup: r1 reads %q after the first pass", got.Status.EstopState)
			}

			if err := pass(ctx); err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if n := s.sent("r1", tc.issuer); n != 1 {
				t.Errorf("a pass that ran before the record of the stop it sent landed sent r1 the carrier's stop "+
					"again: %d stops sent, want 1", n)
			}

			open()
			// Each stop sent is recorded once released.
			sends := s.sent("r1", tc.issuer)
			stops := waitStops(t, c, "r1", func(st []fleetv1.RobotStop) bool { return carrierStops(st) >= sends })
			if n := carrierStops(stops); n != 1 {
				t.Errorf("r1 records %d stops of the carrier, want 1: %+v", n, stops)
			}
			// Once the record has landed, a later pass sends nothing either.
			if err := pass(ctx); err != nil {
				t.Fatalf("third pass: %v", err)
			}
			if n := s.sent("r1", tc.issuer); n != sends {
				t.Errorf("a pass after the record landed sent r1 %d more stops", n-sends)
			}
		})
	}
}

// inForceTargets reads a stop sent and not yet recorded as the record: a robot in a stop state
// is not sent it again. A robot whose stop state was never written is still sent it.
func TestInForceTargets_ASentUnrecordedStopIsTheRecord(t *testing.T) {
	ref := holdRef{kind: fleetv1.EstopHoldFleetZone, name: "z", uid: "z-uid"}
	robot := func(name string, st fleetv1.RobotEstopState) fleetv1.Robot {
		r := fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: name}}
		r.Status.EstopState = st
		return r
	}
	all := []fleetv1.Robot{
		robot("sent-stopped", fleetv1.RobotEstopStopped),
		robot("sent-failed", fleetv1.RobotEstopFailed),
		robot("sent-normal", fleetv1.RobotEstopNormal),
		robot("not-sent-stopped", fleetv1.RobotEstopStopped),
	}
	sent := map[string]bool{"sent-stopped": true, "sent-failed": true, "sent-normal": true}
	stop, recordOnly := inForceTargets(ref, all, all, sent)
	got := map[string]bool{}
	for _, r := range stop {
		got[r.Name] = true
	}
	for _, n := range []string{"sent-normal", "not-sent-stopped"} {
		if !got[n] {
			t.Errorf("%s is not sent the stop: %v", n, got)
		}
	}
	for _, n := range []string{"sent-stopped", "sent-failed"} {
		if got[n] {
			t.Errorf("%s, sent the stop a moment ago and in a stop state, is sent it again", n)
		}
	}
	if len(recordOnly) != 2 {
		t.Errorf("recordOnly = %d robots, want the two sent robots in a stop state, to be recorded", len(recordOnly))
	}
}

func stopStateIsStop(s fleetv1.RobotEstopState) bool {
	return s == fleetv1.RobotEstopStopping || s == fleetv1.RobotEstopStopped || s == fleetv1.RobotEstopFailed
}

// waitStops waits up to 3 s until robot's status.stops satisfies ok, and returns it.
func waitStops(t *testing.T, c client.Client, robot string, ok func([]fleetv1.RobotStop) bool) []fleetv1.RobotStop {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := getRobot(t, c, robot, actionNS).Status.Stops
		if ok(st) || time.Now().After(deadline) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
}
