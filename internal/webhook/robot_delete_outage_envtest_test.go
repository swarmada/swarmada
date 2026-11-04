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

package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The Robot delete guard runs in its own webhook, with failurePolicy Ignore. Under Fail,
// while no manager replica served, every Robot delete was refused, and a namespace
// delete printed "deleted" and stayed Terminating until a replica came back. Under
// Ignore a manager outage does not block the Robot deletes; a FleetZone, FleetTask,
// FirmwareRollout or ModelRollout in the namespace still does, since their DELETE checks
// stay Fail. The trade is that during an outage a held robot can be deleted; with the
// webhook served, it still cannot.
func TestEnvtest_ANamespaceWithAHeldRobotDeletesWhileNoManagerServes(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	ns := freshNamespace(t, w)
	markerFixture(t, w, ns)
	for _, h := range heldRobots {
		seedRobot(t, w, ns, h.name, h.seed)
	}

	// Served: the guard refuses, as before.
	if err := w.admin.Delete(ctx, robotRef(ns, "stopped")); err == nil {
		t.Fatalf("the stopped robot was deleted while the webhook is served; the guard is not in the path")
	}

	w.stopWebhooks()

	// envtest runs no namespace controller, so the deletes it would make are made here.
	if err := w.admin.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("deleting the namespace: %v", err)
	}
	if err := w.admin.DeleteAllOf(ctx, &fleetv1.Robot{}, client.InNamespace(ns)); err != nil {
		t.Fatalf("with no manager serving, the namespace's held robots were not deleted, so the namespace "+
			"would stay Terminating: %v", err)
	}
	var left fleetv1.RobotList
	if err := w.admin.List(ctx, &left, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(left.Items) != 0 {
		t.Fatalf("%d robots left in the terminating namespace", len(left.Items))
	}
}

// The disclosed trade, pinned so a change to it is a decision: with no manager serving,
// a held robot in a live namespace is deleted.
func TestEnvtest_AHeldRobotCanBeDeletedWhileNoManagerServes(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	seedRobot(t, w, w.ns, "stopped", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopped })
	w.stopWebhooks()
	if err := w.admin.Delete(ctx, robotRef(w.ns, "stopped")); err != nil {
		t.Fatalf("with no manager serving, the delete was refused; the delete guard is not failurePolicy "+
			"Ignore: %v", err)
	}
}
