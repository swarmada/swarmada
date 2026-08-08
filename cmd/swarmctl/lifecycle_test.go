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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/admission"
	"github.com/swarmada/swarmada/internal/cli"
)

// authorizer returns a fake clientset whose SelfSubjectAccessReview always
// returns the given allow verdict — standing in for the API server's RBAC
// decision on a custom verb.
func authorizer(allowed bool) *k8sfake.Clientset {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SelfSubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	return cs
}

func discoveredRobot(name, ns string) *fleetv1.DiscoveredRobot {
	return &fleetv1.DiscoveredRobot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: fleetv1.DiscoveredRobotStatus{
			Manufacturer:   "Acme",
			Model:          "Origin",
			AdapterVersion: "1.0",
			ReportedHardware: []fleetv1.DiscoveredHardwareComponent{
				{Name: "cam", Type: fleetv1.HardwareTypeCamera, Status: fleetv1.HardwareHealthy},
			},
		},
	}
}

func robotClass(name, ns string) *fleetv1.RobotClass {
	return &fleetv1.RobotClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.RobotClassSpec{
			Manufacturer: "Acme", Model: "Origin",
			BaseAdapter: fleetv1.BaseAdapterRef{Name: "acme-adapter", Version: "2.0"},
			Hardware:    []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}},
		},
	}
}

// Admit MARKS the DiscoveredRobot with the operator's parameters; the controller creates
// the Robot and removes the staging object. The CLI no longer creates anything, so admitting
// no longer requires blanket `create` on robots — a permission that would let an operator
// bypass the admission gate (§6.6) altogether.
func TestAdmitAllowedMarksWithTheOperatorsParameters(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(discoveredRobot("dr-acme-a3f9", ns), robotClass("acme-class", ns)).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	a := &admitOptions{zone: "zone-aisle-c1", robotClass: "acme-class", name: "amr-acme-042"}
	if err := o.admit(context.Background(), c, authorizer(true), ns, "dr-acme-a3f9", a); err != nil {
		t.Fatalf("admit: %v", err)
	}

	var marked fleetv1.DiscoveredRobot
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dr-acme-a3f9"}, &marked); err != nil {
		t.Fatalf("the DiscoveredRobot must survive until the controller promotes it: %v", err)
	}
	// Nothing is schedulable yet — the CLI does not create the Robot.
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-acme-042"}, &fleetv1.Robot{}); !apierrors.IsNotFound(err) {
		t.Errorf("the CLI must not create the Robot itself, got err=%v", err)
	}

	// Every operator override rides in the mark. A field dropped here is silently lost:
	// the controller has no other source for it, and the robot would come up misconfigured
	// rather than failing.
	p, err := admission.DecodeParams(marked.Annotations[admission.AdmitAnnotation])
	if err != nil {
		t.Fatalf("the mark must decode: %v", err)
	}
	if p.Zone != "zone-aisle-c1" || p.RobotClass != "acme-class" || p.Name != "amr-acme-042" {
		t.Errorf("admission parameters not carried through: %+v", p)
	}
}

// The parameters are validated against the real builder before being written down, so an
// unusable admission fails at the command the operator is watching rather than later, in a
// controller event they have no reason to look for.
func TestAdmitRefusesParametersThatCannotBuildARobot(t *testing.T) {
	ns := "warehouse-a"
	dr := discoveredRobot("dr-1", ns)
	dr.Status.AdapterVersion = "" // no class, no --adapter: nothing names an adapter
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dr).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	err := o.admit(context.Background(), c, authorizer(true), ns, "dr-1", &admitOptions{zone: "z"})
	if err == nil || !strings.Contains(err.Error(), "no adapter") {
		t.Fatalf("expected an adapter-resolution error, got %v", err)
	}
	var after fleetv1.DiscoveredRobot
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dr-1"}, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	// A refused admission leaves no mark; otherwise the controller would keep retrying a
	// decision the CLI already told the operator had failed.
	if _, marked := after.Annotations[admission.AdmitAnnotation]; marked {
		t.Error("a refused admission must not mark the object")
	}
}

func TestAdmitDeniedFailsClosed(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(discoveredRobot("dr-1", ns)).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	err := o.admit(context.Background(), c, authorizer(false), ns, "dr-1", &admitOptions{zone: "z", adapter: "a"})
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected authorization denial, got %v", err)
	}
	// Nothing was created and the DiscoveredRobot survives.
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dr-1"}, &fleetv1.DiscoveredRobot{}); err != nil {
		t.Errorf("DiscoveredRobot should be untouched on denial: %v", err)
	}
	robots := &fleetv1.RobotList{}
	_ = c.List(context.Background(), robots, client.InNamespace(ns))
	if len(robots.Items) != 0 {
		t.Errorf("no robot should be created on denial, got %d", len(robots.Items))
	}
}

