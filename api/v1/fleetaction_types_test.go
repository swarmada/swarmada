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

package v1

import "testing"

func TestActionPriority_CanPreempt(t *testing.T) {
	cases := map[ActionPriority]bool{
		ActionPriorityCritical: true,
		ActionPriorityHigh:     true,
		ActionPriorityNormal:   false,
		ActionPriorityLow:      false,
		ActionPriority(""):     false,
	}
	for prio, want := range cases {
		if got := prio.CanPreempt(); got != want {
			t.Errorf("ActionPriority(%q).CanPreempt() = %v, want %v", prio, got, want)
		}
	}
}
