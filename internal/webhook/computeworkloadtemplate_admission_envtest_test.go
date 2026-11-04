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
// THE THIRD VECTOR, PROVED AT ADMISSION.
//
// Of the three routes pod-create carries, two are refused by the API server
// itself: a foreign ServiceAccount and a hostPath volume are CEL rules compiled
// into the CRD, and internal/controller's envtest posts both to a real API server
// and watches them bounce. The third — privileged mode — CANNOT be a CEL rule
// here: it lives inside the vendored PodTemplateSpec's unbounded containers list,
// and a rule walking that list is refused by the API server on cost estimation,
// taking the whole CRD with it (ADR-0056). It is enforced by a validating webhook
// instead.
//
// Which leaves a gap that calling the validator directly cannot close. Every
// other test in this package calls it by hand, and that proves the RULES while
// proving nothing about whether the API server would ever reach them: a wrong
// path string, a type registered under a different GroupVersionKind, or a
// ValidatingWebhookConfiguration whose rules do not match the resource all look
// exactly like a working gate when the function is invoked directly. The
// difference between vector 3 and the other two would then be that only the
// other two were known to fire.
//
// So this starts a real API server, installs the GENERATED webhook manifest
// rather than a hand-built one, runs the SHIPPED registration
// (SetupWebhookWithManager), and creates templates over the wire. A privileged
// template must come back refused, naming the rule; an ordinary one must be
// admitted. Skips when the envtest binaries are absent, exactly as the rest of
// this package's envtests do.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/compute"
)

func TestEnvtest_WebhookRefusesPrivilegedModeOverTheWire(t *testing.T) {
	if missing := whMissingControlPlaneBinaries(); len(missing) > 0 {
		t.Skipf("envtest control-plane binaries not installed (%s); run `make setup-envtest`",
			strings.Join(missing, "; "))
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme setup: %v", err)
		}
	}

	// Its own environment rather than the package harness's: that one deliberately
	// starts with no webhooks, because what it exists to measure is the CRD's CEL.
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			// The GENERATED manifest, not a fixture: a registration that drifted from
			// the +kubebuilder:webhook marker must fail here.
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
	// The shipped registration path, so a wrong URL path or GVK fails here rather
	// than passing quietly.
	if err := (&ComputeWorkloadTemplateValidator{}).SetupWebhookWithManager(mgr); err != nil {
		t.Fatalf("registering the validator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()

	addr := net.JoinHostPort(opts.LocalServingHost, fmt.Sprint(opts.LocalServingPort))
	if err := waitForTLS(addr, 20*time.Second); err != nil {
		t.Fatalf("the webhook server never accepted a connection on %s: %v", addr, err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	// ── the negative case: vector 3, over the wire ───────────────────────────
	privileged := wireTemplate("escalating")
	yes := true
	privileged.Spec.PodTemplate.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes}

	err = c.Create(ctx, privileged)
	if err == nil {
		t.Fatal("the API server ACCEPTED a ComputeWorkloadTemplate requesting privileged mode. " +
			"When provision: workload dispatch is wired this object is created by the MANAGER's " +
			"ServiceAccount, so admitting it makes create on computeworkloadtemplates equivalent " +
			"to create on a privileged pod — which holds the node's capabilities and devices")
	}
	if !strings.Contains(err.Error(), "Privileged Containers") {
		t.Errorf("refused, but the message does not name the control: %v", err)
	}
	// The refusal an administrator actually reads must carry the rule, not only
	// the field — the same requirement every other refusal in this policy meets.
	if !strings.Contains(err.Error(), compute.PolicyRule) {
		t.Errorf("refused over the wire without quoting the rule it enforces: %v", err)
	}

	// ── the positive case: the gate is a deny-list, not a wall ───────────────
	if err := c.Create(ctx, wireTemplate("ordinary")); err != nil {
		t.Errorf("an ordinary template was refused by the admission chain: %v", err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("the manager stopped with an error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("the manager did not stop")
	}
}

// wireTemplate is an otherwise ordinary template, so a refusal below can only be
// attributed to the one field the caller adds.
func wireTemplate(name string) *fleetv1.ComputeWorkloadTemplate {
	return &fleetv1.ComputeWorkloadTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: fleetv1.ComputeWorkloadTemplateSpec{
			Mode: fleetv1.WorkloadTemplateModeJob,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "runtime",
						Image: "ghcr.io/example/plate-reader@sha256:0000",
					}},
				},
			},
		},
	}
}

// waitForTLS returns once the webhook server is accepting connections, so the
// first Create is not racing the server's startup — a flake there would look
// exactly like a gate that did not fire.
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
