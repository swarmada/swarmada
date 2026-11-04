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

// A ServiceAccount cannot add members to a draft (RFC-0001 #security-permission-matrix):
// with inputs and flow immutable and activation refused to it, growing the draft was the
// one way left for automation to change what a person approves. Everything else a
// ServiceAccount writes to a draft stays admitted, and a person keeps drafting.

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestFleetTaskDraftGrowth(t *testing.T) {
	allow := func() *FleetTaskValidator {
		return &FleetTaskValidator{
			AppendAuthz:   &fakeEstopAuthz{allow: true},
			CancelAuthz:   &fakeEstopAuthz{allow: true},
			ActivateAuthz: &fakeEstopAuthz{allow: true},
		}
	}
	draft := func(actions ...string) *fleetv1.FleetTask {
		var as []fleetv1.FleetTaskAction
		for _, a := range actions {
			as = append(as, ftAct(a))
		}
		t := ftWith("t1", as...)
		t.Spec.Draft = true
		return t
	}
	const sa = "system:serviceaccount:ops:planner"
	saGroups := []string{"system:serviceaccounts", "system:serviceaccounts:ops"}

	refusedRows := map[string]struct {
		user   string
		groups []string
	}{
		"a ServiceAccount by username":                    {sa, saGroups},
		"a ServiceAccount by group, odd username":         {"planner", []string{"system:serviceaccounts"}},
		"a ServiceAccount by username, no group supplied": {sa, nil},
	}
	for name, row := range refusedRows {
		_, err := allow().ValidateUpdate(ctxAs(row.user, row.groups...), draft("a"), draft("a", "b"))
		if err == nil || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "ServiceAccount") ||
			!strings.Contains(err.Error(), "draft") {
			t.Errorf("%s appending a member to a draft was not refused as a ServiceAccount growing a draft: %v", name, err)
		}
	}

	labelled := draft("a")
	labelled.Labels = map[string]string{"seen": "yes"}
	finalized := draft("a")
	finalized.Finalizers = []string{"example.com/hold"}
	cancelled := draft("a")
	cancelled.Spec.DesiredState = fleetv1.DesiredStateCancelled
	live := draft("a")
	live.Spec.Draft = false
	liveGrown := draft("a", "b")
	liveGrown.Spec.Draft = false
	for name, row := range map[string]struct {
		user     string
		groups   []string
		old, new *fleetv1.FleetTask
	}{
		"a person appending to a draft":             {"alice", nil, draft("a"), draft("a", "b")},
		"a ServiceAccount labelling a draft":        {sa, saGroups, draft("a"), labelled},
		"a ServiceAccount adding a finalizer":       {sa, saGroups, draft("a"), finalized},
		"a ServiceAccount cancelling a draft":       {sa, saGroups, draft("a"), cancelled},
		"a ServiceAccount appending to a live task": {sa, saGroups, live, liveGrown},
	} {
		if _, err := allow().ValidateUpdate(ctxAs(row.user, row.groups...), row.old, row.new); err != nil {
			t.Errorf("%s was refused; only a ServiceAccount growing a draft is: %v", name, err)
		}
	}
}
