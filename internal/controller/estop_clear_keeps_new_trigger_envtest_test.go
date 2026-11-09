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

// A clear's last write removes the processed marker, the operator stamp and the stop finalizer.
// It is computed from the carrier as the clear pass read it, so a write that lands while the pass
// runs must survive it: a trigger, with the stamp and the finalizer the webhooks add to it, is a
// new stop and is issued as one; any other write is kept, and the clear is still made once. The
// test writes to the API server just before the marker patch reaches it, so no race is needed.

import (
	"bytes"
	"context"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// writeBeforeMarkerRemoval runs write once, just before the first patch that removes the
// processed marker is sent.
type writeBeforeMarkerRemoval struct {
	client.Client
	mu    sync.Mutex
	write func()
}

func (c *writeBeforeMarkerRemoval) Patch(ctx context.Context, obj client.Object, p client.Patch, o ...client.PatchOption) error {
	c.mu.Lock()
	write := c.write
	if write != nil {
		data, err := p.Data(obj)
		if err != nil || (!bytes.Contains(data, []byte(`"`+annEstopProcessed+`":null`)) && !bytes.Contains(data, []byte(`"annotations":null`))) {
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

// admitTrigger writes the trigger as the webhooks admit it: with the operator's stamp and the stop
// finalizer. An empty value removes the trigger and stamps the clearing operator.
func admitTrigger(t *testing.T, obj client.Object, value, operator string) {
	t.Helper()
	if err := envK8s.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	an := obj.GetAnnotations()
	if an == nil {
		an = map[string]string{}
	}
	if value == "" {
		delete(an, annEstopTriggered)
	} else {
		an[annEstopTriggered] = value
		controllerutil.AddFinalizer(obj, estopscope.StopFinalizer)
	}
	an[annEstopActor] = operator
	obj.SetAnnotations(an)
	if err := envK8s.Patch(context.Background(), obj, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

// clearScope is one carrier: a zone, a namespace's SwarmadaConfig or a robot.
type clearScope struct {
	name string
	// create makes the carrier and returns it; robots are made by the rig.
	create     func(t *testing.T, rig *onceRig) client.Object
	reconciler func(rig *onceRig, c client.Client) reconcileFunc
}

var clearScopes = []clearScope{
	{
		name: "zone",
		create: func(t *testing.T, rig *onceRig) client.Object {
			zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}
			if err := envK8s.Create(context.Background(), zone); err != nil {
				t.Fatal(err)
			}
			return zone
		},
		reconciler: func(rig *onceRig, c client.Client) reconcileFunc {
			return (&ZoneEstopReconciler{Client: c, Scheme: envScheme, Estopper: rig.disp,
				Recorder: record.NewFakeRecorder(100), Audit: rig.aud, APIReader: envK8s}).Reconcile
		},
	},
	{
		name: "namespace",
		create: func(t *testing.T, rig *onceRig) client.Object {
			cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: rig.ns, Name: "swarmada-config"}}
			if err := envK8s.Create(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			return cfg
		},
		reconciler: func(rig *onceRig, c client.Client) reconcileFunc {
			return (&NamespaceEstopReconciler{Client: c, Scheme: envScheme, Estopper: rig.disp, Audit: rig.aud,
				APIReader: envK8s}).Reconcile
		},
	},
	{
		name: "robot",
		create: func(t *testing.T, rig *onceRig) client.Object {
			return getEnvRobot(t, rig.ns, "amr-1")
		},
		reconciler: func(rig *onceRig, c client.Client) reconcileFunc {
			return (&RobotEstopReconciler{Client: c, Scheme: envScheme, Estopper: rig.disp, Audit: rig.aud,
				APIReader: envK8s}).Reconcile
		},
	},
}

// clearWithAWriteBeforeTheMarkerRemoval stops the carrier, clears it, and makes write just before
// the clear's marker patch. It then reconciles once more, as the watch event of the last write
// would. It returns the carrier as the API server holds it.
func clearWithAWriteBeforeTheMarkerRemoval(t *testing.T, rig *onceRig, sc clearScope, other bool,
	write func(carrier client.Object)) client.Object {
	t.Helper()
	carrier := sc.create(t, rig)
	if other {
		setAnnotation(t, carrier, "example.com/owner", "ops")
	}
	c := &writeBeforeMarkerRemoval{Client: envK8s}
	r := sc.reconciler(rig, c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: carrier.GetName()}}

	admitTrigger(t, carrier, "person detected", "alice")
	if _, err := r(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, "amr-1")
	rig.waitState(t, "amr-1", fleetv1.RobotEstopStopped)

	admitTrigger(t, carrier, "", "bob")
	c.mu.Lock()
	c.write = func() { write(carrier.DeepCopyObject().(client.Object)) }
	c.mu.Unlock()
	if _, err := r(context.Background(), req); err != nil {
		t.Fatalf("the clear's pass: %v", err)
	}
	c.mu.Lock()
	unwritten := c.write != nil
	c.mu.Unlock()
	if unwritten {
		t.Fatal("the clear's pass sent no patch removing the processed marker")
	}
	if _, err := r(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := carrier.DeepCopyObject().(client.Object)
	if err := envK8s.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// entries returns the audit entries of the event type, in order.
func (rig *onceRig) entries(eventType string) []audit.Entry {
	rig.aud.mu.Lock()
	defer rig.aud.mu.Unlock()
	var out []audit.Entry
	for _, e := range rig.aud.entries {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// A trigger written while the clear ran is a new stop: it stays on the carrier with its stamp and
// its finalizer, its stop is issued and attributed to its operator, and the clear is recorded once.
// The trigger's value is the cleared stop's own value or a new one, and the carrier carries no other
// annotation, which a merge patch leaves as an empty map, or one more.
func TestClearKeepsNewTrigger_ATriggerWrittenDuringTheClearIsANewStop(t *testing.T) {
	requireEnvtest(t)
	for _, sc := range clearScopes {
		for _, value := range []string{"spill", "person detected"} {
			for _, other := range []bool{false, true} {
				name := sc.name + "/value=" + value
				if other {
					name += "/one-more-annotation"
				}
				t.Run(name, func(t *testing.T) {
					rig := newOnceRig(t, "amr-1")
					got := clearWithAWriteBeforeTheMarkerRemoval(t, rig, sc, other, func(carrier client.Object) {
						admitTrigger(t, carrier, value, "carol")
					})
					an := got.GetAnnotations()
					if v, ok := an[annEstopTriggered]; !ok || v != value {
						t.Errorf("trigger after the clear = %q (present %v), want %q: the trigger written during the clear was lost", v, ok, value)
					}
					if v, ok := an[annEstopProcessed]; !ok || v != value {
						t.Errorf("processed marker = %q (present %v), want %q: the new trigger's stop was not recorded as issued", v, ok, value)
					}
					if !controllerutil.ContainsFinalizer(got, estopscope.StopFinalizer) {
						t.Errorf("finalizers = %v, want the stop finalizer while the new stop is in force", got.GetFinalizers())
					}
					if other && an["example.com/owner"] != "ops" {
						t.Errorf("annotations = %v, want example.com/owner kept", an)
					}
					triggered := rig.entries(audit.EventEstopTriggered)
					if len(triggered) != 2 {
						t.Fatalf("ESTOP_TRIGGERED entries = %d, want 2: the new trigger's stop was not issued", len(triggered))
					}
					if id := triggered[1].Actor.Identity; id != "carol" {
						t.Errorf("the new stop's actor = %q, want carol: the new trigger's stamp was lost", id)
					}
					if n := len(rig.entries(audit.EventEstopCleared)); n != 1 {
						t.Errorf("ESTOP_CLEARED entries = %d, want 1", n)
					}
					rig.waitState(t, "amr-1", fleetv1.RobotEstopStopped)
				})
			}
		}
	}
}

// Any other write that lands while the clear runs is kept, and the clear is still made once: the
// marker, the clearing operator's stamp and the stop finalizer go, and one ESTOP_CLEARED is written.
func TestClearKeepsNewTrigger_AnotherWriteDuringTheClearIsKeptAndTheClearIsMadeOnce(t *testing.T) {
	requireEnvtest(t)
	for _, sc := range clearScopes {
		t.Run(sc.name, func(t *testing.T) {
			rig := newOnceRig(t, "amr-1")
			got := clearWithAWriteBeforeTheMarkerRemoval(t, rig, sc, false, func(carrier client.Object) {
				setAnnotation(t, carrier, "example.com/owner", "ops")
			})
			an := got.GetAnnotations()
			if an["example.com/owner"] != "ops" {
				t.Errorf("annotations = %v, want example.com/owner kept", an)
			}
			for _, k := range []string{annEstopTriggered, annEstopProcessed, annEstopActor} {
				if v, ok := an[k]; ok {
					t.Errorf("%s = %q after the clear, want it gone", k, v)
				}
			}
			if sc.name != "robot" && controllerutil.ContainsFinalizer(got, estopscope.StopFinalizer) {
				t.Errorf("finalizers = %v, want the stop finalizer released", got.GetFinalizers())
			}
			if n := len(rig.entries(audit.EventEstopCleared)); n != 1 {
				t.Errorf("ESTOP_CLEARED entries = %d, want 1", n)
			}
			if n := len(rig.entries(audit.EventEstopTriggered)); n != 1 {
				t.Errorf("ESTOP_TRIGGERED entries = %d, want 1", n)
			}
			rig.waitState(t, "amr-1", fleetv1.RobotEstopResuming)
		})
	}
}
