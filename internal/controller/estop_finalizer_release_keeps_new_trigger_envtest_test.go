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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// writeBeforeFinalizerRelease runs write once, just before the first patch that writes the
// finalizer list without the stop finalizer is sent.
type writeBeforeFinalizerRelease struct {
	client.Client
	mu    sync.Mutex
	write func()
}

func (c *writeBeforeFinalizerRelease) Patch(ctx context.Context, obj client.Object, p client.Patch, o ...client.PatchOption) error {
	c.mu.Lock()
	write := c.write
	if write != nil {
		data, err := p.Data(obj)
		if err != nil || !bytes.Contains(data, []byte(`"finalizers":`)) || bytes.Contains(data, []byte(estopscope.StopFinalizer)) {
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

// releaseWithAWriteBeforeIt makes a carrier that holds the stop finalizer and carries no stop (a
// trigger withdrawn before its pass ran), and runs one pass on it, making write just before the
// pass's release of the finalizer. It fails the test if the pass sent no release.
func releaseWithAWriteBeforeIt(t *testing.T, rig *onceRig, sc clearScope, write func(carrier client.Object)) ctrl.Request {
	t.Helper()
	carrier := sc.create(t, rig)
	admitTrigger(t, carrier, "spill", "alice")
	admitTrigger(t, carrier, "", "alice")
	if !controllerutil.ContainsFinalizer(carrier, estopscope.StopFinalizer) {
		t.Fatalf("setup: finalizers = %v, want the stop finalizer the withdrawn trigger left", carrier.GetFinalizers())
	}
	c := &writeBeforeFinalizerRelease{Client: envK8s}
	c.write = func() { write(carrier.DeepCopyObject().(client.Object)) }
	r := sc.reconciler(rig, c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: carrier.GetName()}}
	if _, err := r(context.Background(), req); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	c.mu.Lock()
	unwritten := c.write != nil
	c.mu.Unlock()
	if unwritten {
		t.Fatal("the pass sent no patch releasing the stop finalizer")
	}
	return req
}

// A trigger admitted after the pass read the carrier, and before the pass released the stop
// finalizer, keeps the finalizer the webhooks added with it, and the same pass issues its stop. A
// delete that comes next leaves the carrier in place.
func TestFinalizerReleaseKeepsNewTrigger_ATriggerWrittenBeforeTheReleaseKeepsItsHold(t *testing.T) {
	requireEnvtest(t)
	for _, sc := range clearScopes {
		t.Run(sc.name, func(t *testing.T) {
			rig := newOnceRig(t, "amr-1")
			req := releaseWithAWriteBeforeIt(t, rig, sc, func(carrier client.Object) {
				admitTrigger(t, carrier, "person detected", "carol")
			})
			carrier := releasedCarrier(t, sc, req)
			if carrier == nil {
				t.Fatal("the carrier is gone after the pass")
			}
			if v := carrier.GetAnnotations()[annEstopTriggered]; v != "person detected" {
				t.Errorf("trigger = %q, want %q", v, "person detected")
			}
			if !controllerutil.ContainsFinalizer(carrier, estopscope.StopFinalizer) {
				t.Errorf("finalizers = %v, want the stop finalizer: the trigger written before the release lost its hold",
					carrier.GetFinalizers())
			}
			triggered := rig.entries(audit.EventEstopTriggered)
			if len(triggered) != 1 {
				t.Fatalf("ESTOP_TRIGGERED entries after the pass = %d, want 1: the pass did not issue the new trigger's stop",
					len(triggered))
			}
			if id := triggered[0].Actor.Identity; id != "carol" {
				t.Errorf("the stop's actor = %q, want carol", id)
			}
			if err := envK8s.Delete(context.Background(), carrier); err != nil {
				t.Fatalf("delete: %v", err)
			}
			after := releasedCarrier(t, sc, req)
			if after == nil {
				t.Fatal("the carrier is gone after a delete, with its stop in force")
			}
		})
	}
}

// With no write between the read and the release, the stop finalizer is released and no stop is
// issued.
func TestFinalizerReleaseKeepsNewTrigger_WithNoWriteTheFinalizerIsReleased(t *testing.T) {
	requireEnvtest(t)
	for _, sc := range clearScopes {
		t.Run(sc.name, func(t *testing.T) {
			rig := newOnceRig(t, "amr-1")
			req := releaseWithAWriteBeforeIt(t, rig, sc, func(client.Object) {})
			carrier := releasedCarrier(t, sc, req)
			if carrier == nil {
				t.Fatal("the carrier is gone after the pass")
			}
			if controllerutil.ContainsFinalizer(carrier, estopscope.StopFinalizer) {
				t.Errorf("finalizers = %v, want the stop finalizer released", carrier.GetFinalizers())
			}
			if n := len(rig.entries(audit.EventEstopTriggered)); n != 0 {
				t.Errorf("ESTOP_TRIGGERED entries = %d, want 0", n)
			}
		})
	}
}

// releasedCarrier reads the carrier from the API server; nil when it is gone.
func releasedCarrier(t *testing.T, sc clearScope, req ctrl.Request) client.Object {
	t.Helper()
	var obj client.Object
	switch sc.name {
	case "zone":
		obj = &fleetv1.FleetZone{}
	case "namespace":
		obj = &fleetv1.SwarmadaConfig{}
	default:
		obj = &fleetv1.Robot{}
	}
	if err := envK8s.Get(context.Background(), req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return obj
}
