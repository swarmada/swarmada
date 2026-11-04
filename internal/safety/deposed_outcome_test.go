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
	"sync/atomic"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A control-plane process sends on an adapter's streams only while it holds leadership, and one
// that has lost it writes no stop outcome (RFC-0001 #safety-robot-stop-table, Rules for every S
// row): the leader that took over derives the stops from their carriers. The stop itself still
// goes out: a stop is safe whoever sends it.
func TestDeposedProcess_SendsTheStopAndWritesNoOutcome(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	var leading atomic.Bool
	leading.Store(true)
	d.Term = func() (uint64, bool) { return 4, leading.Load() }
	sender := &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		leading.Store(false) // leadership lost while the stop is in flight
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}}
	d.RegisterStream(identity("acme"), sender)

	if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot, Carrier{}); err != nil {
		t.Fatalf("TriggerEstop: %v", err)
	}
	if sender.sent.Load() != 1 {
		t.Fatalf("estops sent = %d, want 1: a stop is sent whoever sends it", sender.sent.Load())
	}
	if got := estopState(t, c); got != "" {
		t.Fatalf("a process that no longer leads wrote estopState %q", got)
	}
}

// A held stop re-sent on a new SafetyStream whose window ends after this process lost its
// leadership records no metric and no ESTOP_TRIGGERED entry (a deposed process records no failed
// re-send).
func TestDeposedProcess_RecordsNothingForAResendsAnswer(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	spy := &syncAuditSpy{}
	d.Audit = spy
	var leading atomic.Bool
	leading.Store(true)
	d.Term = func() (uint64, bool) { return 4, leading.Load() }
	setState(t, c, fleetv1.RobotEstopFailed)

	before := commandsCounter(metrics.ResultTimeout)
	s := newRecordingSender()
	d.RegisterStream(identity("acme"), s)
	heldStopResent(t, s) // never answered
	leading.Store(false)

	time.Sleep(2 * (d.deliveryTimeout + d.confirmTimeout))
	if got := spy.triggered(t, 0); len(got) != 0 {
		t.Fatalf("a process that no longer leads audited the re-send: %+v", got)
	}
	if got := commandsCounter(metrics.ResultTimeout) - before; got != 0 {
		t.Fatalf("a process that no longer leads counted the re-send (timeout delta %v)", got)
	}
}
