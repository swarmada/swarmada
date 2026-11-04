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
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The Rejected Event (RFC-0001 #discoveredrobot-state-machine) is the
// manager's. No built-in human role can create an Event, so a CLI-written one was
// refused for every caller and never recorded.

func TestDR_RejectionEmitsTheRejectedEventWithTheReason(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"unrecognised serial", "Normal Rejected rejected by operator: unrecognised serial"},
		{"", "Normal Rejected rejected by operator: no reason recorded by the operator"},
	} {
		now := drBase
		rec := record.NewFakeRecorder(8)
		r, _ := newDRReconciler(t, &now, rejected("dr-1", tc.reason))
		r.Recorder = rec

		reconcileDR(t, r, "dr-1")

		events := drainRecorded(rec)
		if n := countPrefixed(events, "Normal Rejected"); n != 1 {
			t.Fatalf("reason %q: Rejected Events = %d, want 1 (events: %q)", tc.reason, n, events)
		}
		if !strings.Contains(strings.Join(events, "\n"), tc.want) {
			t.Errorf("reason %q: events %q do not contain %q", tc.reason, events, tc.want)
		}
	}
}

func TestDR_TTLSweepEmitsNoRejectedEvent(t *testing.T) {
	now := drBase
	rec := record.NewFakeRecorder(8)
	r, _ := newDRReconciler(t, &now, discovered("dr-stale", time.Hour, fleetv1.DiscoveredRobotPhaseDiscovered))
	r.Recorder = rec

	now = drBase.Add(2 * time.Hour) // past the TTL
	reconcileDR(t, r, "dr-stale")

	if n := countPrefixed(drainRecorded(rec), "Normal Rejected"); n != 0 {
		t.Errorf("a TTL sweep emitted %d Rejected Event(s); only an operator's rejection does", n)
	}
}
