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
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
	swarmadawebhook "github.com/swarmada/swarmada/internal/webhook"
)

// The operator's path out of an estop, end to end, with the robots/status estop guard in
// place (RFC-0001 #safety-estop-state-machine): `swarmctl estop trigger` on the zone,
// `swarmctl estop-clear`, a fresh report from the robot, then `swarmctl estop resume`.
//
// The guard refuses every change of status.estopState that does not come from the
// control plane's own identity. The control plane here is what cmd/manager runs: the
// generated webhook manifest, the zone and robot estop reconcilers and the estop
// dispatcher, all under the manager's ServiceAccount. The operator is a different
// user. So this is the proof that the guard stops only the writes it is meant to stop:
// if it refused the dispatcher's own writes, the stop would never be recorded and the
// resume would never answer.

const operatorPathManager = "system:serviceaccount:swarmada-system:swarmada-manager"

func TestEnvtest_EstopOperatorPath_TriggerClearResumeReleasesThroughTheGuard(t *testing.T) {
	p := startEstopPath(t)
	p.addRobot("amr-1")

	p.swarmctl("estop", "trigger", "bay-1", "--reason", "person in the aisle", "--yes")
	// No SafetyStream is registered, so the stop is unconfirmed: Failed is a stop too.
	p.stateIs("amr-1", fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed)

	p.swarmctl("estop-clear", "bay-1", "--reason", "aisle clear", "--yes")
	p.stateIs("amr-1", fleetv1.RobotEstopResuming)
	// The clear moves the robot to Resuming and drops its record of the zone's stop in a second
	// write; the resume is refused while the record stands ("whose clear is being applied").
	p.holdsNoStopRecord("amr-1")

	time.Sleep(5 * time.Millisecond) // the witnessing report must be later than the clear
	p.report("amr-1")
	out := p.swarmctl("estop", "resume", "amr-1", "--reason", "inspected", "--yes", "--wait", "20s")
	if !strings.Contains(out, "robot.swarmada.io/amr-1 released: estopState Normal") {
		t.Fatalf("swarmctl estop resume did not report the release:\n%s", out)
	}
	p.stateIs("amr-1", fleetv1.RobotEstopNormal)
}

// estopPath is the control plane cmd/manager runs — the generated webhook manifest, the
// zone and robot estop reconcilers and the estop dispatcher, all under the manager's
// ServiceAccount — with a zone bay-1 and a conformant adapter, and an operator who
// reaches it through swarmctl.
type estopPath struct {
	t            *testing.T
	ctx          context.Context
	ns           string
	admin        client.Client
	controlPlane client.Client
	frames       *telemetry.Router
	kcPath       string
}

