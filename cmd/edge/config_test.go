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
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "edge.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigValid(t *testing.T) {
	path := writeConfig(t, `{
		"zones": [{"name": "zone-a", "floor": 1, "polygon": [
			{"x": 0, "y": 0}, {"x": 10, "y": 0}, {"x": 10, "y": 10}, {"x": 0, "y": 10}]}],
		"robotZone": {"amr-1": "zone-a"}
	}`)
	cfg, err := loadConfig(path, "warehouse-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Namespace != "warehouse-a" {
		t.Errorf("namespace = %q, want warehouse-a", cfg.Namespace)
	}
	if len(cfg.Zones) != 1 || cfg.Zones[0].Name != "zone-a" || cfg.Zones[0].Floor != 1 {
		t.Fatalf("zones = %+v", cfg.Zones)
	}
	if len(cfg.Zones[0].Polygon) != 4 {
		t.Errorf("polygon vertices = %d, want 4", len(cfg.Zones[0].Polygon))
	}
	if cfg.RobotZone["amr-1"] != "zone-a" {
		t.Errorf("robotZone = %+v", cfg.RobotZone)
	}
}

func TestLoadConfigRejectsShortPolygon(t *testing.T) {
	path := writeConfig(t, `{"zones": [{"name": "z", "polygon": [{"x":0,"y":0},{"x":1,"y":1}]}]}`)
	if _, err := loadConfig(path, "ns"); err == nil {
		t.Fatal("expected error for a polygon with < 3 vertices")
	}
}

func TestLoadConfigRejectsRobotInUnknownZone(t *testing.T) {
	path := writeConfig(t, `{
		"zones": [{"name": "z", "polygon": [{"x":0,"y":0},{"x":1,"y":0},{"x":0,"y":1}]}],
		"robotZone": {"amr-1": "does-not-exist"}
	}`)
	if _, err := loadConfig(path, "ns"); err == nil {
		t.Fatal("expected error for a robot assigned to an unknown zone")
	}
}

func TestLoadConfigRejectsDuplicateZone(t *testing.T) {
	path := writeConfig(t, `{"zones": [
		{"name": "z", "polygon": [{"x":0,"y":0},{"x":1,"y":0},{"x":0,"y":1}]},
		{"name": "z", "polygon": [{"x":2,"y":2},{"x":3,"y":2},{"x":2,"y":3}]}]}`)
	if _, err := loadConfig(path, "ns"); err == nil {
		t.Fatal("expected error for a duplicate zone name")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := writeConfig(t, `{"zones": [], "bogusField": 1}`)
	if _, err := loadConfig(path, "ns"); err == nil {
		t.Fatal("expected error for an unknown field (strict decoding)")
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.json"), "ns"); err == nil {
		t.Fatal("expected error for a missing config file")
	}
}
