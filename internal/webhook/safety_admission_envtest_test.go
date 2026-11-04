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
// THREE SAFETY BEHAVIOURS, PROVED AT ADMISSION.
//
// Each of these is already asserted by a unit test that calls the validator by hand:
// estop_marker_guard_wiring_test.go for the refusal of a non-controller write of the
// estop idempotence marker, fleettask_flow_depth_test.go for the refusal of a nested
// loop that omits mode, and rollout_terminal_failure_test.go's direct ValidateDelete
// for the deletability of a terminal rollout.
// computeworkloadtemplate_admission_envtest_test.go explains why that is
// not enough: a wrong path, a GVK registered differently, or a manifest whose rules do
// not match the resource all look exactly like a working gate when the function is
// invoked directly. For the marker there is a further gap: the guard attributes the
// write by admission.Request.UserInfo, and a unit test fabricates that context — only
// the API server says what the webhook really receives.
//
// So this starts a real API server, installs the GENERATED webhook manifest, runs the
// SHIPPED registrations, and sends each request over the wire, impersonating the
// principal whose identity is the point: an ordinary user for the refusals, the
// manager's ServiceAccount for the controller's own write.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// safetyWire is one API server with the safety-relevant webhooks served.
type safetyWire struct {
	cfg    *rest.Config
	scheme *runtime.Scheme
	admin  client.Client
	ns     string
	// stopWebhooks stops the manager that serves the webhooks, so a test can watch the
	// API server call a webhook nobody serves. Safe to call more than once.
	stopWebhooks func()
}

// as returns a client that reaches the API server as username. system:masters keeps
// RBAC out of the way, so any refusal below comes from admission, not authorization.
func (w *safetyWire) as(t *testing.T, username string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: []string{"system:masters"}}
	c, err := client.New(cfg, client.Options{Scheme: w.scheme})
	if err != nil {
		t.Fatalf("client as %s: %v", username, err)
	}
	return c
}

// rigStoreNamespace is the control plane's own namespace in the rig, where the FleetAction
// reconcilers the rows run keep each namespace's generation store, as the manager does in its own.
const rigStoreNamespace = "swarmada-system"

