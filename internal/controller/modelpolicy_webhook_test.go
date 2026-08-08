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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const mpNS = "warehouse-a"
const mpName = "item-policy"

var hmacKey = []byte("s3cr3t-shared-key")

func webhookPolicy(authSecretRef string, allowUnauth bool, triggerType fleetv1.ModelPolicyTriggerType) *fleetv1.ModelPolicy {
	return &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: mpNS, Name: mpName},
		Spec: fleetv1.ModelPolicySpec{Trigger: fleetv1.ModelPolicyTrigger{
			Type:    triggerType,
			Webhook: &fleetv1.WebhookTriggerConfig{Enabled: true, AuthSecretRef: authSecretRef, AllowUnauthenticated: allowUnauth},
		}},
	}
}

func newWebhook(t *testing.T, objs ...client.Object) (*ModelPolicyWebhook, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &ModelPolicyWebhook{Client: c}, c
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func post(t *testing.T, w *ModelPolicyWebhook, ns, name string, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhookPathPrefix+ns+"/"+name, bytes.NewReader(body))
	if sig != "" {
		req.Header.Set(signatureHeader, sig)
	}
	rr := httptest.NewRecorder()
	w.handle(rr, req)
	return rr
}

func triggerAnno(t *testing.T, c client.Client) (string, bool) {
	t.Helper()
	var p fleetv1.ModelPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: mpNS, Name: mpName}, &p); err != nil {
		t.Fatal(err)
	}
	v, ok := p.Annotations[triggerAnnotation]
	return v, ok
}

var isaacBody = []byte(`{"schema_version":"1.0","model_name":"item-recognition","model_version":"4.1.0","model_uri":"oci://r/models/item:4.1.0","model_checksum":"sha256:abc","metrics":{"pick_success_rate":0.961}}`)

func TestModelWebhook_ValidSignatureWritesTrigger(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: mpNS, Name: "hmac"},
		Data: map[string][]byte{hmacSecretKey: hmacKey}}
	w, c := newWebhook(t, webhookPolicy("hmac", false, fleetv1.ModelPolicyTriggerWebhook), secret)

	rr := post(t, w, mpNS, mpName, isaacBody, sign(isaacBody))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	raw, ok := triggerAnno(t, c)
	if !ok {
		t.Fatal("expected the model-trigger annotation to be written")
	}
	var got modelTriggerPayload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.ModelVersion != "4.1.0" || got.ModelURI != "oci://r/models/item:4.1.0" ||
		got.ModelChecksum != "sha256:abc" || got.Metrics["pick_success_rate"] != 0.961 {
		t.Fatalf("translated payload = %+v", got)
	}
}

func TestModelWebhook_BadSignatureRejected(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: mpNS, Name: "hmac"},
		Data: map[string][]byte{hmacSecretKey: hmacKey}}
	w, c := newWebhook(t, webhookPolicy("hmac", false, fleetv1.ModelPolicyTriggerWebhook), secret)

	// Wrong signature.
	if rr := post(t, w, mpNS, mpName, isaacBody, "sha256=deadbeef"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig status = %d, want 401", rr.Code)
	}
	// Missing header.
	if rr := post(t, w, mpNS, mpName, isaacBody, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig status = %d, want 401", rr.Code)
	}
	if _, ok := triggerAnno(t, c); ok {
		t.Error("a rejected request must not write the trigger annotation")
	}
}

// With allowUnauthenticated explicitly set (dev/sim opt-in), an unsigned request is
// accepted (ADR-0020).
func TestModelWebhook_DevModeOptInAccepts(t *testing.T) {
	w, c := newWebhook(t, webhookPolicy("", true, fleetv1.ModelPolicyTriggerWebhook)) // opt-in, no secret
	if rr := post(t, w, mpNS, mpName, isaacBody, ""); rr.Code != http.StatusAccepted {
		t.Fatalf("opt-in status = %d, want 202", rr.Code)
	}
	if _, ok := triggerAnno(t, c); !ok {
		t.Error("opt-in request should write the trigger annotation")
	}
}

// Auth is required by default (ADR-0020): no authSecretRef AND no opt-in → 401, and
// no trigger annotation is written.
func TestModelWebhook_NoAuthNoOptInRejected(t *testing.T) {
	w, c := newWebhook(t, webhookPolicy("", false, fleetv1.ModelPolicyTriggerWebhook))
	if rr := post(t, w, mpNS, mpName, isaacBody, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when auth is neither configured nor opted out", rr.Code)
	}
	if _, ok := triggerAnno(t, c); ok {
		t.Error("an unauthenticated request must not write the trigger annotation")
	}
}

func TestModelWebhook_UnknownPolicyIs404(t *testing.T) {
	w, _ := newWebhook(t)
	if rr := post(t, w, mpNS, "ghost", isaacBody, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestModelWebhook_NonWebhookPolicyIs404(t *testing.T) {
	w, _ := newWebhook(t, webhookPolicy("", false, fleetv1.ModelPolicyTriggerRegistryWatch))
	if rr := post(t, w, mpNS, mpName, isaacBody, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-Webhook policy", rr.Code)
	}
}

func TestModelWebhook_MalformedPayloadIs400(t *testing.T) {
	w, _ := newWebhook(t, webhookPolicy("", true, fleetv1.ModelPolicyTriggerWebhook)) // opt-in so auth passes to reach body parsing
	if rr := post(t, w, mpNS, mpName, []byte("not json"), ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	// Well-formed JSON but missing required fields → 400.
	if rr := post(t, w, mpNS, mpName, []byte(`{"metrics":{}}`), ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing model_version/model_uri", rr.Code)
	}
}

func TestVerifyHMAC(t *testing.T) {
	body := []byte("payload")
	good := sign(body)
	if !verifyHMAC(body, good, hmacKey) {
		t.Error("valid signature should verify")
	}
	if verifyHMAC(body, good, []byte("wrong-key")) {
		t.Error("wrong key must not verify")
	}
	if verifyHMAC([]byte("tampered"), good, hmacKey) {
		t.Error("tampered body must not verify")
	}
	if verifyHMAC(body, "md5=abc", hmacKey) {
		t.Error("non-sha256 prefix must not verify")
	}
	if verifyHMAC(body, "sha256=zz", hmacKey) {
		t.Error("non-hex signature must not verify")
	}
}
