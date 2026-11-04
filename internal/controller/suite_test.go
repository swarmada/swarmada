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

package controller

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
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// envtest harness (docs/testing.md layer 2). The controller state machines are
// exercised against a real API server so the status subresource, resourceVersion
// semantics, and CRD schema behave as in production.
//
// The harness is OPTIONAL at run time: when the envtest control-plane binaries are
// not installed (KUBEBUILDER_ASSETS unset — e.g. a plain `go test`), TestMain logs
// and continues, and every envtest test skips itself via requireEnvtest. This keeps
// the package's existing fake-client unit tests runnable everywhere while the
// envtest coverage runs in CI (`make setup-envtest` + `make test-go`).
//
// # Not every failure is a skip
//
// That optionality used to swallow a defect. envtest.Environment.Start() starts the
// control plane AND installs the CRDs, and it returns one error for both, so two
// unrelated conditions landed in the same branch and both skipped:
//
//   - the developer has no envtest binaries — the intended case, and skipping is right;
//   - the generated CRDs are UNINSTALLABLE, because the API server rejected them —
//     a build-breaking defect in this repository.
//
// The second is not a property of the machine. It was reached for real: a CEL rule on
// the FleetZone CRD crossed the API server's rule-cost budget, `make manifests`
// exited 0 and wrote the rejected CRD anyway, and this package then reported
//
//	ok  	github.com/swarmada/swarmada/internal/controller	5.375s
//
// with all 55 envtest tests skipped and the explanation visible only under -v.
// A suite that goes green by not running is worse than one that goes red.
//
// So the two conditions are separated STRUCTURALLY, not by reading Start()'s error
// text — error strings are exactly what rots:
//
//  1. missingControlPlaneBinaries() stats the binaries the harness is about to exec,
//     resolved the same way controller-runtime resolves them. Absent → skip, as before.
//  2. The control plane is started with NO CRDs, so a Start() failure can only mean
//     "the binaries are here and the API server still would not run" → hard failure.
//  3. The CRDs are installed in a SEPARATE, attributable step, so their rejection is
//     reported as their rejection → hard failure naming the path and the reason.
//
// A hard failure is recorded in envFatal and surfaced two ways, so it cannot be
// missed in default (non-verbose) output: TestEnvtestHarnessIsUsable fails with it,
// and TestMain forces a non-zero exit even when that test was filtered out by -run.
//
// # Required mode: where a skip is itself the defect
//
// The skip in (1) is right on a developer's machine and wrong in CI. There, "binaries
// missing" means the pipeline stopped installing them, and skipping would report every
// envtest green without running one. That is exactly how no envtest ran in CI before
// the install step existed. So when envtestRequired() says so (CI=true, which GitHub
// Actions sets, or SWARMADA_REQUIRE_ENVTEST=1), missing binaries are a hard failure too,
// reported under envtestRequiredHeadline. `make test-go` enforces the same rule before it
// calls go test. This covers a direct `go test` as well.
var (
	envK8s    client.Client
	envScheme *runtime.Scheme
	// envCfg is the envtest API server's config, for a test that runs a real manager.
	envCfg   *rest.Config
	envReady bool

	// envFatal is non-empty when the harness SHOULD have come up and did not. It is
	// deliberately distinct from "not available": absent binaries leave it empty.
	envFatal string
)

// envtestHarnessHeadline prefixes every hard-failure report, so the reason a suite
// went red is greppable and reads the same from TestMain and from the guard test.
const envtestHarnessHeadline = "the envtest harness could not come up, and the envtest " +
	"binaries ARE installed — this is a defect in the repository, not a missing tool. " +
	"Every envtest in this package was skipped; none of their results mean anything."

// envtestRequiredHeadline prefixes the one hard failure that is about the machine rather
// than the repository: required mode is on and the binaries are not there.
const envtestRequiredHeadline = "envtest is REQUIRED in this run and its control-plane " +
	"binaries are not installed. Every envtest in this package was skipped, so a green " +
	"result here would claim tests that never ran."

// envFatalHeadline is the headline envFatal is reported under.
var envFatalHeadline = envtestHarnessHeadline

// envtestRequired reports whether missing envtest binaries must fail this run rather
// than skip it, and which variable said so. CI=true is what GitHub Actions and most CI
// systems set; SWARMADA_REQUIRE_ENVTEST=1 opts in anywhere else. With neither, the
// documented developer skip applies.
func envtestRequired() (bool, string) {
	if os.Getenv("SWARMADA_REQUIRE_ENVTEST") == "1" {
		return true, "SWARMADA_REQUIRE_ENVTEST=1"
	}
	if os.Getenv("CI") == "true" {
		return true, "CI=true"
	}
	return false, ""
}

