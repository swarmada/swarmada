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
// A ZONE THAT ANYTHING STILL REFERENCES CANNOT BE DELETED, EXCEPT BY NAMESPACE TEARDOWN.
//
// These send the delete over the wire. The API server calls the shipped vfleetzone
// registration from config/webhook/manifests.yaml, served by the validator wired as
// cmd/manager/main.go wires it: the manager's cached client, plus the uncached API reader
// the delete check reads through. Only vfleetzone is installed, so Robots and FleetActions
// are created here without their own admission hooks. Those hooks are not what is under test.
//
// envtest runs no kube-controller-manager, so no namespace controller. The teardown test
// plays its part: it deletes the namespace, deletes the namespace's objects with
// deletecollection as the namespace controller does, and then finalizes the namespace. The
// test proves the admission decision a terminating namespace gets. It does not prove how a
// particular kube-controller-manager orders its deletes; the exemption does not depend on
// that order.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

type zoneDeleteWire struct {
	cfg   *rest.Config
	admin client.Client
}

// shippedFleetZoneHook returns the vfleetzone registration exactly as the manifest declares
// it, operations included, so the test depends on the shipped registration firing on DELETE.
func shippedFleetZoneHook(t *testing.T) *admissionregistrationv1.ValidatingWebhookConfiguration {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "webhook", "manifests.yaml"))
	if err != nil {
		t.Fatalf("reading the webhook manifest: %v", err)
	}
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := docs.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("splitting the webhook manifest: %v", err)
		}
		var meta metav1.TypeMeta
		if err := utilyaml.Unmarshal(doc, &meta); err != nil || meta.Kind != "ValidatingWebhookConfiguration" {
			continue
		}
		var cfg admissionregistrationv1.ValidatingWebhookConfiguration
		if err := utilyaml.Unmarshal(doc, &cfg); err != nil {
			t.Fatalf("parsing the ValidatingWebhookConfiguration: %v", err)
		}
		for _, h := range cfg.Webhooks {
			if h.Name == "vfleetzone.swarmada.io" {
				cfg.Webhooks = []admissionregistrationv1.ValidatingWebhook{h}
				return &cfg
			}
		}
	}
	t.Fatal("config/webhook/manifests.yaml registers no vfleetzone.swarmada.io webhook")
	return nil
}

func startZoneDeleteWire(t *testing.T) *zoneDeleteWire {
	t.Helper()
	requireWebhookEnvtest(t)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme setup: %v", err)
		}
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			ValidatingWebhooks: []*admissionregistrationv1.ValidatingWebhookConfiguration{shippedFleetZoneHook(t)},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("the control-plane binaries are installed but the API server did not start "+
			"(or the generated CRDs/webhook manifests were rejected): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	opts := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: crwebhook.NewServer(crwebhook.Options{
			Host:    opts.LocalServingHost,
			Port:    opts.LocalServingPort,
			CertDir: opts.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatalf("building the manager: %v", err)
	}
	if err := (&FleetZoneValidator{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).
		SetupWebhookWithManager(mgr); err != nil {
		t.Fatalf("registering the FleetZone webhook: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the manager did not stop")
		}
	})
	addr := net.JoinHostPort(opts.LocalServingHost, fmt.Sprint(opts.LocalServingPort))
	if err := waitForTLS(addr, 20*time.Second); err != nil {
		t.Fatalf("the webhook server never accepted a connection on %s: %v", addr, err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return &zoneDeleteWire{cfg: cfg, admin: admin}
}

func (w *zoneDeleteWire) namespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "zone-delete-"}}
	if err := w.admin.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating a namespace: %v", err)
	}
	return ns.Name
}

func (w *zoneDeleteWire) zone(t *testing.T, ns, name, parent string) *fleetv1.FleetZone {
	t.Helper()
	z := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent},
	}
	if err := w.admin.Create(context.Background(), z); err != nil {
		t.Fatalf("creating zone %s: %v", name, err)
	}
	return z
}

