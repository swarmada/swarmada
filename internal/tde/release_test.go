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

package tde

import (
	"context"
	"errors"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A deleted Robot's adapter-initiated holds and waits are released without a restart, and
// a waiter the engine promotes is told (RFC-0001 #tde-normative-invariants, TDE-7).

type grantLog struct {
	mu     sync.Mutex
	grants []string // namespace/robot/resource
}

func (g *grantLog) notify(_ context.Context, namespace, robotID, resourceName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants = append(g.grants, namespace+"/"+robotID+"/"+resourceName)
}

func (g *grantLog) got() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.grants...)
}

// persistedLift reads the lift's holders and waiters from FleetZone.status.
func persistedLift(t *testing.T, c client.Client) (holders, waiters []string) {
	t.Helper()
	z := &fleetv1.FleetZone{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "z"}, z); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	for _, q := range z.Status.SharedResourceQueues {
		for _, h := range q.CurrentHolders {
			holders = append(holders, h.RobotID)
		}
		for _, w := range q.WaitQueue {
			waiters = append(waiters, w.RobotID)
		}
	}
	return holders, waiters
}

func TestReleaseAbsentRobots_ReleasesADeletedHolderAndTellsTheWaiter(t *testing.T) {
	ctx := context.Background()
	c := recoveryClient(t, liftZoneWithQueue("amr-1", "r2"), wireRobot("r1", "amr-1"), wireRobot("r2", "r2"))
	log := &grantLog{}
	e := New(c, DefaultConfig()).WithGrantNotifier(log.notify)
	if err := e.Recover(ctx, c, RecoverValidate); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := log.got(); len(got) != 0 {
		t.Fatalf("grants pushed by a recovery that promoted nobody: %v", got)
	}

	if err := c.Delete(ctx, wireRobot("r1", "amr-1")); err != nil {
		t.Fatalf("delete r1: %v", err)
	}
	if err := e.ReleaseAbsentRobots(ctx, ns); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h, w := persistedLift(t, c); len(h) != 1 || h[0] != "r2" || len(w) != 0 {
		t.Errorf("persisted lift after r1's Robot was deleted: holders %v waiters %v; want holder r2, no waiters", h, w)
	}
	if out, _ := e.ReserveResource(ctx, ns, "lift", "r2"); out.State != ResourceGranted {
		t.Errorf("r2 after r1's Robot was deleted = %s, want Granted", out.State)
	}
	if got := log.got(); len(got) != 1 || got[0] != ns+"/r2/lift" {
		t.Errorf("reservation_granted pushes = %v, want exactly [%s/r2/lift]", got, ns)
	}

	// Idempotent: a second pass finds nothing to release and tells nobody again.
	if err := e.ReleaseAbsentRobots(ctx, ns); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if got := log.got(); len(got) != 1 {
		t.Errorf("reservation_granted pushes after a second pass = %v, want still one", got)
	}
}

func TestReleaseAbsentRobots_DropsADeletedWaiterAndKeepsTheHolder(t *testing.T) {
	ctx := context.Background()
	c := recoveryClient(t, liftZoneWithQueue("r1", "r2", "r3"),
		wireRobot("r1", "r1"), wireRobot("r2", "r2"), wireRobot("r3", "r3"))
	log := &grantLog{}
	e := New(c, DefaultConfig()).WithGrantNotifier(log.notify)
	if err := e.Recover(ctx, c, RecoverValidate); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if err := c.Delete(ctx, wireRobot("r2", "r2")); err != nil {
		t.Fatalf("delete r2: %v", err)
	}
	if err := e.ReleaseAbsentRobots(ctx, ns); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h, w := persistedLift(t, c); len(h) != 1 || h[0] != "r1" || len(w) != 1 || w[0] != "r3" {
		t.Errorf("persisted lift after waiter r2's Robot was deleted: holders %v waiters %v; want holder r1, waiter r3", h, w)
	}
	if got := log.got(); len(got) != 0 {
		t.Errorf("reservation_granted pushes when no holder changed = %v, want none", got)
	}
}

// Before recovery the engine's queues are not rebuilt, and recovery applies the same
// rule itself, so a release before it does nothing rather than act on empty state.
func TestReleaseAbsentRobots_WaitsForRecovery(t *testing.T) {
	ctx := context.Background()
	c := recoveryClient(t, liftZoneWithQueue("gone", "r2"), wireRobot("r2", "r2"))
	log := &grantLog{}
	e := New(c, DefaultConfig()).WithGrantNotifier(log.notify)
	if err := e.ReleaseAbsentRobots(ctx, ns); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h, _ := persistedLift(t, c); len(h) != 1 || h[0] != "gone" {
		t.Errorf("persisted holders changed before recovery: %v", h)
	}
	if got := log.got(); len(got) != 0 {
		t.Errorf("reservation_granted pushes before recovery = %v, want none", got)
	}
}

// A Robot lookup that fails is an error, and nothing is released on it.
func TestReleaseAbsentRobots_UnreadableRobotsReleaseNothing(t *testing.T) {
	ctx := context.Background()
	base := recoveryClient(t, liftZoneWithQueue("r1", "r2"), wireRobot("r1", "r1"), wireRobot("r2", "r2"))
	fail := false
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*fleetv1.RobotList); ok && fail {
				return errors.New("robots unreadable")
			}
			return cl.List(ctx, list, opts...)
		},
	})
	e := New(c, DefaultConfig())
	if err := e.Recover(ctx, c, RecoverValidate); err != nil {
		t.Fatalf("recover: %v", err)
	}
	fail = true
	if err := e.ReleaseAbsentRobots(ctx, ns); err == nil {
		t.Error("release with unreadable Robots returned no error")
	}
	if h, _ := persistedLift(t, c); len(h) != 1 || h[0] != "r1" {
		t.Errorf("persisted holders after a failed release = %v, want [r1]", h)
	}
}

// A waiter recovery promotes, because its holder's Robot is gone, is told.
func TestRecover_TellsTheWaiterItPromotes(t *testing.T) {
	c := recoveryClient(t, liftZoneWithQueue("gone", "r2"), wireRobot("r2", "r2"))
	log := &grantLog{}
	e := New(c, DefaultConfig()).WithGrantNotifier(log.notify)
	if err := e.Recover(context.Background(), c, RecoverValidate); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := log.got(); len(got) != 1 || got[0] != ns+"/r2/lift" {
		t.Errorf("reservation_granted pushes after recovery promoted r2 = %v, want exactly [%s/r2/lift]", got, ns)
	}
}
