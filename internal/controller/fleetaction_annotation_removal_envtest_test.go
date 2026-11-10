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
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// writeBeforeAnnotationRemoval runs write once, just before the first patch of the action that
// removes key, or that writes the annotation map as null.
type writeBeforeAnnotationRemoval struct {
	client.Client
	key   string
	mu    sync.Mutex
	write func()
}

func (c *writeBeforeAnnotationRemoval) Patch(ctx context.Context, obj client.Object, p client.Patch, o ...client.PatchOption) error {
	c.mu.Lock()
	write := c.write
	if write != nil {
		data, err := p.Data(obj)
		if _, ok := obj.(*fleetv1.FleetAction); !ok || err != nil ||
			(!bytes.Contains(data, []byte(`"`+c.key+`":null`)) && !bytes.Contains(data, []byte(`"annotations":null`))) {
			write = nil
		}
	}
	if write != nil {
		c.write = nil
	}
	c.mu.Unlock()
	if write != nil {
		write()
	}
	return c.Client.Patch(ctx, obj, p, o...)
}

// annotationRemovalSite is one place the FleetAction reconciler removes an annotation, and the
// state that takes a single pass there.
type annotationRemovalSite struct {
	name string
	key  string
	// value is the removed annotation's value on the action.
	value string
	// failed sets the action up Failed with retries left; otherwise it is InProgress on r1.
	failed bool
	// stopped puts r1 under a stop.
	stopped bool
	cancel  command.CancelDisposition
}

var annotationRemovalSites = []annotationRemovalSite{
	{name: "a stop overtook the requeue request", key: annRequeueRequested, value: "zone maintenance", stopped: true},
	{name: "a requeue the adapter acknowledged", key: annRequeueRequested, value: "zone maintenance",
		cancel: command.CancelStoppedSafely},
	{name: "a capability-loss requeue the adapter recovered", key: annRequeueRequested, value: reasonCapabilityLost,
		cancel: command.CancelRecovered},
	{name: "a requeue whose stop was not confirmed", key: annRequeueRequested, value: "zone maintenance",
		cancel: command.CancelStopUnconfirmed},
	{name: "a capability-loss requeue the robot completed", key: annRequeueRequested, value: reasonCapabilityLost,
		cancel: command.CancelCompleted},
	{name: "a failed action retried", key: annFailureAlerted, value: "true", failed: true},
}

// removeWithAWriteBeforeIt makes the site's action, carrying only the annotation the site removes,
// runs one pass with write made just before the removal's patch, and returns the action as the
// API server then holds it.
func removeWithAWriteBeforeIt(t *testing.T, site annotationRemovalSite, write func(ns string)) *fleetv1.FleetAction {
	t.Helper()
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	ad := &fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Name: testActionAdapter, Namespace: ns}}
	must("create adapter", envK8s.Create(ctx, ad))
	ad.Status = fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected,
		Conformance: fleetv1.ConformanceStatePassed, ConformanceContractVersion: contract.Version}
	must("adapter status", envK8s.Status().Update(ctx, ad))
	robot := envtestValidRobot(ns, "r1", "bay")
	robot.Spec.Adapter = fleetv1.AdapterRef{Name: testActionAdapter, Version: "1.0.0"}
	must("create robot", envK8s.Create(ctx, robot))
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: ns, Annotations: map[string]string{site.key: site.value}},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Priority: fleetv1.ActionPriorityNormal},
	}
	must("create action", envK8s.Create(ctx, action))
	long := metav1.NewTime(time.Now().Add(-time.Hour))
	if site.failed {
		robot.Status.Phase = fleetv1.RobotPhaseIdle
		// Alerted when its retries ran out, then given more.
		action.Status = fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseFailed, FailedAt: &long,
			ScheduledAt: &long, FailureReason: "probe", AssignmentGeneration: 4}
	} else {
		robot.Status.Phase = fleetv1.RobotPhaseInProgress
		robot.Status.AssignedAction = "t1"
		if site.stopped {
			robot.Status.EstopState = fleetv1.RobotEstopStopped
		}
		live := metav1.NewTime(time.Now().Add(defaultLeaseDuration).Truncate(time.Second))
		action.Status = fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, AssignedRobot: "r1",
			AssignmentGeneration: 4, LeaseExpiresAt: &live, ScheduledAt: &long}
	}
	must("robot status", envK8s.Status().Update(ctx, robot))
	must("action status", envK8s.Status().Update(ctx, action))

	c := &writeBeforeAnnotationRemoval{Client: envK8s, key: site.key, write: func() { write(ns) }}
	r := &FleetActionReconciler{Client: c, APIReader: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Recorder: record.NewFakeRecorder(1000),
		Commander: &fakeCommander{cancelAck: true, cancelDisp: site.cancel}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "t1"}}); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	c.mu.Lock()
	unwritten := c.write != nil
	c.mu.Unlock()
	if unwritten {
		t.Fatalf("the pass sent no patch removing %s", site.key)
	}
	got := &fleetv1.FleetAction{}
	must("get action", envK8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "t1"}, got))
	return got
}

// setActionAnnotation writes one annotation on t1, as an operator or another tool would.
func setActionAnnotation(t *testing.T, ns, key, value string) {
	t.Helper()
	a := &fleetv1.FleetAction{}
	if err := envK8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "t1"}, a); err != nil {
		t.Fatal(err)
	}
	base := a.DeepCopy()
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	a.Annotations[key] = value
	if err := envK8s.Patch(context.Background(), a, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

// An annotation written while the pass handled the action is kept when the pass removes the one it
// handled, though that was the action's only annotation.
func TestAnnotationRemoval_AnotherAnnotationWrittenMeanwhileIsKept(t *testing.T) {
	for _, site := range annotationRemovalSites {
		t.Run(site.name, func(t *testing.T) {
			got := removeWithAWriteBeforeIt(t, site, func(ns string) {
				setActionAnnotation(t, ns, "example.com/note", "written meanwhile")
			})
			if got.Annotations["example.com/note"] != "written meanwhile" {
				t.Errorf("annotations = %v, want example.com/note kept", got.Annotations)
			}
			if _, ok := got.Annotations[site.key]; ok {
				t.Errorf("%s = %q, want it removed", site.key, got.Annotations[site.key])
			}
		})
	}
}

// A requeue request written over the standing one while the pass handled it is removed with it:
// the requeue check never saw it, since the annotation was already there.
func TestAnnotationRemoval_ARequestWrittenOverTheStandingOneIsRemoved(t *testing.T) {
	for _, site := range annotationRemovalSites {
		if site.key != annRequeueRequested {
			continue
		}
		t.Run(site.name, func(t *testing.T) {
			got := removeWithAWriteBeforeIt(t, site, func(ns string) {
				setActionAnnotation(t, ns, annRequeueRequested, "written over the standing request")
			})
			if v, ok := got.Annotations[annRequeueRequested]; ok {
				t.Errorf("requeue-requested = %q, want it removed", v)
			}
		})
	}
}
