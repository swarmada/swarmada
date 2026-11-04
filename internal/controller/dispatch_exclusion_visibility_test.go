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
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// recordedLine is one Info call, with the verbosity it was emitted at. Level 0
// is what a manager running without -zap-log-level prints; anything above it is
// invisible unless an operator has already decided to raise verbosity — which is
// the wrong order for a diagnostic whose whole purpose is to answer a question
// the operator has not asked yet.
type recordedLine struct {
	Level int
	Msg   string
	KV    []any
}

// recordingSink is a logr.LogSink that keeps every Info call and its level.
// Enabled always returns true so the test observes the level the CALLER chose
// rather than a level the sink filtered on.
type recordingSink struct {
	lines *[]recordedLine
	kv    []any
}

func (s recordingSink) Init(logr.RuntimeInfo) {}
func (s recordingSink) Enabled(int) bool      { return true }
func (s recordingSink) Info(level int, msg string, kv ...any) {
	*s.lines = append(*s.lines, recordedLine{Level: level, Msg: msg, KV: append(append([]any{}, s.kv...), kv...)})
}
func (s recordingSink) Error(error, string, ...any) {}
func (s recordingSink) WithValues(kv ...any) logr.LogSink {
	return recordingSink{lines: s.lines, kv: append(append([]any{}, s.kv...), kv...)}
}
func (s recordingSink) WithName(string) logr.LogSink { return s }

// recordingLogCtx returns a context carrying a recording logger, plus the slice
// the lines land in.
func recordingLogCtx() (context.Context, *[]recordedLine) {
	lines := &[]recordedLine{}
	return log.IntoContext(context.Background(), logr.New(recordingSink{lines: lines})), lines
}

// "Why was my robot not picked" is the most common operator question,
// and until this test existed the answer was emitted at V(1) — off at the default
// verbosity a manager runs with. The reason for a withheld robot lives on another
// object (the robot's estopState, or its FleetAdapter's phase/conformance), so an
// operator reading only the action sees a Pending action, an Idle-looking capable
// robot, and nothing connecting them.
//
// RA-1: this is a log path by construction. filterDispatchEligible runs per
// candidate per reconcile, so a status write here would be both an RA-1 violation
// and write amplification. The assertion is on the emitted level, never on a write.
func TestLogDispatchExclusions_EmitsAtDefaultVerbosity(t *testing.T) {
	ctx, lines := recordingLogCtx()
	action := criticalPending("crit")

	logDispatchExclusions(ctx, action, []dispatchExclusion{
		{Robot: "r1", Reason: "robot is under an active emergency stop (estopState Stopped)"},
		{Robot: "r2", Reason: `FleetAdapter "adapter-a" is not fit for dispatch: phase=Degraded conformance=Passed (want Connected/Passed)`},
	})

	if len(*lines) != 2 {
		t.Fatalf("want one line per exclusion, got %d: %+v", len(*lines), *lines)
	}
	for i, l := range *lines {
		if l.Level != 0 {
			t.Errorf("exclusion %d emitted at V(%d); an operator running at default verbosity never sees it", i, l.Level)
		}
	}
}

// The exclusion line is only useful if it names the three things an operator
// needs to correlate: which action was waiting, which robot was withheld, and
// why. Raising the level would be cosmetic if the payload were not there.
func TestLogDispatchExclusions_NamesActionRobotAndReason(t *testing.T) {
	ctx, lines := recordingLogCtx()

	logDispatchExclusions(ctx, criticalPending("crit"), []dispatchExclusion{
		{Robot: "r1", Reason: "robot has no spec.adapter.name"},
	})

	if len(*lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(*lines))
	}
	kv := (*lines)[0].KV
	want := map[string]string{"action": "crit", "robot": "r1", "reason": "robot has no spec.adapter.name"}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		if exp, ok := want[k]; ok {
			if got, _ := kv[i+1].(string); got != exp {
				t.Errorf("key %q = %q, want %q", k, got, exp)
			}
			delete(want, k)
		}
	}
	if len(want) != 0 {
		t.Errorf("exclusion line is missing keys %v; an operator cannot correlate it", want)
	}
}

