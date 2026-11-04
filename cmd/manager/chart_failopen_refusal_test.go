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

package main

import (
	"sort"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

// webhooksFollowingTheValue renders the chart with failurePolicy=Fail and returns the name of
// every webhook that renders Fail. The chart renders a literal Ignore on the webhooks whose
// generated policy is Ignore, so these are exactly the ones webhooks.failurePolicy=Ignore opens.
func webhooksFollowingTheValue(t *testing.T) []string {
	t.Helper()
	rendered := mustRender(t, "--set", "webhooks.failurePolicy=Fail")
	var names []string
	for _, doc := range strings.Split(rendered, "\n---") {
		var head struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			continue
		}
		switch head.Kind {
		case "ValidatingWebhookConfiguration":
			var cfg admissionv1.ValidatingWebhookConfiguration
			if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
				t.Fatalf("parse %s: %v", head.Kind, err)
			}
			for _, w := range cfg.Webhooks {
				if w.FailurePolicy != nil && *w.FailurePolicy == admissionv1.Fail {
					names = append(names, w.Name)
				}
			}
		case "MutatingWebhookConfiguration":
			var cfg admissionv1.MutatingWebhookConfiguration
			if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
				t.Fatalf("parse %s: %v", head.Kind, err)
			}
			for _, w := range cfg.Webhooks {
				if w.FailurePolicy != nil && *w.FailurePolicy == admissionv1.Fail {
					names = append(names, w.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

// The refusal of failurePolicy=Ignore is what an operator reads before acknowledging fail-open,
// so it names every webhook the value opens, derived here from the rendered chart rather than
// from a list kept beside the message. The robots/status estop guard is named on its own: with
// it open, an estop release through a status write is admitted while the webhook server is down
// (RFC-0001 #security-role-definitions).
func TestChartFailOpenRefusalNamesEveryWebhookTheValueOpens(t *testing.T) {
	opened := webhooksFollowingTheValue(t)
	if len(opened) < 2 {
		t.Fatalf("the rendered chart has %d webhook(s) whose failurePolicy follows the value (%v); "+
			"this test read nothing", len(opened), opened)
	}
	out, err := renderChart(t, "--set", "webhooks.failurePolicy=Ignore")
	if err == nil {
		t.Fatalf("webhooks.failurePolicy=Ignore rendered without the acknowledgement")
	}
	var missing []string
	for _, name := range opened {
		if !strings.Contains(out, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the refusal does not name %d of the %d webhooks Ignore opens: %v\n%s",
			len(missing), len(opened), missing, out)
	}
	if !strings.Contains(out, "robots/status") {
		t.Errorf("the refusal does not say that the robots/status estop guard is opened:\n%s", out)
	}
}
