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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/controller"
)

// SWARMADA_CONFIG_MODIFIED records a change the API server stored (RFC-0001
// #safety-required-events). A review sent to the webhook port by hand, a server-side dry run,
// and an update another admission check refuses store nothing, so the chain gains no entry
// from any of them. Each row ends with one real change, and the chain then holds exactly that
// one entry, written by the controller.

// syncAudit is an audit sink safe for the webhook server's and the controller's goroutines.
type syncAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (s *syncAudit) Record(e audit.Entry) (audit.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *syncAudit) configModified(ns string) []audit.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Entry
	for _, e := range s.entries {
		if e.EventType == audit.EventSwarmadaConfigMod && e.Resource.Namespace == ns {
			out = append(out, e)
		}
	}
	return out
}

// refuseLabelledConfigPath serves a second validating webhook on SwarmadaConfig updates that
// refuses any config labelled refuseLabel: an admission check that runs beside the Swarmada one
// and refuses an update the Swarmada one admits.
const (
	refuseLabelledConfigPath = "/refuse-labelled-swarmadaconfig"
	refuseLabel              = "audit-test.example.com/refuse"
)

// giveRecorder sets every audit.Recorder field of *component to rec, as the manager gives its
// recorder to each component that holds one. The rows then ask of whatever holds a recorder,
// webhook or reconciler, whether a request the API server never stored reaches the chain.
func giveRecorder(component any, rec audit.Recorder) {
	v := reflect.ValueOf(component).Elem()
	recorderType := reflect.TypeOf((*audit.Recorder)(nil)).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); v.Type().Field(i).Type == recorderType && f.CanSet() {
			f.Set(reflect.ValueOf(rec))
		}
	}
}

// wireConfigAudit registers the SwarmadaConfig webhook and reconciler as cmd/manager/main.go
// does, with rec as the manager's audit recorder.
func wireConfigAudit(mgr ctrl.Manager, rec audit.Recorder) error {
	validator := &SwarmadaConfigValidator{EstopAuthz: &SARAuthorizer{Client: mgr.GetClient()}}
	giveRecorder(validator, rec)
	if err := validator.SetupWebhookWithManager(mgr); err != nil {
		return err
	}
	reconciler := &controller.SwarmadaConfigReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	giveRecorder(reconciler, rec)
	return reconciler.SetupWithManager(mgr)
}

type configAuditWire struct {
	admin   client.Client
	rec     *syncAudit
	webhook string // https://host:port of the webhook server
	tlsCfg  *tls.Config
}

