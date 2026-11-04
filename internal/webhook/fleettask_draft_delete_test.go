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

// A parked draft is a proposal: no child action exists and none will be generated while
// spec.draft is true (ADR-0062). Declining it is deleting it. The delete guard's
// allowlist is fail-closed by design, so a phase it does not name is refused — and a
// Draft that the operator could see but not delete would be a proposal that can only
// be accepted.
func TestFleetTaskDelete_AllowedWhenDraft(t *testing.T) {
	v := &FleetTaskValidator{}
	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "proposed", Namespace: "ops"},
		Spec:       fleetv1.FleetTaskSpec{Draft: true},
		Status:     fleetv1.FleetTaskStatus{Phase: fleetv1.FleetTaskPhaseDraft},
	}
	if _, err := v.ValidateDelete(context.Background(), task); err != nil {
		t.Fatalf("deleting a parked Draft was refused; declining a proposal must be possible: %v", err)
	}
}
