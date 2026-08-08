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
	"testing"
	"time"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"

	"github.com/swarmada/swarmada/internal/audit"
)

// ESTOP_LATENCY_VIOLATION (§9.6.5.1) sealed into the tamper-evident chain, alongside the
// Kubernetes Event and the Prometheus counter that already existed.
//
// An Event is subject to namespace retention and sits outside the hash chain, so on its own
// it cannot evidence that an adapter missed the 500 ms guarantee. That mattered because the
// breach is a durable conformance fact about a specific adapter build, not a transient blip.

type auditSpy struct {
	entries []audit.Entry
	err     error
}

func (a *auditSpy) Record(e audit.Entry) (audit.Entry, error) {
	if a.err != nil {
		return audit.Entry{}, a.err
	}
	a.entries = append(a.entries, e)
	return e, nil
}

// slowAckDispatcher wires a dispatcher whose ack arrives `latency` after the send.
func slowAckDispatcher(t *testing.T, latency time.Duration) (*Dispatcher, *auditSpy) {
	t.Helper()
	d, _, _ := newDispatcher(t, "acme")
	spy := &auditSpy{}
	d.Audit = spy
	base := time.Unix(1000, 0)
	calls := 0
	d.now = func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(latency)
	}
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}}
	d.RegisterStream(identity("acme"), sender)
	return d, spy
}

func TestAuditEstop_LatencyViolationIsSealed(t *testing.T) {
	d, spy := slowAckDispatcher(t, 600*time.Millisecond)

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator")
	if err != nil || !res.LatencyViolation {
		t.Fatalf("res=%+v err=%v; want a latency violation", res, err)
	}
	if len(spy.entries) != 1 {
		t.Fatalf("want exactly one chain entry, got %d", len(spy.entries))
	}
	e := spy.entries[0]
	if e.EventType != audit.EventEstopLatencyViolation {
		t.Fatalf("event type = %q", e.EventType)
	}
	if e.Resource.Kind != "Robot" || e.Resource.Name != "amr-1" {
		t.Fatalf("entry must name the robot, got %+v", e.Resource)
	}
	if e.Detail["latency_ms"] != "600" {
		t.Errorf("latency_ms = %q, want 600", e.Detail["latency_ms"])
	}
	// adapter_version is what lets a pattern of violations be attributed to one build
	// rather than to the fleet.
	if _, ok := e.Detail["adapter_version"]; !ok {
		t.Error("adapter_version is a required detail field")
	}
	// Allowed, not Error: the stop was delivered and acknowledged. What breached was the
	// timing guarantee — an entry marked Error would read as a failed emergency stop.
	if e.Outcome != audit.OutcomeAllowed {
		t.Errorf("outcome = %q, want Allowed (the stop succeeded; the SLA did not)", e.Outcome)
	}
}

func TestAuditEstop_WithinSLASealsNothing(t *testing.T) {
	// The chain records breaches, not every estop — ESTOP_TRIGGERED already covers the
	// stop itself. An entry per estop would drown the violations it exists to surface.
	d, spy := slowAckDispatcher(t, 100*time.Millisecond)

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator")
	if err != nil || res.LatencyViolation {
		t.Fatalf("res=%+v err=%v; want no violation", res, err)
	}
	if len(spy.entries) != 0 {
		t.Fatalf("an in-SLA estop must seal no violation entry, got %d", len(spy.entries))
	}
}

func TestAuditEstop_SinkFailureDoesNotAffectTheStop(t *testing.T) {
	// THE SAFETY-CRITICAL DIRECTION. The seal runs after the acknowledgement, so the robot
	// is already stopping; a failing sink must not change the result the caller sees.
	d, spy := slowAckDispatcher(t, 600*time.Millisecond)
	spy.err = errors.New("sink unavailable")

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator")
	if err != nil {
		t.Fatalf("a failing audit sink must not fail the estop: %v", err)
	}
	if !res.Confirmed || !res.Delivered {
		t.Fatalf("a slow but confirmed stop must stay Confirmed/Delivered, got %+v", res)
	}
}

func TestAuditEstop_NilRecorderIsSafe(t *testing.T) {
	d, _ := slowAckDispatcher(t, 600*time.Millisecond)
	d.Audit = nil
	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator")
	if err != nil || !res.Confirmed {
		t.Fatalf("nil Audit must not change behaviour: res=%+v err=%v", res, err)
	}
}
