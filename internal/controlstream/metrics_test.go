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

package controlstream

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Every received TelemetryPayload is counted under the mTLS-verified adapter
// identity, independent of whether it is ingested (it counts frames RECEIVED).
func TestIngest_CountsFramesReceived(t *testing.T) {
	s := &Server{} // nil Ingestor: the frame is still received (and counted), then dropped
	tlsID := TLSIdentity{Namespace: "fr-ns", AdapterName: "fr-adapter", Verified: true}

	fr := metrics.TelemetryFramesReceivedTotal.WithLabelValues("fr-ns", "fr-adapter")
	before := testutil.ToFloat64(fr)

	s.ingest(context.Background(), AdapterIdentity{Namespace: "fr-ns"}, tlsID,
		&fav1.TelemetryPayload{RobotId: "fr-ns/robot-1"})

	if got := testutil.ToFloat64(fr) - before; got != 1 {
		t.Errorf("frames_received{fr-ns,fr-adapter} delta = %v, want 1", got)
	}
}
