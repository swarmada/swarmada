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

package telemetry_test

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/telemetry"
)

type wireSeries struct {
	labels map[string]string
	value  float64
	tsMs   int64
}

// decodeWriteRequest snappy-decompresses and parses a Prometheus remote-write body
// back into series — proving the sink emits valid, wire-compatible remote-write.
func decodeWriteRequest(t *testing.T, body []byte) []wireSeries {
	t.Helper()
	raw, err := snappy.Decode(nil, body)
	if err != nil {
		t.Fatalf("snappy decode: %v", err)
	}
	var out []wireSeries
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		raw = raw[n:]
		if num == 1 && typ == protowire.BytesType { // WriteRequest.timeseries
			ts, m := protowire.ConsumeBytes(raw)
			raw = raw[m:]
			out = append(out, decodeTimeSeries(t, ts))
		} else {
			raw = raw[protowire.ConsumeFieldValue(num, typ, raw):]
		}
	}
	return out
}

func decodeTimeSeries(t *testing.T, b []byte) wireSeries {
	t.Helper()
	s := wireSeries{labels: map[string]string{}}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType: // Label
			lbl, m := protowire.ConsumeBytes(b)
			b = b[m:]
			name, value := decodeLabel(lbl)
			s.labels[name] = value
		case num == 2 && typ == protowire.BytesType: // Sample
			smp, m := protowire.ConsumeBytes(b)
			b = b[m:]
			s.value, s.tsMs = decodeSample(smp)
		default:
			b = b[protowire.ConsumeFieldValue(num, typ, b):]
		}
	}
	return s
}

func decodeLabel(b []byte) (name, value string) {
	for len(b) > 0 {
		num, _, n := protowire.ConsumeTag(b)
		b = b[n:]
		v, m := protowire.ConsumeString(b)
		b = b[m:]
		if num == 1 {
			name = v
		} else if num == 2 {
			value = v
		}
	}
	return name, value
}

func decodeSample(b []byte) (value float64, tsMs int64) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		b = b[n:]
		switch num {
		case 1:
			bits, m := protowire.ConsumeFixed64(b)
			b = b[m:]
			value = math.Float64frombits(bits)
		case 2:
			ts, m := protowire.ConsumeVarint(b)
			b = b[m:]
			tsMs = int64(ts)
		default:
			b = b[protowire.ConsumeFieldValue(num, typ, b):]
		}
	}
	return value, tsMs
}

// A sample round-trips through the sink to a remote-write endpoint as a valid,
// snappy-compressed protobuf WriteRequest with the correct headers.
func TestRemoteWriteSink_RoundTrip(t *testing.T) {
	var got []wireSeries
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header
		body, _ := io.ReadAll(r.Body)
		got = decodeWriteRequest(t, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sink := telemetry.NewRemoteWriteSink(srv.URL, telemetry.SinkPrometheusRemoteWrite)
	s := telemetry.Sample{
		RobotID: "warehouse-a/r1", Timestamp: time.UnixMilli(1_700_000_000_000),
		Metric: "robot_battery_percent", Value: 87, Labels: map[string]string{"robot_id": "warehouse-a/r1"},
	}
	if err := sink.WriteSamples(context.Background(), []telemetry.Sample{s}); err != nil {
		t.Fatalf("WriteSamples: %v", err)
	}

	if hdr.Get("Content-Encoding") != "snappy" || hdr.Get("Content-Type") != "application/x-protobuf" {
		t.Errorf("remote-write headers = %v", hdr)
	}
	if len(got) != 1 {
		t.Fatalf("series = %d, want 1", len(got))
	}
	g := got[0]
	if g.labels["__name__"] != "robot_battery_percent" || g.labels["robot_id"] != "warehouse-a/r1" {
		t.Errorf("labels = %v", g.labels)
	}
	if g.value != 87 || g.tsMs != 1_700_000_000_000 {
		t.Errorf("sample = (%v, %d), want (87, 1700000000000)", g.value, g.tsMs)
	}
}

// A non-2xx response returns an error (so the caller counts the drop) and records
// tsdb_write_errors classified by status.
func TestRemoteWriteSink_ErrorIsClassifiedAndCounted(t *testing.T) {
	cases := []struct {
		status int
		class  string
	}{
		{http.StatusBadRequest, "schema"},
		{http.StatusUnauthorized, "auth"},
		{http.StatusInternalServerError, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			sink := telemetry.NewRemoteWriteSink(srv.URL, telemetry.SinkVictoriaMetrics)

			c := metrics.TelemetryTSDBWriteErrorsTotal.WithLabelValues("ns-"+tc.class, "VictoriaMetrics", tc.class)
			before := testutil.ToFloat64(c)

			err := sink.WriteSamples(context.Background(), []telemetry.Sample{{RobotID: "ns-" + tc.class + "/r1", Metric: "m"}})
			if err == nil {
				t.Fatal("a non-2xx remote-write response must return an error")
			}
			if got := testutil.ToFloat64(c) - before; got != 1 {
				t.Errorf("tsdb_write_errors{%s} delta = %v, want 1", tc.class, got)
			}
		})
	}
}

// NewSink selects NoopWriter for unset/Drop and a RemoteWriteSink for real stores.
func TestNewSink_SelectsByType(t *testing.T) {
	if _, ok := telemetry.NewSink(telemetry.SinkUnset, "").(telemetry.NoopWriter); !ok {
		t.Error("unset sink should be NoopWriter")
	}
	if _, ok := telemetry.NewSink(telemetry.SinkDrop, "").(telemetry.NoopWriter); !ok {
		t.Error("Drop sink should be NoopWriter")
	}
	for _, st := range []string{telemetry.SinkPrometheusRemoteWrite, telemetry.SinkVictoriaMetrics, telemetry.SinkMimir} {
		if _, ok := telemetry.NewSink(st, "http://x").(*telemetry.RemoteWriteSink); !ok {
			t.Errorf("%s sink should be a RemoteWriteSink", st)
		}
	}
}

// An empty batch is a no-op (no POST, no error).
func TestRemoteWriteSink_EmptyBatchNoOp(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { posted = true }))
	defer srv.Close()
	if err := telemetry.NewRemoteWriteSink(srv.URL, telemetry.SinkMimir).WriteSamples(context.Background(), nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if posted {
		t.Error("empty batch must not POST")
	}
}
