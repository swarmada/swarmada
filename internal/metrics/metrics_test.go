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

package metrics_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/swarmada/swarmada/internal/metrics"
)

// allRegistered is the exact §9.3.8 metric-name set the chart's alerts and dashboards
// depend on. Registration MUST produce exactly these, no more, no fewer.
// allRegistered is every §9.3.8 metric Register installs. The name deliberately carries no
// tally: a count baked into an identifier goes stale the moment a metric is added and has to
// be chased through comments and test names, which is the drift this file exists to catch.
var allRegistered = []string{
	"swarmada_estop_command_latency_seconds",
	"swarmada_estop_latency_violations_total",
	"swarmada_estop_commands_total",
	"swarmada_telemetry_dropped_frames_total",
	"swarmada_telemetry_tsdb_write_errors_total",
	"swarmada_telemetry_status_writes_total",
	"swarmada_telemetry_frames_received_total",
	"swarmada_scheduler_assignment_latency_seconds",
	"swarmada_scheduler_assignment_failures_total",
	"swarmada_scheduler_lease_renewals_total",
	"swarmada_scheduler_lease_expiries_total",
	"swarmada_fleetactions_by_phase",
	"swarmada_fleet_adapter_connected",
	"swarmada_fleet_adapter_phase",
	"swarmada_fleet_adapter_reconnects_total",
	"swarmada_robots_by_phase",
	"swarmada_robots_in_estop",
	"swarmada_robot_offline_duration_seconds",
	// Set by Register itself rather than by seedAll — a build-info gauge that is
	// registered but never observed exports nothing at all.
	"swarmada_version",
}

// seedAll touches every metric with a representative label set so a fresh
// registry gathers every family WITH their full label dimensions (a *Vec with
// no children is not gathered). Values are irrelevant here — only presence and
// label names are asserted.
func seedAll(ns string) {
	metrics.ObserveEstopLatency(ns, "ad", "r", metrics.ScopeRobot, 100*time.Millisecond)
	metrics.IncEstopLatencyViolation(ns, "ad", "r")
	metrics.IncEstopCommand(ns, "ad", metrics.ScopeRobot, metrics.ResultAckStopped)
	metrics.TelemetryDroppedFramesTotal.WithLabelValues(ns, "ad").Add(0)
	metrics.TelemetryTSDBWriteErrorsTotal.WithLabelValues(ns, "prometheus", "unknown").Add(0)
	metrics.TelemetryStatusWritesTotal.WithLabelValues(ns, "phase_change").Add(0)
	metrics.TelemetryFramesReceivedTotal.WithLabelValues(ns, "ad").Add(0)
	metrics.SchedulerAssignmentLatencySeconds.WithLabelValues(ns, "Normal").Observe(0.1)
	metrics.SchedulerAssignmentFailuresTotal.WithLabelValues(ns, "NoCandidates").Add(0)
	metrics.SchedulerLeaseRenewalsTotal.WithLabelValues(ns).Add(0)
	metrics.SchedulerLeaseExpiriesTotal.WithLabelValues(ns).Add(0)
	metrics.FleetAdapterReconnectsTotal.WithLabelValues(ns, "ad").Add(0)
	metrics.RobotOfflineDurationSeconds.WithLabelValues(ns).Observe(1)
	metrics.InitNamespace(ns)     // fleetactions_by_phase, robots_by_phase, robots_in_estop
	metrics.InitAdapter(ns, "ad") // fleet_adapter_connected, fleet_adapter_phase
}

// gatherFamilies registers into a fresh registry, seeds, and returns name→labelSet.
func gatherFamilies(t *testing.T, ns string) map[string]map[string]bool {
	t.Helper()
	reg := prometheus.NewRegistry()
	metrics.Register(reg)
	seedAll(ns)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]map[string]bool{}
	for _, mf := range mfs {
		labels := map[string]bool{}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = true
			}
		}
		out[mf.GetName()] = labels
	}
	return out
}

// Register installs EXACTLY the §9.3.8 metric set — no more, no fewer. A metric added to
// the code without a matching row in §9.3.8 now also fails `make spec-check`.
func TestRegister_ExactlyTheDeclaredMetrics(t *testing.T) {
	fams := gatherFamilies(t, "reg-test")
	got := make([]string, 0, len(fams))
	for name := range fams {
		if strings.HasPrefix(name, "swarmada_") {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	want := append([]string(nil), allRegistered...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registered §9.3.8 metrics mismatch:\n got:  %v\n want: %v", got, want)
	}
}

// Register a second time into another registry must not panic (multi-registry safe).
func TestRegister_IsMultiRegistrySafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Register panicked on a second registry: %v", r)
		}
	}()
	metrics.Register(prometheus.NewRegistry())
}

