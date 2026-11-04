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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FleetZone.status.estopStatus is what a reader of the zone tree sees (RFC-0001
// #fleetzone-schema-root-zone): Clear when no estop is active in the zone or reaches it,
// Triggered when one is (its own, or an ancestor's carried down by propagateToChildren),
// ChildTriggered on the parent of a zone whose own estop is active and notifies it
// (propagateToParent). lastEstopAt is set while the status is not Clear and empty when it
// is. These tests run the zone estop controller against a real API server over a
// three-level tree:
//
//	site ─ floor ─┬─ bay   (propagateToParent: true)
//	              └─ dock

func seedEstopTree(t *testing.T, ns string) {
	t.Helper()
	notify := &fleetv1.ZoneEstopPolicy{PropagateToParent: true}
	for _, z := range []struct {
		name, parent string
		policy       *fleetv1.ZoneEstopPolicy
	}{{"site", "", nil}, {"floor", "site", nil}, {"bay", "floor", notify}, {"dock", "floor", nil}} {
		fz := &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: z.name, Namespace: ns},
			Spec:       fleetv1.FleetZoneSpec{ParentZone: z.parent, EstopPolicy: z.policy},
		}
		if err := envK8s.Create(context.Background(), fz); err != nil {
			t.Fatalf("create zone %s: %v", z.name, err)
		}
	}
}

func treeZone(t *testing.T, ns, name string) *fleetv1.FleetZone {
	t.Helper()
	fz := &fleetv1.FleetZone{}
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, fz); err != nil {
		t.Fatalf("get zone %s: %v", name, err)
	}
	return fz
}

// setEstopTrigger sets (value != "") or removes (value == "") a zone's estop-trigger
// annotation, as the estop-trigger and estop-clear verbs do.
func setEstopTrigger(t *testing.T, ns, name, value string) {
	t.Helper()
	fz := treeZone(t, ns, name)
	base := fz.DeepCopy()
	if value == "" {
		delete(fz.Annotations, annEstopTriggered)
	} else {
		if fz.Annotations == nil {
			fz.Annotations = map[string]string{}
		}
		fz.Annotations[annEstopTriggered] = value
	}
	if err := envK8s.Patch(context.Background(), fz, client.MergeFrom(base)); err != nil {
		t.Fatalf("set estop trigger on %s: %v", name, err)
	}
}

// zoneEstopView is one zone's estop status as a reader sees it; an empty status reads Clear.
type zoneEstopView struct {
	status fleetv1.ZoneEstopStatus
	stamp  bool
}

func viewOf(fz *fleetv1.FleetZone) zoneEstopView {
	st := fz.Status.EstopStatus
	if st == "" {
		st = fleetv1.ZoneEstopClear
	}
	return zoneEstopView{status: st, stamp: fz.Status.LastEstopAt != nil}
}

var (
	zClear     = zoneEstopView{status: fleetv1.ZoneEstopClear}
	zTriggered = zoneEstopView{status: fleetv1.ZoneEstopTriggered, stamp: true}
	zChild     = zoneEstopView{status: fleetv1.ZoneEstopChildTriggered, stamp: true}
)

func expectTree(t *testing.T, ns, step string, want map[string]zoneEstopView) {
	t.Helper()
	for _, name := range []string{"site", "floor", "bay", "dock"} {
		if got := viewOf(treeZone(t, ns, name)); got != want[name] {
			t.Errorf("%s: zone %s reads %s (lastEstopAt set: %v), want %s (lastEstopAt set: %v)",
				step, name, got.status, got.stamp, want[name].status, want[name].stamp)
		}
	}
}

func treeVersions(t *testing.T, ns string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"site", "floor", "bay", "dock"} {
		out[name] = treeZone(t, ns, name).ResourceVersion
	}
	return out
}

func newTreeReconciler() *ZoneEstopReconciler {
	return &ZoneEstopReconciler{Client: envK8s, Scheme: envScheme, Estopper: &fakeEstopper{},
		Recorder: record.NewFakeRecorder(100)}
}

