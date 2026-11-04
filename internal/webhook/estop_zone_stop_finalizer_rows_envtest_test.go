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
	"fmt"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// A zone's stop finalizer is held from the moment its trigger lands until its clear has been
// applied (estopscope.StopFinalizer), and it is the control plane's: only the manager, or a
// holder of estop-clear on the zone, removes it while the zone carries a stop, and a namespace
// being torn down releases it. The manager's own writes of a zone's metadata (the finalizer it
// holds, the marker it records) are not refused by a rule about the zone's spec, so a zone that an
// ancestor's tightening left inconsistent (FZ-R8 admits that tightening by construction) still
// holds its stop and still stops a robot that arrives under it.

// The zone tree the rows use: parent P with no rule of its own, child C (the zone that is stopped)
// with a Limited speed rule. tightenParent writes a stricter rule on P, which leaves C more
// permissive than its ancestor: FZ-R8 admits the tightening, and FZ-R7 then refuses any write to
// C that runs the structural checks.
const (
	finParent = "p"
	finChild  = "c"
)

func speedRule(max string) *fleetv1.ZoneRestrictions {
	return &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
		{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: max}}}
}

func (r *rig) finalizerTree() {
	r.t.Helper()
	ctx := context.Background()
	r.must("creating zone "+finParent, r.w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: finParent, Namespace: r.w.ns}}))
	r.must("creating zone "+finChild, r.w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: finChild, Namespace: r.w.ns},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: finParent, Restrictions: speedRule("1.0")}}))
}

// tightenParent writes P's stricter rule as c, and fails the row if it is refused: every row
// below rests on the tightening being admitted.
func (r *rig) tightenParent(c client.Client) {
	r.t.Helper()
	p := r.zoneObj(finParent)
	if err := r.update(c, p, false, func() { p.Spec.Restrictions = speedRule("0.5") }); err != nil {
		r.t.Fatalf("PRECONDITION: the ancestor's tightening was refused, so no zone is left inconsistent: %v", err)
	}
}

// childFailsStructuralChecks reports whether an ordinary write to C (a label) is refused by FZ-R7.
func (r *rig) childFailsStructuralChecks() (bool, error) {
	c := r.zoneObj(finChild)
	err := r.update(r.w.admin, c, false, func() {
		l := c.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		l["touched"] = "yes"
		c.SetLabels(l)
	})
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err), err
}

func (r *rig) zoneFinalizers(name string) []string {
	r.t.Helper()
	z := r.zoneObj(name)
	r.must("reading zone "+name, r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(z), z))
	return z.Finalizers
}

func (r *rig) waitZoneFinalizer(name string, present bool, d time.Duration) bool {
	r.t.Helper()
	ok, _ := r.waitFor(fmt.Sprintf("zone %s stop finalizer present=%v", name, present), d, func() (bool, string) {
		f := r.zoneFinalizers(name)
		return controllerutil.ContainsFinalizer(r.zoneWith(f), estopscope.StopFinalizer) == present, fmt.Sprint(f)
	})
	return ok
}

func (r *rig) zoneWith(finalizers []string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Finalizers: finalizers}}
}

// adminRole is a principal bound to the shipped admin role and nothing else: it holds
// estop-clear on fleetzones.
func (r *rig) adminRole() client.Client {
	r.t.Helper()
	user := "admin-" + r.w.ns
	bindShippedClusterRole(r.t, r.w, "swarmada_admin_role.yaml", map[string]interface{}{
		"kind": "User", "name": user, "apiGroup": "rbac.authorization.k8s.io"})
	return asIdentity(r.t, r.w, user)
}

// dropStopFinalizer removes the stop finalizer from C as c, and applies extra to the same write.
func (r *rig) dropStopFinalizer(c client.Client, extra func(z *fleetv1.FleetZone)) error {
	z := r.zoneObj(finChild)
	return r.update(c, z, false, func() {
		controllerutil.RemoveFinalizer(z, estopscope.StopFinalizer)
		if extra != nil {
			extra(z)
		}
	})
}

