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

// One clear must release a robot that got two stop IDs from one trigger. On a 1.4.0 connection
// each ID needs its own EstopClear and EstopClearAck, so a clear that named one would leave the
// robot latched. The second send is issued directly, as a second pass does, so the row needs no
// race and stays meaningful once the double send is fixed.

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// latchStream latches each stop ID it is sent, and lifts one only on an EstopClear naming it, as
// a 1.4.0 adapter does.
type latchStream struct {
	d       *safety.Dispatcher
	id      controlstream.TLSIdentity
	mu      sync.Mutex
	latched map[string]bool
	cleared []string
}

func (s *latchStream) Send(m *fav1.ControlPlaneSafetyMessage) error {
	switch {
	case m.GetEstop() != nil:
		id := m.GetEstop().GetEstopId()
		s.mu.Lock()
		s.latched[id] = true
		s.mu.Unlock()
		go func() {
			_ = s.d.RouteAck(s.id, s, m.GetRobotId(), &fav1.EstopAck{EstopId: id, State: fav1.EstopState_ESTOP_STATE_STOPPED})
		}()
	case m.GetEstopClear() != nil:
		id := m.GetEstopClear().GetEstopId()
		s.mu.Lock()
		delete(s.latched, id)
		s.cleared = append(s.cleared, id)
		s.mu.Unlock()
		go func() {
			_ = s.d.RouteClearAck(s.id, s, m.GetRobotId(), &fav1.EstopClearAck{EstopId: id, LiftedAtMs: time.Now().UnixMilli()})
		}()
	}
	return nil
}

func TestClearTwoIDs_OneClearReleasesARobotThatGotTwoStopIDs(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "bay")); err != nil {
		t.Fatal(err)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopNormal, time.Now())
	if err := envK8s.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	frames := resumeWitness()
	disp := safety.New(envK8s, record.NewFakeRecorder(10000))
	disp.APIReader = envK8s
	disp.DeriveCapabilities = DeriveCapabilities
	disp.Telemetry = frames
	id := controlstream.TLSIdentity{AdapterName: "acme-adapter", Namespace: ns, Verified: true}
	s := &latchStream{d: disp, id: id, latched: map[string]bool{}}
	t.Cleanup(disp.RegisterStream(id, s))
	disp.ContractReported(id, "1.4.0")

	r := &ZoneEstopReconciler{Client: envK8s, Scheme: envScheme, Estopper: disp, Recorder: record.NewFakeRecorder(100), APIReader: envK8s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "bay"}}
	setEstopTrigger(t, ns, "bay", "person detected")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitRecorded := func(want int) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); len(getEnvRobot(t, ns, "amr-1").Status.Stops) < want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("stop records never reached %d", want)
			}
		}
	}
	waitRecorded(1)
	if _, err := disp.TriggerEstop(ctx, ns, "amr-1", "person detected", "x", metrics.ScopeZone, carrierOf(zoneHold(treeZone(t, ns, "bay")))); err != nil {
		t.Fatal(err)
	}
	waitRecorded(2)
	s.mu.Lock()
	if len(s.latched) != 2 {
		t.Fatalf("adapter latched %d stop IDs, want 2", len(s.latched))
	}
	s.mu.Unlock()

	setEstopTrigger(t, ns, "bay", "")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); len(getEnvRobot(t, ns, "amr-1").Status.Stops) > 0 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	rb := getEnvRobot(t, ns, "amr-1")
	s.mu.Lock()
	latched, cleared := len(s.latched), len(s.cleared)
	s.mu.Unlock()
	t.Logf("after one clear: EstopClears=%d still latched=%d status.stops=%d estopState=%s", cleared, latched, len(rb.Status.Stops), rb.Status.EstopState)
	if cleared != 2 || latched != 0 {
		t.Errorf("EstopClears = %d, still latched = %d; want 2 and 0", cleared, latched)
	}
	if len(rb.Status.Stops) != 0 {
		t.Errorf("status.stops holds %d entries after both acks, want 0", len(rb.Status.Stops))
	}
	if rb.Status.EstopState != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming", rb.Status.EstopState)
	}

	// The operator's release then succeeds on telemetry since the clear (S6).
	time.Sleep(2 * time.Millisecond)
	if err := frames.Ingest(ctx, telemetry.Frame{RobotID: "amr-1", Namespace: ns, Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle}); err != nil {
		t.Fatal(err)
	}
	st, why, err := disp.ConfirmResume(ctx, ns, "amr-1", "alice")
	t.Logf("release: state=%s why=%q err=%v", st, why, err)
	if err != nil || st != fleetv1.RobotEstopNormal {
		t.Errorf("release = %s (%q, err %v), want Normal", st, why, err)
	}
}
