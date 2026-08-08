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
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// defaultSweepInterval is how often the aggregate §9.3.8 gauges are recomputed.
const defaultSweepInterval = 15 * time.Second

// MetricsSweeper periodically recomputes the RFC-0001 §9.3.8 resource-count
// gauges — swarmada_fleetactions_by_phase, swarmada_robots_by_phase,
// swarmada_robots_in_estop, swarmada_fleet_adapter_connected, and
// swarmada_fleet_adapter_phase — from the live resource set.
//
// It is a manager.Runnable that runs on EVERY replica (each replica serves its
// own /metrics), NOT gated on leader election. It is ADDITIVE instrumentation:
// it only reads resources and writes metrics — it never writes resource status,
// so RA-1 is untouched.
//
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetadapters,verbs=get;list;watch
type MetricsSweeper struct {
	client.Client
	// Interval overrides defaultSweepInterval when > 0.
	Interval time.Duration

	// De-stale bookkeeping: keys set on the previous sweep, so a namespace/adapter
	// that has since disappeared has its stale series removed (a deleted adapter
	// must not keep reporting connected=1).
	prevActionNS map[string]bool
	prevRobotNS  map[string]bool
	prevAdapters map[string]bool // "namespace/adapter"
}

// NeedLeaderElection reports false: metrics are emitted per replica, not only on
// the leader, so every replica's /metrics reflects current cluster state.
func (s *MetricsSweeper) NeedLeaderElection() bool { return false }

// Start runs an immediate sweep, then one every Interval until ctx is cancelled.
func (s *MetricsSweeper) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep recomputes every aggregate gauge once. List errors are logged, not fatal
// — metrics are best-effort and must never stall the manager.
func (s *MetricsSweeper) sweep(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("metrics-sweeper")

	// ── FleetAction phase gauge ────────────────────────────────────────────────
	var actions fleetv1.FleetActionList
	if err := s.List(ctx, &actions); err != nil {
		logger.V(1).Info("listing FleetActions failed; skipping fleetactions_by_phase", "err", err.Error())
	} else {
		byNS := map[string]map[string]int{}
		for i := range actions.Items {
			phase := string(actions.Items[i].Status.Phase)
			if phase == "" {
				continue
			}
			ns := actions.Items[i].Namespace
			if byNS[ns] == nil {
				byNS[ns] = map[string]int{}
			}
			byNS[ns][phase]++
		}
		cur := map[string]bool{}
		for ns, counts := range byNS {
			metrics.SetFleetActionsByPhase(ns, counts)
			cur[ns] = true
		}
		for ns := range s.prevActionNS {
			if !cur[ns] {
				metrics.FleetActionsByPhase.DeletePartialMatch(prometheus.Labels{"namespace": ns})
			}
		}
		s.prevActionNS = cur
	}

	// ── Robot phase + estop-state gauges ─────────────────────────────────────
	var robots fleetv1.RobotList
	if err := s.List(ctx, &robots); err != nil {
		logger.V(1).Info("listing Robots failed; skipping robots_by_phase/robots_in_estop", "err", err.Error())
	} else {
		phaseByNS := map[string]map[string]int{}
		estopByNS := map[string]map[string]int{}
		for i := range robots.Items {
			ns := robots.Items[i].Namespace
			if phase := string(robots.Items[i].Status.Phase); phase != "" {
				if phaseByNS[ns] == nil {
					phaseByNS[ns] = map[string]int{}
				}
				phaseByNS[ns][phase]++
			}
			// Only the active estop states are counted (Normal is excluded, §9.3.8).
			switch robots.Items[i].Status.EstopState {
			case fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped:
				if estopByNS[ns] == nil {
					estopByNS[ns] = map[string]int{}
				}
				estopByNS[ns][string(robots.Items[i].Status.EstopState)]++
			}
		}
		cur := map[string]bool{}
		// Union of namespaces from both maps so a namespace with only estop or only
		// phase data is still seeded across both gauges.
		for ns := range phaseByNS {
			cur[ns] = true
		}
		for ns := range estopByNS {
			cur[ns] = true
		}
		for ns := range cur {
			metrics.SetRobotsByPhase(ns, phaseByNS[ns])
			metrics.SetRobotsInEstop(ns, estopByNS[ns])
		}
		for ns := range s.prevRobotNS {
			if !cur[ns] {
				metrics.RobotsByPhase.DeletePartialMatch(prometheus.Labels{"namespace": ns})
				metrics.RobotsInEstop.DeletePartialMatch(prometheus.Labels{"namespace": ns})
			}
		}
		s.prevRobotNS = cur
	}

	// ── FleetAdapter connectivity + phase gauges ─────────────────────────────
	var adapters fleetv1.FleetAdapterList
	if err := s.List(ctx, &adapters); err != nil {
		logger.V(1).Info("listing FleetAdapters failed; skipping fleet_adapter gauges", "err", err.Error())
	} else {
		cur := map[string]bool{}
		for i := range adapters.Items {
			ns, name := adapters.Items[i].Namespace, adapters.Items[i].Name
			phase := string(adapters.Items[i].Status.Phase)
			connected := adapters.Items[i].Status.Phase == fleetv1.FleetAdapterPhaseConnected
			metrics.SetFleetAdapterState(ns, name, phase, connected)
			cur[ns+"/"+name] = true
		}
		for key := range s.prevAdapters {
			if !cur[key] {
				ns, name := splitKey(key)
				metrics.FleetAdapterConnected.DeletePartialMatch(prometheus.Labels{"namespace": ns, "adapter": name})
				metrics.FleetAdapterPhase.DeletePartialMatch(prometheus.Labels{"namespace": ns, "adapter": name})
			}
		}
		s.prevAdapters = cur
	}
}

// splitKey splits a "namespace/adapter" de-stale key at the first slash.
func splitKey(key string) (namespace, adapter string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}
