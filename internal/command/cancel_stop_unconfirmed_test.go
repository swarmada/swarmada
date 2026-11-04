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

package command

import (
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// STOP_UNCONFIRMED reaches the control plane as its own value. Read through the default
// arm it would become a safe stop, which is the claim the value exists to refuse.
func TestCancelDispositionOf_StopUnconfirmedIsItsOwnValue(t *testing.T) {
	if got := cancelDispositionOf(fav1.CancelDisposition_CANCEL_DISPOSITION_STOP_UNCONFIRMED); got != CancelStopUnconfirmed {
		t.Fatalf("cancelDispositionOf(STOP_UNCONFIRMED) = %v, want CancelStopUnconfirmed", got)
	}
}

// The default arm stays a safe stop: a value this build does not know, like the
// UNSPECIFIED a legacy adapter sends, is read as it always was.
func TestCancelDispositionOf_TheDefaultArmIsUnchanged(t *testing.T) {
	for _, d := range []fav1.CancelDisposition{
		fav1.CancelDisposition_CANCEL_DISPOSITION_UNSPECIFIED,
		fav1.CancelDisposition(99),
	} {
		if got := cancelDispositionOf(d); got != CancelStoppedSafely {
			t.Errorf("cancelDispositionOf(%v) = %v, want CancelStoppedSafely", d, got)
		}
	}
}
