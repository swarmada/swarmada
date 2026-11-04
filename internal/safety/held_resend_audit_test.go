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

// A held stop sent again on a new SafetyStream is an estop Command like a first send: it is
// metered (estop_commands_total, with the latency histogram on an ack) and audited as
// ESTOP_TRIGGERED with the robot-scope timing fields (RFC-0001 safety.md, 500ms measurement
// and logging; the ESTOP_TRIGGERED row; control-plane.md, estop metrics).

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// syncAuditSpy is an audit.Recorder safe to call from the re-send's own goroutine.
type syncAuditSpy struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *syncAuditSpy) Record(e audit.Entry) (audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return e, nil
}

// triggered waits for the ESTOP_TRIGGERED entries to reach want.
func (a *syncAuditSpy) triggered(t *testing.T, want int) []audit.Entry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		var got []audit.Entry
		for _, e := range a.entries {
			if e.EventType == audit.EventEstopTriggered {
				got = append(got, e)
			}
		}
		a.mu.Unlock()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// resendLatencyCount is the number of latency observations for amr-1's robot-scope stops.
func resendLatencyCount(t *testing.T) uint64 {
	t.Helper()
	var m dto.Metric
	o := metrics.EstopCommandLatencySeconds.WithLabelValues(ns, "acme", "amr-1", string(metrics.ScopeRobot))
	if err := o.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("read the latency histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestHeldStop_ResendIsMeteredAndAudited(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	spy := &syncAuditSpy{}
	d.Audit = spy
	setState(t, c, fleetv1.RobotEstopStopped)

	before := commandsCounter(metrics.ResultAckStopped)
	beforeLatency := resendLatencyCount(t)
	s := newRecordingSender()
	d.RegisterStream(identity("acme"), s)
	m := heldStopResent(t, s)
	if err := d.RouteAck(identity("acme"), s, "amr-1",
		ackState(m.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatalf("the ack for the held stop was refused: %v", err)
	}

	got := spy.triggered(t, 1)
	if len(got) != 1 {
		t.Fatalf("ESTOP_TRIGGERED entries for the re-send = %d, want 1", len(got))
	}
	e := got[0]
	if e.Outcome != audit.OutcomeAllowed || e.Resource.Name != "amr-1" {
		t.Fatalf("entry = %+v; want outcome Allowed on Robot amr-1", e)
	}
	for _, k := range []string{"reason", "state", "rpc_sent_at", "delivered", "ack_received_at", "ack_latency_ms", "latency_violation"} {
		if _, ok := e.Detail[k]; !ok {
			t.Errorf("detail lacks %q: %v", k, e.Detail)
		}
	}
	if e.Detail["state"] != string(fleetv1.RobotEstopStopped) || e.Detail["delivered"] != "true" {
		t.Errorf("detail state/delivered = %q/%q, want Stopped/true", e.Detail["state"], e.Detail["delivered"])
	}
	if got := commandsCounter(metrics.ResultAckStopped) - before; got != 1 {
		t.Errorf("estop_commands_total{ack_stopped} delta = %v, want 1", got)
	}
	if got := resendLatencyCount(t) - beforeLatency; got != 1 {
		t.Errorf("estop_command_latency_seconds observations delta = %d, want 1", got)
	}
}

func TestHeldStop_UnansweredResendIsMeteredAndAuditedAsError(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	spy := &syncAuditSpy{}
	d.Audit = spy
	setState(t, c, fleetv1.RobotEstopFailed)

	before := commandsCounter(metrics.ResultTimeout)
	s := newRecordingSender()
	d.RegisterStream(identity("acme"), s)
	heldStopResent(t, s) // never answered

	got := spy.triggered(t, 1)
	if len(got) != 1 {
		t.Fatalf("ESTOP_TRIGGERED entries for the unanswered re-send = %d, want 1", len(got))
	}
	if got[0].Outcome != audit.OutcomeError || got[0].Detail["delivered"] != "false" {
		t.Fatalf("entry = %+v; want outcome Error, delivered false", got[0])
	}
	if got := commandsCounter(metrics.ResultTimeout) - before; got != 1 {
		t.Errorf("estop_commands_total{timeout} delta = %v, want 1", got)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
		t.Fatalf("estopState = %s; the re-send writes no state", got)
	}
}