func startSafetyWire(t *testing.T) *safetyWire {
	t.Helper()
	requireWebhookEnvtest(t)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme, authorizationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme setup: %v", err)
		}
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "webhook", "manifests.yaml")},
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
	// As cmd/manager/main.go wires them, for every kind the tests on this wire write.
	// Every hook in the manifest for these kinds is served, so no request fails for a
	// missing handler — the Robot pair included, because mrobot and vrobot are both
	// failurePolicy=Fail and an unserved one would refuse every Robot write here. The
	// Robot delete guard is Ignore: unserved, it would admit every delete, and the delete
	// guard tests would go red rather than pass on a missing handler.
	sar := &SARAuthorizer{Client: mgr.GetClient()}
	for name, setup := range map[string]func(ctrl.Manager) error{
		"Robot defaulter":              (&RobotDefaulter{Client: mgr.GetClient()}).SetupWebhookWithManager,
		"Robot":                        (&RobotAdmissionGate{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), EstopAuthz: sar}).SetupWebhookWithManager,
		"FleetZone estop actor":        (&FleetZoneEstopActorDefaulter{}).SetupWebhookWithManager,
		"SwarmadaConfig estop actor":   (&SwarmadaConfigEstopActorDefaulter{}).SetupWebhookWithManager,
		"FirmwareRollout resume actor": (&FirmwareRolloutResumeActorDefaulter{}).SetupWebhookWithManager,
		"ModelRollout resume actor":    (&ModelRolloutResumeActorDefaulter{}).SetupWebhookWithManager,
		"SwarmadaConfig":               (&SwarmadaConfigValidator{EstopAuthz: sar}).SetupWebhookWithManager,
		"FleetZone":                    (&FleetZoneValidator{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), EstopAuthz: sar}).SetupWebhookWithManager,
		"FleetTask":                    (&FleetTaskValidator{AppendAuthz: sar, CancelAuthz: sar, ActivateAuthz: sar, Reader: mgr.GetClient()}).SetupWebhookWithManager,
		"FleetAction":                  (&FleetActionValidator{CancelAuthz: sar, Reader: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWebhookWithManager,
		"FirmwareRollout":              (&FirmwareRolloutValidator{Client: mgr.GetClient()}).SetupWebhookWithManager,
		"ModelRollout":                 (&ModelRolloutValidator{}).SetupWebhookWithManager,
		"Robot status estop":           (&RobotStatusEstopGuard{}).SetupWebhookWithManager,
		"Robot delete":                 (&RobotDeleteGuard{APIReader: mgr.GetAPIReader()}).SetupWebhookWithManager,
		"FleetAction delete":           (&FleetActionDeleteGuard{APIReader: mgr.GetAPIReader(), EstopAuthz: sar}).SetupWebhookWithManager,
		"SwarmadaConfig delete":        (&SwarmadaConfigDeleteGuard{APIReader: mgr.GetAPIReader()}).SetupWebhookWithManager,
		"ZoneMaintenance":              (&ZoneMaintenanceValidator{Client: mgr.GetClient()}).SetupWebhookWithManager,
	} {
		if err := setup(mgr); err != nil {
			t.Fatalf("registering the %s webhook: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	var once sync.Once
	stopWebhooks := func() {
		once.Do(func() {
			cancel()
			select {
			case <-errCh:
			case <-time.After(20 * time.Second):
				t.Error("the manager did not stop")
			}
		})
	}
	t.Cleanup(stopWebhooks)

	addr := net.JoinHostPort(opts.LocalServingHost, fmt.Sprint(opts.LocalServingPort))
	if err := waitForTLS(addr, 20*time.Second); err != nil {
		t.Fatalf("the webhook server never accepted a connection on %s: %v", addr, err)
	}

	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "safety-"}}
	if err := admin.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating the test namespace: %v", err)
	}
	if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rigStoreNamespace}}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the control plane's namespace: %v", err)
	}
	return &safetyWire{cfg: cfg, scheme: scheme, admin: admin, ns: ns.Name, stopWebhooks: stopWebhooks}
}