// zoneStopFinalizerRows run on the shared wire, where the zone's estop-actor stamper is served
// and holds the finalizer on every write while the trigger stands.
func zoneStopFinalizerRows() []invariantRow {
	var rows []invariantRow

	rows = append(rows, invariantRow{name: "zone-stop-finalizer/an-ancestor-tightening-leaves-a-descendant-inconsistent",
		run: func(t *testing.T, r *rig) {
			r.finalizerTree()
			r.tightenParent(r.fleetManager())
			refused, err := r.childFailsStructuralChecks()
			t.Logf("RESULT a write to %s after its ancestor's tightening: %v", finChild, err)
			if !refused {
				t.Fatalf("PRECONDITION: %s still passes the structural checks after the tightening: %v", finChild, err)
			}
		}})

	// The trigger stands: the stamper holds the finalizer on any write, so a write that drops it
	// does not shed the stop, and a robot that arrives under it is stopped.
	rows = append(rows, invariantRow{name: "zone-stop-finalizer/stays-while-the-trigger-stands", run: func(t *testing.T, r *rig) {
		fm := r.fleetManager()
		r.finalizerTree()
		r.robot("r1", finChild)
		r.trigger(r.zoneObj(finChild), "c-stop")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj(finChild), true)
		if !r.waitZoneFinalizer(finChild, true, 15*time.Second) {
			t.Fatalf("setup: %s never held the stop finalizer", finChild)
		}
		r.tightenParent(fm)
		err := r.dropStopFinalizer(fm, nil)
		t.Logf("RESULT fleet-manager drops %s's stop finalizer under its stop: %v; finalizers now %v",
			finChild, err, r.zoneFinalizers(finChild))
		if !controllerutil.ContainsFinalizer(r.zoneWith(r.zoneFinalizers(finChild)), estopscope.StopFinalizer) {
			t.Errorf("INVARIANT: %s carries a stop and lost its stop finalizer", finChild)
		}
		r.robot("r2", finChild)
		if ok, last := r.waitFor("r2, arriving under the stop, is stopped", 45*time.Second, func() (bool, string) {
			s := r.get("r2").Status.EstopState
			return stopState(s), string(s)
		}); !ok {
			t.Errorf("INVARIANT: r2 arrived in %s under its stop and was not stopped (last %s)", finChild, last)
		}
	}})

	// After the clear and before it is applied, the zone still carries the stop (its processed
	// marker); the stamper holds nothing then, because the trigger is gone. Only the control plane
	// or an estop-clear holder removes the finalizer in that window.
	for _, who := range []string{"fleet-manager", "estop-clear-holder"} {
		who := who
		rows = append(rows, invariantRow{name: "zone-stop-finalizer/until-the-clear-is-applied/" + who, opts: rigOpts{deferStart: true},
			run: func(t *testing.T, r *rig) {
				var c client.Client
				if who == "fleet-manager" {
					c = r.fleetManager()
				} else {
					c = r.adminRole()
				}
				r.finalizerTree()
				r.robot("r1", finChild)
				r.start()
				r.trigger(r.zoneObj(finChild), "c-stop")
				r.waitStopped("r1")
				r.waitMarker(r.zoneObj(finChild), true)
				r.stopMgr() // the clear is admitted and not yet applied
				r.clearStop(r.zoneObj(finChild))
				err := r.dropStopFinalizer(c, nil)
				after := r.zoneFinalizers(finChild)
				t.Logf("RESULT %s drops %s's stop finalizer before its clear is applied: %v; finalizers now %v",
					who, finChild, err, after)
				held := controllerutil.ContainsFinalizer(r.zoneWith(after), estopscope.StopFinalizer)
				switch who {
				case "fleet-manager":
					if !held {
						t.Errorf("INVARIANT: a fleet-manager removed the stop finalizer of %s before its clear was applied", finChild)
					} else if !apierrors.IsForbidden(err) {
						t.Errorf("the removal was refused, but not as Forbidden: %v", err)
					}
				default:
					if err != nil || held {
						t.Errorf("an estop-clear holder's removal of the stop finalizer was refused: %v", err)
					}
				}
			}})
	}

	return rows
}

