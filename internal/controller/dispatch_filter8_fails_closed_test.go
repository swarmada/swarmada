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
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// RFC-0001 candidate filter 8 reads the ZoneMaintenance list. When that read fails,
// no robot can be shown to be clear of an active window, so the filter withholds every
// candidate and the action requeues. Before, the filter was skipped and the action was
// dispatched to whichever candidate the other filters passed — including a robot a
// window was about to pause.
func TestDispatch_Filter8_FailsClosedWhenTheMaintenanceListCannotBeRead(t *testing.T) {
	r, c := newActionReconciler(t, pendingInBand("t1", fleetv1.ActionPriorityNormal), idleCandidate("r1"))
	fails := true
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*fleetv1.ZoneMaintenanceList); ok && fails {
				return errors.New("etcdserver: request timed out")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	var mu sync.Mutex
	var lines []string
	logger := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), logger)

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "t1", Namespace: actionNS}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ft := getAction(t, c, "t1")
	if ft.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q with the ZoneMaintenance list unreadable: filter 8 was skipped "+
			"and the action dispatched to a robot no window could be shown not to hold", ft.Status.AssignedRobot)
	}
	if ft.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending (held, to be retried)", ft.Status.Phase)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v; a held action must be retried", res.RequeueAfter)
	}
	if !strings.Contains(ft.Status.Message, "filter 8") {
		t.Errorf("status.message = %q; want it to name filter 8", ft.Status.Message)
	}

	// The failure is logged at the default level, as an error, where an operator sees it.
	mu.Lock()
	var logged bool
	for _, l := range lines {
		if strings.Contains(l, "filter 8 failed closed") && strings.Contains(l, `"error"=`) {
			logged = true
		}
	}
	mu.Unlock()
	if !logged {
		t.Errorf("no error-level log line says filter 8 failed closed; lines:\n%s", strings.Join(lines, "\n"))
	}

	// Latency, not a lost action: once the list reads, the same action dispatches.
	fails = false
	reconcileAction(t, r, "t1")
	if got := getAction(t, c, "t1").Status.AssignedRobot; got != "r1" {
		t.Fatalf("after the list recovered, assignedRobot = %q, want r1", got)
	}
}
