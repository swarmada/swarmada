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
var (
	whEnvK8s   client.Client
	whEnvReady bool
)

func TestMain(m *testing.M) {
	stop := startWebhookEnvtest()
	code := m.Run()
	stop()
	os.Exit(code)
}

func startWebhookEnvtest() func() {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Printf("envtest: scheme setup failed (%v); envtest tests will skip\n", err)
			return func() {}
		}
	}
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
	whEnvK8s, whEnvReady = c, true
	return func() { _ = env.Stop() }
}

func requireWebhookEnvtest(t *testing.T) {
	t.Helper()
	if !whEnvReady {
		t.Skip("envtest control plane not available; run `make setup-envtest`")
	}
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
