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
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A namespace estop fans out to the robots in the namespace when it is triggered. A Robot
// created there afterwards was never in that fan-out, so the namespace's stop gates
// dispatch by itself, and the namespace-estop controller stops the late arrival.

func stopConfig(ns string, ann map[string]string) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: SwarmadaConfigName, Annotations: ann}}
}

// The namespace's stop withholds every robot in it whatever the robot's own estopState
// says: requested and not yet fanned out, fanned out, and cleared but the clear not yet
// applied. A stop in another namespace, or none, withholds nothing.
func TestDispatch_NoWorkInAStoppedNamespace(t *testing.T) {
	cases := []struct {
		name     string
		config   *fleetv1.SwarmadaConfig
		withheld bool
	}{
		{"fanned out", stopConfig(actionNS, map[string]string{
			annEstopTriggered: "s1", annEstopProcessed: "s1"}), true},
		{"requested, before its fan-out", stopConfig(actionNS, map[string]string{
			annEstopTriggered: "s1"}), true},
		{"a new trigger over an old one", stopConfig(actionNS, map[string]string{
			annEstopTriggered: "s2", annEstopProcessed: "s1"}), true},
		{"cleared, the clear not yet applied", stopConfig(actionNS, map[string]string{
			annEstopProcessed: "s1"}), true},
		{"no stop", stopConfig(actionNS, nil), false},
		{"a stop in another namespace", stopConfig("warehouse-b", map[string]string{
			annEstopTriggered: "s1", annEstopProcessed: "s1"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			robot := robotInPhase("fresh", fleetv1.RobotPhaseIdle, "")
			r, _ := newActionReconciler(t, tc.config, robot)
			eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
				pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*robot})
			why := withheldReason(withheld, "fresh")
			if !tc.withheld {
				if len(eligible) != 1 {
					t.Fatalf("the robot should be dispatchable; withheld: %q", why)
				}
				return
			}
			if len(eligible) != 0 {
				t.Fatalf("a robot with estopState %q is eligible while its namespace's estop is in force",
					robot.Status.EstopState)
			}
			if !strings.Contains(why, "namespace") || !strings.Contains(why, "estop") {
				t.Errorf("the exclusion should name the namespace's estop; got %q", why)
			}
			for _, e := range withheld {
				if e.Robot == "fresh" && e.Filter != filterEstop {
					t.Errorf("the namespace's stop should report as filter %d, got %d", filterEstop, e.Filter)
				}
			}
		})
	}
}

// A SwarmadaConfig list that cannot be read withholds every candidate: whether the
// namespace's estop holds the robot is unknown.
func TestDispatch_NamespaceStopFailsClosedOnAnUnreadableConfigList(t *testing.T) {
	robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
	r, _ := newActionReconciler(t, robot)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*fleetv1.SwarmadaConfigList); ok {
				return errors.New("configs unavailable")
			}
			return c.List(ctx, list, opts...)
		},
	})
	eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
		pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*robot})
	if len(eligible) != 0 {
		t.Fatalf("an unreadable SwarmadaConfig list must withhold the robot")
	}
	if why := withheldReason(withheld, "r1"); !strings.Contains(why, "configs unavailable") {
		t.Errorf("the exclusion should carry the read error; got %q", why)
	}
}

// A Robot created in the namespace after the fan-out is stopped through the confirmed
// per-robot primitive, once per episode. A robot the fan-out reached is not stopped again,
// and a reconcile with nobody new does nothing.
func TestNamespaceEstop_StopsARobotThatArrivesAfterTheFanOut(t *testing.T) {
	est := &fakeEstopper{}
	held := zeRobot("held", "")
	held.Status.EstopState = fleetv1.RobotEstopStopped
	r, c := newNamespaceEstop(t, est, nsConfig("stop", ""), zeRobot("amr-1", ""), held)

	reconcileNsEstop(t, r) // the fan-out
	if got := est.names(); len(got) != 2 {
		t.Fatalf("fan-out stopped %v, want both robots", got)
	}
	reconcileNsEstop(t, r)
	if got := est.names(); len(got) != 2 {
		t.Fatalf("a reconcile with nobody new stopped again: %v", got)
	}

	if err := c.Create(context.Background(), zeRobot("late", "")); err != nil {
		t.Fatal(err)
	}
	reconcileNsEstop(t, r)
	if got := est.names(); len(got) != 3 || !hasName(got, "late") {
		t.Fatalf("stopped %v, want the late arrival added", got)
	}
	reconcileNsEstop(t, r)
	if got := est.names(); len(got) != 3 {
		t.Fatalf("the late arrival was stopped twice in one episode: %v", got)
	}
}

// After a clear, a new stop is a new episode: its fan-out reaches every robot and a robot
// that arrives after it is stopped too.
func TestNamespaceEstop_ANewStopAfterAClearIsANewEpisode(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newNamespaceEstop(t, est, nsConfig("stop", ""), zeRobot("amr-1", ""))
	reconcileNsEstop(t, r) // fan-out of the first stop

	setNsTrigger(t, c, "") // the clear
	reconcileNsEstop(t, r)
	if got := est.clearedNames(); len(got) != 1 {
		t.Fatalf("cleared %v, want amr-1", got)
	}

	setNsTrigger(t, c, "stop") // the same value again, a new stop
	reconcileNsEstop(t, r)
	if err := c.Create(context.Background(), zeRobot("late", "")); err != nil {
		t.Fatal(err)
	}
	reconcileNsEstop(t, r)
	if got := est.names(); len(got) != 3 || !hasName(got, "late") {
		t.Fatalf("stopped %v, want amr-1 twice (one per stop) and the late arrival", got)
	}
}

func setNsTrigger(t *testing.T, c client.Client, trigger string) {
	t.Helper()
	cfg := nsGetConfig(t, c)
	if cfg.Annotations == nil {
		cfg.Annotations = map[string]string{}
	}
	if trigger == "" {
		delete(cfg.Annotations, annEstopTriggered)
	} else {
		cfg.Annotations[annEstopTriggered] = trigger
	}
	if err := c.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func hasName(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
