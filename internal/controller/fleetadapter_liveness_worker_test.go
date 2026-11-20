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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
)

// The stamps of an adapter's Robots (lastSeenAt) wait on the manager's Robot write budget, which a
// stop's fan-out can hold for many seconds. The adapter's own lastHeartbeat must not wait behind
// them: it is written when the heartbeat is recorded, and the stamps follow on the adapter's
// liveness worker.

// heldRobotWrites builds a liveness reconciler whose Robot status writes wait until release is
// closed, as writes waiting on a busy Robot write budget would. It counts the Robot status writes.
func heldRobotWrites(t *testing.T, now time.Time, robots ...string) (*FleetAdapterReconciler, client.Client, chan struct{}, *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	var writes atomic.Int32
	objs := []client.Object{&fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS},
		Status:     fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected},
	}}
	for _, r := range robots {
		objs = append(objs, robotBound(r, faNS, "acme", nil))
	}
	c := fake.NewClientBuilder().WithScheme(livenessScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}).
		WithIndex(&fleetv1.Robot{}, robotAdapterNameField, func(o client.Object) []string {
			if n := o.(*fleetv1.Robot).Spec.Adapter.Name; n != "" {
				return []string{n}
			}
			return nil
		}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, robot := obj.(*fleetv1.Robot); robot {
				<-release
				writes.Add(1)
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		}}).Build()
	return &FleetAdapterReconciler{Client: c, Scheme: c.Scheme(), now: func() time.Time { return now }}, c, release, &writes
}

func lastHeartbeat(t *testing.T, c client.Client) time.Time {
	t.Helper()
	fa := &fleetv1.FleetAdapter{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: faNS, Name: "acme"}, fa); err != nil {
		t.Fatal(err)
	}
	if fa.Status.LastHeartbeat == nil {
		return time.Time{}
	}
	return fa.Status.LastHeartbeat.Time
}

// A heartbeat's lastHeartbeat is written at once, while the Robot stamps wait; the stamps land once
// the writes are let through, at the heartbeat's receipt time.
func TestLiveness_LastHeartbeatDoesNotWaitForTheRobotStamps(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, _ := heldRobotWrites(t, now, "ra1", "ra2")
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	received := now.Add(-2 * time.Second)
	done := make(chan struct{})
	go func() { r.AdapterHeartbeatAt(context.Background(), id, received); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("AdapterHeartbeatAt waited on the Robot stamps")
	}
	if got := lastHeartbeat(t, c); !got.Equal(received.Truncate(time.Second)) {
		t.Errorf("lastHeartbeat = %v, want the heartbeat's receipt time %v", got, received)
	}
	close(release)
	r.liveness.flush()
	for _, name := range []string{"ra1", "ra2"} {
		if got := getRobot(t, c, name, faNS).Status.Connectivity; got == nil || got.LastSeenAt == nil ||
			!got.LastSeenAt.Time.Equal(received.Truncate(time.Second)) {
			t.Errorf("%s lastSeenAt = %+v, want the heartbeat's receipt time %v", name, got, received)
		}
	}
}

// Heartbeats that arrive while a stamp run waits collapse into one more run, at the latest
// heartbeat's receipt time. They are 20 s apart, past the 15 s throttle (T1/2), so the throttle
// does not decide which run writes.
func TestLiveness_HeartbeatsDuringARunCollapseIntoOne(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, writes := heldRobotWrites(t, now, "ra1")
	// The clock moves 20 s at each reading, so the stamps' spacing in real time (T1/2) does not
	// decide which run writes either.
	var ticks atomic.Int64
	r.now = func() time.Time { return now.Add(time.Duration(ticks.Add(1)) * 20 * time.Second) }
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.AdapterHeartbeatAt(context.Background(), id, now.Add(time.Duration(20*i-100)*time.Second))
		}(i)
	}
	wg.Wait()
	close(release)
	r.liveness.flush()
	if n := writes.Load(); n > 2 {
		t.Errorf("%d Robot stamp writes for 5 heartbeats during one run, want at most 2", n)
	}
	got := getRobot(t, c, "ra1", faNS).Status.Connectivity.LastSeenAt.Time
	if want := now.Add(-20 * time.Second).Truncate(time.Second); !got.Equal(want) {
		t.Errorf("lastSeenAt = %v, want the latest heartbeat's receipt time %v", got, want)
	}
}

// After the adapter's stream is gone, no stamp starts: a run in progress stops before its next
// Robot.
func TestLiveness_NoStampStartsAfterTheDisconnect(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, _ := heldRobotWrites(t, now, "ra1", "ra2", "ra3")
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	r.AdapterHeartbeatAt(context.Background(), id, now)
	time.Sleep(50 * time.Millisecond) // the run is now waiting on its first Robot write
	r.AdapterDisconnected(context.Background(), id)
	close(release)
	r.liveness.flush()
	stamped := 0
	for _, name := range []string{"ra1", "ra2", "ra3"} {
		if conn := getRobot(t, c, name, faNS).Status.Connectivity; conn != nil && conn.LastSeenAt != nil {
			stamped++
		}
	}
	if stamped > 1 {
		t.Errorf("%d Robots stamped after the disconnect, want at most the one whose write was in flight", stamped)
	}
}

