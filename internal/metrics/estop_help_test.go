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
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/swarmada/swarmada/internal/metrics"
)

// estopStateList matches the estop_state value list in the swarmada_robots_in_estop HELP
// text, for example "estop_state: Stopping, Stopped, Failed (Normal and Resuming excluded)".
var estopStateList = regexp.MustCompile(`estop_state: ([^()]+?) \(`)

// TestRobotsInEstopHelp_ListsEveryEmittedState pins the HELP text an operator reads on
// /metrics to the label values SetRobotsInEstop actually writes. A state added to
// EstopStates without the HELP naming it, or a HELP naming a state that is never
// emitted, fails here.
func TestRobotsInEstopHelp_ListsEveryEmittedState(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.RobotsInEstop)
	metrics.SetRobotsInEstop("help-pin", map[string]int{})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var help string
	for _, mf := range mfs {
		if mf.GetName() == "swarmada_robots_in_estop" {
			help = mf.GetHelp()
		}
	}
	if help == "" {
		t.Fatal("swarmada_robots_in_estop not gathered")
	}

	m := estopStateList.FindStringSubmatch(help)
	if m == nil {
		t.Fatalf("HELP has no \"estop_state: … (\" list: %q", help)
	}
	var inHelp []string
	for _, s := range strings.Split(m[1], ",") {
		inHelp = append(inHelp, strings.TrimSpace(s))
	}
	if !slices.Equal(inHelp, metrics.EstopStates) {
		t.Errorf("HELP lists estop_state %v; EstopStates (the values emitted) is %v\nHELP: %q",
			inHelp, metrics.EstopStates, help)
	}
}
