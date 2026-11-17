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

package safety

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// windowSender records what the control plane sends on the robot's SafetyStream and answers
// nothing, so a stop it carries stays in flight.
type windowSender struct {
	mu   sync.Mutex
	msgs []*fav1.ControlPlaneSafetyMessage
}

func (s *windowSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *windowSender) count(pick func(*fav1.ControlPlaneSafetyMessage) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.msgs {
		if pick(m) {
			n++
		}
	}
	return n
}

// A stop issued after a clear's epoch, between its check for such a stop and its read of the stops
// in flight, is not ended by the clear. The clear judged the other carriers on a read taken before
// that stop's trigger, so it cannot know the stop's carrier is in force: the stop stands, the
// robot is not moved, and no EstopClear names the stop (RFC-0001 #safety-robot-stop-table, row
// S5's guard).
func TestClear_AStopIssuedInTheEpochWindowStands(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: "amr-1"}},
		Spec: fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"}},
	}
	// Every status write but the clear's own is held until the clear has returned: the new stop's
	// record must not land first, or the clear's write would conflict and be superseded on retry,
	// and the window would not be tested.
	var mu sync.Mutex
	var hold chan struct{}
	funcs := interceptor.Funcs{SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
		obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		data, _ := patch.Data(obj)
		mu.Lock()
		h := hold
		mu.Unlock()
		if h != nil && !bytes.Contains(data, []byte(string(fleetv1.RobotEstopResuming))) {
			<-h
		}
		return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).WithInterceptorFuncs(funcs).Build()
	if err := c.Get(ctx, client.ObjectKeyFromObject(robot), robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	robot.Status.Stops = []fleetv1.RobotStop{{ID: "stop-zone", Scope: fleetv1.StopScopeZone, SentAt: metav1.Now(),
		Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: "bay-1", UID: "zone-uid"}}}
	robot.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-1", UID: "zone-uid"}}
	if err := c.Status().Update(ctx, robot); err != nil {
		t.Fatal(err)
	}

	d := New(c, nil)
	d.Telemetry = &testFrames{}
	d.DeriveCapabilities = testDerivation
	d.deliveryTimeout = 200 * time.Millisecond
	sender := &windowSender{}
	d.RegisterStream(identity("acme"), sender)
	d.ContractReported(identity("acme"), "1.4.0") // the adapter answers EstopClear

	release := make(chan struct{})
	stopDone := make(chan struct{})
	hookRuns := 0
	d.clearHookBeforeInflight = func() {
		hookRuns++
		if hookRuns > 1 {
			return
		}
		mu.Lock()
		hold = release
		mu.Unlock()
		// The namespace's stop, issued now: after the clear's epoch and its superseded check.
		go func() {
			defer close(stopDone)
			_, _ = d.TriggerEstop(ctx, ns, "amr-1", "namespace stop", "operator", metrics.ScopeNamespace,
				Carrier{Kind: fleetv1.StopCarrierSwarmadaConfig, Name: "swarmada-config", UID: "cfg-uid"})
		}()
		deadline := time.Now().Add(5 * time.Second)
		for sender.count(func(m *fav1.ControlPlaneSafetyMessage) bool { return m.GetEstop() != nil }) == 0 {
			if time.Now().After(deadline) {
				t.Error("the namespace's stop was never sent")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	// The carriers as the clear read them after its epoch: nothing else in force. Its zone's own
	// record and stop are the clear's to end.
	ends := Ends{Hold: func(fleetv1.EstopHold) bool { return true }, Stop: func(fleetv1.RobotStop) bool { return true }}
	state, err := d.ClearEstop(ctx, ns, "amr-1", "alice", ends)
	close(release)
	select {
	case <-stopDone:
	case <-time.After(10 * time.Second):
		t.Error("the namespace's stop did not resolve")
	}
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if hookRuns == 0 {
		t.Fatal("the window was not exercised: the clear never reached its read of the stops in flight")
	}
	if state == fleetv1.RobotEstopResuming {
		t.Errorf("ClearEstop returned Resuming: it ended a stop issued after its epoch")
	}
	got := &fleetv1.Robot{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "amr-1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.EstopState == fleetv1.RobotEstopResuming {
		t.Errorf("estopState = Resuming: the clear released the robot from a stop issued after its epoch")
	}
	if n := sender.count(func(m *fav1.ControlPlaneSafetyMessage) bool { return m.GetEstopClear() != nil }); n != 0 {
		t.Errorf("%d EstopClear message(s) sent: the clear ended a stop issued after its epoch", n)
	}
}