func TestEnvtest_SafetyAdmissionOverTheWire(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()

	// ── swarmada.io/estop-processed is the controller's to write ─────────────────
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "dock", Namespace: w.ns}}
	if err := w.admin.Create(ctx, zone); err != nil {
		t.Fatalf("creating the zone: %v", err)
	}
	stampMarker := func(c client.Client, value string) error {
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(zone), cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		if value == "" {
			delete(cur.Annotations, estopProcessedAnnotation)
		} else {
			if cur.Annotations == nil {
				cur.Annotations = map[string]string{}
			}
			cur.Annotations[estopProcessedAnnotation] = value
		}
		return c.Patch(ctx, cur, client.MergeFrom(base))
	}
	markerOnZone := func() (string, bool) {
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(zone), cur); err != nil {
			t.Fatalf("re-reading the zone: %v", err)
		}
		v, ok := cur.Annotations[estopProcessedAnnotation]
		return v, ok
	}

	t.Run("ClientWriteOfEstopProcessedRefused", func(t *testing.T) {
		// The forgery this guards: pre-stamp the value the next trigger will
		// carry ("true" is swarmctl's default), on an update that leaves the trigger
		// annotation alone so no estop verb gate is consulted.
		err := stampMarker(w.as(t, "mallory"), "true")
		if err == nil || !apierrors.IsForbidden(err) {
			t.Fatalf("a non-controller write of %s must be Forbidden over the wire, got: %v",
				estopProcessedAnnotation, err)
		}
		if !strings.Contains(err.Error(), estopProcessedAnnotation) {
			t.Errorf("refused, but the message does not name the marker: %v", err)
		}
		if v, ok := markerOnZone(); ok {
			t.Fatalf("the refused write landed anyway: marker = %q", v)
		}
	})

	t.Run("ControllerWriteOfEstopProcessedAdmitted", func(t *testing.T) {
		// The control: the guard is an identity check, not a wall. The manager's
		// ServiceAccount, as the API server authenticates it, must still stamp it.
		if err := stampMarker(w.as(t, controllerUser), "true"); err != nil {
			t.Fatalf("the controller's own marker write was refused: %v", err)
		}
		if v, _ := markerOnZone(); v != "true" {
			t.Fatalf("marker = %q after the controller's write, want \"true\"", v)
		}
	})

	t.Run("ClientRemovalOfEstopProcessedRefused", func(t *testing.T) {
		// Depends on the subtest above having stamped it.
		err := stampMarker(w.as(t, "mallory"), "")
		if err == nil || !apierrors.IsForbidden(err) {
			t.Fatalf("a non-controller removal of %s must be Forbidden over the wire, got: %v",
				estopProcessedAnnotation, err)
		}
		if _, ok := markerOnZone(); !ok {
			t.Fatal("the refused removal landed anyway")
		}
	})

	// ── a nested loop omitting mode is refused ───────────────────────────────────
	t.Run("NestedLoopWithoutModeRefusedAtAdmission", func(t *testing.T) {
		nested := func(name string, mode fleetv1.LoopMode) *fleetv1.FleetTask {
			task := ftWithFlow(name, &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{
				{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeOnce, Body: fleetv1.FlowNode{
					Loop: &fleetv1.LoopNode{Mode: mode, Body: runNode("patrol")}, // depth 3
				}}},
			}}, ftAct("patrol"))
			task.Namespace = w.ns
			return task
		}

		err := w.admin.Create(ctx, nested("unbounded", ""))
		if err == nil {
			t.Fatal("the API server ADMITTED a FleetTask whose nested loop omits mode; the " +
				"evaluator runs it as Forever, and an unbounded loop is not admitted")
		}
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "mode") {
			t.Errorf("refused, but not as an Invalid naming mode — the refusal may be for an "+
				"unrelated reason: %v", err)
		}
		// The positive control: the identical tree WITH a mode is admitted, so the
		// refusal above can only be the missing mode.
		if err := w.admin.Create(ctx, nested("bounded", fleetv1.LoopModeOnce)); err != nil {
			t.Fatalf("the same tree with mode: Once was refused, so the negative case proves "+
				"nothing: %v", err)
		}
	})

	// ── a terminal rollout can be deleted; a live one cannot ─────────────────────
	t.Run("FailedRolloutIsDeletedOverTheWire", func(t *testing.T) {
		// The model signing gate denies with no namespace SwarmadaConfig; turn it off
		// so creation is not what is under test.
		if err := w.admin.Create(ctx, signingConfig(w.ns, false)); err != nil {
			t.Fatalf("creating the SwarmadaConfig: %v", err)
		}
		ro := &fleetv1.ModelRollout{
			ObjectMeta: metav1.ObjectMeta{Name: "roll", Namespace: w.ns},
			Spec: fleetv1.ModelRolloutSpec{
				TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
				ModelName:      "item-recognition",
				NewVersion:     "3.2.1",
				ModelURI:       "oci://registry/models/item-recognition:3.2.1",
				ModelChecksum:  "sha256:" + strings.Repeat("0", 64),
				Strategy: fleetv1.RolloutStrategy{RollingUpdate: &fleetv1.RollingUpdateStrategy{
					MaxUnavailable: "1", PauseOnError: func(b bool) *bool { return &b }(false),
				}},
			},
		}
		if err := w.admin.Create(ctx, ro); err != nil {
			t.Fatalf("creating the rollout: %v", err)
		}
		setPhase := func(p fleetv1.RolloutPhase) {
			t.Helper()
			cur := &fleetv1.ModelRollout{}
			if err := w.admin.Get(ctx, client.ObjectKeyFromObject(ro), cur); err != nil {
				t.Fatalf("re-reading the rollout: %v", err)
			}
			cur.Status.Phase = p
			if err := w.admin.Status().Update(ctx, cur); err != nil {
				t.Fatalf("writing status.phase=%s: %v", p, err)
			}
		}

		// The control: a live rollout is refused, so the admitted delete below is the
		// phase's doing and not an absent gate.
		setPhase(fleetv1.RolloutPhaseInProgress)
		if err := w.admin.Delete(ctx, ro); err == nil || !apierrors.IsForbidden(err) {
			t.Fatalf("deleting an InProgress rollout must be Forbidden over the wire, got: %v", err)
		}

		setPhase(fleetv1.RolloutPhaseFailed)
		if err := w.admin.Delete(ctx, ro); err != nil {
			t.Fatalf("a Failed rollout must be deletable over the wire, got: %v", err)
		}
		gone := &fleetv1.ModelRollout{}
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(ro), gone); !apierrors.IsNotFound(err) {
			t.Fatalf("the delete was admitted but the record is still there (finalizers %v): %v",
				gone.Finalizers, err)
		}
	})

	// ── the controller records a processed resume whatever the signing policy now says ──
	t.Run("ControllerResumeMarkerOnRolloutAdmittedWhateverTheSigningPolicy", func(t *testing.T) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "rollout-marker-"}}
		if err := w.admin.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
		cfg := signingConfig(ns.Name, false)
		if err := w.admin.Create(ctx, cfg); err != nil {
			t.Fatalf("creating the SwarmadaConfig: %v", err)
		}
		fw := &fleetv1.FirmwareRollout{
			ObjectMeta: metav1.ObjectMeta{Name: "fw", Namespace: ns.Name},
			Spec: fleetv1.FirmwareRolloutSpec{
				TargetSelector:   metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
				NewVersion:       "2.5.0",
				FirmwareURI:      "oci://registry.example/acme:2.5.0",
				FirmwareChecksum: "sha256:" + strings.Repeat("0", 64),
			},
		}
		mr := &fleetv1.ModelRollout{
			ObjectMeta: metav1.ObjectMeta{Name: "mr", Namespace: ns.Name},
			Spec: fleetv1.ModelRolloutSpec{
				TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
				ModelName:      "item-recognition",
				NewVersion:     "3.2.1",
				ModelURI:       "oci://registry/models/item-recognition:3.2.1",
				ModelChecksum:  "sha256:" + strings.Repeat("0", 64),
			},
		}
		for _, o := range []client.Object{fw, mr} {
			if err := w.admin.Create(ctx, o); err != nil {
				t.Fatalf("creating %T while signing is off: %v", o, err)
			}
		}
		mark := func(c client.Client, o client.Object, value string) error {
			if err := w.admin.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				return err
			}
			base := o.DeepCopyObject().(client.Object)
			ann := o.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann["swarmada.io/rollout-resume-processed"] = value
			o.SetAnnotations(ann)
			return c.Patch(ctx, o, client.MergeFrom(base))
		}
		mgr := w.as(t, controllerUser)

		// Signing switched on after the rollouts were admitted without a reference.
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(cfg), cfg); err != nil {
			t.Fatal(err)
		}
		cfg.Spec.Signing.RequireSignatureVerification = boolp(true)
		if err := w.admin.Update(ctx, cfg); err != nil {
			t.Fatalf("switching signing on: %v", err)
		}
		for _, o := range []client.Object{fw, mr} {
			if err := mark(mgr, o, "after signing was switched on"); err != nil {
				t.Errorf("the controller's marker on %T was refused after signing was switched on: %v", o, err)
			}
		}

		// No SwarmadaConfig at all.
		if err := w.admin.Delete(ctx, cfg); err != nil {
			t.Fatalf("deleting the SwarmadaConfig: %v", err)
		}
		for _, o := range []client.Object{fw, mr} {
			if err := mark(mgr, o, "with no SwarmadaConfig"); err != nil {
				t.Errorf("the controller's marker on %T was refused with no SwarmadaConfig: %v", o, err)
			}
		}

		// The control: a spec change is still checked, so the admits above are the write's
		// doing and not an absent gate.
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(fw), fw); err != nil {
			t.Fatal(err)
		}
		fw.Spec.NewVersion = "2.5.1"
		if err := w.admin.Update(ctx, fw); err == nil {
			t.Error("a spec change was admitted with no SwarmadaConfig; the signing check did not run")
		}
	})
}
