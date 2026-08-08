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

package streamauth

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
)

const ns = "warehouse-a"

func verified(adapter string) controlstream.TLSIdentity {
	return controlstream.TLSIdentity{AdapterName: adapter, Namespace: ns, Verified: true}
}

func robot(name, adapter, class string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.RobotSpec{
			RobotClass: class,
			Adapter:    fleetv1.AdapterRef{Name: adapter, Version: "1.0.0"},
		},
	}
}

func adapter(name string, classes ...string) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetAdapterSpec{ServesRobotClasses: classes},
	}
}

func newAuthorizer(t *testing.T, objs ...client.Object) *Authorizer {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Authorizer{Client: c}
}

// The happy path: a robot bound to this adapter, of a served class, is authorized.
func TestAuthorizeRobot_Allows(t *testing.T) {
	a := newAuthorizer(t, robot("amr-1", "acme", "acme-origin"), adapter("acme", "acme-origin"))
	if err := a.AuthorizeRobot(context.Background(), verified("acme"), "amr-1"); err != nil {
		t.Fatalf("authorized robot denied: %v", err)
	}
}

// Every failure mode denies (fail closed).
func TestAuthorizeRobot_Denies(t *testing.T) {
	objs := []client.Object{
		robot("amr-1", "acme", "acme-origin"),
		robot("amr-other", "otheradapter", "acme-origin"),
		adapter("acme", "acme-origin"),
	}
	cases := []struct {
		name    string
		id      controlstream.TLSIdentity
		robotID string
	}{
		{"forged robot_id (unknown robot)", verified("acme"), "does-not-exist"},
		{"robot bound to another adapter", verified("acme"), "amr-other"},
		{"unverified mTLS identity", controlstream.TLSIdentity{AdapterName: "acme", Namespace: ns}, "amr-1"},
		{"empty robot_id", verified("acme"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuthorizer(t, objs...)
			if err := a.AuthorizeRobot(context.Background(), tc.id, tc.robotID); err == nil {
				t.Fatal("expected denial (fail closed), got nil")
			}
		})
	}
}

// A robot whose class the adapter does not serve is denied.
func TestAuthorizeRobot_UnservedClassDenied(t *testing.T) {
	a := newAuthorizer(t, robot("amr-1", "acme", "some-other-class"), adapter("acme", "acme-origin"))
	if err := a.AuthorizeRobot(context.Background(), verified("acme"), "amr-1"); err == nil {
		t.Fatal("robot of an unserved class must be denied")
	}
}

// A cross-namespace adapter (its identity namespace differs) cannot reach the
// robot: the lookup is scoped to the adapter's own namespace and finds nothing.
func TestAuthorizeRobot_CrossNamespaceDenied(t *testing.T) {
	a := newAuthorizer(t, robot("amr-1", "acme", "acme-origin"), adapter("acme", "acme-origin"))
	elsewhere := controlstream.TLSIdentity{AdapterName: "acme", Namespace: "warehouse-b", Verified: true}
	if err := a.AuthorizeRobot(context.Background(), elsewhere, "amr-1"); err == nil {
		t.Fatal("a robot in another namespace must be unreachable")
	}
}

// Announce: a new robot_id is permitted; one already bound to another adapter is not.
func TestAuthorizeAnnounce(t *testing.T) {
	a := newAuthorizer(t, robot("taken", "otheradapter", "acme-origin"))
	if err := a.AuthorizeAnnounce(context.Background(), verified("acme"), "brand-new"); err != nil {
		t.Errorf("first announce of a new robot should be allowed: %v", err)
	}
	if err := a.AuthorizeAnnounce(context.Background(), verified("acme"), "taken"); err == nil {
		t.Error("announcing a robot bound to another adapter must be denied")
	}
	if err := a.AuthorizeAnnounce(context.Background(), controlstream.TLSIdentity{}, "brand-new"); err == nil {
		t.Error("unverified identity must be denied")
	}
}