// startSafetyWireWithoutZoneStamper is the shared wire with the zone's estop-actor stamper
// unreachable. The stamper is failurePolicy Ignore, so the API server admits zone writes without
// it, as it does when the call fails while the validating webhook answers: a trigger then lands
// with no stop finalizer, and a write may drop the finalizer while the trigger stands. A zone
// stopped by a release that had no stop finalizer is in the same state after an upgrade.
func startSafetyWireWithoutZoneStamper(t *testing.T) *safetyWire {
	t.Helper()
	w := startSafetyWire(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := admissionregistrationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(w.cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	var list admissionregistrationv1.MutatingWebhookConfigurationList
	if err := c.List(ctx, &list); err != nil {
		t.Fatalf("listing mutating webhooks: %v", err)
	}
	found := false
	for i := range list.Items {
		mwc := &list.Items[i]
		for j := range mwc.Webhooks {
			if mwc.Webhooks[j].Name != "mfleetzone.swarmada.io" {
				continue
			}
			dead := "https://127.0.0.1:1/mutate-swarmada-io-v1-fleetzone"
			mwc.Webhooks[j].ClientConfig = admissionregistrationv1.WebhookClientConfig{URL: &dead,
				CABundle: mwc.Webhooks[j].ClientConfig.CABundle}
			found = true
		}
		if found {
			if err := c.Update(ctx, mwc); err != nil {
				t.Fatalf("pointing the zone stamper at nothing: %v", err)
			}
			break
		}
	}
	if !found {
		t.Fatalf("no mfleetzone.swarmada.io in the installed mutating webhooks")
	}
	// The API server picks the change up asynchronously: wait until a triggered zone is admitted
	// without the finalizer the stamper would add.
	probe := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "probe-"}}
	if err := w.admin.Create(ctx, probe); err != nil {
		t.Fatalf("probe namespace: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for i := 0; ; i++ {
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("probe-%d", i), Namespace: probe.Name,
			Annotations: map[string]string{estopTriggeredAnnotation: "probe"}}}
		if err := w.admin.Create(ctx, z); err != nil {
			t.Fatalf("probe zone: %v", err)
		}
		if !controllerutil.ContainsFinalizer(z, estopscope.StopFinalizer) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the zone stamper still answers 20 s after it was pointed at nothing")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return w
}