func TestMain(m *testing.M) {
	stop := startEnvtest()
	code := m.Run()
	stop()
	// TestEnvtestHarnessIsUsable already fails on envFatal, which is what makes the
	// reason visible without -v. This guard covers the case where that test did not
	// run at all — `go test -run TestEnvtest_Something` — so a filtered run cannot
	// report success on a harness that never came up.
	if envFatal != "" && code == 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %s\n      %s\n", envFatalHeadline, envFatal)
		code = 1
	}
	os.Exit(code)
}

// controlPlaneBinary reports the path envtest will exec for the named binary.
//
// It reproduces the precedence order of
// sigs.k8s.io/controller-runtime/pkg/internal/testing/process.BinPathFinder, which is
// an internal package and so cannot be called: TEST_ASSET_<NAME>, then
// KUBEBUILDER_ASSETS, then the Environment's BinaryAssetsDirectory (unset here), then
// /usr/local/kubebuilder/bin. Upstream does NOT stat the result — its own doc comment
// says "the asset is not checked for existence on the filesystem" — which is why the
// presence check below is ours to make.
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

// missingControlPlaneBinaries returns the control-plane binaries envtest needs that
// are not present and executable. An empty result means the tool is installed, and
// therefore that any later failure is the repository's fault rather than the
// machine's. kubectl is resolved by envtest too but is not required to Start().
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

// startEnvtest brings up the API server and returns a teardown func. Absent binaries
// leave envReady false and envFatal empty (tests skip, the package still passes); any
// other failure sets envFatal, and the package FAILS.
func startEnvtest() func() {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			// A scheme that will not build is a defect in this repository. It has
			// nothing to do with whether a tool is installed, so it must not skip.
			envFatal = fmt.Sprintf("scheme setup failed: %v", err)
			return func() {}
		}
	}
	envScheme = scheme

	if missing := missingControlPlaneBinaries(); len(missing) > 0 {
		if required, why := envtestRequired(); required {
			envFatalHeadline = envtestRequiredHeadline
			envFatal = fmt.Sprintf("%s is set, so a missing control plane fails this run "+
				"instead of skipping it. Missing: %s. Install the binaries (`make "+
				"setup-envtest`) and point KUBEBUILDER_ASSETS at them (`make test-go` does "+
				"both).", why, strings.Join(missing, "; "))
			return func() {}
		}
		fmt.Printf("envtest: control-plane binaries not installed (%s); envtest tests "+
			"will skip (run `make setup-envtest` to enable)\n", strings.Join(missing, "; "))
		return func() {}
	}

	// Deliberately no CRDDirectoryPaths: with the CRDs out of Start(), its error can
	// only be about the control plane itself, and the CRDs get their own step below.
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		envFatal = fmt.Sprintf("the control-plane binaries are installed, but the API "+
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
		envFatal = fmt.Sprintf("the generated CRDs in %s were REJECTED by the API "+
			"server. `make manifests` can exit 0 and still write a CRD that will not "+
			"install — and a CRD that will not install here will not install on a "+
			"cluster either, because deploy/swarmada/templates/crds.yaml carries the "+
			"same YAML. Rejection: %v", crdPath, err)
		return func() {}
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		stop()
		envFatal = fmt.Sprintf("client build failed: %v", err)
		return func() {}
	}
	envK8s, envCfg, envReady = c, cfg, true
	return stop
}

// TestEnvtestHarnessIsUsable is the guard that makes a broken harness visible in
// default `go test` output. It is the only test in this package that fails rather
// than skips when envtest did not come up, so a red run names its cause once instead
// of fifty-five times.
func TestEnvtestHarnessIsUsable(t *testing.T) {
	if envFatal != "" {
		t.Fatalf("%s\n\n%s", envFatalHeadline, envFatal)
	}
	if !envReady {
		t.Skip("envtest control-plane binaries not installed; run `make setup-envtest`. " +
			"This is the supported optional path: the package's fake-client tests still run.")
	}
}

// requireEnvtest skips the calling test unless the envtest control plane is up.
//
// It still SKIPS on a hard failure rather than failing: TestEnvtestHarnessIsUsable
// has already failed the package, and fifty-five copies of the same message would
// bury it. The skip reason says so, so a reader of -v output cannot mistake this for
// the ordinary "tool not installed" case.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if envReady {
		return
	}
	if envFatal != "" {
		t.Skip("envtest harness FAILED to come up — this run is failing, not passing; " +
			"see TestEnvtestHarnessIsUsable for the cause")
	}
	t.Skip("envtest control plane not available; run `make setup-envtest`")
}

// envtestNamespace creates a uniquely-named namespace and registers its cleanup, so
// each envtest gets an isolated namespace (SwarmadaConfig is a per-namespace
// singleton, and isolation keeps tests order-independent).
func envtestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "et-"}}
	if err := envK8s.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating test namespace: %v", err)
	}
	t.Cleanup(func() { _ = envK8s.Delete(context.Background(), ns) })
	return ns.Name
}
