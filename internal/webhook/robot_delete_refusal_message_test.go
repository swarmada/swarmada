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

package webhook

import (
	"strings"
	"testing"
	"time"
)

// The robot delete refusal for a robot that records an action. While the action is bound to the
// robot the controller renews its lease on every pass, so a time named in the refusal would be
// later at the next attempt; the refusal names the step that releases the record instead. A time
// is named only for a horizon that nothing renews, and no refusal says a cancel was sent.

var refusalDead = time.Date(2026, 10, 8, 8, 14, 2, 0, time.UTC)

const refusalTime = "2026-10-08T08:14:02Z"

func TestExecutorRecordRefusal_BoundActionNamesTheStepAndNoTime(t *testing.T) {
	for _, what := range []string{"its delete", "removing its last finalizer"} {
		msg := executorRecordRefusal("r1", "ns", "x", "r1", what, refusalDead)
		for _, want := range []string{
			`robot "r1" in namespace "ns" records FleetAction "x", which is bound to it`,
			"Cancel or requeue the action first",
			what + " is admitted once the robot's adapter acknowledges the cancel",
			"While the robot is reachable its lease is renewed",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: refusal does not say %q:\n%s", what, want, msg)
			}
		}
		for _, bad := range []string{refusalTime, "the cancel it was sent", "until 2"} {
			if strings.Contains(msg, bad) {
				t.Errorf("%s: refusal for a bound action says %q, which the lease's renewal makes false:\n%s", what, bad, msg)
			}
		}
	}
}

func TestExecutorRecordRefusal_ReleasedHorizonNamesTheTime(t *testing.T) {
	msg := executorRecordRefusal("r1", "ns", "x", "", "its delete", refusalDead)
	for _, want := range []string{
		"and may still be running it",
		"acknowledges a cancel for the action",
		"until " + refusalTime + ", when the lease the robot last held is provably dead",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not say %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "the cancel it was sent") {
		t.Errorf("refusal claims a cancel was sent:\n%s", msg)
	}
}

func TestExecutorRecordRefusal_NoKnownTimeNamesNone(t *testing.T) {
	cases := map[string]string{}
	cases["no horizon"] = executorRecordRefusal("r1", "ns", "x", "", "its delete", time.Time{})
	cases["bound to another robot"] = executorRecordRefusal("r1", "ns", "x", "r2", "its delete", refusalDead)
	for name, msg := range cases {
		if strings.Contains(msg, refusalTime) || strings.Contains(msg, "until 2") {
			t.Errorf("%s: refusal names a time no lease of this robot proves:\n%s", name, msg)
		}
		for _, want := range []string{"acknowledges a cancel for the action", "cancel or requeue the action first"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: refusal does not say %q:\n%s", name, want, msg)
			}
		}
	}
}

func TestExecutorRecordRefusal_GrammarAndCitation(t *testing.T) {
	for _, msg := range []string{
		executorRecordRefusal("r1", "ns", "x", "r1", "its delete", refusalDead),
		executorRecordRefusal("r1", "ns", "x", "", "its delete", refusalDead),
		executorRecordRefusal("r1", "ns", "x", "", "its delete", time.Time{}),
	} {
		if !strings.HasPrefix(msg, "robot ") || strings.HasSuffix(msg, ".") {
			t.Errorf("refusal must start lowercase with the object and carry no trailing period:\n%s", msg)
		}
		if !strings.HasSuffix(msg, "(RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee)") {
			t.Errorf("refusal does not cite the section that states the rule:\n%s", msg)
		}
	}
}
