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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/safety"
)

// §9.6.5.1 estop detail fields.
//
// The <500 ms SLA in §9.6.2.2 is measured on the estop round trip, and §9.5.4 offers the
// audit chain as safety-case evidence. That only holds if the timings are actually in the
// entry. The existing fakeEstopper returns a zero Result, so nothing in the suite reached
// the timing path until these tests; a field whose presence is assumed rather than checked
// is the same defect shape as an event constant with no writer.

// timedEstopper reports a delivered estop with a known latency.
type timedEstopper struct {
	latency   time.Duration
	violation bool
	delivered bool
	robots    []string
}

func (f *timedEstopper) TriggerEstop(_ context.Context, _, robotID, _, _ string) (safety.Result, error) {
	f.robots = append(f.robots, robotID)
	return safety.Result{
		State:            fleetv1.RobotEstopStopped,
		Confirmed:        true,
		Delivered:        f.delivered,
		Latency:          f.latency,
		LatencyViolation: f.violation,
	}, nil
}

func (f *timedEstopper) ClearEstop(_ context.Context, _, _, _ string) (fleetv1.RobotEstopState, error) {
	return fleetv1.RobotEstopNormal, nil
}

// A delivered robot-scope estop records the full timing set.
func TestEstopAudit_RobotScopeRecordsTimings(t *testing.T) {
	d := estopTimingDetail(map[string]string{"reason": "test"},
		time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		safety.Result{Delivered: true, Latency: 265 * time.Millisecond, LatencyViolation: false})

	for _, k := range []string{"rpc_sent_at", "delivered", "ack_received_at", "ack_latency_ms", "latency_violation"} {
		if _, ok := d[k]; !ok {
			t.Errorf("detail is missing %q; §9.6.2.2's SLA evidence is incomplete", k)
		}
	}
	if d["ack_latency_ms"] != "265" {
		t.Errorf("ack_latency_ms = %q, want \"265\"", d["ack_latency_ms"])
	}
	// The interval between the two timestamps MUST equal the measured latency, or the
	// entry contradicts itself and a reviewer cannot trust either value.
	sent, err := time.Parse(time.RFC3339Nano, d["rpc_sent_at"])
	if err != nil {
		t.Fatalf("rpc_sent_at is not RFC3339Nano: %v", err)
	}
	ack, err := time.Parse(time.RFC3339Nano, d["ack_received_at"])
	if err != nil {
		t.Fatalf("ack_received_at is not RFC3339Nano: %v", err)
	}
	if got := ack.Sub(sent); got != 265*time.Millisecond {
		t.Errorf("ack_received_at - rpc_sent_at = %v, want the measured 265ms", got)
	}
}

// An UNDELIVERED estop must not fabricate an ack time. Recording one would put a
// confirmation in the chain for a stop nothing acknowledged.
func TestEstopAudit_UndeliveredOmitsAckFields(t *testing.T) {
	d := estopTimingDetail(map[string]string{}, time.Now(), safety.Result{Delivered: false})

	if d["delivered"] != "false" {
		t.Errorf("delivered = %q, want \"false\"", d["delivered"])
	}
	for _, k := range []string{"ack_received_at", "ack_latency_ms", "latency_violation"} {
		if v, ok := d[k]; ok {
			t.Errorf("undelivered estop recorded %q=%q; there was no acknowledgement to time", k, v)
		}
	}
	if _, ok := d["rpc_sent_at"]; !ok {
		t.Error("rpc_sent_at must be recorded even when undelivered — the attempt happened")
	}
}

// Every value is a string: audit.Entry.Detail is map[string]string and the chain hashes
// the serialised entry, so a number must not leak in unquoted (§9.6.5.2).
func TestEstopAudit_AllValuesAreStrings(t *testing.T) {
	d := estopTimingDetail(map[string]string{}, time.Now(),
		safety.Result{Delivered: true, Latency: 1500 * time.Millisecond, LatencyViolation: true})
	if d["latency_violation"] != "true" {
		t.Errorf("latency_violation = %q, want the string \"true\"", d["latency_violation"])
	}
	if d["ack_latency_ms"] != "1500" {
		t.Errorf("ack_latency_ms = %q, want the string \"1500\"", d["ack_latency_ms"])
	}
}

// A zone fan-out records WHICH robots were in scope, comma-separated with no spaces, plus
// the worst latency across the fan-out — the number the SLA turns on, since the SLA is
// breached if any one robot exceeds it.
func TestEstopAudit_ZoneFanOutRecordsRobotListAndWorstLatency(t *testing.T) {
	zone := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{
			Name: "zone-a", Namespace: "warehouse-a",
			Annotations: map[string]string{"swarmada.io/estop-triggered": "sensor trip"},
		},
		Status: fleetv1.FleetZoneStatus{IsLeaf: true},
	}
	robots := []*fleetv1.Robot{
		{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "warehouse-a"},
			Spec: fleetv1.RobotSpec{Zone: "zone-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "warehouse-a"},
			Spec: fleetv1.RobotSpec{Zone: "zone-a"}},
	}
	est := &timedEstopper{delivered: true, latency: 120 * time.Millisecond}
	spy := &auditSpy{}
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithObjects(zone, robots[0], robots[1]).Build()
	r := &ZoneEstopReconciler{Client: c, Estopper: est, Audit: spy}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "zone-a", Namespace: "warehouse-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var entry *audit.Entry
	for i := range spy.entries {
		if spy.entries[i].EventType == audit.EventEstopTriggered {
			entry = &spy.entries[i]
		}
	}
	if entry == nil {
		t.Fatalf("no ESTOP_TRIGGERED entry recorded; got %v", spy.typesRecorded())
	}
	list := entry.Detail["robots_in_scope"]
	if list == "" {
		t.Fatal("robots_in_scope is empty; the entry does not say which robots were stopped")
	}
	if strings.Contains(list, ", ") {
		t.Errorf("robots_in_scope = %q; the documented encoding is comma-separated with NO spaces", list)
	}
	for _, want := range []string{"r1", "r2"} {
		if !strings.Contains(list, want) {
			t.Errorf("robots_in_scope = %q, missing %q", list, want)
		}
	}
	if entry.Detail["max_ack_latency_ms"] != "120" {
		t.Errorf("max_ack_latency_ms = %q, want \"120\"", entry.Detail["max_ack_latency_ms"])
	}
}
