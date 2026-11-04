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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// entry is one catalog entry: an action type and, for Custom, the verb it is keyed by.
type entry struct{ typ, verb string }

func adapterWithEntries(name, ns string, entries ...entry) *fleetv1.FleetAdapter {
	sa := make([]fleetv1.SupportedAction, 0, len(entries))
	for _, e := range entries {
		sa = append(sa, fleetv1.SupportedAction{ActionType: e.typ, CustomType: e.verb})
	}
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     fleetv1.FleetAdapterStatus{SupportedActions: sa},
	}
}

func customAction(ns, verb string) *fleetv1.FleetAction {
	a := actionOfType(ns, "Custom")
	a.Spec.CustomType = verb
	return a
}

// The catalog pre-filter matches a Custom action on (type, customType): a verb is
// admitted when some catalog keys an entry by it, or carries an unkeyed Custom entry
// (which declares every verb its adapter serves that no keyed entry names). It refuses
// only when every Custom entry in the namespace is keyed and none by this verb.
func TestFleetActionSupported_CustomVerbMatchedAgainstKeyedEntries(t *testing.T) {
	ns := "warehouse-a"
	cases := []struct {
		name    string
		catalog []entry
		verb    string
		admit   bool
	}{
		{"a keyed entry names the verb", []entry{{"Custom", "explore-floor"}}, "explore-floor", true},
		{"no keyed entry names it and none is unkeyed", []entry{{"Navigate", ""}, {"Custom", "explore-floor"}}, "mow", false},
		{"an unkeyed Custom entry covers it", []entry{{"Custom", "explore-floor"}, {"Custom", ""}}, "mow", true},
		{"only an unkeyed Custom entry (today's catalog)", []entry{{"Custom", ""}}, "mow", true},
		{"a Custom action naming no verb, keyed entries only", []entry{{"Custom", "explore-floor"}}, "", true},
		{"two keyed entries, the second names it", []entry{{"Custom", "explore-floor"}, {"Custom", "manual-drive"}}, "manual-drive", true},
		{"no Custom entry at all", []entry{{"Navigate", ""}}, "explore-floor", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &FleetActionValidator{Reader: newSupportedReader(t, adapterWithEntries("sim", ns, tc.catalog...))}
			_, err := v.ValidateCreate(context.Background(), customAction(ns, tc.verb))
			if tc.admit && err != nil {
				t.Fatalf("want admitted, got: %v", err)
			}
			if !tc.admit && err == nil {
				t.Fatal("want refused: no catalog in the namespace declares this verb")
			}
		})
	}
}

// Catalogs are pooled across the namespace's adapters, as they are for a type: a verb one
// adapter keys is admitted even when another adapter's catalog keys only other verbs.
func TestFleetActionSupported_CustomVerbPooledAcrossAdapters(t *testing.T) {
	ns := "warehouse-a"
	v := &FleetActionValidator{Reader: newSupportedReader(t,
		adapterWithEntries("ros2", ns, entry{"Custom", "explore-floor"}),
		adapterWithEntries("mower", ns, entry{"Custom", "mow"}))}
	if _, err := v.ValidateCreate(context.Background(), customAction(ns, "mow")); err != nil {
		t.Fatalf("want admitted on the second adapter's entry, got: %v", err)
	}
}
