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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
var (
	envK8s    client.Client
	envScheme *runtime.Scheme
	envReady  bool
)

func TestMain(m *testing.M) {
	stop := startEnvtest()
	code := m.Run()
	stop()
	os.Exit(code)
}

// startEnvtest attempts to bring up the API server and returns a teardown func. On
// any failure it returns a no-op teardown and leaves envReady false (tests skip).
func startEnvtest() func() {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		fmt.Printf("envtest: scheme setup failed (%v); envtest tests will skip\n", err)
		return func() {}
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		fmt.Printf("envtest: corev1 scheme setup failed (%v); envtest tests will skip\n", err)
		return func() {}
	}
	envScheme = scheme

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Printf("envtest: control plane unavailable (%v); envtest tests will skip "+
			"(run `make setup-envtest` to enable)\n", err)
		return func() {}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		fmt.Printf("envtest: client build failed (%v); envtest tests will skip\n", err)
		return func() {}
	}
	envK8s, envReady = c, true
	return func() { _ = env.Stop() }
}

// requireEnvtest skips the calling test unless the envtest control plane is up.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if !envReady {
		t.Skip("envtest control plane not available; run `make setup-envtest`")
	}
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
