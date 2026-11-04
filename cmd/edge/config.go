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
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/swarmada/swarmada/internal/edge"
	"github.com/swarmada/swarmada/internal/zone"
)

// configFile is the on-disk shape of the edge node's cached view (edge.Config).
// It is the bootstrap/static source for sub-item 1; sub-item 2 keeps edge.Config
// live-synced from the FleetZone and Robot resources. The field layout mirrors
// FleetZone.spec.zones[].polygon and the robot→zone assignments.
type configFile struct {
	Zones []struct {
		Name    string `json:"name"`
		Floor   int32  `json:"floor"`
		Polygon []struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		} `json:"polygon"`
	} `json:"zones"`
	// RobotZone maps robotID → assigned zone name. A frame for a robot absent from
	// this map is ignored by the Node (it cannot act on an unknown/forged robot).
	RobotZone map[string]string `json:"robotZone"`
}

// loadConfig reads and validates the edge node's bootstrap Config from a JSON file.
// namespace is supplied by the operator (it identifies this node in its local audit
// log), not embedded in the file.
func loadConfig(path, namespace string) (edge.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return edge.Config{}, fmt.Errorf("read edge config %q: %w", path, err)
	}
	var cf configFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cf); err != nil {
		return edge.Config{}, fmt.Errorf("parse edge config %q: %w", path, err)
	}
	return cf.toConfig(namespace)
}

func (cf configFile) toConfig(namespace string) (edge.Config, error) {
	cfg := edge.Config{
		Namespace: namespace,
		Zones:     make([]edge.ZonePolygon, 0, len(cf.Zones)),
		RobotZone: map[string]string{},
	}
	seen := map[string]struct{}{}
	for i, z := range cf.Zones {
		if z.Name == "" {
			return edge.Config{}, fmt.Errorf("zone[%d]: name is required", i)
		}
		if _, dup := seen[z.Name]; dup {
			return edge.Config{}, fmt.Errorf("zone %q: duplicate zone name", z.Name)
		}
		seen[z.Name] = struct{}{}
		// A polygon needs at least 3 vertices to bound an area (mirrors the FleetZone
		// CRD's MinItems=3). Fewer cannot define containment, so reject rather than
		// silently guard nothing.
		if len(z.Polygon) < 3 {
			return edge.Config{}, fmt.Errorf("zone %q: polygon needs at least 3 vertices, got %d", z.Name, len(z.Polygon))
		}
		poly := make([]zone.Point, len(z.Polygon))
		for j, p := range z.Polygon {
			poly[j] = zone.Point{X: p.X, Y: p.Y}
		}
		cfg.Zones = append(cfg.Zones, edge.ZonePolygon{Name: z.Name, Floor: z.Floor, Polygon: poly})
	}
	for robot, zoneName := range cf.RobotZone {
		if _, ok := seen[zoneName]; !ok {
			return edge.Config{}, fmt.Errorf("robot %q assigned to unknown zone %q", robot, zoneName)
		}
		cfg.RobotZone[robot] = zoneName
	}
	return cfg, nil
}
