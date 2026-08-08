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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func schedConfig(accept bool) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada", Namespace: actionNS},
		Spec: fleetv1.SwarmadaConfigSpec{
			Scheduling: fleetv1.SwarmadaSchedulingConfig{DefaultAcceptDegradedCapabilities: accept},
		},
	}
}

func degradedAction(accept *bool) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, AcceptDegradedCapabilities: accept},
	}
}

// An explicit per-action value wins over the namespace default.
func TestAcceptDegraded_ActionOverridesNamespaceDefault(t *testing.T) {
	no := false
	r, _ := newActionReconciler(t, schedConfig(true))
	if r.acceptDegraded(context.Background(), degradedAction(&no)) {
		t.Fatal("explicit task acceptDegradedCapabilities=false must override namespace default true")
	}
}

// When the action leaves the field unset, the namespace default applies.
func TestAcceptDegraded_NamespaceDefaultApplies(t *testing.T) {
	r, _ := newActionReconciler(t, schedConfig(true))
	if !r.acceptDegraded(context.Background(), degradedAction(nil)) {
		t.Fatal("namespace default true must apply when the task leaves the field unset")
	}
}

// With no readable config the resolution fails safe to false (Active-only).
func TestAcceptDegraded_FailsSafeWithoutConfig(t *testing.T) {
	r, _ := newActionReconciler(t)
	if r.acceptDegraded(context.Background(), degradedAction(nil)) {
		t.Fatal("must fail safe to false when no SwarmadaConfig is readable")
	}
}
