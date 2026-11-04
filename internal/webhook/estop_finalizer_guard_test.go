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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// A zone's stop finalizer, while the zone carries a stop, is removed only by the control plane,
// by an estop-clear holder, or in a namespace being torn down. Through ValidateUpdate, so the
// call's place ahead of the estop-only exemption is covered too.

func zoneReader(t *testing.T, terminating bool, objs ...client.Object) client.Reader {
	t.Helper()
	sc := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: zoneNS}}
	if terminating {
		now := metav1.Now()
		ns.DeletionTimestamp = &now
		ns.Finalizers = []string{"kubernetes"}
	}
	return fake.NewClientBuilder().WithScheme(sc).WithObjects(append(objs, ns)...).Build()
}

// stoppedZone is zone c carrying a stop (trigger, processed marker or both) and the finalizer.
func stoppedZone(trigger, marker bool) *fleetv1.FleetZone {
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "c",
		Annotations: map[string]string{}, Finalizers: []string{estopscope.StopFinalizer}}}
	if trigger {
		z.Annotations[estopTriggeredAnnotation] = "c-stop"
	}
	if marker {
		z.Annotations[estopProcessedAnnotation] = "c-stop"
	}
	return z
}

func dropFinalizer(z *fleetv1.FleetZone) *fleetv1.FleetZone {
	n := z.DeepCopy()
	n.Finalizers = nil
	return n
}

func TestZoneStopFinalizer_RemovedOnlyByTheControlPlaneOrAnEstopClearHolder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		allow       bool // the requester holds estop-clear on the zone
		terminating bool
		old         *fleetv1.FleetZone
		mutate      func(n *fleetv1.FleetZone)
		admitted    bool
		authzCalled bool
	}{
		{name: "control-plane", ctx: ctxAsUser(controllerUser), old: stoppedZone(true, true), admitted: true},
		{name: "terminating-namespace", ctx: ctxAsUser("fm"), terminating: true, old: stoppedZone(true, true), admitted: true},
		{name: "estop-clear-holder", ctx: ctxAsUser("admin"), allow: true, old: stoppedZone(true, true),
			admitted: true, authzCalled: true},
		{name: "fleet-manager", ctx: ctxAsUser("fm"), old: stoppedZone(true, true), authzCalled: true},
		{name: "fleet-manager-trigger-only", ctx: ctxAsUser("fm"), old: stoppedZone(true, false), authzCalled: true},
		{name: "fleet-manager-before-the-clear-is-applied", ctx: ctxAsUser("fm"), old: stoppedZone(false, true), authzCalled: true},
		{name: "fleet-manager-with-an-actor-change", ctx: ctxAsUser("fm"), old: stoppedZone(true, true), authzCalled: true,
			mutate: func(n *fleetv1.FleetZone) { n.Annotations[AnnEstopActor] = "someone-else" }},
		{name: "fleet-manager-with-a-resume-key", ctx: ctxAsUser("fm"), old: stoppedZone(true, true), authzCalled: true,
			mutate: func(n *fleetv1.FleetZone) { n.Annotations[fleetv1.EstopResumeAnnotation] = "x" }},
		{name: "unattributable", ctx: context.Background(), old: stoppedZone(true, true)},
		{name: "finalizer-kept", ctx: ctxAsUser("fm"), old: stoppedZone(true, true), admitted: true,
			mutate: func(n *fleetv1.FleetZone) { n.Finalizers = []string{estopscope.StopFinalizer} }},
		{name: "no-stop", ctx: ctxAsUser("fm"), old: stoppedZone(false, false), admitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authz := &fakeEstopAuthz{allow: tc.allow}
			v := &FleetZoneValidator{EstopAuthz: authz, APIReader: zoneReader(t, tc.terminating)}
			n := dropFinalizer(tc.old)
			if tc.mutate != nil {
				tc.mutate(n)
			}
			_, err := v.ValidateUpdate(tc.ctx, tc.old, n)
			if tc.admitted && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.admitted && !apierrors.IsForbidden(err) {
				t.Fatalf("err = %v, want Forbidden", err)
			}
			if tc.authzCalled != (authz.calls > 0) || (authz.calls > 0 && authz.gotVerb != estopClearVerb) {
				t.Errorf("estop authorization calls = %d (verb %q), want called=%v for %q",
					authz.calls, authz.gotVerb, tc.authzCalled, estopClearVerb)
			}
		})
	}
}

// The control plane's write of metadata alone is not refused by a structural rule the zone
// fails since an ancestor tightened; any other write, and the control plane's change to spec,
// still is.
func TestZoneControlPlaneMetadataWriteOnAnInconsistentZone(t *testing.T) {
	parent := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "p"},
		Spec: fleetv1.FleetZoneSpec{Restrictions: &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: "0.5"}}}}}
	child := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "c",
		Annotations: map[string]string{estopTriggeredAnnotation: "c-stop"}},
		Spec: fleetv1.FleetZoneSpec{ParentZone: "p", Restrictions: &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: "1.0"}}}}}
	v := &FleetZoneValidator{EstopAuthz: &fakeEstopAuthz{}, APIReader: zoneReader(t, false, parent, child)}

	held := child.DeepCopy()
	held.Finalizers = []string{estopscope.StopFinalizer}
	if _, err := v.ValidateUpdate(ctxAsUser(controllerUser), child, held); err != nil {
		t.Errorf("the control plane's hold of the stop finalizer was refused: %v", err)
	}
	labelled := child.DeepCopy()
	labelled.Labels = map[string]string{"team": "ops"}
	if _, err := v.ValidateUpdate(ctxAsUser("fm"), child, labelled); err == nil {
		t.Errorf("a fleet-manager's write to a zone that fails FZ-R7 was admitted")
	}
	specChange := held.DeepCopy()
	specChange.Spec.MaxConcurrentRobots = 7
	if _, err := v.ValidateUpdate(ctxAsUser(controllerUser), child, specChange); err == nil {
		t.Errorf("the control plane's change to spec skipped the structural checks")
	}
}

// The namespace's SwarmadaConfig carries the namespace stop, and holds its stop finalizer under
// the same rule.
func TestConfigStopFinalizer_RemovedOnlyByTheControlPlaneOrAnEstopClearHolder(t *testing.T) {
	stopped := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "swarmada-config",
		Annotations: map[string]string{estopTriggeredAnnotation: "ns-stop", estopProcessedAnnotation: "ns-stop"},
		Finalizers:  []string{estopscope.StopFinalizer}}}
	dropped := stopped.DeepCopy()
	dropped.Finalizers = nil
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		allow    bool
		admitted bool
	}{
		{"control-plane", ctxAsUser(controllerUser), false, true},
		{"estop-clear-holder", ctxAsUser("admin"), true, true},
		{"without-estop-clear", ctxAsUser("ns-editor"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: tc.allow}}
			_, err := v.ValidateUpdate(tc.ctx, stopped, dropped)
			if tc.admitted && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.admitted && !apierrors.IsForbidden(err) {
				t.Fatalf("err = %v, want Forbidden", err)
			}
		})
	}
}
