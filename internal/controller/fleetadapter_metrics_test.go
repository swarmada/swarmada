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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

func reconnectsCounter() float64 {
	return testutil.ToFloat64(metrics.FleetAdapterReconnectsTotal.WithLabelValues(faNS, "acme"))
}

// A handshake arriving while Disconnected/Degraded is a ControlStream
// re-establishment and increments reconnects_total; a first connect does not.
func TestFleetAdapterMetrics_ReconnectCounted(t *testing.T) {
	now := time.Unix(3000, 0)

	t.Run("from Disconnected counts", func(t *testing.T) {
		r, _ := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseDisconnected, nil)
		before := reconnectsCounter()
		r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation())
		if got := reconnectsCounter() - before; got != 1 {
			t.Errorf("reconnects delta = %v, want 1 (Disconnected→Connected)", got)
		}
	})

	t.Run("from Degraded counts", func(t *testing.T) {
		r, _ := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseDegraded, nil)
		before := reconnectsCounter()
		r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation())
		if got := reconnectsCounter() - before; got != 1 {
			t.Errorf("reconnects delta = %v, want 1 (Degraded→Connected)", got)
		}
	})

	t.Run("first connect does not count", func(t *testing.T) {
		r, _ := newFAReconciler(t, now, fleetv1.FleetAdapterPhasePending, nil)
		before := reconnectsCounter()
		r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation())
		if got := reconnectsCounter() - before; got != 0 {
			t.Errorf("reconnects delta = %v, want 0 (first connect is not a reconnect)", got)
		}
	})
}