func reconcileZoneIn(t *testing.T, r *ZoneEstopReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

func TestEnvtest_ZoneEstopStatus_ChildTriggeredResetsWhenTheChildClears(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedEstopTree(t, ns)
	r := newTreeReconciler()

	setEstopTrigger(t, ns, "bay", "spill")
	reconcileZoneIn(t, r, ns, "bay")
	expectTree(t, ns, "bay triggered", map[string]zoneEstopView{
		"site": zClear, "floor": zChild, "bay": zTriggered, "dock": zClear})

	setEstopTrigger(t, ns, "bay", "")
	reconcileZoneIn(t, r, ns, "bay")
	expectTree(t, ns, "bay cleared", map[string]zoneEstopView{
		"site": zClear, "floor": zClear, "bay": zClear, "dock": zClear})
}

func TestEnvtest_ZoneEstopStatus_DescendantsReachedByPropagationAreWritten(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedEstopTree(t, ns)
	r := newTreeReconciler()

	setEstopTrigger(t, ns, "site", "fire")
	reconcileZoneIn(t, r, ns, "site")
	all := map[string]zoneEstopView{"site": zTriggered, "floor": zTriggered, "bay": zTriggered, "dock": zTriggered}
	expectTree(t, ns, "site triggered", all)

	// RA-1: a settled tree is not written again, whichever zone is reconciled.
	before := treeVersions(t, ns)
	for _, name := range []string{"site", "floor", "bay", "dock"} {
		reconcileZoneIn(t, r, ns, name)
	}
	if after := treeVersions(t, ns); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a settled tree was written again: %v -> %v", before, after)
	}

	setEstopTrigger(t, ns, "site", "")
	reconcileZoneIn(t, r, ns, "site")
	expectTree(t, ns, "site cleared", map[string]zoneEstopView{
		"site": zClear, "floor": zClear, "bay": zClear, "dock": zClear})
}

// Two estops overlap on bay: its own, and site's carried down. Clearing one leaves the
// other in force, and the tree says so until the last is cleared.
func TestEnvtest_ZoneEstopStatus_OverlappingEstops(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedEstopTree(t, ns)
	r := newTreeReconciler()
	all := map[string]zoneEstopView{"site": zTriggered, "floor": zTriggered, "bay": zTriggered, "dock": zTriggered}

	setEstopTrigger(t, ns, "site", "fire")
	reconcileZoneIn(t, r, ns, "site")
	setEstopTrigger(t, ns, "bay", "spill")
	reconcileZoneIn(t, r, ns, "bay")
	// floor is Triggered by site's estop; a child's notification does not demote it.
	expectTree(t, ns, "site and bay triggered", all)

	setEstopTrigger(t, ns, "bay", "")
	reconcileZoneIn(t, r, ns, "bay")
	expectTree(t, ns, "bay cleared, site still active", all)

	setEstopTrigger(t, ns, "bay", "spill-2")
	reconcileZoneIn(t, r, ns, "bay")
	setEstopTrigger(t, ns, "site", "")
	reconcileZoneIn(t, r, ns, "site")
	expectTree(t, ns, "site cleared, bay still active", map[string]zoneEstopView{
		"site": zClear, "floor": zChild, "bay": zTriggered, "dock": zClear})

	setEstopTrigger(t, ns, "bay", "")
	reconcileZoneIn(t, r, ns, "bay")
	expectTree(t, ns, "all cleared", map[string]zoneEstopView{
		"site": zClear, "floor": zClear, "bay": zClear, "dock": zClear})
}

// The same tree under a running manager, where every zone write queues its own reconcile
// and reads are served from the informer cache.
func TestEnvtest_ZoneEstopStatus_FullManager(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	seedEstopTree(t, ns)

	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:  envScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&fleetv1.FleetZone{}).Named("zoneestop-" + ns).
		Complete(&ZoneEstopReconciler{Client: mgr.GetClient(), Scheme: envScheme, Estopper: &fakeEstopper{},
			Recorder: record.NewFakeRecorder(1000)}); err != nil {
		t.Fatalf("register zone estop reconciler: %v", err)
	}
	mctx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(mctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the manager did not stop")
		}
	})

	settle := func(step string, want map[string]zoneEstopView) {
		t.Helper()
		waitFor(t, step, func() bool {
			for name, v := range want {
				if viewOf(treeZone(t, ns, name)) != v {
					return false
				}
			}
			return true
		}, func() string {
			s := ""
			for _, name := range []string{"site", "floor", "bay", "dock"} {
				v := viewOf(treeZone(t, ns, name))
				s += fmt.Sprintf("%s=%s/%v ", name, v.status, v.stamp)
			}
			return s
		})
	}
	all := map[string]zoneEstopView{"site": zTriggered, "floor": zTriggered, "bay": zTriggered, "dock": zTriggered}
	cleared := map[string]zoneEstopView{"site": zClear, "floor": zClear, "bay": zClear, "dock": zClear}

	setEstopTrigger(t, ns, "bay", "spill")
	settle("bay triggered", map[string]zoneEstopView{"site": zClear, "floor": zChild, "bay": zTriggered, "dock": zClear})
	setEstopTrigger(t, ns, "site", "fire")
	settle("site and bay triggered", all)
	setEstopTrigger(t, ns, "bay", "")
	settle("bay cleared, site still active", all)
	setEstopTrigger(t, ns, "site", "")
	settle("all cleared", cleared)

	// Settled: nothing is written while nothing changes.
	before := treeVersions(t, ns)
	time.Sleep(time.Second)
	if after := treeVersions(t, ns); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a settled tree was written again: %v -> %v", before, after)
	}
}