// robot creates a Robot declaring `declared` and, when current is set, writes it to
// status.currentZone as the Zone Controller would.
func (w *zoneDeleteWire) robot(t *testing.T, ns, name, declared, current string) *fleetv1.Robot {
	t.Helper()
	ctx := context.Background()
	r := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "acme-robotics",
			Model:        "acme-picker",
			Zone:         declared,
			Adapter:      fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
		},
	}
	if err := w.admin.Create(ctx, r); err != nil {
		t.Fatalf("creating robot %s: %v", name, err)
	}
	if current != "" {
		base := r.DeepCopy()
		r.Status.CurrentZone = current
		if err := w.admin.Status().Patch(ctx, r, client.MergeFrom(base)); err != nil {
			t.Fatalf("writing robot %s status.currentZone: %v", name, err)
		}
	}
	return r
}

func (w *zoneDeleteWire) action(t *testing.T, ns, name, zone string) *fleetv1.FleetAction {
	t.Helper()
	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone},
	}
	if err := w.admin.Create(context.Background(), a); err != nil {
		t.Fatalf("creating action %s: %v", name, err)
	}
	return a
}

func (w *zoneDeleteWire) exists(t *testing.T, obj client.Object) bool {
	t.Helper()
	err := w.admin.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("reading %s: %v", obj.GetName(), err)
	}
	return true
}

// refused asserts the delete of z was refused as Invalid with every string in want, and
// that z is still there.
func (w *zoneDeleteWire) refused(t *testing.T, z *fleetv1.FleetZone, why string, want ...string) {
	t.Helper()
	err := w.admin.Delete(context.Background(), z)
	if err == nil {
		t.Fatalf("zone %s was DELETED over the wire while %s", z.Name, why)
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("want Invalid, got %T: %v", err, err)
	}
	for _, s := range want {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the refusal does not say %q; got: %v", s, err)
		}
	}
	if !w.exists(t, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: z.Namespace, Name: z.Name}}) {
		t.Fatalf("the delete of zone %s was refused, but the zone is gone", z.Name)
	}
}