// Nothing to say, nothing said: an all-eligible fleet must not print a line per
// reconcile at default verbosity.
func TestLogDispatchExclusions_SilentWhenNothingWithheld(t *testing.T) {
	ctx, lines := recordingLogCtx()

	logDispatchExclusions(ctx, criticalPending("crit"), nil)

	if len(*lines) != 0 {
		t.Fatalf("logged %d lines with no exclusions: %+v", len(*lines), *lines)
	}
}

// End to end through the reconciler: an estopped robot is the candidate an
// operator most often asks about, and the reason must reach the default-level
// log from the real dispatch path, not only from a direct call.
func TestReconcile_WithheldRobotReasonIsVisibleAtDefaultVerbosity(t *testing.T) {
	robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	r, _ := newActionReconciler(t, criticalPending("crit"), robot)

	ctx, lines := recordingLogCtx()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "crit", Namespace: actionNS}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, l := range *lines {
		if l.Msg == "robot withheld from dispatch" && l.Level == 0 {
			return
		}
	}
	t.Fatalf("no default-verbosity exclusion line from the dispatch path: %+v", *lines)
}

// The volume half of the same question. The test above puts the exclusion reason at
// default verbosity because an operator could not otherwise see it; the filters
// added since (2 zone, 4 battery, 7 model, 8 maintenance) eliminate candidates as a
// matter of course, and emitting one default-level line per non-matching robot per
// requeue would bury the estop line that motivated the default verbosity. A
// 500-robot namespace with a zoned action would print 499 lines every 15 seconds.
//
// The routine filters therefore log at V(1) — the per-robot detail is still there
// for whoever needs it — while the aggregate reaches the operator at default
// verbosity on the action's own status.message.
func TestLogDispatchExclusions_RoutineFiltersDoNotFloodDefaultVerbosity(t *testing.T) {
	ctx, lines := recordingLogCtx()

	logDispatchExclusions(ctx, criticalPending("crit"), []dispatchExclusion{
		{Robot: "r1", Filter: filterEstop, Reason: "robot is under an active emergency stop (estopState Stopped)"},
		{Robot: "r2", Filter: filterAdapterReady, Reason: `FleetAdapter "adapter-a" is not fit for dispatch`},
		{Robot: "r3", Filter: filterZoneLeafSet, Reason: "robot is in zone aisle-9"},
		{Robot: "r4", Filter: filterMinBattery, Reason: "robot battery is 5%"},
		{Robot: "r5", Filter: filterModelNotUpdating, Reason: "robot is mid model update"},
		{Robot: "r6", Filter: filterZoneMaintenance, Reason: "robot is held by the active ZoneMaintenance zm-1"},
	})

	if len(*lines) != 6 {
		t.Fatalf("want one line per exclusion, got %d", len(*lines))
	}
	byRobot := map[string]int{}
	for _, l := range *lines {
		for i := 0; i+1 < len(l.KV); i += 2 {
			if k, _ := l.KV[i].(string); k == "robot" {
				name, _ := l.KV[i+1].(string)
				byRobot[name] = l.Level
			}
		}
	}
	for _, robot := range []string{"r1", "r2"} {
		if byRobot[robot] != 0 {
			t.Errorf("%s emitted at V(%d); an exceptional exclusion must reach default verbosity",
				robot, byRobot[robot])
		}
	}
	for _, robot := range []string{"r3", "r4", "r5", "r6"} {
		if byRobot[robot] == 0 {
			t.Errorf("%s emitted at V(0); a routine filter logs one line per non-matching robot per "+
				"requeue and would drown the exclusions that matter", robot)
		}
	}
}

// The exclusion line carries the filter NUMBER as its own key, so an operator or a
// log pipeline can group by filter without parsing the prose.
func TestLogDispatchExclusions_NamesTheFilterNumber(t *testing.T) {
	ctx, lines := recordingLogCtx()

	logDispatchExclusions(ctx, criticalPending("crit"), []dispatchExclusion{
		{Robot: "r1", Filter: filterEstop, Reason: "robot is under an active emergency stop"},
	})

	if len(*lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(*lines))
	}
	kv := (*lines)[0].KV
	for i := 0; i+1 < len(kv); i += 2 {
		if k, _ := kv[i].(string); k == "filter" {
			if got, _ := kv[i+1].(int); got == filterEstop {
				return
			}
		}
	}
	t.Errorf("exclusion line does not carry the filter number: %v", kv)
}
