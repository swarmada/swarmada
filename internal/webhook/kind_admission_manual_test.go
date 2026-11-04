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
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// TestKindAdmission_ServeWebhook serves the REAL admission handler for
// ComputeWorkloadTemplate so a live cluster's admission chain can be pointed at
// it over TLS.
//
// MANUAL, gated on SWARMADA_KIND_WEBHOOK=1 so `make test` never starts it.
//
// It exists because every other test in this file calls the validator directly.
// That proves the RULES and proves nothing about whether the API server would
// ever reach them: a wrong path string, a type registered under the wrong
// GroupVersionKind, or a ValidatingWebhookConfiguration that does not match the
// resource all look identical to a working gate when the function is called by
// hand. This is the only check that exercises the wiring.
func TestKindAdmission_ServeWebhook(t *testing.T) {
	if os.Getenv("SWARMADA_KIND_WEBHOOK") != "1" {
		t.Skip("manual: set SWARMADA_KIND_WEBHOOK=1, SWARMADA_WEBHOOK_CERT_DIR, and point a " +
			"cluster's ValidatingWebhookConfiguration at this process")
	}
	certDir := os.Getenv("SWARMADA_WEBHOOK_CERT_DIR")
	if certDir == "" {
		t.Fatal("SWARMADA_WEBHOOK_CERT_DIR is required")
	}
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	// The same handler ctrl.NewWebhookManagedBy(...).WithValidator(...) builds, so
	// what is served here is the shipped path and not a test double of it.
	h := admission.WithCustomValidator(scheme, &fleetv1.ComputeWorkloadTemplate{},
		&ComputeWorkloadTemplateValidator{})

	mux := http.NewServeMux()
	mux.Handle("/validate-swarmada-io-v1-computeworkloadtemplate", h)
	srv := &http.Server{Addr: ":9443", Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		_ = srv.ListenAndServeTLS(filepath.Join(certDir, "tls.crt"), filepath.Join(certDir, "tls.key"))
	}()
	t.Log("serving /validate-swarmada-io-v1-computeworkloadtemplate on :9443")

	dwell := 90 * time.Second
	if os.Getenv("SWARMADA_WEBHOOK_DWELL") != "" {
		if d, err := time.ParseDuration(os.Getenv("SWARMADA_WEBHOOK_DWELL")); err == nil {
			dwell = d
		}
	}
	time.Sleep(dwell)
	_ = srv.Close()
}