func TestAdmitRequiresAdapterWithoutClass(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(discoveredRobot("dr-1", ns)).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)
	err := o.admit(context.Background(), c, authorizer(true), ns, "dr-1", &admitOptions{zone: "z"})
	if err == nil || !strings.Contains(err.Error(), "no adapter") {
		t.Fatalf("expected missing-adapter error, got %v", err)
	}
}

// Reject MARKS the DiscoveredRobot and leaves the deletion to the control plane, so the
// manager can seal ROBOT_REJECTED (§9.6.5.1) before the object disappears. Deleting here —
// as this did — made an operator's refusal indistinguishable from a TTL sweep.
func TestRejectMarksForTheControlPlaneAndRecordsEvent(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(discoveredRobot("dr-stale", ns)).Build()
	cs := authorizer(true)
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	if err := o.reject(context.Background(), c, cs, ns, "dr-stale", "stale test entry", true); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var marked fleetv1.DiscoveredRobot
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dr-stale"}, &marked); err != nil {
		t.Fatalf("the DiscoveredRobot must survive until the controller records and removes it: %v", err)
	}
	// The annotation is the discriminating signal a TTL sweep can never produce, and it
	// carries the reason the audit entry will record.
	if got := marked.Annotations[annRobotRejected]; got != "stale test entry" {
		t.Errorf("rejection annotation = %q, want the operator's reason", got)
	}
	// The rejection reason was recorded as an Event.
	evs, _ := cs.CoreV1().Events(ns).List(context.Background(), metav1.ListOptions{})
	if len(evs.Items) != 1 || evs.Items[0].Reason != "Rejected" || !strings.Contains(evs.Items[0].Message, "stale test entry") {
		t.Errorf("expected a Rejected event with the reason, got %+v", evs.Items)
	}
	if !strings.Contains(out.String(), "rejected") {
		t.Errorf("missing confirmation output: %q", out.String())
	}
}

func TestRejectAbortsWithoutConfirmation(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(discoveredRobot("dr-keep", ns)).Build()
	out := &bytes.Buffer{}
	o := &options{streams: cli.IOStreams{In: strings.NewReader("n\n"), Out: out, Err: out}, outputFmt: cli.OutputTable}

	if err := o.reject(context.Background(), c, authorizer(true), ns, "dr-keep", "", false); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dr-keep"}, &fleetv1.DiscoveredRobot{}); err != nil {
		t.Errorf("a declined reject must not delete: %v", err)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("expected abort notice, got %q", out.String())
	}
}

func TestActionCancelSetsAnnotation(t *testing.T) {
	ns := "warehouse-a"
	action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "pick-4471", Namespace: ns}, Spec: fleetv1.FleetActionSpec{Type: "pick"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(action).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.actionCancel(context.Background(), c, authorizer(true), ns, "pick-4471", "aisle blocked", true); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := &fleetv1.FleetAction{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "pick-4471"}, got)
	if got.Annotations[annCancelRequested] != "aisle blocked" {
		t.Errorf("cancel annotation = %q, want the reason", got.Annotations[annCancelRequested])
	}
}

func TestActionCancelDeniedFailsClosed(t *testing.T) {
	ns := "warehouse-a"
	action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(action).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	err := o.actionCancel(context.Background(), c, authorizer(false), ns, "t", "x", true)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected denial, got %v", err)
	}
	got := &fleetv1.FleetAction{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "t"}, got)
	if _, set := got.Annotations[annCancelRequested]; set {
		t.Error("a denied cancel must not write the annotation")
	}
}

func TestEstopTriggerAndClear(t *testing.T) {
	ns := "warehouse-a"
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "zone-b3", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(zone).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.estopTrigger(context.Background(), c, authorizer(true), ns, "zone-b3", "person detected", true); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	got := &fleetv1.FleetZone{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "zone-b3"}, got)
	if got.Annotations[annEstopTriggered] != "person detected" {
		t.Fatalf("estop annotation = %q after trigger", got.Annotations[annEstopTriggered])
	}

	if err := o.estopClear(context.Background(), c, authorizer(true), ns, "zone-b3", "aisle clear, inspected", true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got = &fleetv1.FleetZone{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "zone-b3"}, got)
	if _, set := got.Annotations[annEstopTriggered]; set {
		t.Errorf("estop-triggered annotation should be removed after clear, got %v", got.Annotations)
	}
}

func TestEstopClearDeniedFailsClosed(t *testing.T) {
	ns := "warehouse-a"
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: ns, Annotations: map[string]string{annEstopTriggered: "boom"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(zone).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	// estop-clear is admin-only; a denied SSAR must leave the estop in place.
	err := o.estopClear(context.Background(), c, authorizer(false), ns, "z", "drill over", true)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected denial, got %v", err)
	}
	got := &fleetv1.FleetZone{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "z"}, got)
	if got.Annotations[annEstopTriggered] != "boom" {
		t.Error("a denied estop-clear must not resume the zone")
	}
}
