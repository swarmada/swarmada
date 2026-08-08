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
	"time"

	"github.com/go-logr/logr"
)

// ConfigSource loads the edge node's current Config from the control plane (zone
// polygons + robot→zone assignments). A control-plane outage — the exact scenario
// the edge node exists for — surfaces here as an error; the Syncer then retains the
// last-known-good config rather than blanking out.
type ConfigSource interface {
	Load(ctx context.Context) (Config, error)
}

// DefaultSyncInterval is the poll cadence when none is set.
const DefaultSyncInterval = 30 * time.Second

// Syncer keeps a Node's Config current by periodically pulling from a ConfigSource
// and hot-swapping it in via Node.SetConfig. It FAILS SAFE: on a load error it keeps
// the node operating on its last-synced config and never applies an empty config over
// a non-empty one, so a transient outage or partial read cannot silently drop the
// zones/robots the node is guarding.
type Syncer struct {
	node     *Node
	source   ConfigSource
	interval time.Duration
	log      logr.Logger

	// applied counts configs successfully hot-swapped; lastGood is when the node last
	// held a source-confirmed config. Both are for observability/tests only.
	applied  int
	lastGood time.Time
}

// NewSyncer builds a Syncer. A non-positive interval uses DefaultSyncInterval.
func NewSyncer(node *Node, source ConfigSource, interval time.Duration, log logr.Logger) *Syncer {
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	return &Syncer{node: node, source: source, interval: interval, log: log}
}

// Run performs an initial sync, then re-syncs every interval until ctx is cancelled.
// It never returns an error for a failed sync — a failed sync is the fail-safe path
// (retain last-known-good), not a fatal condition.
func (s *Syncer) Run(ctx context.Context) error {
	s.syncOnce(ctx) // best-effort initial load; the bootstrap config remains if it fails
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.syncOnce(ctx)
		}
	}
}

// syncOnce loads once and applies the result unless doing so would be unsafe.
func (s *Syncer) syncOnce(ctx context.Context) {
	cfg, err := s.source.Load(ctx)
	if err != nil {
		// FAIL SAFE: keep operating on the last-synced config. The node keeps
		// guarding its zones through the outage; nothing is dropped.
		s.logger().Info("edge config sync failed; retaining last-known-good config", "error", err.Error())
		return
	}
	// FAIL SAFE: a successful-but-empty read must not blank out active guards. Refuse
	// to replace a non-empty zone set with an empty one; a real zone removal will
	// still apply on a load that carries the remaining zones.
	if len(cfg.Zones) == 0 && len(s.node.view().cfg.Zones) > 0 {
		s.logger().Info("edge config sync returned zero zones; retaining last-known-good to avoid blanking active guards")
		return
	}
	s.node.SetConfig(cfg)
	s.applied++
	s.lastGood = time.Now()
	s.logger().Info("edge config synced", "zones", len(cfg.Zones), "robots", len(cfg.RobotZone))
}

func (s *Syncer) logger() logr.Logger {
	if s.log.GetSink() == nil {
		return logr.Discard()
	}
	return s.log
}
