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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A telemetry frame resolves its Robot from the manager's cache (findTelemetryRobot,
// findRobotByIDInNamespace). These tests run that lookup against a real informer cache over the
// envtest API server: the fake client deep-copies whatever it returns, so it can show neither
// what the lookup copies nor whether it hands back the cache's own objects.

// cachedLookupClient starts an informer cache over namespaces and returns a client that reads
// from it, once it has synced.
func cachedLookupClient(t *testing.T, namespaces ...string) client.Client {
	t.Helper()
	nsConfig := map[string]cache.Config{}
	for _, ns := range namespaces {
		nsConfig[ns] = cache.Config{}
	}
	c, err := cache.New(envCfg, cache.Options{Scheme: envScheme, DefaultNamespaces: nsConfig})
	if err != nil {
		t.Fatalf("build the informer cache: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.Start(ctx) }()
	// Ask for the Robot informer before waiting, so the sync covers it.
	if _, err := c.GetInformer(ctx, &fleetv1.Robot{}); err != nil {
		t.Fatalf("start the Robot informer: %v", err)
	}
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !c.WaitForCacheSync(syncCtx) {
		t.Fatal("the informer cache did not sync")
	}
	cl, err := client.New(envCfg, client.Options{Scheme: envScheme, Cache: &client.CacheOptions{Reader: c}})
	if err != nil {
		t.Fatalf("build the cached client: %v", err)
	}
	return cl
}

// seedLookupRobots creates n Robots in ns, each carrying a condition, so a copy that shares the
// cache's maps or slices can be told from one that does not. It returns their names.
func seedLookupRobots(t *testing.T, ns string, n int) []string {
	t.Helper()
	ctx := context.Background()
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("amr-%03d", i)
		robot := envtestValidRobot(ns, name, "aisle-1") // spec.zone is required; the zone need not exist
		robot.Labels = map[string]string{"fleet": "a"}
		if err := envK8s.Create(ctx, robot); err != nil {
			t.Fatalf("create robot %q: %v", name, err)
		}
		robot.Status.Phase = fleetv1.RobotPhaseIdle
		meta.SetStatusCondition(&robot.Status.Conditions, metav1.Condition{Type: conditionTypeReady,
			Status: metav1.ConditionTrue, Reason: "Seeded", Message: "seeded", LastTransitionTime: metav1.Now()})
		if err := envK8s.Status().Update(ctx, robot); err != nil {
			t.Fatalf("seed robot status %q: %v", name, err)
		}
		names = append(names, name)
	}
	return names
}

// waitForCachedRobots waits until cl lists n Robots in ns.
func waitForCachedRobots(t *testing.T, cl client.Client, ns string, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("the cache to hold %d robots in %s", n, ns), func() bool {
		var list fleetv1.RobotList
		if err := cl.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
			return false
		}
		ready := 0
		for i := range list.Items {
			if len(list.Items[i].Status.Conditions) > 0 {
				ready++
			}
		}
		return ready == n
	})
}

// A frame's lookup copies the robot it finds, not the namespace: its allocations do not grow with
// the number of robots in the namespace. Listed with a deep copy, every lookup copied every Robot.
func TestEnvtest_ARobotLookupCopiesOnlyTheRobotItFinds(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	small, large := envtestNamespace(t), envtestNamespace(t)
	seedLookupRobots(t, small, 10)
	seedLookupRobots(t, large, 100)
	cl := cachedLookupClient(t, small, large)
	waitForCachedRobots(t, cl, small, 10)
	waitForCachedRobots(t, cl, large, 100)

	allocs := func(ns string) float64 {
		return testing.AllocsPerRun(200, func() {
			robot, err := findRobotByIDInNamespace(ctx, cl, ns, "amr-005")
			if err != nil || robot == nil {
				t.Fatalf("lookup in %s: robot %v, err %v", ns, robot, err)
			}
		})
	}
	at10, at100 := allocs(small), allocs(large)
	t.Logf("allocations per lookup: %.0f with 10 robots, %.0f with 100", at10, at100)
	// One deep copy of a Robot is a few dozen allocations; ninety more Robots copied would be
	// thousands. The margin covers the informers' own background allocations.
	if grew := at100 - at10; grew > 40 {
		t.Fatalf("a lookup among 100 robots allocated %.0f more times than among 10 (%.0f against %.0f): "+
			"it copies robots it does not return", grew, at100, at10)
	}
}

// The robot a lookup returns is the caller's own: changing it, as the telemetry sink and the
// ingestors do before they patch, leaves the cache's object unchanged. Listing without a deep copy
// hands back the cache's maps and slices, so the lookup must copy the one robot it returns.
func TestEnvtest_ChangingTheRobotALookupReturnsLeavesTheCacheUnchanged(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns, other := envtestNamespace(t), envtestNamespace(t)
	seedLookupRobots(t, ns, 3)
	seedLookupRobots(t, other, 1) // amr-000 in another namespace: the cluster-wide lookup's case
	cl := cachedLookupClient(t, ns, other)
	waitForCachedRobots(t, cl, ns, 3)
	waitForCachedRobots(t, cl, other, 1)

	lookups := []struct {
		name string
		find func() (*fleetv1.Robot, error)
	}{
		{"in its namespace", func() (*fleetv1.Robot, error) { return findRobotByIDInNamespace(ctx, cl, ns, "amr-001") }},
		{"through a frame with a namespace", func() (*fleetv1.Robot, error) { return findTelemetryRobot(ctx, cl, ns, "amr-002") }},
		{"through a frame with no namespace", func() (*fleetv1.Robot, error) { return findTelemetryRobot(ctx, cl, "", "amr-001") }},
	}
	for _, lk := range lookups {
		t.Run(lk.name, func(t *testing.T) {
			robot, err := lk.find()
			if err != nil || robot == nil {
				t.Fatalf("lookup: robot %v, err %v", robot, err)
			}
			before := robot.DeepCopy()

			robot.Annotations["changed-by-the-caller"] = "yes"
			robot.Labels["fleet"] = "changed"
			robot.Status.Conditions[0].Message = "changed by the caller"
			robot.Status.Phase = fleetv1.RobotPhaseError

			cached := &fleetv1.Robot{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(before), cached); err != nil {
				t.Fatalf("read the cached robot: %v", err)
			}
			if _, ok := cached.Annotations["changed-by-the-caller"]; ok {
				t.Error("the caller's new annotation reached the cache: the lookup returned the cache's annotation map")
			}
			if cached.Labels["fleet"] != "a" {
				t.Errorf("the cache's label is %q: the lookup returned the cache's label map", cached.Labels["fleet"])
			}
			if got := cached.Status.Conditions[0].Message; got != "seeded" {
				t.Errorf("the cache's condition message is %q: the lookup returned the cache's conditions", got)
			}
			if cached.Status.Phase != fleetv1.RobotPhaseIdle {
				t.Errorf("the cache's phase is %q", cached.Status.Phase)
			}

			again, err := lk.find()
			if err != nil || again == nil {
				t.Fatalf("second lookup: robot %v, err %v", again, err)
			}
			if again.Status.Conditions[0].Message != "seeded" || again.Labels["fleet"] != "a" {
				t.Error("a second lookup sees the first caller's changes")
			}
		})
	}
}
