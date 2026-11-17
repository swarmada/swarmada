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
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
)

// conflictingPresence builds a liveness reconciler whose first `conflicts` FleetAdapter status
// patches fail with a conflict, as a patch derived from a cache behind the previous write does.
// It counts the FleetAdapter status patches.
func conflictingPresence(t *testing.T, now time.Time, conflicts int32) (*FleetAdapterReconciler, client.Client, *atomic.Int32) {
	t.Helper()
	var patches atomic.Int32
	c := fake.NewClientBuilder().WithScheme(livenessScheme(t)).WithObjects(&fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS},
		Status:     fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected},
	}).WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, fa := obj.(*fleetv1.FleetAdapter); fa {
				if patches.Add(1) <= conflicts {
					return apierrors.NewConflict(schema.GroupResource{Group: "swarmada.io", Resource: "fleetadapters"},
						obj.GetName(), nil)
				}
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		}}).Build()
	return &FleetAdapterReconciler{Client: c, Scheme: c.Scheme(), APIReader: c, now: func() time.Time { return now }},
		c, &patches
}

// A presence write that conflicts is derived again, once: one conflict still writes lastHeartbeat,
// two leave it for the next heartbeat.
func TestLiveness_AConflictedPresenceWriteIsDerivedAgainOnce(t *testing.T) {
	now := time.Unix(20000, 0)
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	r, c, patches := conflictingPresence(t, now, 1)
	r.AdapterHeartbeatAt(context.Background(), id, now)
	r.liveness.flush()
	if got := lastHeartbeat(t, c); !got.Equal(now) {
		t.Errorf("one conflict: lastHeartbeat = %v, want %v written on the second attempt", got, now)
	}
	if n := patches.Load(); n != 2 {
		t.Errorf("one conflict: %d patches, want 2", n)
	}

	r, c, patches = conflictingPresence(t, now, 5)
	r.AdapterHeartbeatAt(context.Background(), id, now)
	r.liveness.flush()
	if got := lastHeartbeat(t, c); !got.IsZero() {
		t.Errorf("two conflicts: lastHeartbeat = %v, want none written", got)
	}
	if n := patches.Load(); n != 2 {
		t.Errorf("two conflicts: %d patches, want 2 (one retry, no more)", n)
	}
}

// A presence write whose stream has ended starts no stamp run.
func TestLiveness_NoStampRunOnceTheStreamHasEnded(t *testing.T) {
	now := time.Unix(20000, 0)
	r, c, release, writes := heldRobotWrites(t, now, "ra1")
	close(release)
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.AdapterHeartbeatAt(ctx, id, now)
	r.liveness.flush()
	if n := writes.Load(); n != 0 {
		t.Errorf("%d Robot stamps after the stream ended, want none", n)
	}
	if conn := getRobot(t, c, "ra1", faNS).Status.Connectivity; conn != nil && conn.LastSeenAt != nil {
		t.Errorf("ra1 stamped at %v after the stream ended", conn.LastSeenAt)
	}
}
