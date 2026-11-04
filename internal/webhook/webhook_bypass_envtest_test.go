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
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const fleetZoneValidatingHook = "vfleetzone.swarmada.io"

// writeWithoutTheFleetZoneWebhook makes write with the FleetZone validating webhook
// unregistered, then registers it again and waits until the API server calls it. It
// builds a state the webhook refuses today but a cluster can still hold: one admitted
// before a rule existed, or by two writes admitted concurrently, each checked against
// the other's old value.
//
// write is retried until it is admitted, which is how the unregistration is known to
// have reached the API server. The return is known by a probe zone that names itself
// as parent, a write the webhook always refuses and nothing else does.
func writeWithoutTheFleetZoneWebhook(t *testing.T, w *safetyWire, write func() error) {
	t.Helper()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := admissionregistrationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(w.cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	var list admissionregistrationv1.ValidatingWebhookConfigurationList
	if err := c.List(ctx, &list); err != nil {
		t.Fatalf("listing validating webhook configurations: %v", err)
	}
	var cfg *admissionregistrationv1.ValidatingWebhookConfiguration
	for i := range list.Items {
		for _, h := range list.Items[i].Webhooks {
			if h.Name == fleetZoneValidatingHook {
				cfg = &list.Items[i]
			}
		}
	}
	if cfg == nil {
		t.Fatalf("no validating webhook configuration registers %s", fleetZoneValidatingHook)
	}
	saved := append([]admissionregistrationv1.ValidatingWebhook(nil), cfg.Webhooks...)

	const probe = "fz-webhook-probe"
	if err := w.admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: probe, Namespace: w.ns}}); err != nil {
		t.Fatalf("creating the probe zone: %v", err)
	}

	kept := cfg.Webhooks[:0:0]
	for _, h := range cfg.Webhooks {
		if h.Name != fleetZoneValidatingHook {
			kept = append(kept, h)
		}
	}
	cfg.Webhooks = kept
	if err := c.Update(ctx, cfg); err != nil {
		t.Fatalf("unregistering %s: %v", fleetZoneValidatingHook, err)
	}
	restored := false
	restore := func() {
		cur := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cfg), cur); err != nil {
			t.Errorf("reading the webhook configuration to restore it: %v", err)
			return
		}
		cur.Webhooks = saved
		if err := c.Update(ctx, cur); err != nil {
			t.Errorf("re-registering %s: %v", fleetZoneValidatingHook, err)
			return
		}
		restored = true
	}
	t.Cleanup(func() {
		if !restored {
			restore()
		}
	})

	eventually(t, "the write admitted with the FleetZone webhook unregistered", func() (bool, string) {
		if err := write(); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	eventually(t, "the probe zone naming itself as parent", func() (bool, string) {
		err := patchFleetZone(ctx, w.admin, w.ns, probe, func(z *fleetv1.FleetZone) { z.Spec.ParentZone = probe })
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	})

	restore()
	n := 0
	eventually(t, "the API server calling the FleetZone webhook again", func() (bool, string) {
		n++
		err := patchFleetZone(ctx, w.admin, w.ns, probe, func(z *fleetv1.FleetZone) {
			z.Labels = map[string]string{"probe": fmt.Sprint(n)}
		})
		if err != nil && strings.Contains(err.Error(), "cannot be its own parentZone") {
			return true, ""
		}
		return false, fmt.Sprintf("probe write: %v", err)
	})
}
