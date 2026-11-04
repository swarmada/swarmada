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
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The adapter's decline reason on a real API server: written once, on the transition, and
// an unchanged reason on every later requeue costs no write at all — the resourceVersion
// does not move (RA-1). A Pending action requeues on a timer for as long as it is refused,
// so a write per pass would be a telemetry tick by another name.
func TestEnvtest_AdapterDeclined_WrittenOnTheTransitionOnly(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	v := &stubValidator{byRobot: map[string]command.ValidateOutcome{
		"amr-1": {Servable: false, Message: keepoutReason},
	}}
	rec := record.NewFakeRecorder(16)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Validator: v, Recorder: rec}

	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "ward", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	f12SeedRobot(t, ns, "amr-1", "ward")
	req := zrSeedAction(t, ns, "act-1", "ward")

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	got := zrGetAction(t, req)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "AdapterDeclined")
	if cond == nil {
		t.Fatalf("no AdapterDeclined condition (message %q): the adapter's reason is only in "+
			"the manager's log", got.Status.Message)
	}
	if cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, keepoutReason) ||
		!strings.Contains(cond.Message, `"amr-1"`) || !strings.Contains(cond.Message, `"acme-adapter"`) {
		t.Errorf("condition = %+v, want True naming the adapter, the robot and the reason", *cond)
	}
	if got.Status.Phase != fleetv1.ActionPhasePending || got.Status.RetryCount != 0 {
		t.Errorf("phase %q retryCount %d, want Pending 0", got.Status.Phase, got.Status.RetryCount)
	}
	rv := got.ResourceVersion

	for i := 2; i <= 4; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if again := zrGetAction(t, req); again.ResourceVersion != rv {
		t.Errorf("resourceVersion %s -> %s on an unchanged reason: a write per requeue",
			rv, again.ResourceVersion)
	}
	if ev := warnings(rec); len(ev) != 1 {
		t.Errorf("%d events for one reason repeated, want 1: %q", len(ev), ev)
	}

	v.byRobot["amr-1"] = command.ValidateOutcome{Servable: false, Message: "the charging cable is in"}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 5: %v", err)
	}
	moved := zrGetAction(t, req)
	if moved.ResourceVersion == rv {
		t.Errorf("a new reason was not written")
	}
	if c := apimeta.FindStatusCondition(moved.Status.Conditions, "AdapterDeclined"); c == nil ||
		!strings.Contains(c.Message, "the charging cable is in") {
		t.Errorf("condition after a new reason = %v", c)
	}
	if ev := warnings(rec); len(ev) != 1 || !strings.Contains(ev[0], "the charging cable is in") {
		t.Errorf("events after a new reason = %q, want exactly one more, carrying it", ev)
	}
}
