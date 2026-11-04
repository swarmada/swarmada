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
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// A Robot name is unique per namespace only. Two robots with the same name in two
// namespaces are two robots, and each is told the rules in force where it stands,
// once per change of that answer (FA-R7).

// pushesByNamespace records constraint pushes by the namespace they were sent to.
type pushesByNamespace struct {
	mu sync.Mutex
	n  map[string]int
}

func (p *pushesByNamespace) NotifyZoneConstraints(_ context.Context, namespace, _ string, _ command.ZoneConstraints) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n[namespace]++
}

func (p *pushesByNamespace) get(ns string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n[ns]
}

func TestContainmentPush_SameRobotNameInTwoNamespacesEachGetsItsOwnPush(t *testing.T) {
	const nsA, nsB, name = "warehouse-a", "warehouse-b", "amr-1"
	var objs []client.Object
	for _, ns := range []string{nsA, nsB} {
		// The same zone and the same rule in both namespaces, so both robots' rule sets
		// have the same fingerprint: the case a memo keyed by name alone suppresses.
		ward := restrictedZone("ward", wardPts, denies("camera", "patient privacy"))
		ward.Namespace = ns
		objs = append(objs, ward, &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Annotations: map[string]string{RobotIDAnnotation: "rid1"},
		}})
	}
	z, _ := newZoneController(t, objs...)
	push := &pushesByNamespace{n: map[string]int{}}
	z.Constraints = push
	for _, ns := range []string{nsA, nsB} {
		z.setZones(ns, []zone.Candidate{{
			Name: "ward", Floor: 0, Polygon: toZonePoints(wardPts), IsLeaf: true, Depth: 1,
		}})
	}

	ctx := context.Background()
	inside := &telemetry.Position{X: 60, Y: 40, Floor: 0}
	for i := 0; i < 5; i++ {
		for _, ns := range []string{nsA, nsB} {
			z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Namespace: ns, Position: inside})
		}
	}
	if a, b := push.get(nsA), push.get(nsB); a != 1 || b != 1 {
		t.Fatalf("constraint pushes: %s/%s=%d, %s/%s=%d, want 1 and 1 — each robot is told its rules "+
			"once, and five unchanged frames each send nothing more", nsA, name, a, nsB, name, b)
	}
}
