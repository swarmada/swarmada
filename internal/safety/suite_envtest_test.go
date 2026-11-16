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

package safety

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// envtest harness for the safety package, the same contract as internal/controller's and
// internal/webhook's (the reasoning is written out in internal/controller/suite_test.go).
//
// It exists for what a fake client cannot show: the manager's cache trailing the API server.
// The SafetyStream authorizer reads Robots from that cache, so what an EstopAck under a robot
// id the operator has just re-pointed confirms depends on whether the cache has seen the
// re-point. That is only real against a real API server and a real informer.
//
// OPTIONAL at run time: with no envtest binaries, TestMain logs and continues and each envtest
// skips (requireSafetyEnvtest). Required mode (CI=true or SWARMADA_REQUIRE_ENVTEST=1) fails
// instead, as the other harnesses do.
var (
	safetyEnvK8s    client.Client
	safetyEnvCfg    *rest.Config
	safetyEnvScheme *runtime.Scheme
	safetyEnvReady  bool

	// safetyEnvFatal is non-empty when the harness SHOULD have come up and did not.
	safetyEnvFatal string
)

const safetyHarnessHeadline = "the safety envtest harness could not come up, and the envtest " +
	"binaries ARE installed — this is a defect in the repository, not a missing tool. " +
	"Every envtest in this package was skipped; none of their results mean anything."

const safetyRequiredHeadline = "envtest is REQUIRED in this run and its control-plane binaries " +
	"are not installed. Every envtest in this package was skipped, so a green result here " +
	"would claim tests that never ran."

var safetyEnvFatalHeadline = safetyHarnessHeadline

// safetyEnvtestRequired is the other harnesses' rule: SWARMADA_REQUIRE_ENVTEST=1, or CI=true.
func safetyEnvtestRequired() (bool, string) {
	if os.Getenv("SWARMADA_REQUIRE_ENVTEST") == "1" {
		return true, "SWARMADA_REQUIRE_ENVTEST=1"
	}
	if os.Getenv("CI") == "true" {
		return true, "CI=true"
	}
	return false, ""
}

func TestMain(m *testing.M) {
	stop := startSafetyEnvtest()
	code := m.Run()
	stop()
	// A filtered run (`go test -run ...`) skips the guard test below; this keeps it from
	// reporting success on a harness that never came up.
	if safetyEnvFatal != "" && code == 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %s\n      %s\n", safetyEnvFatalHeadline, safetyEnvFatal)
		code = 1
	}
	os.Exit(code)
}

// safetyControlPlaneBinary reproduces controller-runtime's binary-path precedence
// (TEST_ASSET_<NAME>, KUBEBUILDER_ASSETS, /usr/local/kubebuilder/bin).
func safetyControlPlaneBinary(name string) string {
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

func safetyMissingControlPlaneBinaries() []string {
	var missing []string
	for _, name := range []string{"kube-apiserver", "etcd"} {
		p := safetyControlPlaneBinary(name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			missing = append(missing, fmt.Sprintf("%s (looked for %s)", name, p))
		}
	}
	return missing
}

func startSafetyEnvtest() func() {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			safetyEnvFatal = fmt.Sprintf("scheme setup failed: %v", err)
			return func() {}
		}
	}
	safetyEnvScheme = scheme

	if missing := safetyMissingControlPlaneBinaries(); len(missing) > 0 {
		if required, why := safetyEnvtestRequired(); required {
			safetyEnvFatalHeadline = safetyRequiredHeadline
			safetyEnvFatal = fmt.Sprintf("%s is set, so a missing control plane fails this run "+
				"instead of skipping it. Missing: %s. Install the binaries (`make "+
				"setup-envtest`) and point KUBEBUILDER_ASSETS at them (`make test-go` does "+
				"both).", why, strings.Join(missing, "; "))
			return func() {}
		}
		fmt.Printf("envtest: control-plane binaries not installed (%s); envtest tests "+
			"will skip (run `make setup-envtest` to enable)\n", strings.Join(missing, "; "))
		return func() {}
	}

	// No CRDDirectoryPaths: the control plane and the CRDs fail in separate, attributable steps.
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		safetyEnvFatal = fmt.Sprintf("the control-plane binaries are installed, but the API "+
			"server did not start: %v", err)
		return func() {}
	}
	stop := func() { _ = env.Stop() }

	crdPath := filepath.Join("..", "..", "config", "crd", "bases")
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		Scheme:             scheme,
		Paths:              []string{crdPath},
		ErrorIfPathMissing: true,
	}); err != nil {
		stop()
		safetyEnvFatal = fmt.Sprintf("the generated CRDs in %s were REJECTED by the API server: %v",
			crdPath, err)
		return func() {}
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		stop()
		safetyEnvFatal = fmt.Sprintf("client build failed: %v", err)
		return func() {}
	}
	safetyEnvK8s, safetyEnvCfg, safetyEnvReady = c, cfg, true
	return stop
}

// TestSafetyEnvtestHarnessIsUsable makes a broken harness visible without -v.
func TestSafetyEnvtestHarnessIsUsable(t *testing.T) {
	if safetyEnvFatal != "" {
		t.Fatalf("%s\n\n%s", safetyEnvFatalHeadline, safetyEnvFatal)
	}
	if !safetyEnvReady {
		t.Skip("envtest control-plane binaries not installed; run `make setup-envtest`. " +
			"This is the supported optional path: the package's fake-client tests still run.")
	}
}

func requireSafetyEnvtest(t *testing.T) {
	t.Helper()
	if safetyEnvReady {
		return
	}
	if safetyEnvFatal != "" {
		t.Skip("envtest harness FAILED to come up — this run is failing, not passing; " +
			"see TestSafetyEnvtestHarnessIsUsable for the cause")
	}
	t.Skip("envtest control plane not available; run `make setup-envtest`")
}
