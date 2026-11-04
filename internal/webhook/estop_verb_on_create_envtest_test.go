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

// ─────────────────────────────────────────────────────────────────────────────
// AN ESTOP DELIVERED BY CREATING ITS CARRIER.
//
// estop_rbac_sar_envtest_test.go asks who may TRIGGER an estop, and asks it the way
// swarmctl does: a merge patch that adds swarmada.io/estop-triggered to a carrier that
// already exists. That is the update path, and every carrier's validator authorizes the
// estop-trigger verb there.
//
// The annotation can also arrive at birth. A Robot, FleetZone or SwarmadaConfig created
// with swarmada.io/estop-triggered already set is an estop in the same sense: the
// carrier's reconciler has no create predicate, so its first reconcile finds a trigger
// and no processed marker, and fans out. The second test below runs that rather than
// reading it.
//
// So a principal that holds `create` on the kind and not the estop verb could deliver an
// estop without the SubjectAccessReview the update path asks for. For the SwarmadaConfig
// singleton that estop is namespace-wide. No built-in role holds `create` on a carrier
// without estop-trigger on it, so the principals here are bound to two purpose-built
// ClusterRoles: one with create and no estop verb, one with both. They are impersonated
// WITHOUT system:masters, so the SubjectAccessReview answers from RBAC, exactly as it
// does in production.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
)

const (
	// creatorNoVerb may create every estop carrier and holds no estop verb.
	creatorNoVerb = "carrier-creator"
	// creatorWithVerb may create every estop carrier and holds estop-trigger on each.
	creatorWithVerb = "carrier-creator-with-estop"
)

// bindCarrierCreators installs the two ClusterRoles and binds each to its user
// cluster-wide, so the SwarmadaConfig cases can use a fresh namespace per case.
func bindCarrierCreators(t *testing.T, w *safetyWire) {
	t.Helper()
	ctx := context.Background()
	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(w.cfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}
	carriers := []string{robotGR.Resource, fleetZoneGR.Resource, swarmadaConfigGR.Resource}
	for user, extra := range map[string][]string{
		creatorNoVerb:   nil,
		creatorWithVerb: {estopTriggerVerb},
	} {
		role := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "probe-" + user},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{fleetv1.GroupVersion.Group},
				Resources: carriers,
				Verbs:     append([]string{"get", "list", "watch", "create"}, extra...),
			}},
		}
		if err := rc.Create(ctx, role); err != nil {
			t.Fatalf("installing %s: %v", role.Name, err)
		}
		if err := rc.Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role.Name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
		}); err != nil {
			t.Fatalf("binding %s: %v", role.Name, err)
		}
	}
}

// asRBACUser returns a client that reaches the API server as username with no groups,
// so RBAC, not system:masters, decides both the write and the webhook's SAR.
func asRBACUser(t *testing.T, w *safetyWire, username string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username}
	c, err := client.New(cfg, client.Options{Scheme: w.scheme})
	if err != nil {
		t.Fatalf("client as %s: %v", username, err)
	}
	return c
}

// refusedForTheVerb fails unless err is the estop webhook's verb refusal: Forbidden,
// from the admission webhook, naming the verb. The update path's refusal has this shape.
func refusedForTheVerb(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: a create carrying %s by a principal without %q was ADMITTED",
			what, estopTriggeredAnnotation, estopTriggerVerb)
	}
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "admission webhook") {
		t.Fatalf("%s: refused, but not by the estop webhook: %v", what, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("is not permitted to %q", estopTriggerVerb)) {
		t.Errorf("%s: refused by the webhook, but the message does not name the verb: %v", what, err)
	}
}

func TestEnvtest_EstopTriggerVerbIsAuthorizedOnCreate(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	bindCarrierCreators(t, w)
	noVerb := asRBACUser(t, w, creatorNoVerb)
	withVerb := asRBACUser(t, w, creatorWithVerb)
	markerFixture(t, w, w.ns)

	triggered := func() map[string]string {
		return map[string]string{estopTriggeredAnnotation: "true"}
	}

	type carrier struct {
		kind string
		// make returns a schema-valid carrier named for the case, and the namespace it is in.
		make func(name string, ann map[string]string) client.Object
	}
	carriers := []carrier{
		{"Robot", func(name string, ann map[string]string) client.Object {
			return markerRobot(w.ns, name, ann)
		}},
		{"FleetZone", func(name string, ann map[string]string) client.Object {
			return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns, Annotations: ann}}
		}},
		// The name is pinned by CEL, so each case takes its own namespace.
		{"SwarmadaConfig", func(_ string, ann map[string]string) client.Object {
			return &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
				Name: "swarmada-config", Namespace: freshNamespace(t, w), Annotations: ann}}
		}},
	}

	for _, c := range carriers {
		t.Run(c.kind, func(t *testing.T) {
			lower := strings.ToLower(c.kind)

			// The control: the same principal may create the carrier. The refusal
			// below is the verb, not the create.
			plain := c.make(lower+"-plain", nil)
			if err := noVerb.Create(ctx, plain); err != nil {
				t.Fatalf("the create-only principal cannot create an untriggered %s, so the "+
					"refusal below would prove nothing: %v", c.kind, err)
			}

			refused := c.make(lower+"-born-triggered", triggered())
			refusedForTheVerb(t, c.kind, noVerb.Create(ctx, refused))
			if err := w.admin.Get(ctx, client.ObjectKeyFromObject(refused), refused.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
				t.Fatalf("the refused %s create landed anyway: %v", c.kind, err)
			}

			admitted := c.make(lower+"-born-triggered-by-holder", triggered())
			if err := withVerb.Create(ctx, admitted); err != nil {
				t.Fatalf("a principal holding %q could not create a %s carrying %s: %v",
					estopTriggerVerb, c.kind, estopTriggeredAnnotation, err)
			}
		})
	}
}

