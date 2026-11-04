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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A SemanticEventSource's acceptedCount and rejectedCount count the events naming it
// that were evaluated, by outcome. A rejected count that stays 0 while events are being
// refused tells an operator nothing was refused, which is the one thing the counter is
// for. Each event is reconciled twice, as the manager does: once to evaluate it, and
// once when its own Accepted or Rejected status reaches the cache.
func TestEnvtest_SemanticEventSourceCountsAcceptedAndRejectedEvents(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &TaskTriggerReconciler{Client: envK8s, Scheme: envScheme}

	if err := envK8s.Create(ctx, &fleetv1.SemanticEventSource{
		ObjectMeta: metav1.ObjectMeta{Name: "yard-sensors", Namespace: ns},
		Spec:       fleetv1.SemanticEventSourceSpec{EventTypes: []string{"IntruderDetected"}},
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	raise := func(name, eventType string) {
		t.Helper()
		if err := envK8s.Create(ctx, &fleetv1.SemanticEvent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       fleetv1.SemanticEventSpec{Source: "yard-sensors", Type: eventType},
		}); err != nil {
			t.Fatalf("create event %s: %v", name, err)
		}
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
				t.Fatalf("reconcile %s: %v", name, err)
			}
		}
	}
	counts := func() (int64, int64) {
		t.Helper()
		var src fleetv1.SemanticEventSource
		if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "yard-sensors"}, &src); err != nil {
			t.Fatalf("get source: %v", err)
		}
		return src.Status.AcceptedCount, src.Status.RejectedCount
	}

	raise("ev-1", "IntruderDetected")
	raise("ev-2", "IntruderDetected")
	raise("ev-3", "IntruderDetcted") // the typo the rejected count exists to surface
	if a, rj := counts(); a != 2 || rj != 1 {
		t.Fatalf("acceptedCount=%d rejectedCount=%d, want 2 and 1", a, rj)
	}

	// A resync of an evaluated event changes nothing, and writes nothing.
	var before fleetv1.SemanticEventSource
	_ = envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "yard-sensors"}, &before)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "ev-1"}}); err != nil {
		t.Fatalf("resync: %v", err)
	}
	var after fleetv1.SemanticEventSource
	_ = envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "yard-sensors"}, &after)
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("a resync with nothing changed wrote the source (resourceVersion %s -> %s)",
			before.ResourceVersion, after.ResourceVersion)
	}
}
