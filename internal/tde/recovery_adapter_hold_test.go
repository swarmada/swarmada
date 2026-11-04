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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Adapter-initiated shared-resource entries survive recovery while their Robot exists
// (RFC-0001 #tde-restart-recovery, TDE-5); an entry whose Robot is gone is released.

// liftZoneWithQueue is a zone declaring a capacity-1 lift whose persisted queue has the
// given adapter-initiated holder and waiters (ActionID == RobotID, as ReserveResource
// records them).
func liftZoneWithQueue(holder string, waiters ...string) *fleetv1.FleetZone {
	z := fzone("z", 0, liftResource())
	q := fleetv1.SharedResourceQueue{ResourceName: "lift"}
	if holder != "" {
		q.CurrentHolders = []fleetv1.ResourceHolder{{
			RobotID: holder, ActionID: holder, Priority: fleetv1.ActionPriorityNormal, HeldSince: metav1.Now(),
		}}
	}
	for _, w := range waiters {
		q.WaitQueue = append(q.WaitQueue, fleetv1.WaitQueueEntry{
			RobotID: w, ActionID: w, Priority: fleetv1.ActionPriorityNormal, RequestedAt: metav1.Now(),
		})
	}
	z.Status.SharedResourceQueues = []fleetv1.SharedResourceQueue{q}
	return z
}

// wireRobot is a Robot whose adapter knows it as wireID.
func wireRobot(name, wireID string) *fleetv1.Robot {
	return &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, Annotations: map[string]string{fleetv1.RobotIDAnnotation: wireID},
	}}
}

func TestRecover_KeepsAdapterHoldsWhoseRobotExists(t *testing.T) {
	for _, mode := range []RecoveryMode{RecoverValidate, RecoverReleaseReservedOnly, RecoverReleaseAll} {
		t.Run(string(mode), func(t *testing.T) {
			// r1 holds under a robot_id that differs from its object name, so the hold is
			// matched through the wire id, not the name.
			c := recoveryClient(t, liftZoneWithQueue("amr-1", "r2"), wireRobot("r1", "amr-1"), wireRobot("r2", "r2"))
			e := New(c, DefaultConfig())
			if err := e.Recover(context.Background(), c, mode); err != nil {
				t.Fatalf("recover: %v", err)
			}
			ctx := context.Background()
			// A robot that held nothing asks first: the lift is not free.
			if out, _ := e.ReserveResource(ctx, ns, "lift", "r3"); out.State == ResourceGranted {
				t.Error("a third robot was granted the capacity-1 lift after recovery while amr-1 holds it")
			}
			z := &fleetv1.FleetZone{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "z"}, z); err != nil {
				t.Fatalf("get zone: %v", err)
			}
			var holders, waiters []string
			for _, q := range z.Status.SharedResourceQueues {
				for _, h := range q.CurrentHolders {
					holders = append(holders, h.RobotID)
				}
				for _, w := range q.WaitQueue {
					waiters = append(waiters, w.RobotID)
				}
			}
			if len(holders) != 1 || holders[0] != "amr-1" || len(waiters) < 1 || waiters[0] != "r2" {
				t.Errorf("persisted lift queue after recovery: holders %v waiters %v; want holder amr-1, r2 first in line",
					holders, waiters)
			}
		})
	}
}

func TestRecover_ReleasesAdapterHoldWhoseRobotIsGone(t *testing.T) {
	// The holder's Robot was deleted; the waiter's still exists and is promoted.
	c := recoveryClient(t, liftZoneWithQueue("gone", "r2"), wireRobot("r2", "r2"))
	e := New(c, DefaultConfig())
	if err := e.Recover(context.Background(), c, RecoverValidate); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if out, _ := e.ReserveResource(context.Background(), ns, "lift", "r2"); out.State != ResourceGranted {
		t.Errorf("waiter after its holder's Robot is gone = %s, want Granted (promoted)", out.State)
	}
}

func TestRecover_UnreadableRobotsFailRecoveryClosed(t *testing.T) {
	base := recoveryClient(t, liftZoneWithQueue("r1"), wireRobot("r1", "r1"))
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*fleetv1.RobotList); ok {
				return errors.New("robots unreadable")
			}
			return cl.List(ctx, list, opts...)
		},
	})
	e := New(c, DefaultConfig())
	if err := e.Recover(context.Background(), c, RecoverValidate); err == nil {
		t.Fatal("recovery succeeded without being able to validate an adapter hold")
	}
	if e.Recovered() {
		t.Error("the grant gate opened although recovery failed")
	}
}