// startEstopControllers runs the three shipped estop reconcilers against w's API
// server as the manager's ServiceAccount, sharing one recording estopper. See
// startZoneEstopController for why the identity matters.
func startEstopControllers(t *testing.T, w *safetyWire) *recordingEstopper {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:masters"}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  w.scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("building the controller manager: %v", err)
	}
	estopper := &recordingEstopper{}
	for name, setup := range map[string]func(ctrl.Manager) error{
		"robot":     (&controller.RobotEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: estopper}).SetupWithManager,
		"zone":      (&controller.ZoneEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: estopper}).SetupWithManager,
		"namespace": (&controller.NamespaceEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: estopper}).SetupWithManager,
	} {
		if err := setup(mgr); err != nil {
			t.Fatalf("registering the %s estop reconciler: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the controller manager did not stop")
		}
	})
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the controller cache never synced")
	}
	return estopper
}

// TestEnvtest_EstopTriggeredAtCreateIsActedOn establishes what an annotation present
// at birth is worth: whether the control plane acts on it, or only stores it. Each
// carrier is created by the cluster admin, who holds every verb, so admission is not
// the question here.
func TestEnvtest_EstopTriggeredAtCreateIsActedOn(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	estopper := startEstopControllers(t, w)
	markerFixture(t, w, w.ns)

	delivered := func(robots ...string) func() (bool, string) {
		return func() (bool, string) {
			got := estopper.delivered()
			for _, want := range robots {
				found := false
				for _, r := range got {
					found = found || r == want
				}
				if !found {
					return false, fmt.Sprintf("estops delivered so far: %v", got)
				}
			}
			return true, ""
		}
	}

	t.Run("RobotBornTriggeredIsStopped", func(t *testing.T) {
		r := markerRobot(w.ns, "born-stopped", map[string]string{estopTriggeredAnnotation: "true"})
		if err := w.admin.Create(ctx, r); err != nil {
			t.Fatalf("creating the robot: %v", err)
		}
		eventually(t, "a per-robot estop delivered for a Robot created triggered", delivered("born-stopped"))
	})

	t.Run("SwarmadaConfigBornTriggeredStopsTheNamespace", func(t *testing.T) {
		ns := freshNamespace(t, w)
		markerFixture(t, w, ns)
		for _, name := range []string{"ns-robot-a", "ns-robot-b"} {
			if err := w.admin.Create(ctx, markerRobot(ns, name, nil)); err != nil {
				t.Fatalf("creating %s: %v", name, err)
			}
		}
		if err := w.admin.Create(ctx, &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
			Name: "swarmada-config", Namespace: ns,
			Annotations: map[string]string{estopTriggeredAnnotation: "true"},
		}}); err != nil {
			t.Fatalf("creating the SwarmadaConfig: %v", err)
		}
		eventually(t, "a namespace-wide fan-out from a SwarmadaConfig created triggered",
			delivered("ns-robot-a", "ns-robot-b"))
	})

	t.Run("FleetZoneBornTriggeredIsProcessed", func(t *testing.T) {
		// A zone must exist before a robot can declare it or a descendant of it, so at
		// its first reconcile a zone created triggered has nothing in scope. What is
		// asserted is that the reconciler ACTED: it recorded the trigger as processed,
		// which is also what makes later robots in the zone go unstopped.
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{
			Name: "born-triggered-zone", Namespace: w.ns,
			Annotations: map[string]string{estopTriggeredAnnotation: "true"},
		}}
		if err := w.admin.Create(ctx, z); err != nil {
			t.Fatalf("creating the zone: %v", err)
		}
		eventually(t, "the zone reconciler processing a trigger present at create", func() (bool, string) {
			cur := &fleetv1.FleetZone{}
			if err := w.admin.Get(ctx, client.ObjectKeyFromObject(z), cur); err != nil {
				return false, err.Error()
			}
			return cur.Annotations[estopProcessedAnnotation] == "true",
				fmt.Sprintf("marker = %q, zone estop status = %q",
					cur.Annotations[estopProcessedAnnotation], cur.Status.EstopStatus)
		})
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(z), cur); err != nil {
			t.Fatal(err)
		}
		t.Logf("zone created triggered: estopStatus=%q, processed marker=%q",
			cur.Status.EstopStatus, cur.Annotations[estopProcessedAnnotation])
	})
}
