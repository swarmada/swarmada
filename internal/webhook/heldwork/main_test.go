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

// Package heldwork_test holds the over-the-wire rows for held work (RFC-0001 safety.md, *Held
// work*): one API server, one manager serving every webhook, a namespace per row and t.Parallel().
//
// It is its own package for time. internal/webhook starts an API server per test function and
// runs for about ten minutes under -race; this binary has its own budget. The webhook wiring is
// therefore copied from cmd/manager/main.go, through exported names only, and a copy can drift:
// TestDrift_EveryManifestWebhookIsServed fails the day a webhook is added to the markers and not
// to registerWebhooks.
//
// Identities here are judged by RBAC. Unlike internal/webhook's safetyWire, no request carries
// system:masters: a row binds the shipped ClusterRoles it needs in its own namespace.
//
// The envtest contract is internal/webhook's (suite_envtest_test.go): with no control-plane
// binaries the rows skip, unless CI=true or SWARMADA_REQUIRE_ENVTEST=1, which fails the run.
// With the binaries present, a rig that does not come up fails the run.
package heldwork_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	swarmadawebhook "github.com/swarmada/swarmada/internal/webhook"
)

// controllerUser is the manager's principal, as internal/webhook's tests name it.
const controllerUser = "system:serviceaccount:swarmada-system:swarmada-manager"

// repoRoot is the repository root from this package's directory.
var repoRoot = filepath.Join("..", "..", "..")

// shippedRoles are the ClusterRoles the rig installs under their shipped names, by file.
var shippedRoles = []string{
	"swarmada_operator_role.yaml",
	"swarmada_fleet_manager_role.yaml",
	"swarmada_admin_role.yaml",
	"role.yaml",
}

// The rig, started once by TestMain.
var (
	rigCfg    *rest.Config
	rigScheme *runtime.Scheme
	rigAdmin  client.Client
	rigMgr    ctrl.Manager
	rigReady  bool

	// rigFatal is non-empty when the rig SHOULD have come up and did not.
	rigFatal string
)

// rigHarnessHeadline prefixes a hard failure with the binaries installed.
const rigHarnessHeadline = "the held-work envtest rig could not come up, and the envtest binaries ARE " +
	"installed — this is a defect in the repository, not a missing tool. Every row in this package " +
	"was skipped; none of their results mean anything."

// rigRequiredHeadline prefixes the hard failure for required mode with the binaries absent.
const rigRequiredHeadline = "envtest is REQUIRED in this run and its control-plane binaries are not " +
	"installed. Every row in this package was skipped, so a green result here would claim rows that " +
	"never ran."

var rigFatalHeadline = rigHarnessHeadline

func TestMain(m *testing.M) {
	stop := startRig()
	code := m.Run()
	stop()
	// Covers a filtered run (-run ...) in which TestHeldWorkRigIsUsable never executes.
	if rigFatal != "" && code == 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %s\n      %s\n", rigFatalHeadline, rigFatal)
		code = 1
	}
	os.Exit(code)
}

// envtestRequired reports whether missing envtest binaries must fail this run rather than skip
// it, and which variable said so. Same rule as internal/webhook's whEnvtestRequired.
func envtestRequired() (bool, string) {
	if os.Getenv("SWARMADA_REQUIRE_ENVTEST") == "1" {
		return true, "SWARMADA_REQUIRE_ENVTEST=1"
	}
	if os.Getenv("CI") == "true" {
		return true, "CI=true"
	}
	return false, ""
}

// controlPlaneBinary reproduces controller-runtime's binary-path precedence, as internal/webhook's
// whControlPlaneBinary does.
func controlPlaneBinary(name string) string {
	sanitized := regexp.MustCompile("[^A-Z0-9]+").ReplaceAllString(strings.ToUpper(name), "_")
	sanitized = regexp.MustCompile("^[0-9]+").ReplaceAllString(sanitized, "")
	if v, ok := os.LookupEnv("TEST_ASSET_" + sanitized); ok {
		return v
	}
	if v, ok := os.LookupEnv("KUBEBUILDER_ASSETS"); ok {
		return filepath.Join(v, name)
	}
	return filepath.Join("/usr/local/kubebuilder/bin", name)
}

// missingControlPlaneBinaries returns the binaries envtest needs that are not present and
// executable. Empty means the tool is installed, so a later failure is ours.
func missingControlPlaneBinaries() []string {
	var missing []string
	for _, name := range []string{"kube-apiserver", "etcd"} {
		p := controlPlaneBinary(name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			missing = append(missing, fmt.Sprintf("%s (looked for %s)", name, p))
		}
	}
	return missing
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme,
		authorizationv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return scheme, nil
}