func startEstopPath(t *testing.T) *estopPath {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); assets == "" || err != nil {
		if os.Getenv("SWARMADA_REQUIRE_ENVTEST") == "1" || os.Getenv("CI") == "true" {
			t.Fatalf("envtest is required in this run and no control-plane binaries were found (KUBEBUILDER_ASSETS=%q)", assets)
		}
		t.Skip("envtest control plane not available; run `make setup-envtest`")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	scheme := testScheme(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "webhook", "manifests.yaml")},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting the API server with the generated CRDs and webhooks: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	// ── the control plane, as the manager's ServiceAccount ────────────────────────
	if err := swarmadawebhook.SetControllerPrincipal(operatorPathManager); err != nil {
		t.Fatal(err)
	}
	mgrCfg := rest.CopyConfig(cfg)
	mgrCfg.Impersonate = rest.ImpersonationConfig{UserName: operatorPathManager, Groups: []string{"system:masters"}}
	opts := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(mgrCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: crwebhook.NewServer(crwebhook.Options{
			Host: opts.LocalServingHost, Port: opts.LocalServingPort, CertDir: opts.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatalf("building the manager: %v", err)
	}
	sar := &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()}
	for name, setup := range map[string]func(ctrl.Manager) error{
		"Robot defaulter":       (&swarmadawebhook.RobotDefaulter{Client: mgr.GetClient()}).SetupWebhookWithManager,
		"Robot":                 (&swarmadawebhook.RobotAdmissionGate{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), EstopAuthz: sar}).SetupWebhookWithManager,
		"Robot status estop":    (&swarmadawebhook.RobotStatusEstopGuard{}).SetupWebhookWithManager,
		"FleetZone estop actor": (&swarmadawebhook.FleetZoneEstopActorDefaulter{}).SetupWebhookWithManager,
		"FleetZone":             (&swarmadawebhook.FleetZoneValidator{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), EstopAuthz: sar}).SetupWebhookWithManager,
	} {
		if err := setup(mgr); err != nil {
			t.Fatalf("registering the %s webhook: %v", name, err)
		}
	}
	frames := telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&controller.RobotStatusSink{Client: mgr.GetClient()})
	disp := safety.New(mgr.GetClient(), mgr.GetEventRecorderFor("estop"))
	disp.APIReader = mgr.GetAPIReader()
	disp.DeriveCapabilities = controller.DeriveCapabilities
	disp.Telemetry = frames
	if err := (&controller.ZoneEstopReconciler{Client: mgr.GetClient(), Scheme: scheme, Estopper: disp,
		Recorder: mgr.GetEventRecorderFor("zone-estop")}).SetupWithManager(mgr); err != nil {
		t.Fatalf("zone estop reconciler: %v", err)
	}
	if err := (&controller.RobotEstopReconciler{Client: mgr.GetClient(), Scheme: scheme, Estopper: disp, Resumer: disp,
		Recorder: mgr.GetEventRecorderFor("robotestop"), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		t.Fatalf("robot estop reconciler: %v", err)
	}
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
	if err := waitForWebhookServer(addr, 20*time.Second); err != nil {
		t.Fatalf("the webhook server never came up on %s: %v", addr, err)
	}

	// ── the fleet: a zone and a conformant adapter ────────────────────────────────
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "estop-path-"}}
	if err := admin.Create(ctx, nsObj); err != nil {
		t.Fatal(err)
	}
	ns := nsObj.Name
	if err := admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-1", Namespace: ns}}); err != nil {
		t.Fatalf("zone: %v", err)
	}
	adapter := &fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: ns},
		Spec: fleetv1.FleetAdapterSpec{Vendor: "acme-robotics"}}
	if err := admin.Create(ctx, adapter); err != nil {
		t.Fatalf("adapter: %v", err)
	}
	adapter.Status = fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected,
		Conformance: fleetv1.ConformanceStatePassed, ConformanceContractVersion: contract.Version}
	if err := admin.Status().Update(ctx, adapter); err != nil {
		t.Fatalf("adapter status: %v", err)
	}
	// The rest of the status an operator's robot carries is written as the control plane
	// writes it: status.capabilities is the control plane's to compute, and the
	// robots/status guard refuses it from any other identity.
	controlPlane, err := client.New(mgrCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	// ── the operator, through swarmctl ────────────────────────────────────────────
	operator, err := env.ControlPlane.AddUser(envtest.User{Name: "alice", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := operator.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kcPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kcPath, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	return &estopPath{t: t, ctx: ctx, ns: ns, admin: admin, controlPlane: controlPlane, frames: frames, kcPath: kcPath}
}

// addRobot creates a located robot with a lidar in bay-1, served by the adapter, and
// seeds the status it reports.
func (p *estopPath) addRobot(name string) {
	p.t.Helper()
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.ns,
			Annotations: map[string]string{controller.RobotIDAnnotation: name}},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "acme-robotics", Model: "acme-picker", Zone: "bay-1",
			Adapter:  fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"},
			Hardware: []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}},
			Capabilities: []fleetv1.ClassCapability{{Name: "navigation", Type: fleetv1.CapabilityKindHardwareNative,
				RequiredHardware: []string{"lidar"}}},
		},
	}
	if err := p.admin.Create(p.ctx, robot); err != nil {
		p.t.Fatalf("robot: %v", err)
	}
	p.report(name)
	if err := p.controlPlane.Get(p.ctx, client.ObjectKeyFromObject(robot), robot); err != nil {
		p.t.Fatal(err)
	}
	now := metav1.Now()
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.CurrentZone = "bay-1"
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &now}
	robot.Status.Capabilities = controller.DeriveCapabilities(robot)
	if err := p.controlPlane.Status().Update(p.ctx, robot); err != nil {
		p.t.Fatalf("seeding the robot's non-estop status: %v", err)
	}
}

// report is a fresh frame from the robot: Idle, its lidar healthy.
func (p *estopPath) report(name string) {
	p.t.Helper()
	if err := p.frames.Ingest(p.ctx, telemetry.Frame{RobotID: name, Namespace: p.ns, Timestamp: time.Now(),
		Phase: fleetv1.RobotPhaseIdle, Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareHealthy}}); err != nil {
		p.t.Fatalf("robot report: %v", err)
	}
}

// run runs swarmctl as the operator and returns what it printed and its error.
func (p *estopPath) run(args ...string) (string, error) {
	p.t.Helper()
	var out bytes.Buffer
	root, _ := newRootCommand(cli.IOStreams{In: strings.NewReader(""), Out: &out, Err: &out})
	root.SetArgs(append([]string{"--kubeconfig", p.kcPath, "-n", p.ns}, args...))
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.ExecuteContext(p.ctx)
	return out.String(), err
}

// swarmctl runs swarmctl as the operator and fails the test if it fails.
func (p *estopPath) swarmctl(args ...string) string {
	p.t.Helper()
	out, err := p.run(args...)
	if err != nil {
		p.t.Fatalf("swarmctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// stateIs waits for the robot's estopState to reach one of want.
func (p *estopPath) stateIs(name string, want ...fleetv1.RobotEstopState) fleetv1.RobotEstopState {
	p.t.Helper()
	var got fleetv1.RobotEstopState
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var r fleetv1.Robot
		if err := p.admin.Get(p.ctx, client.ObjectKey{Namespace: p.ns, Name: name}, &r); err == nil {
			got = r.Status.EstopState
			for _, w := range want {
				if got == w {
					return got
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("%s estopState = %q, want one of %v", name, got, want)
	return ""
}

// holdsNoStopRecord waits until the robot's status.estopHeldBy is empty: the clear has been
// applied to it.
func (p *estopPath) holdsNoStopRecord(name string) {
	p.t.Helper()
	var held []fleetv1.EstopHold
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var r fleetv1.Robot
		if err := p.admin.Get(p.ctx, client.ObjectKey{Namespace: p.ns, Name: name}, &r); err == nil {
			if held = r.Status.EstopHeldBy; len(held) == 0 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("%s still records %v: the clear was not applied to it", name, held)
}

// waitForWebhookServer waits until the webhook server accepts a TLS connection.
func waitForWebhookServer(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		var conn *tls.Conn
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr,
			&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // a local test server's self-signed cert
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}
