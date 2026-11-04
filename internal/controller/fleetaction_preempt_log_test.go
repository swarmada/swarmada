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
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The preemption log line names the preemptor's own priority. Both Critical and High
// preempt (RFC-0001 #control-plane-scheduler), so a line that always says Critical
// misleads an operator reading why a High action took a robot.
func TestPreempt_LogNamesThePreemptorsPriority(t *testing.T) {
	for _, prio := range []fleetv1.ActionPriority{fleetv1.ActionPriorityHigh, fleetv1.ActionPriorityCritical} {
		t.Run(string(prio), func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
			r, c := newActionReconciler(t,
				pendingInBand("preemptor", prio),
				bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
				robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"),
			)
			ctx, lines := recordingLogCtx()
			if _, err := r.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "preemptor", Namespace: actionNS},
			}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
				t.Fatalf("victim phase = %s, want Preempted", v.Status.Phase)
			}
			var found []string
			for _, l := range *lines {
				if strings.Contains(l.Msg, "preempting") {
					found = append(found, fmt.Sprintf("%s %v", l.Msg, l.KV))
				}
			}
			if len(found) != 1 {
				t.Fatalf("preemption log lines = %q, want one", found)
			}
			other := fleetv1.ActionPriorityCritical
			if prio == fleetv1.ActionPriorityCritical {
				other = fleetv1.ActionPriorityHigh
			}
			if !strings.Contains(found[0], string(prio)) || strings.Contains(found[0], string(other)) {
				t.Errorf("preemption by a %s action logged %q", prio, found[0])
			}
		})
	}
}
