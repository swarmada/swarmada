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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const (
	// hmacSecretKey is the Secret data key holding the shared HMAC secret
	// (crds/modelpolicy.md §9.1.10.3).
	hmacSecretKey = "hmac-secret"
	// signatureHeader carries "sha256=<hex HMAC-SHA256 of the raw body>".
	signatureHeader = "X-Swarmada-Signature-256"
	// webhookMaxBody bounds the request body a webhook will read.
	webhookMaxBody = 1 << 20 // 1 MiB
	// webhookPathPrefix routes to /webhooks/v1/model-policy/{namespace}/{name}.
	webhookPathPrefix = "/webhooks/v1/model-policy/"
)

// isaacWebhookPayload is the Isaac Lab training-completion POST body (§9.1.10.3),
// snake_case per the published contract. It is translated into the internal
// modelTriggerPayload the ModelPolicyReconciler consumes; extra fields
// (model_signature_ref, training_metadata, …) are ignored here.
type isaacWebhookPayload struct {
	ModelVersion      string             `json:"model_version"`
	ModelURI          string             `json:"model_uri"`
	ModelChecksum     string             `json:"model_checksum"`
	ModelSignatureRef string             `json:"model_signature_ref"`
	Metrics           map[string]float64 `json:"metrics"`
}

// ModelPolicyWebhook serves the training-completion webhook (§9.1.10.3) as a
// manager.Runnable. It verifies the HMAC signature and writes the model-trigger
// annotation onto the ModelPolicy; the ModelPolicyReconciler is the SINGLE
// evaluation path (this front-end never evaluates the quality gate itself).
type ModelPolicyWebhook struct {
	Client client.Client
	Addr   string
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Start implements manager.Runnable: serve until ctx is cancelled, then shut down.
func (w *ModelPolicyWebhook) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPathPrefix, w.handle)
	srv := &http.Server{Addr: w.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() {
		logf.Log.WithName("modelpolicy-webhook").Info("serving ModelPolicy webhook", "address", w.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		return err
	}
}

// NeedLeaderElection returns false: the webhook must accept POSTs on every replica.
func (w *ModelPolicyWebhook) NeedLeaderElection() bool { return false }

func (w *ModelPolicyWebhook) handle(rw http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	log := logf.FromContext(ctx).WithName("modelpolicy-webhook")

	if req.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ns, name, ok := parseWebhookPath(req.URL.Path)
	if !ok {
		http.Error(rw, "bad path (want /webhooks/v1/model-policy/{namespace}/{name})", http.StatusNotFound)
		return
	}

	var policy fleetv1.ModelPolicy
	if err := w.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(rw, "model policy not found", http.StatusNotFound)
			return
		}
		http.Error(rw, "resolving policy", http.StatusInternalServerError)
		return
	}
	wc := policy.Spec.Trigger.Webhook
	if policy.Spec.Trigger.Type != fleetv1.ModelPolicyTriggerWebhook || wc == nil || !wc.Enabled {
		http.Error(rw, "webhook trigger not enabled for this policy", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, webhookMaxBody))
	if err != nil {
		http.Error(rw, "reading body", http.StatusBadRequest)
		return
	}

	// Authentication is REQUIRED by default (ADR-0020). An unauthenticated request
	// is accepted only when the policy has no authSecretRef AND has explicitly opted
	// in via allowUnauthenticated (development/simulation only).
	switch {
	case wc.AuthSecretRef != "":
		secret, err := w.hmacSecret(ctx, ns, wc.AuthSecretRef)
		if err != nil {
			log.Error(err, "loading HMAC secret", "policy", name)
			http.Error(rw, "signing secret unavailable", http.StatusInternalServerError)
			return
		}
		if !verifyHMAC(body, req.Header.Get(signatureHeader), secret) {
			http.Error(rw, "invalid or missing X-Swarmada-Signature-256", http.StatusUnauthorized)
			return
		}
	case wc.AllowUnauthenticated:
		// Explicit dev/sim opt-in; accept without a signature.
	default:
		http.Error(rw, "authentication required: configure authSecretRef (or set allowUnauthenticated for dev)", http.StatusUnauthorized)
		return
	}

	var in isaacWebhookPayload
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(rw, "malformed JSON payload", http.StatusBadRequest)
		return
	}
	if in.ModelVersion == "" || in.ModelURI == "" {
		http.Error(rw, "payload missing model_version or model_uri", http.StatusBadRequest)
		return
	}

	// A conversion, not a field-by-field copy. The two types are structurally identical and differ
	// only in their JSON tags (the Isaac wire format is snake_case, the internal payload camelCase),
	// so converting is exact — and if the two schemas ever diverge this stops COMPILING, whereas a
	// literal would silently drop the new field.
	if err := w.writeTrigger(ctx, &policy, modelTriggerPayload(in)); err != nil {
		log.Error(err, "writing trigger annotation", "policy", name)
		http.Error(rw, "recording trigger", http.StatusInternalServerError)
		return
	}
	rw.WriteHeader(http.StatusAccepted)
	_, _ = rw.Write([]byte("accepted"))
}

func (w *ModelPolicyWebhook) hmacSecret(ctx context.Context, ns, ref string) ([]byte, error) {
	var secret corev1.Secret
	if err := w.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref}, &secret); err != nil {
		return nil, fmt.Errorf("get secret %q: %w", ref, err)
	}
	key, ok := secret.Data[hmacSecretKey]
	if !ok || len(key) == 0 {
		return nil, fmt.Errorf("secret %q has no non-empty %q key", ref, hmacSecretKey)
	}
	return key, nil
}

// writeTrigger records the training-completion event as the model-trigger
// annotation; the ModelPolicyReconciler consumes and clears it.
func (w *ModelPolicyWebhook) writeTrigger(ctx context.Context, policy *fleetv1.ModelPolicy, payload modelTriggerPayload) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	base := policy.DeepCopy()
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	policy.Annotations[triggerAnnotation] = string(raw)
	return w.Client.Patch(ctx, policy, client.MergeFrom(base))
}

// verifyHMAC constant-time-checks the "sha256=<hex>" signature header over body
// using the shared secret.
func verifyHMAC(body []byte, header string, secret []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

func parseWebhookPath(p string) (namespace, name string, ok bool) {
	rest := strings.TrimPrefix(p, webhookPathPrefix)
	if rest == p {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
