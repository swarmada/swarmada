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

// Package controlstream implements the control-plane side of the Fleet Adapter
// Protocol ControlStream (RFC-0001 §9.2). The control plane is the gRPC SERVER;
// each Fleet Adapter is the client and dials in, then opens a long-lived
// bidirectional ControlStream over which it multiplexes every robot it manages
// (§9.2.1). This package terminates that stream: it completes the
// AdapterHello/HelloAck handshake, dispatches Discover/Register to a pluggable
// [Registrar], and translates each TelemetryPayload into a
// [github.com/swarmada/swarmada/internal/telemetry.Frame] handed to a
// [TelemetryIngestor].
//
// # Seams (why this package carries no Kubernetes dependency)
//
// The server owns only stream framing, message ordering, and the send path. The
// two control-plane behaviours that need cluster state are behind interfaces so
// the streaming core is unit- and race-testable without envtest:
//
//   - [Registrar] answers Discover/Register (creating/looking up DiscoveredRobot
//     and Robot; §9.3.1). The Kubernetes-backed implementation is wired in a
//     later backlog step; until then [Server] uses a safe reject-all default.
//   - [TelemetryIngestor] receives one translated frame per TelemetryPayload.
//     [github.com/swarmada/swarmada/internal/telemetry.Ingestor] satisfies it
//     directly; the live feed (backlog §E, step 2) injects a real Ingestor wired
//     to the projector and TSDB. Until then telemetry frames are dropped, so no
//     Robot.status write ever happens on a telemetry tick (RA-1).
//
// # Scope of this stage
//
// This is the first ControlStream step: handshake, Discover/Register dispatch,
// and TelemetryPayload receipt/translation. SafetyStream (estop; §9.2.1, RA-6),
// pushed Commands (assignment/lease/pause/verify), heartbeat requests, and the
// per-message mTLS-SAN robot_id authorization (§9.2.7, backlog §F) are later
// items; message arms this stage does not yet drive are accepted and ignored.
package controlstream
