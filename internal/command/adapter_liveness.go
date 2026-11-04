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

package command

// Periodic adapter liveness — the FLOOR under ADR-0026's throttle (ADR-0054).
//
// ADR-0026 projects Fleet Adapter liveness onto every served Robot's
// status.connectivity.lastSeenAt and throttles the fan-out to at most one write per
// heartbeatTimeout/2, on the stated assumption that "the fan-out fires on every
// heartbeat". Nothing sent heartbeats on a cadence. HeartbeatRequest is
// control-plane-initiated by contract (proto/fleet_adapter/v1: "the control plane pushes
// HeartbeatRequest, the adapter replies HeartbeatResponse" — there is no unsolicited
// adapter→control-plane arm), and its only sender was [Dispatcher.Heartbeat], called only
// from the Robot controller's confirming exchange, reached only from the
// `age > heartbeatTimeout` branch. So the clock was refreshed only at the moment the
// control plane had ALREADY decided the robot looked stale: a throttle that was a ceiling
// with no floor. A robot answering every heartbeat it was asked spent about a third of its
// life with a lastSeenAt older than the offline threshold it is compared against.
//
// This is the floor. Every verified ControlStream gets a ticker that pushes an
// adapter-scoped HeartbeatRequest; the adapter's HeartbeatResponse drives the existing
// controlstream.AdapterPresence.AdapterHeartbeat path, which re-asserts
// FleetAdapter.status.lastHeartbeat and fans lastSeenAt out to served Robots under the
// unchanged ADR-0026 throttle. Nothing downstream of the beat changed.
//
// RA-1: this writes no status itself, and what it drives is the adapter PRESENCE path
// (RFC-0001 #fleetadapter-status-fields-and-phase-machine) — the path RA-1 names as the licensed alternative to the telemetry
// tick, and which internal/controlstream's dispatch already calls out as "deliberately NOT
// the telemetry path ... a Heartbeat proves the adapter is alive". The resulting writes are
// exactly the ones ADR-0026 budgeted for: change-gated in mutateStatus, throttled per Robot
// in projectLiveness. No telemetry frame gained a status write.

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

const (
	// defaultAdapterHeartbeatInterval mirrors the CRD default of
	// FleetAdapter.spec.heartbeatIntervalSeconds, applied when the adapter object cannot
	// be read or leaves the field unset. Duplicated rather than imported from
	// internal/controller: this package is a dependency of that one's wiring, not the
	// other way round, and a constant is cheaper than an import edge.
	defaultAdapterHeartbeatInterval = 10 * time.Second

	// minAdapterBeatPeriod floors the push cadence. spec.heartbeatIntervalSeconds has a
	// CRD Minimum of 1, so halving it can reach 500ms; one second is the fastest this
	// path will probe an adapter regardless.
	minAdapterBeatPeriod = time.Second
)

// beatPeriod is how often an adapter-scoped HeartbeatRequest is pushed down one live
// ControlStream: HALF the adapter's spec.heartbeatIntervalSeconds.
//
// Half, not one whole interval, and the halving is load-bearing. The FleetAdapter
// staleness backstop moves a Connected adapter to Degraded at
// `now - lastHeartbeat >= interval` and re-evaluates on a requeue of exactly `interval`
// (internal/controller/fleetadapter_controller.go). Probing on the same period would put
// the comparison on its own boundary, where the round trip and the status write are enough
// to trip it — and a Degraded adapter is one the RobotAdmissionGate refuses to create
// Robots behind. At interval/2 a live adapter's lastHeartbeat is never much more than half
// an interval old, so Degraded means what RFC-0001 #fleetadapter-status-fields-and-phase-machine says it means: at least one probe was
// actually missed. It is the same shape as ADR-0026's own "at most once per
// heartbeatTimeout/2" for the Robot fan-out.
//
// Re-read every tick from the informer cache so an operator raising or lowering
// spec.heartbeatIntervalSeconds takes effect within one period, without restarting the
// stream. A read failure keeps the default rather than stopping the cadence: losing the
// tunable must not cost the adapter its liveness.
func (d *Dispatcher) beatPeriod(ctx context.Context, namespace, adapter string) time.Duration {
	interval := defaultAdapterHeartbeatInterval
	if d.Client != nil && namespace != "" && adapter != "" {
		fa := &fleetv1.FleetAdapter{}
		if err := d.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: adapter}, fa); err == nil {
			if s := fa.Spec.HeartbeatIntervalSeconds; s > 0 {
				interval = time.Duration(s) * time.Second
			}
		}
	}
	if p := interval / 2; p > minAdapterBeatPeriod {
		return p
	}
	return minAdapterBeatPeriod
}

// startAdapterBeats runs the liveness cadence for one registered ControlStream and returns
// a stop func. It is started by [Dispatcher.RegisterStream] and stopped by the deregister
// that call returns, so a beat never outlives the stream it rides on.
//
// The pushed HeartbeatRequest names NO robot. It asks "is this adapter alive", which is a
// different question from "is robot X alive" — the per-robot confirming exchange in
// [Dispatcher.Heartbeat] still asks that one, still names its robot, and is untouched here.
// An adapter-scoped probe is also what makes the cost per adapter rather than per robot:
// one exchange refreshes FleetAdapter.status.lastHeartbeat and, through projectLiveness,
// every Robot that adapter serves.
func (d *Dispatcher) startAdapterBeats(identity controlstream.TLSIdentity, sender CommandSender) func() {
	if sender == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			timer := time.NewTimer(d.beatPeriod(ctx, identity.Namespace, identity.AdapterName))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if err := sender.Send(&fav1.ControlPlaneMessage{
				Payload: &fav1.ControlPlaneMessage_Heartbeat{Heartbeat: &fav1.HeartbeatRequest{
					SentAtMs: d.clock().UnixMilli(),
				}},
			}); err != nil {
				// The stream is going away. Stop rather than spin: the ControlStream
				// receive loop will deregister, AdapterDisconnected fires, and the
				// staleness backstop declares Disconnected if it does not. Logging here
				// would duplicate that path's record of the same fact.
				cancel()
				return
			}
		}
	}()
	return cancel
}
