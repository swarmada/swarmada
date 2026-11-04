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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

// The presence of swarmada.io/estop-triggered is the trigger, whatever its value
// (RFC-0001 #safety-estop-scopes). An empty value is what `kubectl annotate ...
// swarmada.io/estop-triggered=` writes, and what a script writes from an unset shell
// variable. Each controller compared the trigger with the processed marker's value only,
// and an absent marker reads as the empty string, so an empty trigger read as "this exact
// estop already issued": the write was admitted and stopped nothing.

// reasonEstopper is fakeEstopper that also records the reason each robot was stopped with.
type reasonEstopper struct {
	fakeEstopper
	reasons sync.Map // robot name -> reason
}

func (e *reasonEstopper) TriggerEstop(ctx context.Context, ns, robotID, reason, by string,
	scope metrics.EstopScope, carrier safety.Carrier) (safety.Result, error) {
	e.reasons.Store(robotID, reason)
	return e.fakeEstopper.TriggerEstop(ctx, ns, robotID, reason, by, scope, carrier)
}

func (e *reasonEstopper) reason(robot string) string {
	v, _ := e.reasons.Load(robot)
	s, _ := v.(string)
	return s
}

// emptyTrigger puts an empty-valued trigger on obj's annotations, with no processed marker.
func emptyTrigger(meta *metav1.ObjectMeta) {
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[annEstopTriggered] = ""
}

func TestRobotEstop_AnEmptyTriggerStopsTheRobot(t *testing.T) {
	est := &reasonEstopper{}
	robot := robotWithEstop("amr-1", "", "")
	emptyTrigger(&robot.ObjectMeta)
	r, c := newRobotEstop(t, &est.fakeEstopper, robot)
	r.Estopper = est

	reconcileRobotEstop(t, r, "amr-1")
	if got := est.names(); len(got) != 1 || got[0] != "amr-1" {
		t.Fatalf("an empty-valued trigger stopped %v, want [amr-1]", got)
	}
	if got := est.reason("amr-1"); got != "none given" {
		t.Errorf("recorded reason = %q, want %q", got, "none given")
	}
	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zeNS, Name: "amr-1"}, got); err != nil {
		t.Fatal(err)
	}
	if v, marked := got.Annotations[annEstopProcessed]; !marked || v != "" {
		t.Fatalf("processed marker = %q (present %v), want the empty trigger value recorded", v, marked)
	}

	// The same empty trigger, reconciled again, is this stop already issued.
	reconcileRobotEstop(t, r, "amr-1")
	if got := est.names(); len(got) != 1 {
		t.Fatalf("a re-reconcile of the same empty trigger stopped %d times, want 1", len(got))
	}

	// Removing it clears the robot.
	delete(got.Annotations, annEstopTriggered)
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileRobotEstop(t, r, "amr-1")
	if got := est.clearedNames(); len(got) != 1 || got[0] != "amr-1" {
		t.Fatalf("removing the empty trigger cleared %v, want [amr-1]", got)
	}
}

func TestZoneEstop_AnEmptyTriggerStopsTheZone(t *testing.T) {
	est := &reasonEstopper{}
	zone := zeZone("bay-4", "", nil, "")
	emptyTrigger(&zone.ObjectMeta)
	r, c := newZEReconciler(t, &est.fakeEstopper, nil, zone,
		zeZone("bay-4a", "bay-4", nil, ""), zeRobot("r-bay", "bay-4"), zeRobot("r-child", "bay-4a"))
	r.Estopper = est

	reconcileZE(t, r, "bay-4")
	if got := est.names(); len(got) != 2 {
		t.Fatalf("an empty-valued zone trigger stopped %v, want both robots in scope", got)
	}
	if got := est.reason("r-bay"); got != "none given" {
		t.Errorf("recorded reason = %q, want %q", got, "none given")
	}
	z := zeGetZone(t, c, "bay-4")
	if v, marked := z.Annotations[annEstopProcessed]; !marked || v != "" {
		t.Fatalf("processed marker = %q (present %v), want the empty trigger value recorded", v, marked)
	}
	if z.Status.EstopStatus != fleetv1.ZoneEstopTriggered {
		t.Errorf("zone estopStatus = %q, want Triggered while the empty-valued stop is in force", z.Status.EstopStatus)
	}

	// The stop is in force: a re-reconcile does not fan out again, and a robot that
	// arrives in the zone is stopped.
	reconcileZE(t, r, "bay-4")
	if got := est.names(); len(got) != 2 {
		t.Fatalf("a re-reconcile of the same empty trigger re-fanned out: %v", got)
	}
	if err := c.Create(context.Background(), zeRobot("r-late", "bay-4")); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "bay-4")
	if got := est.names(); len(got) != 3 || est.reason("r-late") == "" {
		t.Fatalf("a robot that arrived under the empty-valued stop was not stopped: %v", got)
	}

	// Removing it clears the zone's robots.
	z = zeGetZone(t, c, "bay-4")
	delete(z.Annotations, annEstopTriggered)
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "bay-4")
	if got := est.clearedNames(); len(got) != 3 {
		t.Fatalf("removing the empty zone trigger cleared %v, want all 3 robots", got)
	}
}

func TestNamespaceEstop_AnEmptyTriggerStopsAndClears(t *testing.T) {
	est := &reasonEstopper{}
	cfg := nsConfig("", "")
	emptyTrigger(&cfg.ObjectMeta)
	r, c := newNamespaceEstop(t, &est.fakeEstopper, cfg, zeRobot("amr-1", "z1"), zeRobot("amr-2", ""))
	r.Estopper = est

	reconcileNsEstop(t, r)
	if got := est.names(); len(got) != 2 {
		t.Fatalf("an empty-valued namespace trigger stopped %v, want both robots", got)
	}
	if got := est.reason("amr-1"); got != "none given" {
		t.Errorf("recorded reason = %q, want %q", got, "none given")
	}
	if v, marked := nsGetConfig(t, c).Annotations[annEstopProcessed]; !marked || v != "" {
		t.Fatalf("processed marker = %q (present %v), want the empty trigger value recorded", v, marked)
	}

	got := nsGetConfig(t, c)
	delete(got.Annotations, annEstopTriggered)
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileNsEstop(t, r)
	if got := est.clearedNames(); len(got) != 2 {
		t.Fatalf("removing the empty namespace trigger cleared %v, want both robots", got)
	}
	if _, still := nsGetConfig(t, c).Annotations[annEstopProcessed]; still {
		t.Error("processed marker not dropped on the clear")
	}
}