func startConfigAuditWire(t *testing.T) *configAuditWire {
	t.Helper()
	requireWebhookEnvtest(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme, authorizationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme setup: %v", err)
		}
	}
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	path := refuseLabelledConfigPath
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "webhook", "manifests.yaml")},
			ValidatingWebhooks: []*admissionregistrationv1.ValidatingWebhookConfiguration{{
				ObjectMeta: metav1.ObjectMeta{Name: "refuse-labelled-swarmadaconfig"},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{{
					Name:                    "refuse-labelled.swarmadaconfig.example.com",
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             &none,
					FailurePolicy:           &fail,
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{Name: "webhook-service", Namespace: "system", Path: &path}},
					Rules: []admissionregistrationv1.RuleWithOperations{{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{APIGroups: []string{fleetv1.GroupVersion.Group},
							APIVersions: []string{"v1"}, Resources: []string{"swarmadaconfigs"}}}},
				}},
			}},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("the API server did not start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	opts := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: crwebhook.NewServer(crwebhook.Options{
			Host: opts.LocalServingHost, Port: opts.LocalServingPort, CertDir: opts.LocalServingCertDir}),
	})
	if err != nil {
		t.Fatalf("building the manager: %v", err)
	}
	rec := &syncAudit{}
	if err := wireConfigAudit(mgr, rec); err != nil {
		t.Fatalf("wiring the SwarmadaConfig webhook and controller: %v", err)
	}
	mgr.GetWebhookServer().Register(refuseLabelledConfigPath, &admission.Webhook{Handler: admission.HandlerFunc(
		func(_ context.Context, req admission.Request) admission.Response {
			var meta struct {
				Metadata metav1.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal(req.Object.Raw, &meta); err == nil && meta.Metadata.Labels[refuseLabel] != "" {
				return admission.Denied("refused by a second admission check")
			}
			return admission.Allowed("")
		})})
	ctx, cancel := context.WithCancel(context.Background())
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
	if err := waitForTLS(addr, 20*time.Second); err != nil {
		t.Fatalf("the webhook server never accepted a connection on %s: %v", addr, err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(opts.LocalServingCAData)
	return &configAuditWire{admin: admin, rec: rec, webhook: "https://" + addr,
		tlsCfg: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

// configIn creates ns and its SwarmadaConfig, and waits for the controller's first pass.
func (w *configAuditWire) configIn(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cfg-audit-"}}
	if err := w.admin.Create(ctx, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: controller.SwarmadaConfigName, Namespace: ns.Name}}
	if err := w.admin.Create(ctx, cfg); err != nil {
		t.Fatalf("SwarmadaConfig: %v", err)
	}
	w.waitObserved(t, ns.Name, 1)
	return ns.Name
}

func (w *configAuditWire) get(t *testing.T, ns string) *fleetv1.SwarmadaConfig {
	t.Helper()
	cfg := &fleetv1.SwarmadaConfig{}
	if err := w.admin.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: controller.SwarmadaConfigName}, cfg); err != nil {
		t.Fatalf("reading the SwarmadaConfig: %v", err)
	}
	return cfg
}

func (w *configAuditWire) waitObserved(t *testing.T, ns string, gen int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if w.get(t, ns).Status.ObservedGeneration >= gen {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the controller never observed generation %d of %s", gen, ns)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// update writes one spec change as the admin, with extra applied to the same write.
func (w *configAuditWire) update(t *testing.T, ns string, extra func(*fleetv1.SwarmadaConfig), opts ...client.UpdateOption) error {
	t.Helper()
	cfg := w.get(t, ns)
	cfg.Spec.Health.TelemetryIntervalSeconds++
	if extra != nil {
		extra(cfg)
	}
	return w.admin.Update(context.Background(), cfg, opts...)
}

// forgedReview POSTs a hand-built AdmissionReview of a spec change straight to the webhook
// port, naming a user, as any caller that reaches the port can.
func (w *configAuditWire) forgedReview(t *testing.T, ns string) *admissionv1.AdmissionResponse {
	t.Helper()
	oldCfg := w.get(t, ns)
	oldCfg.TypeMeta = metav1.TypeMeta{APIVersion: fleetv1.GroupVersion.String(), Kind: "SwarmadaConfig"}
	newCfg := oldCfg.DeepCopy()
	newCfg.Spec.Health.TelemetryIntervalSeconds++
	oldRaw, _ := json.Marshal(oldCfg)
	newRaw, _ := json.Marshal(newCfg)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID("forged-1"),
			Kind:      metav1.GroupVersionKind{Group: fleetv1.GroupVersion.Group, Version: "v1", Kind: "SwarmadaConfig"},
			Resource:  metav1.GroupVersionResource{Group: fleetv1.GroupVersion.Group, Version: "v1", Resource: "swarmadaconfigs"},
			Name:      newCfg.Name,
			Namespace: ns,
			Operation: admissionv1.Update,
			UserInfo:  authenticationv1.UserInfo{Username: "alice"},
			Object:    runtime.RawExtension{Raw: newRaw},
			OldObject: runtime.RawExtension{Raw: oldRaw},
		},
	}
	body, _ := json.Marshal(review)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: w.tlsCfg}, Timeout: 10 * time.Second}
	resp, err := hc.Post(w.webhook+"/validate-swarmada-io-v1-swarmadaconfig", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("posting the review: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out admissionv1.AdmissionReview
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	return out.Response
}

// wantOnlyTheStoredChange makes one real change and asserts the chain holds exactly its entry,
// written by the controller once it observed the stored generation.
func (w *configAuditWire) wantOnlyTheStoredChange(t *testing.T, ns string) {
	t.Helper()
	if err := w.update(t, ns, nil); err != nil {
		t.Fatalf("the real change was refused: %v", err)
	}
	gen := w.get(t, ns).Generation
	w.waitObserved(t, ns, gen)
	got := w.rec.configModified(ns)
	if len(got) != 1 {
		t.Fatalf("SWARMADA_CONFIG_MODIFIED entries = %d, want 1 for the one stored change: %+v", len(got), got)
	}
	e := got[0]
	if e.Actor != (audit.Actor{Type: audit.ActorServiceAccount, Identity: "swarmadaconfig-controller"}) ||
		e.Resource.Name != w.get(t, ns).Name || e.Namespace != ns {
		t.Errorf("the entry is not the controller's record of generation %d: %+v", gen, e)
	}
}

func TestEnvtest_ConfigModifiedRecordsOnlyStoredChanges(t *testing.T) {
	w := startConfigAuditWire(t)

	t.Run("a-review-sent-to-the-webhook-port", func(t *testing.T) {
		t.Parallel()
		ns := w.configIn(t)
		resp := w.forgedReview(t, ns)
		t.Logf("RESULT hand-built review: allowed=%v", resp != nil && resp.Allowed)
		if resp == nil || !resp.Allowed {
			t.Fatalf("fixture: the reviewed spec change was meant to be valid: %+v", resp)
		}
		w.wantOnlyTheStoredChange(t, ns)
	})
	t.Run("a-server-side-dry-run", func(t *testing.T) {
		t.Parallel()
		ns := w.configIn(t)
		if err := w.update(t, ns, nil, client.DryRunAll); err != nil {
			t.Fatalf("the dry run was refused: %v", err)
		}
		w.wantOnlyTheStoredChange(t, ns)
	})
	t.Run("an-update-another-check-refuses", func(t *testing.T) {
		t.Parallel()
		ns := w.configIn(t)
		err := w.update(t, ns, func(c *fleetv1.SwarmadaConfig) { c.Labels = map[string]string{refuseLabel: "yes"} })
		t.Logf("RESULT update refused by the second check: %v", err)
		if err == nil {
			t.Fatalf("fixture: the second admission check was meant to refuse the update")
		}
		w.wantOnlyTheStoredChange(t, ns)
	})
	t.Run("a-stored-change", func(t *testing.T) {
		t.Parallel()
		ns := w.configIn(t)
		w.wantOnlyTheStoredChange(t, ns)
	})
}