// registerWebhooks registers every webhook cmd/manager/main.go registers, as it wires them.
func registerWebhooks(mgr ctrl.Manager) error {
	sar := func() *swarmadawebhook.SARAuthorizer { return &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()} }
	for _, w := range []struct {
		name  string
		setup func(ctrl.Manager) error
	}{
		{"Robot defaulter", (&swarmadawebhook.RobotDefaulter{Client: mgr.GetClient()}).SetupWebhookWithManager},
		{"FleetZone estop actor", (&swarmadawebhook.FleetZoneEstopActorDefaulter{}).SetupWebhookWithManager},
		{"SwarmadaConfig estop actor", (&swarmadawebhook.SwarmadaConfigEstopActorDefaulter{}).SetupWebhookWithManager},
		{"FirmwareRollout resume actor", (&swarmadawebhook.FirmwareRolloutResumeActorDefaulter{}).SetupWebhookWithManager},
		{"ModelRollout resume actor", (&swarmadawebhook.ModelRolloutResumeActorDefaulter{}).SetupWebhookWithManager},
		{"Robot", (&swarmadawebhook.RobotAdmissionGate{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), EstopAuthz: sar()}).SetupWebhookWithManager},
		{"Robot delete", (&swarmadawebhook.RobotDeleteGuard{APIReader: mgr.GetAPIReader()}).SetupWebhookWithManager},
		{"FleetAction delete", (&swarmadawebhook.FleetActionDeleteGuard{APIReader: mgr.GetAPIReader(), EstopAuthz: sar()}).SetupWebhookWithManager},
		{"SwarmadaConfig delete", (&swarmadawebhook.SwarmadaConfigDeleteGuard{APIReader: mgr.GetAPIReader()}).SetupWebhookWithManager},
		{"Robot status estop", (&swarmadawebhook.RobotStatusEstopGuard{}).SetupWebhookWithManager},
		{"SwarmadaConfig", (&swarmadawebhook.SwarmadaConfigValidator{EstopAuthz: sar()}).SetupWebhookWithManager},
		{"FleetZone", (&swarmadawebhook.FleetZoneValidator{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), EstopAuthz: sar()}).SetupWebhookWithManager},
		{"ZoneMaintenance", (&swarmadawebhook.ZoneMaintenanceValidator{Client: mgr.GetClient()}).SetupWebhookWithManager},
		{"FleetAction", (&swarmadawebhook.FleetActionValidator{CancelAuthz: sar(), Reader: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWebhookWithManager},
		{"FleetTask", (&swarmadawebhook.FleetTaskValidator{AppendAuthz: sar(), CancelAuthz: sar(), ActivateAuthz: sar(), Reader: mgr.GetClient()}).SetupWebhookWithManager},
		{"ComputeWorkloadTemplate", (&swarmadawebhook.ComputeWorkloadTemplateValidator{}).SetupWebhookWithManager},
		{"FirmwareRollout", (&swarmadawebhook.FirmwareRolloutValidator{Client: mgr.GetClient()}).SetupWebhookWithManager},
		{"ModelRollout", (&swarmadawebhook.ModelRolloutValidator{}).SetupWebhookWithManager},
	} {
		if err := w.setup(mgr); err != nil {
			return fmt.Errorf("registering the %s webhook: %w", w.name, err)
		}
	}
	return nil
}

// newWiredManager builds a manager on cfg with every webhook registered. It is not started.
func newWiredManager(cfg *rest.Config, scheme *runtime.Scheme, server crwebhook.Options) (ctrl.Manager, error) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:        scheme,
		Metrics:       metricsserver.Options{BindAddress: "0"},
		WebhookServer: crwebhook.NewServer(server),
	})
	if err != nil {
		return nil, fmt.Errorf("building the manager: %w", err)
	}
	return mgr, registerWebhooks(mgr)
}