// InitNamespace seeds the phase/estop gauges to 0 for a namespace — a not-yet-seen
// phase reads 0, not absent (§9.3.8 registration requirement). Critically the
// Revoking phase, which §9.3.8 calls out explicitly.
func TestInitNamespace_ZeroesGaugesIncludingRevoking(t *testing.T) {
	ns := "init-zero-ns" // unique so no other test's emissions perturb it
	metrics.InitNamespace(ns)

	if v := testutil.ToFloat64(metrics.FleetActionsByPhase.WithLabelValues(ns, "Revoking")); v != 0 {
		t.Errorf("fleetactions_by_phase{Revoking} = %v, want 0 at init", v)
	}
	if v := testutil.ToFloat64(metrics.RobotsInEstop.WithLabelValues(ns, "Stopped")); v != 0 {
		t.Errorf("robots_in_estop{Stopped} = %v, want 0 at init", v)
	}
	if v := testutil.ToFloat64(metrics.RobotsByPhase.WithLabelValues(ns, "Offline")); v != 0 {
		t.Errorf("robots_by_phase{Offline} = %v, want 0 at init", v)
	}
	// Every FleetAction phase enum value must be present at 0.
	for _, p := range metrics.FleetActionPhases {
		if v := testutil.ToFloat64(metrics.FleetActionsByPhase.WithLabelValues(ns, p)); v != 0 {
			t.Errorf("fleetactions_by_phase{%s} = %v, want 0", p, v)
		}
	}
}

// alertExprs are the §9.3.8 "minimum alert expressions" the Helm chart ships. This
// test is the correctness contract between the chart's PromQL and the registered
// metric surface: every metric name and label selector the alerts reference MUST
// exist in the registered metrics, or the alerts silently never fire.
var alertExprs = []string{
	`increase(swarmada_estop_latency_violations_total[5m]) > 0`,
	`increase(swarmada_telemetry_dropped_frames_total[5m]) > 0`,
	`increase(swarmada_telemetry_tsdb_write_errors_total[5m]) > 0`,
	`histogram_quantile(0.99, rate(swarmada_scheduler_assignment_latency_seconds_bucket{priority="Normal"}[10m])) > 60`,
	`swarmada_fleetactions_by_phase{phase="Revoking"} > 0`,
	`swarmada_fleet_adapter_connected == 0`,
	`swarmada_robots_in_estop{estop_state="Stopped"} > 0`,
}

var (
	reMetric   = regexp.MustCompile(`swarmada_[a-z0-9_]+`)
	reSelector = regexp.MustCompile(`swarmada_[a-z0-9_]+\{([^}]*)\}`)
	reLabelKey = regexp.MustCompile(`(\w+)\s*=`)
)

// baseName strips the histogram query suffixes so a `_bucket` reference resolves
// to the registered family name.
func baseName(n string) string {
	for _, suf := range []string{"_bucket", "_count", "_sum"} {
		if strings.HasSuffix(n, suf) {
			return strings.TrimSuffix(n, suf)
		}
	}
	return n
}

func TestPromQLAlerts_ReferenceRegisteredMetricsAndLabels(t *testing.T) {
	fams := gatherFamilies(t, "promql-ns")

	for _, expr := range alertExprs {
		// Every metric name in the expr resolves to a registered family.
		for _, raw := range reMetric.FindAllString(expr, -1) {
			name := baseName(raw)
			if _, ok := fams[name]; !ok {
				t.Errorf("alert expr %q references unregistered metric %q (base %q)", expr, raw, name)
			}
		}
		// Every label selector references a label the metric actually carries.
		for _, sel := range reSelector.FindAllStringSubmatch(expr, -1) {
			whole, inner := sel[0], sel[1]
			metric := baseName(reMetric.FindString(whole))
			labels := fams[metric]
			for _, lk := range reLabelKey.FindAllStringSubmatch(inner, -1) {
				key := lk[1]
				if !labels[key] {
					t.Errorf("alert expr %q selects label %q not present on metric %q (labels: %v)",
						expr, key, metric, labels)
				}
			}
		}
	}
}
