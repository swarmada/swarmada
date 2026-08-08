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
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/golang/snappy"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/swarmada/swarmada/internal/metrics"
)

// Sink type label values (§9.1.11.1 / §9.3.8 sink_type). Kept as strings so this
// package does not depend on the api/v1 config type.
const (
	SinkUnset                 = ""
	SinkDrop                  = "Drop"
	SinkPrometheusRemoteWrite = "PrometheusRemoteWrite"
	SinkVictoriaMetrics       = "VictoriaMetrics"
	SinkMimir                 = "Mimir"
)

// NewSink selects a [TSDBWriter] from the configured sink type (§9.1.11.1). An
// unset or explicit Drop sink discards high-cadence telemetry ([NoopWriter], never
// forced onto etcd, RA-1); the three real stores all speak the Prometheus
// remote-write protocol, so one [RemoteWriteSink] serves them.
func NewSink(sinkType, endpoint string) TSDBWriter {
	switch sinkType {
	case SinkUnset, SinkDrop:
		return NoopWriter{}
	default:
		return NewRemoteWriteSink(endpoint, sinkType)
	}
}

// RemoteWriteSink writes high-cadence samples to a Prometheus-remote-write endpoint
// (Prometheus, VictoriaMetrics, or Mimir). A write is a single snappy-compressed
// protobuf WriteRequest POST. It is safe for concurrent use (net/http.Client is).
type RemoteWriteSink struct {
	endpoint string
	sinkType string
	client   *http.Client
}

var _ TSDBWriter = (*RemoteWriteSink)(nil)

// NewRemoteWriteSink builds a sink targeting endpoint, tagging errors with sinkType.
func NewRemoteWriteSink(endpoint, sinkType string) *RemoteWriteSink {
	return &RemoteWriteSink{
		endpoint: endpoint,
		sinkType: sinkType,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// WriteSamples POSTs the batch as a Prometheus remote-write request. On any failure
// it records swarmada_telemetry_tsdb_write_errors_total (§9.3.8) and returns the
// error so the caller counts the dropped frame; it never blocks the ingest path
// beyond the client timeout.
func (s *RemoteWriteSink) WriteSamples(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	body := snappy.Encode(nil, encodeWriteRequest(samples))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		s.recordError(samples, errorClassUnknown)
		return fmt.Errorf("remote-write request: %w", err)
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		s.recordError(samples, classifyErr(err))
		return fmt.Errorf("remote-write POST %s: %w", s.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		s.recordError(samples, classifyStatus(resp.StatusCode))
		return fmt.Errorf("remote-write %s: status %d", s.endpoint, resp.StatusCode)
	}
	return nil
}

func (s *RemoteWriteSink) recordError(samples []Sample, class string) {
	ns := ""
	if len(samples) > 0 {
		ns = namespaceOf(samples[0].RobotID) // a batch is one frame = one robot = one namespace
	}
	metrics.IncTSDBWriteError(ns, s.sinkType, class)
}

// ── error classification (§9.3.8 error_class) ─────────────────────────────────

const (
	errorClassTimeout = "timeout"
	errorClassAuth    = "auth"
	errorClassSchema  = "schema"
	errorClassUnknown = "unknown"
)

func classifyStatus(code int) string {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return errorClassAuth
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		return errorClassSchema
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		return errorClassTimeout
	default:
		return errorClassUnknown
	}
}

func classifyErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return errorClassTimeout
	}
	var nerr interface{ Timeout() bool }
	if errors.As(err, &nerr) && nerr.Timeout() {
		return errorClassTimeout
	}
	return errorClassUnknown
}

// ── Prometheus remote-write WriteRequest encoding (protowire) ──────────────────
//
// Wire-compatible with prometheus/prompb (field numbers MUST match):
//
//	WriteRequest { repeated TimeSeries timeseries = 1; }
//	TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	Label        { string name = 1; string value = 2; }
//	Sample       { double value = 1; int64 timestamp = 2; }

func encodeWriteRequest(samples []Sample) []byte {
	var out []byte
	for i := range samples {
		out = protowire.AppendTag(out, 1, protowire.BytesType) // WriteRequest.timeseries
		out = protowire.AppendBytes(out, encodeTimeSeries(samples[i]))
	}
	return out
}

func encodeTimeSeries(s Sample) []byte {
	var ts []byte
	// __name__ is the metric name label, then the rest sorted for determinism.
	ts = appendLabel(ts, "__name__", s.Metric)
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		if k == "__name__" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ts = appendLabel(ts, k, s.Labels[k])
	}

	var sample []byte
	sample = protowire.AppendTag(sample, 1, protowire.Fixed64Type) // Sample.value (double)
	sample = protowire.AppendFixed64(sample, math.Float64bits(s.Value))
	sample = protowire.AppendTag(sample, 2, protowire.VarintType) // Sample.timestamp (ms)
	sample = protowire.AppendVarint(sample, uint64(s.Timestamp.UnixMilli()))
	ts = protowire.AppendTag(ts, 2, protowire.BytesType) // TimeSeries.samples
	ts = protowire.AppendBytes(ts, sample)
	return ts
}

func appendLabel(ts []byte, name, value string) []byte {
	var lbl []byte
	lbl = protowire.AppendTag(lbl, 1, protowire.BytesType)
	lbl = protowire.AppendString(lbl, name)
	lbl = protowire.AppendTag(lbl, 2, protowire.BytesType)
	lbl = protowire.AppendString(lbl, value)
	ts = protowire.AppendTag(ts, 1, protowire.BytesType) // TimeSeries.labels
	ts = protowire.AppendBytes(ts, lbl)
	return ts
}
