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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// commandsCounter reads the current value of the estop_commands_total series for
// (ns, acme, robot, result).
func commandsCounter(result string) float64 {
	return testutil.ToFloat64(metrics.EstopCommandsTotal.WithLabelValues(ns, "acme", string(metrics.ScopeRobot), result))
}

// A confirmed STOPPED estop increments estop_commands_total{result=ack_stopped}.
func TestEstopMetrics_ConfirmedStoppedCounted(t *testing.T) {
	d, _, _ := newDispatcher(t, "acme")
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}}
	d.RegisterStream(identity("acme"), sender)

	before := commandsCounter(metrics.ResultAckStopped)
	if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot); err != nil {
		t.Fatalf("TriggerEstop: %v", err)
	}
	if got := commandsCounter(metrics.ResultAckStopped) - before; got != 1 {
		t.Errorf("estop_commands_total{ack_stopped} delta = %v, want 1", got)
	}
}

// A dropped estop (no EstopAck within the delivery window) increments
// estop_commands_total{result=timeout} — the "Command that never acked" signal.
func TestEstopMetrics_DroppedCountedAsTimeout(t *testing.T) {
	d, _, _ := newDispatcher(t, "acme")
	sender := &scriptedSender{d: d, reply: nil} // never acks
	d.RegisterStream(identity("acme"), sender)

	before := commandsCounter(metrics.ResultTimeout)
	if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot); err != ErrUndelivered {
		t.Fatalf("err = %v, want ErrUndelivered", err)
	}
	if got := commandsCounter(metrics.ResultTimeout) - before; got != 1 {
		t.Errorf("estop_commands_total{timeout} delta = %v, want 1", got)
	}
}

// An estop ACK that breaches the 500ms SLA increments the violations counter
// alongside the existing EstopLatencyViolation event.
func TestEstopMetrics_LatencyViolationCounted(t *testing.T) {
	d, _, _ := newDispatcher(t, "acme")
	base := time.Unix(2000, 0)
	calls := 0
	d.now = func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(600 * time.Millisecond) // 600ms → SLA breach
	}
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}}
	d.RegisterStream(identity("acme"), sender)

	violations := metrics.EstopLatencyViolationsTotal.WithLabelValues(ns, "acme", "amr-1")
	before := testutil.ToFloat64(violations)
	if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot); err != nil {
		t.Fatalf("TriggerEstop: %v", err)
	}
	if got := testutil.ToFloat64(violations) - before; got != 1 {
		t.Errorf("estop_latency_violations_total delta = %v, want 1", got)
	}
}
