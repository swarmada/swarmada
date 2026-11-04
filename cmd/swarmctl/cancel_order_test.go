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
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// promptStreams returns IOStreams whose stdin would block forever if read. A
// command that prompts under these streams hangs rather than passing, so a test
// asserting "does not prompt" fails loudly instead of silently succeeding on a
// prompt that read EOF and defaulted to no.
func promptStreams(out *bytes.Buffer) cli.IOStreams {
	return cli.IOStreams{In: neverReader{}, Out: out, Err: out}
}

type neverReader struct{}

func (neverReader) Read([]byte) (int, error) {
	panic("cancel prompted before it had verified the object exists and the caller is permitted")
}

// A typo'd or wrong-kind name must report not-found without drawing a destructive
// prompt: the operator is not asked to confirm cancelling an object that was never
// verified to exist.
func TestCancelUnknownNameDoesNotPrompt(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	var out bytes.Buffer
	o := &options{streams: promptStreams(&out), outputFmt: cli.OutputTable}

	// yes=false, so a prompt would actually read stdin — which panics here.
	err := o.actionCancel(context.Background(), c, authorizer(true), ns, "typo-4471", "", false)
	if err == nil {
		t.Fatal("expected an error for a name that does not exist")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got %v", err)
	}
	if strings.Contains(out.String(), "Cancel fleetaction") {
		t.Errorf("a confirmation prompt was written for an object that was never fetched:\n%s", out.String())
	}
}

// Naming a FleetTask produces a wrong-kind error regardless of the caller's
// fleetactions/cancel permission. With the SSAR gate running before the fetch, this
// caller — who is denied that verb — was told about permissions instead, which
// reports the wrong problem on a destructive verb under time pressure.
func TestCancelReportsWrongKindBeforePermission(t *testing.T) {
	ns := "warehouse-a"
	task := testTask(ns, "restock-7", fleetv1.DesiredStateRunning, 2)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).Build()
	var out bytes.Buffer
	o := &options{streams: promptStreams(&out), outputFmt: cli.OutputTable}

	// authorizer(false) denies fleetactions/cancel.
	err := o.actionCancel(context.Background(), c, authorizer(false), ns, "restock-7", "", false)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "is a FleetTask") {
		t.Fatalf("expected a wrong-kind error naming FleetTask, got %v", err)
	}
	if !strings.Contains(err.Error(), "cancel task restock-7") {
		t.Errorf("the error must name the command that works; got %v", err)
	}
}

// The symmetric case: a FleetAction name given to `cancel task`.
func TestCancelTaskReportsWrongKind(t *testing.T) {
	ns := "warehouse-a"
	action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "pick-4471", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(action).Build()
	var out bytes.Buffer
	o := &options{streams: promptStreams(&out), outputFmt: cli.OutputTable}

	err := o.taskCancel(context.Background(), c, authorizer(false), ns, "pick-4471", "", false)
	if err == nil || !strings.Contains(err.Error(), "is a FleetAction") {
		t.Fatalf("expected a wrong-kind error naming FleetAction, got %v", err)
	}
}

// The package's mutating commands share one order. This asserts the observable
// consequence of the rule documented in lifecycle.go — that no mutating command
// reports a permission failure, or prompts, for an object that does not exist —
// rather than the source order, which a test cannot see.
//
// `delete robot` is included: it fetches first by design (it routes on which kind
// the name resolves to), which is the same order, reached for a different reason.
//
// EVERY mutating command in the package is a case here, and that is the point.
// From 2026-09-08 to 2026-09-14 this test held three of them — the two `cancel`
// paths and `delete robot` — and passed, while the package was recorded as having
// one order across all of them. It did not: `estop trigger` and `estop-clear`
// still showed an EMERGENCY-STOP confirmation for a zone they had not yet read,
// and `admit`, `reject`, `policy reset` and `rollout resume` still gated before
// fetching. A uniformity test that covers a subset cannot see the thing it is
// named for, so when a mutating command is added, add it here.
func TestVerbGateOrderIsUniform(t *testing.T) {
	ns := "warehouse-a"
	ctx := context.Background()

	empty := func() client.Client { return fake.NewClientBuilder().WithScheme(testScheme(t)).Build() }

	cases := []struct {
		name string
		run  func(o *options, c *bytes.Buffer) error
	}{
		{"cancel action", func(o *options, _ *bytes.Buffer) error {
			return o.actionCancel(ctx, empty(), authorizer(false), ns, "absent", "", false)
		}},
		{"cancel task", func(o *options, _ *bytes.Buffer) error {
			return o.taskCancel(ctx, empty(), authorizer(false), ns, "absent", "", false)
		}},
		{"delete robot", func(o *options, _ *bytes.Buffer) error {
			return o.deleteAdmittedRobot(ctx, empty(), ns, "absent", false)
		}},
		{"estop trigger", func(o *options, _ *bytes.Buffer) error {
			return o.estopTrigger(ctx, empty(), authorizer(false), ns, "absent", "drill", false)
		}},
		{"estop clear", func(o *options, _ *bytes.Buffer) error {
			return o.estopClear(ctx, empty(), authorizer(false), ns, "absent", "drill over", false)
		}},
		{"admit", func(o *options, _ *bytes.Buffer) error {
			return o.admit(ctx, empty(), authorizer(false), ns, "absent", &admitOptions{zone: "z", adapter: "a"})
		}},
		{"reject", func(o *options, _ *bytes.Buffer) error {
			return o.reject(ctx, empty(), authorizer(false), ns, "absent", "stale", false)
		}},
		{"policy reset", func(o *options, _ *bytes.Buffer) error {
			return o.modelPolicyReset(ctx, empty(), authorizer(false), ns, "absent", "why", false)
		}},
		{"rollout resume", func(o *options, _ *bytes.Buffer) error {
			return o.rolloutResume(ctx, empty(), authorizer(false), ns, "absent", "firmware", "why", false)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			o := &options{streams: promptStreams(&out), outputFmt: cli.OutputTable}
			err := tc.run(o, &out)
			if err == nil {
				t.Fatal("expected an error for an object that does not exist")
			}
			// The caller is denied every custom verb, yet the object is absent.
			// The absence is the true condition and must be the one reported.
			if strings.Contains(strings.ToLower(err.Error()), "not permitted") ||
				strings.Contains(strings.ToLower(err.Error()), "not authorized") ||
				strings.Contains(strings.ToLower(err.Error()), "forbidden") {
				t.Errorf("reported a permission failure for an absent object: %v", err)
			}
			// The absent object must be named as absent, not merely "some error".
			if !strings.Contains(strings.ToLower(err.Error()), "not found") {
				t.Errorf("an absent object was not reported as absent: %v", err)
			}
			if strings.Contains(out.String(), "?") {
				t.Errorf("prompted for an absent object:\n%s", out.String())
			}
		})
	}
}

// The gate order must still be gate-before-write: a caller who is denied the verb
// on an object that DOES exist is refused, and nothing is written.
func TestCancelDeniedVerbOnExistingObjectStillRefuses(t *testing.T) {
	ns := "warehouse-a"
	action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "pick-4471", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(action).Build()
	var out bytes.Buffer
	o := &options{streams: promptStreams(&out), outputFmt: cli.OutputTable}

	if err := o.actionCancel(context.Background(), c, authorizer(false), ns, "pick-4471", "", false); err == nil {
		t.Fatal("expected a permission error for an existing object the caller may not cancel")
	}
	got := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "pick-4471"}, got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[annCancelRequested]; ok {
		t.Error("a denied cancel wrote the cancel-requested annotation")
	}
}