func TestEnvtest_FleetZoneDeleteGuardOverTheWire(t *testing.T) {
	w := startZoneDeleteWire(t)
	ctx := context.Background()

	t.Run("Unreferenced", func(t *testing.T) {
		ns := w.namespace(t)
		dock := w.zone(t, ns, "dock", "")
		w.zone(t, ns, "aisle", "")
		w.robot(t, ns, "amr-1", "aisle", "aisle")
		w.action(t, ns, "fetch-1", "aisle")
		if err := w.admin.Delete(ctx, dock); err != nil {
			t.Fatalf("zone dock has no references and must be deletable: %v", err)
		}
	})

	t.Run("Child", func(t *testing.T) {
		ns := w.namespace(t)
		dock := w.zone(t, ns, "dock", "")
		bay := w.zone(t, ns, "dock-bay-1", "dock")
		w.refused(t, dock, "child zone dock-bay-1 names it as parentZone",
			`FleetZone "dock" has child zones [dock-bay-1] (spec.parentZone)`)
		if err := w.admin.Delete(ctx, bay); err != nil {
			t.Fatalf("deleting the leaf child: %v", err)
		}
		if err := w.admin.Delete(ctx, dock); err != nil {
			t.Fatalf("zone dock has no children left and must be deletable: %v", err)
		}
	})

	t.Run("Declared", func(t *testing.T) {
		ns := w.namespace(t)
		dock := w.zone(t, ns, "dock", "")
		r := w.robot(t, ns, "amr-1", "dock", "")
		w.refused(t, dock, "robot amr-1 declares it in spec.zone",
			`FleetZone "dock" is declared by robots [amr-1] (spec.zone)`)
		if err := w.admin.Delete(ctx, r); err != nil {
			t.Fatalf("deleting the robot: %v", err)
		}
		if err := w.admin.Delete(ctx, dock); err != nil {
			t.Fatalf("zone dock has no robots left and must be deletable: %v", err)
		}
	})

	// The robot is declared in aisle and placed in dock. Only the present check sees it:
	// this subtest fails if that check is removed.
	t.Run("PresentWhileDeclaredElsewhere", func(t *testing.T) {
		ns := w.namespace(t)
		dock := w.zone(t, ns, "dock", "")
		w.zone(t, ns, "aisle", "")
		w.robot(t, ns, "amr-2", "aisle", "dock")
		w.refused(t, dock, "robot amr-2 is placed in it (status.currentZone: dock, spec.zone: aisle)",
			`FleetZone "dock" has robots present in it [amr-2] (status.currentZone)`)
	})

	t.Run("Targeted", func(t *testing.T) {
		ns := w.namespace(t)
		dock := w.zone(t, ns, "dock", "")
		a := w.action(t, ns, "fetch-1", "dock")
		w.refused(t, dock, "action fetch-1, not yet run, targets it",
			`FleetZone "dock" is targeted by FleetActions that can still run [fetch-1] (spec.zone)`)
		// A finished action no longer holds the zone.
		base := a.DeepCopy()
		a.Status.Phase = fleetv1.ActionPhaseSucceeded
		if err := w.admin.Status().Patch(ctx, a, client.MergeFrom(base)); err != nil {
			t.Fatalf("writing action fetch-1 phase Succeeded: %v", err)
		}
		if err := w.admin.Delete(ctx, dock); err != nil {
			t.Fatalf("zone dock is targeted only by a Succeeded action and must be deletable: %v", err)
		}
	})

	// Namespace teardown is exempt. The zone is referenced four ways; deleting the namespace
	// must still complete.
	t.Run("NamespaceTeardownCompletes", func(t *testing.T) {
		ns := w.namespace(t)
		dock := w.zone(t, ns, "dock", "")
		w.zone(t, ns, "dock-bay-1", "dock")
		w.zone(t, ns, "aisle", "")
		robot := w.robot(t, ns, "amr-1", "dock-bay-1", "dock")
		w.robot(t, ns, "amr-2", "dock", "")
		w.action(t, ns, "fetch-1", "dock")

		// Control: while the namespace is staying, both a single delete and a
		// deletecollection, the call the namespace controller makes, are refused.
		w.refused(t, dock, "four references hold it and the namespace is staying")
		if err := w.admin.DeleteAllOf(ctx, &fleetv1.FleetZone{}, client.InNamespace(ns)); err == nil {
			t.Fatal("a deletecollection of the namespace's zones was admitted while the namespace is " +
				"staying; the delete check does not see deletecollection")
		}
		if !w.exists(t, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "dock"}}) {
			t.Fatal("the refused deletecollection removed zone dock anyway")
		}

		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := w.admin.Delete(ctx, nsObj); err != nil {
			t.Fatalf("deleting the namespace: %v", err)
		}
		if !w.exists(t, nsObj) || nsObj.DeletionTimestamp == nil {
			t.Fatalf("the namespace should be Terminating with a deletionTimestamp; got %+v", nsObj.Status)
		}

		// The namespace controller's part, zones first, while every reference is still there.
		if err := w.admin.DeleteAllOf(ctx, &fleetv1.FleetZone{}, client.InNamespace(ns)); err != nil {
			t.Fatalf("namespace teardown was REFUSED at the zones, which would hold namespace %s in "+
				"Terminating: %v", ns, err)
		}
		var left fleetv1.FleetZoneList
		if err := w.admin.List(ctx, &left, client.InNamespace(ns)); err != nil {
			t.Fatalf("listing zones: %v", err)
		}
		if len(left.Items) != 0 {
			t.Fatalf("teardown left %d zone(s) behind", len(left.Items))
		}
		if !w.exists(t, robot) {
			t.Fatal("robot amr-1 was gone before the zones were deleted, so the exemption was not exercised")
		}
		if err := w.admin.DeleteAllOf(ctx, &fleetv1.Robot{}, client.InNamespace(ns)); err != nil {
			t.Fatalf("deleting the namespace's robots: %v", err)
		}
		if err := w.admin.DeleteAllOf(ctx, &fleetv1.FleetAction{}, client.InNamespace(ns)); err != nil {
			t.Fatalf("deleting the namespace's actions: %v", err)
		}
		cs, err := kubernetes.NewForConfig(w.cfg)
		if err != nil {
			t.Fatalf("building a clientset: %v", err)
		}
		nsObj.Spec.Finalizers = nil
		if _, err := cs.CoreV1().Namespaces().Finalize(ctx, nsObj, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("finalizing the namespace: %v", err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for w.exists(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) {
			if time.Now().After(deadline) {
				t.Fatalf("namespace %s was not removed after teardown", ns)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}