func TestEnvtest_ZoneStopFinalizerWithoutTheStamper(t *testing.T) {
	var rows []invariantRow

	// The trigger lands with no finalizer on a zone an ancestor's tightening left inconsistent.
	// The control plane holds the finalizer and stops the robots in the zone.
	rows = append(rows, invariantRow{name: "held-by-the-control-plane-on-an-inconsistent-zone", run: func(t *testing.T, r *rig) {
		r.finalizerTree()
		r.tightenParent(r.fleetManager())
		r.robot("r1", finChild)
		r.trigger(r.zoneObj(finChild), "c-stop")
		if ok, last := r.waitFor("r1 is stopped", 20*time.Second, func() (bool, string) {
			s := r.get("r1").Status.EstopState
			return stopState(s), string(s)
		}); !ok {
			t.Errorf("INVARIANT: the stop of %s never reached r1 (last %s)", finChild, last)
		}
		if !r.waitZoneFinalizer(finChild, true, 10*time.Second) {
			t.Errorf("INVARIANT: %s carries a stop and the control plane never held its stop finalizer", finChild)
		}
		r.robot("r2", finChild)
		if ok, last := r.waitFor("r2, arriving under the stop, is stopped", 45*time.Second, func() (bool, string) {
			s := r.get("r2").Status.EstopState
			return stopState(s), string(s)
		}); !ok {
			t.Errorf("INVARIANT: r2 arrived in %s under its stop and was not stopped (last %s)", finChild, last)
		}
	}})

	// While the trigger stands, a fleet-manager's write that drops the finalizer is refused, with
	// or without another change to the zone, and a robot that arrives under the stop is stopped.
	variants := []struct {
		name      string
		tighten   bool
		alongside func(z *fleetv1.FleetZone)
	}{
		{"alone", false, nil},
		{"alone-on-an-inconsistent-zone", true, nil},
		{"with-an-actor-change", true, func(z *fleetv1.FleetZone) { z.Annotations[AnnEstopActor] = "someone-else" }},
		{"with-a-resume-key", true, func(z *fleetv1.FleetZone) { z.Annotations[fleetv1.EstopResumeAnnotation] = "x" }},
	}
	for _, v := range variants {
		v := v
		rows = append(rows, invariantRow{name: "removal-refused-while-the-stop-stands/" + v.name, run: func(t *testing.T, r *rig) {
			fm := r.fleetManager()
			r.finalizerTree()
			r.robot("r1", finChild)
			r.trigger(r.zoneObj(finChild), "c-stop")
			r.waitStopped("r1")
			r.waitMarker(r.zoneObj(finChild), true)
			if !r.waitZoneFinalizer(finChild, true, 15*time.Second) {
				t.Fatalf("setup: the control plane never held %s's stop finalizer", finChild)
			}
			if v.tighten {
				r.tightenParent(fm)
			}
			err := r.dropStopFinalizer(fm, v.alongside)
			t.Logf("RESULT fleet-manager drops %s's stop finalizer (%s): %v; finalizers now %v",
				finChild, v.name, err, r.zoneFinalizers(finChild))
			if err == nil {
				t.Errorf("INVARIANT: a fleet-manager removed the stop finalizer of %s while its stop stands", finChild)
			} else if !apierrors.IsForbidden(err) {
				t.Errorf("the removal was refused, but not as Forbidden: %v", err)
			}
			r.robot("r2", finChild)
			if ok, last := r.waitFor("r2, arriving under the stop, is stopped", 45*time.Second, func() (bool, string) {
				s := r.get("r2").Status.EstopState
				return stopState(s), string(s)
			}); !ok {
				t.Errorf("INVARIANT: r2 arrived in %s under its stop and was not stopped (last %s)", finChild, last)
			}
		}})
	}

	// A namespace being torn down releases the finalizer, as the control plane does on its own.
	rows = append(rows, invariantRow{name: "released-with-a-terminating-namespace", run: func(t *testing.T, r *rig) {
		fm := r.fleetManager()
		r.finalizerTree()
		r.trigger(r.zoneObj(finChild), "c-stop")
		r.waitMarker(r.zoneObj(finChild), true)
		if !r.waitZoneFinalizer(finChild, true, 15*time.Second) {
			t.Fatalf("setup: the control plane never held %s's stop finalizer", finChild)
		}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.w.ns}}
		r.must("deleting the namespace", r.w.admin.Delete(context.Background(), ns))
		r.waitFor("the namespace is terminating", 10*time.Second, func() (bool, string) {
			if err := r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
				return false, err.Error()
			}
			return ns.DeletionTimestamp != nil, fmt.Sprint(ns.DeletionTimestamp)
		})
		err := r.dropStopFinalizer(fm, nil)
		t.Logf("RESULT fleet-manager drops %s's stop finalizer in a terminating namespace: %v", finChild, err)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("a namespace teardown would hang on the zone's stop finalizer: %v", err)
		}
	}})
	shared := startSafetyWireWithoutZoneStamper(t)
	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, shared, row.opts)
			row.run(t, r)
		})
	}
}
