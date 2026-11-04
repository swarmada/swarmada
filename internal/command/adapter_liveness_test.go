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

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// beatRecorder records every HeartbeatRequest pushed down the stream and ignores
// everything else.
type beatRecorder struct {
	mu    sync.Mutex
	beats []*fav1.HeartbeatRequest
}

func (b *beatRecorder) Send(msg *fav1.ControlPlaneMessage) error {
	if hb := msg.GetHeartbeat(); hb != nil {
		b.mu.Lock()
		b.beats = append(b.beats, hb)
		b.mu.Unlock()
	}
	return nil
}

func (b *beatRecorder) snapshot() []*fav1.HeartbeatRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*fav1.HeartbeatRequest(nil), b.beats...)
}

func dispatcherWithAdapter(t *testing.T, intervalSeconds int32) *Dispatcher {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	if intervalSeconds >= 0 {
		b = b.WithObjects(&fleetv1.FleetAdapter{
			ObjectMeta: metav1.ObjectMeta{Namespace: cmdNS, Name: cmdAdapter},
			Spec:       fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: intervalSeconds},
		})
	}
	return New(b.Build())
}

// The cadence is HALF spec.heartbeatIntervalSeconds, because the FleetAdapter staleness
// backstop compares against the whole interval and a probe on the same period would sit
// on its own boundary (ADR-0054).
func TestBeatPeriod_IsHalfTheConfiguredInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval int32 // negative → no FleetAdapter object at all
		want     time.Duration
	}{
		{"CRD default of 10s", 10, 5 * time.Second},
		{"operator override", 30, 15 * time.Second},
		{"unset falls back to the CRD default", 0, 5 * time.Second},
		{"floored at one second", 1, time.Second},
		{"unreadable adapter keeps the default rather than stopping", -1, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dispatcherWithAdapter(t, tc.interval)
			if got := d.beatPeriod(context.Background(), cmdNS, cmdAdapter); got != tc.want {
				t.Fatalf("beatPeriod = %s, want %s", got, tc.want)
			}
		})
	}
}

// The defect ADR-0054 fixes: before this, the ONLY HeartbeatRequest sender was the
// confirming exchange behind `age > heartbeatTimeout`, so lastSeenAt was refreshed only
// once the control plane had already judged the robot stale. A registered stream must now
// beat on its own, with no Robot object in play and nothing having timed out.
func TestAdapterBeats_PushWithoutAnyTimeoutHavingFired(t *testing.T) {
	d := dispatcherWithAdapter(t, 2) // → 1s cadence
	rec := &beatRecorder{}
	deregister := d.RegisterStream(verifiedIdentity(), rec)

	time.Sleep(2200 * time.Millisecond)
	got := rec.snapshot()
	deregister()

	if len(got) < 2 {
		t.Fatalf("got %d adapter heartbeats in 2.2s at a 1s cadence, want at least 2 — "+
			"the liveness floor is missing and lastSeenAt is back to being refreshed only by its own timeout", len(got))
	}
	for i, hb := range got {
		// An adapter-scoped probe names no robot. A non-empty robot_id here would make
		// the periodic beat answer a per-robot confirming exchange nobody asked for.
		if hb.GetRobotId() != "" {
			t.Errorf("beat %d carried robot_id %q, want empty (adapter-scoped)", i, hb.GetRobotId())
		}
		if hb.GetSentAtMs() == 0 {
			t.Errorf("beat %d carried no sent_at_ms", i)
		}
	}
}

// A beat must not outlive the stream it rides on: deregistering stops the cadence.
func TestAdapterBeats_StopOnDeregister(t *testing.T) {
	d := dispatcherWithAdapter(t, 2) // → 1s cadence
	rec := &beatRecorder{}
	deregister := d.RegisterStream(verifiedIdentity(), rec)
	time.Sleep(1200 * time.Millisecond)
	deregister()

	settled := len(rec.snapshot())
	if settled == 0 {
		t.Fatal("no beat before deregister; the test proves nothing about stopping")
	}
	time.Sleep(1500 * time.Millisecond)
	if after := len(rec.snapshot()); after != settled {
		t.Fatalf("beats continued after deregister: %d → %d", settled, after)
	}
}

// An unverified identity is not registered at all, so it gets no cadence either —
// nothing is pushed down an unauthenticated stream.
func TestAdapterBeats_NotStartedForAnUnverifiedIdentity(t *testing.T) {
	d := dispatcherWithAdapter(t, 2)
	rec := &beatRecorder{}
	deregister := d.RegisterStream(controlstream.TLSIdentity{Namespace: cmdNS, AdapterName: cmdAdapter}, rec)
	defer deregister()

	time.Sleep(1200 * time.Millisecond)
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("pushed %d heartbeats to an unverified stream, want 0", n)
	}
}
