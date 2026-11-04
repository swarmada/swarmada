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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// envtest harness for the webhook package (docs/testing.md layer 2), mirroring the one in
// internal/controller.
//
// It exists because some of what this package produces is only validated by the API server:
// the defaulters MUTATE an object, and whether the mutated result is admissible is decided by
// the CRD's CEL rules, not by any code here. Asserting that in a fake client would prove
// nothing — the fake client runs no CEL. So the object mergeRobotClass actually emits is
// posted to a real API server with the generated CRDs installed.
//
// OPTIONAL at run time, same contract as the controller harness: with no envtest binaries
// installed, TestMain logs and continues and each test skips via requireWebhookEnvtest, so
// `go test ./...` stays runnable everywhere while CI (`make setup-envtest`) runs the coverage.
//
// # Not every failure is a skip
//
// This harness carried the same defect as internal/controller's, and the fix is the same
// shape; the reasoning is written out once, there. In short: envtest.Environment.Start()
// starts the control plane AND installs the CRDs and returns one error for both, so
// "no envtest binaries" (skip, correct) and "the generated CRDs are uninstallable"
// (a build-breaking defect) landed in the same branch and both skipped silently.
//
// The two are now separated STRUCTURALLY rather than by matching Start()'s error text:
// the binaries are stat-ed before starting, the control plane is started with NO CRDs, and
// the CRDs are installed in a separate, attributable step. A hard failure sets whEnvFatal,
// which TestTestEnvtestHarnessIsUsable reports in default output and which TestMain turns
// into a non-zero exit even under a filtering -run.
//
// This package's envtests are the ones that matter most for a rejected CRD, because what
// they assert IS the CRD: the defaulters mutate an object and the API server's CEL decides
// whether the result is admissible. A skipped run here proves nothing at all.
//
// Required mode is the same as the controller harness's, too. When CI=true or
// SWARMADA_REQUIRE_ENVTEST=1, missing binaries set whEnvFatal instead of skipping. That fails
// the package, and with it TestEnvtest_WebhookRefusesPrivilegedModeOverTheWire, which checks
// the binaries itself and still skips on its own.
var (
	whEnvK8s   client.Client
	whEnvReady bool

	// whEnvFatal is non-empty when the harness SHOULD have come up and did not.
	whEnvFatal string
)

// whHarnessHeadline prefixes every hard-failure report from this package.
const whHarnessHeadline = "the webhook envtest harness could not come up, and the envtest " +
	"binaries ARE installed — this is a defect in the repository, not a missing tool. " +
	"Every envtest in this package was skipped; none of their results mean anything."

// whRequiredHeadline prefixes the hard failure for required mode with the binaries absent.
const whRequiredHeadline = "envtest is REQUIRED in this run and its control-plane binaries " +
	"are not installed. Every envtest in this package was skipped, so a green result here " +
	"would claim tests that never ran."

// whEnvFatalHeadline is the headline whEnvFatal is reported under.
var whEnvFatalHeadline = whHarnessHeadline

// whEnvtestRequired reports whether missing envtest binaries must fail this run rather than
// skip it, and which variable said so: SWARMADA_REQUIRE_ENVTEST=1, or CI=true (set by GitHub
// Actions). Same rule as the controller harness's envtestRequired.
func whEnvtestRequired() (bool, string) {
	if os.Getenv("SWARMADA_REQUIRE_ENVTEST") == "1" {
		return true, "SWARMADA_REQUIRE_ENVTEST=1"
	}
	if os.Getenv("CI") == "true" {
		return true, "CI=true"
	}
	return false, ""
}

func TestMain(m *testing.M) {
	stop := startWebhookEnvtest()
	code := m.Run()
	stop()
	// Covers the filtered run (`go test -run ...`) in which the guard test below never
	// executes, so a filtered run cannot report success on a harness that never came up.
	if whEnvFatal != "" && code == 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %s\n      %s\n", whEnvFatalHeadline, whEnvFatal)
		code = 1
	}
	os.Exit(code)
}

// whControlPlaneBinary reproduces controller-runtime's binary-path precedence
// (TEST_ASSET_<NAME>, KUBEBUILDER_ASSETS, BinaryAssetsDirectory, /usr/local/kubebuilder/bin).
// Upstream's BinPathFinder is in an internal package and does not stat what it returns.
func whControlPlaneBinary(name string) string {
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

// whMissingControlPlaneBinaries returns the binaries envtest needs that are not present
// and executable. Empty means the tool is installed, so a later failure is ours.
func whMissingControlPlaneBinaries() []string {
	var missing []string
	for _, name := range []string{"kube-apiserver", "etcd"} {
		p := whControlPlaneBinary(name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			missing = append(missing, fmt.Sprintf("%s (looked for %s)", name, p))
		}
	}
	return missing
}

func startWebhookEnvtest() func() {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			// A scheme that will not build is a repository defect, not a missing tool.
			whEnvFatal = fmt.Sprintf("scheme setup failed: %v", err)
			return func() {}
		}
	}

	if missing := whMissingControlPlaneBinaries(); len(missing) > 0 {
		if required, why := whEnvtestRequired(); required {
			whEnvFatalHeadline = whRequiredHeadline
			whEnvFatal = fmt.Sprintf("%s is set, so a missing control plane fails this run "+
				"instead of skipping it. Missing: %s. Install the binaries (`make "+
				"setup-envtest`) and point KUBEBUILDER_ASSETS at them (`make test-go` does "+
				"both).", why, strings.Join(missing, "; "))
			return func() {}
		}
		fmt.Printf("envtest: control-plane binaries not installed (%s); envtest tests "+
			"will skip (run `make setup-envtest` to enable)\n", strings.Join(missing, "; "))
		return func() {}
	}

	// No CRDDirectoryPaths on purpose — see the note above.
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		whEnvFatal = fmt.Sprintf("the control-plane binaries are installed, but the API "+
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
		whEnvFatal = fmt.Sprintf("the generated CRDs in %s were REJECTED by the API "+
			"server. This package's assertions ARE the CRD's CEL rules, so skipping "+
			"them proves nothing. Rejection: %v", crdPath, err)
		return func() {}
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		stop()
		whEnvFatal = fmt.Sprintf("client build failed: %v", err)
		return func() {}
	}
	whEnvK8s, whEnvReady = c, true
	return stop
}

// TestWebhookEnvtestHarnessIsUsable makes a broken harness visible without -v.
func TestWebhookEnvtestHarnessIsUsable(t *testing.T) {
	if whEnvFatal != "" {
		t.Fatalf("%s\n\n%s", whEnvFatalHeadline, whEnvFatal)
	}
	if !whEnvReady {
		t.Skip("envtest control-plane binaries not installed; run `make setup-envtest`. " +
			"This is the supported optional path.")
	}
}

func requireWebhookEnvtest(t *testing.T) {
	t.Helper()
	if whEnvReady {
		return
	}
	if whEnvFatal != "" {
		t.Skip("envtest harness FAILED to come up — this run is failing, not passing; " +
			"see TestWebhookEnvtestHarnessIsUsable for the cause")
	}
	t.Skip("envtest control plane not available; run `make setup-envtest`")
}

func webhookEnvtestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "wh-"}}
	if err := whEnvK8s.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating test namespace: %v", err)
	}
	t.Cleanup(func() { _ = whEnvK8s.Delete(context.Background(), ns) })
	return ns.Name
}
