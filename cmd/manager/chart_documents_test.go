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
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// `helm template` prints whatever the templates produce; `helm install` then refuses any
// document that is not a Kubernetes object ("apiVersion not set"). A template that opens
// with `{{- if … -}}` directly under its licence comment trims the newline between them,
// so the render reads `# … <maintainers@swarmada.io>apiVersion: v1`: the apiVersion line
// becomes part of the comment. Every other chart test reads the render with helm template
// only, so it passes while the install fails. This test holds each rendered document to
// what the install checks.

var chartDocumentSeparator = regexp.MustCompile(`(?m)^---\s*$`)

func TestChartRendersOnlyInstallableDocuments(t *testing.T) {
	for name, args := range map[string][]string{
		"defaults": nil,
		"every optional template on": {
			"--set", "metrics.serviceMonitor.enabled=true",
			"--set", "metrics.prometheusRule.enabled=true",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rendered := mustRender(t, args...)
			for i, doc := range chartDocumentSeparator.Split(rendered, -1) {
				source := "unknown template"
				hasContent := false
				for _, line := range strings.Split(doc, "\n") {
					trimmed := strings.TrimSpace(line)
					if s, ok := strings.CutPrefix(trimmed, "# Source: "); ok {
						source = s
					}
					if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
						hasContent = true
					}
				}
				if !hasContent {
					continue
				}
				var obj struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
				}
				if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
					t.Errorf("document %d (%s) is not valid YAML: %v", i, source, err)
					continue
				}
				if obj.APIVersion == "" || obj.Kind == "" {
					t.Errorf("document %d (%s) renders apiVersion=%q kind=%q; helm install refuses it "+
						"(a `{{-` directly under the template's header comment glues the first line onto the comment)",
						i, source, obj.APIVersion, obj.Kind)
				}
			}
		})
	}
}
