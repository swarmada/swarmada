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

// Parallel estop fan-out (RFC-0001 §9.6.2.1), shared by the zone- and
// namespace-scoped reconcilers.
//
// §9.6.2.1 requires the control plane to "send an estop Command to every in-scope robot's
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
	reason, issuedBy string, scope metrics.EstopScope) ([]estopOutcome, time.Duration) {
	out := make([]estopOutcome, len(robots))
	start := time.Now()

	var wg sync.WaitGroup
	for i := range robots {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			res, err := est.TriggerEstop(ctx, namespace, name, reason, issuedBy, scope)
			// Each goroutine owns exactly one slot, so no lock is needed and the result
			// lands at the robot's original index regardless of completion order.
			out[i] = estopOutcome{robot: name, result: res, err: err}
		}(i, robots[i].Name)
	}
	wg.Wait()

	return out, time.Since(start)
}