// startRig starts the control plane with the CRDs and the whole generated webhook manifest, one
// manager serving every webhook, and the shipped ClusterRoles. It returns the stop function.
func startRig() func() {
	scheme, err := newScheme()
	if err != nil {
		rigFatal = fmt.Sprintf("scheme setup failed: %v", err)
		return func() {}
	}
	rigScheme = scheme

	if missing := missingControlPlaneBinaries(); len(missing) > 0 {
		if required, why := envtestRequired(); required {
			rigFatalHeadline = rigRequiredHeadline
			rigFatal = fmt.Sprintf("%s is set, so a missing control plane fails this run instead of "+
				"skipping it. Missing: %s. Install the binaries (`make setup-envtest`) and point "+
				"KUBEBUILDER_ASSETS at them (`make test-go` does both).", why, strings.Join(missing, "; "))
			return func() {}
		}
		fmt.Printf("envtest: control-plane binaries not installed (%s); held-work rows will skip "+
			"(run `make setup-envtest` to enable)\n", strings.Join(missing, "; "))
		return func() {}
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join(repoRoot, "config", "webhook", "manifests.yaml")},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		rigFatal = fmt.Sprintf("the control-plane binaries are installed but the API server did not "+
			"start (or the generated CRDs/webhook manifest were rejected): %v", err)
		return func() {}
	}
	stopEnv := func() { _ = env.Stop() }

	// One principal for the binary: it is a package global, and no row changes it.
	if err := swarmadawebhook.SetControllerPrincipal(controllerUser); err != nil {
		stopEnv()
		rigFatal = fmt.Sprintf("setting the controller principal: %v", err)
		return func() {}
	}
	opts := env.WebhookInstallOptions
	mgr, err := newWiredManager(cfg, scheme, crwebhook.Options{
		Host: opts.LocalServingHost, Port: opts.LocalServingPort, CertDir: opts.LocalServingCertDir,
	})
	if err != nil {
		stopEnv()
		rigFatal = err.Error()
		return func() {}
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		stopEnv()
		rigFatal = fmt.Sprintf("building the admin client: %v", err)
		return func() {}
	}
	if err := installShippedRoles(admin); err != nil {
		stopEnv()
		rigFatal = err.Error()
		return func() {}
	}
	if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "swarmada-system"}}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		stopEnv()
		rigFatal = fmt.Sprintf("creating the control plane's namespace: %v", err)
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			fmt.Fprintln(os.Stderr, "held-work rig: the manager did not stop")
		}
		stopEnv()
	}
	addr := net.JoinHostPort(opts.LocalServingHost, fmt.Sprint(opts.LocalServingPort))
	if err := waitForTLS(addr, 20*time.Second); err != nil {
		stop()
		rigFatal = fmt.Sprintf("the webhook server never accepted a connection on %s: %v", addr, err)
		return func() {}
	}
	rigCfg, rigAdmin, rigMgr, rigReady = cfg, admin, mgr, true
	return stop
}

// installShippedRoles installs the ClusterRoles of shippedRoles under their shipped names.
func installShippedRoles(admin client.Client) error {
	for _, file := range shippedRoles {
		path := filepath.Join(repoRoot, "config", "rbac", file)
		roles, err := loadClusterRoles(path)
		if err != nil {
			return err
		}
		if len(roles) == 0 {
			return fmt.Errorf("%s holds no ClusterRole", path)
		}
		for _, cr := range roles {
			if err := admin.Create(context.Background(), cr); err != nil {
				return fmt.Errorf("installing ClusterRole %s from %s: %w", cr.Name, path, err)
			}
		}
	}
	return nil
}

// loadClusterRoles parses the ClusterRoles in a manifest, skipping Helm template lines, as
// internal/webhook's loadClusterRoles does. Names are kept as shipped.
func loadClusterRoles(path string) ([]*rbacv1.ClusterRole, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var kept [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("{{")) {
			continue
		}
		kept = append(kept, line)
	}
	var out []*rbacv1.ClusterRole
	for _, doc := range strings.Split(string(bytes.Join(kept, []byte("\n"))), "\n---") {
		if !strings.Contains(doc, "kind: ClusterRole") {
			continue
		}
		cr := &rbacv1.ClusterRole{}
		if err := yaml.UnmarshalStrict([]byte(doc), cr); err != nil {
			return nil, fmt.Errorf("parsing a ClusterRole in %s: %w", path, err)
		}
		out = append(out, cr)
	}
	return out, nil
}

func waitForTLS(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the test's own server
		if err == nil {
			_ = conn.Close()
			return nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

// TestHeldWorkRigIsUsable makes a broken rig visible without -v.
func TestHeldWorkRigIsUsable(t *testing.T) {
	if rigFatal != "" {
		t.Fatalf("%s\n\n%s", rigFatalHeadline, rigFatal)
	}
	if !rigReady {
		t.Skip("envtest control-plane binaries not installed; run `make setup-envtest`. " +
			"This is the supported optional path.")
	}
}

// requireRig skips a row when the rig is not up.
func requireRig(t *testing.T) {
	t.Helper()
	if rigReady {
		return
	}
	if rigFatal != "" {
		t.Skip("the held-work rig FAILED to come up — this run is failing, not passing; " +
			"see TestHeldWorkRigIsUsable for the cause")
	}
	t.Skip("envtest control plane not available; run `make setup-envtest`")
}
