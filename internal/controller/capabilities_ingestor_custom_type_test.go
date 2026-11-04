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
	"strings"
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// One catalog entry per Custom verb (SupportedAction.custom_type, contract 1.4.0): each
// keyed entry is projected with its own verb and its own parameters, in wire order, and
// the unkeyed Custom entry beside them keeps its empty key.
func TestMapSupportedActions_ProjectsOneEntryPerCustomVerb(t *testing.T) {
	got := mapSupportedActions([]*fav1.SupportedAction{
		{ActionType: "Navigate"},
		{ActionType: "Custom", Description: "a Custom that names no verb"},
		{ActionType: "Custom", CustomType: "explore-floor",
			Params: []*fav1.ActionParam{{Name: "bound_ref", Kind: "string", Required: true}}},
		{ActionType: "Custom", CustomType: "manual-drive",
			Params: []*fav1.ActionParam{{Name: "teleop_topic", Kind: "string", Required: true}}},
	})
	if len(got) != 4 {
		t.Fatalf("want 4 entries (a repeated action type is not collapsed), got %d: %+v", len(got), got)
	}
	want := []struct{ typ, verb, param string }{
		{"Navigate", "", ""},
		{"Custom", "", ""},
		{"Custom", "explore-floor", "bound_ref"},
		{"Custom", "manual-drive", "teleop_topic"},
	}
	for i, w := range want {
		if got[i].ActionType != w.typ || got[i].CustomType != w.verb {
			t.Errorf("[%d] = (%q, %q), want (%q, %q)", i, got[i].ActionType, got[i].CustomType, w.typ, w.verb)
		}
		if w.param != "" && (len(got[i].Params) != 1 || got[i].Params[0].Name != w.param) {
			t.Errorf("[%d] params = %+v, want only %q: a keyed entry carries its own verb's parameters", i, got[i].Params, w.param)
		}
	}
}

// custom_type on a type other than Custom is dropped, not projected: the entry stays the
// type-only entry it always was, so a Navigate is not printed with a verb its type
// contradicts and stays admissible exactly as before.
func TestMapSupportedActions_DropsTheVerbFromANonCustomEntry(t *testing.T) {
	got := mapSupportedActions([]*fav1.SupportedAction{{ActionType: "Navigate", CustomType: "explore-floor"}})
	if len(got) != 1 || got[0].ActionType != "Navigate" {
		t.Fatalf("the Navigate entry must survive, got %+v", got)
	}
	if got[0].CustomType != "" {
		t.Errorf("customType = %q on a Navigate entry, want it dropped", got[0].CustomType)
	}
}

// A verb longer than spec.customType's bound (63) names nothing a FleetAction can carry,
// and the CRD's bound would refuse the WHOLE status write — every valid entry beside it
// with it. The control plane drops that one entry instead.
func TestMapSupportedActions_DropsAnEntryWhoseVerbNoActionCanName(t *testing.T) {
	long := strings.Repeat("v", 64)
	got := mapSupportedActions([]*fav1.SupportedAction{
		{ActionType: "Navigate"},
		{ActionType: "Custom", CustomType: long},
		{ActionType: "Custom", CustomType: strings.Repeat("v", 63)},
	})
	if len(got) != 2 {
		t.Fatalf("want the over-long entry dropped and the other two kept, got %+v", got)
	}
	for _, sa := range got {
		if sa.CustomType == long {
			t.Fatalf("the 64-character verb was projected: %+v", got)
		}
	}
}
