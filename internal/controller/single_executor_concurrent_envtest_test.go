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
	"fmt"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The single-executor guarantee under concurrent assignment
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee): N pending
// actions against N idle robots, every action reconciled at once, each reconcile reading
// the robots from the same snapshot, as reconciles sharing one informer do in the window
// before any claim reaches it. Every robot looks idle to every reconcile, so they all
// select the same robot. The robot claim is the serialization point: one reconcile wins
// it and the rest are refused and return their actions to Pending. Rounds repeat, with
// the snapshot refreshed between them, until every action is placed.
//
// Asserted after every round and at the end: no two actions hold one robot, every
// action's robot names that action back, and no action is named by two robots (the RA-4
// property: one action never acquires two robots).

// frozenRobots answers every Robot read from a snapshot taken by refresh; everything
// else, and every write, goes to the API server.
type frozenRobots struct {
	client.Client
	mu   sync.Mutex
	snap []fleetv1.Robot
}

func (f *frozenRobots) refresh(t *testing.T, ns string) {
	t.Helper()
	var list fleetv1.RobotList
	if err := f.Client.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("snapshot robots: %v", err)
	}
	f.mu.Lock()
	f.snap = list.Items
	f.mu.Unlock()
}

func (f *frozenRobots) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r, ok := obj.(*fleetv1.Robot); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.snap {
			if f.snap[i].Namespace == key.Namespace && f.snap[i].Name == key.Name {
				f.snap[i].DeepCopyInto(r)
				return nil
			}
		}
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

func (f *frozenRobots) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if rl, ok := list.(*fleetv1.RobotList); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		rl.Items = nil
		for i := range f.snap {
			rl.Items = append(rl.Items, *f.snap[i].DeepCopy())
		}
		return nil
	}
	return f.Client.List(ctx, list, opts...)
}

func TestEnvtest_SingleExecutor_ConcurrentAssignmentBindsEachRobotOnce(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	const n = 4

	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	gateEnvSeedAdapterStatus(t, ns, "acme-adapter", contract.Version)
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("amr-%d", i)
		if err := envK8s.Create(ctx, envtestValidRobot(ns, name, "z-a")); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		seedResumeStatus(t, ns, name, fleetv1.RobotEstopNormal, time.Now())
	}
	for i := 1; i <= n; i++ {
		if err := envK8s.Create(ctx, &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("task-%d", i), Namespace: ns},
			Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z-a", Priority: fleetv1.ActionPriorityNormal},
		}); err != nil {
			t.Fatalf("create task-%d: %v", i, err)
		}
	}

	view := &frozenRobots{Client: envK8s}
	r := &FleetActionReconciler{Client: view, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}
	for round := 1; round <= 2*n; round++ {
		view.refresh(t, ns)
		var wg sync.WaitGroup
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
			}(fmt.Sprintf("task-%d", i))
		}
		wg.Wait()
		if assertOneActionPerRobot(t, ns) == n {
			return
		}
	}
	t.Fatalf("after %d rounds, %d of %d actions are placed", 2*n, assertOneActionPerRobot(t, ns), n)
}

// assertOneActionPerRobot fails on any double binding and returns how many actions hold
// a robot.
func assertOneActionPerRobot(t *testing.T, ns string) int {
	t.Helper()
	ctx := context.Background()
	var actions fleetv1.FleetActionList
	if err := envK8s.List(ctx, &actions, client.InNamespace(ns)); err != nil {
		t.Fatalf("list actions: %v", err)
	}
	var robots fleetv1.RobotList
	if err := envK8s.List(ctx, &robots, client.InNamespace(ns)); err != nil {
		t.Fatalf("list robots: %v", err)
	}
	holder := map[string]string{}
	for _, rb := range robots.Items {
		holder[rb.Name] = rb.Status.AssignedAction
	}
	byRobot := map[string]string{}
	placed := 0
	for _, a := range actions.Items {
		robot := a.Status.AssignedRobot
		if robot == "" {
			continue
		}
		placed++
		if other, dup := byRobot[robot]; dup {
			t.Fatalf("%s and %s both hold robot %s (phases %s)", other, a.Name, robot, a.Status.Phase)
		}
		byRobot[robot] = a.Name
		if holder[robot] != a.Name {
			t.Fatalf("%s holds robot %s, whose status.assignedAction is %q", a.Name, robot, holder[robot])
		}
	}
	named := map[string]string{}
	for robot, action := range holder {
		if action == "" {
			continue
		}
		if other, dup := named[action]; dup {
			t.Fatalf("robots %s and %s both name action %s", other, robot, action)
		}
		named[action] = robot
	}
	return placed
}
