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
	"time"
)

// Sample is one high-cadence telemetry observation flattened for a time-series
// sink. Every per-tick numeric the control plane tracks — position, battery,
// latency, per-component health — is a Sample, never a write to Robot.status
// (RA-1: etcd is a configuration store, not a telemetry sink).
type Sample struct {
	// RobotID is the robot the sample belongs to (also present in Labels).
	RobotID string
	// Timestamp is the reading time.
	Timestamp time.Time
	// Metric is the time-series name, e.g. "robot_battery_percent".
	Metric string
	// Value is the numeric reading.
	Value float64
	// Labels are the series labels (robot_id, and any future zone/class labels).
	Labels map[string]string
}

// TSDBWriter is the pluggable sink for high-cadence telemetry. Implementations
// target a CNCF-idiomatic time-series store (Prometheus remote-write,
// VictoriaMetrics, Mimir, ...); the control plane never hard-binds one vendor.
type TSDBWriter interface {
	// WriteSamples persists a batch of samples. It MUST be safe for concurrent
	// use and SHOULD be non-blocking or buffered, so a slow sink cannot stall the
	// ControlStream ingestion path.
	WriteSamples(ctx context.Context, samples []Sample) error
}

// NoopWriter discards all samples. It is the safe default when no telemetry sink
// is configured: telemetry is dropped rather than forced onto etcd.
type NoopWriter struct{}

// WriteSamples implements [TSDBWriter] by discarding the batch.
func (NoopWriter) WriteSamples(_ context.Context, _ []Sample) error { return nil }
