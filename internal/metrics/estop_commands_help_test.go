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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/swarmada/swarmada/internal/metrics"
)

// TestEstopCommandsHelp_SaysDeliveredNotIssued pins the swarmada_estop_commands_total HELP to
// what the counter counts (RFC-0001 #control-plane-observability-prometheus-metrics-contract):
// estop Commands that reached an adapter. An estop that could not be delivered increments it
// not at all, so a HELP that says "issued" invites reading a quiet counter as "no estop
// failed".
func TestEstopCommandsHelp_SaysDeliveredNotIssued(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.EstopCommandsTotal)
	metrics.IncEstopCommand("help-pin", "ad", metrics.ScopeRobot, metrics.ResultAckStopped)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var help string
	for _, mf := range mfs {
		if mf.GetName() == "swarmada_estop_commands_total" {
			help = mf.GetHelp()
		}
	}
	if help == "" {
		t.Fatal("swarmada_estop_commands_total not gathered")
	}
	if strings.Contains(help, "issued") {
		t.Errorf("HELP says \"issued\"; the counter counts only Commands that reached an adapter: %q", help)
	}
	for _, claim := range []string{"reached an adapter", "not counted"} {
		if !strings.Contains(help, claim) {
			t.Errorf("HELP does not say %q: %q", claim, help)
		}
	}
}
