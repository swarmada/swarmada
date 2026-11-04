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
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// kubectl renders a `date` printer column as an age, and a timestamp in the future as
// `<invalid>`. A DiscoveredRobot's status.ttlExpiresAt is in the future for exactly as
// long as the robot waits to be admitted, which is the only time anyone reads the
// column, so the served TTL column must print the timestamp itself.
func TestDiscoveredRobotTTLColumnPrintsTheExpiry(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "swarmada.io_discoveredrobots.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name    string `json:"name"`
				Columns []struct {
					Name     string `json:"name"`
					Type     string `json:"type"`
					JSONPath string `json:"jsonPath"`
				} `json:"additionalPrinterColumns"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	for _, v := range crd.Spec.Versions {
		if v.Name != "v1" {
			continue
		}
		for _, col := range v.Columns {
			if col.JSONPath != ".status.ttlExpiresAt" {
				continue
			}
			if col.Type != "string" {
				t.Errorf("column %s is type %q; kubectl prints <invalid> for a future %s, want type string",
					col.Name, col.Type, col.JSONPath)
			}
			return
		}
	}
	t.Fatalf("%s serves no v1 printer column for .status.ttlExpiresAt", path)
}
