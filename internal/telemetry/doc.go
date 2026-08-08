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

// Package telemetry implements the two-data-plane split mandated by
// architectural-review finding RA-1 ("telemetry must not write to etcd at full
// cadence").
//
// # Why two planes
//
// A ControlStream (proto fleet_adapter.v1) delivers one TelemetryPayload per
// robot every telemetryIntervalSeconds (default 10s, can be 1s). Robot.status
// today carries the high-cadence fields Position, BatteryPercent, Hardware, and
// LastHeartbeat. If the control plane projected every telemetry frame onto
// Robot.status, each write would traverse the API server into etcd — Raft
// consensus, watch fan-out, and compaction — once per robot per tick. At fleet
// scale that is a write-amplification cascade that caps the fleet at an untested
// ceiling. etcd is a configuration store, not a telemetry sink.
//
// This package splits the planes:
//
//   - High-cadence telemetry (position, battery, latency, per-component metrics)
//     goes to a time-series database via [TSDBWriter], never etcd.
//   - Robot.status becomes a THROTTLED PROJECTION of material state only,
//     written by [Projector] solely on a material transition (phase change,
//     battery-bucket crossing, hardware health change, assigned-action change),
//     never on the raw stream. Live pose is served from the TSDB; etcd holds at
//     most a coarse, heavily-throttled last-known value. Robot.status is coarse
//     and eventually-consistent BY DESIGN (see RFC-0001 §7 Drawbacks).
//
// Liveness is kept off the write path too: LastHeartbeat is tracked in the
// pipeline, and only the LOSS of liveness (Phase -> Offline) yields a status
// write. The Health Monitor reads from the pipeline, not from etcd timestamps.
//
// # Integration seam (TelemetryPayload -> Frame)
//
// This package is intentionally decoupled from the generated proto so the
// projection logic carries no proto dependency and is trivially unit-testable.
// The ControlStream server (control plane) is responsible for translating each
// wire TelemetryPayload into a [Frame] before calling [Ingestor.Ingest]:
//
//	frame := telemetry.Frame{
//	    RobotID:     p.GetRobotId(),
//	    Timestamp:   time.UnixMilli(p.GetTimestampMs()),
//	    Phase:       mapPhase(p.GetPhase()),   // proto RobotPhase -> fleetv1.RobotPhase
//	    BatteryPct:  p.GetBattery().Percent,   // *int32, proto3 explicit presence preserved
//	    Position:    mapPosition(p.GetPosition()),
//	    Hardware:    mapHardwareDeltas(p.GetHardware()), // delta set; merged by the Projector
//	    AssignedAction: p.GetCurrentAction(),
//	}
//
// The proto RobotPhase enum maps to the CRD RobotPhase as: IDLE->Idle,
// ASSIGNED/IN_PROGRESS->Busy, CHARGING->Charging, ERROR/MAINTENANCE->Degraded,
// OFFLINE->Offline, DISCOVERED->Pending, UNSPECIFIED->"" (leave unchanged). The
// proto Hardware field is delta-compressed (only changed components, all on
// reconnect); the Projector merges deltas onto its last-known full map, so the
// server passes the delta through unchanged.
//
// The live wiring (ControlStream server -> Ingestor, and a controller-backed
// [StatusSink] performing the throttled status patch) is added when the
// ControlStream server itself lands; until then this package stands alone with
// its projection logic verified by unit tests.
package telemetry
