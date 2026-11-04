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

func b(v bool) *bool { return &v }

// Each default-true boolean must read nil as TRUE — the schema default — and must honour an
// explicit false. The second half is the whole point: before these became pointers, false was
// unrepresentable and the operator's setting was silently discarded.
func TestDefaultTrueBoolAccessors(t *testing.T) {
	t.Run("PausesOnError", func(t *testing.T) {
		if !(RollingUpdateStrategy{}).PausesOnError() {
			t.Error("nil must read true (schema default)")
		}
		if !(RollingUpdateStrategy{PauseOnError: b(true)}).PausesOnError() {
			t.Error("explicit true must read true")
		}
		if (RollingUpdateStrategy{PauseOnError: b(false)}).PausesOnError() {
			t.Error("explicit false must read FALSE")
		}
	})

	t.Run("RequiresIdleState", func(t *testing.T) {
		if !(RolloutSafetyConstraints{}).RequiresIdleState() {
			t.Error("nil must read true (schema default)")
		}
		if (RolloutSafetyConstraints{RequireIdleState: b(false)}).RequiresIdleState() {
			t.Error("explicit false must read FALSE")
		}
	})

	t.Run("PropagatesToChildren", func(t *testing.T) {
		if !(ZoneEstopPolicy{}).PropagatesToChildren() {
			t.Error("nil must read true — RFC-0001 gives propagateToChildren a default of true")
		}
		if (ZoneEstopPolicy{PropagateToChildren: b(false)}).PropagatesToChildren() {
			t.Error("explicit false must read FALSE: an estop scoped to one zone is a specified choice")
		}
	})

	// The odd one out, deliberately. The prohibition is enforced by the absence of any
	// auto-clear path, not by the field, so the stored value cannot change the answer.
	// Admission warns when it is set false; the accessor never varies.
	t.Run("ExplicitClearRequiredAfterEstop is unconditional", func(t *testing.T) {
		for _, v := range []*bool{nil, b(true), b(false)} {
			if !(SwarmadaEstopConfig{RequireExplicitClearAfterEstop: v}).ExplicitClearRequiredAfterEstop() {
				t.Errorf("must be true for every stored value, got false for %v", v)
			}
		}
	})
}

// RollingUpdateOrDefault's built-in fallback must agree with the schema default, or a rollout
// with no strategy block would behave differently from one that spells out the defaults.
func TestRollingUpdateOrDefaultPausesOnError(t *testing.T) {
	if !(RolloutStrategy{}).RollingUpdateOrDefault().PausesOnError() {
		t.Error("the built-in fallback strategy must pause on error, like the schema default")
	}
}
