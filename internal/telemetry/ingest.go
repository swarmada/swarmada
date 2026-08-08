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

package telemetry

import (
	"context"
	"strings"

	"github.com/swarmada/swarmada/internal/metrics"
)

// namespaceOf extracts the Kubernetes namespace from a "namespace/name" RobotID,
// returning "" when the id is not namespaced.
func namespaceOf(robotID string) string {
	if i := strings.IndexByte(robotID, '/'); i >= 0 {
		return robotID[:i]
	}
	return ""
}

// StatusSink applies a material status transition to Robot.status. The Robot
// controller implements it as a throttled status patch; the telemetry path
// invokes it only when the [Projector] has decided a write is warranted, so the
// sink never sees high-cadence churn.
type StatusSink interface {
	// ApplyMaterialUpdate writes the changed fields of upd onto the robot's
	// status. Only non-nil fields of the update are applied.
	ApplyMaterialUpdate(ctx context.Context, upd MaterialUpdate) error
}

// PositionObserver is a per-frame subscriber to the ingest fan-out, used by the
// Zone Controller to derive Robot.status.currentZone from position telemetry
// (RFC-0001 §9.3.4, §9.3.7 — "the Zone Controller's position subscription,
// consumed upstream of any sink"). It is invoked on every frame, independent of
// the TSDB and status planes. Implementations MUST be safe for concurrent use and
// MUST NOT write Robot.status except on a material transition (a zone change) —
// never per tick (RA-1).
type PositionObserver interface {
	// ObservePosition consumes one telemetry frame for zone derivation.
	ObservePosition(ctx context.Context, f Frame)
}

// Ingestor is the fan-out point the ControlStream server calls once per
// TelemetryPayload (translated to a [Frame]). It enforces the two-plane split:
//
//   - the frame is offered to the PositionObserver (zone derivation), upstream of
//     any sink;
//   - the full sample set always goes to the TSDB (high cadence, never etcd);
//   - the frame is run through the Projector, and only a material transition is
//     forwarded to the StatusSink (low cadence, etcd).
type Ingestor struct {
	// TSDB receives every frame's flattened samples.
	TSDB TSDBWriter
	// Projector decides which frames are material.
	Projector *Projector
	// Sink writes material transitions to Robot.status; may be nil to disable the
	// status plane entirely (TSDB-only).
	Sink StatusSink
	// PositionObserver, when non-nil, receives every frame for zone derivation. It
	// is consumed upstream of the TSDB and Sink, so zone derivation is independent
	// of sink configuration (§9.3.7 Invariant 2).
	PositionObserver PositionObserver
}

// NewIngestor wires an Ingestor. A nil tsdb defaults to [NoopWriter], so an
// unconfigured telemetry sink drops samples rather than forcing them onto etcd.
func NewIngestor(tsdb TSDBWriter, projector *Projector, sink StatusSink) *Ingestor {
	if tsdb == nil {
		tsdb = NoopWriter{}
	}
	return &Ingestor{TSDB: tsdb, Projector: projector, Sink: sink}
}

// Ingest handles one telemetry frame. It always writes the high-cadence samples
// to the TSDB, then writes to Robot.status only when the frame is a material
// transition. The two planes fail independently: a TSDB error is returned but
// does not suppress the material status write.
func (i *Ingestor) Ingest(ctx context.Context, f Frame) error {
	// Zone derivation consumes the frame upstream of any sink, so it is unaffected
	// by TSDB sink state (§9.3.7 Invariant 2). It writes Robot.status only on a
	// zone transition, never per tick (RA-1).
	if i.PositionObserver != nil {
		i.PositionObserver.ObservePosition(ctx, f)
	}

	tsdbErr := i.TSDB.WriteSamples(ctx, f.Samples())
	if tsdbErr != nil {
		// The frame's samples were not forwardable to the TSDB sink (§9.3.8). This
		// never fires for a Drop/unset sink (NoopWriter never errors); the sink also
		// records the classified tsdb_write_errors on its side.
		metrics.IncTelemetryDroppedFrame(namespaceOf(f.RobotID), f.Adapter)
	}
	if upd := i.Projector.Project(f); upd != nil && i.Sink != nil {
		if err := i.Sink.ApplyMaterialUpdate(ctx, *upd); err != nil {
			return err
		}
		// Count the material Robot.status write by its transition type (§9.3.8).
		// This only fires on a material transition — never per telemetry tick — so
		// the counter is the observable proof of the RA-1 material-transition gate.
		metrics.IncTelemetryStatusWrite(namespaceOf(upd.RobotID), upd.TransitionType)
	}
	return tsdbErr
}
