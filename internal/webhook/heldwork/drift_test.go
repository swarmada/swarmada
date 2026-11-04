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

package heldwork_test

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

// manifestWebhook is one webhook of the generated manifest: its name and the path it is served on.
type manifestWebhook struct{ name, path string }

// manifestWebhooks parses config/webhook/manifests.yaml, both configuration kinds.
func manifestWebhooks(t *testing.T) []manifestWebhook {
	t.Helper()
	path := filepath.Join(repoRoot, "config", "webhook", "manifests.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []manifestWebhook
	add := func(name string, cc admissionregistrationv1.WebhookClientConfig) {
		if cc.Service == nil || cc.Service.Path == nil {
			t.Errorf("%s: webhook %s has no clientConfig.service.path", path, name)
			return
		}
		out = append(out, manifestWebhook{name: name, path: *cc.Service.Path})
	}
	for _, doc := range bytes.Split(raw, []byte("\n---")) {
		switch {
		case bytes.Contains(doc, []byte("kind: MutatingWebhookConfiguration")):
			var c admissionregistrationv1.MutatingWebhookConfiguration
			if err := yaml.UnmarshalStrict(doc, &c); err != nil {
				t.Fatalf("parsing a MutatingWebhookConfiguration in %s: %v", path, err)
			}
			for _, w := range c.Webhooks {
				add(w.Name, w.ClientConfig)
			}
		case bytes.Contains(doc, []byte("kind: ValidatingWebhookConfiguration")):
			var c admissionregistrationv1.ValidatingWebhookConfiguration
			if err := yaml.UnmarshalStrict(doc, &c); err != nil {
				t.Fatalf("parsing a ValidatingWebhookConfiguration in %s: %v", path, err)
			}
			for _, w := range c.Webhooks {
				add(w.Name, w.ClientConfig)
			}
		}
	}
	// Not vacuous: every clientConfig in the text was parsed into a webhook.
	if want := strings.Count(string(raw), "clientConfig:"); len(out) != want || want == 0 {
		t.Fatalf("%s: parsed %d webhooks, the text holds %d clientConfig blocks", path, len(out), want)
	}
	return out
}

// TestDrift_EveryManifestWebhookIsServed (T13): every webhook in the generated manifest has a
// handler on the rig's manager. The rig's wiring is a copy of cmd/manager/main.go's, so this
// turns red the day a webhook is added to the markers and not to registerWebhooks.
func TestDrift_EveryManifestWebhookIsServed(t *testing.T) {
	requireRig(t)
	mux := rigMgr.GetWebhookServer().WebhookMux()
	hooks := manifestWebhooks(t)
	for _, w := range hooks {
		if _, pattern := mux.Handler(&http.Request{Method: http.MethodPost, URL: &url.URL{Path: w.path}}); pattern != w.path {
			t.Errorf("DRIFT: the manifest's webhook %s (%s) has no handler on the held-work rig (pattern %q): "+
				"add its registration to registerWebhooks as cmd/manager/main.go wires it", w.name, w.path, pattern)
		}
	}
	t.Logf("RESULT %d manifest webhooks checked against the rig's handlers", len(hooks))
}
