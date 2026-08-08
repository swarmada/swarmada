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

package edge

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/zone"
)

// KubeConfigSource is a ConfigSource backed by the Kubernetes API: it derives the
// edge node's Config from the FleetZone and Robot resources in its namespace. Zones
// come from each FleetZone's spec.physicalBounds; robot→zone assignments from each
// Robot's spec.zone. The edge node PULLS on the Syncer's cadence; a control-plane
// partition makes Load return an error, and the Syncer then retains last-known-good.
type KubeConfigSource struct {
	reader    client.Reader
	namespace string
}

// NewKubeConfigSource builds a source that reads the given namespace.
func NewKubeConfigSource(reader client.Reader, namespace string) *KubeConfigSource {
	return &KubeConfigSource{reader: reader, namespace: namespace}
}

// Load lists the namespace's FleetZones and Robots and assembles a Config. A list
// error (e.g. the API server is unreachable during a partition) is returned so the
// Syncer can fail safe. A FleetZone with no physicalBounds is skipped (it cannot be
// evaluated for containment); a Robot whose spec.zone has no bounded FleetZone is
// omitted from the map, so the node never acts on a zone it cannot evaluate.
func (s *KubeConfigSource) Load(ctx context.Context) (Config, error) {
	var zones fleetv1.FleetZoneList
	if err := s.reader.List(ctx, &zones, client.InNamespace(s.namespace)); err != nil {
		return Config{}, fmt.Errorf("list FleetZones in %q: %w", s.namespace, err)
	}
	var robots fleetv1.RobotList
	if err := s.reader.List(ctx, &robots, client.InNamespace(s.namespace)); err != nil {
		return Config{}, fmt.Errorf("list Robots in %q: %w", s.namespace, err)
	}

	cfg := Config{
		Namespace: s.namespace,
		Zones:     make([]ZonePolygon, 0, len(zones.Items)),
		RobotZone: make(map[string]string, len(robots.Items)),
		EdgeZones: make(map[string]bool),
	}
	bounded := make(map[string]struct{}, len(zones.Items))
	for i := range zones.Items {
		fz := &zones.Items[i]
		pb := fz.Spec.PhysicalBounds
		if pb == nil || len(pb.Polygon) < 3 {
			continue // no evaluable boundary → cannot guard this zone
		}
		poly := make([]zone.Point, len(pb.Polygon))
		for j, p := range pb.Polygon {
			poly[j] = zone.Point{X: p.X, Y: p.Y}
		}
		cfg.Zones = append(cfg.Zones, ZonePolygon{Name: fz.Name, Floor: pb.Floor, Polygon: poly})
		bounded[fz.Name] = struct{}{}
		// A bounded zone that declares an edge node expects EdgeStream feeds for its
		// robots; the feed reporter raises EdgeFeedUnavailable for missing ones.
		if fz.Spec.EdgeNode != nil {
			cfg.EdgeZones[fz.Name] = true
		}
	}
	for i := range robots.Items {
		r := &robots.Items[i]
		if _, ok := bounded[r.Spec.Zone]; ok {
			cfg.RobotZone[r.Name] = r.Spec.Zone
		}
	}
	return cfg, nil
}
