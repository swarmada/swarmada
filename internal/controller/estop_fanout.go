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
	"sync"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

// Parallel estop fan-out (RFC-0001 #safety-estop-scopes), shared by the zone- and
// namespace-scoped reconcilers.
//
// RFC-0001 #safety-estop-scopes requires the control plane to "send an estop Command to every in-scope robot's
// Fleet Adapter in parallel — not sequentially — so that each command is issued
// independently and a slow acknowledgement from one robot does not delay the estop signal
// to others." Both reconcilers previously looped, awaiting each robot's full outcome before
// sending to the next. On a 50-robot zone that put the last robot's SEND tens of seconds
// after the operator's trigger, while every per-robot round trip still measured healthy —
// see ADR-0042 and swarmada_estop_fanout_duration_seconds.
//
// WHY UNBOUNDED, one goroutine per robot, rather than a worker pool. The expensive part of
// TriggerEstop is not the send; it is waiting for the EstopAck (up to the delivery timeout,
// then the confirm timeout). A pool of size N means robot N+1's SEND waits on some other
// robot's ACK — the same defect at a coarser granularity, just with a larger fleet needed
// to expose it. A bound would be a resource guard purchased with the property this function
// exists to provide.
//
// What that costs, and why it is affordable. Each goroutine performs one robot Get, one
// SafetyStream send, a bounded wait, and one status patch. The actual wire writes are
// already serialised per adapter by the SafetySender's mutex (internal/controlstream:
// "Multiple TriggerEstop calls for different robots on the same adapter may send
// concurrently; the mutex keeps the gRPC Send safe"), so no single stream is overrun. Total
// wall clock becomes the SLOWEST robot rather than the SUM, which is the point: a fan-out
// now completes within one confirm-timeout regardless of fleet size.
//
// If a fleet ever outgrows this, the answer is to split TriggerEstop into a send phase and a
// collect phase — issue every send, then gather every ack — NOT to reintroduce a pool.
// The full reasoning, including the bound-by-adapter variant and why it fails the same way,
// is in ADR-0043.

// estopOutcome is one robot's fan-out result. Collected positionally so aggregation is
// deterministic no matter what order the goroutines finish in: two runs over the same fleet
// must produce the same counts, the same worst latency, and the same audit entry.
type estopOutcome struct {
	robot  string
	result safety.Result
	err    error
	// recErr is the error writing the robot's hold record, when the fan-out records one
	// (estopFanoutHeld).
	recErr error
}

// estopFanout issues a confirmed estop to every named robot concurrently and returns the
// outcomes in the SAME ORDER as robots, plus the wall-clock duration of the whole episode.
//
// The duration spans from before the first send to after the last robot resolves, which is
// exactly the interval swarmada_estop_fanout_duration_seconds exists to measure — the one a
// per-robot latency histogram structurally cannot see.
//
// Errors are returned per robot rather than aborting: an estop fan-out must attempt EVERY
// robot in scope. One unreachable adapter must never prevent the rest of the zone stopping,
// which is the same fail-open-on-attempt / fail-closed-on-confirmation split TriggerEstop
// itself makes (a robot it cannot confirm resolves to Failed, never Stopped).
func estopFanout(ctx context.Context, est ZoneEstopper, namespace string, robots []fleetv1.Robot,
	reason, issuedBy string, scope metrics.EstopScope, carrier safety.Carrier) ([]estopOutcome, time.Duration) {
	return estopFanoutHeld(ctx, est, namespace, robots, reason, issuedBy, scope, carrier, nil)
}

// estopFanoutHeld is estopFanout that also records, for each robot, that the stop holds it
// (record, run in the robot's own goroutine once its stop has resolved, whatever the outcome:
// a Failed stop holds the robot as a Stopped one does). The record is written after the send,
// never before, so it adds nothing to the stop's latency. A nil record records nothing.
func estopFanoutHeld(ctx context.Context, est ZoneEstopper, namespace string, robots []fleetv1.Robot,
	reason, issuedBy string, scope metrics.EstopScope, carrier safety.Carrier,
	record func(ctx context.Context, robot string) error) ([]estopOutcome, time.Duration) {
	ep := startEstopEpisode(ctx, est, namespace, reason, issuedBy, scope, carrier, record)
	ep.launch(robots)
	out, _ := ep.wait()
	return out, time.Since(ep.start)
}

// estopEpisode is one zone's or namespace's fan-out, launched in more than one batch: the
// robots the cache already places in the stop's scope are sent at once, and the robots a read
// past the cache adds are sent when that read returns (row S1: nothing is read, written or
// awaited before a send). Each robot is launched once, in a goroutine of its own, whatever batch
// names it again; wait joins them all.
type estopEpisode struct {
	ctx       context.Context
	est       ZoneEstopper
	namespace string
	reason    string
	issuedBy  string
	scope     metrics.EstopScope
	carrier   safety.Carrier
	record    func(ctx context.Context, robot string) error
	start     time.Time

	mu       sync.Mutex
	wg       sync.WaitGroup
	out      []*estopOutcome
	resolved []time.Time // when each robot's TriggerEstop returned: its outcome written
	launched map[string]bool
}

func startEstopEpisode(ctx context.Context, est ZoneEstopper, namespace, reason, issuedBy string,
	scope metrics.EstopScope, carrier safety.Carrier, record func(ctx context.Context, robot string) error) *estopEpisode {
	return &estopEpisode{ctx: ctx, est: est, namespace: namespace, reason: reason, issuedBy: issuedBy,
		scope: scope, carrier: carrier, record: record, start: time.Now(), launched: map[string]bool{}}
}

// launch sends the stop to every robot not launched yet, each in its own goroutine.
func (e *estopEpisode) launch(robots []fleetv1.Robot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range robots {
		name := robots[i].Name
		if e.launched[name] {
			continue
		}
		e.launched[name] = true
		slot := &estopOutcome{robot: name}
		e.out = append(e.out, slot)
		e.resolved = append(e.resolved, time.Time{})
		at := len(e.resolved) - 1
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			res, err := e.est.TriggerEstop(e.ctx, e.namespace, name, e.reason, e.issuedBy, e.scope, e.carrier)
			done := time.Now()
			var recErr error
			if e.record != nil {
				recErr = e.record(e.ctx, name)
			}
			e.mu.Lock()
			*slot = estopOutcome{robot: name, result: res, err: err, recErr: recErr}
			e.resolved[at] = done
			e.mu.Unlock()
		}()
	}
}

// wait joins every robot launched and returns the outcomes in launch order, and when the last
// robot resolved: its TriggerEstop returned, so its outcome was written. The hold records,
// written after, are not part of it. An episode that launched nothing resolved at its start.
func (e *estopEpisode) wait() ([]estopOutcome, time.Time) {
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]estopOutcome, len(e.out))
	last := e.start
	for i, o := range e.out {
		out[i] = *o
		if e.resolved[i].After(last) {
			last = e.resolved[i]
		}
	}
	return out, last
}

// carrierOf is the stop carrier a zone's or the namespace's hold names: the stop's record on
// the robot names the same object.
func carrierOf(h holdRef) safety.Carrier {
	return safety.Carrier{Kind: fleetv1.StopCarrierKind(h.kind), Name: h.name, UID: h.uid}
}