// A stamp is never moved back, and lastHeartbeat neither: a heartbeat written late carries an
// older receipt time than one already written.
func TestLiveness_NeverMovedBack(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, _ := heldRobotWrites(t, now, "ra1")
	close(release)
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	r.AdapterHeartbeatAt(context.Background(), id, now)
	r.AdapterHeartbeatAt(context.Background(), id, now.Add(-10*time.Second))
	r.liveness.flush()
	if got := lastHeartbeat(t, c); !got.Equal(now) {
		t.Errorf("lastHeartbeat = %v, want %v kept", got, now)
	}
	if err := r.stampLastSeen(context.Background(), client.ObjectKey{Namespace: faNS, Name: "ra1"}, now.Add(-20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := getRobot(t, c, "ra1", faNS).Status.Connectivity.LastSeenAt.Time; !got.Equal(now) {
		t.Errorf("lastSeenAt = %v, want %v kept", got, now)
	}
}

func adapterPhase(t *testing.T, c client.Client) fleetv1.FleetAdapterPhase {
	t.Helper()
	fa := &fleetv1.FleetAdapter{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: faNS, Name: "acme"}, fa); err != nil {
		t.Fatal(err)
	}
	return fa.Status.Phase
}

// lastHeartbeat is when the control plane last confirmed the adapter's liveness (RFC-0001
// §9.1.13.3): every heartbeat received after the one recorded is written, to the second, and one
// received before it is not.
func TestLiveness_EveryNewerHeartbeatIsWrittenToLastHeartbeat(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, _ := heldRobotWrites(t, now)
	close(release)
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	r.AdapterHeartbeatAt(context.Background(), id, now.Add(-9*time.Second))
	r.AdapterHeartbeatAt(context.Background(), id, now.Add(-8*time.Second)) // 1 s later
	if got := lastHeartbeat(t, c); !got.Equal(now.Add(-8 * time.Second)) {
		t.Errorf("lastHeartbeat = %v, want %v: the latest confirmation", got, now.Add(-8*time.Second))
	}
	r.AdapterHeartbeatAt(context.Background(), id, now.Add(-10*time.Second)) // read before the one recorded
	if got := lastHeartbeat(t, c); !got.Equal(now.Add(-8 * time.Second)) {
		t.Errorf("lastHeartbeat = %v, want %v: an older heartbeat is not written", got, now.Add(-8*time.Second))
	}
	r.liveness.flush()
}

// A Robot is stamped at most once per half the offline threshold (15 s by default) in real time,
// whatever receipt times the stamps carry: two heartbeats read 20 s apart write one stamp while the
// clock has not moved, and the next is written once it has moved past the threshold.
func TestLiveness_StampsAreSpacedInRealTime(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, writes := heldRobotWrites(t, now, "ra1")
	close(release)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	r.AdapterHeartbeatAt(context.Background(), id, now.Add(-40*time.Second))
	r.liveness.flush()
	r.AdapterHeartbeatAt(context.Background(), id, now.Add(-20*time.Second)) // 20 s later by receipt, none by the clock
	r.liveness.flush()
	if n := writes.Load(); n != 1 {
		t.Fatalf("%d Robot stamp writes with no time between them, want 1", n)
	}
	clock.Add(int64(16 * time.Second))
	r.AdapterHeartbeatAt(context.Background(), id, now.Add(-4*time.Second))
	r.liveness.flush()
	if n := writes.Load(); n != 2 {
		t.Fatalf("%d Robot stamp writes once 16 s had passed, want 2", n)
	}
	if got := getRobot(t, c, "ra1", faNS).Status.Connectivity.LastSeenAt.Time; !got.Equal(now.Add(-4 * time.Second)) {
		t.Errorf("lastSeenAt = %v, want %v", got, now.Add(-4*time.Second))
	}
}

// A heartbeat older than one interval does not re-assert Connected: the staleness check decides.
func TestLiveness_AStaleHeartbeatDoesNotReassertConnected(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, _ := heldRobotWrites(t, now)
	close(release)
	ctx := context.Background()
	fa := &fleetv1.FleetAdapter{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: faNS, Name: "acme"}, fa); err != nil {
		t.Fatal(err)
	}
	base := fa.DeepCopy()
	fa.Status.Phase = fleetv1.FleetAdapterPhaseDegraded
	if err := c.Status().Patch(ctx, fa, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	r.AdapterHeartbeatAt(ctx, id, now.Add(-15*time.Second))
	if got := adapterPhase(t, c); got != fleetv1.FleetAdapterPhaseDegraded {
		t.Errorf("phase = %s after a heartbeat 15 s old, want Degraded kept", got)
	}
	r.AdapterHeartbeatAt(ctx, id, now.Add(-time.Second))
	if got := adapterPhase(t, c); got != fleetv1.FleetAdapterPhaseConnected {
		t.Errorf("phase = %s after a fresh heartbeat, want Connected", got)
	}
	r.liveness.flush()
}
